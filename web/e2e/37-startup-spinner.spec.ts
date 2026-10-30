/**
 * Loading state of the full-page startup gates: **show nothing when fast, show a spinner only when slow**.
 *
 * # What this group guards
 *
 * `AuthGate` (confirm login state) and `WorkspaceGate` (load the workspace by key) are chained on
 * the path of every full-page refresh. Each used to show a line of text ("checking login state",
 * "opening workspace"), so one refresh flashed two lines in a row, often under half a second in
 * total: too fast to read, and nothing worth reading anyway. Now it is `StartupSpinner`: **it
 * renders null for the first 300ms**, and only after that shows a centered spinner with no text.
 *
 * Both directions must be guarded; guarding only one is fake:
 *
 *   - only guarding "show nothing when fast" -> deleting the spinner entirely is still all green,
 *     and slow networks degrade into a blank, unresponsive screen;
 *   - only guarding "show when slow" -> changing the delay to 0 is still all green, which is exactly
 *     the flash this change set out to remove.
 *
 * # ⚠️ "Should not appear" cannot use toHaveCount(0); that is **the deepest trap here**
 *
 * `expect(...).toHaveCount(0)` **retries automatically until timeout** (this repo sets 10s). So its
 * real meaning is "disappears sooner or later", not "never appeared": once loading ends the spinner
 * is gone anyway, so the assertion always turns green. **This was really hit**: with the quiet
 * period changed to 0 (the spinner was there from 44ms until 581ms), **not one** of the three cases
 * went red, while that was exactly the behavior this change bans.
 *
 * So "never appeared" can only be tested like this: **before the page script runs**
 * (`addInitScript`), install a recorder that polls every frame and records each time the spinner
 * appears/disappears, then **read that record** at the end. It is a statement about the past and
 * cannot be washed clean by retries. Likewise do not use `waitForTimeout` + `toHaveCount(0)`: the
 * snapshot at the moment it wakes up also gets rescued by retries.
 */

import { test, expect, clearSlowGatesMock, slowGatesMock } from "./fixtures";
import type { Page } from "@playwright/test";

/** One state flip captured by the recorder. `at` is milliseconds since the page script started. */
interface SpinnerEvent {
  present: boolean;
  at: number;
  role: string | null;
  ariaLabel: string | null;
  /** Visible text inside the spinner container. This change requires it to always be an empty string. */
  text: string;
}

/**
 * Install the recorder **before the next navigation**: look at whether the spinner is there every
 * frame, and record a change when it flips.
 *
 * rAF instead of MutationObserver: `addInitScript` runs at document_start, when
 * `document.documentElement` may not exist yet, so `observe()` would throw, while the record array
 * has already been created, so reading it back gives an empty array, which looks like "the spinner
 * never appeared". **This false green was really hit once**; when debugging, first confirm
 * `__spinnerRecorderAlive` is true before trusting this log.
 */
async function recordSpinner(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const w = window as unknown as {
      __spinnerLog?: unknown[];
      __spinnerRecorderAlive?: boolean;
    };
    w.__spinnerLog = [];
    w.__spinnerRecorderAlive = true;
    const t0 = performance.now();
    let was = false;
    const tick = () => {
      const el = document.querySelector('[data-testid="startup-spinner"]');
      const present = !!el;
      if (present !== was) {
        was = present;
        w.__spinnerLog!.push({
          present,
          at: Math.round(performance.now() - t0),
          role: el ? el.getAttribute("role") : null,
          ariaLabel: el ? el.getAttribute("aria-label") : null,
          text: el ? ((el as HTMLElement).innerText || "").trim() : "",
        });
      }
      requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
  });
}

/** Read back the record, first confirming the recorder is really alive (otherwise an empty array would be misread as "never appeared"). */
async function spinnerLog(page: Page): Promise<SpinnerEvent[]> {
  const alive = await page.evaluate(
    () => (window as unknown as { __spinnerRecorderAlive?: boolean }).__spinnerRecorderAlive === true,
  );
  expect(alive, "spinner 记录器没活下来，这份日志不能用来判断「没出现过」").toBe(true);
  return page.evaluate(
    () => ((window as unknown as { __spinnerLog?: SpinnerEvent[] }).__spinnerLog ?? []) as SpinnerEvent[],
  );
}

test.afterEach(async ({ app }) => {
  // workers=1, localStorage shared serially: not restoring it makes every later spec's startup wait longer.
  await clearSlowGatesMock(app);
});

test("快路径：整个启动过程中 spinner 一次都不出现", async ({ app }) => {
  // The mock's two gates take 90ms each, both within the 300ms quiet period: this is what most refreshes look like.
  await recordSpinner(app);
  await app.goto("/w/growth");

  // First wait for the page to really finish loading, so "the whole startup" has a definite end; then look back at the record.
  await expect(app.getByTestId("app-scroll")).toBeAttached();

  const log = await spinnerLog(app);
  expect(log.filter((e) => e.present), `spinner 不该出现过，实际记录：${JSON.stringify(log)}`).toEqual([]);
});

test("快路径：门耗时逼近但未超过静默期时，仍然什么都不显示", async ({ app }) => {
  // Drag the gates to 90+150=240ms: still under the 300ms quiet period. This locks in **the threshold
  // meaning itself**: not "too fast to see", but "not shown as long as the quiet period is not exceeded".
  await slowGatesMock(app, 150);
  await recordSpinner(app);
  await app.goto("/w/growth");
  await expect(app.getByTestId("app-scroll")).toBeAttached();

  const log = await spinnerLog(app);
  expect(log.filter((e) => e.present), `未超过静默期不该出 spinner，实际记录：${JSON.stringify(log)}`).toEqual([]);
});

test("慢路径：超过静默期后 spinner 出现（无文字、可读屏），加载完自己消失", async ({ app }) => {
  // Each of the two gates waits an extra 1.2s, well over the 300ms quiet period.
  await slowGatesMock(app, 1_200);
  await recordSpinner(app);
  await app.goto("/w/growth");

  // Past the quiet period -> the spinner must appear. **This is the only guarantee against "degrading into a blank, unresponsive screen".**
  const spinner = app.getByTestId("startup-spinner").first();
  await expect(spinner).toBeVisible();

  // After loading the page must show up normally and the spinner must go away by itself; otherwise it
  // is stuck, not "slow".
  await expect(app.getByTestId("app-scroll")).toBeAttached();

  const log = await spinnerLog(app);
  const appeared = log.filter((e) => e.present);
  expect(appeared.length, `spinner 该出现过，实际记录：${JSON.stringify(log)}`).toBeGreaterThan(0);

  // Visually there is no text (one point of this change), but screen readers must still know it is loading.
  for (const e of appeared) {
    expect(e.text, "spinner 上不该有可见文字").toBe("");
    expect(e.role).toBe("status");
    expect(e.ariaLabel ?? "").toMatch(/加载中|Loading/);
  }

  // The last flip must be "disappear", otherwise it is stuck in the loading state.
  expect(log[log.length - 1]?.present, `spinner 没有消失，记录：${JSON.stringify(log)}`).toBe(false);
});

test("两段旧文案彻底消失", async ({ app }) => {
  // The i18n keys (login.checking / ws.gateOpening) were deleted, and the hard-coded line is gone too.
  // Slow down startup so the "loading" window really exists: if the page opened instantly, this would test nothing.
  await slowGatesMock(app, 1_200);
  await app.goto("/w/growth");

  // Grab the whole page text while the spinner is showing: that is when the two old lines would appear.
  await expect(app.getByTestId("startup-spinner").first()).toBeVisible();
  const bodyText = await app.evaluate(() => document.body.innerText);
  expect(bodyText).not.toContain("正在确认登录状态");
  expect(bodyText).not.toContain("正在打开空间");

  await expect(app.getByTestId("app-scroll")).toBeAttached();
});
