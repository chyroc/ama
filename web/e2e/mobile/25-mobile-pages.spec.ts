/**
 * Per-page mobile contract (iPhone 16 Pro, 402×681).
 *
 * Split with 23-mobile-shell: that one guards the **shell** (fixed top bar, no horizontal overflow, content area scrolls on its own),
 * this one guards whether **the inside of each page** is still usable at 402px. The precondition shared by both (wait for the shell to render) is
 * repeated once in `gotoShell` below. It is deliberately not shared across files; each group of tests should run on its own.
 *
 * # Why there is not a single `toHaveCount(0)` here
 *
 * This repo was bitten twice: `toHaveCount(0)` is satisfied in the frame **before the element has rendered**, so
 * assertions of the "something that should not appear" kind are often falsely green. So:
 *
 *   - "the desktop left column should not be there on narrow screens" → assert instead that **the drawer entry exists and is clickable** (positive existence),
 *     and that the list is **really visible** after opening it;
 *   - "two columns squeezed together" → compare the `y` of the two cells instead (in a single column the lower one has a larger y);
 *     a geometric value is never satisfied by accident because "it has not rendered yet".
 *
 * # What it does not guard
 *
 * Safe areas (`env(safe-area-inset-*)` is always 0 in Chromium), the soft keyboard, standalone mode.
 * The drawer uses `--safe-top/bottom`, but both values are 0px here, so it **must be checked on a real device**.
 */

import { test, expect } from "../fixtures";
import type { Page } from "@playwright/test";

/**
 * Open the URL and **wait until the shell has really rendered**.
 *
 * ⚠️ Required, not just to be safe: when `AuthGate` mounts the page content is not yet in the DOM, **and during that time
 * the whole page is empty**: the startup gate's loading hint became "a text-less spinner that only appears after 300ms"
 * (see StartupSpinner), and on the fast path there is not a single visible element.
 * The foundation tests saw one false green ("no horizontal overflow" passed on a page with only a spinner);
 * **an empty page only makes that trap deeper**, so every test here passes through this gate first.
 */
async function gotoShell(page: Page, path: string) {
  await page.goto(path);
  await expect(page.getByTestId("mobile-topbar")).toBeVisible();
  await expect(page.getByTestId("app-scroll")).toBeAttached();
}

/**
 * The conversation list must have an entry point on narrow screens: this is the most serious feature gap in this change.
 *
 * Before the change, `chat-new` and `chat-list` both measured **0×0** (hidden by the left column with `hidden … lg:flex`),
 * and the page had no other entry point: on a phone you **could not start a conversation, nor see past conversations**.
 * The breakpoint is `lg:` (1024px), so tablets and small laptops were hit too.
 */
test("窄屏：会话列表能从抽屉打开，且能新建对话", async ({ app }) => {
  await gotoShell(app, "/w/growth/chat");

  /*
    Pin the viewport width first. Without this, if the project is misconfigured (device preset overwritten) the test quietly runs on
    a 1280-wide desktop viewport, where the left column is always shown, so this assertion would say nothing.
  */
  expect(await app.evaluate(() => window.innerWidth)).toBe(402);

  // The entry button is in the title bar, visible and clickable.
  const opener = app.getByTestId("chat-sessions-open");
  await expect(opener).toBeVisible();

  /*
    ⚠️ **Before opening, confirm the list is unusable**, otherwise this test would also be green when "the left column was never hidden".

    Use `toBeHidden()` instead of `toHaveCount(0)`: the former holds for both "not in the DOM" and "in the DOM but
    not visible", while the latter only holds when the count is 0. If the implementation goes back to "hide a copy with CSS",
    the count is 1; that implementation clearly has a bug (duplicate testid), yet this assertion would go red in the wrong place.
  */
  await expect(app.getByTestId("chat-list")).toBeHidden();

  await opener.click();

  // The list in the drawer is really visible, and the Feishu conversation from the seed is in it (proving it lists real data, not an empty shell).
  const list = app.getByTestId("chat-list");
  await expect(list).toBeVisible();
  const box = await list.boundingBox();
  /*
    Width and height must both be > 0. This line points straight at the old failure: back then `chat-list` was 0×0:
    the element was in the DOM and the testid could be found, **but the user could not see a single pixel**.
    Isn't asserting `toBeVisible()` enough? It is, but 0×0 is exactly one of the reasons Playwright counts something as invisible;
    writing the numbers out makes the failure tell you directly "how big it is".
  */
  expect(box?.width ?? 0).toBeGreaterThan(0);
  expect(box?.height ?? 0).toBeGreaterThan(0);

  // The new-conversation button is in the drawer too, and clicking it really navigates (the URL goes back to the chat page without runId).
  await expect(app.getByTestId("chat-new")).toBeVisible();
  await app.getByTestId("chat-new").click();
  await expect(app).toHaveURL("/w/growth/chat");

  /*
    After clicking, the drawer must close; otherwise the list covers the conversation just opened, and the user has to tap empty space again to see
    what they picked. Use `toBeHidden` to wait for its exit animation to finish (200ms), no waitForTimeout.
  */
  await expect(app.getByTestId("chat-list")).toBeHidden();
  // The input box is visible = the drawer really got out of the way, not just opacity 0 while still blocking clicks.
  await expect(app.getByTestId("chat-input")).toBeVisible();
});

/**
 * Tapping a past conversation in the drawer switches to it.
 *
 * Separate from the previous test: that one checks "it opens and can create", this one checks "it can switch". Switching is the whole reason
 * the conversation list exists, and it takes one more step (two conversations must exist first).
 */
test("窄屏：抽屉里能切到另一条会话", async ({ app }) => {
  await gotoShell(app, "/w/growth/chat");

  // The growth workspace in the seed has a conversation (the Feishu one); click it to switch.
  await app.getByTestId("chat-sessions-open").click();
  const list = app.getByTestId("chat-list");
  await expect(list).toBeVisible();

  const rows = list.locator('[data-testid^="chat-row-"]');
  /*
    First assert the list has **at least one**. If the seed data ever stops including conversations, `.first()` below would
    time out on "cannot click", an error that does not point at the real cause, "the list is empty".
    A toHaveCount comparison in `expect.poll` style does not work (the count changes); use a numeric > 0 comparison.
  */
  expect(await rows.count()).toBeGreaterThan(0);

  await rows.first().click();

  // The URL has the runId shape = it really switched.
  await expect(app).toHaveURL(/\/w\/growth\/chat\/run_/);
  // The drawer closes itself to make room for the conversation.
  await expect(list).toBeHidden();
});

/**
 * "Status + priority" in the issue form must stack vertically on narrow screens.
 *
 * It used to be `grid grid-cols-2` with no responsive prefix: at 402px each of the two columns had ~153px left,
 * so values like "In progress" or "Priority P2" could not even fit their labels.
 *
 * The check is **the two cells have different y** (stacked), not measuring their widths: a width threshold needs a
 * magic number, while the y coordinate is direct evidence of "one row or two".
 */
test("窄屏：问题表单的状态与优先级竖排，不挤在一行", async ({ app }) => {
  await gotoShell(app, "/w/growth/issues");
  await app.getByTestId("new-issue").click();

  const status = app.getByTestId("issue-status-select");
  const priority = app.getByTestId("issue-priority-select");
  await expect(status).toBeVisible();
  await expect(priority).toBeVisible();

  const a = await status.boundingBox();
  const b = await priority.boundingBox();

  /*
    Stacked: priority sits entirely below status. Writing `>=` together with "top edge strictly greater"
    lets the failure message tell "side by side" (same y) from "overlapping" (y slightly different but not wrapped).
  */
  expect(b!.y).toBeGreaterThan(a!.y);
  expect(b!.y).toBeGreaterThanOrEqual(a!.y + a!.height);

  /*
    ⚠️ Also assert **it was not squeezed narrow**, otherwise an implementation like "grid-cols-2 changed to flex-col but each cell still half width"
    would pass the two lines above. In a single column each cell should take the full available width. The dialog content area on a 402px
    screen is about 330px; take 240 as the lower bound: well above half width (~153px), yet not so high that
    small tweaks to the dialog padding count as a regression.
  */
  expect(a!.width).toBeGreaterThan(240);
  expect(b!.width).toBeGreaterThan(240);
});

/**
 * The same goes for the pair of inline selectors on the issue detail page.
 *
 * It has an extra `max-w-md`, so "not stretching full width on desktop" must not be broken by this change.
 * But that cap (448px) has no effect at all on a 402px screen; the real squeeze comes from grid-cols-2 itself.
 */
test("窄屏：问题详情页的就地选择器竖排", async ({ app }) => {
  await gotoShell(app, "/w/growth/issues");

  /*
    ⚠️ **The seed deliberately has no issues** (the 17-issues test asserts the "no issues yet" empty state),
    so an issue must be created here first before opening the detail. Clicking the first item in `issue-list` directly would time out on
    "cannot click", an error that does not point at the real cause, "the list is empty to begin with".
  */
  await app.getByTestId("new-issue").click();
  await app.getByTestId("issue-title").fill("窄屏下就地选择器挤在一行");
  await app.getByTestId("modal-submit").click();
  await app.getByTestId("issue-list").getByText("窄屏下就地选择器挤在一行").click();

  const status = app.getByTestId("issue-status-inline");
  const priority = app.getByTestId("issue-priority-inline");
  await expect(status).toBeVisible();
  await expect(priority).toBeVisible();

  const a = await status.boundingBox();
  const b = await priority.boundingBox();

  expect(b!.y).toBeGreaterThan(a!.y);
  expect(b!.y).toBeGreaterThanOrEqual(a!.y + a!.height);
  // Page content area is 402 - px-4*2 = 370px; in a single column each cell should be close to it. 240 as above, leaving plenty of margin.
  expect(a!.width).toBeGreaterThan(240);
});

/**
 * Detail table of the usage page: **content is not clipped, and wide content has horizontal scrolling as a fallback**.
 *
 * # This test overturned a judgment that looked very reasonable
 *
 * The original suspicion: the table has `w-full`, which would **cancel out** the outer `overflow-x-auto` (the table shrinks to
 * the container width → `scrollWidth === clientWidth` → horizontal scroll never kicks in → 6 columns squeezed into 338px get crushed),
 * so it should become `min-w-[36rem]` to make horizontal scrolling really work.
 *
 * **Measurements showed that is false, and that change would be wrong.** The measured facts:
 *
 *   - With the seed data 338px is enough: **scanning each cell for `scrollWidth > clientWidth` finds 0**,
 *     so no content is clipped;
 *   - Replacing an agent name with a long one (20 Chinese characters), the table **grows by itself to 487px**,
 *     and the outer `overflow-x-auto` really starts scrolling horizontally.
 *
 * The root cause is that `table-layout` defaults to `auto`: `w-full` is a **lower bound** for it (at least fill the container),
 * not an upper bound. When content needs more width the table gets wider anyway, so a `min-w-*` layer is redundant,
 * and adding it would force **data that fits just right today** into horizontal scrolling, breaking something that works.
 *
 * So this test does not assert "there is something to scroll horizontally right now" (that would force the wrong change above),
 * but the two things that really must hold: **no content is clipped** + **the page as a whole does not slide sideways**.
 */
test("窄屏：用量页表格内容不被裁，且页面整体不横向滑", async ({ app }) => {
  await gotoShell(app, "/w/growth/usage");

  const table = app.getByTestId("usage-table");
  await expect(table).toBeVisible();

  const m = await app.evaluate(() => {
    const t = document.querySelector('[data-testid="usage-table"]') as HTMLElement;
    const wrap = t.parentElement!;
    const main = document.querySelector('[data-testid="app-scroll"]')!;

    /*
      Measure each cell for "content wider than the cell": that is direct evidence of "crushed".
      The total table width cannot show it: the table can be exactly the container width while the text of some cell
      already had its tail cut off by `truncate`.
    */
    const clipped: string[] = [];
    // Array.from instead of for…of: under this tsconfig's target/lib, NodeListOf has no
    // [Symbol.iterator], so iterating directly **does not compile** (TS2488).
    for (const el of Array.from(t.querySelectorAll("th, td, span"))) {
      const e = el as HTMLElement;
      if (e.clientWidth > 0 && e.scrollWidth > e.clientWidth + 1) {
        clipped.push(`${(e.textContent ?? "").trim().slice(0, 20)} (${e.scrollWidth}>${e.clientWidth})`);
      }
    }

    return {
      clipped,
      rowCount: t.querySelectorAll("tbody tr").length,
      wrapOverflowX: getComputedStyle(wrap).overflowX,
      mainScrollWidth: main.scrollWidth,
      mainClientWidth: main.clientWidth,
    };
  });

  /*
    First pin "the table really has rows". Without this, if the seed data ever stops including usage,
    "zero clipped" below would pass easily on an empty table: another false green that verifies nothing.
  */
  expect(m.rowCount).toBeGreaterThan(0);

  // No cell has clipped content.
  expect(m.clipped).toEqual([]);

  /*
    The **ability** to scroll horizontally must be there (`overflow-x: auto`): it is the only place wide content (long agent names, measured to push
    the table to 487px) can go. Changing it to hidden would cut such data off directly,
    while everything is fine with the seed data, so this can only be guarded by asserting the style itself.
  */
  expect(m.wrapOverflowX).toBe("auto");

  /*
    And **the page as a whole still must not slide sideways**. The table's overflow must stop at its own `overflow-x-auto`
    container and not bubble up to the content area. Bubbling up means "the whole page can be dragged sideways", exactly what the foundation tests guard against.
  */
  expect(m.mainScrollWidth).toBeLessThanOrEqual(m.mainClientWidth);
});

/**
 * In the narrow-screen drawer, a conversation row's **status badge** must not break the row.
 *
 * A conversation row is now "title + status chip" on one line and "agent · time + source chip" on another. The drawer is only
 * `w-72 max-w-[85vw]` (about 288px on a 402px screen), so a long title squeezes the chip on the right.
 *
 * The check only trusts **measured rects**, not arithmetic: estimating the chip width from vendor CSS overestimated it by nearly double
 * (missed the size modifier class, and flex containers shrink children; see docs/guides/frontend.md).
 * Three things are measured here:
 *
 *   1. The chip's right edge does not go past the list container's right edge: direct evidence of overflow;
 *   2. The chip has its own width (not squeezed to 0): the guard for the `shrink-0` line. Without it,
 *      a long title squeezes the chip into a sliver, and "the element is still there and still visible", it just cannot be read;
 *   3. Title and chip are on **the same line** (y overlap): direct evidence of broken wrapping.
 */
test("窄屏：会话行的状态标记不溢出、不被压扁、不换行", async ({ app }) => {
  await gotoShell(app, "/w/growth/chat");

  await app.getByTestId("chat-sessions-open").click();
  const list = app.getByTestId("chat-list");
  await expect(list).toBeVisible();

  /*
    ⚠️ **Wait for the drawer to finish sliding before measuring.**

    `toBeVisible()` holds on the **first frame** of the slide-in animation (element mounted, opacity non-zero),
    when the drawer has only slid out a little. The `chat-list` width measured then was **37px**, so every row's
    chip "goes past the container's right edge", the error says "status chip overflowed", and the real cause is
    measuring too early. This kind of false red costs more time than a false green: it points at something that is not broken and calls it broken.

    The check is **reading the same width twice in a row** (the animation stopped), not sleeping a magic number of milliseconds.
  */
  let settled = 0;
  await expect
    .poll(
      async () => {
        const w = (await list.boundingBox())?.width ?? 0;
        const same = w > 0 && w === settled;
        settled = w;
        return same;
      },
      { message: "抽屉宽度一直在变，动画没停下来" },
    )
    .toBe(true);

  const listBox = (await list.boundingBox())!;

  const rows = list.locator('[data-testid^="chat-row-"]');
  const count = await rows.count();
  // Without conversations the whole loop below would quietly run zero times, and this test would pass green.
  expect(count).toBeGreaterThan(0);

  /*
    **Every `chat-row-` must really be a row** (with one status chip).

    This pins a pitfall that was really hit: the source badge used to be called `chat-row-trigger-feishu`, which also starts
    with `chat-row-`, so this prefix selector counted the chip as a row too: 7 conversations were counted as 11
    "rows". The test above, "can switch to another conversation in the drawer", took `.first()` with the same prefix,
    and when it hit a chip it clicked something that does not navigate, with an error saying "cannot click".
    The source badge is now `row-trigger-*`, and this assertion makes sure it is not changed back.
  */
  for (let i = 0; i < count; i++) {
    const id = await rows.nth(i).getAttribute("data-testid");
    await expect(
      rows.nth(i).getByTestId("run-status"),
      `${id} 匹配到了 chat-row- 前缀却不是一整行`,
    ).toHaveCount(1);
  }

  for (let i = 0; i < count; i++) {
    const row = rows.nth(i);
    const chip = row.getByTestId("run-status");
    await expect(chip).toBeVisible();

    const chipBox = (await chip.boundingBox())!;
    // The title is the first span in the row (the `min-w-0 flex-1 truncate` one).
    const titleBox = (await row.locator("span").first().boundingBox())!;

    // 1. No overflow: leave 0.5px for sub-pixel rounding (same as the 402.5 in the foundation tests).
    expect(
      chipBox.x + chipBox.width,
      `第 ${i} 行的状态 chip 右边缘超出了列表`,
    ).toBeLessThanOrEqual(listBox.x + listBox.width + 0.5);

    // 2. Not squeezed: a Chinese label is never narrower than 16px.
    //
    // ⚠️ **This check cannot currently detect `shrink-0`**; do not think it guards that class. Mutation testing showed
    // that removing the chip's `shrink-0` leaves this test all green, because the title has `truncate`,
    // so under flex shrinking the title gives way first and the chip never has to. Its real role is a **backstop**: only if the title's
    // truncate is removed some day, with both classes gone at once, would the chip get squeezed. Keeping it costs nothing,
    // but do not write an assertion claiming "I tested it"; that would be a false green (same as the sequence backstop case).
    expect(chipBox.width, `第 ${i} 行的状态 chip 被压扁了`).toBeGreaterThan(16);

    /*
      3. **Same line as the title**: the chip's top edge must not be lower than the title's top edge.

      ⚠️ This check was corrected once. It used to compare "chip center < row midline", which **could not detect
      anything**: once the title stops truncating it wraps into several lines, the row itself grows taller (measured 54→94px),
      the midline moves down with it, so the chip drops yet the assertion still holds. Mutation testing (removing
      `min-w-0 truncate`) was all green back then; this test was fake.

      Comparing with **the title's top edge** catches it: after the title wraps, the chip is pushed to the second line (y 196→216),
      while the title's top edge stays put. It checks the relative position of the two, unaffected by row height changes.

      Likewise, checking only "the chip's right edge stays inside the container" is not enough: the real failure is not horizontal overflow (flex pushes the title
      back, or wraps it), but **being pushed down vertically**.
    */
    expect(
      chipBox.y,
      `第 ${i} 行的状态 chip 被标题挤到下一行了（标题没 truncate？）`,
    ).toBeLessThanOrEqual(titleBox.y + 0.5);
  }
});

/**
 * The role permission table must be **readable** at 402px.
 *
 * ⚠️ **This test exists because the first version was rejected by a real-device screenshot.**
 *
 * The first version was just one table + `overflow-x-auto`; measurements said "the page does not overflow, the container does not overflow",
 * all numbers passed. **But those numbers were fake**: the real reason nothing overflowed was that the feature name column was squeezed to 52px,
 * so "View workspace content" stacked vertically one character per line: an unreadable table.
 *
 * So this asserts **not "no horizontal overflow"** (that is exactly the one that was falsely green),
 * but "narrow screens use a different layout, and the feature name line is wide enough".
 */
test("窄屏：角色权限说明换成小卡布局，功能名不被压成竖排", async ({ app }) => {
  await gotoShell(app, "/w/growth/settings?tab=members");

  // Pin the viewport width: with a misconfigured project this test would run on a 1280 desktop viewport, where the table layout is
  // roomy anyway and all the assertions below would say nothing (same as the other tests in this file).
  expect(await app.evaluate(() => window.innerWidth)).toBe(402);

  await expect(app.getByTestId("member-row").first()).toBeVisible();
  await app.getByTestId("role-permissions-toggle").click();
  await expect(app.getByTestId("role-permissions-body")).toBeVisible();

  // Narrow screens use small cards: the cards are visible, and the 5-column table is **not visible**.
  //
  // Use toBeHidden() instead of toHaveCount(0): the table is in the DOM (`hidden sm:table`),
  // so the count is always 1, and asserting on count would go red in the wrong place.
  await expect(app.getByTestId("role-permissions-cards")).toBeVisible();
  await expect(app.getByTestId("role-permissions-table")).toBeHidden();

  /*
    ⚠️ **To judge "squeezed", measure how wide that line actually is**, not whether the whole block overflows.

    In the first version's table, the feature name cell was only 52px, and "View workspace content" (six characters) stacked vertically.
    Here it must take at least half the viewport: half of 402 is 201, and a line of Chinese must at least fit
    "View workspace content" (about 6 characters × 12px ≈ 72px) before normal wrapping makes sense. Half leaves plenty of margin
    while reliably catching the "squeezed to 52px" case.
  */
  const label = app.getByTestId("role-capability-card-view-content");
  const box = await label.boundingBox();
  expect(box).not.toBeNull();
  expect(box!.width).toBeGreaterThan(200);

  // The three notes must also be there on narrow screens (the text under the table has nothing to do with layout; both layouts share it).
  await expect(app.getByTestId("role-note-owner")).toContainText("完全相同");
});

/**
 * Layout contract of terminal fullscreen at 402px.
 *
 * # Why "fullscreen" still matters on narrow screens
 *
 * On a phone the terminal is cramped already, and besides the terminal this page has a title bar, container status, and a view toggle. Filling the viewport is
 * where narrow screens **gain the most**, not a desktop-only nicety. So the feature has no breakpoint split,
 * but two things must hold: the two buttons in the top right must not be pushed out, and the fullscreen layer must not overflow horizontally.
 *
 * # What it does not guard
 *
 * Safe areas. The fullscreen shell uses `--safe-top/bottom/left/right`, but those four values are
 * **always 0** in Chromium (same as this file's header). "Half the button eaten by the Home Indicator" can only be checked on a real device.
 */
test("终端全屏：窄屏不横向溢出，两个入口都点得到", async ({ app }) => {
  await app.goto("/w/growth/runs/run_1");
  await app.evaluate(() => window.__amaApi?.setSandboxStatusMock?.("running"));
  await app.goto("/w/growth/runs/run_1?view=terminal");

  // Pin the viewport width first: with a misconfigured project the test quietly runs at 1280, where of course nothing is cramped (same as the other tests in this file).
  expect(await app.evaluate(() => window.innerWidth)).toBe(402);

  await expect(app.getByTestId("sandbox-terminal-host")).toBeVisible();

  // On narrow screens both entry points must be **clickable**, not just in the DOM.
  const newtab = app.getByTestId("sandbox-terminal-newtab");
  const toggle = app.getByTestId("sandbox-terminal-fullscreen-toggle");
  await expect(newtab).toBeVisible();
  await expect(toggle).toBeVisible();

  /*
    ⚠️ **The buttons must sit entirely inside the viewport**; visible alone is not enough: a button whose right edge goes past 402
    is still reported visible by Playwright, while a finger cannot tap its right half. So measure x + width.
  */
  for (const [name, loc] of [
    ["新标签页", newtab],
    ["全屏开关", toggle],
  ] as const) {
    const b = await loc.boundingBox();
    expect(b, `${name} 量不到盒子`).not.toBeNull();
    expect(b!.x, `${name} 被挤出了左边缘`).toBeGreaterThanOrEqual(0);
    expect(b!.x + b!.width, `${name} 被挤出了右边缘（横向溢出）`).toBeLessThanOrEqual(402);
  }

  await toggle.click();
  const shell = app.getByTestId("sandbox-terminal-fullscreen");
  await expect(shell).toBeVisible();

  /*
    The fullscreen layer must not overflow horizontally.

    ⚠️ **Check both documentElement and the fullscreen shell itself**: checking only the former is a false green. The shell is
    `fixed inset-0`, and overflow inside it does not necessarily reach documentElement
    (this repo's lesson "horizontal overflow stops at the scroll container").
  */
  const m = await app.evaluate(() => {
    const el = document.querySelector('[data-testid="sandbox-terminal-fullscreen"]')!;
    return {
      docScrollWidth: document.documentElement.scrollWidth,
      docClientWidth: document.documentElement.clientWidth,
      shellScrollWidth: el.scrollWidth,
      shellClientWidth: el.clientWidth,
    };
  });
  expect(m.docScrollWidth).toBeLessThanOrEqual(m.docClientWidth);
  expect(m.shellScrollWidth).toBeLessThanOrEqual(m.shellClientWidth + 1);

  // After going fullscreen the exit entry is still visible and clickable; especially important on narrow screens (no keyboard shortcut to exit).
  await expect(toggle).toBeVisible();
  await toggle.click();
  await expect(shell).toHaveCount(0);
});
