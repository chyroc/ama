/**
 * Workspace context and gate.
 *
 * Core rule: every operation must happen inside a workspace. It is implemented in two layers:
 * 1. Before rendering any child route, WorkspaceGate fetches the workspace by the key in the URL,
 *    and redirects to the onboarding page if it cannot;
 * 2. The subtree gets the current workspace via useWorkspace(); pages do not query it again.
 *
 * The key lives in the URL instead of global state so links can be shared and a refresh does not
 * lose context.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useState,
  type ReactNode,
} from "react";
import { Link, Navigate, Outlet, useLocation, useParams } from "react-router-dom";
import { api } from "@/data/api";
import { ACCESS_DENIED_CODE, ApiError } from "@/data/http";
import {
  forgetLastWorkspaceKey,
  isAutoOpenedFromLast,
  rememberLastWorkspaceKey,
} from "@/lib/lastWorkspace";
import type { Workspace } from "@/types/domain";
import { PageBody, StartupSpinner } from "@/components/Page";
import { useT } from "@/prefs/PrefsContext";

interface WorkspaceContextValue {
  workspace: Workspace;
  /** Call after workspace data changes, so the whole subtree sees the latest value. */
  refresh: () => Promise<void>;
}

const WorkspaceContext = createContext<WorkspaceContextValue | null>(null);

export function useWorkspace(): WorkspaceContextValue {
  const ctx = useContext(WorkspaceContext);
  if (!ctx) throw new Error("useWorkspace 必须在 WorkspaceGate 内使用");
  return ctx;
}

/**
 * Get the current workspace; **returns null instead of throwing when not inside a workspace**.
 *
 * For the global settings page: it has one route, `/preferences`, but **two kinds of shell**.
 * With `?from=<key>`, PreferencesRoute wraps it in AppShell (so there is context); without it,
 * it renders bare (so there is none). The same component has to work under both shells.
 *
 * **This null is also where the fallback lands**: when `?from=` gives a key that does not exist
 * or belongs to a workspace you are not a member of, PreferencesRoute does not render the shell,
 * so this reads null and the Back link goes to the workspace list automatically. The fallback
 * needs no second check; it is just the "no context" case.
 *
 * ⚠️ **Do not use this to replace useWorkspace.** That throw is deliberate: almost every page can
 * only render inside a workspace, and missing context means the wiring is wrong; throwing makes
 * it visible. Silently returning null would make that kind of bug look like "the page data is all
 * empty", with not a single error.
 */
export function useOptionalWorkspace(): WorkspaceContextValue | null {
  return useContext(WorkspaceContext);
}

/**
 * Feed an already-fetched workspace to the subtree.
 *
 * Why it exists: **the global settings page needs to build the context itself, outside
 * WorkspaceGate** (its route is not under `/w/:key`; see PreferencesRoute). We export a Provider
 * rather than the bare context so that "how to build this value" lives in one place: forgetting
 * refresh is a compile error, whereas a missing refresh shows up as "the sidebar does not update
 * after renaming the workspace", with no error.
 */
export function WorkspaceProvider({
  workspace,
  refresh,
  children,
}: {
  workspace: Workspace;
  refresh: () => Promise<void>;
  children: ReactNode;
}) {
  return (
    <WorkspaceContext.Provider value={{ workspace, refresh }}>{children}</WorkspaceContext.Provider>
  );
}

/** Helper: build a path under the current workspace. */
export function useWorkspacePath(): (path: string) => string {
  const { workspace } = useWorkspace();
  return useCallback((path: string) => `/w/${workspace.key}${path}`, [workspace.key]);
}

type GateState =
  | { phase: "loading" }
  | { phase: "ready"; workspace: Workspace }
  | { phase: "missing" }
  /**
   * Cannot open. `denied` separates "you are not in this workspace" from "this attempt failed".
   *
   * Both look the same in the UI (that message + a link back to the list), but **they treat the
   * "last visited workspace" memory in opposite ways**: the former is permanent, remembering it
   * is useless, so forget it; the latter is a network blip, and forgetting would let one 500 wipe
   * the user's preference, and next time they would only think "why doesn't it remember me". The
   * check comes from the same source as the message text (`ApiError.code`), not the status code.
   */
  | { phase: "forbidden"; message: string; denied: boolean };
// `message` is empty when there is no server message to show; the UI then renders a translated
// fallback (this hook has no `t`, and the text must follow the language at render time).

/**
 * Fetch a workspace by key, mapping the three kinds of failure to three phases.
 *
 * It is a hook because **there are two callers**: WorkspaceGate (blocks the whole subtree if the
 * fetch fails) and PreferencesRoute (skips the shell if the fetch fails; see the comments there).
 * They handle failure in completely opposite ways, but "how to fetch and how to classify failure"
 * must be the same code. If each wrote its own, one of them missing a catch would again mean a
 * permanent "opening workspace".
 *
 * Passing null as `key` means "do not fetch" and goes straight to missing (that is what
 * PreferencesRoute does when there is no query parameter).
 */
export function useWorkspaceByKey(
  key: string | null,
): { state: GateState; refresh: () => Promise<void> } {
  const [state, setState] = useState<GateState>(key ? { phase: "loading" } : { phase: "missing" });

  const fetch = useCallback(async () => {
    if (!key) {
      setState({ phase: "missing" });
      return;
    }
    // **The catch is required**: this used to be a bare await. Any failure other than
    // "404 → null" would reject the Promise, setState would never run, and the page would stay on
    // "opening workspace" forever: a dead state with no error and no visible cause.
    //
    // 403 gets its own phase instead of merging into missing: redirecting to the workspace list
    // would look to the user like "clicked and nothing happened", and they would never get the
    // only useful message, "you are not in this workspace".
    //
    // ⚠️ **With RBAC live, 403 is now normal**: a non-member calling `workspace-by-key` gets a
    // 403. So this catch is not defensive programming; it is the main path.
    try {
      const ws = await api.getWorkspaceByKey(key);
      setState(ws ? { phase: "ready", workspace: ws } : { phase: "missing" });
    } catch (e) {
      const denied = e instanceof ApiError && e.code === ACCESS_DENIED_CODE;
      const message = denied
        ? e.message
        : e instanceof Error
          ? e.message
          : "";
      setState({ phase: "forbidden", message, denied });
    }
  }, [key]);

  useEffect(() => {
    setState(key ? { phase: "loading" } : { phase: "missing" });
    void fetch();
  }, [fetch, key]);

  return { state, refresh: fetch };
}

export function WorkspaceGate() {
  const { workspaceKey } = useParams<{ workspaceKey: string }>();
  const { state, refresh } = useWorkspaceByKey(workspaceKey ?? null);
  const location = useLocation();
  const t = useT();

  /*
   * Remember "which workspace I was in last", so the root path `/` can jump back to it (see
   * lib/lastWorkspace.ts).
   *
   * # Why it is written here, not in each page or in some route listener
   *
   * WorkspaceGate is the element for the `/w/:workspaceKey` level, and **it does not remount when
   * child routes change**: going from overview to chat to skills, this component stays the same
   * instance. So writing here naturally means "write once when entering / switching workspace",
   * not "write on every route change". Hooking it on some global location listener would give the
   * latter: ten sidebar clicks, ten writes, all of the same value.
   *
   * The dependency is the **string** `state.workspace.key`, not the `state` object: `refresh()`
   * (after renaming the workspace, etc.) creates a new object with the same key, and depending on
   * the object would do a useless write on every refresh.
   *
   * Only write when ready: a workspace that cannot be opened should not be remembered; that is
   * exactly what the next effect clears.
   */
  const readyKey = state.phase === "ready" ? state.workspace.key : null;
  useEffect(() => {
    if (readyKey) rememberLastWorkspaceKey(readyKey);
  }, [readyKey]);

  /*
   * If it cannot be opened, forget this memory.
   *
   * Workspaces get deleted, people get removed from members, keys get changed. Without clearing,
   * every visit to `/` sends the user to the same place that cannot be opened and bounces them
   * back to the list page, and **they can never fix it themselves**; and nothing on this path
   * reports an error.
   *
   * `forgetLastWorkspaceKey` only clears it if the remembered key is this key (see the notes
   * there): typing someone else's workspace by hand and hitting a 403 should not also wipe the
   * memory of the workspace the user really uses.
   *
   * ⚠️ **Only clear the ones that "really are gone / really have no permission"** (missing, and
   * forbidden with denied). A 500 or a network drop also lands in forbidden, and that is
   * temporary. Clearing in that case would let one server blip wipe the user's preference, and
   * next time they would only think "why doesn't it remember me", with no clue pointing to that
   * blip.
   */
  const failedKey =
    state.phase === "missing" || (state.phase === "forbidden" && state.denied)
      ? (workspaceKey ?? null)
      : null;
  useEffect(() => {
    if (failedKey) forgetLastWorkspaceKey(failedKey);
  }, [failedKey]);

  if (state.phase === "loading") {
    // While fetching the workspace, **show nothing for the first 300ms**; only after that show a
    // centered spinner without text. See the StartupSpinner comments: this gate runs in series
    // with AuthGate's, so one refresh goes through both. The two old text stages together often
    // lasted under half a second: too short to read, and nothing worth reading.
    return <StartupSpinner />;
  }

  // Workspace does not exist (wrong key typed by hand, or the workspace was deleted) → back to onboarding, instead of rendering half a UI
  if (state.phase === "missing") {
    return <Navigate to="/workspaces" replace />;
  }

  // No access / cannot open: say why, and give a way back.
  if (state.phase === "forbidden") {
    /*
     * ⚠️ **Unless the user never asked to enter this workspace**: that is an automatic jump from
     * `/`, and all they wanted was "take me back to work". Throwing a permission error for a
     * workspace they did not click is just confusing: last time they could get in, this time they
     * open the browser and get told they have no permission. In this case quietly go back to the
     * list page (the memory has already been cleared by the effect above, so next time they land
     * on the list directly).
     *
     * Typing the URL by hand, clicking a bookmark, or a link someone shared **still takes the path
     * below**: they explicitly chose this workspace, a silent redirect would look like "clicked and
     * nothing happened", and "you are not in this workspace" is the only useful message. The two
     * entry paths are told apart by location.state, which is not in the URL; see
     * lib/lastWorkspace.ts for why.
     */
    if (isAutoOpenedFromLast(location.state)) {
      return <Navigate to="/workspaces" replace />;
    }
    return (
      <PageBody>
        <div
          role="alert"
          className="mx-auto flex max-w-md flex-col items-center gap-4 py-16 text-center"
          data-testid="workspace-forbidden"
        >
          <p className="text-sm text-muted">{state.message || t("wsGate.openFailed")}</p>
          <Link to="/workspaces" className="text-sm underline">
            {t("wsGate.backToList")}
          </Link>
        </div>
      </PageBody>
    );
  }

  return (
    <WorkspaceProvider workspace={state.workspace} refresh={refresh}>
      <Outlet />
    </WorkspaceProvider>
  );
}
