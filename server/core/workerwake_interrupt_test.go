package core

import (
	"context"
	"encoding/json"
	"testing"
)

// TestWakeSkipsPureInterrupt: a pure interrupt must not wake the poll loop.
//
// # Why this is a real bug and not just tidiness
//
// An interrupt **stops work**; it does not produce work (the line in chat.go: "an interrupt produces no new work, it stops work").
// Waking for it makes the pod holding the lease fire a useless poll, and the whole point of this change is to cut idle spinning.
//
// ⚠️ More importantly, `RedirectRun` **calls AppendRunEvents twice in a row**
// (first user.interrupt, then user.message). Without skipping interrupts, **one redirect wakes twice**,
// and the first wake is always idle.
//
// The check reuses the existing isInterruptOnly: AppendRunEvents already branches on it twice,
// so reuse the same check here instead of making a second one (two checks drift apart sooner or later).
func TestWakeSkipsPureInterrupt(t *testing.T) {
	ctx := context.Background()
	svc, _, runner := newChatService(t)
	runner.frames = agentReplyFrames("好的")

	wakes := newFakeWakeups()
	svc.workerWakeups = wakes

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}

	// First send a real message to confirm waking works at all (positive control).
	msg := json.RawMessage(`{"type":"user.message","content":[{"type":"text","text":"你好"}]}`)
	if _, err := svc.AppendRunEvents(ctx, run.ID, []json.RawMessage{msg}); err != nil {
		t.Fatalf("发消息失败: %v", err)
	}
	afterMsg := wakes.total()
	if afterMsg == 0 {
		t.Fatal("发真消息没有触发唤醒——用例没驱动起来（唤醒整个没接上）")
	}

	// Then send a pure interrupt: it **must not** cause a new wake.
	interrupt := json.RawMessage(`{"type":"user.interrupt"}`)
	if _, err := svc.AppendRunEvents(ctx, run.ID, []json.RawMessage{interrupt}); err != nil {
		t.Fatalf("发中断失败: %v", err)
	}

	if got := wakes.total(); got != afterMsg {
		t.Errorf("纯中断触发了唤醒（%d → %d）——中断不产生 work，唤醒是白打一发 poll。\n"+
			"更要紧的是 RedirectRun 连调两次 AppendRunEvents（先中断再消息），\n"+
			"不跳过的话一次转向唤醒两回。\n"+
			"修复：core/chat.go 里 wakeEnvForRun 前加 !isInterruptOnly(raw) 判据",
			afterMsg, got)
	}
}
