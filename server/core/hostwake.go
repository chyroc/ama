// HostWakes: in-process fan-out of cross-instance wakeups for host dispatch.
//
// # Where it sits in the whole chain
//
// The host daemon only sends outbound HTTP: the heartbeat is a 20s long poll, and an exec tool
// call has to wait for the daemon to return a result. Neither of these waiters may blindly poll
// PG. The wakeup signal comes from PostgreSQL's `pg_notify('ama_host_wake', host_id)` (sent in
// the same transaction that inserts a dispatch / writes a result / fails the calls of a host that
// went offline). A dedicated LISTEN connection in cmd/amad receives it and then calls Wake.
//
//	another instance writes a result → PG NOTIFY → this instance's LISTEN connection → Wake(hostID)
//	                                          → every goroutine on this machine waiting for that host wakes up
//
// # Why there is one fixed NOTIFY channel with host_id in the payload
//
// Each LISTEN holds a **dedicated** PG connection (it cannot use the pool; see how pgmq is wired
// in cmd/amad/termrelay.go). One channel per host would make the connection count grow with the
// number of hosts. The notify payload allows 8000 bytes and a channel name only 63 characters;
// host_id is safe in both. So every instance LISTENs only on `ama_host_wake`, and this file
// dispatches the signal to waiters by host_id.
//
// # ⚠️ This only makes things "faster"; it is not a correctness part
//
// NOTIFY can be lost (LISTEN reconnect window, transaction rollback boundaries), so:
//   - the daemon heartbeat long poll always returns within 20s, and re-reads the DB anyway when it does;
//   - exec waiters also have a 1s ticker as a fallback;
//   - HostWatcher also actively fails pending calls of offline hosts and sends a notify.
//
// This struct is pure in-process state. By the classification in CLAUDE.md it is a cache where
// "losing it only makes things slower". It carries no correctness.
package core

import (
	"context"
	"sync"
)

// HostWakes is the registry of waiters on this machine. Implementations must be safe for
// concurrent use.
type HostWakes interface {
	// Wait registers a wait on a host and returns a wakeup channel:
	// the channel is closed when the host is woken by Wake; it is unregistered automatically when
	// ctx ends (to avoid goroutine leaks).
	Wait(ctx context.Context, hostID string) <-chan struct{}
	// Wake wakes every goroutine on this machine waiting for the host (pending heartbeats, exec
	// result waits).
	Wake(hostID string)
}

type hostWakeEntry struct {
	// Each waiter has its own chan; closing it is the broadcast. buffered=1 is not for passing
	// values, it only lets Wake not care whether there is a receiver.
	ch chan struct{}
}

type hostWakes struct {
	mu      sync.Mutex
	waiters map[string]map[*hostWakeEntry]struct{}
}

// NewHostWakes creates an in-process wakeup registry (one per amad instance).
func NewHostWakes() HostWakes {
	return &hostWakes{waiters: make(map[string]map[*hostWakeEntry]struct{})}
}

func (w *hostWakes) Wait(ctx context.Context, hostID string) <-chan struct{} {
	e := &hostWakeEntry{ch: make(chan struct{})}

	w.mu.Lock()
	set := w.waiters[hostID]
	if set == nil {
		set = make(map[*hostWakeEntry]struct{})
		w.waiters[hostID] = set
	}
	set[e] = struct{}{}
	w.mu.Unlock()

	go func() {
		<-ctx.Done()
		w.mu.Lock()
		if set := w.waiters[hostID]; set != nil {
			delete(set, e)
			if len(set) == 0 {
				delete(w.waiters, hostID)
			}
		}
		w.mu.Unlock()
		// Do not close e.ch: closing is only used for the "real wakeup" signal. When ctx ends
		// the waiter leaves on its own. Closing an unregistered channel that the waiter is
		// selecting on gains nothing, and would force us to tell two kinds of wakeup apart.
	}()
	return e.ch
}

func (w *hostWakes) Wake(hostID string) {
	w.mu.Lock()
	set := w.waiters[hostID]
	delete(w.waiters, hostID)
	w.mu.Unlock()
	for e := range set {
		close(e.ch)
	}
}
