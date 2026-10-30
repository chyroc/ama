/**
 * Global settings (account-level): appearance and language, API Key.
 *
 * These all belong to **this person**, so the route is outside any workspace (/preferences).
 * The API Key tab's cases are in 14-api-keys.spec.ts; this only checks the page frame itself.
 *
 * # This page has only one route; the shell is decided by `?from=<key>`
 *
 *   /preferences               no shell (back goes to the workspace list)
 *   /preferences?from=growth   with shell (sidebar there, current workspace highlighted, back returns to that workspace)
 *
 * The "shell" group below is **the main line of defense** for this area: it went wrong here twice,
 * in opposite directions with the same symptom: "one click kicked me out of the workspace I was
 * working in". The parameter name is a hard-coded literal (`from`), reconciled with
 * FROM_WORKSPACE_PARAM in `src/app/preferencesLink.ts`: that is an external contract (addresses go
 * into bookmarks and docs), and with a shared constant, renaming it would not turn a single one of
 * these assertions red.
 */

import {
  allowWorkspacesMock,
  denyWorkspaceMock,
  expect,
  readServerPrefs,
  setServerPrefs,
  test,
} from "./fixtures";

/** Preferences live in their own localStorage key, separate from the per-workspace ama.store. */
async function setPrefs(
  page: import("@playwright/test").Page,
  prefs: { language?: string; appearance?: string },
) {
  await page.goto("/workspaces");
  await page.evaluate((p) => {
    localStorage.setItem(
      "ama.prefs.v1",
      JSON.stringify({ language: "zh-CN", appearance: "system", ...p }),
    );
  }, prefs);
}

test("偏好设置页在 workspace 之外，无空间也能进", async ({ page }) => {
  await page.goto("/workspaces");
  await page.evaluate(() => window.__amaApi?.clear());

  // It opens even with no workspaces at all: it does not depend on a workspace context
  await page.goto("/preferences");
  await expect(page.getByTestId("prefs-language")).toBeVisible();
  await expect(page.getByTestId("prefs-appearance-dark")).toBeVisible();
});

test("从空间列表和侧栏「账号」组都能进入全局设置", async ({ app }) => {
  await app.goto("/workspaces");
  await app.getByTestId("open-preferences").click();
  // Entering from the workspace list **carries no from**: there is no workspace context anyway, so the
  // shell-less form is right.
  await expect(app).toHaveURL(/\/preferences$/);

  // Entering from the sidebar inside a workspace. **It is in the "Account" group, not the "Manage"
  // group**: the manage group is workspace-level configuration (changes affect everyone), while global
  // settings only affect yourself; the two sit next to each other, have similar icons, and are both
  // named "xx settings", so the group title is the only thing here that states the scope.
  await app.goto("/w/growth");
  await app.getByRole("navigation").getByRole("link", { name: "全局设置" }).click();
  // Entering from inside a workspace **must carry from**: it is the only switch for "with shell";
  // missing it makes the sidebar disappear.
  await expect(app).toHaveURL(/\/preferences\?from=growth$/);
});

test("全局设置从账号菜单里移走了，不留第二个入口", async ({ app }) => {
  // Two entries to the same destination make people think they are different things (that is why
  // members were once moved from top-level navigation back into a tab). So after moving to the
  // sidebar, the item in the account menu must be deleted.
  await app.goto("/w/growth");
  await app.getByLabel("账号菜单").click();

  await expect(app.getByRole("menuitem", { name: "偏好设置" })).toHaveCount(0);
  await expect(app.getByRole("menuitem", { name: "全局设置" })).toHaveCount(0);
  // Sign out is still there: the account menu is the only place it makes sense.
  await expect(app.getByRole("menuitem", { name: "退出登录" })).toBeVisible();
});

test("侧栏「账号」组与「管理」组分开，范围由分组标题说清", async ({ app }) => {
  await app.goto("/w/growth");
  const nav = app.getByRole("navigation");

  // Both group titles must be there, each covering only its own kind.
  await expect(nav.getByText("管理", { exact: true })).toBeVisible();
  await expect(nav.getByText("账号", { exact: true })).toBeVisible();
  await expect(nav.getByRole("link", { name: "空间设置" })).toBeVisible();
  await expect(nav.getByRole("link", { name: "全局设置" })).toBeVisible();
});

// -- Shell: the two forms with and without ?from= ----------------------------------------------
//
// This group is the main line of defense for this area. **It went wrong here twice, in opposite
// directions with the same symptom**:
//
//   1. the sidebar pointed to the shell-less /preferences       -> sidebar disappears, back goes to the workspace list
//   2. changed to two mount points (/w/:key/preferences)        -> works, but four links each decided on their own; missing one gives the same result
//
// Now it is one route + `?from=<key>`. Each case maps to one specific failure.

/**
 * Assert this page is currently in the **shell-less form**, and the shell does not show up a moment later.
 *
 * # Why `toHaveCount(0)` alone is not enough
 *
 * The shell only renders after `workspace-by-key` returns (mock ~90ms). So before that the nav does
 * not exist anyway, and `toHaveCount(0)` **is satisfied immediately**: a wrong implementation (falling
 * back to some workspace and forcing the shell when the workspace cannot be loaded) would still pass.
 * This trap was really hit: after writing a wrong "degrade" implementation, two of the three
 * shell-less cases were still green.
 *
 * So the shell must get **enough time to show up** before asserting it did not. This is one of the
 * few cases that "must wait a fixed time": what must be proven is "something never appeared", and a
 * retrying assertion only passes the first time it is satisfied, which here is the earliest and least
 * trustworthy frame.
 *
 * 500ms is 5x the mock's 90ms delay; cases with a shell do the reverse and use retrying assertions to
 * wait for the nav to appear. The asymmetry is deliberate.
 */
async function expectNoShell(page: import("@playwright/test").Page) {
  // First confirm the page content really rendered; otherwise "nothing at all" would also satisfy the assertions below.
  await expect(page.getByTestId("prefs-language")).toBeVisible();

  await page.waitForTimeout(500);

  // Neither marker of the shell may be present: the sidebar navigation, and the workspace switcher at the top.
  await expect(page.getByRole("navigation")).toHaveCount(0);
  await expect(page.getByTestId("workspace-switcher")).toHaveCount(0);
  // Back can only go to the workspace list: with no workspace context it is the only place that makes sense.
  await expect(page.getByTestId("prefs-back")).toContainText("空间列表");
}

test("带 ?from= 时侧栏还在，且当前空间高亮（不会被踢出空间）", async ({ app }) => {
  // ⚠️ This really happened: when global settings were only mounted at the shell-less /preferences,
  // clicking in from the sidebar made **the whole shell disappear, with the workspace settings on the
  // left gone**: the user only wanted to change the color scheme, and got kicked out of the workspace
  // they were working in.
  await app.goto("/w/growth");
  await app.getByRole("navigation").getByRole("link", { name: "全局设置" }).click();

  await expect(app.getByTestId("prefs-language")).toBeVisible();

  // The shell must still be there: the sidebar items are always clickable, so users can get back.
  const nav = app.getByRole("navigation");
  await expect(nav.getByRole("link", { name: "空间设置" })).toBeVisible();
  await expect(nav.getByRole("link", { name: "Agents" })).toBeVisible();
  // The workspace switcher at the top must also be there: it answers "which workspace am I still in".
  await expect(app.getByTestId("workspace-switcher")).toContainText("增长");

  // **The current workspace must be highlighted**: a sidebar whose every item points to another
  // workspace means the shell is fake. The test is that links in the sidebar really include growth
  // (NavLink's base comes from the workspace in context).
  await expect(nav.getByRole("link", { name: "空间设置" })).toHaveAttribute(
    "href",
    "/w/growth/settings",
  );
  // The "Global settings" item itself must also carry from, or clicking it again on this page drops out of the shell.
  await expect(nav.getByRole("link", { name: "全局设置" })).toHaveAttribute(
    "href",
    "/preferences?from=growth",
  );
});

test("带 ?from= 时「返回」回到那个空间，不是空间列表", async ({ app }) => {
  // ⚠️ Same as above, this really happened too: the back link was hard-coded to /workspaces, so a user
  // who came in from a workspace to change the color scheme was sent back to the workspace list on
  // clicking back, though they never meant to leave that workspace.
  await app.goto("/preferences?from=growth");

  const back = app.getByTestId("prefs-back");
  // The text must include the workspace name: this page looks the same in both contexts, and the
  // name is the only clue to "where clicking takes me".
  await expect(back).toContainText("增长");
  await back.click();

  await expect(app).toHaveURL(/\/w\/growth$/);
});

test("不带 ?from= 时是无壳形态，且不崩", async ({ page }) => {
  // The user visits /preferences directly (a bookmark, or from the workspace list). This page's content
  // is account-level, and it must open even without a workspace context: **newcomers who belong to no
  // workspace need to come here first** to configure Ark credentials and API Keys.
  await page.goto("/preferences");

  // The shell should **not** appear: with no current workspace, a drawn sidebar could only be fake
  // (where would each item point?).
  await expectNoShell(page);

  // Back really works.
  await page.getByTestId("prefs-back").click();
  await expect(page).toHaveURL(/\/workspaces$/);
});

test("?from= 给不存在的 key → 降级成无壳，不白屏不报错", async ({ page }) => {
  // The URL can be changed by the user, and the key may not exist at all (the workspace was deleted, or
  // it was mistyped). **The page must not crash**: its content is account-level and should be visible
  // anyway; blocking people from their own settings for a purely navigational shell gets priorities backwards.
  await page.goto("/preferences?from=no-such-workspace");

  // The content is there and the shell degrades: the same path as "no parameter at all".
  await expectNoShell(page);

  // It must not jump away either (WorkspaceGate does a Navigate to /workspaces for unknown keys, and
  // this page deliberately does **not**: that would be "clicked settings and got bounced back to the
  // workspace list").
  await expect(page).toHaveURL(/\/preferences\?from=no-such-workspace$/);
  // The gateway's error panels must not show up either.
  await expect(page.getByTestId("workspace-forbidden")).toHaveCount(0);
});

test("?from= 给非成员的空间（403）→ 同样降级成无壳", async ({ app }) => {
  // **RBAC makes this the normal case**: a non-member accessing workspace-by-key gets 403 directly.
  // Someone sharing a link with ?from= is exactly this situation, and a 403 takes a different code path
  // from "does not exist" (catch rather than null), so both must be locked in separately.
  //
  // ⚠️ Use denyWorkspaceMock, **not** failReadsMock: the latter does not touch getWorkspaceByKey, so
  // the 403 never happens, and this case would quietly run on the success path; it was "green" exactly
  // that way for a while (see the notes in fixtures.ts).
  await denyWorkspaceMock(app, "growth");
  try {
    await app.goto("/preferences?from=growth");

    // The page content is still visible and the shell degrades: that is the whole point of this case.
    await expectNoShell(app);
    // Neither of the two no-access panels should appear on this page (they belong to the gateway and
    // the list page, not this page).
    await expect(app.getByTestId("workspace-forbidden")).toHaveCount(0);
    await expect(app.getByTestId("load-denied")).toHaveCount(0);
  } finally {
    // The marker lives in a separate key that reset() cannot wipe; missing the cleanup makes the whole
    // later batch unable to open workspaces.
    await allowWorkspacesMock(app);
  }
});

test("旧地址 /w/:key/preferences 重定向到 /preferences?from=:key", async ({ app }) => {
  // That address **really was used out there** (a released version had it, and docs, bookmarks and old
  // e2e all pointed to it). Without catching it, it lands on the fallback route and jumps back to the
  // workspace list, which is exactly the "clicking a bookmark kicked me out of the workspace" this area
  // keeps trying to remove.
  await app.goto("/w/growth/preferences");
  await expect(app).toHaveURL(/\/preferences\?from=growth$/);
  // The shell must really follow, not just the address being right.
  await expect(app.getByRole("navigation").getByRole("link", { name: "空间设置" })).toBeVisible();

  // The tab must be carried over as-is, or clicking "go to global settings (Ark credentials)" lands
  // on the default appearance page.
  await app.goto("/w/growth/preferences?tab=ark");
  await expect(app).toHaveURL(/\/preferences\?tab=ark&from=growth$/);
});

test("空间内的全局设置与空间外看到的是同一份数据", async ({ app }) => {
  // Both shell forms render the same component and read the same account-level data. If they forked,
  // "I changed the color scheme inside a workspace and it changed back on the workspace list" would be
  // an inconsistency nobody could make sense of.
  await app.goto("/preferences?from=growth");
  await app.getByTestId("prefs-appearance-dark").click();
  await expect(app.locator("html")).toHaveClass(/dark/);

  await app.goto("/preferences");
  await expect(app.locator("html")).toHaveClass(/dark/);
});

test("切换到深色后立即生效且刷新后保持", async ({ app }) => {
  await app.goto("/preferences");

  await app.getByTestId("prefs-appearance-dark").click();

  // Both the class on html and the actual background color must change; checking only the class name
  // might be spinning idle
  await expect(app.locator("html")).toHaveClass(/dark/);
  const darkBg = await app.evaluate(() => getComputedStyle(document.body).backgroundColor);

  await app.getByTestId("prefs-appearance-light").click();
  await expect(app.locator("html")).toHaveClass(/light/);
  const lightBg = await app.evaluate(() => getComputedStyle(document.body).backgroundColor);
  expect(darkBg).not.toBe(lightBg);

  // Back to dark and refresh; the preference should persist
  await app.getByTestId("prefs-appearance-dark").click();
  await app.reload();
  await expect(app.locator("html")).toHaveClass(/dark/);
  expect(await app.evaluate(() => getComputedStyle(document.body).backgroundColor)).toBe(darkBg);
});

test("深色在业务页面同样生效，不只是设置页", async ({ app }) => {
  await setPrefs(app, { appearance: "dark" });

  // Pick the event view: this case needs "a business page with plenty of content", and the vertical
  // timeline spreads out more elements than the chat view folded by turns, so it exposes missed colors better
  await app.goto("/w/growth/runs/run_1?view=events");
  await expect(app.getByTestId("evt-list")).toBeVisible();
  await expect(app.locator("html")).toHaveClass(/dark/);

  // Body text and background must differ in color, otherwise it is the same as not rendering
  const { bg, fg } = await app.evaluate(() => {
    const cs = getComputedStyle(document.body);
    return { bg: cs.backgroundColor, fg: cs.color };
  });
  expect(bg).not.toBe(fg);
});

test("跟随系统模式会解析成系统当前的外观", async ({ page }) => {
  // First make the system preference dark
  await page.emulateMedia({ colorScheme: "dark" });
  await setPrefs(page, { appearance: "system" });
  await page.goto("/preferences");
  await expect(page.locator("html")).toHaveClass(/dark/);
  await expect(page.getByText("当前系统为深色")).toBeVisible();

  // The system switches to light, and the UI should follow in real time (no refresh needed)
  await page.emulateMedia({ colorScheme: "light" });
  await expect(page.locator("html")).toHaveClass(/light/);
});

test("切换语言后界面文案随之改变", async ({ app }) => {
  await app.goto("/preferences");
  await expect(app.getByRole("heading", { name: "全局设置" })).toBeVisible();

  await app.getByTestId("prefs-language").click();
  await app.getByRole("option", { name: "English" }).click();

  await expect(app.getByRole("heading", { name: "Global Settings" })).toBeVisible();
  await expect(app.locator("html")).toHaveAttribute("lang", "en-US");

  // Business pages must follow too, not only the settings page
  await app.goto("/w/growth/agents");
  await expect(app.getByRole("heading", { name: "Agents" })).toBeVisible();
  await expect(app.getByRole("navigation").getByRole("link", { name: "Run History" })).toBeVisible();

  // Kept after a refresh
  await app.reload();
  await expect(app.getByRole("navigation").getByRole("link", { name: "Skills" })).toBeVisible();
});

test("偏好与空间无关：切空间不影响外观与语言", async ({ app }) => {
  await setPrefs(app, { appearance: "dark", language: "en-US" });

  await app.goto("/workspaces");
  await app.getByTestId("new-workspace").click();
  await app.getByTestId("ws-name").fill("Another Space");
  await app.getByTestId("ws-key").fill("another");
  await app.getByTestId("ws-submit").click();
  await expect(app).toHaveURL(/\/w\/another$/);

  // In the new workspace both appearance and language stay the same
  await expect(app.locator("html")).toHaveClass(/dark/);
  await expect(app.locator("html")).toHaveAttribute("lang", "en-US");
});

test("坏掉的偏好数据不会让应用起不来", async ({ page }) => {
  await page.goto("/workspaces");
  await page.evaluate(() => {
    localStorage.setItem("ama.prefs.v1", '{"language":"kl-KL","appearance":"neon"}');
  });
  await page.goto("/preferences");

  // Invalid values should fall back to defaults field by field, not a blank screen
  await expect(page.getByTestId("prefs-language")).toBeVisible();
  await expect(page.locator("html")).toHaveAttribute("lang", "zh-CN");
});

test("全局设置的 tab 齐全，缺省停在外观与语言", async ({ app }) => {
  await app.goto("/preferences");

  // The default tab must be appearance and language: existing users of this page (formerly called
  // "Preferences") should see what was there before on opening it, and an extra tab must not change
  // where they land.
  await expect(app.getByTestId("prefs-language")).toBeVisible();

  // The order is part of the information architecture:
  //   Ark credentials -> authorization  the two are **two forms of the same thing** (one pastes a key,
  //                        the other authorizes with one click), so they sit right next to each other;
  //   -> API Key            the first three are "we call others on your behalf", it is "others call us",
  //                        so outbound comes before inbound;
  //   -> product suggestions  the first four are all **configuration** (changing them affects how the
  //                        platform works for you), this one is a **conversation** (you tell the platform
  //                        something and wait for a reply), a different kind, so it comes after the
  //                        configuration rather than in the middle.
  //                        It must not take general's first place either: that would make everyone who
  //                        comes to change the language first see a "give feedback" entry, as if
  //                        nagging them for feedback.
  //
  // ⚠️ Authorization management was **moved over** from workspace settings. There it was also next to
  // Ark credentials, but the two have opposite scopes (Ark credentials workspace-level / authorization
  // account-level), so being adjacent was itself a trap that only a "personal" Chip could make up for;
  // on this page these tabs are all account-level, so adjacency needs no compensation. See
  // 11-integrations.spec.ts.
  //
  // List them all instead of only asserting "contains these few": this case's job is to lock in **the
  // complete list and its order**; loosened to subset matching, accidentally deleting a tab or putting
  // "product suggestions" first would still be green.
  const tabs = app.locator('button[data-testid^="prefs-tab-"]');
  await expect(tabs).toHaveText([
    "外观与语言",
    "方舟凭证",
    "授权管理",
    "API Key",
    "宿主机",
    "产品建议",
  ]);

  // Switch to the API Key tab; the address must follow (shareable, survives a refresh).
  await app.getByTestId("prefs-tab-apikeys").click();
  await expect(app).toHaveURL(/\/preferences\?tab=apikeys$/);
  await expect(app.getByTestId("create-api-key")).toBeVisible();
  // The appearance controls should give way, instead of two tabs' contents stacking up.
  await expect(app.getByTestId("prefs-language")).toHaveCount(0);

  await app.reload();
  await expect(app.getByTestId("create-api-key")).toBeVisible();
});

test("认不出的 tab 落回外观与语言，不是空壳", async ({ app }) => {
  // The URL can be changed by the user. An unknown value must fall back to the default tab: rendering a
  // page with "only the tab bar, empty below" causes no error, but looks broken.
  await app.goto("/preferences?tab=nonsense");
  await expect(app.getByTestId("prefs-language")).toBeVisible();
});

// -- Storage and cross-device --------------------------------------------------------------
//
// Preferences have two storage places: the local cache (ama.prefs.v1, prevents first-frame flicker)
// and the server (authoritative, cross-device). The cases below all check **who wins when the two
// disagree**, which is the whole value of "storing it", and it cannot be seen when both copies are
// the same.

test("改偏好会写到服务端，不只是本机", async ({ app }) => {
  await setServerPrefs(app, null);
  await app.goto("/preferences");

  await app.getByTestId("prefs-appearance-dark").click();
  await expect(app.locator("html")).toHaveClass(/dark/);

  // Only being stored counts as "configure once globally". Writing only locally would lose it on
  // another device, which is exactly the problem this change solves.
  await expect
    .poll(async () => (await readServerPrefs(app))?.appearance)
    .toBe("dark");
});

test("换设备：本机没有缓存时，用服务端那份", async ({ app }) => {
  // The server remembers dark + English (set on another machine); this machine has nothing (a new machine).
  await app.goto("/workspaces");
  await setServerPrefs(app, { language: "en-US", appearance: "dark" });
  await app.evaluate(() => localStorage.removeItem("ama.prefs.v1"));

  await app.goto("/preferences");

  // After login, sync pulls the server copy down. The first frame is the default (light + Chinese),
  // then corrected, so the assertion must be able to retry.
  await expect(app.locator("html")).toHaveClass(/dark/);
  await expect(app.locator("html")).toHaveAttribute("lang", "en-US");
});

test("换设备：本机缓存与服务端冲突时，以服务端为准", async ({ app }) => {
  // This is the most important one: the user changed the appearance to light **on their phone**, then
  // came back to this computer whose cache is still dark. The server must win, or "cross-device" is
  // fake: their change on the phone would keep being overwritten by this machine's old cache.
  await app.goto("/workspaces");
  await app.evaluate(() =>
    localStorage.setItem(
      "ama.prefs.v1",
      JSON.stringify({ language: "zh-CN", appearance: "dark" }),
    ),
  );
  await setServerPrefs(app, { language: "zh-CN", appearance: "light" });

  await app.goto("/preferences");

  await expect(app.locator("html")).toHaveClass(/light/);
  // The cache must be corrected too, or the next open would flash dark first again.
  await expect
    .poll(() => app.evaluate(() => JSON.parse(localStorage.getItem("ama.prefs.v1") ?? "{}").appearance))
    .toBe("light");
});

test("升级迁移：本机的旧设置不会丢，会被搬到服务端", async ({ app }) => {
  // This one maps to the acceptance requirement "old data is not lost".
  //
  // The scenario is the moment of upgrade: the user set the appearance to dark before storage existed
  // (back then only localStorage), and the server has no record at all. After login it must **move this
  // machine's copy up**, rather than wiping their setting with the server's defaults: that would make
  // users see their color scheme mysteriously change back to light.
  await app.goto("/workspaces");
  await app.evaluate(() =>
    localStorage.setItem(
      "ama.prefs.v1",
      JSON.stringify({ language: "en-US", appearance: "dark" }),
    ),
  );
  await setServerPrefs(app, null); // no server record = the state before the upgrade

  await app.goto("/preferences");

  // The UI keeps the user's original setting; it must not be wiped by the defaults.
  await expect(app.locator("html")).toHaveClass(/dark/);
  await expect(app.locator("html")).toHaveAttribute("lang", "en-US");

  // And it really was written up, so another device can get it next time.
  await expect.poll(async () => await readServerPrefs(app)).toEqual({
    language: "en-US",
    appearance: "dark",
  });
});

test("服务端读失败时沿用本机缓存，不退回默认档", async ({ app }) => {
  // Preferences are "what the UI looks like", not a feature. If they cannot be fetched, keep using the
  // last settings: falling back to defaults would make users see the color scheme mysteriously change
  // without having done anything.
  await app.goto("/workspaces");
  await app.evaluate(() =>
    localStorage.setItem(
      "ama.prefs.v1",
      JSON.stringify({ language: "zh-CN", appearance: "dark" }),
    ),
  );
  // Store a value that cannot be parsed, so the mock's read path goes into catch.
  await app.evaluate(() => localStorage.setItem("ama.mock.userPrefs", "{坏掉的 JSON"));

  await app.goto("/preferences");

  // Still dark.
  await expect(app.locator("html")).toHaveClass(/dark/);
});
