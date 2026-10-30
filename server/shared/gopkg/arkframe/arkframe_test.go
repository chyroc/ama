package arkframe

import (
	"encoding/json"
	"testing"
)

func TestTurnEnded(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{`{"type":"session.status_idle","stop_reason":{"type":"end_turn"}}`, true},
		{`{"type":"session.status_idle","stop_reason":{"type":"user_interrupt"}}`, true},
		{`{"type":"session.status_idle","stop_reason":{"type":"retries_exhausted"}}`, true},
		// requires_action is idle in the middle of a turn: the agent continues after the tool result.
		{`{"type":"session.status_idle","stop_reason":{"type":"requires_action"}}`, false},
		{`{"type":"session.status_terminated"}`, true},
		{`{"type":"agent.message","content":[]}`, false},
		{`[DONE]`, false},
	}
	for _, c := range cases {
		if got := TurnEnded([]byte(c.raw)); got != c.want {
			t.Errorf("TurnEnded(%s) = %v, want %v", c.raw, got, c.want)
		}
	}
}

func TestAgentText(t *testing.T) {
	text, ok := AgentText([]byte(`{"type":"agent.message","content":[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}]}`))
	if !ok || text != "ab" {
		t.Fatalf("AgentText = %q, %v", text, ok)
	}
	if _, ok := AgentText([]byte(`{"type":"agent.thinking"}`)); ok {
		t.Fatal("only agent.message carries the answer")
	}
	if _, ok := AgentText([]byte(`[DONE]`)); ok {
		t.Fatal("[DONE] is not JSON and must be skipped")
	}
}

func TestStartsToolCallKnowsAllThreeKinds(t *testing.T) {
	for _, typ := range []string{"agent.tool_use", "agent.custom_tool_use", "agent.mcp_tool_use"} {
		if !StartsToolCall([]byte(`{"type":"` + typ + `"}`)) {
			t.Errorf("%s must count as a tool call", typ)
		}
	}
	if StartsToolCall([]byte(`{"type":"agent.tool_result"}`)) {
		t.Error("a tool result is not the start of a call")
	}
}

func TestInterruptedBothForms(t *testing.T) {
	if !Interrupted([]byte(`{"type":"user.interrupt"}`)) {
		t.Error("user.interrupt frame")
	}
	if !Interrupted([]byte(`{"type":"session.status_idle","stop_reason":{"type":"user_interrupt"}}`)) {
		t.Error("stop_reason=user_interrupt on the final frame")
	}
	if Interrupted([]byte(`{"type":"session.status_idle","stop_reason":{"type":"end_turn"}}`)) {
		t.Error("end_turn is not an interrupt")
	}
}

func TestFailedUpstream(t *testing.T) {
	if !FailedUpstream([]byte(`{"type":"session.status_idle","stop_reason":{"type":"retries_exhausted"}}`)) {
		t.Error("retries_exhausted")
	}
	if StopReason([]byte(`{"type":"x"}`)) != "" {
		t.Error("no stop_reason gives an empty string")
	}
}

func TestUserMessageKeepsUserTextFirst(t *testing.T) {
	raw, err := UserMessage("hello", "", "<harness:x>ctx</harness:x>")
	if err != nil {
		t.Fatal(err)
	}
	var ev struct {
		Type    string `json:"type"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Type != "user.message" || len(ev.Content) != 2 {
		t.Fatalf("unexpected event %s", raw)
	}
	if ev.Content[0].Text != "hello" {
		t.Fatalf("the user's own text must be the first block, got %q", ev.Content[0].Text)
	}
}
