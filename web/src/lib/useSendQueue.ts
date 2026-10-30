/**
 * Send queue: two different things, now handled by two separate layers.
 *
 * # ⚠️ This file handles two different things; tell them apart before changing it
 *
 * Their symptoms look alike and the entry is the same input box, but they need different things:
 *
 *  (A) Hitting the tool-execution window  A **race** of tens of milliseconds, unrelated to user
 *                        intent (the user was just quick).
 *                        Now **mainly retried by the server** (sendEventsThroughWindow in
 *                        core/chat.go, with a 1-second budget): not only the browser hits the
 *                        window;
 *                        Feishu inbound and schedules use the same endpoint too. The copy in this
 *                        file is the **fallback**:
 *                        when the server has used up its budget and still returns 409,
 *                        `sendThroughWindow` takes over
 *                        and resends locally with backoff. **It does not enter the queue UI; the
 *                        user never notices.**
 *  (B) The user queues on purpose  The user says a second thing while the agent is still running,
 *                        and has to wait for **a whole turn**
 *                        (tens of seconds to minutes). This **cannot** be solved by retrying; they
 *                        must see
 *                        their position, and be able to cancel and edit. **It now lives entirely
 *                        on the server**
 *                        (the pending_messages table + three routes); this file is only its view.
 *
 * To tell which one you are changing: anything about `enqueue` / `cancel` / `take` / `refresh` is
 * (B);
 * anything about `sendThroughWindow` / backoff / `isSendConflict` is (A).
 *
 * There is a third thing **not in this file**: redirect ("send now", interrupt the current turn and
 * say this instead),
 * which goes through `api.redirectRun`; see `sendNow` in ChatPage.
 *
 * # Why (B) must be on the server
 *
 * The queue used to live only in the browser's React state. So four things were wrong:
 *
 *   1. **Close the page / refresh, and queued messages are gone**, while the user thought they
 *      would be sent;
 *   2. Only the web page could queue; on the Feishu path, "the previous message is still running"
 *      meant the user had to remember to resend;
 *   3. Opening another tab meant two queues that did not know about each other, ending in a mixed
 *      order;
 *   4. Who sends? Once the browser closed, nobody did.
 *
 * Storing it in the database fixes all four: everyone's queued messages are in one table, sent by
 * the server when **the turn ends**
 * (DrainPendingMessage in server/core/pendingmessage.go, hooked at the moment the long-lived
 * subscription recognizes turnEnded).
 * So this file went from "a queue that retries itself" down to "a view of the server queue":
 * it no longer decides when to send; it only sends the user's intent up and reads the server state
 * back.
 *
 * # What this 409 is ((A))
 *
 * Under self_hosted, tools run in our own container; between Ark sending `agent.tool_use` and the
 * worker returning
 * `user.tool_result` there is a window, and inside it Ark **rejects** `user.message`
 * (HTTP 400 + code=RequiresActionRejected, which the backend turns into 409 + code=conflict;
 * see arkFailure in server/core/chat.go).
 *
 * **It is normal, not a fault.** Why users hit it often: under self_hosted every tool call pushes
 * a
 * `session.status_idle` (see the sequence in docs/DESIGN.md 3.6.3), and the frontend uses it to
 * drop `streaming`,
 * turning the composer from "interrupt" back to "send"; so during the whole tool execution the UI
 * looks idle,
 * and as soon as the user sends, they hit the window. This happens again and again when the agent
 * calls tools in a row.
 *
 * So the right product behavior is to resend for them, not show them the error (one resend is
 * enough;
 * in production the window measured only 48 milliseconds). **This must not become "put into the
 * server queue"**:
 * the queue waits for a whole turn to end, while the user expects "right now"; the two differ by
 * three orders of magnitude.
 *
 * # Three things you must not change on a hunch
 *
 * **1. The check is `code === "conflict"`, not HTTP 409, and certainly not matching Chinese text.**
 * Text changes (it is a sentence for people), status codes get reused for other meanings; only
 * code is the contract
 * (the ErrConflict → 409 + "conflict" line in the backend's handler.fail). This is the same rule as
 * the http.ts file header's
 * "nullable getters check code, not status".
 *
 * **2. Each queued message remembers its own runId and is sent back to that conversation.** The
 * server guarantees this now
 * (pending_messages.run_id), but after reading it back the frontend must still filter by it; do not
 * substitute "the runId in the current
 * URL": if the user switches to another conversation while queued, this message would show under
 * **someone else's conversation**.
 *
 * **3. When the queue is non-empty, new messages always go to the tail; no jumping ahead with a
 * direct send.** A direct send would race
 * the one the server is draining, and the user would see their two messages swapped. `pendingFor()`
 * guards this on the page side.
 *
 * # Backoff has a cap, and gives up when time is up (only for (A))
 *
 * Retry intervals back off by 1.8x, capped at 5 seconds; **total limit 3 minutes** (tool execution
 * itself has a 120-second
 * ceiling, see the timeout layers in shared/infra/shelltools/timing.go, so 3 minutes covers "tools running all
 * along").
 * If it still cannot send by then, give up and hand the text back to the caller (the page puts it
 * back in the input + gives a note);
 * we neither silently drop the user's words nor hit the backend forever.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "@/data/api";
import { ApiError } from "@/data/http";
import type { PendingMessage } from "@/types/domain";

/**
 * Queue limit. Once full, further sends are not silently dropped; the caller gives a clear hint and
 * puts the text back in the input.
 *
 * **Same value as the server's pendingQueueLimit** (server/core/pendingmessage.go). If the two
 * differ
 * it shows as "the frontend says you can still queue, the backend refuses", and that refusal makes
 * no sense to the user.
 */
export const SEND_QUEUE_LIMIT = 20;

/** Backend error code for "the previous tool is still running". For the contract see point 1 in this file's header. */
export const CONFLICT_CODE = "conflict";

/** Error code the backend gives when the queue is full. On it, hand back the text + a hint; do not treat it as a general fault. */
export const QUEUE_FULL_CODE = "precondition_failed";

/** Wait before the first retry. The window measures only tens of milliseconds, so the first one can be fast. */
const RETRY_BASE_MS = 400;
/** Backoff multiplier. */
const RETRY_FACTOR = 1.8;
/** Backoff cap. When tools run long, watch at this rate without stretching further. */
const RETRY_MAX_MS = 5_000;
/**
 * The longest time to keep resending one message. Past it, give up and hand back the text.
 *
 * 3 minutes because a single tool execution has a 120-second ceiling; a bit wider than that avoids
 * giving up by mistake when "the tool is fine but slow".
 * With intervals capped at 5 seconds this is at most about 36 requests, gentle enough.
 */
const RETRY_DEADLINE_MS = 180_000;

/**
 * A message **being resent locally**, used only for the give-up callback of path (A).
 *
 * ⚠️ This is not a queue item; queue items are the server's `PendingMessage`. Do not mix them up:
 * this lives in memory,
 * is invisible to the user and disappears within a few hundred ms; that one is stored in PG,
 * visible to the user, and can be cancelled and edited.
 */
export interface QueuedSend {
  /** Which conversation it goes to. **Do not substitute the runId in the current URL**; see point 2 in the file header. */
  runId: string;
  text: string;
}

/** Why resending was given up. The page uses it to decide what to say and whether to report it as a fault. */
export type GiveUpReason =
  /** Kept resending until timeout without success. **Not a fault**; do not report it as an error. */
  | "timeout"
  /** Hit some other error (session frozen, 5xx…). This is a real fault; use the existing two-level feedback. */
  | "error";

export interface SendQueue {
  /**
   * Queued messages of the current conversation, first in first out. **Whatever the server
   * returns is it**; the frontend no longer sorts.
   *
   * Only contains the conversation from the last `refresh()`; switching conversations needs a new
   * `refresh`.
   */
  items: PendingMessage[];
  /**
   * Put a message at the tail of the server queue.
   *
   * Returning `false` means **the queue is full and it was not added**: the caller must give a
   * hint and hand the text back to the user;
   * silently dropping is the worst way this feature can fail (the user thinks it was sent).
   *
   * Other errors are **thrown as is**, for the caller to handle as faults.
   */
  enqueue: (runId: string, text: string) => Promise<boolean>;
  /** How many are still queued for this conversation. For the "no jumping ahead with a direct send" gate; see point 3 in the file header. */
  pendingFor: (runId: string) => number;
  /** Cancel a queued message, dropping it. The server is idempotent; clicking twice gives no error. */
  cancel: (id: string) => Promise<void>;
  /**
   * Take out a queued message (delete it from the server and return the text), for "edit".
   *
   * Returns `null` when not found: it may have just been drained and sent by the server, and then
   * the text **must not** be put back in
   * the input, otherwise the user thinks it was not sent and sends it again.
   */
  take: (id: string) => Promise<string | null>;
  /**
   * Fetch this conversation's queue from the server again.
   *
   * **The page must call it when `streaming` changes** (both directions). The server drains the
   * head itself when a turn ends,
   * and the frontend gets no notice; without refetching, the message already sent would keep hanging
   * on
   * the queue UI, while it is long since in the conversation.
   *
   * ⚠️ This is not the same as the old `resume()`. The old one was "nudge the local queue to send";
   * now **the frontend no longer decides when to send**; this method only reads the server state
   * back.
   */
  refresh: (runId: string) => void;
  /**
   * (A) Local resend after hitting the window. **Does not enter the queue UI**; the user does not
   * notice.
   *
   * The caller calls it to take over after `deliver` throws an `isSendConflict` error. It retries
   * with backoff inside,
   * and if still unsuccessful after 3 minutes calls `onGiveUp(…, "timeout")` to hand back the text.
   */
  sendThroughWindow: (runId: string, text: string) => void;
}

/**
 * Build a send queue (a view of the server queue + the local resend of (A)).
 *
 * `deliver` is the "really send it" step (on the page: subscribe + `api.sendMessage` + refresh the
 * left column).
 * It may be a new function on every render; this hook only takes the latest one from a ref, so
 * callers need not memo it.
 *
 * `onGiveUp` is called when **local resending** is given up, with the text and reason; the page
 * uses it to hand back the text + give a note.
 * Items in the server queue never go through it: when those fail to send the server marks them
 * failed, and the UI shows that after reading back.
 */
export function useSendQueue(
  deliver: (runId: string, text: string) => Promise<void>,
  onGiveUp: (item: QueuedSend, reason: GiveUpReason, error: unknown) => void,
): SendQueue {
  const [items, setItems] = useState<PendingMessage[]>([]);

  /*
   * These refs are not laziness: resending is driven by **async callbacks** (timer → deliver →
   * success/failure →
   * schedule again), and reading state inside an async callback gets the snapshot of the frame it
   * was created in.
   */
  const itemsRef = useRef<PendingMessage[]>([]);
  /** Component unmounted. Late callbacks use it to stay quiet: no more setState, no new timers. */
  const stopped = useRef(false);
  /** In-flight local resend timers; all must be cleared on unmount. */
  const timers = useRef<Set<ReturnType<typeof setTimeout>>>(new Set());
  /**
   * Sequence number of the last refresh, used to drop stale responses.
   *
   * When switching conversations two requests are in flight at once; if the earlier one arrives
   * later it would render **someone else's queue**;
   * that error gives zero errors, the messages just show under the wrong conversation.
   */
  const refreshSeq = useRef(0);
  /**
   * How many times the queue was changed locally (`enqueue` / `cancel` / `take` each count once).
   *
   * ⚠️ **`refreshSeq` cannot catch this kind.** It only knows "another refresh was started after
   * this one",
   * while the interleaving here is "a read **already in flight** while one item was removed
   * locally": that read's sequence number is still the latest,
   * so it lands with the old snapshot **from before the cancel**, writing the deleted item back
   * into state.
   *
   * The result is not harmless lag like "looks one short", but a **phantom item**: the server no
   * longer has it
   * (the `cancelPendingMessage` inside `take` really deletes), yet the DOM still has a queued box
   * with
   * "send now / cancel / edit". Clicking it takes the path where the `id` does not exist: `take`
   * returns
   * null and returns silently, so that box does nothing however you click it, with zero errors.
   *
   * The check is "was there a local change after the read started", not a content comparison: two
   * queues with the same content
   * may also come from one delete plus one add, which a comparison cannot tell.
   */
  const mutationSeq = useRef(0);

  const deliverRef = useRef(deliver);
  deliverRef.current = deliver;
  const onGiveUpRef = useRef(onGiveUp);
  onGiveUpRef.current = onGiveUp;

  /**
   * The only write entry for the queue: updates ref and state together; the two must never diverge.
   *
   * `local` true means this is a **local** change (enqueue / cancel / taken by redirect), which
   * also bumps
   * `mutationSeq` by one, so a refresh in flight right now can recognize it is stale when it lands.
   * The commit read back from the server is not a local change, otherwise it would judge itself
   * stale.
   */
  const commit = useCallback((next: PendingMessage[], local = false) => {
    if (local) mutationSeq.current += 1;
    itemsRef.current = next;
    if (!stopped.current) setItems(next);
  }, []);

  const refresh = useCallback(
    (runId: string) => {
      if (stopped.current || !runId) return;
      refreshSeq.current += 1;
      const seq = refreshSeq.current;
      const mutationAtStart = mutationSeq.current;
      void api.listPendingMessages(runId).then(
        (list) => {
          // Stale response: another refresh was started after this one; that one decides.
          if (stopped.current || seq !== refreshSeq.current) return;
          // Also a stale response: the queue was changed locally while the read was in flight (see
          // mutationSeq).
          // This snapshot was taken before that change; writing it back would revive deleted items.
          if (mutationSeq.current !== mutationAtStart) return;
          commit(list);
        },
        () => {
          /*
           * Read failures **do not clear and do not report an error**.
           *
           * Clearing would make users think their queued messages are gone (they are still on the
           * server and will still be sent),
           * which is worse than temporarily not seeing them. An error is pointless too: this is a
           * background refresh the user did not ask for.
           */
        },
      );
    },
    [commit],
  );

  const enqueue = useCallback(
    async (runId: string, text: string): Promise<boolean> => {
      try {
        const list = await api.enqueuePendingMessage(runId, text);
        if (!stopped.current) commit(list, true);
        return true;
      } catch (e) {
        // A full queue is an **expected refusal**, not a fault: hand back the text, and the caller
        // gives a hint.
        if (e instanceof ApiError && e.code === QUEUE_FULL_CODE) return false;
        throw e;
      }
    },
    [commit],
  );

  const pendingFor = useCallback(
    (runId: string) => itemsRef.current.filter((it) => it.runId === runId).length,
    [],
  );

  const cancel = useCallback(
    async (id: string) => {
      const found = itemsRef.current.find((it) => it.id === id);
      if (!found) return;
      // Remove it locally first, then call the backend: a cancel must be visible at once. The
      // server side is idempotent;
      // if it fails it is just still in the database, and the next refresh reads it back (no
      // message is silently lost).
      commit(
        itemsRef.current.filter((it) => it.id !== id),
        true,
      );
      await api.cancelPendingMessage(found.runId, id);
    },
    [commit],
  );

  const take = useCallback(
    async (id: string): Promise<string | null> => {
      const found = itemsRef.current.find((it) => it.id === id);
      if (!found) return null;
      commit(
        itemsRef.current.filter((it) => it.id !== id),
        true,
      );
      await api.cancelPendingMessage(found.runId, id);
      return found.text;
    },
    [commit],
  );

  const sendThroughWindow = useCallback((runId: string, text: string) => {
    const startedAt = Date.now();
    let attempts = 0;

    const attempt = (): void => {
      if (stopped.current) return;
      void deliverRef.current(runId, text).then(
        () => {
          /* Sent. This path never enters the UI, so there is nothing to clean up on success. */
        },
        (e: unknown) => {
          if (stopped.current) return;

          // Not that window conflict: a real fault; hand back the text and let the page treat it as
          // a fault.
          if (!isSendConflict(e)) {
            onGiveUpRef.current({ runId, text }, "error", e);
            return;
          }
          // Resent for too long: give up and hand back the text. **No infinite retries**; see the
          // file header.
          if (Date.now() - startedAt >= RETRY_DEADLINE_MS) {
            onGiveUpRef.current({ runId, text }, "timeout", e);
            return;
          }

          const timer = setTimeout(() => {
            timers.current.delete(timer);
            attempt();
          }, backoffMs(attempts));
          attempts += 1;
          timers.current.add(timer);
        },
      );
    };

    attempt();
  }, []);

  // Stop on unmount: switching conversation, switching workspace and jumping to run detail all
  // get here.
  // Without clearing timers they would keep sending messages after the component is gone (and
  // setState would warn).
  useEffect(() => {
    stopped.current = false;
    return () => {
      stopped.current = true;
      for (const timer of timers.current) clearTimeout(timer);
      timers.current.clear();
    };
  }, []);

  return { items, enqueue, pendingFor, cancel, take, refresh, sendThroughWindow };
}

/**
 * Whether this error is "the previous tool is still running".
 *
 * ⚠️ **Only check code.** Checking `status === 409` would drag other future 409 meanings (like
 * "skill name already exists")
 * into auto-resend too; such errors never get better after ten thousand retries, so the user would
 * wait three minutes on a message that can never be sent.
 * Matching Chinese text is worse: that sentence is for people, and changing the text would
 * silently break it.
 */
export function isSendConflict(e: unknown): boolean {
  return e instanceof ApiError && e.code === CONFLICT_CODE;
}

/** How long to wait for the n-th retry (n from 0). 1.8x backoff, capped at RETRY_MAX_MS. */
function backoffMs(attempts: number): number {
  return Math.min(RETRY_MAX_MS, Math.round(RETRY_BASE_MS * RETRY_FACTOR ** attempts));
}
