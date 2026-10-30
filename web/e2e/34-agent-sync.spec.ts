/**
 * Frontend behavior of the "after saving an Agent, sync it to Ark asynchronously" path.
 *
 * # Only two things are checked here, because the requirement has only two
 *
 * 1. **Success is silent**: after a successful save, nothing sync-related should appear in the UI;
 * 2. **Only failure shows a message, and it is actionable**: it says which step failed, and offers "retry".
 *
 * ⚠️ The `__amaApi.failAgentSync()` in the cases is a mock switch (see data/api.ts).
 * Failures on the real backend are covered by server/core/agentsync_test.go; each side handles half:
 * this one covers **how the UI presents it**, that one covers **whether the sync logic is right**.
 */

import { expect, test } from "./fixtures";

test("保存成功时界面上没有任何同步提示（成功静默）", async ({ app }) => {
  await app.goto("/w/growth/agents/agent_triage");

  await app.getByTestId("cfg-system").fill("你是分诊助手。改一句话触发一次同步。");
  await app.getByTestId("agent-save").click();

  // The save itself must succeed.
  await expect(app.getByTestId("agent-save")).toBeDisabled();

  /*
   * ⚠️⚠️ **To check "does not appear", do not use `expect(...).toHaveCount(0)`: it retries automatically.**
   *
   * This is the most valuable sentence in this case, found only after falling into it twice:
   *
   *   1. first written as `waitForTimeout(2500)` then `toHaveCount(0)`;
   *   2. then changed to `toHaveCount(0)` repeated in a loop.
   *
   * **Neither version catches the bug.** After changing the render condition to
   * `status !== "succeeded"` (the message also pops up while syncing, directly violating "success is
   * silent"), both versions stayed all green.
   *
   * The real cause is that `expect` has a built-in 10-second retry window (see
   * `expect: { timeout: 10_000 }` in playwright.preview.config.ts): `toHaveCount(0)` **waits until the
   * element disappears** before passing. The sync settles to succeeded at ~2.9 seconds and the
   * message disappears on its own, so the assertion always gets the result it wants: what it checks is
   * not "never appeared" but "disappears sooner or later". Measured with a probe: the message was
   * present from 8ms to 2.8 seconds, and the count stayed at 1 the whole time.
   *
   * The fix is an **instant reading with no retry**: `locator.count()` returns the current number in
   * the DOM right away, and we compare it with 0. Anything popping up at any moment gets caught on the spot.
   */
  const deadline = Date.now() + 3200;
  let samples = 0;
  while (Date.now() < deadline) {
    // ⚠️ Use the instant reading of count(), not expect(...).toHaveCount(0) (which retries).
    const errCount = await app.getByTestId("agent-sync-error").count();
    const retryCount = await app.getByTestId("agent-sync-retry").count();
    expect(
      errCount,
      `同步成功是静默的，但第 ${samples} 次采样时冒出了同步提示——` +
        `用户明确要求成功不打扰`,
    ).toBe(0);
    expect(retryCount, "成功时不该出现重试按钮").toBe(0);
    samples++;
    await app.waitForTimeout(150);
  }
  // The number of samples itself must be enough: if the loop somehow never ran, the assertion above
  // was never executed.
  expect(samples, "采样次数太少，这条用例可能什么都没验到").toBeGreaterThan(10);
});

test("同步失败时给出可行动的提示与重试", async ({ app }) => {
  await app.goto("/w/growth/agents/agent_triage");

  // Make the sync of this save fail (the credentials level, which the user can fix themselves).
  await app.evaluate(() => {
    (
      window as unknown as {
        __amaApi: { failAgentSync: (reason: string, stage?: string) => void };
      }
    ).__amaApi.failAgentSync("方舟拒绝了这把 API Key，请到空间设置里检查凭证是否有效", "credential");
  });

  await app.getByTestId("cfg-system").fill("你是分诊助手。这一次同步会失败。");
  await app.getByTestId("agent-save").click();

  const err = app.getByTestId("agent-sync-error");
  await expect(err).toBeVisible();
  // Assert **the specific reason**, not just "an error appeared": checking only existence, an
  // implementation that always shows "sync failed" would still pass, and that is exactly what "not
  // actionable" looks like.
  await expect(err).toContainText("方舟拒绝了这把 API Key");
  // Which step it is stuck at must be readable (the frontend gives different guidance based on it).
  await expect(err).toHaveAttribute("data-sync-stage", "credential");

  // The retry button must be there, and the error disappears after clicking it (retry succeeds in the mock).
  const retry = app.getByTestId("agent-sync-retry");
  await expect(retry).toBeVisible();
  await retry.click();
  await expect(app.getByTestId("agent-sync-error")).toHaveCount(0);
});

test("保存失败与同步失败是两条不同的提示", async ({ app }) => {
  /*
   * These two must be kept apart: a save failure means **the changes were not stored**, a sync failure
   * means **the changes were stored but the Ark side is still old**. Merging them into one message
   * leaves the user not knowing whether to fill in the form again.
   *
   * This only checks "when sync fails, the successful save path is not affected": the save button goes
   * back to disabled (no unsaved changes), which shows the changes really were stored.
   */
  await app.goto("/w/growth/agents/agent_triage");
  await app.evaluate(() => {
    (
      window as unknown as {
        __amaApi: { failAgentSync: (reason: string, stage?: string) => void };
      }
    ).__amaApi.failAgentSync("方舟服务暂时不可用", "environment");
  });

  await app.getByTestId("cfg-system").fill("你是分诊助手。保存成功但同步失败。");
  await app.getByTestId("agent-save").click();

  await expect(app.getByTestId("agent-sync-error")).toBeVisible();
  // The save itself succeeded: the button is disabled (no unsaved changes), and there is no save error.
  await expect(app.getByTestId("agent-save")).toBeDisabled();
  await expect(app.getByTestId("agent-save-error")).toHaveCount(0);
});
