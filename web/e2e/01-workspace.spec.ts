/**
 * Requirement 0: every action happens inside a workspace.
 */

import { clearAll, expect, hideAvatarMock, nav, test } from "./fixtures";

test("没有任何空间时仍是列表页，只是列表为空", async ({ page }) => {
  await clearAll(page);
  await page.goto("/workspaces");

  // **The shell must still be there**: this used to replace the whole page with the create form, and the
  // page header, the preferences entry and the "new workspace" button were all swapped out. /workspaces
  // is the only reachable page when there is no workspace, and preferences are global: exactly when it
  // must not be lost.
  await expect(page.getByRole("heading", { name: "选择空间" })).toBeVisible();
  await expect(page.getByTestId("new-workspace")).toBeVisible();
  await expect(page.getByTestId("open-preferences")).toBeVisible();

  // The empty state gives a hint, not a laid-out form: there should be no input box before clicking "new workspace".
  await expect(page.getByRole("heading", { name: "还没有可进入的空间" })).toBeVisible();
  await expect(page.getByTestId("ws-name")).toHaveCount(0);

  // An empty list has two causes (a new user / not in any workspace's member list), and the text must
  // give both ways: saying only "go create one" would send the latter in the wrong direction.
  await expect(page.getByText(/新建一个空间即可开始/)).toBeVisible();
  await expect(page.getByText(/联系该空间的管理员把你加为成员/)).toBeVisible();

  // In the empty state there is no "enter workspace" entry to get around it
  await expect(page.getByRole("button", { name: "进入空间" })).toHaveCount(0);

  // Typing a business path directly still bounces back to this page
  await page.goto("/w/growth/agents");
  await expect(page).toHaveURL(/\/workspaces$/);
  await expect(page.getByRole("heading", { name: "选择空间" })).toBeVisible();
});

test("空态里的「新建空间」走的是同一个创建弹窗", async ({ page }) => {
  await clearAll(page);
  await page.goto("/workspaces");

  // The button in the empty-state card and the one in the page header are the same path (both
  // setCreating(true)), not two separate forms
  await page.getByTestId("empty-new-workspace").click();
  await page.getByTestId("ws-name").fill("首个空间");
  await page.getByTestId("ws-key").fill("first-one");
  await page.getByTestId("ws-submit").click();

  // After a successful create, go straight into the new workspace
  await expect(page).toHaveURL(/\/w\/first-one$/);
});

test("创建空间后进入该空间，业务导航可用", async ({ page }) => {
  await clearAll(page);
  await page.goto("/workspaces");

  await page.getByTestId("new-workspace").click();
  await page.getByTestId("ws-name").fill("电商增长");
  // The key is derived from the name (here dian-shang-zeng-zhang); this case only cares about navigation,
  // so it is explicitly overridden with a short predictable value. **Automatic derivation itself is locked
  // in by the dedicated case above**.
  await page.getByTestId("ws-key").fill("ec-growth");
  await page.getByTestId("ws-desc").fill("电商业务线");
  await page.getByTestId("ws-submit").click();

  await expect(page).toHaveURL(/\/w\/ec-growth$/);
  // The top of the sidebar is one row combining the logo + current workspace name (the whole row is the
  // workspace switcher). Count the shapes rather than only asserting visible: an empty <svg> also counts
  // as visible, which would make this lock fake.
  const mark = page.getByTestId("brand-mark");
  await expect(mark).toBeVisible();
  expect(await mark.locator("rect, path").count()).toBe(4);
  // **The AMA wordmark gives way to the workspace name**: the logo already carries brand recognition,
  // and the top row has to answer "which workspace am I in".
  await expect(page.getByTestId("workspace-switcher")).toContainText("电商增长");
  await expect(page.getByText("AMA", { exact: true })).toHaveCount(0);
  // The sidebar navigation appears, meaning we are already inside the workspace
  await expect(nav(page, "Agents")).toBeVisible();
  await expect(nav(page, "运行历史")).toBeVisible();
  // Workspace settings is a top-level nav item in the sidebar "Manage" group, no longer hidden in the
  // account menu. Members are **not** in the sidebar: it is a tab of the settings page, and putting it in
  // both places would mean two entries to the same destination.
  await expect(nav(page, "空间设置")).toBeVisible();
  await expect(nav(page, "成员管理")).toHaveCount(0);
  // Global settings are also in the sidebar, but in the **"Account" group**: it is account-level, the
  // opposite scope of workspace settings.
  await expect(nav(page, "全局设置")).toBeVisible();
  // The account menu now only has "Sign out": workspace settings and global settings both moved to the
  // sidebar, and keeping two entries to the same destination would make people think they are different things.
  await page.getByLabel("账号菜单").click();
  await expect(page.getByRole("menuitem", { name: "退出登录" })).toBeVisible();
  await expect(page.getByRole("menuitem", { name: "偏好设置" })).toHaveCount(0);
  await expect(page.getByRole("menuitem", { name: "全局设置" })).toHaveCount(0);
  await expect(page.getByRole("menuitem", { name: "空间设置" })).toHaveCount(0);
});

test("favicon 与站内标志用的是同一份品牌产物，且真的取得到", async ({ app }) => {
  await app.goto("/workspaces");

  // The reference shape in index.html: one SVG covers all sizes, and ico is a fallback for old browsers
  // and crawlers that only fetch the root.
  const hrefs = await app.locator('link[rel="icon"], link[rel="apple-touch-icon"], link[rel="manifest"]').evaluateAll(
    (els) => els.map((e) => e.getAttribute("href")),
  );
  expect(hrefs).toEqual(["/favicon.ico", "/favicon.svg", "/apple-touch-icon.png", "/manifest.json"]);

  // **This is the real lock**: a link tag being written does not mean the file exists. web/public/ is a new
  // directory; missing one committed file, or writing a relative path (which on a /w/xxx deep link resolves
  // to /w/favicon.svg), both show up only as "a blank tab icon": the browser does not error and neither
  // does the build.
  for (const [path, type] of [
    ["/favicon.svg", "image/svg+xml"],
    ["/favicon.ico", "image/"],
    ["/apple-touch-icon.png", "image/png"],
    // The manifest uses .json instead of .webmanifest: the latter is empty in Go's mime table (see index.html).
    ["/manifest.json", "application/json"],
  ]) {
    const res = await app.request.get(path);
    expect(res.status(), `${path} 取不到`).toBe(200);
    // Getting text/html means the SPA fallback was hit, i.e. the file does not actually exist.
    expect(res.headers()["content-type"], `${path} 命中了 SPA fallback`).toContain(type);
  }

  // Icons listed in the manifest must really exist too: they are not referenced by any <link> directly,
  // the kind most easily missed and least noticed (only used on "add to home screen").
  const manifest = await (await app.request.get("/manifest.json")).json();
  for (const icon of manifest.icons) {
    const res = await app.request.get(icon.src);
    expect(res.status(), `清单里的 ${icon.src} 取不到`).toBe(200);
  }

  // In the dark theme the logo background vs. page background contrast is only 1.04:1, relying on
  // ring-border to draw the edge (see BrandMark). This only locks in "the outline class is still there";
  // whether the color is bright enough is guaranteed by theme tokens, and pixels are not asserted in e2e.
  //
  // **The sidebar only exists inside workspace pages**: /workspaces is the workspace picker, outside
  // AppShell, and there is no brand spot there at all (the first version of this case fell into exactly
  // that, with the error "element not found").
  await app.goto("/w/growth");
  await expect(app.getByTestId("brand-mark")).toHaveClass(/ring-border/);
});

test("只填中文名就能创建：key 由名称自动推导成拼音", async ({ app }) => {
  // This covers a real production bug: slugify on a purely Chinese name gave **an empty string**, so
  // submitting got stuck on "workspace Key cannot be empty", while the Key field looked automatically filled in.
  //
  // ⚠️ **All 16 other cases in this file explicitly fill("ws-key")**, so the "automatic derivation" path
  // was never covered once: all green and still a gap. This one **deliberately does not touch ws-key**,
  // which is exactly the path that was missed.
  await app.goto("/workspaces");
  await app.getByTestId("new-workspace").click();

  await app.getByTestId("ws-name").fill("增长中台");
  // Assert the derived key directly appears in the input box, rather than only checking whether creation
  // succeeded: an empty string + a user typing it in would also create it, making this lock meaningless.
  await expect(app.getByTestId("ws-key")).toHaveValue("zeng-zhang-zhong-tai");

  await app.getByTestId("ws-submit").click();
  await expect(app).toHaveURL(/\/w\/zeng-zhang-zhong-tai$/);
});

test("多音字按词读，不是按单字硬拼", async ({ app }) => {
  // The polyphonic character in "Chongqing" reads chong, not zhong; the one in "bank" (yinhang) reads
  // hang, not xing. This locks in the reason pinyin-pro was chosen: it resolves polyphonic characters by
  // dictionary. Switching to a library that looks up character by character would turn this red.
  await app.goto("/workspaces");
  await app.getByTestId("new-workspace").click();

  await app.getByTestId("ws-name").fill("重庆银行");
  await expect(app.getByTestId("ws-key")).toHaveValue("chong-qing-yin-hang");
});

test("手改过 Key 之后，再改名字不会覆盖它", async ({ app }) => {
  // The keyEdited state: derivation only applies while the user has not touched the Key.
  // With pinyin added this breaks more easily (derivation now almost always yields a value, whereas a
  // Chinese name used to give an empty string, which "hid" it).
  await app.goto("/workspaces");
  await app.getByTestId("new-workspace").click();

  await app.getByTestId("ws-name").fill("增长中台");
  await app.getByTestId("ws-key").fill("my-own-key");
  await app.getByTestId("ws-name").fill("换个名字");
  await expect(app.getByTestId("ws-key")).toHaveValue("my-own-key");
});

test("空间 Key 会校验格式与唯一性", async ({ app }) => {
  await app.goto("/workspaces");
  await app.getByTestId("new-workspace").click();

  await app.getByTestId("ws-name").fill("测试空间");
  await app.getByTestId("ws-key").fill("BAD KEY");
  await app.getByTestId("ws-submit").click();
  await expect(app.getByText("只能用小写字母、数字和连字符，且以字母或数字开头")).toBeVisible();

  // Conflicts with growth, which already exists in the seed data
  await app.getByTestId("ws-key").fill("growth");
  await app.getByTestId("ws-submit").click();
  await expect(app.getByText("Key「growth」已被占用")).toBeVisible();
});

test("未知路径回落到引导页而不是留白", async ({ app }) => {
  await app.goto("/nope/whatever");
  await expect(app).toHaveURL(/\/workspaces$/);
});

test("能在多个空间之间切换", async ({ app }) => {
  await app.goto("/workspaces");
  await app.getByTestId("new-workspace").click();
  await app.getByTestId("ws-name").fill("供应链");
  await app.getByTestId("ws-key").fill("supply");
  await app.getByTestId("ws-submit").click();
  await expect(app).toHaveURL(/\/w\/supply$/);

  // The new workspace is clean and did not inherit growth's agents
  await nav(app, "Agents").click();
  await expect(app.getByRole("heading", { name: "还没有 Agent" })).toBeVisible();

  // Switch back to growth with the row at the **top** of the sidebar; the data should come back
  await app.getByLabel("切换空间").click();
  await app.getByRole("menuitem", { name: /growth/ }).first().click();
  await expect(app).toHaveURL(/\/w\/growth/);
});

/**
 * The workspace switcher is at the **top** of the sidebar, combined with the logo into one row; the
 * bottom left only has personal info.
 *
 * This group locks in the move of the switcher from the bottom left to the top left. It overturned the
 * earlier decision "the brand spot is deliberately not clickable": the top is now not "a link to the
 * overview" but "a trigger that opens the workspace menu", so the "both highlighted" concern no longer
 * holds. Without these cases, moving it back to the bottom left would still be all green.
 */
test.describe("空间切换器在顶部", () => {
  test("顶部那一行 = logo + 空间名，点开能切到另一个空间", async ({ app }) => {
    await app.goto("/workspaces");
    await app.getByTestId("new-workspace").click();
    await app.getByTestId("ws-name").fill("供应链");
    await app.getByTestId("ws-key").fill("supply");
    await app.getByTestId("ws-submit").click();
    await expect(app).toHaveURL(/\/w\/supply$/);

    const switcher = app.getByTestId("workspace-switcher");
    // The logo and the workspace name are in the same trigger: that is what "combined into one row" means;
    // asserting each visible separately would still pass if they sat at opposite ends of the sidebar.
    await expect(switcher.getByTestId("brand-mark")).toBeVisible();
    await expect(switcher).toContainText("供应链");

    // It is above the navigation: comparing the y coordinates of the trigger and "Overview" is closer to
    // what users see than asserting DOM order.
    const top = await switcher.boundingBox();
    const overview = await nav(app, "概览").boundingBox();
    expect(top!.y).toBeLessThan(overview!.y);

    // It really can switch away, not just open the menu
    await switcher.click();
    await app.getByRole("menuitem", { name: /growth/ }).first().click();
    await expect(app).toHaveURL(/\/w\/growth/);
    await expect(app.getByTestId("workspace-switcher")).toContainText("增长中台");
  });

  test("菜单向下开，且没被视口裁掉", async ({ app }) => {
    await app.goto("/w/growth");

    const switcher = app.getByTestId("workspace-switcher");
    await switcher.click();

    // "Manage all workspaces" is the lowest item in the menu; it being visible means the whole menu is in the viewport.
    const manage = app.getByRole("menuitem", { name: "管理所有空间" });
    await expect(manage).toBeVisible();

    // **It opens downward**: the menu's top edge is below the trigger's bottom edge. If it moved to the top but
    // placement was not changed from top start to bottom start, the menu would open outside the viewport:
    // exactly what this guards against.
    const trigger = (await switcher.boundingBox())!;
    const menu = (await app.getByRole("menu").boundingBox())!;
    expect(menu.y).toBeGreaterThanOrEqual(trigger.y);

    const viewport = app.viewportSize()!;
    expect(menu.y).toBeGreaterThanOrEqual(0);
    expect(menu.y + menu.height).toBeLessThanOrEqual(viewport.height);

    // The "manage workspaces" entry still works after the move (it is the only way out when no workspace can be listed)
    await manage.click();
    await expect(app).toHaveURL(/\/workspaces$/);
  });

  test("左下角只剩个人信息，没有空间切换器", async ({ app }) => {
    await app.goto("/w/growth");

    // Only one switcher on the whole page: the most likely mistake when moving it is "added the new one,
    // forgot to delete the old one", and then both are there, both work, and it looks quite normal.
    await expect(app.getByTestId("workspace-switcher")).toHaveCount(1);

    // The row at the bottom left is the account, not the workspace: it is below "Overview".
    const user = (await app.getByTestId("current-user").boundingBox())!;
    const overview = (await nav(app, "概览").boundingBox())!;
    expect(user.y).toBeGreaterThan(overview.y);

    // And the switcher is above "Overview": the two identity dimensions each take one end.
    const switcher = (await app.getByTestId("workspace-switcher").boundingBox())!;
    expect(switcher.y).toBeLessThan(overview.y);
  });
});

/**
 * The bottom-left user bar shows the SSO avatar.
 *
 * The avatar goes through auth.Identity.AvatarURL (its source is the picture in SSO userinfo). What is
 * asserted here is that **the image really rendered**, not just whether the element exists: Radix's
 * Avatar.Image only enters the DOM after the image has loaded, and with a broken src it never appears,
 * so "the node can be found" is itself equivalent to "this image loaded successfully", which is exactly
 * what the fallback branch relies on to tell them apart.
 */
test("左下角用户栏展示真实头像，且名字仍在", async ({ app }) => {
  await app.goto("/w/growth");

  const avatar = app.getByTestId("current-user-avatar");
  await expect(avatar).toBeVisible();
  // The mock's avatar is an inline data URI (this machine cannot reach external image hosts, see the notes in data/api.ts)
  await expect(avatar).toHaveAttribute("src", /^data:image\/svg\+xml/);
  // The avatar is decorative: the name sits right next to it, and screen readers should not read the same person twice
  await expect(avatar).toHaveAttribute("alt", "");

  // Having an avatar must not push out the name: that row is "one face + one name"
  await expect(app.getByTestId("current-user")).toHaveText("张三");
});

/**
 * Without an avatar it degrades gracefully to an initial.
 *
 * This branch is the normal case in production, not an exception: the machine identity of the fixed
 * API Key never has an avatar (Resolve in server/auth/service.go builds the Identity directly without
 * querying the database), and SSO does not guarantee a picture either. hideAvatarMock removes avatarUrl
 * to simulate it.
 *
 * The marker lives in a separate localStorage key (same as signOutMock), so afterEach must clear it, or
 * it would leak into later specs: workers=1, running serially, sharing one localStorage.
 */
test.describe("无头像", () => {
  test.afterEach(async ({ page }) => {
    await page.evaluate(() => localStorage.removeItem("ama.mock.noAvatar"));
  });

  test("没有头像时退化成名字首字母，不留空圆", async ({ app }) => {
    await hideAvatarMock(app);
    await app.goto("/w/growth");

    // The image node should not appear at all: Radix only renders img after the image has loaded, and
    // does not render it when src is empty
    await expect(app.getByTestId("current-user-avatar")).toHaveCount(0);
    // The initial covers it, and the name stays as before
    await expect(app.getByTestId("current-user")).toHaveText("张三");
    await expect(app.getByText("张", { exact: true }).first()).toBeVisible();
  });
});
