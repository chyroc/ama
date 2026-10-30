/**
 * A thin wrapper over recharts: one look for axes, grid and tooltip; all colors use theme tokens.
 *
 * The shape is a compound component (`BarChart.XAxis`, `AreaChart.Tooltip` …). The children are recharts'
 * own components plus default props. recharts 3 collects children through context, not by scanning element types,
 * so wrapping does not affect how it recognizes them.
 *
 * Only the /usage page uses it, and that page uses React.lazy, so recharts lands only in that chunk.
 */

import type { ComponentProps, ReactNode } from "react";
import {
  Area,
  AreaChart as RAreaChart,
  Bar,
  BarChart as RBarChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";

const TICK = { fill: "var(--muted)", fontSize: 12 };
const MARGIN = { top: 8, right: 8, bottom: 0, left: 0 };

function Grid(props: ComponentProps<typeof CartesianGrid>) {
  return <CartesianGrid stroke="var(--border)" strokeDasharray="3 3" {...props} />;
}

function ChartXAxis(props: ComponentProps<typeof XAxis>) {
  return <XAxis axisLine={false} tickLine={false} tick={TICK} {...props} />;
}

function ChartYAxis(props: ComponentProps<typeof YAxis>) {
  return <YAxis axisLine={false} tickLine={false} tick={TICK} {...props} />;
}

function ChartTooltipSlot(props: ComponentProps<typeof Tooltip>) {
  return (
    <Tooltip
      cursor={{ fill: "color-mix(in oklab, var(--foreground) 6%, transparent)" }}
      isAnimationActive={false}
      {...props}
    />
  );
}

/* ------------------------------------------------------------------ */
/* ChartTooltip: the tooltip itself (card + rows)                                 */
/* ------------------------------------------------------------------ */

function ChartTooltipRoot({ children }: { children: ReactNode }) {
  return (
    <div className="min-w-36 rounded-lg border border-border bg-overlay px-3 py-2 text-xs text-foreground shadow-overlay">
      <div className="flex flex-col gap-1">{children}</div>
    </div>
  );
}

function ChartTooltipHeader({ children }: { children: ReactNode }) {
  return <div className="mb-0.5 font-medium">{children}</div>;
}

function ChartTooltipItem({ children }: { children: ReactNode }) {
  return <div className="flex items-center gap-2">{children}</div>;
}

function ChartTooltipIndicator({ color }: { color?: string }) {
  return <span aria-hidden className="size-2 shrink-0 rounded-sm" style={{ backgroundColor: color }} />;
}

function ChartTooltipLabel({ children }: { children: ReactNode }) {
  return <span className="flex-1 text-muted">{children}</span>;
}

function ChartTooltipValue({ children }: { children: ReactNode }) {
  return <span className="num font-medium tabular-nums">{children}</span>;
}

export const ChartTooltip = Object.assign(ChartTooltipRoot, {
  Header: ChartTooltipHeader,
  Item: ChartTooltipItem,
  Indicator: ChartTooltipIndicator,
  Label: ChartTooltipLabel,
  Value: ChartTooltipValue,
});

/**
 * Generic tooltip content: title + one row per series. Used as `<X.Tooltip content={<X.TooltipContent />} />`;
 * recharts injects active / payload / label.
 */
interface TooltipPayloadEntry {
  dataKey?: unknown;
  name?: unknown;
  value?: unknown;
  color?: string;
  fill?: string;
  stroke?: string;
  payload?: Record<string, unknown>;
}

function TooltipContent({
  active,
  payload,
  label,
  valueFormatter = (v) => String(v),
}: {
  active?: boolean;
  payload?: readonly TooltipPayloadEntry[];
  label?: unknown;
  valueFormatter?: (value: unknown) => ReactNode;
}) {
  if (!active || !payload?.length) return null;
  return (
    <ChartTooltip>
      {label != null && label !== "" ? <ChartTooltip.Header>{String(label)}</ChartTooltip.Header> : null}
      {payload.map((entry, i) => (
        <ChartTooltip.Item key={String(entry.dataKey ?? i)}>
          <ChartTooltip.Indicator color={entry.color ?? entry.fill ?? entry.stroke} />
          <ChartTooltip.Label>{String(entry.name ?? entry.dataKey ?? "")}</ChartTooltip.Label>
          <ChartTooltip.Value>{valueFormatter(entry.value)}</ChartTooltip.Value>
        </ChartTooltip.Item>
      ))}
    </ChartTooltip>
  );
}

/* ------------------------------------------------------------------ */
/* BarChart / AreaChart                                                */
/* ------------------------------------------------------------------ */

function BarChartRoot({
  height,
  children,
  ...props
}: ComponentProps<typeof RBarChart> & { height: number }) {
  return (
    <ResponsiveContainer width="100%" height={height}>
      <RBarChart margin={MARGIN} {...props}>
        {children}
      </RBarChart>
    </ResponsiveContainer>
  );
}

function AreaChartRoot({
  height,
  children,
  ...props
}: ComponentProps<typeof RAreaChart> & { height: number }) {
  return (
    <ResponsiveContainer width="100%" height={height}>
      <RAreaChart margin={MARGIN} {...props}>
        {children}
      </RAreaChart>
    </ResponsiveContainer>
  );
}

export const BarChart = Object.assign(BarChartRoot, {
  Grid,
  XAxis: ChartXAxis,
  YAxis: ChartYAxis,
  Bar,
  Tooltip: ChartTooltipSlot,
  TooltipContent,
});

export const AreaChart = Object.assign(AreaChartRoot, {
  Grid,
  XAxis: ChartXAxis,
  YAxis: ChartYAxis,
  Area,
  Tooltip: ChartTooltipSlot,
  TooltipContent,
});
