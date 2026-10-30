/**
 * The session event model of Ark Managed Agents.
 *
 * 33 event kinds: 7 sent up by the client + 26 pushed by the server. Field names stay snake_case,
 * matching the Ark API,
 * so they can be passed through directly and checked against the docs under docs/ark-managed-agents/.
 */

import type { TFunc } from "@/prefs/PrefsContext";

// ---------------------------------------------------------------------------
// Content blocks
// ---------------------------------------------------------------------------

export interface TextBlock {
  type: "text";
  text: string;
}

export type ImageSource =
  | { type: "base64"; media_type: string; data: string }
  | { type: "url"; url: string }
  | { type: "file"; file_id: string };

export interface ImageBlock {
  type: "image";
  source: ImageSource;
}

export type DocumentSource =
  | { type: "base64"; media_type: string; data: string }
  | { type: "text"; media_type: "text/plain"; data: string }
  | { type: "url"; url: string }
  | { type: "file"; file_id: string };

export interface DocumentBlock {
  type: "document";
  source: DocumentSource;
  title?: string;
  context?: string;
}

export interface SearchResultBlock {
  type: "search_result";
  source: string;
  title: string;
  content: TextBlock[];
  citations: { enabled: boolean };
}

export type ContentBlock = TextBlock | ImageBlock | DocumentBlock | SearchResultBlock;

// ---------------------------------------------------------------------------
// Common envelope
// ---------------------------------------------------------------------------

export interface RunEventBase {
  id: string;
  /** Total order within a session. Only list endpoints have it; the SSE stream does not carry this field. */
  sequence: number;
  /** null means the event is queued but not processed yet. */
  processed_at: string | null;
  session_thread_id: string | null;
}

/** Evaluation result of the tool permission policy. */
export type EvaluatedPermission = "allow" | "ask" | "deny";

/**
 * Token usage of one model request.
 *
 * ⚠ **All four count fields are optional: in practice Ark omits those whose value is 0.**
 * In a production run with 24 model requests, 1 `model_usage` only gave
 * `{input_tokens, output_tokens, cache_read_input_tokens}`, without
 * `cache_creation_input_tokens` (that request really wrote no cache).
 *
 * Declaring them required costs us **totals turning into NaN**: in `usage.x += e.model_usage.x`,
 * once `undefined` comes in, the running total is NaN forever, and the page shows "written NaNM";
 * one event with a missing field can ruin the whole run's usage summary, with no error at all.
 * So accumulation always uses `?? 0` (see computeStats in `lib/timeline.ts`).
 */
export interface ModelUsage {
  input_tokens?: number;
  output_tokens?: number;
  cache_read_input_tokens?: number;
  cache_creation_input_tokens?: number;
  speed?: "standard" | "fast";
}

/** Token usage total of one run, summed from model_usage of every span.model_request_end. */
export interface TokenUsage {
  input_tokens: number;
  output_tokens: number;
  cache_read_input_tokens: number;
  cache_creation_input_tokens: number;
}

/** Why a session/thread went idle. requires_action carries the IDs of events waiting for confirmation. */
export type StopReason =
  | { type: "end_turn" }
  | { type: "requires_action"; event_ids: string[] }
  | { type: "retries_exhausted" }
  | { type: "user_interrupt" };

export type RetryStatus = { type: "retrying" | "retries_exhausted" | "terminal" };

export type SessionErrorType =
  | "model_overloaded_error"
  | "model_rate_limited_error"
  | "model_request_failed_error"
  | "mcp_connection_failed_error"
  | "mcp_auth_failed_error"
  | "billing_error"
  | "credential_unreachable_error"
  | "unknown_error";

export interface SessionErrorPayload {
  type: SessionErrorType;
  message: string;
  retry_status?: RetryStatus;
  /** Only for MCP errors. */
  mcp_server_name?: string;
  /** Only for credential errors. */
  vault_id?: string;
  credential_id?: string;
}

// ---------------------------------------------------------------------------
// user domain (sent up by the client, 7 kinds)
// ---------------------------------------------------------------------------

export interface UserMessageEvent extends RunEventBase {
  type: "user.message";
  content: ContentBlock[];
}

export interface SystemMessageEvent extends RunEventBase {
  type: "system.message";
  content: TextBlock[];
}

export interface UserInterruptEvent extends RunEventBase {
  type: "user.interrupt";
  reason?: string;
}

export interface UserToolConfirmationEvent extends RunEventBase {
  type: "user.tool_confirmation";
  result: "allow" | "deny";
  tool_use_id: string;
  deny_message?: string;
}

export interface UserToolResultEvent extends RunEventBase {
  type: "user.tool_result";
  tool_use_id: string;
  content: ContentBlock[];
  is_error?: boolean;
}

export interface UserCustomToolResultEvent extends RunEventBase {
  type: "user.custom_tool_result";
  custom_tool_use_id: string;
  content: ContentBlock[];
  is_error?: boolean;
}

export interface UserDefineOutcomeEvent extends RunEventBase {
  type: "user.define_outcome";
  outcome_id: string;
  description: string;
  rubric: { type: "text"; content: string } | { type: "file"; file_id: string };
  max_iterations?: number;
}

// ---------------------------------------------------------------------------
// agent domain (10 kinds)
// ---------------------------------------------------------------------------

export interface AgentMessageEvent extends RunEventBase {
  type: "agent.message";
  content: TextBlock[];
}

/**
 * The wire shape of tool input.
 *
 * The Ark docs (session event structure reference) say in three places
 * `input` is `string` / "a JSON string", but **real Ark actually sends an object**.
 * Both must be accepted, so this is a union type rather than `string`.
 *
 * Always pass it through `prettyJson()` (for the body) or `truncate()` (for a one-line summary);
 * those two functions take unknown and handle both shapes. Calling `.replace()` directly blanks
 * the page.
 */
export type ToolInput = string | Record<string, unknown>;

/**
 * The model's thinking process (chain of thought).
 *
 * ⚠ **`content` is optional, but in practice it is present most of the time: the Ark docs are
 * wrong on this point.**
 * The official docs (session event structure reference) only list 4 fields,
 * and say plainly "no concrete thinking content is returned". This frontend once followed that and
 * made this interface an empty shell, so the thinking process
 * was stored in the database all the way but vanished at the type level. Measured in the
 * production `run_events` table, of 72 agent.thinking events
 * 69 had non-empty `content` (across 23 runs, all block type `text`, the longest body 3.6KB).
 *
 * So treat it as "may or may not be there": show the chain of thought if there is a body,
 * otherwise fall back to the "thinking" signal
 * (those 3 empty ones really were empty). **Do not delete it again because of that line in the
 * docs.**
 */
export interface AgentThinkingEvent extends RunEventBase {
  type: "agent.thinking";
  content?: TextBlock[];
}

export interface AgentToolUseEvent extends RunEventBase {
  type: "agent.tool_use";
  /**
   * Call id, used to pair with the result event's `tool_use_id`.
   *
   * **Optional, because real Ark does not send it**: the call id is simply the event's own `id`
   * (like `call_2k985r...`). When pairing always write `tool_use_id ?? id`;
   * see indexToolCalls in lib/timeline.ts.
   */
  tool_use_id?: string;
  name: string;
  /**
   * Tool input.
   *
   * **The Ark docs say "a JSON string", but real Ark actually sends an object.** So it is declared
   * here
   * as `ToolInput` (string | object) rather than `string`; declaring it `string` would let callers
   * directly
   * `.replace()` / `JSON.parse()`, and on an object the former **blanks the whole page**, the latter
   * renders
   * `[object Object]`. Handle it with prettyJson / truncate, which take both shapes.
   */
  input: ToolInput;
  evaluated_permission?: EvaluatedPermission;
}

export interface AgentToolResultEvent extends RunEventBase {
  type: "agent.tool_result";
  tool_use_id: string;
  content: ContentBlock[];
  is_error?: boolean;
}

export interface AgentMcpToolUseEvent extends RunEventBase {
  type: "agent.mcp_tool_use";
  /** Same as agent.tool_use.tool_use_id: may be missing; when missing, pair by the event's own id. */
  tool_use_id?: string;
  mcp_server_name: string;
  name: string;
  /** Same as agent.tool_use.input: the docs say string, in practice it is an object. */
  input: ToolInput;
  evaluated_permission?: EvaluatedPermission;
}

export interface AgentMcpToolResultEvent extends RunEventBase {
  type: "agent.mcp_tool_result";
  mcp_tool_use_id: string;
  mcp_server_name?: string;
  content: ContentBlock[];
  is_error?: boolean;
}

export interface AgentCustomToolUseEvent extends RunEventBase {
  type: "agent.custom_tool_use";
  name: string;
  /** Same as agent.tool_use.input: the docs say string, in practice it is an object. */
  input: ToolInput;
  evaluated_permission?: EvaluatedPermission;
}

export interface AgentThreadMessageSentEvent extends RunEventBase {
  type: "agent.thread_message_sent";
  content: ContentBlock[];
  to_session_thread_id: string;
  /** Empty when sent to the main agent. */
  to_agent_name?: string;
}

export interface AgentThreadMessageReceivedEvent extends RunEventBase {
  type: "agent.thread_message_received";
  content: ContentBlock[];
  from_session_thread_id: string;
  /** Empty when it comes from the main agent. */
  from_agent_name?: string;
}

export interface AgentThreadContextCompactedEvent extends RunEventBase {
  type: "agent.thread_context_compacted";
  summary?: string;
}

// ---------------------------------------------------------------------------
// session domain (12 kinds)
// ---------------------------------------------------------------------------

export interface SessionStatusRunningEvent extends RunEventBase {
  type: "session.status_running";
}

export interface SessionStatusIdleEvent extends RunEventBase {
  type: "session.status_idle";
  stop_reason?: StopReason;
}

export interface SessionStatusRescheduledEvent extends RunEventBase {
  type: "session.status_rescheduled";
}

export interface SessionStatusTerminatedEvent extends RunEventBase {
  type: "session.status_terminated";
  stop_reason?: StopReason;
}

export interface SessionErrorEvent extends RunEventBase {
  type: "session.error";
  error: SessionErrorPayload;
}

export interface SessionUpdatedEvent extends RunEventBase {
  type: "session.updated";
  name: string;
}

export interface SessionDeletedEvent extends RunEventBase {
  type: "session.deleted";
}

export interface SessionThreadCreatedEvent extends RunEventBase {
  type: "session.thread_created";
  agent_name: string;
  parent_thread_id?: string | null;
}

export interface SessionThreadStatusRunningEvent extends RunEventBase {
  type: "session.thread_status_running";
  agent_name: string;
}

export interface SessionThreadStatusIdleEvent extends RunEventBase {
  type: "session.thread_status_idle";
  agent_name: string;
  stop_reason?: StopReason;
}

export interface SessionThreadStatusRescheduledEvent extends RunEventBase {
  type: "session.thread_status_rescheduled";
  agent_name: string;
}

export interface SessionThreadStatusTerminatedEvent extends RunEventBase {
  type: "session.thread_status_terminated";
  agent_name: string;
}

// ---------------------------------------------------------------------------
// span domain (5 kinds)
// ---------------------------------------------------------------------------

export interface SpanModelRequestStartEvent extends RunEventBase {
  type: "span.model_request_start";
}

export interface SpanModelRequestEndEvent extends RunEventBase {
  type: "span.model_request_end";
  model_request_start_id: string;
  model_usage?: ModelUsage;
  is_error?: boolean;
  error_message?: string;
}

export interface SpanOutcomeEvaluationStartEvent extends RunEventBase {
  type: "span.outcome_evaluation_start";
  outcome_id: string;
  iteration: number;
}

export interface SpanOutcomeEvaluationOngoingEvent extends RunEventBase {
  type: "span.outcome_evaluation_ongoing";
  outcome_id: string;
  iteration: number;
}

export type OutcomeResult =
  | "satisfied"
  | "needs_revision"
  | "max_iterations_reached"
  | "failed"
  | "interrupted";

export interface SpanOutcomeEvaluationEndEvent extends RunEventBase {
  type: "span.outcome_evaluation_end";
  outcome_id: string;
  outcome_evaluation_start_id: string;
  iteration: number;
  result: OutcomeResult;
  explanation?: string;
  usage?: ModelUsage;
}

// ---------------------------------------------------------------------------
// Discriminated union
// ---------------------------------------------------------------------------

export type RunEvent =
  // user domain
  | UserMessageEvent
  | SystemMessageEvent
  | UserInterruptEvent
  | UserToolConfirmationEvent
  | UserToolResultEvent
  | UserCustomToolResultEvent
  | UserDefineOutcomeEvent
  // agent domain
  | AgentMessageEvent
  | AgentThinkingEvent
  | AgentToolUseEvent
  | AgentToolResultEvent
  | AgentMcpToolUseEvent
  | AgentMcpToolResultEvent
  | AgentCustomToolUseEvent
  | AgentThreadMessageSentEvent
  | AgentThreadMessageReceivedEvent
  | AgentThreadContextCompactedEvent
  // session domain
  | SessionStatusRunningEvent
  | SessionStatusIdleEvent
  | SessionStatusRescheduledEvent
  | SessionStatusTerminatedEvent
  | SessionErrorEvent
  | SessionUpdatedEvent
  | SessionDeletedEvent
  | SessionThreadCreatedEvent
  | SessionThreadStatusRunningEvent
  | SessionThreadStatusIdleEvent
  | SessionThreadStatusRescheduledEvent
  | SessionThreadStatusTerminatedEvent
  // span domain
  | SpanModelRequestStartEvent
  | SpanModelRequestEndEvent
  | SpanOutcomeEvaluationStartEvent
  | SpanOutcomeEvaluationOngoingEvent
  | SpanOutcomeEvaluationEndEvent;

export type RunEventType = RunEvent["type"];

// ---------------------------------------------------------------------------
// Display-layer categories
// ---------------------------------------------------------------------------

/**
 * Semantic category of events. 33 event kinds in one flat stream are unreadable; the UI groups
 * them by this for visual identity and filter axes.
 */
export type EventCategory =
  | "conversation"
  | "thinking"
  | "tool"
  | "thread"
  | "status"
  | "trace"
  | "error"
  | "outcome";

const CATEGORY_BY_TYPE: Record<RunEventType, EventCategory> = {
  "user.message": "conversation",
  "system.message": "conversation",
  "agent.message": "conversation",
  "user.interrupt": "status",

  "agent.thinking": "thinking",

  "agent.tool_use": "tool",
  "agent.tool_result": "tool",
  "agent.mcp_tool_use": "tool",
  "agent.mcp_tool_result": "tool",
  "agent.custom_tool_use": "tool",
  "user.tool_confirmation": "tool",
  "user.tool_result": "tool",
  "user.custom_tool_result": "tool",

  "agent.thread_message_sent": "thread",
  "agent.thread_message_received": "thread",
  "agent.thread_context_compacted": "thread",
  "session.thread_created": "thread",
  "session.thread_status_running": "thread",
  "session.thread_status_idle": "thread",
  "session.thread_status_rescheduled": "thread",
  "session.thread_status_terminated": "thread",

  "session.status_running": "status",
  "session.status_idle": "status",
  "session.status_rescheduled": "status",
  "session.status_terminated": "status",
  "session.updated": "status",
  "session.deleted": "status",

  "session.error": "error",

  "span.model_request_start": "trace",
  "span.model_request_end": "trace",

  "user.define_outcome": "outcome",
  "span.outcome_evaluation_start": "outcome",
  "span.outcome_evaluation_ongoing": "outcome",
  "span.outcome_evaluation_end": "outcome",
};

export function categoryOf(type: RunEventType): EventCategory {
  return CATEGORY_BY_TYPE[type] ?? "status";
}

export const EVENT_CATEGORY_LABELS: Record<EventCategory, string> = {
  conversation: "对话",
  thinking: "思考",
  tool: "工具调用",
  thread: "线程",
  status: "状态",
  trace: "追踪",
  error: "错误",
  outcome: "结果评估",
};

/** trace is hidden by default: span.* is high volume and helps little with everyday debugging. */
export const DEFAULT_VISIBLE_CATEGORIES: EventCategory[] = [
  "conversation",
  "thinking",
  "tool",
  "thread",
  "status",
  "error",
  "outcome",
];

// ---------------------------------------------------------------------------
// Extract content text
// ---------------------------------------------------------------------------

/**
 * Tag prefix of platform-injected blocks.
 *
 * It is **the same literal** as `HarnessTagPrefix` in the backend's `server/core/envprompt.go`;
 * the two must match (with a copy on each side, one gets changed and the other missed). There are
 * two kinds of blocks now:
 *
 * - `<harness:env>`: the agent's custom environment variables; it goes into **the Ark Agent's
 *   system prompt**,
 *   so it is **not** in RunEvent, and the frontend cannot see it;
 * - `<harness:lark>`: the source info of a Feishu message (chat_id / thread_id / the command to
 *   fetch history);
 *   it differs per message, so it hangs on the **second content block** of `user.message`,
 *   and so it naturally goes into RunEvent, where the frontend can get it.
 *
 * The frontend only ever meets the latter. Checking the prefix rather than hard-coding
 * `<harness:lark>` means that when a third kind of block is added
 * nothing here needs to change.
 */
export const HARNESS_TAG_PREFIX = "<harness:";

/**
 * Split the content of a user.message into two halves: "what the user really said" and "the
 * platform-injected blocks".
 *
 * # Why split
 *
 * In messages from Feishu, the platform appends a `<harness:lark>` block after the user's text.
 * Without splitting,
 * `contentToText` would join the two, so **the chat bubble would show the user's sentence plus a
 * big block of XML**;
 * that is for the model and should not be shown to users. After splitting, the text shows as usual
 * and the block goes into a collapsed area.
 *
 * # The check is "this block starts with a harness tag", not position
 *
 * Taking by index (e.g. "the second block is the injection") would go wrong when the block count
 * changes, and Ark may later
 * insert image blocks in multimodal messages. Checking the prefix does not depend on order or count.
 */
export function splitHarnessBlocks(
  blocks: ContentBlock[] | undefined,
  t: TFunc,
): {
  /** What the user really said (may be an empty string, e.g. when the bot is only @-mentioned in a group). */
  text: string;
  /** Platform-injected blocks, in order of appearance; an empty array if none. */
  injected: string[];
} {
  if (!blocks?.length) return { text: "", injected: [] };
  const injected: string[] = [];
  const rest: ContentBlock[] = [];
  for (const b of blocks) {
    if (b.type === "text" && b.text.trimStart().startsWith(HARNESS_TAG_PREFIX)) {
      injected.push(b.text.trim());
      continue;
    }
    rest.push(b);
  }
  return { text: contentToText(rest, t), injected };
}

/** Extract readable text from an array of content blocks; non-text blocks degrade to a placeholder description. */
export function contentToText(blocks: ContentBlock[] | undefined, t: TFunc): string {
  if (!blocks?.length) return "";
  return blocks
    .map((b) => {
      switch (b.type) {
        case "text":
          return b.text;
        case "image":
          return b.source.type === "url"
            ? t("evt.blockImageUrl", { url: b.source.url })
            : t("evt.blockImage");
        case "document":
          return b.title ? t("evt.blockDocumentTitled", { title: b.title }) : t("evt.blockDocument");
        case "search_result":
          return t("evt.blockSearchResult", { title: b.title });
      }
    })
    .join("\n");
}

// ---------------------------------------------------------------------------
// SSE delta frames
// ---------------------------------------------------------------------------

/**
 * Frames pushed by the streaming subscription.
 *
 * **Deliberately not in the RunEvent union.** Ark states clearly that event_start / event_delta are
 * best-effort and
 * **do not enter event history** (see the Ark docs on streaming session events);
 * that is, querying history by sequence afterwards will never find these two kinds of frames.
 * Mixing them into the 33-kind union that mirrors history
 * would force every place that exhaustively matches RunEvent, like buildTimeline() /
 * computeStats(), to
 * handle something that is never stored.
 *
 * Its role is only "temporary display scaffolding before the final event arrives":
 *   event_start → open an empty draft
 *   event_delta → append character by character (where the live feel comes from)
 *   event       → the buffered final event, **which is the source of truth**; on arrival it
 *                 replaces the draft as a whole
 *
 * This way, even if delta frames are dropped midway (best-effort allows that), the final result is
 * still right.
 */
export type StreamFrame =
  | {
      kind: "event_start";
      /** Aligned with the final event's id; used to pair the draft with the settled event. */
      id: string;
      /** Ark currently only supports delta previews for these two kinds. */
      type: "agent.message" | "agent.thinking";
      session_thread_id: string | null;
    }
  | { kind: "event_delta"; id: string; text: string }
  | { kind: "event"; event: RunEvent }
  /** Corresponds to the SSE `data: [DONE]` frame. */
  | { kind: "done" };

