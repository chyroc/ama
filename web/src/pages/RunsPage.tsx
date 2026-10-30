/**
 * Run history list.
 *
 * Filter by agent, status and keyword. Each row shows status, duration, tokens and trigger source:
 * the fields looked at first
 * when debugging.
 */

import { useCallback, useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { Button, SearchField, Select, ListBox, ToggleButton } from "@heroui/react";
import { Icon } from "@iconify/react";
import { api } from "@/data/api";
import { useWorkspace, useWorkspacePath } from "@/app/WorkspaceGate";
import {
  AgentDeletedChip,
  AsyncErrorBlock,
  Empty,
  ListSkeleton,
  PageBody,
  PageHeader,
} from "@/components/Page";
import { RunStatusChip } from "@/components/RunStatusChip";
import { formatDuration, formatRelative, formatTokens } from "@/lib/format";
import { useAsyncData } from "@/lib/useAsyncData";
import type { TimelineItem } from "@/lib/timeline";
import { type RunStatus } from "@/types/domain";
import { usePrefs } from "@/prefs/PrefsContext";

/**
 * "This run is really waiting for user approval": only events can tell.
 *
 * The `RunStatus` level **cannot tell this apart**: whether it is "tool running in our container"
 * or "waiting for user approval",
 * Ark gives `status_idle` + `stop_reason: requires_action`, and both derive to
 * `requires_action`. The only way to tell is whether that `tool_use`'s `evaluated_permission` is
 * `ask` with no `user.tool_confirmation` yet; those are event fields the run list does not have.
 *
 * So: the list uses a generic in-progress wording (under self_hosted most cases are running), and
 * **the detail page has events,
 * so it should be precise**. Otherwise the top says "tool running" while the tool row below says
 * "awaiting your confirmation", a contradiction.
 */
export function runAwaitsUser(items: TimelineItem[]): boolean {
  return items.some(
    (i) => i.permission === "ask" && !i.confirmation && i.result?.pending === true,
  );
}

const TRIGGER_ICONS: Record<string, string> = {
  feishu: "gravity-ui:comment",
  manual: "gravity-ui:person",
  api: "gravity-ui:code",
  schedule: "gravity-ui:clock",
};

const ALL_STATUSES: RunStatus[] = [
  "running",
  "requires_action",
  "idle",
  "rescheduling",
  "terminated",
];

/**
 * Search debounce in milliseconds (setTimeout + cleanup).
 *
 * Once filtering is pushed down, **every keystroke is a backend query**; without debounce the
 * "optimization" just loads the backend:
 * searching a run id fires a dozen requests, and if a later one arrives first an old result
 * overwrites the new one.
 * No extra request counter is needed here: requests are made by useAsyncData,
 * and when the dependency (the debounced term) changes it restarts entirely, so stale responses
 * never write back to the current frame.
 */
const SEARCH_DEBOUNCE_MS = 300;

export function RunsPage() {
  const { t, language } = usePrefs();
  const { workspace } = useWorkspace();
  const wp = useWorkspacePath();
  const [params, setParams] = useSearchParams();
  const agentFilter = params.get("agent") ?? "";

  const [search, setSearch] = useState("");
  const [statuses, setStatuses] = useState<Set<RunStatus>>(new Set());

  /**
   * The debounced keyword: **this is what actually goes into the request**; the input is still
   * bound to `search` (it must echo at once).
   *
   * `trim()` is done here, matching the client-side filter from before pushdown word for word:
   * typing only spaces means no filter.
   * The backend does not trim (`q.Get("search")` goes as is into position() in SQL); without trim
   * a single space would filter to "titles containing a space", a silent behavior change.
   */
  const [debouncedSearch, setDebouncedSearch] = useState("");
  useEffect(() => {
    const q = search.trim();
    // Clearing takes effect at once: a user clicking ClearButton wants to see "all", no reason to
    // make them wait 300ms.
    if (q === "") {
      setDebouncedSearch("");
      return;
    }
    const timer = setTimeout(() => setDebouncedSearch(q), SEARCH_DEBOUNCE_MS);
    // Another keystroke cancels this pending query; this cleanup is the debounce itself.
    return () => clearTimeout(timer);
  }, [search]);

  /**
   * Whether this screen is filtering. **The empty-state text branches on it**; see the part in
   * fetch below.
   *
   * agentFilter counts too: `?agent=` is carried over from the Agent detail page, and then
   * "this agent has no runs" and "the whole workspace has no runs" are two different things, and
   * the page header has exactly
   * the "Show all Agents" button to let the user step out.
   */
  const filtering = Boolean(agentFilter || debouncedSearch || statuses.size > 0);

  /*
   * Filtering is **all pushed down to the backend** (done in SQL); there is no more `.filter()`
   * here.
   *
   * It used to be "fetch everything → filter in the browser", with the comment "the data is small,
   * no need to hit the backend every time".
   * That premise does not hold in a workspace with 5000 runs: one full listRuns measured 62.5ms /
   * 19.2MB
   * (see the write-up in OverviewPage), and most of those entries are filtered out before they
   * ever render.
   *
   * ⚠️ The fields search matches must line up exactly with the backend: case-insensitive
   * substring of **title / agentName / run id**
   * (position(lower(…)) in `coredao.PGStore.ListRuns`).
   * Before pushdown this page filtered exactly these three too, so the basis did not change. If
   * they differed, it would show as "searching by id finds nothing,
   * and the page says no matching runs", which looks like search is working fine.
   *
   * ⚠️ status is an allowlist, and **an empty set must be passed as undefined, not an empty
   * array**: an empty array should mean
   * "no filter"; once some layer reads it as "filter to zero", the whole list empties with zero
   * errors.
   */
  // Failures must be visible, not stuck on the skeleton (see the file header of lib/useAsyncData.ts).
  const fetch = useCallback(async () => {
    const [runs, agents] = await Promise.all([
      api.listRuns(workspace.id, {
        agentId: agentFilter || undefined,
        status: statuses.size > 0 ? [...statuses] : undefined,
        search: debouncedSearch || undefined,
      }),
      api.listAgents(workspace.id),
    ]);

    /*
     * The two empty-state sentences must be chosen by a signal that **still holds after pushdown**.
     *
     * It used to be `runs?.length ? no matching runs : no runs yet`. Back then `runs` was the
     * whole workspace, and `length` was exactly "does this workspace have runs". After pushdown
     * `runs` became
     * "how many rows this filtered query returned", so when the search finds nothing it is 0, and
     * the user is told
     * **"no runs yet"** while they were searching a screen full of runs. The two sentences mean
     * opposite things:
     * one tells them to change the filter, the other makes them think the workspace is empty.
     *
     * The check is now "does this workspace have any runs at all", and **we only ask when we really
     * need to tell**:
     * with no filter, or with a filter that found something, the answer is already in hand
     * (`runs.length > 0` proves there are some).
     * Only the "filtered + found nothing" case makes one extra query, and it uses `limit: 1`,
     * asking only whether any exist, fetching no data. So the normal path adds no request at all,
     * and the extra one happens exactly when the user is already confused.
     */
    const hasAnyRuns =
      runs.length > 0
        ? true
        : filtering
          ? (await api.listRuns(workspace.id, { limit: 1 })).length > 0
          : false;

    return { runs, agents, hasAnyRuns };
  }, [workspace.id, agentFilter, statuses, debouncedSearch, filtering]);
  const { data, error, isLoading, reload: load } = useAsyncData(
    fetch,
    t("common.loadFailedUnknown"),
  );
  const runs = data?.runs ?? null;
  const agents = data?.agents ?? [];
  const hasAnyRuns = data?.hasAnyRuns ?? false;

  function toggleStatus(s: RunStatus) {
    setStatuses((prev) => {
      const next = new Set(prev);
      if (next.has(s)) next.delete(s);
      else next.add(s);
      return next;
    });
  }

  const agentName = agents.find((a) => a.id === agentFilter)?.name;

  return (
    <PageBody>
      <PageHeader
        title={t("nav.runs")}
        description={
          agentName ? t("run.listDescAgent", { name: agentName }) : t("run.listDescAll")
        }
        actions={
          agentFilter ? (
            <Button
              variant="outline"
              onPress={() => {
                params.delete("agent");
                setParams(params, { replace: true });
              }}
            >
              {t("run.showAllAgents")}
            </Button>
          ) : undefined
        }
      />

      <div className="flex flex-col gap-3">
        <div className="flex flex-wrap items-center gap-3">
          <SearchField
            className="w-full max-w-xs"
            value={search}
            onChange={setSearch}
            aria-label={t("run.searchLabel")}
          >
            <SearchField.Group>
              <SearchField.SearchIcon />
              <SearchField.Input placeholder={t("run.searchPlaceholder")} data-testid="run-search" />
              <SearchField.ClearButton />
            </SearchField.Group>
          </SearchField>

          <Select
            className="w-52"
            aria-label={t("run.filterByAgent")}
            selectedKey={agentFilter || "__all"}
            onSelectionChange={(k) => {
              const key = String(k);
              if (key === "__all") params.delete("agent");
              else params.set("agent", key);
              setParams(params, { replace: true });
            }}
          >
            <Select.Trigger data-testid="run-agent-filter">
              <Select.Value />
              <Select.Indicator />
            </Select.Trigger>
            <Select.Popover>
              <ListBox>
                <ListBox.Item id="__all" textValue={t("run.allAgents")}>
                  {t("run.allAgents")}
                </ListBox.Item>
                {agents.map((a) => (
                  <ListBox.Item key={a.id} id={a.id} textValue={a.name}>
                    {a.name}
                  </ListBox.Item>
                ))}
              </ListBox>
            </Select.Popover>
          </Select>
        </div>

        <div className="flex flex-wrap items-center gap-2">
          {ALL_STATUSES.map((s) => (
            <ToggleButton
              key={s}
              size="sm"
              isSelected={statuses.has(s)}
              onChange={() => toggleStatus(s)}
              data-testid={`status-filter-${s}`}
            >
              {t(`enum.runStatus.${s}`)}
            </ToggleButton>
          ))}
        </div>
      </div>

      {isLoading ? (
        <ListSkeleton />
      ) : error ? (
        <AsyncErrorBlock error={error} onRetry={load} />
      ) : runs === null ? null : runs.length === 0 ? (
        /* "There are none" and "filtered but no match" are two opposite sentences; for the check see hasAnyRuns
           in fetch: **`runs.length` can no longer be used**; after pushdown it always equals this query's row count. */
        <Empty
          icon="gravity-ui:list-ul"
          title={hasAnyRuns ? t("run.emptyNoMatch") : t("overview.noRuns")}
          description={hasAnyRuns ? t("run.emptyNoMatchDesc") : t("run.emptyDesc")}
        />
      ) : (
        <div className="flex flex-col gap-2" data-testid="run-list">
          {runs.map((r) => (
            <Link
              key={r.id}
              to={wp(`/runs/${r.id}`)}
              className="group no-underline"
              data-testid={`run-row-${r.id}`}
            >
              <div className="flex flex-col gap-3 r-row border border-border px-4 py-3.5 hover:bg-surface-secondary sm:flex-row sm:items-center">
                <div className="flex min-w-0 flex-1 flex-col gap-1">
                  <div className="flex items-center gap-2">
                    <RunStatusChip status={r.status} hasError={Boolean(r.errorSummary)} />
                    <span className="truncate text-sm font-medium">{r.title}</span>
                  </div>
                  <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted">
                    <span className="flex items-center gap-1.5">
                      <Icon icon="gravity-ui:face-robot" className="size-3.5" />
                      {r.agentName}
                      {r.agentDeleted ? <AgentDeletedChip /> : null}
                    </span>
                    <span className="flex items-center gap-1">
                      <Icon
                        icon={TRIGGER_ICONS[r.trigger] ?? "gravity-ui:circle-info"}
                        className="size-3.5"
                      />
                      {t(`enum.trigger.${r.trigger}`)}
                      {r.triggerDetail ? ` · ${r.triggerDetail}` : ""}
                    </span>
                    {r.threads.length > 1 ? (
                      <span className="flex items-center gap-1 num">
                        <Icon icon="gravity-ui:branches-right" className="size-3.5" />
                        {t("run.threadCount", { count: r.threads.length })}
                      </span>
                    ) : null}
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
                  <div className="flex w-20 flex-col gap-0.5">
                    <span className="text-muted">Token</span>
                    <span className="num">
                      {formatTokens(r.usage.input_tokens + r.usage.output_tokens)}
                    </span>
                  </div>
                  <div className="flex w-14 flex-col gap-0.5">
                    <span className="text-muted">{t("run.colEvents")}</span>
                    <span className="num">{r.eventCount}</span>
                  </div>
                  <div className="hidden w-24 flex-col gap-0.5 lg:flex">
                    <span className="text-muted">{t("run.colStarted")}</span>
                    <span>{formatRelative(r.startedAt, language)}</span>
                  </div>
                  <Icon
                    icon="gravity-ui:chevron-right"
                    className="size-4 text-muted"
                  />
                </div>
              </div>
            </Link>
          ))}
        </div>
      )}
    </PageBody>
  );
}
