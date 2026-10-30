/**
 * Requirement 4: workspaces have settings for name, key and members.
 */

import { expect, nav, test } from "./fixtures";

test("空间设置是侧栏一级导航项，成员在它的 tab 栏里", async ({ app }) => {
  await app.goto("/w/growth");

  await nav(app, "空间设置").click();
  await expect(app).toHaveURL(/\/w\/growth\/settings$/);
  await expect(app.getByTestId("settings-name")).toBeVisible();
  await expect(nav(app, "空间设置")).toHaveAttribute("aria-current", "page");

  // Members are **not** in the sidebar: it is a tab of the settings page, and another entry in the
  // sidebar would mean two entries to the same destination
  await expect(nav(app, "成员管理")).toHaveCount(0);

  // Entering members from the tab bar, the address uses a query, not a path segment; the sidebar row
  // stays highlighted
  await app.getByTestId("settings-tab-members").click();
  await expect(app).toHaveURL(/\/w\/growth\/settings\?tab=members$/);
  await expect(app.getByTestId("member-list")).toBeVisible();
  await expect(nav(app, "空间设置")).toHaveAttribute("aria-current", "page");
});

test("空间设置的五个 tab 顺序：成员在基本信息之后，危险操作在最后", async ({ app }) => {
  await app.goto("/w/growth/settings");

  // The order is part of the information architecture: members belong with basic info ("what this workspace is").
  //
  // ⚠️ Both account-level tabs **moved to global settings**: API Key is at /preferences?tab=apikeys,
  // and authorization management at /preferences?tab=integrations. So the tabs on this page are **all
  // workspace-level**. See 14-api-keys.spec.ts and 11-integrations.spec.ts.
  //
  // The selector is limited to `button`: harder to be misled by inline elements in the tab bar in the future.
  const tabs = app.locator('button[data-testid^="settings-tab-"]');
  await expect(tabs).toHaveText(["基本信息", "成员", "宿主机", "方舟凭证", "危险操作"]);
  // The two account-level ones are **still not here**.
  await expect(app.getByTestId("settings-tab-integrations")).toHaveCount(0);
  await expect(app.getByTestId("settings-tab-apikeys")).toHaveCount(0);
});

test("旧地址 /settings/members 重定向到 ?tab=members", async ({ app }) => {
  // Docs, bookmarks and old links have all pointed to this path segment; it must not land on the
  // fallback route back to the workspace list
  await app.goto("/w/growth/settings/members");
  await expect(app).toHaveURL(/\/w\/growth\/settings\?tab=members$/);
  await expect(app.getByTestId("member-list")).toBeVisible();
});

test("成员 tab 只有一个页头：h1 归设置页，区块标题是卡内的 h2", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=members");

  // The version where members had their own page had its own PageHeader; keeping it after turning back
  // into a tab would give one screen two titles. The test is "the whole page has only one h1", not
  // "some sentence cannot be found": the latter breaks as soon as the wording changes.
  await expect(app.locator("h1")).toHaveCount(1);
  await expect(app.locator("h1")).toHaveText("空间设置");

  // The section title must not go away together with the page header: it is the h2 rendered by SectionTitle
  await expect(app.getByRole("heading", { level: 2, name: "成员" })).toBeVisible();

  // "Invite member" is still there, attached to the section title row instead of the page header
  await expect(app.getByTestId("invite-member")).toBeVisible();
});

test("能修改空间名称", async ({ app }) => {
  await app.goto("/w/growth/settings");

  await app.getByTestId("settings-name").fill("增长中台 V2");
  await app.getByTestId("settings-save").click();
  await expect(app.getByText("已保存")).toBeVisible();

  await app.reload();
  await expect(app.getByTestId("settings-name")).toHaveValue("增长中台 V2");
});

test("修改 key 会警示旧链接失效并跳转到新地址", async ({ app }) => {
  await app.goto("/w/growth/settings");

  await app.getByTestId("settings-key").fill("growth-v2");
  await expect(app.getByText(/旧链接失效/)).toBeVisible();

  await app.getByTestId("settings-save").click();
  await expect(app).toHaveURL(/\/w\/growth-v2\//);

  // The new address works; the old one falls back to the start page
  await app.goto("/w/growth-v2/agents");
  await expect(app.locator('[data-testid^="agent-card-"]').first()).toBeVisible();
  await app.goto("/w/growth/agents");
  await expect(app).toHaveURL(/\/workspaces$/);
});

test("key 的格式与唯一性都会校验", async ({ app }) => {
  // First create another workspace to occupy the key
  await app.goto("/workspaces");
  await app.getByTestId("new-workspace").click();
  await app.getByTestId("ws-name").fill("占位空间");
  await app.getByTestId("ws-key").fill("taken-key");
  await app.getByTestId("ws-submit").click();
  await expect(app).toHaveURL(/\/w\/taken-key$/);

  await app.goto("/w/growth/settings");
  await app.getByTestId("settings-key").fill("Bad Key");
  await app.getByTestId("settings-save").click();
  await expect(app.getByText("只能用小写字母、数字和连字符，且以字母或数字开头")).toBeVisible();

  await app.getByTestId("settings-key").fill("taken-key");
  await app.getByTestId("settings-save").click();
  await expect(app.getByText("Key「taken-key」已被占用")).toBeVisible();
});

test("能邀请成员、改角色、移除成员", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=members");

  const rows = app.getByTestId("member-row");
  // 4 members in the seed: one each of owner/admin/member/viewer.
  await expect(rows).toHaveCount(4);

  await app.getByTestId("invite-member").click();
  await app.getByTestId("member-email").fill("erin@example.com");
  await app.getByTestId("member-name").fill("Erin");
  await app.getByTestId("member-role").click();
  await app.getByRole("option", { name: "管理员" }).click();
  await expect(app.getByTestId("member-role")).toContainText("管理员");
  await app.getByTestId("modal-submit").click();
  await expect(rows).toHaveCount(5);
  const erinRow = rows.filter({ hasText: "erin@example.com" });
  await expect(erinRow).toContainText("Erin");
  // The role chosen when inviting really was stored, not the default "member"
  await expect(app.getByTestId("role-erin@example.com")).toContainText("管理员");

  // Change role
  await app.getByTestId("role-erin@example.com").click();
  await app.getByRole("option", { name: "只读" }).click();
  await expect(app.getByTestId("role-erin@example.com")).toContainText("只读");

  // Remove
  await app.getByTestId("remove-erin@example.com").click();
  await app.getByTestId("confirm-danger").click();
  // After removing, back to the seed's 4.
  await expect(rows).toHaveCount(4);
});

test("邮箱先去空白、转小写再入库，名字留空时取 @ 前面那段", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=members");
  await app.getByTestId("invite-member").click();

  // Mixed case + leading/trailing spaces: what is stored must be the normalized one (the member's
  // unique key is the lowercase email).
  await app.getByTestId("member-email").fill("  Frank@Example.COM ");
  await app.getByTestId("modal-submit").click();

  const row = app.getByTestId("member-row").filter({ hasText: "frank@example.com" });
  await expect(row).toBeVisible();
  // No name filled in -> defaults to the part of the email before @
  await expect(row.getByText("frank", { exact: true })).toBeVisible();
  // No role chosen -> defaults to "member"
  await expect(app.getByTestId("role-frank@example.com")).toContainText("成员");
  // The original (not normalized) one should not appear. It must be exact: getByText defaults to
  // case-insensitive substring matching, and without it would hit the normalized row, making the
  // assertion always red (or always green if written as "appears").
  await expect(app.getByText("Frank@Example.COM", { exact: true })).toHaveCount(0);
});

test("成员列表没有头像时落回首字母", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=members");
  await expect(app.getByTestId("member-row")).toHaveCount(4);

  // Seed members have no avatarUrl field, so it must fall back to the initial rather than an empty circle:
  // the <img> does not exist at all (Radix only renders it after the image has loaded), while the initial is there.
  // The seed owner has a Chinese name, so the initial is its first Chinese character.
  await expect(app.getByTestId("member-avatar-alice@example.com")).toHaveCount(0);
  const seedRow = app.getByTestId("member-row").filter({ hasText: "alice@example.com" });
  await expect(seedRow.getByText("张", { exact: true })).toBeVisible();

  // The newly invited person has no avatar either; the initial comes from the name (here the default "grace").
  await app.getByTestId("invite-member").click();
  await app.getByTestId("member-email").fill("grace@example.com");
  await app.getByTestId("modal-submit").click();
  await expect(app.getByTestId("member-avatar-grace@example.com")).toHaveCount(0);
  const graceRow = app.getByTestId("member-row").filter({ hasText: "grace@example.com" });
  await expect(graceRow.getByText("g", { exact: true })).toBeVisible();
});

test("邮箱无效会被拦住，重复邀请也会", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=members");
  const rows = app.getByTestId("member-row");
  await expect(rows).toHaveCount(4);

  // Submit without filling in anything
  await app.getByTestId("invite-member").click();
  await app.getByTestId("modal-submit").click();
  await expect(app.getByTestId("member-error")).toContainText("请填写有效的邮箱地址");

  // A malformed email
  await app.getByTestId("member-email").fill("not-an-email");
  // The input error goes away as soon as the input changes; the previous error must not linger
  await expect(app.getByTestId("member-error")).toHaveCount(0);
  await app.getByTestId("modal-submit").click();
  await expect(app.getByTestId("member-error")).toContainText("请填写有效的邮箱地址");
  await expect(rows).toHaveCount(4);

  // Already a member (bob@example.com is already in this workspace in the seed)
  await app.getByTestId("member-email").fill("bob@example.com");
  await app.getByTestId("modal-submit").click();
  await expect(app.getByText(/已经是该空间成员/)).toBeVisible();
  await expect(rows).toHaveCount(4);
});

test("唯一的拥有者不能被移除或降级", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=members");

  // The seed has only one owner: the remove button is disabled, and the role has no dropdown
  await expect(app.getByTestId("remove-alice@example.com")).toBeDisabled();
  await expect(app.getByTestId("role-alice@example.com")).toHaveCount(0);
});

test("删除空间会清掉空间内的全部资源", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=danger");

  await app.getByTestId("delete-workspace").click();
  await app.getByTestId("confirm-danger").click();

  // The only workspace was deleted -> back to the workspace list page, with an empty list
  await expect(app).toHaveURL(/\/workspaces$/);
  await expect(app.getByRole("heading", { name: "还没有可进入的空间" })).toBeVisible();
});

test("拥有者排在最前面，其余按角色降序", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=members");

  // "Whose workspace this is" is the most important piece of info in this list.
  const rows = app.getByTestId("member-section-people").getByTestId("member-row");
  await expect(rows).toHaveCount(4);
  await expect(rows.nth(0)).toContainText("alice@example.com");
  await expect(rows.nth(0)).toContainText("拥有者");
  // The rest are sorted by role, descending (admin -> member -> read-only).
  await expect(rows.nth(1)).toContainText("bob@example.com");
  await expect(rows.nth(2)).toContainText("carol@example.com");
  await expect(rows.nth(3)).toContainText("dave@example.com");
});

// ---------------------------------------------------------------------------
// Role permission table (the collapsible section at the bottom of the members page)
//
// The table content **is generated** (source of truth server/authz/matrix.go, see the file header of
// web/src/components/RolePermissionTable.tsx), so this **deliberately does not assert "how many rows"
// or "how many checks in a row"**: such assertions would go red when someone adds a route, and then
// the red is not a bug but the table correctly growing. Drift is guarded by the Go-side reconciliation
// test (server/shared/gopkg/authzdoc/); the frontend only guards "UI behavior" and "that
// counter-intuitive fact".
// ---------------------------------------------------------------------------

test("角色权限说明默认收起，点击能展开、再点能收回", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=members");

  // ⚠️ **First wait for the member list to render**: this is the required gate before "asserting
  // something should not appear". Without it, the hidden assertion below would be satisfied in the frame
  // before the page renders (the repo note e2e-absence-assertion-needs-render-delay).
  await expect(app.getByTestId("member-row").first()).toBeVisible();

  const toggle = app.getByTestId("role-permissions-toggle");
  await expect(toggle).toBeVisible();

  // ⚠️ **The test is `toBeHidden()`, not `toHaveCount(0)`.** When HeroUI's Disclosure is collapsed, the
  // content **is still in the DOM** (hidden with aria-hidden), so a count assertion would always be true
  // and test nothing. Exactly the same source as the one in 32-injected-context.
  await expect(app.getByTestId("role-permissions-body")).toBeHidden();

  // -- Expand --
  await toggle.click();
  const body = app.getByTestId("role-permissions-body");
  await expect(body).toBeVisible();

  // All four roles are in the header (these four are the full set of domain.WorkspaceRole and do not
  // change as routes are added or removed).
  await expect(app.getByTestId("role-column-owner")).toContainText("拥有者");
  await expect(app.getByTestId("role-column-admin")).toContainText("管理员");
  await expect(app.getByTestId("role-column-member")).toContainText("成员");
  await expect(app.getByTestId("role-column-viewer")).toContainText("只读");

  // Spot-check that one row of each of the three levels **exists** (not asserting the number of checks;
  // see the start of this section for why). These three ids are stable identifiers in capabilityDecls;
  // only changing them should make this case red.
  await expect(app.getByTestId("role-capability-view-content")).toBeVisible();
  await expect(app.getByTestId("role-capability-edit-agent")).toBeVisible();
  await expect(app.getByTestId("role-capability-manage-member")).toBeVisible();

  // -- Click again to collapse --
  // Testing only expand would miss "clicked and it won't collapse", the most common way a collapsible breaks.
  await toggle.click();
  await expect(body).toBeHidden();
});

test("表下必须说清「拥有者与管理员权限相同」，以及非成员与账号级功能", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=members");
  await expect(app.getByTestId("member-row").first()).toBeVisible();
  await app.getByTestId("role-permissions-toggle").click();
  await expect(app.getByTestId("role-permissions-body")).toBeVisible();

  // ⚠️⚠️ **This is the easiest place to get this table wrong, so it gets its own case.**
  //
  // "Owner has more permission than admin" is a strong intuition, and the fact is the opposite: in
  // authz.levelOf both are LevelAdmin, and owner only has one extra protection, "cannot be removed".
  // When those two columns in the table are identical, users' first reaction is "this table is
  // unfinished", so text must point out "being the same is correct".
  //
  // Without that sentence, users would think they are looking at an incomplete table, and go ask an
  // admin for a difference that does not exist.
  const ownerNote = app.getByTestId("role-note-owner");
  await expect(ownerNote).toContainText("完全相同");
  await expect(ownerNote).toContainText("不能被移除");

  // Non-members: deliberately not drawn as a fifth column (that would suggest they can get in but just
  // cannot do anything).
  await expect(app.getByTestId("role-note-non-member")).toContainText("无法访问");

  // Why account-level features are not in the table: otherwise users looking for "authorization
  // management" would not find it and suspect the table is missing something.
  const accountNote = app.getByTestId("role-note-account-scope");
  await expect(accountNote).toContainText("账号级");
  await expect(accountNote).toContainText("全局设置");
});
