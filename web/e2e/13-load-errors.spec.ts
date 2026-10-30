/**
 * The error state when loading fails.
 *
 * # The regression this spec guards against
 *
 * Pages used to all have this shape: a bare `await` in `load` with no try/catch, and the loading
 * state expressed as `data === null`. So **any reject left the page stuck on the skeleton forever**:
 * visually a blank screen, with no message in the UI, only an unhandled rejection in the console.
 *
 * It stayed hidden for a long time because these APIs never fail in the mock or in local
 * development; after RBAC launched, non-members / insufficient roles always get 403, and that path
 * was really hit.
 *
 * So this checks three things; missing any one lets the bug quietly come back:
 *
 * 1. on failure **the skeleton must disappear** (otherwise it is the old dead state);
 * 2. 403 shows "no access" with **no retry button**: retrying ten thousand times is still 403;
 * 3. other errors show "failed to load" **with a retry button**, and clicking it really recovers.
 *
 * Failures are injected with the mock's own switch, not `page.route`: e2e runs in local mode, and
 * LocalApiClient sends no network requests at all, so there is nothing to intercept (see
 * failReadsMock in fixtures).
 */

import { expect, failReadsMock, restoreReadsMock, test } from "./fixtures";

/*
 * The marker lives in a separate localStorage key (reset() cannot wipe it, which is the point), so
 * it must be cleared after every case: workers=1, running serially and sharing one localStorage,
 * so missing it once makes the whole batch of later specs start with "failed to load".
 */
test.afterEach(async ({ page }) => {
  await restoreReadsMock(page);
});

/** The overview page is the home page of every workspace, and the first page non-members run into. */
test.describe("概览页", () => {
  test("403 时显示无权访问，且不给重试按钮", async ({ app }) => {
    await failReadsMock(app, "access_denied");
    await app.goto("/w/growth");

    const denied = app.getByTestId("load-denied");
    await expect(denied).toBeVisible();
    await expect(denied).toContainText("无权访问");
    // The backend's Chinese message comes through as-is, so the user knows whom to ask
    await expect(denied).toContainText("您不是该工作区的成员，无权访问");

    // **The retry button must not exist**: retrying a 403 ten thousand times is still 403
    await expect(app.getByTestId("load-retry")).toHaveCount(0);
  });

  test("其它错误显示加载失败，点重试能恢复", async ({ app }) => {
    await failReadsMock(app, "internal");
    await app.goto("/w/growth");

    const error = app.getByTestId("load-error");
    await expect(error).toBeVisible();
    await expect(error).toContainText("加载失败");

    // After the API recovers, click retry; the page must really show the data (not stay in the error state)
    await restoreReadsMock(app);
    await app.getByTestId("load-retry").click();

    await expect(app.getByTestId("load-error")).toHaveCount(0);
    await expect(app.getByRole("heading", { name: "增长中台" })).toBeVisible();
  });

  test("失败时不能停在骨架屏上", async ({ app }) => {
    await failReadsMock(app, "access_denied");
    await app.goto("/w/growth");

    // The error state appearing means the skeleton branch has given way: exactly the opposite of the original bug
    await expect(app.getByTestId("load-denied")).toBeVisible();
    // The error state and the skeleton are mutually exclusive branches: the four metric cards on the
    // overview page must not appear either
    await expect(app.getByText("最近运行")).toHaveCount(0);
  });
});

/**
 * The other pages share the same error state.
 *
 * Checking every page is not worth it (same component, same branch), but **we must at least prove
 * the mechanism is wired up on other pages too**: a missing wiring also shows up as a blank screen,
 * not an error.
 */
test.describe("其它页面", () => {
  for (const [name, path] of [
    ["Agent 列表", "/w/growth/agents"],
    ["技能", "/w/growth/skills"],
    ["运行历史", "/w/growth/runs"],
    ["飞书", "/w/growth/feishu"],
  ] as const) {
    test(`${name} 403 时显示无权访问`, async ({ app }) => {
      await failReadsMock(app, "access_denied");
      await app.goto(path);

      await expect(app.getByTestId("load-denied")).toBeVisible();
      await expect(app.getByTestId("load-retry")).toHaveCount(0);
    });
  }
});
