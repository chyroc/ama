package core

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	coremem "github.com/chyroc/ama/server/repository/dao/coremem"
	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

// authSinceStore makes RecordArkAuthFailure return a given "has been failing in a row for how long",
// and counts how many times ClearArkAuthFailure was called. All other methods pass through to the
// in-memory store.
type authSinceStore struct {
	Store
	since   time.Duration
	failErr error
	clears  atomic.Int32
}

func (s *authSinceStore) RecordArkAuthFailure(context.Context, string, string, string) (time.Duration, error) {
	return s.since, s.failErr
}

func (s *authSinceStore) ClearArkAuthFailure(context.Context, string) error {
	s.clears.Add(1)
	return nil
}

func newAuthPausePool(t *testing.T, st *authSinceStore) *WorkerPool {
	t.Helper()
	mem, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败：%v", err)
	}
	st.Store = mem
	svc, err := New(Config{Store: st})
	if err != nil {
		t.Fatalf("装配 Service 失败：%v", err)
	}
	return &WorkerPool{cfg: WorkerConfig{Concurrency: 2}, log: testLogger(), auth: newAuthBackoff(), svc: svc}
}

var errAuth401 = errors.New("worker api status 401: The API key doesn't exist")

// TestAuthFailurePausesAfterThreeDays is the requirement itself: after 3 full days of continuous
// failure, the wait switches from the 5-minute step to about one day (authPausedProbe ±20% jitter).
func TestAuthFailurePausesAfterThreeDays(t *testing.T) {
	const env, fp = "env_dead_key", "len=46 sha=4af2ceef"
	pool := newAuthPausePool(t, &authSinceStore{since: authPauseAfter + time.Hour})

	wait := pool.onAuthFailure(context.Background(), testLogger(), env, "ws_x", fp, errAuth401)
	if wait < authPausedProbe*7/10 {
		t.Fatalf("连续失效超过 3 天应暂停约一天，实际只等 %v", wait)
	}
	// The gate itself must really block: it still does not let through one hour later.
	if blocked, _ := pool.auth.blocked(env, fp, time.Now().Add(time.Hour)); !blocked {
		t.Error("暂停后一小时闸门就放行了——holdUntil 没生效，等于还在按 5 分钟档重试")
	}
	// A changed key must be let through at once; the pause does not apply to a new key.
	if blocked, _ := pool.auth.blocked(env, "len=46 sha=ffff0000", time.Now()); blocked {
		t.Error("暂停期间换了 key 仍被挡住——用户换完 key 要等一天才恢复")
	}
}

// TestAuthFailureNotPausedBeforeThreeDays pairs with the previous test: before 3 full days it still
// follows the steps (at most 5 minutes +20% jitter). With only the previous test, "pause for a day on
// the first failure" would also pass.
func TestAuthFailureNotPausedBeforeThreeDays(t *testing.T) {
	pool := newAuthPausePool(t, &authSinceStore{since: authPauseAfter - time.Hour})
	wait := pool.onAuthFailure(context.Background(), testLogger(), "env_new_bad", "ws_x", "len=46 sha=1", errAuth401)
	if wait > 6*time.Minute {
		t.Errorf("没满 3 天不该暂停，实际等 %v", wait)
	}
}

// TestAuthFailureNotPausedWhenStoreFails: if the failure can't be written to the store, we can't tell
// whether 3 days have passed, so treat it as "not yet". The opposite (pause when unsure) would pause
// for a whole day a key that was just misconfigured and is about to be fixed.
func TestAuthFailureNotPausedWhenStoreFails(t *testing.T) {
	pool := newAuthPausePool(t, &authSinceStore{since: 30 * 24 * time.Hour, failErr: errors.New("pg 挂了")})
	wait := pool.onAuthFailure(context.Background(), testLogger(), "env_x", "ws_x", "len=46 sha=1", errAuth401)
	if wait > 6*time.Minute {
		t.Errorf("写库失败时不该暂停，实际等 %v", wait)
	}
}

// TestPollLoopClearsAuthFailureRecordOnce: after a successful poll, the start time in the store must
// be cleared; otherwise when this key works for a while and then breaks again, the count starts from
// long ago and it pauses for a day on the first failure.
// But clear it only once: successful polls happen every few seconds, and deleting a row each time is
// too wasteful.
func TestPollLoopClearsAuthFailureRecordOnce(t *testing.T) {
	st := &authSinceStore{}
	pool := newAuthPausePool(t, st)
	installFakeProcessor(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	api := &fakeWorkAPI{pollErrs: []error{nil, nil, nil}}
	_ = pool.pollLoop(ctx, api, "env_ok", "worker_test", 2, testLogger())

	if n := st.clears.Load(); n != 1 {
		t.Errorf("一条 pollLoop 里成功了多次，库里的失效记录应恰好清 1 次，实际 %d 次", n)
	}
}

var _ selfhosted.API = (*fakeWorkAPI)(nil)
