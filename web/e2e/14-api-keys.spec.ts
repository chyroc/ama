/**
 * UI cases for platform API Keys.
 *
 * ⚠️ **It lives in "global settings"** (/preferences?tab=apikeys), not in workspace settings.
 * It has always been account-level (the backend table has no workspace_id column); it used to hang
 * in workspace settings for historical reasons, relying on a "personal" Chip to explain "why switching
 * workspaces still shows the same keys".
 *
 * The focus is four things that "cause no error when wrong, just insecure or hard to use":
 *
 *   plaintext appears only once   in the create dialog, not in the list, gone forever after closing
 *   revoke really takes effect    it disappears from the list
 *   no dependency on workspaces   works on pages outside any workspace
 *   the old address still works   ?tab=apikeys must redirect, not render an empty shell
 *
 * ⚠️ This whole file uses mock hooks, **not page.route**: in local mode LocalApiClient sends no
 * network requests, route never fires, and the cases would silently run the success path and then
 * fail on an assertion (see the notes in fixtures.ts).
 */

import { expect, failReadsMock, hangReadsMock, restoreReadsMock, test } from "./fixtures";

/** Open the API Key tab (in global settings, unrelated to workspaces). */
async function openApiKeys(app: import("@playwright/test").Page) {
  await app.goto("/preferences?tab=apikeys");
  await expect(app.getByTestId("create-api-key")).toBeVisible();
}

/**
 * Create a key and return the plaintext shown in the "only this once" dialog.
 *
 * ⚠️ **The submit button must be scoped to each dialog** (`create-api-key-modal` /
 * `api-key-created-modal`); the global `getByTestId("modal-submit")` cannot be used: the FormModal
 * submit button testid is fixed, and both dialogs are mounted in the DOM at the moment of switching
 * (RAC's exit animation waits for animationend), so a global locator hits a strict mode violation.
 * This is not a product bug; it is the inherent shape of "two FormModals on the same page".
 */
async function createKey(app: import("@playwright/test").Page, name: string): Promise<string> {
  await app.getByTestId("create-api-key").click();
  await app.getByTestId("api-key-name").fill(name);
  await app.getByTestId("create-api-key-modal").getByTestId("modal-submit").click();

  // The "only this once" dialog must appear: this is the key step of the whole design.
  const plaintext = app.getByTestId("api-key-plaintext");
  await expect(plaintext).toBeVisible();
  const value = (await plaintext.textContent()) ?? "";
  expect(value).toMatch(/^amak_/);
  return value;
}

/** Close the "only this once" dialog (click "I have saved it"). */
async function dismissCreatedModal(app: import("@playwright/test").Page) {
  await app.getByTestId("api-key-created-modal").getByTestId("modal-submit").click();
  await expect(app.getByTestId("api-key-plaintext")).toHaveCount(0);
}

test("API Key tab 空状态给出引导与文档入口", async ({ app }) => {
  await openApiKeys(app);

  // The empty state must explain "what this is for", not just give an empty list.
  await expect(app.getByText("还没有 API Key")).toBeVisible();
  await expect(app.getByTestId("api-key-list")).toHaveCount(0);

  // The docs link is the next step for agent users; it must be on the page.
  await expect(app.getByRole("link", { name: "/api/openapi.json" })).toBeVisible();
  await expect(app.getByRole("link", { name: "/llms.txt" })).toBeVisible();
});

test("创建 API Key 时明文只显示一次，关掉就拿不到", async ({ app }) => {
  await openApiKeys(app);

  const plaintext = await createKey(app, "ci-deploy");
  // There is a long string after the prefix: showing only the prefix would make this key unusable.
  expect(plaintext.length).toBeGreaterThan(40);

  // The dialog must say clearly "once closed you cannot see it again", or users will not copy it on the spot.
  await expect(app.getByText(/关掉这个弹窗后就再也看不到了/)).toBeVisible();

  // Click "I have saved it" to close.
  await dismissCreatedModal(app);

  // **Key assertion: no plaintext in the list**, only the prefix.
  const list = app.getByTestId("api-key-list");
  await expect(list).toBeVisible();
  await expect(list).toContainText("ci-deploy");
  await expect(list).not.toContainText(plaintext);

  // Cannot get it back after a refresh either: the plaintext is not on any persisted display path.
  await app.reload();
  await expect(app.getByTestId("api-key-list")).not.toContainText(plaintext);
});

test("新建的 key 显示「从未使用」", async ({ app }) => {
  await openApiKeys(app);
  await createKey(app, "fresh-key");
  await dismissCreatedModal(app);

  // A missing lastUsedAt must show as "never used" rather than blank:
  // it is the user's only basis for deciding "can I revoke this".
  await expect(app.getByTestId("api-key-last-used")).toHaveText("从未使用");
});

test("吊销 API Key 后它从列表消失", async ({ app }) => {
  await openApiKeys(app);
  await createKey(app, "throwaway");
  await dismissCreatedModal(app);

  const list = app.getByTestId("api-key-list");
  await expect(list).toContainText("throwaway");

  await app.getByTestId("revoke-api-key").click();
  // Revoking is irreversible and needs a second confirmation.
  await expect(app.getByText(/无法恢复/)).toBeVisible();
  await app.getByTestId("confirm-danger").click();

  // There was only one key; after revoking the whole list is gone -> back to the empty state.
  await expect(app.getByText("还没有 API Key")).toBeVisible();
  await expect(app.getByTestId("api-key-list")).toHaveCount(0);
});

test("空名字被拦下，且弹窗不关", async ({ app }) => {
  await openApiKeys(app);

  await app.getByTestId("create-api-key").click();
  await app.getByTestId("api-key-name").fill("   ");
  await app.getByTestId("create-api-key-modal").getByTestId("modal-submit").click();

  // The text matches the error from the backend auth.CreateAPIKey: LocalApiClient is the contract reference.
  await expect(app.getByText("请给这把 key 起个名字")).toBeVisible();
  // The dialog must stay open so the user can fix it, not close (closing would make them start over).
  await expect(app.getByTestId("api-key-name")).toBeVisible();
});

test("API Key 是账号级的：一个空间都没有时也能管", async ({ app }) => {
  // Clear out all workspaces: the hardest check of "account-level". If this page still depended on
  // some workspace (say, listWorkspaces and put the first one into the request), it would go empty or error here.
  //
  // This is also a real scenario: a newcomer logging in for the first time does not belong to any
  // workspace yet, and that is exactly when they need a key.
  await app.goto("/workspaces");
  await app.evaluate(() => window.__amaApi?.clear());

  await openApiKeys(app);
  await createKey(app, "no-workspace-key");
  await dismissCreatedModal(app);
  await expect(app.getByTestId("api-key-list")).toContainText("no-workspace-key");

  // Still there after a refresh: it should not follow any workspace's lifecycle.
  await app.reload();
  await expect(app.getByTestId("api-key-list")).toContainText("no-workspace-key");
});

test("在空间里建的 key，在全局设置里看得到（同一批）", async ({ app }) => {
  await openApiKeys(app);
  await createKey(app, "cross-space-key");
  await dismissCreatedModal(app);

  // Create a new workspace, then come back to global settings: those keys must all still be there.
  // If it is empty here, someone quietly filtered by workspace, and nothing in the UI would explain
  // "where did the keys I created go".
  await app.goto("/workspaces");
  await app.getByTestId("new-workspace").click();
  await app.getByTestId("ws-name").fill("第二个空间");
  await app.getByTestId("ws-key").fill("second-space");
  await app.getByTestId("ws-submit").click();
  await expect(app).toHaveURL(/\/w\/second-space$/);

  await openApiKeys(app);
  await expect(app.getByTestId("api-key-list")).toContainText("cross-space-key");
});

test("旧地址 /settings?tab=apikeys 重定向到全局设置", async ({ app }) => {
  // That address really was used out there (docs, bookmarks, old e2e). Without catching it the tab is an
  // unknown value, and the page renders a shell with "only the tab bar, empty below": **no error and
  // no redirect**; users would just think "the API Key page is broken", not that the address changed.
  await app.goto("/w/growth/settings?tab=apikeys");

  // **?from= must be carried**: the user came from this workspace's settings page; without that
  // parameter it is the shell-less form and the sidebar disappears entirely. They only went to glance
  // at their keys and should not be kicked out of the workspace.
  await expect(app).toHaveURL(/\/preferences\?tab=apikeys&from=growth$/);
  await expect(app.getByTestId("create-api-key")).toBeVisible();
  // The shell must really be there, not just a parameter in the address.
  await expect(app.getByRole("navigation").getByRole("link", { name: "空间设置" })).toBeVisible();
});

test("读取失败时给重试入口，不是空列表", async ({ app }) => {
  // **"internal" must be passed explicitly**: without code it takes the other branch (access_denied and
  // the like get no retry button, see 13-load-errors.spec.ts). What this checks is the retryable branch.
  await failReadsMock(app, "internal");
  await app.goto("/preferences?tab=apikeys");

  // A read failure must show itself and offer retry: degrading into an empty list would make users
  // think "I have no keys" and create them again.
  await expect(app.getByTestId("load-error")).toBeVisible();
  await expect(app.getByTestId("load-retry")).toBeVisible();

  await restoreReadsMock(app);
  await app.getByTestId("load-retry").click();
  await expect(app.getByTestId("create-api-key")).toBeVisible();
});

test("加载中显示骨架屏", async ({ app }) => {
  await hangReadsMock(app);
  // hangReadsMock only sets a marker; you must reload yourself for it to take effect (see fixtures.ts).
  await app.goto("/preferences?tab=apikeys");

  // The skeleton is there and the "create" button has not appeared yet: proof it really stays on the isLoading branch.
  await expect(app.getByTestId("create-api-key")).toHaveCount(0);
  await expect(app.getByText("还没有 API Key")).toHaveCount(0);
});
