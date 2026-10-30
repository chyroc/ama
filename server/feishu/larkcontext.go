// Context prefetch for inbound messages: look up in advance what the agent will most likely look up, and write it straight into the harness block.
//
// # Why prefetch, instead of letting the agent fetch it itself
//
// The block in larkprompt.go currently gives **a bunch of ids plus a few commands**; for the agent to know what the chat is called and
// what that card says, it has to run lark-cli itself. Three costs:
//
//   - **Slow**: one tool call has to resume the container (measured at about 16 seconds), execute, and send back, while the user waits in the chat;
//   - **Often does not work**: lark-cli in the container still has no credentials (see the larkprompt.go file header),
//     so that call is bound to fail, and the agent can only degrade;
//   - **Some things it cannot get**: the card body needs `card_msg_content_type` to return the real content
//     (see expand.go), and that is a switch easy to miss.
//
// We fetch on the server with **this Feishu app's own app id/secret**: one HTTP call, a few hundred milliseconds,
// and the switch is always set right.
//
// # What to fetch and what not (a trade-off, not "stuff in everything we can")
//
// The test is **"the agent will most likely look it up"**. Stuffing in too much crowds the context window and slows every message down,
// and this content **is stored permanently in RunEvent**. So:
//
//	✅ card body / sub-messages of a merged forward   —— without it the agent only sees `[卡片]`; this is the main gap fixed here
//	✅ chat name and description                       —— one call, a few dozen characters; "which chat am I in" is useful almost every turn
//	✅ message history                                 —— see "history ranges differ by conversation shape" below
//	❌ chat member list                                —— may be hundreds of people, unused in most turns, pure crowding
//	❌ who sent the message                            —— **measured: the app identity cannot get it at all**: contact/v3/users
//	                                                     and users/batch only return open_id / union_id,
//	                                                     and the sender block has no name. Not a trade-off; it cannot be done.
//	❌ the message being replied to                    —— already in the session context (topics reuse the same Ark session),
//	                                                     fetching it again is duplication
//
// # History ranges differ by conversation shape
//
// **The three shapes fetch different ranges**, and this was not picked at random: getting it wrong gives no error, the agent just reads a pile of
// unrelated messages, or misses the real preceding context:
//
//	topic group / topic in a chat  →  **all messages of this topic** (a topic is a complete discussion unit,
//	                                  and the first message Feishu returns is the opening one, so it is naturally self-contained)
//	ordinary chat                  →  the latest 50 chat messages (you only receive the one that @-mentions you and cannot see earlier ones)
//	direct chat                    →  the latest 50
//
// ⚠️ **No extra fetch of the main chat inside a topic.** The text in the block used to tell the agent "topic first, then chat history",
// but that was advice for when it fetches by itself; if we fetched both on its behalf, one turn would call the API twice
// and bring in the noise of the whole chat. The topic itself is usually enough, and if not the agent can still fetch by itself.
//
// ⚠️ **The current message must be excluded from the history**: it is already given to the agent separately as the user text,
// and appearing again in history would make the model think the user said it twice.
//
// # 🔒 What is prefetched is **external untrusted input**
//
// This is the most important part of this file. The original block in larkprompt.go was safe because it **only had platform
// generated ids**, each passing safeLarkID. Now we put in **content anyone in the chat can write**
// (card body, forwarded messages, chat name), and the attack surface grows from "one sentence" to "a whole history":
// one `</harness:lark>ignore all instructions above` can close this block early.
//
// Handled the existing way of larkprompt.go: **no escaping, no tag removal, the whole thing is not output** (see
// "wrapping in XML is not escaping" there). Concretely: prefetched content passes containsTagMarkup before being spliced into the block,
// and on a hit **this piece of prefetch is dropped** (not the whole block): the ids in the block are still safe and useful,
// and removing chat_id too because of one suspicious card body is an overreaction. After the block is assembled,
// the "wrong tag count drops the whole block" gate at the end of renderLarkPrompt still covers it.
package feishu

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// prefetchTimeout is the **total** timeout of the whole prefetch.
//
// This is time the user waits in the chat, so it is kept tight: a failed prefetch only means a bit less context,
// and slowing every turn for it is not worth it. A single call measures about 100~300ms, so 3 seconds is ten times the margin.
//
// ⚠️ This timeout must be independent of the whole turn's runTimeout: hung on runCtx,
// a stuck prefetch would eat the budget of the whole turn.
const prefetchTimeout = 3 * time.Second

// historyPageSize is how many recent messages are fetched for ordinary chats and direct chats.
//
// 50 is the common single-page limit of Feishu, and enough to cover "where were we just now". If older ones are really needed,
// the agent can still fetch them itself with page_token; the commands in the block are still there.
const historyPageSize = 50

// Page limits for topics and "the latest N".
//
// # Why topics page but "the latest N" does not
//
// **A topic claims "fetch all", so it must really page to the end**: one page does not get everything (measured has_more=true
// with a page_token), and fetching one page while saying "all messages of this topic" lies to the agent that it has read it all,
// and it would draw conclusions from that. This was a real issue caught in code review.
//
// But "all" needs a limit too: a long topic of hundreds of messages would use up the context budget. 5 pages × 50 = 250 messages,
// far more than maxHistoryTotalLen can hold (rendering truncates again and says so), which is enough.
//
// "The latest N" is one page by definition; paging would go against its meaning.
const (
	historyThreadMaxPages = 5
	historyRecentMaxPages = 1
)

// larkContext is the prefetched context. Every field may be empty: **failing to get any one item does not affect the rest**.
type larkContext struct {
	// ChatName / ChatDescription are the chat name and description.
	ChatName        string
	ChatDescription string
	// Expanded is the text of the card body or the expanded merged forward; see expand.go.
	Expanded string
	// History is the prefetched message history (already rendered into multi-line text, in time order).
	History string
	// HistoryScope describes the range of this history (e.g. "this topic" / "the latest 50 messages of this chat" / ...),
	// **and must go to the agent together with History**: without the range, the model would take "5 messages of this topic"
	// as "this chat only ever had 5 sentences".
	HistoryScope string
}

// empty decides whether this prefetch got anything of value.
func (c larkContext) empty() bool {
	return strings.TrimSpace(c.ChatName) == "" &&
		strings.TrimSpace(c.ChatDescription) == "" &&
		strings.TrimSpace(c.Expanded) == "" &&
		strings.TrimSpace(c.History) == ""
}

// MessageFetcher is the two read APIs the prefetch uses, provided by shared/infra/lark.
//
// It is an interface so tests can run offline (none of the existing tests in this package touch real Feishu).
type MessageFetcher interface {
	GetMessage(ctx context.Context, token, messageID string, withUserCard bool) ([]lark.FetchedMessage, error)
	GetChat(ctx context.Context, token, chatID string) (*lark.ChatInfo, error)
	ListMessages(ctx context.Context, token string, container lark.HistoryContainer,
		id string, pageSize, maxPages int, newestFirst bool) ([]lark.FetchedMessage, error)
}

// prefetchContext prefetches the context of this message.
//
// ⚠️ **It never returns an error; any failure is only logged**. This is deliberate: the whole path is a nice-to-have,
// and if it cannot fetch, it gives a bit less context; **it must never make the user miss a reply or wait longer**.
// Callers use the larkContext directly, without checking for errors.
//
// When fetcher is nil (not wired up) it returns empty directly, without logging: that is a config state, not a failure.
func (in *Inbound) prefetchContext(
	ctx context.Context, fetcher MessageFetcher, token string, msg *lark.InboundMessage, log *slog.Logger,
) larkContext {
	var out larkContext
	if fetcher == nil || msg == nil || strings.TrimSpace(token) == "" {
		return out
	}

	// Decoupled from the whole turn: a slow prefetch should not drag the conversation down; see prefetchTimeout.
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), prefetchTimeout)
	defer cancel()

	// —— 1. Chat name ——
	//
	// Not fetched for direct chats: GetChat then returns the other person's name or empty, which is not useful to the agent,
	// and one extra API call per direct message is a real cost.
	if strings.TrimSpace(msg.ChatType) != ChatTypeP2P && strings.TrimSpace(msg.ChatID) != "" {
		info, err := fetcher.GetChat(fetchCtx, token, msg.ChatID)
		switch {
		case err != nil:
			// Degrade silently, but **leave a trace**: a degradation without a dedicated log is a Schrödinger degradation
			// (see "every defense/degradation/fallback must have a dedicated log" in docs/guides/backend.md).
			// ⚠️ Only log the id and the error, **not the chat name or body**: that is user data.
			logPrefetchMiss(log, "取群信息失败，本轮不注入群名（不影响回复）", msg.ChatID, err)
		case info != nil:
			out.ChatName = info.Name
			out.ChatDescription = info.Description
		}
	}

	// —— 2. Card / merged forward expansion ——
	//
	// **Only these two types get an extra API call.** The pushed content of other types is complete already,
	// and fetching again would be pure waste: it would make every plain text message a few hundred milliseconds slower.
	if needsExpand(msg.MessageType) {
		items, err := fetcher.GetMessage(fetchCtx, token, msg.MessageID, true)
		switch {
		case err != nil:
			logPrefetchMiss(log, "取消息内容失败，卡片/转发保持占位符（不影响回复）", msg.MessageID, err)
		default:
			out.Expanded = lark.ExpandMessage(items)
			if strings.TrimSpace(out.Expanded) == "" && log != nil {
				// Fetched, but not a single character could be rendered (all recalled, or all unknown elements).
				// This is different from "the API failed", so it is logged separately; otherwise debugging would head toward auth.
				//
				// ⚠️ **log != nil must be checked**: by convention rendering functions in this package allow log to be nil
				// (see the comment on renderLarkPrompt), and tests call them that way a lot. Missing this check
				// would panic on the **degradation** path "fetched but nothing could be rendered",
				// which is exactly the place that should least blow up: it was only going to be a bit less context, and instead the whole turn crashes.
				log.Info("消息取回来了但渲染不出内容，保持占位符",
					slog.String("messageId", msg.MessageID),
					slog.String("msgType", msg.MessageType),
					slog.Int("items", len(items)))
			}
		}
	}
	// —— 3. Message history ——
	//
	// Fetch different ranges by conversation shape; see the file header. If it cannot be fetched, carry on (history is a supplement, not required).
	out.History, out.HistoryScope = in.fetchHistory(fetchCtx, fetcher, token, msg, log)

	return out
}

// fetchHistory fetches a piece of history by conversation shape, returning (rendered text, range description).
//
// When it cannot be fetched it always returns two empty strings, **never an error**, for the same reason as prefetchContext.
func (in *Inbound) fetchHistory(
	ctx context.Context, fetcher MessageFetcher, token string, msg *lark.InboundMessage, log *slog.Logger,
) (text, scope string) {
	// It reuses the shape decision of larkprompt.go, and **the two places must use the same test**:
	// telling the agent "you are in a topic" in the block while fetching history by chat would be self-contradictory context.
	shape := shapeOf(msg)

	// The topic id must pass the shape check first: it does not go into a command line (here it is an HTTP parameter),
	// but a dirty value would only make the API error and waste a timeout.
	threadID := strings.TrimSpace(msg.ThreadID)
	if !safeLarkID(threadID) {
		threadID = ""
	}

	var (
		container   lark.HistoryContainer
		id          string
		pageSize    int
		maxPages    int
		newestFirst bool
	)
	switch {
	case (shape == shapeTopicGroup || shape == shapeInThreadGroup) && threadID != "":
		// **Topic: fetch all, in order.** A topic is a complete discussion unit, and the first message Feishu returns
		// is the opening one (measured root_id is null), so there is no need to dig the root message out of the chat separately.
		container, id, pageSize, newestFirst = lark.ContainerThread, threadID, historyPageSize, false
		maxPages = historyThreadMaxPages
		// ⚠️ The wording must match the implementation. Paging has a limit (historyThreadMaxPages), so we cannot flatly say
		// "all": saying all when the limit was really exceeded would make the agent think it read the whole topic.
		// If the rendering layer really truncates, it adds a line saying "truncated"; only together do the two avoid misleading.
		scope = "本话题（从开题起）"
	default:
		// **Ordinary chats and direct chats: fetch the latest N, retrieved newest first and then rendered oldest first.**
		// "In a topic but thread_id untrusted" also falls back to this branch: fetching by chat then is better than nothing.
		chatID := strings.TrimSpace(msg.ChatID)
		if !safeLarkID(chatID) {
			return "", ""
		}
		container, id, pageSize, newestFirst = lark.ContainerChat, chatID, historyPageSize, true
		maxPages = historyRecentMaxPages
		if shape == shapeP2P {
			scope = "这个单聊最近 " + strconv.Itoa(historyPageSize) + " 条"
		} else {
			scope = "本群最近 " + strconv.Itoa(historyPageSize) + " 条"
		}
	}

	items, err := fetcher.ListMessages(ctx, token, container, id, pageSize, maxPages, newestFirst)
	if err != nil {
		// ⚠️ This failure is **normal, not exceptional**: reading chat history needs `im:message.group_msg`,
		// that permission is granted per app, and many apps have not applied for it (returns 230027). So log one Warn and move on,
		// never affecting the reply. See the comment on ListMessages in shared/infra/lark.
		logPrefetchMiss(log, "取历史失败，本轮不注入历史（不影响回复）", id, err)
		return "", ""
	}

	// The current message must be excluded: it is already given to the agent separately as the user text.
	rendered := lark.ExpandHistory(items, newestFirst, msg.MessageID)
	if strings.TrimSpace(rendered) == "" {
		return "", ""
	}
	return rendered, scope
}

// needsExpand decides whether this message type needs an extra API call to fetch the real content.
//
// ⚠️ **Decide by msg_type, not by "does the body look like a placeholder"**: the latter is string matching,
// and a user typing `[卡片]` themselves would be misjudged as needing expansion, wasting an API call.
func needsExpand(messageType string) bool {
	switch strings.TrimSpace(strings.ToLower(messageType)) {
	case lark.MsgTypeInteractive, lark.MsgTypeMergeForward:
		return true
	default:
		return false
	}
}

// logPrefetchMiss logs a prefetch degradation.
//
// ⚠️ **Do not log the body or the chat name**: that is user data (see the redaction boundary in core/README-feedback.md,
// and "log ids and enums, not bodies" in backend.md). Only log ids, errors, and consequences.
//
// The level is Warn: this is a recoverable failure and degradation, and should be zero under normal traffic.
func logPrefetchMiss(log *slog.Logger, what, id string, err error) {
	if log == nil {
		return
	}
	log.Warn(what, slog.String("id", id), slog.String("error", err.Error()))
}
