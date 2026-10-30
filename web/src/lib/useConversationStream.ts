/**
 * Live state of a conversation.
 *
 * This is the second "live" data source in this frontend (the first is useRegistrationPoll), and
 * like that one it is wrapped in a single
 * hook: pages only read state and never touch subscriptions or timers.
 *
 * Things you must not change on a hunch:
 *
 * 1. **Backfill history first, then subscribe; the order must not be reversed.** Ark states that
 *    "once a subscription is set up you only receive events produced after that point";
 *    there is no history replay (see the Ark docs on streaming session events).
 *    Subscribing before listRunEvents looks faster, but events produced between the two steps
 *    would be overwritten by the history.
 *
 * 2. **The draft is only display scaffolding; the final event is the source of truth.** Text
 *    accumulated from event_delta does not go into
 *    events; when the kind:"event" arrives it replaces the draft by id as a whole. Ark says delta
 *    frames are best-effort and
 *    may be dropped, so this is the only way to make sure the final result is still right after a
 *    dropped frame.
 *
 * 3. **Unmount means cancel.** The effect's cleanup sets a cancelled flag and calls unsubscribe,
 *    so we never setState after the component has
 *    unmounted. Switching workspace, switching conversation, and jumping to run details all hit this.
 *
 * 4. **Do not keep re-attaching when the run does not exist.** This is a permanent error; retries
 *    will never succeed.
 *    Note that the history step (listRunEvents) **will not** detect it for you: the list endpoint
 *    returns
 *    200 + [] for a run that does not exist (on purpose; see "list endpoints never 404" at the top
 *    of server/core/handler.go),
 *    so we can only rely on the subscribe step's onPermanentError callback. When it fires, stop on
 *    error
 *    and do not create another EventSource. In testing, re-attaching made the same dead runId get
 *    subscribed 3 times in 6 seconds
 *    (see the write-up at streamRunEvents in server/core/handler.go).
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "@/data/api";
import type { RunEvent, StreamFrame } from "@/types/events";

/**
 * Error marker for "this conversation no longer exists".
 *
 * We use a fixed marker rather than backend text: the EventSource error event **cannot read the
 * response body**
 * (the browser only gives a detail-less Event; the spec says so), so the frontend cannot read the
 * backend's sentence
 * "this run does not exist". The page uses this marker to look up i18n; the frontend owns the text.
 */
export const STREAM_GONE = "run_gone";

/** The message that is still streaming and not yet final. */
export interface MessageDraft {
  id: string;
  text: string;
}

export interface ConversationState {
  /** Final events, in ascending sequence order. */
  events: RunEvent[];
  /** The reply being streamed; null means there is none right now. */
  draft: MessageDraft | null;
  /**
   * The **thinking process** being streamed; null means there is none right now.
   *
   * **Must be stored separately from `draft`.** Ark's thinking and message share the event_delta
   * frame shape
   * (after server-side translation only `{kind,id,text}` is left; see clientFrames in
   * server/core/handler.go);
   * the only way to tell them apart is the `event_start.type` that arrives first. This used to
   * have only one draft, so thinking
   * deltas were added to the same draft, and **the thinking process was drawn into the assistant
   * bubble as the real reply**,
   * then vanished entirely when the final agent.thinking event arrived. It looked like "the reply
   * flashed and disappeared".
   */
  thinkingDraft: MessageDraft | null;
  /** Whether the agent is thinking. Used to show "thinking" before thinkingDraft has any text. */
  thinking: boolean;
  /** Initial history load in progress. */
  loading: boolean;
  /** Whether this turn is still running (false after done or status_idle). */
  streaming: boolean;
  error: string | null;
}

const INITIAL: ConversationState = {
  events: [],
  draft: null,
  thinkingDraft: null,
  thinking: false,
  loading: false,
  streaming: false,
  error: null,
};

export function useConversationStream(runId: string | null): ConversationState & {
  /** Append an event locally, used to draw the user message just sent right away without waiting for the stream. */
  appendLocal: (event: RunEvent) => void;
  /**
   * Set streaming before sending a message, to avoid a gap where "it was sent but looks like
   * nothing happened".
   *
   * Pass the runId of the run that is **about to** receive the message: for a new conversation it
   * is not in the URL yet, so the hook cannot read it itself.
   */
  markStreaming: (targetRunId: string) => void;
  /**
   * Undo the optimistic flag set by `markStreaming()`.
   *
   * **You must call this when sending fails.** `markStreaming` is optimistic: it assumes this
   * turn is about to start.
   * If the send itself did not succeed (most commonly the 409 from the tool-execution window),
   * the turn never started,
   * and `streaming` would **stay true forever**: no event will ever come to clear it, because
   * there are no events.
   *
   * The effect is worse than "one extra spinner": the composer uses it to swap the send button
   * for "interrupt"
   * (see canSend in ChatComposer), so while queued the user **cannot send a single message**,
   * and the UI shows an interrupt button that has nothing to interrupt. Zero errors.
   *
   * Only touches the `streaming` field: `error` and both drafts are left alone (each has its own
   * source of truth).
   */
  clearStreaming: () => void;
  /**
   * Resolves once the subscription for `targetRunId` is really set up.
   *
   * **You must await this before sending a message.** Ark "only pushes events produced after the
   * subscription is set up" and has no history replay,
   * so if you send first and subscribe later, every event the agent produces in between is lost
   * for good. It shows up as "the message went out,
   * but not a single word comes back" until a refresh fills it in from history. The new
   * conversation path shows this most:
   * between creating the run and subscribing there is a navigate plus a history fetch, measured
   * as a ~80ms window.
   *
   * Has a timeout fallback: better to miss a few frames than hold the message forever because the
   * subscription is slow to arrive.
   */
  whenSubscribed: (targetRunId: string, timeoutMs?: number) => Promise<void>;
} {
  const [state, setState] = useState<ConversationState>(INITIAL);
  // Keep the current runId in a ref so async callbacks can tell whether they are stale.
  const activeRun = useRef<string | null>(null);
  // runId whose subscription is already set up; cleared when switching conversations.
  const subscribedRun = useRef<string | null>(null);
  // Waiters for a subscription. Woken by runId when the subscription is set up.
  const waiters = useRef<{ runId: string; resolve: () => void }[]>([]);
  /**
   * "This turn has already been optimistically marked as running" and which run it belongs to.
   *
   * Why a ref and not just state: `markStreaming()` and the `navigate` right after it make
   * this hook's effect re-run, and `setState` inside the effect cannot see "it was just marked"
   * (state gets reset). A ref lives across renders; it is the only carrier of this fact.
   *
   * Tracking which run it belongs to is required: without it, switching to another conversation
   * that ended long ago would inherit the previous true,
   * and the composer would forever show a useless "interrupt".
   */
  const markedRef = useRef(false);
  const markedRun = useRef<string | null>(null);

  const markSubscribed = useCallback((id: string) => {
    subscribedRun.current = id;
    waiters.current = waiters.current.filter((w) => {
      if (w.runId !== id) return true;
      w.resolve();
      return false;
    });
  }, []);

  const whenSubscribed = useCallback((targetRunId: string, timeoutMs = 3000) => {
    if (subscribedRun.current === targetRunId) return Promise.resolve();
    return new Promise<void>((resolve) => {
      const entry = { runId: targetRunId, resolve };
      waiters.current.push(entry);
      setTimeout(() => {
        // If already woken, nothing to do; otherwise remove from the queue and let it through.
        if (!waiters.current.includes(entry)) return;
        waiters.current = waiters.current.filter((w) => w !== entry);
        resolve();
      }, timeoutMs);
    });
  }, []);

  useEffect(() => {
    activeRun.current = runId;
    subscribedRun.current = null;
    if (!runId) {
      setState(INITIAL);
      return;
    }

    let cancelled = false;
    let unsubscribe: (() => void) | undefined;

    /*
     * Switching conversations must clear everything (events, drafts, errors all belong to the
     * previous conversation), **but `streaming` needs separate handling**.
     *
     * The new conversation path is "create run → markStreaming() → navigate to new URL → send
     * message",
     * and the URL change makes this effect re-run with the new runId. An unconditional
     * `{...INITIAL}` would wipe the
     * `streaming` just set back to false. The effect is not just cosmetic:
     *
     *   - the composer uses it to decide whether to show "interrupt", so that button would flash
     *     away and come back;
     *   - more importantly, `ChatPage.send()` uses it to decide "this turn is still running →
     *     queue it",
     *     and on reading false it **sends directly**. The user's second quick message then skips
     *     the queue and goes to Ark,
     *     and Ark cannot accept it while the agent holds the session. That message is lost, with
     *     zero errors.
     *
     * So we reuse `markedRef` here (it is also re-checked after the history fetch below). It is
     * tied to a runId:
     * `markStreaming()` only sets it when "about to send a message to this run", and switching to
     * another conversation
     * clears it here, so the in-progress state of the previous conversation does not leak over.
     */
    if (markedRun.current !== runId) markedRef.current = false;
    setState({ ...INITIAL, loading: true, streaming: markedRef.current });

    function onFrame(frame: StreamFrame) {
      if (cancelled) return;
      setState((prev) => {
        const next = reduce(prev, frame);
        /*
         * This turn really ended (terminal event or done) → **also undo the optimistic flag**.
         *
         * `reduce` is a pure function and cannot touch the ref, so we do it here. Without this
         * the ref would stay true,
         * and the next effect re-run (switching view, refresh, switching back to this
         * conversation) would read streaming
         * back as true from the ref: a long-finished conversation shows "interrupt" and it never
         * goes away.
         */
        if (prev.streaming && !next.streaming) {
          markedRef.current = false;
          markedRun.current = null;
        }
        return next;
      });
    }

    async function start() {
      try {
        // Fetch the full history first.
        const history = await api.listRunEvents(runId!);
        if (cancelled) return;
        setState((prev) => ({
          ...prev,
          // **Do not write events: history**. When history arrives there may already be local
          // events:
          // the new conversation path is "create run → change URL → send message → appendLocal
          // draws the user bubble",
          // while this effect is triggered by the runId change and fetches history async; the two
          // run concurrently. Assigning wholesale would wipe
          // the user message just drawn (in testing: 0 user bubbles after sending, until the next
          // refresh).
          // The backend most likely has not stored this user.message yet, so history does not
          // have it either.
          events: mergeEvents(prev.events, history),
          loading: false,
          /*
           * Whether this turn is really still running.
           *
           * Both sources count, and **neither can be used alone**:
           *
           *   - `isStillRunning(history)`: when opening a still-running conversation from run
           *     history,
           *     only the history knows this;
           *   - `markedRef.current`: the new conversation path is "create run → markStreaming() →
           *     change URL → send message"; the URL change re-runs this effect while the backend
           *     has not stored anything yet,
           *     so **history is sure to lack this turn**; only this flag knows.
           *
           * Use an explicit flag rather than "are there local events": when a new conversation
           * gets here, local is also
           * empty (the user bubble waits for sendMessage to return before appendLocal), so using
           * the event count
           * cannot tell the two cases apart.
           *
           * The flag is cleared by runId at the top of the effect, so "switching to another
           * conversation that ended long ago" does not
           * inherit the previous true. That leak would make the composer forever show a useless
           * "interrupt".
           */
          streaming: markedRef.current || isStillRunning(history),
        }));
        // Then subscribe. markSubscribed hangs on onOpen rather than right after this line
        // returns: constructing an EventSource
        // is synchronous; on return the request may not even be sent yet, let alone the server
        // being attached to the Ark stream.
        unsubscribe = api.subscribeRunEvents(
          runId!,
          onFrame,
          () => {
            if (!cancelled) markSubscribed(runId!);
          },
          () => {
            // Permanent failure (run does not exist). **Stop here, do not re-attach**; retries
            // will never succeed.
            // Also clear loading, otherwise the page stays on the skeleton and the user cannot
            // see what happened.
            if (cancelled) return;
            setState((prev) => ({
              ...prev,
              loading: false,
              streaming: false,
              error: STREAM_GONE,
            }));
          },
        );
      } catch (e) {
        if (cancelled) return;
        setState((prev) => ({
          ...prev,
          loading: false,
          error: e instanceof Error ? e.message : String(e),
        }));
      }
    }

    void start();

    return () => {
      cancelled = true;
      subscribedRun.current = null;
      unsubscribe?.();
    };
  }, [runId, markSubscribed]);

  const appendLocal = useCallback((event: RunEvent) => {
    setState((prev) =>
      prev.events.some((e) => e.id === event.id)
        ? prev
        : { ...prev, events: [...prev.events, event] },
    );
  }, []);

  /**
   * Optimistically mark this turn as "running".
   *
   * `targetRunId` is the run that is **about to** receive the message. For a new conversation it
   * is not in the URL yet, so it must be
   * passed by the caller and cannot be read from `runId` (which would be undefined then). The
   * flag is tied to a run,
   * so switching conversations does not leak the previous one's in-progress state.
   */
  const markStreaming = useCallback((targetRunId: string) => {
    markedRef.current = true;
    markedRun.current = targetRunId;
    setState((prev) => ({ ...prev, streaming: true, error: null }));
  }, []);

  // Undo markStreaming's optimistic flag. For the reason see the interface declaration above:
  // without undoing it, after a failed send
  // the composer stays in the "interrupt" state forever and the user cannot send any message.
  //
  // The ref and state must be undone together: undoing only state means the next effect re-run
  // (switching view or conversation)
  // reads true back from the ref, and the "stuck in interrupt" bug comes back.
  const clearStreaming = useCallback(() => {
    markedRef.current = false;
    markedRun.current = null;
    setState((prev) => ({ ...prev, streaming: false }));
  }, []);

  return { ...state, appendLocal, markStreaming, clearStreaming, whenSubscribed };
}

/** Whether the tail of the history shows this turn is still running. */
function isStillRunning(events: RunEvent[]): boolean {
  for (let i = events.length - 1; i >= 0; i--) {
    const t = events[i].type;
    if (t === "session.status_idle" || t === "session.status_terminated") return false;
    if (t === "session.status_running" || t === "session.status_rescheduled") return true;
  }
  return false;
}

/**
 * id prefix of local optimistic receipts.
 *
 * When sending a message the backend returns a temporary event it built itself (see
 * acceptedFrom in server/core/arkrun.go):
 * the Ark SDK drops the id from the real receipt, so the backend fills in
 * `sevt-local-<nanoseconds>-<counter>`,
 * so the frontend can draw the message on screen right away.
 *
 * **Ark later pushes this message back on the event stream unchanged, with its own real id**;
 * that one is authoritative.
 */
const LOCAL_ID_PREFIX = "sevt-local-";

/**
 * Merge two batches of events, dedupe, and sort by ascending sequence.
 *
 * History backfill and local append (appendLocal) run concurrently; either may arrive first. Using
 * "merge" rather than "overwrite"
 * is the only way to lose neither side; when ids match, **the server copy wins**. The local one is
 * an optimistic receipt;
 * the server may have filled in fields (sequence especially; the local one often has no real
 * sequence yet).
 *
 * **Deduping by id alone is not enough.** The local receipt and the same message pushed back by
 * Ark **have different ids**
 * (`sevt-local-...` vs `sevt-2026...`), so both stay and the same sentence shows twice on screen.
 * That is exactly what we saw in testing: after sending, the user bubble showed twice, and a
 * refresh fixed it (on refresh there is only history,
 * no local receipt). So when the real event arrives, **the matching local temporary event must be
 * dropped**.
 *
 * Matching is by "same type + same body", not id: the local receipt is just the outgoing event
 * text wrapped in an envelope,
 * and its body is byte-for-byte the same as the one Ark pushes back (the backend deliberately
 * builds it by byte concatenation instead of marshal to ensure this;
 * see the comment on withLocalEnvelope).
 *
 * Events with the same sequence keep their relative order, so same-sequence events do not jump
 * between renders.
 *
 * ⚠ **Even if one side is empty, do not short-circuit and return the other.** There used to be
 * two lines here
 * `if (!local.length) return server` / `if (!server.length) return local`,
 * that looked like they just saved a loop, but actually passed "the side that was never deduped"
 * straight to rendering:
 *
 *   - **A single input can itself contain duplicate ids.** The backend does not dedupe by event id
 *     when storing, and the same Ark
 *     session may have several subscribers at once (Feishu inbound, Feishu resume, browser SSE,
 *     scheduler); each subscriber writes its own row, so history stores the same event id N times,
 *     with different sequences (e.g. 7 and 8) and otherwise identical bytes.
 *   - **The first screen always takes that short-circuit.** When the chat page first opens nothing
 *     has been appendLocal'ed yet, local is empty,
 *     so the whole history passed through and the byId dedupe below never ran.
 *
 * The symptom is the same sentence drawn N times on screen, while the console fills with React
 * duplicate key warnings
 * (the key in `timeline.ts` comes from the event id: simpleItem uses `e.id`, toolCallItem uses
 * `use.id`,
 * so duplicate ids become sibling nodes with the same key).
 *
 * So **every input combination must go through the full byId dedupe**: whatever shape the backend
 * sends, the frontend must reduce it to
 * "each id appears once". Which duplicate row is kept does not matter (bytes are identical); only
 * the count matters.
 */
function mergeEvents(local: RunEvent[], server: RunEvent[]): RunEvent[] {
  const byId = new Map<string, RunEvent>();
  for (const e of local) byId.set(e.id, e);
  // Later writes win: the server version takes priority.
  for (const e of server) byId.set(e.id, e);

  // (type + body) signatures the server has already confirmed. Only collect those with non-local ids.
  const confirmed = new Set<string>();
  for (const e of byId.values()) {
    if (!e.id.startsWith(LOCAL_ID_PREFIX)) confirmed.add(eventSignature(e));
  }
  // For real events that have arrived, drop the matching local temporary one.
  const merged = [...byId.values()].filter(
    (e) => !(e.id.startsWith(LOCAL_ID_PREFIX) && confirmed.has(eventSignature(e))),
  );

  return merged.sort((a, b) => a.sequence - b.sequence);
}

/**
 * The "same message" signature of an event, used to pair a local receipt with the real event Ark
 * pushes back.
 *
 * Uses type + body rather than id: the two ids are always different, which is exactly the problem
 * being solved.
 * Only meaningful for events with content; other types return the id, i.e. they do not take part
 * in pairing.
 */
function eventSignature(e: RunEvent): string {
  const content = (e as { content?: { type?: string; text?: string }[] }).content;
  if (!Array.isArray(content)) return e.id;
  const text = content.map((c) => (typeof c?.text === "string" ? c.text : "")).join("");
  return `${e.type} ${text}`;
}

/**
 * When a real event arrives, remove the local temporary receipt for the same message.
 *
 * Only remove those with the `sevt-local-` prefix; real server events must not knock each other out.
 */
function dropLocalTwin(events: RunEvent[], incoming: RunEvent): RunEvent[] {
  if (incoming.id.startsWith(LOCAL_ID_PREFIX)) return events;
  const sig = eventSignature(incoming);
  return events.filter((e) => !(e.id.startsWith(LOCAL_ID_PREFIX) && eventSignature(e) === sig));
}

/** Push state frame by frame. A pure function, easy to reason about alone. */
function reduce(prev: ConversationState, frame: StreamFrame): ConversationState {
  switch (frame.kind) {
    case "event_start":
      // thinking and message each get their own draft: **their event_delta frames have exactly
      // the same shape**,
      // only this frame carries a type, so "is this id thinking or a reply" must be recorded here
      // (later deltas are routed by matching the id below). Sharing one draft would draw thinking
      // as the reply.
      return frame.type === "agent.thinking"
        ? { ...prev, thinkingDraft: { id: frame.id, text: "" }, thinking: true, streaming: true }
        : { ...prev, draft: { id: frame.id, text: "" }, streaming: true };

    case "event_delta": {
      // Route to the matching draft by id. Check thinking first: its id was registered at
      // event_start.
      if (prev.thinkingDraft?.id === frame.id) {
        return {
          ...prev,
          thinkingDraft: {
            id: frame.id,
            text: prev.thinkingDraft.text + frame.text,
          },
          streaming: true,
        };
      }
      // If a delta arrives with no event_start (dropped frame), open a draft for it.
      // **Falling back to the reply draft is on purpose**: without event_start the type cannot be
      // known, and showing thinking by mistake
      // as a reply (the final event will correct it) is better than not showing it at all.
      const draft = prev.draft?.id === frame.id ? prev.draft : { id: frame.id, text: "" };
      return { ...prev, draft: { id: draft.id, text: draft.text + frame.text }, streaming: true };
    }

    case "event": {
      const event = frame.event;
      // If one with the same id already exists, leave it; otherwise append, **and drop the matching
      // local temporary receipt**.
      //
      // Deduping only by id misses this case: for the same user message the local receipt's id is
      // `sevt-local-...` while the one Ark pushes back is `sevt-2026...`; the ids differ so both stay,
      // and the same sentence shows twice on screen (in testing, the user bubble showed twice after
      // sending).
      // For how pairing works see the comment on eventSignature.
      const events = prev.events.some((e) => e.id === event.id)
        ? prev.events
        : [...dropLocalTwin(prev.events, event), event];

      // Final event arrived → replace the draft with it (aligned by id). Each draft is checked
      // on its own.
      const draft = prev.draft?.id === event.id ? null : prev.draft;
      const thinkingDraft = prev.thinkingDraft?.id === event.id ? null : prev.thinkingDraft;
      // The final thinking event arrived, or the agent has started talking: turn off the thinking
      // signal.
      const thinking =
        event.type === "agent.thinking"
          ? false
          : event.type === "agent.message"
            ? false
            : prev.thinking;

      const ended =
        event.type === "session.status_idle" || event.type === "session.status_terminated";

      return {
        ...prev,
        events,
        draft,
        thinkingDraft,
        thinking,
        streaming: ended ? false : prev.streaming,
      };
    }

    case "done":
      return { ...prev, draft: null, thinkingDraft: null, thinking: false, streaming: false };
  }
}
