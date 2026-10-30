// Tests for trigger modes: when the bot should speak in a chat, and when it must stay quiet.
//
// # Why these tests deserve their own file
//
// Every case pinned here is of the "wrong but no error, just wrong behavior" kind, and getting it wrong in either direction is expensive:
//
//	should trigger but does not  →  the bot plays dead in topic groups, and users think it is broken
//	should not trigger but does  →  **endless self-triggering**, and every turn really burns model and container time
//
// The latter is the most important case in this group; see TestBotOwnMessageNeverTriggersItself.
//
// # Tested at two levels
//
// shouldTrigger is a pure function (see trigger.go), so the decision matrix is laid out and enumerated directly, without starting the whole
// inbound path. A few others go through the real Handle, because what they check is exactly "is this decision wired into the main path
// at all": however correct the decision function is, nobody calling it is a zero-error failure (this package's README recorded one such
// incident, where the whole of reply.go never took effect).

package feishu

import (
	"context"
	"testing"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// Three open_id constants, covering the three identities isSelfMessage must tell apart.
//
// ⚠️ **The three must all differ and all be non-empty**: if any two were equal, the assertions "block ourselves / allow third parties"
// would be always true or always false while the tests stay green (a fixture used for mutation checks must differ on every axis).
const (
	// selfBotOpenID is this app's own bot; its messages must be blocked (self-triggering).
	selfBotOpenID = "ou_self_0000000000000000000000"
	// siblingBotOpenID is the bot of another AMA app in this tenant; it must be blocked too (two bots triggering each other).
	siblingBotOpenID = "ou_sibling_00000000000000000"
	// otherBotOpenID is a third-party bot (an alert bot); it is **allowed**, the same as a real person.
	otherBotOpenID = "ou_thirdparty_0000000000000"
	// humanOpenID is a real person.
	humanOpenID = "ou_human_000000000000000000"
)

// testSelfBots is the set of "our own", matching what inbound.selfBotOpenIDs computes in production.
func testSelfBots() map[string]bool {
	return map[string]bool{selfBotOpenID: true, siblingBotOpenID: true}
}

// topicMsg builds a message from a real person **in a topic group, without @-mentioning the bot**.
//
// This is the only shape the no-@ trigger allows; the other cases start from it and change one dimension, so when an assertion fails,
// the difference is that one dimension, without going back to the constructor.
func topicMsg(id, text string) *lark.InboundMessage {
	msg := inboundMsg(id, text)
	msg.ChatType = chatTypeTopicGroup
	msg.Mentioned = false
	msg.SenderType = "user"
	msg.SenderOpenID = humanOpenID
	return msg
}

// setTriggerMode changes the trigger mode of the pre-filled app.
func setTriggerMode(t *testing.T, store Store, mode TriggerMode) {
	t.Helper()
	ctx := context.Background()
	app, err := store.GetApp(ctx, "fsa_1")
	if err != nil {
		t.Fatalf("取应用失败: %v", err)
	}
	app.TriggerMode = mode
	if _, err := store.SaveApp(ctx, app); err != nil {
		t.Fatalf("存应用失败: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Decision matrix: pure function, enumerated
// ---------------------------------------------------------------------------

// The complete truth table of shouldTrigger.
//
// Covers the combinations of three dimensions: mode (default / no-@) × chat type (topic group / ordinary chat / direct chat) ×
// sender (real person / bot). **Enumerated rather than picking a few**, because the whole risk of this function is
// "some combination was missed"; the missed cell gives no error, it just shows up in production as strange bot behavior.
func TestShouldTriggerMatrix(t *testing.T) {
	cases := []struct {
		name      string
		mode      TriggerMode
		chatType  string
		threadID  string
		mentioned bool
		sender    string
		senderID  string
		want      bool
	}{
		// -- Default mode: only @ counts ------------------------------------------------
		{"默认档_普通群_没@", TriggerMentionOnly, "group", "", false, "user", humanOpenID, false},
		{"默认档_普通群_@了", TriggerMentionOnly, "group", "", true, "user", humanOpenID, true},
		// **The core assertion of existing behavior**: in the default mode topic groups must still require @.
		{"默认档_话题群_没@", TriggerMentionOnly, chatTypeTopicGroup, "", false, "user", humanOpenID, false},
		{"默认档_话题群_@了", TriggerMentionOnly, chatTypeTopicGroup, "", true, "user", humanOpenID, true},

		// -- No-@ mode: only topic groups are relaxed ----------------------------------------
		{"免@档_话题群_没@", TriggerTopicAndMention, chatTypeTopicGroup, "", false, "user", humanOpenID, true},
		{"免@档_话题群_@了", TriggerTopicAndMention, chatTypeTopicGroup, "", true, "user", humanOpenID, true},
		// Non-topic chats are not affected; this is the second half of the mode name "topic group + @ trigger".
		{"免@档_普通群_没@", TriggerTopicAndMention, "group", "", false, "user", humanOpenID, false},
		{"免@档_普通群_@了", TriggerTopicAndMention, "group", "", true, "user", humanOpenID, true},

		// -- Self-trigger prevention: only block "our own", third-party bots trigger as usual --------------
		//
		// ⚠️ Meaning reversed on 2026-09-01: everything here used to be want=false (block all bots).
		// That test also blocked third-party monitoring bots, and in alert chats **only** bots talk
		// (measured: 43 of 44 messages had sender_type=app, 0 from real people), so the whole mode was useless.
		// Now the test is open_id comparison; see isSelfMessage.
		{"免@档_话题群_第三方机器人", TriggerTopicAndMention, chatTypeTopicGroup, "", false, senderTypeApp, otherBotOpenID, true},
		{"免@档_话题内_第三方机器人_真实形状", TriggerTopicAndMention, "group", "omt_1", false, senderTypeApp, otherBotOpenID, true},
		// Sent by us → always blocked; this is the only entry point to the self-trigger loop.
		{"免@档_话题群_我们自己发的", TriggerTopicAndMention, chatTypeTopicGroup, "", false, senderTypeApp, selfBotOpenID, false},
		{"免@档_话题内_我们自己发的_真实形状", TriggerTopicAndMention, "group", "omt_1", false, senderTypeApp, selfBotOpenID, false},
		// The bot of another AMA app → blocked too, otherwise two bots in a chat trigger each other (neither
		// considers the other to be "itself"). It is also in the set; see selfBotOpenIDs.
		{"免@档_话题群_另一个AMA的bot", TriggerTopicAndMention, chatTypeTopicGroup, "", false, senderTypeApp, siblingBotOpenID, false},
		// Ordinary chats (not in a topic) are not affected, regardless of sender.
		{"免@档_普通群_第三方机器人", TriggerTopicAndMention, "group", "", false, senderTypeApp, otherBotOpenID, false},
		// If a bot **explicitly @-mentions** us it still triggers; the @ path does not look at sender.
		{"免@档_话题群_机器人@了我们", TriggerTopicAndMention, chatTypeTopicGroup, "", true, senderTypeApp, otherBotOpenID, true},
		// ⚠️ If we @-mention ourselves it still triggers: the @ path deliberately does not check self; see shouldTrigger.
		// This pins the boundary that "self-trigger prevention only applies to the no-@ path".
		{"免@档_我们自己@了自己", TriggerTopicAndMention, chatTypeTopicGroup, "", true, senderTypeApp, selfBotOpenID, true},

		// -- Direct chats: the inbound layer treats them as naturally @, same behavior in both modes ----------------------
		{"默认档_单聊", TriggerMentionOnly, ChatTypeP2P, "", true, "user", humanOpenID, true},
		{"免@档_单聊", TriggerTopicAndMention, ChatTypeP2P, "", true, "user", humanOpenID, true},

		// -- Existing data: the empty string must fall to the default mode ------------------------------------
		{"空模式_话题群_没@", "", chatTypeTopicGroup, "", false, "user", humanOpenID, false},
		{"空模式_话题群_@了", "", chatTypeTopicGroup, "", true, "user", humanOpenID, true},
		// Dirty values also fall back to the most conservative side, not "allow if unknown".
		{"脏值_话题群_没@", TriggerMode("everything"), chatTypeTopicGroup, "", false, "user", humanOpenID, false},

		// -- ⚠️ The shape Feishu really sends: chat_type=group + non-empty thread_id -------------
		//
		// **This group directly reproduces the production failure.** The test used to be chat_type == "topic_group"
		// (the value range in the SDK comment), while in practice messages in topic groups are sent with chat_type="group",
		// and the topic identity is in thread_id:
		//
		//     chat_type = "group"
		//     thread_id = "omt_0000ed75c64fea21"
		//
		// So the no-@ decision never held, and the whole mode silently failed: the messages really came in, were silently
		// rejected by this one line, and the logs, store, and UI showed nothing wrong.
		{"免@档_话题内_没@_真实形状", TriggerTopicAndMention, "group", "omt_1", false, "user", humanOpenID, true},
		{"免@档_话题内_@了_真实形状", TriggerTopicAndMention, "group", "omt_1", true, "user", humanOpenID, true},
		// In the default mode messages inside a topic still need @: existing behavior is not affected by this fix.
		{"默认档_话题内_没@_真实形状", TriggerMentionOnly, "group", "omt_1", false, "user", humanOpenID, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := &lark.InboundMessage{
				AppID:        "cli_1",
				MessageID:    "om_x",
				ChatID:       "oc_1",
				ChatType:     tc.chatType,
				ThreadID:     tc.threadID,
				Mentioned:    tc.mentioned,
				SenderType:   tc.sender,
				SenderOpenID: tc.senderID,
				Text:         "在吗",
			}
			if got := shouldTrigger(msg, tc.mode, testSelfBots()); got != tc.want {
				t.Errorf("shouldTrigger 期望 %v，实际 %v", tc.want, got)
			}
		})
	}
}

// A nil message should not trigger, and should not panic.
func TestShouldTriggerNilMessage(t *testing.T) {
	if shouldTrigger(nil, TriggerTopicAndMention, testSelfBots()) {
		t.Error("nil 消息不该触发")
	}
}

// ⭐ **When we do not know who we are, fall back to "block all bots".**
//
// Empty selfOpenIDs = botIdentity could not read the identity (Feishu API down, credentials expired).
// Then exact open_id comparison **never matches**, and letting it through means talking to itself in an endless loop,
// with every turn really burning model and container time. So it must fall back to the old behavior.
//
// This is the branch in this change most likely to be "simplified" away: it is normally never reached,
// deleting it keeps every normal test green, and it only blows up the day a real failure happens.
//
// Mutation check: make isSelfMessage return false when len(selfOpenIDs)==0, and this test turns red.
func TestUnknownSelfIdentityBlocksAllBots(t *testing.T) {
	bot := topicMsg("om_1", "P1 报警")
	bot.SenderType = senderTypeApp
	bot.SenderOpenID = otherBotOpenID // third-party bot, normally allowed

	if shouldTrigger(bot, TriggerTopicAndMention, nil) {
		t.Error("不知道自己是谁时，机器人消息必须一律挡——否则可能无限自触发")
	}
	if shouldTrigger(bot, TriggerTopicAndMention, map[string]bool{}) {
		t.Error("空集合与 nil 同义，同样要挡")
	}
	// But real people are not affected: the fallback blocks "bots", not "everyone".
	human := topicMsg("om_2", "帮我看看")
	if !shouldTrigger(human, TriggerTopicAndMention, nil) {
		t.Error("兜底不该殃及真人——那会让免 @ 在身份读不到时整档失效")
	}
}

// The fallback must recognize both sender_type spellings of "bot": app and bot.
//
// # Why both must be recognized
//
// `senderTypeApp = "app"` comes from samples of the **message history API** (taken last round with
// `lark-cli im +chat-messages-list`), while what is judged here is the **event push**:
// the enums of the two paths differ, and three sources contradict each other:
//
//	SDK EventSender (the one events use)   "currently only messages sent by users (user) are supported"
//	field definition in the event docs     ""user" (user) or "bot" (bot)"
//	Sender of the history API              "user / app / anonymous / unknown"
//
// The official docs have **no event example of a bot sending a message** to compare against, and before getting
// the include_bot permission we **had never received a single bot event**, so no sample can be taken in production either.
//
// # The cost of missing one
//
// This fallback is the last gate against self-triggering when selfOpenIDs is empty (our own identity cannot be read).
// Comparing wrongly means it never matches → the bot's own reply is taken as a new message → it answers again → endless loop,
// **with every turn really burning model and container time**. It only caused no trouble before because bot events never came;
// once include_bot takes effect, this gate immediately becomes a defense that is really reached.
//
// Recognizing one more value only makes the fallback stricter and lets no self-trigger through: purely the conservative direction.
func TestUnknownSelfIdentityBlocksBothBotSenderTypes(t *testing.T) {
	for _, senderType := range []string{"app", "bot", "APP", "Bot"} {
		t.Run(senderType, func(t *testing.T) {
			msg := topicMsg("om_1", "P1 报警")
			msg.SenderType = senderType
			msg.SenderOpenID = otherBotOpenID

			if shouldTrigger(msg, TriggerTopicAndMention, nil) {
				t.Errorf("sender_type=%q 也是机器人，不知道自己是谁时必须挡住；"+
					"漏了它就是无限自问自答", senderType)
			}
		})
	}
	// Pin the boundary from the other side: the real-person value must not be hurt.
	human := topicMsg("om_2", "帮我看看")
	human.SenderType = "user"
	if !shouldTrigger(human, TriggerTopicAndMention, nil) {
		t.Error("user 不是机器人，不该被这条兜底挡掉")
	}
}

// ---------------------------------------------------------------------------
// Is it wired into the main path: go through the real Handle
// ---------------------------------------------------------------------------

// 1. In the default mode, no @ in a chat does not trigger (**existing behavior unchanged**).
//
// This guards against the worst outcome, "every chat suddenly starts auto-replying after an upgrade".
func TestDefaultModeIgnoresUnmentionedGroupMessage(t *testing.T) {
	conv := &fakeConversations{agentIDs: []string{"agent_1"}}
	sender := &fakeSender{}
	in, _ := inboundFixture(t, conv, sender)

	// Even topic groups should not trigger: in the default mode chat_type takes no part in the decision.
	msg := topicMsg("om_1", "随便聊聊")

	if err := in.Handle(context.Background(), msg); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if n := conv.createCount(); n != 0 {
		t.Errorf("默认模式下没 @ 的消息不该建会话，实际建了 %d 个", n)
	}
	if cards := sender.sent(); len(cards) != 0 {
		t.Errorf("不该回任何消息，实际发了 %d 条", len(cards))
	}
}

// 2. In topic_and_mention mode, messages in **topic groups** trigger even without @.
func TestTopicModeTriggersInTopicGroupWithoutMention(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("好的。")},
	}
	sender := &fakeSender{}
	in, store := inboundFixture(t, conv, sender)
	setTriggerMode(t, store, TriggerTopicAndMention)

	if err := in.Handle(context.Background(), topicMsg("om_1", "帮我查一下")); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if n := conv.createCount(); n != 1 {
		t.Fatalf("话题群里没 @ 也该触发，期望建 1 个会话，实际 %d 个", n)
	}
	// The user's sentence was really sent into the session, not just an empty session created.
	if evs := conv.appendedEvents(); len(evs) != 1 {
		t.Errorf("该发一次消息，实际 %d 次", len(evs))
	}
}

// 3. In the same mode, messages in **non-topic chats** still do not trigger without @.
//
// It differs from the previous one only in chat_type, pinning the half "no-@ only relaxes topic groups":
// without it, an app with no-@ on would reply to every sentence in every chat.
func TestTopicModeStillRequiresMentionInPlainGroup(t *testing.T) {
	conv := &fakeConversations{agentIDs: []string{"agent_1"}}
	sender := &fakeSender{}
	in, store := inboundFixture(t, conv, sender)
	setTriggerMode(t, store, TriggerTopicAndMention)

	msg := topicMsg("om_1", "随便聊聊")
	msg.ChatType = "group" // ordinary chat

	if err := in.Handle(context.Background(), msg); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if n := conv.createCount(); n != 0 {
		t.Errorf("普通群里没 @ 不该触发，实际建了 %d 个会话", n)
	}
}

// 4. **The bot's own messages do not trigger itself**. This one matters most.
//
// Once no-@ is on, the natural safety net "the bot does not @ itself" is gone. If not blocked, the bot reads what it just
// said, answers again, gets pushed back, in an endless loop, and every turn really burns model and container time.
//
// Here **Bot.OpenID is deliberately not set** (the fixture does not set it either), simulating the case where the OpenID
// comparison in shared/infra/lark fails: that happens when the supervisor cannot read the bot identity, and the comment there says
// "failing to read it is not fatal", which does not hold in no-@ mode, so this defense must be independent of it.
// 4. **Self-trigger prevention**: the bot's own messages must never trigger itself.
//
// This is the most expensive way to be wrong in the whole group: the error is not "one reply fewer" but **endless self-triggering**,
// with every turn really burning model and container time.
//
// ⚠️ **On 2026-09-01 the test changed from sender_type to open_id comparison.** It used to rely on
// "block everything with sender_type==app", which was too wide and also blocked third-party alert bots
// (see TestThirdPartyBotTriggersLikeHuman). Now only "our own" are blocked.
//
// So the SenderOpenID of this test **must** be set to the fixture app's BotOpenID,
// otherwise it tests the fallback branch rather than the new test.
func TestBotOwnMessageNeverTriggersItself(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("我是机器人刚说的话")},
	}
	sender := &fakeSender{}
	in, store := inboundFixture(t, conv, sender)
	setTriggerMode(t, store, TriggerTopicAndMention)

	// The bot's own reply pushed back by Feishu: topic group, no @, sender_type=app,
	// **and the open_id is our own**. Otherwise the shape is exactly the same as the "should trigger" case.
	msg := topicMsg("om_bot_reply", "这是我刚才的回答")
	msg.SenderType = senderTypeApp
	msg.SenderOpenID = selfBotOpenID

	if err := in.Handle(context.Background(), msg); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if n := conv.createCount(); n != 0 {
		t.Fatalf("机器人自己的消息绝不能触发自己（会无限自触发），实际建了 %d 个会话", n)
	}
	if cards := sender.sent(); len(cards) != 0 {
		t.Errorf("不该回任何消息，实际发了 %d 条", len(cards))
	}
}

// ⭐ **Messages from third-party bots must trigger just like real people.** This is the positive assertion of this change.
//
// The production shape (measured in an alert chat): 43 of 44 messages were sent by third-party monitoring
// bots (42 from one alert bot, 1 from another bot), with `sender_type=app`,
// carrying thread_id and @-mentioning nobody, and **0 sent by real people**.
//
// Before the change `!isBotMessage` blocked all of them, so even with topic no-@ on there was no response at all,
// while this is exactly where no-@ is most valuable: nobody talks in alert chats anyway, and what is wanted is
// "bot broadcasts → agent takes over".
//
// Mutation check: add !isBotMessage back into mentionFreeEligible, and this test turns red
// (that mutation specifically pins "the two decision stages must be changed together").
func TestThirdPartyBotTriggersLikeHuman(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("我来看看这个报警")},
	}
	sender := &fakeSender{}
	in, store := inboundFixture(t, conv, sender)
	setTriggerMode(t, store, TriggerTopicAndMention)

	// The real shape of an alert in the alert chat.
	msg := inboundMsg("om_alarm", "P1 报警：xxx 服务错误率上升")
	msg.ChatType = "group"                // it really is group, not topic_group
	msg.ThreadID = "omt_0000b042bc6438a8" // the topic identity is here
	msg.Mentioned = false                 // alerts do not @ anyone
	msg.SenderType = senderTypeApp        // sent by a bot
	msg.SenderOpenID = otherBotOpenID     // ← third party, not us

	if err := in.Handle(context.Background(), msg); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if n := conv.createCount(); n != 1 {
		t.Fatalf("第三方机器人的消息该像真人一样触发，实际建了 %d 个会话", n)
	}
}

// ⭐ **The bot of another AMA app must be blocked too.**
//
// Blocking only "ourselves" is not enough: in production two apps have no-@ on, and they were measured to be in the same chat.
// Their open_ids differ, and neither considers the other to be "itself" → A replies to B, B replies to A in an endless loop,
// just one more participant than self-triggering, at exactly the same cost.
//
// Mutation check: put only this app in selfBotOpenIDs (without walking ListAllApps), and this test turns red.
func TestSiblingAMABotDoesNotTrigger(t *testing.T) {
	conv := &fakeConversations{agentIDs: []string{"agent_1"}}
	sender := &fakeSender{}
	in, store := inboundFixture(t, conv, sender)
	setTriggerMode(t, store, TriggerTopicAndMention)

	// Create another AMA app, whose bot is siblingBotOpenID.
	if _, err := store.CreateApp(context.Background(), &App{
		ID: "fsa_2", WorkspaceID: "ws_2", AppID: "cli_2",
		Name: "另一个 AMA 应用", BotOpenID: siblingBotOpenID,
	}, "secret2"); err != nil {
		t.Fatalf("预置第二个应用失败: %v", err)
	}

	msg := topicMsg("om_sibling", "我是另一个 AMA bot 的回复")
	msg.SenderType = senderTypeApp
	msg.SenderOpenID = siblingBotOpenID

	if err := in.Handle(context.Background(), msg); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if n := conv.createCount(); n != 0 {
		t.Fatalf("另一个 AMA 应用的 bot 不能触发我们（两个 bot 会互相触发到天荒地老），实际建了 %d 个会话", n)
	}
}

// 4.5. **The shape Feishu really sends**: chat_type=group + non-empty thread_id; no-@ must trigger.
//
// # This reproduces the production failure, and is the one in this group to read first
//
// A user turned on "auto reply in topic groups", sent a message in a topic, and **the bot said nothing**. In the end it turned out
// the test was wrong: the code checked chat_type == "topic_group" (the value range in the SDK field comment),
// while what Feishu actually sends for messages in topic groups is:
//
//	chat_type = "group"                 ← not topic_group
//	thread_id = "omt_0000ed75c64fea21"  ← the topic identity is here
//
// So the no-@ path was never reached, and the whole mode silently failed. The messages really came in and were really handled,
// just silently rejected by that one line; the logs, inbound table, and UI showed nothing wrong, the hardest kind to debug.
//
// Go through the real Handle instead of only testing the pure function: the decision has two stages (mentionFreeEligible blocks first,
// then shouldTrigger decides again), and **changing only one leaves the other still blocking**, while the pure function test
// stays green. This test exists to pin "both stages were fixed".
func TestTopicModeTriggersOnRealFeishuShape(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("好的。")},
	}
	sender := &fakeSender{}
	in, store := inboundFixture(t, conv, sender)
	setTriggerMode(t, store, TriggerTopicAndMention)

	// Copy the shape of that production record exactly.
	msg := topicMsg("om_real", "test7")
	msg.ChatType = "group"
	msg.ThreadID = "omt_0000ed75c64fea21"

	if err := in.Handle(context.Background(), msg); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if n := conv.createCount(); n != 1 {
		t.Fatalf("飞书真实形状（chat_type=group + thread_id）下免 @ 该触发，"+
			"期望建 1 个会话，实际 %d 个", n)
	}
	if evs := conv.appendedEvents(); len(evs) != 1 {
		t.Errorf("该把用户那句话发进会话，实际 %d 次", len(evs))
	}
}

// 5. **The first message in a newly created topic group** (no thread_id yet) still triggers.
//
// This reproduces a real production failure: a user created a new topic group, turned on no-@, sent the first sentence, and the bot did not reply.
// The suspicion at the time was "the implementation requires a thread_id to count as a topic message": **the first message after creating a topic group
// often has no thread_id yet**, so it would be missed.
//
// The decision really did not look at thread_id (only chat_type == topic_group), but that was **only implicitly
// true**: topicMsg happened not to set ThreadID, and no assertion said this was intentional. So "someone
// casually adds a thread_id != "" requirement to the decision" would not be caught by existing tests, which is exactly the regression
// this one pins. So ThreadID = "" is written out explicitly here, and noted in the comment, so it is **load-bearing** rather than a coincidence.
//
// chat_type and thread_id are two different things, see scope.go: the former says "is this chat a topic group" (the trigger
// decision uses it), the latter says "is this message inside some topic" (the scope uses it).
func TestTopicModeTriggersOnFirstMessageWithoutThreadID(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("好的。")},
	}
	sender := &fakeSender{}
	in, store := inboundFixture(t, conv, sender)
	setTriggerMode(t, store, TriggerTopicAndMention)

	// The first message in a new topic group: a topic group, no @, sent by a real person, but **all three anchors are empty**:
	// Feishu has not given a thread_id yet, and no reply chain exists.
	msg := topicMsg("om_first", "test")
	msg.ThreadID = ""
	msg.RootID = ""
	msg.ParentID = ""

	if err := in.Handle(context.Background(), msg); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if n := conv.createCount(); n != 1 {
		t.Fatalf("新建话题群的第一条消息（无 thread_id）也该触发，期望建 1 个会话，实际 %d 个", n)
	}
	if evs := conv.appendedEvents(); len(evs) != 1 {
		t.Errorf("该把用户那句话发进会话，实际发了 %d 次", len(evs))
	}
}

// 6. Old data (no triggerMode key in doc) uses the default.
//
// This simulates **real existing rows**: feishu_apps stores the whole App in a jsonb doc, and rows written before the upgrade
// have no triggerMode key at all, which decodes to an empty string. The zero value must fall on the safe side:
// falling on the wrong side means every topic group suddenly starts auto-replying after the upgrade, though nobody flipped any switch.
func TestLegacyAppWithoutTriggerModeStaysMentionOnly(t *testing.T) {
	conv := &fakeConversations{agentIDs: []string{"agent_1"}}
	sender := &fakeSender{}
	in, store := inboundFixture(t, conv, sender)

	// The fixture did not touch TriggerMode when creating the app, which is exactly what an old row looks like when read back. Check it,
	// so that if someone later gives it a default value, this test does not turn into an empty assertion.
	app, err := store.GetApp(context.Background(), "fsa_1")
	if err != nil {
		t.Fatalf("取应用失败: %v", err)
	}
	if app.TriggerMode != TriggerMentionOnly {
		t.Fatalf("老数据读回来该归一化成 %q，实际 %q", TriggerMentionOnly, app.TriggerMode)
	}

	if err := in.Handle(context.Background(), topicMsg("om_1", "随便聊聊")); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if n := conv.createCount(); n != 0 {
		t.Errorf("老数据该按仅 @ 触发，实际建了 %d 个会话", n)
	}
}

// ---------------------------------------------------------------------------
// Normalization and the service layer
// ---------------------------------------------------------------------------

// Existing empty strings and dirty values must map to the default mode; known values stay as they are.
func TestNormalizeTriggerMode(t *testing.T) {
	cases := map[TriggerMode]TriggerMode{
		"":                     TriggerMentionOnly,
		"   ":                  TriggerMentionOnly,
		"everything":           TriggerMentionOnly,
		TriggerMentionOnly:     TriggerMentionOnly,
		TriggerTopicAndMention: TriggerTopicAndMention,
	}
	for in, want := range cases {
		if got := NormalizeTriggerMode(in); got != want {
			t.Errorf("NormalizeTriggerMode(%q) 期望 %q，实际 %q", in, want, got)
		}
	}
}

// Clone must normalize the empty string: the external App should not have an empty triggerMode,
// or the frontend would need another branch to guess its mode.
func TestCloneNormalizesTriggerMode(t *testing.T) {
	app := &App{ID: "a1"}
	if got := app.Clone().TriggerMode; got != TriggerMentionOnly {
		t.Errorf("Clone 后期望 %q，实际 %q", TriggerMentionOnly, got)
	}
	app.TriggerMode = TriggerTopicAndMention
	if got := app.Clone().TriggerMode; got != TriggerTopicAndMention {
		t.Errorf("已设的值不该被改，期望 %q，实际 %q", TriggerTopicAndMention, got)
	}
}

// SetTriggerMode stores and returns the new value; dirty values fall back to the default mode.
func TestServiceSetTriggerMode(t *testing.T) {
	ctx := context.Background()
	svc, app := newBindingService(t, &fakeUnbinder{})

	got, err := svc.SetTriggerMode(ctx, testActor, app.ID, TriggerTopicAndMention)
	if err != nil {
		t.Fatalf("改触发模式失败: %v", err)
	}
	if got.TriggerMode != TriggerTopicAndMention {
		t.Errorf("期望 %q，实际 %q", TriggerTopicAndMention, got.TriggerMode)
	}
	// Really stored, not just a changed return value.
	reread, err := svc.GetApp(ctx, testActor, app.ID)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if reread.TriggerMode != TriggerTopicAndMention {
		t.Errorf("回读期望 %q，实际 %q", TriggerTopicAndMention, reread.TriggerMode)
	}

	// Dirty values fall back to the default mode instead of erroring; see Service.SetTriggerMode for why.
	got, err = svc.SetTriggerMode(ctx, testActor, app.ID, TriggerMode("bogus"))
	if err != nil {
		t.Fatalf("脏值不该报错: %v", err)
	}
	if got.TriggerMode != TriggerMentionOnly {
		t.Errorf("脏值期望退回 %q，实际 %q", TriggerMentionOnly, got.TriggerMode)
	}
}
