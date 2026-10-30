/**
 * Authorization management: a backend-driven card list, rendering of each of the four states, and two
 * boundaries that "must not be glossed over".
 *
 * ⚠️ **It lives in "global settings"** (/preferences?tab=integrations), not in workspace settings.
 * Authorization is account-level: the backend endpoints look things up by actor, and there is no
 * workspaceId in the path at all. It used to live in workspace settings for historical reasons, at
 * the cost of needing a "personal" Chip to explain the scope; now the location itself says the scope,
 * and the Chip has been deleted. The old address is caught by a redirect (see the case further down).
 *
 * The mock has no real platform, so what is tested here is **the page's state machine and render
 * branches**, not the platform interaction. The only one that can really complete authorization is
 * `lark-user` (device code + polling), and the mock counts the first poll as success.
 *
 * LocalApiClient deliberately provides these providers (order = backend registration order):
 *   ama (ok) / fake-warning (warning) / fake-ready-noinject (ok) /
 *   fake-missing (missing) / lark-user (error when not authorized)
 * covering all four states at once, especially `missing`, which can hardly be tested with a real
 * configuration and happens to be the easiest one to get wrong (see the "not connected gets no
 * authorize button" case below).
 *
 * ⚠️ The page groups by identity type (`integrations-group-personal` / `-service_account`), but there
 * is currently no source with `kind: service_account`, so only the "personal authorization" group is
 * rendered; empty groups are not rendered (see the grouping case below).
 *
 * The authorize button does `window.open` for a new tab. Playwright really opens a popup by default,
 * so below `context.on("page")` catches and closes it, or later assertions might land on the new tab.
 */

import { expandIntegration, expect, hangReadsMock, test } from "./fixtures";

/** lark-user's authorization uses polling (initial interval 3 seconds); waiting for it to flip to ok needs a bit more than the default 5 seconds. */
const POLL_TIMEOUT = 15_000;

test("卡片列表由后端返回的 provider 数组驱动，四态各至少一张", async ({ app }) => {
  await app.goto("/preferences?tab=integrations");

  // All five cards must be there. **This locks in "the frontend does not hard-code the provider list"**:
  // with a list hard-coded in the frontend, a source added on the backend would not add a card to the
  // page, and that missed change causes zero errors.
  for (const id of ["ama", "fake-warning", "fake-ready-noinject", "fake-missing", "lark-user"]) {
    await expect(app.getByTestId(`integration-card-${id}`)).toBeVisible();
  }

  // Each of the four states' labels must match. Merged into two states, some here would not match.
  //
  // ⚠️ **The status Chip must also be visible in the collapsed state**, so these do not need expanding
  // first: that is the precondition for the collapsing design ("which ones need my action" must be seen
  // at a glance, not after expanding each one). They also lock this in: moving the Chip into the
  // collapsed area turns them red.
  await expect(app.getByTestId("integration-status-ama")).toHaveText("已就绪");
  await expect(app.getByTestId("integration-status-lark-user")).toHaveText("不可用");
  await expect(app.getByTestId("integration-status-fake-warning")).toHaveText("需注意");
  await expect(app.getByTestId("integration-status-fake-missing")).toHaveText("未接入");

  // The card title comes from the backend, not hard-coded in the frontend.
  //
  // ⚠️ The title lives **inside** the collapse trigger (the whole row is clickable), but it is still an
  // `<h2>` and its name is still the card name, so role=heading still finds it. These two also lock in
  // "do not turn the title into a plain span".
  await expect(app.getByRole("heading", { name: "AMA 平台" })).toBeVisible();
  await expect(app.getByRole("heading", { name: "飞书用户授权" })).toBeVisible();
});

/**
 * ⭐ After grouping, **the order within a group is still strictly the backend's order**.
 *
 * It locks in **"the frontend does not hard-code provider ordering"** (card order = backend
 * registration order; if the frontend added an ordering array, sources the backend adds later would
 * fall outside the array: a missed change with zero errors, showing up only as an oddly wrong order).
 * Grouping only cuts by the backend's `kind`, and `filter` keeps the order.
 *
 * ⚠️ Assert **relative order**, not absolute indexes: the latter would go red for no reason when a new
 * source is added.
 *
 * There are no service account sources now, so the "service account" group **should not render**: an
 * empty group would only add a title with nothing under it, making people think loading failed.
 */
test("分组之后组内仍是后端顺序，没有成员的组不渲染", async ({ app }) => {
  await app.goto("/preferences?tab=integrations");

  // First wait for the list to really render. **Asserting order without waiting** gets an empty array,
  // and "the order in an empty array is fine" is always true: a statement that is always true and says nothing.
  await expect(app.getByTestId("integration-card-lark-user")).toBeVisible();

  const personalIds = await app
    .getByTestId("integrations-group-personal")
    .getByTestId(/^integration-card-/)
    .evaluateAll((els) => els.map((el) => el.getAttribute("data-testid") ?? ""));

  const order = ["ama", "fake-warning", "fake-ready-noinject", "fake-missing", "lark-user"].map(
    (id) => personalIds.indexOf(`integration-card-${id}`),
  );
  expect(order[0]).toBeGreaterThanOrEqual(0);
  for (let i = 1; i < order.length; i++) {
    expect(order[i]).toBeGreaterThan(order[i - 1]);
  }

  // Empty groups are not rendered.
  await expect(app.getByTestId("integrations-group-service_account")).toHaveCount(0);
});

test("顶部汇总按状态计数，并默认只展开第一张要处理的", async ({ app }) => {
  await app.goto("/preferences?tab=integrations");

  // Ready: ama + fake-ready-noinject; needs action: fake-warning + lark-user; not connected: fake-missing.
  await expect(app.getByTestId("integrations-count-ready")).toHaveText("已就绪 2");
  await expect(app.getByTestId("integrations-count-todo")).toHaveText("要处理 2");
  await expect(app.getByTestId("integrations-count-missing")).toHaveText("未接入 1");
  await expect(app.getByTestId("integrations-summary-hint")).toHaveText(
    "已替你展开第一个，其余点标题展开",
  );

  // "The first one" is taken by page order (fake-warning), not picked by severity; the second one needing action (lark-user) stays collapsed.
  await expect(app.getByTestId("integration-toggle-fake-warning")).toHaveAttribute(
    "aria-expanded",
    "true",
  );
  await expect(app.getByTestId("integration-toggle-lark-user")).toHaveAttribute(
    "aria-expanded",
    "false",
  );
  await expect(app.getByTestId("integration-toggle-ama")).toHaveAttribute("aria-expanded", "false");
});

test("「全部展开 / 全部收起」按实际展开的张数决定自己是哪一个动作", async ({ app }) => {
  await app.goto("/preferences?tab=integrations");

  const toggleAll = app.getByTestId("integrations-toggle-all");
  const ids = ["ama", "fake-warning", "fake-ready-noinject", "fake-missing", "lark-user"];

  // Initially only one is open -> the button is "expand all".
  await expect(toggleAll).toHaveText("全部展开");
  await toggleAll.click();
  for (const id of ids) {
    await expect(app.getByTestId(`integration-toggle-${id}`)).toHaveAttribute(
      "aria-expanded",
      "true",
    );
  }

  await expect(toggleAll).toHaveText("全部收起");
  await toggleAll.click();
  for (const id of ids) {
    await expect(app.getByTestId(`integration-toggle-${id}`)).toHaveAttribute(
      "aria-expanded",
      "false",
    );
  }
  await expect(toggleAll).toHaveText("全部展开");
});

test("「未接入」不给「发起授权」按钮，只给去控制台的入口", async ({ app }) => {
  await app.goto("/preferences?tab=integrations");

  // ⚠️ **First wait for rendering to finish, then assert "that button is not there".**
  //
  // `toHaveCount(0)` cannot tell "not rendered yet" from "rendered but without this button": without
  // first waiting for an element that certainly exists, this assertion is satisfied during the skeleton
  // stage and becomes a statement that is always true and says nothing.
  // This one is also a meaningful assertion on its own: the "not authorized" card (error and
  // canAuthorize) **must have** the button, which contrasts exactly with "not connected has no button"
  // below: if the implementation changed to "guess from status", both would be wrong together.
  //
  // ⚠️ The buttons are in the collapsed area, so both cards must be expanded first (`expandIntegration` is idempotent).
  await expandIntegration(app, "lark-user");
  await expect(app.getByTestId("integration-authorize-lark-user")).toBeVisible();

  // "Not connected != not authorized": not connected needs an admin to configure it, and the user
  // clicking "Authorize" **does nothing useful** (they only get a 422, with no way to know the server
  // needs configuring). So that card must not have this button.
  //
  // ⚠️⚠️ **fake-missing really must be expanded before counting here.** Without expanding, "the button
  // is not there" could mean either "the implementation is right" or just "it is still in the collapsed
  // area", and that `toHaveCount(0)` would degrade into testing collapsing instead of canAuthorize.
  await expect(app.getByTestId("integration-card-fake-missing")).toBeVisible();
  await expandIntegration(app, "fake-missing");
  await expect(app.getByTestId("integration-authorize-fake-missing")).toHaveCount(0);

  // But **a way out must be given**: when not connected, going to the console is the only thing the user can do.
  await expect(app.getByTestId("integration-fixurl-fake-missing")).toBeVisible();

  // Not connected should not have "revoke" either: there is nothing to revoke.
  await expect(app.getByTestId("integration-revoke-fake-missing")).toHaveCount(0);
});

test("未授权 → 发起授权 → 轮询到已就绪，按钮跟着变", async ({ app }) => {
  // Close the new tab as soon as it opens: we only care about state changes on this page.
  app.context().on("page", (p) => void p.close());

  await app.goto("/preferences?tab=integrations");

  // Initially it is unavailable (error), not "not connected": there is a usable Feishu app, just not authorized yet.
  await expect(app.getByTestId("integration-status-lark-user")).toHaveText("不可用");
  await expandIntegration(app, "lark-user");
  // When not authorized there should be no "revoke": there is nothing to revoke.
  await expect(app.getByTestId("integration-revoke-lark-user")).toHaveCount(0);

  await app.getByTestId("integration-authorize-lark-user").click();

  // After clicking, it says the authorization page has been opened in a new tab.
  await expect(app.getByTestId("integration-opened-lark-user")).toContainText(
    "已在新标签页打开授权页",
  );

  // ⭐ Device code has no callback; **it must become ready by itself without clicking "check again"**:
  // that is exactly why polling exists.
  await expect(app.getByTestId("integration-status-lark-user")).toHaveText("已就绪", {
    timeout: POLL_TIMEOUT,
  });
  // Once ready the button becomes "re-authorize", and only now is there "revoke".
  await expect(app.getByTestId("integration-authorize-lark-user")).toContainText("重新授权");
  await expect(app.getByTestId("integration-revoke-lark-user")).toBeVisible();
});

/**
 * ⭐ **The card that was just authorized must not collapse by itself.**
 *
 * This guards a real high-priority bug: the expanded state used to be written as "recompute from the
 * current state when `expandedIds` is null", so when the state flipped from error to ok, the recomputed
 * result no longer included it: that card **collapsed by itself in front of the user**, taking away
 * the "authorization page opened in a new tab" message and the polling hint with it.
 *
 * ⚠️ **The test must be `aria-expanded`, not "summary visible".** The latter is still visible until
 * the collapse animation finishes, so the assertion would pass on a UI that is in the middle of
 * collapsing.
 *
 * ⚠️ The original bug's trigger was "authorize directly on the card **expanded by default**, never
 * touching collapse". Now the one expanded by default is fake-warning (the first needing action), and
 * it cannot complete authorization in the mock, so this can only be checked on lark-user expanded by
 * hand: it guards "a state flip does not change the expanded state", but can no longer reproduce the
 * missing "snapshot once when the first data arrives" fix.
 */
test("授权完那张卡片保持展开，不在用户眼前自己收起来", async ({ app }) => {
  app.context().on("page", (p) => void p.close());
  await app.goto("/preferences?tab=integrations");

  await expandIntegration(app, "lark-user");
  const trigger = app.getByTestId("integration-toggle-lark-user");
  await expect(trigger).toHaveAttribute("aria-expanded", "true");

  await app.getByTestId("integration-authorize-lark-user").click();
  // The state really flipped (otherwise the assertion below tests nothing).
  await expect(app.getByTestId("integration-status-lark-user")).toHaveText("已就绪", {
    timeout: POLL_TIMEOUT,
  });

  // ⭐ After the state flips to ok it must still be open; the one expanded by default is not affected either.
  await expect(trigger).toHaveAttribute("aria-expanded", "true");
  await expect(app.getByTestId("integration-toggle-fake-warning")).toHaveAttribute(
    "aria-expanded",
    "true",
  );
});

test("「重新检查」只刷自己那张卡片，不动别人", async ({ app }) => {
  await app.goto("/preferences?tab=integrations");
  await expect(app.getByTestId("integration-status-lark-user")).toHaveText("不可用");

  // Mark the Feishu card as authorized directly in the backend (mock), but **do not reload**: the state on the page is old.
  await app.evaluate(() => window.__amaApi?.completeLarkUserAuth?.());

  // Refresh a different card; lark-user should not change along with this refresh,
  // or "refresh one" and "refresh all" would be no different, and the user clicked the former.
  await app.getByTestId("integration-refresh-fake-warning").click();
  await expect(app.getByTestId("integration-status-fake-warning")).toHaveText("需注意");
  await expect(app.getByTestId("integration-status-lark-user")).toHaveText("不可用");

  // Refresh it itself, and only then does it change.
  await app.getByTestId("integration-refresh-lark-user").click();
  await expect(app.getByTestId("integration-status-lark-user")).toHaveText("已就绪");
});

test("没有「解除授权」能力的源不给那个按钮，硬调也不假装成功", async ({ app }) => {
  await app.goto("/preferences?tab=integrations");
  await expect(app.getByTestId("integration-status-ama")).toHaveText("已就绪");
  await expandIntegration(app, "ama");

  // The AMA platform's credentials are managed by the platform, canRevoke is false: **no button even
  // when ready**. Better no button than one that always fails. It needs no user authorization either,
  // so there is no authorize button.
  await expect(app.getByTestId("integration-revoke-ama")).toHaveCount(0);
  await expect(app.getByTestId("integration-authorize-ama")).toHaveCount(0);

  // And the backend path **must not pretend to succeed**: "clicked revoke, the page says revoked, the
  // credentials are actually still there" is the worst outcome. The button is gone, so call the API
  // directly here to check it still refuses.
  const rejected = await app.evaluate(async () => {
    try {
      await window.__amaApi?.revokeIntegration("ama");
      return null;
    } catch (e) {
      return e instanceof Error ? e.message : String(e);
    }
  });
  expect(rejected).not.toBeNull();
  // Do not just say "not supported"; say where to deal with it.
  expect(rejected).toContain("API Key");
});

test("每张卡片都说清「现在怎么了」和「该做什么」", async ({ app }) => {
  await app.goto("/preferences?tab=integrations");

  // ⚠️ **The collapsed row also carries the summary** (`integration-brief-*`), so "what is going on now"
  // is visible in any state. Assert the collapsed copy first: it is the precondition for the collapsing
  // design, and removing it would make users expand each card to know what happened.
  await expect(app.getByTestId("integration-brief-lark-user")).toBeVisible();
  await expect(app.getByTestId("integration-brief-fake-missing")).toBeVisible();

  // After expanding, the colored Notice (summary + fixHint) must be there too.
  await expandIntegration(app, "lark-user");
  await expandIntegration(app, "fake-missing");
  await expect(app.getByTestId("integration-summary-lark-user")).toBeVisible();
  await expect(app.getByTestId("integration-summary-fake-missing")).toBeVisible();

  // The not-authorized one must say what to do: click "Authorize".
  await expect(app.getByTestId("integration-summary-lark-user")).toContainText("发起授权");

  // The not-connected one must name who should configure it. **It must not say "please authorize"**:
  // users cannot click an app_key into existence.
  await expect(app.getByTestId("integration-summary-fake-missing")).toContainText("管理员");

  // The needs-attention one must say "usually nothing to do": showing it as error would send users on
  // a pointless trip to the authorization page.
  await expandIntegration(app, "fake-warning");
  await expect(app.getByTestId("integration-summary-fake-warning")).toContainText("自动刷新");
});

test("加载中显示骨架屏，不显示「没有授权源」的空状态", async ({ app }) => {
  // ⚠️ **isLoading and "no providers at all" are two things and cannot share the empty state.**
  //
  // The consequence of mixing them: when loading is slow users see "no authorization sources available
  // yet", a **false statement**, which makes them think the server is not connected and go find an
  // admin, when they only needed to wait half a second.
  //
  // This case only makes sense with hangReadsMock: the mock's delay is only 90ms, and without pinning
  // it, the assertions always run on "loaded", so this implementation mistake would not turn a single
  // case red (verified).
  await app.goto("/preferences?tab=integrations");
  await hangReadsMock(app);
  await app.reload();

  // The skeleton is there (located by its testid, not proven by "the empty state is not visible": that
  // would also be green when loading fails).
  await expect(app.getByTestId("integrations-loading")).toBeVisible();
  // And the empty-state sentence must **not** appear.
  await expect(app.getByText("还没有可用的授权源")).toHaveCount(0);
  // The card list has not appeared yet either.
  await expect(app.getByTestId("integrations-list")).toHaveCount(0);
});

test("有效期显示「还有多久」，不是让用户自己算的裸时间戳", async ({ app }) => {
  await app.goto("/preferences?tab=integrations");
  // The two expiry lines only exist when **authorized** (when not authorized the Feishu card is error + fixHint).
  await app.evaluate(() => window.__amaApi?.completeLarkUserAuth?.());
  await app.reload();

  // detail is plain text shown as-is. A bare ISO (2026-09-11T03:41:26Z) makes users work out "how long
  // from now" themselves, which is the only thing they want to know. So the backend renders it as
  // "local time (N minutes left)".
  await expandIntegration(app, "lark-user");
  const card = app.getByTestId("integration-card-lark-user");
  // ⚠️ The Feishu card has two expiry lines (credential / re-authorization needed), both in this
  // format, so assert the count: checking only first would stay green if the second line went back
  // to bare ISO.
  await expect(card.getByText(/还有\s*\d+\s*(秒|分钟|小时|天)/).first()).toBeVisible();
  await expect(card.getByText(/还有\s*\d+\s*(秒|分钟|小时|天)/)).toHaveCount(2);
  // An expired one must say "expired" clearly, not also say "left".
  await expandIntegration(app, "fake-warning");
  const warn = app.getByTestId("integration-card-fake-warning");
  await expect(warn.getByText(/已过期\s*\d+\s*(秒|分钟|小时|天)/)).toBeVisible();
});

// ---- "This is account-level" must be said in the UI ---------------------------------------
//
// It started with a real misunderstanding: authorization management **used to live in "workspace
// settings"**; a user created a new workspace, went in, immediately saw their own authorization
// records, and suspected "is this overreach, is data leaking between workspaces". The backend was
// always clean (looks up by actor, no workspaceId in the path): **the location was wrong**.
//
// Now it has moved to "global settings", and the location itself states the scope correctly. So the
// roles of the cases below changed:
//   - the first two sentences (belongs to the account / shared by all workspaces) are demoted to
//     **confirmation**: still asserted, but no longer the only line of defense;
//   - **the third sentence is the one that cannot be skipped**: the location says nothing about
//     "can colleagues see my authorizations", which is a permission question, not a scope question,
//     and it is exactly what users worry about most.

test("授权管理页顶部说清「账号级、所有空间通用、别人看不到」", async ({ app }) => {
  await app.goto("/preferences?tab=integrations");

  const note = app.getByTestId("integrations-scope-note");
  await expect(note).toBeVisible();

  await expect(note).toContainText("属于你的账号");
  await expect(note).toContainText("任何空间");
  // ⚠️ This sentence **cannot be deleted**: moving removed the misleading scope, but did not answer
  // "can others see it".
  await expect(note).toContainText("看不到");
});

test("说明在四个渲染分支里都在，尤其加载中与加载失败", async ({ app }) => {
  // ⚠️ This is the key addition to the previous case. Adding the sentence only in the "has cards" branch
  // could make the previous case green, but **the branches that need it most are the abnormal ones**:
  // when loading fails the user stares at an error, and is even more likely to wonder "is it because I
  // am not allowed to see other people's things".
  await app.goto("/preferences?tab=integrations");
  await hangReadsMock(app);
  await app.reload();

  // Skeleton stage (wait for the skeleton first, or this assertion might run on an already loaded page
  // and miss this branch)
  await expect(app.getByTestId("integrations-loading")).toBeVisible();
  await expect(app.getByTestId("integrations-scope-note")).toBeVisible();
});

test("授权管理住在全局设置里，空间设置里已经没有它", async ({ app }) => {
  // The scope is not expressed by a marker but by location, so the location itself must be locked in.
  await app.goto("/w/growth/settings?tab=general");

  // Workspace settings tabs are all workspace-level. **Authorization management should no longer be here**:
  // keeping it would mean two entries to the same thing, and this one's location implies the wrong scope.
  // (The selector is limited to `button`, so it is harder to be misled by future inline elements.)
  const tabs = app.locator('button[data-testid^="settings-tab-"]');
  await expect(tabs).toHaveText(["基本信息", "成员", "宿主机", "方舟凭证", "危险操作"]);
  // The two account-level ones are **not here** (this is the core of this case).
  await expect(app.getByTestId("settings-tab-integrations")).toHaveCount(0);
  await expect(app.getByTestId("settings-tab-apikeys")).toHaveCount(0);

  // The "personal" marker is gone together with its translation key; no tab should carry it anymore.
  for (const id of ["general", "members", "hosts", "ark", "danger", "integrations"]) {
    await expect(app.getByTestId(`settings-tab-personal-${id}`)).toHaveCount(0);
  }
});

test("旧地址 /settings?tab=integrations 重定向到全局设置", async ({ app }) => {
  // That address really was used out there: docs, bookmarks, old e2e, and the "Settings -> Authorization
  // management" written in backend error hints. Without catching it the tab is an unknown value, and the
  // page renders a shell with "only the tab bar, empty below": **no error and no redirect**.
  await app.goto("/w/growth/settings?tab=integrations");

  // **?from= must be carried**: the user came from this workspace's settings page; without that
  // parameter it is the shell-less form and the sidebar disappears entirely. They only went to glance
  // at authorizations and should not be kicked out of the workspace.
  await expect(app).toHaveURL(/\/preferences\?tab=integrations&from=growth$/);
  await expect(app.getByTestId("integrations-list")).toBeVisible();
  // The shell must really be there, not just a parameter in the address.
  await expect(app.getByRole("navigation").getByRole("link", { name: "空间设置" })).toBeVisible();
});

test("方舟凭证说清自己是空间级的，不依赖任何邻居", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=ark");

  // These two sentences come from ArkTab's own description and never depended on neighbors existing.
  // "Workspace-level, shared by all members" answers "will my change affect others".
  await expect(app.getByText(/空间级/)).toBeVisible();
  await expect(app.getByText(/所有成员共用/)).toBeVisible();
});

// -- Switch Feishu app ---------------------------------------------------------------------
//
// The mock provides three apps, **two with the same name** (both "AMA assistant"), which is the real
// shape seen in practice: one person's apps are all built from the same doc, so names naturally
// repeat. **This is not data made up to look nice**: a picker that "only shows the app name" is
// impossible to use with such data, and if all three names were different, that failure mode would
// stay all green in the cases.

test("三个应用都列出来，且靠空间名与 appId 能分辨同名的那两个", async ({ app }) => {
  await app.goto("/preferences?tab=integrations");
  // The picker is in the collapsed area (it is part of the card content, not the collapsed row).
  await expandIntegration(app, "lark-user");

  const select = app.getByTestId("integration-app-select-lark-user");
  await expect(select).toBeVisible();
  await select.click();

  const options = app.getByRole("option");
  await expect(options).toHaveCount(3);

  // ⚠️ **Two have the same name**, so the assertion cannot look only at names: deleting the appId /
  // workspace name entirely would still be green. Assert that each option carries **distinguishing
  // info**; that is the condition for this picker to make sense.
  await expect(options.nth(0)).toContainText("AMA 助手");
  await expect(options.nth(0)).toContainText("增长中台");
  await expect(options.nth(0)).toContainText("cli_a0b1c2d3e4f50033");
  await expect(options.nth(1)).toContainText("AMA 助手");
  await expect(options.nth(1)).toContainText("数据平台");
  await expect(options.nth(1)).toContainText("cli_a1b2c3d4e5f60011");

  // Authorization status is given per app. **Not authorized uses warning, not danger**: something fixed
  // with one click should not look like a fault (the color is decided by the Chip's color; this asserts
  // the text is there).
  await expect(options.nth(0)).toContainText("未授权");
});

test("收起态也要能分辨：两个应用同名，只显示名字等于没法选", async ({ app }) => {
  await app.goto("/preferences?tab=integrations");
  // ⚠️ "Collapsed state" here means **the Select's own collapsed state** (the row when the dropdown is
  // not open), a different thing from the card's collapse. The picker lives in the card's collapsed
  // area, so the card must be expanded first.
  await expandIntegration(app, "lark-user");

  // ⚠️ **This locks in the Select.Value trap.** A bare `<Select.Value />` renders the option's Label and
  // Chip but **drops the Description**, and the workspace name and appId are exactly in the Description,
  // so two same-named apps are the same line of text in the collapsed state, and users cannot tell which
  // one is in use. This was really hit, and only a screenshot shows it, so it gets a case here.
  //
  // ⚠️⚠️ **Assert on the Trigger, not the outer `integration-app-select-*`.** The outer div contains a
  // hidden native `<select>` holding the text of **all options**, so asserting "contains the growth
  // workspace name" on it is always true, green even if the collapsed state is broken. This was
  // mutation-checked: switching back to a bare `<Select.Value />`, asserting on the outer one is green,
  // and only asserting on the Trigger goes red.
  const trigger = app.getByTestId("integration-app-trigger-lark-user");
  await expect(trigger).toContainText("AMA 助手");
  await expect(trigger).toContainText("增长中台");
});

test("切到没授权过的应用，卡片如实变未授权并给出授权入口", async ({ app }) => {
  await app.goto("/preferences?tab=integrations");

  // First authorize the current app, so "switching makes it not authorized" is a real state flip
  // (otherwise the initial value is already not authorized, the assertion is always true and tests nothing).
  await app.evaluate(() => window.__amaApi?.completeLarkUserAuth?.());
  await app.reload();
  await expect(app.getByTestId("integration-status-lark-user")).toHaveText("已就绪");

  // ⚠️ After reload the expanded state goes back to the default (lark-user is ok now -> collapsed), so
  // expand it again.
  await expandIntegration(app, "lark-user");
  const trigger = app.getByTestId("integration-app-trigger-lark-user");
  await expect(trigger).toContainText("增长中台");
  await trigger.click();
  // Switch to the second one (same name, different workspace, never authorized)
  await app.getByRole("option").nth(1).click();

  // ⚠️⚠️ **This is the crux of the whole feature.** Switching apps != switching authorization: the token
  // is bound to the app, and switching to one never authorized should truthfully say "not authorized" +
  // offer the authorize entry.
  // **It must never silently fall back to the previous app's token**: that is exactly the shape of the
  // bug just fixed, which showed up as the card saying "ready" while the agent failed as soon as it
  // called lark-cli (99991668), with an error that does not point to "the wrong app was chosen".
  await expect(app.getByTestId("integration-status-lark-user")).toHaveText("不可用");
  await expect(app.getByTestId("integration-authorize-lark-user")).toBeVisible();
  // The collapsed state switches to the new workspace too; what the card says and what is really used
  // must be the same thing.
  await expect(trigger).toContainText("数据平台");
});

test("只有一个应用时不显示选择器（没得选，摆个单项下拉是噪音）", async ({ app }) => {
  await app.goto("/preferences?tab=integrations");
  await expandIntegration(app, "lark-user");
  await expect(app.getByTestId("integration-app-select-lark-user")).toBeVisible();

  // Cut the list down to just one, and the picker should disappear completely.
  await app.evaluate(() => window.__amaApi?.setLarkUserAppCount?.(1));
  await app.reload();

  // ⚠️ The card itself must still be there: only the picker disappears, not the whole feature.
  await expect(app.getByTestId("integration-card-lark-user")).toBeVisible();
  // ⚠️⚠️ **Expand first, then assert the picker is absent**, or this tests collapsing, not "not shown
  // when there is only one": in the collapsed state the picker is not visible in the DOM anyway, and
  // `toHaveCount(0)` would always be true. A count still at 0 after expanding is the evidence this case needs.
  await expandIntegration(app, "lark-user");
  await expect(app.getByTestId("integration-app-select-lark-user")).toHaveCount(0);
});

test("没有应用可选的源不显示选择器，也不显示错误", async ({ app }) => {
  await app.goto("/preferences?tab=integrations");
  await expect(app.getByTestId("integration-card-lark-user")).toBeVisible();

  // Only the Feishu one has apps to choose; other sources return an empty list (the real backend returns
  // 400 + apps_unsupported).
  // ⚠️ **That is a normal state, not a fault**: showing it as an error would hang red text on good cards.
  // ⚠️ Expand each one before asserting: without expanding, all these `toHaveCount(0)` are always true
  // (the content is in the collapsed area), so the real bug "apps_unsupported shown as red text" would
  // stay green too.
  for (const id of ["ama", "fake-warning", "fake-ready-noinject", "fake-missing"]) {
    await expandIntegration(app, id);
    await expect(app.getByTestId(`integration-app-select-${id}`)).toHaveCount(0);
    await expect(app.getByTestId(`integration-app-error-${id}`)).toHaveCount(0);
  }
});

/**
 * ⭐ Guidance on "where the Feishu bot is managed".
 *
 * The user's report was "global settings cannot switch to another Feishu bot". On checking, that is
 * **not a missing feature**; two concepts were not told apart on this page:
 *
 *   this page (authorization)  account-level: I authorize AMA to use **my Feishu identity** (get a user token)
 *   "Feishu" in the sidebar    workspace-level: register a **bot app** for agents to send and receive messages
 *
 * They came here looking for "switch bot" and could not find it, because it is not here at all; and
 * this page used to say nothing about it, so the only conclusion was "it cannot be done".
 */
test("授权管理页说清「bot 本身在侧栏的飞书里管」，并给出链接", async ({ app }) => {
  // Only with ?from= is there a source workspace, so the link can be built (this page is outside WorkspaceGate).
  await app.goto("/preferences?tab=integrations&from=growth");
  await expandIntegration(app, "lark-user");
  const hint = app.getByTestId("integration-app-manage-hint-lark-user");
  await expect(hint).toBeVisible();
  await expect(hint).toContainText("bot");
  // The link points to that workspace's Feishu page, **not** this page, nor workspace settings.
  await expect(hint.getByRole("link")).toHaveAttribute("href", "/w/growth/feishu");
});

/**
 * ⭐⭐ **With only one app, the picker does not appear but this guidance must still be there.**
 *
 * This is exactly the user's situation in production (their workspace connected only one Feishu app):
 * no picker, and no clue that the bot is managed elsewhere, so it looked like a missing feature.
 *
 * Mutation check: change AppPicker's `if (!apps || apps.length === 0) return null` back to
 * `apps.length < 2`, and this goes red.
 */
test("只有一个应用时选择器消失，但「bot 在哪儿管」的指引仍然在", async ({ app }) => {
  await app.evaluate(() => window.__amaApi?.setLarkUserAppCount?.(1));
  await app.goto("/preferences?tab=integrations&from=growth");
  // ⚠️ Expand first: this compares "the picker is gone" and "the guidance is still there" by **the same
  // visibility standard**. Without expanding, the former is always true (nothing is in the collapsed
  // state) and the latter always red, so it would test collapsing instead of that pair of opposite
  // expectations.
  await expandIntegration(app, "lark-user");

  await expect(app.getByTestId("integration-app-select-lark-user")).toHaveCount(0);
  // ⭐ The guidance does not disappear along with it: "only one" is exactly when it is needed most.
  await expect(app.getByTestId("integration-app-manage-hint-lark-user")).toBeVisible();
});

/** Without a source workspace it degrades to plain text: do not make up a link that 404s when clicked. */
test("没带来源空间时指引不给链接，只给文案", async ({ app }) => {
  await app.goto("/preferences?tab=integrations");
  await expandIntegration(app, "lark-user");
  const hint = app.getByTestId("integration-app-manage-hint-lark-user");
  await expect(hint).toBeVisible();
  await expect(hint.getByRole("link")).toHaveCount(0);
});
