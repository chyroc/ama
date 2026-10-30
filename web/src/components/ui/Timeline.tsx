/**
 * Vertical timeline: a track on the left (dot + connecting line), content on the right.
 *
 *   <Timeline>
 *     <Timeline.Item status="success">
 *       <Timeline.Rail>
 *         <Timeline.Marker><Icon … /></Timeline.Marker>
 *         <Timeline.Connector />
 *       </Timeline.Rail>
 *       <Timeline.Content>…</Timeline.Content>
 *     </Timeline.Item>
 *   </Timeline>
 *
 * The dot's fill and border come from the theme color via color-mix, by status (12% fill + 55%
 * border), so they follow both light and dark themes. The last item's connecting line is hidden.
 */

import { createContext, useContext, type HTMLAttributes, type ReactNode } from "react";

export type TimelineStatus = "default" | "muted" | "current" | "success" | "warning" | "danger";

/** status → theme color variable. */
const STATUS_COLOR: Record<TimelineStatus, string> = {
  default: "var(--foreground)",
  muted: "var(--muted)",
  current: "var(--accent)",
  success: "var(--success)",
  warning: "var(--warning)",
  danger: "var(--danger)",
};

const ItemContext = createContext<TimelineStatus>("default");

function cx(...parts: (string | undefined | false)[]) {
  return parts.filter(Boolean).join(" ");
}

function TimelineRoot({
  className,
  density: _density,
  ...props
}: HTMLAttributes<HTMLOListElement> & { density?: "compact" | "comfortable" }) {
  return <ol {...props} className={cx("ama-timeline flex flex-col", className)} />;
}

function TimelineItem({
  status = "default",
  className,
  children,
  ...props
}: HTMLAttributes<HTMLLIElement> & { status?: TimelineStatus; children: ReactNode }) {
  return (
    <ItemContext.Provider value={status}>
      <li {...props} data-status={status} className={cx("ama-timeline__item flex gap-3", className)}>
        {children}
      </li>
    </ItemContext.Provider>
  );
}

function TimelineRail({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div {...props} className={cx("flex shrink-0 flex-col items-center", className)} />;
}

function TimelineMarker({ className, style, ...props }: HTMLAttributes<HTMLDivElement>) {
  const color = STATUS_COLOR[useContext(ItemContext)];
  return (
    <div
      {...props}
      style={{
        backgroundColor: `color-mix(in oklab, ${color} 12%, var(--surface))`,
        borderColor: `color-mix(in oklab, ${color} 55%, transparent)`,
        ...style,
      }}
      className={cx(
        "flex size-7 shrink-0 items-center justify-center rounded-full border [&_svg]:size-[55%]",
        className,
      )}
    />
  );
}

function TimelineConnector({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      {...props}
      aria-hidden
      className={cx("ama-timeline__connector my-1 w-px flex-1 bg-border", className)}
    />
  );
}

function TimelineContent({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div {...props} className={cx("min-w-0 flex-1 pt-1", className)} />;
}

export const Timeline = Object.assign(TimelineRoot, {
  Item: TimelineItem,
  Rail: TimelineRail,
  Marker: TimelineMarker,
  Connector: TimelineConnector,
  Content: TimelineContent,
});
