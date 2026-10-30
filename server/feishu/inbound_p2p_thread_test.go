// Tests for the causal chain of direct-chat "two messages in a row, one blocked".
//
// # The question this group answers
//
// A user reported a symptom that looked like a certain bug: **sending two messages in a row in a Feishu direct chat, the second one was
// blocked by the bot** ("the previous message is still being processed"). Yet reading scope.go, every main-line message in a direct chat computes its own key
// (`message:<its own message_id>`), landing on two unrelated sessions, so they should never block each other.
//
// The code and the symptom did not match, and the missing link in between is **`reply_in_thread`**:
//
//	the user sends the first sentence in the main line
//	  → in runAndReply, inThread := msg.ThreadID == ""; a main-line message has no thread_id, **so it is true**
//	  → the bot replies with reply_in_thread, and Feishu folds this question and answer into a topic
//	  → the user's "second message" lands in this topic in the client (the client's input box is in the topic at this point)
//	  → this message comes in with root_id = the first message's message_id
//	  → scopeForMessage hits directThreadAnchor → computes **the root message's key**
//	  → the same session as the first turn → hits the "one turn per session" serial lock → replies "the previous message is still being processed"
//
// So **it is not a bug, it is the result of two design rules stacking**: 1. the bot's replies open topics;
// 2. later messages in a topic reuse the root message's session (otherwise the agent would not remember what was just said;
// see the scope.go file header). Each is right on its own, and together the effect is "what looks like a second main-line message
// is really a second message inside the topic".
//
// # Why it deserves its own group of tests
//
// **Not a single character of this causal chain was covered by tests before**: `fakeSender` used to drop
// `ReplyCardOption.ReplyInThread`, so "the bot's reply opens a topic", this **hidden input of the scope**,
// did not exist in unit tests at all. Existing tests all build the anchors of inbound messages by hand (`scopedMsg` passes
// `threadID` directly), presetting this link as known: they pinned "with an anchor it reuses", but none said
// "the anchor is **caused by us**".
//
// The consequence is not about test coverage numbers: the next person (and the user themselves) reading the line in scope.go,
// "the same person typing two sentences in the main line always computes two keys", would conclude "two messages in a row should not be blocked",
// and spend time hunting a bug that does not exist. That is exactly what happened this time.
package feishu

import (
	"context"
	"testing"
	"time"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// p2pMsg builds a direct chat message. An empty root means a new main-line thread of talk; non-empty means a later message inside a topic.
//
// thread_id is deliberately **not** set: the direct-chat scope only accepts root_id / parent_id (the omt_ namespace
// does not match the root message's key; see scope.go). What is checked is the root_id path.
func p2pMsg(id, root, text string) *lark.InboundMessage {
	return &lark.InboundMessage{
		AppID:     "cli_1",
		MessageID: id,
		ChatID:    "oc_p2p",
		ChatType:  ChatTypeP2P,
		RootID:    root,
		Text:      text,
		Mentioned: true,
	}
}

// TestP2PMainlineReplyOpensThread pins **the first link** of the causal chain: the reply to a direct-chat main-line message carries
// reply_in_thread.
//
// This link is the starting point of the whole "two messages in a row, one blocked" symptom, and it had no assertion before. Removing
// the check in `inThread := msg.ThreadID == ""`, or hard-coding ReplyInThread of sendFinalReply
// to false, must turn this test red.
func TestP2PMainlineReplyOpensThread(t *testing.T) {
	conv := &stickyConversations{
		frames: [][]byte{agentFrame("你好"), endTurnFrame()},
		status: "idle",
	}
	sender := &fakeSender{}
	in, _ := stickyFixture(t, conv, sender)

	if err := handleWithin(t, in, p2pMsg("om_1", "", "你好"), 5*time.Second); err != nil {
		t.Fatalf("第一条不该报错: %v", err)
	}

	// The final answer reply must carry reply_in_thread: it is exactly what folds this question and answer into a topic,
	// so the sentence the user types next in the client automatically carries root_id.
	var replies int
	var threaded int
	for _, c := range sender.sent() {
		if c.kind != "reply" {
			continue
		}
		replies++
		if c.inThread {
			threaded++
		}
	}
	if replies == 0 {
		t.Fatal("这一轮一条回复都没发出去，用例的前提就不成立")
	}
	if threaded == 0 {
		t.Errorf("单聊主线消息的回复必须带 reply_in_thread（共 %d 条回复，没有一条带）——"+
			"少了它，用户的下一句仍是主线消息，本文件描述的整条因果链就不成立了", replies)
	}
}

// TestP2PGroupReplyStaysThreadedOnlyForMainline: replies to messages inside a topic **no longer** open a new topic.
//
// Paired with the previous one, so that a wrong implementation "always open topics" cannot make the one above green:
// the test for inThread is `msg.ThreadID == ""`, and messages inside a topic carry thread_id,
// so their replies should not open another level.
func TestP2PGroupReplyStaysThreadedOnlyForMainline(t *testing.T) {
	conv := &stickyConversations{
		frames: [][]byte{agentFrame("好"), endTurnFrame()},
		status: "idle",
	}
	sender := &fakeSender{}
	in, _ := stickyFixture(t, conv, sender)

	msg := p2pMsg("om_2", "om_1", "话题里接着说")
	msg.ThreadID = "omt_abc" // a message inside a topic; Feishu includes it
	if err := handleWithin(t, in, msg, 5*time.Second); err != nil {
		t.Fatalf("不该报错: %v", err)
	}

	for _, c := range sender.sent() {
		if c.kind == "reply" && c.inThread {
			t.Error("已经在话题里的消息，回复时不该再带 reply_in_thread 开一层")
		}
	}
}

// TestP2PMainlineTwoMessagesAreIndependent: two **real** main-line messages are unrelated.
//
// This is the experiment the user thought they were doing, and the behavior scope.go promises: two main-line messages compute two different
// keys, each creates a session, and the second is not blocked.
//
// ⚠️ Note it **does not contradict** the production symptom: in production the second one actually carried root_id (because the first turn's reply
// opened a topic), so it took the path of the test below. The two tests differ only in the one field root_id,
// and that is the whole problem.
func TestP2PMainlineTwoMessagesAreIndependent(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("好")},
	}
	sender := &fakeSender{}
	in, store := inboundFixture(t, conv, sender)
	ctx := context.Background()

	if err := in.Handle(ctx, p2pMsg("om_a", "", "第一句")); err != nil {
		t.Fatalf("第一条失败: %v", err)
	}
	if err := in.Handle(ctx, p2pMsg("om_b", "", "第二句")); err != nil {
		t.Fatalf("第二条失败: %v", err)
	}

	first, err := store.GetSessionScope(ctx, "cli_1", "message:om_a", "agent_1")
	if err != nil {
		t.Fatalf("查第一条的绑定失败: %v", err)
	}
	second, err := store.GetSessionScope(ctx, "cli_1", "message:om_b", "agent_1")
	if err != nil {
		t.Fatalf("查第二条的绑定失败: %v", err)
	}
	if first.RunID == second.RunID {
		t.Errorf("两条主线消息必须落到两条不同的会话上，实际都是 %q", first.RunID)
	}
	if busy := busyReplies(sender); busy != 0 {
		t.Errorf("两条主线消息互不相干，不该有任何「还在处理中」，实际 %d 次", busy)
	}
}

// TestP2PThreadedSecondMessageSharesSession: the second message with root_id lands back on the same session.
//
// **This is the path the user actually hit**, and the direct cause of "the second message is blocked": the same session
// only allows one turn at a time (see the comment on TryBeginTurn in runAndReply), so while the previous turn has not ended the second
// always gets "the previous message is still being processed".
//
// This test only asserts the fact "it lands on the same session"; blocking or not is the serial lock's job,
// covered by the inbound_turnlock_test.go group.
func TestP2PThreadedSecondMessageSharesSession(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("好")},
	}
	sender := &fakeSender{}
	in, store := inboundFixture(t, conv, sender)
	ctx := context.Background()

	if err := in.Handle(ctx, p2pMsg("om_root", "", "第一句")); err != nil {
		t.Fatalf("第一条失败: %v", err)
	}
	// The second message comes in with root_id: to the user it is "typed one more sentence",
	// but actually the client sent it into the topic the bot just opened.
	if err := in.Handle(ctx, p2pMsg("om_next", "om_root", "第二句")); err != nil {
		t.Fatalf("第二条失败: %v", err)
	}

	bound, err := store.GetSessionScope(ctx, "cli_1", "message:om_root", "agent_1")
	if err != nil {
		t.Fatalf("查根消息的绑定失败: %v", err)
	}
	// The second message should not create another session: the key it computes is **exactly the same** as the root message's.
	if _, err := store.GetSessionScope(ctx, "cli_1", "message:om_next", "agent_1"); err == nil {
		t.Error("话题内的第二条不该按自己的 message_id 另绑一条会话——" +
			"那样 agent 就不记得第一句说了什么")
	}
	if bound.TurnCount != 2 {
		t.Errorf("两条消息都该记在同一条会话上（turnCount=2），实际 %d", bound.TurnCount)
	}
}
