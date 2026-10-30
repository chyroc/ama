/**
 * Data layer of the event view: **one raw event per row, nothing merged, nothing paired**.
 *
 * # Why not reuse buildTimeline
 *
 * `buildTimeline()` in `lib/timeline.ts` **pairs** `agent.tool_use` with its result event
 * **into one entry** (and also absorbs the idle event of `requires_action`), so there are a third
 * fewer entries than
 * raw events. That is the right shape for the **conversation view**: in the user's mind one tool
 * call is one thing.
 *
 * But the event view means **a faithful raw view** (DESIGN.md 3.6.3: "show all 33 kinds"); when
 * debugging you must be able to
 * match the `run_events` records in the database one by one. Going through the processing
 * pipeline is itself a conceptual mismatch, so this **bypasses it**
 * and consumes `RunEvent[]` directly. The conversation view still uses buildTimeline, untouched.
 *
 * # Timestamps: only processed_at, and it has three traps
 *
 * Measured on 2439 real events in local PG (`raw::jsonb->>'processed_at'`; the copy in the `doc`
 * column is
 * **all null** on real Ark data):
 *
 * 1. **The number of decimals is not fixed**: 9 digits 2360 rows, 4 digits 76, 6 digits 2. You
 *    cannot cut the string at a fixed length.
 * 2. **Two time zone forms coexist**: `+08:00` 2362 rows, a literal `Z` 76. So **string comparison
 *    gives the
 *    wrong order**: the literals `2026-08-25T10:00:00.000Z` and `2026-08-27T21:58:33.878+08:00`
 *    are not comparable as strings. They must be parsed into absolute instants first.
 * 3. **Same-millisecond collisions are normal**: one run's 662 events had only 400 distinct
 *    milliseconds (40% collide),
 *    and JS `Date` only goes to milliseconds, so nanoseconds **are just dropped**. Two events 25
 *    microseconds apart measured
 *    exactly equal `getTime()`.
 *
 * So the sort is the lexicographic order of **(millisecond instant, sequence)**: first by absolute
 * instant, then `sequence`
 * within the same millisecond. `sequence` is Ark's total order within a session, and measured
 * against real instants it has **zero inversions** (0 of 662),
 * so it can be trusted. Sorting by sequence alone would also work, but then "the time is right"
 * would not really be expressed,
 * and the event view is read by time.
 *
 * Events with `processed_at` null (queued, not processed yet) always go last: their instant is
 * **unknown**,
 * not 0; treating it as 0 would bubble it to the top, looking like it happened first.
 */

import {
  categoryOf,
  contentToText,
  splitHarnessBlocks,
  type EventCategory,
  type RunEvent,
  type RunEventType,
  type StopReason,
} from "@/types/events";
import { truncate } from "@/lib/format";
import { domainOf, SLOW_TOOL_MS, type EventDomain } from "@/lib/eventFacets";
import type { TFunc } from "@/prefs/PrefsContext";
import type { Tone } from "@/lib/timeline";

/**
 * One row = one raw event.
 *
 * Deliberately **without** derived fields like body / result / durationMs: those are what
 * `TimelineItem` prepares for
 * "paired semantic entries". Here each row is one event; the detail area fetches the body by type
 * itself.
 */
export interface RawEventRow {
  /** The event's own id. Ark guarantees it is unique within a session, so it is used directly as the React key. */
  key: string;
  event: RunEvent;
  type: RunEventType;
  category: EventCategory;
  icon: string;
  tone: Tone;
  /** Human name of the type, e.g. "tool call". */
  label: string;
  /** One-line summary: the **most informative** sentence for the type. Different for each of the 34 kinds. */
  summary: string;
  threadId: string | null;
  processedAt: string | null;
  /** Millisecond instant parsed from processed_at; null means not processed (sorted last). */
  atMs: number | null;
  sequence: number;
}

/**
 * Parse `processed_at` into a millisecond instant.
 *
 * Hand it straight to `Date`: it understands the `+08:00` offset, and decimals longer than 3
 * digits (extra digits are truncated,
 * which is where "nanosecond precision loss" comes from; sequence covers it). If it cannot be
 * parsed (bad data), return null
 * rather than NaN: NaN always compares false, which would make the sort result depend on the
 * initial order.
 */
export function parseProcessedAt(iso: string | null | undefined): number | null {
  if (!iso) return null;
  const ms = new Date(iso).getTime();
  return Number.isNaN(ms) ? null : ms;
}

/**
 * Sort by real order of occurrence: lexicographic (instant, sequence).
 *
 * Unprocessed ones (atMs null) sink to the bottom, still sorted by sequence among themselves.
 * **Not an in-place sort**: the input may be an array SSE builds fresh every frame, but it may also
 * be referenced elsewhere.
 */
export function sortRawRows(rows: readonly RawEventRow[]): RawEventRow[] {
  return [...rows].sort((a, b) => {
    if (a.atMs !== b.atMs) {
      if (a.atMs === null) return 1;
      if (b.atMs === null) return -1;
      return a.atMs - b.atMs;
    }
    // Same millisecond (including both unprocessed): nanoseconds are already lost, so only Ark's
    // total order can decide.
    return a.sequence - b.sequence;
  });
}

/** Category → default icon and semantic color. Concrete types can override below. */
const CATEGORY_LOOK: Record<EventCategory, { icon: string; tone: Tone }> = {
  conversation: { icon: "gravity-ui:comment", tone: "default" },
  thinking: { icon: "gravity-ui:sparkles", tone: "accent" },
  tool: { icon: "gravity-ui:wrench", tone: "accent" },
  thread: { icon: "gravity-ui:branches-right", tone: "muted" },
  status: { icon: "gravity-ui:circle-info", tone: "muted" },
  trace: { icon: "gravity-ui:hourglass", tone: "muted" },
  error: { icon: "gravity-ui:circle-exclamation", tone: "danger" },
  outcome: { icon: "gravity-ui:shield-check", tone: "success" },
};

/**
 * Concrete type → icon/semantic color override.
 *
 * Only those where "differing from the category default carries information": user messages and
 * agent replies are both conversation
 * but go opposite directions, the two status_* must show success/failure at a glance, and tool
 * results must read as a receipt, not a start.
 */
const TYPE_LOOK: Partial<Record<RunEventType, { icon?: string; tone?: Tone }>> = {
  "user.message": { icon: "gravity-ui:person" },
  "system.message": { icon: "gravity-ui:gear", tone: "muted" },
  "agent.message": { icon: "gravity-ui:face-robot" },
  "user.interrupt": { icon: "gravity-ui:stop", tone: "warning" },

  "agent.tool_result": { icon: "gravity-ui:arrow-left", tone: "default" },
  "user.tool_result": { icon: "gravity-ui:arrow-left", tone: "default" },
  "user.custom_tool_result": { icon: "gravity-ui:arrow-left", tone: "default" },
  "agent.mcp_tool_result": { icon: "gravity-ui:arrow-left", tone: "default" },
  "agent.mcp_tool_use": { icon: "gravity-ui:plug-connection" },
  "agent.custom_tool_use": { icon: "gravity-ui:puzzle" },
  "user.tool_confirmation": { icon: "gravity-ui:shield-check", tone: "warning" },

  "session.status_running": { icon: "gravity-ui:play", tone: "success" },
  "session.status_idle": { icon: "gravity-ui:circle-pause" },
  "session.status_terminated": { icon: "gravity-ui:circle-xmark", tone: "muted" },
  "session.status_rescheduled": { icon: "gravity-ui:arrows-rotate-right", tone: "warning" },
  "session.deleted": { icon: "gravity-ui:trash-bin", tone: "muted" },
  "session.updated": { icon: "gravity-ui:pencil", tone: "muted" },
  "session.thread_status_running": { icon: "gravity-ui:play" },
  "session.thread_status_rescheduled": { icon: "gravity-ui:arrows-rotate-right", tone: "warning" },
  "session.thread_status_terminated": { icon: "gravity-ui:circle-xmark" },
  "agent.thread_message_sent": { icon: "gravity-ui:arrow-right-from-line" },
  "agent.thread_message_received": { icon: "gravity-ui:arrow-left" },
  "agent.thread_context_compacted": { icon: "gravity-ui:layers" },

  "span.model_request_start": { icon: "gravity-ui:cpu" },
  "span.model_request_end": { icon: "gravity-ui:cpu" },
  "user.define_outcome": { icon: "gravity-ui:list-ul", tone: "muted" },
};

/** stop_reason → translation key. Kept in sync with the one in lib/timeline.ts (same meaning). */
const STOP_REASON_KEYS: Record<StopReason["type"], string> = {
  end_turn: "tl.stopEndTurn",
  // ⚠ Under self_hosted, requires_action means "Ark hands control back and waits for our tool
  // result",
  // **not "waiting for user confirmation"**. See TimelineItem.executing in lib/timeline.ts.
  requires_action: "tl.stopRequiresAction",
  retries_exhausted: "tl.stopRetriesExhausted",
  user_interrupt: "tl.stopUserInterrupt",
};

export function stopReasonLabel(sr: StopReason | undefined, t: TFunc): string {
  if (!sr) return "";
  const key = STOP_REASON_KEYS[sr.type];
  return key ? t(key as Parameters<TFunc>[0]) : sr.type;
}

/**
 * One-line summary.
 *
 * **The 34 kinds must not look the same**: the list is for scanning at a glance, and a summary
 * that repeats the type name says nothing.
 * The rule is to pick, for each kind, "the one field you most want to see first when debugging":
 * messages pick the body, tools pick name and input,
 * status picks stop_reason, model requests pick token usage, threads pick the agent name.
 *
 * ⚠ Tool input **always goes through `truncate()`**: the Ark docs say `input` is a JSON string,
 * but in practice it is an object;
 * calling `.slice()` / `.replace()` directly would blank the whole page (truncate takes unknown,
 * handling both shapes).
 */
function summarize(e: RunEvent, t: TFunc): string {
  switch (e.type) {
    // ---- Conversation ----
    case "user.message":
      // The summary only takes what the user really said, **stripping the platform-injected
      // `<harness:*>` block**; otherwise every message from Feishu
      // would read in the list as "<harness:lark> below is the source of this Feishu message…",
      // and you could not tell one from another on a screen.
      //
      // ⚠️ **Only change the summary, not the detail panel**: the event view means "faithfully
      // show the raw
      // event Ark returned" (DESIGN.md 3.6.3) and must match run_events in the database one by
      // one, so the side panel's
      // content is still shown as is; the injected block is part of this event to begin with.
      return truncate(splitHarnessBlocks(e.content, t).text);
    case "agent.message":
    case "system.message":
      return truncate(contentToText(e.content, t));
    case "agent.thinking":
      // content is optional: if there is a body give the body; empty ones (measured: 3 of 72) fall
      // back to a signal word.
      return truncate(contentToText(e.content, t)) || t("evt.thinkingEmpty");
    case "user.interrupt":
      return e.reason ? truncate(e.reason) : t("evt.interruptNoReason");

    // ---- Tools ----
    case "agent.tool_use":
      return `${e.name} · ${truncate(e.input, 60)}`;
    case "agent.mcp_tool_use":
      return `${e.mcp_server_name}/${e.name} · ${truncate(e.input, 50)}`;
    case "agent.custom_tool_use":
      return `${e.name} · ${truncate(e.input, 60)}`;
    case "agent.tool_result":
    case "user.tool_result":
      return (
        (e.is_error ? `${t("timeline.execFailed")} · ` : "") + truncate(contentToText(e.content, t))
      );
    case "user.custom_tool_result":
    case "agent.mcp_tool_result":
      return (
        (e.is_error ? `${t("timeline.execFailed")} · ` : "") + truncate(contentToText(e.content, t))
      );
    case "user.tool_confirmation":
      return e.result === "allow"
        ? t("timeline.approved")
        : `${t("timeline.denied")}${e.deny_message ? ` · ${truncate(e.deny_message, 50)}` : ""}`;

    // ---- Threads ----
    case "session.thread_created":
      return e.parent_thread_id
        ? t("evt.threadCreatedSub", { name: e.agent_name })
        : t("evt.threadCreatedMain", { name: e.agent_name });
    case "session.thread_status_running":
    case "session.thread_status_rescheduled":
    case "session.thread_status_terminated":
      return e.agent_name;
    case "session.thread_status_idle":
      return [e.agent_name, stopReasonLabel(e.stop_reason, t)].filter(Boolean).join(" · ");
    case "agent.thread_message_sent":
      return `→ ${e.to_agent_name ?? t("evt.mainAgent")} · ${truncate(contentToText(e.content, t), 50)}`;
    case "agent.thread_message_received":
      return `← ${e.from_agent_name ?? t("evt.mainAgent")} · ${truncate(contentToText(e.content, t), 50)}`;
    case "agent.thread_context_compacted":
      return e.summary ? truncate(e.summary) : t("evt.compactedNoSummary");

    // ---- Session status ----
    case "session.status_running":
    case "session.status_rescheduled":
      return "";
    case "session.status_idle":
    case "session.status_terminated":
      return stopReasonLabel(e.stop_reason, t);
    case "session.updated":
      return e.name;
    case "session.deleted":
      return "";

    // ---- Errors ----
    case "session.error":
      return truncate(e.error.message);

    // ---- span ----
    case "span.model_request_start":
      return "";
    case "span.model_request_end": {
      if (e.is_error) return `${t("timeline.execFailed")}${e.error_message ? ` · ${truncate(e.error_message, 60)}` : ""}`;
      const u = e.model_usage;
      return u ? t("evt.usageBrief", { in: u.input_tokens ?? 0, out: u.output_tokens ?? 0 }) : "";
    }

    // ---- Outcome evaluation ----
    case "user.define_outcome":
      return truncate(e.description);
    case "span.outcome_evaluation_start":
    case "span.outcome_evaluation_ongoing":
      return t("evt.iteration", { n: e.iteration });
    case "span.outcome_evaluation_end":
      return [t("evt.iteration", { n: e.iteration }), e.result].join(" · ");
  }
  // When Ark adds a new type the frontend has not caught up with, show the type name rather than
  // blank:
  // the event view's job is "show the user what was received".
  return (e as RunEvent).type;
}

/**
 * Raw events → renderable rows. **Nothing is merged**; N in, N rows out.
 *
 * `t` is passed in by the caller (a pure module cannot call hooks; same as buildTimeline).
 */
export function buildRawRows(events: readonly RunEvent[], t: TFunc): RawEventRow[] {
  const rows = events.map<RawEventRow>((e) => {
    const category = categoryOf(e.type);
    const base = CATEGORY_LOOK[category];
    const over = TYPE_LOOK[e.type];
    return {
      key: e.id,
      event: e,
      type: e.type,
      category,
      icon: over?.icon ?? base.icon,
      tone: over?.tone ?? base.tone,
      label: eventTypeLabel(e.type, t),
      summary: summarize(e, t),
      threadId: e.session_thread_id,
      processedAt: e.processed_at,
      atMs: parseProcessedAt(e.processed_at),
      sequence: e.sequence,
    };
  });
  return sortRawRows(rows);
}

/**
 * Type → human name.
 *
 * The key rule is `evtType.<type name without dots>`, matching messages.ts one to one; when a
 * translation is missing it **falls back to the raw
 * type name** rather than an empty string: the event view would rather show `agent.foo` than blank.
 */
export function eventTypeLabel(type: RunEventType, t: TFunc): string {
  const key = `evtType.${type}` as Parameters<TFunc>[0];
  const label = t(key);
  // When t() does not recognize a key it returns the key itself (see PrefsContext); then fall back
  // to the raw type name.
  return label === key ? type : label;
}

/**
 * Counts on the filter, computed over **raw events**.
 *
 * Does not reuse `computeStats()`: its byCategory / byDomain / byType count **paired entries**
 * (its comment even says "tools are paired into one"). The event view's filter must use the same
 * basis as what it filters,
 * otherwise you get "the filter says tool has 8, clicking shows 15"; that inconsistency gives no
 * error,
 * it only makes people suspect the filter is broken.
 *
 * Totals like token usage are still given by `computeStats()` (that is a property of the whole run,
 * unrelated to the view),
 * so this only covers the three count axes + the counts for the slow/failure switches.
 */
export function rawEventStats(rows: readonly RawEventRow[]): RawEventStats {
  const byCategory: Record<EventCategory, number> = {
    conversation: 0,
    thinking: 0,
    tool: 0,
    thread: 0,
    status: 0,
    trace: 0,
    error: 0,
    outcome: 0,
  };
  const byDomain: Record<EventDomain, number> = { user: 0, agent: 0, session: 0, span: 0 };
  const byType: Partial<Record<RunEventType, number>> = {};

  const byId = new Map(rows.map((r) => [r.key, r]));
  let slow = 0;
  let failed = 0;

  for (const r of rows) {
    byCategory[r.category] += 1;
    byDomain[domainOf(r.type)] += 1;
    byType[r.type] = (byType[r.type] ?? 0) + 1;
    if (isFailed(r)) failed += 1;
    const d = modelRequestDurationMs(r, byId);
    if (d != null && d > SLOW_TOOL_MS) slow += 1;
  }

  return { total: rows.length, byCategory, byDomain, byType, slow, failed };
}

export interface RawEventStats {
  total: number;
  byCategory: Record<EventCategory, number>;
  byDomain: Record<EventDomain, number>;
/** Only types that really appeared in this run: the type filter should not list options that are sure to be empty. */
  byType: Partial<Record<RunEventType, number>>;
  slow: number;
  failed: number;
}

/** Whether this event is a "failure". A tool failure is not an event type; only is_error on the receipt tells. */
export function isFailed(r: RawEventRow): boolean {
  if (r.category === "error") return true;
  const e = r.event;
  return "is_error" in e && Boolean(e.is_error);
}

/**
 * The duration a single event can compute.
 *
 * **Only `span.model_request_end` can**: it carries `model_request_start_id`,
 * the only pair Ark gives an explicit link field for. Other events return undefined (unknown, not 0).
 */
export function modelRequestDurationMs(
  r: RawEventRow,
  byId: Map<string, RawEventRow>,
): number | undefined {
  if (r.event.type !== "span.model_request_end") return undefined;
  const start = byId.get(r.event.model_request_start_id);
  if (start?.atMs == null || r.atMs == null) return undefined;
  return r.atMs - start.atMs;
}
