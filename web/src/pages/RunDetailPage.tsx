/**
 * Run detail: two ways to read one run.
 *
 * A fixed summary bar at the top (status, duration, tokens, thread count) is in both views; below
 * it splits by `?view=`:
 * **the default is the conversation view** (folded by turn, see ConversationView); switching to
 * the event view gives the multi-axis filterable
 * stream of 33 event kinds (the Trace view: domain / category / concrete type + keyword + slow
 * calls + failures, see
 * TimelineToolbar). Filter state goes into the URL like `?view=`, so links can be shared and
 * refresh keeps it.
 *
 * Token usage follows the events (summing model_usage of span.model_request_end);
 * the copy on the Run is only a cache.
 */

import { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { Button, Card, Chip, Separator } from "@heroui/react";
import { Icon } from "@iconify/react";
import { api } from "@/data/api";
import type { ArkConsoleRef } from "@/data/api";
import { ArkConsoleLink } from "@/components/ArkConsoleLink";
import { useWorkspacePath } from "@/app/WorkspaceGate";
import {
  AgentDeletedChip,
  AsyncErrorBlock,
  Empty,
  LoadingBlock,
  PageBody,
  PageHeader,
} from "@/components/Page";
import { ThreadSwitcher } from "@/components/RunTimeline";
import { RawEventList } from "@/components/RawEventList";
import { TimelineToolbar, type ToolbarCounts } from "@/components/TimelineToolbar";
import { ConversationView } from "@/components/ConversationView";
import { TracePanel } from "@/components/TracePanel";
import { SandboxPanel } from "@/components/SandboxPanel";
import { ViewSwitch, useRunView } from "@/components/ViewSwitch";
import { buildTimeline, computeStats } from "@/lib/timeline";
import { applyRawFilter, buildRawHaystack, useTimelineFilter } from "@/lib/useTimelineFilter";
import { buildRawRows, rawEventStats } from "@/lib/rawEvents";
import { useAsyncData } from "@/lib/useAsyncData";
import { formatDateTime, formatDuration, formatNumber, formatTokens } from "@/lib/format";
import type { RunEvent, RunEventType } from "@/types/events";
import { RunStatusChip } from "@/components/RunStatusChip";
import { runAwaitsUser } from "@/pages/RunsPage";
import { usePrefs } from "@/prefs/PrefsContext";


/** One cell on the summary bar. */
function Stat({
  label,
  value,
  hint,
  tone,
}: {
  label: string;
  value: string;
  hint?: string;
  tone?: "danger" | "warning";
}) {
  return (
    <div className="flex flex-col gap-1">
      <span className="text-xs text-muted">{label}</span>
      <span
        className={[
          "num text-lg leading-none font-semibold",
          tone === "danger" ? "text-danger" : tone === "warning" ? "text-warning" : "",
        ].join(" ")}
      >
        {value}
      </span>
      {hint ? <span className="num text-xs text-muted">{hint}</span> : null}
    </div>
  );
}

export function RunDetailPage() {
  const { t, language } = usePrefs();
  const { runId } = useParams<{ runId: string }>();
  const wp = useWorkspacePath();

  const [view, setView] = useRunView();

  /*
   * The reference used by "Open in Ark console". **Fetched on its own, not merged into the fetch
   * below**:
   * that one is all-or-nothing, and putting it in would let an optional link bring down the whole
   * detail page.
   * On failure treat it as absent (do not render the link), with no error and no retry.
   */
  const [arkConsole, setArkConsole] = useState<ArkConsoleRef | null>(null);
  useEffect(() => {
    if (!runId) return;
    let alive = true;
    void api
      .getRunArkConsole(runId)
      .then((ref) => {
        if (alive) setArkConsole(ref);
      })
      .catch(() => {
        if (alive) setArkConsole(null);
      });
    return () => {
      alive = false;
    };
  }, [runId]);

  /*
   * Fetch the run and its events at once. This used to be two bare awaits; once getRun rejected
   * (getRun only turns not_found into null; 403 is a reject) run stayed undefined forever,
   * and the page stayed on "loading" forever. See the file header of lib/useAsyncData.ts.
   *
   * run being null still means "this run does not exist", which is different from "failed to
   * load"; each has its own display.
   */
  const fetch = useCallback(async () => {
    if (!runId) return { run: null, events: [] as RunEvent[] };
    const r = await api.getRun(runId);
    if (!r) return { run: null, events: [] as RunEvent[] };
    const events = await api.listRunEvents(runId);
    return { run: r, events };
  }, [runId]);
  const {
    data,
    error,
    isLoading,
    reload: load,
  } = useAsyncData(fetch, t("common.loadFailedUnknown"));
  const run = data?.run ?? null;
  const events = data?.events ?? null;

  // t is a dependency: after switching language the event titles must be recomputed
  const allItems = useMemo(() => (events ? buildTimeline(events, t) : []), [events, t]);
  const stats = useMemo(
    () => (events ? computeStats(events, allItems) : null),
    [events, allItems],
  );

  /*
   * Data for the event view: **skip buildTimeline and use the raw events directly**, one per row,
   * nothing merged.
   * Its meaning is "faithfully show the raw events Ark returned" (DESIGN.md 3.6.3); going through
   * the processing pipeline
   * is itself a conceptual mismatch. See the top of lib/rawEvents.ts. The conversation view still
   * uses allItems above.
   */
  const rawRows = useMemo(() => (events ? buildRawRows(events, t) : []), [events, t]);
  const rawStats = useMemo(() => rawEventStats(rawRows), [rawRows]);

  // The type filter only accepts types that really appeared in this run; unknown URL params are
  // dropped.
  // Taken from **raw events**: pairing would make absorbed types like user.tool_result vanish from
  // the dropdown entirely.
  const presentTypes = useMemo(
    () => Object.keys(rawStats.byType) as RunEventType[],
    [rawStats.byType],
  );
  const { filter, isDirty, ...filterActions } = useTimelineFilter(presentTypes);

  // Every event must be JSON.stringify'd, so cache it; otherwise every keystroke in the keyword
  // re-serializes all events.
  const rawHaystack = useMemo(() => buildRawHaystack(rawRows), [rawRows]);
  const eventRows = useMemo(
    () => applyRawFilter(rawRows, filter, rawHaystack),
    [rawRows, filter, rawHaystack],
  );
  /*
   * The toolbar counts come from **raw events**, on the same basis as what it filters. Using the
   * computeStats() copy would
   * give "the filter says tool has 8, clicking shows 15": no error, it just makes people suspect
   * the filter is broken.
   */
  const toolbarCounts = useMemo<ToolbarCounts>(
    () => ({
      byCategory: rawStats.byCategory,
      byDomain: rawStats.byDomain,
      byType: rawStats.byType,
      slowCount: rawStats.slow,
      failedCount: rawStats.failed,
    }),
    [rawStats],
  );

  // The conversation view only filters by thread: category choices are decided by ConversationView
  // itself when folding by turn;
  // adding another category filter on top would hollow out the folded steps.
  const conversationItems = useMemo(
    () =>
      filter.threadId ? allItems.filter((i) => i.threadId === filter.threadId) : allItems,
    [allItems, filter.threadId],
  );

  if (isLoading) {
    return (
      <PageBody>
        <LoadingBlock label={t("run.loadingDetail")} />
      </PageBody>
    );
  }

  if (error) {
    return (
      <PageBody>
        <AsyncErrorBlock error={error} onRetry={load} />
      </PageBody>
    );
  }

  if (run === null) {
    return (
      <PageBody>
        <Empty
          icon="gravity-ui:circle-info"
          title={t("run.notFound")}
          description={t("run.notFoundDesc")}
          action={
            <Link to={wp("/runs")} className="no-underline">
              <Button variant="outline">{t("run.backToList")}</Button>
            </Link>
          }
        />
      </PageBody>
    );
  }

  const totalTokens = stats
    ? stats.usage.input_tokens + stats.usage.output_tokens
    : 0;

  return (
    <PageBody>
      <PageHeader
        title={run.title}
        above={
          <Link
            to={wp("/runs")}
            className="flex w-fit items-center gap-1 text-sm text-muted no-underline hover:text-foreground"
          >
            <Icon icon="gravity-ui:arrow-left" className="size-4" />
            {t("nav.runs")}
          </Link>
        }
        description={
          <span className="flex flex-wrap items-center gap-1.5">
            {run.agentName}
            {run.agentDeleted ? <AgentDeletedChip /> : null}
            {` · ${t(`enum.trigger.${run.trigger}`)}${
              run.triggerDetail ? ` · ${run.triggerDetail}` : ""
            }`}
          </span>
        }
        actions={
          <>
            <ArkConsoleLink
              kind="sessions"
              refData={arkConsole}
              labelKey="arkConsole.openSession"
              testId="run-ark-console"
            />
            {/* When the agent is deleted this link is sure to 404, so better not to give it. The name is still in the description above. */}
            {run.agentDeleted ? null : (
              <Link to={wp(`/agents/${run.agentId}`)} className="no-underline">
                <Button variant="outline">{t("run.viewAgent")}</Button>
              </Link>
            )}
          </>
        }
      />

      {/* Summary bar: the few numbers looked at first when debugging */}
      <Card>
        <Card.Content className="pt-6">
          <div className="flex flex-wrap items-start gap-x-10 gap-y-5">
            <div className="flex flex-col gap-1">
              <span className="text-xs text-muted">{t("agent.status")}</span>
              <RunStatusChip
                status={run.status}
                hasError={Boolean(run.errorSummary)}
                // The detail page has events, so it should say precisely "waiting for user
                // approval" vs "tool running in the container":
                // both are requires_action at the RunStatus level; see the comment on runAwaitsUser.
                awaitsUser={runAwaitsUser(allItems)}
              />
            </div>
            <Stat label={t("run.colDuration")} value={formatDuration(run.durationMs)} />
            <Stat
              label={t("run.statTokenTotal")}
              value={formatTokens(totalTokens)}
              hint={
                stats
                  ? t("run.statTokenHint", { input: formatTokens(stats.usage.input_tokens), output: formatTokens(stats.usage.output_tokens) })
                  : undefined
              }
            />
            <Stat
              label={t("run.statCacheHit")}
              value={stats ? formatTokens(stats.usage.cache_read_input_tokens) : "0"}
              hint={
                stats ? t("run.statCacheHint", { value: formatTokens(stats.usage.cache_creation_input_tokens) }) : undefined
              }
            />
            <Stat label={t("run.statModelReq")} value={String(stats?.modelRequests ?? 0)} />
            <Stat
              label={t("run.statToolCalls")}
              value={String(stats?.toolCalls ?? 0)}
              hint={stats?.toolErrors ? t("run.statToolErrors", { count: stats.toolErrors }) : undefined}
              tone={stats?.toolErrors ? "warning" : undefined}
            />
            {stats?.errors ? <Stat label={t("run.statErrors")} value={String(stats.errors)} tone="danger" /> : null}
            {stats?.outcomeResult ? (
              <Stat
                label={t("run.statOutcome")}
                value={stats.outcomeResult === "satisfied" ? t("run.outcomeSatisfied") : stats.outcomeResult}
                hint={t("run.statOutcomeRounds", { count: stats.outcomeIterations })}
              />
            ) : null}
            {/*
              The event count follows **raw events**; the step count after pairing is demoted to a
              hint.
              These two used to be the other way round (the main number was the paired entry count),
              and the event view now shows one event
              per row, so the main number must match the rows the user can count in the event view;
              otherwise "summary says 12,
              list has 18 rows" makes people think the list rendered duplicates.
            */}
            <Stat
              label={t("run.statEvents")}
              value={formatNumber(events?.length ?? 0, language)}
              hint={t("run.statMergedSteps", { count: allItems.length })}
            />
          </div>

          <Separator className="my-5" />

          <div className="grid gap-x-8 gap-y-3 text-xs sm:grid-cols-3">
            <div className="flex flex-col gap-1">
              <span className="text-muted">{t("run.startedAt")}</span>
              <span className="num">{formatDateTime(run.startedAt, language)}</span>
            </div>
            <div className="flex flex-col gap-1">
              <span className="text-muted">{t("run.endedAt")}</span>
              <span className="num">{run.endedAt ? formatDateTime(run.endedAt, language) : t("run.inProgress")}</span>
            </div>
            <div className="flex flex-col gap-1">
              <span className="text-muted">Session ID</span>
              <span className="font-mono">{run.id}</span>
            </div>
          </div>

          {run.errorSummary ? (
            <div className="mt-5 flex items-start gap-2 r-inset bg-danger/5 px-3 py-2.5">
              <Icon icon="gravity-ui:triangle-exclamation" className="mt-0.5 size-4 shrink-0 text-danger" />
              <p className="text-sm text-danger">{run.errorSummary}</p>
            </div>
          ) : null}
        </Card.Content>
      </Card>

      {/* Thread structure. Only meaningful with multi-agent collaboration */}
      {run.threads.length > 1 ? (
        <div className="flex flex-col gap-2">
          <p className="text-sm font-medium">{t("run.threads")}</p>
          <div className="flex flex-wrap gap-2">
            {run.threads.map((th) => (
              <div
                key={th.id}
                className="flex items-center gap-2 r-inset border border-border px-3 py-2"
              >
                <Icon
                  icon={th.parent_thread_id ? "gravity-ui:branches-right" : "gravity-ui:play"}
                  className="size-3.5 text-muted"
                />
                <span className="text-sm">{th.agent_name}</span>
                <Chip size="sm" variant="soft">
                  <Chip.Label>{th.status}</Chip.Label>
                </Chip>
                <span className="font-mono text-xs text-muted">{th.id}</span>
              </div>
            ))}
          </div>
        </div>
      ) : null}

      {/* Event stream */}
      <div className="flex flex-col gap-4">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className="text-sm font-medium">
            {view === "events" ? (
              <>
                {t("run.eventStream")}
                <span className="ml-2 num text-xs text-muted">
                  {t("run.eventShown", { shown: eventRows.length, total: rawRows.length })}
                </span>
              </>
            ) : view === "trace" ? (
              t("trace.title")
            ) : view === "files" ? (
              t("sandbox.files")
            ) : view === "terminal" ? (
              t("sandbox.terminal")
            ) : (
              t("chat.title")
            )}
          </p>
          <ViewSwitch view={view} onChange={setView} />
        </div>

        {view === "events" ? (
          <>
            {stats ? (
              <TimelineToolbar
                filter={filter}
                stats={toolbarCounts}
                threads={run.threads}
                isDirty={isDirty}
                onToggleCategory={filterActions.toggleCategory}
                onToggleDomain={filterActions.toggleDomain}
                onSetTypes={filterActions.setTypes}
                onApplyPreset={filterActions.applyPreset}
                onReset={filterActions.reset}
                onQueryChange={filterActions.setQuery}
                onSlowOnlyChange={filterActions.setSlowOnly}
                onErrorsOnlyChange={filterActions.setErrorsOnly}
                onThreadChange={filterActions.setThreadId}
              />
            ) : null}

            {/* When filtering leaves nothing, say "it was filtered out" instead of reusing "this run has no events" */}
            {eventRows.length === 0 && rawRows.length > 0 ? (
              <Empty
                icon="gravity-ui:funnel"
                title={t("filter.noMatch")}
                description={t("filter.noMatchDesc")}
                action={
                  <Button variant="outline" onPress={filterActions.reset}>
                    {t("filter.reset")}
                  </Button>
                }
              />
            ) : (
              <RawEventList
                rows={eventRows}
                showThread={run.threads.length > 1 && filter.threadId === null}
              />
            )}
          </>
        ) : view === "trace" ? (
          /* The call tree is its own view, no longer hung at the end of the event view (see the comment on ViewSwitch).
             **The panel has no scroll wrapper**: this page's scroll container is AppShell's <main>,
             and wrapping another would give two scroll areas.
             Use allItems rather than items: the call tree should not be affected by the event view's filter;
             the two views have their own tradeoffs (filtering out half the events would drop half the call tree's nodes,
             while the user never filtered anything on the call tree). */
          <TracePanel items={allItems} />
        ) : view === "files" || view === "terminal" ? (
          /* Files and terminal are two views sharing one panel (the container state part is shown in both).
             Also **no scroll wrapper** (the scroll container is AppShell's <main>).
             The panel checks state itself: containers pause/get reclaimed on their own, and fetching with the page data would give
             a stale snapshot. */
          <SandboxPanel runId={run.id} mode={view} hostId={run.hostId} />
        ) : (
          <>
            <ThreadSwitcher
              threads={run.threads}
              active={filter.threadId}
              onChange={filterActions.setThreadId}
            />

            {/* max-w-3xl matches the chat page; ChatConversation is deliberately not used here:
                the page itself is the scroll container, and nesting another would give two scroll areas. */}
            <div className="flex w-full max-w-3xl flex-col gap-5">
              <ConversationView
                items={conversationItems}
                agentName={run.agentName}
                // The detail page is a one-time fetch with no live stream, so run.status is the
                // final state:
                // when the terminal event was not stored, it is used to close the last turn. See
                // groupTurns in lib/timeline.ts.
                runStatus={run.status}
              />
            </div>
          </>
        )}
      </div>
    </PageBody>
  );
}
