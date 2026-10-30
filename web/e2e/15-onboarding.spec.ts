/**
 * New-user onboarding.
 *
 * # What this set of cases guards
 *
 * The hardest point in the requirement is "**the same person is never onboarded twice**". It has four
 * independent ways to fail; the first two are here (the last two are the Go-side store suite +
 * service tests):
 *
 *   c. logged out should not trigger onboarding  -> "no onboarding popup when logged out"
 *   no more popups after skip/finish              -> "does not appear after skip" "does not appear after all six steps"
 *
 * "Does not appear again" is the focus of this set, and it has a trap: just asserting "after
 * clicking skip the overlay disappeared" **does not test persistence**; it only shows this session's
 * switch was turned off. So every case must **assert again after a reload**; only that step really
 * checks the state was remembered (the real backend stores it in PG, the mock in a localStorage key
 * that reset() cannot wipe).
 *
 * # Why resetOnboardingMock is needed
 *
 * The mock defaults to "onboarding already seen"; otherwise this bottom-right overlay would cover
 * buttons in that area for the other 100+ cases. So the "new user" state must be turned on
 * explicitly. page.route cannot intercept the API: in local mode LocalApiClient sends no network
 * requests at all, so there is nothing to intercept.
 */

import {
  expect,
  resetOnboardingMock,
  restoreOnboardingMock,
  signOutMock,
  completeLogin,
  test,
} from "./fixtures";
import type { Page } from "@playwright/test";

/**
 * Assert the onboarding overlay **really does not appear** (not "has not rendered yet at this moment").
 *
 * ⚠️ **You cannot just write `expect(card).toHaveCount(0)`.** The onboarding state is fetched
 * asynchronously (the mock has a 90ms delay), and `toHaveCount(0)` returns as soon as it is
 * satisfied: right after a reload the overlay has not rendered yet, so the assertion passes
 * instantly. **That form is equally green for an implementation with no persistence at all**: not a
 * guess, verified: with the markOnboarding line commented out, not one of the six cases went red.
 *
 * So two steps are needed: first wait for a positive anchor proving the page has finished loading,
 * then wait a little longer to cover the timing gap between the two async calls, and only then
 * assert absence.
 */
async function expectNoOnboarding(page: Page): Promise<void> {
  // Positive anchor: this button is on the workspace list page; it being visible means React has
  // mounted and finished one render.
  await expect(page.getByTestId("open-preferences")).toBeVisible();
  // Then leave time for the async onboarding fetch. It has no strict ordering with the anchor above,
  // so an explicit wait is required: a step you cannot skip when "asserting something never appears".
  await page.waitForTimeout(600);
  await expect(page.getByTestId("onboarding-card")).toHaveCount(0);
}

test.afterEach(async ({ page }) => {
  // Must restore: workers=1, localStorage shared across cases, and reset() cannot clear this key;
  // missing it makes every later case run with an onboarding overlay that covers things in the bottom-right corner.
  await restoreOnboardingMock(page);
  // Cases using signOutMock must also restore the logged-in state, or later specs all stop at the landing page.
  await completeLogin(page);
});

test("新用户登录后看到引导，六条概念都讲到", async ({ page }) => {
  await resetOnboardingMock(page);

  const card = page.getByTestId("onboarding-card");
  await expect(card).toBeVisible();
  // The first step must be "the workspace is the container": that is the platform's core rule
  // (DESIGN.md 1.1); in the wrong order onboarding would start from the last thing it should explain.
  await expect(page.getByTestId("onboarding-step-title")).toHaveText("空间是一切的容器");
  await expect(card).toContainText("1/6");

  // Go through every step, confirming all six are there and none is empty talk (each asserts a keyword
  // that could only come from this project).
  const expected = [
    { title: "空间是一切的容器", body: "运行历史" },
    { title: "创建 Agent 并选模型", body: "豆包" },
    { title: "用技能扩展 Agent", body: "SKILL.md" },
    { title: "对话就是一条运行记录", body: "运行历史" },
    { title: "接入飞书，让它在群里干活", body: "一个飞书应用只能绑一个 Agent" },
    { title: "在运行历史里回看，在用量里看花销", body: "用量与洞察" },
  ];
  for (let i = 0; i < expected.length; i++) {
    await expect(page.getByTestId("onboarding-step-title")).toHaveText(expected[i]!.title);
    await expect(card).toContainText(expected[i]!.body);
    await expect(card).toContainText(`${i + 1}/6`);
    if (i < expected.length - 1) await page.getByTestId("onboarding-next").click();
  }

  // The last step's primary button is "Get started", not "Next"; otherwise users reaching the end have no way out.
  await expect(page.getByTestId("onboarding-done")).toBeVisible();
  await expect(page.getByTestId("onboarding-next")).toHaveCount(0);
});

test("走完六步后不再出现，刷新也不出现", async ({ page }) => {
  await resetOnboardingMock(page);
  await expect(page.getByTestId("onboarding-card")).toBeVisible();

  for (let i = 0; i < 5; i++) await page.getByTestId("onboarding-next").click();
  await page.getByTestId("onboarding-done").click();
  await expect(page.getByTestId("onboarding-card")).toHaveCount(0);

  // **This step is the real assertion.** The previous line only shows this session's switch was turned off;
  // not appearing after a reload is what shows "finished" was remembered.
  await page.reload();
  await expectNoOnboarding(page);

  // Confirm again on another page: the state is account-level, unrelated to which page you are on.
  await page.goto("/w/growth");
  await page.waitForTimeout(600);
  await expect(page.getByTestId("onboarding-card")).toHaveCount(0);
});

test("跳过也持久化：刷新后不再引导", async ({ page }) => {
  await resetOnboardingMock(page);
  await expect(page.getByTestId("onboarding-card")).toBeVisible();

  await page.getByTestId("onboarding-skip").click();
  await expect(page.getByTestId("onboarding-card")).toHaveCount(0);

  // Skip **must** persist just like finishing. Without persistence, users get bothered at every login,
  // after they clearly said they don't want to see it: worse than having no onboarding.
  await page.reload();
  await expectNoOnboarding(page);
});

test("引导不阻塞界面：底下的页面照常可点", async ({ page }) => {
  await resetOnboardingMock(page);
  await expect(page.getByTestId("onboarding-card")).toBeVisible();

  // The requirement explicitly says "do not make it a blocking dialog covering the whole UI". The test
  // is that while the overlay is open, things underneath **really can be clicked**; with a backdrop this
  // step would time out.
  await page.getByTestId("open-preferences").click();
  await expect(page).toHaveURL(/\/preferences$/);
  // Switching pages should not remount onboarding back to step one (it lives outside the route table).
  await expect(page.getByTestId("onboarding-card")).toBeVisible();
});

test("未登录时不弹引导", async ({ page }) => {
  // First set the "should onboard" state, then sign out: if the onboarding check misses the login
  // state, an onboarding card would float on the landing page. **Without setting the state this case is
  // fake** (the default is already seen, so it never pops up anyway, and it is green whether or not the
  // implementation is right).
  await resetOnboardingMock(page);
  await expect(page.getByTestId("onboarding-card")).toBeVisible();

  await signOutMock(page);
  await expect(page.getByTestId("landing")).toBeVisible();
  // The positive anchor is already there (landing visible); wait a bit more to cover the async fetch,
  // then assert absence.
  await page.waitForTimeout(600);
  await expect(page.getByTestId("onboarding-card")).toHaveCount(0);
});

test("偏好设置里能重新查看引导（跳过之后也能）", async ({ page }) => {
  // Start from the "already skipped" state: exactly the people who most need the replay entry:
  // skip is stored, and without this entry one accidental click would turn the feature off forever.
  await resetOnboardingMock(page);
  await page.getByTestId("onboarding-skip").click();
  await expect(page.getByTestId("onboarding-card")).toHaveCount(0);

  await page.getByTestId("open-preferences").click();
  await page.getByTestId("replay-onboarding").click();

  // Replay must start from **step one**, not continue from last time's progress.
  await expect(page.getByTestId("onboarding-card")).toBeVisible();
  await expect(page.getByTestId("onboarding-step-title")).toHaveText("空间是一切的容器");
});
