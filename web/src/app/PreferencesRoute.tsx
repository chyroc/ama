/**
 * **Shell wiring** for the global settings page: one route, and the query decides whether it
 * gets AppShell.
 *
 * # Why one route + query, not two mount points
 *
 * The **content** of this page is account-level (it is the same no matter which workspace you
 * open it from); only the **navigation context** comes in two kinds. The earlier solution mounted
 * the same component twice:
 *
 *   /preferences           no shell
 *   /w/:key/preferences    with shell
 *
 * That worked, but the complexity landed on **every link pointing to it**: the sidebar item,
 * the two "go to global settings" links in SettingsPage, and the `?tab=apikeys` redirect. All
 * four had to first ask "am I inside a workspace right now" before choosing a URL. Missing one
 * produced the incident below, with no error.
 *
 * One route moves that decision to **one place** (this component): the source workspace is a
 * parameter in the URL; with it you get the shell, without it you do not. All four links go
 * through `preferencesPath()` and no longer decide on their own.
 *
 * For why the parameter is `from` and why it uses the key instead of the id, see
 * `preferencesLink.ts`.
 *
 * # ⚠️ "With shell" must really render the shell; that is where the incident happened
 *
 * Earlier, the sidebar "Global settings" pointed to the shell-less `/preferences`. One click and
 * **the whole sidebar disappeared, the workspace settings on the left were gone**, and "Back"
 * sent people to the workspace list. The user only wanted to change a color scheme and got kicked
 * out of the workspace they were working in.
 *
 * So the success criteria for "merge into one route" are: with `?from=`, the sidebar must be
 * there, the current workspace must be highlighted, and Back must return to that workspace.
 * **Only keeping the parameter in the address bar without really rendering AppShell brings that
 * bug right back**, with exactly the same symptoms. e2e 07 has four cases watching this.
 *
 * # An untrusted key in the query → fall back to no shell, not an error
 *
 * Users can edit the URL: `?from=` may be a key that does not exist, or someone else's workspace.
 * **Now that RBAC is live, a non-member calling `workspace-by-key` gets a 403.**
 *
 * Neither case **may crash the page or show a blank screen**: the content of this page is
 * account-level and should always be visible. Blocking people from their own settings because of
 * a purely navigational shell gets things backwards, especially since "a new user who belongs to
 * no workspace" is exactly who most needs this page first, to set up Ark credentials and an API
 * Key (the backend routes for those are deliberately ActionNone for this person; see
 * authz/matrix.go).
 *
 * So if we cannot fetch it, **treat it as if the parameter was not there**: no shell, Back goes
 * to the workspace list. The fallback path is **the same path** as "the user opened /preferences
 * directly", not a second branch; one fewer branch is one fewer thing that can break.
 *
 * # The parameter value never reaches the DOM or the request path
 *
 * It only goes through `api.getWorkspaceByKey()` as a query (`buildUrl` in `http.ts` escapes with
 * URLSearchParams), and the workspace name shown on the page always comes from the API's
 * `workspace.name`, not the string in the URL. So `?from=<script>…` gets into neither the request
 * path nor the DOM; it just fails to find a workspace → fall back to no shell.
 */

import { AppShell } from "@/app/AppShell";
import { WorkspaceProvider, useWorkspaceByKey } from "@/app/WorkspaceGate";
import { FROM_WORKSPACE_PARAM } from "@/app/preferencesLink";
import { PreferencesPage } from "@/pages/PreferencesPage";
import { useSearchParams } from "react-router-dom";

export function PreferencesRoute() {
  const [params] = useSearchParams();
  // Treat an empty string as "not given": `?from=` is the same as not writing the parameter at all.
  const from = params.get(FROM_WORKSPACE_PARAM) || null;
  const { state, refresh } = useWorkspaceByKey(from);

  /*
   * Only render the shell when the workspace was really fetched. The other three phases (no
   * parameter / still fetching / fetch failed) all render without a shell and **share this one
   * return**.
   *
   * While fetching, render the content without a shell first instead of spinning while waiting
   * for the shell: the content does not depend on the workspace, and content first beats a
   * spinner first. The cost is that with `?from=` the shell appears a frame later (~90ms under
   * mock), in exchange for "opening settings is always instantly editable".
   *
   * ⚠️ Doing the opposite, "show a skeleton while loading", would make the fallback path flash a
   * skeleton too, and the fallback (a non-member clicking a link someone shared) is exactly the
   * path that should feel the least like waiting.
   */
  if (state.phase !== "ready") return <PreferencesPage />;

  return (
    <WorkspaceProvider workspace={state.workspace} refresh={refresh}>
      <AppShell>
        <PreferencesPage />
      </AppShell>
    </WorkspaceProvider>
  );
}
