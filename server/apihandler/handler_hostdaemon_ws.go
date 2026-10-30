// WebSocket endpoint for the ama daemon: GET /api/host-daemon/ws.
//
// # It is another transport over the same queue semantics as long polling
//
// For auth (hostTokenAuth) and where the route lives (bare mux, not in the matrix), see the file
// header of handler_hostdaemon.go. The in-connection session logic is in service/host/stream.go:
// this file only upgrades the HTTP connection and translates frames into Stream method calls. The
// source of truth for correctness (seq cursor, SKIP LOCKED, lease redelivery, CAS done) is all in
// PG, so losing this connection loses no functionality.
//
// # Two goroutines per connection
//
//	the read loop (its own goroutine) only does ReadJSON -> push to the inbound channel, and never
//	blocks on business handling; otherwise, when the peer pushes results_ack we could not read it,
//	while our own dispatch writes fill up the TCP window: deadlock.
//	The main goroutine handles hello/results serially, plus the 20s protocol-level ping and the
//	cleanup when kicked; the pump goroutine only handles pushing dispatches down after a NOTIFY wakeup.
//
// All writes go through daemonWSWriter (mutex + 10s write timeout); gorilla Conn does not allow
// concurrent writes.
//
// # CheckOrigin: reject anything with an Origin
//
// The browser WebSocket API cannot set an Authorization handshake header, and this endpoint only
// accepts the host_token in Authorization (there is no cookie path), so the precondition for CSWSH
// does not hold in the first place. Explicitly rejecting every handshake with an Origin is a second
// safeguard, and it also gives "someone connecting to this endpoint from a browser" a clear 403 at
// the handshake stage.
package apihandler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/chyroc/ama/server/model/persistmodel"
	hostsvc "github.com/chyroc/ama/server/service/host"
	"github.com/chyroc/ama/server/shared/hostproto"
	"github.com/gorilla/websocket"
)

const (
	// hostDaemonWSFirstHello is the limit for waiting for the first hello frame after upgrade. The
	// daemon normally sends it right after a successful handshake; 25s is a bit longer than one
	// keepalive tick, and a stuck connection should not hold a registry slot for long.
	hostDaemonWSFirstHello = 25 * time.Second
	// Keepalive tick: server-side protocol-level ping. 20s, same as the terminal WS and the daemon's
	// hello tick: common gateways cut connections that are silent for ~30s, and 20s leaves a margin
	// (see the comment on terminalWSPingInterval in sandboxtermws.go).
	hostDaemonWSPingInterval = 20 * time.Second
	// After a pong (or any frame), push the read deadline out to 60s: the link is judged dead only
	// after three keepalive ticks in a row are lost, so one gateway hiccup does not disconnect it.
	hostDaemonWSPongWait  = 60 * time.Second
	hostDaemonWSWriteWait = 10 * time.Second
	// 8MiB: twice the daemon's 4MiB per-frame limit, leaving room for the envelope and future fields.
	hostDaemonWSReadLimit = 8 << 20
)

var hostDaemonWSUpgrader = &websocket.Upgrader{
	HandshakeTimeout: 20 * time.Second,
	CheckOrigin: func(r *http.Request) bool {
		return r.Header.Get("Origin") == ""
	},
}

// daemonWSWriter serializes all writes on one WS connection.
type daemonWSWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *daemonWSWriter) writeJSON(frame hostproto.WSFrame) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(hostDaemonWSWriteWait))
	return w.conn.WriteJSON(frame)
}

func (w *daemonWSWriter) writePing() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(hostDaemonWSWriteWait))
	return w.conn.WriteMessage(websocket.PingMessage, nil)
}

// writeClose sends a close frame with an application code (private codes in the 4000 range); the
// daemon decides how to exit based on the code.
func (w *daemonWSWriter) writeClose(code int, text string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(hostDaemonWSWriteWait))
	return w.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, text), time.Now().Add(hostDaemonWSWriteWait))
}

// wsInbound is one message the read loop hands to the main loop (a frame or a read error).
type wsInbound struct {
	frame hostproto.WSFrame
	err   error
}

func (h *Handler) hostDaemonWS(w http.ResponseWriter, r *http.Request) {
	hostRow, ok := hostFromContext(r.Context())
	if !ok {
		// Should not be reachable in theory (hostTokenAuth already blocks it); an explicit exit just in case.
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}

	// Upgrade only after auth is fully done: after upgrading we cannot write an HTTP status code, so
	// the 401 must come before this.
	conn, err := hostDaemonWSUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// For cases like the handshake being rejected by CheckOrigin, gorilla has already replied
		// with an HTTP error itself; only log here.
		h.log.WarnContext(r.Context(), "宿主机 daemon WS 握手失败", slog.Any("err", err))
		return
	}
	defer conn.Close()
	conn.SetReadLimit(hostDaemonWSReadLimit)

	// The first frame must be hello: it carries "connection-level TouchHost + initial cursor"; without
	// it we do not know which rows of which host to claim.
	_ = conn.SetReadDeadline(time.Now().Add(hostDaemonWSFirstHello))
	var first hostproto.WSFrame
	if err := conn.ReadJSON(&first); err != nil || !validHello(&first) {
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(hostproto.WSCloseBadProtocol, "first frame must be hello"),
			time.Now().Add(hostDaemonWSWriteWait))
		h.log.WarnContext(r.Context(), "宿主机 daemon WS 首帧非法", slog.Any("err", err))
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sess, resp, err := h.hostDaemon.BeginStream(ctx, hostRow, *first.Hello, time.Now())
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			// The host was deleted/revoked in the window after handshake auth passed and before the
			// first frame arrived.
			cw := &daemonWSWriter{conn: conn}
			_ = cw.writeJSON(hostproto.WSFrame{Type: hostproto.WSFrameRevoked, V: hostproto.WSProtoVersion})
			_ = cw.writeClose(hostproto.WSCloseRevoked, "host revoked")
			return
		}
		h.log.ErrorContext(ctx, "宿主机 daemon WS 建会话失败", slog.Any("err", err))
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "internal error"),
			time.Now().Add(hostDaemonWSWriteWait))
		return
	}
	defer sess.Close()

	cw := &daemonWSWriter{conn: conn}
	if err := cw.writeJSON(hostproto.WSFrame{
		Type: hostproto.WSFrameHelloAck, V: hostproto.WSProtoVersion, Calls: resp.Calls,
	}); err != nil {
		return
	}

	// After the first frame, settle into the steady state: a pong/any frame pushes the read deadline
	// out by 60s, and the protocol ping checks liveness.
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(hostDaemonWSPongWait))
	})
	_ = conn.SetReadDeadline(time.Now().Add(hostDaemonWSPongWait))

	// Read loop: only moves frames, no business logic.
	inbound := make(chan wsInbound, 16)
	go func() {
		for {
			var frame hostproto.WSFrame
			err := conn.ReadJSON(&frame)
			select {
			case inbound <- wsInbound{frame: frame, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	// Downstream pump: NOTIFY/tick -> claim -> dispatches.
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		h.hostDaemonWSPump(ctx, cw, sess)
	}()

	ping := time.NewTicker(hostDaemonWSPingInterval)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-sess.Done():
			// A newer connection for the same host on this instance kicked us out: tell the daemon to
			// reconnect more slowly, so it does not keep kicking the new connection (the daemon backs
			// off 10s on 4002).
			if sess.Replaced() {
				_ = cw.writeClose(hostproto.WSCloseReplaced, "replaced by newer connection")
			}
			return
		case <-pumpDone:
			// Writing dispatches failed (the peer is not consuming / the connection is broken); just
			// clean up, and the daemon will reconnect or fall back; unacknowledged rows are redelivered
			// through the lease.
			return
		case <-ping.C:
			if err := cw.writePing(); err != nil {
				return
			}
		case msg := <-inbound:
			if msg.err != nil {
				return // EOF / pong timeout / bad JSON means frame boundaries can no longer be realigned; always reconnect
			}
			if !h.handleDaemonWSFrame(ctx, cw, sess, hostRow.ID, msg.frame) {
				return
			}
		}
	}
}

// validHello checks whether the first frame / any hello frame has a valid shape; an unknown version
// is also treated as a protocol error.
func validHello(f *hostproto.WSFrame) bool {
	return f != nil && f.Type == hostproto.WSFrameHello && f.Hello != nil &&
		(f.V == 0 || f.V == hostproto.WSProtoVersion)
}

// hostDaemonWSPump is the downstream pump: block until woken -> claim -> push frames, until ctx ends
// or a write fails.
func (h *Handler) hostDaemonWSPump(ctx context.Context, cw *daemonWSWriter, sess hostsvc.StreamSession) {
	for {
		if !sess.Wait(ctx) {
			return
		}
		calls, err := sess.Claim(ctx, time.Now())
		if err != nil {
			// Do not wait out temporary database errors: Wait has a 20s tick inside, and the next tick
			// retries naturally.
			h.log.WarnContext(ctx, "宿主机 WS 下行认领失败", slog.Any("err", err))
			continue
		}
		if len(calls) == 0 {
			continue
		}
		if err := cw.writeJSON(hostproto.WSFrame{
			Type: hostproto.WSFrameDispatches, V: hostproto.WSProtoVersion, Calls: calls,
		}); err != nil {
			return
		}
	}
}

// handleDaemonWSFrame handles one upstream frame from the daemon; returning false means the
// connection should be wound down.
func (h *Handler) handleDaemonWSFrame(
	ctx context.Context, cw *daemonWSWriter,
	sess hostsvc.StreamSession, hostID string, frame hostproto.WSFrame,
) bool {
	if frame.V != 0 && frame.V != hostproto.WSProtoVersion {
		_ = cw.writeClose(hostproto.WSCloseBadProtocol, "unsupported protocol version")
		return false
	}
	switch frame.Type {
	case hostproto.WSFrameHello:
		if frame.Hello == nil {
			_ = cw.writeClose(hostproto.WSCloseBadProtocol, "hello frame without payload")
			return false
		}
		resp, err := sess.Hello(ctx, *frame.Hello, time.Now())
		if err != nil {
			if errors.Is(err, persistmodel.ErrNotFound) {
				_ = cw.writeJSON(hostproto.WSFrame{Type: hostproto.WSFrameRevoked, V: hostproto.WSProtoVersion})
				_ = cw.writeClose(hostproto.WSCloseRevoked, "host revoked")
				return false
			}
			h.log.WarnContext(ctx, "宿主机 WS hello 续租失败", slog.Any("err", err))
			// Temporary database errors do not close the connection: reply with an empty ack and retry
			// on the next hello tick; the pump is not affected meanwhile.
			resp = hostproto.HeartbeatResponse{}
		}
		if err := cw.writeJSON(hostproto.WSFrame{
			Type: hostproto.WSFrameHelloAck, V: hostproto.WSProtoVersion, Calls: resp.Calls,
			Revoked: resp.Revoked,
		}); err != nil {
			return false
		}
		if resp.Revoked {
			_ = cw.writeClose(hostproto.WSCloseRevoked, "host revoked")
			return false
		}

	case hostproto.WSFrameResults:
		if frame.Results == nil {
			_ = cw.writeClose(hostproto.WSCloseBadProtocol, "results frame without payload")
			return false
		}
		// Same iron rule as POST /results: hostId is taken from the host authenticated by the token.
		req := *frame.Results
		req.HostID = hostID
		ack := hostproto.WSFrame{
			Type: hostproto.WSFrameResultsAck, V: hostproto.WSProtoVersion, ID: frame.ID, OK: true,
		}
		if err := h.hostDaemon.SubmitResults(ctx, req); err != nil {
			h.log.WarnContext(ctx, "宿主机 WS 结果处理失败", slog.Any("err", err))
			// Temporary errors do not close the connection: ok=false makes the daemon put the whole
			// batch back and retry; CAS keeps it idempotent.
			ack.OK = false
			ack.Error = "temporary error, retry"
		}
		if err := cw.writeJSON(ack); err != nil {
			return false
		}

	default:
		// Frame types for the server->client direction sent by the daemon are a protocol error; so
		// are unknown types.
		_ = cw.writeClose(hostproto.WSCloseBadProtocol, "unexpected frame type: "+frame.Type)
		return false
	}
	return true
}
