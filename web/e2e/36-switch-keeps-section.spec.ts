/**
 * Workspace switcher: **switching workspaces stays in the current section**, and detail pages drop
 * to the list level.
 *
 * Paired cases; missing any one lets a wrong implementation go all green:
 *
 *   /w/A/runs          -> /w/B/runs    the section really is kept (the feature itself)
 *   /w/A/runs/<id>     -> /w/B/runs    **drops a level**, does not carry A's id into B's address
 *   /w/A               -> /w/B         at the root stays at the root (do not build `/w/B/`)
 *   all routes with id -> their list levels  a route added later without config gets caught here
 *
 * # Why the "drop a level" case must be locked in separately
 *
 * With only the first case, an implementation like `navigate(pathname.replace(A, B))` is still all
 * green, and it carries `/w/A/runs/sesn-xxx` over as `/w/B/runs/sesn-xxx`, an id that certainly does
 * not exist in B. This repo explicitly returns 404 for missing resources, so that would be a way to
 * **see an error page just by switching workspaces**, much worse than going back to the root. This is
 * exactly the target of mutation testing.
 *
 * # Why synthetic ids instead of real runs from the seed
 *
 * What is asserted is **the address after switching**, not the content of the detail page. Synthetic
 * ids keep the cases independent of which data is in the seed (the seed has no schedules / issues at
 * all), so they do not need to change when the seed changes. The detail page renders a "does not
 * exist" empty state, while AppShell and the switcher stay in place, which is exactly what we want here.
 */

import { expect, test } from "./fixtures";

/**
 * The second workspace. The seed has only growth; switching needs two to be testable.
 *
 * ⚠️ **Use a pure ASCII name**. The key is derived from the name automatically, and the derivation is
 * **async** (the pinyin dictionary loads on demand, see lib/slug.ts). Filling in a Chinese name and
 * then the key makes them fight: in practice a Chinese name for "supply chain" + "supply" gave the key
 * `gong-ying-liansupply`: the derived value landed after `fill` and was put in front of the filled
 * value. With an ASCII name the derived value is the name itself (`supply`), the same as the
 * hand-filled one, so the race no longer exists. The `expect` step below is also the gate for this race.
 */
const OTHER = { name: "supply", key: "supply" };

/**
 * Create the second workspace and **stay inside it** (the creation flow enters it automatically by design).
 *
 * Each case then does its own `goto` to some growth address, so there is no need to switch back
 * here: that would only add one more navigation and one more thing that can break.
 */
async function withTwoWorkspaces(app: import("@playwright/test").Page) {
  await app.goto("/workspaces");
  await app.getByTestId("new-workspace").click();
  await app.getByTestId("ws-name").fill(OTHER.name);
  await app.getByTestId("ws-key").fill(OTHER.key);
  await app.getByTestId("ws-submit").click();
  // This step is also the gate for the race above: if the key was polluted by the derived value it
  // goes red here, instead of a later assertion reporting an address with no visible cause.
  await expect(app).toHaveURL(new RegExp(`/w/${OTHER.key}$`));
}

/**
 * Open the switcher and pick a menu item (some workspace's key, or "Manage all workspaces").
 * Workspaces are matched by key: names may repeat, keys do not.
 *
 * ⚠️ **On narrow screens open the drawer first**. Below `md` the sidebar is not always shown (the
 * switcher is in `mobile-nav-drawer`); without opening the drawer the switcher is not visible, and
 * the error is "element not visible", not "you did not open the drawer on a narrow screen".
 *
 * ⚠️ **The test is "is the hamburger button visible", not "is the switcher visible".** The latter was
 * hit once: right after `goto`, neither has been drawn yet, `isVisible()` returns false for the
 * switcher, so the desktop viewport also took the open-drawer branch and then got stuck on a hamburger
 * button that is **never visible on desktop** until a 60-second timeout, and the error pointed at
 * the hamburger button, not at "the wrong test was chosen".
 *
 * The hamburger button is the **mutually exclusive** one: it only exists and is visible on narrow
 * screens; on desktop it is in the DOM but hidden by `md:hidden`. So wait for it with `toBeVisible()`
 * (a retrying assertion that can wait for the page to finish drawing), and a short timeout + catch
 * turns "it is never visible on desktop" into a quick no.
 */
async function switchTo(app: import("@playwright/test").Page, item: string) {
  const hamburger = app.getByTestId("mobile-nav-open");
  const narrow = await hamburger
    .waitFor({ state: "visible", timeout: 3_000 })
    .then(() => true)
    .catch(() => false);
  /*
   * ⚠️ **On narrow screens limit the scope to the drawer.**
   *
   * Once the drawer opens there are **two copies** of the sidebar in the DOM (the desktop one is always
   * mounted, just hidden by `md:hidden`, and the drawer embeds another), so
   * `getByTestId("workspace-switcher")` hits Playwright's strict mode: `resolved to 2 elements`.
   * Papering over it with `.first()` is wrong: the first in DOM order is **the desktop copy**, invisible
   * on narrow screens, and clicking it just times out again.
   */
  let scope: import("@playwright/test").Locator | import("@playwright/test").Page = app;
  if (narrow) {
    const drawer = app.getByTestId("mobile-nav-drawer");
    await hamburger.click();
    await drawer.waitFor({ state: "visible" });
    scope = drawer;
  }
  await scope.getByTestId("workspace-switcher").click();
  await app.getByRole("menuitem", { name: new RegExp(item) }).first().click();
}

test("① 在列表页切换空间 → 留在同一个板块", async ({ app }) => {
  await withTwoWorkspaces(app);

  await app.goto("/w/growth/runs");
  await switchTo(app, OTHER.key);

  // Section kept: it reached the new workspace and is still on run history
  await expect(app).toHaveURL(new RegExp(`/w/${OTHER.key}/runs$`));
  /*
   * And it **really entered that workspace**, not just changed the address bar.
   *
   * ⚠️ The test cannot use things in the sidebar (the "Overview" link, the switcher text): **on narrow
   * screens the drawer closes automatically after successful navigation** (AppShell listens to
   * location.key, see the comment there), the sidebar unmounts entirely, and those assertions find no
   * element on iphone, red for reasons unrelated to what this case tests.
   *
   * The workspace name has a stable spot in each viewport: the top bar on narrow screens, the switcher
   * on desktop. Take the union of both, then filter by **visibility** for the one really present in
   * the current viewport.
   *
   * ⚠️ **`.first()` is wrong here** (hit before): the desktop sidebar copy is **still in the DOM** on
   * narrow screens (just hidden by `md:hidden`), `.first()` picks it by DOM order, and then asserts
   * that an always-hidden element is visible: the error says "element is hidden", not pointing at all
   * to "you picked the wrong copy".
   */
  await expect(
    app
      .getByTestId("mobile-topbar")
      .or(app.getByTestId("workspace-switcher"))
      .filter({ hasText: OTHER.name })
      .filter({ visible: true }),
  ).toHaveCount(1);
});

test("② 在**详情页**切换空间 → 降级到列表层，不带旧 id", async ({ app }) => {
  await withTwoWorkspaces(app);

  // Synthetic id: what is asserted is the address, not this run's content (see the file header)
  await app.goto("/w/growth/runs/run_1");
  await switchTo(app, OTHER.key);

  await expect(app).toHaveURL(new RegExp(`/w/${OTHER.key}/runs$`));
  // ⚠️ The regex above already ends with `$`, but lock in "the old id is not in the address" once more
  // explicitly: an implementation that carries the id over either lands on `/runs/run_1` (red above) or
  // stuffs it into the query.
  expect(app.url()).not.toContain("run_1");
});

test("③ 在空间根切换空间 → 仍然是空间根", async ({ app }) => {
  await withTwoWorkspaces(app);

  await app.goto("/w/growth");
  await switchTo(app, OTHER.key);

  // No extra trailing slash: `/w/B/` and `/w/B` are equivalent for routing, but that slash in the
  // address bar is dirty, and it shows the implementation built it as "root + empty section".
  await expect(app).toHaveURL(new RegExp(`/w/${OTHER.key}$`));
});

/**
 * ④ Go through **all routes with an id / sub-path**, asserting that after switching each lands on its
 * list level.
 *
 * This one is for **the future**: when a new `xxx/:id` route is added and the allowlist is forgotten,
 * it goes red. Without it, each one missed is a silent trap: switching workspaces on that page drops
 * the user somewhere else, and nothing reports an error.
 *
 * `expected` is all list levels; not a single id should appear in the results.
 */
const ID_BEARING_ROUTES: { from: string; expected: string }[] = [
  { from: "/agents/agent_triage", expected: "/agents" },
  // No id, but still a sub-path under `/agents`: it makes sense in B (create an agent), yet the
  // allowlist only looks at the first segment, so it still drops to the list. This is a known
  // trade-off, not a bug.
  { from: "/agents/new", expected: "/agents" },
  { from: "/chat/run_1", expected: "/chat" },
  { from: "/schedules/sch_nonexistent", expected: "/schedules" },
  { from: "/runs/run_1", expected: "/runs" },
  { from: "/skills/skill-20260801-triage", expected: "/skills" },
  { from: "/issues/iss_nonexistent", expected: "/issues" },
  // Feishu connect is a three-step flow page with sessionId in the query; after switching workspaces
  // that session is meaningless
  { from: "/feishu/connect", expected: "/feishu" },
];

for (const { from, expected } of ID_BEARING_ROUTES) {
  test(`④ ${from} 切换空间 → ${expected}（列表层）`, async ({ app }) => {
    await withTwoWorkspaces(app);

    await app.goto(`/w/growth${from}`);
    await switchTo(app, OTHER.key);

    await expect(app).toHaveURL(new RegExp(`/w/${OTHER.key}${expected}$`));
  });
}

test("query 不带过去：`?agent=` 的值是 A 空间的 agent id", async ({ app }) => {
  /*
   * This locks in why the smarter-looking "keep the filter, drop the id" implementation is not done.
   *
   * `/runs?agent=` is a filter slot whose value is an agent id (`RunsPage` calls the API with it
   * directly). Carrying it over makes B filter by an id that does not exist, giving **an empty list + a
   * blank filter bar**: zero errors, and the user thinks "workspace B has nothing". So none are carried.
   */
  await withTwoWorkspaces(app);

  await app.goto("/w/growth/runs?agent=agent_triage");
  await switchTo(app, OTHER.key);

  await expect(app).toHaveURL(new RegExp(`/w/${OTHER.key}/runs$`));
  expect(app.url()).not.toContain("agent_triage");
});

test("`?tab=` 也不带：settings/members 那条重定向进来的一样降级", async ({ app }) => {
  // `/settings/members` is first redirected to `/settings?tab=members`, so this also covers
  // "does the redirected address switch correctly" and "the tab does not carry over".
  await withTwoWorkspaces(app);

  await app.goto("/w/growth/settings/members");
  await expect(app).toHaveURL(/\/w\/growth\/settings\?tab=members$/);

  await switchTo(app, OTHER.key);

  await expect(app).toHaveURL(new RegExp(`/w/${OTHER.key}/settings$`));
});

test("在全局设置页切换空间 → 留在这一页，只换外壳的来源", async ({ app }) => {
  /*
   * `/preferences` is the only address that has the switcher but is **not under `/w/:key`** (the shell
   * is decided by `?from=`, see PreferencesRoute). Treating it as a normal sub-page would build
   * `/w/B/preferences`, which is a compatibility redirect: it works but takes an extra hop and loses the tab.
   *
   * This page's content is **account-level** (the same from any workspace), so switching workspaces
   * should stay in place.
   */
  await withTwoWorkspaces(app);

  await app.goto("/preferences?from=growth&tab=ark");
  /*
   * The shell really is applied; otherwise "switching workspaces" does not apply to this page at all.
   *
   * ⚠️ The test is **at least one** switcher, not visible and not exactly one: on narrow screens the
   * switcher is in the closed drawer (not visible), and once the drawer opens there are two copies of
   * the sidebar in the DOM (see the switchTo notes). Neither is related to what this case tests, and
   * pinning the count would only make it go red for unrelated reasons.
   */
  await expect(app.getByTestId("workspace-switcher").first()).toBeAttached();

  await switchTo(app, OTHER.key);

  await expect(app).toHaveURL(/\/preferences\?/);
  expect(new URL(app.url()).searchParams.get("from")).toBe(OTHER.key);
  // The tab must be kept: tabs on this page are a closed enum and point to no resource; losing it
  // would throw the user back to the default appearance page
  expect(new URL(app.url()).searchParams.get("tab")).toBe("ark");
});

test("「管理所有空间」不受影响，仍然去列表页", async ({ app }) => {
  // This branch is deliberately left alone. Without this case, also building a section address from
  // __manage (giving `/w/__manage/runs`) would quietly land on the fallback route, showing up as
  // "clicked and went back to the workspace list": **looks the same** as the correct behavior, just
  // with one extra hop.
  await app.goto("/w/growth/runs");

  // Reuse switchTo's narrow-screen handling, only picking the "Manage" item instead of a workspace
  await switchTo(app, "管理所有空间");

  await expect(app).toHaveURL(/\/workspaces$/);
});
