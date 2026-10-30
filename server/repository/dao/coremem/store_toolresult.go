// In-memory implementation of the self_hosted worker tool-result ledger.
//
// Only used for AMA_STORE=mem (local/e2e/unit tests). Behaves the same as the coredao PG version, but **the ledger
// is in-process and lost on restart**. That is fine for the in-memory Store (it does not span processes anyway).
// Production must use the PG version, which spans instances and survives instance rebuilds; see
// the file header of repository/schema/selfhost_tool_results.sql.
//
// Kept in its own map protected by MemStore's own mu, not in memData/seed (the seed should not carry a runtime ledger).
package coremem

import (
	"context"
	"time"
)

// memToolResult is one in-memory ledger record for a tool call.
type memToolResult struct {
	state       string
	resultEvent []byte
	updatedAt   time.Time
}

// BeginToolResult: see core.Store. Does nothing if it already exists (idempotent).
func (s *MemStore) BeginToolResult(_ context.Context, sessionID, callID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lazyToolResults()
	key := sessionID + "\x00" + callID
	if _, ok := s.toolResults[key]; ok {
		return nil
	}
	s.toolResults[key] = &memToolResult{state: "started", resultEvent: []byte("{}"), updatedAt: time.Now()}
	return nil
}

// MarkToolResultResult: see core.Store.
func (s *MemStore) MarkToolResultResult(_ context.Context, sessionID, callID string, resultEvent []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lazyToolResults()
	key := sessionID + "\x00" + callID
	if r, ok := s.toolResults[key]; ok {
		r.state = "result"
		r.resultEvent = append([]byte(nil), resultEvent...)
		r.updatedAt = time.Now()
	} else {
		s.toolResults[key] = &memToolResult{state: "result", resultEvent: append([]byte(nil), resultEvent...), updatedAt: time.Now()}
	}
	return nil
}

// MarkToolResultSent: see core.Store. Missing also counts as success (idempotent).
func (s *MemStore) MarkToolResultSent(_ context.Context, sessionID, callID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lazyToolResults()
	key := sessionID + "\x00" + callID
	if r, ok := s.toolResults[key]; ok {
		r.state = "sent"
		r.updatedAt = time.Now()
	}
	return nil
}

// GetToolResultState: see core.Store.
func (s *MemStore) GetToolResultState(_ context.Context, sessionID, callID string) (string, []byte, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.toolResults == nil {
		return "", nil, false, nil
	}
	r, ok := s.toolResults[sessionID+"\x00"+callID]
	if !ok {
		return "", nil, false, nil
	}
	return r.state, append([]byte(nil), r.resultEvent...), true, nil
}

// ListToolResultsForRecover: see core.Store. Only returns unsent records of **this session**.
func (s *MemStore) ListToolResultsForRecover(_ context.Context, sessionID string) (
	map[string][]byte, map[string]bool, error,
) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pending := map[string][]byte{}
	sent := map[string]bool{}
	prefix := sessionID + "\x00"
	for key, r := range s.toolResults {
		callID, ok := hasPrefixKey(key, prefix)
		if !ok {
			continue
		}
		switch r.state {
		case "result":
			pending[callID] = append([]byte(nil), r.resultEvent...)
		case "started":
			sent[callID] = true
		}
	}
	return pending, sent, nil
}

// DeleteOldToolResults: see core.Store.
func (s *MemStore) DeleteOldToolResults(_ context.Context, olderThan time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for key, r := range s.toolResults {
		if r.updatedAt.Before(olderThan) {
			delete(s.toolResults, key)
			n++
		}
	}
	return n, nil
}

func (s *MemStore) lazyToolResults() {
	if s.toolResults == nil {
		s.toolResults = map[string]*memToolResult{}
	}
}

func hasPrefixKey(key, prefix string) (string, bool) {
	if len(key) <= len(prefix) || key[:len(prefix)] != prefix {
		return "", false
	}
	return key[len(prefix):], true
}
