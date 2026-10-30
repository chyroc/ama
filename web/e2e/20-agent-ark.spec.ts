/**
 * How three-level Ark credentials look in the UI: agent -> workspace -> account level, the more
 * specific the higher the priority.
 *
 * # What this group guards is "can users tell which key is in use right now"
 *
 * The most likely problem with three-level config is not that it does not work, but that **users do
 * not know which level they are using**. The backend fallback order is locked in by unit tests
 * (core/agentark_test.go); this locks in the UI: each of the three sources has its own source bar,
 * and **following the upper level must not be shown as "custom"**.
 *
 * # The key length matters here
 *
 * The mock uses `apiKey.length % 2` to simulate Ark's validation result (even -> usable, odd ->
 * unusable), so the literals below **must not change length casually**, or the "unusable" branch would
 * silently become "usable". Same as 08-ark-credential.spec.ts.
 */

import type { Page } from "@playwright/test";
import { clearArkMock, expect, test } from "./fixtures";

/** 32 characters, even -> the mock judges it usable. Used at the workspace level. */
const SPACE_KEY = "ARK00000000000000000000000000001";
/** 32 characters, even -> usable. Used at the agent level; different from the workspace one so we can assert which is in use. */
const AGENT_KEY = "ARKAGENT0000000000000000000000AA";

const AGENT_PATH = "/w/growth/agents/agent_analyst";

/** The account in the seed that can list keys. */
const ACCOUNT_OK = "2128853309";
/** The key in the seed used for assertions (`api-key-20260822165212`). */
const KEY_OK = "4169105";

/**
 * Pick an account in the picker.
 *
 * The account level is a **searchable dropdown** (a precondition that folds away after selection to
 * make room for step two), so the popover must be opened before clicking an item; you cannot just
 * `getByText().click()` as when it was laid out flat.
 *
 * ⚠️ **Wait for the account list to finish loading before opening the popover.**
 *
 * StrictMode runs effects twice in dev, and the second run first sets `accounts` to null and then to a
 * new array; RAC's ListBox then rebuilds its collection, and **the items in an already open popover are
 * removed from the DOM**. The symptom is an occasional `element was detached from the DOM` (measured: in
 * 6 batch runs, 3 had one failure each, and not even the same one each time), which looks like flakiness,
 * an animation, or pollution from another case, and never points to the real cause.
 *
 * Tricks like `hover()` to "wait for it to stop moving" **do not work**: the element is not moving, it
 * is being replaced. The right way is to wait for the stable state "the list is in place": the account
 * count text appearing means accounts has settled (it only renders when accounts is non-empty).
 */
async function pickAccount(app: Page, id: string) {
  // See the function header: wait for accounts to settle (the count text appears) before opening, or
  // it is occasionally detached.
  await expect(app.getByTestId("ark-account-combo")).toContainText("个账号");
  await app.getByTestId("ark-account-combo").getByRole("button").click();
  await app.getByTestId(`ark-account-${id}`).click();
}

/** Pick a key. The second level is now also a dropdown and, like pickAccount, must be opened first. */
async function pickKey(app: Page, id: string) {
  await app.getByTestId("ark-key-combo").getByRole("button").click();
  await app.getByTestId(`ark-key-${id}`).click();
}

/**
 * Switch to "fill in manually" and fill in a key.
 *
 * ⚠️ **The manual input is now not rendered by default**: the source dropdown defaults to "pick from
 * Volcengine account", so the source must be switched first to get the input box. A direct
 * `fill("ark-key-input")` reports that the element does not exist.
 */
async function fillManualKey(app: Page, key: string, testId = "ark-key-input") {
  await app.getByTestId("ark-source-select").click();
  await app.getByRole("option", { name: "手动填写" }).click();
  await app.getByTestId(testId).fill(key);
}

/*
 * This group starts from "never configured", and each case then configures upward as needed.
 *
 * ⚠️ The seed now **configures a workspace-level key by default** (see seedArkCredentials in
 * data/api.ts; that keeps the new pre-check guidance on the chat page from blocking 30-odd chat cases).
 * Without clearing it:
 *
 *   - `setSpaceKey` would fail: the `ark-setup` it clicks only renders in the empty state; when
 *     configured it is `ark-replace` there;
 *   - the "none of the three levels configured" case would be unreachable.
 *
 * Both symptoms stop at "element not found", neither points to "the seed already configured one", so
 * it is reset here for all of them.
 */
test.beforeEach(async ({ app }) => {
  await clearArkMock(app);
});

/** Configure a key for this workspace (the middle of the three levels). */
async function setSpaceKey(app: Page) {
  await app.goto("/w/growth/settings?tab=ark");
  await app.getByTestId("ark-setup").click();
  await fillManualKey(app, SPACE_KEY);
  await app.getByTestId("modal-submit").click();
  // ⚠️ Wait for the mask to appear before moving on, or the next navigation would interrupt the save
  // (showing up as "configured but no effect").
  await expect(app.getByTestId("ark-masked")).toContainText("*");
}

test("默认跟随空间：来源条说清了那把 key 从哪来", async ({ app }) => {
  await setSpaceKey(app);
  await app.goto(AGENT_PATH);

  // Following the workspace -> shows the "from workspace settings" bar, and **not** the "its own" one.
  await expect(app.getByTestId("agent-ark-source-inherited")).toBeVisible();
  await expect(app.getByTestId("agent-ark-source-agent")).toHaveCount(0);
  await expect(app.getByTestId("agent-ark-source-inherited")).toContainText("来自空间设置");

  // The effective mask is the workspace key (not empty: that would make people think this agent has no credentials).
  await expect(app.getByTestId("agent-ark-masked")).toContainText("*");

  // The button text is "configure its own", not "replace": it has no key of its own now.
  await expect(app.getByTestId("agent-ark-setup")).toBeVisible();
});

test("给 agent 单独配一把：来源条变成「自己的」，且与空间那把不同", async ({ app }) => {
  await setSpaceKey(app);
  await app.goto(AGENT_PATH);

  const spaceMasked = await app.getByTestId("agent-ark-masked").innerText();

  await app.getByTestId("agent-ark-setup").click();
  await fillManualKey(app, AGENT_KEY);
  await app.getByTestId("modal-submit").click();

  await expect(app.getByTestId("agent-ark-source-agent")).toBeVisible();
  await expect(app.getByTestId("agent-ark-source-inherited")).toHaveCount(0);

  // **The effective key changed**: the evidence in the UI of "the more specific the higher the priority".
  const own = app.getByTestId("agent-ark-masked");
  await expect(own).toContainText("*");
  await expect(own).not.toHaveText(spaceMasked);
  // The plaintext does not stay on the page.
  await expect(app.getByText(AGENT_KEY)).toHaveCount(0);

  // Still there after a refresh.
  await app.reload();
  await expect(app.getByTestId("agent-ark-source-agent")).toBeVisible();
});

test("「改用空间设置」把 agent 退回跟随，空间那把重新生效（核心诉求）", async ({ app }) => {
  await setSpaceKey(app);
  await app.goto(AGENT_PATH);
  const spaceMasked = await app.getByTestId("agent-ark-masked").innerText();

  // First configure its own.
  await app.getByTestId("agent-ark-setup").click();
  await fillManualKey(app, AGENT_KEY);
  await app.getByTestId("modal-submit").click();
  await expect(app.getByTestId("agent-ark-source-agent")).toBeVisible();

  // Then change it back to following.
  await app.getByTestId("agent-ark-use-workspace").click();

  await expect(app.getByTestId("agent-ark-source-inherited")).toBeVisible();
  await expect(app.getByTestId("agent-ark-source-agent")).toHaveCount(0);
  // **The workspace key takes effect again**, rather than becoming "not configured".
  await expect(app.getByTestId("agent-ark-masked")).toHaveText(spaceMasked);
});

test("改空间那把 key，跟随它的 agent 立刻跟着变（联动）", async ({ app }) => {
  await setSpaceKey(app);
  await app.goto(AGENT_PATH);
  const before = await app.getByTestId("agent-ark-masked").innerText();

  // Go back to workspace settings and swap the key (32 characters, even, different from SPACE_KEY).
  await app.goto("/w/growth/settings?tab=ark");
  await app.getByTestId("ark-replace").click();
  await fillManualKey(app, "ARKROTATED000000000000000000BBBB");
  await app.getByTestId("modal-submit").click();
  await expect(app.getByTestId("ark-masked")).toContainText("*");

  // ⚠️ This is the UI evidence that "lower levels do not store a copy of the upper level": if the agent
  // side copied it, this would still show the old mask, while the source bar still says "from workspace
  // settings", with no way to tell.
  await app.goto(AGENT_PATH);
  await expect(app.getByTestId("agent-ark-masked")).not.toHaveText(before);
});

test("三级都没配：给的引导指向空间设置，不是让人给每个 agent 配", async ({ app }) => {
  await app.goto(AGENT_PATH);

  await expect(app.getByTestId("agent-ark-source-missing")).toBeVisible();
  await expect(app.getByTestId("agent-ark-source-missing")).toContainText("空间设置");
  // With no usable key, the mask field should not show (that would make people think it is configured).
  await expect(app.getByTestId("agent-ark-masked")).toHaveCount(0);
});

test("从火山账号选一把：明文不经过前端", async ({ app }) => {
  await app.goto(AGENT_PATH);
  await app.getByTestId("agent-ark-setup").click();

  // Pick an account -> list the keys under that account.
  await expect(app.getByTestId("ark-account-combo")).toBeVisible();
  await pickAccount(app, ACCOUNT_OK);

  await expect(app.getByTestId("ark-key-combo")).toBeVisible();
  // Non-Active keys must be marked: picking one would not work, and the failure then would show as "invalid key".
  // The dropdown must be open to see the items, so this assertion moved around pickKey.
  await app.getByTestId("ark-key-combo").getByRole("button").click();
  await expect(app.getByTestId("ark-key-3314746")).toContainText("Inactive");
  await app.getByTestId(`ark-key-${KEY_OK}`).click();
  await app.getByTestId("modal-submit").click();

  // It was stored, at the "its own" level.
  await expect(app.getByTestId("agent-ark-source-agent")).toBeVisible();
  await expect(app.getByTestId("agent-ark-masked")).toContainText("*");
});

test("没权限的火山账号：报错说清是哪个账号缺什么权限", async ({ app }) => {
  await app.goto(AGENT_PATH);
  await app.getByTestId("agent-ark-setup").click();

  await pickAccount(app, "2100444922");

  // ⚠️ It must not show "this account has no keys": that would make users create a new key, which
  // would also be rejected.
  const err = app.getByTestId("ark-fetch-error");
  await expect(err).toBeVisible();
  await expect(err).toContainText("2100444922");
  await expect(err).toContainText("ark:ListApiKeys");
});

// ---------------------------------------------------------------------------
// "Fetch from Volcengine account" must work in all three places
// ---------------------------------------------------------------------------
//
// ⚠️ These three were added **to fill a gap**: the first version only wired the picker into the Agent
// card, while the personal level (/preferences?tab=ark) and the workspace level (/settings?tab=ark)
// dialogs still only allowed manual input: the backend accepts fetchRef in all three places, and the
// frontend wired only one. That missed wiring causes **zero errors**: manual input works as usual,
// users just cannot see the "fetch from Volcengine account" part, and they would not know it should be
// there. So each of the three places gets a case.

test("个人级（/preferences?tab=ark）也能从火山账号选一把", async ({ app }) => {
  await app.goto("/preferences?tab=ark");
  await app.getByTestId("global-ark-setup").click();

  // The picker part must be there (if not wired up, it does not render at all).
  await expect(app.getByTestId("ark-fetch-picker")).toBeVisible();
  await pickAccount(app, ACCOUNT_OK);
  await pickKey(app, KEY_OK);

  // After selecting there is a clear receipt, and **the manual input is not there at all** (automatic by
  // default, no longer "disabled").
  await expect(app.getByTestId("ark-fetch-picked")).toBeVisible();
  await expect(app.getByTestId("global-ark-key-input")).toHaveCount(0);
  await app.getByTestId("modal-submit").click();

  await expect(app.getByTestId("global-ark-masked")).toContainText("*");
});

test("空间级（/settings?tab=ark）也能从火山账号选一把", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=ark");
  await app.getByTestId("ark-setup").click();

  await expect(app.getByTestId("ark-fetch-picker")).toBeVisible();
  await pickAccount(app, ACCOUNT_OK);
  await pickKey(app, KEY_OK);
  await app.getByTestId("modal-submit").click();

  await expect(app.getByTestId("ark-masked")).toContainText("*");
});

test("手填那条路没被破坏（自动获取只是多一条来路）", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=ark");
  await app.getByTestId("ark-setup").click();

  // Switch to manual and fill it in: this was the only path before automatic fetching was added, and it
  // must not be blocked.
  // ⚠️ The input box is not rendered by default (the source defaults to "pick from Volcengine"), so the
  // source must be switched first.
  await fillManualKey(app, SPACE_KEY);
  await app.getByTestId("modal-submit").click();

  await expect(app.getByTestId("ark-masked")).toContainText("*");
  await expect(app.getByText(SPACE_KEY)).toHaveCount(0);
});
