/**
 * Usage and insights dashboard.
 *
 * Split with the overview page: the overview answers "how are things now" (a few numbers + recent
 * runs); this page answers
 * "over a period, who is spending, who is getting slower, who is failing". So the overview
 * deliberately has no charts
 * (see the comment at the top of OverviewPage); all charts are on this page.
 *
 * # Why the four charts have these shapes
 *
 * 1. **Call counts use horizontal bars**: agent names can be long, and vertical bars' x-axis
 *    labels would be squeezed diagonal.
 * 2. **Durations use grouped bars + three shades of one hue**: P50/P90/mean are **different
 *    percentiles of the same quantity** (ordered);
 *    giving them three different hues would falsely report "ordered" as "three unrelated
 *    identities". All three share one y axis
 *    (all milliseconds), never a dual axis: how the two scales of a dual axis line up is arbitrary,
 *    which invents
 *    a correlation that is not in the data.
 * 3. **Token trend uses stacked area, drawing only Input / Output**: Total = Input + Output,
 *    and **the stacked height itself is Total**; drawing a third line would draw the same data
 *    twice, and Total's
 *    magnitude would flatten Output's changes. Total appears in the tooltip and the top KPIs.
 * 4. **Success/failure uses a meter bar, not a two-color pie**: a two-slice pie does not show the
 *    ratio well
 *    (the eye compares length far better than angle); a progress bar with a percentage carries
 *    more information.
 *
 * # Colors
 *
 * Everything uses the --chart-* tokens in index.css, one set each for light and dark, all checked
 * with the dataviz validator.
 * **Do not hard-code hex here**: dark mode is not light mode with lightness flipped; hard-coded
 * colors on the other background
 * are either invisible or glaring. Semantic colors (success/failure) use --success / --danger.
 *
 * # Invariants
 *
 * The page has two places where "the sums should be equal"; both are guaranteed by the backend,
 * here we only display:
 * sum of daily runs == totalRuns; sum of per-agent runs == totalRuns.
 */

import { useCallback, useMemo } from "react";
import { Card } from "@heroui/react";
import { AreaChart, BarChart, ChartTooltip } from "@/components/ui/Chart";
import { api } from "@/data/api";
import { useWorkspace } from "@/app/WorkspaceGate";
import { AsyncErrorBlock, Empty, ListSkeleton, PageBody, PageHeader } from "@/components/Page";
import { AgentDeletedChip } from "@/components/Page";
import { useAsyncData } from "@/lib/useAsyncData";
import { formatDuration, formatNumber, formatTokens } from "@/lib/format";
import { usePrefs } from "@/prefs/PrefsContext";

/**
 * Compact duration for axis ticks: **only one unit, no spaces**.
 *
 * formatDuration cannot be used directly: it gives a two-part form like "66m 40s", which in a
 * 48px-wide y axis
 * recharts wraps into two lines, so ticks and grid lines no longer line up (in screenshots the
 * tick text is offset up and down).
 * Tooltips and tables still use formatDuration: there is enough width there, and two parts are
 * more precise.
 */
function compactDuration(ms: number): string {
  if (ms < 1000) return `${Math.round(ms)}ms`;
  const sec = ms / 1000;
  if (sec < 60) return `${Math.round(sec)}s`;
  const min = sec / 60;
  if (min < 60) return `${Math.round(min)}m`;
  return `${Math.round(min / 60)}h`;
}

/** Legend swatch + name. HeroUI charts have no legend component; this one shape is hand-written for the whole repo. */
function LegendItem({ color, label }: { color: string; label: string }) {
  return (
    <div className="flex items-center gap-1.5">
      <span className="size-2.5 r-chip shrink-0" style={{ backgroundColor: color }} />
      <span className="text-xs text-muted">{label}</span>
    </div>
  );
}

/** Shared shell for chart cards: title + description + legend at the top right. */
function ChartCard({
  title,
  description,
  legend,
  testId,
  children,
}: {
  title: string;
  description: string;
  legend?: React.ReactNode;
  testId: string;
  children: React.ReactNode;
}) {
  return (
    <Card data-testid={testId}>
      <Card.Content className="flex flex-col gap-4 pt-6">
        <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-2">
          <div className="flex flex-col gap-0.5">
            <h2 className="text-base font-medium">{title}</h2>
            <p className="text-xs text-muted">{description}</p>
          </div>
          {legend ? <div className="flex flex-wrap items-center gap-3">{legend}</div> : null}
        </div>
        {children}
      </Card.Content>
    </Card>
  );
}

function Stat({
  label,
  value,
  hint,
  testId,
}: {
  label: string;
  value: string;
  hint: string;
  testId: string;
}) {
  return (
    <div className="flex flex-col gap-1" data-testid={testId}>
      <span className="text-xs text-muted">{label}</span>
      <span className="num text-2xl leading-none font-semibold">{value}</span>
      <span className="num text-xs text-muted">{hint}</span>
    </div>
  );
}

export function UsagePage() {
  const { t, language } = usePrefs();
  const { workspace } = useWorkspace();

  // Failures must be visible, not stuck on the skeleton (see the file header of lib/useAsyncData.ts).
  const fetch = useCallback(() => api.getWorkspaceUsage(workspace.id), [workspace.id]);
  const {
    data: usage,
    error,
    isLoading,
    reload,
  } = useAsyncData(fetch, t("common.loadFailedUnknown"));

  // Charts want a wide table (one row per category, one column per series), while the API gives an
  // array of objects;
  // the shape is converted once here. t is a dependency: legends/labels must be recomputed when
  // switching language.
  const charts = useMemo(() => {
    if (!usage) return null;
    return {
      calls: usage.agents.map((a) => ({ name: a.agentName, runs: a.runs })),
      // Only draw agents with finished samples: when completedRuns is 0 all three percentiles are 0,
      // drawn as three bars flat on the floor, and readers would think "this agent takes nearly 0 ms".
      latency: usage.agents
        .filter((a) => a.completedRuns > 0)
        .map((a) => ({
          name: a.agentName,
          p50: a.p50Ms,
          p90: a.p90Ms,
          avg: a.avgMs,
        })),
      tokens: usage.daily.map((d) => ({
        date: d.date.slice(5), // keep only MM-DD; the year is noise over this span
        input: d.usage.input_tokens,
        output: d.usage.output_tokens,
      })),
    };
  }, [usage]);

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
        <AsyncErrorBlock error={error} onRetry={reload} />
      </PageBody>
    );
  }

  if (!usage || !charts) return null;

  const totalTokens = usage.usage.input_tokens + usage.usage.output_tokens;
  const succeeded = usage.totalRuns - usage.failedRuns;
  // With no runs at all the denominator is 0: give 0 explicitly rather than computing NaN.
  const successRate = usage.totalRuns > 0 ? (succeeded / usage.totalRuns) * 100 : 0;

  if (usage.totalRuns === 0) {
    return (
      <PageBody>
        <PageHeader title={t("usage.title")} description={t("usage.subtitle")} />
        <Empty
          icon="gravity-ui:chart-line"
          title={t("usage.emptyTitle")}
          description={t("usage.emptyDesc")}
        />
      </PageBody>
    );
  }

  return (
    <PageBody>
      <PageHeader title={t("usage.title")} description={t("usage.subtitle")} />

      {/* KPI row. Total tokens is given here, so the trend chart need not draw a third line. */}
      <Card>
        <Card.Content className="pt-6">
          <div className="grid gap-6 sm:grid-cols-2 lg:grid-cols-4" data-testid="usage-kpis">
            <Stat
              label={t("usage.statRuns")}
              value={formatNumber(usage.totalRuns, language)}
              hint={t("usage.statRunsHint", { count: usage.completedRuns })}
              testId="usage-kpi-runs"
            />
            <Stat
              label={t("usage.statSuccessRate")}
              value={`${successRate.toFixed(1)}%`}
              hint={t("usage.statSuccessHint", { failed: usage.failedRuns })}
              testId="usage-kpi-success"
            />
            <Stat
              label={t("usage.statTokens")}
              value={formatTokens(totalTokens)}
              hint={t("usage.statTokensHint", {
                input: formatTokens(usage.usage.input_tokens),
                output: formatTokens(usage.usage.output_tokens),
              })}
              testId="usage-kpi-tokens"
            />
            <Stat
              label={t("usage.statAvgDuration")}
              value={usage.completedRuns > 0 ? formatDuration(usage.avgMs) : "—"}
              hint={
                usage.completedRuns > 0
                  ? t("usage.statAvgHint", {
                      p50: formatDuration(usage.p50Ms),
                      p90: formatDuration(usage.p90Ms),
                    })
                  : t("usage.noLatencyData")
              }
              testId="usage-kpi-avg"
            />
          </div>
        </Card.Content>
      </Card>

      {/* Success / failure: one meter, not a two-color pie */}
      <ChartCard
        title={t("usage.chartOutcomeTitle")}
        description={t("usage.chartOutcomeDesc")}
        testId="usage-outcome"
        legend={
          <>
            <LegendItem color="var(--success)" label={t("usage.succeeded")} />
            <LegendItem color="var(--danger)" label={t("usage.failed")} />
          </>
        }
      >
        <div className="flex flex-col gap-2">
          {/*
            r-chip (6px) rather than rounded-full: full rounding on this 8px-high track would let
            the rounded corner eat most of the small failure segment, distorting the ratio.
          */}
          <div
            className="flex h-2 w-full overflow-hidden r-chip bg-surface-secondary"
            role="img"
            aria-label={`${t("usage.succeeded")} ${succeeded} / ${t("usage.failed")} ${usage.failedRuns}`}
          >
            <div style={{ width: `${successRate}%`, backgroundColor: "var(--success)" }} />
            {/* Leave a 2px surface gap between the two segments, so the two color blocks do not touch and blur into one */}
            {usage.failedRuns > 0 ? (
              <>
                <div className="w-0.5 shrink-0 bg-surface" />
                <div className="flex-1" style={{ backgroundColor: "var(--danger)" }} />
              </>
            ) : null}
          </div>
          <div className="flex flex-wrap items-center justify-between gap-x-6 text-xs">
            <span className="num text-muted">
              {t("usage.succeeded")} {formatNumber(succeeded, language)} ·{" "}
              {t("usage.failed")} {formatNumber(usage.failedRuns, language)}
            </span>
            <span className="num font-medium">{successRate.toFixed(1)}%</span>
          </div>
        </div>
      </ChartCard>

      {/* Call counts per Agent: horizontal bars */}
      <ChartCard
        title={t("usage.chartCallsTitle")}
        description={t("usage.chartCallsDesc")}
        testId="usage-calls"
      >
        <BarChart
          data={charts.calls}
          layout="vertical"
          height={Math.max(140, charts.calls.length * 44)}
        >
          <BarChart.Grid horizontal={false} />
          <BarChart.XAxis type="number" tickMargin={8} allowDecimals={false} />
          <BarChart.YAxis dataKey="name" type="category" width={104} tickMargin={8} />
          <BarChart.Bar
            barSize={14}
            dataKey="runs"
            fill="var(--chart-1)"
            name={t("usage.tableRuns")}
            radius={[0, 4, 4, 0]}
          />
          <BarChart.Tooltip
            content={
              <BarChart.TooltipContent
                valueFormatter={(v) => t("usage.runsUnit", { count: Number(v) })}
              />
            }
          />
        </BarChart>
      </ChartCard>

      {/* Execution duration distribution: P50 / P90 / mean, three shades of one hue, one shared axis */}
      <ChartCard
        title={t("usage.chartLatencyTitle")}
        description={t("usage.chartLatencyDesc")}
        testId="usage-latency"
        legend={
          <>
            <LegendItem color="var(--chart-ord-1)" label={t("usage.legendP50")} />
            <LegendItem color="var(--chart-ord-2)" label={t("usage.legendP90")} />
            <LegendItem color="var(--chart-ord-3)" label={t("usage.legendAvg")} />
          </>
        }
      >
        {charts.latency.length === 0 ? (
          <p className="py-8 text-center text-sm text-muted">{t("usage.noLatencyData")}</p>
        ) : (
          <>
            <BarChart data={charts.latency} height={240}>
              <BarChart.Grid vertical={false} />
              <BarChart.XAxis dataKey="name" tickMargin={8} />
              {/*
                **Log scale**, and it is labeled on the axis (the note next to the legend).
                A linear scale does not work here: durations have a long tail by nature; in measured
                data the slowest P90 was
                80 times the fastest P50 (65 minutes vs 49 seconds). On a linear scale that P90 alone
                fills the height,
                and the other two are squashed into 1px lines on the floor; P50 effectively vanishes
                from the chart, and it is exactly
                "how fast is typical", the most-read number. So we switch to log; the cost is bar
                length is no longer proportional
                (labeled on the axis, and exact numbers are in the table below).
                The domain's lower bound is 1, not 0: log(0) is negative infinity, and 0 would render
                the whole chart blank.
              */}
              <BarChart.YAxis
                width={44}
                scale="log"
                domain={[1, "dataMax"]}
                allowDataOverflow
                tickFormatter={(v: number) => compactDuration(v)}
              />
              <BarChart.Bar
                barSize={10}
                dataKey="p50"
                fill="var(--chart-ord-1)"
                name={t("usage.legendP50")}
                radius={[4, 4, 0, 0]}
              />
              <BarChart.Bar
                barSize={10}
                dataKey="p90"
                fill="var(--chart-ord-2)"
                name={t("usage.legendP90")}
                radius={[4, 4, 0, 0]}
              />
              <BarChart.Bar
                barSize={10}
                dataKey="avg"
                fill="var(--chart-ord-3)"
                name={t("usage.legendAvg")}
                radius={[4, 4, 0, 0]}
              />
              <BarChart.Tooltip
                content={
                  <BarChart.TooltipContent valueFormatter={(v) => formatDuration(Number(v))} />
                }
              />
            </BarChart>
            {/*
              Sample size must be shown. A P90 from three runs and a P90 from three thousand runs
              are two different things;
              not labeling it means using the former as the latter.
            */}
            <div className="flex flex-wrap gap-x-4 gap-y-1">
              {usage.agents
                .filter((a) => a.completedRuns > 0)
                .map((a) => (
                  <span key={a.agentId} className="num text-xs text-muted">
                    {a.agentName} · {t("usage.sampleSize", { count: a.completedRuns })}
                  </span>
                ))}
            </div>
          </>
        )}
      </ChartCard>

      {/* Token trend: Input / Output stacked; the stacked height is Total */}
      <ChartCard
        title={t("usage.chartTokensTitle")}
        description={t("usage.chartTokensDesc")}
        testId="usage-tokens"
        legend={
          <>
            <LegendItem color="var(--chart-1)" label={t("usage.legendInput")} />
            <LegendItem color="var(--chart-2)" label={t("usage.legendOutput")} />
          </>
        }
      >
        <AreaChart data={charts.tokens} height={240}>
          <defs>
            {/* Each series needs its own gradient id; sharing would let the later one overwrite the earlier one's fill */}
            <linearGradient id="usage-input-fill" x1="0" x2="0" y1="0" y2="1">
              <stop offset="0%" stopColor="var(--chart-1)" stopOpacity={0.24} />
              <stop offset="100%" stopColor="var(--chart-1)" stopOpacity={0.04} />
            </linearGradient>
            <linearGradient id="usage-output-fill" x1="0" x2="0" y1="0" y2="1">
              <stop offset="0%" stopColor="var(--chart-2)" stopOpacity={0.24} />
              <stop offset="100%" stopColor="var(--chart-2)" stopOpacity={0.04} />
            </linearGradient>
          </defs>
          <AreaChart.Grid vertical={false} />
          <AreaChart.XAxis dataKey="date" tickMargin={8} />
          <AreaChart.YAxis width={44} tickFormatter={(v: number) => formatTokens(v)} />
          <AreaChart.Area
            dataKey="input"
            dot={false}
            fill="url(#usage-input-fill)"
            name={t("usage.legendInput")}
            stackId="tokens"
            stroke="var(--chart-1)"
            strokeWidth={2}
            type="monotone"
          />
          <AreaChart.Area
            dataKey="output"
            dot={false}
            fill="url(#usage-output-fill)"
            name={t("usage.legendOutput")}
            stackId="tokens"
            stroke="var(--chart-2)"
            strokeWidth={2}
            type="monotone"
          />
          <AreaChart.Tooltip
            content={({ active, label, payload }) => {
              if (!active || !payload?.length) return null;
              // Total is computed and given here, rather than drawn as a third series: it is the
              // stacked height.
              const total = payload.reduce((s, e) => s + Number(e.value ?? 0), 0);
              return (
                <ChartTooltip>
                  <ChartTooltip.Header>{String(label)}</ChartTooltip.Header>
                  {payload.map((entry) => (
                    <ChartTooltip.Item key={String(entry.dataKey)}>
                      <ChartTooltip.Indicator color={entry.color ?? entry.stroke} />
                      <ChartTooltip.Label>{String(entry.name ?? "")}</ChartTooltip.Label>
                      <ChartTooltip.Value>{formatTokens(Number(entry.value ?? 0))}</ChartTooltip.Value>
                    </ChartTooltip.Item>
                  ))}
                  <ChartTooltip.Item>
                    <ChartTooltip.Label>{t("usage.legendTotal")}</ChartTooltip.Label>
                    <ChartTooltip.Value>{formatTokens(total)}</ChartTooltip.Value>
                  </ChartTooltip.Item>
                </ChartTooltip>
              );
            }}
          />
        </AreaChart>
      </ChartCard>

      {/* Detail table: charts show trends, the table gives exact numbers. It is also the charts' table view (an accessibility fallback). */}
      <Card>
        <Card.Content className="pt-6">
          <div className="overflow-x-auto">
            <table className="w-full text-sm" data-testid="usage-table">
              <thead>
                <tr className="border-b border-separator text-xs text-muted">
                  <th className="py-2 pr-4 text-left font-normal">{t("usage.tableAgent")}</th>
                  <th className="py-2 pr-4 text-right font-normal">{t("usage.tableRuns")}</th>
                  <th className="py-2 pr-4 text-right font-normal">{t("usage.tableFailed")}</th>
                  <th className="py-2 pr-4 text-right font-normal">{t("usage.legendP50")}</th>
                  <th className="py-2 pr-4 text-right font-normal">{t("usage.legendP90")}</th>
                  <th className="py-2 text-right font-normal">{t("usage.tableTokens")}</th>
                </tr>
              </thead>
              <tbody>
                {usage.agents.map((a) => (
                  <tr
                    key={a.agentId}
                    className="border-b border-separator last:border-0"
                    data-testid={`usage-row-${a.agentId}`}
                  >
                    <td className="py-2.5 pr-4">
                      <span className="flex items-center gap-1.5">
                        <span className="truncate">{a.agentName}</span>
                        {a.agentDeleted ? <AgentDeletedChip /> : null}
                      </span>
                    </td>
                    <td className="num py-2.5 pr-4 text-right">
                      {formatNumber(a.runs, language)}
                    </td>
                    <td
                      className={[
                        "num py-2.5 pr-4 text-right",
                        a.failedRuns > 0 ? "text-danger" : "text-muted",
                      ].join(" ")}
                    >
                      {formatNumber(a.failedRuns, language)}
                    </td>
                    {/* With no finished samples give "—", not 0ms: 0 would read as "very fast" */}
                    <td className="num py-2.5 pr-4 text-right">
                      {a.completedRuns > 0 ? formatDuration(a.p50Ms) : "—"}
                    </td>
                    <td className="num py-2.5 pr-4 text-right">
                      {a.completedRuns > 0 ? formatDuration(a.p90Ms) : "—"}
                    </td>
                    <td className="num py-2.5 text-right">
                      {formatTokens(a.usage.input_tokens + a.usage.output_tokens)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card.Content>
      </Card>
    </PageBody>
  );
}

export default UsagePage;
