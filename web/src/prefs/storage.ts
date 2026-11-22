/**
 * Global preferences: language and appearance.
 *
 * Difference from "workspace settings": these are **cross-workspace** user-level preferences that do not change when switching workspaces, so they are neither stored in
 * ama.store (that is per-workspace business data) nor in the URL.
 *
 * Appearance has three states: light / dark / system. system follows prefers-color-scheme and responds in real time
 * (when the system switches between light and dark the UI follows immediately, without a refresh).
 *
 * # ⚠️ The localStorage here is a **cache**, not the authority
 *
 * Preferences are now stored in the database (account level, across devices), and the source of truth is the backend's user_preferences table.
 * Then why is this key still here? Because **the appearance must land on `<html>` before React mounts**
 * (see main.tsx), and at that moment there is no login yet, let alone a network round trip. Relying only on the database,
 * every page open would first flash light and then switch to dark, which is exactly why it existed in the first place.
 *
 * So the division of work is:
 *
 *   localStorage  local cache, responsible for **the first frame** (readable synchronously)
 *   PG            authority, responsible for **across devices** (corrected asynchronously after login)
 *
 * The sync rules are in PrefsContext.tsx. Do not change the reads and writes here to "call the API directly": that would turn a
 * synchronous operation into an asynchronous one, and the first-frame flash would come right back.
 *
 * The types and normalization functions reuse the ones in types/domain.ts (the same definition as the backend contract),
 * without setting up another set here: two definitions would diverge sooner or later, and divergence shows up as "a value the frontend accepts and the backend does not".
 */

import {
  DEFAULT_USER_PREFERENCES,
  normalizeUserPreferences,
  type Appearance,
  type Language,
  type UserPreferences,
} from "@/types/domain";

export type { Appearance, Language };

/** The shape of preferences. It is just UserPreferences; the local alias saves changing a pile of call sites. */
export type Prefs = UserPreferences;

const STORAGE_KEY = "ama.prefs.v1";

export const DEFAULT_PREFS: Prefs = DEFAULT_USER_PREFERENCES;

/** Read from the local cache. **Synchronous**: the first frame needs it, so it cannot be a Promise. */
export function loadPrefs(): Prefs {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return DEFAULT_PREFS;
    // Validate field by field: values from an old version or broken by hand should not stop the whole app from starting.
    // Shares the same implementation as the backend normalizeUserPreferences.
    return normalizeUserPreferences(JSON.parse(raw) as Partial<Prefs>);
  } catch {
    return DEFAULT_PREFS;
  }
}

/** Write the local cache. Fails silently: it is only a cache, and the authoritative copy is in the database. */
export function savePrefs(prefs: Prefs): void {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(prefs));
  } catch {
    // Fail silently when storage is disabled or full: the in-memory state still works, and interaction should not be blocked by it
  }
}

/**
 * Whether the local cache **has** stored preferences.
 *
 * This is the only test for "migration": a cache means the user on this machine set preferences before they were stored in the database,
 * and those settings must be moved into the database, not lost because of the upgrade. Without a cache do nothing:
 * writing the defaults into the database directly would turn an account that "never set anything" into one "set to the defaults",
 * and that would overwrite the preferences they set on **other devices**.
 *
 * Presence is judged with getItem, not by comparing the value with the defaults: someone who really set the language to Chinese
 * and the appearance to follow the system has values that happen to equal the defaults, but they did set them.
 */
export function hasCachedPrefs(): boolean {
  try {
    return localStorage.getItem(STORAGE_KEY) !== null;
  } catch {
    return false;
  }
}

/** Anything other than system is returned as-is; for system, query the system preference. */
export function resolveAppearance(appearance: Appearance): "light" | "dark" {
  if (appearance !== "system") return appearance;
  if (typeof window === "undefined" || !window.matchMedia) return "light";
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

/**
 * Apply the appearance to <html>.
 *
 * HeroUI component styles have branches depending on .dark, so both the class name and color-scheme must be set:
 * the former drives our own token overrides, the latter makes the browser switch scrollbars and form controls to dark too.
 */
export function applyAppearance(appearance: Appearance): void {
  const resolved = resolveAppearance(appearance);
  const root = document.documentElement;
  root.classList.toggle("dark", resolved === "dark");
  root.classList.toggle("light", resolved === "light");
  root.style.colorScheme = resolved;
}

export function applyLanguage(language: Language): void {
  document.documentElement.lang = language;
}
