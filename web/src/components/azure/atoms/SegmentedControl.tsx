import type { KeyboardEvent, ReactNode } from "react";
import { cn } from "../cn";

export interface AzureSegmentOption<T extends string> {
  value: T;
  label: ReactNode;
  testId?: string;
}

/** Segmented control adapted from the upstream primitive; keyboard behavior is preserved. */
export function AzureSegmentedControl<T extends string>({
  options,
  value,
  onChange,
  className,
  label,
}: {
  options: readonly AzureSegmentOption<T>[];
  value: T;
  onChange: (value: T) => void;
  className?: string;
  label: string;
}) {
  const index = options.findIndex((option) => option.value === value);
  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    const next =
      event.key === "Home" ? 0
      : event.key === "End" ? options.length - 1
      : (index + (event.key === "ArrowRight" ? 1 : -1) + options.length) % options.length;
    onChange(options[next].value);
    document.getElementById(options[next].testId ?? "")?.focus();
  };
  return (
    <div
      className={cn("relative inline-grid h-8 select-none rounded-full bg-line/60 p-0.5", className)}
      style={{ gridTemplateColumns: `repeat(${options.length}, 1fr)` }}
      role="tablist"
      aria-label={label}
      onKeyDown={onKeyDown}
    >
      <span
        aria-hidden
        className="absolute inset-y-0.5 rounded-full bg-surface shadow-hairline transition-transform duration-200"
        style={{
          width: `calc((100% - 4px) / ${options.length})`,
          left: 2,
          transform: `translateX(${Math.max(0, index) * 100}%)`,
          transitionTimingFunction: "cubic-bezier(0.23, 1, 0.32, 1)",
        }}
      />
      {options.map((option) => (
        <button
          key={option.value}
          id={option.testId}
          data-testid={option.testId}
          role="tab"
          type="button"
          aria-selected={option.value === value}
          tabIndex={option.value === value ? 0 : -1}
          onClick={() => onChange(option.value)}
          className={cn(
            "relative z-10 rounded-full px-3 text-[13px] font-medium transition-colors duration-150",
            option.value === value ? "text-ink" : "text-ink-3 hover:text-ink-2",
          )}
        >
          {option.label}
        </button>
      ))}
    </div>
  );
}
