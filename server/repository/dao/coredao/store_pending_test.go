// Contract tests for queued messages waiting to be sent.
//
// **Both implementations must run them**: MemStore all green while PG behaves differently is a pattern this repo has hit again and again
// (the AppendRunEvent dedup and both ClaimDueSchedules pitfalls were found this way),
// and drift in this group means **the same message is sent twice**, which the user sees directly.

package coredao

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/model/apimodel"
)

// seedPendingRun builds a conversation that messages can be queued on.
func seedPendingRun(t *testing.T, s core.Store, id string) {
	t.Helper()
	if _, err := s.CreateRun(context.Background(), apimodel.Run{
		ID: id, WorkspaceID: "ws_demo", AgentID: "agent_triage",
		Status: apimodel.RunRunning, Trigger: apimodel.TriggerManual,
		StartedAt: apimodel.NowISO(),
	}); err != nil {
		t.Fatalf("造 run 失败：%v", err)
	}
}

// testPendingFIFO: first in, first out, and **claim order** matches list order.
//
// ⚠️ The queue order is deliberately the **reverse** of the expected order: if queued in order, "sorting" already-sorted input
// is the identity, and flipping the comparison would still pass (this repo's lesson "a stable sort hides a missing tie-breaker").
func testPendingFIFO(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)
	seedPendingRun(t, s, "run-fifo-contract")

	want := []string{"甲", "乙", "丙"}
	base := time.Now().UTC()
	for i := len(want) - 1; i >= 0; i-- {
		if err := s.EnqueuePendingMessage(ctx, apimodel.PendingMessage{
			ID: "pm-" + want[i], RunID: "run-fifo-contract", Text: want[i],
			Status: apimodel.PendingWaiting, Source: apimodel.PendingFromWeb,
		}, base.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("入队失败：%v", err)
		}
	}

	msgs, err := s.ListPendingMessages(ctx, "run-fifo-contract")
	if err != nil {
		t.Fatalf("列排队消息失败：%v", err)
	}
	for i, m := range msgs {
		if m.Text != want[i] {
			t.Errorf("列表第 %d 条应为 %q，实际 %q", i+1, want[i], m.Text)
		}
	}

	for i, expect := range want {
		m, ok, err := s.ClaimNextPendingMessage(
			ctx, "run-fifo-contract", "pod", time.Now(), time.Minute)
		if err != nil || !ok {
			t.Fatalf("第 %d 次认领失败：ok=%v err=%v", i+1, ok, err)
		}
		if m.Text != expect {
			t.Errorf("第 %d 次认领应为 %q，实际 %q —— 顺序错了用户会立刻发现",
				i+1, expect, m.Text)
		}
		if err := s.DeletePendingMessage(ctx, m.ID); err != nil {
			t.Fatalf("删排队消息失败：%v", err)
		}
	}
}

// testPendingClaimIsAtomic: concurrent claims on the same message; only one may win.
//
// **This test only means something on real PG**: `for update skip locked` guards exactly against
// the EvalPlanQual recheck under READ COMMITTED, while the MemStore version satisfies it naturally with one big lock.
// Both run it to keep the behavior the same, not because both can go wrong.
//
// Losing to someone else is not an error, so there must be exactly one winner; one more means the same message is sent twice.
func testPendingClaimIsAtomic(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)
	seedPendingRun(t, s, "run-atomic-contract")

	if err := s.EnqueuePendingMessage(ctx, apimodel.PendingMessage{
		ID: "pm-only", RunID: "run-atomic-contract", Text: "只该被发一次",
		Status: apimodel.PendingWaiting, Source: apimodel.PendingFromWeb,
	}, time.Now().UTC()); err != nil {
		t.Fatalf("入队失败：%v", err)
	}

	const racers = 8

	// ⚠️ **Warm up the connection pool first, otherwise this test is fake.**
	//
	// Without warming, the first thing the 8 goroutines do is each open a PG connection (milliseconds,
	// and staggered), so the first one to get a connection finishes claiming long before the others even connect.
	// **No concurrency actually happens**, and the test degrades into 8 serial runs.
	//
	// This is not a guess: with `for update skip locked` and the outer EPQ recheck **all removed**,
	// it still gets WINS=1 without warming (test all green), and WINS=5 right after warming. That is, the unwarmed
	// version cannot detect "concurrent double claim" at all, the one thing it is supposed to guard against.
	var warm sync.WaitGroup
	for i := 0; i < racers; i++ {
		warm.Add(1)
		go func() {
			defer warm.Done()
			_, _ = s.ListPendingMessages(ctx, "run-atomic-contract")
		}()
	}
	warm.Wait()

	// Starting line: make the 8 goroutines really start at the same time, not one by one in a loop.
	start := make(chan struct{})
	var mu sync.Mutex
	wins := 0
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, ok, err := s.ClaimNextPendingMessage(
				ctx, "run-atomic-contract", "pod", time.Now(), time.Minute)
			if err != nil {
				t.Errorf("认领出错：%v", err)
				return
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Errorf("%d 个实例同时抢，只该有 1 个认领到，实际 %d 个 —— "+
			"多认一次就是同一句话被 agent 回答两遍", racers, wins)
	}
}

// testPendingFailedNotClaimable: messages marked failed are not claimed again.
//
// This is the check for "no infinite retries": without it, a message that can never be sent would be picked
// at the end of every turn, hitting Ark for nothing each time.
func testPendingFailedNotClaimable(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)
	seedPendingRun(t, s, "run-failed-contract")

	if err := s.EnqueuePendingMessage(ctx, apimodel.PendingMessage{
		ID: "pm-bad", RunID: "run-failed-contract", Text: "发不出去",
		Status: apimodel.PendingWaiting, Source: apimodel.PendingFromWeb,
	}, time.Now().UTC()); err != nil {
		t.Fatalf("入队失败：%v", err)
	}
	if err := s.FailPendingMessage(ctx, "pm-bad", "agent 已停用"); err != nil {
		t.Fatalf("标记失败出错：%v", err)
	}

	if _, ok, err := s.ClaimNextPendingMessage(
		ctx, "run-failed-contract", "pod", time.Now(), time.Minute); err != nil {
		t.Fatalf("认领出错：%v", err)
	} else if ok {
		t.Error("failed 的消息不该再被认领——否则每一轮结束都会白试一次")
	}

	// But **the original text must be kept**: the user must be able to see "this one was not sent" and decide to resend or rewrite it.
	msgs, err := s.ListPendingMessages(ctx, "run-failed-contract")
	if err != nil {
		t.Fatalf("列排队消息失败：%v", err)
	}
	if len(msgs) != 1 || msgs[0].Status != apimodel.PendingFailed {
		t.Errorf("failed 的消息要留在列表里给用户看，实际 %+v", msgs)
	}
	if msgs[0].LastError == "" {
		t.Error("要留一句原因，否则用户不知道为什么没发出去")
	}
}
