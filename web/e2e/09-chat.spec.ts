/**
 * The chat page.
 *
 * A chat is a Run with trigger manual, so this tests both the chat interaction itself and its link
 * with run history: the run a chat creates must be visible in /runs, and in reverse you can get back
 * to the chat from the URL.
 *
 * The word-by-word intermediate state is asserted deliberately by "not flushing", and the final result
 * is settled with flushConversation(): waiting for animations with sleep is the form most likely to
 * become a flaky failure.
 */

import {
  clearArkMock,
  expect,
  flushConversation,
  holdThinkingMock,
  releaseStageMock,
  resetMyRoleMock,
  setMyRoleMock,
  test,
} from "./fixtures";

test("空态引导选 agent，发消息后逐字出结果", async ({ app }) => {
  await app.goto("/w/growth/chat");

  // With no session yet, give guidance instead of a blank
  await expect(app.getByRole("heading", { name: "选一个 agent 开始" })).toBeVisible();

  // The agent picker already has one selected by default, saving a click
  await expect(app.getByTestId("chat-agent-select")).toBeVisible();

  await app.getByTestId("chat-input").fill("帮我看下这周的异常");
  await app.getByTestId("chat-send").click();

  // The user message shows up right away, without waiting for the stream to come back
  await expect(app.getByTestId("chat-user-message")).toContainText("帮我看下这周的异常");

  // The URL switches to the form with runId: refresh can continue, links can be shared
  await expect(app).toHaveURL(/\/w\/growth\/chat\/run_/);

  // Intermediate state: while it is still running, "Interrupt" is visible (deliberately not flushed here)
  await expect(app.getByTestId("chat-interrupt")).toBeVisible();
  // This turn has not ended yet, so the tool's execution steps are laid out
  await expect(app.getByTestId("chat-tool-row")).toBeVisible();

  // Push to the end, then assert the final result
  await flushConversation(app);
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();
  // The mock replay ends with session.status_idle, so this turn has ended: the steps are folded by
  // design, and the tools only show after opening the summary row
  await expect(app.getByTestId("chat-tool-row")).not.toBeVisible();
  await app.getByTestId("turn-process-trigger").first().click();
  await expect(app.getByTestId("chat-tool-row")).toBeVisible();
  // After finishing, back to the sendable state
  await expect(app.getByTestId("chat-send")).toBeVisible();
});

test("工具调用可展开看入参与结果", async ({ app }) => {
  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("查一下 TODO");
  await app.getByTestId("chat-send").click();
  await flushConversation(app);

  // This turn has ended; first expand the folded intermediate steps
  await app.getByTestId("turn-process-trigger").first().click();

  const tool = app.getByTestId("chat-tool-row");
  await expect(tool).toBeVisible();
  await tool.click();
  // After expanding, the body of the tool result is visible
  await expect(app.getByText(/HttpApiClient/).first()).toBeVisible();
});

test("思考过程折叠成一行，点开能看到思维链正文", async ({ app }) => {
  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("查一下 TODO");
  await app.getByTestId("chat-send").click();
  await flushConversation(app);

  // This turn has ended; first expand the folded intermediate steps
  await app.getByTestId("turn-process-trigger").first().click();

  const thinking = app.getByTestId("chat-thinking-row").first();
  // **Collapsed by default**: the fold bar is visible but the body is not; this is exactly the
  // requirement's "no need to expand by default".
  //
  // The body assertion must be scoped to the thinking fold area, not a global text search: agent replies
  // also render with .md-message, so a global search hits 2 and reports a strict mode violation, and the
  // cause of that failure is a selector that is too wide, not a broken feature.
  const body = app
    .locator('.disclosure:has([data-testid="chat-thinking-row"])')
    .locator(".md-message");

  await expect(thinking).toBeVisible();
  await expect(thinking).toContainText("已深度思考");
  await expect(body).toBeHidden();
  // The folded state has **only the title**, no body excerpt: for a short thought the excerpt is the
  // whole text, and showing it would repeat it once more on opening.
  await expect(thinking).not.toContainText("先确认用户想问的范围");

  // The full thinking only shows after opening
  await thinking.click();
  await expect(body).toBeVisible();
  await expect(body).toContainText("先确认用户想问的范围");
});

test("流式阶段思考过程实时可见，且不会被当成正式回复", async ({ app }) => {
  /*
   * ⚠️ **The replay must be held at the thinking stage; do not rely on "running fast enough" to hit
   * that intermediate state.**
   *
   * The word-by-word thinking panel naturally lives only ~300ms (4 chunks x 70ms), while Playwright's
   * `expect` retries on a backoff grid (about 0 / 100 / 350 / 850ms...). Measured, this window falls at
   * 465->764ms after the click, **right between two ticks**, so the assertion failed 5/5 consistently.
   * The feature was fine all along (sampling with MutationObserver shows the panel at 736x32 with the
   * "thinking" text rendered normally); no sample ever landed in the window. See holdThinkingMock in
   * fixtures for the synthetic comparison data.
   *
   * So this is now "hold -> assert -> release", making the observation independent of timing.
   */
  await app.goto("/w/growth/chat");
  /*
   * ⚠️ **It must be set after `goto`**: this marker is a module-level variable, and `goto` reloads the
   * page, rebuilding the whole JS module with it, so setting it before navigation is **silently lost**
   * (measured: set before navigation -> still MISSED; set after -> reliably visible at 783ms).
   * This differs from how holdTurnMock is used: those cases set it after the page has already stopped.
   */
  await holdThinkingMock(app);
  try {
    await app.getByTestId("chat-input").fill("边想边答");
    await app.getByTestId("chat-send").click();

    // Word-by-word stage (deliberately not flushed): the thinking panel appears, with a shimmering "thinking".
    const draftPanel = app.getByTestId("chat-thinking-draft");
    await expect(draftPanel).toBeVisible();
    // The body really is coming in word by word (what is held is the moment "deltas finished, final event
    // not landed", so it is the full text). Without this, an implementation that only renders an empty
    // panel shell would also pass.
    await expect(draftPanel).toContainText("先确认用户想问的范围");

    // **Key regression**: thinking deltas must not be accumulated into the agent reply bubble.
    // This was a real bug: event_delta frames for thinking and message have exactly the same shape, and
    // sharing one draft drew the chain of thought as the formal answer, which then vanished entirely when
    // the final event arrived.
    //
    // ⚠️ **Do not write `expect(chat-draft).not.toContainText(...)`**: while held at the thinking stage
    // the reply draft **does not exist yet at all**, and Playwright doing `not.toContainText` on a
    // missing element fails outright (reporting `element(s) not found`): that is a selector problem, not
    // a feature problem, and it judges "no reply bubble", **the most correct state**, as wrong.
    //
    // Instead use "the number of reply bubbles containing this text is 0": both absent and present
    // without this text pass, while "thinking drawn into the reply bubble" is still caught. This is not
    // loosening: both forms prove the same thing, and only this one expresses it right when the element
    // is absent.
    //
    // The `toHaveCount(0)` here does not fear the "satisfied in the frame before rendering finishes" trap:
    // the two assertions above already waited for the thinking panel to be visible with the full body,
    // and **the replay is being held**, so the state will not move forward by itself.
    await expect(
      app.getByTestId("chat-draft").filter({ hasText: "先确认用户想问的范围" }),
    ).toHaveCount(0);

    // Only after releasing does this turn continue; flush pushes it to the end.
    await releaseStageMock(app);
    await flushConversation(app);
    // Once settled, events take over rendering and the word-by-word panel leaves
    await expect(draftPanel).not.toBeVisible();
  } finally {
    // ⚠️ A module-level switch that `reset()` cannot wipe: missing this line would make every later case
    // that sends a message stop at the thinking stage, with failure messages that do not point here at all.
    // finally is used because any failing assertion above would jump out.
    await releaseStageMock(app);
  }
});

test("对话页也能切到事件视图", async ({ app }) => {
  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("看下事件");
  await app.getByTestId("chat-send").click();
  await flushConversation(app);
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();

  await app.getByTestId("view-events").click();
  await expect(app.getByTestId("evt-list")).toBeVisible();
  await expect(app.locator('[data-testid="evt-row-tool"]').first()).toBeVisible();
  // Both views keep the input area; switching over should not stop you from continuing to send
  await expect(app.getByTestId("chat-input")).toBeVisible();

  await app.getByTestId("view-conversation").click();
  await expect(app.getByTestId("evt-list")).toHaveCount(0);
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();
});

test("刷新后对话内容还原（走的是补历史那条路）", async ({ app }) => {
  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("记住这句话");
  await app.getByTestId("chat-send").click();
  await flushConversation(app);
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();

  const url = app.url();
  await app.goto(url);

  // The subscription does not replay history; seeing the content means the listRunEvents step filled it in
  await expect(app.getByTestId("chat-user-message")).toContainText("记住这句话");
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();
});

test("左栏列出历史对话并可切换", async ({ app }) => {
  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("第一次对话");
  await app.getByTestId("chat-send").click();
  await flushConversation(app);
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();
  const firstUrl = app.url();

  // Start a second chat
  await app.getByTestId("chat-new").click();
  await expect(app).toHaveURL("/w/growth/chat");
  await app.getByTestId("chat-input").fill("第二次对话");
  await app.getByTestId("chat-send").click();
  await flushConversation(app);
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();

  // Both are in the left column, with titles taken from the first message
  const list = app.getByTestId("chat-list");
  await expect(list.getByText("第一次对话")).toBeVisible();
  await expect(list.getByText("第二次对话")).toBeVisible();

  // Switch back to the first one
  await list.getByText("第一次对话").click();
  await expect(app).toHaveURL(firstUrl);
  await expect(app.getByTestId("chat-user-message")).toContainText("第一次对话");
});

test("中断正在跑的对话", async ({ app }) => {
  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("跑个长任务");
  await app.getByTestId("chat-send").click();

  // No flush; interrupt while it is still running
  await app.getByTestId("chat-interrupt").click();

  // After interrupting, back to the sendable state.
  //
  // ⚠️ **These two are no longer sync points.** The "Send" button is now **always** present (you can
  // send during streaming too, which queues; see 27-send-queue.spec.ts), so the first assertion is
  // satisfied instantly; only the "Interrupt" button disappearing really means streaming has dropped.
  // Earlier this relied on "Send replacing Interrupt" to incidentally wait for the events to come back;
  // with both buttons present that implicit wait is gone, so looking for "user interrupted" below would
  // run before the events arrive: measured 6/6 failures, with the error pointing in the wrong direction,
  // "LIFECYCLE_CHURN filtered it out".
  await expect(app.getByTestId("chat-send")).toBeVisible();
  await expect(app.getByTestId("chat-interrupt")).toHaveCount(0);

  // **The "user interrupted" entry must stay in the chat.**
  //
  // Its category is status, the same as the self_hosted lifecycle churn like "session started running",
  // but it is **an action the user really took**: cutting by category when reducing noise would make
  // "I interrupted this turn" vanish, and the user would only see the answer cut off halfway with no
  // explanation. So ConversationView's LIFECYCLE_CHURN is an allowlist by type, and this case guards
  // that boundary.
  //
  for (const trigger of await app.getByTestId("turn-process-trigger").all()) {
    await trigger.click();
  }
  await expect(app.getByText("用户中断").first()).toBeVisible();
  // Lifecycle churn within the same turn still should not appear
  await expect(app.getByText("会话开始运行")).toHaveCount(0);
});

test("对话产生的运行出现在运行历史里，触发来源是手动", async ({ app }) => {
  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("GMV 这周怎么样");
  await app.getByTestId("chat-send").click();
  await flushConversation(app);
  // Wait until this turn really finishes before moving on; otherwise the left column title fill-in and
  // the page jump race, and the assertion sees an empty title
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();
  await expect(app.getByTestId("chat-list").getByText("GMV 这周怎么样")).toBeVisible();

  await app.goto("/w/growth/runs");
  const list = app.getByTestId("run-list");
  await expect(list.getByText("GMV 这周怎么样")).toBeVisible();
  /*
    The chat just now should make the number of runs **one more than the seed**.

    ⚠️ **Do not hard-code the total.** The number of runs in the seed keeps changing (run_6 is the
    sample covering all event types, run_7 / run_8 are samples for chat list status display, all added
    recently one after another); a hard-coded literal would turn this case red every time someone adds
    a seed entry, with the error "Expected 7, Received 9" not pointing to the real cause at all: this
    has already happened.

    First counting "the seed runs whose trigger is not this chat" and then asserting +1 is too
    roundabout; a more direct test is used here: the list contains this chat, and the total is one more
    than "without it".
  */
  const rows = app.locator('[data-testid^="run-row-"]');
  const total = await rows.count();
  const seeded = await rows.filter({ hasNotText: "GMV 这周怎么样" }).count();
  expect(total, "对话产生的运行应当让总数比种子多一条").toBe(seeded + 1);
});

test("后端历史里同一个事件存了多行时，界面只显示一条", async ({ app }) => {
  // This guards "the frontend must deduplicate repeated events itself", not "the backend never pushes duplicates".
  //
  // The backend does not dedupe by event id when storing, and one Ark session may have several
  // subscribers at once (Feishu inbound, Feishu resume, browser SSE, scheduler); each subscriber writes
  // its own row, so the same event id is stored N times in history: different sequence, identical bytes
  // otherwise. Without frontend dedup, one sentence is drawn N times, and React's duplicate key warning
  // fires because the timeline's React key comes from the event id.
  //
  // This case takes **the first-screen path** (no local events yet when entering the page), because
  // mergeEvents in useConversationStream used to short-circuit and return the other side when "one side
  // is empty", so un-deduplicated history went straight through to rendering.
  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("这句回复不许出现两遍");
  await app.getByTestId("chat-send").click();
  await flushConversation(app);
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();

  const runId = app.url().split("/chat/")[1];
  expect(runId).toBeTruthy();

  // Put one more agent.message row with **the same id and a different sequence** into the mock store,
  // simulating those N backend rows. It goes through the private store + commit: the mock has no public
  // API to "append a raw event", and what is built here is exactly a stored shape the normal flow cannot
  // produce. Only after commit does it land in localStorage, so a refresh can read it.
  const rows = await app.evaluate((id: string) => {
    const client = window.__amaApi as unknown as {
      store: { events: Record<string, { id: string; type: string; sequence: number }[]> };
      commit: () => void;
    };
    const list = client.store.events[id];
    const at = list.findIndex((e) => e.type === "agent.message");
    if (at < 0) throw new Error("回放没落下 agent.message，注入前提不成立");
    const reply = list[at];
    // Copy a row and insert it after the original, shifting the sequence of later events along: that is
    // the backend's shape: each subscriber takes its own sequence number and writes a row, so the two
    // duplicate rows have different sequences (say 7 and 8), identical bytes otherwise.
    const seq = reply.sequence;
    for (const e of list) if (e.sequence > seq) e.sequence += 1;
    list.splice(at + 1, 0, { ...reply, sequence: seq + 1 });
    client.commit();
    return list.filter((e) => e.id === reply.id).length;
  }, runId);
  // First confirm the duplicate really was built: otherwise, if the injection ever stopped working,
  // the assertion below would pass for nothing "because there is no duplicate".
  expect(rows).toBe(2);

  await app.goto(`/w/growth/chat/${runId}`);

  // Two rows in history, only one entry in the UI
  await expect(app.getByTestId("chat-agent-message")).toHaveCount(1);
  await expect(app.getByTestId("chat-user-message")).toHaveCount(1);
});

test("从 Agent 详情页可以直接试一下", async ({ app }) => {
  await app.goto("/w/growth/agents");
  await app.locator('[data-testid^="agent-card-"]').first().click();
  await app.getByTestId("agent-try").click();

  // Landing on the chat page with ?agent=, the picker is already on that agent
  await expect(app).toHaveURL(/\/w\/growth\/chat\?agent=/);
  await expect(app.getByTestId("chat-input")).toBeEnabled();
});

test("空间里没有 agent 时引导先建 agent", async ({ app }) => {
  await app.goto("/w/growth/chat");
  // Delete all agents, then enter the chat page
  await app.evaluate(async () => {
    const api = window.__amaApi as unknown as {
      listAgents: (id: string) => Promise<{ id: string }[]>;
      listWorkspaces: () => Promise<{ id: string; key: string }[]>;
      deleteAgent: (id: string) => Promise<void>;
    };
    const ws = (await api.listWorkspaces()).find((w) => w.key === "growth")!;
    for (const a of await api.listAgents(ws.id)) await api.deleteAgent(a.id);
  });

  await app.goto("/w/growth/chat");
  await expect(app.getByRole("heading", { name: "这个空间还没有 agent" })).toBeVisible();
  await expect(app.getByRole("button", { name: "创建 Agent" })).toBeVisible();
});

test("关联 agent 被删除后会话冻结，但还看得见归属谁", async ({ app }) => {
  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("先聊一句好留下这条会话");
  await app.getByTestId("chat-send").click();
  await flushConversation(app);
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();

  const runId = app.url().split("/chat/")[1];
  expect(runId).toBeTruthy();

  // **Delete only the agent this session uses**; do not delete all of them like the case above: with
  // no agents left in the workspace, the chat page takes the "this workspace has no agent yet" empty
  // branch and never reaches the frozen path.
  await app.evaluate(async (id: string) => {
    const api = window.__amaApi as unknown as {
      getRun: (id: string) => Promise<{ agentId: string }>;
      deleteAgent: (id: string) => Promise<void>;
    };
    const run = await api.getRun(id);
    await api.deleteAgent(run.agentId);
  }, runId);

  await app.goto(`/w/growth/chat/${runId}`);

  // 1. Frozen: the input box does not accept input
  await expect(app.getByTestId("chat-input")).toBeDisabled();
  // 2. You can still see which agent it was (the name is a snapshot from session creation; it remains
  // after the agent is deleted)
  await expect(app.getByText(/由「工单分诊助手」应答/)).toBeVisible();
  // 3. Clearly marked "deleted"
  await expect(app.getByTestId("agent-deleted-chip").first()).toBeVisible();
  // 4. Say clearly why it cannot send and what can still be done
  await expect(app.getByTestId("chat-frozen")).toContainText("会话已冻结");
  // 5. The agent picker must not be blank: when selectedKey points to a deleted agent the default render is empty;
  //    this guards that fallback (without asserting it would silently regress, showing up on the page as
  //    "this session belongs to nobody")
  await expect(app.getByTestId("chat-agent-select")).toContainText("工单分诊助手");
  // The history messages are still readable
  await expect(app.getByTestId("chat-user-message")).toContainText("先聊一句好留下这条会话");

  // 6. Run history marks it too. It must be scoped by runId: deleting agent_triage also turns the seed's
  //    run_2 / run_5 red, and without scoping it hits Playwright's strict mode.
  await app.goto("/w/growth/runs");
  await expect(
    app.getByTestId(`run-row-${runId}`).getByTestId("agent-deleted-chip"),
  ).toBeVisible();

  // 7. "View Agent" in the run detail would always 404, so it should not be given
  await app.goto(`/w/growth/runs/${runId}`);
  await expect(app.getByTestId("agent-deleted-chip").first()).toBeVisible();
  await expect(app.getByRole("button", { name: "查看 Agent" })).toHaveCount(0);
});

test("关联 agent 被停用后会话冻结，且提示的是「去启用」而不是「去新建」", async ({
  app,
}) => {
  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("先聊一句好留下这条会话");
  await app.getByTestId("chat-send").click();
  await flushConversation(app);
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();

  const runId = app.url().split("/chat/")[1];
  expect(runId).toBeTruthy();

  // Only disable the agent this session uses. Disabling goes through updateAgent, the real user path
  // (the dropdown on the detail page), not a back door.
  await app.evaluate(async (id: string) => {
    const api = window.__amaApi as unknown as {
      getRun: (id: string) => Promise<{ agentId: string }>;
      updateAgent: (id: string, patch: { status: string }) => Promise<unknown>;
    };
    const run = await api.getRun(id);
    await api.updateAgent(run.agentId, { status: "disabled" });
  }, runId);

  await app.goto(`/w/growth/chat/${runId}`);

  // 1. Frozen: messages cannot be sent.
  await expect(app.getByTestId("chat-input")).toBeDisabled();
  // 2. History is readable as usual: "can view but not send" is the core of this design; disabling is temporary.
  await expect(app.getByTestId("chat-user-message")).toContainText(
    "先聊一句好留下这条会话",
  );
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();

  // 3. **It must say "disabled", not "deleted".** This is the real purpose of this case:
  //    with shared text, users would be guided to "create a new Agent", while what they really should do
  //    is change the status back, ending up with an extra agent while the original's skills and Feishu
  //    bindings remain.
  const frozen = app.getByTestId("chat-frozen");
  await expect(frozen).toContainText("已停用");
  await expect(frozen).toContainText("已启用");
  await expect(frozen).not.toContainText("已被删除");

  // 4. Disabled is not deleted: the "deleted" marker should not appear.
  await expect(app.getByTestId("agent-deleted-chip")).toHaveCount(0);

  // 5. Re-enable it, and the session is usable again right away. **Disabling being reversible is its
  //    biggest difference from deleting**; any implementation that caches or stores agentDisabled turns
  //    this step red.
  await app.evaluate(async (id: string) => {
    const api = window.__amaApi as unknown as {
      getRun: (id: string) => Promise<{ agentId: string }>;
      updateAgent: (id: string, patch: { status: string }) => Promise<unknown>;
    };
    const run = await api.getRun(id);
    await api.updateAgent(run.agentId, { status: "active" });
  }, runId);

  await app.goto(`/w/growth/chat/${runId}`);
  await expect(app.getByTestId("chat-input")).toBeEnabled();
  await expect(app.getByTestId("chat-frozen")).toHaveCount(0);
});

test("停用的 agent 在选择器里可见但不可选", async ({ app }) => {
  await app.goto("/w/growth/chat");

  // agent_draft (the weekly report generator) in the seed is disabled, exactly the sample for this case.
  await app.getByTestId("chat-agent-select").click();
  const listbox = app.getByRole("listbox");

  // 1. **Stays in the list**: hiding it would make people think the agent was deleted and go create another one.
  const disabledItem = listbox.getByRole("option", { name: /周报生成器/ });
  await expect(disabledItem).toBeVisible();
  // 2. The reason it is grayed out must be written out, or users only see "cannot click".
  await expect(disabledItem).toContainText("已停用");
  // 3. Really not selectable.
  await expect(disabledItem).toHaveAttribute("aria-disabled", "true");
  // 4. Enabled ones are selectable as usual: guards against the collateral damage of "disable every item".
  await expect(
    listbox.getByRole("option", { name: /工单分诊助手/ }),
  ).not.toHaveAttribute("aria-disabled", "true");
});

/**
 * The **run status** of each row in the session list.
 *
 * # Why this group of cases exists on its own
 *
 * The most important status is **failure**, and failure **is not a RunStatus**: a session that errored
 * also has `status` terminated, and the test is a non-empty `errorSummary` (the same rule as
 * UsageSummary / OverviewPage). So the wrong implementation "render by status only" makes the list
 * look completely normal: a session that crashed calmly shows "ended", and users think it finished.
 * The `errorSummary` case below locks this in.
 *
 * The samples come from the seed's run_7 (manual + terminated + errorSummary) and run_8
 * (feishu + rescheduling). **They were added for this group**: before that, the only run with an
 * errorSummary was run_3, triggered by `api`, while the chat left column only includes manual/feishu
 * (CONVERSATION_TRIGGERS in ChatPage), so the failure state could never render in this list.
 */
test("会话列表每行显示运行状态，失败与已结束区分得开", async ({ app }) => {
  await app.goto("/w/growth/chat");

  const list = app.getByTestId("chat-list");
  await expect(list).toBeVisible();

  /*
    ⚠️ Locate the **row** first, then get the status; do not search globally by text.

    Words like "ended" and "failed" also appear elsewhere on the page (run detail, filters), and the
    list may well have several rows with the same status: a global getByText fails with a strict mode
    violation, an error that says "found N elements", not pointing to "your selector is too wide".
  */
  const failedRow = list.locator('[data-testid="chat-row-run_7"]');
  const status = failedRow.getByTestId("run-status");

  // **This one is the focus of the whole group**: run_7's status is terminated, but it has an errorSummary.
  // An implementation rendering by status only would show "ended" here.
  await expect(status).toHaveText("失败");
  await expect(status).not.toHaveText("已结束");
  // The semantic color must be danger: right text with the wrong color still does not let you spot
  // the failed one when scanning a column.
  await expect(status).toHaveClass(/chip--danger/);

  // Ones that are also terminated but **without** errorSummary must still be the neutral "ended".
  // Without this contrast, an implementation that "marks every terminated as failed" would also make
  // everything above green.
  const endedRow = list.locator('[data-testid="chat-row-run_6"]');
  await expect(endedRow.getByTestId("run-status")).toHaveText("已结束");
  await expect(endedRow.getByTestId("run-status")).not.toHaveClass(/chip--danger/);
});

test("五种运行状态在会话列表里各自渲染正确的文案与语义色", async ({ app }) => {
  await app.goto("/w/growth/chat");
  const list = app.getByTestId("chat-list");
  await expect(list).toBeVisible();

  /*
    Lock in, row by row, "which run -> which text -> which color class".

    Asserting the color too is required: checking only the text, changing the whole STATUS_COLOR table
    to one color (say all default) would still be all green, and that is exactly the failure shape
    "cannot tell them apart at a glance".

    - running / requires_action are both accent: under self_hosted requires_action means "the tool is
      running in our container", the same kind as running, **not** waiting for user confirmation.
    - rescheduling uses warning (waiting for the next dispatch; worth noticing but not an error).
    - terminated is neutral default; only failure is danger.
  */
  const cases: Array<[string, string, RegExp]> = [
    ["chat-row-run_4", "运行中", /chip--accent/],
    ["chat-row-run_5", "工具执行中", /chip--accent/],
    ["chat-row-run_1", "空闲", /chip--success/],
    ["chat-row-run_8", "调度中", /chip--warning/],
    ["chat-row-run_6", "已结束", /chip--default/],
  ];

  for (const [rowId, label, colorClass] of cases) {
    const chip = list.locator(`[data-testid="${rowId}"]`).getByTestId("run-status");
    await expect(chip, `${rowId} 的状态文案`).toHaveText(label);
    await expect(chip, `${rowId} 的语义色`).toHaveClass(colorClass);
  }
});

test("状态与来源两个标记共存，不互相顶掉", async ({ app }) => {
  await app.goto("/w/growth/chat");
  const list = app.getByTestId("chat-list");

  // run_8 came from Feishu and is being rescheduled: both markers must be present, on separate rows.
  //
  // This guards against "adding status squeezed out the trigger chip": that regression causes no error,
  // the list just loses one kind of information, and the lost kind is exactly "where this session came from".
  const row = list.locator('[data-testid="chat-row-run_8"]');
  await expect(row.getByTestId("run-status")).toHaveText("调度中");
  await expect(row.getByTestId("row-trigger-feishu")).toBeVisible();

  // The status is on the upper row (the title row), the source on the lower row: the y coordinate
  // directly proves they are not squeezed together.
  const statusBox = (await row.getByTestId("run-status").boundingBox())!;
  const triggerBox = (await row.getByTestId("row-trigger-feishu").boundingBox())!;
  expect(statusBox.y).toBeLessThan(triggerBox.y);
});

/**
 * When the agent is deleted, the row **only says "deleted"**, with no "idle" beside it.
 *
 * # This is a zero-error defect, and its shape is worth remembering
 *
 * The status chip is on the title row and "deleted" is on the second row; two ternaries each decide on
 * their own, unaware of each other. The "deleted wins" priority in the second row **only applies
 * between agentDeleted and agentDisabled**, and does not cover the title row at all, so the cross-row
 * combination "deleted + idle" slipped through between the two checks. Each chip renders correctly on
 * its own, so there was no error at all; only a human could see the two contradicted each other:
 * "it is gone, but it is idle".
 *
 * Disabled follows the same rule (see frozen in ConversationRow): both stop the session from sending
 * messages, and "disabled + idle" is just as awkward.
 */
test("agent 已删除的会话只显示「已删除」，不再并排状态标签", async ({ app }) => {
  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("先聊一句好留下这条会话");
  await app.getByTestId("chat-send").click();
  await flushConversation(app);
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();

  const runId = app.url().split("/chat/")[1];
  expect(runId).toBeTruthy();

  // Delete only the agent this session uses (deleting all takes the "no agents in the workspace"
  // empty state, which never reaches this path).
  await app.evaluate(async (id: string) => {
    const api = window.__amaApi as unknown as {
      getRun: (id: string) => Promise<{ agentId: string }>;
      deleteAgent: (id: string) => Promise<void>;
    };
    const run = await api.getRun(id);
    await api.deleteAgent(run.agentId);
  }, runId);

  await app.goto(`/w/growth/chat/${runId}`);

  const row = app.getByTestId("chat-list").locator(`[data-testid="chat-row-${runId}"]`);

  /*
    ⚠️ **First wait for "deleted" to really appear, then assert the status label is absent.**

    `toHaveCount(0)` is satisfied in the frame where this row **has not rendered yet**; this repo has
    fallen into it twice (recorded in docs/guides/frontend.md). In that case, even if the priority did
    not work at all, this case would be green: it just ran faster than rendering.
    So the test must be "on the same row, the positive thing is already there and the negative thing is not".
  */
  await expect(row.getByTestId("agent-deleted-chip")).toBeVisible();
  await expect(row.getByTestId("run-status")).toHaveCount(0);

  // Other rows are not affected: guards against the over-fix of "wiping out all status labels".
  // The agent of run_1 in the seed (the data analyst) was not deleted, so its row's status label must still be there.
  const intact = app.getByTestId("chat-list").locator('[data-testid="chat-row-run_1"]');
  await expect(intact.getByTestId("run-status")).toBeVisible();
});

test("agent 被停用的会话同样只显示「已停用」，不并排状态标签", async ({ app }) => {
  await app.goto("/w/growth/chat");
  await app.getByTestId("chat-input").fill("先聊一句好留下这条会话");
  await app.getByTestId("chat-send").click();
  await flushConversation(app);
  await expect(app.getByTestId("chat-agent-message")).toBeVisible();

  const runId = app.url().split("/chat/")[1];

  await app.evaluate(async (id: string) => {
    const api = window.__amaApi as unknown as {
      getRun: (id: string) => Promise<{ agentId: string }>;
      updateAgent: (id: string, patch: { status: string }) => Promise<unknown>;
    };
    const run = await api.getRun(id);
    await api.updateAgent(run.agentId, { status: "disabled" });
  }, runId);

  await app.goto(`/w/growth/chat/${runId}`);

  const row = app.getByTestId("chat-list").locator(`[data-testid="chat-row-${runId}"]`);
  // Same as above: first wait for the positive marker, then assert the status label is absent.
  await expect(row.getByTestId("agent-disabled-chip")).toBeVisible();
  await expect(row.getByTestId("run-status")).toHaveCount(0);
});

// -- **Up-front** guidance when no Ark API Key is configured -------------------------------------
//
// Previously the chat page knew nothing about credentials: the input box rendered as usual, and users
// typed a whole message and clicked send, only to be bounced back by a backend 422 ("please configure
// the Ark API Key in workspace settings first"). A whole message written for nothing, which then had
// to be retyped. Many such reports came from different workspaces, all with the same shape: "the first
// message in a new workspace hits it once", so it must be made clear **before typing**.
//
// ⚠️ This group must clearArkMock first: the seed **configures a key by default** (see
// seedArkCredentials in data/api.ts); otherwise these cases never reach the guidance branch, and would
// quietly run all green on the "configured" path, so breaking it would go unnoticed.

test("没配方舟 Key 时输入框就是禁用的，且给出直达空间设置的入口", async ({ app }) => {
  await clearArkMock(app);
  await app.goto("/w/growth/chat");

  // The guidance bar comes first (it is fetched asynchronously, so wait for the positive signal before
  // asserting disabled, to avoid running on the loading frame).
  await expect(app.getByTestId("chat-no-ark")).toBeVisible();
  await expect(app.getByTestId("chat-no-ark")).toContainText("还没有配置方舟 API Key");

  // **The core behavior of this change**: do not let people type a whole message for nothing.
  await expect(app.getByTestId("chat-input")).toBeDisabled();

  // The owner can configure the key, so a really clickable entry is given, and clicking it must really
  // reach the Ark tab of workspace settings.
  await app.getByTestId("chat-no-ark-action").click();
  await expect(app).toHaveURL(/\/w\/growth\/settings\?tab=ark$/);
});

test("配好 key 之后输入框恢复可用，引导条消失", async ({ app }) => {
  await clearArkMock(app);
  await app.goto("/w/growth/chat");
  await expect(app.getByTestId("chat-input")).toBeDisabled();

  // Go configure the key (32 characters, even -> the mock judges it usable).
  await app.goto("/w/growth/settings?tab=ark");
  await app.getByTestId("ark-setup").click();
  await app.getByTestId("ark-source-select").click();
  await app.getByRole("option", { name: "手动填写" }).click();
  await app.getByTestId("ark-key-input").fill("ARK00000000000000000000000000001");
  await app.getByTestId("modal-submit").click();
  await expect(app.getByTestId("ark-masked")).toContainText("*");

  // Back to the chat page: the guidance bar is gone and the input box accepts typing.
  //
  // ⚠️ This one is the "it does let you through" counterpart. Asserting only "disabled when not
  // configured", an always-true `isDisabled` (say, mistakenly written as `isDisabled={true}`) would be all
  // green, and that would block the whole chat feature. So one case must verify it really lets you through.
  await app.goto("/w/growth/chat");
  await expect(app.getByTestId("chat-no-ark")).toHaveCount(0);
  await expect(app.getByTestId("chat-input")).toBeEnabled();
});

test("引用的全局 key 已不存在：引导指向全局设置，不是空间设置", async ({ app }) => {
  await clearArkMock(app);

  // Build the scenario: first configure a global key, make the workspace reference it, then delete the global one.
  await app.goto("/preferences?tab=ark");
  await app.getByTestId("global-ark-setup").click();
  await app.getByTestId("ark-source-select").click();
  await app.getByRole("option", { name: "手动填写" }).click();
  await app.getByTestId("global-ark-key-input").fill("ARK00000000000000000000000000001");
  await app.getByTestId("global-ark-modal").getByTestId("modal-submit").click();
  await expect(app.getByTestId("global-ark-masked")).toContainText("*");

  await app.goto("/w/growth/settings?tab=ark");
  await app.getByTestId("ark-use-global-empty").click();
  await expect(app.getByTestId("ark-source-inherit")).toBeVisible();

  await app.goto("/preferences?tab=ark");
  // ⚠️ "Clear" only opens a confirmation (it spans several workspaces and is irreversible); you must click
  // confirm once more to really delete. Missing the second click silently deletes nothing, and then the
  // guidance bar never appears, with a failure message that only says "waiting for chat-no-ark", not
  // pointing at all to "it was never deleted".
  await app.getByTestId("global-ark-clear").click();
  await app.getByTestId("confirm-danger").click();

  await app.goto("/w/growth/chat");
  await expect(app.getByTestId("chat-no-ark")).toBeVisible();
  // **The two kinds of "none" must get different messages**: this one must say "the referenced key is
  // gone", not "not configured yet": the latter would send users to workspace settings to paste a new
  // one, while what they really should do is most likely configure the global one again.
  await expect(app.getByTestId("chat-no-ark")).toContainText("已不存在");
  await expect(app.getByTestId("chat-input")).toBeDisabled();

  // The entry points to **global** settings, and must carry ?from=: without it the sidebar disappears entirely.
  await app.getByTestId("chat-no-ark-action").click();
  await expect(app).toHaveURL(/\/preferences\?tab=ark&from=growth$/);
});

test("没权限的人看到的是「联系管理员」，而不是一个点了会 403 的按钮", async ({ app }) => {
  await clearArkMock(app);
  // Configuring the key is the owner/admin level (PUT .../ark-credential declares ActionManage).
  // A viewer clicking "go to workspace settings" would still get 403, which is worse than giving no button.
  await setMyRoleMock(app, "viewer");
  try {
    await app.goto("/w/growth/chat");

    await expect(app.getByTestId("chat-no-ark")).toBeVisible();
    await expect(app.getByTestId("chat-no-ark")).toContainText("请联系空间的管理员或拥有者");
    // The input box is disabled as usual: they really cannot send messages; only whom to ask differs.
    await expect(app.getByTestId("chat-input")).toBeDisabled();
    // **Key point**: do not give an entry that can never be used.
    await expect(app.getByTestId("chat-no-ark-action")).toHaveCount(0);
  } finally {
    // The marker lives in a separate localStorage key that reset() cannot wipe; missing it once would
    // make the whole later batch of cases think they are not admins.
    await resetMyRoleMock(app);
  }
});

test("member 也没有配 key 的权限，同样只给「联系管理员」", async ({ app }) => {
  await clearArkMock(app);
  // A member can change agents and send messages, but **configuring credentials is the manage level**,
  // which they do not have. This is not a duplicate of the viewer case: member is "can write but cannot
  // manage", exactly the level most easily written as "can write, so give the button" (the classic
  // mistake of mixing up useCanWrite and useCanManage).
  await setMyRoleMock(app, "member");
  try {
    await app.goto("/w/growth/chat");
    await expect(app.getByTestId("chat-no-ark")).toBeVisible();
    await expect(app.getByTestId("chat-no-ark")).toContainText("请联系空间的管理员或拥有者");
    await expect(app.getByTestId("chat-no-ark-action")).toHaveCount(0);
  } finally {
    await resetMyRoleMock(app);
  }
});

test("agent 自己配了一把 key：空间没配也照样能聊（三级回落，不能误拦）", async ({ app }) => {
  await clearArkMock(app);

  // Configure it only for the agent, leaving both workspace and account levels empty.
  await app.goto("/w/growth/agents/agent_analyst");
  await app.getByTestId("agent-ark-setup").click();
  await app.getByTestId("ark-source-select").click();
  await app.getByRole("option", { name: "手动填写" }).click();
  await app.getByTestId("ark-key-input").fill("ARKAGENT0000000000000000000000AA");
  await app.getByTestId("modal-submit").click();
  await expect(app.getByTestId("agent-ark-source-agent")).toBeVisible();

  /*
   * ⚠️ This locks in "the check must go through the three-level fallback".
   *
   * Checking only `getArkCredential(workspaceId)` would judge this agent "unusable", so the input box
   * would be disabled, while it can actually run (the backend resolveArkSecretForAgent prefers the
   * agent's row). That is a zero-error misjudgment: the server lets it through as usual, only the UI
   * blocks it, and users have no idea what to do.
   */
  await app.goto("/w/growth/chat?agent=agent_analyst");
  await expect(app.getByTestId("chat-no-ark")).toHaveCount(0);
  await expect(app.getByTestId("chat-input")).toBeEnabled();
});
