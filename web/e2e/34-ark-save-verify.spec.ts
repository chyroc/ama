/**
 * The three levels of online validation when saving Ark credentials.
 *
 * The mock builds the three levels from marker words in the key (LocalApiClient.arkSaveGate):
 *
 *   key contains NOTOPEN    not activated: first save gets 422 ark_managed_agent_not_open -> saved after confirming in a dialog;
 *   key contains BADKEY     bad key: 422 ark_key_invalid, hard block, no "save anyway";
 *   key contains ARKUNAVAIL Ark could not verify: allowed + a transient warning.
 *
 * The marker word must come after the 16-character minimum length, or the local "too short" check
 * blocks it first.
 */

import type { Page } from "@playwright/test";
import { clearArkMock, expect, test } from "./fixtures";

async function fillManualKey(app: Page, key: string, testId = "ark-key-input") {
  await app.getByTestId("ark-source-select").click();
  await app.getByRole("option", { name: "手动填写" }).click();
  await app.getByTestId(testId).fill(key);
}

/** 32-character base + marker word, so length and character set pass the local format check first. */
const BAD_KEY = "ARK000000000000000000-BADKEY-0001";
const NOTOPEN_KEY = "ARK000000000000000-NOTOPEN-00002";
const UNAVAIL_KEY = "ARK00000000000000-ARKUNAVAIL-003";

test.beforeEach(async ({ app }) => {
  await clearArkMock(app);
});

test("坏 key 被硬拦下，弹窗给错误且没有落库", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=ark");
  await app.getByTestId("ark-setup").click();
  await fillManualKey(app, BAD_KEY);
  await app.getByTestId("modal-submit").click();

  // The error shows on the error bar of the form dialog.
  const err = app.getByTestId("modal-error");
  await expect(err).toBeVisible();
  await expect(err).toContainText("方舟拒绝了这把 API Key");

  // The not-activated confirmation dialog must **not** appear: a bad key has no "save anyway" way out.
  await expect(app.getByTestId("ark-not-activated-modal")).toHaveCount(0);

  // After closing, it is still the not-configured empty state: the bad key was not stored.
  await app.getByRole("button", { name: "取消" }).click();
  await expect(app.getByRole("heading", { name: "还没有配置方舟 API Key" })).toBeVisible();
});

test("未开通：先拦下，点「仍然保存」后落库", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=ark");
  await app.getByTestId("ark-setup").click();
  await fillManualKey(app, NOTOPEN_KEY);
  await app.getByTestId("modal-submit").click();

  // The form dialog closes and the not-activated confirmation dialog takes over; the text includes
  // the account id and two ways out.
  const dialog = app.getByTestId("ark-not-activated-modal");
  await expect(dialog).toBeVisible();
  await expect(app.getByTestId("ark-not-activated-message")).toContainText("2101581202");
  await expect(app.getByTestId("ark-not-activated-save-anyway")).toBeVisible();
  await expect(app.getByTestId("ark-not-activated-open-console")).toBeVisible();

  // Still the empty state before confirming.
  await expect(app.getByRole("heading", { name: "还没有配置方舟 API Key" })).toBeVisible();

  await app.getByTestId("ark-not-activated-save-anyway").click();
  await expect(dialog).toHaveCount(0);
  // Stored successfully: the mask appears.
  await expect(app.getByTestId("ark-masked")).toBeVisible();
});

test("未开通时点取消不落库", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=ark");
  await app.getByTestId("ark-setup").click();
  await fillManualKey(app, NOTOPEN_KEY);
  await app.getByTestId("modal-submit").click();

  await app.getByTestId("ark-not-activated-modal").getByRole("button", { name: "取消" }).click();
  await expect(app.getByRole("heading", { name: "还没有配置方舟 API Key" })).toBeVisible();
});

test("方舟没验成（超时/5xx）时放行保存并给易失警告", async ({ app }) => {
  await app.goto("/w/growth/settings?tab=ark");
  await app.getByTestId("ark-setup").click();
  await fillManualKey(app, UNAVAIL_KEY);
  await app.getByTestId("modal-submit").click();

  // Saved successfully, and the card shows the "not verified but saved" warning.
  await expect(app.getByTestId("ark-masked")).toBeVisible();
  const warning = app.getByTestId("ark-save-warning");
  await expect(warning).toBeVisible();
  await expect(warning).toContainText("没能完成校验");

  // The warning is transient: it disappears after a refresh (which goes through GET), and the credential is still there.
  await app.reload();
  await expect(app.getByTestId("ark-masked")).toBeVisible();
  await expect(app.getByTestId("ark-save-warning")).toHaveCount(0);
});

test("坏 key 在全局设置页同样被硬拦", async ({ app }) => {
  await app.goto("/preferences?tab=ark");
  await app.getByTestId("global-ark-setup").click();
  await fillManualKey(app, BAD_KEY, "global-ark-key-input");
  await app.getByTestId("global-ark-modal").getByTestId("modal-submit").click();

  await expect(app.getByTestId("modal-error")).toContainText("方舟拒绝了这把 API Key");
  await expect(app.getByTestId("ark-not-activated-modal")).toHaveCount(0);
});
