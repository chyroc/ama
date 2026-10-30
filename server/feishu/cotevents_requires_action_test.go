// Regression tests for "the chain of thought stays at thinking forever".
//
// # The incident
//
// Production `sesn-example-08` (Feishu direct chat, 2026-09-01 00:36→00:55): the turn on the Ark side
// **finished normally** (582 events, last frame `session.status_idle + stop_reason=end_turn`,
// preceded by a complete `agent.message`), yet the chain of thought the user saw in Feishu **stayed at "thinking" forever**.
//
// # Root cause: requires_action was treated as the end of the turn
//
// Every time the agent calls a tool, Ark sends a frame `session.status_idle +
// stop_reason=requires_action`, meaning "I stopped to wait for the tool result"; after the worker returns the result
// the turn continues. That turn had 61 tool calls and 58 such idle frames.
//
// And `snapshotBuilder.Consume` used to markTerminal on `session.status_idle` **unconditionally**,
// so the **first tool call** (which ran for a few seconds) marked the snapshot as final. There were three consequences, all with zero errors:
//
//	1. Closed too early        the CoT got RUN_FINISHED at the first tool call, and the next 18 minutes of
//	                           thinking and tool calls could not be appended (a closed CoT accepts no more appends)
//	2. Real final state lost   markTerminal has an idempotency gate `if b.terminal { return false }`,
//	                           so the real end_turn and a mid-turn session.error no longer changed anything
//	3. Error reported as success  when a turn really failed, Outcome was still "done"
//
// # Why production had not a single log line
//
// When this path fails, **it takes only success branches**: closing succeeds, `CompleteCOT` returns code:0, the answer is sent as usual,
// and `Handle` returns nil. The pod in question (holding the long-connection lease, alive throughout, writing
// 926 log lines in that period) had only the two start lines for this turn and none for finishing, because finishing does not log in the first place.
// Searching for alerts such as `事件流异常结束` / `思维链封口失败` during the investigation found zero hits over the whole period,
// which easily leads to the wrong conclusion that "the delivery goroutine never started". **The truth is it ran, and believed it succeeded.**
//
// The test is exactly the same as `turnEnded` (inbound.go), turnEnded in `core/scheduler.go`, and
// applyEventToRun in `core/chat.go`: **requires_action does not count as ended**.
// The four places must stay in sync; when changing one, remember the other three.

package feishu

import (
	"context"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// requiresActionIdleFrame builds a frame of "stopped to wait for a tool result".
//
// This is the main character of this file: it **differs only in stop_reason** from the "finished answering" frame, and in production
// this difference was exactly what was not recognized.
func requiresActionIdleFrame() []byte {
	return mustJSON(map[string]any{
		"type":        "session.status_idle",
		"stop_reason": map[string]any{"type": "requires_action"},
	})
}

// endTurnIdleFrame builds a frame of "this turn really finished answering".
func endTurnIdleFrame() []byte {
	return mustJSON(map[string]any{
		"type":        "session.status_idle",
		"stop_reason": map[string]any{"type": "end_turn"},
	})
}

// TestRequiresActionIsNotTerminal: an idle waiting for a tool result must not mark the snapshot as final.
//
// This is the root cause itself, as a minimal reproduction.
func TestRequiresActionIsNotTerminal(t *testing.T) {
	b := newSnapshotBuilder()
	b.Consume(agentThinkingFrame("我先看看日志"))

	if changed := b.Consume(requiresActionIdleFrame()); changed {
		t.Error("等工具结果的 idle 不该改变快照，它不是这一轮的结束")
	}
	if snap := b.Snapshot(); snap.Terminal {
		t.Fatal("requires_action 被当成了终态——线上那条思维链就是这样在第一次工具调用时被提前封口的")
	}

	// The real end must still be recognized, or "closed too early" turns into "never closed".
	if changed := b.Consume(endTurnIdleFrame()); !changed {
		t.Error("end_turn 必须被认成终态")
	}
	if snap := b.Snapshot(); !snap.Terminal || snap.Outcome != "done" {
		t.Errorf("end_turn 之后应是 done 终态，实际 Terminal=%v Outcome=%q", snap.Terminal, snap.Outcome)
	}
}

// TestErrorAfterRequiresActionStillCounts: an error mid-turn must still be able to rewrite the final state.
//
// The hidden consequence of an early markTerminal: markTerminal is idempotent (`if b.terminal` returns directly),
// so once requires_action marked it done first, **a real error later could never be written in**,
// and the user saw a "successful" chain of thought while the turn had actually failed.
func TestErrorAfterRequiresActionStillCounts(t *testing.T) {
	b := newSnapshotBuilder()
	b.Consume(toolUseFrame("call_1", "bash"))
	b.Consume(requiresActionIdleFrame())

	b.Consume(mustJSON(map[string]any{
		"type":    "session.error",
		"content": []map[string]any{{"type": "text", "text": "工具执行失败"}},
	}))

	snap := b.Snapshot()
	if snap.Outcome != "error" {
		t.Errorf("等工具期间出的错必须被认成 error，实际 Outcome=%q——这一轮会被谎报成成功", snap.Outcome)
	}
	if snap.ErrorMessage == "" {
		t.Error("出错了却没有错误说明，用户不知道发生了什么")
	}
}

// TestFailureAfterRequiresActionClosesAsError: for a turn that fails while waiting for tools,
// the chain of thought must close as error, not be falsely reported as success.
//
// This is the end-to-end one: before the fix, the first requires_action frame marked the snapshot as done and final,
// so a later real error **could not change it** (markTerminal is idempotent), and the reason CompleteCOT received
// was still done: the user saw a chain of thought that "completed successfully" while the turn had actually failed.
//
// Assert on the closing reason rather than the event count: the former is what users really see,
// while the latter would become brittle as rendering details change.
func TestFailureAfterRequiresActionClosesAsError(t *testing.T) {
	conv := &fakeConversations{agentIDs: []string{"agent_1"}}
	conv.frames = [][]byte{
		agentThinkingFrame("我先查一下"),
		toolUseFrame("call_1", "bash"),
		requiresActionIdleFrame(), // waiting for a tool result: this turn is not over yet
		mustJSON(map[string]any{"type": "session.status_running"}),
		mustJSON(map[string]any{
			"type":    "session.error",
			"content": []map[string]any{{"type": "text", "text": "工具执行失败"}},
		}),
	}

	in, _, _, cot, _ := trioFixture(t, conv)
	if err := in.Handle(context.Background(), inboundMsg("om_1", "帮我查个问题")); err != nil {
		t.Fatalf("处理入站消息失败: %v", err)
	}

	_, reasons, completes := cot.snapshot()
	if completes != 1 {
		t.Fatalf("思维链应恰好封口一次，实际 %d 次", completes)
	}
	if len(reasons) != 1 || reasons[0] != lark.COTCompleteError {
		t.Errorf("这一轮失败了，思维链必须以 error 封口，实际 %v——"+
			"被谎报成成功时用户不知道该重试", reasons)
	}
}

// TestCardFallbackDoesNotSayDoneMidRun: the fallback card must not show "done" while waiting for tools.
//
// This is the third symptom of entering the final state early, and the one **users see most directly**: tenants without the CoT ability
// use the fallback card, and the card text is decided directly by `snapshot.Terminal`. After requires_action marked the snapshot
// as final early, the agent had only just called its first tool, and the card in the chat already said "done".
//
// Like the ones above, it gives zero errors: the card update succeeds, the logs are clean, only the content lies.
func TestCardFallbackDoesNotSayDoneMidRun(t *testing.T) {
	b := newSnapshotBuilder()
	b.Consume(agentThinkingFrame("我先查一下日志"))
	b.Consume(toolUseFrame("call_1", "bash"))
	b.Consume(requiresActionIdleFrame()) // the agent stops to wait for a tool result

	card := renderProgressCard(b.Snapshot())
	if strings.Contains(card, "已完成") {
		t.Errorf("等工具结果期间卡片说「已完成」，可这一轮还在跑：%q", card)
	}
	if !strings.Contains(card, "思考中") {
		t.Errorf("等工具结果期间卡片应显示「思考中」，实际 %q", card)
	}
}
