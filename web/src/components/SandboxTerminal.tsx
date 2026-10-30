import { lazy, Suspense, useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { Icon } from "@iconify/react";
import { Button, Tooltip } from "@heroui/react";

import { type SandboxStatus } from "@/data/api";
import { usePrefs } from "@/prefs/PrefsContext";
import { useWorkspace } from "@/app/WorkspaceGate";
import { terminalTabPath, useTerminalFullscreen } from "@/lib/useTerminalFullscreen";
import { connectTerminal, type Transport } from "@/lib/terminalTransport";

/**
 * Container terminal: a **real terminal**, not a command echo box.
 *
 * # Why xterm.js is used
 *
 * This used to be "one POST per command + output shown in a `<pre>`", with the reason written as "there is no streaming PTY;
 * feeding xterm would just use it as an expensive pre". **That premise was wrong**:
 * AIO Sandbox has `/v1/shell/ws`, a genuine PTY channel. It was changed after this was verified.
 *
 * So now it can do what it could not before: full-screen programs like `vim` / `top`, `Ctrl-C` to interrupt,
 * colored output, cursor control, Tab completion: the same experience as the platform's own terminal.
 *
 * # On the browser side it is SSE + POST, not WebSocket
 *
 * ⚠️ **The production gateway uses HTTP/2, and WebSocket cannot run over h2**: the upgrade headers are dropped, and the server sees
 * an invalid handshake (the log says `'upgrade' token not found in 'Connection' header`,
 * and the browser gets 400). Local `./dev.sh` is HTTP/1.1, so **this failure only appears in production**.
 *
 * So: downstream uses SSE (`GET .../sandbox/terminal`), and upstream is one POST per keystroke
 * (`POST .../sandbox/terminal/input`). Both are plain HTTP and work fine under h2.
 *
 * **Nothing of the real PTY is lost**: the amad side still connects to the container over WebSocket (that is an HTTP/1.1 connection
 * it opens itself, not going through the h2 gateway). See `server/core/sandboxterm.go`.
 *
 * It also solves the credential problem: the container channel needs `?jwt_token=`, and that token must never go to the browser
 * (it would go into history, bypass AMA's authorization layer, and could not be audited). Now it only exists in the amad process.
 *
 * # xterm is lazy-loaded
 *
 * `@xterm/xterm` is about 1 MB (**zero transitive dependencies**, passing the rule in DESIGN.md §5). But it is only needed
 * when the user really opens the terminal; downloading it on entering the chat page is paying for nothing. So it uses `React.lazy`,
 * the same approach as md-box.
 *
 * # Full screen and "open in new tab"
 *
 * Two entries at the top right, both **always visible in and out of full screen** (see the comment on TerminalActions).
 * Full screen is a CSS-level `fixed inset-0`, not the Fullscreen API; for the reason and parameter shape see
 * `lib/useTerminalFullscreen.ts`.
 *
 * The container status line is passed in by the parent through `meta`, on **the same row** as those two buttons
 * (see that comment in SandboxPanel: two rows would waste about 60px, and height is what the terminal lacks most).
 */
export function SandboxTerminal({
  runId,
  status,
  meta,
}: {
  runId: string;
  /**
   * Container status. **This component is rendered in every state**, and shows a disabled state itself when unavailable.
   *
   * ⚠️ This is a fixed bug: the parent used to write `status === "running" && <terminal/>`,
   * so as soon as the container paused (it does after 60 seconds idle) the whole component was unmounted, **React state was destroyed with it**,
   * and all the history output the user had typed was gone. The API was fine all along; looking only at the backend would never find it.
   */
  status: SandboxStatus;
  /**
   * Left half of the toolbar: container status badge + container id.
   *
   * The parent passes a node instead of this component checking status itself: the status is the copy the parent just fetched
   * (see "the state is not cached" in SandboxPanel); checking again here would give two possibly inconsistent truths.
   */
  meta?: React.ReactNode;
}) {
  const { t } = usePrefs();
  const [full, setFull] = useTerminalFullscreen();

  // When the container is unavailable, only show the explanation; do not connect.
  //
  // **Note there is no "open terminal" button here any more**: the user clicked the "terminal" tab because they want into the terminal;
  // making them click once more is a pure extra step (and after clicking they still wait for the connection, two waits).
  //
  // ⚠️ **This branch must also give a way out of full screen**: the container may be reclaimed while in full screen (idle past TTL),
  // and at that moment this replaces the tree below. Without it, the user is faced with a full screen of "the terminal can only open while the container is running"
  // and nowhere to go: exactly the "hiding controls by default traps people" rule.
  if (status !== "running") {
    return (
      <TerminalFrame full={full} onFullChange={setFull} runId={runId} meta={meta}>
        <div className="flex flex-col gap-3" data-testid="sandbox-terminal-closed">
          <p className="text-muted text-xs">{t("sandbox.terminalNeedsRunning")}</p>
        </div>
      </TerminalFrame>
    );
  }

  return (
    <TerminalFrame full={full} onFullChange={setFull} runId={runId} meta={meta}>
      <div className="flex min-h-0 flex-1 flex-col gap-3" data-testid="sandbox-terminal">
        <Suspense
          fallback={
            <div className="bg-surface-secondary text-muted r-container p-4 text-xs">
              {t("sandbox.terminalConnecting")}
            </div>
          }
        >
          <XtermView runId={runId} status={status} full={full} />
        </Suspense>
      </div>
    </TerminalFrame>
  );
}

/**
 * The terminal's shell: transparent when not full screen (only two entries hanging at the top right), and the viewport-filling layer in full screen.
 *
 * # Why the shell wraps the "container unavailable" branch
 *
 * Both branches share it, so **the exit full screen button is there in every state**. Written separately, one of them would surely
 * miss it, and the one that misses it is exactly the one that needs it most (the container is reclaimed during full screen, and the user faces an explanation
 * with no way out).
 *
 * # Each value of the full screen layer has a reason
 *
 * - `z-40` instead of `z-50`: HeroUI's modal / drawer / toast backdrops are all `z-50`,
 *   and so is AppShell's mobile drawer. 50 would put it at the same level, deciding by DOM order (fragile),
 *   while the right meaning is "dialogs and drawers should cover the full screen terminal". Same trade-off as OnboardingTour.
 * - `bg-background`: this layer must be **opaque**. Translucent would let the page text below show through,
 *   and the terminal itself is dark; mixed together neither can be read.
 * - Safe area on all four sides: the edges of `fixed inset-0` are the physical edges of the screen; the top would be covered by the Dynamic Island
 *   and the bottom by the Home Indicator (and in landscape the notch on the left/right). This trap is noted in both AppShell's drawer
 *   and OnboardingTour.
 */
function TerminalFrame({
  full,
  onFullChange,
  runId,
  meta,
  children,
}: {
  full: boolean;
  onFullChange: (next: boolean) => void;
  runId: string;
  meta?: React.ReactNode;
  children: React.ReactNode;
}) {
  /*
   * Toolbar: on the left "what container this is", on the right "what to do with it", **one row**.
   *
   * `justify-between` + `min-w-0` on the left half: the container id has 38 characters; without min-w-0
   * the flex child will not shrink (default `min-width:auto`), and on narrow screens it pushes the two buttons on the right out of
   * the viewport: a horizontal overflow only visible by measuring the boundingBox.
   *
   * `items-center` is written explicitly: the two sides have different heights (the Chip is shorter than the icon buttons),
   * and the default stretch would stretch the badge.
   */
  const toolbar = (
    <div className="flex items-center justify-between gap-2" data-testid="sandbox-terminal-toolbar">
      <div className="flex min-w-0 items-center">{meta}</div>
      <TerminalActions full={full} onFullChange={onFullChange} runId={runId} />
    </div>
  );

  if (!full) {
    return (
      <div className="flex flex-col gap-2">
        {toolbar}
        {children}
      </div>
    );
  }

  return (
    <div
      className="bg-background fixed inset-0 z-40 flex flex-col gap-2 p-3"
      style={{
        paddingTop: "calc(0.75rem + var(--safe-top))",
        paddingBottom: "calc(0.75rem + var(--safe-bottom))",
        paddingLeft: "calc(0.75rem + var(--safe-left))",
        paddingRight: "calc(0.75rem + var(--safe-right))",
      }}
      data-testid="sandbox-terminal-fullscreen"
    >
      {toolbar}
      {children}
    </div>
  );
}

/**
 * The two entries at the top right.
 *
 * # Why "exit full screen" was built, and only as a button without Esc
 *
 * **Built**: without it, once in full screen you cannot get out: refreshing is still full screen (the parameter is in the URL),
 * leaving only "delete the query by hand" or "press browser back", and the latter under `replace: true`
 * kicks the user right out of this page. The "hiding controls by default is dangerous" rule is a hard flaw here.
 *
 * **No Esc**: ⚠️ in full screen the keyboard focus is in xterm, and **Esc in the terminal must pass through to the
 * shell as is**: vim uses it to leave insert mode, readline uses it as the Alt prefix. Attaching a
 * `keydown` listener (on window or the container) would kick vim users out of full screen every time they press Esc,
 * while all they wanted was to leave insert mode.
 *
 * "Only respond when focus is not in the terminal" does not work either: the point of full screen is to give focus to the terminal,
 * so that condition is almost always false, which means building a feature that does not work, plus a code path that can hurt.
 * So only a button: it is always visible (see TerminalFrame), which is enough.
 *
 * # Icon direction
 *
 * Enter uses `chevrons-expand-up-right` (expanding out), exit uses `chevrons-collapse-up-right`
 * (collapsing in), a pair from the same family, not xmark: xmark here would be read as "close the terminal",
 * while it only changes the size.
 */
function TerminalActions({
  full,
  onFullChange,
  runId,
}: {
  full: boolean;
  onFullChange: (next: boolean) => void;
  runId: string;
}) {
  const { t } = usePrefs();
  const { workspace } = useWorkspace();

  return (
    <div className="flex shrink-0 items-center gap-1">
      {/* New tab: **use <Link> + target instead of window.open**, so middle click,
          Cmd/Ctrl+click and right-click "open in new tab" all work as usual: those are free browser behaviors for anchors,
          and onPress would lose them all. `rel` is required: without it the opened page
          could control this page back through window.opener (same as the note in MarkdownBody).

          ⚠️ It **stays in full screen too**: then it is even more useful (watch the terminal full screen on one side,
          and open another tab to look at other runs on the other). */}
      <Tooltip>
        <Tooltip.Trigger>
          <Link
            to={terminalTabPath(workspace.key, runId)}
            target="_blank"
            rel="noopener noreferrer"
            aria-label={t("sandbox.terminalOpenInNewTab")}
            className="text-muted hover:bg-surface-secondary hover:text-foreground r-inset flex size-8 items-center justify-center no-underline cursor-[var(--cursor-interactive)]"
            data-testid="sandbox-terminal-newtab"
          >
            <Icon icon="gravity-ui:arrow-up-right-from-square" className="size-4" />
          </Link>
        </Tooltip.Trigger>
        <Tooltip.Content>{t("sandbox.terminalOpenInNewTab")}</Tooltip.Content>
      </Tooltip>

      <Tooltip>
        <Tooltip.Trigger>
          <Button
            size="sm"
            variant="ghost"
            isIconOnly
            aria-label={full ? t("sandbox.terminalExitFullscreen") : t("sandbox.terminalFullscreen")}
            data-testid="sandbox-terminal-fullscreen-toggle"
            data-full={full ? "1" : "0"}
            onPress={() => onFullChange(!full)}
          >
            <Icon
              icon={
                full ? "gravity-ui:chevrons-collapse-up-right" : "gravity-ui:chevrons-expand-up-right"
              }
              className="size-4"
            />
          </Button>
        </Tooltip.Trigger>
        <Tooltip.Content>
          {full ? t("sandbox.terminalExitFullscreen") : t("sandbox.terminalFullscreen")}
        </Tooltip.Content>
      </Tooltip>
    </div>
  );
}

type TerminalCtor = typeof import("@xterm/xterm").Terminal;
type FitAddonCtor = typeof import("@xterm/addon-fit").FitAddon;

/**
 * The layer that actually mounts xterm, lazy-loaded.
 *
 * It is pulled out so `React.lazy` has something to split: it can only wrap a default-exported component,
 * while the layer above must render the warnings and buttons on the synchronous path.
 */
const XtermView = lazy(async () => {
  const [{ Terminal }, { FitAddon }] = await Promise.all([
    import("@xterm/xterm"),
    import("@xterm/addon-fit"),
    // Pull the CSS in too: without it character measurement and the cursor are all wrong (xterm depends on its own classes).
    import("@xterm/xterm/css/xterm.css"),
  ]);
  return { default: makeXtermView(Terminal, FitAddon) };
});

/**
 * Builds a view component bound to the xterm constructors.
 *
 * A factory instead of a top-level import: these two classes are only available after the lazy load finishes.
 */
function makeXtermView(Terminal: TerminalCtor, FitAddon: FitAddonCtor) {
  return function XtermViewInner({
    runId,
    status,
    full,
  }: {
    runId: string;
    status: SandboxStatus;
    /**
     * Whether full screen. **Only decides CSS classes**; size adaptation is handled by the ResizeObserver in connect
     * (see the comment there: switching full screen changes exactly the host's box, so the observer is sure to fire).
     */
    full: boolean;
  }) {
    const { t } = usePrefs();
    const hostRef = useRef<HTMLDivElement>(null);
    const termRef = useRef<InstanceType<TerminalCtor> | null>(null);
    /**
     * The current transport layer (WS or the SSE fallback). **No longer holds an EventSource directly**:
     * route choice, reconnects and degrading are all in lib/terminalTransport.ts; this only sends and receives frames.
     */
    const transportRef = useRef<Transport | null>(null);
    /**
     * Whether this connection has already reported failure. **One connection reports only once.**
     *
     * ⚠️ It must be **reset to false** in the dispose of connect() below,
     * not just initialized once here: under StrictMode the effect runs twice (mount → unmount → mount again),
     * and the second run reuses the same ref object. Set but never reset, the second mount starts **already latched**,
     * so the failure notice and the report silently stop working in development, while production (no StrictMode)
     * is fine: this kind of "broken only in dev" difference is the hardest to notice.
     * Same as pollAbort in useIntegrationAuthorize.ts.
     */
    const failedRef = useRef(false);
    /**
     * Reconnect counter. **Changing it means rebuilding the whole connection**: it is in the dependency array of the effect below,
     * so React first runs the full dispose (closing the old connection, the old xterm, and resetting failedRef),
     * then connect() again.
     *
     * Why not write a reconnect() function that calls connect() directly: then you would have to remember to clean up the previous round
     * yourself, while the "clean up" logic is already in dispose. Letting the effect rerun is the only way
     * that cannot miss cleanup: this component already fell into the StrictMode trap once, exactly because
     * it tried to bypass the effect and manage the lifecycle itself.
     *
     * ⚠️ Note this is a **manual** reconnect (the user clicks a button). Automatic reconnects are done by the transport layer itself (with backoff and a cap),
     * not through here: the two reconnect mechanisms must be kept apart; putting both here would make the backoff ladder skip steps.
     */
    const [attempt, setAttempt] = useState(0);
    const reconnect = useCallback(() => setAttempt((n) => n + 1), []);
    /**
     * Connection state. **One enum, not two variables "connected boolean + failed string".**
     *
     * ⚠️ Written that way it would show "connected" and "disconnected" at the same time: two variables each saying their own thing,
     * while they describe two sides of the same thing. That is exactly what the screenshot looked like.
     *
     * `recycled` is a **sub-case** of closed, not a fifth parallel state: failing to connect when the container has been reclaimed
     * is an **expected result**, and saying "disconnected" would make people think the platform is broken. It is still just one way of saying
     * the single fact "cannot connect", so it goes into the same enum instead of another variable.
     */
    const [phase, setPhase] = useState<"connecting" | "connected" | "closed" | "recycled">(
      "connecting",
    );
    /**
     * Which path is currently used. **Only affects that small line of text, not any behavior.**
     *
     * Why show it: WS and SSE feel clearly different (the latter goes out of order when typing fast, one RTT per character).
     * When users complain "the terminal is laggy", the first question to answer is "which path are you on now";
     * without showing it that question could only be answered by digging through the network panel.
     */
    const [viaWS, setViaWS] = useState<boolean | null>(null);

    // Once the container is no longer running, disconnect and explain it on screen.
    //
    // **The component is not unmounted** (that was exactly the earlier bug), so the history already typed stays on screen.
    useEffect(() => {
      if (status !== "running" && transportRef.current) {
        transportRef.current.close();
        transportRef.current = null;
        termRef.current?.writeln(`\r\n\x1b[33m${t("sandbox.terminalClosed")}\x1b[0m`);
      }
    }, [status, t]);

    /**
     * One connection failure: classify → show → report.
     *
     * # Why the two failure paths share it
     *
     * "Cannot receive output" (SSE dropped) and "cannot send input" (POST failed) are two different faults,
     * so their errorCode differs; but **there is only one check for "does this count as a fault"**, and it must be implemented in only one
     * place. With one copy each, sooner or later only one side gets that gate, and the side that missed it starts spamming noise,
     * with zero errors.
     *
     * # ⚠️ The check is not the `status` parameter here
     *
     * The top of the component already says "no connection unless running", so when this function is reached `status` is always
     * `running`: using it as the check is no check at all. The real check is **querying the container again now**
     * (in reportConnectionFailure), because the container is reclaimed exactly between the two moments "entering the page" and "connection failed".
     *
     * The UI must also follow the situation: when the container has been reclaimed, saying "disconnected" sounds like the platform is broken,
     * while it is really the normal ending after a session ends. So here we also query once to decide the wording.
     *
     * # ⚠️ One connection is handled only once, latched by failedRef
     *
     * "Cannot send input" fires **once per keystroke**: tested in production, someone typed 25 times in a row within a minute,
     * and each time ran two API round trips (one to decide the wording, one for the report check): 75 requests,
     * all on the same already broken connection.
     *
     * With the latch, this function really runs only once in a connection's lifetime. **No information is lost**:
     * the backend already dedups by `routePattern|errorCode|httpStatus` and only adds up occurrences,
     * so the 2nd to 25th have nothing new besides noise.
     */
    const failConnection = useCallback(
      (errorCode: string, message: string) => {
        // ⚠️ The latch is **at the very front**, before any await: this function is called repeatedly by several keystrokes
        // in the same event loop turn, and with the check after an await they would all have passed the gate already.
        if (failedRef.current) return;
        failedRef.current = true;

        // First show it as "really disconnected". The fresh query takes one round trip, and users should not wait staring at "connecting";
        // if the result shows it was reclaimed, correct the wording then (see .then below).
        setPhase("closed");
        // Also say it in the terminal. **The small line in the status bar alone is not enough**: the user's attention is on the cursor,
        // and those 25 consecutive keystrokes in production show they had no idea they were typing blind. Written into the terminal,
        // the sentence appears exactly where they are looking, and stays in the scroll history.
        termRef.current?.writeln(`\r\n\x1b[33m${message}\x1b[0m`);
        void (async () => {
          try {
            const { reportConnectionFailure } = await import("@/lib/feedback");
            const { api } = await import("@/data/api");
            // ⚠️ This query is **only for the wording**; the report side does its own query.
            // Sharing one result would make "the report check" depend on "the render timing"; those are two things,
            // and once tied together any change to one side would silently change the other.
            const state = await api.getRunSandbox(runId);
            if (state.status === "gone" || state.status === "none") setPhase("recycled");
            void reportConnectionFailure(errorCode, message, runId);
          } catch {
            // If it cannot be determined, keep "disconnected", and **do not report** (same as above: better to miss one).
          }
        })();
      },
      [runId],
    );

    const connect = useCallback(() => {
      const host = hostRef.current;
      if (!host) return undefined;

      const term = new Terminal({
        cursorBlink: true,
        fontSize: 12,
        fontFamily: '"Cascadia Code", "Fira Code", Menlo, Consolas, monospace',
        // convertEol: treat \n as a newline even when the container only sends \n, otherwise output piles up in a staircase to the right.
        convertEol: true,
        theme: { ...TERMINAL_THEME },
      });
      const fit = new FitAddon();
      term.loadAddon(fit);
      term.open(host);
      fit.fit();
      termRef.current = term;

      // Transport layer: **WebSocket first, falling back to SSE + POST if it cannot connect.**
      //
      // Route choice, automatic reconnects (with backoff and a cap), and the degrade decision are all in lib/terminalTransport.ts.
      // This only does two things: draw received frames onto xterm, and hand keystrokes over.
      //
      // ⚠️ **Do not move the reconnect logic back here.** The transport layer's reconnect is "swap the underlying connection, keeping the same
      // Transport object", while this component's attempt is "rebuild xterm and the connection entirely".
      // Mixing the two would make the backoff ladder skip steps (one manual reconnect click would reset the automatic backoff).
      const transport = connectTerminal(runId, {
        onFrame: (msg) => {
          switch (msg.type) {
            case "output": {
              // The host daemon's PTY output uses dataB64 (raw bytes, possibly invalid UTF-8);
              // container output is still text data. After atob, feed it to xterm as a Uint8Array and let it
              // decode UTF-8 itself, avoiding invalid bytes being replaced through an intermediate string.
              if (typeof (msg as { dataB64?: unknown }).dataB64 === "string" &&
                  (msg as { dataB64?: string }).dataB64) {
                const bin = atob((msg as { dataB64: string }).dataB64);
                const bytes = new Uint8Array(bin.length);
                for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
                term.write(bytes);
              } else {
                term.write(String(msg.data ?? ""));
              }
              break;
            }
            case "restore_output":
              term.write(String(msg.data ?? ""));
              break;
            case "ready":
            case "terminal_restored":
              term.writeln(`\x1b[32m${String(msg.data ?? "")}\x1b[0m`);
              break;
            case "ping":
              // Application-level heartbeat: sent by the container, and must be answered.
              //
              // ⚠️ This is separate from the **gateway**'s 30-second idle disconnect: the latter is held off by the server's WS ping
              // (the browser replies pong automatically, invisible to JS), and the frontend need not care. What is answered here is the ping frame
              // in the container's protocol; without an answer the platform would decide this PTY is dead.
              // silent: a control frame; if sending fails the user feels nothing, so it should not count as a connection fault.
              transportRef.current?.send({ type: "pong", timestamp: msg.data }, { silent: true });
              break;
            case "error":
              term.writeln(`\r\n\x1b[31m${String(msg.data ?? "")}\x1b[0m`);
              break;
            default:
              // Things like session_id are internal to the protocol and not shown.
              break;
          }
        },
        onOpen: (usedWS) => {
          setPhase("connected");
          setViaWS(usedWS);
          // Tell the container the window size, otherwise it lays out at 80x24 and all line breaks are wrong.
          // ⚠️ **silent must stay.** The terminal sends this frame as soon as it connects; if its failure also counted as
          // a "connection fault", then when "upstream is entirely down" the UI would turn into "disconnected" **before the user
          // types the first character**, while they have done nothing. In the old implementation the failure of this frame was
          // voided (only the keystroke path called failConnection); that distinction must not be lost.
          transportRef.current?.send(
            { type: "resize", data: { cols: term.cols, rows: term.rows } },
            { silent: true },
          );
        },
        onReconnected: () => {
          // ⚠️ **Once a permanent failure has been reported, do not switch the state back to "connected".**
          //
          // failedRef being latched means "this connection has been judged dead": the UI then has a red dot and
          // a "reconnect" button, and the user is about to click it. Quietly flipping back to "connected" because the underlying reconnect
          // succeeded would make the user think they misread, while the real fault (like keystrokes not reaching the container)
          // is still there: **downstream working does not mean upstream works**.
          //
          // Actually hit: the mock SSE stub closes after sending one frame, so after "upstream failure was decided"
          // the underlying reconnect succeeded and the state flipped back to connected, covering up the real fault.
          if (failedRef.current) return;

          setPhase("connected");
          // ⚠️ **This line must be said; do not pretend nothing happened.**
          //
          // After reconnecting it is a **new PTY**: shell history, the current directory, and running foreground processes are all gone.
          // The user thinking they are still in the old shell is worse: they might go on with a command that depends on earlier context
          // (`rm -rf ./x` after a `cd`, that kind), with consequences far worse than "seeing a hint line".
          //
          // Written into the terminal, not just the status bar: the user's attention is on the cursor.
          term.writeln(`\r\n\x1b[33m${t("sandbox.terminalReconnected")}\x1b[0m`);
          // The new PTY must be told the size again. Also silent (see the onOpen part).
          transportRef.current?.send(
            { type: "resize", data: { cols: term.cols, rows: term.rows } },
            { silent: true },
          );
        },
        onClosed: (errorCode, reason) => {
          // This is only reached **after the transport layer gives up reconnecting**, so reporting once here is clean
          // (reporting on every retry along the way would spam noise).
          //
          // ⚠️ errorCode comes from the transport layer and **must not be hard-coded here**:
          // "cannot receive output" and "keystrokes cannot get in" must be reported separately; when debugging you need to tell whether the user
          // "typed and nothing happened" or "the screen does not refresh"; the causes are completely different.
          failConnection(errorCode, t("sandbox.terminalDisconnectedWith", { reason: t(reason) }));
        },
      });
      transportRef.current = transport;

      term.onData((d) => {
        // ⚠️ **No error handling needed any more**: the WS path is fire-and-forget (failures are taken over by onclose → automatic reconnect),
        // and POST failures on the SSE fallback path are also turned into reconnect decisions in the transport layer.
        // Catching again here would report the same fault twice.
        transport.send({ type: "input", data: d });
      });
      term.onResize(({ cols, rows }) => {
        // A control frame, silent. Users cannot see it fail, and should not see "disconnected" because of it.
        transport.send({ type: "resize", data: { cols, rows } }, { silent: true });
      });

      /*
       * Size adaptation. **Switching full screen goes through this too; there is no second path.**
       *
       * # There used to be one more "rAF + fit when switching full screen" effect here, rejected by a mutation check
       *
       * That effect's comment said "ResizeObserver is not enough", with the reason "the PTY size relies on
       * fit to trigger onResize". **That reason was wrong**: ResizeObserver observes exactly the host,
       * and the full screen layer changes exactly the host's box (`h-[60vh]` → `flex-1`), so it surely fires,
       * surely fits, and `term.onResize` surely reports the PTY size along with it.
       *
       * How it was found: removing that effect alone, the "more rows after full screen" case **stayed all green**,
       * because this caught it. Removing this alone was also all green, because that effect caught it.
       * Removing either layer alone is a no-op; only **removing both together** turns red (same as this repo's rule
       * "mutating redundant defenses must remove the whole group"). Since only one layer is needed, keep the one that came first,
       * instead of keeping new code that argues for its own existence with a wrong reason.
       *
       * # Why there is no need to "wait one frame"
       *
       * The ResizeObserver callback **already runs after layout** (the browser notifies observers after computing the boxes),
       * so what it reads is always the new size. That rAF was for "reading the box synchronously inside an effect",
       * and this path is not inside an effect, so the problem does not exist.
       *
       * ⚠️ **`full` must never go into connect's dependencies** (this still holds): then one full screen switch would
       * rebuild the whole terminal: SSE reconnect, a new xterm, and all the history already typed gone. That is the same kind as the bug
       * noted at the top of the file, only the trigger changes from "container paused" to "clicked full screen".
       * So size changes can only rely on observing the DOM, not on rerunning connect.
       */
      const ro = new ResizeObserver(() => {
        try {
          fit.fit();
        } catch {
          // When the panel is hidden its width and height are 0 and fit throws; just ignore it.
        }
      });
      ro.observe(host);

      // Attach the term instance to the host node, **only for testability**.
      //
      // xterm v6 renders to canvas by default, so there is no text in the DOM to assert on: without this opening,
      // "what does the terminal actually show" is completely unobservable in automation, only by eyeballing screenshots.
      // It attaches a read-only reference and changes no behavior.
      (host as HTMLDivElement & { __term?: unknown }).__term = term;

      // **Collect everything built in this round at once**: the observer, the transport layer, the xterm instance, and those refs.
      // Miss any one, and StrictMode's second run leaves an orphan connection.
      //
      // ⚠️ `transport.close()` also stops **the automatic reconnect timer**. Without it,
      // after the component unmounts the timer would wake up and reconnect once: an orphan connection tied to an unmounted component,
      // and its failure callback would write to an xterm that no longer exists.
      return () => {
        ro.disconnect();
        transport.close();
        term.dispose();
        delete (host as HTMLDivElement & { __term?: unknown }).__term;
        transportRef.current = null;
        termRef.current = null;
        // ⚠️ Reset the latch flag: the next connection must be able to report a failure again. See the comment on failedRef.
        failedRef.current = false;
      };
    }, [runId, t, failConnection, attempt]);

    useEffect(() => {
      // ⚠️ **Under StrictMode this effect runs twice** (on purpose by React: mount → unmount →
      // mount again, to expose cleanup that is not complete).
      //
      // The first version fell here: connect()'s guard looked at `termRef.current`, which was set back to
      // null during cleanup, so the second run built another one: **two terminals, two SSE connections**. The screenshot showed exactly
      // that: two prompt lines on screen, and the status bar saying "connected" and "disconnected" at the same time
      // (the later one connected, the earlier one was cleaned up, and two variables each said their own thing).
      //
      // The right way is **to make cleanup really collect everything of this round**, not to add a flag to dodge
      // the second run: dodging only hides the problem, and in production (no StrictMode) the same
      // cleanup defect would show up as "connection leaks" when switching runs / tabs.
      //
      // So: connect returns a complete dispose, and this calls it unconditionally.
      const dispose = connect();
      return dispose;
    }, [connect]);

    return (
      <div className={`flex flex-col gap-2 ${full ? "min-h-0 flex-1" : ""}`}>
        <div className="flex shrink-0 items-center gap-2 text-xs">
          <span
            className={`size-2 rounded-full ${
              phase === "connected"
                ? "bg-success"
                : phase === "closed"
                  ? "bg-danger"
                  : // recycled and connecting both use a neutral color: reclaimed **is not a fault**,
                    // and a red dot would make a normal thing look like an error.
                    "bg-muted"
            }`}
            data-testid="sandbox-terminal-state"
            data-phase={phase}
          />
          {/* **Only one sentence is shown**; "connected" and "disconnected" never appear side by side. */}
          <span className={phase === "closed" ? "text-danger" : "text-muted"}>
            {phase === "connected"
              ? t("sandbox.terminalConnected")
              : phase === "closed"
                ? t("sandbox.terminalDisconnected")
                : phase === "recycled"
                  ? t("sandbox.terminalRecycled")
                  : t("sandbox.terminalConnecting")}
          </span>
          {/*
            Show a badge when on the fallback channel. **Only on fallback**: WS is the expected state,
            and a badge for it too would only take space (same as "controls hidden by default need derived visibility":
            what should be shown is the exception, not the norm).

            ⚠️ The check is `viaWS === false`, not `!viaWS`: viaWS starts as null
            (not connected yet), and `!null` is true, so "compatibility mode" would flash while connecting.
          */}
          {phase === "connected" && viaWS === false && (
            <Tooltip>
              <Tooltip.Trigger>
                <span
                  className="text-warning border-warning/40 r-inset border px-1.5 py-0.5 text-[11px]"
                  data-testid="sandbox-terminal-fallback"
                >
                  {t("sandbox.terminalViaFallback")}
                </span>
              </Tooltip.Trigger>
              <Tooltip.Content>{t("sandbox.terminalViaFallbackHint")}</Tooltip.Content>
            </Tooltip>
          )}
          {/*
            When disconnected, offer a "reconnect". **This is half of this fix**: of those 67
            terminal_input_failed entries in production, a single one reached 25 occurrences: every character the user typed failed,
            and apart from one small line of text there was nothing actionable on screen, so they just kept typing.

            ⚠️ **Only offered when closed, not when recycled**: reconnecting when the container is reclaimed is sure to fail again,
            and a button that does nothing is worse than none (same trade-off as Page.tsx giving no retry for 403).
          */}
          {phase === "closed" && (
            <Button
              size="sm"
              variant="ghost"
              onPress={reconnect}
              data-testid="sandbox-terminal-reconnect"
            >
              {t("sandbox.terminalReconnect")}
            </Button>
          )}
        </div>
        {/*
          Two sets of heights, and the full screen set **must not use `h-full`**.

          `h-[60vh] min-h-80` is the fixed height inside the panel (the panel itself is in the page scroll area,
          so a percentage height would collapse to 0). In full screen it becomes `min-h-0 flex-1`: the shell is
          a `fixed inset-0` flex column, so only flex-1 gets "however much is left".

          ⚠️ **`min-h-0` must stay**: a flex child's default `min-height:auto` makes it grow to the
          content height instead of being compressed, so a terminal with long output pushes the whole layer past the bottom of the viewport: the last few rows
          and the safe-bottom we just added are pushed off screen together.
        */}
        <div
          ref={hostRef}
          className={`r-container overflow-hidden p-2 ${
            full ? "min-h-0 flex-1" : "h-[60vh] min-h-80"
          }`}
          style={{ background: TERMINAL_THEME.background }}
          data-testid="sandbox-terminal-host"
          data-full={full ? "1" : "0"}
        />
      </div>
    );
  };
}

/**
 * Terminal colors: **one fixed dark scheme, not following the page theme.**
 *
 * ⚠️ A trap was hit here: the foreground used to read the page's `--foreground`, thinking "following the theme fits better".
 * The result: **in light mode the whole terminal was black and unreadable**: `--foreground` in light mode is near black
 * (`oklch(0.1721 …)`, the body text color for white backgrounds), while the terminal background is black: black text on black.
 * And it reported nothing; the screenshot was just a solid black block.
 *
 * The root cause was that the assumption itself was wrong: **the terminal is not part of the page; it is a screen embedded in it.**
 * No terminal in the world turns its text black because the IDE switched to a light theme. So here is a
 * self-consistent dark scheme (values taken from VS Code dark), the same in light and dark modes:
 * the opposite direction of the md-box trap: that time it was "should follow the theme but was hard-coded", this time "should not follow but did".
 *
 * The 16 ANSI colors are filled in too: otherwise xterm uses its own defaults, and the blue directories in `ls --color`
 * are `#0000ee` on black, almost unreadable.
 */
const TERMINAL_THEME = {
  background: "#1e1e1e",
  foreground: "#d4d4d4",
  cursor: "#d4d4d4",
  cursorAccent: "#1e1e1e",
  selectionBackground: "#264f78",
  black: "#000000",
  red: "#f14c4c",
  green: "#23d18b",
  yellow: "#f5f543",
  blue: "#3b8eea",
  magenta: "#d670d6",
  cyan: "#29b8db",
  white: "#e5e5e5",
  brightBlack: "#666666",
  brightRed: "#f14c4c",
  brightGreen: "#23d18b",
  brightYellow: "#f5f543",
  brightBlue: "#3b8eea",
  brightMagenta: "#d670d6",
  brightCyan: "#29b8db",
  brightWhite: "#ffffff",
} as const;
