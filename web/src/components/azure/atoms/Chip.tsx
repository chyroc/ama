import type { ReactNode } from "react";
import { cn } from "../cn";

/** Monospace token chip, adapted from the upstream Chip primitive. */
export function AzureChip({
  children,
  tone = "neutral",
  className,
}: {
  children: ReactNode;
  tone?: "neutral" | "accent" | "orange";
  className?: string;
}) {
  const tones = {
    neutral: "bg-inset text-ink-2",
    accent: "bg-accent-tint text-accent-ink",
    orange: "bg-orange-tint text-orange",
  };
  return (
    <code
      className={cn(
        "inline rounded-md px-1.5 py-0.5 align-[-1px] font-mono text-[12px] leading-none",
        tones[tone],
        className,
      )}
    >
      {children}
    </code>
  );
}
