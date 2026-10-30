// Unit tests for SSE frame translation: Ark's frame shape -> the shape the frontend parseFrame
// understands.
//
// # Why these were moved here from core/chat_test.go
//
// They test clientFrames / sseData, and those two functions belong to the **HTTP layer**
// (translating Ark's raw frames into frontend frames, splitting lines by SSE rules). Step 15 of
// the package split moved them into this package together with the handler. Tests follow the code
// they test.
//
// ⚠️ **This is not "moved because it would not compile"**: none of the chat business cases on the
// core side (creating sessions, fetching missed events, stream-drop recovery) were touched; they
// are still in core/chat_test.go. The test is "does this case assert a protocol conversion or a
// business invariant"; all four here are the former.
//
// The two **end-to-end** cases in core/chat_test.go (TestStreamEndpointEmitsSSE /
// TestStreamEndpointRejectsMissingRunWith404) **deliberately stay in core**: they need the
// core-internal test fixtures fakeRunner and newChatService, and moving them would mean moving
// the whole fixture set. They still run (see core/chat_test.go for how core builds the handler).

package apihandler

import (
	"encoding/json"
	"strings"
	"testing"
)

// sseDataPayload strips the payload after data: out of one SSE frame.
func sseDataPayload(t *testing.T, frame []byte) []byte {
	t.Helper()
	line := strings.TrimSuffix(string(frame), "\n\n")
	payload, ok := strings.CutPrefix(line, "data: ")
	if !ok {
		t.Fatalf("不是一帧合法 SSE：%q", frame)
	}
	return []byte(payload)
}

// TestClientFramesTranslatesDeltas: delta frames must be translated into the kind shape the
// frontend understands.
//
// **This is the easiest place to get wrong in the whole chat path.** Ark tells frames apart by the
// top-level type, while the frontend parseFrame reads the top-level kind. If frames were passed
// through as-is, the frontend would hit the fallback branch and put every delta frame into the
// timeline as a full event: dozens of empty messages show up, and the real reply is not shown.
func TestClientFramesTranslatesDeltas(t *testing.T) {
	// event_start: id must be lifted from the nested event to the top level.
	frames := clientFrames([]byte(`{"event":{"id":"sevt-4","type":"agent.message"},"type":"event_start"}`))
	if len(frames) != 1 {
		t.Fatalf("event_start 应产出 1 帧，实际 %d", len(frames))
	}
	payload := sseDataPayload(t, frames[0])
	var start struct {
		Kind     string  `json:"kind"`
		ID       string  `json:"id"`
		Type     string  `json:"type"`
		ThreadID *string `json:"session_thread_id"`
	}
	if err := json.Unmarshal(payload, &start); err != nil {
		t.Fatalf("解 event_start 失败：%v（%s）", err, payload)
	}
	if start.Kind != "event_start" || start.ID != "sevt-4" || start.Type != "agent.message" {
		t.Errorf("event_start 翻译错了：%s", payload)
	}
	// session_thread_id is "nullable, not optional": the key must be present.
	if !strings.Contains(string(payload), `"session_thread_id"`) {
		t.Errorf("event_start 缺 session_thread_id 键：%s", payload)
	}

	// event_delta: text must be lifted from delta.content.text to the top-level text.
	frames = clientFrames([]byte(
		`{"type":"event_delta","event_id":"sevt-4","delta":{"type":"content_delta","index":0,"content":{"type":"text","text":"我是"}}}`))
	if len(frames) != 1 {
		t.Fatalf("event_delta 应产出 1 帧，实际 %d", len(frames))
	}
	payload = sseDataPayload(t, frames[0])
	var delta struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &delta); err != nil {
		t.Fatalf("解 event_delta 失败：%v（%s）", err, payload)
	}
	if delta.Kind != "event_delta" || delta.ID != "sevt-4" || delta.Text != "我是" {
		t.Errorf("event_delta 翻译错了：%s", payload)
	}
}

// TestClientFramesPassesFinalEventsByteExact: final events must be passed through byte for byte.
//
// The frontend draft logic is "replace the draft with the final event by id". If the SSE copy and
// the listRunEvents copy differ in bytes, they are treated as two messages and drawn twice.
// Marshaling again would reorder keys and escape < > &.
func TestClientFramesPassesFinalEventsByteExact(t *testing.T) {
	raw := []byte(`{"id":"sevt-4","type":"agent.message","content":[{"type":"text","text":"a < b && c > d"}],"session_thread_id":"sthr-1"}`)
	frames := clientFrames(raw)
	if len(frames) != 1 {
		t.Fatalf("最终事件应产出 1 帧，实际 %d", len(frames))
	}
	payload := sseDataPayload(t, frames[0])
	if string(payload) != string(raw) {
		t.Errorf("最终事件不是逐字节透传：\n原文 %s\n实际 %s", raw, payload)
	}
}

// TestSSEDataFraming: every line of a multi-line payload must carry the data: prefix.
//
// A bare newline is taken by the SSE parser as the end of a frame, so the second half becomes the
// start of the next frame.
func TestSSEDataFraming(t *testing.T) {
	got := string(sseData([]byte("{\"a\":1,\n\"b\":2}")))
	want := "data: {\"a\":1,\ndata: \"b\":2}\n\n"
	if got != want {
		t.Errorf("多行载荷分帧不对：\n实际 %q\n期望 %q", got, want)
	}
	// Single line is the most common shape; it must end with an empty line (the frame terminator).
	if got := string(sseData([]byte(`{"a":1}`))); got != "data: {\"a\":1}\n\n" {
		t.Errorf("单行分帧不对：%q", got)
	}
}
