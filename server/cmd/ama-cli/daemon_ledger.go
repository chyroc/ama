// execLedger: a bounded dedup ledger of completed calls (in process only, not persisted).
//
// # Why it is needed
//
// The redelivery rule of host_dispatches is "if delivered and the claim lease has expired (150s for
// exec / same lease for tool), deliver again". WS disconnects and reconnects often, and rerunning a
// bash/write with side effects would cause real duplicate side effects; the ledger dedupes by call
// id: a redelivery only returns the cached result and never reruns.
//
// # What happens if it is lost
//
// The ledger only guards against "redelivery within a short time". After a daemon restart or ledger
// eviction, an old call received again is still rerun once; that is the same as the existing crash
// recovery semantics (the server's CAS only accepts the first result, and the agent never gets two
// outputs). After a process crash there is no way to know exactly "where it got to last time", and
// rerunning is the only safe fallback, so the ledger is deliberately not persisted.
//
// # Shared by exec and tool
//
// Both kinds of result are a piece of JSON, and the ledger stores the raw bytes uniformly: the caller
// marshals it before remember, and on redelivery it goes through the matching return channel by
// dispatch kind, sending the bytes as is.
package main

import (
	"sync"
)

// The bound. Normally a result is forgotten right after its ACK, so the map is almost empty; it only
// piles up when the transport is down for a long time.
// var rather than const: the eviction tests need to make it smaller temporarily.
var (
	ledgerMaxItems = 256
	ledgerMaxBytes = 32 << 20
)

type execLedger struct {
	mu    sync.Mutex
	items map[string][]byte
	// order is insertion order (not cleaned up on forget; entries are removed along the way during over-capacity eviction).
	order []string
	bytes int
}

func newExecLedger() *execLedger {
	return &execLedger{items: make(map[string][]byte)}
}

// remember records the raw JSON of a completed result. Remembering the same callID again does not
// overwrite the first (belt and braces: the same call runs only once on the daemon side).
func (l *execLedger) remember(callID string, raw []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.items[callID]; exists {
		return
	}
	l.items[callID] = raw
	l.order = append(l.order, callID)
	l.bytes += len(raw)
	l.evictLocked()
}

func (l *execLedger) lookup(callID string) ([]byte, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	raw, ok := l.items[callID]
	return raw, ok
}

// forget is called after the server confirms it received the result (it can never be redelivered after that, so there is no point keeping it).
func (l *execLedger) forget(callIDs ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range callIDs {
		if raw, ok := l.items[id]; ok {
			l.bytes -= len(raw)
			delete(l.items, id)
		}
	}
	if l.bytes < 0 {
		l.bytes = 0
	}
}

// evictLocked evicts from the oldest records when over capacity. The holes forget leaves in order
// are compacted and rebuilt along the way here.
func (l *execLedger) evictLocked() {
	if len(l.items) <= ledgerMaxItems && l.bytes <= ledgerMaxBytes {
		return
	}
	alive := l.order[:0]
	for _, id := range l.order {
		raw, ok := l.items[id]
		if !ok {
			continue // a hole left by forget
		}
		if len(l.items) > ledgerMaxItems || l.bytes > ledgerMaxBytes {
			l.bytes -= len(raw)
			delete(l.items, id)
			continue
		}
		alive = append(alive, id)
	}
	l.order = alive
}
