// Ark events → CoT snapshot → AG-UI events.
//
// There is a "snapshot" layer in between instead of translating Ark events straight into AG-UI events because delivery must be able to **crash and start over**:
// on restart only the snapshot in the store is available, not the increments collected in memory. So:
//
//	Ark event stream ──accumulate──> ProgressSnapshot ──diff──> AG-UI event batch
//	                                 (stored)                   (only the new part is sent)
//
// # Two measured Ark pitfalls (contrary to the docs)
//
// Both are recorded in web/src/types/events.ts, found by the frontend, and apply here too:
//
//   - **tool_use_id is often not sent**. Pair tool calls and results with `tool_use_id ?? id`;
//     recognizing only the former means no tool call matches its result, and the CoT is full of "running" tools.
//   - **input is a JSON string per the docs, but an object in practice**. Treating it as a string renders
//     `[object Object]`.
//
// # Why closing at the end cannot be skipped
//
// When the agent is interrupted, Ark does not send a "tool finished" event for you; it just stops. But the CoT client
// only knows events: a tool with only TOOL_CALL_START and no TOOL_CALL_END **keeps showing as spinning**,
// even when the whole chain of thought is closed. So on termination we close every unclosed one ourselves; see terminalEvents.

package feishu

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// snapshotBuilder accumulates the Ark event stream into a snapshot. Safe for concurrent use.
type snapshotBuilder struct {
	mu       sync.Mutex
	thinking strings.Builder
	tools    []ProgressToolCall
	// byID is the index of tool calls, used to pair results.
	byID map[string]int
	// terminal / outcome / errMessage are the final state info.
	terminal   bool
	outcome    string
	errMessage string
}

func newSnapshotBuilder() *snapshotBuilder {
	return &snapshotBuilder{byID: make(map[string]int)}
}

// Consume eats one Ark event frame, returning true if the snapshot changed.
//
// The return value is for throttling: if nothing changed do not push; the agent's stream has many frames that do not affect display.
func (b *snapshotBuilder) Consume(raw []byte) bool {
	var ev arkFrame
	if err := json.Unmarshal(raw, &ev); err != nil {
		// Non-JSON frames like [DONE] are skipped quietly.
		return false
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	switch ev.Type {
	case "agent.thinking":
		text := ev.text()
		if text == "" {
			return false
		}
		b.thinking.WriteString(text)
		return true

	case "agent.tool_use", "agent.custom_tool_use", "agent.mcp_tool_use":
		// **tool_use_id is often not sent**; see the file header.
		id := ev.toolID()
		if id == "" {
			return false
		}
		if _, seen := b.byID[id]; seen {
			return false
		}
		b.byID[id] = len(b.tools)
		b.tools = append(b.tools, ProgressToolCall{
			ID:   id,
			Name: ev.Name,
			Args: renderToolInput(ev.Input),
			// **This line is the whole source of "order"**: it records how far the thinking text had accumulated when this call happened.
			// Without it the snapshot only has "which tools exist", and rendering could only pile them in a lump after the text.
			// See "How order is expressed" on ProgressSnapshot.
			AfterTextLen: b.thinking.Len(),
		})
		return true

	// ⚠️ **`user.tool_result` must be here**; it is the main path under self_hosted.
	//
	// Tools run in our own containers (see core/worker.go and shared/infra/dockertools/), and after the worker runs one
	// it returns the result to Ark with **`user.tool_result`**, one of the 7 user-side events Ark accepts.
	// `agent.tool_result` only appears in Ark's managed sandbox (the cloud environment), and this repo **forbids cloud**.
	//
	// That is: recognizing only agent.* means in production **every** tool fails to match its result, so the snapshot
	// is all Done=false, and every tool in the Feishu CoT shows "(not finished)", even though they all actually succeeded.
	// The incident was sesn-example-09: 21 tool calls, all 21 user.tool_result in
	// the event stream, and 0 marked done in the snapshot.
	//
	// **This pitfall gives zero errors**: the event stream itself is complete, and failed pairing just silently does nothing;
	// closing at the end also adds END + RESULT to every tool, so it does not even "keep spinning":
	// they dutifully show as "finished but with no output", which looks like the tools really failed.
	case "user.tool_result", "agent.tool_result", "agent.mcp_tool_result":
		id := ev.toolID()
		idx, ok := b.byID[id]
		if !ok {
			return false
		}
		if b.tools[idx].Done {
			return false
		}
		b.tools[idx].Result = truncateForCOT(ev.text())
		b.tools[idx].Done = true
		return true

	case "session.status_idle":
		// ⚠️ **requires_action is not the end of this turn**; judging it as the end was the root cause of that production issue,
		// "the chain of thought stays at thinking forever" (incident sesn-example-08).
		//
		// Every time the agent calls a tool there is a frame `session.status_idle +
		// stop_reason=requires_action`, which means "I stopped to wait for the tool result";
		// after the worker returns the result the turn continues. That turn had 61 tool calls and
		// 58 such idle frames, and **the very first frame marked the snapshot as final**.
		//
		// The test is exactly the same as turnEnded / core.scheduler.turnEnded / core.applyEventToRun,
		// and the four places must stay in sync; see turnEnded in inbound.go.
		if ev.stopReason() == "requires_action" {
			return false
		}
		return b.markTerminal("done", "")

	case "session.status_terminated":
		return b.markTerminal("done", "")

	case "session.error":
		return b.markTerminal("error", ev.text())

	default:
		return false
	}
}

// MarkTerminal marks the final state from outside (e.g. timeout, or we gave up).
func (b *snapshotBuilder) MarkTerminal(outcome, message string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.markTerminal(outcome, message)
}

func (b *snapshotBuilder) markTerminal(outcome, message string) bool {
	if b.terminal {
		return false
	}
	b.terminal = true
	b.outcome = outcome
	b.errMessage = message
	return true
}

// Snapshot returns a copy of the current snapshot.
func (b *snapshotBuilder) Snapshot() ProgressSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return ProgressSnapshot{
		Thinking: b.thinking.String(),
		Tools:    append([]ProgressToolCall(nil), b.tools...),
		// Snapshots built by this process always have AfterTextLen fully recorded. See ProgressSnapshot.Ordered.
		Ordered:      true,
		Terminal:     b.terminal,
		Outcome:      b.outcome,
		ErrorMessage: b.errMessage,
	}
}

// arkFrame is a minimal projection of an Ark event.
type arkFrame struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Name      string `json:"name"`
	// Input is a JSON string per the docs, but **an object in practice**, so it is taken as any. See the file header.
	Input   any `json:"input"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	// Output is another carrier form of the tool result.
	Output any `json:"output"`
	// StopReason says whether this frame's idle is "finished answering" or "stopped to wait for a tool".
	// **The two must be kept apart**; see the session.status_idle branch in Consume.
	StopReason struct {
		Type string `json:"type"`
	} `json:"stop_reason"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// stopReason gets the stop reason of this frame, defaulting to an empty string.
func (f arkFrame) stopReason() string {
	return strings.TrimSpace(f.StopReason.Type)
}

// toolID gets the tool call identifier. **When tool_use_id is missing, use the event's own id**; see the file header.
func (f arkFrame) toolID() string {
	if id := strings.TrimSpace(f.ToolUseID); id != "" {
		return id
	}
	return strings.TrimSpace(f.ID)
}

// text extracts the text from the event.
func (f arkFrame) text() string {
	var b strings.Builder
	for _, c := range f.Content {
		if c.Type == "text" || c.Type == "" {
			b.WriteString(c.Text)
		}
	}
	if b.Len() > 0 {
		return b.String()
	}
	if msg := strings.TrimSpace(f.Error.Message); msg != "" {
		return msg
	}
	if f.Output != nil {
		return renderToolInput(f.Output)
	}
	return ""
}

// renderToolInput renders tool input/results as text. Both shapes must be handled; see the file header.
func renderToolInput(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(raw)
	}
}

// truncateForCOT truncates overly long text.
//
// The content limit of a single CoT event is 4096 bytes, and over it **the whole batch is rejected**: one tool with long output
// would keep every other event in the batch from being sent. So it is cut at the source, leaving enough room for the JSON wrapping.
//
// Accumulate by rune instead of cutting by byte: cutting by byte would break multi-byte characters, making content invalid UTF-8,
// which the platform also rejects, and the error message only says "invalid parameter".
func truncateForCOT(s string) string {
	const limit = lark.COTMaxContentBytes / 2
	if len(s) <= limit {
		return s
	}
	const ellipsis = "…（已截断）"
	budget := limit - len(ellipsis)
	var b strings.Builder
	for _, r := range s {
		if b.Len()+len(string(r)) > budget {
			break
		}
		b.WriteRune(r)
	}
	return b.String() + ellipsis
}

// ---------------------------------------------------------------------------
// Snapshot → AG-UI events
// ---------------------------------------------------------------------------

// cotClock guarantees the timestamps of one batch of events are **strictly increasing**.
//
// The platform requires strictly increasing, not even equal (see lark.ValidateCOTEvents), and a batch is almost always generated within
// the same millisecond. So instead of using the current time directly, it keeps a counter that only goes up.
type cotClock struct {
	last int64
}

func (c *cotClock) next(now time.Time) string {
	ms := now.UnixMilli()
	if ms <= c.last {
		ms = c.last + 1
	}
	c.last = ms
	return strconv.FormatInt(ms, 10)
}

// cotEventBuilder translates the difference "previous snapshot → this snapshot" into AG-UI events.
type cotEventBuilder struct {
	runID    string
	threadID string
	clock    cotClock
}

func newCOTEventBuilder(runID string) *cotEventBuilder {
	return &cotEventBuilder{runID: runID, threadID: runID}
}

// Start is the first batch of events of this chain of thought.
func (b *cotEventBuilder) Start(now time.Time) []lark.COTEvent {
	return []lark.COTEvent{
		b.event(now, lark.COTEventRunStarted, map[string]string{
			"runId": b.runID, "threadId": b.threadID,
		}),
		b.event(now, lark.COTEventReasoningStart, map[string]string{
			"messageId": b.messageID(),
		}),
		b.event(now, lark.COTEventReasoningMessageStart, map[string]string{
			"messageId": b.messageID(), "role": "assistant",
		}),
	}
}

// Diff translates the difference between two snapshots into events. A nil prev means this is the first one.
//
// # The order of events is the client's display order
//
// The client sorts by timestamp, and timestamps are incremented by cotClock in **production order** (see its comment).
// That is: the order this function appends = the order users see. So "tool calls interleaved in the text"
// needs no extra protocol support; just append in the real order here.
//
// Conversely, once this sends all the text first and then goes through the tools, users always see "a big block of thinking +
// a lump of tools piled at the bottom", which does not match the agent's actual rhythm (think a bit, check a bit, think again),
// and the more tools the worse it gets. The fix is to cut the text with AfterTextLen; see ProgressSnapshot.
//
// # How "send only increments" holds with multiple segments
//
// Resending everything would make the client show the same passage twice. With one segment TrimPrefix was enough; with multiple segments
// it relies on sent, a **byte watermark that only goes up**: each segment is cut starting at sent, and after sending sent moves to the segment end,
// so over the whole turn every byte is sent exactly once, regardless of how many segments there are.
func (b *cotEventBuilder) Diff(prev *ProgressSnapshot, next ProgressSnapshot, now time.Time) []lark.COTEvent {
	var out []lark.COTEvent
	var prevThinking string
	var prevTools []ProgressToolCall
	if prev != nil {
		prevThinking, prevTools = prev.Thinking, prev.Tools
	}

	// sent is the watermark of thinking text already pushed to the client, in bytes. See "send only increments" above.
	sent := len(prevThinking)
	if !strings.HasPrefix(next.Thinking, prevThinking) {
		// Thinking text should only grow (snapshotBuilder only appends). Really getting here means
		// upstream changed the content, and resending from the start is better than misaligning every later segment; it is only a fallback, the normal path never gets here.
		sent = 0
	}

	// emitText sends the text in (sent, upto] and advances the watermark.
	emitText := func(upto int) {
		if upto > len(next.Thinking) {
			upto = len(next.Thinking)
		}
		if upto <= sent {
			return
		}
		for _, chunk := range chunkText(next.Thinking[sent:upto], lark.COTMaxContentBytes/2) {
			out = append(out, b.event(now, lark.COTEventReasoningMessageContent, map[string]string{
				"messageId": b.messageID(), "delta": chunk,
			}))
		}
		sent = upto
	}

	// Existing snapshots (Ordered false) have no trustworthy interleaving positions, so use **the old order from before the fix**:
	// send all text increments first, then the tools. Their AfterTextLen (all 0) must not be used to cut:
	// that would throw all tools to the very top of the message, messier than not interleaving, and the victims would be exactly the turns
	// running during a rolling deploy. See ProgressSnapshot.Ordered.
	if !next.Ordered {
		emitText(len(next.Thinking))
	}

	for i, tool := range next.Tools {
		// **First add the thinking before this call**, so the tool events land where they really happened.
		if next.Ordered {
			emitText(tool.AfterTextLen)
		}

		var before *ProgressToolCall
		if i < len(prevTools) {
			before = &prevTools[i]
		}
		if before == nil {
			out = append(out, b.event(now, lark.COTEventToolCallStart, map[string]string{
				"toolCallId": tool.ID, "toolCallName": nonEmpty(tool.Name, "tool"),
			}))
			if tool.Args != "" {
				out = append(out, b.event(now, lark.COTEventToolCallArgs, map[string]string{
					"toolCallId": tool.ID, "delta": truncateForCOT(tool.Args),
				}))
			}
		}
		justDone := tool.Done && (before == nil || !before.Done)
		if justDone {
			out = append(out, b.toolEnd(now, tool)...)
		}
	}

	// The thinking text after the last tool (with no tools, that is the whole increment).
	emitText(len(next.Thinking))
	return out
}

// Terminal is the final batch: close everything unclosed, then finish. See the file header.
func (b *cotEventBuilder) Terminal(snapshot ProgressSnapshot, now time.Time) []lark.COTEvent {
	var out []lark.COTEvent

	// **Unfinished tools get END + RESULT added**, or the client keeps showing them as spinning.
	for _, tool := range snapshot.Tools {
		if tool.Done {
			continue
		}
		closed := tool
		closed.Result = nonEmpty(tool.Result, "（未完成）")
		out = append(out, b.toolEnd(now, closed)...)
	}

	out = append(out,
		b.event(now, lark.COTEventReasoningMessageEnd, map[string]string{"messageId": b.messageID()}),
		b.event(now, lark.COTEventReasoningEnd, map[string]string{"messageId": b.messageID()}),
	)

	if snapshot.Outcome == "error" {
		out = append(out, b.event(now, lark.COTEventRunError, map[string]string{
			"message": nonEmpty(snapshot.ErrorMessage, "执行失败"),
			"code":    "run_error",
		}))
		return out
	}
	out = append(out, b.event(now, lark.COTEventRunFinished, map[string]string{
		"runId": b.runID, "threadId": b.threadID, "status": "success",
	}))
	return out
}

// toolEnd is the two events that finish a tool.
func (b *cotEventBuilder) toolEnd(now time.Time, tool ProgressToolCall) []lark.COTEvent {
	return []lark.COTEvent{
		b.event(now, lark.COTEventToolCallEnd, map[string]string{"toolCallId": tool.ID}),
		b.event(now, lark.COTEventToolCallResult, map[string]string{
			"messageId":  b.messageID(),
			"toolCallId": tool.ID,
			"content":    nonEmpty(truncateForCOT(tool.Result), "（无输出）"),
			"role":       "tool",
		}),
	}
}

func (b *cotEventBuilder) event(now time.Time, eventType string, fields map[string]string) lark.COTEvent {
	raw, _ := json.Marshal(fields)
	return lark.COTEvent{
		EventType: eventType,
		Content:   string(raw),
		Timestamp: b.clock.next(now),
	}
}

// messageID is the identifier of "the assistant's reasoning message" in this chain of thought, reused throughout.
func (b *cotEventBuilder) messageID() string { return "msg-" + b.runID }

// chunkText cuts long text into pieces, each no more than limit bytes.
//
// A single event over the content limit makes **the whole batch rejected**, so better a few more events.
func chunkText(s string, limit int) []string {
	if limit <= 0 || len(s) <= limit {
		return []string{s}
	}
	var out []string
	runes := []rune(s)
	var cur strings.Builder
	for _, r := range runes {
		if cur.Len()+len(string(r)) > limit {
			out = append(out, cur.String())
			cur.Reset()
		}
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// batchCOTEvents splits events into batches no larger than the platform limit.
func batchCOTEvents(events []lark.COTEvent) [][]lark.COTEvent {
	if len(events) == 0 {
		return nil
	}
	var out [][]lark.COTEvent
	for start := 0; start < len(events); start += lark.COTMaxEvents {
		end := start + lark.COTMaxEvents
		if end > len(events) {
			end = len(events)
		}
		out = append(out, events[start:end])
	}
	return out
}

func nonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
