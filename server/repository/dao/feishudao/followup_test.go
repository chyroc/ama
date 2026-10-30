// The two paths for "sending the next message right away": queue and redirect.
//
// # These two are two outcomes of the same precondition, so they live together
//
// The precondition for both is "the previous turn is still running and the user said something else" (simulated by holding the inflight record).
// The only difference is which mode the agent is configured with:
//
//	queue     send after this turn finishes → reply "the previous message is still being processed" + hourglass reaction
//	redirect  interrupt this turn and switch to the new one → call RedirectRun + fast-forward reaction, **no such reply**
//
// ⚠️ **The default is redirect.** The empty string of apimodel.FollowUpMode is redirect, which is the opposite of this repo's
// convention for other enums, "empty string falls on the most conservative side" (see the comment on that type for why). So tests for queueing
// **must explicitly turn on** queueingAgents. Without setting it explicitly, the test exercises the redirect path,
// and the error would say "should reply with an explanation", which does not point at the real cause at all.

package feishudao

import (
	"context"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// TestFollowUpRedirectInterruptsRunningTurn, default mode (redirect): the previous turn is interrupted and the new message is used instead.
//
// # The core of the assertion is "the running conversation was interrupted, then this message was sent on its own"
//
// Asserting only "the busy reply was not sent" is not enough: an implementation that "does nothing and silently drops it" would also
// make that assertion green, and that is the worst failure mode of this feature (the user waits for a reply that never comes).
// So check that the interrupt really hit that conversation, **and that the new message really went out on the normal path**.
//
// ⚠️ Items 2 and 3 of this test once asserted the opposite behavior ("the interrupt must carry the body", "must not go through
// AppendRunEvents again"), which is exactly the shape of a production bug: the new message was stuffed into the running conversation,
// and as soon as the interrupt took effect that goroutine stopped streaming and left. **Nobody was waiting for the answer to the new message**.
// What really happened was that it landed in the buffer of the message after that, mixing two turns' replies.
func TestFollowUpRedirectInterruptsRunningTurn(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("在想")},
		// queueingAgents not set = default mode (redirect). The absence of this line is part of the assertion itself.
	}
	sender, reactor := &fakeSender{}, &followUpReactor{}
	in, store := inboundFixtureWithReactor(t, conv, sender, reactor)
	runID := occupyTurn(t, in, store)
	// After the interrupt the previous turn wraps up and gives back the serial lock; in the real path that is
	// DeleteInflightTurn in its defer. Without simulating this step, the next step of redirect (waiting for the lock) would wait forever.
	conv.onInterrupt = func() {
		if err := store.DeleteInflightTurn(context.Background(), runID); err != nil {
			t.Errorf("模拟上一轮交还锁失败: %v", err)
		}
	}

	before := len(conv.appendedEvents())
	if err := in.Handle(context.Background(),
		scopedMsg("om_2", "oc_1", "group", "", "不查了，改看 CI")); err != nil {
		t.Fatalf("转向不该返回错误（会让飞书重投）: %v", err)
	}

	// 1. The interrupt really happened, and it hit **the running conversation**.
	calls := conv.redirectCalls()
	if len(calls) != 1 {
		t.Fatalf("应当中断一次，实际 %d 次", len(calls))
	}
	if calls[0].runID != runID {
		t.Errorf("中断打错了会话：期望 %q，实际 %q", runID, calls[0].runID)
	}
	// 2. **The interrupt carries no body.** Carrying the body is the old merge primitive, and nobody waits for the answer on that path.
	if calls[0].text != "" {
		t.Errorf("中断不该带正文（新消息走正常路径发），实际带了 %q", calls[0].text)
	}

	// 3. **The new message must go through AppendRunEvents on its own.** This is the only evidence that "every message has its own
	//    owner": going this way means a goroutine is subscribed and waiting for its answer.
	if got := len(conv.appendedEvents()); got != before+1 {
		t.Errorf("转向后这句话应当自己发一轮（+1 批事件），实际从 %d 变成 %d", before, got)
	}

	// 4. **Must not reply "the previous message is still being processed"**: that is the queue mode; redirect is not waiting for anything.
	for _, c := range sender.sent() {
		if strings.Contains(c.text, "上一句还在处理") {
			t.Error("转向档不该回排队那句话——用户会以为消息被挡下了")
		}
	}

	// 5. Use the redirect reaction. When the previous turn suddenly stops, this is the only signal telling the user "I did that".
	if !reactor.hasAdded(lark.ReactionRedirected) {
		t.Errorf("转向应当打 %q，实际打了 %v", lark.ReactionRedirected, reactor.added())
	}
}

// TestFollowUpQueueWaitsForRunningTurn, queue mode: reply with an explanation + hourglass reaction, and **do not** interrupt.
//
// A pair with the previous test. Checking only redirect is not enough; we must also check "with queue configured it really does not redirect",
// otherwise an implementation that "ignores the config and always redirects" could make the test above all green, and it would silently change
// the behavior the user explicitly chose (the previous turn gets killed, while they wanted to wait for it to finish).
func TestFollowUpQueueWaitsForRunningTurn(t *testing.T) {
	conv := &fakeConversations{
		agentIDs:       []string{"agent_1"},
		frames:         [][]byte{agentFrame("在想")},
		queueingAgents: map[string]bool{"agent_1": true},
	}
	sender, reactor := &fakeSender{}, &followUpReactor{}
	in, store := inboundFixtureWithReactor(t, conv, sender, reactor)
	occupyTurn(t, in, store)

	if err := in.Handle(context.Background(),
		scopedMsg("om_2", "oc_1", "group", "", "第二个问题")); err != nil {
		t.Fatalf("被挡住的那条不该返回错误: %v", err)
	}

	// 1. **Never redirect**: the user chose to wait, not to kill the previous turn.
	if got := conv.redirectCalls(); len(got) != 0 {
		t.Errorf("排队档不该转向，实际转了 %d 次", len(got))
	}
	// 2. Must send the explanation, otherwise the user thinks the bot is broken.
	var busy bool
	for _, c := range sender.sent() {
		if strings.Contains(c.text, "上一句还在处理") {
			busy = true
		}
	}
	if !busy {
		t.Error("排队档应当回一句说明，不能静默丢掉")
	}
	// 3. Use the hourglass reaction, and **different from the redirect one**: both mean "received" but with opposite consequences;
	//    sharing one, the user cannot tell whether the bot is waiting or has already thrown the previous turn away.
	got := reactor.added()
	if len(got) == 0 || got[len(got)-1] != lark.ReactionQueued {
		t.Errorf("排队应当打 %q，实际打了 %v", lark.ReactionQueued, got)
	}
	if lark.ReactionQueued == lark.ReactionRedirected {
		t.Error("排队与转向必须用不同的表情，否则这个开关没有可见反馈")
	}
}

// TestFollowUpRedirectFallsBackWhenRedirectFails: when redirect fails, fall back to the queue notice; never stay silent.
//
// On failure the user's message was **neither queued nor sent**. Doing nothing would leave them waiting forever for a reply
// that never comes, and this path has zero errors (Handle returns nil so that Feishu does not redeliver).
func TestFollowUpRedirectFallsBackWhenRedirectFails(t *testing.T) {
	conv := &fakeConversations{
		agentIDs:    []string{"agent_1"},
		frames:      [][]byte{agentFrame("在想")},
		redirectErr: context.DeadlineExceeded,
	}
	sender, reactor := &fakeSender{}, &followUpReactor{}
	in, store := inboundFixtureWithReactor(t, conv, sender, reactor)
	occupyTurn(t, in, store)

	if err := in.Handle(context.Background(),
		scopedMsg("om_2", "oc_1", "group", "", "改看 CI")); err != nil {
		t.Fatalf("转向失败也不该返回错误（会让飞书重投）: %v", err)
	}

	// Must say something: this is the check for "not silent".
	var busy bool
	for _, c := range sender.sent() {
		if strings.Contains(c.text, "上一句还在处理") {
			busy = true
		}
	}
	if !busy {
		t.Error("转向失败要退回排队提示，否则用户等一个永远不来的回复")
	}
	// On failure it **must not** use the "redirected" reaction: that would falsely report the previous turn was interrupted.
	for _, e := range reactor.added() {
		if e == lark.ReactionRedirected {
			t.Error("转向失败却打了转向表情——那是谎报")
		}
	}
}
