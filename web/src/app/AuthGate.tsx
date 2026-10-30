/**
 * Login state context and gate.
 *
 * Its shape copies WorkspaceGate (three states + Provider + subtree reads it via a hook), because
 * it solves the same kind of problem: **confirm one thing before rendering anything; if it cannot
 * be confirmed, do not render half a UI.**
 *
 * # Login is enforced here
 *
 * AuthGate wraps the whole route table (see router.tsx), so pages outside WorkspaceGate such as
 * /workspaces and /preferences also require login. There is no "look around first, log in later"
 * gap: the requirement is "everyone must log in once before using it".
 *
 * When not logged in it renders LandingPage (product intro + login card). There are two ways to
 * log in, and they come back here differently:
 *
 * - **Email + password**: the page never unmounts; LandingPage gets `{user}` and calls
 *   `onLoggedIn`, and this component switches straight to authed.
 * - **OIDC**: a full-page redirect to the IdP, then a 303 back from the backend. That is a fresh
 *   load: this component mounts again, and useEffect naturally runs check() once more.
 *
 * # Mock mode is always logged in
 *
 * With `VITE_API_MODE=local`, LocalApiClient's getCurrentUser() returns a built-in user, so
 * AuthGate passes at once and the 67 e2e cases need no changes. **This is not a back door for
 * local development**: local development runs HttpApiClient (both `./dev.sh` and the vite proxy
 * hit the real backend) and still requires login. Machine callers that really need to skip login
 * (CI / scripts / probes) use the backend's fixed API Key path.
 *
 * # Session expires mid-way
 *
 * Sessions have a lifetime (7 days by default) and can also be logged out elsewhere. After that,
 * every request gets 401. http.ts reports it once at its single request entry/exit point, and on
 * receiving it this component switches the state back to logged out. So the user sees the login
 * page instead of a screen full of "please log in first" error popups.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";
import { api } from "@/data/api";
import type { AuthUser } from "@/data/api";
import { setUnauthenticatedHandler } from "@/data/http";
import { StartupSpinner } from "@/components/Page";
import { LandingPage } from "@/pages/LandingPage";
import { usePrefs } from "@/prefs/PrefsContext";

interface AuthContextValue {
  /** The current logged-in user. Always non-null inside AuthGate. */
  user: AuthUser;
  /** Log out and go back to the login page. */
  logout: () => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

/** Get the current logged-in user. Only usable inside AuthGate. */
export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth 必须在 AuthGate 内使用");
  return ctx;
}

type GateState =
  | { phase: "loading" }
  | { phase: "authed"; user: AuthUser }
  | { phase: "anonymous" };

export function AuthGate({ children }: { children: React.ReactNode }) {
  const { syncFromServer } = usePrefs();
  const [state, setState] = useState<GateState>({ phase: "loading" });

  const check = useCallback(async () => {
    try {
      const user = await api.getCurrentUser();
      setState(user ? { phase: "authed", user } : { phase: "anonymous" });
    } catch {
      // If the backend is unreachable, **treat it as logged out** instead of staying in loading.
      // If it got stuck, the user would see a blank page that spins forever, with nothing to do
      // and no idea why; landing on the login page at least shows a hint like "login service is
      // temporarily unavailable", and allows retrying.
      setState({ phase: "anonymous" });
    }
  }, []);

  useEffect(() => {
    void check();
  }, [check]);

  /**
   * After login is confirmed, sync preferences with the server once (and migrate this device's
   * old settings if needed).
   *
   * # Why AuthGate triggers this instead of PrefsProvider doing it itself
   *
   * PrefsProvider sits **outside** this component (the whole tree needs it for the language; the
   * login page text also goes through t()). If it fetched on mount, that request would happen
   * while logged out and get a 401, and when http.ts sees a 401 it tells this component to switch
   * the app back to the login page. The result is worse than "could not fetch preferences": a user
   * in the middle of logging in gets interrupted by that 401 and has to start over.
   *
   * So the order is reversed: **first confirm there is an identity, then let it sync**.
   * syncFromServer is idempotent, so if this effect re-runs for other reasons it will not fetch
   * again.
   */
  useEffect(() => {
    if (state.phase !== "authed") return;
    void syncFromServer();
  }, [state.phase, syncFromServer]);

  // Session expires mid-way: any request that gets a 401 switches the whole app back to the login page. See the file header.
  useEffect(() => {
    return setUnauthenticatedHandler(() => {
      setState((prev) => (prev.phase === "authed" ? { phase: "anonymous" } : prev));
    });
  }, []);

  const logout = useCallback(async () => {
    try {
      await api.logout();
    } finally {
      // Even if backend logout fails, switch the frontend to the login page: a user who clicked log out but is still inside is worse.
      setState({ phase: "anonymous" });
    }
  }, []);

  const onLoggedIn = useCallback((user: AuthUser) => {
    setState({ phase: "authed", user });
  }, []);

  const value = useMemo(
    () => (state.phase === "authed" ? { user: state.user, logout } : null),
    [state, logout],
  );

  if (state.phase === "loading") {
    // While confirming identity, **show nothing for the first 300ms**; only after that show a
    // centered spinner without text. This path runs on every full-page refresh and usually ends
    // in one or two hundred ms; the old "checking login status" text became a flash of noise on
    // refresh (followed right after by WorkspaceGate's second stage). See the StartupSpinner
    // comments.
    return <StartupSpinner />;
  }

  if (state.phase === "anonymous" || !value) {
    // When logged out, show one screen of product intro + login card (see LandingPage).
    //
    // onLoggedIn is only for password login / sign-up: those are same-page JSON requests, the
    // backend has already set the cookie, and the user we get is the same one /api/auth/me would
    // return, so no need to query again. OIDC leaves the page and comes back, going through
    // check() on mount, so it does not use this.
    return <LandingPage onLoggedIn={onLoggedIn} />;
  }

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}
