/**
 * The state machine "start authorization → open the authorization page → poll until authorized",
 * **one copy shared by two entry points**.
 *
 * # ⚠️⚠️ Why polling is required: device codes have no callback
 *
 * The two kinds of authorization sources "complete" in completely different ways (decided by
 * `pollsAuthorization` from the backend):
 *
 *  - Sources that use a browser redirect: after the user clicks on the platform's page, **the
 *    platform records it itself**. Next time we check status we
 *    see it, so not polling is also correct (one click on "check again" is enough).
 *  - Feishu user authorization: uses a **device code**, with **no callback**. Only when we
 *    actively poll do we take the
 *    device_code, exchange it for a token and store it. **Without polling, after the user clicks
 *    approve in Feishu, our side
 *    shows "not authorized" forever**, even if they click "check again" a hundred times.
 *
 * So "not polling" is not just a slightly worse experience; **the feature does not exist**, and
 * with zero errors: every API returns 200,
 * the console is clean, the user really clicked approve, only the state never changes.
 *
 * # Why it is extracted
 *
 * Two entry points now start authorization: the "Authorization management" card in global
 * settings, and the
 * in-place authorization row on the agent config page. With a copy on each side, the three rules
 * below would sooner or later degrade on one side, and the symptoms
 * (hundreds of extra API calls / setState after unmount / users waiting for nothing) do not point
 * at "someone copied this code".
 *
 * # Three rules that must not degrade
 *
 * 1. **Has a cap** (`authPollDeadlineMs`, 3 minutes). Users may give up halfway, and then we should
 *    not hit the
 *    API forever. Stopping after the timeout and saying "start again" is more honest than spinning.
 * 2. **The interval follows the backend** (`pollInterval` in the response; Feishu gives 5 seconds).
 *    Guessing our own is either too fast
 *    and rate-limited by the platform (slow_down), or too slow and keeps users waiting.
 * 3. **Stop on unmount** (a ref, not state). State captured by a closure is always the old value
 *    of that frame,
 *    so "already unmounted" would always read false, and polling would never stop.
 *
 * # Text is passed in by the caller
 *
 * Same as `fallbackMessage` in `useAsyncData`: this file does not touch i18n; each entry point uses
 * its own
 * wording (the authorization page says "card", the agent form says "this item").
 */

import { useEffect, useRef, useState } from "react";

import { api } from "@/data/api";
import type { AuthorizationStatus, IntegrationState } from "@/types/domain";

/**
 * Initial polling interval and total cap.
 *
 * The interval is only the initial value: the backend includes Feishu's suggested `pollInterval`
 * in the response (measured 5 seconds), and we switch
 * to it once we have it. Cap of 3 minutes: Feishu's device code is valid for 1 hour, but **the
 * user's patience is not**; past this
 * time they most likely gave up or walked away, and hitting the API more is pointless.
 */
const defaultAuthPollIntervalMs = 3000;
const authPollDeadlineMs = 3 * 60_000;

/** Wait a bit. Used in the polling loop. */
function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** Two fallback messages. The caller takes them from `t()`, so this file does not touch i18n. */
export interface IntegrationAuthorizeMessages {
  /** Starting authorization itself failed (could not get the authorization page URL). */
  authorizeFailed: string;
  /** Reached the cap without the user clicking approve. **Not an error**: the credentials are fine, they may just not have clicked yet. */
  timeout: string;
}

export interface IntegrationAuthorize {
  /** Start an authorization: get the URL → open in a new tab → (if it should poll) start polling. */
  start: () => Promise<void>;
  /** Fetching the authorization page URL (the network round trip of starting). */
  isBusy: boolean;
  /** Polling "has the user clicked approve yet". */
  isPolling: boolean;
  /** The authorization page is open, with no result yet. Used to show the "go approve in Feishu" sentence. */
  hasOpened: boolean;
  /** The failure or timeout sentence, shown as is. null if none. */
  error: string | null;
  /** Clear the error. Callers use it when starting another action (check again / revoke). */
  clearError: () => void;
}

/**
 * @param provider  Authorization source id. **The caller takes it from `IntegrationState.id` given
 *                  by the backend**;
 *                  never hard-code literals like `lark-user` anywhere.
 * @param pollsAuthorization  Whether the backend says this source needs polling. Missing is
 *                  treated as false (older backends do not return
 *                  this field, and their behavior is no polling).
 * @param onAuthorized  Called after credentials are obtained, with the freshly fetched state. The
 *                  caller **only replaces
 *                  this one provider** with it; do not refetch the whole list, which would mix up
 *                  other states and the form being
 *                  edited.
 * @param workspaceId  Optional. **Decides "which Feishu app is being authorized"**; must be passed
 *                  when used from the agent form
 *                  (the agent belongs to a workspace, and the token is bound to a Feishu app).
 *                  The authorization management page is account-level and does not pass it.
 *
 *                  ⚠️ It feeds **both starting and polling**; missing either is a dead end:
 *                  starting records the device_code
 *                  on the workspace's app while polling looks under the account default; finding
 *                  nothing, the backend returns
 *                  "this authorization flow has expired", whose pending is false,
 *                  so this hook treats it as terminal and stops on the first tick. It shows up as
 *                  **being told it expired right after clicking authorize, while starting
 *                  actually succeeded**.
 */
export function useIntegrationAuthorize(
  provider: string,
  pollsAuthorization: boolean | undefined,
  onAuthorized: (next: IntegrationState) => void,
  messages: IntegrationAuthorizeMessages,
  workspaceId?: string,
): IntegrationAuthorize {
  const [isBusy, setBusy] = useState(false);
  const [isPolling, setPolling] = useState(false);
  const [hasOpened, setOpened] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Polling must stop after unmount: use a ref, not state, because the polling loop reads the
  // **current** value,
  // while state captured by a closure is always the old value of that frame, which would make
  // "already unmounted" always read false.
  const pollAbort = useRef(false);
  useEffect(() => {
    // ⚠️ Reset to false on entry; setting true only in cleanup is not enough.
    // In StrictMode the effect runs twice, "mount → unmount → mount again"; with only the cleanup,
    // after the second mount
    // this flag stays true, so polling exits as soon as it starts. It looks like "clicked and
    // nothing happened",
    // and only in development.
    pollAbort.current = false;
    return () => {
      pollAbort.current = true;
    };
  }, []);

  /*
   * Once the polling loop runs it can last up to 3 minutes, during which the component re-renders
   * many times.
   * The `onAuthorized` / `messages` captured by the closure would stay at **the frame that
   * started it**, so they go into a ref refreshed on every
   * render; otherwise after the caller swaps the callback (e.g. the parent rebuilt that function),
   * the success callback would hit an old closure nobody listens to, looking like "authorization
   * succeeded but the UI did not change".
   */
  const onAuthorizedRef = useRef(onAuthorized);
  onAuthorizedRef.current = onAuthorized;
  const messagesRef = useRef(messages);
  messagesRef.current = messages;

  /**
   * Poll "has the user clicked approve yet" until success, expiry or timeout.
   *
   * For the three rules see the file header. One more word on catch here: when polling itself
   * fails (network hiccup, backend 5xx)
   * **do not give up at once**: the user may be clicking in Feishu right now, and stopping on one
   * failure would waste their click.
   * Keep polling; the deadline is the fallback.
   */
  async function pollUntilAuthorized() {
    const startedAt = Date.now();
    setPolling(true);
    try {
      let intervalMs = defaultAuthPollIntervalMs;
      while (!pollAbort.current && Date.now() - startedAt < authPollDeadlineMs) {
        await sleep(intervalMs);
        if (pollAbort.current) return;
        let status: AuthorizationStatus;
        try {
          status = await api.pollIntegrationAuthorization(provider, workspaceId);
        } catch (e) {
          void e;
          continue;
        }
        if (status.pollInterval && status.pollInterval > 0) {
          intervalMs = status.pollInterval * 1000;
        }
        if (status.done) {
          setOpened(false);
          // Fetch the latest state once: this is the evidence of "authorization succeeded" in the UI.
          //
          // ⚠️⚠️ **Must carry the same workspaceId**. Authorization is per workspace (the token is
          // recorded under
          // that workspace's app), so the re-check must ask by the same workspace; without it the
          // backend looks up
          // the **account-level default** app, and the authorization that just succeeded is shown
          // as "not authorized" on the spot.
          // And this is the only place the user can see the result; they will think authorization
          // failed and click again,
          // each time it really succeeds, and each time they are told it is not authorized.
          onAuthorizedRef.current(await api.getIntegration(provider, workspaceId));
          return;
        }
        if (!status.pending) {
          // Neither done nor waiting: this flow has expired (code expired, user refused).
          // Show the backend's sentence as is: it says "what to do".
          setError(status.message ?? messagesRef.current.authorizeFailed);
          setOpened(false);
          return;
        }
      }
      if (!pollAbort.current) {
        // Timeout. **Not reported as an error**: the user most likely just has not clicked yet,
        // and the credentials are fine.
        setError(messagesRef.current.timeout);
      }
    } finally {
      setPolling(false);
    }
  }

  async function start() {
    setBusy(true);
    setError(null);
    try {
      const url = await api.startIntegrationAuthorize(provider, workspaceId);
      // Open in a new tab, not location.href: authorization finishes on the platform side, and
      // after clicking the user must be able to come back to this page
      // to see the state. Replacing the current page would leave them to find the way back; on the
      // agent form it is worse,
      // since it would **lose the unsaved draft too**.
      window.open(url, "_blank", "noopener,noreferrer");
      setOpened(true);
      // ⚠️ Decide by pollsAuthorization from the backend; **do not hard-code by id in the frontend**.
      // Calling this endpoint for a source that should not poll returns 400 (poll_unsupported).
      if (pollsAuthorization) {
        void pollUntilAuthorized();
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : messagesRef.current.authorizeFailed);
    } finally {
      setBusy(false);
    }
  }

  return {
    start,
    isBusy,
    isPolling,
    hasOpened,
    error,
    clearError: () => setError(null),
  };
}
