/**
 * The "authorizations" and "custom env" sections of an agent.
 *
 * The three cases map to the items named in the requirement's acceptance:
 *
 *	the AMA provider appears in the authorization list with status ok
 *	a new agent **selects no authorization by default**
 *	custom env can be filled in, marked sensitive, and invalid input is blocked on the spot
 *
 * "Checked authorizations really get injected into the container at runtime" lives in the backend:
 * the frontend cannot see injection, only the checked state, so that one should not pretend to be
 * tested here.
 */
import { expandIntegration, expect, test } from "./fixtures";
import type { Page } from "@playwright/test";

// Same approach as 02-agent.spec.ts: toBeChecked must target the input, clicks must target the label
// (HeroUI's Checkbox puts the testid on the outer wrapper, not on the input).
const checkbox = (app: Page, testId: string) => app.getByTestId(testId).locator("input");
const clickCheckbox = (app: Page, testId: string) =>
  app.getByTestId(testId).locator("label").click();

test("AMA 授权出现在授权管理页且状态为已授权", async ({ app }) => {
  await app.goto("/preferences?tab=integrations");

  await expect(app.getByTestId("integration-card-ama")).toBeVisible();
  await expect(app.getByTestId("integration-status-ama")).toHaveText("已就绪");

  // ⚠️ Cards are now collapsible, and ama is ok -> **collapsed by default**, so the assertions below
  // about "what is in the card" must expand it first. The status Chip one does not (it is in the
  // collapsed row, see 11-integrations).
  await expandIntegration(app, "ama");

  // ⚠️ **The ok state no longer shows the credential prefix/creation time** (simplified): this source is
  // fully automatic, and the user has no decision to make on this card; those three lines of metadata
  // and the revoke button are on the "API Key" page in global settings, which is where credentials are managed.
  //
  // The assertion is now reversed: **not a single character of plaintext is allowed**. This matters more
  // than the old one: the plaintext can be computed (deterministic derivation), while Summary/FixHint
  // are free text that the compiler does not check.
  // The backend side also has TestAMAProviderNeverLeaksPlaintextKey scanning every field.
  const amaCard = app.getByTestId("integration-card-ama");
  await expect(amaCard).not.toContainText("凭据前缀");
  // amak_ followed by 40+ characters = the shape of a whole key. The prefix is only 6 characters, so no false hits.
  await expect(amaCard).not.toContainText(/amak_[A-Za-z0-9_-]{40,}/);

  // It is the "platform creates its own credentials" kind: **neither action button is given**.
  // "Authorize" makes no sense (the platform creates it itself), and "revoke" is **explicitly not
  // supported by the requirement** (see the file header of server/core/integration_ama.go; this used
  // to assert the revoke button was visible, left over from the old "revocable" requirement; do not
  // change it back).
  //
  // ⚠️ **These two toHaveCount(0) must come after the assertions above.** They also hold in the frame
  // where "the card has not rendered yet", so placing them alone right after goto would be a false
  // green; the "status is ready" + "shows the amak_ prefix" above already wait for the card to finish
  // rendering, and only then can these expose "rendered but the buttons are missing".
  // ⚠️⚠️ Collapsing adds **one more false green**: the buttons are in the collapsed area, and when the
  // card is collapsed they also count 0. The `expandIntegration` call above is the precondition for
  // these two to be meaningful again; do not delete it.
  await expect(app.getByTestId("integration-authorize-ama")).toHaveCount(0);
  await expect(app.getByTestId("integration-revoke-ama")).toHaveCount(0);

  // ⚠️ **The "go to the API Key page" pointer was removed from the card**, and this assertion changed with it.
  //
  // It guards "do not silently block the way out". That concern still holds, but **the way out is not
  // blocked**: "API Key" is **a sibling tab on the same page** (it is in the /preferences tab bar),
  // visible at a glance, with no need for the card to point to it again. And that text also explained
  // "revoke = reset, not rotate", which is for the API Key page itself to say.
  //
  // So the test changed from "the card says so" to **"that tab really exists"**: the tab is the way out
  // itself. Without this rewrite, deleting the pointer would have deleted the guard along with it.
  await expect(app.getByTestId("prefs-tab-apikeys")).toBeVisible();

  // When none of the three buttons exist, the divider at the bottom should not remain either: it says
  // "there is more below", while below is an empty zero-height row. Verified: before the change
  // actionRowChildren=0 while the divider still rendered.
  await expect(
    app.getByTestId("integration-card-ama").locator('[role="separator"], hr'),
  ).toHaveCount(0);
});

// ⚠️ No button in the UI != the capability is turned off.
//
// `canRevoke: false` only stops the frontend from rendering the button, while
// `DELETE /api/integrations/{provider}` is a generic route, and calling the API directly bypasses the
// UI. So "revoke cannot be done" can only be **checked at the data layer**; the `toHaveCount(0)`
// above cannot prove it (this repo has already fallen into it once: "no control in the UI" was taken
// as "the mock will reject").
test("直接打解除授权接口会被拒（藏起按钮挡不住 API）", async ({ app }) => {
  await app.goto("/preferences?tab=integrations");
  await expect(app.getByTestId("integration-card-ama")).toBeVisible();

  const result = await app.evaluate(async () => {
    try {
      await window.__amaApi?.revokeIntegration("ama");
      return { ok: true, message: "" };
    } catch (e) {
      return { ok: false, message: e instanceof Error ? e.message : String(e) };
    }
  });

  expect(result.ok, "解除授权该被拒绝，不该成功——那是一个没有界面的隐藏口子").toBe(false);
  // The rejection message must point to where to deal with it, not just say "no" (the page shows it
  // to the user as-is).
  expect(result.message).toContain("API Key");
});

// ⚠️⚠️ This one is **a guard for a bug that was really hit**, not a formality.
//
// The list used to be filtered with `status === "ok"`, which was wrong in two directions at once:
//
//   one too many: a pure diagnostic item (injects no credentials into the container) has status ok,
//                 so it showed up in the checkbox area: checking it does nothing, while the toggle looks effective;
//   one too few:  AMA has status missing when the server has no key configured, so the whole row
//                 disappeared, the user never knew the capability existed, and would never ask an admin to set it up.
//
// Now the test is `injectable` from the backend (a structural property); status only decides whether
// the checkbox is disabled.
test("授权勾选区只列会注入凭据的源，不列纯诊断项", async ({ app }) => {
  await app.goto("/w/growth/agents/new");

  const box = app.getByTestId("agent-integrations");
  await expect(box).toBeVisible();

  // Injectable and ready ones must be there.
  await expect(app.getByTestId("bind-integration-ama")).toBeVisible();

  /*
   * ⚠️ Neither of these **injects anything into the container**, so neither should appear.
   *
   * The key is `fake-ready-noinject`: the mock has a card made specially for this with
   * **injectable:false + status:"ok"**, which is **the only way to reproduce** the bug "filtering by
   * status lists sources that inject nothing": pure diagnostic sources have exactly this shape (status
   * is ok, but they inject nothing). Reverting the filter to `status === "ok"` makes it appear, and
   * this case turns red.
   *
   * `fake-warning` is also injectable:false (its status is just not ok), asserted along the way.
   */
  await expect(app.getByTestId("bind-integration-fake-ready-noinject")).toHaveCount(0);
  await expect(app.getByTestId("bind-integration-fake-warning")).toHaveCount(0);
});

test("要管理员配的源不出现在 agent 表单里；用户自己能授权的则就地给入口", async ({
  app,
}) => {
  await app.goto("/w/growth/agents/new");
  await expect(app.getByTestId("agent-integrations")).toBeVisible();

  /*
   * ⚠️ **The test is "can the user make it usable themselves", not "is it usable right now".**
   *
   * These two levels used to be one (everything not ok was hidden), and they were split based on real
   * user feedback; the words were "I shouldn't need to go to another page to authorize". So now:
   *
   *   status=missing (needs an admin to configure the server)  still hidden. The person configuring the
   *                                     agent can do nothing about "ask an admin to set
   *                                     auth.managedKeySecret"; a disabled checkbox + a sentence they
   *                                     cannot act on is pure noise.
   *   canAuthorize (one click by themselves)  **must be shown**, with an "Authorize" button right there.
   *                                     Hiding it creates a dead end: not visible in the form -> they
   *                                     don't know the capability exists -> to make it appear they must
   *                                     authorize elsewhere first -> but they don't know to go there.
   *
   * For the first level, "this capability exists" is communicated by the authorization management
   * page; what really must not be silent is **runtime**: when an agent explicitly checked it but gets
   * no credentials, the backend reports an error (locked in by a case in core/sessionenv.go), so a
   * missing row in the UI != a swallowed failure.
   */

  // First assert **the row that should appear really appears**, then assert the other row does not exist.
  // The order is deliberate: `toHaveCount(0)` also holds in the frame where "the whole block has not
  // rendered yet"; waiting for one row to be visible means the render window has passed, otherwise the
  // one below is a false green.
  // The mock has no Feishu user authorization by default -> lark-user is error + canAuthorize.
  const larkUser = app.getByTestId("bind-integration-lark-user");
  await expect(larkUser).toBeVisible();
  // Shown but **cannot be checked**: the status is error (not authorized yet), and checking it could not inject anything.
  // ⚠️ The testid is on the outer wrapper of the HeroUI Checkbox, not the input, so look one level inside.
  await expect(larkUser.locator("input")).toBeDisabled();
  // The way to unlock it must be **right in this row**: that is exactly what that feedback asked for.
  await expect(app.getByTestId("bind-integration-authorize-lark-user")).toBeVisible();

  // The level that needs an admin is still hidden.
  await expect(app.getByTestId("bind-integration-fake-missing")).toHaveCount(0);
});

// ⚠️ This is the second item named in the requirement, and the easiest one to get wrong.
//
// The default goes the opposite way from the old credentialMode (where empty = inject), so "just copy
// over a set of default checks" would violate the requirement. And getting it wrong shows up as
// **credentials injected even though the user checked nothing**, with zero errors.
test("新建 agent 表单默认一个授权都不勾", async ({ app }) => {
  await app.goto("/w/growth/agents/new");

  // The authorization area must really have rendered (otherwise "not checked" below is a false green:
  // an element that does not exist is of course not checked).
  await expect(app.getByTestId("agent-integrations")).toBeVisible();
  await expect(checkbox(app, "bind-integration-ama")).not.toBeChecked();

  // The risk warning only appears after checking; with nothing checked it should not be there.
  await expect(app.getByTestId("agent-integration-risk")).toHaveCount(0);
});

test("勾上授权后出现风险提示，取消后消失", async ({ app }) => {
  await app.goto("/w/growth/agents/new");

  await clickCheckbox(app, "bind-integration-ama");
  await expect(checkbox(app, "bind-integration-ama")).toBeChecked();
  // The warning must say clearly "as the workspace owner": the person checking may not be the owner.
  await expect(app.getByTestId("agent-integration-risk")).toContainText("空间所有者");

  // It can be unchecked. **This is not a formality**: if someone adds a "checked by default" ref gate to
  // this block, the next render after unchecking would fill it back in, showing up as "cannot uncheck".
  await clickCheckbox(app, "bind-integration-ama");
  await expect(checkbox(app, "bind-integration-ama")).not.toBeChecked();
  await expect(app.getByTestId("agent-integration-risk")).toHaveCount(0);
});

test("新建 agent 时勾的授权会存下来", async ({ app }) => {
  await app.goto("/w/growth/agents/new");
  await app.getByTestId("agent-name").fill("带授权的 agent");
  await app.getByTestId("agent-system").fill("你是一个助手。");
  await clickCheckbox(app, "bind-integration-ama");
  await app.getByTestId("agent-submit").click();

  // Landed on the detail page; that item in the config tab should be checked.
  await expect(app.getByRole("heading", { name: "带授权的 agent" })).toBeVisible();
  await expect(checkbox(app, "bind-integration-ama")).toBeChecked();
});

test("自定义 env 能添加、标敏感，并且敏感值回显为掩码", async ({ app }) => {
  await app.goto("/w/growth/agents/new");

  await app.getByTestId("env-add").click();
  await app.getByTestId("env-key-0").fill("DEPLOY_REGION");
  await app.getByTestId("env-value-0").fill("cn-east");

  // Normal item: the value is visible in plaintext and editable.
  await expect(app.getByTestId("env-value-0")).toHaveValue("cn-east");

  await app.getByTestId("env-add").click();
  await app.getByTestId("env-key-1").fill("GRAFANA_TOKEN");
  await app.getByTestId("env-value-1").fill("glsa_super_secret_value");
  // ⚠️ Switch has the same trap as Checkbox: the testid is on the outer wrapper, and clicking the input
  // directly is judged "not visible" (what is really clickable is the label). Same as the pair of
  // helpers in 02-agent.spec.ts.
  await app.getByTestId("env-secret-1").locator("label").click();

  // After marking it sensitive the value becomes a mask and read-only. **If the mask went straight into an
  // editable input, a user saving once without touching it would store the mask itself as the new
  // value**, and that credential would be ruined.
  await expect(app.getByTestId("env-value-1")).not.toHaveValue("glsa_super_secret_value");
  await expect(app.getByTestId("env-value-1")).toHaveAttribute("readonly", "");

  // That note must say three things at once, and "the agent can still read it" matters most: without
  // it, users would mark credentials that really should not go to the agent as sensitive and think
  // they are safe.
  const note = app.getByTestId("agent-env-note");
  await expect(note).toContainText("系统提示词");
  await expect(note).toContainText("掩码");
  await expect(note).toContainText("即可读到明文");
});

test("非法与保留的变量名当场拦住", async ({ app }) => {
  await app.goto("/w/growth/agents/new");

  await app.getByTestId("env-add").click();

  // Starts with a digit: not a valid variable name in shell.
  await app.getByTestId("env-key-0").fill("2FOO");
  await expect(app.getByTestId("env-error-0")).toContainText("不能以数字开头");

  // Same for hyphens.
  await app.getByTestId("env-key-0").fill("MY-VAR");
  await expect(app.getByTestId("env-error-0")).toBeVisible();

  // ⚠️ Platform-reserved names: **this is a security boundary**. Without blocking, a user could set
  // AMA_TOKEN=<stolen key> and make the container call the platform as anyone: privilege escalation.
  await app.getByTestId("env-key-0").fill("AMA_TOKEN");
  await expect(app.getByTestId("env-error-0")).toContainText("平台保留");

  // A valid name has no error.
  await app.getByTestId("env-key-0").fill("MY_VAR");
  await expect(app.getByTestId("env-error-0")).toHaveCount(0);
});

test("重复的变量名只在后一条上报错", async ({ app }) => {
  await app.goto("/w/growth/agents/new");

  await app.getByTestId("env-add").click();
  await app.getByTestId("env-key-0").fill("FOO");
  await app.getByTestId("env-add").click();
  await app.getByTestId("env-key-1").fill("FOO");

  // Only report on the later one: if both rows were red the user would not know which to change.
  await expect(app.getByTestId("env-error-1")).toContainText("重复");
  await expect(app.getByTestId("env-error-0")).toHaveCount(0);
});

test("删掉一行 env 之后其余行不受影响", async ({ app }) => {
  await app.goto("/w/growth/agents/new");

  for (const [i, key] of ["A_VAR", "B_VAR", "C_VAR"].entries()) {
    await app.getByTestId("env-add").click();
    await app.getByTestId(`env-key-${i}`).fill(key);
  }

  // Delete the middle one. Indexes all shift left: **if the implementation does not move the "being
  // edited" marker along, another row suddenly enters edit mode with its value cleared**, zero errors
  // but very strange.
  await app.getByTestId("env-remove-1").click();

  await expect(app.getByTestId("env-key-0")).toHaveValue("A_VAR");
  await expect(app.getByTestId("env-key-1")).toHaveValue("C_VAR");
  await expect(app.getByTestId("env-row-2")).toHaveCount(0);
});

test("编辑已有 agent 时授权与 env 不会让保存按钮凭空亮起", async ({ app }) => {
  // agent_analyst in the seed has two authorizations and two envs (see web/src/data/seed.ts).
  //
  // ⚠️ This one watches the trap of toAgentDraft missing fields: if integrationIds or envs are missing,
  // "the agent read back" and "the draft" are never equal, and **the save button is lit as soon as the
  // page opens and does not go away after clicking**.
  await app.goto("/w/growth/agents/agent_analyst");

  await expect(app.getByTestId("agent-save")).toBeDisabled();

  // The two envs in the seed must show up, with the sensitive one masked.
  await expect(app.getByTestId("env-key-0")).toHaveValue("DEPLOY_REGION");
  await expect(app.getByTestId("env-value-0")).toHaveValue("cn-east");
  await expect(app.getByTestId("env-value-1")).not.toHaveValue("glsa_seed_only_not_real");

  // It should only light up after a change.
  await clickCheckbox(app, "bind-integration-ama");
  await expect(app.getByTestId("agent-save")).toBeEnabled();
});

/**
 * The authorization block non-admins see: **a read-only list + a line "admin permission required"**,
 * neither disappearing nor a row of checkboxes that cannot be clicked.
 *
 * # Why these three must be together
 *
 * This block's design sits where two existing conventions meet, and either can be "simplified" away:
 *
 *   hide the whole block  -> members think the capability does not exist, and don't know whether the agent has credentials
 *   disabled checkboxes   -> a control that can never be clicked is more confusing than no control
 *
 * So the test is three things holding at once: the current state is visible, no checkboxes are
 * visible, and "whom to ask to change it" is visible. Leave out any one assertion and the matching
 * regression goes unguarded.
 */
test("非管理员看到只读的授权清单，而不是勾选框", async ({ app }) => {
  // agent_analyst in the seed injects ama + lark-user (see web/src/data/seed.ts).
  await app.goto("/w/growth/agents/agent_analyst");
  // First confirm the checkboxes are there from the admin's view: without this step, the toHaveCount(0)
  // below might only pass because the page never finished rendering (same as
  // e2e-absence-assertion-needs-render-delay).
  await expect(app.getByTestId("bind-integration-ama")).toBeVisible();

  await app.evaluate(() => window.__amaApi?.setMyRole?.("member"));
  await app.reload();

  // 1. The current state is visible: which two are injected, listed one by one.
  await expect(app.getByTestId("agent-integrations-readonly")).toBeVisible();
  await expect(app.getByTestId("injected-integration-ama")).toBeVisible();
  await expect(app.getByTestId("injected-integration-lark-user")).toBeVisible();

  // 2. No checkboxes at all, including "disabled checkboxes".
  await expect(app.getByTestId("agent-integrations")).toHaveCount(0);
  await expect(app.getByTestId("bind-integration-ama")).toHaveCount(0);

  // 3. Say clearly why it cannot be changed and whom to ask. **This sentence is the whole difference
  // between "read-only" and "gone".**
  await expect(app.getByTestId("agent-integrations-need-admin")).toBeVisible();

  await app.evaluate(() => window.__amaApi?.resetMyRole?.());
});

test("非管理员且一个授权都没注入时，那块显示空态而不是只剩标题", async ({ app }) => {
  // The draft integrationIds on the create page is [].
  //
  // Without testing this, "injects nothing" would degrade into an empty box with only a title, and
  // members could not tell "nothing injected" from "this block is broken".
  await app.goto("/w/growth/agents/agent_analyst");
  await app.evaluate(() => window.__amaApi?.setMyRole?.("viewer"));

  await app.goto("/w/growth/agents/new");
  await expect(app.getByTestId("agent-integrations-readonly")).toBeVisible();
  await expect(app.getByTestId("injected-integration-none")).toBeVisible();
  await expect(app.getByTestId("agent-integrations-need-admin")).toBeVisible();

  await app.evaluate(() => window.__amaApi?.resetMyRole?.());
});

// ---------------------------------------------------------------------------
// Credential aliases (advanced)
// ---------------------------------------------------------------------------

/**
 * ⭐ This block is **collapsed, but its entry must be visible**.
 *
 * The test is "can the user keep going from this UI": the title row is always in the DOM and
 * clickable, and the form only appears after expanding. Hiding it "completely" would leave people
 * who need it stuck: they know the feature exists (the docs say so), but the UI gives no clue.
 */
test("凭据别名默认收起，但入口可见、展开后能配", async ({ app }) => {
  await app.goto("/w/growth/agents/new");

  // 1. The entry is visible (also in the collapsed state).
  const toggle = app.getByTestId("env-alias-toggle");
  await expect(toggle).toBeVisible();
  // 2. Collapsed by default: the form is **not visible**.
  //
  // ⚠️ Assert `toBeHidden`, not `toHaveCount(0)`: when HeroUI's Disclosure is collapsed the content is
  // **still in the DOM** (just not visible), so judging by "does the element exist" would be red, and
  // that red would say "collapsing did not work", pointing in completely the wrong direction.
  // Whether the user can see it is what must be guaranteed here, so judge by visibility.
  await expect(app.getByTestId("env-alias-add")).toBeHidden();

  await toggle.click();
  await expect(app.getByTestId("env-alias-add")).toBeVisible();

  await app.getByTestId("env-alias-add").click();
  await app.getByTestId("env-alias-key-0").fill("VOLC_JWT");

  // ⭐⭐ 3. **No value input box**: this is where "map instead of fill in a value" lands in the UI.
  //
  // Users can only pick which platform credential to reference; the value is filled in by the server.
  // This assertion looks like it tests "something is missing", but it actually locks in the whole
  // design: adding a value input would open a privilege-escalation hole
  // (see the AgentEnvAlias comment in server/internal/domain).
  await expect(app.getByTestId("env-alias-value-0")).toHaveCount(0);

  // 4. The source is a dropdown, not free input.
  await expect(app.getByTestId("env-alias-from-0")).toBeVisible();
});

test("别名的变量名当场校验：保留名与撞名都要拦", async ({ app }) => {
  await app.goto("/w/growth/agents/new");
  await app.getByTestId("env-alias-toggle").click();
  await app.getByTestId("env-alias-add").click();

  // ⚠️ Platform-reserved names: the same security boundary as custom env. If allowed, a user could
  // declare `AMA_TOKEN <- LARKSUITE_CLI_USER_ACCESS_TOKEN` and swap the identity inside the container.
  await app.getByTestId("env-alias-key-0").fill("AMA_TOKEN");
  await expect(app.getByTestId("env-alias-error-0")).toContainText("平台保留");

  // A valid name has no error.
  await app.getByTestId("env-alias-key-0").fill("VOLC_JWT");
  await expect(app.getByTestId("env-alias-error-0")).toHaveCount(0);

  // Same name as a custom env: with both present it is unclear which one takes effect, so it must be blocked on the spot.
  await app.getByTestId("env-add").click();
  await app.getByTestId("env-key-0").fill("VOLC_JWT");
  await expect(app.getByTestId("env-alias-error-0")).toContainText("只能留一个");
});

test("非管理员看到只读的别名清单，而不是编辑器", async ({ app }) => {
  await app.goto("/w/growth/agents/new");
  // First confirm the entry is there from the admin's view: without this step, the toHaveCount(0) below
  // might only pass because the page never finished rendering (same reason as the read-only case above).
  await expect(app.getByTestId("env-alias-toggle")).toBeVisible();

  await app.evaluate(() => window.__amaApi?.setMyRole?.("member"));
  await app.reload();

  // Read-only state: give the conclusion, not the controls.
  await expect(app.getByTestId("agent-env-aliases-readonly")).toBeVisible();
  await expect(app.getByTestId("env-alias-toggle")).toHaveCount(0);
  await expect(app.getByTestId("env-alias-add")).toHaveCount(0);
  // Say clearly why it cannot be changed and whom to ask: the whole difference between "read-only" and "gone".
  await expect(app.getByTestId("env-alias-need-admin")).toBeVisible();

  await app.evaluate(() => window.__amaApi?.resetMyRole?.());
});

/**
 * ⭐⭐ "When authorizing from the agent page, which Feishu app gets authorized": this group guards the
 * defect fixed this time.
 *
 * The user's words: "when authorizing on the agent detail page I need to be able to choose which
 * Feishu authorization".
 *
 * The defect's shape is that two paths used different rules:
 *
 *	injection (what runtime really uses)   pinned to **the workspace the run belongs to**, never falls back
 *	authorize (what the user clicks)       used to follow **the account-level default**, ignoring the workspace
 *
 * So clicking "Authorize" on an agent page in workspace B might authorize the app of workspace A; the
 * agent still could not use lark-cli, and the runtime reported "no user authorization yet for Feishu
 * app cli_xxx", **a sentence that does not point to "you authorized the wrong one"**.
 *
 * In the mock the first app is attached to the seed workspace (ws_demo / growth), and the other two
 * to other workspaces, so "pick by workspace" and "pick by account default" give different answers.
 */
test("agent 表单在授权按钮旁写出「即将授权哪个飞书应用」", async ({ app }) => {
  await app.goto("/w/growth/agents/new");
  await expect(app.getByTestId("agent-integrations")).toBeVisible();

  // First wait for that row to appear, then assert its content; otherwise the assertions below would
  // run on the not-yet-rendered frame.
  await expect(app.getByTestId("bind-integration-lark-user")).toBeVisible();

  const target = app.getByTestId("bind-integration-target-lark-user");
  await expect(target).toBeVisible();
  /*
   * ⚠️ What is asserted is **this workspace's** app (growth -> cli_a0b1c2d3e4f50033), not "the account
   * default one". The two happen to be the same in the mock's default state, so the next case uses a
   * **different** workspace to really tell them apart; this one only locks in "is the target written out".
   *
   * appId must be shown too: in practice one person's several apps were all named "AMA assistant", so
   * giving only the name says nothing.
   */
  await expect(target).toContainText("cli_a0b1c2d3e4f50033");
});

/**
 * ⭐⭐⭐ This is the most important one in the group: **the target app follows the agent's workspace**,
 * instead of staying put with "the account-level default".
 *
 * Mutation check: change the `workspaceId` passed to useIntegrationAuthorize in AgentRuntimeSections
 * to `undefined` (or remove workspaceId from listIntegrationApps), and this goes red: then the agent
 * pages of two workspaces would show the same app.
 */
test("换一个空间的 agent，目标飞书应用跟着换（不是账号级默认那个）", async ({ app }) => {
  // The growth workspace: uses its own app.
  await app.goto("/w/growth/agents/new");
  await expect(app.getByTestId("bind-integration-lark-user")).toBeVisible();
  await expect(app.getByTestId("bind-integration-target-lark-user")).toContainText(
    "cli_a0b1c2d3e4f50033",
  );

  /*
   * Now make the mock's "resolve app by workspace" fail (simulating "this workspace has not connected
   * a Feishu app yet"). So the same page must say something different: **it must not fall back to the
   * account default app**.
   *
   * This hook is used instead of really switching to another workspace: the seed has only one
   * workspace, and "resolvable / not resolvable" is the only fork on this path that changes the conclusion.
   */
  await app.evaluate(() => window.__amaApi?.setLarkUserAppsEmpty?.(true));
  await app.reload();
  await expect(app.getByTestId("bind-integration-lark-user")).toBeVisible();

  // ⭐ The target row disappears, replaced by "this workspace has not connected yet" + a link to connect.
  await expect(app.getByTestId("bind-integration-target-lark-user")).toHaveCount(0);
  const noApp = app.getByTestId("bind-integration-noapp-lark-user");
  await expect(noApp).toBeVisible();
  await expect(noApp.getByRole("link")).toHaveAttribute("href", "/w/growth/feishu");

  // ⭐⭐ And the "Authorize" button must **be put away**: clicking it would always give 422 (the backend
  // cannot resolve the app). A button that always fails is more confusing than no button.
  await expect(app.getByTestId("bind-integration-authorize-lark-user")).toHaveCount(0);
});
