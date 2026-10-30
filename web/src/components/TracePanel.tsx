import { useMemo, useState } from "react";

import { Icon } from "@iconify/react";
import { Chip, Separator, Tooltip } from "@heroui/react";
import { Segment } from "@/components/ui/Segment";

import { usePrefs } from "@/prefs/PrefsContext";
import type { TimelineItem } from "@/lib/timeline";
import {
  buildSpanTree,
  concurrentSiblings,
  countSpans,
  durationTone,
  findSpan,
  flattenSpans,
  isSpanLike,
  SPAN_BADGE,
  sortByDuration,
  spanTiming,
  traceWindow,
  type SpanNode,
  type TraceWindow,
} from "@/lib/traceTree";
import { formatDateTime, formatDuration, formatNumber } from "@/lib/format";

/**
 * Trace panel: the **call tree** of one run (matching the official `CozeloopTraceDetail`).
 *
 * # The structure follows the official component; only style / colors / look change
 *
 * The **structure of this panel is taken from `CozeloopTraceDetail` in the CozeLoop observability components**;
 * it was not designed from scratch. The mapping:
 *
 * | Official | Here |
 * | --- | --- |
 * | `VerticalTraceDetail` (vertical layout: header / tree / detail, three parts) | the three parts of `TracePanel` |
 * | `VerticalTraceHeader` (root span icon + trace name) | `TraceHeader` |
 * | `TraceGraphs` → `TraceTree` → `CustomTreeNode` | `SpanTree` → `SpanRow` |
 * | `SpanDetail` + `useSpanOverviewField` + Input/Output | `SpanDetail` |
 * | `SpanNode` (`OutputSpan & {children,isLeaf}`) | `SpanNode` in `lib/traceTree.ts` |
 *
 * **Vertical** (`layout="vertical"`) was chosen over horizontal: the official horizontal layout is a "tree left, detail right" split,
 * which needs `react-resizable-panels` (not installed in this repo; see "Component choices" below), and this panel
 * hangs in the right column of the run detail page, already narrow; splitting it in half horizontally would make both sides unreadable.
 * The official component offers both layouts, and the vertical one is what it prepared for narrow containers.
 *
 * ⚠️ **An earlier version used a structure we invented ourselves** (run → turn → span, three levels), completely different from the official one.
 * It has been changed back:
 * tree nodes are spans, levels come from span parent/child relations, and **there is no "turn" level any more**.
 *
 * # Details copied item by item
 *
 * What one row of the official `CustomTreeNode` is made of, left to right:
 * `color block + two-letter abbreviation` → `span_name` → `type Tag` (shows the model name for model) →
 * `clock icon + duration` (three color levels: >60s red / >10s orange / otherwise gray) → `token icon + tokens`.
 * Nothing is missing and the order is unchanged; **only fixed color values are replaced with semantic tokens** (the official `#b4baf6` /
 * `#00BF40` do not follow the theme in dark mode, exactly the shape of the md-box trap).
 *
 * The field grid of the official `useSpanOverviewField`:
 * `Status / StatusCode / SpanID / Type / Latency / StartTime / Tokens`,
 * 224px per cell. Here also seven cells, with the width changed to a responsive grid (the panel width is not fixed; a hard-coded 224
 * would overflow horizontally in a narrow column).
 *
 * # Why not install the official package directly
 *
 * It brings hundreds of transitive dependencies, nests its own copy of React 18 (this repo is React 19; two Reacts on one page
 * = hooks are sure to blow up), and is Tailwind v3 + puts `!important` rules on `:root`.
 * So it is "redrawn with HeroUI following its structure": **zero new dependencies, zero new CSS**.
 *
 * # Data is derived from `run_events`
 *
 * The call tree is computed by `buildSpanTree` from this run's event stream, without depending on any external observability platform.
 *
 * # Component choices (transitive dependencies all checked)
 *
 * ⚠️ **The ready-made `file-tree` kind of component does not fit**: it looks made exactly for this (internally wrapping RAC's `Tree`),
 * but `utils/tree-motion.js` imports `motion/react`, and motion is not installed in this repo → the build breaks right away.
 * Likewise `resizable` needs `react-resizable-panels` (not installed), so the official horizontal split cannot be chosen.
 * The tree's indentation and connecting lines are **drawn ourselves by depth** (flattened into a list + CSS; see `flattenSpans`),
 * while the official one uses SVG (`lineStyle` + `PathEnum`); copying that would mean copying its whole geometry calculation too.
 * See the transitive dependency table in docs/guides/heroui.md.
 */

export interface TracePanelProps {
  /** Timeline entries used by the event view, reusing the output of buildTimeline directly. */
  items: TimelineItem[];
  /**
   * The panel **has no scroll shell**.
   *
   * This is deliberate, copying the discipline of ConversationView: the chat page is `h-full` + inner scrolling,
   * while the run detail page's scroll shell is in AppShell's `<main>`. A panel with its own `overflow-y-auto`
   * would give two scroll areas on the latter (the outer scrolls a bit, the inner scrolls again, and the wheel behaves chaotically).
   * So it only outputs content, and **the scroll shell is decided by the caller**.
   */
  className?: string;
}

type SortMode = "sequence" | "duration";

export function TracePanel({ items, className }: TracePanelProps) {
  const { t } = usePrefs();
  const [sort, setSort] = useState<SortMode>("sequence");
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(() => new Set<string>());
  // The official one uses `selectedSpanId` + `onSelect`, with root selected by default (`defaultSelectedSpanID`).
  const [selectedId, setSelectedId] = useState<string | null>(null);

  // Building the tree is not cheap, and items is a new array on every frame as SSE advances, so it must be memoized:
  // same as buildHaystack (see docs/guides/frontend.md).
  const root = useMemo(() => buildSpanTree(items, t), [items, t]);

  const toggle = (id: string) => {
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  // When nothing is selected, treat root as selected: same as the official default behavior of defaultSelectedSpanID.
  const selected = root ? findSpan(root, selectedId ?? root.span_id) ?? root : null;

  return (
    <div className={className} data-testid="trace-panel">
      {/* Official VerticalTraceHeader: root span icon + trace name. */}
      <TraceHeader root={root} />

      {root === null ? (
        <p className="mt-4 text-xs text-muted" data-testid="trace-empty">
          {t("trace.noSpans")}
        </p>
      ) : (
        <>
          <SpanTree
            root={root}
            sort={sort}
            onSortChange={setSort}
            collapsed={collapsed}
            onToggle={toggle}
            selectedId={selected?.span_id ?? root.span_id}
            onSelect={setSelectedId}
          />
          {selected ? <SpanDetail span={selected} /> : null}
        </>
      )}
    </div>
  );
}

/** Official `VerticalTraceHeader`: root span icon + trace name. */
function TraceHeader({ root }: { root: SpanNode | null }) {
  const { t } = usePrefs();
  const badge = SPAN_BADGE[root?.span_type ?? "agent"];

  return (
    <>
      <div className="flex min-w-0 items-center gap-2">
        <span
          className={`inline-flex size-4 shrink-0 items-center justify-center r-chip text-[10px] font-semibold ${badge.className}`}
          aria-hidden="true"
        >
          {badge.character}
        </span>
        <h3 className="min-w-0 flex-1 truncate text-sm font-medium">
          {root?.span_name ?? t("trace.title")}
        </h3>
      </div>
      <p className="mt-1 text-xs leading-relaxed text-muted">{t("trace.durationHint")}</p>
    </>
  );
}

/**
 * Official `TraceGraphs` → `TraceTree`: summary row + sort switch + list of span rows.
 *
 * The official layer also has a search box (`enableTraceSearch`) and a trajectory switch (`showTrajectory`);
 * neither is here: search already exists on the event view side (`timeline-search`),
 * and trajectory depends on agent trajectory data from the platform side, which we do not have.
 * **We deliberately do not put in a search box that finds nothing**: that is worse than none.
 */
function SpanTree({
  root,
  sort,
  onSortChange,
  collapsed,
  onToggle,
  selectedId,
  onSelect,
}: {
  root: SpanNode;
  sort: SortMode;
  onSortChange: (m: SortMode) => void;
  collapsed: ReadonlySet<string>;
  onToggle: (id: string) => void;
  selectedId: string;
  onSelect: (id: string) => void;
}) {
  const { t } = usePrefs();

  // When sorting by duration, **only reorder within each level**, without flattening levels: a tree flattened into a descending list
  // is no longer a call tree. Parent/child relations are the main information of this view.
  const shown = useMemo(() => {
    const ordered = sort === "duration" ? sortByDuration(root) : root;
    return flattenSpans(ordered, collapsed);
  }, [root, sort, collapsed]);

  // The horizontal axis of the Gantt bars. **The window is always computed from the original tree, unaffected by sorting**: sorting by duration only changes row order,
  // and the time axis itself should not change (if it did, the same span would sit at different positions under the two sorts, which is odd).
  const win = useMemo(() => traceWindow(root), [root]);

  // span_id → its parent node, used to decide "is it concurrent with siblings at the same level".
  const parentOf = useMemo(() => {
    const m = new Map<string, SpanNode>();
    const walk = (n: SpanNode) => {
      for (const c of n.children) {
        m.set(c.span_id, n);
        walk(c);
      }
    };
    walk(root);
    return m;
  }, [root]);

  return (
    <>
      <div className="mt-4 flex flex-wrap items-center justify-between gap-2">
        <div className="flex flex-wrap items-center gap-3 text-xs text-muted">
          <span>
            {t("trace.spanCount")}
            <span className="num ml-1 text-foreground">{countSpans(root)}</span>
          </span>
          <span>
            {t("trace.totalDuration")}
            <span className="num ml-1 text-foreground">{formatDuration(root.duration)}</span>
          </span>
        </div>

        {/* Small binary switches use Segment (the detached look). */}
        <Segment
          size="sm"
          selectedKey={sort}
          onSelectionChange={(key) => onSortChange(key as SortMode)}
          aria-label={t("trace.title")}
        >
          <Segment.Item id="sequence" data-testid="trace-sort-sequence">
            {t("trace.sortBySequence")}
          </Segment.Item>
          <Segment.Item id="duration" data-testid="trace-sort-duration">
            {t("trace.sortByDuration")}
          </Segment.Item>
        </Segment>
      </div>

      {/* Axis ticks. **Same width and position as the track** (w-[40%] on the right), otherwise you cannot read which second a bar position means.
          Only three ticks (0 / midpoint / total): this panel is narrow, and any denser would crowd together. */}
      {win ? (
        <div className="mt-3 flex items-center gap-1.5 pl-1.5 text-[10px] text-muted">
          <span className="min-w-0 flex-1 truncate">{t("trace.axisLabel")}</span>
          <div className="relative h-3 w-[40%] shrink-0" data-testid="trace-axis">
            <span className="num absolute left-0">0</span>
            <span className="num absolute left-1/2 -translate-x-1/2">
              {formatDuration(Math.round(win.spanMs / 2))}
            </span>
            <span className="num absolute right-0">{formatDuration(win.spanMs)}</span>
          </div>
        </div>
      ) : null}

      {/* The tree uses ul/li: the levels are a **semantic** tree, and screen readers must be able to read the nesting. */}
      <ul className="mt-1 flex flex-col" data-testid="trace-tree">
        {shown.map(({ node, depth }) => (
          <SpanRow
            key={node.span_id}
            node={node}
            depth={depth}
            win={win}
            parent={parentOf.get(node.span_id) ?? null}
            isCollapsed={collapsed.has(node.span_id)}
            isSelected={node.span_id === selectedId}
            onToggle={onToggle}
            onSelect={onSelect}
          />
        ))}
      </ul>
    </>
  );
}

/**
 * Official `CustomTreeNode`: one span row.
 *
 * Left to right in its order: collapse toggle → color block abbreviation → span_name → type Tag →
 * clock + duration → token + tokens. The official row height is fixed at 32px (`h-[32px]`); here it follows with
 * `min-h-8` (not a hard-coded `h-8`: Chinese titles wrap in a narrow column, and a hard-coded height would clip them).
 */
function SpanRow({
  node,
  depth,
  win,
  parent,
  isCollapsed,
  isSelected,
  onToggle,
  onSelect,
}: {
  node: SpanNode;
  depth: number;
  win: TraceWindow | null;
  parent: SpanNode | null;
  isCollapsed: boolean;
  isSelected: boolean;
  onToggle: (id: string) => void;
  onSelect: (id: string) => void;
}) {
  const { t, language } = usePrefs();
  const hasChildren = node.children.length > 0;
  const badge = SPAN_BADGE[node.span_type];
  const tokens = node.custom_tags?.tokens;
  const timing = win ? spanTiming(node, win) : null;
  // Overlapping with a sibling at the same level = concurrent. root has no parent, so it naturally does not count.
  const concurrent = parent ? concurrentSiblings(parent, node) : [];

  return (
    <li
      // ⚠️ The testid **must not start with `event-`**: e2e uses `[data-testid^="event-"]`
      // to count timeline entries, and any `event-*` would be counted as an event (see frontend.md).
      data-testid={`trace-node-${node.span_type}`}
      data-depth={depth}
      data-span-id={node.span_id}
    >
      <div
        className={`flex min-h-8 min-w-0 items-center gap-1.5 r-row px-1.5 py-1 ${
          // The official selected state adds a highlight background to the node + changes the connecting line color. Here we use the surface token
          // (design taste: selected uses a surface-level background + a subtle shadow; unselected only changes on hover).
          isSelected ? "bg-surface-secondary shadow-surface" : "hover:bg-surface-secondary"
        }`}
        style={{ paddingLeft: `${depth * 1.25 + 0.375}rem` }}
      >
        {/* Collapse toggle. Without children it takes the same space, otherwise titles at each level would be misaligned left and right. */}
        {hasChildren ? (
          <button
            type="button"
            onClick={() => onToggle(node.span_id)}
            aria-label={isCollapsed ? t("trace.expand") : t("trace.collapse")}
            aria-expanded={!isCollapsed}
            data-testid="trace-toggle"
            className="shrink-0 text-muted cursor-[var(--cursor-interactive)] hover:text-foreground"
          >
            <Icon
              icon={isCollapsed ? "gravity-ui:chevron-right" : "gravity-ui:chevron-down"}
              className="size-3.5"
            />
          </button>
        ) : (
          <span className="size-3.5 shrink-0" aria-hidden="true" />
        )}

        {/* Official CustomIconWrapper: color block + two-letter abbreviation. */}
        <span
          className={`inline-flex size-4 shrink-0 items-center justify-center r-chip text-[10px] font-semibold ${badge.className}`}
          aria-hidden="true"
        >
          {badge.character}
        </span>

        {/* The whole row is selectable (officially onSelect is on the whole node). Use a button instead of div+onClick:
            the keyboard must be able to select it, and a div cannot get focus. */}
        <button
          type="button"
          onClick={() => onSelect(node.span_id)}
          className="min-w-0 flex-1 truncate text-left text-xs cursor-[var(--cursor-interactive)]"
          aria-current={isSelected ? "true" : undefined}
          data-testid="trace-span-name"
        >
          <span className={node.status === "error" ? "text-danger" : ""}>{node.span_name}</span>
        </button>

        {/* Concurrency mark. Overlap can already be seen on the Gantt bars, but **when the parent is collapsed the bars disappear too**,
            so here is one more mark that does not depend on the bars: you know at a glance this step ran in parallel. */}
        {concurrent.length > 0 ? (
          <Tooltip>
            <Tooltip.Trigger>
              <span
                className="flex shrink-0 items-center gap-0.5 text-xs text-accent"
                data-testid="trace-concurrent"
              >
                <Icon icon="gravity-ui:bars" className="size-3" aria-hidden="true" />
                <span className="num">{concurrent.length + 1}</span>
              </span>
            </Tooltip.Trigger>
            <Tooltip.Content>
              {t("trace.concurrentHint", { n: concurrent.length + 1 })}
            </Tooltip.Content>
          </Tooltip>
        ) : null}

        {/* The official type Tag. For model it prefers showing the model name, but here the model name is already the span_name,
            so this cell always shows the type name, to avoid the same string appearing twice (design taste:
            one piece of data should not have two visual representations). */}
        <Chip size="sm" variant="soft" className="shrink-0">
          {node.span_type}
        </Chip>

        {/* Official: clock icon + duration, three color levels (>60s / >10s / otherwise). */}
        {node.duration != null ? (
          <span
            className={`flex shrink-0 items-center gap-0.5 text-xs ${durationTone(node.duration)}`}
            data-testid="trace-duration"
          >
            <Icon icon="gravity-ui:clock" className="size-3" aria-hidden="true" />
            <span className="num">{formatDuration(node.duration)}</span>
          </span>
        ) : (
          <Tooltip>
            <Tooltip.Trigger>
              <span className="shrink-0 text-xs text-muted" data-testid="trace-duration-unknown">
                {t("trace.durationUnknown")}
              </span>
            </Tooltip.Trigger>
            <Tooltip.Content>{t("trace.durationUnknownHint")}</Tooltip.Content>
          </Tooltip>
        )}

        {/* Official: token icon + tokens, only shown when present and non-zero, with in/out split in the tooltip. */}
        {tokens != null && tokens > 0 ? (
          <Tooltip>
            <Tooltip.Trigger>
              <span
                className="flex shrink-0 items-center gap-0.5 text-xs text-muted"
                data-testid="trace-tokens"
              >
                <Icon icon="gravity-ui:layers" className="size-3" aria-hidden="true" />
                <span className="num">{formatNumber(tokens)}</span>
              </span>
            </Tooltip.Trigger>
            <Tooltip.Content>
              {t("trace.tokensBreakdown", {
                input: node.custom_tags?.input_tokens ?? 0,
                output: node.custom_tags?.output_tokens ?? 0,
              })}
            </Tooltip.Content>
          </Tooltip>
        ) : null}

        {/* ── Gantt track ──────────────────────────────────────────────
            **All rows share one time axis** (horizontal axis = the time window of the whole tree), so a bar's
            left edge = start time and width = duration. Two concurrent bars overlap horizontally,
            while serial ones are staggered: exactly the question a "share bar" cannot answer.

            The track width is hard-coded to 40% (instead of flex-1) so the axes of all rows **align exactly**.
            With flex-1, rows with long names would get a narrower track, and the same moment would land at
            different x positions in different rows, breaking the whole comparison. */}
        <div
          className="relative h-3 w-[40%] shrink-0 r-chip bg-surface-secondary"
          data-testid="trace-track"
        >
          {timing ? (
            <Tooltip>
              <Tooltip.Trigger>
                <div
                  className={`absolute inset-y-0 r-chip cursor-[var(--cursor-interactive)] ${
                    node.status === "error" ? "bg-danger" : "bg-accent"
                  }`}
                  data-testid="trace-bar"
                  data-offset={timing.offset.toFixed(4)}
                  data-width={timing.width.toFixed(4)}
                  style={{
                    left: `${(timing.offset * 100).toFixed(2)}%`,
                    width: `${(timing.width * 100).toFixed(2)}%`,
                  }}
                />
              </Tooltip.Trigger>
              {/* On hover, give the **exact** start → end. The bar itself can only express relative position;
                  "what hour, minute, second" needs a place where it can be read. */}
              <Tooltip.Content>
                {t("trace.barRange", {
                  start: clockOf(timing.startMs, language),
                  end: clockOf(timing.endMs, language),
                  dur: formatDuration(node.duration),
                })}
              </Tooltip.Content>
            </Tooltip>
          ) : node.started_at && win ? (
            // Duration cannot be computed (still running): only draw a start tick, **no long bar**.
            // Filling the length with "until now" would give a bar spanning the whole axis when viewing history from three days ago.
            <div
              className="absolute inset-y-0 w-0.5 bg-warning"
              data-testid="trace-bar-open"
              style={{
                left: `${Math.min(
                  Math.max(((Date.parse(node.started_at) - win.startMs) / win.spanMs) * 100, 0),
                  99.5,
                ).toFixed(2)}%`,
              }}
            />
          ) : null}
        </div>
      </div>
    </li>
  );
}

/**
 * Official `SpanDetail`: details of the selected span.
 *
 * Two parts, in its order:
 * 1. The field grid of `useSpanOverviewField`: Status / StatusCode / SpanID / Type /
 *    Latency / StartTime / Tokens;
 * 2. The two big Input / Output areas.
 *
 * The official cells are hard-coded at 224px wide; here a responsive grid is used instead: this panel hangs in the right column of the run detail page,
 * its width is not fixed, and a hard-coded width would overflow horizontally.
 */
function SpanDetail({ span }: { span: SpanNode }) {
  const { t, language } = usePrefs();
  const isError = span.status === "error";

  const fields: Array<{ key: string; title: string; value: React.ReactNode }> = [
    {
      key: "status",
      title: t("trace.fieldStatus"),
      value: (
        <Chip color={isError ? "danger" : "success"} size="sm" variant="soft">
          {isError ? t("trace.statusError") : t("trace.statusSuccess")}
        </Chip>
      ),
    },
    {
      key: "type",
      title: t("trace.fieldType"),
      value: span.span_type,
    },
    {
      key: "latency",
      title: t("trace.fieldLatency"),
      // The official one here is `${duration.toLocaleString()}ms`, always milliseconds. We keep formatDuration's
      // levels (19000ms reads better as 19.0s than 19,000ms), but when it cannot be computed we say "unknown"
      // instead of the official '-': "unknown" and "0" are two different things; see durationUnknownHint.
      value: (
        <span className={`num ${durationTone(span.duration)}`}>
          {span.duration == null ? t("trace.durationUnknown") : formatDuration(span.duration)}
        </span>
      ),
    },
    {
      key: "end_time",
      title: t("trace.fieldEndTime"),
      // End time = start + duration. **This cell is the final answer to the "concurrency" question**:
      // whether the [start, end] intervals of two spans overlap cannot be seen from durations alone;
      // both ends must be laid out. The Gantt bar gives intuition; this gives comparable numbers.
      value:
        span.started_at && span.duration != null ? (
          <span className="num">
            {clockOf(Date.parse(span.started_at) + span.duration, language)}
          </span>
        ) : (
          "—"
        ),
    },
    {
      key: "start_time",
      title: t("trace.fieldStartTime"),
      // ⚠️ **It must include milliseconds**, so formatDateTime is not used directly (it only goes to seconds).
      // Two concurrent calls may differ by only tens of milliseconds (tested: one pair differed by 16 microseconds), and at second precision they would
      // show as the same moment, while "who started first" is exactly what this cell must answer.
      // The date is still given by formatDateTime (runs crossing days must show which day).
      value: span.started_at ? (
        <span className="num">
          {formatDateTime(span.started_at, language)}
          <span className="text-muted">
            .{String(new Date(span.started_at).getMilliseconds()).padStart(3, "0")}
          </span>
        </span>
      ) : (
        "—"
      ),
    },
    {
      key: "tokens",
      title: t("trace.fieldTokens"),
      value:
        span.custom_tags?.tokens != null ? (
          <span className="num">{formatNumber(span.custom_tags.tokens)}</span>
        ) : (
          "—"
        ),
    },
    {
      key: "span_id",
      title: t("trace.fieldSpanId"),
      value: <code className="num break-all text-xs">{span.span_id}</code>,
    },
  ];

  // The official one has one more cell, status_code, with content only on failure. Only add this cell on failure:
  // on success it is always '-', a cell of pure noise.
  if (isError && span.status_code) {
    fields.push({
      key: "status_code",
      title: t("trace.fieldStatusCode"),
      value: <span className="text-danger">{span.status_code}</span>,
    });
  }

  return (
    <div className="mt-4" data-testid="trace-span-detail">
      <Separator className="mb-3" />

      {/* Single column on narrow screens. Each cell of this dl is `truncate`; with two columns at 402px each cell has only ~170px,
          and values like span names and trace ids would be cut to their first few characters, so the cell carries no information. */}
      <dl className="grid grid-cols-1 gap-x-4 gap-y-2 sm:grid-cols-2">
        {fields.map((f) => (
          <div key={f.key} className="flex min-w-0 flex-col gap-0.5">
            <dt className="text-xs text-muted">{f.title}</dt>
            <dd className="min-w-0 truncate text-xs">{f.value}</dd>
          </div>
        ))}
      </dl>

      {span.input ? <IoBlock title={t("trace.fieldInput")} body={span.input} kind="input" /> : null}
      {span.output ? (
        <IoBlock title={t("trace.fieldOutput")} body={span.output} kind="output" />
      ) : null}
    </div>
  );
}

/** Input / Output areas. The official ones are collapsible big blocks + a JSON viewer; here monospace preformatted text. */
function IoBlock({ title, body, kind }: { title: string; body: string; kind: "input" | "output" }) {
  return (
    <div className="mt-3 flex flex-col gap-1" data-testid={`trace-${kind}`}>
      <span className="text-xs text-muted">{title}</span>
      {/* The panel may be very narrow; long content must scroll horizontally instead of stretching the page.
          max-h + overflow: one grep output can have thousands of lines; without a cap it would push the details out of sight. */}
      <pre className="max-h-48 max-w-full overflow-auto r-inset bg-surface-secondary p-2 text-xs">
        {body}
      </pre>
    </div>
  );
}

/**
 * "hour:minute:second.millisecond" of a time.
 *
 * **Must include milliseconds**: two concurrent calls may differ by only tens of milliseconds (tested: one pair differed by 16 microseconds);
 * at second precision they would show as the same moment, and "who started first" could not be seen.
 * formatDateTime is not used: that one is for "which day"; here the whole tree is within the same minute,
 * and showing the date would only crowd out useful digits.
 */
function clockOf(ms: number, locale: string): string {
  const d = new Date(ms);
  const hms = d.toLocaleTimeString(locale, { hour12: false });
  return `${hms}.${String(d.getMilliseconds()).padStart(3, "0")}`;
}

// isSpanLike is re-exported from lib/traceTree so existing callers can keep using it.
export { isSpanLike };
