// Tests for server-side queuing.
//
// They watch four things that **report no error when wrong**:
//
//   - reordering: two messages the user queued are sent in reverse order; they notice right away,
//     but the code does not show it;
//   - duplicate sends: several instances each take once, and the agent answers the same message
//     twice (directly visible to the user);
//   - silent drops: the queue is full but says nothing; the user thinks it was sent and waits for
//     a reply that never comes;
//   - endless loop: a message that can never be sent is retried at the end of every turn.

package core

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/gopkg/arkframe"
)

// newPendingRun builds a session that can queue (running state, agent healthy).
func newPendingRun(t *testing.T, svc *Service, id string) apimodel.Run {
	t.Helper()
	run := apimodel.Run{
		ID: id, WorkspaceID: seedWS, AgentID: "agent_triage",
		Status: apimodel.RunRunning, Trigger: apimodel.TriggerManual,
		StartedAt: apimodel.NowISO(),
	}
	if _, err := svc.store.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("造 run 失败：%v", err)
	}
	return run
}

// TestPendingMessagesAreFIFO: queued messages are sent in enqueue order, first in first out.
//
// # Order is a contract, not an internal detail
//
// The user sees "message 2/3" in the UI, and would notice a reorder right away. But the code
// **does not show it**: an implementation that uses the queue as a stack still sends all three.
//
// So we check both list order and **take order**; they are two different things: the list could
// be ordered correctly while the take grabs the last one.
func TestPendingMessagesAreFIFO(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newChatService(t)
	run := newPendingRun(t, svc, "run-fifo")

	/*
	 * ⚠️ **The enqueue order must differ from the expected order**, otherwise this test proves nothing.
	 *
	 * The first version enqueued in the order of want, so the SliceStable sort in MemStore
	 * was an identity transform on **already sorted** input: reversing the comparison (last in first out) still passed.
	 * That is exactly the shape of this repo's known trap, "a stable sort hides the same-key tiebreak from tests".
	 *
	 * So here createdAt runs **opposite** to the append order: enqueue directly through Store with given times,
	 * so the sort really has to do work to get it right.
	 */
	want := []string{"第一句", "第二句", "第三句"}
	base := time.Now().UTC()
	for i := len(want) - 1; i >= 0; i-- {
		m := apimodel.PendingMessage{
			ID: "pmsg-" + want[i], RunID: run.ID, Text: want[i],
			Status: apimodel.PendingWaiting, Source: apimodel.PendingFromWeb,
		}
		// Append in reverse, but give createdAt in forward order: the sort must use createdAt, not insertion order.
		if err := svc.store.EnqueuePendingMessage(
			ctx, m, base.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("入队失败：%v", err)
		}
	}

	// 1. List order
	msgs, err := svc.ListPendingMessages(ctx, run.ID)
	if err != nil {
		t.Fatalf("列排队消息失败：%v", err)
	}
	if len(msgs) != len(want) {
		t.Fatalf("应有 %d 条排队，实际 %d 条", len(want), len(msgs))
	}
	for i, m := range msgs {
		if m.Text != want[i] {
			t.Errorf("第 %d 条应为 %q，实际 %q —— 顺序错了用户会立刻发现", i+1, want[i], m.Text)
		}
	}

	// 2. **Take order**: claim one by one; it must also be first in first out.
	//    Checking the list alone is not enough: list sorted right while take grabs the last one
	//    are two independent bugs.
	for i, expect := range want {
		m, ok, err := svc.store.ClaimNextPendingMessage(
			ctx, run.ID, "pod-a", time.Now(), time.Minute)
		if err != nil || !ok {
			t.Fatalf("第 %d 次认领失败：ok=%v err=%v", i+1, ok, err)
		}
		if m.Text != expect {
			t.Errorf("第 %d 次抽取应为 %q，实际 %q", i+1, expect, m.Text)
		}
		// Delete once sent, so the next one becomes the head.
		if err := svc.store.DeletePendingMessage(ctx, m.ID); err != nil {
			t.Fatalf("删排队消息失败：%v", err)
		}
	}
}

// TestPendingClaimIsExclusive: the same queued message is never claimed by two instances at once.
//
// # This CAS matters more than the one on the long-lived subscription
//
// A duplicate subscription only wastes a connection; a duplicate take means **the same message is
// sent twice**, and the agent answers it twice, which the user sees directly.
//
// Grab concurrently; only one may succeed. Use goroutines rather than two sequential calls:
// sequential calls only check "the second one gets nothing", and cannot catch races under
// concurrency (which is exactly why for update skip locked exists).
func TestPendingClaimIsExclusive(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newChatService(t)
	run := newPendingRun(t, svc, "run-claim-pending")

	if _, err := svc.EnqueuePendingMessage(ctx, run.ID, "只该被发一次"); err != nil {
		t.Fatalf("入队失败：%v", err)
	}

	const racers = 8
	var wins int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, ok, err := svc.store.ClaimNextPendingMessage(
				ctx, run.ID, "pod", time.Now(), time.Minute)
			if err != nil {
				t.Errorf("认领出错：%v", err)
				return
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if wins != 1 {
		t.Errorf("%d 个实例同时抢，只该有 1 个认领到，实际 %d 个 —— "+
			"多认一次就是同一句话被发两遍", racers, wins)
	}
}

// TestPendingQueueLimitRejects: a full queue must be rejected clearly, not silently dropped.
//
// Silently dropping is the worst way this feature can fail: the user thinks it was sent, then
// waits for a reply that never comes. The limit has the same value as SEND_QUEUE_LIMIT on the
// frontend; if the two differ, it shows up as "the frontend says you can queue more, the backend
// rejects", and that rejection makes no sense to the user.
func TestPendingQueueLimitRejects(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newChatService(t)
	run := newPendingRun(t, svc, "run-limit")

	for i := 0; i < pendingQueueLimit; i++ {
		if _, err := svc.EnqueuePendingMessage(ctx, run.ID, "排队"); err != nil {
			t.Fatalf("第 %d 条入队失败：%v", i+1, err)
		}
	}
	_, err := svc.EnqueuePendingMessage(ctx, run.ID, "这条该被拒")
	if err == nil {
		t.Fatal("满了之后应当拒绝，静默收下会让用户等一条永远不来的回复")
	}
	if !strings.Contains(userMessage(err), "排队") {
		t.Errorf("拒绝的文案要说清是排队满了，实际 %q", userMessage(err))
	}

	// The queue did not grow past the limit: the limit really blocks.
	msgs, _ := svc.ListPendingMessages(ctx, run.ID)
	if len(msgs) != pendingQueueLimit {
		t.Errorf("队列不该超过 %d，实际 %d 条", pendingQueueLimit, len(msgs))
	}
}

// TestPendingRejectsFrozenRun: frozen sessions cannot queue.
//
// **It must block at enqueue time**, not find out at take time that it cannot be sent: without
// it, users could pile messages into a queue that can never be sent, and each one would have to
// run all pendingMaxAttempts before failing.
//
// The text matches the one in AppendRunEvents word for word: to the user it is the same thing.
func TestPendingRejectsFrozenRun(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)
	run := newPendingRun(t, svc, "run-frozen")

	if err := store.DeleteAgent(ctx, "agent_triage"); err != nil {
		t.Fatalf("删 agent 失败：%v", err)
	}
	_, err := svc.EnqueuePendingMessage(ctx, run.ID, "agent 都没了还排什么")
	if err == nil {
		t.Fatal("agent 已删除时不该让消息排进队列")
	}
	if !strings.Contains(userMessage(err), "已被删除") {
		t.Errorf("文案要与发消息那条一致，实际 %q", userMessage(err))
	}
}

// TestDrainSendsOneAndDeletes: a take sends only one message, and deletes it once sent.
//
// # Why only one at a time
//
// Taking two would send both into Ark at once, and Ark rejects the second one while a turn is in
// progress (that tens-of-milliseconds window, but here it is a whole turn). So after sending one
// we stop, and take the next when its turn ends.
//
// "Delete once sent" must be checked too: without deleting, the next turn would send the same
// message again.
func TestDrainSendsOneAndDeletes(t *testing.T) {
	ctx := context.Background()
	svc, _, runner := newChatService(t)
	run := newPendingRun(t, svc, "run-drain")

	for _, text := range []string{"排队甲", "排队乙"} {
		if _, err := svc.EnqueuePendingMessage(ctx, run.ID, text); err != nil {
			t.Fatalf("入队失败：%v", err)
		}
	}
	runner.sentEvents = nil

	if !svc.DrainPendingMessage(ctx, run.ID, "pod-a") {
		t.Fatal("队列非空时应当发出一条")
	}

	// 1. Only one was sent, and it was the head.
	if len(runner.sentEvents) != 1 {
		t.Fatalf("一次只该发 1 条，实际发了 %d 条", len(runner.sentEvents))
	}
	if text := TitleFromEvents(runner.sentEvents); text != "排队甲" {
		t.Errorf("应当先发队首「排队甲」，实际 %q", text)
	}

	// 2. The sent one was deleted; only the later one is left in the queue.
	msgs, err := svc.ListPendingMessages(ctx, run.ID)
	if err != nil {
		t.Fatalf("列排队消息失败：%v", err)
	}
	if len(msgs) != 1 || msgs[0].Text != "排队乙" {
		t.Errorf("发完应当只剩「排队乙」，实际 %+v —— 不删的话下一轮会重复发送", msgs)
	}

	// 3. Taking again after the queue is empty does nothing and returns no error (the case for most turns).
	if err := svc.store.DeletePendingMessage(ctx, msgs[0].ID); err != nil {
		t.Fatalf("清空队列失败：%v", err)
	}
	if svc.DrainPendingMessage(ctx, run.ID, "pod-a") {
		t.Error("队列空时不该报告「发出了一条」")
	}
}

// TestDrainGivesUpOnPermanentFailure: a message that can never be sent must be given up in place,
// not retried forever.
//
// A precondition failure such as a disabled agent will not get better however many times we
// retry. Without giving up in place, the message would be retried at **the end of every turn**,
// each time calling Ark for nothing.
//
// And the original text must be kept (status=failed instead of deleted): the user must be able to
// see "this one was not sent" and decide whether to resend or rewrite it. Silently dropping is
// the worst.
func TestDrainGivesUpOnPermanentFailure(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)
	run := newPendingRun(t, svc, "run-perm-fail")

	if _, err := svc.EnqueuePendingMessage(ctx, run.ID, "发不出去的那条"); err != nil {
		t.Fatalf("入队失败：%v", err)
	}
	// Disable the agent after enqueuing: simulates "the agent was stopped while the message was queued".
	agent, err := store.GetAgent(ctx, "agent_triage")
	if err != nil {
		t.Fatalf("读 agent 失败：%v", err)
	}
	agent.Status = apimodel.StatusDisabled
	if _, err := store.SaveAgent(ctx, agent); err != nil {
		t.Fatalf("停用 agent 失败：%v", err)
	}

	if svc.DrainPendingMessage(ctx, run.ID, "pod-a") {
		t.Error("发不出去时不该报告「发出了一条」")
	}

	msgs, err := svc.ListPendingMessages(ctx, run.ID)
	if err != nil {
		t.Fatalf("列排队消息失败：%v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("原文应当留着给用户，实际剩 %d 条", len(msgs))
	}
	// **Only marking it failed keeps the next turn from taking it again**; that is the check for "no endless retries".
	if msgs[0].Status != apimodel.PendingFailed {
		t.Errorf("前置条件失败应当就地标 failed，实际 %s —— "+
			"不标的话每一轮结束都会再试一遍", msgs[0].Status)
	}
	if msgs[0].LastError == "" {
		t.Error("要留一句原因，否则用户不知道为什么没发出去")
	}

	// Failed messages are no longer taken; check this directly, do not just trust the status field.
	if _, ok, _ := svc.store.ClaimNextPendingMessage(
		ctx, run.ID, "pod-a", time.Now(), time.Minute); ok {
		t.Error("failed 的消息不该再被认领")
	}
}

// TestCancelPendingIsIdempotent: cancel is idempotent; cancelling twice returns no error.
//
// Users often click cancel twice, and "does not exist" on the second click only makes them think
// something went wrong.
func TestCancelPendingIsIdempotent(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newChatService(t)
	run := newPendingRun(t, svc, "run-cancel")

	msgs, err := svc.EnqueuePendingMessage(ctx, run.ID, "要被取消的")
	if err != nil {
		t.Fatalf("入队失败：%v", err)
	}
	id := msgs[0].ID

	if err := svc.CancelPendingMessage(ctx, run.ID, id); err != nil {
		t.Fatalf("取消失败：%v", err)
	}
	if err := svc.CancelPendingMessage(ctx, run.ID, id); err != nil {
		t.Errorf("重复取消不该报错：%v", err)
	}

	left, _ := svc.ListPendingMessages(ctx, run.ID)
	if len(left) != 0 {
		t.Errorf("取消后队列应当为空，实际 %d 条", len(left))
	}
}

// TestDrainAllowedExcludesUserInterrupt: an interrupt by the user does not count as "the next
// message may go".
//
// # This blocks a reorder that really happens
//
// Redirect ("send now") = two steps, interrupt + message (RedirectRun). The interrupt makes Ark
// push a session.status_idle, which **satisfies turnEnded**. Without blocking, the take would send
// the head of the queue between those two steps: the user clicked "send message 3 right now", and
// what goes out is message 3 and message 1.
//
// This trap used to be blocked by a `redirecting` ref on the frontend; after the queue moved to
// the server that defense stopped working (the server cannot see the browser's intent), so it
// must be blocked here.
func TestDrainAllowedExcludesUserInterrupt(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "正常结束可以抽",
			raw:  `{"type":"session.status_idle","stop_reason":{"type":"end_turn"}}`,
			want: true,
		},
		{
			// This one is why this test exists.
			name: "用户中断不抽",
			raw:  `{"type":"session.status_idle","stop_reason":{"type":"user_interrupt"}}`,
			want: false,
		},
		{
			// Retries exhausted is also a real end: that turn will have no more events, so queued messages should go.
			name: "重试耗尽可以抽",
			raw:  `{"type":"session.status_idle","stop_reason":{"type":"retries_exhausted"}}`,
			want: true,
		},
		{
			// Session terminated: same as above, no more events.
			name: "会话终止可以抽",
			raw:  `{"type":"session.status_terminated"}`,
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := drainAllowed([]byte(tc.raw)); got != tc.want {
				t.Errorf("drainAllowed(%s) = %v，期望 %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestTurnEndedStillAcceptsInterrupt: turnEnded itself **must not** be narrowed along with it.
//
// It is also used by the scheduled task path (scheduler.go), which wants exactly "however it
// ended, this turn is over". A user interrupting a scheduled run really did end that turn; if it
// were not accepted, that run would stay in running forever.
//
// Paired with the test above: one guards against letting things through (the take must block
// interrupts), the other against blocking wrongly (turnEnded must not block). With only the
// former, putting the narrowing into turnEnded would also make it pass, and that is wrong.
func TestTurnEndedStillAcceptsInterrupt(t *testing.T) {
	raw := []byte(`{"type":"session.status_idle","stop_reason":{"type":"user_interrupt"}}`)
	if !arkframe.TurnEnded(raw) {
		t.Error("turnEnded 必须仍然认「用户中断」为一轮结束——" +
			"定时任务那条路靠它收尾，不认会让 run 永远停在 running")
	}
}
