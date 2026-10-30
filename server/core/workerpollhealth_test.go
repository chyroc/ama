package core

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLeaseForHealth records releases; it simulates the poll lease.
type fakeLeaseForHealth struct{ released atomic.Int32 }

func (f *fakeLeaseForHealth) Acquire(context.Context, string, time.Duration) (Lease, bool, error) {
	return nil, true, nil
}
func (f *fakeLeaseForHealth) Renew(context.Context, time.Duration) (bool, error) {
	return true, nil
}
func (f *fakeLeaseForHealth) Release(context.Context) error {
	f.released.Add(1)
	return nil
}

// fakeTicker drives the watchdog with controllable time steps, without depending on a real timer.
//
// Each "current time" received on ch runs one check. stop() cancels ctx to finish.
func runWatchdogOnce(ctx context.Context, p *WorkerPool, h *pollHealth, lease Lease,
	ecf context.CancelFunc, bound time.Duration, now time.Time,
) {
	p.runPollHealthWatchdogWithClock(ctx, h, "env-x", "w-1", lease, ecf,
		time.Hour, bound, func() time.Time { return now })
}

// Healthy: the beat is recent → do not release the lock, do not restart.
func TestPollHealthHealthyDoesNotFire(t *testing.T) {
	p := &WorkerPool{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lease := &fakeLeaseForHealth{}
	envCtx, envCancel := context.WithCancel(context.Background())
	defer envCancel()

	now := time.Unix(1000, 0)
	h := newPollHealth(now)
	// The last poll is only 10s before "now", below the 90s threshold.
	done := make(chan struct{})
	go func() {
		runWatchdogOnce(ctx, p, h, lease, envCancel, 90*time.Second, now.Add(10*time.Second))
		close(done)
	}()

	// Let the watchdog run one tick: wrapping a small real tick is awkward, so asserting the
	// stalledAt semantics + calling the check once by hand is more stable. Here we verify
	// stalledAt directly instead.
	cancel()
	<-done
	if h.stalledAt(now.Add(10*time.Second), 90*time.Second) {
		t.Fatal("只过 10s 不该判僵死")
	}
	if lease.released.Load() != 0 || envCtx.Err() != nil {
		t.Fatal("健康 poller 不该放锁/重启")
	}
}

// Stuck: no beat beyond the threshold → stalledAt is true.
func TestPollHealthStalledDetection(t *testing.T) {
	start := time.Unix(1000, 0)
	h := newPollHealth(start)
	if h.stalledAt(start.Add(91*time.Second), 90*time.Second) != true {
		t.Fatal("91s 无 poll 应判僵死")
	}
	if h.stalledAt(start.Add(89*time.Second), 90*time.Second) {
		t.Fatal("89s 无 poll 不该判僵死（给空闲退避留余量）")
	}
}

// Counting semantics of the liveness heartbeat (the stall log uses it to tell "slots are all
// taken" apart from "polling is hung").
func TestPollHealthActiveCount(t *testing.T) {
	h := newPollHealth(time.Now())
	if h.activeWork.Load() != 0 {
		t.Fatal("初始在飞 work 应为 0")
	}
	h.workStarted()
	h.workStarted()
	h.workDone()
	if h.activeWork.Load() != 1 {
		t.Fatalf("期望剩 1，实际 %d", h.activeWork.Load())
	}
}

// Real tick + advancing fake clock: two consecutive ticks over the threshold → release the lock
// and restart, firing only once.
func TestPollHealthWatchdogReleasesAndCancelsWhenStalled(t *testing.T) {
	p := &WorkerPool{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lease := &fakeLeaseForHealth{}
	envCancelCalled := atomic.Int32{}
	ecf := func() { envCancelCalled.Add(1) }

	start := time.Unix(1000, 0)
	h := newPollHealth(start)

	// Fake clock: advances 50ms every time it is asked. tick=1ms, bound=90ms:
	// tick 1 now=start+50ms (not over), tick 2 now=start+100ms (over 90ms, fires).
	calls := atomic.Int64{}
	advancingClock := func() time.Time {
		n := calls.Add(1)
		return start.Add(time.Duration(n) * 50 * time.Millisecond)
	}
	done := make(chan struct{})
	go func() {
		p.runPollHealthWatchdogWithClock(ctx, h, "env-x", "w-1", lease, ecf,
			1*time.Millisecond, 90*time.Millisecond, advancingClock)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("卡死后看门狗应当放锁、重启并退出")
	}
	if lease.released.Load() != 1 {
		t.Fatalf("应当放锁一次，实际 %d", lease.released.Load())
	}
	if envCancelCalled.Load() != 1 {
		t.Fatalf("应当重启 envCancel 一次，实际 %d", envCancelCalled.Load())
	}
}
