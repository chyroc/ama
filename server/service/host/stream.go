// WebSocket streaming session: the domain-layer session object for one daemon WS connection.
//
// # It and Heartbeat are two transport shapes of the same primitives
//
// HTTP long polling (heartbeat.go): one request = touch → claim (if empty, wait up to the 20s cap)
// → write the response. WS streaming session: touch+claim once when the connection is set up, then
//
//	daemon sends hello every 20s   → Stream.Hello: touch + claim (cursor refreshed with hello)
//	server woken by NOTIFY         → pump: Stream.Wait returns → Stream.Claim → push dispatches
//
// Queue behavior (seq cursor, SKIP LOCKED, lease redelivery, CAS done) does not change at all; WS holds no
// state that "only lives on the connection and breaks the feature if lost": when the connection drops, rows already claimed are
// redelivered after the lease expires to the next connection/heartbeat, exactly the same recovery path as when a long-polling connection drops today.
//
// # Connections from the same host are mutually exclusive
//
// The daemon is a single process and normally holds only one connection. When another one arrives in this process (a half-open zombie old connection, or
// a stolen token), the registry uses the new connection to kick out the old one (the old handler receives Done and replies close
// 4002). Across instances (rolling deploys, multiple pods) they cannot see each other, and we deliberately do **not** add a cross-instance lock:
// when both sides Claim concurrently, SKIP LOCKED gives the row to one of them, and rows the other got are redelivered after the lease
// expires (exec 150s / term 30s), so it settles; correctness is still decided by the database.
package host

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/hostproto"
)

// StreamSession is the session interface that one WS connection exposes from the domain layer to the HTTP layer. It is defined in this package, and
// apihandler declares a narrow interface of the same shape itself: Go interfaces match structurally, so neither side references the other.
//
// Concurrency layout: apihandler's read loop calls Hello, the pump goroutine calls Wait/Claim,
// and both may want to Claim at the same time; the implementation serializes with mu and shares the cursor, so the same batch of rows is never sent in two frames.
// Wait does not hold mu (it only registers a waiter / waits); registration and unregistration are cleaned up with the passed ctx.
type StreamSession interface {
	// Hello handles the periodic frame: refresh cursor → touch to renew → non-blocking claim. If the host was deleted it returns
	// an error wrapping persistmodel.ErrNotFound, and the handler replies revoked based on it.
	Hello(ctx context.Context, req hostproto.HeartbeatRequest, now time.Time) (hostproto.HeartbeatResponse, error)
	// Wait blocks until a NOTIFY wake-up or the fallback tick (≤longPollTick); returns false if ctx ends first.
	Wait(ctx context.Context) bool
	// Claim claims a batch without blocking after wake/tick, using the session's current cursor.
	Claim(ctx context.Context, now time.Time) ([]hostproto.Dispatch, error)
	// Done is closed when the session is closed or kicked out by a new connection from the same host.
	Done() <-chan struct{}
	// Replaced reports whether it was kicked out by a newer connection (decides close 4002).
	Replaced() bool
	// Close finishes normally, idempotent; when already replaced it does not wrongly remove the new connection.
	Close()
}

// Stream is the concrete implementation of StreamSession.
type Stream struct {
	s      *Service
	hostID string

	mu      sync.Mutex
	lastSeq int64

	closeOnce sync.Once
	done      chan struct{}
	// replaced is only set by streamRegistry.take when kicking out an old connection; after the handler exits via Done
	// it reads it to decide the close code (4002 instead of a normal disconnect).
	replacedMu sync.Mutex
	replaced   bool
}

// Compile-time check that *Stream satisfies the session interface.
var _ StreamSession = (*Stream)(nil)

// BeginStream starts one WS session: register the connection (kicking out this host's old connection on this instance) →
// first touch to renew → first non-blocking claim.
//
// The returned first batch of calls is for the handler to write hello_ack. When touch reports ErrNotFound (the host was deleted/revoked
// right after the handshake) the error is returned upward; the session has already removed itself from the registry, and the handler replies revoked +
// close 4001.
func (s *Service) BeginStream(
	ctx context.Context, h persistmodel.Host, req hostproto.HeartbeatRequest, now time.Time,
) (StreamSession, hostproto.HeartbeatResponse, error) {
	if req.HostID == "" {
		req.HostID = h.ID
	}
	st := &Stream{s: s, hostID: h.ID, lastSeq: req.LastSeq, done: make(chan struct{})}
	if prev := s.streams.take(h.ID, st); prev != nil {
		s.log.Info("宿主机新 WS 连接踢掉旧连接", slog.String("host", h.ID))
	}

	if err := s.touchFromHeartbeat(ctx, h.ID, req, now); err != nil {
		s.streams.drop(h.ID, st)
		return nil, hostproto.HeartbeatResponse{}, err
	}
	calls, err := s.claimDispatches(ctx, h.ID, req.LastSeq, now)
	if err != nil {
		s.streams.drop(h.ID, st)
		return nil, hostproto.HeartbeatResponse{}, err
	}
	return st, hostproto.HeartbeatResponse{Calls: calls}, nil
}

// Hello handles one periodic heartbeat frame: refresh cursor → touch to renew → non-blocking claim.
// When the host was deleted it returns the underlying ErrNotFound (matched by errors.Is), and the handler replies revoked based on it.
func (st *Stream) Hello(
	ctx context.Context, req hostproto.HeartbeatRequest, now time.Time,
) (hostproto.HeartbeatResponse, error) {
	st.mu.Lock()
	st.lastSeq = req.LastSeq
	st.mu.Unlock()

	if err := st.s.touchFromHeartbeat(ctx, st.hostID, req, now); err != nil {
		return hostproto.HeartbeatResponse{}, err
	}
	calls, err := st.Claim(ctx, now)
	if err != nil {
		return hostproto.HeartbeatResponse{}, err
	}
	return hostproto.HeartbeatResponse{Calls: calls}, nil
}

// Wait blocks until this host's NOTIFY wake-up or one fallback tick (≤longPollTick).
// true means "there may be something new, go Claim"; false means ctx ended first (connection finishing).
func (st *Stream) Wait(ctx context.Context) bool {
	return st.s.waitForDispatch(ctx, st.hostID) == nil
}

// Claim claims a batch without blocking. The cursor is the latest value in the session (from the most recent hello).
func (st *Stream) Claim(ctx context.Context, now time.Time) ([]hostproto.Dispatch, error) {
	st.mu.Lock()
	seq := st.lastSeq
	st.mu.Unlock()
	return st.s.claimDispatches(ctx, st.hostID, seq, now)
}

// Done is closed when the session is closed (normal finish or kicked out by a new connection).
func (st *Stream) Done() <-chan struct{} { return st.done }

// Replaced reports whether this session was kicked out by a newer connection from the same host.
func (st *Stream) Replaced() bool {
	st.replacedMu.Lock()
	defer st.replacedMu.Unlock()
	return st.replaced
}

// Close is the normal finish: only when the registry still holds itself does it remove itself and close Done.
// Idempotent; if already replaced by a newer connection, it does nothing (the old connection must not remove the new connection's ownership).
func (st *Stream) Close() {
	st.s.streams.drop(st.hostID, st)
}

func (st *Stream) shutdown() {
	st.closeOnce.Do(func() { close(st.done) })
}

func (st *Stream) markReplaced() {
	st.replacedMu.Lock()
	st.replaced = true
	st.replacedMu.Unlock()
	st.shutdown()
}

// streamRegistry is this process's registry of "host → current WS session".
//
// It is deliberately volatile in-process state: it only decides "should the old connection be kicked out", and takes no part in delivery correctness
// (the source of truth for delivery is PG). After an instance is rebuilt the table is empty, and even if old connections linger they settle through leases,
// which fits "in-process only holds things whose loss only makes it slower".
type streamRegistry struct {
	mu     sync.Mutex
	active map[string]*Stream
}

func newStreamRegistry() *streamRegistry {
	return &streamRegistry{active: make(map[string]*Stream)}
}

// take registers a new session, and returns and kicks out this host's old session (nil if none).
func (r *streamRegistry) take(hostID string, st *Stream) *Stream {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.active[hostID]
	r.active[hostID] = st
	if prev != nil {
		prev.markReplaced()
	}
	return prev
}

// drop removes and closes only when the current registry entry is st; otherwise (already replaced by a new connection) it does nothing.
func (r *streamRegistry) drop(hostID string, st *Stream) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active[hostID] == st {
		delete(r.active, hostID)
	}
	st.shutdown()
}
