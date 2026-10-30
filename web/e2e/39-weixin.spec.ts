/**
 * WeChat channel: connect a bot by scanning a QR code, bind it to an agent, disconnect it.
 *
 * Runs on the mock (LocalApiClient): the mock reports "scanned" on the first poll and confirms once
 * completeWeixinLogin() is called, standing in for a person scanning with their phone.
 */

import { completeWeixinLogin, expect, test } from "./fixtures";

test("扫码接入微信机器人，绑定 Agent，再断开", async ({ app }) => {
  await app.goto("/w/growth/weixin");
  await app.getByTestId("weixin-connect").click();

  const panel = app.getByTestId("weixin-login-panel");
  await expect(panel).toBeVisible();
  await expect(app.getByTestId("weixin-qr")).toBeVisible();

  await completeWeixinLogin(app);
  await expect(app.getByTestId("weixin-login-status")).toHaveText("已接入", { timeout: 15000 });

  const card = app.locator('[data-testid^="weixin-card-"]').first();
  await expect(card).toBeVisible();
  await expect(card).toContainText("还没绑定 Agent");
  const accountId = (await card.getAttribute("data-testid"))!.replace("weixin-card-", "");

  await app.getByTestId("weixin-login-close").click();
  await expect(panel).toHaveCount(0);

  await app.getByTestId(`weixin-agent-select-${accountId}`).click();
  await app.getByRole("option").nth(1).click();
  await expect(card).not.toContainText("还没绑定 Agent");

  // The binding survives a reload (it is stored, not just page state).
  await app.reload();
  await expect(app.getByTestId(`weixin-card-${accountId}`)).not.toContainText("还没绑定 Agent");

  await app.getByTestId(`weixin-delete-${accountId}`).click();
  await app.getByRole("dialog").getByRole("button", { name: "断开接入" }).click();
  await expect(app.getByTestId(`weixin-card-${accountId}`)).toHaveCount(0);
});
