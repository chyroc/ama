package channeldao

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chyroc/ama/server/channel"
)

// eachStore runs the same contract against the in-memory and the PG implementation.
func eachStore(t *testing.T, run func(t *testing.T, s channel.Store)) {
	t.Run("mem", func(t *testing.T) { run(t, channel.NewMemStore()) })
	t.Run("pg", func(t *testing.T) {
		s, err := NewPGStore(context.Background(), newTestClient(t))
		if err != nil {
			t.Fatal(err)
		}
		run(t, s)
	})
}

func TestClaimInbound(t *testing.T) {
	eachStore(t, func(t *testing.T, s channel.Store) {
		ctx := context.Background()
		msg := channel.InboundMessage{Channel: channel.Weixin, AccountID: "acc", MessageID: "m1", CreatedAt: time.Now()}
		if ok, err := s.ClaimInbound(ctx, msg); err != nil || !ok {
			t.Fatalf("first claim = %v, %v", ok, err)
		}
		if ok, _ := s.ClaimInbound(ctx, msg); ok {
			t.Fatal("a pending message must not be claimed twice")
		}
		// Same message id on another account or channel is a different message.
		other := msg
		other.AccountID = "acc2"
		if ok, _ := s.ClaimInbound(ctx, other); !ok {
			t.Fatal("the key includes the account")
		}
		if err := s.MarkInbound(ctx, channel.Weixin, "acc", "m1", channel.InboundFailed); err != nil {
			t.Fatal(err)
		}
		if ok, _ := s.ClaimInbound(ctx, msg); !ok {
			t.Fatal("a failed message must be let through again")
		}
		_ = s.MarkInbound(ctx, channel.Weixin, "acc", "m1", channel.InboundProcessed)
		if ok, _ := s.ClaimInbound(ctx, msg); ok {
			t.Fatal("a processed message must not be claimed again")
		}
		if _, err := s.ClaimInbound(ctx, channel.InboundMessage{Channel: channel.Weixin, AccountID: "acc"}); !errors.Is(err, channel.ErrInvalidArgument) {
			t.Fatalf("empty message id: %v", err)
		}
	})
}

func TestScopeBindTouchReset(t *testing.T) {
	eachStore(t, func(t *testing.T, s channel.Store) {
		ctx := context.Background()
		now := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
		key := channel.ScopeKey{Channel: channel.Weixin, AccountID: "acc", Key: "user:u1", AgentID: "agent_a"}
		if _, err := s.GetScope(ctx, key); !errors.Is(err, channel.ErrScopeNotBound) {
			t.Fatalf("unbound: %v", err)
		}
		first := channel.Scope{ScopeKey: key, RunID: "run_1", WorkspaceID: "ws", CreatedAt: now, LastUsedAt: now, TurnCount: 1}
		if _, ok, err := s.BindScope(ctx, first); err != nil || !ok {
			t.Fatalf("bind = %v, %v", ok, err)
		}
		second := first
		second.RunID = "run_2"
		winner, ok, err := s.BindScope(ctx, second)
		if err != nil || ok || winner.RunID != "run_1" {
			t.Fatalf("the loser must get the winner back: %+v %v %v", winner, ok, err)
		}
		if err := s.TouchScope(ctx, key, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetScope(ctx, key)
		if err != nil || got.TurnCount != 2 || !got.LastUsedAt.Equal(now.Add(time.Minute)) {
			t.Fatalf("after touch: %+v %v", got, err)
		}
		// Another agent on the same conversation has its own scope.
		otherAgent := key
		otherAgent.AgentID = "agent_b"
		if _, ok, _ := s.BindScope(ctx, channel.Scope{ScopeKey: otherAgent, RunID: "run_3", CreatedAt: now, LastUsedAt: now}); !ok {
			t.Fatal("agent id is part of the key")
		}
		if err := s.ResetScope(ctx, channel.Weixin, "acc", "user:u1"); err != nil {
			t.Fatal(err)
		}
		for _, k := range []channel.ScopeKey{key, otherAgent} {
			if _, err := s.GetScope(ctx, k); !errors.Is(err, channel.ErrScopeNotBound) {
				t.Fatalf("reset must forget every agent's session: %v", err)
			}
		}
		if err := s.TouchScope(ctx, key, now); !errors.Is(err, channel.ErrScopeNotBound) {
			t.Fatalf("touching an unbound scope: %v", err)
		}
	})
}

func TestMigrateTwice(t *testing.T) {
	client := newTestClient(t)
	for i := 0; i < 2; i++ {
		if err := Migrate(context.Background(), client); err != nil {
			t.Fatalf("migrate #%d: %v", i+1, err)
		}
	}
}
