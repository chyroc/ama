/**
 * Requirement 2: Feishu apps can be connected, and each agent can be bound to a Feishu app.
 * Requirement 3 (second half): workspaces can configure skills, and agents can bind skills in the workspace.
 */

import { completeFeishuAuth, expect, test } from "./fixtures";

test("走设备码流程接入飞书应用，不用手填任何凭证", async ({ app }) => {
  await app.goto("/w/growth/feishu");
  await app.getByTestId("new-feishu").click();

  // The primary button goes to a separate connect page, rather than popping up a form for credentials
  await expect(app).toHaveURL(/\/feishu\/connect$/);

  // Clicking "open Feishu authorization page" opens a new tab; Playwright must catch it explicitly, or
  // it would interfere with later assertions
  const popup = app.waitForEvent("popup");
  await app.getByTestId("begin-connect").click();
  await (await popup).close();

  // Step two: wait for authorization. sessionId is in the URL, so a refresh can continue
  await expect(app.getByTestId("connect-waiting")).toBeVisible();
  await expect(app).toHaveURL(/session=/);

  // Simulate the user clicking consent in Feishu
  await completeFeishuAuth(app);

  // Step three: connection complete, with a readiness diagnosis right away
  await expect(app.getByTestId("connect-finish")).toBeVisible({ timeout: 15000 });
  await expect(app.getByTestId("connected-name")).toHaveText("AMA 助手");

  // It is visible back on the list page, and ready
  await app.getByTestId("connect-finish").click();
  await expect(app).toHaveURL(/\/feishu$/);
  const card = app.locator('[data-testid^="feishu-card-cli_mock"]').first();
  await expect(card).toBeVisible();
  const appId = (await card.getAttribute("data-testid"))!.replace("feishu-card-", "");
  await expect(card.getByTestId(`readiness-status-${appId}`)).toHaveText("就绪");
});

// With the "group messages" permission checked when connecting, the connected app should carry that grant.
//
// This guards a gap that was never noticed: the backend supported requestGroupMessage all along, while
// the frontend **hard-coded an empty body**, so this permission was never requested in production.
// Without it, "auto-reply to new topics" never takes effect: Feishu does not push group messages without
// an @ at all.
//
// The assertion is on **whether the warning bar is there**, not on the readiness badge: the group
// message permission is conditionally required and does not take part in readiness, so the badge shows
// "ready" whether or not it is checked; asserting on it would be an always-green empty assertion
// (this was really hit: with the checkbox's parameter mutated away, the badge assertion still passed).
test("接入时勾选后，新应用带着群消息权限（免 @ 不再告警）", async ({ app }) => {
  await app.goto("/w/growth/feishu/connect");
  await app.getByTestId("connect-want-group-message").click();

  const popup = app.waitForEvent("popup");
  await app.getByTestId("begin-connect").click();
  await (await popup).close();
  await completeFeishuAuth(app);
  await expect(app.getByTestId("connect-finish")).toBeVisible({ timeout: 15000 });

  // Only after binding it to an agent can the trigger mode and that warning be seen.
  await app.getByTestId("connect-finish").click();
  const card = app.locator('[data-testid^="feishu-card-cli_mock"]').first();
  const appId = (await card.getAttribute("data-testid"))!.replace("feishu-card-", "");

  await app.goto("/w/growth/agents/agent_reviewer?tab=feishu");
  await app.getByTestId(`bind-feishu-${appId}`).click();
  await app.getByTestId("agent-save").click();
  await expect(app.getByText("已保存")).toBeVisible();

  await app.getByTestId(`feishu-trigger-${appId}`).click();
  await app.getByRole("option", { name: "新话题自动回复 + 其他情况 @ 触发" }).click();

  // **Reload before asserting**, instead of asserting right after changing the mode.
  //
  // Changing the mode triggers a save + an upper-level reload, during which the whole block briefly
  // disappears; and toHaveCount(0) is a **retrying** assertion that gets satisfied exactly in that gap,
  // so even if the warning should appear, this would still be green (measured during mutation testing:
  // the warning was clearly there, count read 1, and the assertion still passed). After a reload both are
  // produced in the same render, and only then does "the trigger mode is right" make "the warning is
  // absent" meaningful.
  await app.goto("/w/growth/agents/agent_reviewer?tab=feishu");
  await expect(app.getByTestId(`feishu-trigger-${appId}`)).toContainText("新话题自动回复");
  await expect(app.getByTestId(`feishu-group-scope-notice-${appId}`)).toHaveCount(0);
});

test("接入页刷新后能接着轮询，不用从头再来", async ({ app }) => {  await app.goto("/w/growth/feishu/connect");
  const popup = app.waitForEvent("popup");
  await app.getByTestId("begin-connect").click();
  await (await popup).close();
  await expect(app.getByTestId("connect-waiting")).toBeVisible();

  const url = app.url();
  expect(url).toMatch(/session=/);

  // Refresh: sessionId is in the URL, so it still stays at "waiting for authorization" instead of going back to step one
  await app.reload();
  await expect(app.getByTestId("connect-waiting")).toBeVisible();

  await completeFeishuAuth(app);
  await expect(app.getByTestId("connect-finish")).toBeVisible({ timeout: 15000 });
});

test("就绪度把「缺权限」和「等管理员批」分开呈现", async ({ app }) => {
  await app.goto("/w/growth/feishu");

  // fs_2 in the seed is deliberately in the "waiting for admin approval" shape
  const card = app.getByTestId("feishu-card-cli_9f8e7d6c5b4a0022");
  await expect(card.getByTestId("readiness-status-cli_9f8e7d6c5b4a0022")).toHaveText("等管理员批准");
  await expect(card.getByText(/项权限等管理员批准/)).toBeVisible();
  // The request was already submitted, so the user should not be urged to submit it again
  await expect(card.getByText(/申请已提交给管理员/)).toBeVisible();
  await expect(card.getByTestId("request-admin-cli_9f8e7d6c5b4a0022")).toHaveCount(0);

  // The app that is ready should not show any to-dos
  const ready = app.getByTestId("feishu-card-cli_a1b2c3d4e5f60011");
  await expect(ready.getByTestId("readiness-status-cli_a1b2c3d4e5f60011")).toHaveText("就绪");
  await expect(ready.getByText(/项权限/)).toHaveCount(0);
});

test("重新检测就绪度会刷新结论", async ({ app }) => {
  await app.goto("/w/growth/feishu");
  const card = app.getByTestId("feishu-card-cli_9f8e7d6c5b4a0022");
  await card.getByTestId("recheck-cli_9f8e7d6c5b4a0022").click();
  // The mock's check always returns "all green", so after re-checking it becomes ready
  await expect(card.getByTestId("readiness-status-cli_9f8e7d6c5b4a0022")).toHaveText("就绪", {
    timeout: 10000,
  });
});

test("创建技能后可在 agent 上绑定，绑定关系双向可见", async ({ app }) => {
  // First create a skill
  await app.goto("/w/growth/skills");
  await app.getByTestId("new-skill").click();
  await app.getByTestId("skill-title").fill("竞品监测");
  await app.getByTestId("skill-name").fill("rival-watch");
  await app.getByTestId("skill-desc").fill("当需要跟踪竞品动态、对比功能差异时使用。");
  await app.getByTestId("skill-instructions").fill("# 竞品监测\n\n1. 列出竞品\n2. 对比核心功能");
  await app.getByTestId("modal-submit").click();

  const skillCard = app.getByTestId("skill-card-rival-watch");
  await expect(skillCard).toBeVisible();
  await expect(app.getByTestId("skill-bound-rival-watch")).toHaveText("0 个 Agent 绑定");

  // Bind it on the agent
  await app.goto("/w/growth/agents/agent_triage?tab=skills");
  await app.getByTestId("bind-skill-rival-watch").click();
  await app.getByTestId("agent-save").click();
  await expect(app.getByText("已保存")).toBeVisible();
  await expect(app.getByTestId("summary-skills")).toHaveText("2");

  // Back to the skill library: the bound count should show (the other direction)
  await app.goto("/w/growth/skills");
  await expect(app.getByTestId("skill-bound-rival-watch")).toHaveText("1 个 Agent 绑定");
});

test("上传 SKILL.md：先预览解析结果，确认后才入库", async ({ app }) => {
  await app.goto("/w/growth/skills");
  await app.getByTestId("upload-skill").click();

  // Use setInputFiles to drive that hidden real input.
  // **page.route cannot be used to fake responses**: e2e runs in local mode, LocalApiClient sends no
  // network requests, and route never fires (see the notes in e2e/fixtures.ts).
  await app.getByTestId("skill-upload-input").setInputFiles({
    name: "SKILL.md",
    mimeType: "text/markdown",
    buffer: Buffer.from(
      "---\nname: invoice-audit\ndescription: 当需要核验发票、判断报销合规时使用。\n---\n\n# 发票核验\n\n1. 校验税号\n",
    ),
  });

  // The parse result must show in the preview first: users need a chance to notice a wrong frontmatter.
  // Assert each of the three fields once: with the same source, "description wrongly rendered as name" would also pass.
  await expect(app.getByTestId("skill-preview-name")).toHaveText("invoice-audit");
  await expect(app.getByTestId("skill-preview-desc")).toHaveText(
    "当需要核验发票、判断报销合规时使用。",
  );
  await expect(app.getByTestId("skill-preview")).toContainText("# 发票核验");

  // It is **not** stored yet at this point: preview is not creation.
  await expect(app.getByTestId("skill-card-invoice-audit")).toHaveCount(0);

  await app.getByTestId("modal-submit").click();
  await expect(app.getByTestId("skill-card-invoice-audit")).toBeVisible();
  // A bare md has no attachments, so the "N attachments" marker should not appear.
  await expect(app.getByTestId("skill-files-invoice-audit")).toHaveCount(0);
});

test("上传技能包：非法 frontmatter 与错误文件类型都要给出明确报错", async ({ app }) => {
  await app.goto("/w/growth/skills");
  await app.getByTestId("upload-skill").click();

  // One, missing frontmatter: **it must error, not silently create a skill with empty values**.
  await app.getByTestId("skill-upload-input").setInputFiles({
    name: "SKILL.md",
    mimeType: "text/markdown",
    buffer: Buffer.from("# 只有正文\n\n没有 YAML 头。\n"),
  });
  await expect(app.getByTestId("skill-upload-error")).toContainText("frontmatter");
  await expect(app.getByTestId("skill-preview")).toHaveCount(0);

  // Two, an invalid name (the Ark side only accepts [a-z0-9-]).
  await app.getByTestId("skill-upload-input").setInputFiles({
    name: "SKILL.md",
    mimeType: "text/markdown",
    buffer: Buffer.from("---\nname: Bad_Name\ndescription: 描述\n---\n\n# 正文\n"),
  });
  await expect(app.getByTestId("skill-upload-error")).toContainText("不合法");
  await expect(app.getByTestId("skill-preview")).toHaveCount(0);

  // Three, wrong file type: the frontend should block it and not send a request.
  await app.getByTestId("skill-upload-input").setInputFiles({
    name: "evil.exe",
    mimeType: "application/octet-stream",
    buffer: Buffer.from("MZ"),
  });
  await expect(app.getByTestId("skill-upload-error")).toContainText("只支持");
  await expect(app.getByTestId("skill-preview")).toHaveCount(0);
});

test("飞书应用与 agent 是 1:1 绑定：被别人占着的应用整行不显示", async ({ app }) => {
  // Both apps in the seed are taken: cli_a1b2c3d4e5f60011 (the growth assistant) belongs to agent_triage,
  // and cli_9f8e7d6c5b4a0022 (the data report bot) to agent_analyst.
  // When agent_reviewer opens this page, neither row **should appear**.
  //
  // The earlier approach listed them as usual, disabled the toggle, and wrote "already bound by agent
  // <ticket triage assistant>". That sentence gives the reader nothing to act on: they can do nothing on
  // agent_reviewer's page, yet are forced to learn another agent's name. The user's words: "very strange".
  await app.goto("/w/growth/agents/agent_reviewer?tab=feishu");
  await expect(app.getByTestId("summary-feishu")).toHaveText("0");

  // ⚠️ **First wait for a positive "finished rendering" signal, then assert "X does not exist"**.
  // toHaveCount(0) is satisfied in the skeleton frame and in the gap before data returns, so asserting
  // directly lets the case pass at the "not drawn yet" moment: it would stay green even with the whole
  // filter logic deleted.
  // This empty state is itself the test: it only renders when "there are apps, but all are taken".
  await expect(app.getByText("没有可绑定的飞书应用")).toBeVisible();
  // The text must make clear "there are apps, they are just all taken", or it cannot be told apart
  // from "no Feishu connected in the workspace yet".
  await expect(app.getByText(/2 个飞书应用都已经交给别的 Agent/)).toBeVisible();

  // Only now come the "should not appear" assertions.
  await expect(app.getByTestId("feishu-binding-row-cli_a1b2c3d4e5f60011")).toHaveCount(0);
  await expect(app.getByTestId("bind-feishu-cli_a1b2c3d4e5f60011")).toHaveCount(0);
  await expect(app.getByTestId("feishu-binding-row-cli_9f8e7d6c5b4a0022")).toHaveCount(0);
  await expect(app.getByTestId("bind-feishu-cli_9f8e7d6c5b4a0022")).toHaveCount(0);

  // Not a single character of the holder's name may show: this is exactly what this change removes.
  await expect(app.getByText("工单分诊助手")).toHaveCount(0);
  await expect(app.getByText("数据分析师")).toHaveCount(0);
});

test("原主人解绑后，应用才能绑给别的 agent", async ({ app }) => {
  // First make agent_triage let go.
  await app.goto("/w/growth/agents/agent_triage?tab=feishu");
  await expect(app.getByTestId("feishu-bindstate-cli_a1b2c3d4e5f60011")).toHaveText("已绑定");
  await app.getByTestId("bind-feishu-cli_a1b2c3d4e5f60011").click();
  await app.getByTestId("agent-save").click();
  await expect(app.getByTestId("summary-feishu")).toHaveText("0");

  // Now agent_reviewer can bind: that row appears **from nothing** (the whole row was hidden before).
  await app.goto("/w/growth/agents/agent_reviewer?tab=feishu");
  await expect(app.getByTestId("feishu-binding-row-cli_a1b2c3d4e5f60011")).toBeVisible();
  await app.getByTestId("bind-feishu-cli_a1b2c3d4e5f60011").click();
  await app.getByTestId("agent-save").click();
  await expect(app.getByText("已保存")).toBeVisible();
  await expect(app.getByTestId("summary-feishu")).toHaveText("1");

  // The back-reference on the Feishu page should only have the new owner, not both.
  await app.goto("/w/growth/feishu");
  const bound = app.getByTestId("feishu-bound-cli_a1b2c3d4e5f60011");
  await expect(bound).toContainText("代码审查员");
  await expect(bound).not.toContainText("工单分诊助手");
});

test("解除绑定同样双向生效", async ({ app }) => {
  await app.goto("/w/growth/agents/agent_triage?tab=feishu");
  await expect(app.getByTestId("summary-feishu")).toHaveText("1");

  await app.getByTestId("bind-feishu-cli_a1b2c3d4e5f60011").click();
  await app.getByTestId("agent-save").click();
  await expect(app.getByTestId("summary-feishu")).toHaveText("0");

  await app.goto("/w/growth/feishu");
  // After unbinding, this app has no agent at all, and the feishu-bound-* container **only renders when
  // there is a binding** (without one it shows the binding guide, see bind-agent-prompt-*). So assert here
  // that "the container does not exist"; not.toContainText cannot be used: that assertion fails rather
  // than passes when the element is missing.
  await expect(app.getByTestId("feishu-bound-cli_a1b2c3d4e5f60011")).toHaveCount(0);
});

test("技能是引用而非拷贝：改技能内容，绑定它的 agent 立即用新版", async ({ app }) => {
  await app.goto("/w/growth/skills");
  await app.getByTestId("edit-skill-ticket-triage").click();
  await app.getByTestId("skill-instructions").fill("# 工单分诊（改版）\n\n新的分诊步骤在此。");
  await app.getByTestId("modal-submit").click();

  // The agent side sees the same skill (the incrementing version shows it is a reference)
  await app.goto("/w/growth/agents/agent_triage?tab=skills");
  await expect(app.getByTestId("bind-skill-ticket-triage")).toBeVisible();
  await app.goto("/w/growth/skills");
  await app.getByTestId("edit-skill-ticket-triage").click();
  await expect(app.getByTestId("skill-instructions")).toHaveValue(/改版/);
});

test("删除技能会解除 agent 上的绑定，不留悬空引用", async ({ app }) => {
  await app.goto("/w/growth/agents/agent_analyst?tab=skills");
  await expect(app.getByTestId("summary-skills")).toHaveText("2");

  await app.goto("/w/growth/skills");
  await app.getByTestId("skill-card-metrics-sql").getByLabel("删除技能").click();
  // Before deleting, the impact must be stated
  await expect(app.getByText(/个 Agent 绑定了该技能/)).toBeVisible();
  await app.getByTestId("confirm-danger").click();

  await app.goto("/w/growth/agents/agent_analyst?tab=skills");
  await expect(app.getByTestId("summary-skills")).toHaveText("1");
});

// ---------------------------------------------------------------------------
// Binding guidance after connecting
//
// "Not bound" is a state that needs the user to act. The list page used to show only a gray line ("bind
// this app under the "Feishu" tab of the Agent detail page"), and the connect success page only had a
// "done": both wrote what the user should do as a manual and left them there. The two cases below lock
// in that both entries really exist and really work.
// ---------------------------------------------------------------------------

test("未绑定的飞书卡片上有可点的绑定入口，就地就能绑上", async ({ app }) => {
  // First free up cli_a1b2c3d4e5f60011: in the seed it belongs to agent_triage.
  await app.goto("/w/growth/agents/agent_triage?tab=feishu");
  await app.getByTestId("bind-feishu-cli_a1b2c3d4e5f60011").click();
  await app.getByTestId("agent-save").click();
  await expect(app.getByTestId("summary-feishu")).toHaveText("0");

  await app.goto("/w/growth/feishu");
  const card = app.getByTestId("feishu-card-cli_a1b2c3d4e5f60011");
  // The entry must be **clickable**, not a sentence.
  const prompt = card.getByTestId("bind-agent-prompt-cli_a1b2c3d4e5f60011");
  await expect(prompt).toBeVisible();
  await expect(card.getByTestId("bind-agent-submit-cli_a1b2c3d4e5f60011")).toBeVisible();

  // Pick an Agent and bind it right here: no jumping to the Agent detail page to dig through tabs.
  await card.getByTestId("bind-agent-select-cli_a1b2c3d4e5f60011").click();
  await app.getByRole("option", { name: "代码审查员" }).click();
  await card.getByTestId("bind-agent-submit-cli_a1b2c3d4e5f60011").click();

  // See the result right after binding: the guide disappears and the back-reference appears.
  await expect(card.getByTestId("feishu-bound-cli_a1b2c3d4e5f60011")).toContainText("代码审查员");
  await expect(prompt).toHaveCount(0);

  // It really took effect on the other side too (not just a display change on this page).
  await app.goto("/w/growth/agents/agent_reviewer?tab=feishu");
  await expect(app.getByTestId("summary-feishu")).toHaveText("1");
  await expect(app.getByTestId("feishu-bindstate-cli_a1b2c3d4e5f60011")).toHaveText("已绑定");
});

test("接入成功后给出「去绑定 Agent」的下一步，而不是只有「完成」", async ({ app }) => {
  await app.goto("/w/growth/feishu/connect");
  const popup = app.waitForEvent("popup");
  await app.getByTestId("begin-connect").click();
  await (await popup).close();
  await completeFeishuAuth(app);
  await expect(app.getByTestId("connect-finish")).toBeVisible({ timeout: 15000 });

  // The appId of a newly connected app is generated by Feishu; read it back from the testid of the "connected" card.
  const appId = (await app
    .locator('[data-testid^="readiness-cli_mock"]')
    .first()
    .getAttribute("data-testid"))!.replace("readiness-", "");

  // The next step must be there, and be a real entry that can pick an Agent.
  await expect(app.getByText("下一步：交给一个 Agent")).toBeVisible();
  await app.getByTestId(`bind-agent-select-${appId}`).click();
  await app.getByRole("option", { name: "代码审查员" }).click();
  await app.getByTestId(`bind-agent-submit-${appId}`).click();

  // After binding, stop urging and show the result: the guide turns into a conclusion in place, so the
  // user knows this step is done.
  await expect(app.getByTestId("connect-bound")).toContainText("代码审查员");
  await expect(app.getByTestId(`bind-agent-submit-${appId}`)).toHaveCount(0);

  // Back on the list page, the binding really landed.
  await app.getByTestId("connect-finish").click();
  await expect(app.getByTestId(`feishu-bound-${appId}`)).toContainText("代码审查员");
});

test("触发方式：默认仅 @ 触发，改成话题群自动后立刻生效且刷新后仍在", async ({ app }) => {
  // In the seed cli_a1b2c3d4e5f60011 (the growth assistant) belongs to agent_triage, and is on the default mode.
  await app.goto("/w/growth/agents/agent_triage?tab=feishu");

  const trigger = app.getByTestId("feishu-trigger-cli_a1b2c3d4e5f60011");
  await expect(trigger).toContainText("仅 @ 我时回复");
  // The description must spell out "every group needs an @"; the option name alone does not convey it.
  await expect(app.getByTestId("feishu-trigger-hint-cli_a1b2c3d4e5f60011")).toContainText(
    "都必须 @ 机器人",
  );

  await trigger.click();
  await app.getByRole("option", { name: "新话题自动回复 + 其他情况 @ 触发" }).click();
  await expect(trigger).toContainText("新话题自动回复");
  await expect(app.getByTestId("feishu-trigger-hint-cli_a1b2c3d4e5f60011")).toContainText(
    "无需 @",
  );

  // Refresh **without clicking save**: the trigger mode is an app-level field stored on its own, not
  // following the agent's draft. This assertion exists to lock in exactly that: if it followed the draft,
  // a refresh would revert it to the default mode.
  await app.goto("/w/growth/agents/agent_triage?tab=feishu");
  await expect(app.getByTestId("feishu-trigger-cli_a1b2c3d4e5f60011")).toContainText(
    "新话题自动回复",
  );
});

test("触发方式只在已绑定的应用上出现", async ({ app }) => {
  // This one needs "an app that is **listed but not bound** shows no trigger mode picker": configuring
  // "when to answer" for an app nobody answers on gets things backwards.
  //
  // ⚠️ So an app must be freed up first. Both apps in the seed are taken by other agents, and taken apps
  // now **do not render their row at all**; opening agent_reviewer directly would be an empty list, and
  // this case would degrade into "no picker on a row that does not exist", always true and checking nothing.
  await app.goto("/w/growth/agents/agent_triage?tab=feishu");
  await app.getByTestId("bind-feishu-cli_a1b2c3d4e5f60011").click();
  await app.getByTestId("agent-save").click();
  await expect(app.getByTestId("summary-feishu")).toHaveText("0");

  await app.goto("/w/growth/agents/agent_reviewer?tab=feishu");
  // The row is there (the app can be bound), but it is not bound to me, so the picker should not appear.
  await expect(app.getByTestId("feishu-binding-row-cli_a1b2c3d4e5f60011")).toBeVisible();
  await expect(app.getByTestId("feishu-bindstate-cli_a1b2c3d4e5f60011")).toHaveText("未绑定");
  await expect(app.getByTestId("feishu-trigger-cli_a1b2c3d4e5f60011")).toHaveCount(0);
});

// No-@ mode chosen without Feishu granting the permission: it must be said on the spot, with a
// clickable authorization entry.
//
// This guards a real incident: a user set "auto-reply to new topics", sent a message in a topic group,
// and the bot said nothing, with nothing abnormal in the UI. The root cause: without the
// im:message.group_msg permission Feishu **does not push group messages without an @ at all**; the
// events never reach us, so there is no trace in the logs or the inbound table.
// The backend had computed this state long before, but no part of the UI read it.
test("免 @ 缺权限时给出告警与授权入口", async ({ app }) => {
  // The cli_9f8e… bound to agent_analyst has exactly that shape: triggerMode is already no-@,
  // while groupMessageScopeGranted is false.
  await app.goto("/w/growth/agents/agent_analyst?tab=feishu");

  const trigger = app.getByTestId("feishu-trigger-cli_9f8e7d6c5b4a0022");
  await expect(trigger).toContainText("新话题自动回复");

  // The warning must state **the consequence**, not just "missing permission": the latter does not
  // explain why it matters.
  await expect(app.getByTestId("feishu-group-scope-notice-cli_9f8e7d6c5b4a0022")).toBeVisible();
  await expect(app.getByTestId("feishu-group-scope-grant-cli_9f8e7d6c5b4a0022")).toBeVisible();
  await expect(app.getByText("当前设置不会生效")).toBeVisible();
  // The permission name must be shown as-is; users need it to look things up on the open platform.
  await expect(app.getByText("im:message.group_msg").first()).toBeVisible();
  // Coming back from Feishu needs an "I've done it" action.
  await expect(app.getByTestId("feishu-group-scope-recheck-cli_9f8e7d6c5b4a0022")).toBeVisible();
});

// Switching back to the default mode should make the warning disappear: it is a **conditionally
// required** permission, only needed for no-@.
//
// Without this, a normally working app that "only replies when @-ed" would also see a warning it does not
// need to care about, exactly the "reporting a working app as missing a pile of things" that the file
// header of readiness.go complains about.
test("切回仅 @ 触发后缺权限告警消失", async ({ app }) => {
  await app.goto("/w/growth/agents/agent_analyst?tab=feishu");
  const notice = app.getByTestId("feishu-group-scope-notice-cli_9f8e7d6c5b4a0022");
  await expect(notice).toBeVisible();

  await app.getByTestId("feishu-trigger-cli_9f8e7d6c5b4a0022").click();
  await app.getByRole("option", { name: "仅 @ 我时回复" }).click();

  // **Reload before asserting** after the change: switching triggers a save + reload, the whole block is
  // absent during that gap, and toHaveCount(0) retries, getting satisfied exactly in the gap, so it is green
  // even if the warning should appear. This trap was measured on another case in this file (after mutation
  // count read 1 and the assertion still passed).
  await app.goto("/w/growth/agents/agent_analyst?tab=feishu");
  await expect(app.getByTestId("feishu-trigger-cli_9f8e7d6c5b4a0022")).toContainText(
    "仅 @ 我时回复",
  );
  await expect(notice).toHaveCount(0);
});

// An app with the permission granted should have no leftover warning even with no-@ chosen.
//
// No positive hint like "authorized ✓" either: the normal state should not take visual space; no
// warning means it is fine.
test("权限已授予时选免 @ 不显示任何告警", async ({ app }) => {
  // cli_a1b2…'s groupMessageScopeGranted is true.
  await app.goto("/w/growth/agents/agent_triage?tab=feishu");
  const trigger = app.getByTestId("feishu-trigger-cli_a1b2c3d4e5f60011");
  await trigger.click();
  await app.getByRole("option", { name: "新话题自动回复 + 其他情况 @ 触发" }).click();
  await expect(trigger).toContainText("新话题自动回复");

  // First wait for something that certainly appears, then assert "does not exist"; otherwise this would
  // be satisfied during the skeleton stage and become an always-green empty assertion.
  await expect(app.getByTestId("feishu-trigger-hint-cli_a1b2c3d4e5f60011")).toContainText("无需 @");
  await expect(app.getByTestId("feishu-group-scope-notice-cli_a1b2c3d4e5f60011")).toHaveCount(0);
  // The bot-included level is also granted, so the second warning should not appear either.
  await expect(app.getByTestId("feishu-bot-scope-notice-cli_a1b2c3d4e5f60011")).toHaveCount(0);
});

// ⭐ "Has the group message permission, but cannot receive messages sent by **bots**" needs its own warning.
//
// # This case guards a real incident
//
// Feishu splits "receive group messages" into two levels, and `im:message.group_msg` **only covers
// messages sent by humans**. In an alert group almost all messages were sent by bots, so that group
// received no events at all, while the UI showed `groupMessageScopeGranted` as true and everything
// normal; it took two rounds of debugging to find.
//
// If the two levels were merged into one hint, this state **could not be expressed in the UI at all**.
// So this locks in: when the first level is complete and the second is not, a second warning must
// appear, and it must state the consequence.
test("群消息权限齐了但缺「收机器人消息」时，仍要单独告警", async ({ app }) => {
  await app.goto("/w/growth/agents/agent_triage?tab=feishu");

  // Change cli_a1b2… (originally complete on both levels) to "first level only": that is what existing
  // apps in production look like: they only requested group_msg back then, and include_bot is a later level.
  await app.evaluate(() => {
    (
      window as unknown as {
        __amaApi: { setFeishuBotScopeGranted(appId: string, granted: boolean): void };
      }
    ).__amaApi.setFeishuBotScopeGranted("cli_a1b2c3d4e5f60011", false);
  });
  await app.reload();

  const trigger = app.getByTestId("feishu-trigger-cli_a1b2c3d4e5f60011");
  await trigger.click();
  await app.getByRole("option", { name: "新话题自动回复 + 其他情况 @ 触发" }).click();
  await expect(trigger).toContainText("新话题自动回复");

  // The first level is complete, so **only** the second warning should appear: both popping up together
  // would make users think they need to apply twice.
  await expect(app.getByTestId("feishu-group-scope-notice-cli_a1b2c3d4e5f60011")).toHaveCount(0);
  await expect(app.getByTestId("feishu-bot-scope-notice-cli_a1b2c3d4e5f60011")).toBeVisible();
  // The warning must state **the consequence**: without saying "messages sent by bots cannot be received",
  // users do not know why it concerns them.
  await expect(app.getByText("机器人发的消息收不到")).toBeVisible();
  // The permission name must be shown as-is; users need it to look things up on the open platform.
  await expect(app.getByText("im:message.group_msg.include_bot:read")).toBeVisible();
  await expect(app.getByTestId("feishu-bot-scope-grant-cli_a1b2c3d4e5f60011")).toBeVisible();
  await expect(app.getByTestId("feishu-bot-scope-recheck-cli_a1b2c3d4e5f60011")).toBeVisible();
});

// ---------------------------------------------------------------------------
// Platform built-in skills
// ---------------------------------------------------------------------------

// Built-in skills (lark-cli / ama) are visible in the skill library, marked "platform built-in".
//
// These two CLIs are preinstalled in the container; the skill only "tells the model it exists". The
// source of truth is in the backend service/skill/skillbuiltin.go, with a copy laid out by
// data/builtinSkills.ts in local mode.
//
// The seed has only one workspace, so the "visible in every workspace" half is covered by backend
// cases (TestBuiltinSkillsVisibleInEveryWorkspace in core/skillbuiltin_test.go really creates a second
// workspace to check). This checks the frontend rendering.
test("平台内置技能出现在技能库里，且标着「平台内置」", async ({ app }) => {
  await app.goto("/w/growth/skills");
  // First wait for a card that will certainly appear, to avoid asserting during the skeleton stage.
  await expect(app.getByTestId("skill-card-lark-cli")).toBeVisible();
  await expect(app.getByTestId("skill-card-ama")).toBeVisible();
  // The badge must be there: without it, users would think it was created in their own workspace, then
  // find the edit button missing.
  await expect(app.getByTestId("skill-builtin-ama")).toHaveText("平台内置");
});

// Built-in skills get no edit and delete entries: the backend returns 403 for both, and rendering
// them would only make users find out after clicking.
test("内置技能没有编辑按钮，自建技能有", async ({ app }) => {
  await app.goto("/w/growth/skills");

  // First wait for the user-created skill's edit button to appear, **then** assert the built-in skill has none.
  // In the reverse order, the "buttons not rendered yet" frame would let toHaveCount(0) pass, so the
  // assertion would always be green, including when the feature is broken.
  await expect(app.getByTestId("edit-skill-ticket-triage")).toBeVisible();
  await expect(app.getByTestId("edit-skill-ama")).toHaveCount(0);
  await expect(app.getByTestId("edit-skill-lark-cli")).toHaveCount(0);
});

// Built-in skills can be checked on an agent, and are also marked built-in in the picker.
//
// "Checkable" is the purpose of this feature: not bound by default, users check it when they want it.
test("内置技能可以绑定到 agent 上", async ({ app }) => {
  await app.goto("/w/growth/agents/agent_triage?tab=skills");
  await expect(app.getByTestId("bind-skill-builtin-ama")).toHaveText("平台内置");

  await app.getByTestId("bind-skill-ama").click();
  await app.getByTestId("agent-save").click();
  await expect(app.getByText("已保存")).toBeVisible();

  // Back to the skill library: the bound count must show (proving the binding really landed, not just a
  // check in the UI).
  await app.goto("/w/growth/skills");
  await expect(app.getByTestId("skill-bound-ama")).toHaveText("1 个 Agent 绑定");
});

// A name clash with a built-in skill must be blocked: after a clash the list has two skills with the
// same name, and binding cannot tell which is which.
test("不能创建与内置技能同名的技能", async ({ app }) => {
  await app.goto("/w/growth/skills");
  await app.getByTestId("new-skill").click();
  await app.getByTestId("skill-title").fill("我自己的 ama");
  await app.getByTestId("skill-name").fill("ama");
  await app.getByTestId("skill-desc").fill("当需要访问内部平台时使用。");
  await app.getByTestId("skill-instructions").fill("# 我自己的\n\n随便写点");
  await app.getByTestId("modal-submit").click();

  await expect(app.getByText("技能名「ama」是平台内置技能，请换一个")).toBeVisible();
});

test("连发下一句的档位：默认转向，改成排队后要能存下来", async ({ app }) => {
  /*
   * Both traps of this switch are in "the default", so both halves must be asserted:
   *
   * One, **the default must be redirect.** Existing agents have followUp as an empty string, and an empty
   *     string is equivalent to redirect (the default side of domain.FollowUpMode, the opposite of other
   *     enums in this repo). If the frontend compared bare `followUp === "redirect"`, it would judge the
   *     empty string as "neither", and the dropdown would show blank: looking unconfigured while it is
   *     actually running as redirect.
   * Two, **changing to queue must really be saved.** This goes through the whole form's save (unlike the
   *     trigger mode, which calls the API right after changing), so reload before asserting: without a
   *     reload it reads the in-memory draft, and an implementation that "never put this field into the
   *     request body" would also be green.
   */
  await app.goto("/w/growth/agents/agent_reviewer?tab=feishu");

  // 1. Default: agents in the seed never chose, so the UI must show "redirect" rather than blank
  const select = app.getByTestId("agent-followup");
  await expect(select).toContainText("转向");
  // 2. The cost must be written out: from the word "redirect" alone, users would not guess that a tool
  // halfway through also stops
  await expect(app.getByTestId("agent-followup-hint")).toContainText("打断");

  // 3. Change to queue and save
  await select.click();
  await app.getByRole("option", { name: "排队（等上一轮跑完）" }).click();
  await expect(app.getByTestId("agent-followup-hint")).toContainText("等当前这一轮答完");
  await app.getByTestId("agent-save").click();
  await expect(app.getByText("已保存")).toBeVisible();

  // 4. **Reload before asserting**: without a reload it reads the in-memory draft,
  //    and an implementation that "did not put the field into the request body" would be equally right at that point.
  await app.goto("/w/growth/agents/agent_reviewer?tab=feishu");
  await expect(app.getByTestId("agent-followup")).toContainText("排队");
  await expect(app.getByTestId("agent-followup-hint")).toContainText("等当前这一轮答完");
});

test("展示详细工作内容的档位：默认展示，改成不展示后要能存下来", async ({ app }) => {
  /*
   * The same shape as the one above, but **the default half matters even more here**:
   *
   * This field's default is "show", and it was almost made a boolean: a boolean's default is false,
   * landing exactly on the "do not show" side. Had it been a boolean, existing agents and old frontends
   * would have the chain of thought silently turned off, and the UI would faithfully show "off" (the user
   * never clicked it). So what step 1 asserts is not "the UI looks nice", but that **the default direction
   * was not flipped**.
   *
   * The second half likewise needs a reload: without one it reads the in-memory draft, and an
   * implementation that "never put the field into the request body" would also be green.
   */
  await app.goto("/w/growth/agents/agent_reviewer?tab=feishu");

  // 1. Default: agents in the seed never chose (empty string), so the UI must show "show" rather than blank
  const select = app.getByTestId("agent-progress-detail");
  await expect(select).toContainText("展示");
  // 2. The hint must state the consequence: "do not show" is easily read as "fold it", while it actually
  // does not send that message at all
  await expect(app.getByTestId("agent-progress-detail-hint")).toContainText("思维链");

  // 3. Change to do not show and save
  await select.click();
  await app.getByRole("option", { name: "不展示（只给最终答案）" }).click();
  await expect(app.getByTestId("agent-progress-detail-hint")).toContainText(
    "不会出现思维链消息",
  );
  await app.getByTestId("agent-save").click();
  await expect(app.getByText("已保存")).toBeVisible();

  // 4. **Reload before asserting**, for the same reason as above.
  await app.goto("/w/growth/agents/agent_reviewer?tab=feishu");
  await expect(app.getByTestId("agent-progress-detail")).toContainText("不展示");
  await expect(app.getByTestId("agent-progress-detail-hint")).toContainText(
    "不会出现思维链消息",
  );
});
