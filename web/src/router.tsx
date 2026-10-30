import { createBrowserRouter, Navigate, useParams, useSearchParams } from "react-router-dom";
import { lazy } from "react";
import { WorkspaceGate } from "@/app/WorkspaceGate";
import { AppShell } from "@/app/AppShell";
import { PreferencesRoute } from "@/app/PreferencesRoute";
import { preferencesPath } from "@/app/preferencesLink";
import { AUTO_OPEN_STATE, loadLastWorkspaceKey } from "@/lib/lastWorkspace";
import { WorkspacesPage } from "@/pages/WorkspacesPage";
import { OverviewPage } from "@/pages/OverviewPage";
import { AgentsPage, NewAgentPage } from "@/pages/AgentsPage";
import { AgentDetailPage } from "@/pages/AgentDetailPage";
import { ChatPage } from "@/pages/ChatPage";
import { RunsPage } from "@/pages/RunsPage";
import { SchedulesPage } from "@/pages/SchedulesPage";
import { ScheduleDetailPage } from "@/pages/ScheduleDetailPage";
import { RunDetailPage } from "@/pages/RunDetailPage";
import { SkillsPage } from "@/pages/SkillsPage";
import { SkillDetailPage } from "@/pages/SkillDetailPage";
import { IssuesPage } from "@/pages/IssuesPage";
import { IssueDetailPage } from "@/pages/IssueDetailPage";
import { FeishuPage } from "@/pages/FeishuPage";
import { FeishuConnectPage } from "@/pages/FeishuConnectPage";
import { WeixinPage } from "@/pages/WeixinPage";
import { SettingsPage } from "@/pages/SettingsPage";

/*
 * The usage page is the only lazy-loaded route in the whole site.
 *
 * It is the only page that uses charts, and HeroUI charts are built on recharts (about 100 KB gzip).
 * A static import would pull it into the main chunk, making every page pay for one page's
 * dependency; with React.lazy, recharts only lands in the usage chunk.
 *
 * The matching <Suspense> is outside AppShell's <Outlet />: without it, switching to this route
 * makes React throw "A component suspended while responding to synchronous input".
 */
const UsagePage = lazy(() =>
  import("@/pages/UsagePage").then((m) => ({ default: m.UsagePage })),
);

/**
 * Old address `/settings/members` -> `/settings?tab=members`.
 *
 * Members once had their own page on this path segment (side by side with "Workspace settings" in
 * the sidebar "Manage" group), and are now back as a tab of the settings page. This redirect stays
 * because that address **really was used out there**: docs, bookmarks and existing e2e tests all
 * pointed to it, and deleting it would send them all to the fallback route, showing up as "clicking
 * a bookmark goes back to the workspace list". Users would not think the address changed; they
 * would think things were lost.
 *
 * `replace` is required: without replacing this history entry, pressing the browser's back button
 * from the settings page goes to the old address, which redirects back again, and the back button
 * stops working.
 */
function MembersRedirect() {
  const { workspaceKey } = useParams<{ workspaceKey: string }>();
  return <Navigate to={`/w/${workspaceKey}/settings?tab=members`} replace />;
}

/**
 * Old address `/w/:key/preferences` -> `/preferences?from=:key`.
 *
 * Global settings were once mounted at two points, and the one inside a workspace was this address.
 * After merging into a single route it must still be caught, for exactly the same reason as
 * MembersRedirect: **that address really was used out there**. Otherwise it lands on the fallback
 * route and jumps back to `/workspaces`, showing up as "clicking a bookmark kicks me out of the
 * workspace I was working in", which is exactly what two rounds of changes in this area set out to
 * remove.
 *
 * `tab` must be **carried over as-is** (the `?tab=ark` links point to this address), or a user who
 * clicks "go to global settings" lands on the default appearance page while looking for the Ark
 * credentials. `replace` is the same as MembersRedirect: without replacing this history entry, the
 * browser's back button goes to the old address, which redirects back again, and the back button
 * stops working.
 */
function PreferencesRedirect() {
  const { workspaceKey } = useParams<{ workspaceKey: string }>();
  const [params] = useSearchParams();
  return (
    <Navigate
      to={preferencesPath({ workspaceKey, tab: params.get("tab") ?? undefined })}
      replace
    />
  );
}

/**
 * Root path `/` -> the last visited workspace, or the workspace list if there is no record.
 *
 * # Why it is a component, instead of keeping a hard-coded `<Navigate to="/workspaces">`
 *
 * The destination is computed on the spot, and its input (localStorage) is **synchronously
 * readable**; that is the pivot of the whole design: the component decides where to go during
 * render, so `WorkspacesPage` **is never mounted even once**, and there is no flash of "see the list,
 * then get bounced away". So do not change it to "render the list page first, then jump away in an
 * effect", and do not await an API call here to confirm the workspace still exists: that would make
 * the first screen wait on a network round trip, and `/` is the first address everyone hits every day.
 *
 * **Validity is not checked here**; it is left to the existing `WorkspaceGate` on `/w/:key`: it
 * already loads the workspace by key, already has the "opening workspace" loading state, and
 * already tells "does not exist" apart from "no access". Checking again here would do the same
 * thing twice, with one more rule that can drift.
 * When the jump fails, the gate forgets this memory and goes back to the list page (see the notes
 * in WorkspaceGate).
 *
 * `AUTO_OPEN_STATE` is a where-from marker for the gate: when a workspace cannot be opened, "typed
 * in by the user" should explain why, while "auto-jumped here" should quietly go back to the list
 * page. See lib/lastWorkspace.ts for the reasoning.
 *
 * `replace` is required: if the `/` history entry stays, a user pressing the browser's back button
 * inside a workspace goes back to `/`, gets sent to the same workspace again, and the back button
 * stops working (same as MembersRedirect).
 */
function RootRoute() {
  const lastKey = loadLastWorkspaceKey();
  if (!lastKey) return <Navigate to="/workspaces" replace />;
  return <Navigate to={`/w/${lastKey}`} replace state={AUTO_OPEN_STATE} />;
}

/**
 * Route table.
 *
 * Except for the /workspaces start page and the /preferences global settings, all business routes
 * live under /w/:workspaceKey: this is where the rule "every action must happen inside a workspace"
 * lands at the routing layer. WorkspaceGate validates the key.
 *
 * # `/` automatically enters the last visited workspace; `/workspaces` is always the list page
 *
 * The two addresses **deliberately lead to different places**: `/` means "take me back to work",
 * `/workspaces` means "let me pick one". If the latter also auto-jumped, users could never get back
 * to the list page: the "Manage all workspaces" entry in the sidebar workspace switcher points to
 * it, and that is the only way back from inside a workspace to the list.
 *
 * # Global settings have only **one** mount point; the source workspace is kept in the query
 *
 *   /preferences               no shell    <- entered from the workspace list, or not in any workspace yet
 *   /preferences?from=<key>    with shell  <- entered from the sidebar "Account" group (sidebar stays, back returns to that workspace)
 *
 * Its **content** is account-level (the same whichever workspace you view it from); only the
 * **navigation context** has two kinds. The logic deciding whether to show the shell lives in one
 * place, `app/PreferencesRoute.tsx`, which also records the two times "one click kicked me out of
 * the workspace" went wrong, and why an invalid key degrades instead of erroring.
 *
 * **This used to be mounted twice** (`/preferences` + `/w/:key/preferences`). That worked, but
 * required every link pointing to it to first check "am I inside a workspace right now": the
 * sidebar, two jumps in SettingsPage, and the `?tab=apikeys` redirect each built it separately in
 * four places, and missing one meant the sidebar disappeared once. Now all four use
 * `preferencesPath()`.
 */
export const router = createBrowserRouter([
  { path: "/", element: <RootRoute /> },
  { path: "/workspaces", element: <WorkspacesPage /> },
  { path: "/preferences", element: <PreferencesRoute /> },
  {
    path: "/w/:workspaceKey",
    element: <WorkspaceGate />,
    children: [
      {
        element: <AppShell />,
        children: [
          { index: true, element: <OverviewPage /> },
          { path: "agents", element: <AgentsPage /> },
          { path: "agents/new", element: <NewAgentPage /> },
          { path: "agents/:agentId", element: <AgentDetailPage /> },
          // Chat: runId goes in the URL, so a refresh can continue and links can be shared. No runId means "new chat".
          { path: "chat", element: <ChatPage /> },
          { path: "chat/:runId", element: <ChatPage /> },
          { path: "schedules", element: <SchedulesPage /> },
          // Schedule detail: view + edit + run history. **Editing is no longer a dialog**: a prompt can be
          // thousands of characters (real ones in production are), which cannot really be edited in a
          // dialog. `?tab=` works like the agent detail page: links can be shared and survive a refresh.
          { path: "schedules/:scheduleId", element: <ScheduleDetailPage /> },
          { path: "runs", element: <RunsPage /> },
          { path: "runs/:runId", element: <RunDetailPage /> },
          { path: "usage", element: <UsagePage /> },
          { path: "skills", element: <SkillsPage /> },
          // Skill detail: view all files inside the skill (SKILL.md by default). id goes in the URL; links can be shared.
          { path: "skills/:skillId", element: <SkillDetailPage /> },
          // Issues: list + detail. The detail id goes in the URL; links can be shared and survive a refresh.
          { path: "issues", element: <IssuesPage /> },
          { path: "issues/:issueId", element: <IssueDetailPage /> },
          { path: "feishu", element: <FeishuPage /> },
          { path: "feishu/connect", element: <FeishuConnectPage /> },
          { path: "weixin", element: <WeixinPage /> },
          { path: "settings", element: <SettingsPage /> },
          // Members are back as a tab of the settings page; this path segment only keeps a redirect (see MembersRedirect)
          { path: "settings/members", element: <MembersRedirect /> },
          // Old address `/w/:key/preferences` -> `/preferences?from=:key`.
          //
          // This address **really was used out there** (a version with it was released, and docs, bookmarks
          // and existing e2e tests all pointed to it), so it cannot just be deleted: without it, it would
          // land on the fallback route and jump back to the workspace list, showing up as "clicking a bookmark
          // kicked me out of the workspace", exactly what this area keeps trying to avoid.
          { path: "preferences", element: <PreferencesRedirect /> },
        ],
      },
    ],
  },
  // Fallback: unknown paths go back to the start page instead of showing a blank page
  { path: "*", element: <Navigate to="/workspaces" replace /> },
]);
