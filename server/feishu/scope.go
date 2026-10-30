// Session scope: which Ark session an inbound message belongs to.
//
// # The problem it solves
//
// Before the change, dispatch called CreateConversation once for every message, so **every sentence was a
// brand-new Ark session**. The result was a bot with no memory at all: one sentence asks "what is the weather in Hangzhou", the next asks
// "what about tomorrow", and it does not know what "about" refers to. No error, just baffling answers.
//
// # Four mapping rules
//
//	messages in a topic in a chat (with thread_id / root_id)   one session per topic
//	messages in an ordinary chat (no topic anchor)             one session per chat
//	a **new thread of talk** in a direct chat (no reply chain)  **one session per message**
//	**later messages inside a topic** in a direct chat (root_id) reuse the session of **the topic root message**
//
// Topics come first because a topic is itself Feishu's signal that "this is a separate sub-conversation"; merging it into the chat session
// would let the contexts of two groups of people pollute each other.
//
// # Why every new thread of talk in a direct chat is a new session
//
// Direct chats used to reuse one session by chat_id like ordinary chats, i.e. "one fixed session per direct chat".
// Now, per product requirements, it is **one separate session per new direct message**, because the cost of "the whole direct chat sharing one session"
// outweighs the benefit:
//
//   - **A stuck previous turn drags down the next message.** Only one turn may run on a session at a time (see the comment on
//     TryBeginTurn in inbound.go), and direct chats are one-on-one rapid typing: if the previous turn did not finish properly
//     (Ark does not close the stream, the process is killed, deleting the record fails), every follow-up question only gets
//     "the previous message is still being processed" until runTimeout runs out. In a group others talking acts as a buffer; in a direct chat there is none,
//     so the same pitfall is hardest to bear in direct chats.
//   - **The longer the history, the more expensive and slower.** Reusing a session means feeding the whole history to the model again every turn;
//     direct chats are often a series of quick follow-ups, the session can grow very long, and most of the questions are unrelated to each other.
//
// # "One session per message" and "reuse inside a topic" do not contradict each other
//
// **These two rules govern two different user actions; do not treat them as fighting each other and then delete one.**
//
// Both reasons above are about "the user typing unrelated questions one after another in the direct chat's main line": in that case reuse
// only makes the context grow longer and lets the previous turn's failure drag down the next. But when the user **deliberately goes into the topic formed by the bot's
// reply** and continues, they mean exactly the opposite: "I am talking about that same thing". Without reuse there,
// the agent remembers nothing of what was said, and a user asking "do you have lark commands" gets an amnesiac answer.
//
// Neither reason holds inside a topic: a topic is opened explicitly by the user, and one topic is a bounded sub-conversation
// (history does not grow without limit); and when the previous turn is stuck, the user can always go back to the main line and send a new message to open a new session,
// unlike "the whole direct chat is one session", where there is no way out.
//
// So the order of checks is **topic anchor first, then fall back to "new thread of talk"**. Before the change the order was reversed (the p2p branch
// returned directly, and the topic check was never reached); that was exactly the root cause of "continuing in a topic opened a new session".
//
// # Why direct-chat anchors only accept root_id / parent_id, not thread_id
//
// **Because we must compute exactly the same key as the root message.** The root message was stored as a "new thread of talk", with key
// `message:<its own message_id>`; for later messages in the topic to reuse it, they must compute the same string.
//
// root_id and message_id are in **the same namespace** (the SDK comments on root_id / parent_id both say
// "open_message_id", like om_xxx), while thread_id is another namespace (like omt_xxx).
// Building the key from thread_id gives `message:omt_xxx`, which the root message never bound, so another
// new session is created, which fixes nothing. **In direct chats thread_id never matches the root message; that is the only reason it is excluded**,
// not an omission.
//
// The upside is that existing data needs no migration: the old `message:<message_id>` records in the store are exactly the keys later
// messages in old topics now compute, so continuing in **existing old topics** reuses them directly too.
//
// # Why group anchors prefer thread_id / root_id over parent_id
//
// parent_id is **the previous** message; on an ordinary reply chain it changes with every reply. Using it as the key would make every message on the reply chain
// a new scope, i.e. no reuse at all. root_id is the root of the whole reply chain, thread_id is the topic
// itself; both are constant within a sub-conversation, so they are the anchors that allow reuse.
//
// parent_id is only the fallback when neither anchor exists (defensive: in practice when root_id has a value parent_id always does,
// but not necessarily the reverse). The direct-chat half is the same, except thread_id is not among the candidates; see the previous section.
//
// ⚠️ **"parent_id is the previous message" only holds for ordinary reply chains, not inside topics.** Measured:
// in a topic created by reply_in_thread, every member's parent_id collapses onto the topic root, equal to root_id
// (the parent_id of om_...5069 points to the root om_...c248, not to the bot card right before it).
// This does not affect the priority above (parent_id is only the third fallback), but do not reason from "parent_id changes every time"
// to other conclusions.
package feishu

import (
	"strings"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// Scope types, written into SessionScopeRecord.ScopeType, and also the prefix of scopeKey.
const (
	// scopeThread is "one session per topic (or reply chain)".
	ScopeThread = "thread"
	// scopeChat is "one session per chat (group)".
	ScopeChat = "chat"
	// scopeMessage is "one session per message"; only direct chats use it.
	//
	// The key carries a message_id: a **new thread of talk** uses its own, **later messages inside a topic** use the topic
	// root message's (root_id). For the former, Feishu guarantees message_id is globally unique and never reused, so it naturally
	// "is never bound when looked up", and every main-line direct message reaches session creation; for the latter, the computed key is
	// **exactly the same** as the one the root message stored, so it hits directly and reuses the context. See the file header.
	ScopeMessage = "message"
)

// chatTypeP2P is the ChatType value of a direct chat.
const ChatTypeP2P = "p2p"

// chatTypeTopicGroup is the ChatType value of a **topic group**.
//
// Feishu gives three chat_type values: p2p / group / topic_group. A topic group is a subtype of group,
// and its chat_type is topic_group, not group, so "is this chat a topic group"
// **comes with the inbound message; no extra API call is needed**. The no-@ decision of trigger modes uses it; see trigger.go.
//
// Note it is different from thread_id: thread_id says "is this message inside some topic",
// chat_type says "is this chat itself a topic group". The scope uses the former (scopeForMessage),
// the trigger decision uses the latter.
const chatTypeTopicGroup = "topic_group"

// sessionScope is the scope an inbound message belongs to.
type SessionScope struct {
	// Type is scopeThread / scopeChat.
	Type string
	// Key is the scope key; together with larkAppID it uniquely identifies an Ark session.
	//
	// **It is deterministic**: the same message always computes the same key, so multiple instances computing it independently land on
	// the same row, with no coordination needed. Group / topic keys are decided by chat_id / anchor, so multiple messages in the same chat
	// share one key; direct-chat keys are decided by a message_id: a new thread of talk in the main line uses its own
	// (so each gets its own key), later messages inside a topic use the root message's (so they share a key with the root message).
	// See the file header.
	Key string
	// ThreadID is the topic anchor, non-empty only when Type == scopeThread. Stored purely for debugging.
	ThreadID string
}

// scopeForMessage computes which scope a message belongs to. Rules are in the file header.
//
// **A pure function**: it only looks at the message itself, with no store queries and no clock. So it can be tested exhaustively, and it guarantees
// "the same message computes the same key on any instance", which is the precondition for multiple instances converging on one session.
func ScopeForMessage(msg *lark.InboundMessage) SessionScope {
	if msg == nil {
		return SessionScope{}
	}
	chatID := strings.TrimSpace(msg.ChatID)

	// Direct chat: first check whether it is in a topic, then fall back to "new thread of talk". **The order cannot be reversed**: reversed (p2p returning
	// directly) is the root cause of the "continuing in a topic opened a new session" bug, since the topic check is never
	// reached. See the file header for why.
	if strings.TrimSpace(msg.ChatType) == ChatTypeP2P {
		// Later messages inside a topic: reuse the session of **the topic root message**.
		//
		// The anchor only accepts root_id / parent_id (both om_xxx, the same namespace as message_id),
		// and **deliberately not thread_id** (omt_xxx): the root message was stored with the "new thread of talk" key
		// `message:<its own message_id>`, and only the om_ value can build the same string.
		// thread_id would build `message:omt_xxx`, which the root message never bound, fixing nothing.
		//
		// The type is still scopeMessage, not scopeThread: the key shape must be **exactly
		// the same** as the root message's to be reused; a different prefix would be a new scope again.
		if anchor := directThreadAnchor(msg); anchor != "" {
			return SessionScope{
				Type: ScopeMessage,
				Key:  ScopeMessage + ":" + anchor,
			}
		}

		// New thread of talk: one separate session each.
		//
		// The key is message_id, not chat_id: Feishu guarantees message_id is globally unique, so the same person
		// typing two sentences in the main line always computes two keys, landing on two unrelated sessions. **It still goes through the scope
		// path** (instead of bypassing it and creating the session directly) on purpose: dedup relies on this key, and when Feishu redelivers the same
		// message the key stays the same, so a second session is not created for the same sentence.
		//
		// ⚠️ **"Two sentences in a row always give two keys" only holds when both are really main-line messages, and in actual use
		// they often are not.** When the bot replies to a main-line message it opens a topic (in runAndReply,
		// `inThread := msg.ThreadID == ""`), and after Feishu folds it the client's input focus is in that
		// topic: the user thinks they are continuing in the main line, but the second sentence carries root_id, so it takes the branch above,
		// reuses the root message's session, hits the "one turn per session" serial lock, and gets
		// "the previous message is still being processed". **That is expected behavior, not a bug** (the result of two design rules stacking),
		// see the "two direct messages in a row, the second one is blocked" section of the README;
		// the causal chain is pinned by inbound_p2p_thread_test.go. Do not "fix" the mutual exclusion logic based on this sentence.
		return SessionScope{
			Type: ScopeMessage,
			Key:  ScopeMessage + ":" + strings.TrimSpace(msg.MessageID),
		}
	}

	if anchor := threadAnchor(msg); anchor != "" {
		return SessionScope{
			Type:     ScopeThread,
			Key:      ScopeThread + ":" + anchor,
			ThreadID: anchor,
		}
	}
	return SessionScope{Type: ScopeChat, Key: ScopeChat + ":" + chatID}
}

// threadAnchor gets this message's topic anchor, or an empty string if it is not in any topic / reply chain.
//
// **Group chats use it**; direct chats use directThreadAnchor (thread_id is not a candidate; see the file header for why).
//
// The order is the priority; see the file header for why: thread_id (the topic itself) > root_id (the root of the reply chain) >
// parent_id (fallback).
func threadAnchor(msg *lark.InboundMessage) string {
	for _, candidate := range []string{msg.ThreadID, msg.RootID, msg.ParentID} {
		if v := strings.TrimSpace(candidate); v != "" {
			return v
		}
	}
	return ""
}

// directThreadAnchor gets the root message ID of the topic this message belongs to in a **direct chat**, or an empty string if not in a topic.
//
// The only difference from threadAnchor is that **thread_id is not a candidate**, and that is not an omission: a direct chat reuses
// the root message's session, whose key is `message:<the root message's message_id>`. root_id / parent_id are
// om_xxx (the same namespace as message_id; the SDK comment says open_message_id), so the built key matches exactly;
// thread_id is omt_xxx, and its key matches nothing that was bound. Measured: for a message inside a topic,
// root_id == the root message's message_id, while thread_id is a different value like omt_0000fb76f7d5ca5d.
//
// root_id comes before parent_id for the same reason as threadAnchor. Inside topics the two were measured equal, but on ordinary reply chains
// parent_id changes with each message, so the order still matters.
func directThreadAnchor(msg *lark.InboundMessage) string {
	for _, candidate := range []string{msg.RootID, msg.ParentID} {
		if v := strings.TrimSpace(candidate); v != "" {
			return v
		}
	}
	return ""
}

// valid decides whether this scope can be used to look up / create a mapping.
//
// When the key degrades to only a prefix (chat_id, the three anchors, and message_id all empty) it is unusable: such messages must not be
// taken over by the reuse logic, or an empty chat_id would mix all such messages into the same session.
func (s SessionScope) Valid() bool {
	switch s.Type {
	case ScopeThread, ScopeChat, ScopeMessage:
	default:
		return false
	}
	return strings.TrimSpace(strings.TrimPrefix(s.Key, s.Type+":")) != ""
}
