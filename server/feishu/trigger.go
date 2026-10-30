// Trigger decision: should an inbound message make the agent speak.
//
// # This gate used to be one-size-fits-all
//
// Before the change, inbound.go started with `if !msg.Mentioned { return nil }`, with a comment saying "this is the easiest
// filter, put it first". The cost of easy was having to @ the bot on every sentence in a chat, and in a **topic group**,
// a user opening a topic already says "what follows is for you", so requiring an @ on every message is awkward.
//
// So now it branches on the app's TriggerMode (see app.go):
//
//	mention_only        triggers only when @-mentioned. **The default mode**, i.e. the behavior before the change
//	topic_and_mention   **the root of a topic** (the message that opens it) triggers automatically without @; everything else needs @
//
// ⚠️ The test is a non-empty `thread_id`, **not** `chat_type == "topic_group"`; the latter is an assumption
// disproved in practice: Feishu sends chat_type `group` for messages in topic groups. See isTopicMessage.
//
// # ⚠️ No-@ is only for "topic roots", not for replies inside a topic: a production false trigger fixed on 2026-09-02
//
// **A topic is a tree; only the root can trigger without @, and all descendants need @.**
//
// This used to check only `isTopicMessage` (is there a thread_id), so **every message in a topic counted**.
// The production result: in an alert chat, a **third-party bot not managed by AMA** replied inside a topic,
// and the AMA bot immediately added a reaction to it and auto-replied for a turn. That message was not a new topic
// and did not @ us, so it should never have disturbed the agent.
//
// The user's definition of this mode is "trigger without @ when a **new topic** is created; **later messages inside** a topic must @":
// opening a new topic is a clear "I have something new to say", while replies inside a topic are others
// continuing their own discussion, and AMA just happens to be in this chat too. **The former is looking for us; the latter is not.**
//
// ⚠️ The cost of this bug was magnified by the shape of topic groups: in an alert chat every alert opens its own topic,
// and replies inside topics (extra broadcasts and status updates from third-party bots) **far outnumber root messages**,
// so false triggers vastly outnumbered real ones. And every turn really burns model and container time.
//
// The test is that root_id / parent_id are **both empty**; see isTopicRootMessage.
//
// # Why a separate file, and why pure functions
//
// For the same reason as scope.go: this is a decision **that can be tested exhaustively**. The inputs are only "message + mode",
// with no store queries, no clock, no network, so a combination like "topic group + no-@ + the bot's own message" can
// be set up and asserted directly, without starting the whole inbound path. Inside inbound.go it would be tangled with dedup,
// session creation, and message sending, and testing one branch would need seven or eight fake dependencies first.
//
// # Self-trigger prevention: after removing the @, bots must be blocked explicitly
//
// **This is the most dangerous part of the whole change, and the main reason this file exists.**
//
// The bot's reply is itself a new message in this chat, and Feishu pushes it back. In @-trigger mode that is no problem:
// the bot does not @ itself, `Mentioned` is naturally false, and that one-size-fits-all gate also blocks self-triggering along the way.
// **Once no-@ is on, that safety net is gone**: the bot's own reply lands in the topic group, the no-@ decision says "trigger",
// so it reads what it just said, answers again, gets pushed back... **endless self-triggering, and every turn really burns
// model and container time**.
//
// # ⚠️ The test narrowed from "all bots" to "only our own": changed on 2026-09-01
//
// This used to be `!isBotMessage(msg)`, blocking **all** bot messages by `sender_type == "app"`.
// That test was too wide and also blocked a whole class of real uses:
//
//	in one alert chat, 43 of 44 messages were sent by third-party monitoring bots
//	(42 from one alert bot, 1 from another bot), and 0 by real people.
//	The user turned on topic no-@ expecting the agent to handle these alerts automatically, and this one line made the whole mode useless.
//
// This is exactly where no-@ is most valuable: **nobody real talks in alert chats anyway**, and what is wanted is
// "bot broadcasts → agent takes over". So now third-party bots **are treated the same as** real people.
//
// Self-trigger prevention is now handled by `isSelfMessage`, and the test is **exact open_id comparison**, not sender_type:
//
//	SenderOpenID == this app's own BotOpenID            → block (this is what I just said)
//	SenderOpenID ∈ bots of other AMA apps in this tenant → block (see "bots triggering each other" below)
//	other bots (third-party alert bots)                 → allow, same as real people
//
// ⚠️ **When we do not know who we are, fall back to the old behavior** (`selfOpenID` empty → still block everything by sender_type).
// This fallback cannot be dropped: botIdentity in `supervisor.go` leaves it empty when it cannot read the identity,
// and then exact comparison never matches, so **without the fallback it is endless self-triggering**. Better to miss a trigger once
// than to let a chat talk to itself and burn forever; the two costs differ by orders of magnitude.
//
// # ⚠️ Bots triggering each other: blocking only ourselves is not enough
//
// In production more than one AMA app has no-@ on (in practice two AMA apps' bots
// were in the same test chat). The two apps have different BotOpenIDs, and **neither considers the other to be "itself"**,
// so A replies to B, B replies to A, the same endless loop, just with one more participant.
//
// So `selfOpenIDs` takes **a set**, not a single id: the bot open_ids of all AMA apps in this tenant
// must be excluded. The set is computed on the fly by the inbound layer from `Store.ListAllApps` (see
// selfBotOpenIDs in inbound.go), and this function remains pure.
//
// ⚠️ **This check is deliberately only on the no-@ path.** The @-trigger path stays as is: a person @-mentioning the bot is explicit
// intent and must not be blocked by any test here.
package feishu

import (
	"strings"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// senderTypeApp / senderTypeBot are the sender_type values Feishu gives for "messages sent by bots".
// Real people are user.
//
// ⚠️ **Both are needed**: `app` comes from the message history API, `bot` from the field definition in the event docs,
// and which one the event side really sends cannot be confirmed. See the comment on isBotMessage, which explains why
// missing one costs endless self-triggering.
const (
	senderTypeApp = "app"
	senderTypeBot = "bot"
)

// shouldTrigger decides whether this message should make the agent speak. **A pure function**; see the file header for why.
//
// mode need not be normalized before being passed in; this function runs NormalizeTriggerMode itself,
// so the empty string of old data and dirty values in the config all fall to the default mode mention_only.
//
// selfOpenIDs is "the bot open_ids of all AMA apps in this tenant", used to block self-triggering and bots triggering each other.
// **Passing nil / an empty set means "we do not know who we are"**, and then it falls back to the old behavior (bots never trigger);
// see the file header for why: without this fallback it is endless self-triggering.
func shouldTrigger(msg *lark.InboundMessage, mode TriggerMode, selfOpenIDs map[string]bool) bool {
	if msg == nil {
		return false
	}
	// If @-mentioned it triggers, regardless of mode. Direct chats are already treated as "naturally @" by the inbound layer (see longconn.go),
	// so direct messages are not affected by any mode.
	if msg.Mentioned {
		return true
	}
	// Messages without an @: only the combination "inside a topic + no-@ enabled" is allowed.
	if NormalizeTriggerMode(mode) != TriggerTopicAndMention {
		return false
	}
	if !isTopicMessage(msg) {
		return false
	}
	// ⚠️ **Only the topic root (the opening message) can skip the @**; replies inside a topic always need an @.
	// Without this layer we get that production false trigger: a third-party bot replies in a topic, and we start talking too.
	// See the file header and isTopicRootMessage.
	if !isTopicRootMessage(msg) {
		return false
	}
	// The no-@ path only blocks "our own"; third-party bots are treated the same as real people. See the file header.
	return !isSelfMessage(msg, selfOpenIDs)
}

// isSelfMessage decides whether this message was sent by **our own** (this app, or the bot of another
// AMA app in this tenant). Their messages must be blocked, or there is an endless loop; see the trigger.go file header.
//
// # The test is exact open_id comparison, not sender_type
//
// sender_type can only answer "is it a bot", not "is it us". Using it costs blocking
// third-party monitoring bots too, which is exactly where no-@ is most valuable (nobody real talks in alert chats).
//
// # ⚠️ When selfOpenIDs is empty, fall back to "block all bots"
//
// That means we **do not know who we are** (botIdentity could not read the identity). Then exact comparison never matches,
// and letting it through means talking to itself in an endless loop. Better to miss a trigger once than to burn forever;
// this fallback branch is now the only call site of isBotMessage, so do not delete it as dead code.
func isSelfMessage(msg *lark.InboundMessage, selfOpenIDs map[string]bool) bool {
	if len(selfOpenIDs) == 0 {
		return isBotMessage(msg)
	}
	return selfOpenIDs[strings.TrimSpace(msg.SenderOpenID)]
}

// isTopicMessage decides whether this message is "a message in a topic".
//
// # ⚠️ The test is thread_id, not chat_type: this was corrected by measurement
//
// This used to be `chat_type == "topic_group"`, because the SDK's field comment says
// chat_type has three values p2p / group / topic_group. **That value range is wrong**,
// at least "messages in topic groups" do not follow it:
//
//	Measured on 2026-08-28, a message in the topic group test had the inbound record
//	    chat_type = "group"                    ← not topic_group
//	    thread_id = "omt_0000ed75c64fea21"     ← the topic ID is here
//
// So the no-@ decision never held, and the whole mode silently failed: the user turned on "auto reply in topic groups",
// sent messages in a topic, the bot said nothing, and the logs, store, and UI showed nothing wrong
// (the messages really came in, they were just silently rejected by this one line).
//
// Use a non-empty thread_id instead: **it is the source of truth for "is this message in a topic"**,
// and that is exactly what the user means: "I opened a topic, what follows is for you".
// Whether the chat itself is a topic group does not matter much: the real test is that the message lands in a topic.
//
// Direct chats are not affected: the inbound layer treats p2p as always "naturally @", so they never get here
// (p2p messages also carry thread_id, see the 13:28 record in the store, so this order cannot be reversed).
func isTopicMessage(msg *lark.InboundMessage) bool {
	if strings.TrimSpace(msg.ThreadID) != "" {
		return true
	}
	// chat_type is still checked: if Feishu ever really sends topic_group it is covered, at the cost of one string comparison.
	return strings.TrimSpace(msg.ChatType) == chatTypeTopicGroup
}

// isTopicRootMessage decides whether this message is **the root of a topic** (the message that opens it).
//
// Only it can trigger without @ in topic_and_mention mode; replies inside the topic (descendants) always need @.
// For the meaning and reasons see the file header; this only records how the test was verified.
//
// # The test: root_id / parent_id are both empty
//
// A topic is a tree. In **inbound events** Feishu uses root_id to say "which tree I hang on":
//
//	topic root (the opening message)   thread_id = omt_xxx, **no root_id / parent_id**
//	reply inside the topic             thread_id = omt_xxx, **root_id = the root message's message_id**
//
// So "root_id is empty" is equivalent to "this message is itself the root". Checking parent_id too is defensive:
// in practice when root_id has a value parent_id always does, but not necessarily the reverse (the same reason threadAnchor in scope.go
// uses parent_id as the third fallback). Checking one fewer would not let wrong ones through; checking one more would not block wrong ones.
//
// # ⚠️ These two values are only given by the **event channel**; read APIs do not return them
//
// Do not cross-check against what `lark-cli im +threads-messages-list` / `+chat-messages-list` return:
// **those two APIs always return root_id: null / parent_id: null for messages in topic groups**,
// even for real replies (measured: all 6 messages of topic omt_0000c624372b1cb7 were null;
// the field that tells root from reply in the read APIs is called thread_message_position).
// Writing the test from the shape of the read APIs would conclude "every message is a root", which fixes nothing.
//
// The test comes from the **inbound payload of production runs** (the copy we store into RunEvent ourselves);
// on 2026-09-02 two were taken from the same alert chat oc_example_topic_chat:
//
//	run sesn-example-11 (topic root)
//	    chat_type = "group"   thread_id = "omt_0000c624372b1cb7"   no root_id
//	run sesn-example-10 (a reply inside a topic, the shape of this false trigger)
//	    chat_type = "group"   thread_id = "omt_0000ec7971b4c6d0"
//	    root_id   = "om_x100b0000a987ce6b72e5f82e7cd5025"
//
// And the message root_id points to is, in the main chat, a topic root with the same thread_id: both ends match.
//
// # Why direct chats are not affected
//
// p2p is already treated as "naturally @" by the inbound layer (longconn.go sets mentioned to true), and shouldTrigger
// returns on its first line, so it never gets here. So the fact that "later messages inside a direct-chat topic carry root_id"
// (the part in the scope.go file header) is not hurt by this test.
func isTopicRootMessage(msg *lark.InboundMessage) bool {
	return strings.TrimSpace(msg.RootID) == "" && strings.TrimSpace(msg.ParentID) == ""
}

// isBotMessage decides whether this message was sent by a bot (including us and other bots in the chat).
//
// ⚠️ **It is no longer the main test for no-@**: since 2026-09-01 the main test is the open_id
// comparison in isSelfMessage, because "is a bot" does not equal "is us", and cutting by the former would also block third-party alert bots
// (exactly the scenario no-@ serves). It now **has only one call site**:
// the fallback of isSelfMessage when "we do not know who we are". Do not delete it because it looks like dead code.
//
// # ⚠️ Both values must be recognized, because which one the event side sends **cannot be confirmed** (checked on 2026-09-02)
//
// The value `senderTypeApp = "app"` comes from the **message history API** (sampled from `lark-cli im
// +chat-messages-list`), while what is judged here is the **event push**; the enums of the two paths differ:
//
//	SDK im/v1/model.go:2909  EventSender (the one P2MessageReceiveV1 uses)
//	                         "Message sender type. Currently only messages sent by users (user) are supported."
//	SDK im/v1/model.go:6539  Sender of the history API
//	                         "user / app / anonymous / unknown"
//
// The field definition in the event docs says `"user" (user) or "bot" (bot)`, and the **only
// JSON example in the docs was sent by a real person**, with no bot sample to compare. Plus before this we **had never received
// a single bot event** (missing the include_bot permission, see lark/scopes.go), so no sample could be taken from production logs
// either. The three sources contradict each other and none can be disproved.
//
// So both are recognized. This is **purely the conservative direction**: recognizing one more value only makes the fallback stricter (more easily judged
// "is a bot" → more easily blocked), and lets no self-trigger through.
//
// ⚠️ **Recognizing only "app" costs endless self-talk.** This function is the last gate against
// self-triggering when selfOpenIDs is empty, and comparing wrongly means it never matches. It caused no trouble before because bot events never came;
// once the include_bot permission takes effect, it immediately becomes a defense that is really reached, and **every round of
// self-triggering really burns model and container time**. It can be narrowed once real payloads are available, but until then do not delete either one.
//
// The empty string is treated as "not a bot": that is a trade-off; wrongly blocking all normal messages is far worse than missing one abnormal shape.
func isBotMessage(msg *lark.InboundMessage) bool {
	switch strings.ToLower(strings.TrimSpace(msg.SenderType)) {
	case senderTypeApp, senderTypeBot:
		return true
	default:
		return false
	}
}
