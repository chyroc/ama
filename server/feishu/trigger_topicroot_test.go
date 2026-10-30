// No-@ is only for "topic roots": replies inside a topic must @ to trigger.
//
// # This group pins down a production false trigger (2026-09-02)
//
// In an alert chat, a **third-party bot not managed by AMA** replied inside a topic,
// and the AMA bot immediately added a reaction to it and auto-replied for a turn. That message was neither a new topic nor did it @ us.
//
// The root cause was that shouldTrigger only checked "is there a thread_id", so **every message in a topic counted**.
// The user's definition of this mode is "new topics need no @, later messages inside a topic must @":
// a topic is a tree, and only the root can skip the @.
//
// # Why they must be written in pairs
//
// With only "the root should trigger", "everything triggers" (the wrong implementation before the fix) stays green;
// with only "replies should not trigger", "nothing triggers" (turning the whole mode off) also stays green.
// **Wrong implementations in either direction can fool one-sided assertions**, so not one of the four paired tests can be dropped.
//
// The test is that root_id / parent_id are both empty; the measured basis is in isTopicRootMessage in trigger.go.
// ⚠️ Those two values are only given by the **event channel**; Feishu's read APIs always return null, so do not cross-check with the read APIs.
package feishu

import (
	"context"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// The real shapes of the two production messages, taken from the same alert chat oc_example_topic_chat.
//
// ⚠️ Both have chat_type **group** (not topic_group), and the topic identity is in thread_id;
// what tells root from reply is root_id. See isTopicRootMessage in trigger.go.
const (
	// prodThreadID is the topic the production reply was in (run sesn-example-10).
	prodThreadID = "omt_0000ec7971b4c6d0"
	// prodRootMessageID is the opening message of that topic, the one the reply's root_id points to.
	prodRootMessageID = "om_x100b0000a987ce6b72e5f82e7cd5025"
)

// topicRootMsg builds a **topic root** (the opening message): inside a topic, with root_id / parent_id both empty.
func topicRootMsg(id, text string) *lark.InboundMessage {
	msg := inboundMsg(id, text)
	msg.ChatType = "group" // it really is group
	msg.ThreadID = prodThreadID
	msg.RootID = "" // ← the mark of a root
	msg.ParentID = ""
	msg.Mentioned = false
	msg.SenderType = senderTypeApp    // everyone talking in the alert chat is a bot
	msg.SenderOpenID = otherBotOpenID // third-party bot, not one of our own
	return msg
}

// topicReplyMsg builds a **reply inside a topic** (a descendant): the shape of this false trigger.
//
// It **differs from topicRootMsg only in root_id / parent_id**, so when an assertion fails the difference
// is that one dimension, without going back to the constructor.
func topicReplyMsg(id, text string) *lark.InboundMessage {
	msg := topicRootMsg(id, text)
	msg.RootID = prodRootMessageID // ← the mark of a reply: points to the opening message
	msg.ParentID = prodRootMessageID
	return msg
}

// ---------------------------------------------------------------------------
// Four paired tests: pure function level
// ---------------------------------------------------------------------------

// ⭐ The complete truth table of "topic root / reply / @ or not" in no-@ mode.
//
// **All four must be present together**; see the file header: any one-sided assertion lets a wrong implementation in the opposite direction through.
func TestTopicRootOnlyMatrix(t *testing.T) {
	cases := []struct {
		name      string
		threadID  string
		rootID    string
		parentID  string
		mentioned bool
		want      bool
		why       string
	}{
		// ① topic root + no @ → **triggers**. This is the scenario this mode serves (a new topic arrived).
		{
			"话题root_未@_触发", prodThreadID, "", "", false, true,
			"新话题免 @ 触发正是这档模式的用途，否掉它等于把整档功能关掉",
		},
		// ② topic descendant + no @ → **does not trigger**. ⭐ The one that falsely triggered in production.
		{
			"话题子孙_未@_不触发", prodThreadID, prodRootMessageID, prodRootMessageID, false, false,
			"话题内的跟帖是别人在自己那摊事里继续讨论，没 @ 我们就不该开口（线上误触发）",
		},
		// ③ topic descendant + @ → **triggers**. If @-mentioned it should reply, regardless of mode.
		{
			"话题子孙_已@_触发", prodThreadID, prodRootMessageID, prodRootMessageID, true, true,
			"@ 是显式意图，任何模式下都要触发——这条钉住「仅 root」那层没有越界挡掉 @",
		},
		// ④ non-topic chat + no @ → does not trigger. Existing behavior, not affected by this change.
		{
			"非话题_未@_不触发", "", "", "", false, false,
			"普通群里没 @ 就不触发，这是改造前就有的行为",
		},

		// -- Edge: only parent_id without root_id also counts as a reply ----------------------
		//
		// isTopicRootMessage checking both is defensive (in practice when root_id has a value parent_id always does,
		// but not necessarily the reverse). This pins that "checking one more" is not just for show.
		{
			"话题内_只有parentID_未@_不触发", prodThreadID, "", prodRootMessageID, false, false,
			"parent_id 非空同样说明它挂在别人下面，不是开题那条",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := &lark.InboundMessage{
				AppID: "cli_1", MessageID: "om_x", ChatID: "oc_1",
				ChatType:     "group", // in practice topic groups send group
				ThreadID:     tc.threadID,
				RootID:       tc.rootID,
				ParentID:     tc.parentID,
				Mentioned:    tc.mentioned,
				SenderType:   senderTypeApp,
				SenderOpenID: otherBotOpenID,
				Text:         "P1 报警：错误率上升",
			}
			if got := shouldTrigger(msg, TriggerTopicAndMention, testSelfBots()); got != tc.want {
				t.Errorf("shouldTrigger 期望 %v，实际 %v\n理由：%s", tc.want, got, tc.why)
			}
		})
	}
}

// The two decision stages **must reach the same conclusion**.
//
// mentionFreeEligible is the first gate in Handle (no store read), shouldTrigger is the second.
// If the first wrongly rejects, fixing the second does not help (it returned first); this package's README recorded such an
// incident. This pins the two judgments of the same message together.
func TestMentionFreeEligibleAgreesWithShouldTrigger(t *testing.T) {
	root := topicRootMsg("om_root", "P1 报警")
	reply := topicReplyMsg("om_reply", "已确认，在处理")

	if !mentionFreeEligible(root) {
		t.Error("话题根该通过第一道闸，否则第二道判定永远走不到")
	}
	if mentionFreeEligible(reply) {
		t.Error("话题内的跟帖该在第一道闸就被否掉（省一次读库）")
	}
	// Same conclusion as the second gate.
	if !shouldTrigger(root, TriggerTopicAndMention, testSelfBots()) {
		t.Error("两道闸结论不一致：第一道放过了话题根，第二道却否了")
	}
	if shouldTrigger(reply, TriggerTopicAndMention, testSelfBots()) {
		t.Error("两道闸结论不一致：跟帖不该触发")
	}
}

// A rejected reply must report **its own** reason, not be mixed into "not in a topic".
//
// The two call for completely different actions: seeing "not in a topic", whoever debugs it would check why thread_id was lost,
// while the truth is it is in a topic, just not the opening message. The whole dropped.go file exists for this kind of
// "the log cannot say which gate it got stuck at".
func TestDropReasonDistinguishesNotTopicRoot(t *testing.T) {
	if got := dropReasonForIneligible(topicReplyMsg("om_reply", "跟帖")); got != dropReasonNotTopicRoot {
		t.Errorf("话题内跟帖该报「不是话题根」，实际报了 %q", got)
	}
	plain := inboundMsg("om_plain", "普通群闲聊")
	plain.ChatType = "group"
	if got := dropReasonForIneligible(plain); got != dropReasonIneligible {
		t.Errorf("普通群消息该报「不在话题里」，实际报了 %q", got)
	}
	// The two reasons must really differ, otherwise the two assertions above say nothing.
	if dropReasonNotTopicRoot == dropReasonIneligible {
		t.Fatal("两条原因不能相同，否则日志仍然分不出卡在哪一关")
	}
}

// ---------------------------------------------------------------------------
// Through the real Handle: **neither path**, reaction nor auto reply, may be triggered by a reply
// ---------------------------------------------------------------------------

// ⭐⭐ **A full reproduction of that production false trigger: a reply should get neither a reaction nor a reply.**
//
// The user reported two symptoms: "it added a reaction" and "it auto-replied". This repo has been bitten by "a multi-level feature only wired
// at one level", so they are **asserted separately**: checking only the session count, if the reaction used a different test,
// we would still see the bot reacting to replies after the fix.
//
// In practice the reaction goes through the same gate (markReceived is in dispatch, its only call site is inbound.go:868,
// after the trigger decision); this test pins that "same gate", so if someone later moves the reaction before the decision,
// it turns red.
func TestTopicReplyTriggersNeitherReactionNorReply(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("我来看看这个报警")},
	}
	in, sender, reactor, _, _ := trioFixture(t, conv)
	prepareTopicFixture(t, in)
	setTriggerMode(t, mustStore(t, in), TriggerTopicAndMention)

	// The message a third-party bot replied inside the topic.
	msg := topicReplyMsg("om_thirdparty_reply", "已确认，正在处理中")

	if err := in.Handle(context.Background(), msg); err != nil {
		t.Fatalf("处理失败: %v", err)
	}

	// 1. No session should be created (= no agent run, no auto reply).
	if n := conv.createCount(); n != 0 {
		t.Errorf("话题内的跟帖不该触发 agent，实际建了 %d 个会话", n)
	}
	// 2. No message should be sent.
	if msgs := sender.sent(); len(msgs) != 0 {
		t.Errorf("话题内的跟帖不该收到自动回复，实际发了 %d 条：%+v", len(msgs), msgs)
	}
	// 3. **No reaction at all should be added**. This is the other of the two symptoms the user reported.
	if ops := reactor.snapshot(); len(ops) != 0 {
		t.Errorf("话题内的跟帖不该被打表情，实际有 %d 次表情操作：%+v", len(ops), ops)
	}
}

// The paired positive test: a **topic root** still gets a reaction and a reply.
//
// It differs from the test above only in root_id. Without it, the wrong implementation "turn the whole mode off"
// would make the test above look nicely green.
func TestTopicRootStillReactsAndReplies(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("我来看看这个报警")},
	}
	in, sender, reactor, _, _ := trioFixture(t, conv)
	prepareTopicFixture(t, in)
	setTriggerMode(t, mustStore(t, in), TriggerTopicAndMention)

	msg := topicRootMsg("om_thirdparty_root", "P1 报警：xxx 服务错误率上升")

	if err := in.Handle(context.Background(), msg); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if n := conv.createCount(); n != 1 {
		t.Fatalf("话题根（新话题）该免 @ 触发，期望建 1 个会话，实际 %d 个", n)
	}
	if msgs := sender.sent(); len(msgs) == 0 {
		t.Error("话题根该收到自动回复")
	}
	// The reaction must be added too: "received, processing".
	ops := reactor.snapshot()
	if len(ops) == 0 {
		t.Fatal("话题根该被打 OnIt 表情")
	}
	if ops[0].kind != "add" || ops[0].emoji != lark.ReactionOnIt {
		t.Errorf("第一个表情该是 add OnIt，实际 %+v", ops[0])
	}
	if ops[0].msgID != msg.MessageID {
		t.Errorf("表情该打在用户那条消息上，期望 %q，实际 %q", msg.MessageID, ops[0].msgID)
	}
}

// The Handle version of ③: **a reply that @-mentions us must trigger**, with reaction and reply as usual.
//
// This pins that the "root only" layer **does not overstep**: it should only govern the no-@ path, and @-mentioned messages have nothing to do with it.
// Without it, an overreaching implementation "nothing inside a topic triggers (not even with @)" would be all green,
// and that bug shows up as "@-mentioning the bot in a topic and it plays dead", more infuriating than a false trigger.
func TestMentionedTopicReplyStillTriggers(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("在的，我看看")},
	}
	in, sender, reactor, _, _ := trioFixture(t, conv)
	prepareTopicFixture(t, in)
	setTriggerMode(t, mustStore(t, in), TriggerTopicAndMention)

	msg := topicReplyMsg("om_mentioned_reply", "这个报警什么情况")
	msg.Mentioned = true    // ← the only difference
	msg.SenderType = "user" // a real person @-mentions the bot in a topic
	msg.SenderOpenID = humanOpenID

	if err := in.Handle(context.Background(), msg); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if n := conv.createCount(); n != 1 {
		t.Fatalf("话题内 @ 了机器人必须触发（@ 与模式无关），期望 1 个会话，实际 %d 个", n)
	}
	if msgs := sender.sent(); len(msgs) == 0 {
		t.Error("@ 了就该收到回复")
	}
	if ops := reactor.snapshot(); len(ops) == 0 {
		t.Error("@ 了就该打表情")
	}
}

// In the default mode (mention_only), a topic root should not trigger without @ either: existing behavior is not affected by this change.
//
// ⚠️ **prepareTopicFixture is needed here too**, even though the mode is not changed. Without setting BotOpenID,
// this test would be green because "the fallback branch blocked all bots", **not because the default mode requires @**;
// then deleting NormalizeTriggerMode entirely would still leave it green. With it set, what rejects it is really the mode.
func TestTopicRootStillNeedsMentionUnderDefaultMode(t *testing.T) {
	conv := &fakeConversations{agentIDs: []string{"agent_1"}}
	in, _, reactor, _, _ := trioFixture(t, conv)
	prepareTopicFixture(t, in)
	// Deliberately do not call setTriggerMode, keeping the default mention_only.

	if err := in.Handle(context.Background(), topicRootMsg("om_root_default", "P1 报警")); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if n := conv.createCount(); n != 0 {
		t.Errorf("默认档下话题根也要 @ 才触发，实际建了 %d 个会话", n)
	}
	if ops := reactor.snapshot(); len(ops) != 0 {
		t.Errorf("默认档下不该打表情，实际 %d 次", len(ops))
	}
}

// mustStore pulls the Store used by Inbound out of it, for setTriggerMode to change the mode.
//
// trioFixture does not return the store (it returns progress), and this group of tests all need to change the trigger mode.
func mustStore(t *testing.T, in *Inbound) Store {
	t.Helper()
	if in == nil || in.cfg.Store == nil {
		t.Fatal("入站处理器没有 Store")
	}
	return in.cfg.Store
}

// prepareTopicFixture adjusts trioFixture to what this group needs: turn on no-@ mode, and **set
// BotOpenID on the app**.
//
// ⚠️ **BotOpenID must be set; this is where this group most easily produces a false green.** The app built by trioFixture does not set it,
// so selfBotOpenIDs computes an empty set, and isSelfMessage falls back to the "block all bots" branch.
// That path also blocks third-party bots, so the "replies do not trigger" test **is green for the wrong reason**,
// while the "topic root should trigger" one turns red. That is exactly how it showed up in practice (the same pitfall noted in the comment at
// inbound_test.go:375, where the whole package was green and it was nearly taken as fixed).
//
// With it set to selfBotOpenID, the set is non-empty → exact open_id comparison → third-party bots are allowed,
// and only then does this group really test the "root only" layer.
func prepareTopicFixture(t *testing.T, in *Inbound) {
	t.Helper()
	store := mustStore(t, in)
	ctx := context.Background()
	app, err := store.GetApp(ctx, "fsa_1")
	if err != nil {
		t.Fatalf("取应用失败: %v", err)
	}
	app.BotOpenID = selfBotOpenID
	if _, err := store.SaveApp(ctx, app); err != nil {
		t.Fatalf("存应用失败: %v", err)
	}
	// Confirm the fallback branch is really not reached, or the assertions below test the old logic.
	if isSelfMessage(topicRootMsg("om_probe", "探针"), map[string]bool{selfBotOpenID: true}) {
		t.Fatal("第三方 bot 被当成了自己人，说明常量撞了")
	}
}

// Unit tests of the test itself: isTopicRootMessage only looks at root_id / parent_id.
//
// ⚠️ **Whitespace must count as empty**: Feishu occasionally returns strings with spaces; without TrimSpace
// a root message would be misjudged as a reply, and the whole mode would silently fail for those messages.
func TestIsTopicRootMessage(t *testing.T) {
	cases := []struct {
		name     string
		rootID   string
		parentID string
		want     bool
	}{
		{"都为空_是根", "", "", true},
		{"都是空白_也是根", "  ", "\t", true},
		{"有rootID_不是根", prodRootMessageID, "", false},
		{"有parentID_不是根", "", prodRootMessageID, false},
		{"都有_不是根", prodRootMessageID, prodRootMessageID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := &lark.InboundMessage{ThreadID: prodThreadID, RootID: tc.rootID, ParentID: tc.parentID}
			if got := isTopicRootMessage(msg); got != tc.want {
				t.Errorf("isTopicRootMessage 期望 %v，实际 %v", tc.want, got)
			}
		})
	}
}

// Direct chats are not affected by this test: p2p is already treated as "naturally @" by the inbound layer.
//
// ⚠️ This one matters: **later messages inside a direct-chat topic carry root_id** (measured, see the part in the scope.go file header),
// exactly the same shape as replies in a chat. If the "root only" layer came before the @ check,
// every follow-up sentence in a direct chat would be silently dropped, a zero-error serious regression.
func TestP2PThreadReplyUnaffectedByRootOnlyRule(t *testing.T) {
	msg := inboundMsg("om_p2p_reply", "那明天呢")
	msg.ChatType = ChatTypeP2P
	msg.RootID = prodRootMessageID // later messages inside a direct-chat topic really carry root_id
	msg.ParentID = prodRootMessageID
	msg.Mentioned = true // the inbound layer always sets it to true for p2p, see longconn.go
	msg.SenderType = "user"
	msg.SenderOpenID = humanOpenID

	for _, mode := range []TriggerMode{TriggerMentionOnly, TriggerTopicAndMention} {
		if !shouldTrigger(msg, mode, testSelfBots()) {
			t.Errorf("模式 %q：单聊话题内的后续消息必须照常触发（p2p 是天然 @）", mode)
		}
	}
}

// Self-trigger prevention **was not weakened by this change**: topic roots sent by ourselves must still be blocked.
//
// The no-@ path now has an extra "root only" layer, which could make people think the self-trigger risk dropped; it did not:
// when the bot replies to a main-chat message it **opens a new topic** (reply_in_thread in runAndReply),
// and that reply is itself a topic root, exactly the same shape as a user's new topic.
// Without the isSelfMessage gate, it would immediately start talking to itself.
func TestSelfMessageAsTopicRootStillBlocked(t *testing.T) {
	self := topicRootMsg("om_self_root", "这是我自己刚说的话")
	self.SenderOpenID = selfBotOpenID

	if shouldTrigger(self, TriggerTopicAndMention, testSelfBots()) {
		t.Error("我们自己发的话题根必须挡住，否则无限自触发")
	}
	// The same goes for sibling AMA bots.
	sibling := topicRootMsg("om_sibling_root", "另一个 AMA bot 的话题")
	sibling.SenderOpenID = siblingBotOpenID
	if shouldTrigger(sibling, TriggerTopicAndMention, testSelfBots()) {
		t.Error("另一个 AMA 应用的 bot 发的话题根也要挡，否则两个 bot 互相触发")
	}
	// When we do not know who we are, fall back to "block all bots"; this fallback is not bypassed either.
	if shouldTrigger(topicRootMsg("om_unknown", "报警"), TriggerTopicAndMention, nil) {
		t.Error("selfOpenIDs 为空时该退回「机器人一律挡」，这个兜底不能被「仅 root」绕过")
	}
}

// The root_id of the production reply must **really differ** from the opening message of its topic,
// otherwise all the "root vs reply" assertions above compare the same thing.
//
// Same as the comment on the three open_id constants in trigger_test.go: every dimension of the fixture must really differ.
func TestProdFixtureConstantsDiffer(t *testing.T) {
	if strings.TrimSpace(prodRootMessageID) == "" || strings.TrimSpace(prodThreadID) == "" {
		t.Fatal("fixture 常量不能为空")
	}
	if prodRootMessageID == prodThreadID {
		t.Fatal("root_id 与 thread_id 是两个命名空间（om_ / omt_），相等说明 fixture 抄错了")
	}
	if !strings.HasPrefix(prodRootMessageID, "om_") || !strings.HasPrefix(prodThreadID, "omt_") {
		t.Errorf("前缀不对：root_id 该是 om_、thread_id 该是 omt_，实际 %q / %q",
			prodRootMessageID, prodThreadID)
	}
}
