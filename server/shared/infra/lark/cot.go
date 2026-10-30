// Feishu CoT messages (chain of thought): show the agent's thinking and tool calls live in the
// chat.
//
// # This is a dedicated message type, not a card
//
// Feishu provides a dedicated set of message endpoints for agent scenarios, with event names taken
// from AG-UI
// （RUN_STARTED / REASONING_MESSAGE_CONTENT / TOOL_CALL_START ...）：
//
//	POST /im/v1/message_cot                 create a CoT message
//	PUT  /im/v1/message_cot                 append events (incremental, at most 50 per batch)
//	POST /im/v1/message_cot/complete/{id}   close it
//
// The client renders the collapse, expand and "thinking" interactions; we only feed events in. This
// is much better than building a "thinking…" interactive card ourselves and PATCHing it: that
// cannot collapse, long thinking floods the chat, and every PATCH redraws the whole card.
//
// # Use the open.feishu.cn domain
//
// Use open.feishu.cn like the rest of this package; do not special-case an address for CoT.
//
// # Keep "outcome unknown" apart from "definitely rejected"
//
// This is the most important thing in this file. Create/append/close are all operations **with
// remote side effects**, and on a timeout or 5xx we **do not know whether the other side actually
// did it**:
//
//	definitely rejected (4xx, business code)  surely not applied  → safe to retry
//	outcome unknown (timeout, 5xx)            may be applied      → a retry may create a 2nd CoT message
//
// Treating them the same means users see two chains of thought in the chat. So the latter gets its
// own ErrCOTOutcomeUnknown wrapper, letting the caller follow the "uncertain window" logic instead
// of retrying blindly.
package lark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Hard platform limits on one batch of events. Over the limit, the whole batch is rejected.
const (
	// COTMaxEvents is the max number of events in one append.
	COTMaxEvents = 50
	// COTMaxContentBytes is the max bytes of one event's content.
	COTMaxContentBytes = 4096
)

// AG-UI event types. The names are defined by the platform; do not change them.
const (
	COTEventRunStarted              = "RUN_STARTED"
	COTEventRunFinished             = "RUN_FINISHED"
	COTEventRunError                = "RUN_ERROR"
	COTEventStepStarted             = "STEP_STARTED"
	COTEventStepFinished            = "STEP_FINISHED"
	COTEventTextMessageStart        = "TEXT_MESSAGE_START"
	COTEventTextMessageContent      = "TEXT_MESSAGE_CONTENT"
	COTEventTextMessageEnd          = "TEXT_MESSAGE_END"
	COTEventTextMessageChunk        = "TEXT_MESSAGE_CHUNK"
	COTEventToolCallStart           = "TOOL_CALL_START"
	COTEventToolCallArgs            = "TOOL_CALL_ARGS"
	COTEventToolCallEnd             = "TOOL_CALL_END"
	COTEventToolCallResult          = "TOOL_CALL_RESULT"
	COTEventToolCallChunk           = "TOOL_CALL_CHUNK"
	COTEventMessagesSnapshot        = "MESSAGES_SNAPSHOT"
	COTEventReasoningStart          = "REASONING_START"
	COTEventReasoningMessageStart   = "REASONING_MESSAGE_START"
	COTEventReasoningMessageContent = "REASONING_MESSAGE_CONTENT"
	COTEventReasoningMessageEnd     = "REASONING_MESSAGE_END"
	COTEventReasoningMessageChunk   = "REASONING_MESSAGE_CHUNK"
	COTEventReasoningEnd            = "REASONING_END"
	COTEventReasoningEncryptedValue = "REASONING_ENCRYPTED_VALUE"
)

// COTCompleteReason is the reason for closing.
type COTCompleteReason string

const (
	// COTCompleteDone means a normal finish.
	COTCompleteDone COTCompleteReason = "done"
	// COTCompleteError means finished with an error.
	COTCompleteError COTCompleteReason = "error"
	// COTCompleteTimeout means timed out.
	//
	// **The platform accepts it, but we do not use it.** The client only renders the inner
	// RUN_FINISHED state when the outer reason is done; using timeout would show a "canceled" run
	// as timed out, even though the event stream clearly says interrupted.
	COTCompleteTimeout COTCompleteReason = "timeout"
)

// ErrCOTOutcomeUnknown means the request was sent but we do not know whether it took effect
// (timeout / 5xx).
//
// For how it differs from "definitely rejected", see the file header. A caller that gets it
// **should not retry directly**: retrying a CoT creation may show users two chains of thought.
var ErrCOTOutcomeUnknown = errors.New("lark: CoT 操作结果不确定")

// COTMessage is a CoT message that has been created.
type COTMessage struct {
	// COTID identifies the chain of thought; append and close both need it.
	COTID string
	// MessageID is its message ID in the chat.
	MessageID string
}

// COTEvent is one AG-UI event.
type COTEvent struct {
	// EventType is the AG-UI event type, one of the constants above.
	EventType string `json:"event_type"`
	// Content is the event payload. It **must be a string holding a JSON object**, and each type
	// has required fields; see cotRequiredFields.
	Content string `json:"content"`
	// Timestamp is a millisecond timestamp as a string. **It must strictly increase within one
	// batch**; see ValidateCOTEvents.
	Timestamp string `json:"timestamp"`
}

// CreateCOTOption holds the options for creating a CoT message.
type CreateCOTOption struct {
	// OriginMessageID is the user message to reply to.
	OriginMessageID string
	// ReplyInThread, when true, opens a thread.
	ReplyInThread bool
	// AllowReplyInThreadFallback, when true, retries once without that parameter if the chat does
	// not support thread replies.
	AllowReplyInThreadFallback bool
}

// Creating a CoT also has three optional booleans (enable_badge / update_feed_rank / cot_hidden),
// and **none of them are sent here on purpose**, so all take the platform default false. This is
// not an oversight; it is a decision made after testing.
//
// # cot_hidden=true does not hide the message; it leaves the message stuck at "thinking"
//
// The docs say it "hides the COT message after the COT task completes", which sounds right for
// "throw the thinking away once read".
// Tested (real bot, real direct chat), **that is not the effect at all**:
//
//	the one with cot_hidden=false  turns into "completed" after closing, as expected
//	the one with cot_hidden=true   still shows "thinking" after closing; neither hidden nor completed
//
// Both were sent in the same batch and were identical except for this parameter; closing returned
// code: 0 for both. Checked afterwards with GET /im/v1/messages/{id}: deleted was false for both,
// the body length was the same, and update_time was updated by the close for both. **The difference
// only exists in client rendering**; nothing looks wrong on the server side.
//
// So the cost of turning this on: users see a chain of thought that spins forever, while on our
// side **everything is 200 and nothing shows up in the logs**. If you really want it, verify it
// first on the target client version; do not trust the docs.
//
// # The other two are harmless, but do not turn them on casually either
//
// enable_badge / update_feed_rank only affect the red dot and the chat's position in the list.
// Tested, they **do not affect whether the message renders** (three control groups differing only
// in these two parameters all displayed fine). Default false suits us: the chain of thought is
// appended while running, one turn pushes many batches of events, and turning either on would bump
// the chat to the top / keep growing the red dot on every thinking step of the agent.

// CreateCOT creates a CoT message.
//
// receiveIDType is one of chat_id / open_id / union_id / user_id / email.
func (c *Client) CreateCOT(
	ctx context.Context, token, receiveIDType, receiveID string, opt CreateCOTOption,
) (*COTMessage, error) {
	receiveIDType = strings.TrimSpace(receiveIDType)
	receiveID = strings.TrimSpace(receiveID)
	if !validCOTReceiveIDType(receiveIDType) {
		return nil, fmt.Errorf("%w: receiveIDType %q 不合法", ErrInvalidArgument, receiveIDType)
	}
	if receiveID == "" {
		return nil, fmt.Errorf("%w: receiveID 不能为空", ErrInvalidArgument)
	}

	body := map[string]any{"receive_id": receiveID}
	if id := strings.TrimSpace(opt.OriginMessageID); id != "" {
		body["origin_message_id"] = id
	}
	if opt.ReplyInThread {
		body["reply_in_thread"] = true
	}

	path := "/im/v1/message_cot?receive_id_type=" + url.QueryEscape(receiveIDType)
	msg, err := c.postCOT(ctx, path, token, body)
	// When the chat does not support thread replies, retry without that parameter. This is
	// **definitely rejected**, with no remote side effect, so the retry is safe; without the
	// fallback the whole chain of thought would never show up.
	if err != nil && opt.AllowReplyInThreadFallback && opt.ReplyInThread && isCOTThreadUnsupported(err) {
		delete(body, "reply_in_thread")
		return c.postCOT(ctx, path, token, body)
	}
	return msg, err
}

func (c *Client) postCOT(ctx context.Context, path, token string, body map[string]any) (*COTMessage, error) {
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			COTID     string `json:"cot_id"`
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	if err := c.postJSON(ctx, path, token, body, &res); err != nil {
		return nil, classifyCOTError(err)
	}
	if res.Code != 0 {
		return nil, &APIError{Code: fmt.Sprint(res.Code), Message: res.Msg}
	}
	msg := &COTMessage{
		COTID:     strings.TrimSpace(res.Data.COTID),
		MessageID: strings.TrimSpace(res.Data.MessageID),
	}
	if msg.COTID == "" || msg.MessageID == "" {
		// Created successfully but no identifier returned means we cannot get hold of it, so we can
		// neither append nor close later. Treat it as "unknown" rather than a failure: it was very
		// likely really created remotely.
		return nil, fmt.Errorf("%w: 建 CoT 成功但没拿到 cot_id/message_id", ErrCOTOutcomeUnknown)
	}
	return msg, nil
}

// AppendCOT appends events to a CoT message. At most COTMaxEvents per batch.
func (c *Client) AppendCOT(ctx context.Context, token, cotID, messageID string, events []COTEvent) error {
	cotID, messageID = strings.TrimSpace(cotID), strings.TrimSpace(messageID)
	if cotID == "" || messageID == "" {
		return fmt.Errorf("%w: cotID / messageID 不能为空", ErrInvalidArgument)
	}
	if err := ValidateCOTEvents(events); err != nil {
		return err
	}
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	body := map[string]any{"cot_id": cotID, "message_id": messageID, "events": events}
	if err := c.putJSON(ctx, "/im/v1/message_cot", token, body, &res); err != nil {
		return classifyCOTError(err)
	}
	if res.Code != 0 {
		return &APIError{Code: fmt.Sprint(res.Code), Message: res.Msg}
	}
	return nil
}

// CompleteCOT closes a CoT message. No more appends after closing.
func (c *Client) CompleteCOT(ctx context.Context, token, cotID, messageID string, reason COTCompleteReason) error {
	cotID, messageID = strings.TrimSpace(cotID), strings.TrimSpace(messageID)
	if cotID == "" || messageID == "" {
		return fmt.Errorf("%w: cotID / messageID 不能为空", ErrInvalidArgument)
	}
	if !validCOTCompleteReason(reason) {
		return fmt.Errorf("%w: 封口原因 %q 不合法", ErrInvalidArgument, reason)
	}
	query := url.Values{"message_id": []string{messageID}, "reason": []string{string(reason)}}
	path := "/im/v1/message_cot/complete/" + url.PathEscape(cotID) + "?" + query.Encode()
	var res struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := c.postJSON(ctx, path, token, struct{}{}, &res); err != nil {
		return classifyCOTError(err)
	}
	if res.Code != 0 {
		return &APIError{Code: fmt.Sprint(res.Code), Message: res.Msg}
	}
	return nil
}

// ValidateCOTEvents checks a batch of events before sending.
//
// **Local validation is worth it**: the platform validates the whole batch, one bad event rejects
// the whole batch, and the error does not say which event or which field. Better to report it
// clearly here than send 50 events and then guess from a vague 400.
func ValidateCOTEvents(events []COTEvent) error {
	if len(events) == 0 {
		return fmt.Errorf("%w: 事件不能为空", ErrInvalidArgument)
	}
	if len(events) > COTMaxEvents {
		return fmt.Errorf("%w: 一批最多 %d 条事件，给了 %d 条", ErrInvalidArgument, COTMaxEvents, len(events))
	}
	var prevMS int64
	for i, ev := range events {
		if !ValidCOTEventType(ev.EventType) {
			return fmt.Errorf("%w: 第 %d 条事件类型 %q 不合法", ErrInvalidArgument, i, ev.EventType)
		}
		content := strings.TrimSpace(ev.Content)
		if content == "" || content[0] != '{' || !json.Valid([]byte(content)) {
			return fmt.Errorf("%w: 第 %d 条事件的 content 必须是 JSON 对象", ErrInvalidArgument, i)
		}
		if len(ev.Content) > COTMaxContentBytes {
			return fmt.Errorf("%w: 第 %d 条事件 content %d 字节，超过上限 %d",
				ErrInvalidArgument, i, len(ev.Content), COTMaxContentBytes)
		}
		if missing := missingCOTFields(ev.EventType, content); missing != "" {
			return fmt.Errorf("%w: 第 %d 条 %s 事件缺字段 %s", ErrInvalidArgument, i, ev.EventType, missing)
		}
		ms, err := strconv.ParseInt(strings.TrimSpace(ev.Timestamp), 10, 64)
		if err != nil {
			return fmt.Errorf("%w: 第 %d 条事件的时间戳 %q 不是毫秒整数", ErrInvalidArgument, i, ev.Timestamp)
		}
		// **Timestamps must strictly increase within one batch**; equal is not allowed either.
		// Events produced in the same millisecond are common, so the side that builds the
		// events must spread the timestamps apart (see clock in cotevents.go).
		if ms <= prevMS {
			return fmt.Errorf("%w: 第 %d 条事件的时间戳 %d 没有严格大于前一条 %d",
				ErrInvalidArgument, i, ms, prevMS)
		}
		prevMS = ms
	}
	return nil
}

// ValidCOTEventType reports whether the event type is one the platform knows.
func ValidCOTEventType(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case COTEventRunStarted, COTEventRunFinished, COTEventRunError,
		COTEventStepStarted, COTEventStepFinished,
		COTEventTextMessageStart, COTEventTextMessageContent,
		COTEventTextMessageEnd, COTEventTextMessageChunk,
		COTEventToolCallStart, COTEventToolCallArgs, COTEventToolCallEnd,
		COTEventToolCallResult, COTEventToolCallChunk,
		COTEventMessagesSnapshot,
		COTEventReasoningStart, COTEventReasoningMessageStart,
		COTEventReasoningMessageContent, COTEventReasoningMessageEnd,
		COTEventReasoningMessageChunk, COTEventReasoningEnd,
		COTEventReasoningEncryptedValue:
		return true
	default:
		return false
	}
}

// cotRequiredFields are the required string fields of each event type. Missing one rejects the
// whole batch.
func cotRequiredFields(eventType string) []string {
	switch strings.TrimSpace(eventType) {
	case COTEventRunStarted:
		return []string{"runId", "threadId"}
	case COTEventRunFinished:
		return []string{"runId", "threadId", "status"}
	case COTEventRunError:
		return []string{"message", "code"}
	case COTEventStepStarted, COTEventStepFinished:
		return []string{"stepName", "stepId"}
	case COTEventReasoningStart, COTEventReasoningEnd, COTEventReasoningMessageEnd:
		return []string{"messageId"}
	case COTEventReasoningMessageStart:
		return []string{"messageId", "role"}
	case COTEventReasoningMessageContent:
		return []string{"messageId", "delta"}
	case COTEventToolCallStart:
		return []string{"toolCallId", "toolCallName"}
	case COTEventToolCallArgs:
		return []string{"toolCallId", "delta"}
	case COTEventToolCallEnd:
		return []string{"toolCallId"}
	case COTEventToolCallChunk:
		return []string{"toolCallId", "toolCallName", "delta"}
	case COTEventToolCallResult:
		return []string{"messageId", "toolCallId", "content", "role"}
	default:
		return nil
	}
}

// missingCOTFields returns the first missing required field name, or an empty string when all are
// present.
func missingCOTFields(eventType, content string) string {
	required := cotRequiredFields(eventType)
	if len(required) == 0 {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &obj); err != nil || obj == nil {
		return "(content 解不开)"
	}
	for _, field := range required {
		raw, ok := obj[field]
		if !ok {
			return field
		}
		var s string
		if json.Unmarshal(raw, &s) != nil || strings.TrimSpace(s) == "" {
			return field
		}
	}
	return ""
}

func validCOTReceiveIDType(v string) bool {
	switch v {
	case "chat_id", "open_id", "union_id", "user_id", "email":
		return true
	default:
		return false
	}
}

func validCOTCompleteReason(r COTCompleteReason) bool {
	switch r {
	case COTCompleteDone, COTCompleteError, COTCompleteTimeout:
		return true
	default:
		return false
	}
}

// classifyCOTError sorts transport errors into "definitely rejected" and "outcome unknown". See the
// file header.
//
// The rule: 4xx (except 408) means the other side clearly refused, so it surely did not take
// effect; 408 and 5xx, as well as connection-level failures (no response at all), may already have
// taken effect.
func classifyCOTError(err error) error {
	if err == nil {
		return nil
	}
	apiErr, ok := AsAPIError(err)
	if !ok {
		// No APIError parsed means not even a response came back (timeout, dropped connection); the
		// outcome is unknown.
		return fmt.Errorf("%w: %v", ErrCOTOutcomeUnknown, err)
	}
	if apiErr.HTTPStatusCode == http.StatusRequestTimeout ||
		apiErr.HTTPStatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("%w: %v", ErrCOTOutcomeUnknown, err)
	}
	return err
}

// IsCOTOutcomeUnknown reports whether the error is "outcome unknown".
func IsCOTOutcomeUnknown(err error) bool { return errors.Is(err, ErrCOTOutcomeUnknown) }

// isCOTThreadUnsupported reports whether it is "this chat does not support thread replies".
func isCOTThreadUnsupported(err error) bool {
	if IsCOTOutcomeUnknown(err) {
		return false
	}
	apiErr, ok := AsAPIError(err)
	if !ok {
		return false
	}
	return strings.Contains(strings.ToLower(apiErr.Message), "reply_in_thread is not supported")
}
