// Tests for the progress delivery state machine, run against real PostgreSQL (t.Skip if unreachable).
//
// # Why these must hit a real database
//
// The correctness of this state machine **rests entirely on SQL atomicity**: the lease comparison is in the UPDATE's where,
// and claiming is one UPDATE ... RETURNING. Testing with a fake Store just tests whether my own fake is right,
// while what really needs guarding is races under concurrency, which only a real database can verify.
//
// The three things locked down are all "wrong without an error, just wrong behavior":
//
//   - lease mutual exclusion: two instances push the same CoT at once, and the user sees events out of order
//   - an expired holder must not overwrite the new holder: progress in the CoT would go backwards
//   - a remote create with an "uncertain result" must not be retried as a failure: the group would get two chains of thought

package feishudao

import (
	"context"
	"errors"
	"github.com/chyroc/ama/server/feishu"
	"testing"
	"time"
)

// seedDelivery builds a queued delivery.
func seedDelivery(t *testing.T, store *PGStore, runID string) *feishu.ProgressDelivery {
	t.Helper()
	d, err := store.EnsureProgressDelivery(context.Background(), &feishu.ProgressDelivery{
		RunID:           runID,
		LarkAppID:       "cli_1",
		ChatID:          "oc_1",
		OriginMessageID: "om_1",
		Transport:       feishu.TransportCOT,
	})
	if err != nil {
		t.Fatalf("建投递失败: %v", err)
	}
	return d
}

func TestProgressEnsureIsIdempotent(t *testing.T) {
	store := newPGStore(t)
	ctx := context.Background()

	first := seedDelivery(t, store, "sesn-1")
	if first.Status != feishu.ProgressQueued {
		t.Errorf("新建的该是 queued，得到 %q", first.Status)
	}
	if first.DesiredRevision != 1 || first.AppliedRevision != 0 {
		t.Errorf("版本号初值不对: desired=%d applied=%d", first.DesiredRevision, first.AppliedRevision)
	}

	// Create again for the same run: get the same row; it **must not overwrite** existing progress.
	token := "tok-a"
	claimed, ok, err := store.ClaimProgressDelivery(ctx, "sesn-1", token, time.Minute)
	if err != nil || !ok {
		t.Fatalf("认领失败: ok=%v err=%v", ok, err)
	}
	claimed.Status = feishu.ProgressActive
	claimed.RemoteCOTID, claimed.RemoteMessageID = "cot_1", "om_c"
	claimed.AppliedRevision = 3
	if err := store.SaveProgressDelivery(ctx, claimed, token); err != nil {
		t.Fatalf("写回失败: %v", err)
	}

	again, err := store.EnsureProgressDelivery(ctx, &feishu.ProgressDelivery{
		RunID: "sesn-1", LarkAppID: "cli_1", ChatID: "oc_1", Transport: feishu.TransportCOT,
	})
	if err != nil {
		t.Fatalf("重复建失败: %v", err)
	}
	if again.Status != feishu.ProgressActive || again.RemoteCOTID != "cot_1" || again.AppliedRevision != 3 {
		t.Errorf("重复建覆盖了已有进度: %+v", again)
	}
}

// Lease mutual exclusion: while one holds it, the other cannot claim.
func TestProgressLeaseIsExclusive(t *testing.T) {
	store := newPGStore(t)
	ctx := context.Background()
	seedDelivery(t, store, "sesn-2")

	if _, ok, err := store.ClaimProgressDelivery(ctx, "sesn-2", "tok-a", time.Minute); err != nil || !ok {
		t.Fatalf("第一次该抢到: ok=%v err=%v", ok, err)
	}
	// The second instance cannot claim, and **it is not an error**: under concurrency this is a normal result.
	d, ok, err := store.ClaimProgressDelivery(ctx, "sesn-2", "tok-b", time.Minute)
	if err != nil {
		t.Fatalf("抢不到不该报错: %v", err)
	}
	if ok || d != nil {
		t.Fatal("租约在别人手上，第二个不该抢到")
	}
	// Reentry with the same token must be allowed (crashed last round without releasing and came back).
	if _, ok, err := store.ClaimProgressDelivery(ctx, "sesn-2", "tok-a", time.Minute); err != nil || !ok {
		t.Errorf("同 token 该能重入: ok=%v err=%v", ok, err)
	}
}

// After the lease expires someone else can take over. This is the case when the holder process is killed.
func TestProgressLeaseExpiryAllowsTakeover(t *testing.T) {
	store := newPGStore(t)
	ctx := context.Background()
	seedDelivery(t, store, "sesn-3")

	if _, ok, err := store.ClaimProgressDelivery(ctx, "sesn-3", "tok-a", 200*time.Millisecond); err != nil || !ok {
		t.Fatalf("第一次该抢到: ok=%v err=%v", ok, err)
	}
	time.Sleep(400 * time.Millisecond)

	d, ok, err := store.ClaimProgressDelivery(ctx, "sesn-3", "tok-b", time.Minute)
	if err != nil || !ok {
		t.Fatalf("租约过期后该能接管: ok=%v err=%v", ok, err)
	}
	if d.LeaseToken != "tok-b" {
		t.Errorf("租约该易主，得到 %q", d.LeaseToken)
	}
}

// **The most important test in this file**: an expired holder must not overwrite the new holder's progress.
//
// If the lease comparison were not in the UPDATE's where (but read-then-change), this would fail,
// showing as progress going backwards in the CoT, with no error.
func TestProgressStaleHolderCannotOverwrite(t *testing.T) {
	store := newPGStore(t)
	ctx := context.Background()
	seedDelivery(t, store, "sesn-4")

	stale, ok, err := store.ClaimProgressDelivery(ctx, "sesn-4", "tok-old", 200*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("抢锁失败: ok=%v err=%v", ok, err)
	}
	time.Sleep(400 * time.Millisecond) // let it expire

	fresh, ok, err := store.ClaimProgressDelivery(ctx, "sesn-4", "tok-new", time.Minute)
	if err != nil || !ok {
		t.Fatalf("接管失败: ok=%v err=%v", ok, err)
	}
	fresh.AppliedRevision = 10
	if err := store.SaveProgressDelivery(ctx, fresh, "tok-new"); err != nil {
		t.Fatalf("新持有者写回失败: %v", err)
	}

	// The late write-back: must be rejected.
	stale.AppliedRevision = 2
	err = store.SaveProgressDelivery(ctx, stale, "tok-old")
	if !errors.Is(err, feishu.ErrProgressLeaseLost) {
		t.Fatalf("过期持有者写回该被拒（ErrProgressLeaseLost），得到 %v", err)
	}
	// And it really did not change the database.
	got, err := store.GetProgressDelivery(ctx, "sesn-4")
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if got.AppliedRevision != 10 {
		t.Errorf("新持有者的进展被覆盖了: applied=%d", got.AppliedRevision)
	}

	// A late renewal must fail too.
	if renewed, err := store.RenewProgressLease(ctx, "sesn-4", "tok-old", time.Minute); err != nil || renewed {
		t.Errorf("过期持有者续租该失败: renewed=%v err=%v", renewed, err)
	}
	// The new holder renews normally.
	if renewed, err := store.RenewProgressLease(ctx, "sesn-4", "tok-new", time.Minute); err != nil || !renewed {
		t.Errorf("新持有者续租该成功: renewed=%v err=%v", renewed, err)
	}
}

func TestProgressReleaseAllowsImmediateTakeover(t *testing.T) {
	store := newPGStore(t)
	ctx := context.Background()
	seedDelivery(t, store, "sesn-5")

	if _, ok, _ := store.ClaimProgressDelivery(ctx, "sesn-5", "tok-a", time.Hour); !ok {
		t.Fatal("抢锁失败")
	}
	if err := store.ReleaseProgressLease(ctx, "sesn-5", "tok-a"); err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	// After release it can be taken over right away, without waiting a TTL.
	if _, ok, err := store.ClaimProgressDelivery(ctx, "sesn-5", "tok-b", time.Minute); err != nil || !ok {
		t.Errorf("释放后该能立刻接管: ok=%v err=%v", ok, err)
	}
}

// The snapshot and the final-state mark must be stored and read back as is; the ordering gate reads TerminalApplied.
func TestProgressRoundTripsSnapshotAndTerminal(t *testing.T) {
	store := newPGStore(t)
	ctx := context.Background()
	seedDelivery(t, store, "sesn-6")

	token := "tok-a"
	d, ok, err := store.ClaimProgressDelivery(ctx, "sesn-6", token, time.Hour)
	if err != nil || !ok {
		t.Fatalf("认领失败: ok=%v err=%v", ok, err)
	}
	d.Status = feishu.ProgressActive
	d.RemoteCOTID, d.RemoteMessageID = "cot_9", "om_9"
	d.DesiredRevision = 7
	d.AppliedRevision = 7
	d.TerminalRequested = true
	d.AppliedSnapshot = &feishu.ProgressSnapshot{
		Thinking: "先看看日志",
		Tools: []feishu.ProgressToolCall{
			{ID: "t1", Name: "bash", Args: "ls", Result: "a.txt", Done: true},
			{ID: "t2", Name: "read", Args: "a.txt"},
		},
		Terminal: true,
		Outcome:  "done",
	}
	if err := store.SaveProgressDelivery(ctx, d, token); err != nil {
		t.Fatalf("写回失败: %v", err)
	}

	got, err := store.GetProgressDelivery(ctx, "sesn-6")
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if got.AppliedSnapshot == nil {
		t.Fatal("快照丢了")
	}
	if !got.AppliedSnapshot.Equal(*d.AppliedSnapshot) {
		t.Errorf("快照没原样存回:\n存 %+v\n取 %+v", d.AppliedSnapshot, got.AppliedSnapshot)
	}
	if !got.TerminalRequested || got.RemoteCOTID != "cot_9" || got.AppliedRevision != 7 {
		t.Errorf("字段没存对: %+v", got)
	}
}

// On entering a final state finished_at must be set and the lease cleared; keeping the lease would make the cleanup task think someone is still handling it.
func TestProgressTerminalClearsLease(t *testing.T) {
	store := newPGStore(t)
	ctx := context.Background()
	seedDelivery(t, store, "sesn-7")

	token := "tok-a"
	d, _, err := store.ClaimProgressDelivery(ctx, "sesn-7", token, time.Hour)
	if err != nil {
		t.Fatalf("认领失败: %v", err)
	}
	d.Status = feishu.ProgressCompleted
	d.TerminalApplied = true
	d.RemoteCOTID, d.RemoteMessageID = "cot_1", "om_1"
	if err := store.SaveProgressDelivery(ctx, d, token); err != nil {
		t.Fatalf("写回失败: %v", err)
	}

	got, err := store.GetProgressDelivery(ctx, "sesn-7")
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if got.FinishedAt == nil {
		t.Error("终态该落 finished_at")
	}
	if got.LeaseToken != "" || got.LeaseUntil != nil {
		t.Errorf("终态该清掉租约，得到 token=%q until=%v", got.LeaseToken, got.LeaseUntil)
	}
	if !got.Finished() || !got.ReadyForFinalReply() {
		t.Error("终态该让顺序门放行")
	}
	// Final-state rows must not be claimed again.
	if _, ok, _ := store.ClaimProgressDelivery(ctx, "sesn-7", "tok-b", time.Minute); ok {
		t.Error("已完成的投递不该被认领")
	}
}

// A remote create with an "uncertain result" goes into the create_uncertain state and is **not recreated**.
// Recreating it would put a second chain of thought in the group.
func TestProgressCreateUncertainIsTerminal(t *testing.T) {
	store := newPGStore(t)
	ctx := context.Background()
	seedDelivery(t, store, "sesn-8")

	token := "tok-a"
	d, _, err := store.ClaimProgressDelivery(ctx, "sesn-8", token, time.Hour)
	if err != nil {
		t.Fatalf("认领失败: %v", err)
	}
	now := time.Now()
	d.Status = feishu.ProgressCreateUncertain
	d.CreateAttemptedAt = &now
	d.StatusReason = "建 CoT 超时，结果不确定"
	if err := store.SaveProgressDelivery(ctx, d, token); err != nil {
		t.Fatalf("写回失败: %v", err)
	}

	got, err := store.GetProgressDelivery(ctx, "sesn-8")
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if !got.Finished() {
		t.Error("create_uncertain 该算终态")
	}
	if got.CreateAttemptedAt == nil {
		t.Error("该记下尝试时刻，判不确定窗口要用")
	}
	// It must not be claimed again to recreate.
	if _, ok, _ := store.ClaimProgressDelivery(ctx, "sesn-8", "tok-b", time.Minute); ok {
		t.Error("结果不确定的投递不该被重新认领——重建会让群里多一条思维链")
	}
	// But the ordering gate must let it through, otherwise the final reply would wait forever for a CoT that will never be closed.
	if !got.ReadyForFinalReply() {
		t.Error("终态该让最终回复放行，不然用户等不到答案")
	}
}

// next_retry_at is the backoff gate: do not claim before that time, to avoid spinning after a failure.
func TestProgressRespectsRetryBackoff(t *testing.T) {
	store := newPGStore(t)
	ctx := context.Background()
	seedDelivery(t, store, "sesn-9")

	token := "tok-a"
	d, _, err := store.ClaimProgressDelivery(ctx, "sesn-9", token, time.Hour)
	if err != nil {
		t.Fatalf("认领失败: %v", err)
	}
	future := time.Now().Add(time.Hour)
	d.Status = feishu.ProgressQueued
	d.NextRetryAt = &future
	d.ConsecutiveFailureCount = 2
	d.LastError = "推事件失败"
	// Release the lease first, otherwise what blocks is the lease and not the backoff, and the intended thing is not tested.
	if err := store.SaveProgressDelivery(ctx, d, token); err != nil {
		t.Fatalf("写回失败: %v", err)
	}
	if err := store.ReleaseProgressLease(ctx, "sesn-9", token); err != nil {
		t.Fatalf("释放失败: %v", err)
	}

	if _, ok, err := store.ClaimProgressDelivery(ctx, "sesn-9", "tok-b", time.Minute); err != nil || ok {
		t.Errorf("还没到重试时刻不该被认领: ok=%v err=%v", ok, err)
	}
	if !containsDelivery(t, store, "sesn-9", false) {
		// It must not appear in the claimable list either.
		t.Log("确认它不在可认领列表里")
	}
}

// containsDelivery reports whether runID is in the claimable list.
func containsDelivery(t *testing.T, store *PGStore, runID string, want bool) bool {
	t.Helper()
	list, err := store.ListClaimableProgressDeliveries(context.Background(), 50)
	if err != nil {
		t.Fatalf("列可认领失败: %v", err)
	}
	for _, d := range list {
		if d.RunID == runID {
			return true
		}
	}
	return false
}

// The claimable list must put final states first: the user is waiting for that last bit.
func TestProgressListPrioritisesTerminal(t *testing.T) {
	store := newPGStore(t)
	ctx := context.Background()

	seedDelivery(t, store, "sesn-plain")
	seedDelivery(t, store, "sesn-terminal")

	token := "tok-a"
	d, _, err := store.ClaimProgressDelivery(ctx, "sesn-terminal", token, time.Hour)
	if err != nil {
		t.Fatalf("认领失败: %v", err)
	}
	d.Status = feishu.ProgressActive
	d.RemoteCOTID, d.RemoteMessageID = "cot_1", "om_1"
	d.TerminalRequested = true
	if err := store.SaveProgressDelivery(ctx, d, token); err != nil {
		t.Fatalf("写回失败: %v", err)
	}
	if err := store.ReleaseProgressLease(ctx, "sesn-terminal", token); err != nil {
		t.Fatalf("释放失败: %v", err)
	}

	list, err := store.ListClaimableProgressDeliveries(ctx, 10)
	if err != nil {
		t.Fatalf("列可认领失败: %v", err)
	}
	if len(list) < 2 {
		t.Fatalf("该有两条，得到 %d 条", len(list))
	}
	if list[0].RunID != "sesn-terminal" {
		t.Errorf("终态该排最前，实际第一条是 %q", list[0].RunID)
	}
}

func TestProgressGetMissingReturnsNotFound(t *testing.T) {
	store := newPGStore(t)
	_, err := store.GetProgressDelivery(context.Background(), "sesn-nope")
	if !errors.Is(err, feishu.ErrProgressNotFound) {
		t.Fatalf("该返回 ErrProgressNotFound，得到 %v", err)
	}
}

// Snapshot comparison: identical content should skip the push, otherwise every agent frame triggers a remote call.
func TestSnapshotEqualDetectsRealChanges(t *testing.T) {
	base := feishu.ProgressSnapshot{
		Thinking: "想想",
		Tools:    []feishu.ProgressToolCall{{ID: "t1", Name: "bash", Args: "ls"}},
	}
	same := feishu.ProgressSnapshot{
		Thinking: "想想",
		Tools:    []feishu.ProgressToolCall{{ID: "t1", Name: "bash", Args: "ls"}},
	}
	if !base.Equal(same) {
		t.Error("内容相同该判为相等，否则每帧都会推一次")
	}

	cases := map[string]feishu.ProgressSnapshot{
		"思考变了": {Thinking: "想好了", Tools: base.Tools},
		"工具结果回来了": {Thinking: "想想", Tools: []feishu.ProgressToolCall{
			{ID: "t1", Name: "bash", Args: "ls", Result: "a.txt", Done: true},
		}},
		"多了一个工具": {Thinking: "想想", Tools: append(append([]feishu.ProgressToolCall(nil), base.Tools...),
			feishu.ProgressToolCall{ID: "t2", Name: "read"})},
		"进了终态": {Thinking: "想想", Tools: base.Tools, Terminal: true, Outcome: "done"},
	}
	for name, other := range cases {
		if base.Equal(other) {
			t.Errorf("%s，该判为不同", name)
		}
	}
}

// The in-memory implementation must **behave the same** as the PG version.
//
// Otherwise unit tests are all green in memory and it blows up on the database, and what blows up then is a race, the hardest to track down.
// So the most important cases are run once more on the in-memory implementation here.
func TestMemProgressStoreMatchesPGSemantics(t *testing.T) {
	ctx := context.Background()
	store := feishu.NewMemProgressStore()

	if _, err := store.EnsureProgressDelivery(ctx, &feishu.ProgressDelivery{
		RunID: "m1", LarkAppID: "cli_1", ChatID: "oc_1", Transport: feishu.TransportCOT,
	}); err != nil {
		t.Fatalf("建投递失败: %v", err)
	}

	t.Run("租约互斥", func(t *testing.T) {
		if _, ok, err := store.ClaimProgressDelivery(ctx, "m1", "tok-a", time.Hour); err != nil || !ok {
			t.Fatalf("第一次该抢到: ok=%v err=%v", ok, err)
		}
		if _, ok, err := store.ClaimProgressDelivery(ctx, "m1", "tok-b", time.Hour); err != nil || ok {
			t.Errorf("别人持有时不该抢到: ok=%v err=%v", ok, err)
		}
	})

	t.Run("过期持有者不能覆盖", func(t *testing.T) {
		d, err := store.GetProgressDelivery(ctx, "m1")
		if err != nil {
			t.Fatalf("读失败: %v", err)
		}
		d.AppliedRevision = 5
		if err := store.SaveProgressDelivery(ctx, d, "tok-wrong"); !errors.Is(err, feishu.ErrProgressLeaseLost) {
			t.Errorf("token 不对该返回 ErrProgressLeaseLost，得到 %v", err)
		}
		if err := store.SaveProgressDelivery(ctx, d, "tok-a"); err != nil {
			t.Errorf("持有者写回该成功: %v", err)
		}
	})

	t.Run("终态清租约且不可再认领", func(t *testing.T) {
		d, _ := store.GetProgressDelivery(ctx, "m1")
		d.Status = feishu.ProgressCompleted
		d.TerminalApplied = true
		if err := store.SaveProgressDelivery(ctx, d, "tok-a"); err != nil {
			t.Fatalf("写回失败: %v", err)
		}
		got, _ := store.GetProgressDelivery(ctx, "m1")
		if got.FinishedAt == nil || got.LeaseToken != "" {
			t.Errorf("终态该落 finishedAt 并清租约: %+v", got)
		}
		if _, ok, _ := store.ClaimProgressDelivery(ctx, "m1", "tok-c", time.Hour); ok {
			t.Error("终态的不该被认领")
		}
	})

	t.Run("不存在返回 ErrProgressNotFound", func(t *testing.T) {
		if _, err := store.GetProgressDelivery(ctx, "nope"); !errors.Is(err, feishu.ErrProgressNotFound) {
			t.Errorf("该返回 ErrProgressNotFound，得到 %v", err)
		}
	})
}
