// Package arkframe reads the few fields we care about from one Ark session event frame.
//
// # Why it exists
//
// Every place that subscribes to a session (the Feishu inbound path, scheduled tasks, the run watcher,
// and every chat channel in server/channel) needs the same handful of answers about a frame: "what type
// is it", "did this turn end", "what did the agent say". Before this package each of them carried its own
// copy, and the copies had already started to differ (one recognized user_interrupt in stop_reason, one
// did not). A difference there gives no error; it shows up as "the bot sometimes sends a pointless
// 'something went wrong' message". One copy removes that whole class of drift.
//
// Pure functions, no I/O. Frames that are not JSON (the `[DONE]` sentinel) give zero values.
package arkframe

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Stop reasons Ark puts in stop_reason.type (see docs/DESIGN.md).
const (
	StopEndTurn          = "end_turn"
	StopRequiresAction   = "requires_action"
	StopUserInterrupt    = "user_interrupt"
	StopRetriesExhausted = "retries_exhausted"
)

// Type returns the top-level type of a frame, or "" when the frame is not JSON.
func Type(raw []byte) string {
	var ev struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return ""
	}
	return ev.Type
}

// StopReason returns stop_reason.type, or "" when the frame has none.
func StopReason(raw []byte) string {
	var ev struct {
		StopReason struct {
			Type string `json:"type"`
		} `json:"stop_reason"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return ""
	}
	return ev.StopReason.Type
}

// AgentText returns the text of an agent.message frame. ok is false for every other frame.
//
// Only agent.message counts: thinking, tool calls and status changes are not the answer, and delta
// frames (event_start / event_delta) carry a different top-level type.
func AgentText(raw []byte) (text string, ok bool) {
	var ev struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return "", false
	}
	if ev.Type != "agent.message" {
		return "", false
	}
	var b strings.Builder
	for _, c := range ev.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String(), true
}

// StartsToolCall reports whether the agent is about to call a tool.
//
// ⚠️ All three kinds must be recognized: built-in tools are agent.tool_use, while custom and MCP tools
// have their own types. Recognizing only the first glues the "I am about to call a tool" narration to
// the final answer for agents that use custom or MCP tools.
func StartsToolCall(raw []byte) bool {
	switch Type(raw) {
	case "agent.tool_use", "agent.custom_tool_use", "agent.mcp_tool_use":
		return true
	}
	return false
}

// Interrupted reports whether this frame says the turn was interrupted by the user.
//
// Ark says it two ways: a user.interrupt frame before the final frame, and stop_reason=user_interrupt
// on the final frame itself. Both must be recognized; missing one makes an interrupted turn look like a
// turn that produced no answer.
func Interrupted(raw []byte) bool {
	return Type(raw) == "user.interrupt" || StopReason(raw) == StopUserInterrupt
}

// FailedUpstream reports whether Ark gave up on this turn because the model side kept failing (most
// often rate limiting). It also satisfies TurnEnded, but it is not "the agent finished answering".
func FailedUpstream(raw []byte) bool {
	return StopReason(raw) == StopRetriesExhausted
}

// TurnEnded reports whether this frame ends the current turn.
//
// Ark does not close the stream after a turn; it keeps waiting for the next user message. So a
// subscriber must spot the final frame itself:
//
//	session.status_idle + any stop reason except requires_action   ended
//	session.status_idle + requires_action                          NOT ended: waiting for a tool result
//	session.status_terminated                                      ended
//
// requires_action matters most: it shows up in the middle of a normal turn, and treating it as the end
// drops the stream right before the agent continues, so the user gets half an answer.
func TurnEnded(raw []byte) bool {
	switch Type(raw) {
	case "session.status_idle":
		return StopReason(raw) != StopRequiresAction
	case "session.status_terminated":
		return true
	}
	return false
}

// UserMessage builds a user.message event.
//
// text is what the user said and goes first. Each non-empty extra becomes its own text block after it
// (for example the `<harness:lark>` context block). Order matters: the session title is taken from the
// text blocks of the first user.message, so platform context placed first would become every title.
func UserMessage(text string, extra ...string) (json.RawMessage, error) {
	content := []map[string]any{{"type": "text", "text": text}}
	for _, e := range extra {
		if strings.TrimSpace(e) != "" {
			content = append(content, map[string]any{"type": "text", "text": e})
		}
	}
	raw, err := json.Marshal(map[string]any{"type": "user.message", "content": content})
	if err != nil {
		return nil, fmt.Errorf("拼消息事件失败: %w", err)
	}
	return raw, nil
}
