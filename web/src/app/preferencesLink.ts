/**
 * The URL of the global settings page: **one route, `/preferences`, with the source workspace
 * kept in the query.**
 *
 * # Why this is its own leaf module
 *
 * Four places build this URL (the "Account" group in the sidebar, the two "go to global
 * settings" links in SettingsPage, and the `?tab=apikeys` redirect), spread across AppShell and
 * SettingsPage. If the parameter name were hard-coded in each, missing one during a rename would
 * be a **silent** failure: the parameter is still in the address bar, but PreferencesRoute cannot
 * read it, so the shell quietly does not appear. That is exactly the incident described below.
 *
 * It is a separate module (not inside WorkspaceGate) to **avoid a circular import**:
 * PreferencesRoute lives in `app/PreferencesRoute.tsx` and imports AppShell, and AppShell needs
 * this link. With the constant on a leaf module, both depend only on it, not on each other.
 *
 * # The parameter is named `from`
 *
 * `from` rather than `ws` / `fw`: it answers "**where did you come from**", not "which workspace
 * does this belong to". The content of this page belongs to no workspace (preferences are
 * account-level and do not change when you switch workspaces). Writing `ws=` would make people
 * think preferences are stored per workspace; that is exactly what this page keeps having to
 * explain to users, and the parameter name should not say the opposite. `from` also matches the
 * meaning of the "Back" button on the page.
 *
 * The value is the **key, not the id**: a key in the URL is readable (`?from=growth` tells you at
 * a glance which workspace it is), and the `workspace-by-key` API already exists, so no new
 * fetch-by-id path is needed.
 */

/** Query parameter name. **Changing it requires changing e2e too**: those tests assert the literal. */
export const FROM_WORKSPACE_PARAM = "from";

/**
 * Build the global settings URL.
 *
 * Passing undefined / an empty string as `workspaceKey` gives the shell-less URL (that is what
 * the entry on the workspace list page uses). `tab` is the tab inside the page and is independent
 * of `from`.
 *
 * **The key is escaped with URLSearchParams**, not interpolated with a template string: it ends
 * up in an `<a href>`, and although keys have a character rule (backend
 * `^[a-z0-9][a-z0-9-]{1,31}$`), what we get here is any string the caller passes. Skipping
 * escaping because "upstream should be clean" is a common way injection holes appear.
 */
export function preferencesPath(opts: { workspaceKey?: string; tab?: string } = {}): string {
  const params = new URLSearchParams();
  if (opts.tab) params.set("tab", opts.tab);
  if (opts.workspaceKey) params.set(FROM_WORKSPACE_PARAM, opts.workspaceKey);
  const qs = params.toString();
  return qs ? `/preferences?${qs}` : "/preferences";
}
