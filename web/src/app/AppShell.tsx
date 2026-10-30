/**
 * App shell: fixed sidebar on the left + content area on the right.
 *
 * The sidebar and content area are flex siblings, not nested (a design rule). The sidebar fills
 * the viewport height with a thin line on its right edge, and has the same background as the
 * page. Navigation is split into three groups, "Build", "Observe" and "Manage", separated by
 * spacing + small section labels, not dividers.
 *
 * At the top of the sidebar is one row made of **logo + current workspace name**; the whole row
 * is the trigger of the workspace switcher (see the WorkspaceSwitcher comments), followed by
 * "Overview". **The bottom-left only holds the personal info row.** Both places show just a
 * name; the workspace key does not appear here (see workspace settings for it).
 *
 * **Switching workspace keeps you in the current section** (`/w/A/runs` → `/w/B/runs`); detail
 * pages fall back to the list level. The rules and reasons are all on `switchWorkspacePath`.
 *
 * **"Workspace settings" is a top-level nav item** in the "Manage" group. It used to be tucked
 * into the account menu at the bottom-left, on the grounds that "configuration is not a
 * destination"; but in practice it is exactly a frequent destination. Hidden behind the avatar,
 * it was hard to find, and the sidebar could not show "I am in settings now". So the account
 * menu only keeps personal things (personal info, preferences, log out), and everything
 * workspace-level is back in the navigation. **Members is a tab in the settings page, not in
 * the sidebar**; see MANAGE_NAV.
 */

import { Avatar, Dropdown, Header, Button, Tooltip } from "@heroui/react";
import { Icon } from "@iconify/react";
import { NavLink, Outlet, useLocation, useNavigate } from "react-router-dom";
import { Suspense, useEffect, useState, type ReactNode } from "react";
import { BrandMark } from "@/components/BrandMark";
import { AppShellScope, ListSkeleton } from "@/components/Page";
import { api } from "@/data/api";
import { useWorkspace } from "@/app/WorkspaceGate";
import { preferencesPath } from "@/app/preferencesLink";
import { useAuth } from "@/app/AuthGate";
import { useVisualViewport } from "@/lib/useVisualViewport";
import { useT } from "@/prefs/PrefsContext";
import type { MessageKey } from "@/prefs/messages";
import type { Workspace } from "@/types/domain";

interface NavItem {
  to: string;
  /** Translation key, turned into the current language via t() at render time. */
  labelKey: MessageKey;
  icon: string;
  /** Leaf routes use exact matching, so /agents is not highlighted as selected when on /agents/new. */
  end?: boolean;
  /**
   * Site-level address: **does not prepend `base`, and does not take part in the "current
   * workspace" prefix matching**.
   *
   * Only global settings uses it. Its route is `/preferences` (not under `/w/:key`), and the
   * source workspace is kept in `?from=`, so the address is computed by `preferencesPath()`. See
   * the ACCOUNT_NAV comment below, which records how this item once kicked users out of their
   * workspace.
   */
  absolutePath?: (workspaceKey: string) => string;
}

const BUILD_NAV: NavItem[] = [
  { to: "/agents", labelKey: "nav.agents", icon: "gravity-ui:face-robot" },
  // Chat belongs to "Build", not "Observe": it is a way to drive an agent, not an after-the-fact review.
  { to: "/chat", labelKey: "nav.chat", icon: "gravity-ui:comments" },
  { to: "/skills", labelKey: "nav.skills", icon: "gravity-ui:puzzle" },
  // Schedules belong to "Build", not "Observe": they are a capability the user configures;
  // the "Observe" group holds produced data (run history, usage).
  { to: "/schedules", labelKey: "nav.schedules", icon: "gravity-ui:clock" },
  { to: "/feishu", labelKey: "nav.feishu", icon: "gravity-ui:comment" },
  // Issues go at the end of "Build": they are collaboration output in the workspace, like agents / skills they answer "what does this workspace have".
  { to: "/issues", labelKey: "nav.issues", icon: "gravity-ui:bug" },
];

const OBSERVE_NAV: NavItem[] = [
  { to: "/runs", labelKey: "nav.runs", icon: "gravity-ui:list-ul" },
  { to: "/usage", labelKey: "nav.usage", icon: "gravity-ui:chart-line" },
];

/**
 * The "Manage" group: workspace-level configuration, with only one item, "Workspace settings".
 *
 * It used to sit in the account menu at the bottom-left. It moved back to the navigation because
 * it is a **frequent destination**, not an occasional action: hidden behind the avatar it is hard
 * to find, and the sidebar cannot show "I am in settings now". The account menu now only holds
 * personal things.
 *
 * **Members is not here; it is a tab in the settings page** (`/settings?tab=members`). Members
 * was once a top-level nav item in this group too, but then it had two entries (one in the
 * sidebar, one in the tab bar), and the sidebar highlight would jump between two rows after
 * clicking in. Since only one could stay, it stayed in the settings page: it is part of "what
 * this workspace is".
 *
 * So this item **does not set `end`**: there are no further path segments under `/settings`, all
 * tabs live on `?tab=` (which does not affect pathname), so prefix and exact matching are the
 * same. The old `end` was there to stop `/settings/members` from also lighting up this row; that
 * guard became moot when members moved back to a tab.
 */
const MANAGE_NAV: NavItem[] = [
  { to: "/settings", labelKey: "nav.workspaceSettings", icon: "gravity-ui:gear" },
];

/**
 * The "Account" group: configuration that belongs only to you and is the same in every
 * workspace.
 *
 * # Why it is its own group, instead of going into "Manage"
 *
 * The "Manage" group holds **workspace-level configuration** (changes affect everyone in the
 * workspace). Global settings are account-level (changes only affect you). Putting both in the
 * same group, with similar icons and both named "xx settings", is exactly the trap noted in the
 * workspace settings page header: **adjacent + look alike + opposite meaning**.
 *
 * Putting it in "Manage" with a "Personal" Chip would also work, but that is **using a marker to
 * make up for a wrong grouping**. The group title is exactly where "what scope is this area" is
 * answered; letting it say so directly is simpler, and does not require users to notice a small
 * Chip.
 *
 * The cost is one more single-item group at the bottom of the sidebar. That cost is known and
 * accepted: getting the scope wrong (the user thinks they are changing this workspace but are
 * changing all their workspaces, or the reverse) costs more than one extra title line.
 *
 * # ⚠️ This item must carry the current workspace, or users get kicked out of it
 *
 * Global settings has **one** route, `/preferences` (not under `/w/:key`), and whether it shows
 * the sidebar is decided by `?from=<key>` (see `app/PreferencesRoute.tsx`). So this item builds
 * `preferencesPath({ workspaceKey })`: a user clicking it inside a workspace should enter with
 * that workspace: the sidebar stays, the current workspace is highlighted, and Back returns to it.
 *
 * **This broke twice, in opposite directions, with the same symptom**: "one click kicks you out
 * of the workspace you were working in":
 *
 *   1. The earliest version used `absolute: true`, pointing straight at the shell-less
 *      `/preferences`: the whole sidebar disappeared, workspace settings on the left were gone,
 *      and Back sent people to the workspace list.
 *   2. It was later changed to prepend `base` (`/w/:key/preferences`) with a mount point inside
 *      the workspace. That worked, but two mount points meant every link had to decide on its
 *      own "am I in a workspace", built separately in four places.
 *
 * Now it is one route + one query parameter: **the address is only produced by
 * `preferencesPath()`**, and none of the four callers decide on their own. Forgetting `?from=`
 * still means "the sidebar disappears", so e2e 07 has a case watching it (mutation-tested: remove
 * workspaceKey and that case fails at once).
 */
const ACCOUNT_NAV: NavItem[] = [
  {
    to: "/preferences",
    labelKey: "nav.globalSettings",
    icon: "gravity-ui:person-gear",
    absolutePath: (key) => preferencesPath({ workspaceKey: key }),
  },
];

/**
 * Whitelist of sections that "can land as-is after switching workspace", **computed from the
 * sidebar groups**, not a second hand-written list.
 *
 * Sidebar items naturally look the way this whitelist should: each is a **list-level** address
 * (`/runs`, `/skills` …), and the list level exists in every workspace. It holds no resource id
 * of this workspace, so moving to another workspace just shows that workspace's list (maybe
 * empty, which is what the empty state is for), never a 404 or 403.
 *
 * The item with `absolutePath` (global settings) must be excluded: its route is not under
 * `/w/:key` at all, and building `/w/<key>/preferences` would just hit the compatibility redirect.
 *
 * **A second hand-written list is the only real trap here**: missing an item only means "the
 * section is not kept after switching" (minor), but an extra item, or one wrong letter, becomes
 * an entry where **switching workspace lands on the fallback route**. Computing it means adding
 * or removing sidebar items is followed automatically, with no second place to remember.
 */
const PORTABLE_SECTIONS = new Set(
  [...BUILD_NAV, ...OBSERVE_NAV, ...MANAGE_NAV]
    .filter((it) => !it.absolutePath)
    .map((it) => it.to.replace(/^\//, "")),
);

/**
 * Where to go when switching workspace: **keep the current section, but only down to the list
 * level**.
 *
 *   /w/A/runs            → /w/B/runs         section carried over
 *   /w/A/runs/sesn-xxx    → /w/B/runs         **falls back to the list**, old id dropped
 *   /w/A                 → /w/B              already at the root
 *   /preferences?from=A   → /preferences?from=B  changes the shell's source, stays on this page
 *
 * # ⚠️ Why a whitelist, not "remove the segment with the id"
 *
 * On today's route table both give the same result, but **they fail in opposite directions when
 * something is not configured**, and that is the whole reason for choosing this:
 *
 *   whitelist (here)  missing entry → lands on the workspace root. Section not kept, **but the
 *                     address is always valid**.
 *   blacklist         missing entry → carries A's resource id into B's address → 404 / 403.
 *
 * And this repo explicitly says "a missing resource returns 404", so one missing blacklist entry
 * is an entry where **switching workspace shows an error page**, much worse than going to the
 * root. The whitelist makes that impossible by construction: any path not claimed goes to the
 * root, and a new route without config cannot hurt anyone.
 *
 * # ⚠️ No query parameters are carried over
 *
 * This is not laziness: **among the query parameters that would land, "filter" and "resource id"
 * are the same key**. At the list level there are only `/runs?agent=` and `/chat?agent=`
 * (`/settings?tab=` is pure UI state), and the value of `agent` is an agent id from workspace A.
 * Carrying it over would make B filter by an id that does not exist, giving **an empty list plus
 * a blank filter bar**, with no error, and the user would think "workspace B has nothing".
 *
 * So "keep filters, drop ids" would require classifying each key, and if that classification
 * missed one, it would break in exactly that silent empty-list way. Carrying none is
 * **maintenance-free and always valid**: the landing page is always that section's default view.
 * The cost is known and accepted: switching from `?tab=members` lands on the settings page's
 * "Basic info".
 *
 * `/preferences` is the only exception, and it does not break the rule above: this page's content
 * is **account-level** (the same in any workspace), `from` is only the shell's source marker and
 * `tab` is a closed enum; neither points to any resource. The address is still built by
 * `preferencesPath()`, the only place that produces it (see preferencesLink.ts).
 */
export function switchWorkspacePath(
  pathname: string,
  search: string,
  targetKey: string,
): string {
  if (pathname === "/preferences") {
    const tab = new URLSearchParams(search).get("tab");
    return preferencesPath({ workspaceKey: targetKey, tab: tab ?? undefined });
  }
  // Take the first segment after `/w/<key>/`. If none (at the workspace root) or not in the whitelist → go to the root.
  const section = /^\/w\/[^/]+\/([^/]+)/.exec(pathname)?.[1] ?? "";
  return PORTABLE_SECTIONS.has(section) ? `/w/${targetKey}/${section}` : `/w/${targetKey}`;
}

function SideLink({ item, base }: { item: NavItem; base: string }) {
  const t = useT();
  /*
   * Site-level items (global settings) are computed via absolutePath; the rest prepend base.
   *
   * `base` looks like `/w/growth`; cut out the key and pass it to absolutePath. This item has to
   * put the current workspace in the query to get the shell.
   */
  const to = item.absolutePath ? item.absolutePath(base.replace(/^\/w\//, "")) : `${base}${item.to}`;
  return (
    <NavLink
      to={to}
      end={item.end}
      className={({ isActive }) =>
        [
          "flex h-9 items-center gap-2.5 r-row px-3 text-sm cursor-[var(--cursor-interactive)]",
          // Selected state uses the surface background + a light shadow; unselected only gets a neutral background on hover
          isActive
            ? "bg-surface shadow-surface font-medium text-foreground"
            : "text-muted hover:bg-surface-secondary hover:text-foreground",
        ].join(" ")
      }
    >
      <Icon icon={item.icon} className="size-4 shrink-0" />
      <span className="truncate">{t(item.labelKey)}</span>
    </NavLink>
  );
}

function NavGroup({
  labelKey,
  items,
  base,
}: {
  labelKey: MessageKey;
  items: NavItem[];
  base: string;
}) {
  const t = useT();
  return (
    <div className="flex flex-col gap-1">
      <p className="px-3 pb-1 text-xs text-muted">{t(labelKey)}</p>
      {items.map((it) => (
        <SideLink key={it.to} item={it} base={base} />
      ))}
    </div>
  );
}

/**
 * The top row of the sidebar: logo + current workspace name + dropdown arrow; the whole row is
 * the trigger of the workspace switcher.
 *
 * **This used to be "a non-clickable brand mark" (logo + AMA wordmark), deliberately not a
 * NavLink**, on the grounds that "it is an identity, not navigation; the destination is already
 * covered by 'Overview' right below, and two items pointing to the same address would mean two
 * highlights". That concern went away with this change: this row is **no longer a link to an
 * address**, but a trigger that opens the workspace menu. It is not the same destination as
 * "Overview", so there is no "is it selected" state to handle.
 *
 * **The AMA wordmark gives way to the workspace name**: the logo already carries the brand, and
 * the question this spot at the top of the sidebar really needs to answer is "which workspace am
 * I in now" (Slack / Linear / Notion all do this). With the workspace name moved up from the
 * bottom-left, the bottom-left only holds personal info.
 */
function WorkspaceSwitcher() {
  const t = useT();
  const { workspace } = useWorkspace();
  const navigate = useNavigate();
  // When switching we need to know "which section we are in now". Use useLocation instead of
  // reading window.location in onAction: under React Router the latter is not guaranteed to match
  // the committed frame, and this component re-renders with the route anyway, so subscribing is
  // free.
  const location = useLocation();
  const [all, setAll] = useState<Workspace[]>([]);

  // The catch is required: without it, one failure becomes an unhandled rejection, and the
  // switcher quietly lists zero workspaces; the user thinks all their workspaces are gone. When
  // listing fails, keep the empty list (the "Manage workspaces" entry below still works), and leave
  // the real cause in the console.
  useEffect(() => {
    api.listWorkspaces()
      .then(setAll)
      .catch((e: unknown) => console.error("列空间失败（空间切换器）", e));
  }, []);

  return (
    <Dropdown>
      {/*
        Dropdown.Trigger itself renders as a <button> (it inherits react-aria's Button). Nesting a
        <Button> inside would give a nested button, and React would report a DOM validation
        error. So style the Trigger directly, without an extra button.

        Height matches h-9 and horizontal padding px-3, on the same vertical line as each nav item
        below. The mark is size-7 rather than the landing page's size-9: the sidebar is only 240px
        wide with 36px rows, and size-9 would overpower every nav item.
      */}
      <Dropdown.Trigger
        className="flex h-9 w-full items-center gap-2 r-row px-3 text-left cursor-[var(--cursor-interactive)] hover:bg-surface-secondary"
        aria-label={t("nav.switchWorkspace")}
        data-testid="workspace-switcher"
      >
        <BrandMark className="size-7" />
        {/* Only the workspace name, no key; see workspace settings for the key. min-w-0 is what
            makes truncate work: names can be long, and without truncate they push the arrow out or wrap to two lines. */}
        <span className="min-w-0 flex-1 truncate text-sm font-medium">{workspace.name}</span>
        {/* The dropdown arrow tells users this row can be expanded, not a static label for the current location */}
        <Icon icon="gravity-ui:chevron-down" className="size-3.5 shrink-0 text-muted" />
      </Dropdown.Trigger>
      {/* It sits against the top edge of the viewport, so the menu must open downward. It used to
          be at the bottom-left, hence top start; copying that here would open the menu off-screen. */}
      <Dropdown.Popover placement="bottom start" className="min-w-56">
        <Dropdown.Menu
          onAction={(key) => {
            if (key === "__manage") navigate("/workspaces");
            // Switching workspace **stays in the current section** (`/w/A/runs` → `/w/B/runs`);
            // detail pages with an id fall back to the list level. The rules are all in
            // switchWorkspacePath, which records why it is a whitelist and why no query is carried.
            else navigate(switchWorkspacePath(location.pathname, location.search, String(key)));
          }}
        >
          <Dropdown.Section>
            {all.map((w) => (
              <Dropdown.Item key={w.key} id={w.key} textValue={w.name}>
                <span className="flex flex-col">
                  <span className="text-sm">{w.name}</span>
                  <span className="text-xs text-muted">{w.key}</span>
                </span>
              </Dropdown.Item>
            ))}
          </Dropdown.Section>
          <Dropdown.Section>
            <Dropdown.Item id="__manage" textValue={t("nav.manageWorkspaces")}>
              {t("nav.manageWorkspaces")}
            </Dropdown.Item>
          </Dropdown.Section>
        </Dropdown.Menu>
      </Dropdown.Popover>
    </Dropdown>
  );
}

function CurrentUser() {
  const t = useT();
  // **The real logged-in user**, from AuthGate's session.
  //
  // This used to be `members.find(m => m.role === "owner")`: back when there was no login
  // state, that was the only way to guess "who am I", so everyone saw the workspace owner's
  // name. Now there is a real identity.
  const { user, logout } = useAuth();

  const displayName = user.name || user.email;

  return (
    <Dropdown>
      <Dropdown.Trigger
        className="flex h-9 w-full items-center gap-2 r-row px-3 text-left cursor-[var(--cursor-interactive)] hover:bg-surface-secondary"
        aria-label={t("nav.accountMenu")}
      >
        {/*
          The real avatar comes from the picture field of the SSO userinfo (backend
          auth.Identity.AvatarURL).

          **None of the three "no image" cases need handling here**: Radix only renders the <img>
          after the image has loaded, and the Fallback stays in place before that and after a
          load failure. So "SSO gave no avatar", "still loading" and "URL is broken" all land on
          the initial, with no need for conditional rendering like user.avatarUrl && ...; writing
          that would actually leave an empty circle when loading fails.

          The layout does not jump either: the size is pinned on the outer .avatar's size-6, and
          the image is absolute inset-0 (built into HeroUI's .avatar__image), so it never takes
          part in layout. Fallback deliberately has no delayMs: that is meant to prevent flicker
          when "the image arrives quickly", but here having no avatar is normal (API Key machine
          identities never have one), so a delay would only show a blank slot first.

          alt is an empty string: the name is right next to it, and having screen readers read the
          avatar too would be a repeat. This is the standard pattern for "decorative images", not
          a missing value. That is why e2e finds it by testid.
        */}
        <Avatar className="size-6 shrink-0">
          <Avatar.Image src={user.avatarUrl} alt="" data-testid="current-user-avatar" />
          <Avatar.Fallback className="text-xs">{displayName.slice(0, 1)}</Avatar.Fallback>
        </Avatar>
        {/* Only the user name, no email; the full identity is shown in the expanded menu */}
        <span className="min-w-0 flex-1 truncate text-sm" data-testid="current-user">
          {displayName}
        </span>
      </Dropdown.Trigger>
      {/* It sits at the very bottom of the sidebar, so it opens upward (the workspace switcher at the top opens downward) */}
      <Dropdown.Popover placement="top start" className="min-w-56">
        <Dropdown.Menu
          onAction={(key) => {
            if (key === "logout") void logout();
          }}
        >
          {/*
            Personal info: the email is the user's unique ID (Actor). The trigger row only fits
            the name, so the full identity is shown here. The Header is not clickable: it is
            information, not a destination.
          */}
          <Dropdown.Section>
            <Header className="flex min-w-0 items-center gap-2.5 px-2 py-1.5">
              {/* size-8 in the menu: this is where you "confirm who I am", worth one size larger than the trigger row */}
              <Avatar className="size-8 shrink-0">
                <Avatar.Image src={user.avatarUrl} alt="" />
                <Avatar.Fallback className="text-xs">{displayName.slice(0, 1)}</Avatar.Fallback>
              </Avatar>
              <span className="flex min-w-0 flex-col">
                <span className="truncate text-sm font-medium">{displayName}</span>
                <span className="truncate text-xs text-muted" data-testid="account-email">
                  {user.email}
                </span>
              </span>
            </Header>
          </Dropdown.Section>
          {/*
            Only "Log out" is left here.

            "Global settings" used to be here; it is now a top-level nav item in the sidebar's
            "Account" group. The reason is **word for word the same** as when "Workspace
            settings" moved out of here: it is a frequent destination, not an occasional action;
            hidden behind the avatar it is hard to find, and once inside you cannot tell "I am in
            settings now".

            **If it moves, delete it here; do not keep both**: two entries for one destination
            make users think they are different things (that is why members went back from a
            top-level nav item to a tab).

            "Switch workspace" is not here either: it is the top row of the sidebar (logo +
            workspace name).
          */}
          <Dropdown.Section>
            {/* Log out: the account menu is the only place it makes sense. */}
            <Dropdown.Item id="logout" textValue={t("login.logout")}>
              {t("login.logout")}
            </Dropdown.Item>
          </Dropdown.Section>
        </Dropdown.Menu>
      </Dropdown.Popover>
    </Dropdown>
  );
}

/**
 * The shell.
 *
 * Either `children` or `<Outlet />`, **not both**:
 *
 *   Most pages use Outlet: they are child routes under `/w/:key`, filled in by the route table.
 *   Global settings uses children: its route is `/preferences` (not under `/w/:key`), and
 *   PreferencesRoute explicitly puts the page in (see the comments in that file).
 *
 * Rendering both would, on the global settings path, make Outlet resolve to null while the page
 * renders the parent/child structure twice; `??` makes it explicit: "if there are children,
 * ignore Outlet".
 */
export function AppShell({ children }: { children?: ReactNode } = {}) {
  const t = useT();
  const { workspace } = useWorkspace();
  const base = `/w/${workspace.key}`;
  const [mobileOpen, setMobileOpen] = useState(false);
  const location = useLocation();

  /*
    **Close the narrow-screen drawer automatically after navigation.**

    The drawer used to have only two toggles: the hamburger button opens it, the overlay closes
    it. **Nav items did not close it**, so after a narrow-screen user tapped "Run history", the
    drawer still covered the page they had just switched to, and they had to tap the overlay
    again to see it. That is not how mobile works (a drawer closing once you pick something is
    the default expectation for this kind of interaction).

    # Why listen to the route, instead of adding onClick to every nav item

    Adding `onClick={() => setMobileOpen(false)}` to each one is the most obvious way, but it is
    **entry-driven**: the drawer now has 12 nav items, breadcrumbs, the workspace switcher's jumps,
    and the account menu. Every new entry needs one more line, **any one that is missed will not
    close**, and missing it gives no error; the next person adding a nav item would never know
    about this rule. This is **result-driven** instead: however you navigate (nav item,
    breadcrumbs, workspace switcher, programmatic navigate, browser back/forward), the drawer
    closes as soon as the location changes.
    **Adding a nav item costs nothing; there is nothing to remember.**

    # ⚠️ It depends on location.key, not pathname

    `pathname` **does not change** when "the item you tapped is the current page", so the drawer
    would not close, and that is exactly the most common case (the user opens the drawer on the
    run history page and taps "Run history" again). `key` is new on every navigation:
    react-router turns a Link to the same path into a `replace` automatically
    (`createPath(location) === createPath(path)` in `useLinkClickHandler`), and `replace` also
    goes through `createLocation()`, where `key` is freshly generated by `createKey()`.
    So checking key covers same-page navigation; checking pathname misses it.

    No side effects on desktop: `mobileOpen` is always false there, setting it to false is a
    no-op, and this **only touches the mobileOpen state**, not resetting anything desktop-related
    along the way.
  */
  useEffect(() => {
    setMobileOpen(false);
  }, [location.key]);

  /*
    Sync the soft keyboard height to `--kb`. It is mounted here because AppShell is the only
    shell for the whole site: the keyboard is device-level state, listening once is enough, and
    having every page listen would just set the same variable repeatedly.
    On browsers without visualViewport it does nothing; see that hook's comments.
  */
  useVisualViewport();

  /*
    `--safe-left` is applied on the sidebar itself (not the outer drawer/container): it has
    `bg-background`, so the padding lets the background reach the physical edge while only the
    text moves right. Applied on the outer layer, the background would stop at the outer edge of
    the safe area, leaving a strip of base color on the left.

    ⚠️ This is **needed on desktop too**, not "mobile only": iPhone landscape is 874px, already
    past the `md` (768px) breakpoint, so what shows then is this directly mounted desktop sidebar,
    whose left edge is the physical screen edge, right where the notch is. In portrait and on
    devices without a notch the variable is 0px, so nothing changes.
  */
  const sidebar = (
    <div
      className="flex h-full w-60 shrink-0 flex-col gap-6 border-r border-border bg-background px-3 py-4"
      style={{ paddingLeft: "calc(0.75rem + var(--safe-left))" }}
    >
      {/*
        Top of the sidebar: logo + current workspace name; the whole row is the workspace switcher
        (see the WorkspaceSwitcher comments).

        This spot used to be a non-clickable brand mark (logo + AMA wordmark). It became the
        switcher because what the top of the sidebar really needs to answer is "which workspace
        am I in"; the logo alone is enough for brand recognition. gap-6 separates it from the
        navigation, the same spacing as between the three nav groups.
      */}
      <WorkspaceSwitcher />

      <nav className="flex flex-1 flex-col gap-6 overflow-y-auto">
        <div className="flex flex-col gap-1">
          <SideLink
            item={{ to: "", labelKey: "nav.overview", icon: "gravity-ui:chart-column", end: true }}
            base={base}
          />
        </div>
        <NavGroup labelKey="nav.groupBuild" items={BUILD_NAV} base={base} />
        <NavGroup labelKey="nav.groupObserve" items={OBSERVE_NAV} base={base} />
        <NavGroup labelKey="nav.groupManage" items={MANAGE_NAV} base={base} />
        <NavGroup labelKey="nav.groupAccount" items={ACCOUNT_NAV} base={base} />
      </nav>

      {/*
        The bottom-left only holds personal info. The workspace switcher has moved to the very top
        of the sidebar, which answers both "what product is this" and "which workspace am I in",
        so the bottom-left only answers "who am I"; the two identity dimensions each take one end.
        It is not inside the nav landmark: it is a menu for "switching identity context", not page
        navigation. The account menu now only has "Log out"; global settings moved to the
        "Account" group above, and workspace-level items are in the "Manage" group.
      */}
      <CurrentUser />
    </div>
  );

  return (
    /*
      The shell has a fixed height and does not scroll itself (`h-dvh overflow-hidden`); scrolling
      only happens in the `<main>` below. This is the whole secret of the "app feel": the page
      level has no room to scroll, so the top bar cannot be scrolled away and there is no
      whole-page rubber band. For why dvh and not vh, see the html/body/#root section in
      index.css.

      ⚠️ **`overflow-hidden` is defense in depth, and e2e cannot see it**: remove it and the three
      mobile cases **all stay green**, because `#root { height: 100dvh }` in index.css already
      pins the shell's height, so it cannot overflow without it. It stays because it guards
      against "someone changing #root's height in the future"; then this is the last barrier.
      Do not assume it is useless just because "deleting it does not fail any test".
    */
    <AppShellScope>
      <div className="flex h-dvh overflow-hidden">
      {/* Fixed sidebar on desktop */}
      <div className="hidden md:flex h-full">{sidebar}</div>

      {/*
        Narrow screens use a drawer, opened by a hamburger button.

        The safe-area padding is on the drawer itself, not on the sidebar inside: the drawer is
        `fixed inset-0`, its top and bottom edges are the physical screen edges, and without the
        safe area the top nav items would be covered by the Dynamic Island and the personal info
        row at the bottom by the Home Indicator.

        **Left and right are not handled here**: what touches the left physical edge is the
        sidebar inside, which has `bg-background`. Applying it on the drawer would make the
        background stop at the outer edge of the safe area, leaving a strip of base color on the
        left that looks like the sidebar does not fill; applying it on the sidebar lets the
        background reach the edge while only the text moves right.
        So `--safe-left` is in the sidebar's `pl` (see the sidebar section above), and both uses,
        mounted directly on desktop and inside the drawer, **share one place**, never doubled.
      */}
      {mobileOpen ? (
        <div
          className="fixed inset-0 z-50 flex md:hidden"
          style={{
            paddingTop: "var(--safe-top)",
            paddingBottom: "var(--safe-bottom)",
          }}
          data-testid="mobile-nav-drawer"
        >
          <div className="h-full">{sidebar}</div>
          <button
            type="button"
            aria-label={t("nav.closeNav")}
            className="flex-1 bg-overlay/60"
            onClick={() => setMobileOpen(false)}
          />
        </div>
      ) : null}

      <div className="flex min-w-0 flex-1 flex-col">
        {/*
          Narrow-screen top bar. **It is outside the scroll container**; that is the point of
          this change.

          It used to live inside `<main>`, so it scrolled away with the content, which on narrow
          screens meant no fixed top bar (and so no stable navigation entry). Moved to be a
          sibling of main, main owns the scrolling and the top bar always stays.

          `--safe-top` lets the background extend under the Dynamic Island without the text being
          covered: in index.html, `apple-mobile-web-app-status-bar-style=black-translucent` makes
          content extend under the status bar, so this padding is required, not decoration.

          The bottom border replaces the old `<Separator className="md:hidden" />` (drawing the
          same line twice takes an extra 1px in the dvh layout and makes the top bar height
          inaccurate).
        */}
        <header
          className="flex shrink-0 items-center gap-2 border-b border-border bg-background px-4 pb-3 md:hidden"
          style={{
            paddingTop: "calc(var(--safe-top) + 0.75rem)",
            paddingLeft: "calc(1rem + var(--safe-left))",
            paddingRight: "calc(1rem + var(--safe-right))",
          }}
          data-testid="mobile-topbar"
        >
          <Tooltip>
            <Tooltip.Trigger>
              <Button
                variant="ghost"
                isIconOnly
                aria-label={t("nav.openNav")}
                onPress={() => setMobileOpen(true)}
                data-testid="mobile-nav-open"
              >
                <Icon icon="gravity-ui:bars" className="size-5" />
              </Button>
            </Tooltip.Trigger>
            <Tooltip.Content>{t("nav.openNav")}</Tooltip.Content>
          </Tooltip>
          <span className="truncate text-sm font-medium">{workspace.name}</span>
        </header>

        {/*
          The only scroll area. `overscroll-contain` stops "keep dragging past the bottom" from
          pulling the whole page (iOS rubber band); supported in Safari 16+.
        */}
        <main className="flex-1 overflow-y-auto overscroll-contain" data-testid="app-scroll">
          {/*
            Suspense is the fallback for the lazily loaded /usage route (see router.tsx). Without
            it, switching to that route makes React throw "A component suspended while responding
            to synchronous input" and replaces the whole subtree with blank. The fallback uses the
            same skeleton as the other pages, not null; null on a slow network looks like
            "clicked the nav and nothing happened".
          */}
          <Suspense fallback={<ListSkeleton rows={3} />}>{children ?? <Outlet />}</Suspense>
        </main>
      </div>
      </div>
    </AppShellScope>
  );
}
