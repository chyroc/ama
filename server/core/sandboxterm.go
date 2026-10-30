// The webshell terminal channel: browser <-SSE/POST-> amad <-WebSocket-> the real PTY in the
// container.
//
// # Why the browser side is not WebSocket
//
// ⚠️ **The production gateway uses HTTP/2, and WebSocket cannot run over HTTP/2.**
//
// This conclusion was measured on the real system, not inferred. Sending a standard WS handshake
// straight to production got `HTTP/2 401` (the gateway answered with h2), while the browser's real
// request got 400, and the server log said:
//
//	'upgrade' token not found in 'Connection' header
//
// h2 has no `Connection: Upgrade` mechanism (RFC 7540 explicitly bans hop-by-hop headers), so the
// gateway drops the browser's upgrade headers, and by the time the request reaches amad it is no
// longer a valid WS handshake.
//
// **Why it works locally**: `./dev.sh` serves HTTP/1.1, and upgrade headers pass through
// unchanged. So this failure **only appears in production** and can never be caught locally. It is
// the same kind of problem as the "all green on one machine, intermittent in production" described
// at the top of CLAUDE.md, except this time the dividing line is the HTTP version.
//
// Switch to **SSE downstream + POST upstream**: both are plain HTTP requests with no problem under
// h2, and this repo already runs that path (the chat page's event stream is SSE).
//
// # But the amad -> container leg is still WebSocket
//
// The container side has only one PTY channel, `/v1/shell/ws`, so there is no choice. **That is
// fine**: it is a connection amad opens itself with Go's HTTP/1.1 client, and it does not go through
// the h2 gateway.
//
//	browser  --SSE(down)-->  amad  --WebSocket-->  container PTY
//	         --POST(up)-->
//
// So vim/top, Ctrl-C, Tab completion **all still work**: the real PTY is on the container side,
// and the browser side just uses a different way to carry the bytes.
//
// # How the session is matched across two requests
//
// SSE and POST are **two independent HTTP requests**, so something must bind them to the same PTY.
// An in-process `terminalHub` does it: when SSE is set up it generates a `termID` and sends it as
// the first frame, and later POSTs carry it to find the matching connection.
//
// # ⚠️ An in-process table misses under multiple instances; this was once a real failure
//
// This used to say "deliberately accepted: losing it only means reopening the terminal". **That
// judgment was wrong**, and production logs laid out the cost: the load balancer sends **every
// single keystroke** independently to a random instance, while the PTY's WS is held only by the
// instance that opened the SSE. One session's requests looked like this:
//
//	instance A (holds SSE)   POST -> 204 × 7
//	instance B (does not)    POST -> 404 × 8
//
// The hit rate is about 1/number of instances, **and the frontend showed no hint at the time**:
// users were typing blind, and one person reported many failures within a minute. "Losing it only
// means reopening" underestimated it: users did not know they had to reopen; they only saw that
// what they typed never appeared.
//
// # Fix: write directly on this instance, else relay via a queue to the connection holder
//
// Why it has to be this way: both simpler paths are blocked by the platform (measured on the real
// system; the probes are kept in the repo):
//
//   - Let each instance open its own PTY connection -> the platform allows only one WS, and the
//     second one gets
//     `Session already has an active WebSocket connection` (cmd/probe-term-reattach);
//   - Send upstream through the stateless `/v1/shell/write` -> returns `404 Session not found`,
//     because its id space is not shared with `/v1/shell/ws` (cmd/probe-term-write).
//
// So keystrokes go into a table that every instance can see (shared/infra/pgmq) and are consumed
// by the instance holding the connection. Steady-state latency is set by LISTEN/NOTIFY, measured at
// p50 0.08ms (cmd/probe-term-relay); when the request lands on this instance it **does not touch the
// database at all** and is as fast as before the fix.
package core

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/shared/hostproto"
	"github.com/gorilla/websocket"
)

// terminalIdleTimeout is the idle limit for a terminal connection.
//
// When it is reached the connection is closed, so a user who leaves with the page open does not
// leave a long connection hanging (each one holds a goroutine and a connection to the container).
// 30 minutes is shorter than the container TTL (1 hour) and much longer than a normal pause for
// thought.
const terminalIdleTimeout = 30 * time.Minute

// terminalKeepAlive is how long the fact "the terminal is still in use" stays valid.
//
// # What it prevents
//
// The container is paused after **a chat round has been idle for 60 seconds** (the idle logic in
// shared/infra/dockertools/session.go), and that check only looks at the chat; it has no idea
// someone has a webshell open and is typing commands in the container. So a user halfway through an
// edit in `vim`, or watching logs with `tail -f`, gets the container frozen: pausing freezes every
// process in the container (cgroup freezer), with no hint on screen; output just stops.
//
// Every terminal activity pushes this moment later (see the two call sites of touchTerminal), and
// before pausing we check once; if it has not expired yet, this round is skipped.
//
// # Why 5 minutes
//
// The lower bound comes from "how long without activity counts as gone": terminal use is
// **intermittent**. Reading a screen of output, thinking a bit, then typing another command, with a
// minute or two of silence in between, is normal. Too short (e.g. 60 seconds, the same as chat
// idle) would decide the user left while they are still reading output, which is the same as doing
// nothing.
//
// The upper bound comes from "do not waste resources": extending too long lets someone who closed
// the page keep holding the host's CPU and memory. 5 minutes sits between "the person is still
// here" and "do not waste resources", and it also matches the duration users explicitly asked for.
//
// ⚠️ **This is not a life extension.** A container idle longer than containerTTL is still deleted
// by the reclaim sweep (based on the activity ledger; see "reclaim sweep" in
// shared/infra/dockertools/README.md). Pausing and reclaiming are two independent axes.
// What this buys is "do not freeze", not "live longer".
const terminalKeepAlive = 5 * time.Minute

// terminalSession is one live terminal.
//
// Two forms:
//
//   - Container terminal: upstream is non-nil; write writes to the container terminal, close
//     closes it (the SSE fallback path);
//   - Host terminal: upstream is nil, browser is non-nil; write writes to the browser WS
//     (the daemon's output is delivered via DeliverHostTermFrame), close only closes the done
//     signal.
type terminalSession struct {
	upstream TerminalConn
	// When browser is non-nil, write goes to it (host terminal); otherwise to upstream (container
	// terminal).
	browser terminalWriter
	// writeMu serializes writes to that connection.
	//
	// **It is required**: gorilla's Conn does not support concurrent writes, and writes come from
	// several goroutines (one per POST in the SSE era; daemon output and ping concurrently in the host
	// era). Without the lock, two frames colliding would corrupt the connection, showing up as "the
	// terminal disconnected for no reason".
	writeMu sync.Mutex
	// done is closed when this terminal ends.
	done chan struct{}
	once sync.Once
}

// terminalWriter is the minimal interface for "write one frame to the browser terminal
// connection"; *clientWriter satisfies it.
type terminalWriter interface {
	write(mt int, data []byte) error
}

// close shuts things down idempotently.
func (t *terminalSession) close() {
	t.once.Do(func() {
		close(t.done)
		// A host terminal has no upstream container connection (upstream is nil); closing the browser WS
		// is handled by the defer when the streamHostTerminalWS main loop exits via done, so Close must not
		// be called on nil here.
		if t.upstream != nil {
			_ = t.upstream.Close()
		}
	})
}

// write sends one frame and is safe for concurrent use. A host terminal sends to the browser, a
// container terminal sends to the container.
func (t *terminalSession) write(payload []byte) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if t.browser != nil {
		return t.browser.write(websocket.TextMessage, payload)
	}
	return t.upstream.WriteMessage(websocket.TextMessage, payload)
}

// doneCh returns the end signal channel.
func (t *terminalSession) doneCh() <-chan struct{} { return t.done }

// terminalHub keeps live terminals by termID. In-process is enough; see the file header for why.
type terminalHub struct {
	mu    sync.Mutex
	items map[string]*terminalSession
	// runs records "which run this terminal belongs to", **only for authorization**.
	//
	// It is a separate table from items instead of a field on terminalSession: on the cross-instance
	// path we want to answer the ownership question even when "this instance does not have the
	// terminal". The current implementation cannot answer it (the binding is also in this instance's
	// memory), but keeping them separate makes the boundary explicit, so nobody mistakes "found the
	// session" for "found the ownership".
	runs map[string]string
}

func (h *terminalHub) add(id string, s *terminalSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.items == nil {
		h.items = map[string]*terminalSession{}
	}
	h.items[id] = s
}

func (h *terminalHub) get(id string) *terminalSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.items[id]
}

func (h *terminalHub) drop(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.items, id)
}

// bind records which run this terminal belongs to.
func (h *terminalHub) bind(termID, runID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.runs == nil {
		h.runs = map[string]string{}
	}
	h.runs[termID] = runID
}

func (h *terminalHub) unbind(termID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.runs, termID)
}

// runOf returns which run this terminal belongs to; a false second return value means this
// instance does not know (the terminal is on another instance, or has already ended).
//
// ⚠️ Callers must tell "do not know" apart from "does not match": "do not know" must not be
// treated as an authorization rejection, or every normal cross-instance keystroke would be blocked,
// which is exactly the failure being fixed here.
func (h *terminalHub) runOf(termID string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	runID, ok := h.runs[termID]
	return runID, ok
}

// relayHandler builds the handler for "what to do after receiving a relayed keystroke".
//
// ⚠️ **It is pulled out on its own to be testable**. When it was inlined in
// StreamRunSandboxTerminal, testing it needed a real container + a real PTY WebSocket, so this code
// **had no tests at all**, even though it carries the **only** ownership check on the
// cross-instance path. Measured: deleting the whole f.RunID check below left every case green. A
// security check without tests is the same as none.
//
// write is passed in by the caller (the real path is terminalSession.write), so tests can plug in
// a recorder without starting a WebSocket.
//
// # Why both undecodable and mismatched return nil
//
// Returning an error would leave the message in the queue and **redeliver it again and again**
// after the lease expires. But in both cases redelivery gives the same result every time: a frame
// of undecodable JSON will not fix itself, and a keystroke that does not belong to this terminal
// will not suddenly start belonging to it. So just drop it and keep one log line.
func (s *Service) relayHandler(runID string, write func([]byte) error) func(context.Context, RelayMessage) error {
	return func(_ context.Context, m RelayMessage) error {
		var f relayFrame
		if uerr := json.Unmarshal(m.Payload, &f); uerr != nil {
			s.log().Warn("终端中转收到解不开的帧，已丢弃",
				slog.String("run", runID), slog.String("err", uerr.Error()))
			return nil
		}
		// ⚠️ The ownership check is on this end: **only here do we know who this terminal belongs to**.
		// The publishing side (the instance that received the POST) has no binding and cannot check.
		// Without this line, an attacker could fill in someone else's terminalId, have the request land on
		// any instance that does not hold the terminal, and write commands into someone else's container,
		// and which instance it lands on is decided randomly by the load balancer.
		if f.RunID != runID {
			s.log().Warn("终端中转收到不属于本终端的帧，已丢弃",
				slog.String("run", runID), slog.String("claimed", f.RunID))
			return nil
		}
		return write(f.Payload)
	}
}

// StreamRunSandboxTerminal opens a terminal: it connects to the container, then pushes the
// container's output to the browser over SSE.
//
// **Authorization is already done in the HTTP layer** (authz's ActionManage; see matrix.go).
//
// Returning an error means "SSE never even opened", and the caller returns a plain HTTP error; once
// streaming starts, any problem can only be told to the frontend as an event frame.
func (s *Service) StreamRunSandboxTerminal(w http.ResponseWriter, r *http.Request, runID string) error {
	ctx := r.Context()

	// ⚠️ **Assert Flusher first, before writing any response headers.**
	// If it is not supported, the wiring is broken (the logging middleware did not pass Flush through).
	// That is a real error, not a fallback: a silent fallback would leave the terminal spinning forever,
	// and that symptom points nowhere near here.
	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("服务端不支持流式响应")
	}

	// Host runs do not support the SSE fallback: a host terminal has only the WS browser link. Return
	// a clear 426/precondition error here instead of going on to dial a container (which would give a
	// misleading "container does not exist" error).
	runRec, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if runRec.HostID != "" {
		return fmt.Errorf("%w: 宿主机终端仅支持 WebSocket，请使用 /sandbox/terminal/ws",
			ErrPrecondition)
	}

	// First confirm the container is available, **before opening the stream**: that way the frontend
	// gets a structured {code,message} instead of a vague error frame.
	state, err := s.RunSandboxState(ctx, runID)
	if err != nil {
		return err
	}
	if state.Status != SandboxStatusRunning {
		return fmt.Errorf("%w: 容器当前不可用（状态：%s），请先恢复容器",
			ErrPrecondition, state.Status)
	}

	rec, err := s.store.GetRunSandbox(ctx, runID)
	if err != nil {
		return err
	}
	sess, err := s.attachSandbox(ctx, runID, rec.SandboxID)
	if err != nil {
		return err
	}

	// Audit: who, when, and which container's terminal was connected. **Logged before connecting**.
	s.log().Info("webshell 终端连接",
		slog.String("actor", auth.ActorFrom(ctx)),
		slog.String("run", runID),
		slog.String("container", rec.SessionID))

	upstream, err := sess.OpenTerminal(ctx)
	if err != nil {
		s.log().Error("连接容器终端失败",
			slog.String("run", runID), slog.String("err", err.Error()))
		return errors.New("连接容器终端失败，请稍后重试")
	}

	sessTerm := &terminalSession{upstream: upstream, done: make(chan struct{})}
	termID := newTerminalID()
	s.terminals().add(termID, sessTerm)
	defer func() {
		s.terminals().drop(termID)
		sessTerm.close()
	}()

	// Record which run this terminal belongs to, **for authorization**.
	//
	// ⚠️ Without it there is a privilege-escalation hole: the write endpoint's auth looks at the runId in
	// the URL, but what actually decides "which container gets written" is the terminalId in the body,
	// and the two used to be unrelated. So someone could pass auth with a run they have access to, put
	// someone else's terminalId in the body, and write commands into a container in another workspace.
	// With this table, WriteRunSandboxTerminal can verify that the two really are a pair.
	s.terminals().bind(termID, runID)
	defer s.terminals().unbind(termID)

	// Start the relay consumer: keystrokes received by other instances are sent to this topic.
	//
	// ⚠️ **It must start here**, because only this instance holds the WebSocket for this PTY. The
	// platform explicitly allows only one WS (measured: a second one gets `Session already has an active
	// WebSocket connection`), so "whoever holds the connection consumes" is not a choice; it is the only
	// workable split.
	//
	// Its lifetime follows this SSE: when ctx ends the consumer stops, leaving no orphan goroutine.
	if s.termRelay != nil {
		relayCtx, stopRelay := context.WithCancel(ctx)
		defer stopRelay()
		go func() {
			err := s.termRelay.Consume(relayCtx, terminalTopic(termID),
				func(_ context.Context, m RelayMessage) error {
					var f relayFrame
					if uerr := json.Unmarshal(m.Payload, &f); uerr != nil {
						// If it cannot be decoded, drop it. **Return nil, not an error**: an error would leave it in
						// the queue to be redelivered again and again, and an undecodable message stays undecodable no
						// matter how many times it is redelivered.
						s.log().Warn("终端中转收到解不开的帧，已丢弃",
							slog.String("run", runID), slog.String("err", uerr.Error()))
						return nil
					}
					// ⚠️ The ownership check is on this end: only here do we know who this terminal belongs to.
					// If it does not match, drop it (also return nil, no redelivery).
					if f.RunID != runID {
						s.log().Warn("终端中转收到不属于本终端的帧，已丢弃",
							slog.String("run", runID), slog.String("claimed", f.RunID))
						return nil
					}
					return sessTerm.write(f.Payload)
				})
			if err != nil && relayCtx.Err() == nil {
				s.log().Warn("终端中转消费退出", slog.String("run", runID), slog.String("err", err.Error()))
			}
		}()
	}

	// Headers match the chat SSE (including X-Accel-Buffering, to stop the gateway from buffering).
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// The first frame sends termID; the frontend uses it to POST input. **It must be the first
	// frame**: the frontend must not send any keystrokes before receiving it.
	writeSSEEvent(w, flusher, "ready", map[string]string{"terminalId": termID})

	// Extend once as soon as the terminal opens; do not wait for the first keystroke.
	//
	// ⚠️ **This one cannot be skipped.** Many uses are "terminal open, only watching, not typing":
	// watching logs with `tail -f`, waiting for a build to finish, reading the long output of the last
	// command. Those cases have no keystrokes at all; relying only on the extension in
	// WriteRunSandboxTerminal would leave all of them unprotected, and they are exactly the ones most
	// hurt by a frozen container (after freezing, output just stops, with no hint at all).
	s.touchTerminal(ctx, runID)

	// Container -> browser: reading runs in its own goroutine; the main loop only handles heartbeat
	// and exit.
	frames := make(chan []byte, 64)
	go func() {
		defer close(frames)
		for {
			_ = upstream.SetReadDeadline(time.Now().Add(terminalIdleTimeout))
			_, data, rerr := upstream.ReadMessage()
			if rerr != nil {
				return
			}
			select {
			case frames <- data:
			case <-sessTerm.done:
				return
			}
		}
	}()

	// Heartbeat: same as the chat SSE, a comment line every 15 seconds, so middle layers do not cut
	// the idle connection.
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// The user closed the page / switched away.
			return nil
		case <-ticker.C:
			if _, werr := fmt.Fprint(w, ": heartbeat\n\n"); werr != nil {
				return nil
			}
			flusher.Flush()
			// The heartbeat also serves as the extension.
			//
			// **This is the main thing for "terminal open, only watching, not typing" uses** (tail -f,
			// waiting for a build, reading long output): they have no keystrokes at all, but the SSE stays
			// connected, so this 15-second heartbeat is the only thing still moving. The extension lasts 5
			// minutes and the heartbeat fires every 15 seconds, so missing a few does not leave a gap.
			//
			// ⚠️ It comes **after** a successful write: a failed write means the browser is gone (the next
			// line returns), and extending then would just waste resources on a terminal nobody is watching.
			s.touchTerminal(ctx, runID)
		case data, alive := <-frames:
			if !alive {
				// The container side disconnected; tell the frontend, then shut down.
				writeSSEEvent(w, flusher, "closed", map[string]string{})
				return nil
			}
			// **Forward the container's JSON frames unchanged**, without parsing: the protocol is defined by
			// the platform, and if it adds a frame type we should not have to change our code.
			if _, werr := fmt.Fprintf(w, "data: %s\n\n", data); werr != nil {
				return nil
			}
			flusher.Flush()
		}
	}
}

// WriteRunSandboxTerminal sends one frame of browser input into the container.
//
// # Two paths: write directly here, or relay via a queue to the connection holder
//
// The terminal's downstream SSE and every upstream keystroke are **independent HTTP requests**,
// and the load balancer sends each one independently to a random instance. So when this method is
// called, this process **most likely does not have** the terminal. In production, more than half of
// one session's keystrokes landed on other instances (returning 404, users typing blind).
//
//	this process has the terminal -> write to the WS directly (fast path, measured p50 0.00ms,
//	                                  as fast as before the fix)
//	it does not                    -> put it in the queue for the instance holding the
//	                                  connection (measured p50 0.08ms)
//
// ⚠️ **Why each instance cannot open its own connection**: the platform allows only one WebSocket
// per PTY, and a second one gets `Session already has an active WebSocket connection`; and the
// stateless `/v1/shell/write` cannot write into the PTY (the id space is not shared, it returns 404).
// Both were measured on the real system; see cmd/probe-term-reattach and cmd/probe-term-write.
//
// # runID is not decoration; it is half of the authorization
//
// ⚠️ The caller's auth (authz's ActionManage) checks the **runId in the URL**, but what decides
// "which container gets written" is the **terminalId in the body**. Without checking that the two
// are a pair, someone could pass auth with a run they have access to, fill in someone else's
// terminalId, and write into a container in another workspace.
// So it must be compared here, **on both paths**: comparing only on the fast path means an
// unauthorized request just lands on another instance and gets around it.
func (s *Service) WriteRunSandboxTerminal(
	ctx context.Context, runID, terminalID string, payload []byte,
) error {
	// ⚠️ **The extension comes first, before the hub lookup.**
	//
	// This POST landing on an instance without the terminal is **the normal case** (see the file
	// header: in production more than half of the keystrokes landed on the wrong instance). But "a
	// keystroke reached here" is itself enough to prove **someone is using this terminal**, a fact
	// unrelated to whether this instance holds that WS.
	//
	// If it came after the hub lookup, exactly the requests that need it most (the ones that landed on
	// the wrong instance) would never extend, so the feature would be badly weakened in production,
	// and nobody would notice.
	s.touchTerminal(ctx, runID)

	if strings.TrimSpace(terminalID) == "" {
		return fmt.Errorf("%w: 缺少终端 id", ErrInvalidArgument)
	}

	// This instance holds the terminal: check ownership, then write directly.
	if t := s.terminals().get(terminalID); t != nil {
		// Host terminal input only accepts browser WS frames (handleHostBrowserFrame -> sent to the
		// term_input queue); there is no "POST upstream" channel. A host session is marked by browser
		// being non-nil. Reject explicitly here: the POST input route is still ActionManage, so if allowed,
		// an admin could bypass the rule "only the owner can open a personal direct terminal": they cannot
		// open the WS but could still write keystrokes into it.
		if t.browser != nil {
			return ErrNotFound
		}
		if owner, ok := s.terminals().runOf(terminalID); ok && owner != runID {
			// Return 404 instead of 403: do not confirm that "this terminalId exists", or this endpoint
			// becomes a way to probe other people's terminal ids.
			return ErrNotFound
		}
		if err := t.write(payload); err != nil {
			t.close()
			return fmt.Errorf("写入终端失败: %w", err)
		}
		return nil
	}

	// This instance does not have it. With no relay configured we can only return 404 (the pre-fix
	// behavior; with a single instance this is never reached).
	if s.termRelay == nil {
		return ErrNotFound
	}

	// ⚠️ On the cross-instance path **ownership cannot be checked**: the binding is in the memory of
	// the instance holding the connection. So the runID is sent along, and that end compares it before
	// writing into the PTY (see the consumer logic in StreamRunSandboxTerminal). The check always stays
	// on "the end that knows the truth", so it does not vary with which instance the request lands on.
	frame, err := json.Marshal(relayFrame{RunID: runID, Payload: payload})
	if err != nil {
		return fmt.Errorf("打包终端输入失败: %w", err)
	}
	if err := s.termRelay.Publish(ctx, terminalTopic(terminalID), frame, terminalInputTTL); err != nil {
		return fmt.Errorf("转发终端输入失败: %w", err)
	}
	return nil
}

// relayFrame is the envelope for cross-instance delivery: one keystroke frame + which run it
// claims to belong to.
//
// The runID is carried so that **the end holding the connection** can check ownership; only that
// end has the binding.
type relayFrame struct {
	RunID string `json:"runId"`
	// HostOutput=true: this is an **output** frame reported by the host daemon, to be written to the
	// browser (direction daemon->browser). The holding end must scrub secrets with scrubShellOutput
	// before writing, and skips the runID ownership comparison (the sender does not hold the
	// termID->runID binding).
	HostOutput bool   `json:"hostOutput,omitempty"`
	Payload    []byte `json:"payload"`
}

// terminalInputTTL is how long one keystroke frame stays valid.
//
// 30 seconds: **keystrokes are strongly time-sensitive**. Flooding into the container after piling
// up is harmful: after reconnecting, users would see a string of characters typed half a minute ago
// suddenly executed. Better dropped than late.
const terminalInputTTL = 30 * time.Second

// terminalTopic is a terminal's topic in the queue.
func terminalTopic(terminalID string) string { return "term:" + terminalID }

// newTerminalID generates a terminal id.
//
// ⚠️ **Do not use s.nextID**: that is an "in-process counter" (service.go literally says "the ID
// only needs to be unique within the process"), while termID is now a **cross-instance** queue key.
// If two instances each counted up from a millisecond timestamp, a collision would only be a matter
// of time, and a collision means sending keystrokes into someone else's terminal.
//
// Same approach as arkWorkerID (core/worker.go): a random suffix guarantees no collision.
func newTerminalID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// If random bytes cannot be obtained, fall back to a timestamp. **Do not panic**: a terminal
		// connection should not fail to open because of this, and in practice this path never happens
		// (crypto/rand failing means the system already has big problems).
		return fmt.Sprintf("term_%d", time.Now().UnixNano())
	}
	return "term_" + hex.EncodeToString(b[:])
}

// touchTerminal records "this Run's terminal was in use just now", pushing the pause back by
// terminalKeepAlive.
//
// **Failures are only logged at Debug, not returned**: the cost of a failed extension is that the
// container may be paused while the user still has the terminal open, which is a degraded
// experience; turning it into an error would make **the keystroke itself fail**, which is much
// worse. It is the same trade-off as RecordSession's "a bookkeeping failure must not fail the tool
// call".
//
// Debug rather than Warn: this runs on **every keystroke**, so with Warn one database hiccup would
// flood dozens of log lines and bury the real problems.
func (s *Service) touchTerminal(ctx context.Context, runID string) {
	if runID == "" {
		return
	}
	if err := s.store.TouchRunSandboxTerminal(ctx, runID, terminalKeepAlive); err != nil {
		s.log().Debug("续期终端占用失败，容器可能在使用中被暂停",
			slog.String("run", runID), slog.String("err", err.Error()))
	}
}

// terminals returns this Service's terminal table, created lazily.
func (s *Service) terminals() *terminalHub {
	s.termOnce.Do(func() { s.termHub = &terminalHub{} })
	return s.termHub
}

// writeSSEEvent sends one SSE frame with an event name.
func writeSSEEvent(w http.ResponseWriter, f http.Flusher, event string, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body)
	f.Flush()
}

// DeliverHostTermFrame delivers one frame of **terminal output reported by the host daemon** (data
// direction: daemon -> amad -> browser).
//
// It is the opposite direction from WriteRunSandboxTerminal (browser -> container). If this process
// holds the terminalID (SSE/WS opened in this process), it scrubs and writes directly; otherwise it
// sends it via pgmq to the holding instance, where hostOutputHandler scrubs and writes. Terminal
// output may contain plaintext tokens printed by `env`, so it must go through the same scrub as
// webshell.
// hostBrowserFrame is one frame of host terminal data written to the browser. Output frames use
// DataB64 (raw PTY bytes, decoded with atob in the browser); closed/error use Data (a text reason).
type hostBrowserFrame struct {
	Type    string `json:"type"`
	Data    string `json:"data,omitempty"`
	DataB64 string `json:"dataB64,omitempty"`
}

// DeliverHostTermFrame handles one frame of host terminal data reported by the daemon.
//
// When dataB64 is non-empty (output frame): base64-decode the raw PTY bytes -> scrub them with this
// session's credentials (the same scrubbing as webshell / the six tools) -> base64 again. **It stays
// b64 all the way into the browser frame**, so it never goes through the JSON UTF-8 output check on
// Go strings, and invalid bytes are not replaced with �. closed/error frames (data text) take the
// old path.
func (s *Service) DeliverHostTermFrame(ctx context.Context, terminalID, frameType, data, dataB64 string) error {
	runID, _ := s.terminals().runOf(terminalID)

	var browserPayload []byte
	if dataB64 != "" {
		raw, err := base64.StdEncoding.DecodeString(dataB64)
		if err != nil {
			// Broken b64 cannot be delivered, so drop it (a weakly reliable frame); do not fall back to data,
			// so base64 text is not treated as output.
			s.log().Warn("宿主机终端 output 帧 base64 非法，已丢弃",
				slog.String("term", terminalID), slog.Any("err", err))
			return nil
		}
		cleaned := s.scrubHostBytes(ctx, runID, raw)
		browserPayload, _ = json.Marshal(hostBrowserFrame{
			Type:    frameType,
			DataB64: base64.StdEncoding.EncodeToString(cleaned),
		})
	} else {
		var err error
		browserPayload, err = json.Marshal(hostBrowserFrame{Type: frameType, Data: data})
		if err != nil {
			return err
		}
	}

	if t := s.terminals().get(terminalID); t != nil {
		if werr := t.write(browserPayload); werr != nil {
			return werr
		}
		// After writing a terminal frame (shell exited / open failed) to the browser, actively shut down:
		// the browser WS, the relay consumer, and the hub binding are all released by closing this session.
		// Without that, after the shell exits the WS would hang until the browser closes it itself (the
		// host path has no 30-minute read timeout safety net like the container version).
		// sync.Once keeps this idempotent with the readLoop / browser-close paths.
		if frameType == hostproto.TermFrameClosed || frameType == hostproto.TermFrameError {
			t.close()
		}
		return nil
	}
	if s.termRelay == nil {
		return ErrNotFound
	}
	// Cross-instance relay: browserPayload is already in final form (scrubbed, b64), so the holding end
	// writes it directly without decoding/scrubbing again.
	frame, err := json.Marshal(relayFrame{HostOutput: true, Payload: browserPayload})
	if err != nil {
		return err
	}
	return s.termRelay.Publish(ctx, terminalTopic(terminalID), frame, terminalInputTTL)
}

// scrubHostBytes scrubs credentials from raw PTY bytes. scrubShellOutput takes a string, but the
// scrubber works by substring replacement internally, so string(raw) is used as a working copy (a
// Go string can hold any bytes and is not rewritten here; only the json.Marshal output would
// rewrite them, and our output goes through b64). After scrubbing, convert back to bytes.
func (s *Service) scrubHostBytes(ctx context.Context, runID string, raw []byte) []byte {
	return []byte(s.scrubShellOutput(ctx, runID, string(raw)))
}

// hostOutputHandler consumes relayed host output frames.
//
// The relayed Payload is already the **final browser frame** built by the delivering instance
// (output decoded, scrubbed, and re-encoded as b64). Here we only recognize the envelope and write it
// unchanged; we must not scrub again (that would match secrets against the JSON frame itself as if it
// were command output).
func (s *Service) hostOutputHandler(runID string, write func([]byte) error) func(context.Context, RelayMessage) error {
	return func(_ context.Context, m RelayMessage) error {
		var f relayFrame
		if err := json.Unmarshal(m.Payload, &f); err != nil {
			s.log().Warn("宿主机终端输出中转帧解不开，已丢弃", slog.Any("err", err))
			return nil
		}
		if !f.HostOutput {
			return nil
		}
		return write(f.Payload)
	}
}

// addHostBrowserSession registers a terminal of the "host -> browser" form: no container upstream,
// only writes out to the browser WS. Used by streamHostTerminalWS.
func (h *terminalHub) addHostBrowserSession(id string, w terminalWriter) *terminalSession {
	t := &terminalSession{
		browser: w,
		done:    make(chan struct{}),
	}
	h.add(id, t)
	return t
}
