/**
 * Terminal transport layer: **prefer WebSocket; if it cannot connect, fall back to SSE + POST.**
 *
 * It is a separate module rather than stuffed into SandboxTerminal.tsx because "which path to take,
 * how to reconnect, when to
 * give up" has its own state machine, unrelated to xterm rendering. Mixed together, that component
 * would manage
 * three things at once (canvas, connection, fallback), and it has already tripped once over "the
 * connection state machine"
 * (see the comment on phase in SandboxTerminal.tsx).
 *
 * # Why two paths, rather than keeping only WS
 *
 * WS is the main path; it fixed two real problems of the old path: **out-of-order echo** (the old
 * path sent one separate POST per keystroke,
 * concurrently in flight, with arrival order decided by the network) and **lag** (one full RTT per
 * character).
 *
 * But it brought two new risks, and if either goes wrong the terminal is entirely unusable:
 *
 *   - **It may connect to another domain** (like `ama-ws.example.com`). That domain's gateway
 *     config,
 *     cross-datacenter traffic switching, and `/websocket` prefix routing are not in our code; we
 *     once measured
 *     a state of "traffic switch did not take effect, every path under the prefix always 500".
 *   - **The gateway drops idle connections after ~30 seconds** (measured: alive at 28s, EOF read
 *     at 30s). The server has a 20-second heartbeat
 *     to hold it, but the heartbeat only works after connecting.
 *
 * So the SSE path is kept: **when WS is unavailable the user still has a working terminal**, just
 * slower, and fast typing may arrive out of order.
 * This is not purism: "the new path broke and the terminal will not open at all" is much worse than
 * "the terminal is a bit slow".
 *
 * ⚠️ **Fallback is one-time and does not go back to WS on its own.** The check is "WS failed once
 * during this mount",
 * not trying WS again on every reconnect: when WS is fully broken that would make every reconnect
 * wait out a handshake timeout for nothing,
 * and users would feel "the more I use it, the slower it gets". To go back to WS, refresh the page.
 */

import { api } from "@/data/api";
import type { MessageKey } from "@/prefs/messages";

/** Events the transport layer raises upward. The caller (the xterm layer) decides how to present them. */
export type TransportEvents = {
  /** Received one frame of container output (an already-decoded JSON frame). */
  onFrame: (frame: { type?: string; data?: unknown; dataB64?: string }) => void;
  /** Connection ready. `viaWebSocket` is used to say clearly in the UI which path is in use. */
  onOpen: (viaWebSocket: boolean) => void;
  /**
   * The connection is lost, and **will not reconnect automatically anymore** (backoff exhausted,
   * or this kind of failure should not reconnect at all).
   *
   * `errorCode` goes straight into reports, and **the two failures must use different codes**:
   *
   *   - `terminal_disconnected`: no container output is received (downstream broken)
   *   - `terminal_input_failed`: keystrokes cannot get into the container (upstream broken)
   *
   * With the same code, debugging could not tell whether the user "typed and nothing happened" or
   * "the screen did not refresh",
   * and the two have completely different causes (the POST landed on another instance vs the
   * container side broke).
   * This is an existing rule, and **must not be merged** during refactors.
   *
   * `reason` is a translation key, not text: this module has no `t`, the xterm layer translates it.
   */
  onClosed: (errorCode: string, reason: MessageKey) => void;
  /**
   * Automatic reconnect succeeded. **The UI must say something**; it cannot pretend nothing
   * happened.
   *
   * ⚠️ After a reconnect it is a **new PTY**: shell history, current directory and running
   * foreground processes are all gone.
   * A user who thinks they are still in the old shell is worse off: they may type a command that
   * depends on what came before.
   */
  onReconnected: () => void;
};

/** What the transport layer looks like from outside. Each path implements it. */
export type Transport = {
  /**
   * Send one upstream frame (keystroke / resize / pong). Synchronous, never throws.
   *
   * `silent` means "even if this frame fails to send, do not judge the connection as failed".
   *
   * ⚠️ **Control frames must use silent; only user keystrokes do not.** The reason is the check must
   * match what the user perceives:
   *
   *   - a keystroke that cannot be sent = the user typed and nothing happened, **a failure they can
   *     feel**; report it and show a hint.
   *   - resize / pong that cannot be sent = the user feels nothing (layout may be off, that is all).
   *
   * Without the split there is a very hidden consequence: the terminal sends a resize frame as soon
   * as it connects (telling the container the window size),
   * so when "upstream is entirely broken" it is judged disconnected **before the user types the
   * first character**; the UI goes straight to
   * "connection lost" while they have done nothing. In the old implementation the resize frame's
   * failure was voided
   * (only the keystroke path hung failConnection); this distinction must not be lost in refactors.
   */
  send: (frame: unknown, opts?: { silent?: boolean }) => void;
  /** Shut down: close the connection, stop reconnecting, clear timers. **Must be idempotent**. */
  close: () => void;
};

/**
 * The reconnect backoff ladder (milliseconds).
 *
 * ⚠️ **It must have a cap; no infinite reconnects**. This repo has been bitten: infinite SSE
 * reconnects are always the app layer reconnecting
 * (the browser gives up by itself on a 404), and that time it showed up as a reconnect storm.
 *
 * 5 steps, about 15 seconds total: enough to get through one rolling deploy (a pod restarts in a
 * few seconds) and short network hiccups,
 * without leaving users staring at a UI that is forever "reconnecting". When used up, report
 * disconnected and offer a manual reconnect button.
 */
const RECONNECT_BACKOFF_MS = [500, 1000, 2000, 4000, 8000];

/**
 * Where the WS address comes from.
 *
 * Built from an environment variable rather than having the backend return a full URL: **it is part
 * of the deployment shape, not a property of one request**.
 * The backend's ticket endpoint deliberately returns only the ticket value, for the same reason.
 *
 * ⚠️ **If not configured, WS is simply not used** (returns an empty string), rather than falling
 * back to the current domain: if the main domain's
 * gateway uses h2, the WS handshake is sure to fail. Then every time we would wait out a
 * handshake timeout before falling back, slower than using SSE from the start.
 *
 * # ⚠️ The special value `same-origin`: local development must use it
 *
 * Locally `dev.sh` serves frontend and backend from the same origin (the build output is go:embed'd
 * into the binary, one port only), so WS should connect to
 * **the current page's host**. But **`http://127.0.0.1:5273` must not be written into the build
 * output**:
 * that literal would be bundled into the js, and the browser may be running on **another machine**
 * (a colleague opens
 * `http://192.168.1.10:5273`), where `127.0.0.1` means their own computer, which has no amad.
 * The symptom is `WebSocket connection to 'ws://127.0.0.1:5273/...' failed`; we actually hit it.
 *
 * So dev.sh passes `same-origin`, and here it is computed **at runtime** from `location`:
 * ws if the page is http, wss if https, and the host simply follows the page.
 * This way, whether opened from 127.0.0.1, localhost or a LAN IP, it connects to that page's own
 * server.
 */
function wsBase(): string {
  const raw = (import.meta.env.VITE_TERMINAL_WS_BASE ?? "").trim();
  if (raw === "") return "";
  // Built from the current page at runtime. **Must be computed at runtime**; see above (hard-coding
  // would connect to the user's own machine).
  if (raw === "same-origin") {
    if (typeof location === "undefined") return "";
    return `${location.protocol === "https:" ? "wss:" : "ws:"}//${location.host}`;
  }
  return raw.replace(/\/+$/, "");
}

/** Whether this deployment has a WS channel configured. For the UI (decides whether to mention "fell back"). */
export function terminalWSConfigured(): boolean {
  return wsBase() !== "";
}

/**
 * Create a terminal connection. Prefer WS; on failure fall back to SSE.
 *
 * The returned Transport is usable right away: `send` calls before connecting are buffered (see
 * pending below).
 */
export function connectTerminal(runId: string, events: TransportEvents): Transport {
  let closed = false;
  let attempt = 0;
  /** Once the WS path has failed, it is not tried again; see "fallback is one-time" in the file header. */
  let wsGaveUp = false;
  /** The currently live underlying connection, swapped on reconnect. */
  let current: Transport | null = null;
  let retryTimer: ReturnType<typeof setTimeout> | null = null;
  /** Whether it has connected once: used to tell "first connect failed" from "reconnect after drop". */
  let everOpened = false;

  /**
   * Upstream frames saved up before connecting.
   *
   * ⚠️ They must not be dropped: xterm's `onResize` fires once right after mount, and that frame
   * decides the container's
   * layout width. If dropped, the container lays out at 80x24 and every line break is wrong; that
   * symptom gives no error,
   * it just "looks messy", and it is hard to connect it to one lost resize frame.
   */
  let pending: Array<{ frame: unknown; opts?: { silent?: boolean } }> = [];

  function flushPending(send: Transport["send"]) {
    const queued = pending;
    pending = [];
    for (const q of queued) send(q.frame, q.opts);
  }

  function scheduleRetry(errorCode: string, reason: MessageKey) {
    if (closed) return;
    if (attempt >= RECONNECT_BACKOFF_MS.length) {
      // Backoff exhausted. **This is the only place that should report "disconnected"**.
      events.onClosed(errorCode, reason);
      return;
    }
    const wait = RECONNECT_BACKOFF_MS[attempt];
    attempt++;
    retryTimer = setTimeout(() => {
      retryTimer = null;
      if (!closed) open();
    }, wait);
  }

  function open() {
    if (closed) return;
    const useWS = !wsGaveUp && terminalWSConfigured();
    const start = useWS ? openWS : openSSE;
    current = start(runId, {
      onFrame: events.onFrame,
      onOpen: (viaWS) => {
        // Connected: reset backoff, send the saved frames.
        attempt = 0;
        if (current) flushPending(current.send);
        if (everOpened) {
          // This is a successful **reconnect**. The UI must say "this is a new shell".
          events.onReconnected();
        } else {
          everOpened = true;
          events.onOpen(viaWS);
        }
      },
      onFailed: ({ errorCode, reason, duringHandshake, retryable }) => {
        // The WS handshake never succeeded → this path does not work; permanently fall back to
        // SSE and **retry at once** (no backoff):
        // users should not pay a backoff wait for one protocol probe.
        if (useWS && duringHandshake) {
          wsGaveUp = true;
          if (!closed) open();
          return;
        }
        // ⚠️ **Failures that cannot be reconnected report disconnected directly; no backoff.**
        //
        // The typical one is an upstream failure (keystrokes cannot get into the container): it
        // means the server's PTY is already gone,
        // reconnecting would only fail again, and the user would type blind through the ten-plus
        // seconds of backoff; the production record
        // with occurrences=25 piled up exactly this way. Telling them "it is broken, click here to
        // reconnect" right away is better.
        if (!retryable) {
          events.onClosed(errorCode, reason);
          return;
        }
        scheduleRetry(errorCode, reason);
      },
    });
  }

  open();

  return {
    send(frame: unknown, opts?: { silent?: boolean }) {
      if (closed) return;
      if (current) {
        current.send(frame, opts);
      } else {
        // Still reconnecting: save it. See the comment on pending (the resize frame must not be
        // lost).
        pending.push({ frame, opts });
      }
    },
    close() {
      closed = true;
      if (retryTimer) clearTimeout(retryTimer);
      retryTimer = null;
      current?.close();
      current = null;
      pending = [];
    },
  };
}

/** Callbacks shared by the underlying implementations. Two more dimensions than TransportEvents. */
type LowerEvents = {
  onFrame: (frame: { type?: string; data?: unknown; dataB64?: string }) => void;
  onOpen: (viaWebSocket: boolean) => void;
  /**
   * This underlying connection failed.
   *
   * - `duringHandshake`: it never connected. That means **this whole path may not work**,
   *   and the upper layer uses it to decide whether to fall back at once (WS → SSE) rather than
   *   retry with backoff.
   * - `retryable`: whether reconnecting makes sense for this kind of failure.
   *   ⚠️ **An upstream failure (keystrokes cannot get in) is false**: it means this PTY is already
   *   gone on the server
   *   (terminalId invalid); reconnecting would only fail again, while the user types blind for those
   *   15 seconds;
   *   worse than telling them directly "it is broken, click here to reconnect".
   * - `errorCode`: for reporting; `terminal_disconnected` and `terminal_input_failed`
   *   must be kept apart; see TransportEvents.onClosed.
   */
  onFailed: (info: {
    errorCode: string;
    reason: MessageKey;
    duringHandshake: boolean;
    retryable: boolean;
  }) => void;
};

/**
 * The WebSocket path.
 *
 * Shape: first get a one-time ticket (same-origin POST, with Cookie), then build the WS address
 * with the ticket.
 * For why a ticket is needed see the comment on issueTerminalTicket in data/api.ts.
 */
function openWS(runId: string, events: LowerEvents): Transport {
  let sock: WebSocket | null = null;
  let disposed = false;
  /** Whether the handshake ever succeeded. Decides the duringHandshake reported upward on failure. */
  let opened = false;

  void (async () => {
    let ticket: string;
    try {
      // ⚠️ **Every connection needs a new ticket**; never reuse one: tickets are one-time, reuse
      // gets 401,
      // and that failure looks like "auth is broken" when it is really "the ticket was used up".
      ticket = (await api.issueTerminalTicket(runId)).ticket;
    } catch {
      // Ticket failed: maybe no permission, the container is not running, or the backend does not
      // have this endpoint yet.
      // Treat all as "the WS path does not work" and fall back to SSE; **do not retry the ticket**:
      // none of those causes get better with retries, and the SSE path may be fine.
      events.onFailed({
        errorCode: "terminal_disconnected",
        reason: "sandbox.termReasonTicket",
        duringHandshake: true,
        // With duringHandshake=true the upper layer falls back to SSE at once and ignores this
        // field;
        // but false is honest: the causes of a ticket failure (no permission, container not
        // running,
        // backend lacks this endpoint) do not get better with retries.
        retryable: false,
      });
      return;
    }
    if (disposed) return;

    const url = `${wsBase()}/api/runs/${encodeURIComponent(runId)}/sandbox/terminal/ws?ticket=${encodeURIComponent(ticket)}`;
    try {
      sock = new WebSocket(url);
    } catch {
      // Construction throws right away (invalid address and the like).
      events.onFailed({
        errorCode: "terminal_disconnected",
        reason: "sandbox.termReasonWsUrl",
        duringHandshake: true,
        retryable: true,
      });
      return;
    }

    sock.onopen = () => {
      opened = true;
      events.onOpen(true);
    };
    sock.onmessage = (ev) => {
      const frame = parseFrame(ev.data);
      if (frame) events.onFrame(frame);
    };
    sock.onerror = () => {
      // onerror is always followed by an onclose, so do nothing here;
      // reporting in both places would give the upper layer two failures, skipping steps on the
      // backoff ladder.
    };
    sock.onclose = () => {
      if (disposed) return;
      events.onFailed({
        errorCode: "terminal_disconnected",
        reason: opened ? "sandbox.termReasonNoOutput" : "sandbox.termReasonWsHandshake",
        duringHandshake: !opened,
        // Connected once and then dropped → worth reconnecting (pod restart, network hiccup and
        // gateway idle drops all fall here).
        retryable: true,
      });
    };
  })();

  return {
    send(frame: unknown, _opts?: { silent?: boolean }) {
      // ⚠️ **Must check OPEN**: in the CONNECTING state send throws InvalidStateError.
      // The upper layer's pending buffer saves frames before connecting, so dropping here is safe
      // (they are flushed on connect).
      if (sock && sock.readyState === WebSocket.OPEN) {
        sock.send(JSON.stringify(frame));
      }
    },
    close() {
      disposed = true;
      // Remove the listeners first, then close, so the onclose triggered by close() does not
      // report another failure.
      if (sock) {
        sock.onopen = null;
        sock.onmessage = null;
        sock.onerror = null;
        sock.onclose = null;
        sock.close();
      }
      sock = null;
    },
  };
}

/**
 * The SSE + POST path (fallback).
 *
 * ⚠️ **It has a known defect that cannot be fixed**: one separate POST per keystroke, several
 * fetches in flight at once,
 * with arrival order decided by the network rather than send order, so **echo may be out of order
 * when typing fast**. That is exactly the
 * bug that was reported originally. WS is a single connection and keeps order naturally, so this
 * one is only a fallback, not "an equal alternative".
 *
 * # ⚠️ This path **does not take part in the upper layer's reconnect**; be careful in two places
 *
 * 1. **EventSource has built-in auto-reconnect.** So when downstream drops, do not report "failed"
 * upward for the upper layer to build another one:
 * the upper layer builds a **new EventSource** while the old one is still reconnecting by itself,
 * doubling the connection count.
 * That is exactly what we measured: e2e counted **3 SSE connections** (1 initial + 2 upper-layer
 * reconnects),
 * and that test case was meant to guard against StrictMode leaks, but happened to catch this too.
 * Only `readyState === CLOSED` (it gave up completely) is reported upward, and as "not
 * reconnectable".
 *
 * 2. **Upstream failures fire once per keystroke.** In production someone typed 25 times in one
 * minute,
 * and every one failed. Reporting it as "connection failed" to the upper layer would schedule one
 * reconnect per keystroke:
 * 25 keystrokes, 25 rounds of reconnect. So this **latches itself**: one connection reports only
 * once.
 */
function openSSE(runId: string, events: LowerEvents): Transport {
  let es: EventSource | null = null;
  let terminalId = "";
  let disposed = false;
  let opened = false;
  /**
   * Whether this connection has already reported a failure. **One connection reports only once.**
   *
   * See point 2 above: without the latch, 25 fast keystrokes would schedule 25 rounds of reconnect.
   */
  let reported = false;

  function reportOnce(info: {
    errorCode: string;
    reason: MessageKey;
    duringHandshake: boolean;
    retryable: boolean;
  }) {
    if (disposed || reported) return;
    reported = true;
    events.onFailed(info);
  }

  es = new EventSource(`/api/runs/${encodeURIComponent(runId)}/sandbox/terminal`, {
    withCredentials: true,
  });

  // The first frame gives the terminalId, which all later upstream frames must carry.
  es.addEventListener("ready", (ev) => {
    try {
      terminalId = (JSON.parse((ev as MessageEvent).data) as { terminalId: string }).terminalId;
      opened = true;
      events.onOpen(false);
    } catch {
      reportOnce({
        errorCode: "terminal_disconnected",
        reason: "sandbox.termReasonBadReadyFrame",
        duringHandshake: true,
        retryable: true,
      });
    }
  });

  es.addEventListener("closed", () => {
    // The server says clearly "this terminal is closed". **This is not a reconnectable hiccup.**
    //
    // ⚠️ **Must close() explicitly.** EventSource reconnects by itself after the connection ends,
    // and here we already
    // know "no need to connect again"; without closing, the browser keeps retrying at its own pace,
    // showing as
    // a growing number of SSE requests (measured with a stub SSE: from 1 to 3 within 4 seconds),
    // and those requests are neither part of our reconnect logic nor stoppable.
    es?.close();
    reportOnce({
      errorCode: "terminal_disconnected",
      reason: "sandbox.termReasonClosed",
      duringHandshake: false,
      retryable: false,
    });
  });

  es.onmessage = (ev) => {
    const frame = parseFrame(ev.data);
    if (frame) events.onFrame(frame);
  };

  es.onerror = () => {
    if (disposed) return;
    // ⚠️ EventSource **reconnects by itself**, and onerror also fires during reconnecting. Two cases:
    //
    //  1. `readyState === CLOSED`: the browser gave up completely (got a non-2xx, e.g. 404).
    //     **This is a real disconnect**; report it upward.
    //  2. `readyState === CONNECTING`: the stream ended and the browser is about to reconnect by
    //     itself.
    //     **Let it reconnect on its own; we stay out.**
    //
    // Why case 2 is not reported: EventSource's auto-reconnect is spec behavior, and it does it
    // better than we would
    // (with Last-Event-ID, using the retry interval the server gives). Reporting here would make
    // the upper layer **build another**
    // EventSource while the old one is still reconnecting; two reconnect systems stacked, doubling
    // the connection count.
    //
    // We measured this stacking: with a stub SSE ending every response at once, SSE requests grew
    // 1 → 3 → 5.
    // The old implementation also only checked CLOSED, for the same reason (that comment existed
    // before this file).
    //
    // The cost is that a half-dead state of "the stream keeps ending and reconnecting" is not
    // judged a failure. Acceptable:
    // on the user's side the terminal occasionally stalls but still works, and a real full
    // disconnect always reaches CLOSED.
    if (es?.readyState !== EventSource.CLOSED) return;
    reportOnce({
      errorCode: "terminal_disconnected",
      reason: opened ? "sandbox.termReasonNoOutput" : "sandbox.termReasonConnect",
      duringHandshake: !opened,
      // ⚠️ **Once in CLOSED, do not reconnect.**
      //
      // The browser gave up because it got a definite non-2xx response; our retries would only get
      // the same thing.
      // Measured: in mock mode SSE returned 404, and earlier this reported retryable=true, so the
      // upper layer built
      // two more EventSources and e2e counted 3; that test case was meant to guard against
      // StrictMode leaks,
      // but happened to catch this too.
      retryable: false,
    });
  };

  return {
    send(frame: unknown, opts?: { silent?: boolean }) {
      // Drop it if the terminalId has not arrived yet: those few characters are not worth an
      // error, and the user still sees "connecting".
      if (!terminalId) return;
      void api.writeRunSandboxTerminal(runId, terminalId, frame).catch(() => {
        // ⚠️ **Control frame (resize / pong) failures are not judged as faults.** See the comment
        // on Transport.send:
        // the terminal sends a resize as soon as it connects; without the exemption "upstream
        // broken" would, before the user types the first character,
        // put the UI into "connection lost".
        if (opts?.silent) return;
        // ⚠️ Go through reportOnce rather than onFailed directly: this callback **fires once per
        // keystroke**
        // (in production someone typed 25 times and all failed); reporting directly would make the
        // upper layer schedule 25 rounds of reconnect.
        reportOnce({
          // ⚠️ **Must be kept apart from the downstream code**: debugging must tell "typed and
          // nothing happened" from
          // "the screen does not refresh"; the two have completely different causes. This is an
          // existing rule.
          errorCode: "terminal_input_failed",
          reason: "sandbox.termReasonInput",
          duringHandshake: false,
          // ⚠️ **Not reconnectable**: an upstream failure means the server's PTY is already gone
          // (terminalId invalid),
          // reconnecting would only fail again, while the user types blind through the ten-plus
          // seconds of backoff.
          retryable: false,
        });
      });
    },
    close() {
      disposed = true;
      es?.close();
      es = null;
      terminalId = "";
    },
  };
}

/** Decode one downstream message; if it cannot be decoded, treat it as not received (the protocol is set by the platform and may have frames we do not know). */
function parseFrame(raw: unknown): { type?: string; data?: unknown } | null {
  if (typeof raw !== "string") return null;
  try {
    return JSON.parse(raw) as { type?: string; data?: unknown };
  } catch {
    return null;
  }
}
