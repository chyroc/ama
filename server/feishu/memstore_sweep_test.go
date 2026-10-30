// Tests for MemStore's internal behavior.
//
// This one is **deliberately kept in the feishu package**: it asserts on memStore's private fields (how many
// entries are left in the sessions map), which is an implementation detail, not part of the Store contract. Moving it to repository/dao/feishudao
// would require exporting all 10 of those fields, and once the fields are exported anyone can build a half-initialized memStore
// (miss one map and it panics on the first write).
//
// The externally visible half ("expired sessions cannot be read") lives in the contract suite and runs once against each implementation.

package feishu

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Expired sessions must be cleared on read; they must not stay in memory forever.
func TestMemStoreSweepsExpiredSessions(t *testing.T) {
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	store := &memStore{
		sessions: make(map[string]sessionEntry),
		apps:     make(map[string]*App),
		byAppID:  make(map[string]string),
		secrets:  make(map[string]string),
		now:      func() time.Time { return now },
	}
	ctx := context.Background()
	if err := store.PutSession(ctx, "s1", Session{ActorID: testActor}, time.Minute); err != nil {
		t.Fatalf("写会话失败: %v", err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := store.GetSession(ctx, "s1"); !errors.Is(err, ErrSessionLost) {
		t.Fatalf("期望 %v，实际 %v", ErrSessionLost, err)
	}
	if len(store.sessions) != 0 {
		t.Errorf("过期会话应被清掉，实际还剩 %d 个", len(store.sessions))
	}
}
