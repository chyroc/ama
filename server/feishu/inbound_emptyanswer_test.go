// Three sources of false alarms for the fallback text "this turn produced no reply, something may have gone wrong during execution".
//
// # Why a real event sequence again
//
// For the same reason as inbound_realseq_test.go, and more thoroughly this time: each group of frames below was read frame by frame from **the run_events
// table in local PG** by (run_id, sequence) and then redacted, not made up.
// The reason they cannot be made up is concrete: all three shapes break the intuition of "what a turn's event stream looks like":
//
//  1. A turn can **end with tool_use**: after the tool result comes back the agent ends directly and says nothing more.
//     So the text answerBuffer cleared at that tool_use **is the final answer**,
//     and the old code dropped it, and the user got "something went wrong during execution".
//  2. `stop_reason` has **a fourth** value, `retries_exhausted` (model-side retries exhausted, most likely rate limiting).
//     It satisfies turnEnded but is not "finished answering", and the old code described a known cause as an unknown failure.
//  3. An interrupt has **two** forms: the `user.interrupt` event, and the final frame's own
//     `stop_reason=user_interrupt`. The old code only recognized the first.
//
// What the three shapes share: **not a single word in the logs** (the fallback branch only assigned a value), so searching for that text in production
// gave 0 hits, and 0 could mean it never happened or was never logged. That is why the cause could only be pinned down by digging through the raw event stream.
//
// Evidence (local store, 5 of 378 turns hit the fallback):
//
//	sesn-example-01  seq 3→13   ends with tool_use, the real answer at seq 6 was dropped
//	sesn-example-02  seq 3→25   seq 21 model_rate_limited_error → retries_exhausted
//	sesn-example-03  seq 3→60   retries_exhausted after six tool calls
//	sesn-example-04  seq 3→74   ends with tool_use
//	sesn-example-05  seq 3→17   ends with tool_use
//
// Frames are redacted: only types and stop_reason are kept, and bodies are replaced with equivalent text without identity info.

package feishu

import (
	"context"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/channel"
	"github.com/chyroc/ama/server/shared/gopkg/arkframe"
)

// The **content** of these two bodies does not matter; what matters is that neither is a substring of the other, otherwise the Contains assertions would
// satisfy each other and the test would be always true (this repo has hit that shape).
const (
	// answerBeforeTool is the reply the agent finished saying **before** calling a tool.
	// Taken from seq 6 of sesn-example-01 (the original was a question clarifying definitions).
	answerBeforeTool = "在开始分析之前，我需要先确认几个口径问题：时间范围、异常的判定标准。"
	// narrationOnly is pure transitional narration, which **should not** become the final answer.
	narrationOnly = "让我先去翻一下昨天的记录。"
)

// endTurnIdle builds a frame of a normal final state.
func endTurnIdle() []byte {
	return mustJSON(map[string]any{
		"type":        "session.status_idle",
		"stop_reason": map[string]any{"type": "end_turn"},
	})
}

// requiresActionIdle builds an intermediate idle frame of "waiting for a tool result" (**not** the end).
func requiresActionIdle() []byte {
	return mustJSON(map[string]any{
		"type":        "session.status_idle",
		"stop_reason": map[string]any{"type": "requires_action"},
	})
}

// toolTerminatedTurnFrames reconstructs a turn that "ends with tool_use".
//
// Matches seq 3→13 of sesn-example-01: the agent finished the reply first (seq 6),
// then called a tool right after (seq 7); after the tool result came back (seq 9) it **said nothing more** and went straight to end_turn.
//
// The old code cleared the seq 6 text at the seq 7 tool_use, so the final answer was empty,
// and the user got "something went wrong during execution", while the answer had been produced long before.
func toolTerminatedTurnFrames() [][]byte {
	return [][]byte{
		mustJSON(map[string]any{"type": "session.status_running"}),
		agentFrame(answerBeforeTool),
		toolUseFrame("call_ls", "bash"),
		requiresActionIdle(),
		mustJSON(map[string]any{"type": "user.tool_result"}),
		mustJSON(map[string]any{"type": "session.status_running"}),
		// There is **no** agent.message here: the agent ended after reading the tool result.
		endTurnIdle(),
	}
}

// retriesExhaustedTurnFrames reconstructs a turn "killed by rate limiting".
//
// Matches seq 3→25 of sesn-example-02: several model requests failed in a row,
// one session.error frame (model_rate_limited_error), then the final state retries_exhausted.
// Not a single character of text.
func retriesExhaustedTurnFrames() [][]byte {
	return [][]byte{
		mustJSON(map[string]any{"type": "session.status_running"}),
		mustJSON(map[string]any{
			"type":  "session.error",
			"error": map[string]any{"type": "model_rate_limited_error"},
		}),
		mustJSON(map[string]any{
			"type":        "session.status_idle",
			"stop_reason": map[string]any{"type": "retries_exhausted"},
		}),
	}
}

// stopReasonInterruptFrames reconstructs a turn where "the interrupt only shows in stop_reason".
//
// Ark has two forms for an interrupt; this group uses the one with **no user.interrupt event**, only a final frame carrying
// stop_reason=user_interrupt (drainAllowed on the core side uses exactly this test;
// see core/pendingmessage.go). The old code only recognized the user.interrupt event, so it missed this one,
// and a normal interrupt was described as "something went wrong during execution".
func stopReasonInterruptFrames() [][]byte {
	return [][]byte{
		mustJSON(map[string]any{"type": "session.status_running"}),
		mustJSON(map[string]any{
			"type":        "session.status_idle",
			"stop_reason": map[string]any{"type": "user_interrupt"},
		}),
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestToolTerminatedTurnKeepsItsAnswer: a turn ending with tool_use **must not lose its answer**.
//
// This is the worst of the three false alarm sources: the answer was already produced, and the user not only did not get it but was told execution failed,
// so they would chase a failure that does not exist, while the right content sits in the event stream.
func TestToolTerminatedTurnKeepsItsAnswer(t *testing.T) {
	conv := &fakeConversations{agentIDs: []string{"agent_1"}, frames: toolTerminatedTurnFrames()}
	in, sender, _, _, _ := trioFixture(t, conv)

	if err := in.Handle(context.Background(), inboundMsg("om_tool_end", "帮我看下昨天的异常")); err != nil {
		t.Fatalf("这一轮不该返回错误: %v", err)
	}

	sent := sender.sent()
	if len(sent) != 1 {
		t.Fatalf("应当恰好发一条回复，实际 %d 条", len(sent))
	}
	body := sent[0].text
	if !strings.Contains(body, answerBeforeTool) {
		t.Errorf("调工具之前产出的答复必须发出去，实际发的是 %q", body)
	}
	if strings.Contains(body, "执行出了问题") {
		t.Errorf("答案明明产出了，不该说执行出了问题：%q", body)
	}
}

// TestRetriesExhaustedSaysWhyAndWhatToDo: a turn killed by rate limiting must state the cause and the next step.
//
// The generic "something may have gone wrong during execution" is **misleading** here: the cause is known (model-side retries exhausted),
// and following that sentence users would suspect they asked wrong, or ask someone to chase a bug that does not exist.
// The right next step is simple: wait a bit and resend the same sentence.
func TestRetriesExhaustedSaysWhyAndWhatToDo(t *testing.T) {
	conv := &fakeConversations{agentIDs: []string{"agent_1"}, frames: retriesExhaustedTurnFrames()}
	in, sender, _, _, _ := trioFixture(t, conv)

	if err := in.Handle(context.Background(), inboundMsg("om_ratelimit", "帮我查一下")); err != nil {
		t.Fatalf("这一轮不该返回错误: %v", err)
	}

	sent := sender.sent()
	if len(sent) != 1 {
		t.Fatalf("应当恰好发一条回复，实际 %d 条", len(sent))
	}
	body := sent[0].text
	// It must state the cause: retries exhausted / rate limiting.
	if !strings.Contains(body, "重试次数已用尽") {
		t.Errorf("应说明这一轮是被模型侧重试耗尽打死的，实际 %q", body)
	}
	// It must give the next step, instead of leaving the user staring.
	if !strings.Contains(body, "再发一次") {
		t.Errorf("兜底文案必须给出下一步动作，实际 %q", body)
	}
}

// TestStopReasonInterruptSendsNothing: the second form of interrupt must not send a message either.
//
// Pairs with TestInterruptedTurnSendsNothing: that one uses the user.interrupt event,
// this one only has stop_reason=user_interrupt on the final frame. The two are two ways of writing the same thing,
// and missing either would report a normal interrupt as a failure.
func TestStopReasonInterruptSendsNothing(t *testing.T) {
	conv := &fakeConversations{agentIDs: []string{"agent_1"}, frames: stopReasonInterruptFrames()}
	in, sender, _, _, _ := trioFixture(t, conv)

	if err := in.Handle(context.Background(), inboundMsg("om_stop_intr", "1")); err != nil {
		t.Fatalf("这一轮不该返回错误: %v", err)
	}

	if got := sender.sent(); len(got) != 0 {
		var texts []string
		for _, c := range got {
			texts = append(texts, c.text)
		}
		t.Errorf("被中断的轮次不该发任何消息，实际发了 %d 条: %q", len(got), texts)
	}
}

// ---------------------------------------------------------------------------
// answerBuffer segmenting semantics
// ---------------------------------------------------------------------------

// TestAnswerBufferPrefersLatestSegment: when there is a new segment it **must not** fall back to the backup.
//
// This guards that the fallback does not go too far: in the normal case (the agent keeps talking after the tool result) the new segment must be sent,
// otherwise the narration before the tool call shows up for the user again, exactly the bug TestFinalReplyIsOnlyTheLastSegment
// was written to fix. Without this test, the "fallback" fix could degrade into "no segmenting at all".
func TestAnswerBufferPrefersLatestSegment(t *testing.T) {
	var a channel.AnswerBuffer
	a.Write(narrationOnly)
	a.Reset()
	a.Write(answerBeforeTool)

	if got := a.Text(); got != answerBeforeTool {
		t.Errorf("有新段时该用新段，实际 %q", got)
	}
	if strings.Contains(a.Text(), narrationOnly) {
		t.Errorf("过场旁白不该出现在最终答案里：%q", a.Text())
	}
}

// TestAnswerBufferFallsBackWhenNothingFollows: when the new segment is empty, fall back to the backup.
//
// This is the core of the "ends with tool_use" turn: after clearing there is no more content,
// so the cleared segment **is** the final answer. Better to send text that may be narration
// than to replace a reply already produced with "something went wrong during execution".
func TestAnswerBufferFallsBackWhenNothingFollows(t *testing.T) {
	var a channel.AnswerBuffer
	a.Write(answerBeforeTool)
	a.Reset()

	if got := a.Text(); got != answerBeforeTool {
		t.Errorf("新段为空时该回落到被清掉的那段，实际 %q", got)
	}
}

// TestAnswerBufferKeepsFirstBackupAcrossEmptySegments: two segment splits in a row must not flush the backup.
//
// Shape: the agent says something → calls a tool → after the tool result comes back it **says nothing** and calls another tool → ends.
// There is no text between the two tool_use calls; if the second reset() overwrote the backup unconditionally, the real reply would be
// flushed by an empty string, back to "the user gets something went wrong during execution".
func TestAnswerBufferKeepsFirstBackupAcrossEmptySegments(t *testing.T) {
	var a channel.AnswerBuffer
	a.Write(answerBeforeTool)
	a.Reset() // first tool_use
	a.Reset() // second tool_use, nothing said in between

	if got := a.Text(); got != answerBeforeTool {
		t.Errorf("连续划段不该冲掉已有备份，实际 %q", got)
	}
}

// TestStartsToolCallRecognizesAllVariants: all three kinds of tool calls must be recognized.
//
// Built-in tools are agent.tool_use, while custom tools and MCP tools each have their own type (both really appeared
// in the local store). Recognizing only the first means agents using those two kinds cannot split segments, and the narration before a tool call
// is sent glued to the reply: the same bug, revived with a different tool type.
func TestStartsToolCallRecognizesAllVariants(t *testing.T) {
	for _, tc := range []struct {
		typ  string
		want bool
	}{
		{"agent.tool_use", true},
		{"agent.custom_tool_use", true},
		{"agent.mcp_tool_use", true},
		{"agent.message", false},
		{"user.tool_result", false},
		{"agent.thinking", false},
	} {
		got := arkframe.StartsToolCall(mustJSON(map[string]any{"type": tc.typ}))
		if got != tc.want {
			t.Errorf("arkframe.StartsToolCall(%s) = %v，想要 %v", tc.typ, got, tc.want)
		}
	}
}

// TestTurnFailedUpstreamOnlyOnRetriesExhausted: only retries_exhausted counts as upstream giving up.
//
// No other final state counts: end_turn is a normal finish, requires_action is an intermediate state (waiting for a tool result),
// user_interrupt is the user redirecting. Counting any of them would make the "rate limited" message appear where it
// should not, and that kind of false alarm is worse than having no dedicated text: it points in the wrong direction.
func TestTurnFailedUpstreamOnlyOnRetriesExhausted(t *testing.T) {
	for _, tc := range []struct {
		stop string
		want bool
	}{
		{"retries_exhausted", true},
		{"end_turn", false},
		{"requires_action", false},
		{"user_interrupt", false},
	} {
		raw := mustJSON(map[string]any{
			"type":        "session.status_idle",
			"stop_reason": map[string]any{"type": tc.stop},
		})
		if got := arkframe.FailedUpstream(raw); got != tc.want {
			t.Errorf("arkframe.FailedUpstream(stop=%s) = %v，想要 %v", tc.stop, got, tc.want)
		}
	}
}
