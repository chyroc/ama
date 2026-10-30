/**
 * Turns the raw event stream into renderable timeline entries.
 *
 * Laying out all 33 event kinds flat is unreadable, so this does three things:
 * 1. Pairs tool_use and tool_result by id into one "tool call": Ark splits it into two events,
 *    but in the user's mind it is one thing; after pairing there are a third fewer entries and it
 *    is easier to follow;
 * 2. Gives each event kind a visual identity (icon + semantic color + title + summary);
 * 3. Totals token usage and evaluation rounds, for the summary at the top of the detail page.
 */

import {
  categoryOf,
  contentToText,
  splitHarnessBlocks,
  type ContentBlock,
  type EventCategory,
  type RunEvent,
  type RunEventType,
  type StopReason,
} from "@/types/events";
import type { TokenUsage } from "@/types/domain";
import { domainOf, SLOW_TOOL_MS, type EventDomain } from "@/lib/eventFacets";
import { prettyJson, truncate } from "@/lib/format";
import type { TFunc } from "@/prefs/PrefsContext";

/** Semantic color. conversation is neutral, errors are danger, and tool failures must be able to turn red too. */
export type Tone = "default" | "muted" | "accent" | "success" | "warning" | "danger";

export interface TimelineItem {
  /** Stable key. Paired entries use the tool_use event id. */
  key: string;
  category: EventCategory;
  /**
   * Concrete type of the main event (paired entries take the tool_use side).
   *
   * Used for "filter by domain" and "filter by concrete type"; the domain is derived from its
   * prefix (`lib/eventFacets.ts`).
   * Stored redundantly rather than read from `raw[0].type` each time: filtering is a hot path run
   * on every keystroke,
   * and whether raw's first item is the main event is something only buildTimeline knows.
   */
  type: RunEventType;
  icon: string;
  tone: Tone;
  /** Entry title, e.g. "user message", "call read". */
  title: string;
  /** One-line summary, shown when the list is collapsed. */
  summary: string;
  /** Body, shown when expanded; conversation entries are expanded by default. */
  body?: string;
  /**
   * Platform-injected `<harness:*>` blocks; only `user.message` may have them.
   *
   * A separate field rather than left in `body`: it is structured facts **for the model**
   * (Feishu's chat_id / thread_id / the command to fetch history), pure noise to users in a chat
   * bubble.
   * The UI shows it as a collapsed-by-default section labeled "injected by platform"; **do not
   * write "system prompt"**,
   * which would make people think it is the agent's persona.
   */
  injected?: string[];
  /** Whether the body renders as monospace code. */
  bodyIsCode?: boolean;
  /**
   * The command line in the tool input (`input.command`); only tools like bash have it.
   *
   * It is pulled out separately so the UI can put it before the input JSON: when debugging a bash
   * call,
   * the command line is the only thing you want to see first, and buried in braces you must read
   * through a layer of structure.
   */
  command?: string;
  threadId: string | null;
  /** Time for sorting and display. processed_at null means not processed yet. */
  processedAt: string | null;
  sequence: number;
  /** Conversation and error entries are expanded by default; the rest collapsed. */
  defaultExpanded: boolean;
  /**
   * Derived duration of a tool call: the receipt event's processed_at minus the call event's.
   *
   * ⚠ **This is not the command's execution time.** Ark gives no duration field; all we have is
   * the events'
   * processed_at (in seconds), so this difference includes queueing, and also the 16~29 seconds of
   * resuming a container from pause
   * (`server/shared/infra/dockertools/provider.go`). It answers "how long this step made the user wait".
   *
   * undefined when processed_at of either end is null: that means the event is queued but not
   * processed,
   * **it cannot be computed, and must not be 0** (0 would bubble it to the top in a duration sort,
   * looking like the fastest step).
   */
  durationMs?: number;
  /** The result part of a paired tool call. */
  result?: {
    text: string;
    isError: boolean;
    /** The result has not come back yet (the run is still in progress). */
    pending: boolean;
    /** processed_at of the receipt event, used to derive duration and show "receipt time". */
    processedAt: string | null;
  };
  /** The raw events this entry covers, for "view raw payload". */
  raw: RunEvent[];
  /** State when the tool call needs user confirmation. */
  permission?: "allow" | "ask" | "deny";
  confirmation?: { result: "allow" | "deny"; denyMessage?: string };
  /**
   * This tool call is still **running in our own container** (Ark has handed control back to AMA).
   *
   * ⚠ **`requires_action` does not mean "waiting for the user".** Under the self_hosted architecture
   * Ark only does model inference;
   * tool calls go into the work queue, are picked up by the long-running worker in amad and sent to
   * the session's Docker container to run
   * (`server/core/worker.go`). So for **every tool call** Ark always first emits `tool_use`,
   * then goes idle with `stop_reason: requires_action`; that is the mechanism detail of "I have
   * handed control back,
   * waiting for you to return `user.tool_result`", and **nobody is waiting for the user to click
   * anything**
   * (the backend comments it the same way everywhere: `server/feishu/resume.go:302`,
   * `server/core/worker.go:20`).
   *
   * Rendering it as a separate event called "idle / waiting for user confirmation" would show users,
   * during exactly the time the tool runs longest,
   * "it has stopped, and I need to click something"; neither is true.
   *
   * There is only one check for really waiting for a user click: `permission === "ask"` and
   * `confirmation` has not arrived.
   * The two paths must not be mixed: the former is in progress, the latter is blocked.
   */
  executing?: boolean;
}

/** stop_reason → translation key. This is a pure module; text comes from the t passed in by the caller. */
const STOP_REASON_KEYS: Record<StopReason["type"], string> = {
  end_turn: "tl.stopEndTurn",
  // See TimelineItem.executing: requires_action is "Ark waiting for the tool result to come back",
  // **not** "waiting for user confirmation". This key was once translated as "waiting for user
  // confirmation", which was wrong.
  requires_action: "tl.stopRequiresAction",
  retries_exhausted: "tl.stopRetriesExhausted",
  user_interrupt: "tl.stopUserInterrupt",
};

function stopReasonText(sr: StopReason | undefined, t: TFunc): string {
  if (!sr) return "";
  const key = STOP_REASON_KEYS[sr.type];
  const label = key ? t(key as Parameters<TFunc>[0]) : sr.type;
  if (sr.type === "requires_action" && sr.event_ids?.length) {
    return label + t("tl.stopPending", { n: sr.event_ids.length });
  }
  return label;
}

/** Tool name → icon. Each built-in tool gets an icon you can recognize at a glance. */
const TOOL_ICONS: Record<string, string> = {
  bash: "gravity-ui:terminal",
  read: "gravity-ui:file-text",
  write: "gravity-ui:pencil-to-line",
  edit: "gravity-ui:pencil-to-square",
  glob: "gravity-ui:magnifier",
  grep: "gravity-ui:magnifier",
  web_fetch: "gravity-ui:globe",
  web_search: "gravity-ui:magnifier",
};

function toolIcon(name: string): string {
  return TOOL_ICONS[name] ?? "gravity-ui:wrench";
}

/**
 * Turn the input into an object.
 *
 * The Ark docs say `input` is a "JSON string", but real Ark often sends it already as an object.
 * Both must be handled, and when a string cannot be parsed return null rather than throw: the
 * display layer should not blank the page because of one bad row.
 */
function inputAsObject(rawInput: unknown): Record<string, unknown> | null {
  if (typeof rawInput === "string") {
    try {
      const parsed = JSON.parse(rawInput) as unknown;
      return parsed && typeof parsed === "object" ? (parsed as Record<string, unknown>) : null;
    } catch {
      return null;
    }
  }
  return rawInput && typeof rawInput === "object"
    ? (rawInput as Record<string, unknown>)
    : null;
}

/** The command line in the input. Only tools like bash have `command`; the rest return undefined. */
function toolCommand(rawInput: unknown): string | undefined {
  const obj = inputAsObject(rawInput);
  const cmd = obj?.["command"];
  return typeof cmd === "string" && cmd ? cmd : undefined;
}

/** One-line summary of tool input: pick the most informative field first, rather than cramming in the whole JSON. */
/**
 * One-line summary.
 *
 * Takes `unknown`: the Ark docs say `input` is a "JSON string", but what actually comes is often
 * already an object
 * (real Ark sends an object in practice). So branch on type first and then read those common keys;
 * do not JSON.parse first.
 */
function toolInputSummary(rawInput: unknown): string {
  const obj = inputAsObject(rawInput);

  if (obj) {
    for (const k of ["command", "sql", "file_path", "path", "pattern", "query", "url"]) {
      const v = obj[k];
      if (typeof v === "string" && v) return truncate(v, 100);
    }
    return truncate(JSON.stringify(obj), 100);
  }
  return truncate(rawInput, 100);
}

function blocksText(content: ContentBlock[] | undefined, t: TFunc): string {
  return contentToText(content, t);
}

interface ToolCall {
  useEvent: Extract<
    RunEvent,
    { type: "agent.tool_use" | "agent.mcp_tool_use" | "agent.custom_tool_use" }
  >;
  resultEvent?: Extract<
    RunEvent,
    {
      type:
        | "agent.tool_result"
        | "agent.mcp_tool_result"
        | "user.tool_result"
        | "user.custom_tool_result";
    }
  >;
  confirmation?: Extract<RunEvent, { type: "user.tool_confirmation" }>;
  /**
   * The `requires_action` idle events Ark sends when it hands control back for this call.
   *
   * They **belong to this tool call** (`stop_reason.event_ids` points clearly at the use event),
   * so they are absorbed here rather than each becoming an "idle" entry. For why see
   * `TimelineItem.executing`.
   */
  idleEvents: RunEvent[];
}

/**
 * Whether an idle event is the "Ark hands control back, waiting for tool results" kind; if so,
 * gives the event ids it is waiting for.
 *
 * Returns an id array rather than boolean: a type guard can only narrow `e`'s type, not
 * the `stop_reason` level (`StopReason` is a discriminated union, and `event_ids` is only on the
 * requires_action
 * branch), so callers with the narrowed `e` still could not read `event_ids`.
 */
function requiresActionEventIds(e: RunEvent): string[] | null {
  if (e.type !== "session.status_idle" && e.type !== "session.thread_status_idle") return null;
  if (e.stop_reason?.type !== "requires_action") return null;
  return e.stop_reason.event_ids ?? [];
}

/**
 * Build the pairing index for tool calls.
 *
 * The link fields differ: MCP result events use `mcp_tool_use_id`, and custom tool results use
 * `custom_tool_use_id`, pointing at the use event's `id`.
 *
 * **The use event side cannot rely only on `tool_use_id`.** The Ark docs say `agent.tool_use`
 * carries
 * `tool_use_id`, but in practice real Ark **never sends this field**: the call id sits in the
 * event's own `id`
 * (like `call_2k985r...`, and the result event's `tool_use_id` points exactly at it).
 * So `calls.set(e.tool_use_id, ...)` all landed on the single key `undefined`:
 *   - several tool calls in one turn overwrote each other, leaving only the last;
 *   - three tool rows got the same React key (`use.id` taken from the one left after overwriting),
 *     the console filled with "two children with the same key", and render order was unstable;
 *   - results never paired, and tool rows were empty when opened.
 * So here it is `tool_use_id ?? id`, accepting both shapes.
 */
function indexToolCalls(events: RunEvent[]): Map<string, ToolCall> {
  const calls = new Map<string, ToolCall>();

  for (const e of events) {
    if (e.type === "agent.tool_use" || e.type === "agent.mcp_tool_use") {
      calls.set(e.tool_use_id ?? e.id, { useEvent: e, idleEvents: [] });
    } else if (e.type === "agent.custom_tool_use") {
      // custom tools have no separate tool_use_id; on return they link by the event's own id
      calls.set(e.id, { useEvent: e, idleEvents: [] });
    }
  }

  // use event id → its key in the map. `stop_reason.event_ids` points at **event ids**,
  // while the map key may be tool_use_id; the two are not equal under real Ark (see the comment
  // above).
  const keyByEventId = new Map<string, string>();
  for (const [key, call] of calls) keyByEventId.set(call.useEvent.id, key);

  for (const e of events) {
    if (e.type === "agent.tool_result" || e.type === "user.tool_result") {
      const c = calls.get(e.tool_use_id);
      if (c) c.resultEvent = e;
    } else if (e.type === "agent.mcp_tool_result") {
      const c = calls.get(e.mcp_tool_use_id);
      if (c) c.resultEvent = e;
    } else if (e.type === "user.custom_tool_result") {
      const c = calls.get(e.custom_tool_use_id);
      if (c) c.resultEvent = e;
    } else if (e.type === "user.tool_confirmation") {
      const c = calls.get(e.tool_use_id);
      if (c) c.confirmation = e;
    } else {
      // `event_ids` points at the use events waiting for a return; hang this idle on them.
      // Those that cannot be hung (event_ids empty, or pointing at events not fetched this time)
      // stay on the timeline as their own entry:
      // in that case we have nothing better to say, and swallowing them would lose information.
      for (const id of requiresActionEventIds(e) ?? []) {
        const key = keyByEventId.get(id);
        if (key) calls.get(key)?.idleEvents.push(e);
      }
    }
  }

  return calls;
}

/**
 * Idle events already absorbed by some tool call, which should not appear on the timeline on their
 * own.
 *
 * A Set rather than recomputing the check in `buildTimeline`: the check (whether `event_ids` can
 * hang on some
 * call) is known only to `indexToolCalls`; recomputing it would surely drift.
 */
function absorbedIdleIds(calls: Map<string, ToolCall>): Set<string> {
  const ids = new Set<string>();
  for (const call of calls.values()) {
    for (const e of call.idleEvents) ids.add(e.id);
  }
  return ids;
}

/** Event types already absorbed by pairing, which should not appear on the timeline on their own. */
const ABSORBED_TYPES = new Set<RunEventType>([
  "agent.tool_result",
  "agent.mcp_tool_result",
  "user.tool_result",
  "user.custom_tool_result",
  "user.tool_confirmation",
]);

function toolCallItem(call: ToolCall, t: TFunc): TimelineItem {
  const use = call.useEvent;
  const isMcp = use.type === "agent.mcp_tool_use";
  const isCustom = use.type === "agent.custom_tool_use";

  const resultText = call.resultEvent ? blocksText(call.resultEvent.content, t) : "";
  const isError = Boolean(call.resultEvent?.is_error);
  const denied = call.confirmation?.result === "deny";
  // The check for really waiting for a user click. **Only this one**; do not use requires_action
  // in its place.
  const awaitingUser = use.evaluated_permission === "ask" && !call.confirmation;
  /**
   * Still running: Ark handed control back (there is a requires_action idle), the result has not
   * come back,
   * and it is not waiting for user approval. Only with all three is it "our container is running
   * this command".
   *
   * No idle event may also mean it simply has not arrived yet (the run_4 shape), so the other check
   * is that
   * both "idle but no result" and "no idle and no result" count as in progress; pending covers it.
   */
  const executing = !call.resultEvent && !awaitingUser;

  // input on the wire may be a JSON string or already an object (the Ark docs say string,
  // in practice it is an object). prettyJson handles both, so custom and non-custom need no
  // separate branches.
  const inputForDisplay = prettyJson(use.input);

  let title: string;
  if (isMcp) title = t("tl.callMcp", { server: use.mcp_server_name, name: use.name });
  else if (isCustom) title = t("tl.callCustom", { name: use.name });
  else title = t("tl.callTool", { name: use.name });

  let tone: Tone = "default";
  if (isError) tone = "danger";
  else if (denied) tone = "warning";
  else if (call.resultEvent) tone = "success";
  else if (awaitingUser) tone = "warning";

  /**
   * Derived duration. Both ends need processed_at to compute it; null means queued but not
   * processed,
   * and then it can only be undefined; see the comment on TimelineItem.durationMs.
   *
   * **Calls in progress deliberately get no "elapsed time".** The only extra timestamp at hand is
   * the idle's
   * `processed_at`, but that is the moment "Ark handed control back" (only 1 second after tool_use
   * in the seed),
   * and using it as elapsed time would show "1.0s" on a command that has run for minutes. Subtracting
   * from wall-clock now does not work either:
   * viewing history from three days ago gives "3 days". **If it cannot be computed, do not show
   * it**; the same rule as "durationMs must not be 0".
   * Running is expressed separately by `executing`.
   */
  const usedAt = use.processed_at ? Date.parse(use.processed_at) : NaN;
  const doneAt = call.resultEvent?.processed_at
    ? Date.parse(call.resultEvent.processed_at)
    : NaN;
  const durationMs =
    Number.isFinite(usedAt) && Number.isFinite(doneAt) && doneAt >= usedAt
      ? doneAt - usedAt
      : undefined;

  return {
    key: use.id,
    category: "tool",
    type: use.type,
    icon: isMcp ? "gravity-ui:plug-connection" : toolIcon(use.name),
    tone,
    title,
    summary: toolInputSummary(use.input),
    body: inputForDisplay,
    bodyIsCode: true,
    command: toolCommand(use.input),
    threadId: use.session_thread_id,
    processedAt: use.processed_at,
    sequence: use.sequence,
    defaultExpanded: false,
    durationMs,
    result: call.resultEvent
      ? {
          text: resultText,
          isError,
          pending: false,
          processedAt: call.resultEvent.processed_at,
        }
      : { text: "", isError: false, pending: true, processedAt: null },
    permission: use.evaluated_permission,
    confirmation: call.confirmation
      ? { result: call.confirmation.result, denyMessage: call.confirmation.deny_message }
      : undefined,
    executing,
    // Absorbed idles also go into raw: they no longer show as their own entry, but "view raw
    // payload" must still
    // show them, otherwise we would be hiding events Ark really sent.
    raw: [
      use,
      ...call.idleEvents,
      ...(call.confirmation ? [call.confirmation] : []),
      ...(call.resultEvent ? [call.resultEvent] : []),
    ],
  };
}

/** A single non-tool event → timeline entry. */
function simpleItem(e: RunEvent, t: TFunc): TimelineItem {
  const base = {
    key: e.id,
    category: categoryOf(e.type),
    type: e.type,
    threadId: e.session_thread_id,
    processedAt: e.processed_at,
    sequence: e.sequence,
    raw: [e],
  };

  switch (e.type) {
    case "user.message": {
      // Platform-injected blocks (Feishu messages carry <harness:lark>) are pulled out of the body
      // and rendered separately,
      // otherwise the chat bubble would show a big block of XML meant for the model. See
      // splitHarnessBlocks.
      const { text, injected } = splitHarnessBlocks(e.content, t);
      return {
        ...base,
        icon: "gravity-ui:person",
        tone: "accent",
        title: t("tl.userMessage"),
        summary: truncate(text, 120),
        body: text,
        injected,
        defaultExpanded: true,
      };
    }
    case "system.message":
      return {
        ...base,
        icon: "gravity-ui:gear",
        tone: "muted",
        title: t("tl.systemPrompt"),
        summary: truncate(blocksText(e.content, t), 120),
        body: blocksText(e.content, t),
        defaultExpanded: false,
      };
    case "agent.message":
      return {
        ...base,
        icon: "gravity-ui:face-robot",
        tone: "default",
        title: t("tl.agentReply"),
        summary: truncate(blocksText(e.content, t), 120),
        body: blocksText(e.content, t),
        defaultExpanded: true,
      };
    case "agent.thinking": {
      // **In practice Ark does send thinking text**: the official docs' line "no concrete thinking
      // content is returned" is wrong;
      // see the comment on AgentThinkingEvent in types/events.ts (with measured data).
      //
      // But a few events still have empty content (measured: 3 of 72); then fall back to a one-line
      // "thinking" signal: **do not leave an empty body area**, otherwise the UI shows a collapsible
      // bar with nothing inside,
      // which is more confusing than not showing it.
      const text = blocksText(e.content, t);
      return {
        ...base,
        icon: "gravity-ui:sparkles",
        tone: "muted",
        title: text ? t("tl.thoughtDone") : t("tl.thinking"),
        summary: text ? truncate(text, 120) : t("tl.thinkingSummary"),
        // Empty strings become undefined: the render layer always checks `item.body ?` for a body.
        body: text || undefined,
        defaultExpanded: false,
        // **Deliberately no durationMs (the number "thought for N seconds" cannot be computed).**
        // In practice Ark stamps agent.thinking and agent.message with the same processed_at
        // (measured in one turn: 39.802783696 vs 39.802800, tens of microseconds apart), i.e. the
        // thinking event's
        // timestamp is "the whole model request ended" rather than "thinking ended". Subtracting
        // model_request_start from it
        // gives the whole model request duration (including generating the answer); labeling it
        // "thought 5.8s" would be making up a number.
        // Ark has no field that times thinking on its own, so no duration is shown here.
      };
    }
    case "user.interrupt":
      return {
        ...base,
        icon: "gravity-ui:xmark",
        tone: "warning",
        title: t("tl.userInterrupt"),
        summary: e.reason ?? t("tl.userInterruptSummary"),
        defaultExpanded: false,
      };
    case "agent.thread_message_sent":
      return {
        ...base,
        icon: "gravity-ui:arrow-right-from-line",
        tone: "muted",
        title: t("tl.sentTo", { name: e.to_agent_name ?? t("tl.mainThread") }),
        summary: truncate(blocksText(e.content, t), 120),
        body: blocksText(e.content, t),
        defaultExpanded: false,
      };
    case "agent.thread_message_received":
      return {
        ...base,
        icon: "gravity-ui:arrow-right-from-line",
        tone: "muted",
        title: t("tl.receivedFrom", { name: e.from_agent_name ?? t("tl.mainThread") }),
        summary: truncate(blocksText(e.content, t), 120),
        body: blocksText(e.content, t),
        defaultExpanded: false,
      };
    case "agent.thread_context_compacted":
      return {
        ...base,
        icon: "gravity-ui:layers",
        tone: "muted",
        title: t("tl.compacted"),
        summary: e.summary ? truncate(e.summary, 120) : t("tl.compactedSummary"),
        body: e.summary,
        defaultExpanded: false,
      };
    case "session.thread_created":
      return {
        ...base,
        icon: "gravity-ui:branches-right",
        tone: "muted",
        title: t("tl.threadCreated"),
        summary: e.agent_name + (e.parent_thread_id ? t("tl.threadSub") : t("tl.threadMain")),
        defaultExpanded: false,
      };
    case "session.thread_status_running":
      return { ...base, icon: "gravity-ui:play", tone: "muted", title: t("tl.threadRunning"), summary: e.agent_name, defaultExpanded: false };
    case "session.thread_status_idle":
      return {
        ...base,
        icon: "gravity-ui:clock",
        // requires_action reaching here means it **could not hang on any tool call**
        // (event_ids empty, or pointing at events not fetched this time). It is still not
        // "waiting for the user",
        // so say it as it is, "handed control back, waiting for tool results", with a neutral
        // semantic color.
        tone: "muted",
        title: e.stop_reason?.type === "requires_action" ? t("tl.threadHandoff") : t("tl.threadIdle"),
        summary: `${e.agent_name} · ${stopReasonText(e.stop_reason, t)}`,
        defaultExpanded: false,
      };
    case "session.thread_status_rescheduled":
      return { ...base, icon: "gravity-ui:arrow-rotate-left", tone: "warning", title: t("tl.threadRescheduled"), summary: e.agent_name, defaultExpanded: false };
    case "session.thread_status_terminated":
      return { ...base, icon: "gravity-ui:circle-xmark", tone: "muted", title: t("tl.threadTerminated"), summary: e.agent_name, defaultExpanded: false };
    case "session.status_running":
      return { ...base, icon: "gravity-ui:play", tone: "muted", title: t("tl.sessionRunning"), summary: "", defaultExpanded: false };
    case "session.status_idle":
      return {
        ...base,
        icon: "gravity-ui:clock",
        // Same as thread_status_idle: a requires_action reaching here is the kind that cannot hang
        // on a call,
        // and it is still not "waiting for the user".
        tone: "muted",
        title: e.stop_reason?.type === "requires_action" ? t("tl.sessionHandoff") : t("tl.sessionIdle"),
        summary: stopReasonText(e.stop_reason, t),
        defaultExpanded: false,
      };
    case "session.status_rescheduled":
      return { ...base, icon: "gravity-ui:arrow-rotate-left", tone: "warning", title: t("tl.sessionRescheduled"), summary: t("tl.sessionRescheduledSummary"), defaultExpanded: false };
    case "session.status_terminated":
      return {
        ...base,
        icon: "gravity-ui:circle-xmark",
        tone: "muted",
        title: t("tl.sessionTerminated"),
        summary: stopReasonText(e.stop_reason, t),
        defaultExpanded: false,
      };
    case "session.updated":
      return { ...base, icon: "gravity-ui:pencil-to-square", tone: "muted", title: t("tl.sessionRenamed"), summary: e.name, defaultExpanded: false };
    case "session.deleted":
      return { ...base, icon: "gravity-ui:trash-bin", tone: "muted", title: t("tl.sessionDeleted"), summary: t("tl.sessionDeletedSummary"), defaultExpanded: false };
    case "session.error": {
      const retry = e.error.retry_status?.type;
      // Receiving an error does not mean the run failed: recoverable errors are rescheduled
      // automatically and keep running, so the retry status must be shown
      const retryText =
        retry === "retrying"
          ? t("tl.retrying")
          : retry === "retries_exhausted"
            ? t("tl.retriesExhausted")
            : retry === "terminal"
              ? t("tl.terminal")
              : "";
      return {
        ...base,
        icon: "gravity-ui:triangle-exclamation",
        tone: retry === "retrying" ? "warning" : "danger",
        title: e.error.type,
        summary: retryText ? `${e.error.message} · ${retryText}` : e.error.message,
        body: [
          e.error.message,
          e.error.mcp_server_name ? t("tl.mcpServer", { name: e.error.mcp_server_name }) : "",
          e.error.vault_id ? t("tl.vault", { id: e.error.vault_id }) : "",
          e.error.credential_id ? t("tl.credential", { id: e.error.credential_id }) : "",
          retryText ? t("tl.retryStatus", { status: retryText }) : "",
        ]
          .filter(Boolean)
          .join("\n"),
        defaultExpanded: true,
      };
    }
    case "span.model_request_start":
      return { ...base, icon: "gravity-ui:cpu", tone: "muted", title: t("tl.modelStart"), summary: "", defaultExpanded: false };
    case "span.model_request_end": {
      const u = e.model_usage;
      return {
        ...base,
        icon: "gravity-ui:cpu",
        tone: e.is_error ? "danger" : "muted",
        title: t("tl.modelEnd"),
        summary: u
          ? t("tl.usageSummary", { input: u.input_tokens ?? 0, output: u.output_tokens ?? 0, cache: u.cache_read_input_tokens ?? 0 }) + (u.speed ? ` · ${u.speed}` : "")
          : (e.error_message ?? ""),
        defaultExpanded: false,
      };
    }
    case "user.define_outcome":
      return {
        ...base,
        icon: "gravity-ui:flask",
        tone: "accent",
        title: t("tl.defineOutcome"),
        summary: truncate(e.description, 120),
        body: [
          t("tl.outcomeGoal", { desc: e.description }),
          e.rubric.type === "text"
            ? t("tl.outcomeRubric", { content: e.rubric.content })
            : t("tl.outcomeRubricFile", { id: e.rubric.file_id }),
          e.max_iterations ? t("tl.outcomeMaxIterations", { n: e.max_iterations }) : "",
        ]
          .filter(Boolean)
          .join("\n\n"),
        defaultExpanded: false,
      };
    case "span.outcome_evaluation_start":
      return { ...base, icon: "gravity-ui:flask", tone: "muted", title: t("tl.outcomeStart", { n: e.iteration }), summary: "", defaultExpanded: false };
    case "span.outcome_evaluation_ongoing":
      return { ...base, icon: "gravity-ui:flask", tone: "muted", title: t("tl.outcomeOngoing"), summary: t("tl.round", { n: e.iteration }), defaultExpanded: false };
    case "span.outcome_evaluation_end": {
      const toneByResult: Record<string, Tone> = {
        satisfied: "success",
        needs_revision: "warning",
        max_iterations_reached: "warning",
        failed: "danger",
        interrupted: "muted",
      };
      const keyByResult: Record<string, string> = {
        satisfied: "tl.outcomeSatisfied",
        needs_revision: "tl.outcomeNeedsRevision",
        max_iterations_reached: "tl.outcomeMaxIter",
        failed: "tl.outcomeFailed",
        interrupted: "tl.outcomeInterrupted",
      };
      return {
        ...base,
        icon: "gravity-ui:flask",
        tone: toneByResult[e.result] ?? "muted",
        title: t("tl.outcomeEnd", {
          result: keyByResult[e.result] ? t(keyByResult[e.result] as Parameters<TFunc>[0]) : e.result,
        }),
        summary: e.explanation ? truncate(e.explanation, 120) : t("tl.round", { n: e.iteration }),
        body: e.explanation,
        defaultExpanded: e.result !== "satisfied",
      };
    }
    // Tool events are all handled in the pairing stage and never get here
    default:
      return {
        ...base,
        icon: "gravity-ui:circle-info",
        tone: "muted",
        title: (e as RunEvent).type,
        summary: "",
        defaultExpanded: false,
      };
  }
}

/** Turn the raw event stream into timeline entries. */
export function buildTimeline(events: RunEvent[], t: TFunc): TimelineItem[] {
  const calls = indexToolCalls(events);
  const absorbedIdles = absorbedIdleIds(calls);
  const items: TimelineItem[] = [];

  for (const e of events) {
    if (ABSORBED_TYPES.has(e.type)) continue;
    // requires_action idles are already merged into the tool call they belong to (shown as
    // "running"),
    // and no longer become a separate "idle" entry. See TimelineItem.executing.
    if (absorbedIdles.has(e.id)) continue;

    if (
      e.type === "agent.tool_use" ||
      e.type === "agent.mcp_tool_use" ||
      e.type === "agent.custom_tool_use"
    ) {
      // The key must be taken exactly as indexToolCalls does, otherwise the call cannot be found.
      // custom uses the event's own id; others prefer tool_use_id, falling back to id
      // (real Ark does not send tool_use_id; see the comment on indexToolCalls).
      const key = e.type === "agent.custom_tool_use" ? e.id : (e.tool_use_id ?? e.id);
      const call = calls.get(key);
      if (call) items.push(toolCallItem(call, t));
      continue;
    }

    items.push(simpleItem(e, t));
  }

  return items.sort((a, b) => a.sequence - b.sequence);
}

// ---------------------------------------------------------------------------
// Group by turn
// ---------------------------------------------------------------------------

/**
 * One segment within a turn.
 *
 * output and process alternate: in one turn the agent may first say "let me check", call a few
 * tools,
 * then give the final answer, which is three segments: output → process → output. **The middle
 * sentence is also an answer,
 * and must not be folded into the process** (seed run_1 has exactly this shape).
 */
export type TurnSegment =
  | { kind: "output"; item: TimelineItem }
  | { kind: "process"; items: TimelineItem[] };

export interface Turn {
  /** Stable key: the input event's id; the leading segment uses a fixed string. */
  key: string;
  /** The user's question. Events before the first user.message form their own segment, with no input. */
  input: TimelineItem | null;
  segments: TurnSegment[];
  /** Whether this turn has finished. When unfinished the UI expands the process; only when finished is it folded. */
  done: boolean;
}

/** Whether the entry is the user's message: the point where turns are split. */
export function isUserMessage(item: TimelineItem): boolean {
  return item.raw[0]?.type === "user.message";
}

/** Whether the entry counts as "output". Only agent.message does: system.message is a system prompt and belongs to the process. */
function isOutput(item: TimelineItem): boolean {
  return item.raw[0]?.type === "agent.message";
}

/**
 * Whether this event means "a turn really ended".
 *
 * **A `requires_action` idle does not count as an end.** On the Ark side it really is idle, but
 * under self_hosted
 * it only means "I handed control back, waiting for your worker to return `user.tool_result`";
 * **the tool is still running in our
 * container**, and this turn is far from over (the backend judges it the same way:
 * `server/core/scheduler.go:602`,
 * `server/feishu/inbound.go:931`).
 *
 * Treating it as an end has two consequences, both hard to debug: first, the tool call still
 * running gets folded, while it is exactly
 * the only thing the user wants to see right now (seed run_5 has this shape); second, the real
 * `end_turn` later
 * would be counted as "the next turn", cutting one conversation turn in half.
 */
function endsTurn(item: TimelineItem): boolean {
  const e = item.raw[0];
  if (!e) return false;
  if (e.type === "session.status_terminated") return true;
  if (e.type !== "session.status_idle") return false;
  return e.stop_reason?.type !== "requires_action";
}

/**
 * Whether the run's status means "this run will not produce more events".
 *
 * The values match `domain.RunStatus`, and the check matches `isTerminalRunStatus` in the
 * backend's `feishu/resume.go`
 * word for word (it also excludes `requires_action` from terminal states).
 *
 * ⚠️ **`requires_action` is not terminal**: the agent is stuck waiting for tool results, and the
 * worker will keep pushing it forward.
 * The same rule as excluding `requires_action` in `endsTurn()`.
 */
function isTerminalRunStatus(status: string | undefined): boolean {
  return status === "idle" || status === "terminated";
}

/**
 * Group timeline entries by turn.
 *
 * Ark has no turn concept (the envelope has no `turn_id`), so this is a **frontend-derived**
 * structure:
 * split at `user.message`, and within a segment alternate output / process by "is it the agent's
 * answer".
 *
 * The input must be the output of `buildTimeline()` (sorted by ascending sequence, tools paired).
 *
 * # `runStatus` is the fallback for the last turn, not an optional nicety
 *
 * Looking only at events, the **only** check for "this turn ended" is that `session.status_idle`
 * (end_turn). And it **may never reach the frontend**; that is not hypothetical, there are two real
 * gaps along the path:
 *
 *   1. **Backfill only happens while `running`** (`ListRunEvents` in `server/core/ark.go`).
 *      When SSE drops during tool execution, this run's status in the database is
 *      `requires_action` (see
 *      `applyEventToRun` in `chat.go`), so that gate blocks the backfill outright,
 *      and the tail events can never be filled in. ⚠️ Note that elsewhere "which states count as
 *      alive" is
 *      the three `running/rescheduling/requires_action` (`ListActiveRuns`);
 *      **only backfill here accepts just running**, and that mismatch is exactly this hole.
 *   2. **`retireMissingRun` only changes status and appends no event** (`server/core/runwatcher.go`).
 *      After the Ark session is reclaimed the run is set to `terminated`, but **no
 *      `session.status_terminated` event is stored**, and the event stream is forever missing that
 *      ending.
 *
 * In both cases `endsTurn()` returns false for every event, the last turn's `done` stays false,
 * and the design "after a turn ends only keep input and output" **fails completely for these
 * sessions**: the process stays expanded forever.
 * Zero errors, the page is just wrong.
 *
 * So the last turn has one more check: **if the run itself is already terminal, treat this turn as
 * finished**.
 * The frontend should not leave the process expanded forever just because one event was missed.
 *
 * ⚠️ It must be right whether passed or not: the chat page cannot get (and should not get) a
 * terminal state **while streaming**,
 * so this parameter is optional, and without it the behavior is exactly as before.
 */
export function groupTurns(items: TimelineItem[], runStatus?: string): Turn[] {
  const turns: Turn[] = [];
  let current: Turn | null = null;

  const push = (item: TimelineItem) => {
    if (!current) {
      // Events before the first user message (status_running / thread_created and the like) form a
      // leading segment.
      current = { key: "turn-preamble", input: null, segments: [], done: false };
      turns.push(current);
    }
    if (isOutput(item)) {
      current.segments.push({ kind: "output", item });
      return;
    }
    // Consecutive non-output entries merge into the same process segment.
    const last = current.segments[current.segments.length - 1];
    if (last?.kind === "process") last.items.push(item);
    else current.segments.push({ kind: "process", items: [item] });
  };

  for (const item of items) {
    if (isUserMessage(item)) {
      current = { key: item.key, input: item, segments: [], done: false };
      turns.push(current);
      continue;
    }
    if (endsTurn(item)) {
      // The ending event itself carries no information; it only marks the end and goes into no
      // segment.
      if (current) current.done = true;
      else push(item);
      continue;
    }
    push(item);
  }

  // There is another turn after it, so the previous turn ended long ago (even without an explicit
  // idle event).
  for (let i = 0; i < turns.length - 1; i++) turns[i].done = true;

  // Fallback for the last turn: the terminal event did not arrive, but the run itself is already
  // terminal; see the function header.
  const last = turns[turns.length - 1];
  if (last && !last.done && isTerminalRunStatus(runStatus)) last.done = true;

  return turns;
}

/** Summary of a process segment, for the one-line summary when folded. */
export interface ProcessStats {
  /** Number of steps. */
  steps: number;
  toolCalls: number;
  toolErrors: number;
  /** Difference between first and last processed_at in the segment; undefined if none are processed. */
  durationMs?: number;
}

export function processStats(items: TimelineItem[]): ProcessStats {
  let toolCalls = 0;
  let toolErrors = 0;
  const times: number[] = [];

  for (const it of items) {
    if (it.category === "tool") {
      toolCalls += 1;
      if (it.result?.isError) toolErrors += 1;
    }
    // processed_at null means queued but not processed; no time can be computed, so skip it rather
    // than use 0.
    if (it.processedAt) {
      const ms = Date.parse(it.processedAt);
      if (!Number.isNaN(ms)) times.push(ms);
    }
  }

  return {
    steps: items.length,
    toolCalls,
    toolErrors,
    durationMs: times.length >= 2 ? Math.max(...times) - Math.min(...times) : undefined,
  };
}

// ---------------------------------------------------------------------------
// Totals
// ---------------------------------------------------------------------------

export interface RunStats {
  usage: TokenUsage;
  /** Number of model requests. */
  modelRequests: number;
  /** Number of tool calls and how many of them failed. */
  toolCalls: number;
  toolErrors: number;
  /** Number of error events. */
  errors: number;
  /** Outcome evaluation rounds and the final result. */
  outcomeIterations: number;
  outcomeResult?: string;
  /** Entry count per category, for the counts on the filter. */
  byCategory: Record<EventCategory, number>;
  /** Entry count per domain. Counted by **entry**, not raw event: tools are paired into one. */
  byDomain: Record<EventDomain, number>;
  /**
   * Entry count per concrete type.
   *
   * Partial rather than filling in all 34 keys: the type filter only lists "types that really
   * appeared in this run",
   * and filling the absent ones with 0 would stuff the dropdown with options that are sure to be
   * empty.
   */
  byType: Partial<Record<RunEventType, number>>;
  /** Number of tool calls whose derived duration exceeds SLOW_TOOL_MS, for the "slow calls only" count. */
  slowToolCalls: number;
}

/**
 * Compute stats from the event stream.
 *
 * Token usage is summed from model_usage of span.model_request_end: this is the only place Ark
 * gives usage,
 * and the total on the Run is only a cache; following the events is more accurate.
 */
export function computeStats(events: RunEvent[], items: TimelineItem[]): RunStats {
  const usage: TokenUsage = {
    input_tokens: 0,
    output_tokens: 0,
    cache_read_input_tokens: 0,
    cache_creation_input_tokens: 0,
  };
  let modelRequests = 0;
  let errors = 0;
  let outcomeIterations = 0;
  let outcomeResult: string | undefined;

  for (const e of events) {
    if (e.type === "span.model_request_end") {
      modelRequests += 1;
      if (e.model_usage) {
        usage.input_tokens += e.model_usage.input_tokens ?? 0;
        usage.output_tokens += e.model_usage.output_tokens ?? 0;
        usage.cache_read_input_tokens += e.model_usage.cache_read_input_tokens ?? 0;
        usage.cache_creation_input_tokens += e.model_usage.cache_creation_input_tokens ?? 0;
      }
    } else if (e.type === "span.outcome_evaluation_end") {
      outcomeIterations = Math.max(outcomeIterations, e.iteration);
      outcomeResult = e.result;
      // Evaluation itself also burns tokens; count it in the total
      if (e.usage) {
        usage.input_tokens += e.usage.input_tokens ?? 0;
        usage.output_tokens += e.usage.output_tokens ?? 0;
        usage.cache_read_input_tokens += e.usage.cache_read_input_tokens ?? 0;
        usage.cache_creation_input_tokens += e.usage.cache_creation_input_tokens ?? 0;
      }
    } else if (e.type === "session.error") {
      errors += 1;
    }
  }

  const byCategory = {
    conversation: 0,
    thinking: 0,
    tool: 0,
    thread: 0,
    status: 0,
    trace: 0,
    error: 0,
    outcome: 0,
  } satisfies Record<EventCategory, number>;

  const byDomain = { user: 0, agent: 0, session: 0, span: 0 } satisfies Record<
    EventDomain,
    number
  >;
  const byType: Partial<Record<RunEventType, number>> = {};

  let toolCalls = 0;
  let toolErrors = 0;
  let slowToolCalls = 0;
  for (const it of items) {
    byCategory[it.category] += 1;
    byDomain[domainOf(it.type)] += 1;
    byType[it.type] = (byType[it.type] ?? 0) + 1;
    if (it.category === "tool") {
      toolCalls += 1;
      if (it.result?.isError) toolErrors += 1;
      // Those with durationMs undefined (no receipt yet, or missing processed_at) count as neither
      // slow nor fast:
      // it is simply unknown, and should not be counted on either side.
      if (it.durationMs != null && it.durationMs > SLOW_TOOL_MS) slowToolCalls += 1;
    }
  }

  return {
    usage,
    modelRequests,
    toolCalls,
    toolErrors,
    errors,
    outcomeIterations,
    outcomeResult,
    byCategory,
    byDomain,
    byType,
    slowToolCalls,
  };
}
