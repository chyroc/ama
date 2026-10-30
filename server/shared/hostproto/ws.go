// WebSocket subprotocol messages between ama daemon ↔ amad.
//
// # Communication model (coexists with the three HTTP routes in messages.go)
//
// The daemon prefers WebSocket by default and automatically falls back to HTTP long polling when
// the handshake or keepalive fails (the fallback state machine is in cmd/ama-cli; the server keeps
// both paths forever):
//
//	GET  /api/host-daemon/ws       host_token auth (the Authorization header goes on the HTTP
//	                                handshake); after upgrade, frames are pushed both ways
//	POST /api/host-daemon/heartbeat /results  the old path, kept as is for old daemons and the fallback period
//
// The source of truth for correctness is still the PG host_dispatches table; WS only moves claimed
// rows over faster: after a NOTIFY wakes it, the server pushes dispatches proactively, and the
// daemon's cursor/redelivery/idempotency semantics are exactly the same as on the HTTP path.
//
// # Frame directions
//
//	daemon → amad: hello (every 20s, = the HTTP heartbeat's TouchHost + cursor + snapshot),
//	              results (exec results / terminal frames / acks; same payload shape as POST /results)
//	amad → daemon: hello_ack (reply to hello; may carry the calls claimed this tick),
//	              dispatches (proactive push after a NOTIFY wakeup),
//	              results_ack (whole-frame confirmation of results), revoked (host revoked/deleted)
//
// results must wait for results_ack: exec results are strongly reliable; on daemon timeout or ok=false
// the whole batch goes back to the head of the queue for retry, the same as handling a non-2xx on
// HTTP; the server's CompleteDispatch CAS and AckDispatches' batch set-done are both idempotent, so
// retransmission has no side effects. Terminal frames are weakly reliable, but frames are not
// distinguished at this level: same ack/retry, and losing one only means a few bytes missing on
// screen.
//
// # Keepalive
//
// Each direction ticks every 20s: the daemon sends hello (renewing TouchHost along the way), the
// server sends protocol-level pings. 20s is shorter than the ~30s idle disconnect measured on the WS
// gateway (still up at 28s, EOF at 30s; see the header of httpd/wsprobe.go), and far below the 90s
// host offline threshold.
//
// # Why this file still depends only on the standard library
//
// Same reason as messages.go: hostproto is referenced by both amad and the daemon, and the daemon
// must not drag in the gorilla/Ark/PG stack. The wire shape of frames is plain JSON; gorilla only
// carries it.
package hostproto

// WSProtoVersion is the subprotocol version, carried in a frame's V field. When the server does not
// recognize a higher version it closes with 4000 instead of silently parsing with the old shape.
const WSProtoVersion = 1

// WS frame types (WSFrame.Type).
const (
	// WSFrameHello (C→S): periodic heartbeat; the Hello payload is a HeartbeatRequest.
	WSFrameHello = "hello"
	// WSFrameHelloAck (S→C): reply to hello; Calls are the rows claimed after this TouchHost.
	WSFrameHelloAck = "hello_ack"
	// WSFrameDispatches (S→C): pushed proactively after the server is woken by NOTIFY (or by the fallback tick).
	WSFrameDispatches = "dispatches"
	// WSFrameResults (C→S): upstream results; the Results payload is a ResultsRequest.
	WSFrameResults = "results"
	// WSFrameResultsAck (S→C): whole-frame confirmation of results; with OK=false the daemon retries the whole batch.
	WSFrameResultsAck = "results_ack"
	// WSFrameRevoked (S→C): the host was revoked/deleted while the connection was open; followed by close 4001.
	WSFrameRevoked = "revoked"
)

// Application-level close codes (the private 4000-4999 range of RFC 6455). Standard codes like
// 1000/1001 keep their meaning (normal close/going away), and the daemon treats them as an ordinary
// disconnect and reconnects.
const (
	// WSCloseBadProtocol: a frame cannot be parsed, the version is unsupported, or the frame
	// direction is wrong (for example the daemon sends dispatches). This is "this version of the code
	// speaks differently"; reconnecting is pointless, and the daemon logs an error.
	WSCloseBadProtocol = 4000
	// WSCloseRevoked: the host was revoked or deleted. The daemon wipes the local host_token and
	// exits, taking the same exit as the HTTP heartbeat's 401/Revoked, and tells the user to register
	// again.
	WSCloseRevoked = 4001
	// WSCloseReplaced: a newer connection from the same host kicked out the old one. Normally this
	// only happens when the old connection is a half-open zombie and the new one is already up; the
	// daemon reconnects with a slow backoff and does not fight the new connection.
	WSCloseReplaced = 4002
)

// WSFrame is the single envelope of the WebSocket subprotocol, used in both directions.
//
// It uses "optional payloads that appear per direction" rather than one type per direction: both
// ends share one decode shape, and it avoids an extra {data:...} nesting level. Which fields are
// non-empty for each Type is in the direction table above.
type WSFrame struct {
	// Type takes a WSFrame* constant.
	Type string `json:"type"`
	// V is WSProtoVersion. Frames sent by the daemon always carry it; when missing it is parsed as 1
	// (compatible with old clients during the first version's gradual rollout), and the server
	// replies close 4000 to unsupported versions.
	V int `json:"v,omitempty"`
	// ID is only used for results/results_ack: the daemon assigns each results frame a number that
	// increases monotonically within one connection, the server echoes it in the ack, and the daemon
	// uses it to wake the matching in-flight request. Numbers restarting after a reconnect is fine:
	// the in-flight ack table is reset with the connection too.
	ID int64 `json:"id,omitempty"`

	// Hello is the hello frame payload (C→S), the same shape as the body of POST /heartbeat;
	// hostId is still overwritten by the server with the host authenticated at handshake; whatever
	// the frame claims is not trusted.
	Hello *HeartbeatRequest `json:"hello,omitempty"`

	// Calls is the payload of hello_ack / dispatches (S→C).
	Calls []Dispatch `json:"calls,omitempty"`
	// Revoked true in hello_ack means the host has been revoked (same meaning as the field of the
	// same name in the HTTP heartbeat response), and the daemon takes the wipe-credentials exit; the
	// revoked frame itself carries no business fields.
	Revoked bool `json:"revoked,omitempty"`

	// Results is the results frame payload (C→S), the same shape as the body of POST /results.
	Results *ResultsRequest `json:"results,omitempty"`

	// OK / Error are the results_ack payload (S→C). OK=false (including temporary server errors)
	// does not close the connection; the daemon retries the whole batch as failed.
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}
