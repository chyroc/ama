import type { ReactNode } from "react";
import { cn } from "../cn";

export type AzureStatusTone = "green" | "orange" | "red" | "accent" | "neutral";

/** Status pill adapted from the upstream StatusPill primitive. */
export function AzureStatusPill({
  tone = "neutral",
  children,
  dot = true,
  className,
}: {
  tone?: AzureStatusTone;
  children?: ReactNode;
  dot?: boolean;
  className?: string;
}) {
  const tones: Record<AzureStatusTone, string> = {
    green: "bg-green-tint text-green",
    orange: "bg-orange-tint text-orange",
    red: "bg-red-tint text-red",
    accent: "bg-accent-tint text-accent-ink",
    neutral: "bg-inset text-ink-2",
  };
  const dotColor: Record<AzureStatusTone, string> = {
    green: "bg-green",
    orange: "bg-orange",
    red: "bg-red",
    accent: "bg-accent",
    neutral: "bg-ink-3",
  };
  return (
    <span
      className={cn(
        "inline-flex h-6 items-center gap-1.5 rounded-full px-2.5 text-[13px] leading-none font-medium",
        tones[tone],
        className,
      )}
    >
      {dot ? <span className={cn("size-1.5 rounded-full", dotColor[tone])} /> : null}
      {children}
    </span>
  );
}
