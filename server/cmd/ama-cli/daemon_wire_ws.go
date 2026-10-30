// WebSocket transport: the daemon prefers it by default.
//
// # Connection shape
//
//	dial (Authorization: Bearer host_token in the handshake header) → immediately send hello and
//	wait for hello_ack (the first frame does TouchHost + the first claim at the same time) → stay connected:
//	  - this goroutine is the read loop: hello_ack/dispatches go to the downstream queue, results_ack
//	    wakes in-flight submissions, revoked/close are classified and returned;
//	  - one hello ticker goroutine sends a frame every 20s (lease renewal + holding off the
//	    gateway's 30s idle disconnect);
//	  - submitResults may be called concurrently by the flush loop; writes are serialized by
//	    writeMu, and the ack map waits for replies.
//
// # Proxy
//
// ⚠️ gorilla's zero-value websocket.Dialer{Proxy: nil} **does not use a proxy**; only
// DefaultDialer presets ProxyFromEnvironment (confirmed in the v1.5.0 source). The daemon runs on
// users' machines/company networks, so it must be set explicitly, matching the Clone() of
// newDaemonHTTPClient on the HTTP side; when missing, the symptom is a dial timeout that looks like
// "WS does not work", so every user gets pushed to the fallback path, with no error pointing at the
// real cause.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/chyroc/ama/server/shared/hostproto"
	"github.com/gorilla/websocket"
)

const (
	wsDialTimeout     = 15 * time.Second
	wsHelloAckWait    = 25 * time.Second
	wsHelloInterval   = 20 * time.Second
	wsWriteTimeout    = 10 * time.Second
	wsAckTimeout      = 30 * time.Second
	wsReadLimit       = 8 << 20
	wsResultsMaxBytes = 4 << 20
)

// Classified return values of serve. The supervisor uses them to tell apart "never connected",
// "died after connecting", "the server does not support it at all", "revoked" and "kicked out by a
// newer connection", each with its own backoff/fallback strategy.
var (
	errWSUnauthorized = errors.New("ws: 握手被拒（401）")
	errWSUnsupported  = errors.New("ws: 服务端或网关不支持（404/405/426/协议不符）")
	errWSDial         = errors.New("ws: 拨号或首帧失败")
	errWSReplaced     = errors.New("ws: 被同主机的新连接顶掉")
	errWSDropped      = errors.New("ws: 连接已建立后中断")
	// errWSResultNack is "the connection is fine, and the server explicitly replied ok=false to a
	// results frame". Kept separate from connection-level errors: in this case it must never switch to
	// resending via the HTTP fallback (a live WS means the path works, and a temporary server error
	// should be retried over WS), otherwise the same result would arrive once via each path.
	errWSResultNack = errors.New("ws: 服务端 results_ack ok=false")
)

type wsWire struct {
	rt  *daemonRuntime
	url string

	conn    *websocket.Conn
	writeMu sync.Mutex

	closeOnce sync.Once
	closed    chan struct{}

	ackMu  sync.Mutex
	nextID int64
	acks   map[int64]chan error

	// established is when the first hello_ack succeeded; zero means never connected. The supervisor
	// uses it to tell a dial failure from a short-lived connection, and to judge "lived past 60s".
	established time.Time
}

func newWSWire(rt *daemonRuntime, url string) *wsWire {
	return &wsWire{rt: rt, url: url, closed: make(chan struct{}), acks: make(map[int64]chan error)}
}

func (w *wsWire) name() string { return transportNameWS }

func (w *wsWire) isClosed() bool {
	select {
	case <-w.closed:
		return true
	default:
		return false
	}
}

func (w *wsWire) shutdown() {
	w.closeOnce.Do(func() {
		close(w.closed)
	})
}

func (w *wsWire) close() error {
	// Try to send a normal close frame, then close the connection; the read loop's close wait is covered by closed.
	_ = w.writeControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"))
	if w.conn != nil {
		_ = w.conn.Close()
	}
	return nil
}

func (w *wsWire) writeControl(messageType int, data []byte) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if w.conn == nil {
		return errWSDropped
	}
	_ = w.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return w.conn.WriteControl(messageType, data, time.Now().Add(wsWriteTimeout))
}

func (w *wsWire) writeJSON(frame hostproto.WSFrame) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if w.conn == nil {
		return errWSDropped
	}
	_ = w.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	return w.conn.WriteJSON(frame)
}

// dialWS dials + does the first hello round trip. See errWS* above for failure classification; on
// success it returns the calls carried by the first hello_ack (the caller is responsible for handing
// them to the worker; the re-probe upgrade path must hand them over too).
func (w *wsWire) dialWS(ctx context.Context) ([]hostproto.Dispatch, error) {
	dialer := &websocket.Dialer{
		// See the file header: the proxy must be set explicitly; a zero-value Dialer does not use HTTP_PROXY.
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: wsDialTimeout,
	}
	dialCtx, cancel := context.WithTimeout(ctx, wsDialTimeout)
	defer cancel()
	conn, resp, err := dialer.DialContext(dialCtx, w.url, http.Header{
		"Authorization": []string{"Bearer " + w.rt.cfg.Token},
	})
	if err != nil {
		if resp != nil {
			// When the handshake fails, gorilla does not close resp.Body for us (it only hands over
			// conn on success); without closing, repeated reconnects leak connections/FDs. Close it right
			// after reading, then classify the error by status code.
			_ = resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				return nil, errWSUnauthorized
			case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUpgradeRequired:
				return nil, errWSUnsupported
			}
		}
		return nil, errors.Join(errWSDial, err)
	}
	w.conn = conn
	conn.SetReadLimit(wsReadLimit)

	// First frame hello + wait for hello_ack (25s). Only when this succeeds does the "connection established".
	hello := w.rt.heartbeatRequest()
	if err := w.writeJSON(hostproto.WSFrame{
		Type: hostproto.WSFrameHello, V: hostproto.WSProtoVersion, Hello: &hello,
	}); err != nil {
		_ = conn.Close()
		return nil, errors.Join(errWSDial, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(wsHelloAckWait))
	var first hostproto.WSFrame
	if err := conn.ReadJSON(&first); err != nil {
		_ = conn.Close()
		return nil, errors.Join(errWSDial, err)
	}
	if first.Type == hostproto.WSFrameRevoked {
		_ = conn.Close()
		return nil, errWSUnauthorized
	}
	if first.Type != hostproto.WSFrameHelloAck {
		_ = w.closeNow(hostproto.WSCloseBadProtocol, "expected hello_ack")
		return nil, errWSUnsupported
	}
	w.established = time.Now()
	// Read deadline once connected: keepalive frames/any frame push it back to 50s (more than twice the 20s hello tick).
	_ = conn.SetReadDeadline(time.Now().Add(wsAckTimeout + wsHelloInterval))
	return first.Calls, nil
}

func (w *wsWire) closeNow(code int, text string) error {
	_ = w.writeControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, text))
	if w.conn != nil {
		return w.conn.Close()
	}
	return nil
}

// serve completes one full connection lifecycle (dial → stay connected sending and receiving → disconnect, classify and return).
func (w *wsWire) serve(ctx context.Context, in chan<- inboundBatch) error {
	firstCalls, err := w.dialWS(ctx)
	if err != nil {
		return err
	}
	return w.runConnected(ctx, in, firstCalls)
}

// runConnected is the connected part after a successful dial. In HTTP mode, after a successful
// re-probe, the supervisor calls it directly to upgrade the new connection to the active transport,
// with no need to disconnect and dial again.
func (w *wsWire) runConnected(ctx context.Context, in chan<- inboundBatch, firstCalls []hostproto.Dispatch) error {
	// hello ticker: one frame every 20s, lease renewal + keepalive. The ticker must start before the
	// defer is registered; the close order depends on it: shutdown (close w.closed; the ticker gets the
	// signal at once) → wait for the ticker goroutine to exit → close the connection. The other way
	// round (wait for the ticker first, then shutdown) would waste a 20s tick after a disconnect.
	tickerDone := make(chan struct{})
	go func() {
		defer close(tickerDone)
		w.helloLoop(ctx)
	}()
	defer func() {
		w.shutdown()
		<-tickerDone
		_ = w.conn.Close()
	}()

	// The first hello_ack may already carry the first calls (read while dialing); hand them over before entering the read loop.
	if len(firstCalls) > 0 {
		if err := w.deliver(ctx, in, inboundBatch{calls: firstCalls}); err != nil {
			return err
		}
	}

	readDeadline := wsAckTimeout + wsHelloInterval
	for {
		var frame hostproto.WSFrame
		_ = w.conn.SetReadDeadline(time.Now().Add(readDeadline))
		err := w.conn.ReadJSON(&frame)
		if err != nil {
			return classifyWSClose(err)
		}
		// Any normal frame pushes back the read deadline (application-level hello_ack/results_ack count as keepalive too).
		switch frame.Type {
		case hostproto.WSFrameHelloAck, hostproto.WSFrameDispatches:
			if frame.Revoked {
				return errWSUnauthorized
			}
			if len(frame.Calls) > 0 {
				if err := w.deliver(ctx, in, inboundBatch{calls: frame.Calls}); err != nil {
					return err
				}
			}
		case hostproto.WSFrameResultsAck:
			w.deliverAck(frame)
		case hostproto.WSFrameRevoked:
			return errWSUnauthorized
		default:
			// The server should not send anything else; if it does, the two sides speak different versions, so do not blindly reconnect.
			_ = w.closeNow(hostproto.WSCloseBadProtocol, "unexpected frame")
			return errWSUnsupported
		}
	}
}

// probe makes one attempt at "can a WS be established" in HTTP fallback mode: dial + first frame round
// trip. On success it does not tear down the connection but returns the wire (already established),
// and the supervisor switches the active transport and starts runConnected; on failure it returns a
// classified error. It uses its own short ctx, so it does not slow down the main HTTP path.
func probeWS(ctx context.Context, rt *daemonRuntime, url string) (*wsWire, []hostproto.Dispatch, error) {
	w := newWSWire(rt, url)
	firstCalls, err := w.dialWS(ctx)
	if err != nil {
		if w.conn != nil {
			_ = w.conn.Close()
		}
		return nil, nil, err
	}
	return w, firstCalls, nil
}

// classifyWSClose translates a read error into an exit classification. gorilla's CloseError carries the application code.
func classifyWSClose(err error) error {
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		switch ce.Code {
		case hostproto.WSCloseRevoked:
			return errWSUnauthorized
		case hostproto.WSCloseReplaced:
			return errWSReplaced
		case hostproto.WSCloseBadProtocol:
			return errWSUnsupported
		}
	}
	return errors.Join(errWSDropped, err)
}

// helloLoop sends a hello frame every 20s; on a write failure it closes the connection and shuts
// down, so the read loop and in-flight submitResults get the exit signal immediately (conn.Close alone
// is not enough: then shutdown would wait for runConnected's defer after the read loop returns, and
// in-flight submits would wait out the full 30s ack timeout).
func (w *wsWire) helloLoop(ctx context.Context) {
	t := time.NewTicker(wsHelloInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.closed:
			return
		case <-t.C:
			hello := w.rt.heartbeatRequest()
			if err := w.writeJSON(hostproto.WSFrame{
				Type: hostproto.WSFrameHello, V: hostproto.WSProtoVersion, Hello: &hello,
			}); err != nil {
				w.shutdown()
				if w.conn != nil {
					_ = w.conn.Close()
				}
				return
			}
		}
	}
}

func (w *wsWire) deliver(ctx context.Context, out chan<- inboundBatch, batch inboundBatch) error {
	select {
	case out <- batch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-w.closed:
		return errWSDropped
	}
}

func (w *wsWire) deliverAck(frame hostproto.WSFrame) {
	w.ackMu.Lock()
	ch := w.acks[frame.ID]
	delete(w.acks, frame.ID)
	w.ackMu.Unlock()
	if ch == nil {
		return // a late ack after reconnect/timeout; nobody is waiting
	}
	if frame.OK {
		ch <- nil
	} else {
		ch <- errors.Join(errWSResultNack, errors.New(frame.Error))
	}
}

// submitResults sends one results frame and waits for the whole-frame ack. If none arrives within
// 30s (or the connection is dead) it returns an error, and the upper layer puts this block back and
// retries; CAS guarantees a retransmission does not overwrite the first result.
func (w *wsWire) submitResults(ctx context.Context, req hostproto.ResultsRequest) error {
	if w.isClosed() {
		return errWSDropped
	}
	req.HostID = w.rt.cfg.HostID

	w.ackMu.Lock()
	w.nextID++
	id := w.nextID
	ch := make(chan error, 1)
	w.acks[id] = ch
	w.ackMu.Unlock()
	defer func() {
		w.ackMu.Lock()
		delete(w.acks, id)
		w.ackMu.Unlock()
	}()

	if err := w.writeJSON(hostproto.WSFrame{
		Type: hostproto.WSFrameResults, V: hostproto.WSProtoVersion, ID: id, Results: &req,
	}); err != nil {
		return err
	}

	timer := time.NewTimer(wsAckTimeout)
	defer timer.Stop()
	select {
	case err := <-ch:
		return err
	case <-timer.C:
		log.Printf("WS results_ack 超时（id=%d），按失败重试", id)
		return errors.New("ws: results_ack 超时")
	case <-w.closed:
		return errWSDropped
	case <-ctx.Done():
		return ctx.Err()
	}
}
