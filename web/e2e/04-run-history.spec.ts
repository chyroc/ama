/**
 * Requirement 3: agent run history can be viewed, and run history has all kinds of events.
 *
 * This is the most complex part, so it is tested in the most detail: categorized rendering of 33 event
 * kinds, tool_use/tool_result pairing, multi-thread grouping, filtering and noise reduction, token
 * totals, error and retry states, tools that need confirmation.
 */

import { closeFilters, expect, openFilters, test } from "./fixtures";

test("运行列表展示状态、耗时、token 与触发来源", async ({ app }) => {
  await app.goto("/w/growth/runs");

  const list = app.getByTestId("run-list");
  await expect(list).toBeVisible();
  // 9 entries: run_1..run_5 + run_6 (the debugging sample covering all event types)
  // + run_7 (the failed chat) / run_8 (rescheduling), samples for the chat list status display,
  // see the comment above run_7 in data/seed.ts;
  // + run_9 (the one whose closing event was not stored, folded thanks to the run.status fallback).
  await expect(app.locator('[data-testid^="run-row-"]')).toHaveCount(9);

  // All five statuses must be distinguishable.
  //
  // ⚠ requires_action means "tool running", **not** "waiting for confirmation": under self_hosted Ark only
  // does model inference, and tools are picked up by amad's worker and run in our containers, so on every
  // tool call the Ark side goes idle with stop_reason=requires_action (meaning "control handed back,
  // waiting for tool_result"). The list level has no events and cannot tell "really waiting for user
  // approval", so it only shows a generic in-progress state.
  await expect(list.getByText("运行中")).toBeVisible();
  await expect(list.getByText("工具执行中")).toBeVisible();
  await expect(list.getByText("空闲").first()).toBeVisible();
  // terminated with an error -> marked "failed", not mixed in with normal endings.
  // `.first()`: there are now two failures (run_3 and run_7), and without scoping it hits strict mode.
  await expect(list.getByText("失败").first()).toBeVisible();
  // There must also be a rescheduling one (run_8), or this one of the five would never have been rendered
  await expect(list.getByText("调度中")).toBeVisible();

  // Trigger source
  await expect(list.getByText(/飞书 · 增长数据日常/)).toBeVisible();
  await expect(list.getByText(/API · ci-bot/)).toBeVisible();
});

test("可按 agent、状态、关键词筛选运行", async ({ app }) => {
  await app.goto("/w/growth/runs");

  // 3 terminated: run_3 (failed), run_6 (session deleted), run_7 (chat failed)
  await app.getByTestId("status-filter-terminated").click();
  await expect(app.locator('[data-testid^="run-row-"]')).toHaveCount(3);
  await app.getByTestId("status-filter-terminated").click();

  await app.getByTestId("run-search").fill("GMV");
  await expect(app.locator('[data-testid^="run-row-"]')).toHaveCount(1);
  await app.getByTestId("run-search").fill("");

  await app.getByTestId("run-agent-filter").click();
  await app.getByRole("option", { name: "工单分诊助手" }).click();
  // 5 entries: run_4 / run_5 / run_6 / run_8 / run_9 all belong to this agent
  await expect(app.locator('[data-testid^="run-row-"]')).toHaveCount(5);
});

/**
 * When nothing is found it says "no matching runs", **not "no run records yet"**.
 *
 * These two sentences mean completely opposite things to the user: one makes them change the filter,
 * the other makes them think this workspace is empty.
 *
 * After filtering moved to the backend this would **silently flip**. The old test was
 * `runs?.length ? no match : none yet`; back then runs was the whole workspace, so length happened to
 * equal "does this workspace have runs"; after the move, runs became "how many this filtered query
 * returned", so with no match it is 0, and a user searching while looking at a screen of run records
 * would be told "no run records yet".
 *
 * ⚠️ Both sentences must be asserted. Asserting only that "no matching runs" is visible, writing the
 * test as the constant true would also pass, and that would make **a really empty workspace** (the one
 * created in 06-journey) say the wrong thing. That case is locked in on the other side; here the
 * negative is locked in once more: the empty-state title must not be "no run records yet".
 */
test("搜不到时说「没有匹配」，而不是「还没有运行记录」", async ({ app }) => {
  await app.goto("/w/growth/runs");
  // First confirm this workspace **really has** runs; otherwise the assertion below tests something else and is always true.
  await expect(app.getByTestId("run-list")).toBeVisible();

  await app.getByTestId("run-search").fill("这个词不可能匹配到任何一条运行");

  await expect(
    app.getByRole("heading", { name: "没有匹配的运行" }),
  ).toBeVisible();
  await expect(
    app.getByRole("heading", { name: "还没有运行记录" }),
  ).toHaveCount(0);

  // Also walk the status filter path: it and the keyword are two entries sharing the same hasAnyRuns test.
  // run_2 is the only running one in the growth workspace, so with an "impossible to match" word it must filter to zero.
  await app.getByTestId("run-search").fill("");
  await expect(app.getByTestId("run-list")).toBeVisible();
  await app.getByTestId("status-filter-rescheduling").click();
  await app.getByTestId("run-search").fill("GMV");
  await expect(
    app.getByRole("heading", { name: "没有匹配的运行" }),
  ).toBeVisible();
});

test("运行详情渲染多种类别的事件", async ({ app }) => {
  await app.goto("/w/growth/runs/run_1?view=events");

  await expect(app.getByTestId("evt-list")).toBeVisible();

  // At least these kinds must appear (conversation/thinking/tool/thread/status/error)
  for (const cat of ["conversation", "thinking", "tool", "thread", "status", "error"]) {
    await expect(app.locator(`[data-testid="evt-row-${cat}"]`).first()).toBeVisible();
  }

  // The bodies of user messages and agent replies are expanded by default.
  // Note the scope is limited with event-conversation: the folded "raw payload" also contains the same
  // text (hidden="until-found", which Playwright still matches), and without scoping it hits strict mode.
  const conversations = app.locator('[data-testid="evt-row-conversation"]');
  await expect(conversations.filter({ hasText: "帮我看下上周华东区的 GMV" }).first()).toBeVisible();
  await expect(conversations.filter({ hasText: "上周（8/17–8/23）华东区 GMV" }).first()).toBeVisible();
});

/*
  ⚠️ **The meaning of this case was deliberately reversed.**

  It used to be "tool_use and tool_result are paired into one entry", asserting the two events merged
  into **the same row** (both input and result inside firstTool). After the event view became "the
  faithful raw view", pairing no longer happens: the start and the receipt **each take a row**, and the
  detail is in the side panel instead of an inline expansion.

  Pairing itself did not disappear; it is the right shape of **the chat view**, covered in the
  `chat-tool-row` cases (lines 146 / 316 of this file, 09-chat.spec.ts:48). This now asserts "it really
  split into two rows, and each detail matches": exactly what "one event per row" must guard.
*/
test("tool_use 与 tool_result 各占一行，详情分别给入参与结果", async ({ app }) => {
  await app.goto("/w/growth/runs/run_1?view=events");

  // The start row: the detail has the input, and **no** result
  const use = app.locator('[data-testid="evt-row-tool"][data-evt-type="agent.tool_use"]').first();
  await expect(use).toContainText("read");
  await use.click();
  const detail = app.getByTestId("evt-detail");
  await expect(detail).toContainText("gmv_definition.md");
  await expect(detail).not.toContainText("GMV = SUM(pay_amount)");

  // The receipt row: the detail has the result. It is **another row**, not an expansion of the same row
  const result = app
    .locator('[data-testid="evt-row-tool"][data-evt-type="agent.tool_result"]')
    .first();
  await result.click();
  await expect(detail).toContainText("执行结果");
  await expect(detail).toContainText("GMV = SUM(pay_amount)");

  // The raw JSON is always a single event (the old version was an array due to pairing, shown as "raw payload (2)")
  await expect(detail.getByTestId("evt-raw-json")).toBeVisible();
});

/*
  ⚠️ **The meaning was reversed as well.** It used to be "pairing makes the number of entries smaller
  than the number of raw events", asserting shown < raw.

  Now the event view has one event per row, so **the row count equals the raw event count**; the main
  number on the summary card also changed to the raw count (the paired step count became a hint line),
  otherwise it would contradict itself with "the summary says 12, the list has 18 rows".
*/
test("事件视图的行数等于原始事件数，摘要主数字与它一致", async ({ app }) => {
  await app.goto("/w/growth/runs/run_1?view=events");
  await expect(app.getByTestId("evt-list")).toBeVisible();

  // Summary card: the main number is the raw count, and the hint line gives "N steps after merging"
  const text = await app.locator("main").innerText();
  const m = text.match(/事件数\s*(\d+)\s*合并后\s*(\d+)\s*步/);
  expect(m, `未在摘要里找到事件数，实际文本片段：${text.slice(0, 400)}`).not.toBeNull();
  const rawCount = Number(m![1]);
  const merged = Number(m![2]);
  // Pairing still exists (used by the chat view), so the merged step count should be smaller
  expect(merged).toBeLessThan(rawCount);

  // After turning on the trace kinds hidden by default, the list row count should **exactly equal** the raw event count
  await openFilters(app);
  await app.getByTestId("filter-trace").click();
  await expect(app.locator('[data-testid^="evt-row-"]')).toHaveCount(rawCount);
});

test("MCP 工具调用与失败结果会被标出来", async ({ app }) => {
  await app.goto("/w/growth/runs/run_1?view=events");

  // The start row: the summary gives "service/tool name"
  const mcp = app
    .locator('[data-testid="evt-row-tool"][data-evt-type="agent.mcp_tool_use"]')
    .first();
  await expect(mcp).toContainText("data-warehouse");

  /*
    The failure is on the **receipt** row, no longer "seen after expanding the start row": the two are
    separate events. Use hasText to filter for the receipt with the failure marker (the seed has one
    failure caused by querying the wrong table).
  */
  const failed = app
    .locator('[data-testid="evt-row-tool"][data-evt-type="agent.mcp_tool_result"]')
    .filter({ hasText: "执行失败" })
    .first();
  await expect(failed).toBeVisible();
  await failed.click();
  const detail = app.getByTestId("evt-detail");
  await expect(detail).toContainText("执行失败");
  await expect(detail).toContainText(/not found/);
});

/*
  The "approved / awaiting your confirmation" states are a product of **pairing** (they combine the
  tool_use's evaluated_permission with the later user.tool_confirmation), so they belong to **the chat
  view**, covered in the case at line 316 of this file. On the event view side this now asserts two
  more modest things: the start row gives the raw evaluated_permission value, and the confirmation
  event is its own row with the approval result.
*/
test("需要用户确认的工具：权限判定与确认事件各自可查", async ({ app }) => {
  await app.goto("/w/growth/runs/run_1?view=events");

  // The start row's detail gives the **raw value** of evaluated_permission (ask), with no semantic processing
  const write = app
    .locator('[data-testid="evt-row-tool"][data-evt-type="agent.tool_use"]')
    .filter({ hasText: "write" })
    .first();
  await write.click();
  await expect(app.getByTestId("evt-detail")).toContainText("ask");

  // The confirmation itself is a separate event (user.tool_confirmation), no longer merged into the row above
  const confirm = app
    .locator('[data-testid="evt-row-tool"][data-evt-type="user.tool_confirmation"]')
    .first();
  await expect(confirm).toBeVisible();
  await confirm.click();
  await expect(app.getByTestId("evt-detail").getByTestId("evt-confirmation")).toContainText(
    "已批准",
  );
});

/*
  "Waiting for the result" is also a product of pairing (it is "waiting" only when the start exists and
  the receipt has not come), and belongs to the chat view. The **faithful** expression on the event view
  is: that call in run_4 **has no receipt event at all**, so assert the tool_use exists and there is not a
  single matching tool_result. That is firmer than a hint sentence.
*/
test("未完成的运行：只有发起事件、没有回执事件，且未处理的标为待处理", async ({ app }) => {
  await app.goto("/w/growth/runs/run_4?view=events");
  await expect(app.getByTestId("evt-list")).toBeVisible();

  await expect(
    app.locator('[data-testid="evt-row-tool"][data-evt-type="agent.mcp_tool_use"]'),
  ).toHaveCount(1);
  // Not a single receipt: this is what "not finished yet" looks like at the raw event level
  await expect(
    app.locator('[data-testid="evt-row-tool"][data-evt-type="agent.mcp_tool_result"]'),
  ).toHaveCount(0);

  // Events with processed_at null are marked pending. In the seed this one is span.model_request_start,
  // which belongs to the trace kind, hidden by default, so turn on the trace filter first
  await openFilters(app);
  await app.getByTestId("filter-trace").click();
  await expect(app.getByText("待处理").first()).toBeVisible();
});

test("默认隐藏 span 追踪事件，可手动打开", async ({ app }) => {
  await app.goto("/w/growth/runs/run_1?view=events");

  const count = () => app.locator('[data-testid^="evt-row-"]').count();
  const before = await count();
  await expect(app.locator('[data-testid="evt-row-trace"]')).toHaveCount(0);

  await openFilters(app);
  await app.getByTestId("filter-trace").click();
  await expect(app.locator('[data-testid="evt-row-trace"]').first()).toBeVisible();
  expect(await count()).toBeGreaterThan(before);
});

test("默认是对话视图：一轮结束后只见输入输出，中间过程折叠可展开", async ({ app }) => {
  await app.goto("/w/growth/runs/run_1");

  // Not the event stream by default: that vertical timeline and its entries should not appear at all
  await expect(app.getByTestId("evt-list")).toHaveCount(0);
  await expect(app.locator('[data-testid="evt-row-tool"]')).toHaveCount(0);
  await expect(app.locator('[data-testid="evt-row-status"]')).toHaveCount(0);

  // Both input and output are on the main line
  await expect(app.getByTestId("chat-user-message")).toContainText("帮我看下上周华东区的 GMV");
  await expect(
    app.getByTestId("chat-agent-message").filter({ hasText: "华东区 GMV" }).first(),
  ).toBeVisible();

  // run_1 has finished, so the steps are folded: the tool rows are hidden in a hidden="until-found" panel
  const process = app.getByTestId("turn-process").first();
  await expect(process).toBeVisible();
  await expect(app.getByTestId("chat-tool-row").first()).not.toBeVisible();

  // The summary row gives the step count. Note that the first segment of run_1 (system prompt + thread +
  // thinking) has no tool calls: the tools are all in the second segment after the reply "let me confirm
  // the definition first", so look for "tool calls" in that segment.
  await expect(process).toContainText("个步骤");
  const toolProcess = app.getByTestId("turn-process").filter({ hasText: "次工具调用" }).first();
  await expect(toolProcess).toBeVisible();

  // The specific tools only show after opening
  await toolProcess.getByTestId("turn-process-trigger").click();
  await expect(app.getByTestId("chat-tool-row").first()).toBeVisible();
});

test("收尾事件没落库时，run 已是终态就仍然折叠", async ({ app }) => {
  /*
    The shape seen in a real production session: one turn of the chat had long ended (the answer was
    complete), but the intermediate steps stayed laid out. The root cause was **not the folding logic**:
    the test in `endsTurn()` matches `turnEnded` in the backend `scheduler.go` word for word; it was that
    the `session.status_idle (end_turn)` never reached the frontend (the catch-up gate only accepts
    running, see core/ark.go).

    run_9 has exactly that shape: the event stream stops at a `requires_action` idle, followed only by
    the agent's answer and no closing event; while at the run level status is already idle. The test is
    that **the steps are still folded**.

    ⚠️ Asserting only that `turn-process` is visible is not enough: it is also visible when laid out.
    What must be judged is that **the tool rows are not visible** (the folded panel is hidden="until-found"):
    that is what "folded" means.
  */
  await app.goto("/w/growth/runs/run_9");

  // Both input and output are there
  await expect(app.getByTestId("chat-user-message")).toContainText("超时订单");
  await expect(
    app.getByTestId("chat-agent-message").filter({ hasText: "11:00" }).first(),
  ).toBeVisible();

  // The intermediate steps collapse into one summary row: the tool rows are hidden
  const process = app.getByTestId("turn-process").first();
  await expect(process).toBeVisible();
  await expect(process).toContainText("个步骤");
  await expect(app.getByTestId("chat-tool-row")).not.toBeVisible();

  // Opening it still shows the steps: folding does not lose information
  await process.getByTestId("turn-process-trigger").click();
  await expect(app.getByTestId("chat-tool-row").first()).toBeVisible();
});

test("仍在跑的会话不会被兜底误折叠", async ({ app }) => {
  /*
    The opposite of the previous case. The fallback only accepts **final states** (idle / terminated):
    `requires_action` and `running` both mean the agent will keep producing events, and folding the steps
    then would hide "what it is doing right now" from the user.

    Without this contrast, wrongly writing the fallback as "fold whenever runStatus is passed" would
    still be all green.
  */
  // run_4: status=running, the last segment is an active process
  await app.goto("/w/growth/runs/run_4");
  await expect(app.getByTestId("chat-tool-row").first()).toBeVisible();

  // run_5: status=requires_action, the tool is running in the container
  await app.goto("/w/growth/runs/run_5");
  await expect(app.getByTestId("chat-tool-row").first()).toBeVisible();
});

test("两个视图可来回切，且视图进 URL 可分享", async ({ app }) => {
  await app.goto("/w/growth/runs/run_1");

  await app.getByTestId("view-events").click();
  await expect(app).toHaveURL(/\?view=events$/);
  await expect(app.getByTestId("evt-list")).toBeVisible();

  // Switch back to chat: the default view is not written into the URL, to avoid a useless query
  await app.getByTestId("view-conversation").click();
  await expect(app).toHaveURL(/\/runs\/run_1$/);
  await expect(app.getByTestId("evt-list")).toHaveCount(0);

  // A link with ?view=events lands directly on the event view
  await app.goto("/w/growth/runs/run_1?view=events");
  await expect(app.getByTestId("evt-list")).toBeVisible();
});

test("没有答案的运行不折叠过程 —— 过程就是答案", async ({ app }) => {
  // run_3: after a tool call, MCP auth failed, retries ran out and it was terminated, with no agent.message
  await app.goto("/w/growth/runs/run_3");
  await expect(app.getByTestId("chat-agent-message")).toHaveCount(0);
  // The failure reason should not be hidden
  await expect(app.getByTestId("chat-error-row").first()).toBeVisible();
  await expect(app.getByTestId("chat-tool-row").first()).toBeVisible();

  // run_5: evaluated_permission=ask with no confirmation event, so this turn really is stuck waiting for
  // the user and does not count as finished: that pending call must be visible at a glance.
  // (It also has a requires_action idle, but that one was merged into the tool row; see the dedicated group below.)
  await app.goto("/w/growth/runs/run_5");
  await expect(app.getByTestId("chat-tool-row").first()).toBeVisible();
  await expect(app.getByTestId("chat-tool-row").first()).toContainText("调用 bash");
});

test("多线程运行可按线程分组查看", async ({ app }) => {
  await app.goto("/w/growth/runs/run_1?view=events");

  // Main thread + SQL sub-thread
  await expect(app.getByText("SQL 执行子智能体").first()).toBeVisible();

  const rows = app.locator('[data-testid^="evt-row-"]');
  const all = await rows.count();

  // Entry counts must be asserted with retrying expect(...).toHaveCount / expect.poll,
  // not compared right away with a bare count(): the filter state now lives in the URL (?thread=),
  // and a click goes through setSearchParams -> route re-render, one more tick than component state.
  // A bare count() does not retry, reads the previous frame's row count, and reports "the entry count did not change after filtering".
  // ThreadSwitcher was also moved into the filter popover
  await openFilters(app);
  await app.getByTestId("thread-sth_sub_sql_01").click();
  await closeFilters(app);
  await expect.poll(() => rows.count()).toBeLessThan(all);
  expect(await rows.count()).toBeGreaterThan(0);

  await openFilters(app);
  await app.getByTestId("thread-all").click();
  await closeFilters(app);
  await expect(rows).toHaveCount(all);
});

test("token 用量由事件汇总得出", async ({ app }) => {
  await app.goto("/w/growth/runs/run_1?view=events");
  // Wait for the detail to finish loading, or it reads "loading run detail"
  await expect(app.getByTestId("evt-list")).toBeVisible();

  const main = await app.locator("main").innerText();
  expect(main).toContain("Token 总量");
  expect(main).toContain("缓存命中");
  expect(main).toContain("模型请求");
  // run_1 in the seed has two model_request_end
  expect(main).toMatch(/模型请求\s*2/);
});

test("可恢复错误显示重试中，不可恢复的才算失败", async ({ app }) => {
  /*
    The error type and retry_status are now in **the detail panel** (the row only has a one-line
    summary), so that row must be opened first. The error type string is deliberately given as-is in
    monospace: when debugging you grep the database with it.
  */
  // The rate-limit error in run_1 retries automatically, and the run ends normally in the end
  await app.goto("/w/growth/runs/run_1?view=events");
  await app.locator('[data-testid="evt-row-error"]').first().click();
  const detail = app.getByTestId("evt-detail");
  await expect(detail.getByTestId("evt-error-type")).toContainText("model_rate_limited_error");
  await expect(detail).toContainText("正在重试");

  // run_3's MCP auth failure ran out of retries, and the run was terminated
  await app.goto("/w/growth/runs/run_3?view=events");
  await app.locator('[data-testid="evt-row-error"]').first().click();
  await expect(app.getByTestId("evt-detail").getByTestId("evt-error-type")).toContainText(
    "mcp_auth_failed_error",
  );
  await expect(app.getByTestId("evt-detail")).toContainText("重试耗尽");
  // Use the exact seed text (the Chinese for "credential has expired"); a version missing one
  // character is a different string
  await expect(app.getByTestId("evt-detail")).toContainText(/凭证已过期/);
});

test("产出评估的多轮迭代会完整呈现", async ({ app }) => {
  await app.goto("/w/growth/runs/run_2?view=events");

  /*
    The outcome kind is shown by default; the trial click here only confirms that option exists (whether
    it exists depends on whether this run has outcome events), and is skipped if it cannot be clicked.
    Close the popover after clicking: the rows in the list are clicked next, and the popover's dismiss
    layer would intercept all those clicks.
  */
  await openFilters(app);
  await app.getByTestId("filter-outcome").click({ trial: true }).catch(() => {});
  await closeFilters(app);
  const ends = app.locator(
    '[data-testid="evt-row-outcome"][data-evt-type="span.outcome_evaluation_end"]',
  );
  // Each of the two evaluation rounds has an end event (round one needs_revision, round two satisfied)
  await expect(ends).toHaveCount(2);

  /*
    The result value is Ark's **raw enum string** (needs_revision / satisfied), no longer a processed
    title like "outcome evaluation: needs revision": the event view is for debugging, and only raw values
    match the records in the database. The summary row carries it, so it can be scanned without opening.
  */
  await expect(ends.nth(0)).toContainText("needs_revision");
  await expect(ends.nth(1)).toContainText("satisfied");

  // Open to see the detail: the result is a Chip, with the iteration round
  await ends.nth(1).click();
  const detail = app.getByTestId("evt-detail");
  await expect(detail.getByTestId("evt-outcome-result")).toContainText("satisfied");
  await expect(detail).toContainText("2");
});

/**
 * requires_action and "waiting for user approval" are **two different paths**; this group keeps them
 * from being confused again.
 *
 * They were confused once, and the symptom was a string of entries after one tool call:
 *   call bash / thinking / thread idle / …waiting for user confirmation (2 pending) / session idle / waiting for user confirmation
 * while the call's permission was actually allow, and **nothing needed a user click**: under
 * self_hosted Ark only does model inference, and tools are picked up by amad's worker and run in our
 * containers, so on every tool call the Ark side always goes idle with stop_reason=requires_action,
 * meaning "control is handed back to you, waiting for user.tool_result". It is a mechanism detail, not
 * "it stopped".
 */
test("requires_action 的 idle 呈现为工具执行中，不再另起「空闲」条目", async ({ app }) => {
  // The MCP call in run_4 has permission=allow, no receipt, and two requires_action idles
  // (thread level + session level): exactly the real shape of every tool call under self_hosted.
  /*
    ⚠️ **This whole case moved to the chat view.**

    "Absorbing the requires_action idle into the tool row and showing it as running" is the behavior of
    `buildTimeline()`, and that pipeline now **only serves the chat view**: after the event view became the
    faithful raw view, every event takes its own row and nothing is absorbed (which is exactly what it
    wants). So the regression guard for the absorbing logic must hang on the chat view, or this case would
    become an assertion about a shape that no longer exists.
  */
  await app.goto("/w/growth/runs/run_4");

  const tool = app.getByTestId("chat-tool-row").first();
  await expect(tool).toContainText("MCP 调用");
  // The thread-level idle was merged into this tool call, shown as the in-progress state
  await expect(tool.getByTestId("tool-executing")).toBeVisible();
  await expect(tool).toContainText("执行中");

  // **The absorbed one no longer appears as its own entry.**
  //
  // The test must be "the title it would show as its own entry" (tl.threadHandoff); asserting only that
  // the old text "thread idle" does not appear is not enough: that text was renamed and cannot be found
  // whether absorbed or not, so the assertion is always true and catches no regression.
  // This was measured: removing the `absorbedIdles.has` line in buildTimeline turns the line below red.
  await expect(app.getByText("线程交还控制权")).toHaveCount(0);
  // There should also be no wording asking the user to act (this call has permission=allow; nobody needs to click anything)
  await expect(app.getByText("待你确认")).toHaveCount(0);

  /*
    Contrast: **both idles are there in the event view**, not one missing.

    Absorbing is the chat view's noise reduction, not "deleting events": the debugging view must match
    the database records one by one. This line guards two things at once: absorbing did not spread to the
    event view, and raw events were not dropped.
  */
  await app.goto("/w/growth/runs/run_4?view=events");
  await expect(
    app.locator('[data-testid^="evt-row-"][data-evt-type="session.thread_status_idle"]'),
  ).toHaveCount(1);
  await expect(
    app.locator('[data-testid^="evt-row-"][data-evt-type="session.status_idle"]'),
  ).toHaveCount(1);
});

test("真的要用户批准时仍然醒目提示，且不会被说成「执行中」", async ({ app }) => {
  // run_5: evaluated_permission=ask with no user.tool_confirmation: **this** is waiting for the user.
  // The test is ask + no confirmation, not stop_reason (run_5 also has a requires_action idle, but that
  // one was merged into the tool row, a different thing from "waiting for the user").
  await app.goto("/w/growth/runs/run_5");

  // The default chat view must show where it is stuck: this turn has not finished, so the steps are not
  // folded, and the hint is visible even in the folded state (otherwise users would never know to go confirm)
  const toolRow = app.getByTestId("chat-tool-row").first();
  await expect(toolRow).toContainText("调用 bash");
  await expect(toolRow.getByTestId("tool-needs-confirm")).toBeVisible();
  await expect(toolRow).toContainText("待你确认");
  // While waiting for the user it **must not** also say "running": nothing is running then
  await expect(toolRow.getByTestId("tool-executing")).toHaveCount(0);

  // The top status must be accurate too. At the RunStatus level both run_5 and run_4 are requires_action;
  // the detail page has events so it can tell them apart; otherwise the top would say "tool running" and
  // the bottom "awaiting your confirmation", contradicting itself.
  await expect(app.getByText("待你确认").first()).toBeVisible();
  await expect(app.getByText("工具执行中")).toHaveCount(0);

  // After expanding, spell out "what you need to do"
  await toolRow.click();
  await expect(app.getByText("需要你批准后才会执行").first()).toBeVisible();

  /*
    The event view does not repeat the "awaiting your confirmation" judgment (that is a product of
    pairing, see the cases above); instead it asserts **the raw facts**: run_5's tool_use has
    evaluated_permission=ask, and there is **no** user.tool_confirmation event: that is what "waiting for
    the user" looks like at the raw event level.
  */
  await app.goto("/w/growth/runs/run_5?view=events");
  const use = app
    .locator('[data-testid="evt-row-tool"][data-evt-type="agent.tool_use"]')
    .first();
  await use.click();
  await expect(app.getByTestId("evt-detail")).toContainText("ask");
  await expect(
    app.locator('[data-testid^="evt-row-"][data-evt-type="user.tool_confirmation"]'),
  ).toHaveCount(0);
});

test("对话视角不播报 self_hosted 的生命周期抖动", async ({ app }) => {
  // On every tool call the session/thread flips between running and idle once (hand back control ->
  // worker returns tool_result -> Ark starts running again), so N tool calls in a turn give N sets. That is
  // self_hosted's heartbeat, not chat content: the user's question would be washed away by a string of
  // status announcements, none of which tells them anything.
  await app.goto("/w/growth/runs/run_1");
  await expect(app.getByTestId("chat-user-message").first()).toBeVisible();

  // Lay out all process segments, confirming these rows do not exist even **after expanding** (not "hidden in a fold")
  for (const trigger of await app.getByTestId("turn-process-trigger").all()) {
    await trigger.click();
  }
  await expect(app.getByTestId("chat-tool-row").first()).toBeVisible();

  await expect(app.getByText("会话开始运行")).toHaveCount(0);
  await expect(app.getByText("线程开始运行")).toHaveCount(0);
  // The thread-level end_turn idle is noise too: it used to hang at the end of every turn as "thread idle ·
  // normal end" (the session-level one was eaten by groupTurns, but nobody ate the thread-level one).
  await expect(app.getByText("线程空闲")).toHaveCount(0);

  // **But the event view still gives all 33 kinds**: what is removed is the noise in the "chat" way of
  // reading, not data. Without this half of the assertion, deleting the noise from buildTimeline would
  // also pass, and that would really be losing data.
  await app.goto("/w/growth/runs/run_1?view=events");
  await expect(app.getByText("会话开始运行").first()).toBeVisible();
  await expect(app.getByText("线程开始运行").first()).toBeVisible();
  await expect(app.getByText("线程空闲").first()).toBeVisible();
});

test("requires_action 不算一轮结束：那次调用不会被折叠起来", async ({ app }) => {
  // endsTurn() returns false for requires_action, so this turn has done=false:
  // shown as the steps laid out, the tool rows directly visible (no need to open the summary row), and the
  // summary row saying "running".
  //
  // Treating it as ended has two consequences: the call that is still running/pending gets folded (and it
  // is exactly the only thing the user wants to see), and the real end_turn later is counted as the next
  // turn, cutting one turn of the chat in half.
  //
  // ⚠ **The only path that reaches this gate is the session-level idle in run_4 with empty event_ids.**
  // Idles that can attach to a tool call are already absorbed at the buildTimeline stage and never reach
  // endsTurn; so without that "cannot attach" seed, this gate would be untestable dead code (measured:
  // changing the `!== "requires_action"` in endsTurn to return true, only the run_4 half goes red).
  await app.goto("/w/growth/runs/run_4");
  await expect(app.getByTestId("chat-tool-row").first()).toBeVisible();
  await expect(app.getByTestId("turn-process-trigger").first()).toContainText("正在执行");

  // run_5 takes another path (ask with no confirmation, no idle involved in the judgment), and should not
  // be folded either: that pending call is the only thing the user wants to see when opening the page.
  await app.goto("/w/growth/runs/run_5");
  await expect(app.getByTestId("chat-tool-row").first()).toBeVisible();

  // Lock in the contrast the other way: run_1 ends with end_turn, so that turn really did end -> steps folded
  await app.goto("/w/growth/runs/run_1");
  await expect(app.getByTestId("chat-tool-row").first()).not.toBeVisible();
});

test("从 agent 详情能进到该 agent 的运行历史", async ({ app }) => {
  await app.goto("/w/growth/agents/agent_analyst?tab=runs");

  await expect(app.getByTestId("agent-run-run_1")).toBeVisible();
  await app.getByTestId("agent-run-run_1").click();
  await expect(app).toHaveURL(/\/runs\/run_1$/);
  await expect(app.getByRole("heading", { name: "华东区上周 GMV 表现分析" })).toBeVisible();
});

// ---------------------------------------------------------------------------
// Multi-dimensional filtering of the Trace view.
//
// All three axes are real Ark categories: domain (user/agent/session/span), semantic kind (8), and
// concrete type (those seen in this run). On top are three orthogonal conditions: keyword, slow calls,
// failures. Entry counts are always asserted with retrying toHaveCount / expect.poll: the filter state is
// in the URL, a click takes one more route tick, and a bare count() reads the previous frame.
// ---------------------------------------------------------------------------

test("按事件域筛选可收窄事件流", async ({ app }) => {
  await app.goto("/w/growth/runs/run_1?view=events");

  const rows = app.locator('[data-testid^="evt-row-"]');
  // span belongs to the trace kind, hidden by default, so turn it on first, then narrow by domain
  await openFilters(app);
  await app.getByTestId("filter-trace").click();
  await expect(app.locator('[data-testid="evt-row-trace"]').first()).toBeVisible();
  const all = await rows.count();

  await openFilters(app);
  await app.getByTestId("filter-domain-agent").click();
  await openFilters(app);
  await app.getByTestId("filter-domain-user").click();
  await openFilters(app);
  await app.getByTestId("filter-domain-session").click();
  // Only the span domain is left: domains are split by type prefix, crossing kinds rather than nesting
  await expect.poll(() => rows.count()).toBeLessThan(all);
  await expect(app.locator('[data-testid="evt-row-trace"]').first()).toBeVisible();
  await expect(app.locator('[data-testid="evt-row-conversation"]')).toHaveCount(0);
});

test("快速筛选「模型」会连带打开默认隐藏的追踪类", async ({ app }) => {
  // This guards a trap that causes no error: span.model_request_* belongs to the trace kind, and trace is
  // hidden by default. If a preset only set the concrete type without the kind, clicking it would show
  // nothing, with no visible reason in the UI.
  await app.goto("/w/growth/runs/run_1?view=events");
  await expect(app.locator('[data-testid="evt-row-trace"]')).toHaveCount(0);

  await openFilters(app);
  await app.getByTestId("filter-preset-model").click();
  await expect(app.locator('[data-testid="evt-row-trace"]').first()).toBeVisible();
  await expect(app.locator('[data-testid="evt-row-tool"]')).toHaveCount(0);
  await expect(app.locator('[data-testid="evt-row-conversation"]')).toHaveCount(0);
});

test("关键词搜索命中入参与原始 payload", async ({ app }) => {
  await app.goto("/w/growth/runs/run_1?view=events");
  const tools = app.locator('[data-testid="evt-row-tool"]');

  // gmv_definition.md only appears in read's input -> only the start entry matches
  await app.getByTestId("timeline-search").fill("gmv_definition");
  await expect(tools).toHaveCount(1);
  await expect(tools.first()).toContainText("read");

  /*
    toolu_mcp_01 only appears in the raw payload (it is an id used for pairing, not rendered in the UI):
    finding it means the raw JSON really is in the search scope.

    ⚠️ **It matches 2, not 1**: that id appears in the payloads of both the mcp_tool_use and the
    mcp_tool_result events, and the event view no longer merges them into one. The old version had 1 here.
  */
  await app.getByTestId("timeline-search").fill("toolu_mcp_01");
  await expect(tools).toHaveCount(2);
  await expect(tools.first()).toContainText("data-warehouse");

  await app.getByTestId("timeline-search").fill("这串东西哪儿都没有");
  await expect(app.locator('[data-testid^="evt-row-"]')).toHaveCount(0);
  await expect(app.getByText("没有匹配的事件")).toBeVisible();
});

test("筛选态进 URL，刷新后仍生效", async ({ app }) => {
  await app.goto("/w/growth/runs/run_1?view=events");

  await openFilters(app);
  await app.getByTestId("filter-preset-tool").click();
  // The popover's dismiss layer would intercept typing into the search box; close it first
  await closeFilters(app);
  await app.getByTestId("timeline-search").fill("run_query");
  await expect(app).toHaveURL(/cat=tool/);
  await expect(app).toHaveURL(/q=run_query/);

  // Wait for the filter to really land before taking the baseline count: run_query only matches those
  // MCP entries, and taking it too early gets the row count of the frame "preset applied, keyword not yet".
  /*
    3 entries: the same number as the old version, but **the meaning changed**: the old version was "3
    paired MCP calls", now it is "3 mcp_tool_use events whose payload contains run_query".
    The receipt events' payloads do not contain this string (they only carry mcp_tool_use_id and the result
    body), so they do not match. Checked against the seed data: run_1 has exactly 3 events containing
    run_query, all mcp_tool_use.
  */
  const tools = app.locator('[data-testid="evt-row-tool"]');
  await expect(tools).toHaveCount(3);

  await app.reload();
  // Both the view and the filters are restored from the URL; it should not fall back to the defaults
  await expect(app.getByTestId("evt-list")).toBeVisible();
  await expect(tools).toHaveCount(3);
  await expect(app.getByTestId("timeline-search")).toHaveValue("run_query");

  // Reset removes all filter parameters together, leaving only ?view=events
  await app.getByTestId("filter-reset").click();
  await expect(app).toHaveURL(/\/runs\/run_1\?view=events$/);
});

test("默认筛选不写进 URL", async ({ app }) => {
  // Defaults in the URL would leave a meaningless query, and make "shared links" hard to read
  await app.goto("/w/growth/runs/run_1?view=events");
  await expect(app.getByTestId("evt-list")).toBeVisible();
  await expect(app).toHaveURL(/\/runs\/run_1\?view=events$/);
  await expect(app.getByTestId("filter-reset")).toHaveCount(0);
});

/*
  ⚠️ **"Derived duration of tool calls" no longer exists in the event view, so this case changed what it asserts.**

  That duration is "receipt time - start time", which can only be computed by **pairing** the two
  events, and the event view deliberately does not pair. Forcing a duration onto a single event could
  only be made up: the start does not know when it ended, the receipt does not know when it started.

  The only computable duration in the event view is `span.model_request_end`, because Ark gives an
  explicit `model_request_start_id` (the only pair with a linking field). That one is covered by
  "span.model_request_end computes duration" in 26-events-view.spec.ts.

  "Only slow calls" is the same: in the event view it only filters model requests, and **anything whose
  duration cannot be computed never matches** (rather than being let through as "not slow"). This
  asserts that behavior.
*/
test("事件视图：慢筛选只对算得出耗时的模型请求生效，算不出的不假造", async ({ app }) => {
  await app.goto("/w/growth/runs/run_1?view=events");

  // Model requests are only visible after turning on trace (hidden by default)
  await openFilters(app);
  await app.getByTestId("filter-trace").click();
  const slowToggle = app.getByTestId("filter-slow");
  await expect(slowToggle).toBeVisible();
  await slowToggle.click();

  /*
    Only "model request end" events over the duration threshold remain. Not a single tool entry should
    remain: not because tools are all fast, but because **a single tool event has no computable duration**,
    and the right behavior then is "no match", not "let it through as not slow".
  */
  const rows = app.locator('[data-testid^="evt-row-"]');
  // run_1 has 2 pairs of model requests, each 7s (>5s threshold), so 2 end events remain.
  // The numbers were checked against the seed data, not guessed.
  await expect(rows).toHaveCount(2);
  await expect(rows.first()).toHaveAttribute("data-evt-type", "span.model_request_end");
  await expect(rows.nth(1)).toHaveAttribute("data-evt-type", "span.model_request_end");
  await expect(app.locator('[data-testid="evt-row-tool"]')).toHaveCount(0);
});

test("对话视图：一轮的耗时汇总给出来，算不出的不假造", async ({ app }) => {
  /*
    The paired duration belongs to the chat view (see the notes on the previous case). It shows in **the
    turn summary** row ("N steps · M tool calls · duration"), rather than a marker on each tool row: in
    the folded state users want "how long this turn took".

    ⚠️ Do not look for `row-duration` here: that testid belongs to RunTimeline, which no longer renders.
    The first version was written that way, and ended up asserting an element that does not exist in the DOM.
  */
  await app.goto("/w/growth/runs/run_1");

  // The segment of run_1 with tool calls: its summary should give the step count, tool call count and duration
  const summary = app.getByTestId("turn-process").filter({ hasText: "次工具调用" }).first();
  await expect(summary).toBeVisible();
  // The duration is formatted by formatDuration, like `19.0s` / `1m 5s`
  await expect(summary).toContainText(/\d+(\.\d+)?s/);

  /*
    That call in run_4 has no receipt yet -> the duration cannot be computed. The test is that **no
    duration appears in the summary** (ProcessSummary omits that part entirely when durationMs is null,
    instead of showing 0s or a placeholder).
  */
  await app.goto("/w/growth/runs/run_4");
  const pending = app.getByTestId("chat-tool-row").first();
  await expect(pending).toBeVisible();
  const run4Summary = app.getByTestId("turn-process").first();
  await expect(run4Summary).not.toContainText(/\d+(\.\d+)?s/);
});

test("原始 JSON 可复制", async ({ app, context }) => {
  await context.grantPermissions(["clipboard-write"]);
  await app.goto("/w/growth/runs/run_1?view=events");

  // The raw JSON is now an **always-present section** of the detail panel, no longer a second-level fold
  // in the row: clicking the row shows it
  await app.locator('[data-testid="evt-row-tool"]').first().click();
  const raw = app.getByTestId("evt-detail").getByTestId("evt-raw-json");
  await expect(raw).toBeVisible();

  // Only assert that the button gave success feedback, not the clipboard content: that behaves
  // differently across platforms in headless mode. Feedback is read from data-copy-state rather than the
  // tooltip text: the tooltip only renders on hover.
  const copyBtn = raw.getByTestId("copy-button").first();
  await copyBtn.click();
  await expect(copyBtn).toHaveAttribute("data-copy-state", "done");
});

/**
 * The three statistics on the overview page must use the **full** scope, not "only the first page".
 *
 * # Why this deserves its own case
 *
 * The overview page only shows the latest 5 runs, yet must also give three full numbers (total tokens /
 * total runs / failures). Changing the fetch to `listRuns({limit: 5})` is the most natural "while I'm
 * here optimization" for this page, and it would make those three numbers silently count only the first
 * 5: **the page still renders a number, only that number is wrong**: zero errors, typecheck and build
 * all green, existing e2e all green too (those cases only assert "there is a number / a list", which the
 * wrong implementation also satisfies). In code review it looks like just adding a parameter.
 *
 * So the test here is "exactly equal to the full scope", **paired with counterexamples**: first prove the
 * first-5 scope really differs from the full one (with the seed data, 12883 vs 40044 tokens, 1 vs 2
 * failures), or the three equality assertions might be always true on two numbers that happen to match.
 *
 * ⚠️ **The workspace id is fetched from the API, not hard-coded.** The first version hard-coded an id
 * that does not exist, so `listRuns` returned an empty list and all three assertions passed on
 * `0 === 0`: the case was green while checking nothing. The two `toBeGreaterThan` below guard against
 * that: the sample must be meaningful first for the assertions to mean anything.
 */
test("概览的三个统计数字是全量口径，不是只算显示出来的那几条", async ({ app }) => {
  await app.goto("/w/growth");
  await expect(app.getByText("最近运行")).toBeVisible();

  const got = await app.evaluate(async () => {
    const api = (window as { __amaApi?: any }).__amaApi;
    const ws = (await api.getWorkspaceByKey("growth")).id;
    const [all, page1, usage] = await Promise.all([
      api.listRuns(ws),
      api.listRuns(ws, { limit: 5 }),
      api.getWorkspaceUsage(ws),
    ]);
    const tokens = (rs: any[]) =>
      rs.reduce((s: number, r: any) => s + r.usage.input_tokens + r.usage.output_tokens, 0);
    return {
      allLen: all.length,
      page1Len: page1.length,
      fullTokens: tokens(all),
      page1Tokens: tokens(page1),
      fullFailed: all.filter((r: any) => r.errorSummary).length,
      usageTokens: usage.usage.input_tokens + usage.usage.output_tokens,
      usageTotal: usage.totalRuns,
      usageFailed: usage.failedRuns,
    };
  });

  // The sample must be meaningful first: the full set is non-empty, and really more than one page.
  // Without these two, the assertions below might be always true on 0.
  expect(got.allLen).toBeGreaterThan(got.page1Len);
  expect(got.fullTokens).toBeGreaterThan(0);

  // Counterexample: the first-5 scope **must differ** from the full one; this is where the case's real
  // discriminating power lies.
  expect(got.page1Tokens).not.toBe(got.fullTokens);

  // Server-side aggregation == the full scope. Pin each of the three numbers, not just one.
  expect(got.usageTotal).toBe(got.allLen);
  expect(got.usageTokens).toBe(got.fullTokens);
  expect(got.usageFailed).toBe(got.fullFailed);

  // The page shows the full number (not the first-5 one).
  await expect(
    app.getByText(`${got.allLen} 次运行`, { exact: false }).first(),
  ).toBeVisible();
});
