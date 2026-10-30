/**
 * Schedule detail page: view + edit, replacing the old edit dialog.
 *
 * # Why it must be a separate page and not a dialog
 *
 * `prompt` is the core of the task, and **a real inspection task's prompt has 6180 characters**;
 * the
 * 4-line TextArea in a dialog simply cannot edit that. This is the first reason this page exists,
 * so it gets 18 lines here,
 * a monospace font, and its own save button (same tradeoffs as FileEditor in SkillDetailPage).
 *
 * The second thing it solves along the way is **observability**: a schedule's value is long-term
 * stability, and a dialog only has room for
 * the form. Here the three counters, the last error, and "how many more failures until auto-pause"
 * are spelled out.
 *
 * # Two tabs, state in the URL
 *
 * `?tab=config` (default) and `?tab=history`, the same approach as AgentDetailPage: links can be
 * shared,
 * refresh keeps it. The overview is not inside a tab and is visible under both (it is "how is
 * this task doing now",
 * which should be in view whether you are looking at config or history).
 *
 * # Gaps in run history must be stated
 *
 * The history list looks up by `run.scheduleId` (a column added later). **Runs before that have
 * no such
 * link and cannot be backfilled**, so the count found may be less than `runCount`; then we must
 * say explicitly "the earlier N runs cannot be found"
 * rather than showing a short list. Without that sentence, users read it as "this task has only
 * run this many times",
 * which is the opposite of the truth. The check is `runCount > rows found`.
 */

import { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { Button, Chip, Switch, Tooltip } from "@heroui/react";
import { Icon } from "@iconify/react";
import { api } from "@/data/api";
import { useWorkspace, useWorkspacePath } from "@/app/WorkspaceGate";
import {
  AgentDeletedChip,
  AsyncErrorBlock,
  DescList,
  Empty,
  LoadingBlock,
  Notice,
  PageBody,
  PageHeader,
  SectionTitle,
} from "@/components/Page";
import { ConfirmModal } from "@/components/FormModal";
import { RunStatusChip } from "@/components/RunStatusChip";
import { ScheduleForm, describeSpec, validateScheduleDraft } from "@/pages/SchedulesPage";
import type { DraftErrors } from "@/pages/SchedulesPage";
import { formatDateTime, formatDuration } from "@/lib/format";
import { useAsyncData } from "@/lib/useAsyncData";
import { useCanWrite } from "@/lib/useCanManage";
import { RevisionPanel } from "@/components/RevisionPanel";
import { usePrefs } from "@/prefs/PrefsContext";
import {
  DEFAULT_SCHEDULE_TZ,
  MAX_CONSECUTIVE_FAILURES,
} from "@/types/domain";
import type { MessageKey } from "@/prefs/messages";
import type { Run, Schedule, ScheduleDraft } from "@/types/domain";

/**
 * Two tabs. Same as AgentDetailPage: store **text keys, not text**; this is a module-level
 * constant,
 * and `t()` can only be called inside the component body.
 */
const TABS = [
  { id: "config", labelKey: "sched.tabConfig" },
  { id: "history", labelKey: "sched.tabHistory" },
  // ⚠️ "Version history" and the neighboring "history" (run history) **are two different things**:
  // that one is "how many times it ran and how it went", this one is "what the config was changed
  // to".
  // So the id is revisions, not history2 or the like; the two tabs' text must be distinct too.
  { id: "revisions", labelKey: "revision.tab" },
] as const satisfies readonly { id: string; labelKey: MessageKey }[];

type TabId = (typeof TABS)[number]["id"];

function toDraft(sc: Schedule): ScheduleDraft {
  return {
    agentId: sc.agentId,
    name: sc.name,
    spec: sc.spec,
    prompt: sc.prompt,
    contextSummary: sc.contextSummary,
    delivery: sc.delivery,
    status: sc.status,
  };
}

/**
 * Overview: how this task is doing now.
 *
 * Deliberately placed **outside** the tabs: it answers "do I need to deal with it", which has
 * nothing to do with whether you are looking at config or history.
 */
function StatusOverview({ sc, language }: { sc: Schedule; language: string }) {
  const { t } = usePrefs();

  // "How many more failures until auto-pause".
  //
  // This hint is the most important information on the page: consecutive failures up to the
  // threshold get it **auto-paused**, and that
  // "stops without anyone knowing" (the server only logs a warning). Without stating the remaining
  // count, the user sees
  // a task that suddenly stopped running, and the UI only shows "paused", looking as if someone
  // stopped it by hand.
  const left = MAX_CONSECUTIVE_FAILURES - sc.consecutiveFailures;
  const nearAutoPause = sc.consecutiveFailures > 0 && left > 0;
  // Already auto-paused: paused + consecutive failures reached the threshold. **Both conditions are
  // needed**: checking only paused would
  // also describe tasks the user paused by hand as "stopped due to failures".
  const autoPaused =
    sc.status === "paused" && sc.consecutiveFailures >= MAX_CONSECUTIVE_FAILURES;

  return (
    <section className="flex flex-col gap-4 r-container bg-surface p-6 shadow-surface">
      <DescList
        items={[
          {
            label: t("sched.nextRun"),
            value: sc.nextRunAt ? (
              <span className="num">{formatDateTime(sc.nextRunAt, language)}</span>
            ) : (
              // Same as the list page: an empty nextRunAt must be told apart by status; active means
              // "running now"
              // (claiming clears next_run_at), not "the user did not configure it".
              <span className="text-muted" data-testid="detail-not-scheduled">
                {t(sc.status === "active" ? "sched.running" : "sched.notScheduled")}
              </span>
            ),
          },
          {
            label: t("sched.lastRunAt"),
            value: sc.lastRunAt ? (
              // The last run links to run detail. lastRunId may be empty (e.g. the round where
              // creating the session failed);
              // then only show the time, with no dead link.
              sc.lastRunId ? (
                <Link
                  to={`../../runs/${sc.lastRunId}`}
                  relative="path"
                  className="num underline"
                  data-testid="detail-last-run-link"
                >
                  {formatDateTime(sc.lastRunAt, language)}
                </Link>
              ) : (
                <span className="num">{formatDateTime(sc.lastRunAt, language)}</span>
              )
            ) : (
              <span className="text-muted">{t("sched.never")}</span>
            ),
          },
          {
            label: t("sched.countRuns"),
            value: (
              <span className="num" data-testid="detail-run-count">
                {t("sched.times", { count: sc.runCount })}
              </span>
            ),
          },
          {
            label: t("sched.countFails"),
            value: (
              <span
                className={sc.failCount > 0 ? "num text-danger" : "num"}
                data-testid="detail-fail-count"
              >
                {t("sched.times", { count: sc.failCount })}
              </span>
            ),
          },
        ]}
      />

      {/* Consecutive failures: only shown when it really fails in a row; takes no space normally */}
      {nearAutoPause ? (
        <Notice
          tone="warning"
          icon="gravity-ui:triangle-exclamation"
          title={t("sched.countConsecutive") + "：" + t("sched.times", { count: sc.consecutiveFailures })}
          hint={t("sched.autoPauseHint", { left })}
          data-testid="detail-near-autopause"
        />
      ) : null}
      {autoPaused ? (
        <Notice
          tone="danger"
          icon="gravity-ui:circle-exclamation"
          title={t("sched.autoPaused")}
          data-testid="detail-autopaused"
        />
      ) : null}
      {sc.lastError ? (
        <Notice
          tone="danger"
          icon="gravity-ui:circle-xmark"
          title={t("sched.lastErrorTitle")}
          hint={sc.lastError}
          data-testid="detail-last-error"
        />
      ) : null}
      {/*
        If the agent is deleted this task cannot produce results. The text matches the backend's
        RunScheduleNow 422 word for word
        (core/schedule.go), so the error users get when clicking test-run is the same sentence as
        this one.
      */}
      {sc.agentDeleted ? (
        <Notice
          tone="danger"
          icon="gravity-ui:circle-xmark"
          title={t("sched.agentDeletedTitle")}
          hint={t("sched.agentDeletedDesc")}
          data-testid="detail-agent-deleted"
        />
      ) : null}
      {/*
        oneshot is deleted once it runs (success or failure), so "being able to see it on this page"
        itself means it has not fired yet.
        Without this sentence, users seeing a task with empty run history would think it is broken.
      */}
      {sc.spec.kind === "oneshot" ? (
        <Notice
          tone="info"
          icon="gravity-ui:circle-info"
          title={t("sched.oneshotTitle")}
          hint={t("sched.oneshotDesc")}
          data-testid="detail-oneshot"
        />
      ) : null}
    </section>
  );
}

/** Run history tab. */
function HistoryView({
  sc,
  runs,
  language,
}: {
  sc: Schedule;
  runs: Run[];
  language: string;
}) {
  const { t } = usePrefs();

  // Gap from old data: it ran runCount times in total, but only those after scheduleId was added
  // can be found.
  //
  // **The difference must be stated explicitly**, otherwise this short list would be read as "it
  // only ran this many times".
  const missing = Math.max(0, sc.runCount - runs.length);

  if (runs.length === 0) {
    return (
      <div className="flex flex-col gap-4">
        <Empty
          icon="gravity-ui:clock"
          title={t("sched.historyEmpty")}
          description={t("sched.historyEmptyDesc")}
        />
        {missing > 0 ? (
          <Notice
            tone="info"
            icon="gravity-ui:circle-info"
            title={t("sched.historyGap", { count: missing })}
            data-testid="detail-history-gap"
          />
        ) : null}
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-3">
      <p className="text-xs text-muted num" data-testid="detail-history-count">
        {t("sched.historyCount", { shown: runs.length, total: sc.runCount })}
      </p>
      {missing > 0 ? (
        <Notice
          tone="info"
          icon="gravity-ui:circle-info"
          title={t("sched.historyGap", { count: missing })}
          data-testid="detail-history-gap"
        />
      ) : null}
      {/*
        ⚠️ The row testid prefix is `detail-history-row-` and **not** `detail-run-`:
        this page also has `detail-run-count` / `detail-run-now`, and in e2e
        `[data-testid^="detail-run-"]` would count those two as well (measured: it counted 5 rows
        instead of 3).
        Exactly the same trap as the CLAUDE.md rule "do not add testids starting with event-".
      */}
      <div className="flex flex-col gap-2" data-testid="detail-history-list">
        {runs.map((r) => (
          <Link
            key={r.id}
            to={`../../runs/${r.id}`}
            relative="path"
            className="group no-underline"
            data-testid={`detail-history-row-${r.id}`}
          >
            <div className="flex flex-col gap-3 r-row border border-border px-4 py-3.5 hover:bg-surface-secondary sm:flex-row sm:items-center">
              <div className="flex min-w-0 flex-1 flex-col gap-1">
                <div className="flex items-center gap-2">
                  <RunStatusChip status={r.status} hasError={Boolean(r.errorSummary)} />
                  <span className="num truncate text-xs text-muted">
                    {formatDateTime(r.startedAt, language)}
                  </span>
                </div>
                {r.errorSummary ? (
                  <p className="truncate text-xs text-danger">{r.errorSummary}</p>
                ) : null}
              </div>
              <div className="flex shrink-0 items-center gap-6 text-xs">
                <div className="flex w-16 flex-col gap-0.5">
                  <span className="text-muted">{t("run.colDuration")}</span>
                  <span className="num">{formatDuration(r.durationMs)}</span>
                </div>
                <div className="flex w-16 flex-col gap-0.5">
                  <span className="text-muted">{t("run.colEvents")}</span>
                  <span className="num">{r.eventCount}</span>
                </div>
              </div>
            </div>
          </Link>
        ))}
      </div>
    </div>
  );
}

export function ScheduleDetailPage() {
  const { t, language } = usePrefs();
  const { scheduleId = "" } = useParams<{ scheduleId: string }>();
  const { workspace } = useWorkspace();
  const wp = useWorkspacePath();
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  const tab = (params.get("tab") ?? "config") as TabId;

  const [draft, setDraft] = useState<ScheduleDraft | null>(null);
  const [errors, setErrors] = useState<DraftErrors>({});
  const [saving, setSaving] = useState(false);
  const [savedAt, setSavedAt] = useState<number | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);

  const fetch = useCallback(async () => {
    if (!scheduleId) {
      return { schedule: null, agents: [], chats: [], runs: [] };
    }
    const sc = await api.getSchedule(scheduleId);
    // If the task does not exist, do not query its sub-resources: each would return an empty array,
    // looking like "the task exists but has no content".
    if (!sc) return { schedule: null, agents: [], chats: [], runs: [] };
    const [agents, chats, runs] = await Promise.all([
      api.listAgents(workspace.id),
      api.listFeishuChats(workspace.id),
      api.listRuns(workspace.id, { scheduleId }),
    ]);
    return { schedule: sc, agents, chats, runs };
  }, [scheduleId, workspace.id]);

  const { data, error, isLoading, reload } = useAsyncData(
    fetch,
    t("common.loadFailedUnknown"),
  );
  // ⚠️ Rollback needs write (member and above), not manage. See the comment on useCanWrite.
  const canWrite = useCanWrite(workspace.id) ?? false;
  const schedule = data?.schedule ?? null;

  // The draft is a local editable copy, not bound directly to data.schedule (same as
  // AgentDetailPage).
  useEffect(() => {
    setDraft(schedule ? toDraft(schedule) : null);
  }, [schedule]);

  const dirty = useMemo(
    () => Boolean(schedule && draft && JSON.stringify(toDraft(schedule)) !== JSON.stringify(draft)),
    [schedule, draft],
  );

  if (isLoading || (schedule && !draft)) {
    return (
      <PageBody>
        <LoadingBlock label={t("sched.loading")} />
      </PageBody>
    );
  }
  if (error) {
    return (
      <PageBody>
        <AsyncErrorBlock error={error} onRetry={reload} />
      </PageBody>
    );
  }
  // A 404 gets Empty + back to list, **no retry** (it still does not exist after ten thousand
  // retries).
  // oneshot is deleted once it runs, so this path is hit normally; the text explains that.
  if (schedule === null || draft === null) {
    return (
      <PageBody>
        <Empty
          icon="gravity-ui:clock"
          title={t("sched.notFound")}
          description={t("sched.notFoundDesc")}
          action={
            <Link to={wp("/schedules")} className="no-underline">
              <Button variant="outline" data-testid="detail-back-empty">
                {t("sched.backToList")}
              </Button>
            </Link>
          }
        />
      </PageBody>
    );
  }

  const sc = schedule;
  const agents = data?.agents ?? [];
  const chats = data?.chats ?? [];
  const runs = data?.runs ?? [];

  async function save() {
    if (!draft) return;
    const errs = validateScheduleDraft(draft, t);
    setErrors(errs);
    if (Object.keys(errs).length > 0) return;

    setSaving(true);
    setSaveError(null);
    try {
      await api.updateSchedule(sc.id, draft);
      await reload();
      setSavedAt(Date.now());
      setTimeout(() => setSavedAt(null), 2400);
    } catch (err) {
      // The server's error text is a Chinese sentence that can be shown directly (see the
      // convention in server/core).
      setSaveError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  }

  async function toggle() {
    await api.setScheduleStatus(sc.id, sc.status === "active" ? "paused" : "active");
    await reload();
  }

  async function runNow() {
    setSaveError(null);
    try {
      await api.runScheduleNow(sc.id);
      // Test-run is async (the server only marks it "due", the scheduler does the real run),
      // so we can only say "queued"; saying "done" would be false.
      setNotice(t("sched.runNowQueued", { name: sc.name }));
      await reload();
    } catch (err) {
      setSaveError(err instanceof Error ? err.message : String(err));
    }
  }

  return (
    <PageBody>
      <PageHeader
        above={
          <Link
            to={wp("/schedules")}
            className="flex w-fit items-center gap-1 text-sm text-muted no-underline hover:text-foreground"
            data-testid="detail-back"
          >
            <Icon icon="gravity-ui:arrow-left" className="size-4" />
            {t("sched.backToList")}
          </Link>
        }
        title={sc.name}
        description={
          <span className="flex flex-wrap items-center gap-2">
            <Chip
              size="sm"
              variant="soft"
              color={sc.status === "active" ? "success" : "warning"}
            >
              <Chip.Label data-testid="detail-status">
                {sc.status === "active" ? t("sched.statusActive") : t("sched.paused")}
              </Chip.Label>
            </Chip>
            <span className="flex items-center gap-1.5 text-xs text-muted">
              <Icon icon="gravity-ui:face-robot" className="size-3.5" />
              {sc.agentName}
              {sc.agentDeleted ? <AgentDeletedChip /> : null}
            </span>
            <span className="num flex items-center gap-1.5 text-xs text-muted">
              <Icon icon="gravity-ui:clock" className="size-3.5" />
              {describeSpec(sc, {
                every: (min) => t("sched.everyMinutes", { count: min }),
                oneshot: t("sched.kindOneshot"),
              })}
            </span>
          </span>
        }
        actions={
          <>
            {saveError ? (
              <span
                className="flex items-center gap-1 text-sm text-danger"
                data-testid="detail-save-error"
              >
                <Icon icon="gravity-ui:circle-xmark" className="size-4" />
                {saveError}
              </span>
            ) : null}
            {savedAt ? (
              <span className="flex items-center gap-1 text-sm text-success" data-testid="detail-saved">
                <Icon icon="gravity-ui:circle-check" className="size-4" />
                {t("sched.savedNeedsNothing")}
              </span>
            ) : null}
            <Tooltip>
              <Tooltip.Trigger>
                <Button
                  variant="ghost"
                  isIconOnly
                  aria-label={t("common.delete")}
                  onPress={() => setDeleting(true)}
                  data-testid="detail-delete"
                >
                  <Icon icon="gravity-ui:trash-bin" className="size-4" />
                </Button>
              </Tooltip.Trigger>
              <Tooltip.Content>{t("common.delete")}</Tooltip.Content>
            </Tooltip>
            <Button
              variant="outline"
              isDisabled={sc.agentDeleted}
              onPress={() => void runNow()}
              data-testid="detail-run-now"
            >
              {t("sched.runNowFromDetail")}
            </Button>
            {/* Toggle at the end of the row, matching the list page and the agent detail page */}
            <Switch
              isSelected={sc.status === "active"}
              onChange={() => void toggle()}
              aria-label={t("sched.toggleLabel", { name: sc.name })}
              data-testid="detail-toggle"
            >
              <Switch.Content className="text-sm">
                <Switch.Control>
                  <Switch.Thumb />
                </Switch.Control>
                {t("sched.enabled")}
              </Switch.Content>
            </Switch>
          </>
        }
      />

      {notice ? (
        <p className="text-sm text-muted" data-testid="detail-notice">
          {notice}
        </p>
      ) : null}

      <StatusOverview sc={sc} language={language} />

      {/* Tab bar. The same hand-rolled shape as AgentDetailPage (HeroUI Tabs are not used elsewhere either) */}
      <div className="flex gap-1 border-b border-border">
        {TABS.map((item) => (
          <button
            key={item.id}
            type="button"
            className={`relative cursor-[var(--cursor-interactive)] px-3 py-2 text-sm ${
              tab === item.id
                ? "font-medium text-foreground after:absolute after:inset-x-0 after:-bottom-px after:h-0.5 after:bg-accent"
                : "text-muted hover:text-foreground"
            }`}
            onClick={() => {
              params.set("tab", item.id);
              setParams(params, { replace: true });
            }}
            data-testid={`detail-tab-${item.id}`}
          >
            {t(item.labelKey)}
          </button>
        ))}
      </div>

      {tab === "revisions" ? (
        <RevisionPanel kind="schedule" entityId={sc.id} canWrite={canWrite} onRolledBack={reload} />
      ) : tab === "history" ? (
        <HistoryView sc={sc} runs={runs} language={language} />
      ) : (
        <section className="flex flex-col gap-5" data-testid="detail-config">
          <SectionTitle
            title={t("sched.tabConfig")}
            description={t("sched.tzHint", { tz: sc.spec.tz ?? DEFAULT_SCHEDULE_TZ })}
            actions={
              <Button
                onPress={() => void save()}
                isDisabled={!dirty}
                isPending={saving}
                data-testid="detail-save"
              >
                {t("sched.saveConfig")}
              </Button>
            }
          />
          {/*
            The form itself **shares one component** with the create dialog (imported from
            SchedulesPage). With a copy
            the two would drift, and the drift always goes "a new field only got into one side": a
            gap with zero errors.
            The only difference is promptRows: 4 lines in the dialog, 18 here, which is exactly why
            this page exists.
          */}
          <ScheduleForm
            draft={draft}
            setDraft={setDraft}
            errors={errors}
            agents={agents}
            chats={chats}
            promptRows={18}
            testIdPrefix="detail"
          />
        </section>
      )}

      <ConfirmModal
        isOpen={deleting}
        onOpenChange={(open) => !open && setDeleting(false)}
        title={t("sched.deleteTitle", { name: sc.name })}
        description={t("sched.deleteDesc")}
        confirmLabel={t("common.delete")}
        onConfirm={async () => {
          await api.deleteSchedule(sc.id);
          navigate(wp("/schedules"));
        }}
      />
    </PageBody>
  );
}
