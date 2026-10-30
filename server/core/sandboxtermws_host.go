// Web terminal WebSocket channel for the host executor: browser <-WS-> amad <-long-poll
// queue-> the ama daemon on the user's machine (real PTY).
//
// It uses the same browser-side protocol, the same URL and the same one-time ticket as the
// container version (StreamRunSandboxTerminalWS → dial the container WS); all the differences
// are behind amad: the container version dials an upstream WS, while the host version writes
// frames into the host_dispatches queue for the daemon to pull. The daemon's output comes back
// via /api/host-daemon/results to DeliverHostTermFrame, and terminalHub writes it back here.
//
// So this file holds no direct connection to the host: the browser WS is wrapped as a
// terminalSession and registered in terminalHub (the container version's SSE does the same),
// and output delivery fully reuses the existing "direct write on this instance / pgmq
// cross-instance relay" path.
package core

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// HostTerminalDispatcher is the port for sending terminal frames to the host queue
// (host.Service satisfies it). A narrow interface declared in core avoids a reverse dependency
// on service/host; it is injected by a setter during wiring.
type HostTerminalDispatcher interface {
	// HostOnline reports whether the host's heartbeat is currently fresh (within 90s).
	HostOnline(ctx context.Context, hostID string) (bool, error)
	// EnqueueTermFrame sends one terminal action (term_open/input/resize/close).
	EnqueueTermFrame(ctx context.Context, hostID, sessionID, terminalID, kind string, payload []byte) error
	// WorkspaceHostBound re-checks that the host is still bound to the given workspace (used
	// before taking the session snapshot, so existing agents do not keep sending to that
	// machine after it is unbound).
	WorkspaceHostBound(ctx context.Context, workspaceID, hostID string) (bool, error)
	// HostExists re-checks that a personally connected host row still exists (a personal
	// machine is not bound through a workspace, and stops working once its owner revokes it).
	// ErrNotFound becomes (false,nil).
	HostExists(ctx context.Context, hostID string) (bool, error)
	// HostOwnerEmail gets the host owner: the terminal ticket for a personally connected run is
	// only issued to the owner.
	HostOwnerEmail(ctx context.Context, hostID string) (string, error)
}

// SetHostTerminalDispatcher injects the ability to send host terminal frames. When it is nil,
// terminal connections for host runs get a clear 503 before the handshake.
func (s *Service) SetHostTerminalDispatcher(d HostTerminalDispatcher) {
	s.hostTerms = d
}

// browserTerminalFrame is a browser → amad frame (same protocol as the container WS).
type browserTerminalFrame struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// streamHostTerminalWS takes over the WS after the ticket and run checks pass and the run is
// confirmed to belong to a host.
//
// It shares the upgrader / clientWriter / ping pace / ticket check with the container version;
// it just has no upstream container connection behind it.
func (s *Service) streamHostTerminalWS(w http.ResponseWriter, r *http.Request, runID, hostID string) {
	ctx := r.Context()
	if s.hostTerms == nil {
		http.Error(w, "服务端未启用宿主机终端", http.StatusServiceUnavailable)
		return
	}
	online, err := s.hostTerms.HostOnline(ctx, hostID)
	if err != nil {
		http.Error(w, "读取宿主机状态失败", http.StatusBadGateway)
		return
	}
	if !online {
		http.Error(w, "宿主机当前离线（ama daemon 心跳超时），请确认它正在运行",
			http.StatusConflict)
		return
	}

	conn, err := s.terminalWSUpgrader().Upgrade(w, r, nil)
	if err != nil {
		s.log().Warn("宿主机终端 WS 升级失败",
			slog.String("run", runID), slog.String("err", err.Error()))
		return
	}
	cw := &clientWriter{conn: conn}
	defer func() { _ = conn.Close() }()

	termID := newTerminalID()
	// When the connection ends (pod restart / gateway drop / disconnect / stream open failure),
	// tell the daemon to close the PTY, best effort.
	// ⚠️ Cannot use the request ctx (it is already canceled at defer time); use a separate
	// short-timeout context. Do not block exit; on failure leave it to the daemon side's
	// redelivery / fallback cleanup. The browser-closed-the-page case is already sent in the
	// loop; sending again here is idempotent (the daemon treats close on a closed terminal as a
	// no-op).
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.hostTerms.EnqueueTermFrame(closeCtx, hostID, runID, termID, "term_close", nil)
	}()
	// Register in terminalHub: output frames reported by the daemon (DeliverHostTermFrame) use
	// this to find the browser connection; across instances they are relayed to this process
	// via pgmq.
	sess := s.terminals().addHostBrowserSession(termID, cw)
	s.terminals().bind(termID, runID)
	defer func() {
		s.terminals().drop(termID)
		s.terminals().unbind(termID)
	}()

	s.log().Info("宿主机网页终端连接（WS）",
		slog.String("run", runID), slog.String("host", hostID), slog.String("term", termID))

	// Cross-instance output consumption: the daemon's results request may land on **another
	// instance**, which publishes the PTY output to term:<termID>; only this instance, which
	// holds the browser WS, receives it by subscribing.
	// ⚠️ Same reason as the container SSE path, it must be started here. If it is missing, in a
	// multi-instance deployment the web terminal intermittently shows no output (direct write on
	// a single instance always works, so it only shows up in production). The handler already
	// includes scrubbing and host output frame recognition (hostOutputHandler).
	if s.termRelay != nil {
		relayCtx, stopRelay := context.WithCancel(ctx)
		defer stopRelay()
		go func() {
			if err := s.termRelay.Consume(relayCtx, terminalTopic(termID),
				s.hostOutputHandlerWithClose(runID, sess)); err != nil {
				s.log().Warn("宿主机终端中继消费退出",
					slog.String("term", termID), slog.String("err", err.Error()))
			}
		}()
	}

	// Send term_open. env is fetched now by arkSessionID (== runID), same snapshot behavior as
	// opening a shell in a container.
	openPayload, _ := json.Marshal(map[string]any{
		"cols": 120,
		"rows": 30,
		"cwd":  ".ama/runs/" + runID,
		"env":  s.hostTerminalEnv(ctx, runID),
	})
	if err := s.hostTerms.EnqueueTermFrame(ctx, hostID, runID, termID, "term_open", openPayload); err != nil {
		s.writeTerminalWSError(cw, "打开宿主机终端失败: "+err.Error())
		return
	}
	s.touchTerminal(ctx, runID)

	ping := time.NewTicker(terminalWSPingInterval)
	defer ping.Stop()
	in := make(chan browserTerminalFrame, 16)
	go func() {
		defer close(in)
		for {
			// No read timeout on the browser side (silence is normal; the ping keeps the gateway
			// from idling out, same as the container version).
			_ = conn.SetReadDeadline(time.Time{})
			mt, data, err := conn.ReadMessage()
			if err != nil || mt != websocket.TextMessage {
				return
			}
			var f browserTerminalFrame
			if json.Unmarshal(data, &f) != nil {
				continue // A bad frame does not kill the connection.
			}
			select {
			case in <- f:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-in:
			if !ok {
				// Browser closed the page: tell the daemon to close the PTY, then exit normally.
				s.enqueueHostTermOrLog(ctx, hostID, runID, termID, "term_close", nil)
				return
			}
			if !s.handleHostBrowserFrame(ctx, hostID, runID, termID, f) {
				return
			}
		case <-ping.C:
			if err := cw.write(websocket.PingMessage, nil); err != nil {
				return
			}
			s.touchTerminal(ctx, runID)
			// Actively check that the host is online; if it dropped, wrap up instead of leaving
			// the browser hanging.
			if online, err := s.hostTerms.HostOnline(ctx, hostID); err == nil && !online {
				s.writeTerminalWSError(cw, "宿主机连接已断开（daemon 心跳超时）")
				return
			}
		case <-sess.done:
			// The daemon reported closed (shell exited); the delivery side already wrote the
			// closed frame.
			return
		}
	}
}

// handleHostBrowserFrame turns one browser input frame into a queue action. Returning false
// means end the connection.
func (s *Service) handleHostBrowserFrame(
	ctx context.Context, hostID, runID, termID string, f browserTerminalFrame,
) bool {
	switch f.Type {
	case "input":
		// The browser frame is {"type":"input","data":"ls"}, so f.Data is a **bare JSON string**
		// ("ls"); but the daemon's term_input wants an object {"data":"ls"}. Passing the bare
		// string through makes the daemon's Unmarshal fail and every keystroke is silently
		// dropped. So wrap it here.
		payload, perr := json.Marshal(termInputEnvelope{Data: f.Data})
		if perr != nil {
			return true
		}
		return s.enqueueHostTermOrLog(ctx, hostID, runID, termID, "term_input", payload)
	case "resize":
		// Data is already a {cols,rows} object; pass it through to the daemon's term_resize.
		return s.enqueueHostTermOrLog(ctx, hostID, runID, termID, "term_resize", f.Data)
	default:
		// Ignore unknown frame types (forward compatible: when a new frame type is added later,
		// an old server should not drop the terminal).
		return true
	}
}

func (s *Service) enqueueHostTermOrLog(
	ctx context.Context, hostID, runID, termID, kind string, payload []byte,
) bool {
	if err := s.hostTerms.EnqueueTermFrame(ctx, hostID, runID, termID, kind, payload); err != nil {
		s.log().Warn("下发宿主机终端帧失败",
			slog.String("host", hostID), slog.String("kind", kind), slog.String("err", err.Error()))
		return false
	}
	return true
}

// hostTerminalEnv collects the env snapshot to inject when opening the shell (session env).
//
// Same source and same session id (== runID) as the six tools: ama must work in the terminal,
// and user-defined env must take effect too. If it is not wired or fails, inject nothing; never
// block the terminal.
func (s *Service) hostTerminalEnv(ctx context.Context, runID string) map[string]string {
	envs := make(map[string]string)
	if s.shellEnvs != nil {
		if ev, _, _, err := s.shellEnvs.SessionEnvs(ctx, runID); err == nil {
			for k, v := range ev {
				envs[k] = v
			}
		}
	}
	return envs
}

// termInputEnvelope wraps the browser's bare string input into the object the daemon's
// term_input wants.
type termInputEnvelope struct {
	// Data is still json.RawMessage: on the browser side it is already a string literal (with
	// quotes), and marshaling a RawMessage field embeds it as is, giving {"data":"ls"}.
	Data json.RawMessage `json:"data"`
}

// hostOutputHandlerWithClose is the relay consumer used by the host WS itself: after scrubbing
// secrets and writing to the browser, on a closed/error final frame it must also close the
// session (triggering <-sess.done so the main loop exits).
// It does the same thing as the DeliverHostTermFrame direct-write path on this instance; both
// places must wrap up.
func (s *Service) hostOutputHandlerWithClose(runID string, sess *terminalSession,
) func(context.Context, RelayMessage) error {
	base := s.hostOutputHandler(runID, sess.write)
	return func(ctx context.Context, m RelayMessage) error {
		if err := base(ctx, m); err != nil {
			return err
		}
		// Decode the frame type to see if it is final (base already decoded it once; decoding
		// again here costs next to nothing, and base's signature is not changed for this, so
		// SSE/WS keep sharing most of the logic).
		var f relayFrame
		if json.Unmarshal(m.Payload, &f) != nil {
			return nil
		}
		var tf struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(f.Payload, &tf) == nil &&
			(tf.Type == "closed" || tf.Type == "error") {
			sess.close()
		}
		return nil
	}
}
