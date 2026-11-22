/**
 * Preferences context: provides the current language/appearance and the translation function to the whole tree.
 *
 * Mounted above the router (see main.tsx), because both /preferences and /workspaces use it, and after switching
 * the whole UI must respond immediately; so it is a context rather than a module-level singleton.
 *
 * # Two storage places; which one decides
 *
 *   localStorage (ama.prefs.v1)  local cache, **readable synchronously**, responsible for the first frame
 *   PG (user_preferences table)  authority, **across devices**, corrected asynchronously after login
 *
 * The order is: on mount, render the UI from the cache first (zero delay, no flash) → after login fetch the copy in the database →
 * if they differ, the database wins and the cache is written back. That way "my color scheme is still there on another computer" and
 * "opening does not flash light" both hold.
 *
 * # ⚠️ This Provider is **outside** AuthGate, so it must not send requests unconditionally
 *
 * The whole tree needs it to provide the language (the login page text also goes through t()), so it must be mounted before auth.
 * But that means **the user may not be logged in at all at mount time**: calling /api/user-preferences then gives 401,
 * and when http.ts receives a 401 it tells AuthGate to switch the whole app back to the login page.
 *
 * The consequence is not as light as "preferences could not be fetched": a user in the middle of scanning a code to log in would be interrupted by that 401,
 * the page jumps from the login card back to the landing page, and they have to click again. So syncing **is triggered explicitly by AuthGate after login
 * succeeds** (syncFromServer), and the Provider does not fetch on its own.
 */

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import {
  applyAppearance,
  applyLanguage,
  hasCachedPrefs,
  loadPrefs,
  resolveAppearance,
  savePrefs,
  type Appearance,
  type Language,
  type Prefs,
} from "@/prefs/storage";
import { MESSAGES, type MessageKey } from "@/prefs/messages";
import { api } from "@/data/api";

/** Translation function: t("ws.memberCount", { count: 4 }) */
export type TFunc = (key: MessageKey, vars?: Record<string, string | number>) => string;

interface PrefsContextValue extends Prefs {
  /** The actual appearance after resolving system, used when the UI shows "the system is currently dark". */
  resolvedAppearance: "light" | "dark";
  setLanguage: (lang: Language) => void;
  setAppearance: (appearance: Appearance) => void;
  /**
   * Sync with the server once. **Called by AuthGate after login is confirmed**; see the file header.
   *
   * Idempotent and runs only once after each login (a ref inside blocks repeats): it is triggered by AuthGate's
   * effect, and that effect's dependencies include the user object.
   */
  syncFromServer: () => Promise<void>;
  t: TFunc;
}

const PrefsContext = createContext<PrefsContextValue | null>(null);

function interpolate(template: string, vars?: Record<string, string | number>): string {
  if (!vars) return template;
  return template.replace(/\{(\w+)\}/g, (whole, name: string) =>
    name in vars ? String(vars[name]) : whole,
  );
}

export function PrefsProvider({ children }: { children: ReactNode }) {
  const [prefs, setPrefs] = useState<Prefs>(() => loadPrefs());
  // In system mode changes of the system theme must be reflected in real time, so the resolved result is stored separately
  const [resolved, setResolved] = useState<"light" | "dark">(() =>
    resolveAppearance(loadPrefs().appearance),
  );

  /**
   * Already synced with the server once.
   *
   * A ref instead of state: it only blocks repeated runs and should not trigger re-renders.
   * AuthGate's effect reruns when the user object changes (and in React strict mode it runs twice during development),
   * and without this gate it would fetch repeatedly and might overwrite a value the user just changed.
   */
  const synced = useRef(false);

  // When preferences change, apply them to <html> and **the local cache**.
  //
  // ⚠️ This deliberately **does not write to the server**: this effect runs on every prefs change, including
  // the setPrefs "after fetching from the server", which would immediately write back the value just read,
  // turning one read into a read plus a write. Storing in the database is started explicitly by setLanguage / setAppearance.
  useEffect(() => {
    applyAppearance(prefs.appearance);
    applyLanguage(prefs.language);
    setResolved(resolveAppearance(prefs.appearance));
    savePrefs(prefs);
  }, [prefs]);

  // Only system mode needs to listen to the system theme; listening in other modes is wasted
  useEffect(() => {
    if (prefs.appearance !== "system") return;
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = () => {
      applyAppearance("system");
      setResolved(resolveAppearance("system"));
    };
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, [prefs.appearance]);

  /**
   * After login, sync with the server once, and move the old local settings up when needed.
   *
   * # Two branches, decided by "does this machine have a cache"
   *
   *   cache + nothing in the database  →  **migrate**: write this machine's copy into the database (the user set it before it was stored, it must not be lost)
   *   otherwise                        →  **the database wins**: apply the database copy to memory and the cache
   *
   * Migration only happens when "the database has the defaults". The test must be "never set in the database", not
   * "the cache differs from the database": the latter would make someone who changed preferences on another device have the new settings
   * overwritten by the old cache on this machine as soon as they log in, though they had just changed it on their phone.
   *
   * ⚠️ But "never set in the database" shows up in the API as **the defaults** (the backend does not distinguish "no record" from
   * "what is stored is the default"; see the notes in schema_prefs_pg.sql). So the test here is
   * "the database equals the defaults and the local cache does not": only this combination can confirm
   * "this machine has real settings and the server does not". Conversely, if the local cache itself is the defaults,
   * migrating or not makes no difference, so just skip it.
   */
  const syncFromServer = useCallback(async () => {
    if (synced.current) return;
    synced.current = true;

    try {
      const remote = await api.getUserPreferences();
      const cached = loadPrefs();
      const remoteIsDefault = remote.language === "zh-CN" && remote.appearance === "system";
      const cachedIsDefault = cached.language === "zh-CN" && cached.appearance === "system";

      if (hasCachedPrefs() && remoteIsDefault && !cachedIsDefault) {
        // Migration: this machine's copy was set before the upgrade, and the server does not have it yet. Write it up; the UI does not change.
        setPrefs(cached);
        await api.saveUserPreferences(cached);
        return;
      }

      // The database wins. **setPrefs even if it equals the cache**: the backend normalizes dirty values,
      // so what comes back may differ from what was sent, and then the UI should follow the value really in effect.
      setPrefs(remote);
    } catch {
      // **If it cannot be fetched, use the cache and do nothing.**
      //
      // Preferences are "how the UI looks", not functionality. Popping an error for it, or falling back to the defaults,
      // are both much worse than "keep using the last settings": users would see the color scheme change for no reason,
      // while they did nothing. Same as the silent failure of savePrefs.
      //
      // ⚠️ synced is not reset here either: retrying again and again when it cannot connect is pointless (AuthGate's
      // effect may rerun for other reasons), and the next page refresh will naturally try again.
    }
  }, []);

  /**
   * Change one field: update memory first (the UI changes immediately), then write to the database asynchronously.
   *
   * **Do not wait for the API to return before updating the UI**: waiting a round trip before dark mode takes effect is a clearly visible delay,
   * and the cost of this failing is only "it did not follow to another device". A failed write is not rolled back either:
   * bouncing back the color scheme the user just picked is more confusing than not saving it (they would think they clicked wrong).
   * The local cache copy is written by the effect above, so it is still the new value after a refresh.
   */
  const persist = useCallback((next: Prefs) => {
    setPrefs(next);
    void api.saveUserPreferences(next).catch(() => {
      // Silent: see above. **This catch is required**: the rejection of a bare void promise becomes
      // an unhandled rejection, which looks like a real bug in the console.
    });
  }, []);

  const setLanguage = useCallback(
    (language: Language) => {
      setPrefs((p) => {
        const next = { ...p, language };
        persist(next);
        return next;
      });
    },
    [persist],
  );

  const setAppearance = useCallback(
    (appearance: Appearance) => {
      setPrefs((p) => {
        const next = { ...p, appearance };
        persist(next);
        return next;
      });
    },
    [persist],
  );

  const t = useCallback<TFunc>(
    (key, vars) => interpolate(MESSAGES[prefs.language][key] ?? key, vars),
    [prefs.language],
  );

  const value = useMemo<PrefsContextValue>(
    () => ({
      ...prefs,
      resolvedAppearance: resolved,
      setLanguage,
      setAppearance,
      syncFromServer,
      t,
    }),
    [prefs, resolved, setLanguage, setAppearance, syncFromServer, t],
  );

  return <PrefsContext.Provider value={value}>{children}</PrefsContext.Provider>;
}

export function usePrefs(): PrefsContextValue {
  const ctx = useContext(PrefsContext);
  if (!ctx) throw new Error("usePrefs 必须在 PrefsProvider 内使用");
  return ctx;
}

/** Use it when only the translation function is needed, saving destructuring everywhere. */
export function useT(): TFunc {
  return usePrefs().t;
}
