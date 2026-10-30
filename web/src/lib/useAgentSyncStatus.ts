/**
 * Polling for "after saving an Agent, has Ark caught up yet".
 *
 * # Why it is needed
 *
 * Saving only writes the database; catching Ark up is **async** (see server/core/agentsync.go).
 * Success is silent,
 * but failure must be told to the user; otherwise "I changed the prompt and the agent still talks
 * the old way" has zero errors
 * and lasts forever. This hook is where that "only tell on failure" comes from.
 *
 * # Four rules copied from useRegistrationPoll, plus one it does not have
 *
 * 1. **Recursive setTimeout, not setInterval**: request time is unpredictable, and setInterval
 *    would stack requests when responses are
 *    slow. Scheduling the next one only after a result arrives keeps it serial.
 * 2. **Stop at a terminal state**: after succeeded / failed / none nothing more is scheduled.
 * 3. **Unmount means cancel**: cleanup clears the timer and sets cancelled, so we never setState
 *    after unmount.
 * 4. **Backoff**: the interval grows each time (1s → 1.5s → …, capped at 5s). Sync usually
 *    finishes within a few seconds,
 *    so the first few ticks must be close together; but if the backend gets stuck, dense polling
 *    turns into a request storm.
 * 5. ⚠️ **Hard deadline** (useRegistrationPoll does not have this; useIntegrationAuthorize does):
 *    stop when time is up, **without an error**. This is the last gate against storms: without
 *    it, a task stuck in running forever
 *    would make this page keep sending requests until the user closes the tab. This repo has been
 *    bitten by
 *    "infinite SSE reconnects are always the app layer's fault".
 *
 * # Why a timeout is not an error
 *
 * A timeout only means "we did not get an answer this time", not that the sync failed; it may
 * succeed the very next second.
 * Reporting it as failed would send the user to fix a problem that does not exist. When it really
 * fails the backend clearly returns failed,
 * and that is what should be shown. Same as the line in useIntegrationAuthorize: "the user most
 * likely just has not clicked yet".
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "@/data/api";
import { isAgentSyncTerminal, type AgentSyncTask } from "@/types/domain";

/** Interval of the first tick. Sync usually finishes within a few seconds, so start dense. */
const firstIntervalMs = 1000;
/** Multiplier per tick. */
const backoffFactor = 1.5;
/** Interval cap, so backoff does not stretch the wait too long. */
const maxIntervalMs = 5000;
/**
 * Hard deadline.
 *
 * 90 seconds: one backend sync round is capped at 2 minutes (syncRunTimeout), with up to 3 retries,
 * so the worst case is far longer than this. **We deliberately do not wait it out**: waiting the
 * full time is worth little (the user has long left),
 * and the cost is a page sending requests the whole time. If no answer comes, stop quietly; the
 * next visit to this page checks again.
 */
const deadlineMs = 90_000;

export interface AgentSyncState {
  /** Current state. `none` means there is no sync record (an old Agent that was never saved). */
  status: AgentSyncTask["status"];
  /** Failure reason, a Chinese sentence that can be shown directly. */
  failureReason: string;
  /** Which step it got stuck on; the frontend gives different guidance based on it. */
  failureStage: NonNullable<AgentSyncTask["failureStage"]>;
  /** Retrying (between clicking the retry button and getting the result). */
  isRetrying: boolean;
}

const idleState: AgentSyncState = {
  status: "none",
  failureReason: "",
  failureStage: "",
  isRetrying: false,
};

/**
 * Poll the sync status of one Agent.
 *
 * `token` is incremented after each save and restarts polling. **Do not use agentId as the only
 * dependency**:
 * then saving the same agent twice in a row would not restart polling (the dependency did not
 * change, the effect does not re-run),
 * and the result of the second save would never arrive.
 */
export function useAgentSyncStatus(agentId: string | null, token: number) {
  const [state, setState] = useState<AgentSyncState>(idleState);
  // A ref records "whether this round has been cancelled", so retry can interrupt in-flight polling.
  const cancelRef = useRef(false);
  /**
   * Retry starts polling again.
   *
   * ⚠️ **Only calling setState in retry is not enough**: that only draws "retrying", and nobody
   * ever asks again whether
   * this retry **finished or succeeded**. If it fails again, the UI stays stuck in
   * that optimistic in-between state. So retry increments this counter, making the effect below
   * re-run.
   */
  const [retryRound, setRetryRound] = useState(0);

  useEffect(() => {
    // token 0 means "not saved yet during this page visit"; no polling then:
    // opening the detail page of an existing agent should not send a query right away.
    if (!agentId || token === 0) {
      setState(idleState);
      return;
    }

    cancelRef.current = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let intervalMs = firstIntervalMs;
    const startedAt = Date.now();

    async function tick() {
      if (cancelRef.current) return;
      if (Date.now() - startedAt > deadlineMs) {
        // Timeout. **Do not change state, do not report an error** (see the file header): it only
        // means no answer arrived.
        return;
      }
      try {
        const task = await api.getAgentSyncTask(agentId!);
        if (cancelRef.current) return;
        setState({
          status: task.status,
          failureReason: task.failureReason ?? "",
          failureStage: task.failureStage ?? "",
          isRetrying: false,
        });
        if (isAgentSyncTerminal(task.status)) return;
      } catch {
        // The query itself failing (network hiccup) **is not a sync failure**: that would show a
        // read failure
        // as "your config did not sync", and the two need completely different handling. Just keep
        // polling.
        if (cancelRef.current) return;
      }
      intervalMs = Math.min(intervalMs * backoffFactor, maxIntervalMs);
      timer = setTimeout(() => void tick(), intervalMs);
    }

    // Wait one tick before asking: the save just returned, the background task most likely has
    // not finished, so asking at once would surely be pending.
    timer = setTimeout(() => void tick(), firstIntervalMs);

    return () => {
      cancelRef.current = true;
      if (timer) clearTimeout(timer);
    };
  }, [agentId, token, retryRound]);

  /**
   * Schedule another sync.
   *
   * Optimistically set the state to running and mark isRetrying, **but reset on failure**,
   * otherwise the user sees a retry that spins forever (this repo has noted "an optimistic flag
   * must be reset by hand on failure").
   */
  const retry = useCallback(async () => {
    if (!agentId) return;
    setState((s) => ({ ...s, isRetrying: true }));
    try {
      const task = await api.retryAgentSync(agentId);
      setState({
        status: task.status,
        failureReason: task.failureReason ?? "",
        failureStage: task.failureStage ?? "",
        isRetrying: false,
      });
      // Start a new polling round to follow the result of this retry. **This step cannot be
      // skipped**: if we only draw "retrying"
      // without asking for the result, and it fails again, the UI stays in the optimistic
      // in-between state forever.
      if (!isAgentSyncTerminal(task.status)) setRetryRound((n) => n + 1);
    } catch (err) {
      // Could not schedule (e.g. the narrow window where the previous one just finished). Replace
      // the reason with this one,
      // and **reset isRetrying**; otherwise the button stays loading forever.
      setState((s) => ({
        ...s,
        status: "failed",
        failureReason: err instanceof Error ? err.message : String(err),
        isRetrying: false,
      }));
    }
  }, [agentId]);

  return { ...state, retry };
}
