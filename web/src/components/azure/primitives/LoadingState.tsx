import { useEffect, useState } from "react";

const chevron = Array.from({ length: 9 }, (_, i) => {
  const row = Math.floor(i / 3), column = i % 3;
  return (column + Math.abs(row - 1)) * 90;
});

function LoaderGrid() {
  return (
    <span aria-hidden className="grid shrink-0 grid-cols-[repeat(3,4px)] gap-[1.5px]">
      {chevron.map((delay, index) => (
        <span
          key={index}
          className="size-[4px] rounded-[1px] bg-ink"
          style={{ opacity: 0.15, animation: `pixel-on 650ms ease-in-out ${delay}ms infinite` }}
        />
      ))}
    </span>
  );
}

function useElapsed() {
  const [ds, setDs] = useState(0);
  useEffect(() => {
    const timer = setInterval(() => setDs((current) => current + 1), 100);
    return () => clearInterval(timer);
  }, []);
  const total = ds / 10;
  if (total < 60) return `${total.toFixed(1)}s`;
  return `${Math.floor(total / 60)}m ${(total % 60).toFixed(1)}s`;
}

/** Loading primitive adapted from upstream; external video is intentionally omitted. */
export function AzureLoadingState({ label = "Churning" }: { label?: string }) {
  const elapsed = useElapsed();
  return (
    <div role="status" aria-label={label} className="flex w-fit items-center gap-2.5">
      <LoaderGrid />
      <span
        className="bg-clip-text text-[13px] font-medium text-transparent"
        style={{
          backgroundImage: "linear-gradient(90deg, var(--ink-3) 35%, var(--ink) 50%, var(--ink-3) 65%)",
          backgroundSize: "200% 100%",
          animation: "shimmer-text 1.4s linear infinite",
        }}
      >
        {label}
      </span>
      <span className="font-mono text-[12px] tabular-nums text-ink-3">{elapsed}</span>
    </div>
  );
}
