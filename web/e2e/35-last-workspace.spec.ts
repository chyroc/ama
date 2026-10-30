/**
 * Root path `/` automatically enters the last visited workspace.
 *
 * Four paired cases; **missing any one lets a wrong implementation go all green**:
 *
 *   no record                 -> list page              (must not build `/w/` from an empty key)
 *   record and valid          -> straight into it       (the feature itself)
 *   record but invalid        -> fall back to list page (not an error page; this one is the easiest to lose)
 *   typing /workspaces        -> always the list page   (auto-jump must not lock people into a workspace)
 *
 * # Why write localStorage directly instead of "visiting a workspace first so it records itself"
 *
 * That would test the combination of "write" and "read", where either half breaking shows the same
 * red, and it is **order-dependent**: a workspace visited by one case leaks into the next. Writing
 * the value directly makes each case lock in only one thing. "Write" is locked in by the last case on
 * its own (following setPrefs in 07-preferences: goto first, then page.evaluate; **addInitScript
 * cannot be used**, it is not used anywhere in the repo, and the hook must wait for the bundle to run).
 *
 * ⚠️ This key is unlike the mock's `ama.mock.*` switches: **`reset()` cannot wipe it** (it is not in
 * the store at all), so every case has to clean up after itself; see afterEach.
 */

import { allowWorkspacesMock, denyWorkspaceMock, expect, test } from "./fixtures";

const LAST_KEY = "ama.lastWorkspace.v1";

/** Plant a memory of "last in this workspace". goto comes first: localStorage is stored per origin. */
async function setLastWorkspace(page: import("@playwright/test").Page, key: string) {
  await page.goto("/workspaces");
  await page.evaluate(
    ([storageKey, value]) => localStorage.setItem(storageKey, value),
    [LAST_KEY, key],
  );
}

async function readLastWorkspace(page: import("@playwright/test").Page): Promise<string | null> {
  return page.evaluate((storageKey) => localStorage.getItem(storageKey), LAST_KEY);
}

test.afterEach(async ({ page }) => {
  // Both must be cleared: the memory would make later specs drop from `/` into some workspace,
  // and the deny switch would make them **unable to open any workspace at all** (a red that does not
  // point to the real cause at all).
  await page.evaluate((storageKey) => localStorage.removeItem(storageKey), LAST_KEY);
  await allowWorkspacesMock(page);
});

test("① 没有记录时访问 / → 进空间列表页", async ({ app }) => {
  // The app fixture already reset; clear the memory explicitly once more here to make the "no record"
  // precondition certain, without depending on whether the previous case's afterEach ran.
  await app.evaluate((storageKey) => localStorage.removeItem(storageKey), LAST_KEY);

  await app.goto("/");

  await expect(app).toHaveURL(/\/workspaces$/);
  await expect(app.getByRole("heading", { name: "选择空间" })).toBeVisible();
});

test("② 有记录且有效时访问 / → 直接进那个空间，不经过列表页", async ({ app }) => {
  await setLastWorkspace(app, "growth");

  await app.goto("/");

  await expect(app).toHaveURL(/\/w\/growth$/);
  // Really entered the workspace: the sidebar switcher shows this workspace's name
  await expect(app.getByTestId("workspace-switcher")).toContainText("增长中台");

  /*
   * ⚠️ **The first frame must not flash the list page.**
   *
   * "Render WorkspacesPage first, then jump away in an effect" would also pass the two assertions
   * above; that is exactly the implementation this case rules out. The test is that the list page
   * heading **never entered the DOM from start to finish**: the destination is computed synchronously
   * during render (localStorage is synchronously readable), so it should never be mounted even once.
   *
   * Poll with `waitForFunction` instead of checking once afterward: checking afterward always sees the
   * final "already jumped" state, which cannot show whether it flashed. Here a 500ms observation
   * window starts right after navigation (the mock's read delay is about 90ms, enough for the wrong
   * implementation to flash).
   */
  const flashed = await app.evaluate(() => {
    return new Promise<boolean>((resolve) => {
      let seen = false;
      const check = () => {
        if (document.body.textContent?.includes("选择空间")) seen = true;
      };
      const timer = setInterval(check, 10);
      setTimeout(() => {
        clearInterval(timer);
        resolve(seen);
      }, 500);
    });
  });
  expect(flashed).toBe(false);
});

test("③ 记录指向已被删除 / 打不开的空间 → 回落列表页，不是报错页", async ({ app }) => {
  // The seed has only one workspace, growth, so this key is not found -> getWorkspaceByKey returns null.
  // This takes the "workspace does not exist" half (deleted / key changed), which is a **different code
  // path** from the 403 half below (null vs reject); both must be locked in separately.
  await setLastWorkspace(app, "deleted-space");

  await app.goto("/");

  await expect(app).toHaveURL(/\/workspaces$/);
  await expect(app.getByRole("heading", { name: "选择空间" })).toBeVisible();
  // Do not throw a permission error page in the user's face: they never asked to enter this workspace
  await expect(app.getByTestId("workspace-forbidden")).toHaveCount(0);

  // And the bad memory must be forgotten; otherwise every visit sends the user to the same place that
  // cannot open and bounces them back to the list page, **which they can never fix themselves**, with
  // zero errors throughout.
  expect(await readLastWorkspace(app)).toBeNull();
});

test("③ 记录指向无权访问的空间（403）→ 同样回落列表页", async ({ app }) => {
  await setLastWorkspace(app, "growth");
  // "Removed from members" is a 403 under RBAC. ⚠️ denyWorkspaceMock must be used, not
  // failReadsMock: the latter does not touch getWorkspaceByKey, so the 403 never happens and the case
  // quietly runs on the success path (07-preferences records the same lesson).
  await denyWorkspaceMock(app, "growth");

  await app.goto("/");

  await expect(app).toHaveURL(/\/workspaces$/);
  await expect(app.getByRole("heading", { name: "选择空间" })).toBeVisible();
  await expect(app.getByTestId("workspace-forbidden")).toHaveCount(0);
  expect(await readLastWorkspace(app)).toBeNull();
});

test("手输 /w/:key 撞上 403 仍然要说清原因，不能静默跳走", async ({ app }) => {
  /*
   * **Paired** with the previous case: the same 403 must give opposite results depending on where the
   * user came from.
   *
   * An auto-jump silently goes back to the list (the user never asked to enter this workspace), while
   * typing it / clicking a bookmark / a link someone shared must show "you are not in this workspace":
   * a silent jump away would look like "I clicked and nothing happened", and that sentence is the only
   * useful information. Without this case, turning every forbidden into a jump would also be all green.
   */
  await denyWorkspaceMock(app, "growth");

  await app.goto("/w/growth");

  await expect(app.getByTestId("workspace-forbidden")).toBeVisible();
  await expect(app).toHaveURL(/\/w\/growth$/);
});

test("④ 手动访问 /workspaces → 永远是列表页，即便有记录", async ({ app }) => {
  await setLastWorkspace(app, "growth");

  await app.goto("/workspaces");

  await expect(app).toHaveURL(/\/workspaces$/);
  await expect(app.getByRole("heading", { name: "选择空间" })).toBeVisible();
  // Not carried into a workspace along the way
  await expect(app.getByTestId("workspace-switcher")).toHaveCount(0);

  /*
   * ⚠️ The three assertions above are all satisfied **before any jump happens**, so they cannot stop
   * the "/workspaces also auto-jumps" implementation, which is exactly the target of mutation testing.
   * So one more observation window is kept: still on this page after 500ms.
   * This is the same discipline as the waitForTimeout in expectNoShell in 07-preferences:
   * **to prove "something never happened", a retrying assertion only passes the first time it is satisfied.**
   */
  await app.waitForTimeout(500);
  await expect(app).toHaveURL(/\/workspaces$/);
  await expect(app.getByRole("heading", { name: "选择空间" })).toBeVisible();

  // The memory was not wiped by this visit: "go pick one from the list" should not be read as "forget where I just was"
  expect(await readLastWorkspace(app)).toBe("growth");
});

test("进入空间时把它记下来，从此 / 直接回这里", async ({ app }) => {
  // This one locks in "write": the cases above all plant the value directly, so they stay all green even if writing breaks.
  await app.evaluate((storageKey) => localStorage.removeItem(storageKey), LAST_KEY);

  await app.goto("/w/growth");
  await expect(app.getByTestId("workspace-switcher")).toContainText("增长中台");
  expect(await readLastWorkspace(app)).toBe("growth");

  // End to end: visiting `/` now should come back here
  await app.goto("/");
  await expect(app).toHaveURL(/\/w\/growth$/);
});

test("localStorage 里被手改成非法 key → 当没记录，不拼出打不开的地址", async ({ app }) => {
  // This value is put into a URL, and localStorage can be changed by the user. Without validation,
  // `/w/<invalid string>` would really be visited once, with an error that does not point to "a bad
  // value stored on this machine".
  await setLastWorkspace(app, "Bad Key/../etc");

  await app.goto("/");

  await expect(app).toHaveURL(/\/workspaces$/);
  await expect(app.getByRole("heading", { name: "选择空间" })).toBeVisible();
});
