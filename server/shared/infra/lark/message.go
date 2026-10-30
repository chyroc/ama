// Outbound messages: send the agent's reply back to Feishu.
//
// # Why send a card instead of plain text
//
// The agent's reply is **produced as a stream**, a few characters at a time. A plain text message
// is fixed once sent, so you either wait until everything is said (on long tasks the user waits
// without knowing if anything is happening), or send one message per chunk (flooding the chat). A
// card can be sent first and then updated in place, which avoids both problems.
//
//	POST /im/v1/messages/{id}/reply    first frame: reply to the original message, create a card
//	PATCH /im/v1/messages/{id}         after that: edit this card in place
//
// # Three pitfalls
//
// **One: the card content must be serialized into a string and then put into JSON.** Like inbound
// content, it is "a JSON string holding JSON". Putting the object in directly is rejected by
// Feishu.
//
// **Two: reply_in_thread is not accepted in some chats** (error code 230019). When that happens,
// drop the parameter and send again, otherwise the whole reply is lost, and the user sees a bot
// that read the message but never answered.
//
// **Three: PATCH must be throttled.** PATCHing every few characters from the agent would hammer
// Feishu and make the card flicker too much to read. The caller should only call Update when the
// content really changed and enough time passed since the last update.
package lark

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// codeReplyThreadUnsupported means "this chat does not support thread replies". See pitfall two.
const codeReplyThreadUnsupported = 230019

// uuidMaxLength is Feishu's limit on idempotency key length. Longer keys must be shortened; see
// idempotencyKey.
const uuidMaxLength = 50

// SentMessage is a message that has been sent.
type SentMessage struct {
	// MessageID looks like om_xxx; UpdateCard needs it later.
	MessageID string
}

// ReplyCardOption holds the options for replying with a card.
type ReplyCardOption struct {
	// ReplyInThread, when true, opens a thread on the original message.
	//
	// When it is not accepted it automatically falls back to a normal reply (see pitfall two); the
	// caller does not need to handle it.
	ReplyInThread bool

	// IdempotencyKey is the idempotency key. **Strongly recommended**: when resending after a
	// network timeout, Feishu uses it to recognize the same message instead of a new one; otherwise
	// the user sees two identical cards.
	//
	// Build it from stable things (for example run id + purpose), not a timestamp: if the value
	// changes on retry it is useless.
	IdempotencyKey string
}

// ReplyCard replies with a card and returns the new message's ID.
//
// messageID is the user message to reply to.
func (c *Client) ReplyCard(
	ctx context.Context, token, messageID string, card map[string]any, opt ReplyCardOption,
) (*SentMessage, error) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return nil, fmt.Errorf("%w: messageID 不能为空", ErrInvalidArgument)
	}
	content, err := cardContent(card)
	if err != nil {
		return nil, err
	}

	body := map[string]any{
		"msg_type": "interactive",
		"content":  content,
	}
	if opt.ReplyInThread {
		body["reply_in_thread"] = true
	}
	if key := idempotencyKey(opt.IdempotencyKey); key != "" {
		body["uuid"] = key
	}

	path := "/im/v1/messages/" + url.PathEscape(messageID) + "/reply"
	sent, err := c.postMessage(ctx, path, token, body)
	if err == nil {
		return sent, nil
	}

	// Pitfall two: when thread replies are not accepted, drop the parameter and send again.
	// A normal reply still quotes the original message, which is much better than losing the whole
	// reply.
	if opt.ReplyInThread && isReplyThreadUnsupported(err) {
		delete(body, "reply_in_thread")
		return c.postMessage(ctx, path, token, body)
	}
	return nil, err
}

// SendCard sends a card directly into a chat (not replying to any message).
//
// Used when there is no original message to reply to. The everyday path should use ReplyCard: a
// reply carries context and does not look out of place in a group.
func (c *Client) SendCard(
	ctx context.Context, token, chatID string, card map[string]any, idempotency string,
) (*SentMessage, error) {
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return nil, fmt.Errorf("%w: chatID 不能为空", ErrInvalidArgument)
	}
	content, err := cardContent(card)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"receive_id": chatID,
		"msg_type":   "interactive",
		"content":    content,
	}
	if key := idempotencyKey(idempotency); key != "" {
		body["uuid"] = key
	}
	return c.postMessage(ctx, "/im/v1/messages?receive_id_type=chat_id", token, body)
}

// UpdateCard updates a sent card in place.
//
// **It must be throttled** (see pitfall three): the caller should only call it when the content
// really changed and enough time passed since the last update.
func (c *Client) UpdateCard(ctx context.Context, token, messageID string, card map[string]any) error {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return fmt.Errorf("%w: messageID 不能为空", ErrInvalidArgument)
	}
	content, err := cardContent(card)
	if err != nil {
		return err
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	path := "/im/v1/messages/" + url.PathEscape(messageID)
	if err := c.patchJSON(ctx, path, token, map[string]string{"content": content}, &res); err != nil {
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("更新卡片失败（code=%d msg=%s）: %w", res.Code, res.Msg, ErrInvalidResponse)
	}
	return nil
}

// postMessage sends a message and gets back the message_id.
func (c *Client) postMessage(ctx context.Context, path, token string, body map[string]any) (*SentMessage, error) {
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	if err := c.postJSON(ctx, path, token, body, &res); err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, &APIError{Code: fmt.Sprint(res.Code), Message: res.Msg}
	}
	id := strings.TrimSpace(res.Data.MessageID)
	if id == "" {
		return nil, fmt.Errorf("发消息成功但没拿到 message_id: %w", ErrInvalidResponse)
	}
	return &SentMessage{MessageID: id}, nil
}

// TextCard renders the simplest card from a piece of text.
//
// It uses the Feishu card 2.0 schema. The markdown element takes the agent's markdown output
// directly, with no conversion on our side: bold, lists and code blocks all display.
func TextCard(text string) map[string]any {
	text = strings.TrimSpace(text)
	if text == "" {
		// Feishu rejects empty content. Use a placeholder so the user sees "thinking" instead of an
		// error card.
		text = "…"
	}
	return map[string]any{
		"schema": "2.0",
		"config": map[string]any{
			"wide_screen_mode": true,
			// update_multi must be true, otherwise the card cannot be changed after it is sent,
			// and streaming updates break entirely.
			"update_multi": true,
		},
		"body": map[string]any{
			"elements": []map[string]any{
				{"tag": "markdown", "content": text},
			},
		},
	}
}

// cardContent serializes the card object into the string Feishu wants. See pitfall one.
func cardContent(card map[string]any) (string, error) {
	if len(card) == 0 {
		return "", fmt.Errorf("%w: 卡片内容不能为空", ErrInvalidArgument)
	}
	raw, err := json.Marshal(card)
	if err != nil {
		return "", fmt.Errorf("序列化卡片失败: %w", err)
	}
	return string(raw), nil
}

// idempotencyKey shortens the idempotency key: overly long keys are folded with sha256.
//
// Fold instead of truncate: truncation would make "two different purposes of the same run" collide
// into one key when their first 50 characters match, and then Feishu would drop the second as a
// duplicate and the user would see one message fewer.
func idempotencyKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) <= uuidMaxLength {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return "ama-" + hex.EncodeToString(sum[:])[:32]
}

// isReplyThreadUnsupported reports whether the error is "this chat does not support thread
// replies".
func isReplyThreadUnsupported(err error) bool {
	apiErr, ok := AsAPIError(err)
	if !ok {
		return false
	}
	return strings.TrimSpace(apiErr.Code) == fmt.Sprint(codeReplyThreadUnsupported)
}

// ---------------------------------------------------------------------------
// Emoji reactions
// ---------------------------------------------------------------------------

// ReactionOnIt is the "got it, working on it" emoji.
//
// An emoji instead of a "working on it" message: groups are noisy enough, one more pure-noise
// message is annoying, while an emoji sits on the user's own message and is visible at a glance.
const ReactionOnIt = "OnIt"

// ReactionDone is the "this turn is done" emoji.
//
// When a turn ends, OnIt is removed first and then this one is added, so the message **always has
// exactly one emoji** and it is the current state. Only removing without adding would make the
// message look "ignored" again, and users scrolling the chat history could not tell "the bot
// answered" from "the bot never saw it".
//
// The emoji_type values are defined by the platform (`OK` / `THUMBSUP` / `DONE` / ...), and **a
// wrong value causes no error**: the API returns success but the emoji does not appear.
const ReactionDone = "DONE"

// ReactionQueued is the "got it, but it has to wait for the previous turn" emoji.
//
// Keeping it apart from ReactionOnIt is **the only visible feedback** for this setting: when a
// second message is sent in a row the bot does not answer right away, and the user must be able to
// see at a glance "it got it and is queued" rather than "it did not see it".
//
// ⚠️ The value must be one of Feishu's 185 valid emoji_type values. `OneSecond` (one moment) fits
// the meaning exactly. **The first version used `Hourglass`, a value that does not exist at all**;
// the API still returned success, but the emoji never appeared (see the warning at the bottom of
// this file).
const ReactionQueued = "OneSecond"

// ReactionRedirected is the "got it, interrupted the previous turn to do this instead" emoji.
//
// Redirecting is a **destructive** action (the previous turn was killed), so it must use a
// different emoji from queuing. Both mean "got it", but the results are opposite, and sharing one
// emoji would leave users unable to tell whether the bot is waiting for the previous turn or has
// already thrown it away. `GoGoGo` (off we go) fits "already doing this instead". Same as above,
// **the first version's `Fast` was also a value that does not exist**.
const ReactionRedirected = "GoGoGo"

// ⚠️⚠️ **A wrong emoji_type causes no error**: the API returns success but the emoji does not
// appear (see the note on ReactionDone). **The first two versions of this file each got one
// wrong**: `Hourglass` and `Fast` were both made up on a hunch, and neither is among Feishu's 185
// valid values.
//
// The valid list is on Feishu's official "emoji text reference" page. New values **must** be
// checked against that table; neither unit tests nor HTTP 200 can prove an emoji will display.

// AddReaction adds an emoji reaction to a message and returns the reaction ID (needed to remove
// it).
//
// Needs the im:message.reactions:write_only permission.
func (c *Client) AddReaction(ctx context.Context, token, messageID, emojiType string) (string, error) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" || strings.TrimSpace(emojiType) == "" {
		return "", fmt.Errorf("%w: messageID / emojiType 不能为空", ErrInvalidArgument)
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			ReactionID string `json:"reaction_id"`
		} `json:"data"`
	}
	path := "/im/v1/messages/" + url.PathEscape(messageID) + "/reactions"
	body := map[string]any{"reaction_type": map[string]string{"emoji_type": emojiType}}
	if err := c.postJSON(ctx, path, token, body, &res); err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", &APIError{Code: fmt.Sprint(res.Code), Message: res.Msg}
	}
	return strings.TrimSpace(res.Data.ReactionID), nil
}

// RemoveReaction removes an emoji reaction.
//
// Failing to remove it should not fail the caller: that only leaves one extra emoji in the chat,
// which is better than making the whole turn error out.
func (c *Client) RemoveReaction(ctx context.Context, token, messageID, reactionID string) error {
	messageID, reactionID = strings.TrimSpace(messageID), strings.TrimSpace(reactionID)
	if messageID == "" || reactionID == "" {
		return fmt.Errorf("%w: messageID / reactionID 不能为空", ErrInvalidArgument)
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	path := "/im/v1/messages/" + url.PathEscape(messageID) +
		"/reactions/" + url.PathEscape(reactionID)
	if err := c.deleteJSON(ctx, path, token, &res); err != nil {
		return err
	}
	if res.Code != 0 {
		return &APIError{Code: fmt.Sprint(res.Code), Message: res.Msg}
	}
	return nil
}
