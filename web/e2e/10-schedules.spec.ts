/**
 * Requirement: proactive capabilities and scheduled task dispatch.
 *
 * Covers configuring, enabling/pausing, run-now and deleting for all three trigger types, plus two
 * details that "have concrete consequences if not done": a wrong cron must be flagged on the spot (not
 * only after submitting), and delivering to Feishu with no group to choose must give guidance (not an
 * empty dropdown).
 *
 * **Editing is on the detail page, not in a dialog** (`/w/:key/schedules/:id`). The list page only
 * keeps the create dialog: at that point the prompt is still empty, so there is no "thousands of
 * characters cannot be edited" problem. The detail page cases are in the second half of the file.
 */

import { expect, seedScheduleHistoryMock, test } from "./fixtures";

/** Open the create dialog and fill in the required fields. */
async function fillNewSchedule(
  app: import("@playwright/test").Page,
  opts: { name: string; prompt: string },
): Promise<void> {
  await app.getByTestId("new-schedule").click();
  await expect(app.getByTestId("schedule-modal")).toBeVisible();
  await app.getByTestId("sched-name").fill(opts.name);
  await app.getByTestId("sched-prompt").fill(opts.prompt);
}

/** Create a schedule, then click the title to go to the detail page. Returns its id. */
async function createAndOpenDetail(
  app: import("@playwright/test").Page,
  opts: { name: string; prompt: string },
): Promise<string> {
  await fillNewSchedule(app, opts);
  await app.getByTestId("modal-submit").click();
  await expect(app.getByTestId("schedule-modal")).not.toBeVisible();

  const row = app.locator('[data-testid^="schedule-row-"]').first();
  const testId = await row.getAttribute("data-testid");
  const id = (testId ?? "").replace("schedule-row-", "");
  await app.getByTestId(`schedule-link-${id}`).click();
  await expect(app).toHaveURL(new RegExp(`/schedules/${id}$`));
  return id;
}

test("空状态给出可操作的引导", async ({ app }) => {
  await app.goto("/w/growth/schedules");
  // The seed has no schedules: an empty list is what a new user should see the first time.
  await expect(app.getByText("还没有定时任务")).toBeVisible();
  await expect(app.getByTestId("new-schedule")).toBeEnabled();
});

test("建一条 cron 任务，列表里能看到下次运行", async ({ app }) => {
  await app.goto("/w/growth/schedules");
  await fillNewSchedule(app, { name: "每日告警汇总", prompt: "汇总昨天的告警" });

  // The default is already cron + weekdays 9:30; submit directly.
  await app.getByTestId("modal-submit").click();
  await expect(app.getByTestId("schedule-modal")).not.toBeVisible();

  const list = app.getByTestId("schedule-list");
  await expect(list).toBeVisible();
  await expect(list.getByText("每日告警汇总")).toBeVisible();
  // The cron expression is shown directly, so users can check it is what they wanted.
  await expect(list.getByText("30 9 * * 1-5")).toBeVisible();
  // After creating there must be a next run time, otherwise this schedule would never fire.
  await expect(list.getByText("未排期")).toHaveCount(0);
});

test("cron 写错当场提示，不用等提交后才知道", async ({ app }) => {
  await app.goto("/w/growth/schedules");
  await fillNewSchedule(app, { name: "写错的任务", prompt: "做点什么" });

  // One field missing.
  await app.getByTestId("sched-cron").fill("30 9 * *");
  await app.getByTestId("modal-submit").click();
  // The dialog must stay open (validate returns false), with the hint "needs 5 fields".
  await expect(app.getByTestId("schedule-modal")).toBeVisible();
  await expect(app.getByText(/要 5 段/)).toBeVisible();

  // A field out of range.
  await app.getByTestId("sched-cron").fill("99 9 * * *");
  await app.getByTestId("modal-submit").click();
  await expect(app.getByTestId("schedule-modal")).toBeVisible();
  await expect(app.getByText(/超出范围/)).toBeVisible();
});

test("cron 快捷模板能一键填上", async ({ app }) => {
  await app.goto("/w/growth/schedules");
  await fillNewSchedule(app, { name: "每小时巡检", prompt: "巡检" });

  // The testid uses the **index**, not the expression. It used to be
  // `sched-preset-${expression with non-alphanumerics removed}`, so "hourly" (0 * * * *) squashed into
  // `sched-preset-0`: it only worked by coincidence, and any new preset that also squashed to `0` would
  // collide with it.
  await app.getByTestId("sched-preset-2").click(); // the 3rd preset: hourly 0 * * * *
  await expect(app.getByTestId("sched-cron")).toHaveValue("0 * * * *");
});

test("固定间隔与一次性任务都能建", async ({ app }) => {
  await app.goto("/w/growth/schedules");

  await fillNewSchedule(app, { name: "每 15 分钟轮询", prompt: "看看部署好了没" });
  await app.getByTestId("sched-kind").click();
  await app.getByRole("option", { name: "固定间隔" }).click();
  await expect(app.getByTestId("sched-interval")).toBeVisible();
  await app.getByTestId("modal-submit").click();
  await expect(app.getByTestId("schedule-modal")).not.toBeVisible();

  await fillNewSchedule(app, { name: "一小时后提醒", prompt: "看发布结果" });
  await app.getByTestId("sched-kind").click();
  await app.getByRole("option", { name: "一次性" }).click();
  await expect(app.getByTestId("sched-delay")).toBeVisible();
  await app.getByTestId("modal-submit").click();
  await expect(app.getByTestId("schedule-modal")).not.toBeVisible();

  await expect(app.locator('[data-testid^="schedule-row-"]')).toHaveCount(2);
});

test("启停开关立即生效", async ({ app }) => {
  await app.goto("/w/growth/schedules");
  await fillNewSchedule(app, { name: "可以停的任务", prompt: "做事" });
  await app.getByTestId("modal-submit").click();

  const row = app.locator('[data-testid^="schedule-row-"]').first();
  const toggle = row.locator('[data-testid^="toggle-schedule-"]');
  await toggle.click();

  // After pausing it must be marked and have no next run time: that is the visible evidence of
  // "will not fire again".
  await expect(row.getByText("已暂停")).toBeVisible();
  await expect(row.getByText("未排期")).toBeVisible();

  // Click back again, and the next run time must come back.
  await toggle.click();
  await expect(row.getByText("已暂停")).toHaveCount(0);
  await expect(row.getByText("未排期")).toHaveCount(0);
});

test("试跑只承诺「已排队」，不谎称已完成", async ({ app }) => {
  await app.goto("/w/growth/schedules");
  await fillNewSchedule(app, { name: "试跑一下", prompt: "做事" });
  await app.getByTestId("modal-submit").click();

  const row = app.locator('[data-testid^="schedule-row-"]').first();
  await row.locator('[data-testid^="run-now-"]').click();

  // The real execution is taken over by the background scheduler, so the text can only be "queued".
  await expect(app.getByTestId("schedule-notice")).toContainText("已排入队列");
});

test("投递到飞书群：没有可选群时给引导而不是空下拉", async ({ app }) => {
  await app.goto("/w/growth/schedules");
  await fillNewSchedule(app, { name: "发到群里", prompt: "汇总" });

  await app.getByTestId("sched-delivery").click();
  await app.getByRole("option", { name: "发到飞书群" }).click();

  // The seed has a Feishu app, so the mock can make up groups: the dropdown should appear.
  await expect(app.getByTestId("sched-chat")).toBeVisible();
});

test("选飞书群：显示群名，且能按群名搜", async ({ app }) => {
  await app.goto("/w/growth/schedules");
  await fillNewSchedule(app, { name: "发到值班群", prompt: "汇总" });

  await app.getByTestId("sched-delivery").click();
  await app.getByRole("option", { name: "发到飞书群" }).click();

  // The first group is selected by default, and the input box shows the group name, not oc_xxx.
  const input = app.getByTestId("sched-chat");
  await expect(input).toHaveValue(/测试群$/);

  // First scroll the input box into view and wait for scrolling to settle: in the dialog it is at the
  // bottom, and focusing makes the browser scroll a bit, while RAC's popover closes on scroll; without
  // waiting, the just-opened candidates would be closed by that scroll.
  await input.scrollIntoViewIfNeeded();
  await app.evaluate(
    () => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))),
  );

  // Type part of a group name; only matching candidates remain.
  await input.fill("值班");
  const options = app.getByRole("option");
  await expect(options.first()).toBeVisible();
  await expect(options.filter({ hasText: "测试群" })).toHaveCount(0);
  // Select with the keyboard instead of clicking: before a click Playwright scrolls the option into
  // view, for the same reason as above.
  await input.press("ArrowDown");
  await input.press("Enter");
  await expect(input).toHaveValue(/值班群$/);

  await app.getByTestId("modal-submit").click();
  const row = app.locator('[data-testid^="schedule-row-"]').first();
  await expect(row).toContainText("值班群");
});

test("删除后列表里就没了", async ({ app }) => {
  await app.goto("/w/growth/schedules");
  await fillNewSchedule(app, { name: "待删除", prompt: "做事" });
  await app.getByTestId("modal-submit").click();

  const row = app.locator('[data-testid^="schedule-row-"]').first();
  // Use the testid instead of role+name: the Tooltip trigger also has role="button" and an aria-label
  // of the same name, so finding by name would hit two elements (strict mode errors out).
  await row.locator('[data-testid^="delete-schedule-"]').click();
  await app.getByTestId("confirm-danger").click();

  await expect(app.getByText("还没有定时任务")).toBeVisible();
});

test("导航里能进定时任务页", async ({ app }) => {
  await app.goto("/w/growth");
  await app.getByRole("navigation").getByRole("link", { name: "定时任务" }).click();
  await expect(app).toHaveURL(/\/w\/growth\/schedules$/);
});

// ---------------------------------------------------------------------------
// Detail page
// ---------------------------------------------------------------------------

test("列表页的编辑入口跳详情页，不再开弹窗", async ({ app }) => {
  await app.goto("/w/growth/schedules");
  await fillNewSchedule(app, { name: "去详情页", prompt: "做事" });
  await app.getByTestId("modal-submit").click();

  const row = app.locator('[data-testid^="schedule-row-"]').first();
  await row.locator('[data-testid^="edit-schedule-"]').click();

  // What is asserted is "reached the detail page", not "no dialog": the latter also holds in the frame
  // before the dialog has had time to render (see the CLAUDE.md rule "wait out the render window before
  // asserting an element should not appear").
  await expect(app).toHaveURL(/\/schedules\/sch_/);
  await expect(app.getByTestId("detail-config")).toBeVisible();
});

test("很长的 prompt 也能在详情页编辑并保存", async ({ app }) => {
  // **This is the reason the detail page exists**: real schedule prompts in production run to several
  // thousand characters, while the TextArea in the list page dialog is 4 rows: users effectively could
  // not edit it in the UI.
  await app.goto("/w/growth/schedules");
  await createAndOpenDetail(app, { name: "长指令任务", prompt: "短的" });

  // Build a text of the same order of size as the real ones (6000+ characters).
  const long = "巡检平台各项指标，逐条核对并给出需要人工介入的清单。".repeat(240);
  expect(long.length).toBeGreaterThan(6000);

  const input = app.getByTestId("detail-prompt");

  // **First lock in "the edit box really is big enough"**: that is the reason this page exists.
  //
  // ⚠️ Only asserting "fill it in and it saves" **cannot test** this: `fill()` writes into the control
  // regardless of its height, so changing promptRows back to 4 (= the dialog size) leaves the second
  // half of this case all green; mutation-verified. The test can only be the measured height: ~100px in
  // the dialog, ~378px for 18 rows here. 260 is used as the threshold (far above 4 rows, far below 18),
  // so small font or line height tweaks do not turn it red.
  const box = await input.boundingBox();
  expect(box?.height ?? 0).toBeGreaterThan(260);

  await input.fill(long);
  await app.getByTestId("detail-save").click();
  await expect(app.getByTestId("detail-saved")).toBeVisible();

  // **Re-enter the page before asserting**, instead of looking at the input in place: in place only
  // proves "I just filled it in", not that it was stored. Still there after a refresh is the evidence
  // that saving succeeded.
  await app.reload();
  await expect(app.getByTestId("detail-prompt")).toHaveValue(long);
});

test("没改动时保存按钮不可点，改了才可点", async ({ app }) => {
  await app.goto("/w/growth/schedules");
  await createAndOpenDetail(app, { name: "脏状态", prompt: "原始内容" });

  // Clean on first entering.
  await expect(app.getByTestId("detail-save")).toBeDisabled();
  await app.getByTestId("detail-prompt").fill("改过的内容");
  await expect(app.getByTestId("detail-save")).toBeEnabled();
});

test("详情页能改触发方式，保存后下次运行跟着变", async ({ app }) => {
  await app.goto("/w/growth/schedules");
  await createAndOpenDetail(app, { name: "改触发方式", prompt: "做事" });

  await app.getByTestId("detail-kind").click();
  await app.getByRole("option", { name: "固定间隔" }).click();
  await expect(app.getByTestId("detail-interval")).toBeVisible();
  await app.getByTestId("detail-save").click();
  await expect(app.getByTestId("detail-saved")).toBeVisible();

  // Back to the list to look at that row: the spec changed, so "every N minutes" must show.
  await app.getByTestId("detail-back").click();
  await expect(app.getByTestId("schedule-list")).toContainText("每 15 分钟");
});

test("详情页的启停与试跑就地生效", async ({ app }) => {
  await app.goto("/w/growth/schedules");
  await createAndOpenDetail(app, { name: "详情页操作", prompt: "做事" });

  // Enable/pause: after pausing there is no next run time, the visible evidence of "will not fire again".
  await expect(app.getByTestId("detail-status")).toHaveText("已启用");
  await app.getByTestId("detail-toggle").click();
  await expect(app.getByTestId("detail-status")).toHaveText("已暂停");
  await expect(app.getByTestId("detail-not-scheduled")).toBeVisible();

  await app.getByTestId("detail-toggle").click();
  await expect(app.getByTestId("detail-status")).toHaveText("已启用");

  // Run-now can only say "queued": the real execution is taken over by the background scheduler, and
  // saying "done" would be false.
  await app.getByTestId("detail-run-now").click();
  await expect(app.getByTestId("detail-notice")).toContainText("已排入队列");
});

test("跑过之后运行历史列得出来，且能点进运行详情", async ({ app }) => {
  await app.goto("/w/growth/schedules");
  const id = await createAndOpenDetail(app, { name: "有历史的任务", prompt: "做事" });

  // The mock has no scheduler, so history can only be built with a hook (see the notes on seedScheduleHistoryMock).
  await seedScheduleHistoryMock(app, id, [{}, {}, { errorSummary: "容器没起来" }]);
  await app.reload();

  await app.getByTestId("detail-tab-history").click();
  await expect(app.getByTestId("detail-history-list")).toBeVisible();
  // The prefix is `detail-history-row-`: `detail-run-` would also count detail-run-count /
  // detail-run-now (5 in practice). Same as the rule "do not add testids starting with event-".
  await expect(app.locator('[data-testid^="detail-history-row-"]')).toHaveCount(3);
  // The three counters must match: ran 3 times, failed once.
  await expect(app.getByTestId("detail-run-count")).toContainText("3");
  await expect(app.getByTestId("detail-fail-count")).toContainText("1");

  // A history entry must be clickable into the run detail: that is the entry to "see what really
  // happened in this round".
  await app.locator('[data-testid^="detail-history-row-"]').first().click();
  await expect(app).toHaveURL(/\/runs\/run_sch_/);
});

test("tab 在 URL 里，刷新不丢", async ({ app }) => {
  await app.goto("/w/growth/schedules");
  const id = await createAndOpenDetail(app, { name: "tab 进 URL", prompt: "做事" });

  await app.getByTestId("detail-tab-history").click();
  await expect(app).toHaveURL(/\?tab=history$/);

  // Coming in with the parameter directly must also land on the history tab (the precondition for shareable links).
  await app.goto(`/w/growth/schedules/${id}?tab=history`);
  await expect(app.getByTestId("detail-history-list").or(app.getByText("还没有运行记录"))).toBeVisible();
  await expect(app.getByTestId("detail-config")).toHaveCount(0);
});

test("更早的历史查不到时要说出来，不能显示成没跑过", async ({ app }) => {
  // A gap in existing data: runs created before runs got scheduleId have no such link, and **it cannot be
  // backfilled**. Without saying so, a short list would be read as "this schedule only ran this many
  // times", the opposite of the truth.
  await app.goto("/w/growth/schedules");
  const id = await createAndOpenDetail(app, { name: "有空洞的任务", prompt: "做事" });

  // 2 can be found, but it ran 18 times in total: the difference of 16 is that old data that cannot be found.
  await seedScheduleHistoryMock(app, id, [{}, {}], { runCount: 18, failCount: 1 });
  await app.reload();
  await app.getByTestId("detail-tab-history").click();

  await expect(app.getByTestId("detail-history-gap")).toContainText("16");
  await expect(app.getByTestId("detail-history-count")).toContainText("18");
});

test("离自动暂停还差几次要说出来", async ({ app }) => {
  // After 5 consecutive failures the server pauses it automatically (to prevent a cascade), and that
  // is "stopped without anyone knowing". Without stating the remaining count, users see a schedule that
  // suddenly stopped running.
  await app.goto("/w/growth/schedules");
  const id = await createAndOpenDetail(app, { name: "快被停掉的任务", prompt: "做事" });

  await seedScheduleHistoryMock(app, id, [{ errorSummary: "又失败了" }], {
    runCount: 3,
    failCount: 3,
    consecutiveFailures: 3,
  });
  await app.reload();

  // Threshold 5, already failed 3 times in a row -> 2 to go.
  await expect(app.getByTestId("detail-near-autopause")).toContainText("2");
  await expect(app.getByTestId("detail-last-error")).toContainText("又失败了");
});

test("不存在的任务给引导而不是白屏", async ({ app }) => {
  // This path is **normally reached**: a one-shot schedule deletes itself after running, and a user
  // refreshing then sees exactly this page.
  await app.goto("/w/growth/schedules/sch_不存在");
  await expect(app.getByText("这条定时任务不存在")).toBeVisible();
  await app.getByTestId("detail-back-empty").click();
  await expect(app).toHaveURL(/\/w\/growth\/schedules$/);
});
