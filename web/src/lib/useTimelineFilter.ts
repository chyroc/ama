/**
 * Filter state of the Trace view, the one and only copy.
 *
 * State **all goes in the URL** (`useSearchParams`), matching the site-wide rule "links can be
 * shared, refresh keeps context",
 * and continuing the `useRunView()` approach of `ViewSwitch`. The run detail page and the chat
 * page share this
 * hook; before it, each page kept its own `visibleCategories` + `threadId`, the same logic
 * copied twice, and changing one would surely miss the other.
 *
 * Why not the single `?filter=` the requirement asked for: that would need our own separator and
 * escaping rules, while keywords
 * may well contain commas, colons and spaces. Split into several meaningful parameters,
 * `URLSearchParams` handles escaping for free,
 * and the URL is readable by people and directly assertable in e2e. The real requirement "filter
 * state goes in the URL" is unchanged.
 */

import { useCallback, useMemo } from "react";
import { useSearchParams } from "react-router-dom";
import {
  DEFAULT_VISIBLE_CATEGORIES,
  type EventCategory,
  type RunEventType,
} from "@/types/events";
import {
  domainOf,
  EVENT_DOMAINS,
  FACET_PRESETS,
  SLOW_TOOL_MS,
  type EventDomain,
  type FacetPresetId,
} from "@/lib/eventFacets";
import {
  isFailed,
  modelRequestDurationMs,
  sortRawRows,
  type RawEventRow,
} from "@/lib/rawEvents";

const P_CAT = "cat";
const P_DOM = "dom";
const P_TYPE = "etype";
const P_QUERY = "q";
const P_SLOW = "slow";
const P_FAILED = "failed";
const P_THREAD = "thread";

const ALL_CATEGORIES: EventCategory[] = [
  "conversation",
  "thinking",
  "tool",
  "thread",
  "status",
  "trace",
  "error",
  "outcome",
];

export interface TimelineFilterState {
  /** Allowlist of semantic categories. trace is hidden by default (span.* is high volume and not needed for daily debugging). */
  categories: Set<EventCategory>;
  /** Allowlist of Ark event domains. All 4 on by default. */
  domains: Set<EventDomain>;
  /** Allowlist of concrete event types. **An empty set means no limit by type**, not "show nothing". */
  types: Set<RunEventType>;
  query: string;
  slowOnly: boolean;
  errorsOnly: boolean;
  threadId: string | null;
}

/** Keyword index: entry key → the joined lowercase search string. */
export type Haystack = Map<string, string>;

function sameSet<T>(a: Set<T>, b: readonly T[]): boolean {
  return a.size === b.length && b.every((v) => a.has(v));
}

/** Comma-separated enum parameter → Set. Unknown values are dropped; if all are dropped, fall back to fallback. */
function parseSet<T extends string>(
  raw: string | null,
  allowed: readonly T[],
  fallback: readonly T[],
): Set<T> {
  if (raw === null) return new Set(fallback);
  const picked = raw
    .split(",")
    .map((s) => s.trim())
    .filter((s): s is T => (allowed as readonly string[]).includes(s));
  // A misspelled parameter should not empty the page: the user would see "no events",
  // while the real cause (the parameter was not recognized) is completely invisible in the UI. So
  // fall back to the default.
  return new Set(picked.length ? picked : fallback);
}

/** Sort when writing back to the URL: the same selection always gives the same string, so links are stable and e2e can assert easily. */
function serializeSet<T extends string>(set: Set<T>): string {
  return [...set].sort().join(",");
}

export function useTimelineFilter(allTypes: readonly RunEventType[]) {
  const [params, setParams] = useSearchParams();

  const filter = useMemo<TimelineFilterState>(() => {
    return {
      categories: parseSet(params.get(P_CAT), ALL_CATEGORIES, DEFAULT_VISIBLE_CATEGORIES),
      domains: parseSet(params.get(P_DOM), EVENT_DOMAINS, EVENT_DOMAINS),
      // The fallback for types is the empty set: its default meaning is "no limit",
      // so parseSet's "if empty, go back to default" cannot be applied here.
      types: new Set(
        (params.get(P_TYPE) ?? "")
          .split(",")
          .map((s) => s.trim())
          .filter((s): s is RunEventType => (allTypes as readonly string[]).includes(s)),
      ),
      query: params.get(P_QUERY) ?? "",
      slowOnly: params.get(P_SLOW) === "1",
      errorsOnly: params.get(P_FAILED) === "1",
      threadId: params.get(P_THREAD),
    };
  }, [params, allTypes]);

  /**
   * Incremental rewrite. Neither of these two can be skipped:
   *
   * 1. **Must be based on the current URL, not a whole replacement**: the chat page also has
   *    `?agent=`, plus `?view=` that decides whether
   *    this is the Trace view; overwriting wholesale would wipe them (it shows as jumping back to
   *    the conversation view on any filter change).
   *
   * 2. **The base must be `window.location.search`, not `params` from the closure.**
   *    We were bitten by this: when two filters change in a row (click a preset, then type a
   *    keyword right away), the second call gets `params`
   *    still from the render snapshot before the first, so the first change is silently
   *    overwritten. It shows as "clicked the tools preset,
   *    and typing jumps back to all", with no error at all.
   *
   *    Note that the functional overload of `setSearchParams` **does not fix this**: react-router
   *    7 implements it as
   *    `nextInit(new URLSearchParams(searchParams))`, and the argument is still that memoized
   *    snapshot
   *    (see the useSearchParams source; `searchParams` depends on `[location.search]`).
   *    So we read the address bar directly here: it is the only always-current source of truth.
   */
  const write = useCallback(
    (mutate: (next: URLSearchParams) => void) => {
      const next = new URLSearchParams(window.location.search);
      mutate(next);
      // replace: adjusting filters should not push a browser history entry, or "back" would become
      // undoing filters step by step.
      setParams(next, { replace: true });
    },
    [setParams],
  );

  /**
   * Toggle one option in an enum set. Defaults are never written to the URL, to avoid leaving a
   * query with no information.
   *
   * The current value is read from `next` (i.e. the address bar), not from the closure's filter;
   * for why see the comment on write.
   */
  const toggleIn = useCallback(
    <T extends string>(key: string, value: T, allowed: readonly T[], defaults: readonly T[]) =>
      write((next) => {
        const cur = parseSet(next.get(key), allowed, defaults);
        if (cur.has(value)) cur.delete(value);
        else cur.add(value);
        if (sameSet(cur, defaults)) next.delete(key);
        else next.set(key, serializeSet(cur));
      }),
    [write],
  );

  const toggleCategory = useCallback(
    (key: string) =>
      toggleIn(P_CAT, key as EventCategory, ALL_CATEGORIES, DEFAULT_VISIBLE_CATEGORIES),
    [toggleIn],
  );

  const toggleDomain = useCallback(
    (domain: EventDomain) => toggleIn(P_DOM, domain, EVENT_DOMAINS, EVENT_DOMAINS),
    [toggleIn],
  );

  const setTypes = useCallback(
    (types: Set<RunEventType>) =>
      write((next) => {
        if (types.size === 0) next.delete(P_TYPE);
        else next.set(P_TYPE, serializeSet(types));
      }),
    [write],
  );

  /**
   * Preset: map with one click onto the two axes "category + concrete type".
   *
   * ⚠ Categories must be set **at the same time** here: the `span.model_request_*` that the
   * "model" preset wants belongs to category
   * `trace`, and trace is hidden by default. Setting only types without categories would make this
   * preset
   * show nothing at all: no error, no pointer to the cause, the hardest kind to debug.
   *
   * Clicking the same preset again cancels it (back to default), so it is a toggle rather than a
   * one-way action.
   */
  const applyPreset = useCallback(
    (id: FacetPresetId) => {
      const preset = FACET_PRESETS.find((p) => p.id === id);
      if (!preset) return;

      write((next) => {
        // "Is this preset already active" must also be judged from the address bar's current
        // value, not the closure's filter;
        // same reason as the comment in write.
        const cats = parseSet(next.get(P_CAT), ALL_CATEGORIES, DEFAULT_VISIBLE_CATEGORIES);
        const types = new Set(
          (next.get(P_TYPE) ?? "")
            .split(",")
            .map((s) => s.trim())
            .filter(Boolean),
        );
        const active =
          sameSet(cats, preset.categories as EventCategory[]) &&
          sameSet(types, preset.types as readonly string[]);

        // Clicking the same preset again cancels it, so it is a toggle rather than a one-way action.
        if (active) {
          next.delete(P_CAT);
          next.delete(P_TYPE);
          return;
        }
        next.set(P_CAT, serializeSet(new Set(preset.categories as EventCategory[])));
        if (preset.types.length) next.set(P_TYPE, serializeSet(new Set(preset.types)));
        else next.delete(P_TYPE);
      });
    },
    [write],
  );

  const setQuery = useCallback(
    (q: string) =>
      write((next) => {
        if (q.trim()) next.set(P_QUERY, q);
        else next.delete(P_QUERY);
      }),
    [write],
  );

  const setFlag = useCallback(
    (key: string, on: boolean) =>
      write((next) => {
        if (on) next.set(key, "1");
        else next.delete(key);
      }),
    [write],
  );

  const setThreadId = useCallback(
    (id: string | null) =>
      write((next) => {
        if (id) next.set(P_THREAD, id);
        else next.delete(P_THREAD);
      }),
    [write],
  );

  const reset = useCallback(
    () =>
      write((next) => {
        for (const k of [P_CAT, P_DOM, P_TYPE, P_QUERY, P_SLOW, P_FAILED, P_THREAD]) {
          next.delete(k);
        }
      }),
    [write],
  );

  const isDirty = useMemo(
    () =>
      [P_CAT, P_DOM, P_TYPE, P_QUERY, P_SLOW, P_FAILED, P_THREAD].some((k) =>
        params.has(k),
      ),
    [params],
  );

  return {
    filter,
    isDirty,
    toggleCategory,
    toggleDomain,
    setTypes,
    applyPreset,
    setQuery,
    setSlowOnly: (on: boolean) => setFlag(P_SLOW, on),
    setErrorsOnly: (on: boolean) => setFlag(P_FAILED, on),
    setThreadId,
    reset,
  };
}

/*
 * There used to be two more versions here that took `TimelineItem`: `buildHaystack()` and
 * `applyFilter()`.
 *
 * **They became dead code with the event view rewrite**: the only caller is the event view, which
 * now uses
 * the pair below that takes `RawEventRow` (`buildRawHaystack` / `applyRawFilter`).
 * The conversation view never filters (category choices are decided by ConversationView itself
 * when folding by turn), and neither does the call tree.
 *
 * So they were deleted rather than kept for "maybe useful later": if kept, the
 * `filter.sort === "duration"` in `applyFilter` would become the only place still reading `sort`,
 * and that field no longer exists (see the file header).
 */

// ---------------------------------------------------------------------------
// Event view (raw events, one per row)
// ---------------------------------------------------------------------------

/**
 * Keyword index for raw event rows.
 *
 * Separate from `buildHaystack` rather than reused: that one took **paired entries** (with derived
 * fields like command / result),
 * while this takes single raw events. What is indexed is the whole event's JSON plus the type name
 * and
 * summary. The raw JSON already has every field, so "search input / search stdout / search
 * tool_use_id"
 * all match naturally.
 *
 * **Must be memoized** (the caller caches by `[rows]`): every event is `JSON.stringify`'d,
 * and without caching every keystroke would re-serialize all events, which lags right away on long
 * sessions.
 */
export function buildRawHaystack(rows: readonly RawEventRow[]): Haystack {
  const map: Haystack = new Map();
  for (const r of rows) {
    map.set(r.key, [r.label, r.summary, r.type, JSON.stringify(r.event)].join("\n").toLowerCase());
  }
  return map;
}

/**
 * Narrow raw event rows by the filter.
 *
 * Four differences from `applyFilter`, all coming from the meaning "one event per row":
 *
 * 1. **Sorted not by sequence but by (instant, sequence)**: see `sortRawRows`. The event view is
 *    read by
 *    time, and processed_at has nanoseconds while JS only has milliseconds, so sequence breaks ties
 *    within a millisecond.
 * 2. **"Slow calls only" cannot filter here**: duration is "receipt instant minus start instant",
 *    a product of pairing,
 *    and a single event has no such quantity. So this option **only keeps
 *    `span.model_request_end`, which has an explicit link field** (its `model_request_start_id` is
 *    given by Ark),
 *    and everything else does not match, instead of quietly letting it through as "none are slow".
 * 3. **"Failures only" recognizes three kinds**: error-category events, `is_error` on a receipt,
 *    and `is_error` on `span.model_request_end`.
 * 4. Sorting by duration falls back to sorting by time (same as above; a single event has no
 *    duration to sort by).
 */
export function applyRawFilter(
  rows: readonly RawEventRow[],
  filter: TimelineFilterState,
  haystack: Haystack,
  slowMs: number = SLOW_TOOL_MS,
): RawEventRow[] {
  const q = filter.query.trim().toLowerCase();
  // To compute a model request's duration, we must first find the start row by id.
  const byId = new Map(rows.map((r) => [r.key, r]));

  const out = rows.filter((r) => {
    if (!filter.categories.has(r.category)) return false;
    if (!filter.domains.has(domainOf(r.type))) return false;
    if (filter.types.size && !filter.types.has(r.type)) return false;
    if (filter.errorsOnly && !isFailed(r)) return false;
    if (filter.slowOnly) {
      // Anything whose duration cannot be computed does not match (rather than passing as "not
      // slow"): under the event view this option
      // can only be computed for model requests; see point 2 in the function header comment.
      const d = modelRequestDurationMs(r, byId);
      if (d == null || d <= slowMs) return false;
    }
    if (q && !(haystack.get(r.key) ?? "").includes(q)) return false;
    if (filter.threadId && r.threadId !== filter.threadId) return false;
    return true;
  });

  return sortRawRows(out);
}
