// Store contract tests for agent sync tasks to Ark.
//
// ⚠️ **The most important one is testAgentSyncClaimIsAtomic**: it pins the correctness pivot of the whole path.
// amad runs as multiple instances in production; N processes see the same pending task in the same second, and claiming must let
// **exactly one** win. Otherwise the same agent is changed by two instances at once, while single-instance test runs are always all green.
//
// ⚠️ That test **must warm up the connection pool first**, otherwise it cannot detect the one thing it guards against:
// without warming, the first thing the N goroutines do is each open a PG connection (milliseconds, staggered),
// and the first one connected finishes claiming long before the rest. No concurrency happens; the test degrades into N serial runs.
// Measured precedent: with `for update skip locked` and the outer recheck all deleted, the unwarmed version was still all green.

package coredao

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// newSyncTask builds a pending sync task.
func newSyncTask(id, agentID string, createdAt time.Time) persistmodel.AgentSyncTaskRow {
	return persistmodel.AgentSyncTaskRow{
		ID:          id,
		WorkspaceID: "ws_demo",
		AgentID:     agentID,
		Status:      string(apimodel.AgentSyncPending),
		CreatedAt:   createdAt,
		UpdatedAt:   createdAt,
	}
}

// testAgentSyncEnqueueDedups: while an agent's task is unfinished, another must not be queued.
//
// This is where "clicking twice runs only once" lands, decided by the database (partial unique index + on conflict).
func testAgentSyncEnqueueDedups(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	first, created, err := s.EnqueueAgentSyncTask(ctx, newSyncTask("astask_a", "agent_triage", now))
	if err != nil {
		t.Fatalf("入队失败：%v", err)
	}
	if !created {
		t.Fatal("第一条该是新建的")
	}

	second, created2, err := s.EnqueueAgentSyncTask(ctx,
		newSyncTask("astask_b", "agent_triage", now.Add(time.Second)))
	if err != nil {
		t.Fatalf("第二次入队失败：%v", err)
	}
	if created2 {
		t.Error("同一个 agent 已有没跑完的任务时不该新建——" +
			"排两条会让两个实例同时改同一个方舟 Agent")
	}
	if second.ID != first.ID {
		t.Errorf("该复用既有那条，实际 %q vs %q", second.ID, first.ID)
	}

	// After it finishes, another one should be queueable. **This part cannot be dropped**: if FinishAgentSyncTask forgets
	// finished_at, the unique index keeps blocking, and the symptom is "no more syncing on any later save", with zero errors.
	if err := s.FinishAgentSyncTask(ctx, first.ID,
		apimodel.AgentSyncSucceeded, "", "", now.Add(time.Minute)); err != nil {
		t.Fatalf("结算失败：%v", err)
	}
	_, created3, err := s.EnqueueAgentSyncTask(ctx,
		newSyncTask("astask_c", "agent_triage", now.Add(2*time.Minute)))
	if err != nil {
		t.Fatalf("第三次入队失败：%v", err)
	}
	if !created3 {
		t.Error("上一条已经跑完，该能排新的了——" +
			"排不上说明结算时没写 finished_at，那个 agent 从此再也不会被同步")
	}
}

// testAgentSyncResyncMarkedWhileRunning: saving again while it runs must record "run another round".
func testAgentSyncResyncMarkedWhileRunning(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	if _, _, err := s.EnqueueAgentSyncTask(ctx, newSyncTask("astask_r", "agent_triage", now)); err != nil {
		t.Fatalf("入队失败：%v", err)
	}
	claimed, err := s.ClaimDueAgentSyncTasks(ctx, "inst-a", now, time.Minute, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("认领失败：%v（%d 条）", err, len(claimed))
	}

	// The user saved again while it was running.
	got, created, err := s.EnqueueAgentSyncTask(ctx,
		newSyncTask("astask_r2", "agent_triage", now.Add(time.Second)))
	if err != nil {
		t.Fatalf("再次入队失败：%v", err)
	}
	if created {
		t.Error("在跑的时候不该新建任务")
	}
	if !got.ResyncRequested {
		t.Error("同步期间又保存了，该记下「跑完再跑一轮」——" +
			"不记的话这次保存的改动要等下次开对话才生效，而用户看到的是「同步成功」")
	}
}

// testAgentSyncZombieReclaim: a running task whose lease expired must be reclaimable.
//
// **This branch cannot be dropped**: when the instance that claimed it is rebuilt (deploy / scale-down / health check failure) the write-back never
// comes, and without this branch the task stays running forever while the frontend polls until timeout.
// The schedules side has the same branch.
func testAgentSyncZombieReclaim(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	if _, _, err := s.EnqueueAgentSyncTask(ctx, newSyncTask("astask_z", "agent_triage", now)); err != nil {
		t.Fatalf("入队失败：%v", err)
	}
	if got, err := s.ClaimDueAgentSyncTasks(ctx, "dead", now, time.Minute, 10); err != nil || len(got) != 1 {
		t.Fatalf("首次认领失败：%v（%d 条）", err, len(got))
	}

	// The lease has not expired, so it cannot be taken.
	got, err := s.ClaimDueAgentSyncTasks(ctx, "inst-b", now.Add(30*time.Second), time.Minute, 10)
	if err != nil {
		t.Fatalf("认领失败：%v", err)
	}
	if len(got) != 0 {
		t.Errorf("租约还有效时不该被抢走，实际领到 %d 条——那会让同一个 agent 被两个实例同时同步", len(got))
	}

	// After expiry it must be reclaimable.
	got, err = s.ClaimDueAgentSyncTasks(ctx, "inst-b", now.Add(2*time.Minute), time.Minute, 10)
	if err != nil {
		t.Fatalf("认领失败：%v", err)
	}
	if len(got) != 1 {
		t.Fatalf("租约过期后该被回收，实际 %d 条——少了这一支，实例一挂那条任务就永远停在 running", len(got))
	}
	if got[0].AttemptCount != 2 {
		t.Errorf("回收要计一次尝试（好在反复失败时能放弃），实际 %d", got[0].AttemptCount)
	}
}

// testAgentSyncClaimIsAtomic is the most important one in this group: with concurrent claims only one may win.
//
// ⚠️ **It only means something on real PG**: the MemStore run cannot reach the two pitfalls of EPQ recheck and skip locked
// (see the postmortem in store_pg_schedule.go).
//
// ⚠️ **Both warm-up steps are required**, otherwise the test cannot detect the one thing it guards against:
// (1) first run a round of side-effect-free reads concurrently to fill the connection pool; (2) use a starting line so the goroutines really start at once.
func testAgentSyncClaimIsAtomic(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	const total = 20
	const instances = 8
	for i := 0; i < total; i++ {
		task := newSyncTask(fmt.Sprintf("astask_race_%02d", i),
			fmt.Sprintf("agent_race_%02d", i), now)
		if _, _, err := s.EnqueueAgentSyncTask(ctx, task); err != nil {
			t.Fatalf("入队失败：%v", err)
		}
	}

	// Warm-up one: run a round of side-effect-free reads concurrently to fill the connection pool.
	var warm sync.WaitGroup
	for i := 0; i < instances; i++ {
		warm.Add(1)
		go func() {
			defer warm.Done()
			_, _ = s.LatestAgentSyncTask(ctx, "agent_race_00")
		}()
	}
	warm.Wait()

	// Warm-up two: the starting line, so all goroutines really start at the same time.
	start := make(chan struct{})
	var mu sync.Mutex
	claimedBy := map[string][]string{}

	var wg sync.WaitGroup
	for i := 0; i < instances; i++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			owner := fmt.Sprintf("instance-%d", k)
			<-start
			got, err := s.ClaimDueAgentSyncTasks(ctx, owner, now, 5*time.Minute, 0)
			if err != nil {
				t.Errorf("%s 认领失败：%v", owner, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, task := range got {
				claimedBy[task.ID] = append(claimedBy[task.ID], owner)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for id, owners := range claimedBy {
		if len(owners) != 1 {
			t.Errorf("任务 %s 被 %d 个实例同时领到（%v）—— 认领不是原子的，"+
				"线上会让两个实例同时改同一个方舟 Agent", id, len(owners), owners)
		}
	}
	if len(claimedBy) != total {
		t.Errorf("%d 条待同步任务只有 %d 条被领到，剩下的漏跑了", total, len(claimedBy))
	}
}

// testAgentSyncClaimLimit: limit takes effect, and one claim never exceeds it.
//
// ⚠️ This pins the CTE pitfall: putting limit in a plain subquery makes it rerun row by row as the inner side;
// on the schedules side `limit 2` measured as blocking nothing (all 5 were claimed).
func testAgentSyncClaimLimit(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	for i := 0; i < 5; i++ {
		task := newSyncTask(fmt.Sprintf("astask_lim_%d", i), fmt.Sprintf("agent_lim_%d", i), now)
		if _, _, err := s.EnqueueAgentSyncTask(ctx, task); err != nil {
			t.Fatalf("入队失败：%v", err)
		}
	}

	claimed, err := s.ClaimDueAgentSyncTasks(ctx, "inst-a", now, 5*time.Minute, 2)
	if err != nil {
		t.Fatalf("认领失败：%v", err)
	}
	if len(claimed) != 2 {
		t.Errorf("limit=2 应只领到 2 条，实际 %d——超了说明 limit 被计划形状打穿了", len(claimed))
	}
}

// testAgentSyncFieldsSurviveRoundTrip: field-by-field round trip.
//
// ⚠️ **Every field has a different value**: with the same value everywhere, two fields swapped in place would still be all green
// (when workspace_members was split out, name / avatar_url were missed, while the ordering, counting and role tests were all green).
func testAgentSyncFieldsSurviveRoundTrip(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	if _, _, err := s.EnqueueAgentSyncTask(ctx, newSyncTask("astask_rt", "agent_rt", now)); err != nil {
		t.Fatalf("入队失败：%v", err)
	}
	finishedAt := now.Add(90 * time.Second)
	if err := s.FinishAgentSyncTask(ctx, "astask_rt", apimodel.AgentSyncFailed,
		"方舟拒绝了这把 API Key", apimodel.AgentSyncStageCredential, finishedAt); err != nil {
		t.Fatalf("结算失败：%v", err)
	}

	got, err := s.LatestAgentSyncTask(ctx, "agent_rt")
	if err != nil {
		t.Fatalf("查任务失败：%v", err)
	}
	if got.ID != "astask_rt" {
		t.Errorf("id 不对：%q", got.ID)
	}
	if got.WorkspaceID != "ws_demo" {
		t.Errorf("workspaceId 不对：%q", got.WorkspaceID)
	}
	if got.AgentID != "agent_rt" {
		t.Errorf("agentId 不对：%q", got.AgentID)
	}
	if got.Status != string(apimodel.AgentSyncFailed) {
		t.Errorf("status 不对：%q", got.Status)
	}
	if got.FailureReason != "方舟拒绝了这把 API Key" {
		t.Errorf("failureReason 不对：%q——失败原因丢了，用户只看到「失败了」", got.FailureReason)
	}
	if got.FailureStage != string(apimodel.AgentSyncStageCredential) {
		t.Errorf("failureStage 不对：%q——前端据此给引导，错了会指向错误的地方", got.FailureStage)
	}
	if got.FinishedAt.IsZero() {
		t.Error("finishedAt 该有值——它是「能不能再入一条队」的判据")
	}
	// Finishing must clear the lease, otherwise debugging would assume someone is still running it.
	if got.ClaimedBy != "" || !got.ClaimedUntil.IsZero() {
		t.Errorf("终态该清掉租约，实际 by=%q until=%v", got.ClaimedBy, got.ClaimedUntil)
	}
}

// testAgentSyncLatestPicksNewest takes the latest one; ties at the same instant are ordered by ord.
//
// ⚠️ The ord tie-breaker half cannot be dropped: two tasks queued in the same millisecond cannot be ordered by time alone,
// and PG may order ties arbitrarily, which would make the frontend sometimes read the previous, finished task
// and show "syncing" as "done".
func testAgentSyncLatestPicksNewest(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	// The first finished, the second is queued, and both have **exactly the same** created_at.
	if _, _, err := s.EnqueueAgentSyncTask(ctx, newSyncTask("astask_old", "agent_two", now)); err != nil {
		t.Fatalf("入队失败：%v", err)
	}
	if err := s.FinishAgentSyncTask(ctx, "astask_old",
		apimodel.AgentSyncSucceeded, "", "", now); err != nil {
		t.Fatalf("结算失败：%v", err)
	}
	if _, _, err := s.EnqueueAgentSyncTask(ctx, newSyncTask("astask_new", "agent_two", now)); err != nil {
		t.Fatalf("第二次入队失败：%v", err)
	}

	got, err := s.LatestAgentSyncTask(ctx, "agent_two")
	if err != nil {
		t.Fatalf("查任务失败：%v", err)
	}
	if got.ID != "astask_new" {
		t.Errorf("该取最新那条（astask_new），实际 %q——"+
			"取错会让前端把「正在同步」显示成「已完成」", got.ID)
	}
}

// testAgentSyncLatestMissingIsNotFound: with no record, return ErrNotFound.
//
// **That is a different thing from "sync failed"**: the former only means this agent was never saved,
// and the handler must return 200 + status:"none" instead of rendering it as an error.
func testAgentSyncLatestMissingIsNotFound(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)
	if _, err := s.LatestAgentSyncTask(ctx, "agent_never_synced"); !errors.Is(err, persistmodel.ErrNotFound) {
		t.Errorf("没有记录该返回 ErrNotFound，实际 %v", err)
	}
}

// testAgentSyncRequeueClearsLease: requeue must clear the lease.
//
// ⚠️ Otherwise the next claim tick is blocked by its own unexpired lease (the running branch of the claim condition
// requires `claimed_until < now`), wasting one lease period.
func testAgentSyncRequeueClearsLease(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	if _, _, err := s.EnqueueAgentSyncTask(ctx, newSyncTask("astask_rq", "agent_rq", now)); err != nil {
		t.Fatalf("入队失败：%v", err)
	}
	if got, err := s.ClaimDueAgentSyncTasks(ctx, "inst-a", now, 10*time.Minute, 10); err != nil || len(got) != 1 {
		t.Fatalf("认领失败：%v（%d 条）", err, len(got))
	}
	if err := s.RequeueAgentSyncTask(ctx, "astask_rq", now.Add(time.Second)); err != nil {
		t.Fatalf("重排失败：%v", err)
	}

	// The lease still had 10 minutes left; after clearing, it should be claimable **right away**.
	got, err := s.ClaimDueAgentSyncTasks(ctx, "inst-b", now.Add(2*time.Second), time.Minute, 10)
	if err != nil {
		t.Fatalf("认领失败：%v", err)
	}
	if len(got) != 1 {
		t.Errorf("重排之后该能立刻再认领，实际 %d 条——"+
			"领不到说明重排没清租约，那要白等一整个租约周期", len(got))
	}
}

// testAgentSyncPurgeRemovesOnlyFinished: cleanup only deletes finished rows that are old enough.
func testAgentSyncPurgeRemovesOnlyFinished(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)
	now := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	// One old finished row, one pending.
	if _, _, err := s.EnqueueAgentSyncTask(ctx, newSyncTask("astask_done", "agent_done", now)); err != nil {
		t.Fatalf("入队失败：%v", err)
	}
	if err := s.FinishAgentSyncTask(ctx, "astask_done",
		apimodel.AgentSyncSucceeded, "", "", now); err != nil {
		t.Fatalf("结算失败：%v", err)
	}
	if _, _, err := s.EnqueueAgentSyncTask(ctx, newSyncTask("astask_live", "agent_live", now)); err != nil {
		t.Fatalf("入队失败：%v", err)
	}

	n, err := s.PurgeFinishedAgentSyncTasks(ctx, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("清理失败：%v", err)
	}
	if n != 1 {
		t.Errorf("该清掉 1 条已完成的，实际 %d", n)
	}
	if _, err := s.LatestAgentSyncTask(ctx, "agent_live"); err != nil {
		t.Errorf("没跑完的不该被清掉：%v——清掉了就再也没人跑它了", err)
	}
}

// Let the compiler confirm this group's signatures match storeFactory.
var _ = []func(*testing.T, storeFactory){
	testAgentSyncEnqueueDedups,
	testAgentSyncResyncMarkedWhileRunning,
	testAgentSyncZombieReclaim,
	testAgentSyncClaimIsAtomic,
	testAgentSyncClaimLimit,
	testAgentSyncFieldsSurviveRoundTrip,
	testAgentSyncLatestPicksNewest,
	testAgentSyncLatestMissingIsNotFound,
	testAgentSyncRequeueClearsLease,
	testAgentSyncPurgeRemovesOnlyFinished,
}

var _ core.Store = (*PGStore)(nil)
