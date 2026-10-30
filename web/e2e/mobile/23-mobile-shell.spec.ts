/**
 * Layout contract of the mobile shell (iPhone 16 Pro, 402×681).
 *
 * # What these tests guard and what they do not
 *
 * They guard the **layout contract**: fixed top bar, no horizontal overflow, scrolling happens in the content area and not the whole page.
 * These three are measured accurately in Chromium, because they only concern the DOM and scroll containers.
 *
 * They **do not guard** safe areas and the keyboard: `env(safe-area-inset-*)` is always 0 in Chromium, the soft keyboard never really
 * opens, and `visualViewport` behaves differently from iOS. Those two can only be checked on a real device; do not take this file going green
 * as proof that mobile is fine (the config says the same).
 *
 * # All three assertions deliberately avoid the "element does not exist" style
 *
 * This repo has been bitten: `toHaveCount(0)` is satisfied in the frame **before the element has rendered**, so assertions of the
 * "something that should not appear" kind are often falsely green. So everything here uses numeric comparisons (scrollTop / scrollWidth /
 * boundingBox), and each one also asserts "the precondition really holds"; see the comments in each test.
 */

import { test, expect } from "../fixtures";
import type { Page } from "@playwright/test";

/**
 * Open the URL and **wait until the shell has really rendered**.
 *
 * ⚠️ This wait is not just to be safe; it is required. When `AuthGate` mounts it first probes the login state, and during that time
 * neither `app-scroll` nor the top bar is in the DOM. **And now the page during that time is completely empty**:
 * the startup gate's loading hint became "a text-less spinner that only appears after 300ms" (see StartupSpinner),
 * and on the fast path there is not even a spinner.
 *
 * One false green was observed: the "no horizontal overflow" test **passed on a page with only a spinner**. A page with only a
 * spinner of course does not overflow horizontally, but it verified nothing. **With the empty page this trap got deeper**
 * (an empty page also satisfies every "must not have X" assertion), so all three tests pass through this gate first.
 */
async function gotoShell(page: Page, path: string) {
  await page.goto(path);
  await expect(page.getByTestId("mobile-topbar")).toBeVisible();
  await expect(page.getByTestId("app-scroll")).toBeAttached();
}

/** Scroll the content area to the bottom and return scrollTop before and after, for asserting "it really scrolled". */
async function scrollMain(page: Page) {
  return page.evaluate(() => {
    const main = document.querySelector('[data-testid="app-scroll"]');
    if (!main) throw new Error("找不到 app-scroll —— AppShell 的滚动容器不在了");
    const before = main.scrollTop;
    main.scrollTop = main.scrollHeight;
    return { before, after: main.scrollTop, scrollHeight: main.scrollHeight };
  });
}

test("固定顶栏：内容滚动时顶栏不动", async ({ app }) => {
  // Use the run history page: its content is long, so there is room to scroll.
  await gotoShell(app, "/w/growth/runs");
  const topbar = app.getByTestId("mobile-topbar");

  const before = await topbar.boundingBox();
  const scrolled = await scrollMain(app);

  /*
    ⚠️ This line is the key to avoiding a false green.

    If the page content is less than one screen (less seed data, or the content area did not render), there is nothing to scroll,
    and the top bar of course does not move; the test would go green even "when the implementation is already broken". So first assert
    **the content really scrolled**, then compare the top bar position.
  */
  expect(scrolled.after).toBeGreaterThan(scrolled.before);

  const after = await topbar.boundingBox();
  expect(after?.y).toBe(before?.y);
});

test("无横向溢出：整页不能左右滑动", async ({ app }) => {
  await gotoShell(app, "/w/growth/runs");

  const m = await app.evaluate(() => {
    const main = document.querySelector('[data-testid="app-scroll"]')!;
    return {
      docScrollWidth: document.documentElement.scrollWidth,
      docClientWidth: document.documentElement.clientWidth,
      mainScrollWidth: main.scrollWidth,
      mainClientWidth: main.clientWidth,
      innerWidth: window.innerWidth,
    };
  });

  /*
    Pin the viewport width first. Without this, if the project is misconfigured (say the device preset got overwritten) the test would
    quietly run on a 1280-wide desktop viewport, where of course nothing overflows, and this assertion would say nothing.
    402 is the CSS width of iPhone 16 Pro.
  */
  expect(m.innerWidth).toBe(402);

  // The whole page must not scroll horizontally.
  expect(m.docScrollWidth).toBeLessThanOrEqual(m.docClientWidth);

  /*
    ⚠️ **The scroll container itself must be checked too; checking only documentElement is a false green.**

    Mutation testing forced this: after adding `w-[500px]` to PageBody, the version that only checks documentElement
    **was still all green**. The reason is that `<main>` has `overflow-y-auto`, and by the CSS spec, when one axis
    is not visible, visible on the other axis **computes to auto**. So the horizontal overflow is caught by main itself,
    documentElement's scrollWidth does not move at all, yet the user really can drag left and right inside main.

    In other words: overflow does not bubble up to documentElement; it stops at the nearest scroll container. To guard against
    "the user can swipe sideways", check the element the user is actually swiping.
  */
  expect(m.mainScrollWidth).toBeLessThanOrEqual(m.mainClientWidth);
});

test("滚动发生在内容区，不是整页", async ({ app }) => {
  await gotoShell(app, "/w/growth/runs");

  const scrolled = await scrollMain(app);
  expect(scrolled.after).toBeGreaterThan(scrolled.before);

  /*
    This line is the dividing line between "content area scrolls" and "whole page scrolls", and the only reason this test exists.

    Asserting only `main.scrollTop > 0` is not enough: when the shell is changed back to page-flow scrolling (`h-full` without
    `overflow-hidden`), main may still have a scrollTop and the test stays green. Only documentElement having no room to scroll
    really proves **the whole page cannot scroll**, that is, the user never gets the feeling of
    "the whole page sliding up and down", which is exactly requirement 1.
  */
  const pageScroll = await app.evaluate(() => ({
    docTop: document.documentElement.scrollTop,
    docCanScroll: document.documentElement.scrollHeight > document.documentElement.clientHeight,
  }));
  expect(pageScroll.docTop).toBe(0);
  expect(pageScroll.docCanScroll).toBe(false);
});

/**
 * Narrow-screen drawer: **after picking a nav item it must close itself**.
 *
 * # The failure this guards against
 *
 * The drawer used to have only two switches: the hamburger button opens it, the overlay closes it. **Nav items did not close it**, so after clicking
 * "Run history" the page did switch, but the drawer was still covering it; the user had to tap the overlay again to see
 * the page they just opened. It is not a crash, it is "not how mobile is expected to work", so only a test can pin it.
 *
 * # ⚠️ Assertion timing: do not start with toBeHidden / toHaveCount(0)
 *
 * This repo was bitten twice by false greens "satisfied in the frame before the element rendered". The defense here is **to first positively assert
 * the drawer really is open** (`toBeVisible` + a measured width), confirming that "disappearing" has a real
 * starting state, and only then wait for it to disappear. Without that step, even with no implementation change at all (the drawer never opened)
 * this test would be green.
 *
 * Closing is **unmounting**, not fading out (the drawer is conditionally rendered with no exit animation, on purpose; see the comment in
 * AppShell), so `toBeHidden()` waits for it to leave the DOM, with no need to wait for animation frames.
 */
test("窄屏抽屉：点导航项之后自动收起，且页面真的切过去了", async ({ app }) => {
  await gotoShell(app, "/w/growth/agents");

  /*
    Pin the viewport width. Without this, a misconfigured project makes the test quietly run on a 1280 desktop viewport,
    where the sidebar is always shown and the hamburger button does not exist, and the error would point at "button not found" instead of "wrong viewport".
  */
  expect(await app.evaluate(() => window.innerWidth)).toBe(402);

  const drawer = app.getByTestId("mobile-nav-drawer");
  // Precondition: before tapping the hamburger, the drawer must not be there.
  await expect(drawer).toBeHidden();

  await app.getByTestId("mobile-nav-open").click();

  /*
    ⚠️ **Positively confirm it really is open**; this is the foundation that keeps the whole test from being a false green.
    Measure the width, not just toBeVisible(): a 0-width overlay may also count as "visible" to Playwright,
    and in that state "disappearing" means nothing.
  */
  await expect(drawer).toBeVisible();
  const box = await drawer.boundingBox();
  expect(box?.width ?? 0).toBeGreaterThan(0);

  /*
    Click "Run history" inside the drawer. **It must be scoped to the drawer**: the same sidebar JSX is also rendered in the desktop copy
    (`hidden md:flex`, still in the DOM), and without scoping Playwright would count
    2 links with the same name and fail in strict mode, an error that does not point at "rendered twice".
  */
  await drawer.getByRole("link", { name: "运行历史" }).click();

  // 1. The page really switched (not just closing the drawer without navigating).
  await expect(app).toHaveURL("/w/growth/runs");
  await expect(app.getByTestId("run-list")).toBeVisible();

  // 2. The drawer closed itself: this is exactly what this change fixes.
  await expect(drawer).toBeHidden();

  /*
    3. ⚠️ Also confirm it is **not just transparent while still blocking clicks**. The check uses an element "clickable only once the drawer gets out of the way":
    the search box in the content area. The drawer is `fixed inset-0` covering the full screen, so as long as it is still there,
    this click is taken by it and the click times out.
  */
  await app.getByTestId("run-search").click();
  await expect(app.getByTestId("run-search")).toBeFocused();
});

/**
 * The same drawer: **tapping the "current page" item must close it too**.
 *
 * ⚠️ This is a separate test because it guards **a different failure mode**: an implementation that closes the drawer on `pathname` change
 * passes the test above but fails here. The user opens the drawer on the run history page and taps
 * "Run history" again; pathname does not change, so the drawer never closes. And this is a very common tap
 * (users use it to "close the drawer", or just want to refresh the current page).
 *
 * The implementation relies on `location.key`: react-router automatically downgrades a same-path Link to replace
 * (`createPath(location) === createPath(path)` in useLinkClickHandler), and replace
 * also generates a new key. So this test also pins "do not change key back to pathname".
 */
test("窄屏抽屉：点的是当前页那一项，也要收起来", async ({ app }) => {
  // Start on the run history page, so tapping "Run history" does not change pathname.
  await gotoShell(app, "/w/growth/runs");
  expect(await app.evaluate(() => window.innerWidth)).toBe(402);

  const drawer = app.getByTestId("mobile-nav-drawer");
  await app.getByTestId("mobile-nav-open").click();
  await expect(drawer).toBeVisible();
  expect((await drawer.boundingBox())?.width ?? 0).toBeGreaterThan(0);

  await drawer.getByRole("link", { name: "运行历史" }).click();

  // The URL did not change (that is the premise of this test), but the drawer must already be closed.
  await expect(app).toHaveURL("/w/growth/runs");
  await expect(drawer).toBeHidden();
  await app.getByTestId("run-search").click();
  await expect(app.getByTestId("run-search")).toBeFocused();
});
