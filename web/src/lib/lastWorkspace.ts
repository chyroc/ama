/**
 * "The most recently visited workspace": a local memory used so the root path `/` goes straight
 * into that workspace.
 *
 * # Why localStorage and not the database
 *
 * The repo has two opposite precedents; do not copy the wrong one:
 *
 *   prefs/storage.ts        localStorage as a cache, for synchronous reads on the first frame
 *   app/OnboardingTour.tsx  **deliberately does not use** localStorage; state is stored in PG
 *
 * Onboarding must be stored in the database because "the same person is never onboarded twice"
 * is a **correctness requirement**:
 * localStorage is gone in another browser, which would mean old users get onboarded again.
 *
 * This memory is not that kind: losing it only falls back to the workspace list for the user to
 * pick, a **degradable preference**.
 * Adding a table, an endpoint and a first-screen round trip for it would only buy "remember where
 * I was last time even on another computer",
 * and a first-screen round trip is exactly the cost this feature can least afford (see "does not
 * block the first screen" below).
 * So it is not persisted to the server here; **this is a decision, not an omission**.
 *
 * # Does not block the first screen
 *
 * Reads must be **synchronous**: the root path must decide where to go on the first frame, so it
 * does not "render the list page first, then jump away"
 * with a flash. This is the same reason as prefs' "appearance must be applied to <html> before
 * React mounts".
 *
 * # Deliberately does not listen to storage events
 *
 * When two tabs have different workspaces open, the last write wins; that is acceptable. But it
 * must **never** sync in another tab's
 * writes: switching workspace in tab A would then drag tab B along,
 * while the user may be typing in B. This value is read only once, at the moment the root path is
 * entered.
 */

import { validateWorkspaceKey } from "@/types/domain";

/** Versioned, like `ama.prefs.v1`: when the shape changes, old values simply do not match, no migration needed. */
const STORAGE_KEY = "ama.lastWorkspace.v1";

/**
 * Read the last visited workspace key; null if missing or invalid. **Synchronous**: the first
 * frame needs it.
 *
 * ⚠️ **What is read must go through `validateWorkspaceKey`**. This value is put into the URL
 * (`/w/${key}`),
 * and localStorage can be edited by the user; without validation a hand-broken value would be put
 * into the path as is,
 * building an address that does not open and whose error does not point at the real cause.
 * Validity uses the same rule as creating a workspace,
 * with no separate regex here: two sets of rules drift apart sooner or later.
 */
export function loadLastWorkspaceKey(): string | null {
  try {
    const key = localStorage.getItem(STORAGE_KEY);
    if (!key) return null;
    return validateWorkspaceKey(key) === null ? key : null;
  } catch {
    // Private mode / storage disabled: treat as "not remembered", go back to the list page and let
    // the user pick
    return null;
  }
}

/** Remember this workspace. Fails silently: it is only a convenience memory; failing to write should not block any interaction. */
export function rememberLastWorkspaceKey(key: string): void {
  try {
    localStorage.setItem(STORAGE_KEY, key);
  } catch {
    // Storage disabled or full: the next visit to `/` falls back to the list page, nothing more
  }
}

/**
 * Forget this workspace, **only if the remembered one is this one**.
 *
 * A workspace that cannot be opened must be forgotten right away, otherwise every visit to `/`
 * sends the user to the same 404/403,
 * then bounces them back to the list page, and they can never fix it themselves.
 *
 * ⚠️ Must compare the key; do not blindly `removeItem`. When a user types someone else's workspace
 * key and hits a 403,
 * a blind clear would also wipe the memory the user **actually uses**: a silent loss of function
 * with zero errors.
 */
export function forgetLastWorkspaceKey(key: string): void {
  try {
    if (localStorage.getItem(STORAGE_KEY) === key) localStorage.removeItem(STORAGE_KEY);
  } catch {
    // Same as above, silent
  }
}

/**
 * Marker put on `location.state` during the automatic redirect.
 *
 * # Why tell "redirected automatically" apart from "typed by the user"
 *
 * For the same workspace that cannot be opened, the two origins must get **opposite** results:
 *
 *   typed by the user / bookmark  → say clearly "you are not in this workspace" (WorkspaceGate's
 *                                   forbidden page).
 *                                   A silent redirect would look like "clicked and nothing happened".
 *   redirected from `/`           → go straight back to the workspace list. They never asked to
 *                                   enter this workspace;
 *                                   throwing a permission error for a workspace they never clicked
 *                                   would only confuse them.
 *
 * # Why location.state and not a query parameter
 *
 * `?from=root` goes into the address bar, so it gets bookmarked and shared; for **someone else**
 * that link would mean
 * "automatic redirect" while they actually clicked it by hand, which flips the table above.
 * `location.state` is not in the URL, so when users type it by hand it is naturally absent.
 */
export const AUTO_OPEN_STATE = { fromLastWorkspace: true } as const;

/** Whether this entry into the workspace was an automatic redirect from `/`. */
export function isAutoOpenedFromLast(state: unknown): boolean {
  return (
    typeof state === "object" &&
    state !== null &&
    (state as { fromLastWorkspace?: unknown }).fromLastWorkspace === true
  );
}
