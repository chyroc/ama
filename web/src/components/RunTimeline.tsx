/**
 * Event timeline (the main part of the Trace view).
 *
 * This is the debugging view of run history: a vertical stream of events, each with an icon, time and semantic color, expandable to see the body and
 * the raw payload. Tool calls are already paired into single entries in buildTimeline.
 *
 * The noise-reduction controls (33 event kinds cannot be read unfiltered) are in `TimelineToolbar`, and the filter state goes into the URL;
 * this file only renders, plus two controls reused by the toolbar (CategoryFilter / ThreadSwitcher).
 *
 * The result area of a tool call lays out the container execution details: exit code, stdout, stderr. **Those are not structured
 * fields from the backend**: they are hidden in the result text, parsed by `lib/toolResult.ts` according to
 * the format of `shared/infra/shelltools/bash.go`; if parsing fails, the whole text is shown as is.
 *
 * The skeleton uses components/ui/Timeline: track, dots, connecting lines and spacing are all handled by it; no more hand-written absolutely positioned
 * connecting lines.
 *
 * ⚠ The result area has no syntax highlighting: tool results are mostly plain-text logs, and a highlighter would add
 * tens of KB to the main chunk for little gain. If highlighted code is needed later, lazy import it on demand separately.
 */

import { useEffect, useRef, useState } from "react";
import { Button, Chip, Disclosure, Separator, ToggleButton, Tooltip } from "@heroui/react";
import { Timeline, type TimelineStatus } from "@/components/ui/Timeline";
import { Icon } from "@iconify/react";
import type { TimelineItem, Tone } from "@/lib/timeline";
import { SLOW_TOOL_MS } from "@/lib/eventFacets";
import { parseToolResult } from "@/lib/toolResult";
import { formatDuration, formatTime } from "@/lib/format";
import { usePrefs } from "@/prefs/PrefsContext";
import { copyText } from "@/lib/clipboard";

const TONE_ICON_CLASS: Record<Tone, string> = {
  default: "text-foreground",
  muted: "text-muted",
  accent: "text-accent",
  success: "text-success",
  warning: "text-warning",
  danger: "text-danger",
};

/**
 * Tone → the semantic level of the Timeline marker.
 *
 * The two enums map almost one to one; only accent is called current in Timeline (it means "this is the step in progress",
 * which is exactly what AMA uses accent for).
 *
 * With status given, Timeline derives the marker's fill and border with color-mix, so the old
 * TONE_RING_CLASS (bg-accent/10 and the like) is no longer needed: a translucent fill on a translucent fill lets the connecting line show through.
 */
const TONE_STATUS: Record<Tone, TimelineStatus> = {
  default: "default",
  muted: "muted",
  accent: "current",
  success: "success",
  warning: "warning",
  danger: "danger",
};

/**
 * Copy button.
 *
 * The repo had no copy feature before, so this builds a minimal one. Two points that must be handled:
 *
 * 1. **`navigator.clipboard` may not exist.** It only exists in a secure context (https or localhost);
 *    when accessed via `http://<hostname or intranet IP>` it is undefined. So we use copyText from `lib/clipboard`
 *    (it falls back to execCommand), and when both fail we show visible feedback instead of failing silently.
 * 2. **Feedback must show on the button.** A successful copy has no system notice, so the user cannot tell if it worked;
 *    so the icon briefly turns into a check mark. The timer is cleared on unmount, otherwise it would setState on an unmounted component.
 */
function CopyButton({ text, label }: { text: string; label?: string }) {
  const { t } = usePrefs();
  const [state, setState] = useState<"idle" | "done" | "failed">("idle");
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => () => {
    if (timer.current) clearTimeout(timer.current);
  }, []);

  const copy = async () => {
    // In a non-secure context (http + not localhost) there is no navigator.clipboard, and copyText falls back to
    // execCommand; it only fails when both fail.
    setState((await copyText(text)) ? "done" : "failed");
    if (timer.current) clearTimeout(timer.current);
    timer.current = setTimeout(() => setState("idle"), 2000);
  };

  const hint =
    state === "done" ? t("timeline.copied") : state === "failed" ? t("timeline.copyFailed") : (label ?? t("timeline.copy"));

  return (
    <Tooltip>
      <Tooltip.Trigger>
        <Button
          size="sm"
          variant="ghost"
          isIconOnly
          aria-label={hint}
          onPress={() => void copy()}
          data-testid="copy-button"
          // The copy result also goes into a data attribute: the tooltip only renders on hover,
          // so relying on it alone, "copied" is not observable at all without a mouse (including e2e).
          data-copy-state={state}
        >
          <Icon
            icon={state === "done" ? "gravity-ui:copy-check" : "gravity-ui:copy"}
            className={state === "failed" ? "size-3.5 text-danger" : "size-3.5"}
          />
        </Button>
      </Tooltip.Trigger>
      <Tooltip.Content>{hint}</Tooltip.Content>
    </Tooltip>
  );
}

/** A log block with a title and copy button. stdout / stderr / container errors share this shape. */
function LogSection({
  label,
  text,
  tone,
  hint,
  testId,
}: {
  label: string;
  text: string;
  tone?: "danger" | "warning";
  hint?: string;
  testId: string;
}) {
  return (
    <div className="flex flex-col gap-1" data-testid={testId}>
      <div className="flex items-center gap-1.5">
        <span
          className={[
            "text-xs font-medium",
            tone === "danger" ? "text-danger" : tone === "warning" ? "text-warning" : "text-muted",
          ].join(" ")}
        >
          {label}
        </span>
        {hint ? (
          <Tooltip>
            <Tooltip.Trigger>
              <span className="cursor-help text-muted">
                <Icon icon="gravity-ui:circle-info" className="size-3" />
              </span>
            </Tooltip.Trigger>
            <Tooltip.Content>{hint}</Tooltip.Content>
          </Tooltip>
        ) : null}
        <span className="ml-auto">
          <CopyButton text={text} />
        </span>
      </div>
      <pre
        className={[
          "max-h-64 overflow-auto whitespace-pre-wrap r-inset p-3 text-xs leading-relaxed",
          tone === "danger" ? "bg-danger/5 text-danger" : "bg-surface-secondary",
        ].join(" ")}
      >
        {text}
      </pre>
    </div>
  );
}

function RawPayload({ item }: { item: TimelineItem }) {
  const { t } = usePrefs();
  const json = JSON.stringify(item.raw, null, 2);
  return (
    <Disclosure>
      <Disclosure.Heading>
        <Disclosure.Trigger className="flex items-center gap-1.5 text-xs text-muted cursor-[var(--cursor-interactive)] hover:text-foreground">
          <Disclosure.Indicator>
            <Icon icon="gravity-ui:chevron-right" className="size-3" />
          </Disclosure.Indicator>
          {t("timeline.rawPayload")}
          <span className="num">({item.raw.length})</span>
        </Disclosure.Trigger>
      </Disclosure.Heading>
      <Disclosure.Content>
        <Disclosure.Body>
          <div className="mt-2 flex flex-col gap-1">
            <div className="flex items-center gap-1.5">
              <span className="text-xs text-muted">{t("timeline.rawJson")}</span>
              <span className="ml-auto">
                <CopyButton text={json} />
              </span>
            </div>
            <pre className="max-h-80 overflow-auto r-inset bg-surface-secondary p-3 text-xs leading-relaxed">
              {json}
            </pre>
          </div>
        </Disclosure.Body>
      </Disclosure.Content>
    </Disclosure>
  );
}

/**
 * Inputs and result of a tool call.
 *
 * The result text hides the container's execution details (exit code / stdout / stderr), which `lib/toolResult.ts`
 * parses out and shows in sections. Those are not structured fields from the backend; the format matches `formatResult()` in
 * `shared/infra/shelltools/bash.go` exactly. **If parsing fails, the whole text is shown as is** (MCP, read/write/grep and
 * seed data all take this fallback path); do not guess just to "look structured".
 */
function ToolResultBlock({
  item,
  result,
}: {
  item: TimelineItem;
  result: NonNullable<TimelineItem["result"]>;
}) {
  const { t } = usePrefs();
  if (result.pending) {
    // No result yet has two cases that must be worded differently: the tool running in the container (executing) is "waiting for the result",
    // waiting for user approval is "waiting for you". Writing both as "waiting for the result" would make the latter look like it will resolve by itself.
    const awaitingUser = item.permission === "ask" && !item.confirmation;
    return (
      <div
        className={[
          "flex items-center gap-2 r-inset border border-dashed px-3 py-2 text-xs",
          awaitingUser ? "border-warning text-warning" : "border-border text-muted",
        ].join(" ")}
      >
        <Icon
          icon={awaitingUser ? "gravity-ui:triangle-exclamation" : "gravity-ui:clock"}
          className="size-3.5 shrink-0"
        />
        {awaitingUser ? t("timeline.awaitingYou") : t("timeline.waitingResult")}
      </div>
    );
  }

  const parsed = parseToolResult(result.text);
  const slow = item.durationMs != null && item.durationMs > SLOW_TOOL_MS;

  return (
    <div className="flex flex-col gap-2">
      {/* Status row: success/failure, exit code, duration, truncation, all at a glance */}
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs">
        <Icon
          icon={result.isError ? "gravity-ui:circle-xmark" : "gravity-ui:circle-check"}
          className={result.isError ? "size-3.5 text-danger" : "size-3.5 text-success"}
        />
        <span className={result.isError ? "text-danger" : "text-muted"}>
          {result.isError ? t("timeline.execFailed") : t("timeline.execResult")}
        </span>

        {/*
          The exit code is **neutral** information, not the success criterion: grep exits 1 when nothing matches, and that is not a failure
          (shared/infra/shelltools/search.go explicitly handles it this way). So non-zero uses warning, not
          danger; success is still decided by the event's own is_error (the row above).
        */}
        {parsed.exitCode != null || parsed.exitState ? (
          <Tooltip>
            <Tooltip.Trigger>
              <Chip
                size="sm"
                variant="soft"
                color={parsed.exitCode === 0 ? "success" : "warning"}
                data-testid="tool-exit-code"
              >
                <Chip.Label>
                  {parsed.exitState === "not_exited"
                    ? t("timeline.notExited")
                    : t("timeline.exitCode", { code: parsed.exitCode ?? "" })}
                </Chip.Label>
              </Chip>
            </Tooltip.Trigger>
            <Tooltip.Content>{t("timeline.exitCodeHint")}</Tooltip.Content>
          </Tooltip>
        ) : null}

        {item.durationMs != null ? (
          <Tooltip>
            <Tooltip.Trigger>
              <Chip
                size="sm"
                variant="soft"
                color={slow ? "warning" : "default"}
                data-testid="tool-duration"
              >
                <Chip.Label className="num">
                  {formatDuration(item.durationMs)}
                  {slow ? ` · ${t("timeline.slow")}` : ""}
                </Chip.Label>
              </Chip>
            </Tooltip.Trigger>
            <Tooltip.Content>{t("timeline.durationHint")}</Tooltip.Content>
          </Tooltip>
        ) : null}

        {parsed.truncated ? (
          <Tooltip>
            <Tooltip.Trigger>
              <Chip size="sm" variant="soft" color="warning" data-testid="tool-truncated">
                <Chip.Label>{t("timeline.truncated")}</Chip.Label>
              </Chip>
            </Tooltip.Trigger>
            <Tooltip.Content>{t("timeline.truncatedHint")}</Tooltip.Content>
          </Tooltip>
        ) : null}
      </div>

      {parsed.structured ? (
        <>
          {parsed.sandboxError ? (
            <LogSection
              label={t("timeline.sandboxError")}
              text={parsed.sandboxError}
              tone="danger"
              hint={t("timeline.sandboxErrorHint")}
              testId="tool-sandbox-error"
            />
          ) : null}
          {parsed.stdout ? (
            <LogSection
              label={t("timeline.stdout")}
              text={parsed.stdout}
              testId="tool-stdout"
            />
          ) : null}
          {parsed.stderr ? (
            <LogSection
              label={t("timeline.stderr")}
              text={parsed.stderr}
              tone="warning"
              testId="tool-stderr"
            />
          ) : null}
          {/* When all three sections are empty (the command printed nothing), still say so, otherwise expanding shows a blank */}
          {!parsed.sandboxError && !parsed.stdout && !parsed.stderr ? (
            <p className="text-xs text-muted">{t("timeline.emptyResult")}</p>
          ) : null}
        </>
      ) : (
        <LogSection
          label={t("timeline.execResult")}
          text={result.text || t("timeline.emptyResult")}
          tone={result.isError ? "danger" : undefined}
          testId="tool-result-raw"
        />
      )}
    </div>
  );
}

function TimelineRow({ item, showThread }: { item: TimelineItem; showThread: boolean }) {
  const { t, language } = usePrefs();
  const [expanded, setExpanded] = useState(item.defaultExpanded);
  const hasDetail = Boolean(item.body) || Boolean(item.result);
  const slow = item.durationMs != null && item.durationMs > SLOW_TOOL_MS;
  /**
   * Really waiting for the user to click. The check is `ask` **and no confirmation yet**: checking only `ask` would leave calls already
   * approved stuck at "awaiting your confirmation" forever (the write in seed run_1 has exactly this shape: ask + allow).
   *
   * ⚠ Do not replace it with `stop_reason === "requires_action"`: under self_hosted every tool call
   * produces that kind of idle (meaning "Ark hands back control and waits for our worker to return a result"), with nobody waiting for
   * the user. That path is now merged into the tool entry and shown as "running"; see lib/timeline.ts.
   */
  const awaitingUser = item.permission === "ask" && !item.confirmation;

  return (
    <Timeline.Item
      status={TONE_STATUS[item.tone]}
      data-testid={`event-${item.category}`}
      // Slow calls must be locatable in the collapsed state too. Use a data attribute instead of changing the testid:
      // existing e2e selects rows entirely by event-<category>, and changing it would break a whole set of cases.
      data-slow={slow ? "true" : undefined}
    >
      <Timeline.Rail>
        {/*
         * The marker's fill/border is derived by Timeline from status (color-mix gives 12% fill + 55% border);
         * do not add another translucent fill: both are translucent, and stacked they let the connecting line show through the dot.
         * The icon size is also controlled by Timeline (the svg in Marker is fixed at 55%), so no size-4.
         */}
        <Timeline.Marker>
          <Icon icon={item.icon} className={TONE_ICON_CLASS[item.tone]} />
        </Timeline.Marker>
        <Timeline.Connector />
      </Timeline.Rail>

      <Timeline.Content className="flex min-w-0 flex-col gap-1.5 pb-5">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
          <span className="text-sm font-medium">{item.title}</span>

          {awaitingUser ? (
            <Chip size="sm" color="warning" variant="primary" data-testid="tool-needs-confirm">
              <Chip.Label>{t("timeline.needsConfirm")}</Chip.Label>
            </Chip>
          ) : null}
          {item.confirmation ? (
            <Chip
              size="sm"
              color={item.confirmation.result === "allow" ? "success" : "danger"}
              variant="soft"
            >
              <Chip.Label>
                {item.confirmation.result === "allow" ? t("timeline.approved") : t("timeline.denied")}
              </Chip.Label>
            </Chip>
          ) : null}
          {/*
            Running: the tool runs in AMA's own container, and the requires_action idle on the Ark side has already
            been merged into this entry (see lib/timeline.ts). Neutral color + a tooltip making clear "Ark idle ≠ stopped":
            this is where people debugging are most likely to draw the wrong conclusion when they see the Ark status is idle.
          */}
          {item.executing ? (
            <Tooltip>
              <Tooltip.Trigger>
                <Chip size="sm" variant="soft" data-testid="tool-executing">
                  <Chip.Label>{t("timeline.executing")}</Chip.Label>
                </Chip>
              </Tooltip.Trigger>
              <Tooltip.Content>{t("timeline.executingHint")}</Tooltip.Content>
            </Tooltip>
          ) : null}
          {showThread && item.threadId ? (
            <span className="truncate font-mono text-xs text-muted">{item.threadId}</span>
          ) : null}

          {/*
            The duration is in the title row of the collapsed state: when looking for "which step it is stuck on", you should not have to expand each one.

            ⚠ The testid is `row-duration` and **must not** be `event-duration`: e2e uses
            `[data-testid^="event-"]` to count timeline entries, and any new testid starting with event-
            would be counted as an event. That time it showed as "the entry count did not change after filtering by thread",
            and the error did not point to the real cause at all.
          */}
          {item.durationMs != null ? (
            <Tooltip>
              <Tooltip.Trigger>
                <span
                  className={[
                    "num flex cursor-help items-center gap-1 text-xs",
                    slow ? "text-warning" : "text-muted",
                  ].join(" ")}
                  data-testid="row-duration"
                >
                  <Icon icon="gravity-ui:hourglass" className="size-3" />
                  {formatDuration(item.durationMs)}
                </span>
              </Tooltip.Trigger>
              <Tooltip.Content>{t("timeline.durationHint")}</Tooltip.Content>
            </Tooltip>
          ) : null}

          <span className="ml-auto shrink-0 num text-xs text-muted">
            {item.processedAt ? (
              formatTime(item.processedAt, language)
            ) : (
              <Tooltip>
                <Tooltip.Trigger>
                  <span className="cursor-help">{t("timeline.pending")}</span>
                </Tooltip.Trigger>
                <Tooltip.Content>{t("timeline.pendingHint")}</Tooltip.Content>
              </Tooltip>
            )}
          </span>
        </div>

        {item.summary && !expanded ? (
          <p className="truncate text-sm text-muted">{item.summary}</p>
        ) : null}

        {expanded && item.body ? (
          item.category === "tool" ? (
            <div className="flex flex-col gap-2">
              {/* The bash command line is pulled to the front: when debugging it is the one line you want to see first;
                  buried in the input JSON you would have to read through a layer of braces first. Non-bash tools have no command; skipped. */}
              {item.command ? (
                <div className="flex flex-col gap-1" data-testid="tool-command">
                  <div className="flex items-center gap-1.5">
                    <span className="text-xs text-muted">{t("timeline.command")}</span>
                    <span className="ml-auto">
                      <CopyButton text={item.command} />
                    </span>
                  </div>
                  <pre className="overflow-auto whitespace-pre-wrap r-inset bg-surface-secondary p-3 font-mono text-xs leading-relaxed">
                    {item.command}
                  </pre>
                </div>
              ) : null}
              <LogSection label={t("timeline.input")} text={item.body} testId="tool-input" />
            </div>
          ) : (
            <pre
              className={[
                "overflow-auto whitespace-pre-wrap r-inset bg-surface-secondary p-3 text-sm leading-relaxed",
                item.bodyIsCode ? "text-xs" : "",
              ].join(" ")}
            >
              {item.body}
            </pre>
          )
        ) : null}

        {expanded && item.confirmation?.denyMessage ? (
          <p className="text-xs text-danger">
            {t("timeline.denyReason", { msg: item.confirmation.denyMessage })}
          </p>
        ) : null}

        {expanded && item.result ? <ToolResultBlock item={item} result={item.result} /> : null}

        <div className="flex items-center gap-3">
          {hasDetail ? (
            <button
              type="button"
              className="flex items-center gap-1 text-xs text-muted cursor-[var(--cursor-interactive)] hover:text-foreground"
              onClick={() => setExpanded((v) => !v)}
              data-testid="toggle-event"
            >
              <Icon
                icon="gravity-ui:chevron-right"
                className={`size-3 transition-none ${expanded ? "rotate-90" : ""}`}
              />
              {expanded ? t("timeline.collapse") : t("timeline.expand")}
            </button>
          ) : null}
          {expanded ? <RawPayload item={item} /> : null}
        </div>
      </Timeline.Content>
    </Timeline.Item>
  );
}

export function RunTimeline({
  items,
  showThread,
}: {
  items: TimelineItem[];
  showThread: boolean;
}) {
  const { t } = usePrefs();
  if (items.length === 0) {
    return (
      <div className="r-container border border-border border-dashed py-12 text-center">
        <p className="text-sm text-muted">{t("timeline.noEvents")}</p>
      </div>
    );
  }

  return (
    <Timeline density="comfortable" data-testid="timeline">
      {items.map((it) => (
        <TimelineRow key={it.key} item={it} showThread={showThread} />
      ))}
    </Timeline>
  );
}

/** Category filter. Uses a standalone toggle group instead of a dropdown, to show at a glance how many entries each kind has. */
export function CategoryFilter({
  counts,
  selected,
  onToggle,
  labels,
}: {
  counts: Record<string, number>;
  selected: Set<string>;
  onToggle: (key: string) => void;
  labels: Record<string, string>;
}) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      {Object.entries(labels).map(([key, label]) => {
        const count = counts[key] ?? 0;
        if (count === 0) return null;
        return (
          <ToggleButton
            key={key}
            size="sm"
            isSelected={selected.has(key)}
            onChange={() => onToggle(key)}
            data-testid={`filter-${key}`}
          >
            {label}
            <span className="ml-1.5 num text-xs opacity-70">{count}</span>
          </ToggleButton>
        );
      })}
    </div>
  );
}

/** Thread switcher. Not rendered with a single thread, to avoid a pointless control. */
export function ThreadSwitcher({
  threads,
  active,
  onChange,
}: {
  threads: { id: string; agent_name: string; parent_thread_id: string | null }[];
  active: string | null;
  onChange: (id: string | null) => void;
}) {
  const { t } = usePrefs();
  if (threads.length <= 1) return null;

  return (
    <div className="flex flex-wrap items-center gap-2">
      <span className="text-xs text-muted">{t("run.threads")}</span>
      <Button
        size="sm"
        variant={active === null ? "secondary" : "ghost"}
        onPress={() => onChange(null)}
        data-testid="thread-all"
      >
        {t("common.all")}
      </Button>
      <Separator orientation="vertical" className="h-5" />
      {threads.map((th) => (
        <Button
          key={th.id}
          size="sm"
          variant={active === th.id ? "secondary" : "ghost"}
          onPress={() => onChange(th.id)}
          data-testid={`thread-${th.id}`}
        >
          {th.agent_name}
          {th.parent_thread_id ? (
            <Icon icon="gravity-ui:branches-right" className="ml-1 size-3 opacity-60" />
          ) : null}
        </Button>
      ))}
    </div>
  );
}
