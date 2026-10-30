/**
 * Status marker of one run/session.
 *
 * # Why it is in components/ and not left in RunsPage
 *
 * It used to live in `pages/RunsPage.tsx`, imported by the run list, run detail and Agent detail.
 * The session list (`components/ChatSessions.tsx`) needs it too, and **no file in `components/`
 * has ever imported `@/pages/`**: that direction is backwards. Pages are built from components; components should not
 * depend on a page. If ChatSessions really imported RunsPage, the side effect is that the whole run list page
 * (with its filters, `useAsyncData`, icons) would be dragged into the chat page's chunk, and that
 * reports no error; the bundle just grows.
 *
 * So it moved here, and the four call sites now import it from components.
 *
 * # Failure is not a status value; it must be checked separately
 *
 * ⚠️ **`RunStatus` has no "failed"**. A run that errored has status `terminated`, exactly the same as a normal
 * finish; the check is **`errorSummary` non-empty** (same rule as UsageSummary in `types/domain.ts`
 * and OverviewPage). Rendering by status alone would show a crashed session as "finished",
 * and the user would think it ran fine. This is the main reason this component exists, not a side effect.
 */

import { Chip } from "@heroui/react";
import { usePrefs } from "@/prefs/PrefsContext";
import type { RunStatus } from "@/types/domain";

const STATUS_COLOR: Record<RunStatus, "default" | "accent" | "success" | "warning" | "danger"> = {
  running: "accent",
  idle: "success",
  // **accent (in progress), not warning.** Under self_hosted, requires_action means
  // "the tool is running in our container and Ark is waiting for the result", the same kind as running; warning would make the whole column
  // look like a pile of pending alerts, while none of them needs a human. See the note in types/domain.ts.
  requires_action: "accent",
  rescheduling: "warning",
  terminated: "default",
};

/**
 * States that are still moving: these deserve a breathing dot; a static row cannot show "it is running right now".
 *
 * `rescheduling` counts too: it is waiting for the next scheduling and will also change on its own.
 */
const LIVE: ReadonlySet<RunStatus> = new Set<RunStatus>([
  "running",
  "requires_action",
  "rescheduling",
]);

export function RunStatusChip({
  status,
  hasError,
  awaitsUser,
}: {
  status: RunStatus;
  /** `Boolean(run.errorSummary)`. See the file header: failure does not look at status. */
  hasError?: boolean;
  /** Computed by `runAwaitsUser()`. Only the detail page passes it (the list has no events). */
  awaitsUser?: boolean;
}) {
  const { t } = usePrefs();
  // terminated + error summary → mark as danger, otherwise "finished" and "failed" look the same
  const failed = hasError && status === "terminated";
  // When really waiting for the user, override "tool running": the two mean opposite things, and this is the one the user must act on.
  const blocked = awaitsUser && status === "requires_action";
  const color = failed ? "danger" : blocked ? "warning" : STATUS_COLOR[status];
  // Failure is final and should not breathe; waiting for the user has **stopped** to wait for a person, so it is not "moving" either.
  const live = LIVE.has(status) && !failed && !blocked;

  return (
    <Chip
      // shrink-0: the chip is a "compact marker sized by its content"; if flex squeezes it, "tool running"
      // wraps or gets clipped. It is the only thing on the right of a session list row, so without pinning, the title next to it will squeeze it.
      className="shrink-0"
      size="sm"
      color={color}
      variant={blocked ? "primary" : "soft"}
      data-testid="run-status"
      data-run-status={failed ? "failed" : blocked ? "awaits_user" : status}
    >
      {live ? (
        /*
          Breathing dot. `animate-pulse` is a slow fade over 2 seconds, not a high-frequency animation;
          what taste forbids is **hover transitions on list rows** (moving every time you pass over, which feels laggy);
          this only appears when something is really running, and is not triggered by interaction.

          `motion-reduce:animate-none`: no blinking when the system has "reduce motion" on.
          aria-hidden: there is a text label right next to it; reading the dot again is noise for screen readers.
        */
        <span
          aria-hidden="true"
          className="size-1.5 shrink-0 rounded-full bg-current animate-pulse motion-reduce:animate-none"
        />
      ) : null}
      <Chip.Label>
        {failed
          ? t("run.statusFailed")
          : blocked
            ? t("timeline.needsConfirm")
            : t(`enum.runStatus.${status}`)}
      </Chip.Label>
    </Chip>
  );
}
