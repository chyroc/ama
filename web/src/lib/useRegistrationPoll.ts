/**
 * Polling state machine for device-code onboarding.
 *
 * This is the first polling logic in this frontend (before it there was zero setInterval on the
 * site), so it is kept in one hook;
 * pages only read state and never touch timers.
 *
 * Four deliberate choices:
 *
 * 1. **Recursive setTimeout, not setInterval**: request time is unpredictable, and setInterval
 *    would stack requests
 *    when responses are slow. Scheduling the next one only after a result arrives keeps it serial.
 * 2. **The backend sets the interval**: when Feishu thinks we poll too fast it returns slow_down,
 *    and the backend raises
 *    pollIntervalSeconds accordingly; the frontend just follows it, it does not decide on its own.
 * 3. **Stop at a terminal state**: after completed / expired / denied / invalid nothing more is
 *    scheduled.
 * 4. **Unmount means cancel**: the effect's cleanup clears the timer and sets a cancelled flag,
 *    so we never setState after the component has
 *    unmounted (switching workspace or going back to the list page both hit this).
 */

import { useEffect, useRef, useState } from "react";
import { api } from "@/data/api";
import type { FeishuApp, FeishuRegistrationStatus } from "@/types/domain";

export interface PollState {
  status: FeishuRegistrationStatus;
  /** The app obtained on completion. */
  app: FeishuApp | null;
  /** Failure reason, a Chinese sentence that can be shown directly. */
  reason: string;
}

/**
 * Poll an onboarding session until it reaches a terminal state.
 *
 * Does nothing when sessionId is empty; that is the page's state before it has started.
 */
export function useRegistrationPoll(
  workspaceId: string,
  sessionId: string | null,
  initialIntervalSeconds: number,
): PollState {
  const [state, setState] = useState<PollState>({ status: "pending", app: null, reason: "" });
  // Keep the interval in a ref: backend responses rewrite it, but a rewrite should not re-run the effect.
  const intervalRef = useRef(initialIntervalSeconds);

  useEffect(() => {
    if (!sessionId) return;

    intervalRef.current = initialIntervalSeconds;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;

    async function tick() {
      if (cancelled) return;
      try {
        const res = await api.pollFeishuRegistration(workspaceId, sessionId!);
        if (cancelled) return;
        if (res.pollIntervalSeconds > 0) intervalRef.current = res.pollIntervalSeconds;
        setState({
          status: res.status,
          app: res.app ?? null,
          reason: res.reason ?? "",
        });
        // Only schedule the next one while still waiting.
        if (res.status === "pending") schedule();
      } catch (e) {
        if (cancelled) return;
        // Errors like an expired session count as terminal: polling more will not make it better.
        setState({
          status: "invalid",
          app: null,
          reason: e instanceof Error ? e.message : String(e),
        });
      }
    }

    function schedule() {
      timer = setTimeout(() => void tick(), Math.max(1, intervalRef.current) * 1000);
    }

    // Wait one interval before asking: right after starting the user has not had time to
    // authorize, so asking at once would surely be pending.
    schedule();

    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
  }, [workspaceId, sessionId, initialIntervalSeconds]);

  return state;
}
