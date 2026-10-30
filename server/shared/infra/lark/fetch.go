// Use the bot's own credentials to "fetch a message back and see it clearly": expand the child
// messages of a merged forward and restore card bodies.
//
// # Why one more API call is required, instead of only decoding content
//
// The content pushed over the long connection is **incomplete** for both of these types, and
// incomplete without any hint. Both points were observed (real Feishu + a few real events in local
// run_events), not inferred from the docs:
//
//  1. **The content of a merged forward is a fixed sentinel string** `Merged and Forwarded
//     Message`, **not even JSON**. Not one word of the dozens of forwarded messages is in it. So
//     the agent receives that English sentence: it neither knows the content nor knows that it
//     missed something.
//
//  2. **The content of a card is a "downgraded render"**, not the card that was sent. An observed
//     card message arrives like this:
//
//     {"title":null,"elements":[[{"tag":"img","image_key":"img_v3_…"},
//     {"tag":"text","text":"Please upgrade to the latest client to view this content"}]]}
//
//     The real body ("Got it, I'm here 👋 test passed, I am an alert bot…")
//     **is not in the push at all**. Decode this and feed it to the agent, and it sees
//     "please upgrade the client", which is worse than `[卡片]`, because it looks like real content.
//
// # Two API details, both confirmed by testing
//
//		GET /im/v1/messages/{id}                              fetch this message
//		GET /im/v1/messages/{id}?card_msg_content_type=…      fetch **the original card as sent**
//
//	  - **The child messages of a merged forward are in `data.items` of the same GET**, as a
//	    **flat array** of "1 parent + N children" (not nested), linked by `upper_message_id`.
//	    No second call is needed, and there is **no** such thing as
//	    `container_id_type=merge_forward` (that parameter only belongs to the list messages
//	    endpoint, and only accepts `chat`).
//	  - **`card_msg_content_type=user_card_content` is the only switch to get the real card.**
//	    With it, the "please upgrade the client" message above returns
//	    `{"schema":"2.0","body":{"elements":[{"tag":"markdown","content":"Got it, I'm here 👋…"}]}}`.
//	    Without it you always get the downgraded form, and **both cases are HTTP 200 + code 0**.
//	    This is a classic "succeeded but got fake content"; without the switch nothing tells you.
//	    It works for child messages too (tested: the card inside a forward also got its body back).
//
// # Failures always degrade, never block the reply
//
// This path is **a nice-to-have**: when the fetch works the agent sees more clearly, and when it
// fails the bot must still reply as usual. So every method in this file follows "return (result,
// error), and the caller only logs the error" instead of passing it up. **Timeouts are not handled
// here**: they are the caller's policy (how long the user waits in the chat is a business decision,
// not an integration-layer decision), passed in through ctx by prefetchTimeout in
// feishu/larkcontext.go.
package lark

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// cardContentTypeUserCard is the "give me the original card as sent" switch. See the file header.
//
// ⚠️ **Leaving this parameter out does not cause an error; you just silently get the downgraded
// placeholder content.**
const cardContentTypeUserCard = "user_card_content"

// FetchedMessage is a message fetched back from Feishu (possibly a child message inside a merged
// forward).
//
// It only keeps the fields needed to render as text. **It deliberately has no sender name**: in
// testing, the sender block fetched with the app identity only has `{id, id_type, sender_type,
// tenant_key}`, no name (contact/v3/users and users/batch also only return open_id / union_id).
// Better to leave it out than keep a field that is always empty and makes callers think "sometimes
// it is available".
type FetchedMessage struct {
	// MessageID is this message's own id. **Use it, not the parent message's id, to fetch
	// attachments**: images in a forward belong to the child message, and fetching with the
	// parent's id finds nothing.
	MessageID string
	// MessageType is msg_type: text / post / interactive / image / …
	MessageType string
	// Content is the raw content string (still "a JSON string holding JSON", decoded by
	// decodeContent).
	Content string
	// SenderID is the sender id (open_id or app_id, depending on SenderType).
	SenderID string
	// SenderType is user / app / anonymous.
	SenderType string
	// UpperMessageID is the id of the message one level up in a merged forward; only child messages
	// have it.
	UpperMessageID string
	// CreateTime is the send time (millisecond timestamp string).
	CreateTime string
	// Deleted is true when the message was recalled; Content is usually empty then.
	Deleted bool
}

// GetMessage fetches a message by id.
//
// **The first item of the returned slice is the message itself**, and the rest are its child
// messages (only merged forwards have them, see the file header). Callers link parents and children
// through UpperMessageID.
//
// When withUserCard is true it adds card_msg_content_type=user_card_content, and cards are restored
// to the original JSON as sent. **It must be true to get card bodies**; see the file header.
func (c *Client) GetMessage(
	ctx context.Context, token, messageID string, withUserCard bool,
) ([]FetchedMessage, error) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return nil, fmt.Errorf("%w: messageID 不能为空", ErrInvalidArgument)
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("%w: token 不能为空", ErrInvalidArgument)
	}

	path := "/im/v1/messages/" + url.PathEscape(messageID)
	if withUserCard {
		path += "?card_msg_content_type=" + cardContentTypeUserCard
	}

	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Items []struct {
				MessageID string `json:"message_id"`
				MsgType   string `json:"msg_type"`
				Body      struct {
					Content string `json:"content"`
				} `json:"body"`
				Sender struct {
					ID         string `json:"id"`
					SenderType string `json:"sender_type"`
				} `json:"sender"`
				UpperMessageID string `json:"upper_message_id"`
				CreateTime     string `json:"create_time"`
				Deleted        bool   `json:"deleted"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, path, token, &res); err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, &APIError{Code: fmt.Sprint(res.Code), Message: res.Msg}
	}

	out := make([]FetchedMessage, 0, len(res.Data.Items))
	for _, it := range res.Data.Items {
		out = append(out, FetchedMessage{
			MessageID:      strings.TrimSpace(it.MessageID),
			MessageType:    strings.TrimSpace(it.MsgType),
			Content:        it.Body.Content,
			SenderID:       strings.TrimSpace(it.Sender.ID),
			SenderType:     strings.TrimSpace(it.Sender.SenderType),
			UpperMessageID: strings.TrimSpace(it.UpperMessageID),
			CreateTime:     strings.TrimSpace(it.CreateTime),
			Deleted:        it.Deleted,
		})
	}
	return out, nil
}

// HistoryContainer is the container type when fetching history.
//
// ⚠️ **The two values cover two completely different ranges**; picking the wrong one causes no
// error, the agent just reads the wrong context: `chat` is the whole chat (group main stream or
// direct chat), `thread` is one thread/reply chain.
type HistoryContainer string

const (
	// ContainerChat fetches by chat (group main stream, direct chat).
	ContainerChat HistoryContainer = "chat"
	// ContainerThread fetches by thread.
	ContainerThread HistoryContainer = "thread"
)

// ListMessages fetches a stretch of history.
//
//	container=chat   + id=chat_id     group main stream / direct chat
//	container=thread + id=thread_id   one thread
//
// When newestFirst is true it is in **reverse** time order (newest first, for "fetch the latest
// N"); when false, in forward order (oldest first, for "read a thread from start to end").
//
// ⚠️ **The first item returned by the thread endpoint is the root message that started the thread**
// (tested: its root_id is null), so there is no need to dig the root message out of the group main
// stream. Doing that would pull the main stream for nothing and bring in noise from other threads.
//
// ⚠️ Reading group history needs `im:message.group_msg` in addition to the basic read permission;
// without it you get 230027.
// That permission is **granted per app**, and each user's registered app has to request it
// separately, so this method failing is **normal, not exceptional**, and the caller must degrade
// (see feishu/larkcontext.go).
//
// # ⚠️ It pages, but with a hard limit
//
// **One page is not everything**: tested with page_size=2, the response has `has_more=true` and a
// `page_token`. Fetching only one page while claiming "fetch everything" would **silently give less
// context** once a thread is longer than a page, while the range description still says "all",
// effectively lying to the agent that it has seen everything. This was a real problem caught in
// code review.
//
// So this loops until `has_more=false`, but **there must be a limit** (maxHistoryPages): a chat
// with thousands of messages would eat up memory, time and the context budget all at once. It stops
// at the limit, and the caller decides from "how many items came back" whether to say "truncated".
//
// maxPages <= 0 is treated as 1 page (fetch only one page); callers that want everything must pass
// a number explicitly.
func (c *Client) ListMessages(
	ctx context.Context, token string, container HistoryContainer,
	id string, pageSize, maxPages int, newestFirst bool,
) ([]FetchedMessage, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: 容器 id 不能为空", ErrInvalidArgument)
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("%w: token 不能为空", ErrInvalidArgument)
	}
	if pageSize <= 0 {
		pageSize = 50
	}
	if maxPages <= 0 {
		maxPages = 1
	}

	sort := "ByCreateTimeAsc"
	if newestFirst {
		sort = "ByCreateTimeDesc"
	}

	var out []FetchedMessage
	pageToken := ""
	for page := 0; page < maxPages; page++ {
		q := url.Values{}
		q.Set("container_id_type", string(container))
		q.Set("container_id", id)
		q.Set("page_size", strconv.Itoa(pageSize))
		q.Set("sort_type", sort)
		// Cards must be restored to real content too; cards mixed into history are common (alerts
		// sent by bots are cards).
		// Without this switch, every card in history would turn into "Please upgrade to the latest
		// client".
		q.Set("card_msg_content_type", cardContentTypeUserCard)
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}

		items, next, err := c.listMessagesPage(ctx, token, q)
		if err != nil {
			// ⚠️ **Keep the pages already fetched**: if page two fails, returning the earlier pages
			// beats dropping everything (a bit less context vs none at all). Only a failure on the
			// first page really means there is nothing.
			if len(out) > 0 {
				return out, nil
			}
			return nil, err
		}
		out = append(out, items...)
		if next == "" {
			break
		}
		pageToken = next
	}
	return out, nil
}

// listMessagesPage fetches one page and returns (the messages on this page, the next page token).
//
// An empty next page token means there is no more. **The rule must look at both has_more and
// page_token**: looking only at page_token, some implementations still return a token on the last
// page, so the loop never stops.
func (c *Client) listMessagesPage(
	ctx context.Context, token string, q url.Values,
) ([]FetchedMessage, string, error) {
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			HasMore   bool   `json:"has_more"`
			PageToken string `json:"page_token"`
			Items     []struct {
				MessageID string `json:"message_id"`
				MsgType   string `json:"msg_type"`
				Body      struct {
					Content string `json:"content"`
				} `json:"body"`
				Sender struct {
					ID         string `json:"id"`
					SenderType string `json:"sender_type"`
				} `json:"sender"`
				UpperMessageID string `json:"upper_message_id"`
				CreateTime     string `json:"create_time"`
				Deleted        bool   `json:"deleted"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/im/v1/messages?"+q.Encode(), token, &res); err != nil {
		return nil, "", err
	}
	if res.Code != 0 {
		return nil, "", &APIError{Code: fmt.Sprint(res.Code), Message: res.Msg}
	}

	out := make([]FetchedMessage, 0, len(res.Data.Items))
	for _, it := range res.Data.Items {
		out = append(out, FetchedMessage{
			MessageID:      strings.TrimSpace(it.MessageID),
			MessageType:    strings.TrimSpace(it.MsgType),
			Content:        it.Body.Content,
			SenderID:       strings.TrimSpace(it.Sender.ID),
			SenderType:     strings.TrimSpace(it.Sender.SenderType),
			UpperMessageID: strings.TrimSpace(it.UpperMessageID),
			CreateTime:     strings.TrimSpace(it.CreateTime),
			Deleted:        it.Deleted,
		})
	}

	next := ""
	if res.Data.HasMore {
		next = strings.TrimSpace(res.Data.PageToken)
	}
	return out, next, nil
}

// ChatInfo is the basic info of a chat.
type ChatInfo struct {
	// Name is the group name. For a direct chat it is the other person's name or empty.
	Name string
	// Description is the group description, which often says what the group is for.
	Description string
	// ChatMode is group / topic (topic group).
	ChatMode string
}

// GetChat fetches the basic info of a chat.
//
// Tested: the app identity can get name / description (unlike sender names, which are blocked).
func (c *Client) GetChat(ctx context.Context, token, chatID string) (*ChatInfo, error) {
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return nil, fmt.Errorf("%w: chatID 不能为空", ErrInvalidArgument)
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("%w: token 不能为空", ErrInvalidArgument)
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			ChatMode    string `json:"chat_mode"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, "/im/v1/chats/"+url.PathEscape(chatID), token, &res); err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, &APIError{Code: fmt.Sprint(res.Code), Message: res.Msg}
	}
	return &ChatInfo{
		Name:        strings.TrimSpace(res.Data.Name),
		Description: strings.TrimSpace(res.Data.Description),
		ChatMode:    strings.TrimSpace(res.Data.ChatMode),
	}, nil
}
