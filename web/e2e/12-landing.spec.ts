/**
 * Logged-out state: product intro page + login card (email/password and OIDC; the mock enables both).
 *
 * # Why these cases call signOutMock() first
 *
 * The mock is always logged in by default (see the "login" part of data/api.ts), so AuthGate can never
 * enter the logged-out branch: **the landing page is unreachable by default in all other cases**. So
 * these cases explicitly switch to logged out, and the marker is cleared by the afterEach below so it
 * does not leak into other specs (workers=1, running serially, sharing one localStorage).
 *
 * These cases use a bare page instead of the app fixture: that fixture first does goto /workspaces
 * and then reset(), but when logged out there is no workspace UI on the page at all, and reset is not
 * what these cases check.
 *
 * # How far "after clicking" is checked
 *
 * Clicking the OIDC button **leaves this site** for the IdP. e2e cannot and should not really go
 * there (that needs a real person clicking consent, and real credentials). So this intercepts that
 * navigation and asserts **whether the address it is going to is right**: the test is "what the login
 * kick-off step got right", not "what the authorization page looks like".
 */

import { completeLogin, expect, signOutMock, test } from "./fixtures";

test.afterEach(async ({ page }) => {
  // Restore logged-in, or all later specs would stop at the landing page.
  await completeLogin(page);
});

test("未登录时先看到产品介绍", async ({ page }) => {
  await signOutMock(page);

  await expect(page.getByTestId("landing")).toBeVisible();
  await expect(page.getByRole("heading", { name: "企业级智能体托管与执行平台" })).toBeVisible();
  await expect(page.getByTestId("login-email")).toBeVisible();
  await expect(page.getByTestId("login-password")).toBeVisible();
  await expect(page.getByTestId("login-submit")).toHaveText("登录");
  await expect(page.getByTestId("login-oidc")).toHaveText("使用 SSO 登录");

  // The brand mark is an inline SVG (not <img>, see components/BrandMark.tsx).
  // **Assert the child count, not whether the element exists**: the lesson of the iconify trap is that
  // an empty <svg> also counts as "visible", so count that it really contains the base plate + three
  // color blocks = 4 shapes.
  const mark = page.getByTestId("brand-mark");
  await expect(mark).toBeVisible();
  expect(await mark.locator("rect, path").count()).toBe(4);

  // The Ark external link in the intro sentence: opens in a new tab, with rel (without it the opened
  // page could get window.opener)
  const arkLink = page.getByTestId("landing-ark-link");
  await expect(arkLink).toHaveText("方舟 Managed Agents");
  await expect(arkLink).toHaveAttribute(
    "href",
    "https://console.volcengine.com/ark/region:cn-beijing/managed-agents/quickstart",
  );
  await expect(arkLink).toHaveAttribute("target", "_blank");
  await expect(arkLink).toHaveAttribute("rel", /noreferrer/);

  // All four capability cards are there
  for (const title of ["托管 Agent", "多智能体协同", "自建容器执行", "飞书原生接入"]) {
    await expect(page.getByText(title, { exact: true })).toBeVisible();
  }

  // No failure message (this time we did not come back from a failed callback)
  await expect(page.getByTestId("login-error")).toHaveCount(0);
});

test("点 OIDC 登录跳到后端的发起登录接口", async ({ page }) => {
  await signOutMock(page);

  // Intercept that full-page navigation: its target is off-site (the backend 303s to the IdP),
  // and e2e should not really go there. The test is **whether the requested URL is right**.
  const navigations: string[] = [];
  await page.route("**/api/auth/oidc/login*", async (route) => {
    navigations.push(route.request().url());
    // Abort directly: do not let it really go; the page stays put and later assertions can continue.
    await route.abort();
  });

  await page.getByTestId("login-oidc").click();

  await expect.poll(() => navigations.length).toBeGreaterThan(0);
  const url = new URL(navigations[0]);
  expect(url.pathname).toBe("/api/auth/oidc/login");
  // next carries **the current page**. signOutMock switched to logged out on /workspaces
  // (it has to goto and then reload, see fixtures), so here it is /workspaces,
  // not "/". The case below checks deep links specifically.
  expect(url.searchParams.get("next")).toBe("/workspaces");
});

test("从别的页面进来时，登录后要能回到那一页", async ({ page }) => {
  await signOutMock(page);

  const navigations: string[] = [];
  await page.route("**/api/auth/oidc/login*", async (route) => {
    navigations.push(route.request().url());
    await route.abort();
  });

  // Visit a deep link directly. Logged out -> AuthGate shows the landing page, but the URL is still that deep link.
  await page.goto("/preferences");
  await expect(page.getByTestId("landing")).toBeVisible();

  await page.getByTestId("login-oidc").click();

  await expect.poll(() => navigations.length).toBeGreaterThan(0);
  // next must carry the original page, or after logging in you would oddly end up on the home page.
  expect(new URL(navigations[0]).searchParams.get("next")).toBe("/preferences");
});

/**
 * When the OIDC callback fails, the backend 303s back to `/?login_error=<code>`, and the landing page
 * must turn it into a readable message.
 *
 * ⚠️ **This group is the regression lock for "the login page must offer a way out"**: if login fails
 * and the page has no explanation (or is just blank), users think they missed the click and keep
 * clicking the same thing that is bound to fail. So there must be an explanation, and the login card
 * must still be there.
 */
test("回调失败时显示原因，并给一个重新登录的出路", async ({ page }) => {
  await signOutMock(page);

  const cases: { code: string; text: string }[] = [
    { code: "denied", text: "你取消了登录授权，请重新登录" },
    { code: "expired", text: "这个登录链接已失效，请重新登录" },
    { code: "no_identity", text: "没能确定你的邮箱，请联系管理员" },
    { code: "email_unverified", text: "你的邮箱在身份提供方那边还没有验证，验证后再登录" },
    { code: "unavailable", text: "登录服务暂时不可用，请稍后重试" },
    // An unknown code must fall back to "service unavailable"; no blank screen and no raw code shown.
    { code: "something-new", text: "登录服务暂时不可用，请稍后重试" },
  ];

  for (const c of cases) {
    await page.goto(`/?login_error=${c.code}`);
    const err = page.getByTestId("login-error");
    await expect(err).toBeVisible();
    await expect(err).toContainText(c.text);
    // Way out: the login card is still there.
    await expect(page.getByTestId("login-oidc")).toBeVisible();
    // The product intro is still visible: do not swap out the whole page for one error message.
    await expect(page.getByTestId("landing")).toBeVisible();
  }
});

test("已登录后直接进入应用", async ({ page }) => {
  await signOutMock(page);
  await expect(page.getByTestId("landing")).toBeVisible();

  // Simulate "the user clicked consent at the IdP + the backend set the cookie and sent them back".
  // In the real flow coming back is a fresh load, so reload once here too.
  await completeLogin(page);
  await page.reload();

  await expect(page.getByRole("heading", { name: "选择空间" })).toBeVisible();
  await expect(page.getByTestId("landing")).toHaveCount(0);
});

test("邮箱密码登录：密码错显示原因，对了直接进入应用", async ({ page }) => {
  await signOutMock(page);
  await expect(page.getByTestId("landing")).toBeVisible();

  await page.getByTestId("login-email").fill("someone@example.com");
  // Mock convention: when the password is wrong it returns invalid_credentials.
  await page.getByTestId("login-password").fill("wrong");
  await page.getByTestId("login-password").press("Enter");
  await expect(page.getByTestId("login-submit-error")).toHaveText("邮箱或密码不正确");

  await page.getByTestId("login-password").fill("correct-horse");
  await page.getByTestId("login-submit").click();
  // Same-page login, no reload: AuthGate switches to logged in directly via onLoggedIn.
  await expect(page.getByRole("heading", { name: "选择空间" })).toBeVisible();
  await expect(page.getByTestId("landing")).toHaveCount(0);
});

test("切到注册：密码太短被拒，改对后注册并登录", async ({ page }) => {
  await signOutMock(page);
  await page.getByTestId("login-toggle-signup").click();
  await expect(page.getByTestId("login-submit")).toHaveText("注册并登录");
  await expect(page.getByTestId("login-password")).toHaveAttribute("autocomplete", "new-password");

  await page.getByTestId("login-email").fill("new@example.com");
  await page.getByTestId("login-password").fill("short");
  await page.getByTestId("login-submit").click();
  await expect(page.getByTestId("login-submit-error")).toHaveText("密码长度要在 8–72 个字符之间");

  await page.getByTestId("login-password").fill("long-enough-pw");
  await page.getByTestId("login-submit").click();
  await expect(page.getByRole("heading", { name: "选择空间" })).toBeVisible();
});
