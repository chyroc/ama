package channel

import (
	"context"
	"strings"
	"sync"
	"time"
)

// MemStore is the in-memory Store, for tests and AMA_STORE=mem (one local process only).
type MemStore struct {
	mu      sync.Mutex
	inbound map[string]InboundMessage
	scopes  map[ScopeKey]Scope
}

var _ Store = (*MemStore)(nil)

// NewMemStore creates an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{inbound: map[string]InboundMessage{}, scopes: map[ScopeKey]Scope{}}
}

func inboundKey(channel Kind, accountID, messageID string) string {
	return string(channel) + "\x00" + accountID + "\x00" + messageID
}

// ClaimInbound implements Store.
func (m *MemStore) ClaimInbound(_ context.Context, msg InboundMessage) (bool, error) {
	if strings.TrimSpace(msg.MessageID) == "" || strings.TrimSpace(msg.AccountID) == "" {
		return false, ErrInvalidArgument
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := inboundKey(msg.Channel, msg.AccountID, msg.MessageID)
	if old, ok := m.inbound[k]; ok && old.Status != InboundFailed {
		return false, nil
	}
	msg.Status = InboundPending
	m.inbound[k] = msg
	return true, nil
}

// MarkInbound implements Store.
func (m *MemStore) MarkInbound(_ context.Context, channel Kind, accountID, messageID string, status InboundStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := inboundKey(channel, accountID, messageID)
	msg, ok := m.inbound[k]
	if !ok {
		return nil
	}
	msg.Status = status
	m.inbound[k] = msg
	return nil
}

// GetScope implements Store.
func (m *MemStore) GetScope(_ context.Context, key ScopeKey) (Scope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.scopes[key]
	if !ok {
		return Scope{}, ErrScopeNotBound
	}
	return s, nil
}

// BindScope implements Store.
func (m *MemStore) BindScope(_ context.Context, scope Scope) (Scope, bool, error) {
	if scope.Key == "" || scope.RunID == "" {
		return Scope{}, false, ErrInvalidArgument
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.scopes[scope.ScopeKey]; ok {
		return old, false, nil
	}
	m.scopes[scope.ScopeKey] = scope
	return scope, true, nil
}

// TouchScope implements Store.
func (m *MemStore) TouchScope(_ context.Context, key ScopeKey, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.scopes[key]
	if !ok {
		return ErrScopeNotBound
	}
	s.LastUsedAt = now
	s.TurnCount++
	m.scopes[key] = s
	return nil
}

// ResetScope implements Store.
func (m *MemStore) ResetScope(_ context.Context, channel Kind, accountID, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.scopes {
		if k.Channel == channel && k.AccountID == accountID && k.Key == key {
			delete(m.scopes, k)
		}
	}
	return nil
}
