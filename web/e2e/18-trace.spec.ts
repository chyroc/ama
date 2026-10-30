import { expect, test } from "./fixtures";

/**
 * The Trace panel (call tree / span tree).
 *
 * The panel renders under **its own view** (`?view=trace`, the third tab next to chat / events), so
 * always `goto` an address with `?view=trace` directly; do not click the view switch, which waits for
 * one more route tick and is not deterministic (same as other timeline-asserting cases in the suite).
 *
 * ⚠️ **It used to hang at the bottom of the event view** (back then it needed `?view=trace`). It was
 * promoted to its own view because long runs have hundreds of events, and seeing the call tree meant
 * scrolling past the whole timeline first; and the two are read in different ways anyway.
 *
 * # What this set watches is "the structure matches the official component"
 *
 * The panel went through two changes, and these assertions keep it from going back to either shape:
 *
 * 1. **at first** it was "a flat list sorted by duration descending" + a deep link to an external
 *    observability platform;
 * 2. **in between** it became a tree, but with a self-invented **run -> turn -> span three-level**
 *    hierarchy, completely different from the structure of the official `CozeloopTraceDetail`;
 * 3. **now** it follows the official `SpanNode`: **tree nodes are spans**, the root is the whole run,
 *    model requests and tool calls hang under it (two levels), and clicking a node shows details below.
 *
 * So there are these kinds of assertions: **the hierarchy is a span tree** (not a flat list, and no
 * "turn"), and **node rows are built like the official ones** (color block abbreviation / type /
 * duration / tokens).
 *
 * The seed's run_1 has 2 model requests + several tool calls, the only run that covers both kinds of span.
 */
test.describe("Trace 面板", () => {
  test("是与对话/事件并列的第三个 tab，不再挂在事件视图里", async ({ app }) => {
    // All three views are on the switch.
    await app.goto("/w/growth/runs/run_1");
    await expect(app.getByTestId("view-conversation")).toBeVisible();
    await expect(app.getByTestId("view-events")).toBeVisible();
    await expect(app.getByTestId("view-trace")).toBeVisible();

    // **The default view is not the call tree** (the default is still chat); the panel should not appear.
    await expect(app.getByTestId("trace-panel")).toHaveCount(0);

    // **The event view should not have the panel anymore either**: this is the point of this change: it
    // used to hang at the bottom of the event view, and long runs had to scroll past hundreds of events to
    // see it. Asserting only "it is in the trace view" is not enough: "rendered in both places" would still
    // be green, and that would show the same panel twice on the page.
    await app.goto("/w/growth/runs/run_1?view=events");
    await expect(app.getByTestId("evt-list")).toBeVisible();
    await expect(app.getByTestId("trace-panel")).toHaveCount(0);

    // Clicking the third view switches over, and the URL gets ?view=trace (links can be shared / opened directly).
    await app.getByTestId("view-trace").click();
    await expect(app).toHaveURL(/\?view=trace/);
    await expect(app.getByTestId("trace-panel")).toBeVisible();
    // After switching, the event timeline should no longer be there (the three views are exclusive, not stacked).
    await expect(app.getByTestId("evt-list")).toHaveCount(0);
  });

  test("对话页也是三档，调用树独立一档", async ({ app }) => {
    await app.goto("/w/growth/chat/run_1");
    await expect(app.getByTestId("view-trace")).toBeVisible();
    await expect(app.getByTestId("trace-panel")).toHaveCount(0);

    await app.goto("/w/growth/chat/run_1?view=events");
    await expect(app.getByTestId("trace-panel")).toHaveCount(0);

    await app.goto("/w/growth/chat/run_1?view=trace");
    await expect(app.getByTestId("trace-panel")).toBeVisible();
  });

  test("是 span 树：root=运行，模型请求与工具调用挂在它下面", async ({ app }) => {
    await app.goto("/w/growth/runs/run_1?view=trace");
    await expect(app.getByTestId("trace-panel")).toBeVisible();
    await expect(app.getByTestId("trace-tree")).toBeVisible();

    // **Exactly one root** (an official tree has only one root span).
    await expect(app.getByTestId("trace-node-agent")).toHaveCount(1);
    await expect(app.getByTestId("trace-node-agent")).toHaveAttribute("data-depth", "0");

    // Both kinds of child spans are there, both at depth=1: **this is the core assertion of "following
    // the official two-level structure"**.
    const models = app.getByTestId("trace-node-model");
    const tools = app.getByTestId("trace-node-tool");
    await expect(models.first()).toBeVisible();
    await expect(tools.first()).toBeVisible();
    await expect(models.first()).toHaveAttribute("data-depth", "1");
    await expect(tools.first()).toHaveAttribute("data-depth", "1");

    // **There should be no "turn" level anymore.** In the old self-invented run->turn->span three levels,
    // the turn's testid was trace-node-turn, and spans were at depth=2. Both must be locked in: asserting
    // only that the testid is absent, "keep the turn under a new name" would still be green.
    await expect(app.getByTestId("trace-node-turn")).toHaveCount(0);
    await expect(app.getByTestId("trace-tree").locator('[data-depth="2"]')).toHaveCount(0);

    // Flat lists must be ruled out too: a root exists => not a flat list (a flat list has no agent node).
    // The toHaveCount(1) above already covers this.
  });

  test("节点行的构成照官方：色块缩写 + 名称 + 类型 + 耗时", async ({ app }) => {
    await app.goto("/w/growth/runs/run_1?view=trace");
    await expect(app.getByTestId("trace-tree")).toBeVisible();

    // The official CustomTreeNode's identifying symbol is "color block + two-letter abbreviation" (Ag / Mo / tool).
    // The root is agent type -> Ag.
    const rootRow = app.getByTestId("trace-node-agent");
    await expect(rootRow).toContainText("Ag");
    // Model span -> Mo.
    await expect(app.getByTestId("trace-node-model").first()).toContainText("Mo");

    // The type Tag (that cell in the official one).
    await expect(app.getByTestId("trace-node-model").first()).toContainText("model");
    await expect(app.getByTestId("trace-node-tool").first()).toContainText("tool");

    // The name is clickable (the official onSelect is on the node).
    await expect(rootRow.getByTestId("trace-span-name")).toBeVisible();
  });

  test("点某个 span 看它的详情，字段照官方 useSpanOverviewField", async ({ app }) => {
    await app.goto("/w/growth/runs/run_1?view=trace");
    await expect(app.getByTestId("trace-tree")).toBeVisible();

    // The root is selected by default (the default behavior of the official defaultSelectedSpanID).
    const detail = app.getByTestId("trace-span-detail");
    await expect(detail).toBeVisible();

    // Click a tool span; the detail must switch along, and show input/output.
    const tool = app.getByTestId("trace-node-tool").first();
    await tool.getByTestId("trace-span-name").click();

    // These cells are always in the official field grid: status / type / duration / Span ID.
    await expect(detail).toContainText("状态");
    await expect(detail).toContainText("类型");
    await expect(detail).toContainText("耗时");
    await expect(detail).toContainText("Span ID");

    // The two big areas of the official right-hand detail: Input / Output. Tool calls have both.
    await expect(app.getByTestId("trace-output")).toBeVisible();
  });

  test("时间轴：所有行共用一根轴，条的位置=开始时刻、宽度=时长", async ({ app }) => {
    // This is what a "proportion bar" cannot answer. Each row's bar used to be "this row's duration /
    // the slowest one", which can only answer "which is longer", **not "when it happened, and what it
    // overlapped with"**.
    await app.goto("/w/growth/runs/run_1?view=trace");
    await expect(app.getByTestId("trace-tree")).toBeVisible();

    // The axis ticks are there (three: 0 / midpoint / total length).
    await expect(app.getByTestId("trace-axis")).toBeVisible();

    const bars = app.getByTestId("trace-bar");
    expect(await bars.count()).toBeGreaterThan(3);

    // The root covers the whole axis (it is the window itself).
    const rootBar = app.getByTestId("trace-node-agent").getByTestId("trace-bar");
    await expect(rootBar).toHaveAttribute("data-offset", "0.0000");
    await expect(rootBar).toHaveAttribute("data-width", "1.0000");

    // **Bars must be staggered by time**; they cannot all start at 0 (that would go back to proportion bars).
    const offsets = await bars.evaluateAll((els) =>
      els.map((e) => Number((e as HTMLElement).dataset.offset)),
    );
    const distinct = new Set(offsets.map((o) => o.toFixed(3)));
    expect(distinct.size, "各条的起点应当不同——都从 0 开始说明画的是占比条而不是时间轴")
      .toBeGreaterThan(3);
    // **Start points are non-decreasing**: the row order must match the bars' horizontal positions.
    //
    // ⚠️ This caught a real bug: rows used to be sorted by `item.sequence`, and the item of a model span
    // is its `_end` event, so it sorted by the **end** sequence while drawing the **start** time. Measured in production:
    //     seq 31 model_start(13.1s) -> seq 33 tool_use(17.0s) -> seq 35 model_end
    // the model was sorted after the tool, while its bar was left of the tool: on the page it showed up
    // as "the bar of the row above is further right", which looks like the timeline math is wrong, while
    // what was actually wrong was the row order.
    for (let i = 1; i < offsets.length; i++) {
      expect(
        offsets[i],
        `第 ${i} 行的条比上一行更靠左（${offsets[i]} < ${offsets[i - 1]}）——` +
          "行序与条的位置不一致，多半是按 sequence 而不是按 started_at 排的",
      ).toBeGreaterThanOrEqual(offsets[i - 1] - 0.0001);
    }
  });

  test("模型请求被工具打断时，行序仍按开始时刻", async ({ app }) => {
    // In the seed's run_1, model requests and tool calls interleave (the model starts first, and a tool
    // is called inside its interval). This case watches the shape "model and tool mixed in the same level"
    // on its own: the previous case asserts global monotonicity, but if the tree happened to contain only
    // one type, it would not cover interleaving.
    await app.goto("/w/growth/runs/run_1?view=trace");
    await expect(app.getByTestId("trace-tree")).toBeVisible();

    const rows = await app
      .getByTestId("trace-tree")
      .locator('li[data-depth="1"]')
      .evaluateAll((els) =>
        els.map((el) => ({
          kind: el.getAttribute("data-testid")?.replace("trace-node-", "") ?? "",
          off: Number(
            el.querySelector<HTMLElement>('[data-testid="trace-bar"]')?.dataset.offset ?? "NaN",
          ),
        })),
      );

    // Confirm this level really has both kinds (otherwise this case spins idle).
    const kinds = new Set(rows.map((r) => r.kind));
    expect(kinds.has("model") && kinds.has("tool"), "这一层应当 model 与 tool 都有").toBe(true);

    const withBars = rows.filter((r) => Number.isFinite(r.off));
    for (let i = 1; i < withBars.length; i++) {
      expect(withBars[i].off).toBeGreaterThanOrEqual(withBars[i - 1].off - 0.0001);
    }
  });

  test("并发的两个工具：同一起点、不同长度，且都标出并发", async ({ app }) => {
    // The seed's run_1 has a pair of bash calls **started at the same moment** (toolu_par_01/02),
    // one finishing at 8s, the other at 11s: real production data has exactly this shape.
    //
    // This case is the **core** of this change: duration numbers alone do not show concurrency; the two
    // [start, end] intervals must overlap to tell them apart.
    await app.goto("/w/growth/runs/run_1?view=trace");
    await expect(app.getByTestId("trace-tree")).toBeVisible();

    const marks = app.getByTestId("trace-concurrent");
    expect(await marks.count(), "应当有节点被标成并发").toBeGreaterThan(0);

    // Take those two bash calls and compare their bars.
    // ⚠️ Use `:has-text`, not `:text-is`: the name button wraps another span (which turns red on failure),
    // and `:text-is` requires the text to be a **direct** child, matching 0.
    const bashRows = app
      .getByTestId("trace-tree")
      .locator('li:has([data-testid="trace-span-name"])')
      .filter({ hasText: "调用 bash" });
    expect(await bashRows.count()).toBe(2);

    const a = await bashRows.nth(0).getByTestId("trace-bar").evaluate((e) => ({
      off: Number((e as HTMLElement).dataset.offset),
      w: Number((e as HTMLElement).dataset.width),
    }));
    const b = await bashRows.nth(1).getByTestId("trace-bar").evaluate((e) => ({
      off: Number((e as HTMLElement).dataset.offset),
      w: Number((e as HTMLElement).dataset.width),
    }));

    // They start at the same moment.
    expect(Math.abs(a.off - b.off), "两条并发调用应当在同一起点").toBeLessThan(0.005);
    // **Different lengths**: otherwise this case would be green for the wrong implementation "all bars equally long".
    expect(Math.abs(a.w - b.w)).toBeGreaterThan(0.01);
    // The intervals really overlap.
    expect(Math.min(a.off + a.w, b.off + b.w)).toBeGreaterThan(Math.max(a.off, b.off));
  });

  test("串行的调用不该被标成并发", async ({ app }) => {
    // Paired with the previous one: asserting only "there is a concurrency marker", "mark every node as
    // concurrent" would still be green.
    await app.goto("/w/growth/runs/run_1?view=trace");
    await expect(app.getByTestId("trace-tree")).toBeVisible();

    const total = await app.getByTestId("trace-node-tool").count();
    const marked = await app.getByTestId("trace-concurrent").count();
    expect(marked, "不该每个节点都被标成并发").toBeLessThan(total);
    // **The first** read is clearly serial ([10,11], no overlap before or after).
    //
    // ⚠️ Use `.first()`; do not assert `toHaveCount(1)`: the seed now has **two** reads; the one added
    // later (toolu_mid_01) is deliberately placed inside a model request interval, to reproduce the "row
    // order by sequence is wrong" bug. With a hard-coded count, adding a same-named tool to the seed would
    // turn this case red for reasons unrelated to what it checks.
    const readRow = app
      .getByTestId("trace-tree")
      .locator('li:has([data-testid="trace-span-name"])')
      .filter({ hasText: "调用 read" })
      .first();
    await expect(readRow.getByTestId("trace-concurrent")).toHaveCount(0);

    // **Touching endpoints do not count as concurrency.** In the seed mcp_02 is [43,45] and mcp_03 is
    // [45,49], end touching start: that is one after the other. The check must use strict inequality;
    // written as `<=`, these two would be marked concurrent, and that mistake is invisible anywhere else.
    //
    // Count the total marked nodes: only the truly concurrent pair of bash + the web_search overlapping
    // them. If the two touching mcp calls got mixed in, this number would grow.
    const markedNames = await app
      .getByTestId("trace-tree")
      .locator('li:has([data-testid="trace-concurrent"]) [data-testid="trace-span-name"]')
      .allTextContents();
    for (const n of markedNames) {
      expect(n, "端点相接的 MCP 调用不该被标成并发").not.toContain("run_query");
    }
  });

  test("详情里给出开始与结束时刻，且带毫秒", async ({ app }) => {
    // The Gantt bars give intuition; the detail gives comparable numbers. **Milliseconds cannot be
    // dropped**: two concurrent calls may differ by only tens of milliseconds (one pair measured in
    // production differed by 16 microseconds), and second precision would show them as the same moment.
    await app.goto("/w/growth/runs/run_1?view=trace");
    await expect(app.getByTestId("trace-tree")).toBeVisible();

    await app.getByTestId("trace-node-tool").first().getByTestId("trace-span-name").click();
    const detail = app.getByTestId("trace-span-detail");
    await expect(detail).toContainText("开始时刻");
    await expect(detail).toContainText("结束时刻");
    // Shaped like …:ss.mmm
    await expect(detail).toContainText(/\d{2}:\d{2}:\d{2}\.\d{3}/);
  });

  test("模型请求也要有耗时，不能整片显示「未知」", async ({ app }) => {
    // ⚠️ This watches a real trap: `buildTimeline` **only computes durationMs for tool calls**; model
    // requests are two separate entries, start / end, with `durationMs` always undefined. Using the end
    // entry directly as the node would make every model request on the tree show "unknown", and the whole
    // point of the call tree is to look at durations, so half the nodes would be useless.
    // So traceTree pairs start<->end in order itself and computes the difference.
    await app.goto("/w/growth/runs/run_1?view=trace");
    await expect(app.getByTestId("trace-tree")).toBeVisible();

    const modelNodes = app.getByTestId("trace-node-model");
    const n = await modelNodes.count();
    expect(n).toBeGreaterThan(0);
    // Every model request node must have a known duration; not a single "unknown" allowed.
    await expect(modelNodes.getByTestId("trace-duration")).toHaveCount(n);
    await expect(modelNodes.getByTestId("trace-duration-unknown")).toHaveCount(0);
  });

  test("模型 span 显示 token 用量", async ({ app }) => {
    // The official node row shows that tag when tokens exist and are non-zero, and the type is model.
    await app.goto("/w/growth/runs/run_1?view=trace");
    await expect(app.getByTestId("trace-tree")).toBeVisible();

    await expect(app.getByTestId("trace-tokens").first()).toBeVisible();
  });

  test("折叠 root 会藏掉子 span，但 root 自己还在", async ({ app }) => {
    await app.goto("/w/growth/runs/run_1?view=trace");
    await expect(app.getByTestId("trace-tree")).toBeVisible();

    const before = await app.getByTestId("trace-tree").locator("li").count();
    expect(before).toBeGreaterThan(1);

    // Collapse the root. All descendants disappear, the root itself stays; otherwise there would be
    // nothing to click to expand.
    await app.getByTestId("trace-toggle").first().click();
    await expect.poll(async () => app.getByTestId("trace-tree").locator("li").count()).toBe(1);
    await expect(app.getByTestId("trace-node-agent")).toHaveCount(1);

    // Clicking again must expand it back.
    await app.getByTestId("trace-toggle").first().click();
    await expect.poll(async () => app.getByTestId("trace-tree").locator("li").count()).toBe(before);
  });

  test("按耗时排序只重排层内，不打平层级", async ({ app }) => {
    await app.goto("/w/growth/runs/run_1?view=trace");
    await expect(app.getByTestId("trace-tree")).toBeVisible();

    await app.getByTestId("trace-sort-duration").click();

    // **The hierarchy must still be there.** The old implementation was "a flat list sorted by duration
    // descending"; if switching to sort-by-duration flattened the tree, the root would be gone, and this goes red.
    await expect(app.getByTestId("trace-node-agent")).toHaveCount(1);
    await expect(app.getByTestId("trace-node-agent")).toHaveAttribute("data-depth", "0");
    await expect(app.getByTestId("trace-node-model").first()).toHaveAttribute("data-depth", "1");

    // Descending within a level: take all known durations at depth=1 and compare in DOM order.
    const texts = await app
      .getByTestId("trace-tree")
      .locator('[data-depth="1"] [data-testid="trace-duration"]')
      .allTextContents();
    const ms = texts.map((s) => {
      const n = Number.parseFloat(s);
      if (s.includes("ms")) return n;
      if (s.includes("m ")) {
        const [mm, ss] = s.split("m ");
        return Number.parseFloat(mm) * 60_000 + Number.parseFloat(ss) * 1000;
      }
      return n * 1000;
    });
    for (let i = 1; i < ms.length; i++) {
      expect(ms[i - 1]).toBeGreaterThanOrEqual(ms[i]);
    }
  });

  test("耗时算不出来时说「未知」，不能当 0", async ({ app }) => {
    // **It must not be treated as 0**: treated as 0, it would bubble up to the "fastest" end in duration
    // sorting, looking like a call that finished instantly, while the truth is it has not come back yet.
    //
    // **run_4 must be used**, not run_1. run_4 has one `span.model_request_start` **with no matching
    // `_end`** (the request is still in progress), the only real shape of "cannot be computed" on the
    // tree. In run_1 the model requests have both start/end and every tool call has a receipt, so after
    // pairing there is not a single unknown node: asserting on it would always fail, with a failure
    // message (element not found) that does not point at all to "the wrong fixture was chosen".
    await app.goto("/w/growth/runs/run_4?view=trace");
    await expect(app.getByTestId("trace-tree")).toBeVisible();

    await expect(app.getByTestId("trace-duration-unknown").first()).toBeVisible();
    // "Unknown" must not be shown as 0ms / 0.0s.
    const texts = await app.getByTestId("trace-duration-unknown").allTextContents();
    for (const s of texts) {
      expect(s).not.toMatch(/^0(\.0)?(ms|s)$/);
    }
  });

  test("对话页的调用树档里也有调用树", async ({ app }) => {
    // The chat page and the run detail page have different scroll models (the former is fixed height with
    // inner scroll, the latter scrolls at page level), and the panel must render properly in both: exactly
    // what "the component does not bring its own scroll shell" guarantees.
    await app.goto("/w/growth/chat/run_1?view=trace");
    await expect(app.getByTestId("trace-panel")).toBeVisible();
    await expect(app.getByTestId("trace-tree")).toBeVisible();
    await expect(app.getByTestId("trace-node-agent")).toHaveCount(1);
  });

  test("未配对的工具调用照样占一个 span，耗时显示未知", async ({ app }) => {
    // run_3 was terminated by an MCP auth failure: there is only one agent.mcp_tool_use, **with no receipt**.
    //
    // So it checks: the call tree **does not swallow** such an unclosed call (that is exactly the "which
    // step it got stuck on" users most want to see), but it does not make up a duration for it either.
    await app.goto("/w/growth/runs/run_3?view=trace");
    await expect(app.getByTestId("trace-panel")).toBeVisible();

    await expect(app.getByTestId("trace-node-tool")).toHaveCount(1);
    await expect(
      app.getByTestId("trace-node-tool").getByTestId("trace-duration-unknown"),
    ).toHaveCount(1);
    // With nodes, the empty state should not show.
    await expect(app.getByTestId("trace-empty")).toHaveCount(0);
  });

  /*
    The two below are **a pair** and must both be there. There is only one test:
    **only a final `session.error` counts as this session failing; a single child span's `is_error` does not.**
    ("Final" means excluding `retry_status: retrying`: rate limiting reschedules automatically and keeps running.)

    Keeping only the first, the wrong implementation "any is_error means overall failure" is still all
    green; keeping only the second, the wrong implementation "root always succeeds" is still all green.
    Both directions must be locked in.
  */

  test("有 is_error 的子 span，但没有终局错误时 root 仍是成功", async ({ app }) => {
    /*
      ⚠️ **This one was corrected**; it used to assert exactly the opposite ("when a child span fails,
      the root is also marked failed").

      The reason for the change was a real session (checked record by record in the database): of 17
      tool calls **1** had `is_error: true` (a Python TypeError), **0** `session.error`, the last idle's
      `stop_reason` was `end_turn`, and `errorSummary` was empty. That is, the agent read the error, fixed
      the code, and finished the job: **that is success**. But the trace page showed it as "failed",
      while the same run was a success on the list page (the list's test is `errorSummary`). **The same run
      with two conclusions on two pages: one of them must be lying.**

      The consequence was not "occasionally mislabeling one": agent tool calls erroring is an extremely
      common, normal thing, so **the vast majority of successful sessions were marked failed**, and the
      status became completely untrustworthy.

      **run_1 happens to lock this in**: it has one MCP receipt with `is_error: true` (the only one in
      seed.ts), while it has `status: "idle"` and no `errorSummary`. The `session.error` inside it is a
      rate-limit error with `retry_status: retrying`: it ended normally after automatic retry, also not a
      session failure.
    */
    await app.goto("/w/growth/runs/run_1?view=trace");
    await expect(app.getByTestId("trace-tree")).toBeVisible();

    // The panel selects the root by default, so this status in the detail is the root's.
    const detail = app.getByTestId("trace-span-detail");
    await expect(detail).toContainText("成功");
    await expect(detail).not.toContainText("失败");

    /*
      ⚠️ **But the child span that errored must still be marked red itself.** This half must be asserted
      too: "deleting the error marker entirely" is a very natural over-correction when fixing the root
      status, and then "which step errored" could no longer be seen, which is exactly the reason to open
      this panel.
    */
    const failedNode = app.getByTestId("trace-node-tool").filter({ has: app.locator(".text-danger") });
    await expect(failedNode).toHaveCount(1);
  });

  test("有 session.error 且重试耗尽时 root 标成失败", async ({ app }) => {
    // run_3: MCP authentication failed, retries ran out and it was terminated; `errorSummary` is not empty.
    // This is a real session failure, and the root must show it; otherwise collapsing means hiding errors.
    await app.goto("/w/growth/runs/run_3?view=trace");
    await expect(app.getByTestId("trace-tree")).toBeVisible();

    const detail = app.getByTestId("trace-span-detail");
    await expect(detail).toContainText("失败");
  });

  test("一个 span 都没有时显示空态", async ({ app }) => {
    // run_5 is stuck at requires_action (a tool call waiting for user confirmation); first confirm the panel
    // finished rendering, then judge which branch it took: **do not just assert toHaveCount(0)**, which is
    // satisfied during the skeleton stage.
    await app.goto("/w/growth/runs/run_5?view=trace");
    await expect(app.getByTestId("trace-panel")).toBeVisible();

    const hasTree = await app.getByTestId("trace-tree").count();
    if (hasTree === 0) {
      await expect(app.getByTestId("trace-empty")).toBeVisible();
    } else {
      // Having nodes is reasonable (run_5 has a call waiting for confirmation), so the empty state should not show.
      await expect(app.getByTestId("trace-empty")).toHaveCount(0);
      await expect(app.getByTestId("trace-node-agent")).toHaveCount(1);
    }
  });
});
