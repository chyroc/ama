/**
 * End-to-end main path: walk through the platform's core requirements from scratch.
 *
 * A single case, checking that the modules also work when put together: the files before this one
 * test each module in detail; this one is integration.
 */

import { builtinSkillCount, clearAll, completeFeishuAuth, expect, nav, test } from "./fixtures";

test("从零创建空间 → 配技能 → 接飞书 → 建 agent 并绑定 → 查运行历史 → 改设置", async ({
  page,
}) => {
  // 0. Start with no workspaces: the list page is there, the list is just empty
  await clearAll(page);
  await page.goto("/workspaces");
  await expect(page.getByRole("heading", { name: "还没有可进入的空间" })).toBeVisible();

  await page.getByTestId("new-workspace").click();
  await page.getByTestId("ws-name").fill("端到端验证");
  await page.getByTestId("ws-key").fill("e2e-demo");
  await page.getByTestId("ws-submit").click();
  await expect(page).toHaveURL(/\/w\/e2e-demo$/);

  // The new workspace is empty, and the overview page gives a clear next step
  await expect(page.getByRole("heading", { name: "从创建第一个 Agent 开始" })).toBeVisible();

  // 1. Configure a workspace skill
  await nav(page, "技能库").click();
  // The new workspace's skill library is **not empty**: the platform's built-in skills (lark-cli / ama)
  // are visible in every workspace, so there is no longer a "no skills yet" empty state here. Asserting
  // a built-in skill is there also checks "a newly created workspace gets the built-in skills too",
  // which is why they exist.
  // ⚠️ Only assert one that **certainly exists**; do not count the total: the count changes as built-in
  // skills are added or removed (see the summary-skills part below), and pinning the count would plant
  // the same kind of mine again.
  await expect(page.getByTestId("skill-card-lark-cli")).toBeVisible();
  await page.getByRole("button", { name: "新建技能" }).first().click();
  await page.getByTestId("skill-title").fill("发票核验");
  await page.getByTestId("skill-name").fill("invoice-check");
  await page.getByTestId("skill-desc").fill("当需要核验发票信息、判断报销合规性时使用。");
  await page.getByTestId("skill-instructions").fill("# 发票核验\n\n1. 校验税号\n2. 核对金额");
  await page.getByTestId("modal-submit").click();
  await expect(page.getByTestId("skill-card-invoice-check")).toBeVisible();

  // 2. Connect a Feishu app (device code flow: no credentials typed in by hand)
  await nav(page, "飞书").click();
  await page.getByRole("button", { name: "接入应用" }).first().click();
  const popup = page.waitForEvent("popup");
  await page.getByTestId("begin-connect").click();
  await (await popup).close();
  await expect(page.getByTestId("connect-waiting")).toBeVisible();
  await completeFeishuAuth(page);
  await expect(page.getByTestId("connect-finish")).toBeVisible({ timeout: 15000 });
  await page.getByTestId("connect-finish").click();

  // appId is generated on the Feishu side and no longer chosen by the case, so read it back from the card's testid
  const fsCard = page.locator('[data-testid^="feishu-card-cli_"]').first();
  await expect(fsCard).toBeVisible();
  const appId = (await fsCard.getAttribute("data-testid"))!.replace("feishu-card-", "");
  await expect(fsCard.getByTestId(`readiness-status-${appId}`)).toHaveText("就绪");

  // 3. Create an agent, binding prompt, model, skills, Feishu
  await nav(page, "Agents").click();
  await page.getByTestId("new-agent").click();
  await page.getByTestId("agent-name").fill("报销审核员");
  await page.getByTestId("agent-desc").fill("核验发票并判断报销合规");
  await page
    .getByTestId("agent-system")
    .fill("你是报销审核员。逐项核对发票，给出合规结论与依据。");
  await page.getByTestId("agent-model").click();
  // Locate by model ID, not a label prefix: `/DeepSeek V4 Pro/` would match both
  // "DeepSeek V4 Pro" and "DeepSeek V4 Pro (260425)" (strict mode error).
  // Those really are two models, differing only by `-ga-`; see server/shared/infra/ma/README.md.
  await page.getByRole("option", { name: /deepseek-v4-pro-ga-260813/ }).click();
  await page.getByTestId("agent-submit").click();
  await expect(page).toHaveURL(/\/agents\/agent_/);

  await page.getByTestId("tab-skills").click();
  await page.getByTestId("bind-skill-invoice-check").click();
  await page.getByTestId("tab-feishu").click();
  await page.getByTestId(`bind-feishu-${appId}`).click();
  await page.getByTestId("agent-save").click();
  await expect(page.getByText("已保存")).toBeVisible();
  /*
    The "skills" count in the summary bar **includes the built-in skills checked by default**, so the
    expected value is "number of built-ins + the 1 bound by the case".

    # Why not hard-code the number

    This used to be `toHaveText("1")`, assuming a new agent has only the one skill the case bound,
    while a new agent checks all built-in skills by default (the defaultsApplied effect in
    AgentsPage). When the built-in skills went from 2 to 3 (adding `ama`) it went red, reporting "the
    skills count in the summary bar is wrong", which reads like the summary bar is broken, **while the
    product behavior was completely right**. Hard-coding "4" would just push the same mine to the next
    time built-in skills change.

    # This number including built-ins is right, not a compromise

    The summary bar label uses the same i18n key as the "skills" one of the four metric cards on the
    overview page (`overview.metricSkills`), and the overview page counts the length of `listSkills`,
    **which includes built-in skills** (both the mock and the backend list them first). The same key
    meaning two different scopes on two pages would be the real problem.
    Also, Ark's limit of 20 counts the total, and built-ins take slots too (locked in by the backend
    TestBuiltinSkillCountsTowardArkLimit), and "bound N / M" in SkillsTab below also counts the total:
    a summary bar counting only workspace skills would contradict it.

    ⚠️ The create page line "bound N **workspace skills**" (`picked-skill-summary`) deliberately does
    **not** include built-ins; do not use it as a precedent to reason about this one: its label says
    "workspace skills" itself, and the three built-in skill checkboxes are visible in the block right
    above it. The two places count differently because of their own meanings; do not "just align them".
  */
  const builtins = await builtinSkillCount(page, "e2e-demo");
  // Self-check: the built-in skills really were checked by default. Without this, if builtinSkillCount
  // ever returned 0, the assertion below would degrade to "equals 1", exactly the hard-coded value this
  // change removes, and would stay green forever.
  expect(builtins).toBeGreaterThan(0);
  await expect(page.getByTestId("summary-skills")).toHaveText(String(builtins + 1));
  await expect(page.getByTestId("summary-feishu")).toHaveText("1");

  // The binding is visible from the other side too
  await nav(page, "飞书").click();
  await expect(page.getByTestId(`feishu-bound-${appId}`)).toContainText("报销审核员");
  await nav(page, "技能库").click();
  await expect(page.getByTestId("skill-bound-invoice-check")).toHaveText("1 个 Agent 绑定");

  // 4. Run history (the new workspace has no runs yet; it should show an empty state, not an error)
  await nav(page, "运行历史").click();
  await expect(page.getByRole("heading", { name: "还没有运行记录" })).toBeVisible();

  // 5. Change workspace settings: name and members (the entry is "Workspace settings" in the sidebar
  // "Manage" group; members are one of its tabs)
  await nav(page, "空间设置").click();
  await page.getByTestId("settings-name").fill("端到端验证（已改名）");
  await page.getByTestId("settings-save").click();
  await expect(page.getByText("已保存")).toBeVisible();

  // Members are a tab of the settings page; no need to leave this page
  await page.getByTestId("settings-tab-members").click();
  await expect(page).toHaveURL(/\/settings\?tab=members$/);
  await page.getByTestId("invite-member").click();
  // Fill in the other person's login email directly
  await page.getByTestId("member-email").fill("erin@example.com");
  await page.getByTestId("modal-submit").click();
  // ⚠️ To count rows of **people** use `member-row`, not `[data-testid="member-list"] > div`:
  // under member-list are section containers, not rows of people, so that form counts "sections".
  await expect(page.getByTestId("member-row")).toHaveCount(2);

  // 6. Everything is still there after a refresh (the data really persisted)
  // Note the refresh stays on the members tab (the tab is in the query and survives a refresh), so
  // explicitly go back to general before checking the name
  await page.reload();
  await expect(page.getByText("erin@example.com")).toBeVisible();
  await page.goto("/w/e2e-demo/settings?tab=general");
  await expect(page.getByTestId("settings-name")).toHaveValue("端到端验证（已改名）");
  await page.goto("/w/e2e-demo/agents");
  // Both the card title and the prompt preview contain this name, so use the heading to locate the card precisely
  await expect(page.locator('[data-testid^="agent-card-"]')).toHaveCount(1);
  await expect(page.locator('[data-testid^="agent-card-"]').first()).toContainText("报销审核员");
});

test("种子空间里运行历史能完整走一遍：列表 → 详情 → 筛选 → 展开", async ({ app }) => {
  await app.goto("/w/growth");
  /*
    Recent runs on the overview can be clicked into the detail.

    ⚠️ **The overview only lists the latest 5** (`runs.slice(0, 5)` in OverviewPage), while the seed
    now has 6: run_6 (the debugging sample covering all event types) has the newest startedAt and comes
    first, so run_1 is pushed out of those 5. So this now goes to the run list page first and clicks
    run_1 there, no longer relying on it appearing on the overview.
  */
  await expect(app.getByTestId("overview-run-run_6")).toBeVisible();
  await app.goto("/w/growth/runs");
  await app.getByTestId("run-row-run_1").click();
  await expect(app).toHaveURL(/\/runs\/run_1$/);

  // The detail page defaults to the chat view: the question and answer show first
  await expect(app.getByTestId("chat-user-message").first()).toBeVisible();

  // Switch to the event view; the event stream is available
  await app.getByTestId("view-events").click();
  await expect(app.getByTestId("evt-list")).toBeVisible();
  /*
    The event view is now one event per row with the detail in a side panel (no longer an inline
    expansion of paired input + result). "Execution result" is in the detail of the **receipt** row,
    so click that one instead of the start row.
  */
  const tool = app
    .locator('[data-testid="evt-row-tool"][data-evt-type="agent.tool_result"]')
    .first();
  await tool.click();
  // "Execution result" is in the detail panel, not on the row (the row only has a one-line summary)
  await expect(app.getByTestId("evt-detail")).toContainText("执行结果");

  // Back to the list, filter by agent
  await nav(app, "运行历史").click();
  await app.getByTestId("run-agent-filter").click();
  await app.getByRole("option", { name: "数据分析师" }).click();
  // 3 entries: run_1 / run_4 / run_7 all belong to the data analyst (run_7 is the failed chat)
  await expect(app.locator('[data-testid^="run-row-"]')).toHaveCount(3);
});
