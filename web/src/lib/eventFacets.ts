/**
 * Multi-axis filters for events.
 *
 * The Trace view must be able to narrow the event stream along three axes, all of which are
 * **categories that really exist in Ark**, not buckets we made up:
 *
 * 1. **Domain** (this file): Ark's `{domain}.{action}` naming convention, 4 values. The coarsest cut.
 *    See the Session event stream page under `docs/ark-managed-agents/guide/`.
 * 2. **Semantic category**: the 8 `EventCategory` values in `types/events.ts`, each with an icon +
 *    semantic color;
 *    AMA's own display grouping (DESIGN.md 3.6.2). This file does not touch it.
 * 3. **Concrete type**: the 34 `RunEventType` values. The finest cut.
 *
 * Domain and category **cross** each other rather than nest: one tool call spans the user and
 * agent domains (agent starts it,
 * the framework acknowledges it), while `session.status_*` and `session.thread_status_*` are both
 * in the session domain but belong to
 * the status and thread categories respectively. So both axes are needed; we cannot keep only one.
 *
 * Pure module: imports neither React nor PrefsContext (text is stored only as keys; see
 * FACET_PRESETS).
 */

import type { RunEventType } from "@/types/events";
import type { MessageKey } from "@/prefs/messages";

/** Ark's four event domains, taken from the prefix of `{domain}.{action}`. */
export type EventDomain = "user" | "agent" | "session" | "span";

/** Fixed iteration order (the order the Ark docs list them), so the filter does not jump around. */
export const EVENT_DOMAINS: readonly EventDomain[] = ["user", "agent", "session", "span"];

/**
 * Event type → domain.
 *
 * Mechanically taking the prefix is enough, **with only one exception**: `system.message` starts
 * with `system.`, but Ark states clearly
 * it belongs to the **User domain**. The rule is direction, not prefix: it is a dynamic system
 * prompt sent up by the client, and must
 * be sent in the same request as `user.message`. Classifying by prefix would invent a "system
 * domain",
 * while Ark only has four domains.
 */
export function domainOf(type: RunEventType): EventDomain {
  if (type === "system.message") return "user";
  const prefix = type.slice(0, type.indexOf("."));
  switch (prefix) {
    case "user":
    case "agent":
    case "session":
    case "span":
      return prefix;
    default:
      // When Ark adds a new domain and the frontend has not caught up, file it under session
      // (lifecycle) rather than throw:
      // the event view's job is "show the user what was received", and it should not crash the
      // whole page just because it does not recognize something.
      return "session";
  }
}

/**
 * Threshold for "slow".
 *
 * ⚠ This duration is **derived wall-clock time** (the ack event's processed_at minus the call
 * event's),
 * not the command's execution time: it includes queueing, and resuming a container from pause
 * takes 16~29 seconds
 * (see the state machine in `server/shared/infra/dockertools/provider.go`). So "over 5 seconds" means
 * **"this step made the user wait over 5 seconds"**; using it to find "where it got stuck" is
 * right, using it to argue "the command itself is slow" is wrong.
 * The UI must say this (`filter.slowOnlyHint`), otherwise it is misleading.
 */
export const SLOW_TOOL_MS = 5_000;

/*
 * There used to be `TimelineSort` and `TIMELINE_SORTS` ("sequence" | "duration") here.
 *
 * **Removed, because that sort option never worked**: the event view's `applyRawFilter` never
 * read
 * `filter.sort`, and switching to "duration descending" changed no order at all.
 *
 * And under "one event per row" it **should not exist**: a single event has no duration
 * (the only exception is `span.model_request_end`, which carries the `model_request_start_id`
 * given by Ark).
 * A dropdown that does nothing when switched is worse than none: it makes people think they
 * sorted wrong.
 */

export type FacetPresetId = "tool" | "model" | "error" | "system";

/**
 * Presets: map plain wishes like "I want to see tools/model/errors/system" onto the three real
 * axes above with one click.
 *
 * Store translation keys, not text: these are module-level constants, evaluated before the React
 * context exists, so `t()` cannot be called
 * (same as `STOP_REASON_KEYS` in `lib/timeline.ts`).
 *
 * Empty `types` means "category is enough for this preset" (e.g. tools: all 8 types belong to
 * category `tool`,
 * and filtering by category is more stable than listing 8 types; when Ark adds tool events later,
 * nothing here needs to change).
 */
export const FACET_PRESETS: readonly {
  id: FacetPresetId;
  labelKey: MessageKey;
  /** Key of the hint text. Only given when we need to say "what this preset does not cover". */
  hintKey?: MessageKey;
  /** Categories to show when selected. */
  categories: readonly string[];
  /** Concrete types to limit to when selected; an empty array means no limit. */
  types: readonly RunEventType[];
}[] = [
  {
    id: "tool",
    labelKey: "filter.presetTool",
    categories: ["tool"],
    types: [],
  },
  {
    id: "model",
    labelKey: "filter.presetModel",
    // Model inference has only these two events, and they belong to category `trace`, which is
    // hidden by default,
    // so this preset must also turn trace on (include it in categories), otherwise clicking does
    // nothing.
    categories: ["trace"],
    types: ["span.model_request_start", "span.model_request_end"],
  },
  {
    id: "error",
    labelKey: "filter.presetError",
    // Only the error category; a tool failure is not an event type (it is is_error on the result
    // event),
    // and is expressed by the "failures only" switch.
    categories: ["error"],
    types: [],
  },
  {
    id: "system",
    labelKey: "filter.presetSystem",
    hintKey: "filter.presetSystemHint",
    // State changes of sessions and threads. **Environment setup / container lifecycle is not here,
    // because they produce no events**:
    // container create/pause/resume only writes server logs (shared/infra/dockertools/provider.go).
    categories: ["status", "thread"],
    types: [],
  },
];
