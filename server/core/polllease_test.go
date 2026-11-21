package core

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

type testPollLease struct{ renews atomic.Int32 }

func (l *testPollLease) Renew(context.Context, time.Duration) (bool, error) {
	l.renews.Add(1)
	return false, nil
}
func (l *testPollLease) Release(context.Context) error { return nil }

func TestKeepPollLeaseStopsAfterLosingOwnership(t *testing.T) {
	lease := &testPollLease{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		KeepPollLease(ctx, lease, time.Second, time.Millisecond, cancel, slog.Default())
		close(done)
	}()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("losing the lease must cancel polling")
	}
	<-done
	if lease.renews.Load() != 1 {
		t.Fatalf("expected one renewal, got %d", lease.renews.Load())
	}
}

func TestAcquirePollLeaseWithoutStoreAllowsPolling(t *testing.T) {
	lease, ok, err := AcquirePollLease(context.Background(), nil, "env_example", time.Second)
	if err != nil || !ok || lease != nil {
		t.Fatalf("missing lease store must degrade to polling: lease=%v ok=%v err=%v", lease, ok, err)
	}
}

func TestAuthBackoffKeyAndHoldRules(t *testing.T) {
	gate := NewAuthBackoff([]time.Duration{time.Minute}, nil)
	now := time.Unix(1700000000, 0)
	wait, failures := gate.Fail("env_example", "old", now)
	if wait != time.Minute || failures != 1 {
		t.Fatalf("unexpected first failure: wait=%v failures=%d", wait, failures)
	}
	gate.HoldUntil("env_example", "new", now.Add(24*time.Hour))
	if blocked, _ := gate.Blocked("env_example", "old", now.Add(time.Hour)); blocked {
		t.Fatal("a different key must not extend the old key's gate")
	}
	gate.HoldUntil("env_example", "old", now.Add(24*time.Hour))
	if blocked, _ := gate.Blocked("env_example", "old", now.Add(time.Hour)); !blocked {
		t.Fatal("matching key must extend the gate")
	}
	gate.HoldUntil("env_example", "old", now)
	if blocked, _ := gate.Blocked("env_example", "new", now); blocked {
		t.Fatal("a changed key must be released immediately")
	}
	if _, failures := gate.Fail("env_example", "new", now); failures != 1 {
		t.Fatalf("new key must start its own count, got %d", failures)
	}
	gate.Clear("env_example")
	if blocked, _ := gate.Blocked("env_example", "new", now); blocked {
		t.Fatal("a successful poll must clear the gate")
	}
}
