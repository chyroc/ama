// WebSocket channel for the terminal: browser <-WS-> amad <-WS-> the real PTY in the container.
//
// # This path replaces "SSE down + POST up", and removes a whole cross-instance relay along the way
//
// The old shape was forced by the gateway on the main domain: the gateway (e.g.
// `ama.example.com`) negotiates h2 via ALPN, and h2 has no `Connection: Upgrade` (RFC 7540
// bans hop-by-hop headers), so the WS handshake never reaches amad.
//
// A separate entry point that **only negotiates HTTP/1.1** (e.g. `ama-ws.example.com`)
// was tested to give 101 + two-way traffic + 20 frames kept in order. So:
//
//	browser --WS--> amad --WS--> container PTY        (one connection, one process)
//
// Compared with the old shape `SSE (down) + POST (up) + pgmq (cross-instance relay)`, **three
// problems go away at once**:
//
//  1. **Out-of-order echo** (reported by users): the old path sent one separate POST per
//     keystroke, with several fetches in flight at once, so arrival order was decided by the
//     network, not by send order. A single WS keeps order by nature, so this bug cannot exist.
//  2. **Lag** (reported by users): the old path waited one full RTT per character
//     (browser→gateway→amad→pgmq→instance holding the connection→container). WS is a one-way
//     push and does not wait for a response.
//  3. **The whole cross-instance relay is no longer needed**: up and down are on **the same
//     connection, in the same process**, so pgmq is no longer needed to deliver keystrokes to
//     "the instance holding the PTY". That machinery existed for "the upstream lands on a
//     different instance", which cannot happen with WS.
//
// ⚠️ **The old path is not deleted; it is still the fallback**. The new path has two new
// risks: the gateway's ~30-second idle disconnect, and one more step in cross-origin auth.
// When either goes wrong the frontend must be able to fall back to SSE+POST instead of the
// terminal becoming unusable. See the path selection in web/src/components/SandboxTerminal.tsx.
//
// # Auth: there is no Cookie on this connection, only a one-time ticket
//
// Switching domains cuts both credential channels at once (the Cookie is host-only, and WS
// does not support custom request headers); see the file header of core/terminalticket.go
// for the full argument. So this route:
//
//   - **is registered on the bare mux, not through Protect/Guard**: they would return 401
//     before the handshake, and then the browser only sees "connection failed" and cannot tell
//     "not logged in" from "the gateway has a problem";
//   - **auth is redeeming that ticket**: the ticket already went through authentication and
//     authorization when issued (same-origin API, with Cookie, authz checked ActionManage),
//     and the actor and runID returned on redemption are the result of that authorization.
//
// ⚠️ **So it cannot be listed in authz/matrix.go** (that would trigger the reverse panic
// "declared but not registered"), which is exactly how it bypasses this repo's "new routes
// must declare permissions" gate. So the auth here is **hand-written**, pinned down by the
// TestTerminalWSRequiresValidTicket tests: the gate is bypassed, so tests must make up for it.
package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chyroc/ama/server/auth"
	"github.com/gorilla/websocket"
)

// terminalWSPingInterval is the interval for sending pings to the browser.
//
// ⚠️ **This value is set by the gateway's idle limit, not picked at random.** Tested:
// common gateways drop the connection after **30 seconds of silence** (alive at 28s, EOF at
// 30s, found by bisection). amad's own terminalIdleTimeout is 30 **minutes**, so that drop
// comes from the gateway side, and we cannot change it.
//
// 20 seconds leaves 1/3 of margin. **Do not raise it to 28**: that is the measured upper bound
// for staying alive, and a bit of network jitter would cross it.
//
// # Why the server pings, instead of the frontend sending something on a timer
//
// Two reasons:
//
//   - **The browser replies with pong automatically** (required by RFC 6455, and invisible to
//     JS), so one server ping produces traffic in **both** directions, resetting the gateway's
//     idle timers both ways. If the frontend sent it, only the upstream would have traffic.
//   - **The frontend needs no code for it**, so a hole like "forgot to implement the
//     heartbeat" cannot exist. The heartbeat has only one implementation, and it is on the
//     server, which is also the only place that knows "what the gateway limit is".
const terminalWSPingInterval = 20 * time.Second

// terminalWSWriteTimeout is the timeout for writing one frame to the browser.
//
// Keep it short: when the browser side is not reading (tab suspended, network down), writes
// block on the TCP buffer. Staying stuck would leak this goroutine together with the
// container connection it holds.
const terminalWSWriteTimeout = 10 * time.Second

// terminalWSUpgrader upgrades an HTTP connection to WebSocket.
//
// ⚠️ **CheckOrigin must check, and must not be left open like the probe endpoint.** This
// connection can run any command in the container; leaving it open means any website could
// open a terminal using the visitor's login (CSWSH, cross-site WebSocket hijacking).
//
// But the check cannot require same origin either: the page is on `ama.example.com` and the
// WS connects to `ama-ws.example.com`, so the Origin is always different. So it checks against
// an **allowlist** given by config (allowedWSOrigin), and when the config is empty it
// **rejects all cross-origin requests** (fail-closed).
//
// ⚠️ This must not fail open. "Allow everything when config is missing" on this path means
// opening the terminal to the whole internet, with zero errors: exactly the kind of failure
// this repo keeps warning about.
func (s *Service) terminalWSUpgrader() *websocket.Upgrader {
	allowed := s.allowedWSOrigins()
	return &websocket.Upgrader{
		HandshakeTimeout: 20 * time.Second,
		CheckOrigin: func(r *http.Request) bool {
			origin := strings.TrimSpace(r.Header.Get("Origin"))
			if origin == "" {
				// No Origin means a non-browser client (curl, probes, our own verification scripts).
				// Allow it: CSWSH requires "the browser attaches credentials automatically", but the
				// credential here is the ticket in the URL. A client without Origin cannot get someone
				// else's ticket, so the threat model does not apply.
				return true
			}
			for _, a := range allowed {
				if origin == a {
					return true
				}
			}
			s.log().Warn("终端 WS 拒绝跨域来源",
				slog.String("origin", origin),
				slog.Int("allowed_count", len(allowed)))
			return false
		},
	}
}

// StreamRunSandboxTerminalWS is the WebSocket endpoint for the terminal.
//
// It coexists with StreamRunSandboxTerminal (the SSE one): this is the main path, that one is
// the fallback.
//
// ⚠️ **This function does its own auth**, because it is registered on the bare mux (see the
// file header). The order must not change:
//
//	redeem ticket → compare runID → check container → upgrade protocol → connect container → relay both ways
//
// "Upgrade protocol" must come after auth: once upgraded, no HTTP status code can be written,
// so rejecting can only be done by closing the connection, and the browser sees "connected
// then dropped" without knowing why.
func (s *Service) StreamRunSandboxTerminalWS(
	w http.ResponseWriter, r *http.Request, runID string,
) {
	ctx := r.Context()

	// 1. Redeem the ticket. **This is the only auth point for this connection.**
	row, err := s.RedeemTerminalTicket(ctx, r.URL.Query().Get("ticket"))
	if err != nil {
		// Ticket missing / already used / expired are not told apart (see the comment on the Store
		// side). Return 401, not 404: this is "you have no valid credential", not "this thing does
		// not exist".
		http.Error(w, "终端入场票无效或已过期", http.StatusUnauthorized)
		return
	}

	// 2. Compare runID. ⚠️ **This step must not be skipped.**
	//
	// Without it, any valid ticket could open the terminal of **any** run: get a ticket for a run
	// you have access to, then swap the runId in the URL for someone else's. That is "one-time use
	// done right, authorization scope lost", and the latter is the real security boundary (authz
	// checks permissions per run).
	//
	// Same as the terminalId/runId pairing check in WriteRunSandboxTerminal: the same shape of
	// privilege escalation.
	if row.RunID != runID {
		s.log().Warn("终端 WS 票据与运行不匹配（可能是越权尝试）",
			slog.String("actor", row.Actor),
			slog.String("ticket_run", row.RunID),
			slog.String("url_run", runID))
		http.Error(w, "终端入场票与该运行不匹配", http.StatusForbidden)
		return
	}

	// 3. Put the identity into context.
	//
	// Downstream code (renewal, audit, container operations) always takes the actor from context,
	// exactly like every other API. That code does not need to know "the identity of this
	// connection came from a ticket".
	ctx = s.withTicketActor(ctx, row.Actor)

	// 3.5. Split by executor: a run that snapshotted a host_id takes the host terminal path
	// (browser → host_dispatches → the ama daemon's PTY); a run without host_id takes the
	// container path. Both paths share the same ticket and the same URL, so the split must happen
	// here, before the protocol upgrade.
	//
	// ⚠️ A GetRun error **must not silently fall back to the container path**: for a host run, a
	// DB failure would then turn into a misleading error like "container missing/unavailable",
	// sending debugging in the wrong direction. Failing to read the run is a storage-layer 502;
	// return it directly.
	runRec, err := s.store.GetRun(ctx, runID)
	if err != nil {
		http.Error(w, "读取运行信息失败", http.StatusBadGateway)
		return
	}
	if runRec.HostID != "" {
		s.streamHostTerminalWS(w, r, runID, runRec.HostID)
		return
	}

	// 4. Confirm the container is available, **before upgrading the protocol**.
	//
	// That way a failure returns an HTTP response with a status code, and the frontend can show
	// a message per case (container reclaimed vs paused). Finding out after the upgrade could
	// only close the connection, and the frontend would only know "cannot connect".
	state, err := s.RunSandboxState(ctx, runID)
	if err != nil {
		http.Error(w, "读取容器状态失败", http.StatusBadGateway)
		return
	}
	if state.Status != SandboxStatusRunning {
		// 409: this is neither "bad parameter" nor "no permission"; it is "cannot be done in the
		// current state".
		http.Error(w, fmt.Sprintf("容器当前不可用（状态：%s）", state.Status),
			http.StatusConflict)
		return
	}

	rec, err := s.store.GetRunSandbox(ctx, runID)
	if err != nil {
		http.Error(w, "读取容器信息失败", http.StatusBadGateway)
		return
	}
	sess, err := s.attachSandbox(ctx, runID, rec.SandboxID)
	if err != nil {
		http.Error(w, "连接容器失败", http.StatusBadGateway)
		return
	}

	// 5. Upgrade the protocol. **Only after all auth has passed.**
	client, err := s.terminalWSUpgrader().Upgrade(w, r, nil)
	if err != nil {
		// When Upgrade fails gorilla has already written a response; only log here.
		s.log().Warn("终端 WS 升级失败",
			slog.String("run", runID), slog.String("actor", row.Actor),
			slog.String("err", err.Error()))
		return
	}
	defer func() { _ = client.Close() }()

	// Audit: who connected to which container's terminal, and when.
	s.log().Info("webshell 终端连接（WS）",
		slog.String("actor", row.Actor),
		slog.String("run", runID),
		slog.String("session", rec.SessionID))

	// 6. Connect to the container (docker exec an interactive shell).
	upstream, err := sess.OpenTerminal(ctx)
	if err != nil {
		s.log().Error("连接容器终端失败（WS）",
			slog.String("run", runID), slog.String("err", err.Error()))
		// There is **no concurrency** here (relay has not started; only the current goroutine), but
		// it still goes through cw, so that "there is only one path for writes to the browser" always
		// holds.
		s.writeTerminalWSError(&clientWriter{conn: client}, "连接容器终端失败，请稍后重试")
		return
	}
	defer func() { _ = upstream.Close() }()

	// Renew once as soon as the terminal opens; do not wait for the first keystroke.
	//
	// ⚠️ **This one must not be skipped**: many uses are "terminal open, only watching, not
	// typing" (tail -f on logs, waiting for a build). Those have no keystrokes at all; relying only
	// on renewal from the upstream side would leave all of them unprotected, and they are exactly
	// the ones that suffer most if the container is frozen. Same as the comment on the SSE path.
	s.touchTerminal(ctx, runID)

	s.relayTerminalWS(ctx, client, upstream, runID)
}

// clientWriter serializes writes to the browser connection.
//
// ⚠️ **gorilla's Conn does not support concurrent writes** (the docs say so: only one writer
// at a time), and this connection has **three sources** writing:
//
//	(1) the container→browser goroutine forwarding PTY output
//	(2) the main loop sending a ping frame every 20 seconds (holding off the gateway's 30-second idle disconnect)
//	(3) an error frame sent when either side fails
//
// (1) is in a goroutine, (2) and (3) are in the main loop, so they **really do run concurrently**.
//
// # This bug was caught in code review, and the original comment was wrong
//
// That comment said "ping and writes both happen in this goroutine, so no write lock is
// needed". That is right for the main loop alone, but it missed the forwarding goroutine (1).
// Concurrent writes to a gorilla Conn cause **interleaved frames** (bytes of two frames mixed
// together); the browser cannot decode them and drops the connection. Worse, it is a race
// that does not show up at all under light load, so neither local runs nor e2e catch it.
//
// The existing terminalSession solved the same problem long ago with writeMu
// (sandboxterm.go), and this copies that shape. That also shows this was not a new finding:
// a known constraint was lost during a refactor.
type clientWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

// write sends one frame, serialized and with a write timeout.
func (w *clientWriter) write(mt int, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(terminalWSWriteTimeout))
	return w.conn.WriteMessage(mt, data)
}

// relayTerminalWS relays in both directions between the browser and the container until
// either side disconnects.
//
// # Shape: two goroutines, one per direction; whichever fails first, both shut down
//
// One goroutine cannot read both sides in turn: `ReadMessage` blocks, so while reading the
// browser, the container's output piles up in the TCP buffer, and programs with constant
// output like `top` visibly stutter.
//
// Cleanup relies on the two Close calls in defer: an error in either direction closes both
// connections, so the other goroutine's Read returns at once and both sides exit.
//
// ⚠️ **But writes need extra synchronization** (see clientWriter): three sources write to the
// browser connection concurrently.
func (s *Service) relayTerminalWS(
	ctx context.Context, client *websocket.Conn, upstream TerminalConn, runID string,
) {
	// All writes to the browser go through it; **do not call client.WriteMessage directly**.
	cw := &clientWriter{conn: client}
	// Container → browser.
	//
	// In a goroutine rather than the main loop: the main loop has to handle both the ping timer
	// and browser input.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			// The container-side read timeout uses amad's own idle limit (30 minutes),
			// unrelated to the gateway's 30 seconds: this leg does not go through the gateway.
			_ = upstream.SetReadDeadline(time.Now().Add(terminalIdleTimeout))
			mt, data, err := upstream.ReadMessage()
			if err != nil {
				return
			}
			if err := cw.write(mt, data); err != nil {
				return
			}
		}
	}()

	// Browser → container, plus the heartbeat.
	//
	// ⚠️ All writes go through cw (see clientWriter): this goroutine and the forwarding goroutine
	// above write to the same connection concurrently, and gorilla's Conn does not allow that.
	ping := time.NewTicker(terminalWSPingInterval)
	defer ping.Stop()

	// Browser input is also read in a separate goroutine, so the ping timer is not blocked by Read.
	type inbound struct {
		mt   int
		data []byte
		err  error
	}
	in := make(chan inbound, 16)
	go func() {
		defer close(in)
		for {
			// The browser side has **no read timeout**: silence is normal (the user is reading output or
			// thinking), and the gateway's 30 seconds are held off by our ping, so no need to check here
			// too. Setting one would wrongly treat "the user is not typing" as a disconnect.
			mt, data, err := client.ReadMessage()
			in <- inbound{mt: mt, data: data, err: err}
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			// The container side dropped (PTY exited, container paused/reclaimed).
			// Tell the browser before leaving, otherwise it only sees the connection close out of nowhere.
			s.writeTerminalWSError(cw, "容器终端已关闭")
			return
		case <-ping.C:
			// ⚠️ **This frame is the only reason this connection survives past 30 seconds** (the
			// gateway's idle limit). The browser replies with pong automatically (RFC 6455, invisible to
			// JS), so there is traffic in both directions.
			if err := cw.write(websocket.PingMessage, nil); err != nil {
				return
			}
			// Also renew "terminal still in use". **Watch-only use depends entirely on this**
			// (same as above: tail -f on logs has no keystrokes at all).
			s.touchTerminal(ctx, runID)
		case msg, ok := <-in:
			if !ok || msg.err != nil {
				// The browser closed the tab / the network dropped. Exit normally and log no error:
				// a client disconnect is not a server failure (this repo's
				// client-disconnect-must-not-be-5xx rule).
				return
			}
			// Forward to the container as is. **Do not parse the content**: the protocol is defined by the
			// platform (`{"type":"input","data":"ls"}` and so on); we only move bytes.
			//
			// ⚠️ No merging/batching here either. It is not needed with WS: one connection keeps order by
			// nature, and each frame costs only a few bytes of frame header, not an HTTP round trip.
			_ = upstream.SetWriteDeadline(time.Now().Add(terminalWSWriteTimeout))
			if err := upstream.WriteMessage(msg.mt, msg.data); err != nil {
				s.writeTerminalWSError(cw, "写入容器终端失败")
				return
			}
		}
	}
}

// writeTerminalWSError sends an error frame to the browser.
//
// The shape matches the container side's error frame (`{"type":"error","data":"..."}`),
// so the frontend has only one parsing path. It cannot tell whether the frame came from the
// container or from amad, and does not need to.
func (s *Service) writeTerminalWSError(cw *clientWriter, msg string) {
	payload, err := json.Marshal(map[string]string{"type": "error", "data": msg})
	if err != nil {
		return
	}
	// Go through cw instead of writing directly: this function is called from **two goroutines**.
	_ = cw.write(websocket.TextMessage, payload)
}

// withTicketActor puts the identity from the ticket into context, in exactly the same shape
// the auth middleware writes.
//
// # Why it must use auth.WithIdentity instead of putting in a key of our own
//
// Downstream code (audit in touchTerminal, credential resolution in attachSandbox, logs) all
// reads `auth.ActorFrom(ctx)`. With a different key of our own they would all read an empty
// string. The symptom is "the terminal works, but the audit log has no operator and the
// container gets no credentials", and neither of those reports an error.
//
// ⚠️ **Fill in only Email; do not make up Name / AvatarURL and the like.** The ticket only has
// the email, and leaving other fields empty is honest; a made-up display name would put a
// username in the audit log that cannot be looked up.
func (s *Service) withTicketActor(ctx context.Context, actor string) context.Context {
	return auth.WithIdentity(ctx, auth.Identity{Email: actor})
}

// allowedWSOrigins returns the allowlist of page origins allowed to open a terminal WS.
//
// ⚠️ **When empty, CheckOrigin rejects every request with an Origin** (fail-closed). That is
// deliberate: this connection can run any command in the container, and "allow all when not
// configured" would open the terminal to the whole internet, with zero errors. Better that the
// terminal cannot connect (logged, traceable) than silently open it.
//
// The value comes from the Service config. In local development the frontend is on 5273; in
// production it is a domain like ama.example.com. Both must be set explicitly; no wildcards.
func (s *Service) allowedWSOrigins() []string {
	if len(s.wsOrigins) == 0 {
		s.log().Warn("终端 WS 没有配置允许的跨域来源，将拒绝一切带 Origin 的连接",
			slog.String("hint", "配置 server.terminalWSOrigins"))
		return nil
	}
	return s.wsOrigins
}
