package weixin

import (
	"context"
	"encoding/base32"
	"sort"
	"sync"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
)

var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// MemStore is the in-memory Store, for tests and AMA_STORE=mem (one local process only).
type MemStore struct {
	mu       sync.Mutex
	accounts map[string]AccountRecord
	logins   map[string]LoginRecord
}

var _ Store = (*MemStore)(nil)

// NewMemStore creates an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{accounts: map[string]AccountRecord{}, logins: map[string]LoginRecord{}}
}

// UpsertAccount implements Store.
func (m *MemStore) UpsertAccount(_ context.Context, rec AccountRecord) (AccountRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, old := range m.accounts {
		if old.BotID != rec.BotID {
			continue
		}
		if old.WorkspaceID != rec.WorkspaceID {
			return AccountRecord{}, ErrBotInOtherSpace
		}
		old.BotToken, old.BaseURL, old.OwnerUserID = rec.BotToken, rec.BaseURL, rec.OwnerUserID
		old.Status, old.LastError, old.UpdatedAt = apimodel.WeixinAccountActive, "", rec.UpdatedAt
		m.accounts[id] = old
		return old, nil
	}
	if rec.Status == "" {
		rec.Status = apimodel.WeixinAccountActive
	}
	m.accounts[rec.ID] = rec
	return rec, nil
}

// GetAccount implements Store.
func (m *MemStore) GetAccount(_ context.Context, id string) (AccountRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[id]
	if !ok {
		return AccountRecord{}, ErrAccountNotFound
	}
	return a, nil
}

func (m *MemStore) list(keep func(AccountRecord) bool) []AccountRecord {
	out := []AccountRecord{}
	for _, a := range m.accounts {
		if keep(a) {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ListAccounts implements Store.
func (m *MemStore) ListAccounts(_ context.Context, workspaceID string) ([]AccountRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.list(func(a AccountRecord) bool { return a.WorkspaceID == workspaceID }), nil
}

// ListActiveAccounts implements Store.
func (m *MemStore) ListActiveAccounts(_ context.Context) ([]AccountRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.list(func(a AccountRecord) bool { return a.Status != apimodel.WeixinAccountExpired }), nil
}

func (m *MemStore) update(id string, f func(*AccountRecord)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[id]
	if !ok {
		return ErrAccountNotFound
	}
	f(&a)
	m.accounts[id] = a
	return nil
}

// SetAccountAgent implements Store.
func (m *MemStore) SetAccountAgent(_ context.Context, id, agentID, now string) error {
	return m.update(id, func(a *AccountRecord) { a.AgentID, a.UpdatedAt = agentID, now })
}

// SetAccountCursor implements Store.
func (m *MemStore) SetAccountCursor(_ context.Context, id, cursor string) error {
	return m.update(id, func(a *AccountRecord) { a.SyncCursor = cursor })
}

// SetAccountStatus implements Store.
func (m *MemStore) SetAccountStatus(
	_ context.Context, id string, status apimodel.WeixinAccountStatus, lastError, now string,
) error {
	return m.update(id, func(a *AccountRecord) { a.Status, a.LastError, a.UpdatedAt = status, lastError, now })
}

// TouchAccountInbound implements Store.
func (m *MemStore) TouchAccountInbound(_ context.Context, id, at string) error {
	return m.update(id, func(a *AccountRecord) { a.LastInboundAt = at })
}

// DeleteAccount implements Store.
func (m *MemStore) DeleteAccount(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.accounts[id]; !ok {
		return ErrAccountNotFound
	}
	delete(m.accounts, id)
	return nil
}

// RecentBotTokens implements Store.
func (m *MemStore) RecentBotTokens(_ context.Context, n int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := m.list(func(a AccountRecord) bool { return a.BotToken != "" })
	out := []string{}
	for i := len(all) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, all[i].BotToken)
	}
	return out, nil
}

// CreateLogin implements Store.
func (m *MemStore) CreateLogin(_ context.Context, rec LoginRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logins[rec.ID] = rec
	return nil
}

// GetLogin implements Store.
func (m *MemStore) GetLogin(_ context.Context, id string) (LoginRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.logins[id]
	if !ok {
		return LoginRecord{}, ErrLoginNotFound
	}
	return l, nil
}

// UpdateLogin implements Store.
func (m *MemStore) UpdateLogin(_ context.Context, rec LoginRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.logins[rec.ID]; !ok {
		return ErrLoginNotFound
	}
	m.logins[rec.ID] = rec
	return nil
}

// DeleteExpiredLogins implements Store.
func (m *MemStore) DeleteExpiredLogins(_ context.Context, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, l := range m.logins {
		if l.ExpiresAt.Before(now) {
			delete(m.logins, id)
		}
	}
	return nil
}
