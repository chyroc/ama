// Tests for Ark events → snapshot → AG-UI events. All offline.
//
// They pin four things; getting any of them wrong gives no error, the CoT just displays wrong:
//
//   - when tool_use_id is missing, pair by the event id, otherwise no tool matches its result and all show "running"
//   - when input is an object it must be serialized; treating it as a string renders [object Object]
//   - on termination, close unclosed tools, otherwise the client keeps showing them as spinning
//   - timestamps within a batch are strictly increasing, otherwise the platform rejects the whole batch

package feishu

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

func frame(v map[string]any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

func textFrame(evType, text string) []byte {
	return frame(map[string]any{
		"type":    evType,
		"content": []map[string]any{{"type": "text", "text": text}},
	})
}

func TestSnapshotBuilderAccumulatesThinking(t *testing.T) {
	b := newSnapshotBuilder()
	if !b.Consume(textFrame("agent.thinking", "先看看")) {
		t.Error("思考帧该让快照变化")
	}
	if !b.Consume(textFrame("agent.thinking", "日志")) {
		t.Error("第二帧也该让快照变化")
	}
	// agent.message is the final reply and **does not go into the CoT**: it is the content of another message.
	if b.Consume(textFrame("agent.message", "结论是……")) {
		t.Error("最终回复不该进思维链")
	}
	if got := b.Snapshot().Thinking; got != "先看看日志" {
		t.Errorf("思考该拼起来，得到 %q", got)
	}
	// Non-JSON frames ([DONE]) must be skipped quietly.
	if b.Consume([]byte("[DONE]")) {
		t.Error("[DONE] 不该让快照变化")
	}
}

// **tool_use_id is often not sent**; then the event's own id must be used for pairing.
// An implementation that only recognizes tool_use_id would make no tool match its result, and the CoT would be all "running".
func TestSnapshotBuilderPairsToolsWithoutToolUseID(t *testing.T) {
	b := newSnapshotBuilder()

	// The call event has no tool_use_id, only id.
	b.Consume(frame(map[string]any{
		"type": "agent.tool_use", "id": "call_abc", "name": "bash",
		"input": map[string]any{"command": "ls"},
	}))
	// The result event also only has id.
	b.Consume(frame(map[string]any{
		"type": "agent.tool_result", "id": "call_abc",
		"content": []map[string]any{{"type": "text", "text": "a.txt"}},
	}))

	snap := b.Snapshot()
	if len(snap.Tools) != 1 {
		t.Fatalf("该有一个工具调用，得到 %d 个", len(snap.Tools))
	}
	tool := snap.Tools[0]
	if !tool.Done {
		t.Error("结果回来了该标记完成——没配上的话 CoT 里会一直显示执行中")
	}
	if tool.Result != "a.txt" {
		t.Errorf("结果该配上，得到 %q", tool.Result)
	}
	// **input is an object in practice**, and must be serialized to JSON rather than fmt.Sprint.
	if !strings.Contains(tool.Args, `"command"`) || !strings.Contains(tool.Args, "ls") {
		t.Errorf("对象入参该序列化成 JSON，得到 %q", tool.Args)
	}
}

// When tool_use_id exists it takes priority.
func TestSnapshotBuilderPrefersToolUseID(t *testing.T) {
	b := newSnapshotBuilder()
	b.Consume(frame(map[string]any{
		"type": "agent.tool_use", "id": "evt_1", "tool_use_id": "call_x", "name": "read",
		"input": "a.txt",
	}))
	b.Consume(frame(map[string]any{
		"type": "agent.tool_result", "tool_use_id": "call_x",
		"content": []map[string]any{{"type": "text", "text": "内容"}},
	}))
	snap := b.Snapshot()
	if len(snap.Tools) != 1 || !snap.Tools[0].Done {
		t.Fatalf("该配对上: %+v", snap.Tools)
	}
	// String input is kept as-is, not wrapped in JSON quotes.
	if snap.Tools[0].Args != "a.txt" {
		t.Errorf("字符串入参该原样保留，得到 %q", snap.Tools[0].Args)
	}
}

func TestSnapshotBuilderIgnoresDuplicatesAndUnknownResults(t *testing.T) {
	b := newSnapshotBuilder()
	b.Consume(frame(map[string]any{"type": "agent.tool_use", "id": "c1", "name": "bash"}))
	if b.Consume(frame(map[string]any{"type": "agent.tool_use", "id": "c1", "name": "bash"})) {
		t.Error("同一个工具调用重复出现不该再变")
	}
	// A result that matches no call: ignore it, do not make up a tool out of nothing.
	if b.Consume(frame(map[string]any{"type": "agent.tool_result", "id": "unknown"})) {
		t.Error("配不上的结果该忽略")
	}
	if len(b.Snapshot().Tools) != 1 {
		t.Errorf("该只有一个工具，得到 %d", len(b.Snapshot().Tools))
	}
}

func TestSnapshotBuilderMarksTerminal(t *testing.T) {
	b := newSnapshotBuilder()
	if !b.Consume(frame(map[string]any{"type": "session.status_idle"})) {
		t.Error("idle 该标记终态")
	}
	snap := b.Snapshot()
	if !snap.Terminal || snap.Outcome != "done" {
		t.Errorf("该是 done 终态: %+v", snap)
	}
	// Once final it no longer changes.
	if b.Consume(frame(map[string]any{"type": "session.status_idle"})) {
		t.Error("重复终态不该再变")
	}

	errB := newSnapshotBuilder()
	errB.Consume(frame(map[string]any{
		"type": "session.error", "error": map[string]any{"message": "模型挂了"},
	}))
	snap = errB.Snapshot()
	if snap.Outcome != "error" || snap.ErrorMessage != "模型挂了" {
		t.Errorf("该是 error 终态并带上原因: %+v", snap)
	}
}

// ---------------------------------------------------------------------------
// Snapshot → AG-UI events
// ---------------------------------------------------------------------------

// Timestamps within a batch must be **strictly increasing**, not even equal: a batch is almost always generated within the same millisecond.
func TestCOTClockIsStrictlyIncreasing(t *testing.T) {
	b := newCOTEventBuilder("sesn-1")
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

	// Generate all with the same now, simulating "a batch of events in the same millisecond".
	events := b.Start(now)
	events = append(events, b.Diff(nil, ProgressSnapshot{
		Thinking: "想想",
		Tools:    []ProgressToolCall{{ID: "t1", Name: "bash", Args: "ls", Result: "ok", Done: true}},
	}, now)...)
	events = append(events, b.Terminal(ProgressSnapshot{Terminal: true, Outcome: "done"}, now)...)

	if len(events) < 5 {
		t.Fatalf("该生成多条事件，得到 %d", len(events))
	}
	var prev int64
	for i, ev := range events {
		ms, err := strconv.ParseInt(ev.Timestamp, 10, 64)
		if err != nil {
			t.Fatalf("第 %d 条时间戳不是整数: %q", i, ev.Timestamp)
		}
		if ms <= prev {
			t.Fatalf("第 %d 条时间戳 %d 没有严格大于前一条 %d", i, ms, prev)
		}
		prev = ms
	}
	// And the whole batch must pass the platform's validation.
	if err := lark.ValidateCOTEvents(events); err != nil {
		t.Fatalf("生成的事件该能过校验: %v", err)
	}
}

func TestCOTDiffEmitsOnlyIncrements(t *testing.T) {
	b := newCOTEventBuilder("sesn-1")
	now := time.Now()

	first := ProgressSnapshot{Thinking: "先看看"}
	events := b.Diff(nil, first, now)
	if !hasEvent(events, lark.COTEventReasoningMessageContent, "先看看") {
		t.Errorf("该发第一段思考: %s", dumpEvents(events))
	}

	// The second time only the new part is sent: resending everything would make the client show it twice.
	second := ProgressSnapshot{Thinking: "先看看日志"}
	events = b.Diff(&first, second, now)
	if hasEvent(events, lark.COTEventReasoningMessageContent, "先看看日志") {
		t.Errorf("不该全量重发，只该发增量: %s", dumpEvents(events))
	}
	if !hasEvent(events, lark.COTEventReasoningMessageContent, "日志") {
		t.Errorf("该发增量「日志」: %s", dumpEvents(events))
	}

	// No change in content means no events.
	if got := b.Diff(&second, second, now); len(got) != 0 {
		t.Errorf("内容没变不该有事件，得到 %s", dumpEvents(got))
	}
}

func TestCOTDiffEmitsToolLifecycle(t *testing.T) {
	b := newCOTEventBuilder("sesn-1")
	now := time.Now()

	running := ProgressSnapshot{Tools: []ProgressToolCall{{ID: "t1", Name: "bash", Args: "ls"}}}
	events := b.Diff(nil, running, now)
	if !hasType(events, lark.COTEventToolCallStart) || !hasType(events, lark.COTEventToolCallArgs) {
		t.Errorf("新工具该发 START + ARGS: %s", dumpEvents(events))
	}
	if hasType(events, lark.COTEventToolCallEnd) {
		t.Errorf("还没结束不该发 END: %s", dumpEvents(events))
	}

	done := ProgressSnapshot{Tools: []ProgressToolCall{
		{ID: "t1", Name: "bash", Args: "ls", Result: "a.txt", Done: true},
	}}
	events = b.Diff(&running, done, now)
	if !hasType(events, lark.COTEventToolCallEnd) || !hasType(events, lark.COTEventToolCallResult) {
		t.Errorf("结束该发 END + RESULT: %s", dumpEvents(events))
	}
	// Ones already finished should not be sent again.
	if got := b.Diff(&done, done, now); len(got) != 0 {
		t.Errorf("没变化不该再发: %s", dumpEvents(got))
	}
}

// **The most important one in this file**: on termination, close the unclosed tools.
//
// When the agent is interrupted Ark does not send a "tool finished" event, and the CoT client only knows events:
// a tool with only START and no END keeps showing as spinning, even when the whole chain of thought is closed.
func TestCOTTerminalClosesDanglingTools(t *testing.T) {
	b := newCOTEventBuilder("sesn-1")
	now := time.Now()

	snapshot := ProgressSnapshot{
		Thinking: "想想",
		Tools: []ProgressToolCall{
			{ID: "t1", Name: "bash", Args: "ls", Result: "ok", Done: true},
			{ID: "t2", Name: "read", Args: "a.txt"}, // not finished
			{ID: "t3", Name: "grep"},                // not finished either
		},
		Terminal: true,
		Outcome:  "done",
	}
	events := b.Terminal(snapshot, now)

	// Both unfinished ones must get END.
	for _, id := range []string{"t2", "t3"} {
		if !hasEvent(events, lark.COTEventToolCallEnd, id) {
			t.Errorf("没闭合的工具 %s 该补 END，否则客户端一直显示它在转: %s", id, dumpEvents(events))
		}
		if !hasEvent(events, lark.COTEventToolCallResult, id) {
			t.Errorf("没闭合的工具 %s 该补 RESULT: %s", id, dumpEvents(events))
		}
	}
	// Ones already finished should not be closed again.
	if strings.Count(dumpEvents(events), `"toolCallId":"t1"`) != 0 {
		t.Errorf("已结束的工具不该重复收尾: %s", dumpEvents(events))
	}
	// The reasoning segment must be finished, then RUN_FINISHED.
	for _, want := range []string{
		lark.COTEventReasoningMessageEnd, lark.COTEventReasoningEnd, lark.COTEventRunFinished,
	} {
		if !hasType(events, want) {
			t.Errorf("终止批该有 %s: %s", want, dumpEvents(events))
		}
	}
	if err := lark.ValidateCOTEvents(events); err != nil {
		t.Fatalf("终止批该能过校验: %v", err)
	}
}

func TestCOTTerminalEmitsRunErrorOnFailure(t *testing.T) {
	b := newCOTEventBuilder("sesn-1")
	events := b.Terminal(ProgressSnapshot{
		Terminal: true, Outcome: "error", ErrorMessage: "模型挂了",
	}, time.Now())

	if !hasType(events, lark.COTEventRunError) {
		t.Errorf("出错该发 RUN_ERROR: %s", dumpEvents(events))
	}
	if hasType(events, lark.COTEventRunFinished) {
		t.Errorf("出错就不该再发 RUN_FINISHED: %s", dumpEvents(events))
	}
	if !hasEvent(events, lark.COTEventRunError, "模型挂了") {
		t.Errorf("该带上错误原因: %s", dumpEvents(events))
	}
	if err := lark.ValidateCOTEvents(events); err != nil {
		t.Fatalf("该能过校验: %v", err)
	}
}

// Overly long content must be truncated: a single event over 4096 bytes makes **the whole batch** rejected.
func TestCOTTruncatesOversizedContent(t *testing.T) {
	huge := strings.Repeat("日志", 4000) // far over the limit, and multi-byte characters
	got := truncateForCOT(huge)
	if len(got) > lark.COTMaxContentBytes {
		t.Errorf("截断后仍超限: %d 字节", len(got))
	}
	if !strings.HasSuffix(got, "（已截断）") {
		t.Error("该标明被截断了")
	}
	// Truncation must not break multi-byte characters: broken content is invalid UTF-8, which the platform also rejects.
	if !utf8Valid(got) {
		t.Error("截断产生了非法 UTF-8")
	}

	// Generate events with it; the whole batch must pass validation.
	b := newCOTEventBuilder("sesn-1")
	events := b.Diff(nil, ProgressSnapshot{
		Tools: []ProgressToolCall{{ID: "t1", Name: "bash", Args: huge, Result: huge, Done: true}},
	}, time.Now())
	if err := lark.ValidateCOTEvents(events); err != nil {
		t.Fatalf("超长内容截断后该能过校验: %v", err)
	}
}

// Very long thinking must be split into several events, not one over the limit.
func TestCOTChunksLongThinking(t *testing.T) {
	long := strings.Repeat("推理", 3000)
	b := newCOTEventBuilder("sesn-1")
	events := b.Diff(nil, ProgressSnapshot{Thinking: long}, time.Now())
	if len(events) < 2 {
		t.Fatalf("超长思考该切成多条，得到 %d 条", len(events))
	}
	if err := lark.ValidateCOTEvents(events); err != nil {
		t.Fatalf("切分后该能过校验: %v", err)
	}
}

// More than 50 events in a batch must be split into batches.
func TestBatchCOTEventsRespectsLimit(t *testing.T) {
	events := make([]lark.COTEvent, lark.COTMaxEvents*2+3)
	for i := range events {
		events[i] = lark.COTEvent{
			EventType: lark.COTEventRunStarted,
			Content:   `{"runId":"r","threadId":"t"}`,
			Timestamp: strconv.Itoa(i + 1),
		}
	}
	batches := batchCOTEvents(events)
	if len(batches) != 3 {
		t.Fatalf("该拆成 3 批，得到 %d 批", len(batches))
	}
	total := 0
	for i, batch := range batches {
		if len(batch) > lark.COTMaxEvents {
			t.Errorf("第 %d 批有 %d 条，超过上限", i, len(batch))
		}
		total += len(batch)
	}
	if total != len(events) {
		t.Errorf("拆批丢了事件：原 %d 条，拆后 %d 条", len(events), total)
	}
	if batchCOTEvents(nil) != nil {
		t.Error("空输入该返回 nil")
	}
}

// **Under self_hosted tool results come as `user.tool_result`, not `agent.tool_result`.**
//
// Tools run in our own containers, and after running one the worker returns the result to Ark with user.tool_result.
// Recognizing only agent.* means in production every tool fails to match its result, and the Feishu CoT shows "(not finished)" for all of them,
// while the event stream itself is complete and nothing shows up in the logs. Incident sesn-example-09:
// 21 calls and all 21 results present, and 0 marked done in the snapshot.
func TestSnapshotBuilderPairsUserToolResult(t *testing.T) {
	b := newSnapshotBuilder()
	b.Consume(frame(map[string]any{
		"type": "agent.tool_use", "id": "call_abc", "name": "bash",
		"input": map[string]any{"command": "ls"},
	}))
	// The real shape: its own id is evt-*, the pairing key is in tool_use_id; content items **have no type field**.
	if !b.Consume(frame(map[string]any{
		"type": "user.tool_result", "id": "evt-123", "tool_use_id": "call_abc",
		"content": []map[string]any{{"text": "a.txt"}},
	})) {
		t.Fatal("user.tool_result 该让快照变化——不认它线上每个工具都配不上结果")
	}

	snap := b.Snapshot()
	if len(snap.Tools) != 1 {
		t.Fatalf("该有一个工具，得到 %d", len(snap.Tools))
	}
	if !snap.Tools[0].Done {
		t.Error("结果回来了该标记完成，否则 CoT 里显示「（未完成）」")
	}
	if snap.Tools[0].Result != "a.txt" {
		t.Errorf("结果该配上，得到 %q", snap.Tools[0].Result)
	}

	// And it should not be taken by the termination close-up as an unclosed tool and get a "(not finished)".
	events := newCOTEventBuilder("sesn-1").Terminal(snap, time.Now())
	if hasEvent(events, lark.COTEventToolCallResult, "未完成") {
		t.Errorf("已完成的工具不该被补成「未完成」: %s", dumpEvents(events))
	}
}

// ---------------------------------------------------------------------------
// Order: tool calls must be interleaved in the text, not piled at the bottom
// ---------------------------------------------------------------------------

// **The most important one in this group**: when a turn goes "think → call → think → call → think", events must come out in
// that order. The event order is the client's display order (timestamps increase in production order), so sending all the text
// first and then going through the tools gives users "a big block of thinking + a lump of tools at the bottom".
//
// The assertion is on **the whole sequence**, not "are certain events present": presence holds both before and after the fix,
// and cannot reveal a wrong order.
func TestCOTDiffInterleavesToolsWithText(t *testing.T) {
	b := newCOTEventBuilder("sesn-1")
	now := time.Now()

	// Think "check the logs first" → call bash → think "then read the file" → call read → think "done".
	snapshot := ProgressSnapshot{
		Thinking: "先看日志再读文件好了",
		Ordered:  true,
		Tools: []ProgressToolCall{
			{ID: "t1", Name: "bash", Args: "ls", AfterTextLen: len("先看日志")},
			{ID: "t2", Name: "read", Args: "a.txt", AfterTextLen: len("先看日志再读文件")},
		},
	}

	got := traceOrder(b.Diff(nil, snapshot, now))
	want := []string{
		"text:先看日志",
		"tool_start:t1",
		"tool_args:t1",
		"text:再读文件",
		"tool_start:t2",
		"tool_args:t2",
		"text:好了",
	}
	assertOrder(t, got, want)
}

// A tool's end events must also land in its own position, not be added all together at the end.
func TestCOTDiffInterleavesFinishedTools(t *testing.T) {
	b := newCOTEventBuilder("sesn-1")
	now := time.Now()

	snapshot := ProgressSnapshot{
		Thinking: "查一下然后再查",
		Ordered:  true,
		Tools: []ProgressToolCall{
			{ID: "t1", Name: "bash", Args: "ls", Result: "a.txt", Done: true, AfterTextLen: len("查一下")},
			{ID: "t2", Name: "grep", Args: "foo", Result: "hit", Done: true, AfterTextLen: len("查一下然后再查")},
		},
	}

	got := traceOrder(b.Diff(nil, snapshot, now))
	want := []string{
		"text:查一下",
		"tool_start:t1", "tool_args:t1", "tool_end:t1", "tool_result:t1",
		"text:然后再查",
		"tool_start:t2", "tool_args:t2", "tool_end:t2", "tool_result:t2",
	}
	assertOrder(t, got, want)
}

// Across several Diffs (in reality pushed once a second after throttling), both order and "send only increments" must hold:
// every byte is sent exactly once, no more, no less. With multiple segments this is the most likely to degrade into resending.
func TestCOTDiffKeepsIncrementsAcrossPushes(t *testing.T) {
	b := newCOTEventBuilder("sesn-1")
	now := time.Now()

	first := ProgressSnapshot{
		Thinking: "先看日志",
		Ordered:  true,
		Tools:    []ProgressToolCall{{ID: "t1", Name: "bash", Args: "ls", AfterTextLen: len("先看日志")}},
	}
	assertOrder(t, traceOrder(b.Diff(nil, first, now)),
		[]string{"text:先看日志", "tool_start:t1", "tool_args:t1"})

	// Second push: the tool finished, another bit of thinking, and a second tool started.
	second := ProgressSnapshot{
		Thinking: "先看日志再读文件",
		Ordered:  true,
		Tools: []ProgressToolCall{
			{ID: "t1", Name: "bash", Args: "ls", Result: "a.txt", Done: true, AfterTextLen: len("先看日志")},
			{ID: "t2", Name: "read", Args: "a.txt", AfterTextLen: len("先看日志再读文件")},
		},
	}
	got := traceOrder(b.Diff(&first, second, now))
	assertOrder(t, got, []string{
		"tool_end:t1", "tool_result:t1",
		"text:再读文件",
		"tool_start:t2", "tool_args:t2",
	})

	// The part already sent must never appear again: resending would make the client show the same passage twice.
	for _, entry := range got {
		if strings.HasPrefix(entry, "text:") && strings.Contains(entry, "先看日志") {
			t.Errorf("第二次推重发了已发过的文本: %v", got)
		}
	}

	// No change means no events.
	if evs := b.Diff(&second, second, now); len(evs) != 0 {
		t.Errorf("内容没变不该有事件，得到 %s", dumpEvents(evs))
	}
}

// Existing snapshots have no ordered flag (the field did not exist then), and must **keep the behavior from before the fix exactly**,
// instead of interleaving with their AfterTextLen (all 0): that would throw all tools to the very top of the message,
// messier than before the fix, and the victims would be exactly the turns running during a rolling deploy.
//
// This pins "0 is ambiguous": 0 means both "happened before all text" and "the field does not exist",
// and guessing from 0 is bound to be wrong half the time. So the writer declares ordered explicitly; see ProgressSnapshot.Ordered.
func TestCOTDiffLegacySnapshotWithoutOffsets(t *testing.T) {
	var legacy ProgressSnapshot
	if err := json.Unmarshal([]byte(
		`{"thinking":"想了很久","tools":[{"id":"t1","name":"bash","args":"ls"}]}`), &legacy); err != nil {
		t.Fatalf("存量快照该能解出来: %v", err)
	}
	if legacy.Ordered {
		t.Fatal("存量行没有 ordered 字段，该解出 false")
	}
	if legacy.Tools[0].AfterTextLen != 0 {
		t.Fatalf("存量行没有 afterTextLen，该是 0，得到 %d", legacy.Tools[0].AfterTextLen)
	}

	// The order before the fix: all thinking text first, then tools. **The key is that tools do not move to the front of the text.**
	b := newCOTEventBuilder("sesn-1")
	got := traceOrder(b.Diff(nil, legacy, time.Now()))
	assertOrder(t, got, []string{"text:想了很久", "tool_start:t1", "tool_args:t1"})
}

// After the snapshot is stored with AfterTextLen and read back, the order is still right.
// When crashing and starting over only the stored snapshot is at hand; if serialization lost this field, the order info was never stored.
func TestProgressSnapshotRoundTripsToolOffsets(t *testing.T) {
	original := ProgressSnapshot{
		Thinking: "先看日志再读文件",
		Ordered:  true,
		Tools: []ProgressToolCall{
			{ID: "t1", Name: "bash", Args: "ls", AfterTextLen: len("先看日志")},
			{ID: "t2", Name: "read", Args: "a.txt", AfterTextLen: len("先看日志再读文件")},
		},
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("该能编码: %v", err)
	}
	var restored ProgressSnapshot
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatalf("该能解码: %v", err)
	}
	if !restored.Equal(original) {
		t.Fatalf("往返之后该逐字段相同:\n原 %+v\n回 %+v", original, restored)
	}

	b := newCOTEventBuilder("sesn-1")
	assertOrder(t, traceOrder(b.Diff(nil, restored, time.Now())), []string{
		"text:先看日志", "tool_start:t1", "tool_args:t1",
		"text:再读文件", "tool_start:t2", "tool_args:t2",
	})
}

// AfterTextLen is recorded by snapshotBuilder while eating events, not filled in by the caller.
// This pins "upstream is ordered" itself: the Ark event stream arrives in real order, and the interleaving positions should be fixed at that time.
func TestSnapshotBuilderRecordsToolTextOffsets(t *testing.T) {
	b := newSnapshotBuilder()
	b.Consume(textFrame("agent.thinking", "先看日志"))
	b.Consume(frame(map[string]any{
		"type": "agent.tool_use", "tool_use_id": "t1", "name": "bash",
		"input": map[string]any{"cmd": "ls"},
	}))
	b.Consume(textFrame("agent.thinking", "再读文件"))
	b.Consume(frame(map[string]any{
		"type": "agent.tool_use", "tool_use_id": "t2", "name": "read",
		"input": map[string]any{"path": "a.txt"},
	}))

	snap := b.Snapshot()
	if len(snap.Tools) != 2 {
		t.Fatalf("该有两个工具，得到 %d", len(snap.Tools))
	}
	if got, want := snap.Tools[0].AfterTextLen, len("先看日志"); got != want {
		t.Errorf("第一个工具该记在 %d 字节处，得到 %d", want, got)
	}
	if got, want := snap.Tools[1].AfterTextLen, len("先看日志再读文件"); got != want {
		t.Errorf("第二个工具该记在 %d 字节处，得到 %d", want, got)
	}

	// End to end: a snapshot accumulated from a real event stream renders interleaved.
	assertOrder(t, traceOrder(newCOTEventBuilder("sesn-1").Diff(nil, snap, time.Now())), []string{
		"text:先看日志", "tool_start:t1", "tool_args:t1",
		"text:再读文件", "tool_start:t2", "tool_args:t2",
	})
}

// After interleaving the three hard platform constraints must still hold: 50 per batch, 4096 bytes per event, strictly increasing timestamps.
// Changing the order changes the production order of events, and timestamps increase exactly in production order.
func TestCOTInterleavedEventsStillSatisfyPlatformLimits(t *testing.T) {
	b := newCOTEventBuilder("sesn-1")
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

	// 20 tools interleaved between 20 long pieces of text, forcing multiple batches.
	var thinking strings.Builder
	var tools []ProgressToolCall
	for i := 0; i < 20; i++ {
		thinking.WriteString(strings.Repeat("思", 900))
		tools = append(tools, ProgressToolCall{
			ID: "t" + strconv.Itoa(i), Name: "bash",
			Args:   strings.Repeat("x", 5000), // over 4096, must be truncated
			Result: strings.Repeat("y", 5000), Done: true,
			AfterTextLen: thinking.Len(),
		})
	}
	snapshot := ProgressSnapshot{Thinking: thinking.String(), Tools: tools, Ordered: true}

	events := b.Start(now)
	events = append(events, b.Diff(nil, snapshot, now)...)
	events = append(events, b.Terminal(ProgressSnapshot{Terminal: true, Outcome: "done"}, now)...)

	// Timestamps strictly increasing (across the whole sequence, not just within a batch).
	var prev int64
	for i, ev := range events {
		ms, err := strconv.ParseInt(ev.Timestamp, 10, 64)
		if err != nil {
			t.Fatalf("第 %d 条时间戳不是整数: %q", i, ev.Timestamp)
		}
		if ms <= prev {
			t.Fatalf("第 %d 条时间戳 %d 没有严格大于前一条 %d", i, ms, prev)
		}
		prev = ms
	}
	// Every batch must pass platform validation (count, bytes, and increasing are all in it).
	batches := batchCOTEvents(events)
	if len(batches) < 2 {
		t.Fatalf("这么多事件该拆成多批，得到 %d 批", len(batches))
	}
	for i, batch := range batches {
		if err := lark.ValidateCOTEvents(batch); err != nil {
			t.Fatalf("第 %d 批该能过校验: %v", i, err)
		}
	}
}

// traceOrder compresses an event sequence into a comparable sequence of "type:key field" strings.
//
// Assert the whole order rather than "is a certain event present": presence holds both before and after the fix,
// and cannot reveal the "tools piled at the bottom" problem.
func traceOrder(events []lark.COTEvent) []string {
	var out []string
	for _, ev := range events {
		var payload struct {
			Delta      string `json:"delta"`
			ToolCallID string `json:"toolCallId"`
		}
		_ = json.Unmarshal([]byte(ev.Content), &payload)
		switch ev.EventType {
		case lark.COTEventReasoningMessageContent:
			out = append(out, "text:"+payload.Delta)
		case lark.COTEventToolCallStart:
			out = append(out, "tool_start:"+payload.ToolCallID)
		case lark.COTEventToolCallArgs:
			out = append(out, "tool_args:"+payload.ToolCallID)
		case lark.COTEventToolCallEnd:
			out = append(out, "tool_end:"+payload.ToolCallID)
		case lark.COTEventToolCallResult:
			out = append(out, "tool_result:"+payload.ToolCallID)
		}
	}
	return out
}

func assertOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("事件条数不对，期望 %d 条 %v，得到 %d 条 %v", len(want), want, len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条顺序不对，期望 %q，得到 %q\n完整序列: %v\n期望序列: %v",
				i, want[i], got[i], got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func hasType(events []lark.COTEvent, eventType string) bool {
	for _, ev := range events {
		if ev.EventType == eventType {
			return true
		}
	}
	return false
}

func hasEvent(events []lark.COTEvent, eventType, substring string) bool {
	for _, ev := range events {
		if ev.EventType == eventType && strings.Contains(ev.Content, substring) {
			return true
		}
	}
	return false
}

func dumpEvents(events []lark.COTEvent) string {
	var b strings.Builder
	for _, ev := range events {
		b.WriteString("\n  " + ev.EventType + " " + ev.Content)
	}
	return b.String()
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
