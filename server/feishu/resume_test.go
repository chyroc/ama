// Tests for restart recovery. All offline: Ark and Feishu both use fakes.
//
// Every case pinned here is of the "wrong but no error, just wrong behavior" kind, which is exactly why this feature exists:
//
//   - take over the old card: with a card anchor it must PATCH the original card, not send another (otherwise the user sees two cards)
//   - do not redeliver the message: the turn on the Ark side is still running, and redelivering would make the agent answer the same question twice
//   - unpoison: the final state must change pending, otherwise when the user resends the sentence it is blocked as a duplicate delivery
//   - CAS claim: multiple instances scan at once and only one can take over, otherwise two instances write the same card and the content jumps back and forth
//   - no resume after timeout: forcing a resume past the container TTL only gives an agent confused in an empty directory

package feishu

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// errFakeSync simulates failing to fill in Ark events.
var errFakeSync = errors.New("方舟抖了一下")

// inboundStatusOf reads the current handling status of an inbound message.
//
// The Store interface has no method for "read one inbound message" (the business does not need it), but here we must tell
// pending from processed: the two behave the same for dedup, so the status can only be looked at directly.
// The in-memory implementation is a type in this package, so tests can reach it directly.
func inboundStatusOf(t *testing.T, store Store, messageID string) string {
	t.Helper()
	mem, ok := store.(*memStore)
	if !ok {
		t.Fatalf("这个断言只对内存实现有效，实际类型 %T", store)
	}
	mem.mu.Lock()
	defer mem.mu.Unlock()
	rec, ok := mem.inbound[messageID]
	if !ok {
		t.Fatalf("入站消息 %q 不在库里", messageID)
	}
	return rec.Status
}

// resumeFixture wires up a recovery environment with an unfinished turn record.
//
// The returned store already has an app and an inflight record pre-filled, so calling
// in.ResumeInflightTurns directly runs the recovery.
func resumeFixture(
	t *testing.T, conv *fakeConversations, sender *fakeSender, rec InflightTurnRecord,
) (*Inbound, Store) {
	t.Helper()
	in, store := inboundFixture(t, conv, sender)
	// The inbound message row must exist first: recovery has to get it out of pending.
	if rec.SrcMessageID != "" {
		claimed, err := store.ClaimInboundMessage(context.Background(), InboundMessageRecord{
			LarkAppID: rec.LarkAppID,
			MessageID: rec.SrcMessageID,
			ChatID:    rec.ChatID,
			Status:    inboundPending,
			CreatedAt: time.Now(),
		})
		if err != nil || !claimed {
			t.Fatalf("预置入站消息失败: claimed=%v err=%v", claimed, err)
		}
	}
	if err := store.PutInflightTurn(context.Background(), rec); err != nil {
		t.Fatalf("预置未结轮次失败: %v", err)
	}
	return in, store
}

// inflightRec builds a typical unfinished turn record: the card was created and some content already pushed.
func inflightRec() InflightTurnRecord {
	return InflightTurnRecord{
		RunID:         "sesn-1",
		LarkAppID:     "cli_1",
		ChatID:        "oc_1",
		SrcMessageID:  "om_1",
		CardMessageID: "om_card_1",
		ReplyInThread: true,
		LastText:      "正在分析日志",
		Status:        inflightOpen,
		StartedAt:     time.Now(),
	}
}

// inflightByID gets a record from the store, returning false if not found.
func inflightByID(t *testing.T, store Store, runID string) (InflightTurnRecord, bool) {
	t.Helper()
	recs, err := store.ListInflightTurns(context.Background())
	if err != nil {
		t.Fatalf("列未结轮次失败: %v", err)
	}
	for _, rec := range recs {
		if rec.RunID == runID {
			return rec, true
		}
	}
	return InflightTurnRecord{}, false
}

// TestResumeReattachesToLiveRun: when the turn is still running on the Ark side, reattach the stream so the reply keeps being written.
//
// This is the core test of "seamless recovery": users should feel the card keeps updating, not stopping at the moment of interruption.
func TestResumeReattachesToLiveRun(t *testing.T) {
	conv := &fakeConversations{
		status: "running",
		frames: [][]byte{agentFrame("，发现 3 处异常。")},
	}
	sender := &fakeSender{}
	in, store := resumeFixture(t, conv, sender, inflightRec())

	in.ResumeInflightTurns(context.Background())

	// The gap must be filled, and the stream must be attached: filling without attaching means giving up the part still running.
	if conv.syncCalls != 1 {
		t.Errorf("应补齐方舟事件一次，实际 %d 次", conv.syncCalls)
	}
	order := conv.callOrder()
	if !contains(order, "sync") || !contains(order, "stream") {
		t.Errorf("应先补齐再挂流，实际调用顺序 %v", order)
	}

	// **The user message must never be redelivered**: the turn on the Ark side is still running, and redelivering would make the agent answer twice.
	if contains(order, "append") {
		t.Error("恢复不该重投用户消息——方舟侧推理还在继续，重投会让 agent 答两遍")
	}

	cards := sender.sent()
	if len(cards) == 0 {
		t.Fatal("恢复后应把答案发给用户")
	}
	last := cards[len(cards)-1]
	// The half already produced must be kept, with new content appended after it. **Ark does not replay frames consumed by the previous process**,
	// so clearing and starting over would permanently lose the beginning of the answer.
	if !strings.Contains(last.text, "正在分析日志") || !strings.Contains(last.text, "发现 3 处异常") {
		t.Errorf("答案应在原内容之后接上续跑的内容，实际 %q", last.text)
	}

	// The record must be cleared when done, or the next startup would recover the same turn again.
	if _, ok := inflightByID(t, store, "sesn-1"); ok {
		t.Error("恢复完成后应清掉未结轮次记录")
	}
}

// TestResumeReusesAnswerIdempotencyKey: when sending the answer on recovery, use **exactly the same** idempotency key
// as the original turn, or the user gets two answers.
//
// In the new path the answer is a separate message, no longer "a card PATCHed again and again", so the old constraint "do not send an extra card"
// became this one: the real window is "the answer was already sent, but it crashed before deleting the inflight record";
// after a restart this turn is recovered again and the answer sent once more. The only thing stopping it is this key,
// and a wrongly computed key gives no error, just one more identical reply in the chat.
func TestResumeReusesAnswerIdempotencyKey(t *testing.T) {
	conv := &fakeConversations{status: "idle"}
	sender := &fakeSender{}
	in, _ := resumeFixture(t, conv, sender, inflightRec())

	in.ResumeInflightTurns(context.Background())

	cards := sender.sent()
	if len(cards) == 0 {
		t.Fatal("恢复后应把攒到的答案发出去")
	}
	// The key used by the original turn: segment index 0, total 1, and the turn key is run + source message.
	want := segmentIdempotencyKey(ProgressDeliveryKey("sesn-1", "om_1"), 0, 1)
	if cards[0].idem != want {
		t.Errorf("恢复发答案的幂等键必须和原轮次一致，想要 %q 实际 %q——不一致会让用户收到两份答案",
			want, cards[0].idem)
	}
}

// TestResumeStillAnswersWhenNothingProduced: it must reply even when it crashed "before producing a single character".
//
// This is the branch most easily missed: LastText in the store is empty, it looks like "nothing to send", so it just finishes,
// and the user's message never gets any reply, while the inflight record is already cleared, so only resending helps.
// Better to reply "this turn produced no reply" than to stay silent.
func TestResumeStillAnswersWhenNothingProduced(t *testing.T) {
	conv := &fakeConversations{status: "idle"}
	sender := &fakeSender{}
	rec := inflightRec()
	rec.LastText = ""
	in, _ := resumeFixture(t, conv, sender, rec)

	in.ResumeInflightTurns(context.Background())

	cards := sender.sent()
	if len(cards) == 0 || cards[0].kind != "reply" {
		t.Fatalf("一个字都没产出时也该回一句，实际 %+v", cards)
	}
	if !strings.Contains(cards[0].text, "没有产生回复") {
		t.Errorf("兜底文案应说明这一轮没产出，实际 %q", cards[0].text)
	}
}

// TestResumeFinalizesFinishedRun: if Ark finished during the gap, finalize and finish, without reattaching the stream.
//
// This is the most common case: a restart only takes tens of seconds, and Ark finished producing the answer long ago.
func TestResumeFinalizesFinishedRun(t *testing.T) {
	conv := &fakeConversations{status: "idle"}
	sender := &fakeSender{}
	in, store := resumeFixture(t, conv, sender, inflightRec())

	in.ResumeInflightTurns(context.Background())

	if conv.syncCalls != 1 {
		t.Errorf("应补齐方舟事件一次，实际 %d 次", conv.syncCalls)
	}
	if contains(conv.callOrder(), "stream") {
		t.Error("已终态的轮次不该再挂流——白占一个 goroutine，还可能把定稿覆盖回去")
	}
	if len(sender.sent()) == 0 {
		t.Error("已终态的轮次也要把答案发出去，否则用户永远等不到回复")
	}
	if _, ok := inflightByID(t, store, "sesn-1"); ok {
		t.Error("定稿后应清掉未结轮次记录")
	}
}

// TestResumeTreatsRequiresActionAsLive: requires_action is **not** final, and must be resumed.
//
// This is exactly the state a worker-side restart leaves (the agent waiting for a tool result, picked up again after the worker restarts).
// Judging it final would freeze the card right when the agent is about to continue, and the user gets half an answer.
func TestResumeTreatsRequiresActionAsLive(t *testing.T) {
	conv := &fakeConversations{status: "requires_action"}
	sender := &fakeSender{}
	in, _ := resumeFixture(t, conv, sender, inflightRec())

	in.ResumeInflightTurns(context.Background())

	if !contains(conv.callOrder(), "stream") {
		t.Error("requires_action 是「等工具结果」不是终态，必须重新挂流接着听")
	}
}

// TestResumeSkipsAgedOutTurn: past the recoverable time limit it is not resumed, but is finalized with an explanation.
//
// The limit is the TTL of the execution container: past it the workspace has been reclaimed, and resuming only gives an agent confused in an empty directory.
func TestResumeSkipsAgedOutTurn(t *testing.T) {
	conv := &fakeConversations{status: "running"}
	sender := &fakeSender{}
	rec := inflightRec()
	rec.StartedAt = time.Now().Add(-inflightMaxAge - time.Minute)
	in, store := resumeFixture(t, conv, sender, rec)

	in.ResumeInflightTurns(context.Background())

	if conv.syncCalls != 0 {
		t.Errorf("超时的轮次不该再碰方舟，实际调了 %d 次", conv.syncCalls)
	}
	if contains(conv.callOrder(), "stream") {
		t.Error("超时的轮次不该挂流")
	}

	cards := sender.sent()
	if len(cards) == 0 {
		t.Fatal("超时也要给用户一句说明，不能让他永远等不到回复")
	}
	last := cards[len(cards)-1].text
	if !strings.Contains(last, "正在分析日志") {
		t.Errorf("说明应追加在已有内容之后，不覆盖用户已读到的部分，实际 %q", last)
	}
	if !strings.Contains(last, "请重发") {
		t.Errorf("超时的说明要告诉用户重发，实际 %q", last)
	}
	if _, ok := inflightByID(t, store, "sesn-1"); ok {
		t.Error("超时的轮次应被清掉，否则每次启动都白扫一遍")
	}
}

// TestResumeGivesUpAfterMaxRetries: still not finished after repeated resumes, give up and let the user resend.
//
// Without this limit, a turn that "never finishes" would be run for nothing on every restart.
func TestResumeGivesUpAfterMaxRetries(t *testing.T) {
	conv := &fakeConversations{status: "running"}
	sender := &fakeSender{}
	rec := inflightRec()
	rec.RetryCount = inflightMaxRetries
	in, store := resumeFixture(t, conv, sender, rec)

	in.ResumeInflightTurns(context.Background())

	if conv.syncCalls != 0 {
		t.Errorf("重试用尽后不该再碰方舟，实际调了 %d 次", conv.syncCalls)
	}
	cards := sender.sent()
	if len(cards) == 0 {
		t.Fatal("放弃时也要给用户一句说明")
	}
	if !strings.Contains(cards[len(cards)-1].text, "请重发") {
		t.Errorf("放弃的说明要告诉用户重发，实际 %q", cards[len(cards)-1].text)
	}
	if _, ok := inflightByID(t, store, "sesn-1"); ok {
		t.Error("放弃后应清掉记录")
	}
}

// TestResumeUnpoisonsPendingMessage: recovery must get the inbound message out of pending.
//
// **If this fails, the user can never ask this sentence again**: ClaimInboundMessage treats pending as a duplicate
// delivery and acks directly, so resending is blocked too, with no error at all.
func TestResumeUnpoisonsPendingMessage(t *testing.T) {
	conv := &fakeConversations{status: "idle"}
	in, store := resumeFixture(t, conv, &fakeSender{}, inflightRec())

	// Before recovering, confirm it really is in the "poisoned" state, otherwise this test might be checking an empty condition.
	if status := inboundStatusOf(t, store, "om_1"); status != inboundPending {
		t.Fatalf("前置条件不成立：入站消息应是 %q，实际 %q", inboundPending, status)
	}

	in.ResumeInflightTurns(context.Background())

	// pending and processed **behave the same** for dedup (both block redelivery), so the status can only be looked at directly.
	// The difference is in meaning: pending means "some process is handling it", but that process no longer exists:
	// this row will never be advanced again, and that sentence can never be asked again.
	if status := inboundStatusOf(t, store, "om_1"); status != inboundProcessed {
		t.Errorf("恢复后入站消息应标成 %q，实际 %q——留在 pending 会让用户永远问不出这句话",
			inboundProcessed, status)
	}
}

// TestResumeClaimIsSingleWinner: multiple instances scan at once, and only one can claim successfully.
//
// Two instances taking over at once would write to the same card, and the user would see the content jump back and forth.
func TestResumeClaimIsSingleWinner(t *testing.T) {
	store := NewMemStore()
	if err := store.PutInflightTurn(context.Background(), inflightRec()); err != nil {
		t.Fatalf("预置未结轮次失败: %v", err)
	}

	const racers = 8
	var wg sync.WaitGroup
	won := make([]bool, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ok, err := store.ClaimInflightTurn(context.Background(), "sesn-1")
			if err != nil {
				t.Errorf("认领报错: %v", err)
				return
			}
			won[idx] = ok
		}(i)
	}
	wg.Wait()

	winners := 0
	for _, ok := range won {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("并发认领应只有一个赢家，实际 %d 个——多个实例同写一张卡会让内容来回跳", winners)
	}
}

// TestResumeSkipsClaimedTurn: a turn already taken over should not be taken over again.
func TestResumeSkipsClaimedTurn(t *testing.T) {
	conv := &fakeConversations{status: "running"}
	rec := inflightRec()
	rec.Status = inflightResuming // another instance already took over
	in, _ := resumeFixture(t, conv, &fakeSender{}, rec)

	in.ResumeInflightTurns(context.Background())

	if conv.syncCalls != 0 {
		t.Errorf("已被接管的轮次不该再碰方舟，实际调了 %d 次", conv.syncCalls)
	}
}

// TestResumeKeepsRecordWhenSyncFails: when filling fails, keep the record and try again on the next startup.
//
// Deleting it would lose this turn forever, while the failure is often temporary (an Ark hiccup, credentials being refreshed right then).
func TestResumeKeepsRecordWhenSyncFails(t *testing.T) {
	conv := &fakeConversations{status: "running", syncErr: errFakeSync}
	in, store := resumeFixture(t, conv, &fakeSender{}, inflightRec())

	in.ResumeInflightTurns(context.Background())

	rec, ok := inflightByID(t, store, "sesn-1")
	if !ok {
		t.Fatal("补齐失败时应留着记录，下次启动带着涨过的次数再试")
	}
	if rec.RetryCount != 1 {
		t.Errorf("认领应把续跑次数 +1，实际 %d", rec.RetryCount)
	}
}

// TestResumeHandlesEmptyTable: with no unfinished turns it should quietly do nothing.
func TestResumeHandlesEmptyTable(t *testing.T) {
	conv := &fakeConversations{}
	in, _ := inboundFixture(t, conv, &fakeSender{})

	in.ResumeInflightTurns(context.Background())

	if conv.syncCalls != 0 {
		t.Errorf("没有未结轮次时不该碰方舟，实际调了 %d 次", conv.syncCalls)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
