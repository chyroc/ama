/**
 * Folds timeline entries into a **call tree** (span tree).
 *
 * # The structure copies the official component; we do not invent our own
 *
 * The data structure here **deliberately keeps the same shape as `SpanNode` in the CozeLoop
 * observability component**:
 *
 * ```ts
 * // the official one
 * type SpanNode = OutputSpan & { children?: SpanNode[]; isCollapsed: boolean; isLeaf: boolean }
 * ```
 *
 * That is: **tree nodes are the spans themselves**, parent/child links come from the spans'
 * parent/child ids,
 * and a tree has exactly one root span. **Do not put in self-invented middle layers like
 * "turns"**:
 * the earlier version did that (run → turn → span, three levels), and ended up with a structure
 * completely unlike the official component.
 *
 * Rewriting the rendering (switching to HeroUI colors and style) is fine; **changing the structure
 * is not**.
 *
 * # Where our span levels come from
 *
 * Every model request / tool call hangs under this run's root span, i.e.:
 *
 * ```
 * root span (one Run, span_type=agent)
 * ├─ model.request   (span_type=model)
 * ├─ tool.bash       (span_type=tool)
 * └─ …
 * ```
 *
 * **Two levels, not three**.
 * Ark has no deeper parent/child info (the envelope has no `parent_span_id`), so deeper levels
 * cannot be built,
 * and should not be.
 *
 * # Duration is derived wall-clock time
 *
 * Same source as `TimelineItem.durationMs`; it includes queueing, and also the 16~29 seconds of
 * resuming a container from pause.
 * **When it cannot be computed it is explicitly undefined, never 0** (0 would bubble it to the top
 * in a duration sort,
 * looking like the fastest step).
 *
 * The root's duration takes **the span from first to last** rather than the sum of children: the
 * two differ, and the difference is exactly the queueing and
 * container resume part; summing would systematically underestimate "how long the user waited".
 * The official component's root span
 * uses the same basis (its duration comes from the span's own start and end times).
 */

import type { TimelineItem } from "@/lib/timeline";
import { isUserMessage } from "@/lib/timeline";
import type { TFunc } from "@/prefs/PrefsContext";

/**
 * Span type. **The values are the ones from the official `SpanType` that we actually produce**
 * (the official set has 18: custom/prompt/model/parser/embedding/memory/plugin/function/
 * tool/graph/remote/loader/transformer/vector_store/vector_retriever/agent/bot/llm_call).
 *
 * We only produce three, one to one with `span_type` in the backend's `mapping.go`:
 * `agent` (root) / `model` / `tool`. **Do not add types we never produce to "look richer"**;
 * that would put legends on the tree for things that never appear.
 */
export type SpanType = "agent" | "model" | "tool";

/** Span status. The official one has two states, `SpanStatus.Success | Error`. */
export type SpanStatus = "success" | "error";

/**
 * A call tree node: **shape aligned with the official `SpanNode`**.
 *
 * The official one is `OutputSpan & { children, isCollapsed, isLeaf }`, where `OutputSpan` has
 * `span_id / span_name / span_type / type / status / duration / started_at /
 * custom_tags{tokens,input_tokens,output_tokens}`. Here we keep those we actually have data for,
 * and the names follow it too (**snake_case**, because these fields mirror an external structure,
 * matching the
 * naming convention in `docs/guides/frontend.md`).
 */
export interface SpanNode {
  span_id: string;
  span_name: string;
  span_type: SpanType;
  status: SpanStatus;
  /**
   * Duration (ms). **undefined when it cannot be computed, not 0**; see the file header.
   * On the official side duration is always present (the platform computes it); on ours, not
   * being able to compute it is normal.
   */
  duration?: number;
  /** Start time, ISO string. The official one is a ms timestamp; here we keep our database's shape. */
  started_at: string | null;
  /** The official one puts tokens under custom_tags; that level is copied here. */
  custom_tags?: {
    tokens?: number;
    input_tokens?: number;
    output_tokens?: number;
    /** Model name. The official node row shows it instead of the type name when span_type==='model'. */
    model_name?: string;
  };
  children: SpanNode[];
  isLeaf: boolean;
  /** The matching timeline entry. root has none (it aggregates the whole Run). */
  item?: TimelineItem;
  /** Input / output to show when expanded. These are the two big sections of the official right-hand detail. */
  input?: string;
  output?: string;
  /** Error message, present when status==='error'. */
  status_code?: string;
}

/** Only these two kinds of entries take a span on the call tree. */
export function isSpanLike(item: TimelineItem): boolean {
  return item.category === "tool" || item.type.startsWith("span.model_request");
}

/**
 * Whether this entry is a **final** `session.error`, i.e. "this session really failed".
 *
 * ⚠️ **`retrying` does not count.** Recoverable errors (rate limits, model overload) are
 * rescheduled automatically and keep running,
 * then finish normally; counting them as failures would show "succeeded after retry" runs as
 * failed.
 * This is the same basis as the tone of `session.error` in `timeline.ts`
 * (`retrying` → warning, everything else → danger).
 *
 * A missing `retry_status` counts as failure: when Ark does not give this field there is no retry
 * planned,
 * so it is final. **When unsure, count it as failure** is a deliberate direction: missing one real
 * failure
 * (the user thinks it succeeded, but it did not finish) is worse than one extra report.
 */
function isTerminalSessionError(item: TimelineItem): boolean {
  if (item.type !== "session.error") return false;
  const raw = item.raw[0];
  if (!raw || raw.type !== "session.error") return false;
  return raw.error.retry_status?.type !== "retrying";
}

/** Difference between two timestamps. Returns undefined if either end is missing; see the file header. */
function spanMs(from: string | null | undefined, to: string | null | undefined): number | undefined {
  if (!from || !to) return undefined;
  const a = Date.parse(from);
  const b = Date.parse(to);
  if (Number.isNaN(a) || Number.isNaN(b)) return undefined;
  const d = b - a;
  return d >= 0 ? d : undefined;
}

/**
 * A model request is **two** events in the stream (`span.model_request_start` / `_end`),
 * but **one** span on the call tree, and **its duration must be computed ourselves**.
 *
 * ⚠️ **`buildTimeline` does not pair model requests, nor give them durationMs.** The tool call
 * side
 * is already paired (one TimelineItem holds two raw events and has `durationMs`; see
 * `toolCallItem`),
 * but model requests are two separate entries, and `durationMs` is **always undefined**.
 * Using the `_end` entry directly as the node would show "unknown" for every model request on the
 * tree,
 * and the whole point of the call tree is to see durations; half the nodes would be wasted.
 *
 * So here start and end are paired **in order of appearance** (Ark sends no link id,
 * and model requests within a turn are serial, so pairing by order is right); the duration is the
 * difference of the two `processed_at`.
 *
 * With only `_start` and no `_end` (request still running / run interrupted), the node is still
 * shown,
 * but **the duration is left empty**; the same rule as "a running tool does not show elapsed time":
 * subtracting from now would give "3 days" when viewing history from three days ago.
 */
function modelSpans(items: TimelineItem[], t: TFunc): SpanNode[] {
  const starts = items.filter((it) => it.type === "span.model_request_start");
  const ends = items.filter((it) => it.type === "span.model_request_end");

  const out: SpanNode[] = [];
  for (let i = 0; i < Math.max(starts.length, ends.length); i++) {
    const start = starts[i];
    const end = ends[i];
    const anchor = end ?? start;
    if (!anchor) continue;

    const usage = end ? usageOf(end) : undefined;
    out.push({
      span_id: anchor.key,
      // The official node row shows model_name for the model type, falling back to the type name
      // only if absent.
      span_name: usage?.model_name ?? t("trace.spanModel"),
      span_type: "model",
      status: anchor.tone === "danger" ? "error" : "success",
      duration: end ? spanMs(start?.processedAt, end.processedAt) : undefined,
      started_at: start?.processedAt ?? anchor.processedAt,
      custom_tags: usage,
      children: [],
      isLeaf: true,
      item: anchor,
      output: anchor.summary || undefined,
      status_code: anchor.tone === "danger" ? anchor.summary || undefined : undefined,
    });
  }
  return out;
}

/**
 * Take usage from the `model_usage` of `span.model_request_end`.
 *
 * The official node row only shows that label when tokens exist and are non-zero, with the
 * tooltip listing
 * input / output separately. Here we give the same shape.
 */
function usageOf(item: TimelineItem): SpanNode["custom_tags"] {
  const raw = item.raw[0];
  if (!raw || raw.type !== "span.model_request_end") return undefined;
  const u = raw.model_usage;
  if (!u) return undefined;
  const total = (u.input_tokens ?? 0) + (u.output_tokens ?? 0);
  return {
    tokens: total > 0 ? total : undefined,
    input_tokens: u.input_tokens,
    output_tokens: u.output_tokens,
  };
}

/** One tool call → one span. */
function toolSpan(item: TimelineItem): SpanNode {
  const isError = item.result?.isError === true;
  return {
    span_id: item.key,
    span_name: item.title,
    span_type: "tool",
    status: isError ? "error" : "success",
    duration: item.durationMs,
    started_at: item.processedAt,
    children: [],
    isLeaf: true,
    item,
    // The official right-hand detail has two big sections, Input / Output; we match it: the input
    // is Input, the receipt is Output.
    input: item.command ?? item.body,
    output: item.result?.text,
    status_code: isError ? item.result?.text : undefined,
  };
}

/**
 * Derive the span tree from timeline entries.
 *
 * The input must be the output of `buildTimeline()` (sorted by ascending sequence, tools paired).
 * `t` is passed in by the caller: this is a pure module, the same rule as `lib/timeline.ts`
 * (module-level constants cannot call `t()`; see docs/guides/i18n.md).
 *
 * Returns the root span; returns null when there are no child spans (the caller shows an empty
 * state).
 * This matches the position of the official `getRootSpan`: it too "gets the root first, then
 * renders down from it".
 */
export function buildSpanTree(items: TimelineItem[], t: TFunc): SpanNode | null {
  const spans = items.filter(isSpanLike);
  const children = [
    ...modelSpans(spans, t),
    ...spans.filter((it) => it.category === "tool").map(toolSpan),
  ]
    // Sort by **start time**: the call tree is read by time, and the row order must match the
    // horizontal position of the Gantt bars.
    //
    // ⚠️ **Do not sort by `item.sequence`.** A model span's `item` is the `_end` event
    // (usage and error info are on it), so its sequence is the **end** sequence number. But the
    // bar is drawn from
    // `started_at`; the two are not the same thing. Measured in one production run:
    //
    //     seq 31  span.model_request_start   13.123s   ← the model starts here
    //     seq 33  agent.tool_use             16.992s   ← a tool is called in the middle of it
    //     seq 35  span.model_request_end     17.005s   ← item takes this one
    //
    // Sorting by sequence would put the model **after** the tool (35 > 33), yet its bar is drawn to
    // the tool's left,
    // so "the bar in the row above is further right", looking like the time axis is wrong. What is
    // actually wrong is the row order.
    //
    // When start times are equal, sequence breaks the tie to keep the order stable (apart from the
    // React key, an unstable sort would make
    // the row order jitter on every re-render).
    .sort((a, b) => {
      const sa = a.started_at ? Date.parse(a.started_at) : Number.POSITIVE_INFINITY;
      const sb = b.started_at ? Date.parse(b.started_at) : Number.POSITIVE_INFINITY;
      if (sa !== sb && Number.isFinite(sa) && Number.isFinite(sb)) return sa - sb;
      return (a.item?.sequence ?? 0) - (b.item?.sequence ?? 0);
    });

  if (children.length === 0) return null;

  const stamps: number[] = [];
  for (const it of items) {
    if (!isSpanLike(it) && !isUserMessage(it)) continue;
    for (const s of [it.processedAt, it.result?.processedAt]) {
      if (!s) continue;
      const v = Date.parse(s);
      if (!Number.isNaN(v)) stamps.push(v);
    }
  }

  return {
    span_id: "root",
    span_name: t("trace.spanRoot"),
    span_type: "agent",
    // ⚠️ **Only a final `session.error` makes this session failed; child span errors do not bubble
    // up.**
    //
    // This used to be `children.some((c) => c.status === "error")`, which was wrong:
    // **agent tool call errors are an extremely common, normal thing**. The model for the rule is a
    // real session
    // (checked row by row in the database): of 17 tool calls 1 was
    // an `is_error: true` Python TypeError, 0 `session.error`, and the last idle's
    // `stop_reason` was `end_turn` with an empty `errorSummary`: the agent read the error, fixed the
    // code,
    // and finished the job; **that is success**. Yet this panel showed it as "failed", while the
    // same run on the list page
    // was success (the list checks `errorSummary`). **One run, two pages, two conclusions.**
    //
    // The result is not "occasionally mislabels one", but **the vast majority of successful
    // sessions labeled failed**,
    // so the status flag becomes completely untrustworthy and nobody looks at it anymore; worse
    // than showing no status.
    //
    // ⚠️ **`retrying` is not a failure**: recoverable errors are rescheduled automatically, keep
    // running, and then finish normally.
    // This matches this repo's established basis (the tone `timeline.ts` gives `session.error`:
    // `retrying` gets warning, the rest get danger), and also the list/overview pages'
    // `RunStatusChip` (the check is `errorSummary` non-empty **and** status is terminated).
    // Missing this would label "succeeded after rate-limit retry" runs as failed again; the same
    // bug through another door.
    //
    // Each child span still turns red on its own (status in `toolSpan` / `modelSpans`),
    // which is useful for debugging: which step errored must be visible at a glance. The only
    // difference is that it does not decide the root.
    status: items.some(isTerminalSessionError) ? "error" : "success",
    duration: stamps.length >= 2 ? Math.max(...stamps) - Math.min(...stamps) : undefined,
    started_at: stamps.length > 0 ? new Date(Math.min(...stamps)).toISOString() : null,
    children,
    isLeaf: false,
  };
}

/**
 * Flatten the tree into a list for rendering, with depth and "is it the last at its level".
 *
 * The official side calls this step `flattenTreeData` (`graphs/tree/util.d.ts`), producing
 * `{ nodes, lines }`: nodes for rendering rows, lines for drawing SVG connector lines.
 * We do not draw SVG (that would drag in its `lineStyle` system and `PathEnum`);
 * connector lines are drawn with CSS by depth, so only nodes + `isLastChild` are needed.
 *
 * `collapsed` is the set of ids of collapsed nodes: **none of their descendants appear**,
 * but the node itself stays (otherwise there would be nothing to click to expand). This is the same
 * meaning as the official `isCollapsed` on nodes,
 * except we lift the state outside, avoiding "change the tree to change UI state".
 */
export interface FlatSpan {
  node: SpanNode;
  depth: number;
  isLastChild: boolean;
}

export function flattenSpans(root: SpanNode, collapsed: ReadonlySet<string>): FlatSpan[] {
  const out: FlatSpan[] = [];
  const walk = (node: SpanNode, depth: number, isLastChild: boolean) => {
    out.push({ node, depth, isLastChild });
    if (collapsed.has(node.span_id)) return;
    node.children.forEach((c, i) => walk(c, depth + 1, i === node.children.length - 1));
  };
  walk(root, 0, true);
  return out;
}

/**
 * A span's position on the **shared time axis**.
 *
 * # Why it must be a shared time axis, not a separate ratio bar per row
 *
 * Earlier each row's bar was the ratio "this row's duration / the slowest duration"; that only
 * answers "which took longer",
 * **not "when did it happen, and what does it overlap"**. And agents call tools concurrently:
 * measured in one production run
 *
 *   call_v0i5…  00:43:53.409532 → 00:44:53.557  (60.1s)
 *   call_dwu4…  00:43:53.409548 → 00:44:54.023  (60.6s)   ← starts 16µs later, ends 0.5s later
 *
 * These two **really ran at the same time**. Ratio bars would draw them as two independent bars of
 * almost the same length, hiding the concurrency;
 * on one shared time axis you see at a glance that they overlap horizontally. "Are these two
 * parallel or one after the other"
 * is the first question when debugging durations, and ratio bars cannot answer it.
 *
 * # Precision
 *
 * ⚠️ Ark's docs say `processed_at` is in seconds, but **in practice it has nanoseconds**
 * (`2026-08-28T00:43:53.409532457+08:00`). The 16µs difference above comes from that digit.
 * So **do not** round to seconds here; after rounding those two would start at the same moment,
 * and "which started first" would be lost.
 */
export interface SpanTiming {
  /** Offset of the start relative to the whole tree's start, 0~1. */
  offset: number;
  /** Duration as a fraction of the whole tree's span, 0~1. */
  width: number;
  /** Absolute start / end times, for tooltips and details. */
  startMs: number;
  endMs: number;
}

/** Time window of the whole tree. null means it cannot be computed (not enough timestamps). */
export interface TraceWindow {
  startMs: number;
  endMs: number;
  spanMs: number;
}

/** Get the whole tree's time window: the horizontal axis of the Gantt bars. */
export function traceWindow(root: SpanNode): TraceWindow | null {
  let min = Number.POSITIVE_INFINITY;
  let max = Number.NEGATIVE_INFINITY;
  const walk = (n: SpanNode) => {
    const t = n.started_at ? Date.parse(n.started_at) : NaN;
    if (!Number.isNaN(t)) {
      min = Math.min(min, t);
      // Nodes without a duration only contribute their start: **do not treat them as zero-length**
      // and stretch the window's right edge;
      // that would make a "still running" call look like it ended instantly.
      max = Math.max(max, t + (n.duration ?? 0));
    }
    n.children.forEach(walk);
  };
  walk(root);
  if (!Number.isFinite(min) || !Number.isFinite(max) || max <= min) return null;
  return { startMs: min, endMs: max, spanMs: max - min };
}

/**
 * Compute a span's position on the time axis.
 *
 * Returns null when the duration cannot be computed (still running / missing timestamps); the
 * caller then draws a "start marker"
 * rather than a bar of doubtful length. **Do not fill it in with "until now"**: viewing history
 * from three days ago would give
 * a bar across the whole axis (same origin as the durationMs rule).
 */
export function spanTiming(node: SpanNode, win: TraceWindow): SpanTiming | null {
  if (!node.started_at) return null;
  const startMs = Date.parse(node.started_at);
  if (Number.isNaN(startMs)) return null;
  if (node.duration == null) return null;

  const offset = (startMs - win.startMs) / win.spanMs;
  const width = node.duration / win.spanMs;
  return {
    // Clamp to [0,1]: timestamps sometimes go slightly outside the window (the root's start comes
    // from user.message,
    // and a child span may be earlier than it; that is existing noise in Ark's event order and
    // should not make bars run out of the container).
    offset: Math.min(Math.max(offset, 0), 1),
    // Give a minimum visible width: a call taking tens of ms on an axis of several minutes computes
    // to
    // under 0.1%, and rounding to 0 width would make the whole bar vanish, looking like "this step
    // never happened".
    width: Math.min(Math.max(width, 0.004), 1 - Math.min(Math.max(offset, 0), 1)),
    startMs,
    endMs: startMs + node.duration,
  };
}

/**
 * Find which siblings at the same level a span overlaps in time, i.e. "is it concurrent".
 *
 * Only siblings under **the same parent** are checked: overlap across levels is a given (a child
 * span is inside its parent's interval anyway),
 * and marking it is just noise.
 */
export function concurrentSiblings(parent: SpanNode, node: SpanNode): SpanNode[] {
  const range = (n: SpanNode): [number, number] | null => {
    if (!n.started_at || n.duration == null) return null;
    const s = Date.parse(n.started_at);
    if (Number.isNaN(s)) return null;
    return [s, s + n.duration];
  };
  const mine = range(node);
  if (!mine) return [];
  return parent.children.filter((sib) => {
    if (sib.span_id === node.span_id) return false;
    const other = range(sib);
    if (!other) return false;
    // Interval intersection (half-open; touching endpoints do not count as overlap, that is one
    // after the other).
    return other[0] < mine[1] && mine[0] < other[1];
  });
}

/** Number of all spans in the tree (including root). */
export function countSpans(root: SpanNode): number {
  return 1 + root.children.reduce((sum, c) => sum + countSpans(c), 0);
}

/** Find the node with a given id. The right-hand detail fetches the span by the selected id. */
export function findSpan(root: SpanNode, spanId: string): SpanNode | null {
  if (root.span_id === spanId) return root;
  for (const c of root.children) {
    const hit = findSpan(c, spanId);
    if (hit) return hit;
  }
  return null;
}

/**
 * Re-sort **within each level** by duration, keeping parent/child links.
 *
 * Unknown durations are explicitly sorted last; do not write `?? 0`: in descending order `?? 0`
 * happens to also put unknowns at the end,
 * so right now the two behave the same, but it is a **fragile coincidence**: the day an ascending
 * option is added,
 * `?? 0` would immediately bubble unknowns to the "fastest" end, looking like a batch of instant
 * calls.
 */
export function sortByDuration(node: SpanNode): SpanNode {
  if (node.children.length === 0) return node;
  const children = node.children
    .map(sortByDuration)
    .slice()
    .sort((a, b) => {
      if (a.duration == null && b.duration == null) return 0;
      if (a.duration == null) return 1;
      if (b.duration == null) return -1;
      return b.duration - a.duration;
    });
  return { ...node, children };
}

/**
 * Node visual config: **aligned with the official `NODE_CONFIG_MAP`**.
 *
 * The official one gives each span type a "color block + two-letter abbreviation" (`Mo` / `Ag`…),
 * rendered as a 16×16 square with `border-radius: 4px` and white bold 10px text
 * (`CustomIconWrapper`). It is the most visible identifying mark of its node rows, **and the
 * structure must be kept**.
 *
 * ⚠️ **Colors are switched to this repo's semantic tokens; its hex values are not copied.**
 * The official set uses fixed values like `#b4baf6` / `#00BF40`, which do not follow the theme in
 * dark mode
 * (exactly the shape of that md-box trap). Here they become tokens:
 *
 * | official | official color | here |
 * | --- | --- | --- |
 * | `agent` `Ag` | `#d1aef4` | `bg-accent` |
 * | `model` `Mo` | `#b4baf6` | `bg-accent-soft` |
 * | `tool` `tool` | `#00BF40` | `bg-success` |
 *
 * ⚠️ **The background must not use opacity variants like `bg-accent/70` with
 * `text-accent-foreground`.**
 * Measured: `/70` blends the background into the page background, and in dark mode it comes out as
 * `oklab(0.9576 … / 0.7)` against `oklch(0.1798 …)`, only **1.11:1**,
 * i.e. "light text on light background"; the two letters are almost unreadable.
 * The foreground is matched to an **opaque** background; once the background is transparent the
 * pairing breaks.
 * For a lighter level use the **paired** soft tokens (`bg-accent-soft` +
 * `text-accent-soft-foreground`), which are designed as a set.
 *
 * Abbreviations follow the official ones (`Ag` / `Mo`; the official tool character is the four
 * letters `tool`,
 * which do not fit in a 16px square, so `To` is used; this is the only deviation, for layout
 * reasons).
 */
export const SPAN_BADGE: Record<SpanType, { character: string; className: string }> = {
  agent: { character: "Ag", className: "bg-accent text-accent-foreground" },
  model: { character: "Mo", className: "bg-accent-soft text-accent-soft-foreground" },
  tool: { character: "To", className: "bg-success text-success-foreground" },
};

/**
 * Duration color levels: **the official three levels, copied** (`timeColor` in `CustomTreeNode`):
 *
 * ```js
 * duration > 60000 ? '#D0292F' : duration > 10000 ? '#CC8533' : '#757a8c'
 * ```
 *
 * i.e. **>60s alert, >10s notice, the rest neutral**. The thresholds are kept; colors become
 * semantic tokens
 * (same reason as SPAN_BADGE: fixed values do not follow the theme in dark mode).
 */
export function durationTone(ms: number | undefined): string {
  if (ms == null) return "text-muted";
  if (ms > 60_000) return "text-danger";
  if (ms > 10_000) return "text-warning";
  return "text-muted";
}
