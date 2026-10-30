/**
 * Event view: **one row per raw event**; click to see details tailored by type.
 *
 * # How the work is split with the chat view
 *
 * The chat view (`ConversationView`) goes through `buildTimeline()`, where tool calls are paired into one entry; that is the
 * right form for it. The event view is the **debugging view** (DESIGN.md 3.6.3), meaning "faithfully show the raw events
 * returned by Ark", so it **bypasses that processing pipeline** and takes `RunEvent[]` directly; `agent.tool_use` and
 * `user.tool_result` each get their own row. This way the list matches the `run_events` table in the database one by one,
 * which is exactly the hard standard of "faithful".
 *
 * This leads to two deliberate trade-offs:
 * - **Entry count == raw event count** (no longer "a third fewer");
 * - **No derived durations**. A duration is "receipt time minus start time", a product of pairing; here instead it is computed once
 *   on `span.model_request_end` from `model_request_start_id` (the only pair with an explicit
 *   link field), and the details note that it includes queueing.
 *
 * # No inline expansion; details go in a side panel
 *
 * The detail shapes of the 34 event kinds differ a lot (Markdown body / JSON input / stop_reason / token usage);
 * expanding them inline would make a long run's list jump vertically too much to scan. So the list only does "scan at a glance"
 * (time · type · the one most informative summary line), and details are on the right (a drawer on narrow screens).
 *
 * # Raw JSON is a first-class citizen
 *
 * The old version folded it into a second-level Disclosure inside the row, and because of pairing it was an **array**. Here it is a permanent section
 * of the detail panel, and **always a single event**: when debugging, "what does this one really look like" should not take two levels of clicks.
 */

import { useEffect, useMemo, useRef, useState } from "react";
import {
  Chip,
  Drawer,
  Separator,
  Tooltip,
  useMediaQuery,
} from "@heroui/react";
import { Icon } from "@iconify/react";
import { MarkdownMessage } from "@/components/MarkdownMessage";
import { CopyButton } from "@/components/CopyButton";
import { parseToolResult } from "@/lib/toolResult";
import { formatDuration, formatTime, prettyJson } from "@/lib/format";
import { stopReasonLabel, type RawEventRow } from "@/lib/rawEvents";
import type { Tone } from "@/lib/timeline";
import type { ContentBlock, RunEvent } from "@/types/events";
import { contentToText } from "@/types/events";
import { usePrefs } from "@/prefs/PrefsContext";

const TONE_ICON_CLASS: Record<Tone, string> = {
  default: "text-foreground",
  muted: "text-muted",
  accent: "text-accent",
  success: "text-success",
  warning: "text-warning",
  danger: "text-danger",
};

// ---------------------------------------------------------------------------
// Common sections in the details
// ---------------------------------------------------------------------------

/** A section with a title and a copy button. All sections in the detail panel share this shape. */
function Section({
  label,
  hint,
  children,
  copyText,
  testId,
}: {
  label: string;
  hint?: string;
  children: React.ReactNode;
  copyText?: string;
  testId?: string;
}) {
  return (
    <div className="flex flex-col gap-1.5" data-testid={testId}>
      <div className="flex items-center gap-1.5">
        <span className="text-xs font-medium text-muted">{label}</span>
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
        {copyText ? (
          <span className="ml-auto">
            <CopyButton text={copyText} />
          </span>
        ) : null}
      </div>
      {children}
    </div>
  );
}

/** Monospace body block. Long output scrolls itself with max-h + overflow, without stretching the panel. */
function Pre({ text, tone }: { text: string; tone?: "danger" | "warning" }) {
  return (
    <pre
      className={[
        "max-h-72 overflow-auto whitespace-pre-wrap break-words r-inset p-3 text-xs leading-relaxed",
        tone === "danger"
          ? "bg-danger/5 text-danger"
          : tone === "warning"
            ? "bg-surface-secondary text-warning"
            : "bg-surface-secondary",
      ].join(" ")}
    >
      {text}
    </pre>
  );
}

/**
 * Markdown body.
 *
 * `agent.message` / `agent.thinking` go this way: they are Markdown already (headings, code blocks,
 * tables), and shown as plain text `**bold**` would appear with its asterisks. Reuses `MarkdownMessage`:
 * the only entry in the site allowed to import md-box (`React.lazy` isolates the 160KB core),
 * and dark mode relies on it passing mode explicitly inside: md-box does not look at `<html class="dark">`.
 */
function MarkdownSection({ label, text }: { label: string; text: string }) {
  return (
    <Section label={label} copyText={text} testId="evt-markdown">
      <div className="r-inset bg-surface-secondary p-3">
        <MarkdownMessage>{text}</MarkdownMessage>
      </div>
    </Section>
  );
}

/** Content block array: text goes through Markdown; non-text blocks (images/documents/search results) are listed separately. */
function ContentSection({
  label,
  blocks,
  markdown,
}: {
  label: string;
  blocks: ContentBlock[] | undefined;
  markdown: boolean;
}) {
  const { t } = usePrefs();
  const text = contentToText(blocks, t);
  if (!text) return <p className="text-xs text-muted">{t("evt.emptyContent")}</p>;
  if (markdown) return <MarkdownSection label={label} text={text} />;
  return (
    <Section label={label} copyText={text} testId="evt-content">
      <Pre text={text} />
    </Section>
  );
}

/** Tool input. The command line comes first: when debugging bash calls it is the one line you want to see first. */
function ToolInputSection({ input }: { input: unknown }) {
  const { t } = usePrefs();
  const json = prettyJson(input);
  // Tested: input is an object (the docs say a JSON string), so check the type before taking command.
  const command =
    input && typeof input === "object" && typeof (input as { command?: unknown }).command === "string"
      ? (input as { command: string }).command
      : null;

  return (
    <div className="flex flex-col gap-3">
      {command ? (
        <Section label={t("timeline.command")} copyText={command} testId="evt-command">
          <Pre text={command} />
        </Section>
      ) : null}
      <Section label={t("timeline.input")} copyText={json} testId="evt-input">
        <Pre text={json} />
      </Section>
    </div>
  );
}

/**
 * Tool result.
 *
 * The result text hides the container's execution details (exit code / stdout / stderr), parsed by `lib/toolResult.ts`
 * according to the format of `shared/infra/shelltools/bash.go`; if parsing fails, the whole text is shown as is (MCP and seed data
 * take this fallback path). **Do not guess just to "look structured".**
 */
function ToolResultSection({
  blocks,
  isError,
}: {
  blocks: ContentBlock[] | undefined;
  isError: boolean;
}) {
  const { t } = usePrefs();
  const text = contentToText(blocks, t);
  const parsed = parseToolResult(text);

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-2 text-xs">
        <Icon
          icon={isError ? "gravity-ui:circle-xmark" : "gravity-ui:circle-check"}
          className={isError ? "size-3.5 text-danger" : "size-3.5 text-success"}
        />
        <span className={isError ? "text-danger" : "text-muted"}>
          {isError ? t("timeline.execFailed") : t("timeline.execResult")}
        </span>
        {/* The exit code is **neutral** information, not the success criterion: grep exits 1 when nothing matches, and that is not a failure.
            So non-zero uses warning, not danger; success is still decided by the event's is_error. */}
        {parsed.exitCode != null || parsed.exitState ? (
          <Tooltip>
            <Tooltip.Trigger>
              <Chip
                size="sm"
                variant="soft"
                color={parsed.exitCode === 0 ? "success" : "warning"}
                data-testid="evt-exit-code"
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
        {parsed.truncated ? (
          <Tooltip>
            <Tooltip.Trigger>
              <Chip size="sm" variant="soft" color="warning" data-testid="evt-truncated">
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
            <Section
              label={t("timeline.sandboxError")}
              hint={t("timeline.sandboxErrorHint")}
              copyText={parsed.sandboxError}
              testId="evt-sandbox-error"
            >
              <Pre text={parsed.sandboxError} tone="danger" />
            </Section>
          ) : null}
          {parsed.stdout ? (
            <Section label={t("timeline.stdout")} copyText={parsed.stdout} testId="evt-stdout">
              <Pre text={parsed.stdout} />
            </Section>
          ) : null}
          {parsed.stderr ? (
            <Section label={t("timeline.stderr")} copyText={parsed.stderr} testId="evt-stderr">
              <Pre text={parsed.stderr} tone="warning" />
            </Section>
          ) : null}
          {!parsed.sandboxError && !parsed.stdout && !parsed.stderr ? (
            <p className="text-xs text-muted">{t("timeline.emptyResult")}</p>
          ) : null}
        </>
      ) : (
        <Section
          label={t("timeline.execResult")}
          copyText={text}
          testId="evt-result-raw"
        >
          <Pre text={text || t("timeline.emptyResult")} tone={isError ? "danger" : undefined} />
        </Section>
      )}
    </div>
  );
}

/** stop_reason is highlighted. Under self_hosted the meaning of requires_action must be explained right there. */
function StopReasonSection({ event }: { event: RunEvent }) {
  const { t } = usePrefs();
  const sr =
    "stop_reason" in event ? (event as { stop_reason?: Parameters<typeof stopReasonLabel>[0] }).stop_reason : undefined;
  if (!sr) return null;

  const isRequiresAction = sr.type === "requires_action";
  return (
    <Section label={t("evt.stopReason")} testId="evt-stop-reason">
      <div
        className={[
          "flex flex-col gap-1.5 r-inset p-3",
          isRequiresAction ? "bg-accent/5" : "bg-surface-secondary",
        ].join(" ")}
      >
        <div className="flex flex-wrap items-center gap-2">
          <Chip size="sm" variant="soft" color={isRequiresAction ? "accent" : "default"}>
            <Chip.Label className="font-mono">{sr.type}</Chip.Label>
          </Chip>
          <span className="text-xs text-foreground">{stopReasonLabel(sr, t)}</span>
        </div>
        {/*
          ⚠ requires_action **is not "waiting for user confirmation"**: under self_hosted Ark only does model inference,
          and tool calls are put into the work queue, picked up by amad's worker, and sent to a Docker container to run.
          So for every tool call the Ark side always turns idle and gives this stop_reason, meaning
          "I handed back control; waiting for you to return user.tool_result"; nobody is waiting for the user to click anything.
        */}
        {isRequiresAction ? (
          <p className="text-xs text-muted">{t("evt.requiresActionHint")}</p>
        ) : null}
        {sr.type === "requires_action" && sr.event_ids?.length ? (
          <p className="num text-xs text-muted">
            {t("evt.pendingEventIds", { ids: sr.event_ids.join(", ") })}
          </p>
        ) : null}
      </div>
    </Section>
  );
}

/** One "key: value" row. Used to list link fields like thread id and tool_use_id. */
function Field({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5 text-xs">
      <span className="text-muted">{label}</span>
      <span className={mono ? "break-all font-mono text-foreground" : "text-foreground"}>
        {value}
      </span>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Detail bodies tailored by type
// ---------------------------------------------------------------------------

/**
 * Detail body: **dispatched by event type**.
 *
 * 34 kinds cannot all look the same; here they are grouped by "what you want to see for this kind when debugging":
 * - Messages → Markdown rendering (user/system messages are plain text; only the agent side is Markdown)
 * - Tool start → command line + input JSON
 * - Tool receipt → success/failure + exit code + stdout/stderr sections
 * - Status → stop_reason highlighted
 * - Model request → token usage and duration
 */
function EventDetailBody({ row, rows }: { row: RawEventRow; rows: readonly RawEventRow[] }) {
  const { t } = usePrefs();
  const e = row.event;

  switch (e.type) {
    // ---- Chat: the agent side uses Markdown, the user/system side is plain text ----
    case "agent.message":
      return <ContentSection label={t("evt.body")} blocks={e.content} markdown />;
    case "agent.thinking":
      // content is optional. Tested: present most of the time (69 of 72); the empty ones are really empty.
      return e.content?.length ? (
        <ContentSection label={t("evt.thinkingBody")} blocks={e.content} markdown />
      ) : (
        <p className="text-xs text-muted">{t("evt.thinkingEmptyHint")}</p>
      );
    case "user.message":
    case "system.message":
      return <ContentSection label={t("evt.body")} blocks={e.content} markdown={false} />;
    case "user.interrupt":
      return e.reason ? (
        <Section label={t("evt.interruptReason")} copyText={e.reason}>
          <Pre text={e.reason} />
        </Section>
      ) : (
        <p className="text-xs text-muted">{t("evt.interruptNoReason")}</p>
      );

    // ---- Tool start ----
    case "agent.tool_use":
      return (
        <div className="flex flex-col gap-3">
          <Field label={t("evt.toolName")} value={e.name} mono />
          {/* Tested: tool_use_id is often missing (real Ark does not send it); when missing, the pairing key is the event's own id. */}
          <Field label={t("evt.toolUseId")} value={e.tool_use_id ?? `${e.id} ${t("evt.fallbackId")}`} mono />
          {e.evaluated_permission ? (
            <Field label={t("evt.permission")} value={e.evaluated_permission} mono />
          ) : null}
          <ToolInputSection input={e.input} />
        </div>
      );
    case "agent.mcp_tool_use":
      return (
        <div className="flex flex-col gap-3">
          <Field label={t("evt.mcpServer")} value={e.mcp_server_name} mono />
          <Field label={t("evt.toolName")} value={e.name} mono />
          <Field label={t("evt.toolUseId")} value={e.tool_use_id ?? `${e.id} ${t("evt.fallbackId")}`} mono />
          <ToolInputSection input={e.input} />
        </div>
      );
    case "agent.custom_tool_use":
      return (
        <div className="flex flex-col gap-3">
          <Field label={t("evt.toolName")} value={e.name} mono />
          <ToolInputSection input={e.input} />
        </div>
      );

    // ---- Tool receipt ----
    case "agent.tool_result":
    case "user.tool_result":
      return (
        <div className="flex flex-col gap-3">
          <Field label={t("evt.toolUseId")} value={e.tool_use_id} mono />
          <ToolResultSection blocks={e.content} isError={Boolean(e.is_error)} />
        </div>
      );
    case "user.custom_tool_result":
      return (
        <div className="flex flex-col gap-3">
          <Field label={t("evt.toolUseId")} value={e.custom_tool_use_id} mono />
          <ToolResultSection blocks={e.content} isError={Boolean(e.is_error)} />
        </div>
      );
    case "agent.mcp_tool_result":
      return (
        <div className="flex flex-col gap-3">
          {e.mcp_server_name ? (
            <Field label={t("evt.mcpServer")} value={e.mcp_server_name} mono />
          ) : null}
          <Field label={t("evt.toolUseId")} value={e.mcp_tool_use_id} mono />
          <ToolResultSection blocks={e.content} isError={Boolean(e.is_error)} />
        </div>
      );
    case "user.tool_confirmation":
      return (
        <div className="flex flex-col gap-3">
          <Field label={t("evt.toolUseId")} value={e.tool_use_id} mono />
          <div className="flex items-center gap-2">
            <Chip
              size="sm"
              variant="soft"
              color={e.result === "allow" ? "success" : "danger"}
              data-testid="evt-confirmation"
            >
              <Chip.Label>
                {e.result === "allow" ? t("timeline.approved") : t("timeline.denied")}
              </Chip.Label>
            </Chip>
          </div>
          {e.deny_message ? (
            <Section label={t("evt.denyMessage")} copyText={e.deny_message}>
              <Pre text={e.deny_message} tone="danger" />
            </Section>
          ) : null}
        </div>
      );

    // ---- Threads ----
    case "session.thread_created":
      return (
        <div className="flex flex-col gap-1.5">
          <Field label={t("evt.agentName")} value={e.agent_name} />
          <Field
            label={t("evt.parentThread")}
            value={e.parent_thread_id ?? t("evt.noParentThread")}
            mono
          />
        </div>
      );
    case "session.thread_status_running":
    case "session.thread_status_rescheduled":
    case "session.thread_status_terminated":
      return <Field label={t("evt.agentName")} value={e.agent_name} />;
    case "session.thread_status_idle":
      return (
        <div className="flex flex-col gap-3">
          <Field label={t("evt.agentName")} value={e.agent_name} />
          <StopReasonSection event={e} />
        </div>
      );
    case "agent.thread_message_sent":
      return (
        <div className="flex flex-col gap-3">
          <Field label={t("evt.toAgent")} value={e.to_agent_name ?? t("evt.mainAgent")} />
          <Field label={t("evt.toThread")} value={e.to_session_thread_id} mono />
          <ContentSection label={t("evt.body")} blocks={e.content} markdown />
        </div>
      );
    case "agent.thread_message_received":
      return (
        <div className="flex flex-col gap-3">
          <Field label={t("evt.fromAgent")} value={e.from_agent_name ?? t("evt.mainAgent")} />
          <Field label={t("evt.fromThread")} value={e.from_session_thread_id} mono />
          <ContentSection label={t("evt.body")} blocks={e.content} markdown />
        </div>
      );
    case "agent.thread_context_compacted":
      return e.summary ? (
        <MarkdownSection label={t("evt.compactedSummary")} text={e.summary} />
      ) : (
        <p className="text-xs text-muted">{t("evt.compactedNoSummary")}</p>
      );

    // ---- Session status ----
    case "session.status_idle":
    case "session.status_terminated":
      return <StopReasonSection event={e} />;
    case "session.status_running":
    case "session.status_rescheduled":
    case "session.deleted":
      return <p className="text-xs text-muted">{t("evt.noExtraFields")}</p>;
    case "session.updated":
      return <Field label={t("evt.sessionName")} value={e.name} />;

    // ---- Errors ----
    case "session.error":
      return (
        <div className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center gap-2">
            <Chip size="sm" variant="soft" color="danger" data-testid="evt-error-type">
              <Chip.Label className="font-mono">{e.error.type}</Chip.Label>
            </Chip>
            {/* Recoverable and unrecoverable must be told apart: retrying fixes itself, retries_exhausted does not. */}
            {e.error.retry_status ? (
              <Chip
                size="sm"
                variant="soft"
                color={e.error.retry_status.type === "retrying" ? "warning" : "danger"}
              >
                <Chip.Label>{t(`evt.retry_${e.error.retry_status.type}` as never)}</Chip.Label>
              </Chip>
            ) : null}
          </div>
          <Section label={t("evt.errorMessage")} copyText={e.error.message}>
            <Pre text={e.error.message} tone="danger" />
          </Section>
          {e.error.mcp_server_name ? (
            <Field label={t("evt.mcpServer")} value={e.error.mcp_server_name} mono />
          ) : null}
          {e.error.credential_id ? (
            <Field label={t("evt.credentialId")} value={e.error.credential_id} mono />
          ) : null}
        </div>
      );

    // ---- span: model request ----
    case "span.model_request_start":
      return <p className="text-xs text-muted">{t("evt.modelStartHint")}</p>;
    case "span.model_request_end": {
      /*
       * The duration is computed here: `model_request_start_id` is an **explicit** link field from Ark,
       * the only pair of spans that can be matched without guessing. If the start cannot be found (filtered out / not arrived yet), nothing is shown,
       * not 0: 0 would be read as "finished instantly".
       */
      const start = rows.find((r) => r.key === e.model_request_start_id);
      const durationMs =
        start?.atMs != null && row.atMs != null ? row.atMs - start.atMs : undefined;
      const u = e.model_usage;
      return (
        <div className="flex flex-col gap-3">
          <Field label={t("evt.startEventId")} value={e.model_request_start_id} mono />
          {durationMs != null ? (
            <Section label={t("evt.duration")} hint={t("timeline.durationHint")} testId="evt-duration">
              <p className="num text-sm text-foreground">{formatDuration(durationMs)}</p>
            </Section>
          ) : null}
          {e.is_error ? (
            <Section label={t("evt.errorMessage")} copyText={e.error_message ?? ""}>
              <Pre text={e.error_message || t("evt.noErrorMessage")} tone="danger" />
            </Section>
          ) : null}
          {u ? (
            <Section label={t("evt.usage")} testId="evt-usage">
              <div className="grid grid-cols-2 gap-x-4 gap-y-1.5">
                <Field label={t("evt.inputTokens")} value={String(u.input_tokens)} />
                <Field label={t("evt.outputTokens")} value={String(u.output_tokens)} />
                <Field label={t("evt.cacheRead")} value={String(u.cache_read_input_tokens)} />
                <Field label={t("evt.cacheWrite")} value={String(u.cache_creation_input_tokens)} />
                {u.speed ? <Field label={t("evt.speed")} value={u.speed} /> : null}
              </div>
            </Section>
          ) : null}
        </div>
      );
    }

    // ---- Outcome evaluation ----
    case "user.define_outcome":
      return (
        <div className="flex flex-col gap-3">
          <Field label={t("evt.outcomeId")} value={e.outcome_id} mono />
          {e.max_iterations != null ? (
            <Field label={t("evt.maxIterations")} value={String(e.max_iterations)} />
          ) : null}
          <Section label={t("evt.description")} copyText={e.description}>
            <Pre text={e.description} />
          </Section>
          <Section label={t("evt.rubric")}>
            <Pre
              text={
                e.rubric.type === "text" ? e.rubric.content : `file: ${e.rubric.file_id}`
              }
            />
          </Section>
        </div>
      );
    case "span.outcome_evaluation_start":
    case "span.outcome_evaluation_ongoing":
      return (
        <div className="flex flex-col gap-1.5">
          <Field label={t("evt.outcomeId")} value={e.outcome_id} mono />
          <Field label={t("evt.iterationLabel")} value={String(e.iteration)} />
        </div>
      );
    case "span.outcome_evaluation_end":
      return (
        <div className="flex flex-col gap-3">
          <Field label={t("evt.outcomeId")} value={e.outcome_id} mono />
          <Field label={t("evt.iterationLabel")} value={String(e.iteration)} />
          <div className="flex items-center gap-2">
            <Chip
              size="sm"
              variant="soft"
              color={e.result === "satisfied" ? "success" : "warning"}
              data-testid="evt-outcome-result"
            >
              <Chip.Label className="font-mono">{e.result}</Chip.Label>
            </Chip>
          </div>
          {e.explanation ? (
            <MarkdownSection label={t("evt.explanation")} text={e.explanation} />
          ) : null}
        </div>
      );
  }

  // Ark added a new type: the raw JSON section is still rendered below, so here we only say one line.
  return <p className="text-xs text-muted">{t("evt.unknownType")}</p>;
}

/** Content of the detail panel (title + metadata + tailored body + raw JSON). Shared by the drawer and the side panel. */
function EventDetail({ row, rows }: { row: RawEventRow; rows: readonly RawEventRow[] }) {
  const { t, language } = usePrefs();
  const json = JSON.stringify(row.event, null, 2);

  return (
    <div className="flex min-w-0 flex-col gap-4" data-testid="evt-detail">
      <div className="flex flex-col gap-2">
        <div className="flex flex-wrap items-center gap-2">
          <Icon icon={row.icon} className={`size-4 ${TONE_ICON_CLASS[row.tone]}`} />
          <span className="text-sm font-medium">{row.label}</span>
        </div>
        {/* The type name is given as is in monospace: when debugging you grep the database with it; a translated name cannot be found. */}
        <code className="break-all text-xs text-muted" data-testid="evt-detail-type">
          {row.type}
        </code>
      </div>

      <Separator />

      <div className="flex flex-col gap-1.5">
        <Field label={t("evt.eventId")} value={row.event.id} mono />
        <Field label={t("evt.sequence")} value={String(row.sequence)} mono />
        {/* The time gives the **complete raw** processed_at, not just HH:mm:ss: nanoseconds and the time zone offset
            are all in this string, while the list column is shortened so it can be scanned. */}
        <Field
          label={t("evt.processedAt")}
          value={row.processedAt ?? t("timeline.pending")}
          mono
        />
        {row.threadId ? <Field label={t("evt.thread")} value={row.threadId} mono /> : null}
      </div>

      <Separator />

      <EventDetailBody row={row} rows={rows} />

      <Separator />

      {/* Raw JSON is a permanent section, not a second-level fold: it is where "faithful" lands.
          Always a **single** event (the old version was an array because of pairing). */}
      <Section label={t("timeline.rawJson")} copyText={json} testId="evt-raw-json">
        <pre className="max-h-96 overflow-auto r-inset bg-surface-secondary p-3 text-xs leading-relaxed">
          {json}
        </pre>
      </Section>

      <p className="num text-xs text-muted">
        {row.processedAt ? formatTime(row.processedAt, language) : ""}
      </p>
    </div>
  );
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

/**
 * One row.
 *
 * Time · icon · type name · summary, all on one line; `truncate` keeps the row height constant:
 * the list must be quick to scan vertically, and once row heights change it can no longer be scanned.
 */
function EventRow({
  row,
  selected,
  showThread,
  onSelect,
}: {
  row: RawEventRow;
  selected: boolean;
  showThread: boolean;
  onSelect: () => void;
}) {
  const { t, language } = usePrefs();

  return (
    <button
      type="button"
      onClick={onSelect}
      // The testid uses the `evt-` prefix and **not** `event-`: existing e2e uses
      // `[data-testid^="event-"]` to count "paired entries", and two things with different meanings sharing one
      // prefix would turn those count assertions into a riddle.
      data-testid={`evt-row-${row.category}`}
      data-evt-type={row.type}
      data-evt-seq={row.sequence}
      aria-current={selected ? "true" : undefined}
      className={[
        "flex w-full items-center gap-2 r-row px-2 py-1.5 text-left cursor-[var(--cursor-interactive)]",
        selected ? "bg-surface-secondary shadow-surface" : "hover:bg-surface-secondary/60",
      ].join(" ")}
    >
      <span className="num shrink-0 text-xs text-muted">
        {row.processedAt ? (
          formatTime(row.processedAt, language)
        ) : (
          <span className="text-warning">{t("timeline.pending")}</span>
        )}
      </span>
      <Icon icon={row.icon} className={`size-3.5 shrink-0 ${TONE_ICON_CLASS[row.tone]}`} />
      <span className="shrink-0 text-xs font-medium">{row.label}</span>
      {showThread && row.threadId ? (
        <span className="max-w-28 shrink-0 truncate font-mono text-xs text-muted">
          {row.threadId}
        </span>
      ) : null}
      {row.summary ? (
        <span className="min-w-0 flex-1 truncate text-xs text-muted">{row.summary}</span>
      ) : (
        <span className="min-w-0 flex-1" />
      )}
      <Icon
        icon="gravity-ui:chevron-right"
        className="size-3 shrink-0 text-muted opacity-0 group-hover:opacity-100"
      />
    </button>
  );
}

export interface RawEventListProps {
  rows: RawEventRow[];
  /** In multi-thread runs, the thread is marked in the row; not marked for a single thread or when already filtered by thread (it is redundant then). */
  showThread: boolean;
}

/**
 * Main part of the event view.
 *
 * Wide screens: list on the left + details on the right (details sticky, not moving when the list scrolls); narrow screens: list + drawer.
 * `useMediaQuery` **renders one of the two** instead of CSS rendering both and hiding one:
 * the latter makes the same testid appear twice in the DOM, and Playwright strict mode reports
 * "found 2 elements", with the error not pointing to "you rendered it twice" at all.
 */
export function RawEventList({ rows, showThread }: RawEventListProps) {
  const { t } = usePrefs();
  const isWide = useMediaQuery("(min-width: 1024px)");
  const [selectedKey, setSelectedKey] = useState<string | null>(null);

  /**
   * The selected row. **Looked up by key instead of storing the whole row**: after filters change, the old object becomes stale
   * (the reference is still there, but it is no longer in the list), and storing the object would leave the details on an event already filtered out.
   */
  const selected = useMemo(
    () => rows.find((r) => r.key === selectedKey) ?? null,
    [rows, selectedKey],
  );

  // Clear the selection when the selected row is filtered out, otherwise the narrow-screen drawer would hold an invisible event.
  const prevHas = useRef(true);
  useEffect(() => {
    const has = selectedKey != null && rows.some((r) => r.key === selectedKey);
    if (selectedKey != null && !has && prevHas.current) setSelectedKey(null);
    prevHas.current = has;
  }, [rows, selectedKey]);

  if (rows.length === 0) {
    return (
      <div className="r-container border border-dashed border-border py-12 text-center">
        <p className="text-sm text-muted">{t("timeline.noEvents")}</p>
      </div>
    );
  }

  const list = (
    <div className="flex min-w-0 flex-col gap-0.5" data-testid="evt-list">
      {rows.map((row) => (
        <EventRow
          key={row.key}
          row={row}
          selected={selected?.key === row.key}
          showThread={showThread}
          onSelect={() => setSelectedKey(row.key)}
        />
      ))}
    </div>
  );

  if (!isWide) {
    return (
      <>
        {list}
        {/*
          Controlled drawer: pass isOpen/onOpenChange only to `Drawer.Backdrop`, **without the `<Drawer>` root**
          (the root renders RAC's DialogTrigger, which is the uncontrolled path).
          Clear the selection on close, otherwise isOpen and selected get out of sync.
        */}
        <Drawer.Backdrop
          isOpen={selected != null}
          onOpenChange={(open) => {
            if (!open) setSelectedKey(null);
          }}
        >
          <Drawer.Content placement="right">
            {/* The drawer is fixed inset-0, so its top and bottom edges are the physical edges of the screen: without safe-area
                the top is covered by the Dynamic Island and the bottom by the Home Indicator.
                Same on the right: this drawer slides in from the right, and in landscape the notch may be on that side. */}
            <Drawer.Dialog
              className="flex w-96 max-w-[92vw] flex-col gap-3 overflow-y-auto px-4 py-4"
              style={{
                paddingTop: "calc(var(--safe-top) + 1rem)",
                paddingBottom: "calc(var(--safe-bottom) + 1rem)",
                paddingRight: "calc(var(--safe-right) + 1rem)",
              }}
              aria-label={t("evt.detailTitle")}
              data-testid="evt-detail-drawer"
            >
              {selected ? <EventDetail row={selected} rows={rows} /> : null}
            </Drawer.Dialog>
          </Drawer.Content>
        </Drawer.Backdrop>
      </>
    );
  }

  return (
    <div className="grid grid-cols-[minmax(0,1fr)_minmax(0,26rem)] gap-4">
      {list}
      {/* The detail column is sticky: when scrolling a long list the details follow, so there is no need to scroll back.
          It has no scroll shell of its own: the outer layer (chat page/run detail page) is the scroll container. */}
      <aside className="sticky top-0 self-start">
        {selected ? (
          <div className="r-container bg-surface p-4 shadow-surface">
            <EventDetail row={selected} rows={rows} />
          </div>
        ) : (
          <div className="r-container border border-dashed border-border p-6 text-center">
            <p className="text-xs text-muted">{t("evt.pickOne")}</p>
          </div>
        )}
      </aside>
    </div>
  );
}
