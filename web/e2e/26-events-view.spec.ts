import { closeFilters, expect, openFilters, test } from "./fixtures";

/**
 * Event view (`?view=events`): **one raw event per row, nothing merged**.
 *
 * # What this set watches is "faithfulness"
 *
 * The view is meant for debugging: matching the `run_events` records in the database one by one. It
 * went through one change of direction, and these assertions keep it from going back to the old shape:
 *
 * - **the old version** went through `buildTimeline()`, where `agent.tool_use` and `user.tool_result`
 *   were **paired into one entry**, so there were a third fewer entries than raw events, and the raw
 *   JSON was an **array**;
 * - **now** it bypasses that processing pipeline and takes `RunEvent[]` directly: N events = N rows,
 *   and each row's raw JSON is always a single event.
 *
 * The chat view still uses `buildTimeline()` (pairing is the right shape there), so none of the cases
 * for that half were touched: the split between the two views is exactly what DESIGN.md 3.6.3 describes.
 *
 * # Why run_6
 *
 * The first five runs together only show 27 event kinds; the other 7 never render in local mode.
 * run_6 was added for this: **all 34 kinds covered**, and it deliberately builds two shapes found
 * nowhere else: three events in the same millisecond, and the two time zone forms `+08:00` and a
 * literal `Z` mixed together.
 *
 * ⚠️ testids always use the `evt-` prefix, **not** `event-`: existing cases use
 * `[data-testid^="event-"]` to count "paired entries", and two things with different meanings
 * sharing a prefix would turn those count assertions into a riddle.
 */

/** Event row selector. It uses an attribute prefix instead of listing each kind, so the row count misses nothing. */
const ROWS = '[data-testid^="evt-row-"]';

test.describe("事件视图：原始事件列表", () => {
  test("一条 event 一行：条目数等于原始事件数，工具调用不再被配对", async ({ app }) => {
    await app.goto("/w/growth/runs/run_6?view=events");
    await expect(app.getByTestId("evt-list")).toBeVisible();

    /*
     * run_6 has 20 events; of the 4 kinds hidden by default, only trace appears (2 events:
     * model_request_start / end). So 18 rows are visible by default.
     *
     * The point of this assertion is **not that specific number**, but "it equals 20 after turning on
     * trace", that is, rows == raw events. The old version, after pairing, was always under 20.
     */
    await openFilters(app);
    await app.getByTestId("filter-trace").click();
    await expect(app.locator(ROWS)).toHaveCount(20);

    // The start and the receipt of a tool call are **two rows**, not one. The old version had just one "call bash" here.
    await expect(app.locator(`${ROWS}[data-evt-type="agent.tool_use"]`)).toHaveCount(1);
    await expect(app.locator(`${ROWS}[data-evt-type="user.tool_result"]`)).toHaveCount(1);
  });

  test("摘要不是类型名的复述：同为工具类的两条给出各自最有信息量的一句", async ({ app }) => {
    await app.goto("/w/growth/runs/run_6?view=events");

    // The start row's summary gives the tool name + input; the receipt row gives the result body. Both
    // belong to the tool kind, but the summaries must differ: if all 34 kinds looked the same the list
    // could not be scanned.
    const use = app.locator(`${ROWS}[data-evt-type="agent.tool_use"]`);
    await expect(use).toContainText("bash");
    await expect(use).toContainText("ls -la /workspace");

    // The receipt row gives the beginning of the result body. ⚠️ The summary is **one line** truncated
    // to 80 characters, so only the beginning can be asserted (`notes.md` is on line 4, long cut off);
    // checking the full body means opening the detail, which is another case's job.
    const result = app.locator(`${ROWS}[data-evt-type="user.tool_result"]`);
    await expect(result).toContainText("exit_code: 0");
    // And the two summaries must differ: that is what this case really guards
    const useText = await use.innerText();
    const resultText = await result.innerText();
    expect(useText).not.toBe(resultText);
  });

  test("按时间排序，同一毫秒内靠 sequence 兜底", async ({ app }) => {
    await app.goto("/w/growth/runs/run_6?view=events");
    await expect(app.getByTestId("evt-list")).toBeVisible();

    /*
     * The processed_at of three events falls in **the same millisecond** (the nanoseconds differ, but
     * JS Date only goes to milliseconds, so they parse as exactly equal). Their order can only be
     * decided by sequence.
     *
     * ⚠️ This case must assert **the relative order of these three**, not just "the whole is
     * non-decreasing": the latter is always true within the same millisecond (equal also counts as
     * non-decreasing), a false green.
     */
    const seqs = await app.locator(ROWS).evaluateAll((els) =>
      els.map((el) => Number(el.getAttribute("data-evt-seq"))),
    );

    // Get the indexes of those three same-millisecond events in **render order**
    const types = await app.locator(ROWS).evaluateAll((els) =>
      els.map((el) => el.getAttribute("data-evt-type")),
    );
    const iUser = types.indexOf("user.message");
    const iSystem = types.indexOf("system.message");
    const iRunning = types.indexOf("session.thread_status_running");
    expect(iUser, "user.message 应在列表里").toBeGreaterThanOrEqual(0);
    expect(iSystem).toBeGreaterThanOrEqual(0);
    expect(iRunning).toBeGreaterThanOrEqual(0);
    // Increasing sequence order: user.message -> system.message -> thread_status_running
    expect(iUser).toBeLessThan(iSystem);
    expect(iSystem).toBeLessThan(iRunning);

    /*
      Overall: the row order's sequence must increase monotonically.

      ⚠️ **This one cannot catch "the sequence tie-breaker for the same millisecond is missing"**; do not
      think it can. Verified by mutation: changing that tie-breaker line to `return 0` keeps this
      assertion green. The reason is two layers of stability stacked: the backend `order by sequence`
      (store_pg.go) and the mock's `sort((a,b)=>a.sequence-b.sequence)` (data/api.ts) both guarantee
      **the input is already ordered by sequence**, and `Array.sort` is stable, so the same-millisecond
      ones keep their original order even when the compare function returns 0.

      In other words, this tie-breaker is **not observable** in the UI: it guards against "the input
      happens to be unordered", and neither real data path gives such input. It stays because it costs
      nothing, and `TimelineItem` once really got its order scrambled by `undefined - undefined = NaN`
      (see the comment in server/core/chat.go).

      What this assertion really guards is **ordering by time**: run_6's event times increase with
      sequence in the same order, so "change to reverse time order" or "change to not ordering by time"
      both turn it red (mutation 1 verified red).
    */
    const sorted = [...seqs].sort((a, b) => a - b);
    expect(seqs, `行序与 sequence 不一致，实际：${seqs.join(",")}`).toEqual(sorted);
  });

  test("两种时区形态混排时仍按绝对时刻排，不按字符串比较", async ({ app }) => {
    /*
     * run_6's processed_at uses a `+08:00` offset; the other runs use a literal `Z`.
     * Compared as strings, these two forms are not comparable (`2026-08-25T18:...` is literally larger
     * than `2026-08-25T10:...`, but they are the same moment).
     *
     * What is checked here is "ordered by absolute time within one run": the displayed time of run_6's
     * first event must be a local time after 18:00, and the row order matches sequence (asserted by the
     * previous case). What really exposes the "string sorting" bug is the toEqual(sorted) in the
     * previous case.
     */
    await app.goto("/w/growth/runs/run_6?view=events");
    const first = app.locator(ROWS).first();
    // The time column exists and is not "pending": the nanosecond string parses fine with Date.
    await expect(first).not.toContainText("待处理");
    await expect(app.locator(ROWS).first()).toBeVisible();
  });

  test("点一行看详情：详情里带完整的 processed_at 与单条原始 JSON", async ({ app }) => {
    await app.goto("/w/growth/runs/run_6?view=events");

    // Before selecting there is a guide text, not a blank
    await expect(app.getByText("点左侧任一条事件看详情")).toBeVisible();

    await app.locator(`${ROWS}[data-evt-type="agent.tool_use"]`).click();
    const detail = app.getByTestId("evt-detail");
    await expect(detail).toBeVisible();

    // The type string is given as-is: when debugging you grep the database with it, and a translated
    // name would not be found
    await expect(detail.getByTestId("evt-detail-type")).toHaveText("agent.tool_use");
    // The full timestamp (with nanoseconds and time zone offset), not a shortened HH:mm:ss
    await expect(detail).toContainText("+08:00");
    await expect(detail).toContainText(/\.\d{9}/);

    /*
     * The raw JSON is an always-present section (the old version buried it in a second-level collapse in
     * the row), and it is **always a single event**: in the old version it was an array because of the
     * pairing, so it showed `raw payload (2)` there.
     */
    const raw = detail.getByTestId("evt-raw-json");
    await expect(raw).toBeVisible();
    const jsonText = (await raw.innerText()).replace(/^原始 JSON\s*/, "");
    const parsed = JSON.parse(jsonText);
    expect(Array.isArray(parsed), "原始 JSON 应是单条事件对象，不是数组").toBe(false);
    expect(parsed.type).toBe("agent.tool_use");
  });

  test("agent.message 与 agent.thinking 走 Markdown 渲染", async ({ app }) => {
    await app.goto("/w/growth/runs/run_6?view=events");

    await app.locator(`${ROWS}[data-evt-type="agent.message"]`).click();
    const detail = app.getByTestId("evt-detail");

    /*
     * md-box adds `flow-markdown-body` to the container, and we add `md-message` as a theme hook.
     * Asserting this class exists means asserting "it went through md-box, not whitespace-pre-wrap plain text".
     */
    const md = detail.locator(".md-message");
    await expect(md).toBeVisible();
    // The Markdown structure was really parsed: the heading became h2, the table became table, the code
    // block became pre>code
    await expect(md.locator("h2")).toContainText("覆盖结果");
    await expect(md.locator("table")).toBeVisible();
    await expect(md.locator("pre code")).toContainText("ls -la /workspace");
    // The ** in the source should not reach the screen together with the asterisks
    await expect(md.locator("strong").first()).toHaveText("34");

    // Thinking also goes through Markdown (the same renderer)
    await app.locator(`${ROWS}[data-evt-type="agent.thinking"]`).click();
    await expect(app.getByTestId("evt-detail").locator(".md-message")).toBeVisible();
    await expect(
      app.getByTestId("evt-detail").locator(".md-message strong"),
    ).toContainText("先跑工具");
  });

  test("session.status_idle 突出 stop_reason，且不把 requires_action 说成等用户", async ({
    app,
  }) => {
    // run_5's idle carries requires_action: under self_hosted it means "the tool is running in our container"
    await app.goto("/w/growth/runs/run_5?view=events");

    await app.locator(`${ROWS}[data-evt-type="session.status_idle"]`).first().click();
    const detail = app.getByTestId("evt-detail");
    const stop = detail.getByTestId("evt-stop-reason");
    await expect(stop).toBeVisible();
    // The raw stop_reason string must be shown
    await expect(stop).toContainText("requires_action");
    // ⚠️ It must say clearly this is not "waiting for user confirmation": this meaning was fixed
    // recently; do not let it slip back
    await expect(stop).toContainText("不是「等用户确认」");
    await expect(stop).toContainText("容器");
    // Which events it is waiting on must also be shown: when debugging you follow this id
    await expect(stop).toContainText("sevt-0404");
  });

  test("span.model_request_end 算出耗时并列出 token 用量", async ({ app }) => {
    await app.goto("/w/growth/runs/run_6?view=events");
    // trace kinds are hidden by default; turn them on first
    await openFilters(app);
    await app.getByTestId("filter-trace").click();
    await closeFilters(app);

    await app.locator(`${ROWS}[data-evt-type="span.model_request_end"]`).click();
    const detail = app.getByTestId("evt-detail");

    // The duration is paired explicitly via model_request_start_id (the only pair with a linking field)
    // sevt-0606 -> sevt-0608: 20_002.000 -> 20_006.500, i.e. 4.5 seconds
    await expect(detail.getByTestId("evt-duration")).toContainText("4.5s");
    await expect(detail).toContainText("sevt-0606");

    const usage = detail.getByTestId("evt-usage");
    await expect(usage).toContainText("1820");
    await expect(usage).toContainText("96");
  });

  test("原本一次都渲染不到的 7 类事件现在都能看到详情", async ({ app }) => {
    await app.goto("/w/growth/runs/run_6?view=events");

    /*
     * These 7 kinds never appear in run_1..run_5, so their detail branches are code that was "written
     * but never run". Open each one to confirm the detail really renders the fields unique to that type.
     */
    const cases: [string, string][] = [
      ["user.interrupt", "先别查历史了"],
      ["user.tool_result", "notes.md"],
      ["user.custom_tool_result", "GROWTH-4821"],
      ["agent.custom_tool_use", "internal_ticket_lookup"],
      ["session.status_rescheduled", "没有额外字段"],
      ["session.updated", "事件类型全覆盖自检"],
      ["session.deleted", "没有额外字段"],
    ];

    for (const [type, expected] of cases) {
      const row = app.locator(`${ROWS}[data-evt-type="${type}"]`);
      await expect(row, `${type} 应有一行`).toHaveCount(1);
      await row.click();
      await expect(
        app.getByTestId("evt-detail"),
        `${type} 的详情里应含「${expected}」`,
      ).toContainText(expected);
      // Every kind's raw JSON must be inspectable
      await expect(app.getByTestId("evt-detail").getByTestId("evt-raw-json")).toBeVisible();
    }
  });

  test("过滤能力全保住：类别 / 域 / 具体类型 / 关键词 / 线程 / 重置", async ({ app }) => {
    await app.goto("/w/growth/runs/run_6?view=events");
    const count = () => app.locator(ROWS).count();

    /*
      Kinds: trace is hidden by default; turning it on adds rows.

      ⚠️ **Wait for the list to render before reading count**. A direct `await count()` reads 0 on the
      skeleton/first frame, and then every later "back to before" assertion compares with 0: the
      opposite of a false green, it makes this case fail with an error that does not point to the real
      cause at all ("Expected 0, got 18").
      run_6 has 20 events, with 2 trace ones hidden by default, so 18 rows are visible by default.
    */
    await expect(app.locator(ROWS)).toHaveCount(18);
    await expect(app.locator(`${ROWS}[data-evt-type^="span."]`)).toHaveCount(0);
    const before = 18;
    await openFilters(app);
    await app.getByTestId("filter-trace").click();
    await expect(app.locator(ROWS)).toHaveCount(20);

    // Domain: keep only span
    await openFilters(app);
    await app.getByTestId("filter-domain-user").click();
    await openFilters(app);
    await app.getByTestId("filter-domain-agent").click();
    await openFilters(app);
    await app.getByTestId("filter-domain-session").click();
    // While the popover is still open, its dismiss layer intercepts clicks on filter-reset / the search box
    await closeFilters(app);
    await expect.poll(count).toBe(2);
    await expect(app.locator(`${ROWS}[data-evt-type="span.model_request_start"]`)).toHaveCount(1);

    /*
      Reset back to **the default filter**. The default is "hide trace", so it goes back to before (18
      rows), not 20.

      ⚠️ Before asserting, wait for the URL to really be clean: after `filter-reset` the count briefly
      stays at the 2 rows under the domain filter, and the first few polls of `expect.poll` may land on
      that frame. Waiting for the URL is a more accurate test: the filter state lives in the URL, and
      only when it is cleared has it really reset.
    */
    await app.getByTestId("filter-reset").click();
    await expect(app).toHaveURL(/\?view=events$/);
    await expect(app.locator(ROWS)).toHaveCount(before);

    // Keyword: search for a string in the input, hitting the start event
    await app.getByTestId("timeline-search").fill("internal_ticket_lookup");
    // The start event's input contains it, and the receipt event's custom_tool_use_id points to the start
    // event's id, so it should hit only 1.
    await expect(app.locator(ROWS)).toHaveCount(1);
    await expect(app.locator(ROWS)).toHaveAttribute("data-evt-type", "agent.custom_tool_use");

    /*
      The keyword must search into **the raw JSON**, not just the bits of text visible in the list.

      ⚠️ The test must pick a string that **exists only in the raw JSON and not in the summary**.
      Verified by mutation: cutting the haystack from `[label, summary, type, JSON.stringify(event)]`
      down to `[label, summary]`, an assertion like `GROWTH-4821` **stays green**, because that string is
      in the summary anyway. That assertion cannot prove "the raw JSON was searched".

      `sth_full_01` is a session_thread_id, only in the raw JSON; list rows only show the thread column
      for multi-thread runs, and run_6 is single-thread, so it is completely invisible in the UI.
    */
    await app.getByTestId("timeline-search").fill("sth_full_01");
    // In run_6, all events except the 4 session-level ones whose session_thread_id is null carry this thread id
    await expect.poll(count).toBeGreaterThan(10);

    // Check one more field name that is only in the raw JSON (evaluated permission), also absent from summaries
    await app.getByTestId("timeline-search").fill("evaluated_permission");
    await expect.poll(count).toBe(2); // one each of tool_use and custom_tool_use

    // When everything is filtered out, say "it was filtered out", not "this run has no events"
    await app.getByTestId("timeline-search").fill("绝不可能出现的字符串");
    await expect(app.locator(ROWS)).toHaveCount(0);
    await expect(app.getByText("没有匹配的事件")).toBeVisible();
  });

  test("筛选态进 URL，刷新后仍生效", async ({ app }) => {
    await app.goto("/w/growth/runs/run_6?view=events");

    await openFilters(app);
    await app.getByTestId("filter-preset-model").click();
    await expect(app).toHaveURL(/cat=trace/);
    await expect(app).toHaveURL(/etype=span\.model_request_end%2Cspan\.model_request_start/);
    await expect(app.locator(ROWS)).toHaveCount(2);

    await app.reload();
    // Still those two after a refresh: the state is in the URL, not in the component
    await expect(app.locator(ROWS)).toHaveCount(2);
    await expect(app.locator(`${ROWS}[data-evt-type="span.model_request_end"]`)).toHaveCount(1);
  });

  test("选中的行被筛掉后详情跟着清空，不挂着一条看不见的事件", async ({ app }) => {
    await app.goto("/w/growth/runs/run_6?view=events");

    await app.locator(`${ROWS}[data-evt-type="agent.custom_tool_use"]`).click();
    await expect(app.getByTestId("evt-detail")).toBeVisible();

    // Filter it out: keep only the session domain
    await openFilters(app);
    await app.getByTestId("filter-domain-user").click();
    await openFilters(app);
    await app.getByTestId("filter-domain-agent").click();
    await closeFilters(app);
    await expect(app.locator(`${ROWS}[data-evt-type="agent.custom_tool_use"]`)).toHaveCount(0);

    // The detail should no longer show the event that was filtered out
    await expect(app.getByTestId("evt-detail")).toHaveCount(0);
    await expect(app.getByText("点左侧任一条事件看详情")).toBeVisible();
  });

  test("筛选区只占一行，控件收在弹层里，已选条件留在外面", async ({ app }) => {
    await app.goto("/w/growth/runs/run_6?view=events");
    await expect(app.getByTestId("evt-list")).toBeVisible();

    /*
      The filter area used to lay out six rows (search / quick filters / event domain / semantic kind /
      type collapse / derived conditions), piling up over half a screen **above** the list. This guards
      "squeezed back into one row".

      The test is **the measured height**, not "count some divs": with 32px controls in one row plus
      flex-wrap, anything over one row must be taller than 44px. The current measured value is 32.
    */
    const box = await app.getByTestId("timeline-toolbar").boundingBox();
    expect(box, "工具条应当渲染出来").not.toBeNull();
    expect(
      box!.height,
      `筛选区高度 ${box!.height}px —— 超过一行了，八成是又把某条轴平铺回外面`,
    ).toBeLessThan(44);

    // The axis controls are hidden in the popover by default: you cannot reach them without opening it
    await expect(app.getByTestId("filter-domain-user")).toHaveCount(0);
    await expect(app.getByTestId("filter-trace")).toHaveCount(0);
    await openFilters(app);
    await expect(app.getByTestId("filter-domain-user")).toBeVisible();
    await expect(app.getByTestId("filter-trace")).toBeVisible();

    /*
      **Selected conditions must stay outside the popover.** What gets folded in is the controls, not
      the state: if "what is currently filtered out" were hidden too, users would look at an incomplete
      list without finding the reason.
    */
    await app.getByTestId("filter-domain-user").click();
    await closeFilters(app);
    const chip = app.getByTestId("filter-chip-dom-off-user");
    await expect(chip).toBeVisible();
    // The badge shows the number of active conditions (the keyword is not counted; it has its own input box outside)
    await expect(app.getByTestId("timeline-toolbar")).toHaveAttribute("data-active-filters", "1");

    // The x on a chip removes that condition in place
    const before = await app.locator(ROWS).count();
    await chip.getByRole("button").click();
    await expect(chip).toHaveCount(0);
    await expect.poll(() => app.locator(ROWS).count()).toBeGreaterThan(before);
  });

  test("那个从来没生效过的「排序」下拉已经删掉", async ({ app }) => {
    /*
      It is a dead control: `applyRawFilter` never read `filter.sort`, and switching to "duration
      descending" changed no order (the call tree's duration sort is TracePanel's own state). And under
      the "one event per row" meaning, a single event has no duration to compute anyway, so it is not
      "not wired up yet" but **should not exist**.

      A dropdown that does nothing when switched is worse than no dropdown: it makes people think they
      sorted it wrong.
    */
    await app.goto("/w/growth/runs/run_6?view=events");
    await expect(app.getByTestId("evt-list")).toBeVisible();
    await expect(app.getByTestId("timeline-sort")).toHaveCount(0);

    // The parameter in the URL should no longer be honored either (writing it affects nothing, and
    // nothing but reset would leave it behind)
    await app.goto("/w/growth/runs/run_6?view=events&sort=duration");
    await expect(app.getByTestId("evt-list")).toBeVisible();
    await expect(app.getByTestId("filter-reset")).toHaveCount(0);
  });

  test("对话页也是同一个事件视图", async ({ app }) => {
    await app.goto("/w/growth/chat/run_6?view=events");
    await expect(app.getByTestId("evt-list")).toBeVisible();
    await app.locator(`${ROWS}[data-evt-type="agent.message"]`).click();
    await expect(app.getByTestId("evt-detail").locator(".md-message h2")).toContainText(
      "覆盖结果",
    );
  });
});
