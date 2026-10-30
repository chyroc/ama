/**
 * Workspace-level Ark credentials: fill in / masked display / change region / verify / clear.
 *
 * The key length **matters** here: the mock uses `apiKey.length % 2` to simulate Ark's validation
 * result (even length -> usable, odd -> unusable), so the literals below must not change length
 * casually, or the "unusable" branch would silently become "usable".
 */

import type { Page } from "@playwright/test";
import { clearArkMock, expect, test } from "./fixtures";

/**
 * Switch to "fill in manually" and fill in a key.
 *
 * ⚠️ **The manual input is not rendered by default**: the source dropdown defaults to "pick from
 * Volcengine account", so the source must be switched first to get the input box (see the file header
 * of ArkFetchPicker). Filling directly reports that the element does not exist.
 */
async function fillManualKey(app: Page, key: string, testId = "ark-key-input") {
  await app.getByTestId("ark-source-select").click();
  await app.getByRole("option", { name: "手动填写" }).click();
  await app.getByTestId(testId).fill(key);
}

/** 32 characters, even -> the mock judges it usable. */
const VALID_KEY = "ARK00000000000000000000000000001";
/** 31 characters, odd -> the mock judges it unusable. */
const INVALID_KEY = "ARK0000000000000000000000000001";

/*
 * **All** cases in this group start from "never configured", so the seed's key is cleared first for all of them.
 *
 * ⚠️ The seed now **configures one key by default** (see seedArkCredentials in data/api.ts):
 * otherwise the new pre-check guidance on the chat page would block 30-odd chat cases that do not
 * care about credentials at all (the input box disabled), with a failure message that only says "the
 * input box is disabled", not pointing at all to "the seed has no key".
 *
 * The cost lands on this group: "not configured" is no longer the default state and must be restored
 * explicitly. It is in beforeEach rather than called per case because every case here needs it:
 * calling it per case, missing one means "the empty-state assertion never gets satisfied", and that
 * error also does not point to the real cause.
 */
test.beforeEach(async ({ app }) => {
  await clearArkMock(app);
});

test("未配置时是空态，填写后只回显掩码", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=ark");

  await expect(app.getByRole("heading", { name: "还没有配置方舟 API Key" })).toBeVisible();

  await app.getByTestId("ark-setup").click();
  await fillManualKey(app, VALID_KEY);
  await app.getByTestId("modal-submit").click();

  // The plaintext does not stay on the page; only the mask is left
  const masked = app.getByTestId("ark-masked");
  await expect(masked).toBeVisible();
  await expect(masked).not.toHaveText(VALID_KEY);
  await expect(masked).toContainText("*");
  await expect(app.getByText(VALID_KEY)).toHaveCount(0);

  // Still there after a refresh (persisted in localStorage)
  await app.reload();
  await expect(app.getByTestId("ark-masked")).toContainText("*");
});

test("太短的 key 会被拦住", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=ark");

  await app.getByTestId("ark-setup").click();
  await fillManualKey(app, "too-short");
  await app.getByTestId("modal-submit").click();

  await expect(app.getByText("API Key 长度不足，请确认是否复制完整")).toBeVisible();
  // The dialog stays open; nothing was saved by mistake
  await expect(app.getByTestId("ark-key-input")).toBeVisible();
});

test("校验通过与失败各自有明确反馈", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=ark");

  await app.getByTestId("ark-setup").click();
  await fillManualKey(app, VALID_KEY);
  await app.getByTestId("modal-submit").click();

  // Just saved means "not verified"; it should not falsely report success
  await expect(app.getByText("未验证")).toBeVisible();

  await app.getByTestId("ark-verify").click();
  await expect(app.getByText("可用")).toBeVisible();

  // Switch to a key that will be rejected: the verification result must be invalidated, with a reason
  await app.getByTestId("ark-replace").click();
  await fillManualKey(app, INVALID_KEY);
  await app.getByTestId("modal-submit").click();
  await expect(app.getByText("未验证")).toBeVisible();

  await app.getByTestId("ark-verify").click();
  await expect(app.getByText("不可用")).toBeVisible();
  await expect(app.getByTestId("ark-error")).toContainText("401");
});

test("换区域会让已有的校验结论作废", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=ark");

  await app.getByTestId("ark-setup").click();
  await fillManualKey(app, VALID_KEY);
  await app.getByTestId("modal-submit").click();
  await app.getByTestId("ark-verify").click();
  await expect(app.getByText("可用")).toBeVisible();

  await app.getByTestId("ark-region").click();
  await app.getByRole("option", { name: /BytePlus/ }).click();

  // The same key may not have the service activated in another region, so the result must go back to not verified
  await expect(app.getByText("未验证")).toBeVisible();
});

test("能清除凭证，清除后回到空态", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=ark");

  await app.getByTestId("ark-setup").click();
  await fillManualKey(app, VALID_KEY);
  await app.getByTestId("modal-submit").click();
  await expect(app.getByTestId("ark-masked")).toBeVisible();

  await app.getByTestId("ark-clear").click();
  await app.getByTestId("confirm-danger").click();

  await expect(app.getByRole("heading", { name: "还没有配置方舟 API Key" })).toBeVisible();
});

test("凭证按空间隔离，互不可见", async ({ app }) => {
  // Configure a key in workspace A
  await app.goto("/w/growth/settings?tab=ark");
  await app.getByTestId("ark-setup").click();
  await fillManualKey(app, VALID_KEY);
  await app.getByTestId("modal-submit").click();
  await expect(app.getByTestId("ark-masked")).toBeVisible();

  // Create workspace B; it should still be in the empty state
  await app.goto("/workspaces");
  await app.getByTestId("new-workspace").click();
  await app.getByTestId("ws-name").fill("另一个空间");
  await app.getByTestId("ws-key").fill("other-ws");
  await app.getByTestId("ws-submit").click();
  // A successful create jumps into the new workspace; wait for that jump to settle before moving on,
  // or the goto below would throw away the create request
  await expect(app).toHaveURL(/\/w\/other-ws$/);

  await app.goto("/w/other-ws/settings?tab=ark");
  await expect(app.getByRole("heading", { name: "还没有配置方舟 API Key" })).toBeVisible();

  // Workspace A is not affected
  await app.goto("/w/growth/settings?tab=ark");
  await expect(app.getByTestId("ark-masked")).toBeVisible();
});

// -- Configure once globally, reference it from workspaces ----------------------------------------
//
// This group watches four invariants of the "reference mechanism". Each one, when wrong, **causes no
// error**; the behavior just quietly goes wrong:
//
//   inherit reads the global value             -- wrong shows up as "referenced, yet says no key configured"
//   change global, referencing workspaces follow -- wrong shows up as "changed global, no effect"; this is the core need
//   override is not affected by global          -- wrong shows up as "someone else changed the key and broke my workspace"
//   one explicit configuration step             -- wrong shows up as the workspace mysteriously working, without the user knowing which key it uses

/** Configure a key in global settings. */
async function setGlobalKey(app: import("@playwright/test").Page, key: string) {
  await app.goto("/preferences?tab=ark");
  const setup = app.getByTestId("global-ark-setup");
  const replace = app.getByTestId("global-ark-replace");
  // ⚠️ **Wait for the card to finish loading first**: this page has a skeleton (the isLoading branch of
  // useAsyncData), and calling isVisible() directly during the skeleton gets false for both buttons, so
  // neither branch can click, and the timeout says "waiting for global-ark-replace", not pointing at
  // all to "it just has not loaded yet".
  await expect(setup.or(replace)).toBeVisible();
  // If already configured use "replace"; otherwise use the empty-state button.
  if (await setup.isVisible().catch(() => false)) await setup.click();
  else await replace.click();
  await fillManualKey(app, key, "global-ark-key-input");
  await app.getByTestId("global-ark-modal").getByTestId("modal-submit").click();
  await expect(app.getByTestId("global-ark-masked")).toBeVisible();
}

test("空间不会静默继承：新空间仍是未配置，要显式点一次才引用", async ({ app }) => {
  await setGlobalKey(app, VALID_KEY);

  // Global is configured, but the workspace **is still in the empty state**: this is "one explicit configuration step".
  // With silent inheritance users would not know which key they are using, and credential settings are
  // the last thing people should have to guess about.
  await app.goto("/w/growth/settings?tab=ark");
  await expect(app.getByRole("heading", { name: "还没有配置方舟 API Key" })).toBeVisible();

  // It only takes effect after clicking "use global config" once.
  await app.getByTestId("ark-use-global-empty").click();

  await expect(app.getByTestId("ark-source-inherit")).toBeVisible();
  // The mask must be of the global key, **not empty**: if empty, the UI would show "not configured",
  // and users would think the reference did not work and paste the key again.
  await expect(app.getByTestId("ark-masked")).toContainText("*");
});

test("改全局，引用它的空间跟着变（核心诉求）", async ({ app }) => {
  await setGlobalKey(app, VALID_KEY);
  await app.goto("/w/growth/settings?tab=ark");
  await app.getByTestId("ark-use-global-empty").click();
  await expect(app.getByTestId("ark-source-inherit")).toBeVisible();
  const before = await app.getByTestId("ark-masked").textContent();

  // Swap in a new global key, **without touching any setting of this workspace**.
  const OTHER_KEY = "ARKZZZZZZZZZZZZZZZZZZZZZZZZZZZ99";
  await setGlobalKey(app, OTHER_KEY);

  await app.goto("/w/growth/settings?tab=ark");
  const after = await app.getByTestId("ark-masked").textContent();
  expect(after).not.toBe(before);
  // The mask keeps 4 characters at each end, so the tail of the new key must show up in the mask.
  expect(after).toContain(OTHER_KEY.slice(-4));
});

test("本空间自定义时不受全局影响", async ({ app }) => {
  // First paste the workspace's own key.
  await app.goto("/w/growth/settings?tab=ark");
  await app.getByTestId("ark-setup").click();
  await fillManualKey(app, VALID_KEY);
  await app.getByTestId("modal-submit").click();
  await expect(app.getByTestId("ark-masked")).toBeVisible();

  // Configure a completely different global key.
  await setGlobalKey(app, "ARKGGGGGGGGGGGGGGGGGGGGGGGGGGG88");

  await app.goto("/w/growth/settings?tab=ark");
  // The "from global settings" source hint should not appear.
  await expect(app.getByTestId("ark-source-inherit")).toHaveCount(0);
  // The mask is still of its own key.
  await expect(app.getByTestId("ark-masked")).toContainText(VALID_KEY.slice(-4));
});

test("引用之后还能改回本空间自定义", async ({ app }) => {
  await setGlobalKey(app, VALID_KEY);
  await app.goto("/w/growth/settings?tab=ark");
  await app.getByTestId("ark-use-global-empty").click();
  await expect(app.getByTestId("ark-source-inherit")).toBeVisible();

  // "Use this workspace's own": the button text changes under inherit; it is no longer "replace".
  await app.getByTestId("ark-replace").click();
  const OWN_KEY = "ARKMMMMMMMMMMMMMMMMMMMMMMMMMMM77";
  await fillManualKey(app, OWN_KEY);
  await app.getByTestId("modal-submit").click();

  // Switch back to override: the source hint disappears, and the mask is of its own key.
  await expect(app.getByTestId("ark-source-inherit")).toHaveCount(0);
  await expect(app.getByTestId("ark-masked")).toContainText(OWN_KEY.slice(-4));
});

test("没配全局就点引用会被拦下，并说清去哪配", async ({ app }) => {
  // Clicking "use global config" when there is no global key must give an actionable error.
  // If it were allowed, users would think the reference succeeded, then a chat would fail with
  // "credential does not exist", and they would neither know where to configure it nor whether the
  // reference worked.
  await app.goto("/w/growth/settings?tab=ark");
  await app.getByTestId("ark-use-global-empty").click();

  const err = app.getByTestId("ark-use-global-error");
  await expect(err).toBeVisible();
  await expect(err).toContainText("全局设置");
});

test("删掉全局之后，引用它的空间给出指向全局设置的引导", async ({ app }) => {
  await setGlobalKey(app, VALID_KEY);
  await app.goto("/w/growth/settings?tab=ark");
  await app.getByTestId("ark-use-global-empty").click();
  await expect(app.getByTestId("ark-source-inherit")).toBeVisible();

  // Go back to global settings and clear that key.
  await app.goto("/preferences?tab=ark");
  await app.getByTestId("global-ark-clear").click();
  await app.getByTestId("confirm-danger").click();

  // The workspace must show "the referenced global key no longer exists", **not "not configured yet"**:
  // the latter would lead users to paste a workspace key, while what they really need is to configure
  // the global one again.
  await app.goto("/w/growth/settings?tab=ark");
  const dangling = app.getByTestId("ark-source-dangling");
  await expect(dangling).toBeVisible();
  await expect(dangling).toContainText("全局设置");
});
