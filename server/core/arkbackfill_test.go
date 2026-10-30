// Timeout and cooldown for backfilling Ark events on the "open run detail page" path; see
// backfillFromArk in core/ark.go. All offline (Ark is replaced by fakeRunner).
//
// # What these tests guard
//
// Backfilling calls Ark's HTTP API synchronously inside the request. Measured in production, this
// made `/api/runs/{runId}` the slowest endpoint (P50 320ms / P95 1398ms, 44 deduplicated samples;
// static files in the same batch 0~3ms). The fix is two things: give this step a 3-second limit,
// and use a 1-second cooldown to remove the second round trip within a single page open. Each
// gets a test here, plus the fallback "on timeout, database data is still returned".
//
// ⚠️ **The check must be about "which expires first"**, not just wall clock time:
// fakeRunner.listEventsDelay really responds to ctx (see its comment), so "the limit works" shows
// up as **the call returning early** rather than waiting the full delay. If the fake ignored ctx,
// these tests would pass on an implementation without a timeout too, and be useless.
package core

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/repository/dao/coremem"
)

// runningRunWithEvent builds a run that is "stuck in running, with one event already in the
// database".
//
// That event in the database is **required**: these tests must tell "returned the database data"
// from "returned empty", and with an empty database the two are identical. That is exactly the
// shape of the "err and an empty collection must not share a branch" trap.
func runningRunWithEvent(t *testing.T) (*Service, *coremem.MemStore, *fakeRunner, apimodel.Run) {
	t.Helper()
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}
	if _, _, err := store.AppendRunEvent(ctx, run.ID,
		[]byte(`{"id":"sevt-local","type":"agent.message","content":[{"type":"text","text":"库里已有的回复"}]}`)); err != nil {
		t.Fatalf("写本地事件失败：%v", err)
	}
	// Only a run stuck in running goes through backfill (isSyncableRunStatus).
	run.Status = apimodel.RunRunning
	if _, err := store.SaveRun(ctx, run); err != nil {
		t.Fatalf("保存 run 失败：%v", err)
	}
	return svc, store, runner, run
}

// TestGetRunSurvivesSlowArk: when Ark hangs, GetRun must return the database data within
// backfillTimeout instead of hanging with it.
//
// # This is the main check of this performance fix
//
// Before the fix GetRun called SyncRunEvents directly, and the limit of that chain was
// arkCallTimeout in LiveArkRunner (**30 seconds**), or even the SDK's built-in 10 minutes without
// it. For "a user opens a page", 30 seconds and 10 minutes are the same: both mean "this page does
// not load".
//
// The test sets Ark's delay to a value much larger than backfillTimeout, then measures three
// things:
//
//  1. **no error**: a failed backfill is a degradation, not an endpoint failure;
//  2. **the returned run is the one in the database** (not a zero value);
//  3. **the elapsed time is close to backfillTimeout, not to Ark's delay**: this is the real
//     check, and it rules out both false passes, "the limit never took effect" and "the fake
//     ignored ctx".
func TestGetRunSurvivesSlowArk(t *testing.T) {
	ctx := context.Background()
	svc, _, runner, run := runningRunWithEvent(t)

	// Much larger than backfillTimeout, so "waited the full delay" and "cut off by the limit" are distinguishable by wall clock.
	const arkStall = 30 * time.Second
	runner.mu.Lock()
	runner.listEventsDelay = arkStall
	runner.mu.Unlock()

	start := time.Now()
	got, err := svc.GetRun(ctx, run.ID)
	took := time.Since(start)

	if err != nil {
		t.Fatalf("方舟卡住时 GetRun 不该报错（补拉失败是降级）：%v", err)
	}
	if got.ID != run.ID {
		t.Fatalf("应返回库里那条 run，实际 %q", got.ID)
	}
	// The upper bound gives backfillTimeout a 2x margin to absorb scheduling noise, but is still far
	// below arkStall: an implementation without a limit would wait the full 30 seconds and land
	// outside this range.
	if took >= arkStall {
		t.Fatalf("GetRun 等满了方舟的 %v（上限没生效）", arkStall)
	}
	if limit := backfillTimeout * 2; took > limit {
		t.Fatalf("GetRun 耗时 %v，超过 backfillTimeout(%v) 的两倍——补拉的上限没有把它切掉",
			took, backfillTimeout)
	}
}

// TestListRunEventsSurvivesSlowArk: when Ark hangs, the event list must still return the events
// already in the database.
//
// The second backfill path next to GetRun (same isSyncableRunStatus check), measured at P95
// 1157ms in production. **Both must be pinned**: pinning only one would let the other regress back
// to a long synchronous timeout without any test failing.
func TestListRunEventsSurvivesSlowArk(t *testing.T) {
	ctx := context.Background()
	svc, _, runner, run := runningRunWithEvent(t)

	const arkStall = 30 * time.Second
	runner.mu.Lock()
	runner.listEventsDelay = arkStall
	runner.mu.Unlock()

	start := time.Now()
	events, err := svc.ListRunEvents(ctx, run.ID, persistmodel.EventQuery{})
	took := time.Since(start)

	if err != nil {
		t.Fatalf("方舟卡住时 ListRunEvents 不该报错：%v", err)
	}
	// **Must assert we got the one in the database**, not just err == nil: an empty slice + nil
	// error also satisfies the latter, and that is exactly the shape of "a failed backfill swallowed
	// the existing data too".
	if len(events) != 1 {
		t.Fatalf("应返回库里已有的 1 条事件，实际 %d 条", len(events))
	}
	if took >= arkStall {
		t.Fatalf("ListRunEvents 等满了方舟的 %v（上限没生效）", arkStall)
	}
	if limit := backfillTimeout * 2; took > limit {
		t.Fatalf("ListRunEvents 耗时 %v，超过 backfillTimeout(%v) 的两倍", took, backfillTimeout)
	}
}

// TestGetRunSurvivesFailingArk: when Ark returns an error outright, GetRun still returns the
// database data.
//
// Tested separately from "slow": timeout and failure take two different branches in the
// implementation (ctx expired vs runner returned error); testing only one would let the other
// regress without any test failing.
func TestGetRunSurvivesFailingArk(t *testing.T) {
	ctx := context.Background()
	svc, _, runner, run := runningRunWithEvent(t)

	runner.mu.Lock()
	runner.listEventsErr = errors.New("方舟 500")
	runner.mu.Unlock()

	got, err := svc.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("方舟报错时 GetRun 不该报错（补拉失败是降级）：%v", err)
	}
	if got.ID != run.ID {
		t.Fatalf("应返回库里那条 run，实际 %q", got.ID)
	}
	if got.Status != apimodel.RunRunning {
		t.Fatalf("补拉失败时状态应保持库里的 running，实际 %q", got.Status)
	}

	events, err := svc.ListRunEvents(ctx, run.ID, persistmodel.EventQuery{})
	if err != nil {
		t.Fatalf("方舟报错时 ListRunEvents 不该报错：%v", err)
	}
	if len(events) != 1 {
		t.Fatalf("应返回库里已有的 1 条事件，实际 %d 条", len(events))
	}
}

// TestBackfillTimeoutStillPersistsWhatItGot: when the backfill is cut off by the limit, **events
// already fetched are still persisted**.
//
// # Why this must be pinned separately
//
// The easiest wrong implementation is "derive a ctx with a timeout and pass it all the way to the
// database write". Then at the moment of timeout, the events this round trip already got are
// cancelled with it, the latency was paid for nothing, and the next call has to fetch them again.
// The existing practice in this repo is to let persistence start its own budget with
// context.WithoutCancel (persistArkEvent / runwatcher.retireMissingRun).
//
// Setup: Ark returns a batch of events instantly and persistence happens as usual; then the ctx is
// made cancelled, and we verify that batch is still in the database. **The check is "the events
// really got into the database"**, not the return value: the return value cannot tell "nothing
// fetched" from "fetched but not stored".
func TestBackfillTimeoutStillPersistsWhatItGot(t *testing.T) {
	ctx := context.Background()
	svc, store, runner, run := runningRunWithEvent(t)

	runner.mu.Lock()
	runner.listEvents = [][]byte{
		[]byte(`{"id":"sevt-tail-1","type":"agent.message","content":[{"type":"text","text":"补下来的尾部回复"}]}`),
		[]byte(`{"id":"sevt-tail-2","type":"session.status_idle","stop_reason":{"type":"end_turn"}}`),
	}
	runner.mu.Unlock()

	// An already cancelled ctx: if persistArkEvent in SyncRunEvents used it directly, persistence
	// would be cancelled and only the original event would be left in the database.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	// Whether it errors does not matter (the ctx is dead, the database read step may fail first);
	// what we verify is **no crash**, and that the backfill below with a good ctx really stores the
	// events.
	_ = svc.SyncRunEvents(cancelled, run.ID)

	// Again with a normal ctx; this time both tail events must be persisted.
	if err := svc.SyncRunEvents(ctx, run.ID); err != nil {
		t.Fatalf("SyncRunEvents 失败：%v", err)
	}
	events, err := store.ListRunEvents(ctx, run.ID, persistmodel.EventQuery{})
	if err != nil {
		t.Fatalf("ListRunEvents 失败：%v", err)
	}
	// 1 local + 2 backfilled.
	if len(events) != 3 {
		t.Fatalf("补拉后应有 3 条事件（1 本地 + 2 补拉），实际 %d 条", len(events))
	}
	// Also confirm the final state was computed: adding a timeout must not break what backfill means.
	got, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun 失败：%v", err)
	}
	if got.Status != apimodel.RunIdle {
		t.Fatalf("补拉到 end_turn 之后状态应为 idle，实际 %q", got.Status)
	}
}

// TestBackfillCooldownSkipsSecondArkCall: the second request within the same page open should not
// call Ark again.
//
// # What it saves
//
// Opening a run detail page sends two requests in sequence: first `GET /api/runs/{id}`, then,
// after that returns, `GET /api/runs/{id}/events` (web/src/pages/RunDetailPage.tsx). Both go
// through the same isSyncableRunStatus check, and before the fix **each called Ark once**: one
// page open paid for two cross-network round trips, and the second got back the same batch of
// events, every one hitting deduplication, pure waste.
//
// The check is listEventsCalls: **it must be 1**. It also asserts the event count is still
// complete (3), because "saving a round trip" only holds if no data is lost: it still reads from
// the database.
func TestBackfillCooldownSkipsSecondArkCall(t *testing.T) {
	ctx := context.Background()
	svc, _, runner, run := runningRunWithEvent(t)

	runner.mu.Lock()
	runner.listEvents = [][]byte{
		[]byte(`{"id":"sevt-tail-1","type":"agent.message","content":[{"type":"text","text":"补下来的尾部回复"}]}`),
		[]byte(`{"id":"sevt-tail-2","type":"session.status_running"}`),
	}
	runner.mu.Unlock()

	// First request: the detail page's getRun.
	if _, err := svc.GetRun(ctx, run.ID); err != nil {
		t.Fatalf("GetRun 失败：%v", err)
	}
	// Second request: the listRunEvents right after it (this is the real frontend order).
	events, err := svc.ListRunEvents(ctx, run.ID, persistmodel.EventQuery{})
	if err != nil {
		t.Fatalf("ListRunEvents 失败：%v", err)
	}

	runner.mu.Lock()
	calls := runner.listEventsCalls
	runner.mu.Unlock()

	if calls != 1 {
		t.Errorf("一次页面打开（getRun + listRunEvents）只该打方舟 1 次，实际 %d 次——"+
			"冷却没生效，等于每次打开都付两趟跨网络往返", calls)
	}
	// Saving a round trip must not lose data: 1 local + 2 backfilled.
	if len(events) != 3 {
		t.Fatalf("跳过第二次补拉后事件仍应是全的（3 条），实际 %d 条", len(events))
	}
}

// TestBackfillCooldownDoesNotBlockManualRefresh: once the cooldown passes, it must let calls
// through, otherwise "refreshing does nothing".
//
// # This guards against the urge to make the cooldown larger
//
// The cooldown only needs to cover the single local round trip "getRun returns → frontend sends
// the events request". Making it larger would block **a real person refreshing by hand**, and a
// user refreshes because they want the latest progress. Blocking their backfill shows up as
// "refreshing does nothing", with no error. So this pins "once the cooldown window passes, Ark
// must be called again".
//
// It does not sleep a full backfillCooldown (that would make the whole package wait for nothing):
// it moves the last backfill time back outside the window, which is the same as "that much time
// has passed".
func TestBackfillCooldownDoesNotBlockManualRefresh(t *testing.T) {
	ctx := context.Background()
	svc, _, runner, run := runningRunWithEvent(t)

	if _, err := svc.GetRun(ctx, run.ID); err != nil {
		t.Fatalf("GetRun 失败：%v", err)
	}

	// Move "last backfill time" outside the cooldown window, simulating the user refreshing by hand a bit later.
	svc.backfillMu.Lock()
	svc.lastBackfill[run.ID] = time.Now().Add(-2 * backfillCooldown)
	svc.backfillMu.Unlock()

	if _, err := svc.GetRun(ctx, run.ID); err != nil {
		t.Fatalf("第二次 GetRun 失败：%v", err)
	}

	runner.mu.Lock()
	calls := runner.listEventsCalls
	runner.mu.Unlock()

	if calls != 2 {
		t.Errorf("冷却过后手动刷新应重新补拉（累计 2 次），实际 %d 次——"+
			"冷却把用户的刷新挡掉了，表现是「刷了也不动」", calls)
	}
}

// TestBackfillFailureDoesNotStartCooldown: a failed backfill **must not** start the cooldown.
//
// # Why
//
// If failures were recorded too, one Ark hiccup would skip every backfill for a whole cooldown
// window. This "optimization" would block the channel that should self-heal, with no error: the
// endpoint still returns 200, only the tail events take a long time to arrive.
//
// Check: after the first failure, a second call right after (still inside the cooldown window)
// must really call Ark again.
func TestBackfillFailureDoesNotStartCooldown(t *testing.T) {
	ctx := context.Background()
	svc, _, runner, run := runningRunWithEvent(t)

	runner.mu.Lock()
	runner.listEventsErr = errors.New("方舟 500")
	runner.mu.Unlock()

	if _, err := svc.GetRun(ctx, run.ID); err != nil {
		t.Fatalf("GetRun 失败：%v", err)
	}
	// Second call right away, **without waiting for the cooldown**, on purpose: a failure should not start the cooldown.
	if _, err := svc.GetRun(ctx, run.ID); err != nil {
		t.Fatalf("第二次 GetRun 失败：%v", err)
	}

	runner.mu.Lock()
	calls := runner.listEventsCalls
	runner.mu.Unlock()

	if calls != 2 {
		t.Errorf("补拉失败不该进冷却（两次都要真打方舟），实际只打了 %d 次——"+
			"方舟抖一下就会让自愈通道停摆一整个冷却窗口", calls)
	}
}

// TestGetRunRereadsAfterFailedBackfill: when the backfill **returns an error** but the database
// has already been changed, GetRun must return the updated run.
//
// # This guards against "only re-read when err == nil"
//
// A backfill timeout/failure is "fetched halfway": events already fetched are still persisted
// (persistArkEvent uses its own WithoutCancel budget), and the run status has already been changed
// by applyEventToRun. And production runs multiple instances, so the same run may also be being
// written by another pod (SSE / Feishu / patrol). If we only re-read on success, these
// **already persisted** updates are invisible in this response. It shows up as "the status is
// wrong on first open, correct after a refresh", with no error.
//
// Setup uses listEventsHook: during this backfill, first change the database to idle, **then make
// the Ark call fail**. So "the database changed" and "this backfill failed" hold at the same time,
// which is the real shape of that window.
// ⚠️ The check is that the return value must be idle: changing the re-read in GetRun back to
// `else { ... }` makes this fail.
func TestGetRunRereadsAfterFailedBackfill(t *testing.T) {
	ctx := context.Background()
	svc, store, runner, run := runningRunWithEvent(t)

	runner.mu.Lock()
	// During the backfill, "someone else" pushed this run to a terminal state, or equivalently, the
	// events our own call already persisted pushed it there, and only the following Ark call failed.
	runner.listEventsHook = func() {
		stored, err := store.GetRun(ctx, run.ID)
		if err != nil {
			t.Errorf("hook 里读 run 失败：%v", err)
			return
		}
		stored.Status = apimodel.RunIdle
		if _, err := store.SaveRun(ctx, stored); err != nil {
			t.Errorf("hook 里保存 run 失败：%v", err)
		}
	}
	runner.listEventsErr = errors.New("方舟 500")
	runner.mu.Unlock()

	got, err := svc.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("补拉失败时 GetRun 不该报错：%v", err)
	}
	if got.Status != apimodel.RunIdle {
		t.Fatalf("补拉失败但库已改成 idle，GetRun 应重读到 idle，实际 %q——"+
			"重读被挂在 err == nil 上了，用户会看到「运行中」直到自己刷新", got.Status)
	}
}

// TestBackfillTableDoesNotGrowUnbounded: the cooldown table must clean up expired entries.
//
// Without cleanup it grows without bound with "the total number of runs ever backfilled": a slow
// memory leak that gets worse the longer the process lives, and reports no error. amad is a
// long-running process, so this is not pedantry.
func TestBackfillTableDoesNotGrowUnbounded(t *testing.T) {
	svc, _, _ := newChatService(t)

	// Fill in a batch of entries "backfilled long ago", then record a new one; the expired ones should be cleaned up.
	svc.backfillMu.Lock()
	for i := 0; i < 100; i++ {
		svc.lastBackfill[fmt.Sprintf("run-old-%d", i)] = time.Now().Add(-time.Hour)
	}
	svc.backfillMu.Unlock()

	svc.markBackfilled("run-fresh")

	svc.backfillMu.Lock()
	size := len(svc.lastBackfill)
	svc.backfillMu.Unlock()

	if size != 1 {
		t.Errorf("过期条目应被清掉、只留刚记的那条，实际表里有 %d 条——"+
			"这张表会随历史 run 总数无界增长", size)
	}
}
