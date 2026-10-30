// Tests for Purger. **These do not hit the database and always run.**
//
// # Why this file uses a fake DB while pgmq_test.go uses a real database
//
// The rule is "where the thing being checked lives". pgmq_test.go checks SQL semantics
// (atomic claiming, the EvalPlanQual recheck), which **only a real database has**; a fake
// cannot test them.
//
// Purger has no SQL of its own: it is pure scheduling and leader election logic. How often it
// runs, whether to run when the lock is not acquired, whether to run when taking the lock
// errors, whether it exits when ctx ends. These are **better** tested with a fake DB: no
// container needed (so they really run in CI and for people without docker), and we can
// assert exactly "how many times PurgeExpired was called", which on a real database could only
// be inferred from the data.
//
// ⚠️ This is not "settling for second best because the database is unreachable". If it were
// written as "test only when connected", that skip branch would always be taken on machines
// without the container, which is the same as shipping a green test that never ran.
package pgmq

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeDB records every SQL statement executed, without really touching a database.
//
// It only needs to implement the three DB methods; Purger only uses Exec (PurgeExpired goes
// through it).
type fakeDB struct {
	mu    sync.Mutex
	execs []string
	err   error
}

func (f *fakeDB) Exec(_ context.Context, q string, _ ...any) (sql.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs = append(f.execs, q)
	if f.err != nil {
		return nil, f.err
	}
	return fakeResult{}, nil
}

func (f *fakeDB) Query(context.Context, string, ...any) (*sql.Rows, error) {
	panic("Purger 不该调 Query")
}
func (f *fakeDB) QueryRow(context.Context, string, ...any) *sql.Row {
	panic("Purger 不该调 QueryRow")
}

// execCount reports how many times Exec was called.
func (f *fakeDB) execCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.execs)
}

// fakeResult is a minimal implementation of sql.Result.
//
// RowsAffected returns 1 on purpose: so the log branch for "deleted a non-zero count" also
// runs. Otherwise that code would never execute in tests.
type fakeResult struct{}

func (fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (fakeResult) RowsAffected() (int64, error) { return 1, nil }

// fakeLeases is a controllable leader election: it grants the lock or refuses it, as you want.
type fakeLeases struct {
	mu         sync.Mutex
	grant      bool
	err        error
	acquires   int
	releases   int
	releaseErr error
}

func (f *fakeLeases) Acquire(context.Context, string, time.Duration) (Lease, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquires++
	if f.err != nil {
		return nil, false, f.err
	}
	if !f.grant {
		return nil, false, nil
	}
	return fakeLease{f}, true, nil
}

func (f *fakeLeases) counts() (acquires, releases int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acquires, f.releases
}

type fakeLease struct{ owner *fakeLeases }

func (l fakeLease) Release(context.Context) error {
	l.owner.mu.Lock()
	defer l.owner.mu.Unlock()
	l.owner.releases++
	return l.owner.releaseErr
}

// TestPurgerSkipsWhenLockNotAcquired: if the lock is not acquired, **do not clean**.
//
// ⚠️ This is the whole point of leader election. Missing this check gives no error: N
// instances run a full-table DELETE on the same table at the same time every hour, showing up
// as periodic lock contention and IO spikes, with nothing unusual in the logs.
func TestPurgerSkipsWhenLockNotAcquired(t *testing.T) {
	t.Parallel()

	db := &fakeDB{}
	leases := &fakeLeases{grant: false}
	p := NewPurger(PurgerConfig{DB: db, Leases: leases})

	p.tick(context.Background())

	if got := db.execCount(); got != 0 {
		t.Errorf("抢不到锁时不该执行任何 SQL，实际执行了 %d 条", got)
	}
	if acq, rel := leases.counts(); acq != 1 || rel != 0 {
		t.Errorf("应当抢 1 次、不释放，实际 抢=%d 释放=%d", acq, rel)
	}
}

// TestPurgerRunsAndReleasesWhenLockAcquired: if the lock is acquired, clean, and release it
// afterwards.
//
// ⚠️ **The release cannot be skipped**: without it the lock only expires after the TTL
// (5 minutes), and no other instance can take it in the meantime. The symptom is that the
// cleanup rate quietly drops, again with no error.
func TestPurgerRunsAndReleasesWhenLockAcquired(t *testing.T) {
	t.Parallel()

	db := &fakeDB{}
	leases := &fakeLeases{grant: true}
	p := NewPurger(PurgerConfig{DB: db, Leases: leases})

	p.tick(context.Background())

	if got := db.execCount(); got != 1 {
		t.Errorf("抢到锁时应当执行 1 条清理 SQL，实际 %d 条", got)
	}
	if acq, rel := leases.counts(); acq != 1 || rel != 1 {
		t.Errorf("应当抢 1 次、释放 1 次，实际 抢=%d 释放=%d", acq, rel)
	}
}

// TestPurgerRunsWhenLockErrors: when taking the lock **errors**, clean as usual (fail-open).
//
// ⚠️ This looks like it contradicts "do not clean if the lock is not acquired" above, but they
// are two different things, and the directions are opposite on purpose:
//
//	not acquired (ok=false)  → someone else is cleaning, I do not      ← normal election result
//	lock error (err!=nil)    → Redis blipped, I clean as usual         ← lock unavailable does not mean the work should not be done
//
// Getting it backwards means: once Redis goes down, cleanup stops entirely, while this leak
// never depended on the lock for correctness (delete is idempotent). Same check as in
// runwatcher.
func TestPurgerRunsWhenLockErrors(t *testing.T) {
	t.Parallel()

	db := &fakeDB{}
	leases := &fakeLeases{err: errors.New("redis 抖了")}
	p := NewPurger(PurgerConfig{DB: db, Leases: leases})

	p.tick(context.Background())

	if got := db.execCount(); got != 1 {
		t.Errorf("抢锁报错时应当照常清（fail-open），实际执行了 %d 条 SQL", got)
	}
	if _, rel := leases.counts(); rel != 0 {
		t.Errorf("没拿到锁就不该释放，实际释放了 %d 次", rel)
	}
}

// TestPurgerWithoutLeasesAlwaysRuns: without Redis configured, every instance cleans.
//
// That is safe (delete is idempotent), and it is the normal case in local development. Not
// having Redis should not mean not cleaning.
func TestPurgerWithoutLeasesAlwaysRuns(t *testing.T) {
	t.Parallel()

	db := &fakeDB{}
	p := NewPurger(PurgerConfig{DB: db}) // Leases is nil

	p.tick(context.Background())

	if got := db.execCount(); got != 1 {
		t.Errorf("没配选主时应当直接清，实际执行了 %d 条 SQL", got)
	}
}

// TestPurgerRunsImmediatelyOnStart: run once at startup, without waiting a full interval.
//
// ⚠️ **This pins a behavior that is easy to leave out**: with only
// `for { <-ticker.C; tick() }`, after an instance restarts it would wait a full interval
// (1 hour) before the first cleanup. After a restart the table often has expired messages
// left from the previous round piled up, so the restart would achieve nothing.
func TestPurgerRunsImmediatelyOnStart(t *testing.T) {
	t.Parallel()

	db := &fakeDB{}
	// The interval is long enough that this round **can only** come from the immediate run at
	// startup.
	p := NewPurger(PurgerConfig{DB: db, Every: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	waitUntil(t, func() bool { return db.execCount() >= 1 })
	cancel()

	if err := <-done; err != nil {
		t.Fatalf("正常取消应当返回 nil，得到 %v", err)
	}
	if got := db.execCount(); got != 1 {
		t.Errorf("启动后应当恰好清 1 次（间隔 1 小时内不该有第二次），实际 %d 次", got)
	}
}

// TestPurgerStopsOnContextDone: Run must return when ctx ends.
//
// If this is missing, the symptom is **the process cannot exit**, not an error.
func TestPurgerStopsOnContextDone(t *testing.T) {
	t.Parallel()

	p := NewPurger(PurgerConfig{DB: &fakeDB{}, Every: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("正常取消应当返回 nil，得到 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 结束 2 秒后 Run 仍未返回")
	}
}

// TestPurgerSurvivesDBError: a failed cleanup must not kill Run; the next tick must try again.
//
// ⚠️ One failed delete is often just a blip. Treating it as a reason to stop would let one
// blip **stop cleanup forever**, and after that no error would ever remind you.
func TestPurgerSurvivesDBError(t *testing.T) {
	t.Parallel()

	db := &fakeDB{err: errors.New("库抖了")}
	p := NewPurger(PurgerConfig{DB: db, Every: 20 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	// After an error there must still be later attempts (the startup one + at least one tick).
	waitUntil(t, func() bool { return db.execCount() >= 2 })
	cancel()

	if err := <-done; err != nil {
		t.Errorf("清理出错不该让 Run 返回错误，得到 %v", err)
	}
}

// TestNewPurgerRejectsNilDB: with no DB it returns nil, instead of building something that
// would crash.
func TestNewPurgerRejectsNilDB(t *testing.T) {
	t.Parallel()

	if p := NewPurger(PurgerConfig{}); p != nil {
		t.Errorf("DB 为 nil 时应当返回 nil，得到 %#v", p)
	}
}

// waitUntil polls until the condition holds, and fails the test on timeout.
//
// No fixed Sleep: too short and it fails now and then on slow machines, too long and it slows
// down the whole suite.
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("等条件成立超时（2 秒）")
}
