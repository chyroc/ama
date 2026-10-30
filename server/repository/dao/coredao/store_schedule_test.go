// Store contract tests for scheduled tasks.
//
// Hooked into TestStoreSuite, so **every assertion runs once each against MemStore and PGStore**
// (the PG run is skipped entirely with t.Skip when the database is unreachable, see store_pg_test.go). The two implementations must match case by case,
// otherwise "works fine in local memory, wrong on PG", the most painful kind of problem to track down.
//
// The most important test in this file is testClaimIsAtomicUnderConcurrency: it pins the correctness pivot of the whole
// scheduling path. amad runs as multiple instances in production (see CLAUDE.md at the repo root); if claiming is not atomic, users get
// N duplicate reports, and **with a single instance this bug never shows up in tests**.

package coredao

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/model/apimodel"
)

// newTestSchedule builds a cron task; nextRunAt is given by the caller.
func newTestSchedule(id, nextRunAt string) apimodel.Schedule {
	return apimodel.Schedule{
		ID:          id,
		WorkspaceID: "ws_demo",
		AgentID:     "agent_triage",
		AgentName:   "工单分诊助手",
		Name:        "每日巡检",
		Spec: apimodel.ScheduleSpec{
			Kind: apimodel.ScheduleCron,
			Cron: "30 9 * * 1-5",
			TZ:   apimodel.DefaultScheduleTZ,
		},
		Prompt:    "汇总昨天的告警",
		Origin:    apimodel.OriginUser,
		Status:    apimodel.ScheduleActive,
		Delivery:  apimodel.DeliveryTarget{Kind: apimodel.DeliveryRunOnly},
		NextRunAt: nextRunAt,
		CreatedAt: apimodel.NowISO(),
		UpdatedAt: apimodel.NowISO(),
	}
}

// testScheduleCRUD: the basic loop of create, get, list, overwrite, delete.
func testScheduleCRUD(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	sch := newTestSchedule("sch_test_1", apimodel.NowISO())
	created, err := s.CreateSchedule(ctx, sch)
	if err != nil {
		t.Fatalf("建定时任务失败：%v", err)
	}
	if created.ID != sch.ID || created.Name != sch.Name {
		t.Fatalf("回执与入参不一致：%+v", created)
	}

	got, err := s.GetSchedule(ctx, sch.ID)
	if err != nil {
		t.Fatalf("查定时任务失败：%v", err)
	}
	// spec and delivery are embedded structs; compare them as a whole, since on the PG side they go through a JSON round trip in doc.
	if got.Spec != sch.Spec {
		t.Errorf("spec 往返后不一致：%+v != %+v", got.Spec, sch.Spec)
	}
	if got.Delivery != sch.Delivery {
		t.Errorf("delivery 往返后不一致：%+v != %+v", got.Delivery, sch.Delivery)
	}
	if got.NextRunAt != sch.NextRunAt {
		t.Errorf("nextRunAt 往返后不一致：%q != %q（PG 那侧要过一次 timestamptz）",
			got.NextRunAt, sch.NextRunAt)
	}

	list, err := s.ListSchedules(ctx, "ws_demo")
	if err != nil {
		t.Fatalf("列定时任务失败：%v", err)
	}
	if len(list) != 1 {
		t.Fatalf("应有 1 条，实际 %d", len(list))
	}

	// Other workspaces cannot see it.
	other, err := s.ListSchedules(ctx, "ws_nope")
	if err != nil {
		t.Fatalf("列别的空间失败：%v", err)
	}
	if len(other) != 0 {
		t.Errorf("别的空间不该看到这条任务，实际 %d 条", len(other))
	}

	sch.Name = "改过的名字"
	sch.Status = apimodel.SchedulePaused
	sch.NextRunAt = "" // paused means no next fire time
	if _, err := s.SaveSchedule(ctx, sch); err != nil {
		t.Fatalf("覆盖写失败：%v", err)
	}
	got, err = s.GetSchedule(ctx, sch.ID)
	if err != nil {
		t.Fatalf("再查失败：%v", err)
	}
	if got.Name != "改过的名字" || got.Status != apimodel.SchedulePaused {
		t.Errorf("覆盖写没生效：%+v", got)
	}
	if got.NextRunAt != "" {
		t.Errorf("暂停后 nextRunAt 应为空，实际 %q", got.NextRunAt)
	}

	if err := s.DeleteSchedule(ctx, sch.ID); err != nil {
		t.Fatalf("删除失败：%v", err)
	}
	if _, err := s.GetSchedule(ctx, sch.ID); err != core.ErrNotFound {
		t.Errorf("删完再查应返回 ErrNotFound，实际 %v", err)
	}
	// Idempotent: deleting again is not an error.
	if err := s.DeleteSchedule(ctx, sch.ID); err != nil {
		t.Errorf("重复删除应当幂等，实际 %v", err)
	}
	// Writing something missing returns ErrNotFound.
	if _, err := s.SaveSchedule(ctx, sch); err != core.ErrNotFound {
		t.Errorf("覆盖写不存在的任务应返回 ErrNotFound，实际 %v", err)
	}
}

// testClaimOnlyDueAndActive: claiming only picks "due + active + not claimed by someone else".
func testClaimOnlyDueAndActive(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	now := time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC)
	past := apimodel.FormatISO(now.Add(-time.Minute))
	future := apimodel.FormatISO(now.Add(time.Hour))

	// Due and active: should be claimed.
	due := newTestSchedule("sch_due", past)
	// Not due yet: should not be claimed.
	notYet := newTestSchedule("sch_future", future)
	// Due but paused: should not be claimed.
	paused := newTestSchedule("sch_paused", past)
	paused.Status = apimodel.SchedulePaused
	// No nextRunAt: should not be claimed (the shape of paused or already claimed).
	noNext := newTestSchedule("sch_nonext", "")

	for _, sch := range []apimodel.Schedule{due, notYet, paused, noNext} {
		if _, err := s.CreateSchedule(ctx, sch); err != nil {
			t.Fatalf("建 %s 失败：%v", sch.ID, err)
		}
	}

	claimed, err := s.ClaimDueSchedules(ctx, "instance-A", now, 15*time.Minute, 0)
	if err != nil {
		t.Fatalf("认领失败：%v", err)
	}
	if len(claimed) != 1 {
		var ids []string
		for _, c := range claimed {
			ids = append(ids, c.ID)
		}
		t.Fatalf("只该领到 1 条（到点且 active 的那条），实际领到 %d 条：%v", len(claimed), ids)
	}
	if claimed[0].ID != "sch_due" {
		t.Errorf("领错了：%s", claimed[0].ID)
	}
	// **Claiming must clear nextRunAt**: otherwise, since one fire takes several minutes,
	// every scan cycle in between would claim the same task again.
	if claimed[0].NextRunAt != "" {
		t.Errorf("认领后 nextRunAt 应被置空，实际 %q —— 否则 fire 期间会被反复认领",
			claimed[0].NextRunAt)
	}
	// The stored copy must be empty too (the next scan looks at the database, not at this return value in memory).
	reread, err := s.GetSchedule(ctx, "sch_due")
	if err != nil {
		t.Fatalf("重读失败：%v", err)
	}
	if reread.NextRunAt != "" {
		t.Errorf("库里的 nextRunAt 也该是空的，实际 %q", reread.NextRunAt)
	}

	// Claim again right away: the lease has not expired, so nothing should be claimed.
	again, err := s.ClaimDueSchedules(ctx, "instance-B", now, 15*time.Minute, 0)
	if err != nil {
		t.Fatalf("二次认领失败：%v", err)
	}
	if len(again) != 0 {
		t.Errorf("租约未过期时不该被别人领走，实际领到 %d 条", len(again))
	}
}

// testClaimRespectsLeaseExpiry: after the lease expires another instance can take over.
//
// ⚠️ **This only checks the "lease" gate**: midway it puts NextRunAt back by hand,
// so it takes the "normally due" branch. **When an instance really dies, nobody does this step**;
// that shape is covered by testClaimReclaimsZombieAfterLeaseExpiry, and the two must be read as a pair.
// With only this one, the self-lock below where "the task stops forever" cannot be detected:
// it is exactly this manual backfill that does for the dead process what it can never do.
func testClaimRespectsLeaseExpiry(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	now := time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC)
	sch := newTestSchedule("sch_lease", apimodel.FormatISO(now.Add(-time.Minute)))
	if _, err := s.CreateSchedule(ctx, sch); err != nil {
		t.Fatalf("建任务失败：%v", err)
	}

	lease := 10 * time.Minute
	claimed, err := s.ClaimDueSchedules(ctx, "instance-A", now, lease, 0)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("首次认领应成功：err=%v n=%d", err, len(claimed))
	}

	// A claimed it and never came back (the process died). This task's nextRunAt has been cleared,
	// so simulate "the scheduler puts it back for the next time"; on the real path this step is done by the write-back at the end of fire,
	// which does not happen when the instance dies. So it is put back directly here, and what is checked is the **lease** gate.
	revived := claimed[0]
	revived.NextRunAt = apimodel.FormatISO(now.Add(-time.Minute))
	if _, err := s.SaveSchedule(ctx, revived); err != nil {
		t.Fatalf("排回下一次失败：%v", err)
	}

	// Within the lease: B cannot take it.
	inLease, err := s.ClaimDueSchedules(ctx, "instance-B", now.Add(lease-time.Minute), lease, 0)
	if err != nil {
		t.Fatalf("租约内认领出错：%v", err)
	}
	if len(inLease) != 0 {
		t.Errorf("租约内不该被抢走，实际领到 %d 条", len(inLease))
	}

	// After the lease expires: B can take over.
	afterLease, err := s.ClaimDueSchedules(ctx, "instance-B", now.Add(lease+time.Minute), lease, 0)
	if err != nil {
		t.Fatalf("租约过期后认领出错：%v", err)
	}
	if len(afterLease) != 1 {
		t.Fatalf("租约过期后应能接管，实际领到 %d 条 —— 否则实例重启会让任务永久卡死",
			len(afterLease))
	}
}

// testClaimReclaimsZombieAfterLeaseExpiry: after the process that claimed it dies, the task can still be rescued.
//
// # This pins a failure that really happened
//
// An "every 60 minutes" sweep task **stopped for nearly a day**; the page showed "next run: not scheduled",
// while status was still active, consecutiveFailures still 0, lastError empty:
// all four signals said "everything is fine", so no mechanism noticed it had stopped.
//
// The cause is that the side effect of claiming and the precondition of claiming lock each other:
//
//	claim → set next_run_at = null → process interrupted by a deploy, the write-back never comes
//	     → the record no longer meets "next_run_at is not null" → can never be claimed → nobody ever writes back
//
// The key is **not to put NextRunAt back on behalf of the dead process**: testClaimRespectsLeaseExpiry
// does exactly that, so it takes the "normally due" branch and completely hides this self-lock.
// This test **deliberately does nothing** and only moves the clock forward; that is the real shape of an instance dying.
//
// ⚠️ Mutation check: removing the "zombie reclaim" branch in ClaimDueSchedules (one place each in PG and MemStore)
// must turn this test red.
func testClaimReclaimsZombieAfterLeaseExpiry(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	now := time.Date(2026, 8, 30, 16, 41, 0, 0, time.UTC)
	sch := newTestSchedule("sch_zombie", apimodel.FormatISO(now.Add(-time.Minute)))
	if _, err := s.CreateSchedule(ctx, sch); err != nil {
		t.Fatalf("建任务失败：%v", err)
	}

	lease := 15 * time.Minute
	claimed, err := s.ClaimDueSchedules(ctx, "instance-A", now, lease, 0)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("首次认领应成功：err=%v n=%d", err, len(claimed))
	}
	// Side effect of claiming: NextRunAt is cleared. **This is exactly the ambiguous state**: it means both
	// "running" and "stuck", and it alone cannot tell them apart.
	if claimed[0].NextRunAt != "" {
		t.Fatalf("认领后 NextRunAt 应为空，实际 %q", claimed[0].NextRunAt)
	}

	// instance-A is interrupted by a deploy here, **with no write-back at all**: no finish, no rescheduleOnly,
	// no ReleaseScheduleClaim. What is left in the database is a zombie with next_run_at null.
	reread, err := s.GetSchedule(ctx, sch.ID)
	if err != nil {
		t.Fatalf("重读失败：%v", err)
	}
	if reread.NextRunAt != "" || reread.Status != apimodel.ScheduleActive {
		t.Fatalf("僵尸的形状应是「NextRunAt 空 + 仍是 active」，实际 next=%q status=%s",
			reread.NextRunAt, reread.Status)
	}

	// The lease has not expired: B must not take it, which would run the same task twice in parallel.
	// (claimLease 15 minutes > fireTimeout 10 minutes, so within the lease the original owner may still be alive.)
	inLease, err := s.ClaimDueSchedules(ctx, "instance-B", now.Add(lease-time.Minute), lease, 0)
	if err != nil {
		t.Fatalf("租约内认领出错：%v", err)
	}
	if len(inLease) != 0 {
		t.Errorf("租约内不该被抢走，实际领到 %d 条", len(inLease))
	}

	// Lease expired: the original owner must be dead (the lease is longer than the upper bound of one round), and B must be able to take over.
	afterLease, err := s.ClaimDueSchedules(ctx, "instance-B", now.Add(lease+time.Minute), lease, 0)
	if err != nil {
		t.Fatalf("租约过期后认领出错：%v", err)
	}
	if len(afterLease) != 1 {
		t.Fatalf("租约过期的僵尸必须能被重新认领，实际领到 %d 条"+
			"——否则一次发布就会让这条任务永久停摆（真出过）",
			len(afterLease))
	}
	if afterLease[0].ID != sch.ID {
		t.Fatalf("领错了任务：%s", afterLease[0].ID)
	}
}

// testClaimDoesNotStealNeverClaimedSchedules: the zombie reclaim branch **must not hit tasks that are simply unscheduled**.
//
// A pair with the previous test: with only "can rescue zombies" and without this one, writing the reclaim condition as
// `next_run_at is null` would make the previous test green, at the cost of **every unscheduled task being claimed and run once**,
// such as one the user just paused, or one stopped because its next fire time cannot be computed.
// The PG side blocks it with `claimed_by is not null`; MemStore blocks it with "is there a lease entry".
func testClaimDoesNotStealNeverClaimedSchedules(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	now := time.Date(2026, 8, 30, 16, 41, 0, 0, time.UTC)
	// active but **never claimed and not scheduled** (empty NextRunAt).
	sch := newTestSchedule("sch_no_next", "")
	if _, err := s.CreateSchedule(ctx, sch); err != nil {
		t.Fatalf("建任务失败：%v", err)
	}

	// It must never be claimed no matter how long passes: it is not a zombie, just unscheduled.
	for _, at := range []time.Time{now, now.Add(time.Hour), now.Add(48 * time.Hour)} {
		claimed, err := s.ClaimDueSchedules(ctx, "instance-A", at, 15*time.Minute, 0)
		if err != nil {
			t.Fatalf("认领出错：%v", err)
		}
		if len(claimed) != 0 {
			t.Fatalf("没排期且从没被认领过的任务不该被领走（%s），实际领到 %d 条",
				apimodel.FormatISO(at), len(claimed))
		}
	}
}

// **This is the test most worth pinning in this implementation.** amad runs as multiple instances in production, and N processes find the same
// task due in the same second. If claiming were "select the due ones, then update one by one", both sides would think they claimed it,
// and users would get N duplicate reports. This bug never appears with a single instance.
//
// MemStore relies on "the whole thing under one write lock", PGStore on a single `update ... where` (PostgreSQL
// locks each row and rechecks where). Two mechanisms, one assertion.
func testClaimIsAtomicUnderConcurrency(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	now := time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC)
	past := apimodel.FormatISO(now.Add(-time.Minute))

	// All 20 are due, and 8 "instances" race for them at once.
	const total = 20
	const instances = 8
	for i := 0; i < total; i++ {
		sch := newTestSchedule(fmt.Sprintf("sch_race_%02d", i), past)
		if _, err := s.CreateSchedule(ctx, sch); err != nil {
			t.Fatalf("建任务失败：%v", err)
		}
	}

	var mu sync.Mutex
	claimedBy := map[string][]string{} // scheduleID → list of instances that claimed it

	var wg sync.WaitGroup
	for i := 0; i < instances; i++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			owner := fmt.Sprintf("instance-%d", k)
			got, err := s.ClaimDueSchedules(ctx, owner, now, 15*time.Minute, 0)
			if err != nil {
				t.Errorf("%s 认领失败：%v", owner, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, sch := range got {
				claimedBy[sch.ID] = append(claimedBy[sch.ID], owner)
			}
		}(i)
	}
	wg.Wait()

	// Each task is claimed by **exactly** one instance: more is a duplicate fire, fewer is a missed run.
	for id, owners := range claimedBy {
		if len(owners) != 1 {
			t.Errorf("任务 %s 被 %d 个实例同时领到（%v）—— 认领不是原子的，"+
				"线上会让用户收到多份重复结果", id, len(owners), owners)
		}
	}
	if len(claimedBy) != total {
		t.Errorf("%d 条到点任务只有 %d 条被领到，剩下的漏跑了", total, len(claimedBy))
	}
}

// testClaimLimit: limit takes effect, and one claim never exceeds it.
func testClaimLimit(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	now := time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC)
	past := apimodel.FormatISO(now.Add(-time.Minute))
	for i := 0; i < 5; i++ {
		if _, err := s.CreateSchedule(ctx, newTestSchedule(fmt.Sprintf("sch_lim_%d", i), past)); err != nil {
			t.Fatalf("建任务失败：%v", err)
		}
	}

	claimed, err := s.ClaimDueSchedules(ctx, "instance-A", now, 15*time.Minute, 2)
	if err != nil {
		t.Fatalf("认领失败：%v", err)
	}
	if len(claimed) != 2 {
		t.Errorf("limit=2 应只领到 2 条，实际 %d", len(claimed))
	}
}

// testClaimPausedOnlyWhenRunNowRequested: both sides of claiming a paused task, **which must be read as a pair**.
//
// One side: paused + try-run clicked → can be claimed. Without it, run-now on paused returns 200 with zero effect
// (nextRunAt updated, the frontend looks like success, while the scheduler never claims it).
//
// The other side: paused + try-run **not** clicked, even with nextRunAt due → cannot be claimed. Without it,
// a fix like "delete the status condition entirely" would also pass the first side, and that would make **every task run once
// the moment it is paused**: the nextRunAt left over from before pausing is still in the past.
//
// Both assertions are in the same test on the same data exactly so that "fixing too much" also goes red.
func testClaimPausedOnlyWhenRunNowRequested(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	now := time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC)
	past := apimodel.FormatISO(now.Add(-time.Minute))

	// paused with try-run clicked: should be claimed.
	tryRun := newTestSchedule("sch_paused_trial", past)
	tryRun.Status = apimodel.SchedulePaused
	tryRun.RunNowRequested = true
	// paused and due, but nobody clicked try-run: **must not** be claimed (this is the "nextRunAt left over from before pausing").
	leftover := newTestSchedule("sch_paused_leftover", past)
	leftover.Status = apimodel.SchedulePaused

	for _, sch := range []apimodel.Schedule{tryRun, leftover} {
		if _, err := s.CreateSchedule(ctx, sch); err != nil {
			t.Fatalf("建 %s 失败：%v", sch.ID, err)
		}
	}

	claimed, err := s.ClaimDueSchedules(ctx, "instance-A", now, 15*time.Minute, 0)
	if err != nil {
		t.Fatalf("认领失败：%v", err)
	}

	var ids []string
	for _, c := range claimed {
		ids = append(ids, c.ID)
	}
	if len(claimed) != 1 || claimed[0].ID != "sch_paused_trial" {
		t.Fatalf("应当**只**领到那条被点了试跑的 paused 任务，实际领到 %d 条：%v\n"+
			"  领不到 sch_paused_trial → run-now 对 paused 零效果（点了什么都没发生）；\n"+
			"  领到 sch_paused_leftover → 每条任务在暂停那一刻都会被立刻跑一次",
			len(claimed), ids)
	}

	// **The try-run mark is one-time**: claiming consumes it. Otherwise, every time someone puts nextRunAt back on this paused task
	// (say by hand, or after another try-run), it would be claimed for another round.
	if claimed[0].RunNowRequested {
		t.Errorf("认领后返回值里的试跑标记应已被消费，实际仍是 true")
	}
	reread, err := s.GetSchedule(ctx, "sch_paused_trial")
	if err != nil {
		t.Fatalf("重读失败：%v", err)
	}
	if reread.RunNowRequested {
		t.Errorf("库里的试跑标记也该被清掉，实际仍是 true —— 否则这条 paused 任务会被反复认领")
	}

	// After consumption, put a due time back: with no try-run mark, this paused task must not be claimed again.
	// **This pins "one click buys only one round"**; otherwise try-run would turn a paused task into a periodic one.
	reread.NextRunAt = past
	if _, err := s.SaveSchedule(ctx, reread); err != nil {
		t.Fatalf("排回到期时刻失败：%v", err)
	}
	if err := s.ReleaseScheduleClaim(ctx, reread.ID); err != nil {
		t.Fatalf("释放租约失败：%v", err)
	}
	again, err := s.ClaimDueSchedules(ctx, "instance-A", now.Add(time.Second), 15*time.Minute, 0)
	if err != nil {
		t.Fatalf("二次认领出错：%v", err)
	}
	if len(again) != 0 {
		t.Errorf("试跑标记已消费，paused 的任务不该再被领到，实际领到 %d 条 —— "+
			"一次试跑只该换来一轮", len(again))
	}
}

// testClaimRunNowStillNeedsDueTime: the try-run mark must not let a task that is "not due yet" be claimed.
//
// The claim condition is "(active or try-run) **and** due"; the two are combined with and. Written as or, a task with a leftover
// try-run mark would be claimed right away, while an empty nextRunAt already means "there is no pending fire".
func testClaimRunNowStillNeedsDueTime(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	now := time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC)

	// Try-run clicked, but nextRunAt is empty (pausing clears it).
	noNext := newTestSchedule("sch_runnow_nonext", "")
	noNext.Status = apimodel.SchedulePaused
	noNext.RunNowRequested = true
	// Try-run clicked, but the fire is in the future.
	future := newTestSchedule("sch_runnow_future", apimodel.FormatISO(now.Add(time.Hour)))
	future.Status = apimodel.SchedulePaused
	future.RunNowRequested = true

	for _, sch := range []apimodel.Schedule{noNext, future} {
		if _, err := s.CreateSchedule(ctx, sch); err != nil {
			t.Fatalf("建 %s 失败：%v", sch.ID, err)
		}
	}

	claimed, err := s.ClaimDueSchedules(ctx, "instance-A", now, 15*time.Minute, 0)
	if err != nil {
		t.Fatalf("认领失败：%v", err)
	}
	if len(claimed) != 0 {
		var ids []string
		for _, c := range claimed {
			ids = append(ids, c.ID)
		}
		t.Errorf("试跑标记要与「已到期的 nextRunAt」成对才生效，实际领到 %d 条：%v", len(claimed), ids)
	}
}

// testScheduleRunNowFlagRoundTrips: the try-run mark must be stored and read back.
//
// On the PG side it is **a separate column** (not the copy in doc), so this pins "the column is really written,
// and really read back". Missing the write is the worst: run-now looks successful while that column in the database is false,
// so claiming still cannot get it, exactly the same symptom as before the fix.
func testScheduleRunNowFlagRoundTrips(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	// Created with the mark already set (the insert of CreateSchedule must cover it).
	born := newTestSchedule("sch_flag_born", apimodel.NowISO())
	born.RunNowRequested = true
	if _, err := s.CreateSchedule(ctx, born); err != nil {
		t.Fatalf("建任务失败：%v", err)
	}
	got, err := s.GetSchedule(ctx, born.ID)
	if err != nil {
		t.Fatalf("查任务失败：%v", err)
	}
	if !got.RunNowRequested {
		t.Errorf("建的时候带的试跑标记没存住 —— PG 那侧它是独立一列，insert 漏了它")
	}

	// The list path must carry it too (the frontend row uses it to show "about to run").
	list, err := s.ListSchedules(ctx, "ws_demo")
	if err != nil {
		t.Fatalf("列任务失败：%v", err)
	}
	if len(list) != 1 || !list[0].RunNowRequested {
		t.Errorf("列表路径也要读出试跑标记，实际 %+v", list)
	}

	// The update of SaveSchedule must cover it; test both directions.
	got.RunNowRequested = false
	if _, err := s.SaveSchedule(ctx, got); err != nil {
		t.Fatalf("覆盖写失败：%v", err)
	}
	if reread, err := s.GetSchedule(ctx, born.ID); err != nil {
		t.Fatalf("重读失败：%v", err)
	} else if reread.RunNowRequested {
		t.Errorf("把标记改回 false 没生效")
	}

	// The default must be false: a task created without the mark must not "come with a try-run",
	// otherwise every paused task would run once right away.
	plain := newTestSchedule("sch_flag_default", apimodel.NowISO())
	if _, err := s.CreateSchedule(ctx, plain); err != nil {
		t.Fatalf("建任务失败：%v", err)
	}
	if p, err := s.GetSchedule(ctx, plain.ID); err != nil {
		t.Fatalf("查任务失败：%v", err)
	} else if p.RunNowRequested {
		t.Errorf("没点试跑的任务，标记默认必须是 false")
	}
}

// testSchedulesForAgent looks up the tasks under an agent, used for cascade cleanup when the agent is deleted.
// testReleaseClaimAllowsReclaim: after the lease is released, the same task can be claimed again right away.
//
// **This guards against a very hidden consequence**: the lease is 15 minutes, and if a round does not release it when done, any task with an interval shorter than
// the lease ("sweep every 5 minutes") cannot be claimed before then. It shows as **the interval quietly stretched to
// 15 minutes**, with nothing visible in the logs, and next_run_at in the UI is right too.
// This bug was caught by TestSchedulerAutoPausesAfterFailures: five ticks in a row ran only the first one.
func testReleaseClaimAllowsReclaim(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	now := time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC)
	past := apimodel.FormatISO(now.Add(-time.Minute))
	sch := newTestSchedule("sch_release", past)
	if _, err := s.CreateSchedule(ctx, sch); err != nil {
		t.Fatalf("建任务失败：%v", err)
	}

	claimed, err := s.ClaimDueSchedules(ctx, "instance-A", now, 15*time.Minute, 0)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("首次认领应成功：err=%v n=%d", err, len(claimed))
	}

	// Simulate a round finishing: put the next fire time back, then release the lease (**in exactly the order of the production code**).
	done := claimed[0]
	done.NextRunAt = past // due again (interval shorter than the lease)
	if _, err := s.SaveSchedule(ctx, done); err != nil {
		t.Fatalf("排回下次触发失败：%v", err)
	}
	if err := s.ReleaseScheduleClaim(ctx, done.ID); err != nil {
		t.Fatalf("释放租约失败：%v", err)
	}

	// The lease is far from expiring, but it was released, so it should be claimable right away.
	again, err := s.ClaimDueSchedules(ctx, "instance-A", now.Add(time.Second), 15*time.Minute, 0)
	if err != nil {
		t.Fatalf("二次认领出错：%v", err)
	}
	if len(again) != 1 {
		t.Fatalf("释放租约后应能立刻再认领，实际领到 %d 条 —— "+
			"否则间隔短于租约的任务会被悄悄拉长到租约时长", len(again))
	}

	// Idempotent: releasing something missing, or releasing twice, is not an error.
	if err := s.ReleaseScheduleClaim(ctx, "sch_nope"); err != nil {
		t.Errorf("释放不存在的租约应当幂等，实际 %v", err)
	}
}

func testSchedulesForAgent(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	mine := newTestSchedule("sch_mine", apimodel.NowISO())
	other := newTestSchedule("sch_other", apimodel.NowISO())
	other.AgentID = "agent_analyst"
	for _, sch := range []apimodel.Schedule{mine, other} {
		if _, err := s.CreateSchedule(ctx, sch); err != nil {
			t.Fatalf("建任务失败：%v", err)
		}
	}

	ids, err := s.SchedulesForAgent(ctx, "agent_triage")
	if err != nil {
		t.Fatalf("反查失败：%v", err)
	}
	if len(ids) != 1 || ids[0] != "sch_mine" {
		t.Errorf("反查结果不对：%v", ids)
	}

	// An agent with no tasks must return an empty slice, not nil (lists are always non-nil, see the Store interface comment).
	none, err := s.SchedulesForAgent(ctx, "agent_nobody")
	if err != nil {
		t.Fatalf("反查空结果失败：%v", err)
	}
	if none == nil {
		t.Error("空结果应是长度 0 的切片而不是 nil")
	}
}

// testDeleteWorkspaceCascadesSchedules: deleting a workspace must clean up its scheduled tasks too.
//
// Otherwise they become dangling data, and **fail once on every scan cycle**:
// each time really creating an Ark session and burning tokens.
func testDeleteWorkspaceCascadesSchedules(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	if _, err := s.CreateSchedule(ctx, newTestSchedule("sch_cascade", apimodel.NowISO())); err != nil {
		t.Fatalf("建任务失败：%v", err)
	}
	if err := s.DeleteWorkspace(ctx, "ws_demo"); err != nil {
		t.Fatalf("删空间失败：%v", err)
	}
	left, err := s.ListSchedules(ctx, "ws_demo")
	if err != nil {
		t.Fatalf("列任务失败：%v", err)
	}
	if len(left) != 0 {
		t.Errorf("删空间后应无残留任务，实际 %d 条", len(left))
	}
	if _, err := s.GetSchedule(ctx, "sch_cascade"); err != core.ErrNotFound {
		t.Errorf("任务本体也该没了，实际 %v", err)
	}
}
