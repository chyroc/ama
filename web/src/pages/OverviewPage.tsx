/**
 * Workspace overview.
 *
 * Ordered "summary first, details after": a few summary numbers first, then recent runs. No
 * charts:
 * at the current data size, one list of recent runs is more useful than any chart.
 */

import { useCallback } from "react";
import { Link } from "react-router-dom";
import { Button, Card, Separator } from "@heroui/react";
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
import { usePrefs } from "@/prefs/PrefsContext";

function MetricCard({
  label,
  value,
  hint,
  icon,
  to,
}: {
  label: string;
  value: string;
  hint?: string;
  icon: string;
  to: string;
}) {
  return (
    <Link to={to} className="no-underline">
      <Card className="h-full">
        <Card.Content className="flex flex-col gap-3 pt-6">
          <div className="flex items-center justify-between">
            <span className="text-xs text-muted">{label}</span>
            <Icon icon={icon} className="size-4 text-muted" />
          </div>
          <div className="flex flex-col gap-0.5">
            <span className="num text-2xl leading-none font-semibold">{value}</span>
            {hint ? <span className="num text-xs text-muted">{hint}</span> : null}
          </div>
        </Card.Content>
      </Card>
    </Link>
  );
}

export function OverviewPage() {
  const { t, language } = usePrefs();
  const { workspace } = useWorkspace();
  const wp = useWorkspacePath();

  /*
   * A failure of any of these five endpoints must be **visible**.
   *
   * This used to be a bare `await Promise.all([...])` with no try/catch: any reject meant
   * `setData` was never reached, `data` stayed `null` forever, and the page stayed on the
   * skeleton forever; visually a blank page,
   * with no hint on screen. After RBAC shipped this path really does get hit:
   * the gateway only checks "is this person in the member table", while these endpoints check "is
   * the role at least viewer",
   * so someone with an abnormal role in the member table gets in, and all these endpoints return 403.
   *
   * See the file header of `lib/useAsyncData.ts`.
   *
   * # ⚠️ Why `listRuns({limit: 5})` + `getWorkspaceUsage()`, two calls, instead of one full fetch
   *
   * This page needs two things: the **latest 5** runs, and three **full** stat numbers (total
   * tokens,
   * total runs, failure count). It used to feed both from one `listRuns()` with no arguments:
   * the first 5 via `slice(0, 5)`, the three numbers via `reduce` / `filter` over all runs in the
   * browser.
   *
   * That `listRuns()` takes 62.5ms / 19.2MB in a workspace with 5000 runs (measured with
   * `BenchmarkListRunsPaged20`, and **growing linearly with the total**), while this page shows
   * only 5 of them.
   *
   * **But only adding `limit: 5` is wrong, and the worst kind of wrong**: the three numbers would
   * silently become
   * "only counting the latest 5"; the page still renders a number, it is just the wrong number:
   * zero errors, e2e all green, and code review would not see it either (the change looks like it
   * just added a parameter).
   *
   * So the stats now come from `getWorkspaceUsage`, which computes these three numbers **on the
   * server**
   * (`server/core/usage.go`), on exactly the same basis as the three lines the frontend used to
   * have:
   *
   *   | What this page needs | Old frontend logic | Now taken from |
   *   | --- | --- | --- |
   *   | Total runs | `runs.length` | `usage.totalRuns` |
   *   | Failures | `runs.filter(r => r.errorSummary).length` | `usage.failedRuns` |
   *   | Total tokens | `sum(input + output)` | `usage.usage.input_tokens + output_tokens` |
   *
   * The failure check on both sides is **errorSummary non-empty** (not status: RunStatus has no
   * "failed"
   * value, and terminated covers both normal endings and errors). The file header of the backend's
   * usage.go writes this basis next to
   * this page; change one side and you must change the other.
   *
   * ⚠️ **Tokens only add input + output, not the two cache fields**, matching the old logic here.
   * `usage.usage` has all four fields; adding them all on a whim would make this number grow out
   * of nowhere with no error.
   *
   * # What this change actually saves (do not overstate the gain)
   *
   * `getWorkspaceUsage` is still one full scan on the server (`ListRunsForUsage`), so
   * **the server's scan cost has not gone away**; UsagePage was paying it already. What is
   * really saved is the other two parts,
   * which happen to be the bulk of the 62.5ms (measured breakdown: doc transfer 57%,
   * deserialization 32%,
   * SQL sort scan only 11%):
   *
   *   1. **The 19MB run list no longer travels over the network to the browser**; it becomes a few
   *      KB of aggregated JSON;
   *   2. The browser no longer parses those 19MB, nor walks all runs to reduce.
   *
   * And the `listRuns({limit: 5})` call dropped from 62.5ms to 5.6ms (allocations down to 1/270),
   * and **its time no longer depends on the total**. The two requests run concurrently in the same
   * `Promise.all`, with no serial waiting.
   */
  const fetch = useCallback(
    async () => {
      const [agents, skills, apps, recent, usage] = await Promise.all([
        api.listAgents(workspace.id),
        api.listSkills(workspace.id),
        api.listFeishuApps(workspace.id),
        // Only the latest 5. **This 5 must match the number rendered below**: it used to fetch
        // everything and then
        // `slice(0, 5)`, so changing the rendered count did not touch the request; now both places
        // must change together.
        api.listRuns(workspace.id, { limit: 5 }),
        api.getWorkspaceUsage(workspace.id),
      ]);
      return { agents, skills, apps, recent, usage };
    },
    [workspace.id],
  );
  const { data, error, isLoading, reload } = useAsyncData(fetch, t("common.loadFailedUnknown"));

  if (isLoading) {
    return (
      <PageBody>
        <ListSkeleton rows={3} />
      </PageBody>
    );
  }

  if (error) {
    return (
      <PageBody>
        <PageHeader title={workspace.name} description={t("overview.subtitle")} />
        <AsyncErrorBlock error={error} onRetry={reload} />
      </PageBody>
    );
  }

  if (!data) return null;

  const { agents, skills, apps, recent, usage } = data;
  const activeAgents = agents.filter((a) => a.status === "active").length;
  // All three numbers come from the server aggregate, **not computed from `recent`**, which has
  // only 5 rows.
  // Tokens only add input + output (not the two cache fields), matching this page's old basis.
  const totalTokens = usage.usage.input_tokens + usage.usage.output_tokens;
  const failed = usage.failedRuns;
  const totalRuns = usage.totalRuns;

  // When not a single workspace is set up, give a clear next step instead of a pile of zeros
  if (agents.length === 0) {
    return (
      <PageBody>
        <PageHeader title={workspace.name} description={t("overview.subtitle")} />
        <Empty
          icon="gravity-ui:face-robot"
          title={t("overview.startTitle")}
          description={t("overview.startDesc")}
          action={
            <Link to={wp("/agents/new")} className="no-underline">
              <Button data-testid="overview-new-agent">{t("agent.new")}</Button>
            </Link>
          }
        />
      </PageBody>
    );
  }

  return (
    <PageBody>
      <PageHeader
        title={workspace.name}
        description={workspace.description || t("overview.subtitle")}
        actions={
          <Link to={wp("/agents/new")} className="no-underline">
            <Button>{t("agent.new")}</Button>
          </Link>
        }
      />

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <MetricCard
          label="Agent"
          value={String(agents.length)}
          hint={t("overview.metricAgentsHint", { count: activeAgents })}
          icon="gravity-ui:face-robot"
          to={wp("/agents")}
        />
        <MetricCard
          label={t("overview.metricSkills")}
          value={String(skills.length)}
          hint={t("overview.metricSkillsHint")}
          icon="gravity-ui:puzzle"
          to={wp("/skills")}
        />
        <MetricCard
          label={t("nav.feishu")}
          value={String(apps.length)}
          hint={t("overview.metricFeishuHint", { count: apps.filter((a) => a.status === "connected").length })}
          icon="gravity-ui:comment"
          to={wp("/feishu")}
        />
        <MetricCard
          label={t("overview.metricTokens")}
          value={formatTokens(totalTokens)}
          hint={
            t("overview.metricRuns", { count: totalRuns }) +
            (failed ? t("overview.metricFailed", { count: failed }) : "")
          }
          icon="gravity-ui:cpu"
          to={wp("/runs")}
        />
      </div>

      <Card>
        <Card.Content className="flex flex-col gap-4 pt-6">
          <div className="flex items-center justify-between">
            <h2 className="text-base font-medium">{t("overview.recentRuns")}</h2>
            <Link to={wp("/runs")} className="no-underline">
              <Button variant="ghost" size="sm">
                {t("overview.viewAll")}
              </Button>
            </Link>
          </div>

          {/*
            `recent` is already the first 5 cut on the server (`listRuns({limit: 5})`), so no more
            `slice` here. Checking it alone is enough for the empty state: limit only affects how many are fetched; if none come back there really are none.
          */}
          {recent.length === 0 ? (
            <p className="py-6 text-center text-sm text-muted">{t("overview.noRuns")}</p>
          ) : (
            <div className="flex flex-col">
              {recent.map((r, i) => (
                <div key={r.id}>
                  {i > 0 ? <Separator /> : null}
                  <Link
                    to={wp(`/runs/${r.id}`)}
                    className="flex items-center gap-3 py-3 no-underline"
                    data-testid={`overview-run-${r.id}`}
                  >
                    <RunStatusChip status={r.status} hasError={Boolean(r.errorSummary)} />
                    <span className="min-w-0 flex-1 truncate text-sm">{r.title}</span>
                    <span className="hidden shrink-0 items-center gap-1.5 text-xs text-muted sm:flex">
                      {r.agentName}
                      {r.agentDeleted ? <AgentDeletedChip /> : null}
                    </span>
                    <span className="num w-14 shrink-0 text-right text-xs text-muted">
                      {formatDuration(r.durationMs)}
                    </span>
                    <span className="w-20 shrink-0 text-right text-xs text-muted">
                      {formatRelative(r.startedAt, language)}
                    </span>
                  </Link>
                </div>
              ))}
            </div>
          )}
        </Card.Content>
      </Card>
    </PageBody>
  );
}
