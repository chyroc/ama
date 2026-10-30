// Tests for the two delivery paths and the driver. All offline.
//
// The most important one: **degrade only when "definitely rejected", not when "the result is uncertain".**
// The latter means the CoT may already have been created, and creating another card would leave two progress messages running in the chat.
// And that mistake gives no error; the chat just has one more message.

package feishu

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// fakeCOT is a fake implementation of COTMessenger.
type fakeCOT struct {
	mu sync.Mutex

	createErr error
	creates   int
	// events collects every event appended so far.
	events    []lark.COTEvent
	appendErr error
	// completed records the closing reason.
	completed []lark.COTCompleteReason
}

func (f *fakeCOT) CreateCOT(_ context.Context, _, _, _ string, _ lark.CreateCOTOption) (*lark.COTMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &lark.COTMessage{COTID: "cot_1", MessageID: "om_cot"}, nil
}

func (f *fakeCOT) AppendCOT(_ context.Context, _, _, _ string, events []lark.COTEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appendErr != nil {
		return f.appendErr
	}
	f.events = append(f.events, events...)
	return nil
}

func (f *fakeCOT) CompleteCOT(_ context.Context, _, _, _ string, reason lark.COTCompleteReason) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed = append(f.completed, reason)
	return nil
}

func (f *fakeCOT) snapshot() ([]lark.COTEvent, []lark.COTCompleteReason, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]lark.COTEvent(nil), f.events...),
		append([]lark.COTCompleteReason(nil), f.completed...), f.creates
}

// driverFixture sets up a driver with the delivery record pre-filled.
func driverFixture(t *testing.T, cot COTMessenger, sender MessageSender) (*progressDriver, ProgressStore) {
	t.Helper()
	store := NewMemProgressStore()
	ctx := context.Background()
	if _, err := store.EnsureProgressDelivery(ctx, &ProgressDelivery{
		RunID: "sesn-1", LarkAppID: "cli_1", ChatID: "oc_1",
		OriginMessageID: "om_src", Transport: TransportCOT,
	}); err != nil {
		t.Fatalf("建投递失败: %v", err)
	}
	token := "tok-a"
	if _, ok, err := store.ClaimProgressDelivery(ctx, "sesn-1", token, time.Hour); err != nil || !ok {
		t.Fatalf("认领失败: ok=%v err=%v", ok, err)
	}
	now := func() time.Time { return time.Now() }
	d := &progressDriver{
		store:     store,
		transport: newCOTTransport(cot, "t", "sesn-1", now, nil),
		token:     token,
		log:       slog.Default(),
		now:       now,
	}
	if sender != nil {
		d.fallback = newCardTransport(sender, "t", "sesn-1")
	}
	return d, store
}

func TestDriverPushesThroughCOT(t *testing.T) {
	cot := &fakeCOT{}
	driver, store := driverFixture(t, cot, nil)
	ctx := context.Background()

	if err := driver.push(ctx, "sesn-1", ProgressSnapshot{Thinking: "先看看"}); err != nil {
		t.Fatalf("推送失败: %v", err)
	}

	events, _, creates := cot.snapshot()
	if creates != 1 {
		t.Errorf("该建一条 CoT，实际 %d 次", creates)
	}
	if !hasType(events, lark.COTEventRunStarted) {
		t.Errorf("该发开场事件: %s", dumpEvents(events))
	}
	if !hasEvent(events, lark.COTEventReasoningMessageContent, "先看看") {
		t.Errorf("该发思考内容: %s", dumpEvents(events))
	}

	// The created ID must be stored right away; otherwise a crash loses it forever.
	d, err := store.GetProgressDelivery(ctx, "sesn-1")
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if d.RemoteCOTID != "cot_1" || d.RemoteMessageID != "om_cot" {
		t.Errorf("远端标识该落库: %+v", d)
	}
	if d.Status != ProgressActive {
		t.Errorf("该转 active，得到 %q", d.Status)
	}
}

// If the snapshot has not changed it should not be pushed; without the comparison every agent frame becomes a remote call.
func TestDriverSkipsUnchangedSnapshot(t *testing.T) {
	cot := &fakeCOT{}
	driver, _ := driverFixture(t, cot, nil)
	ctx := context.Background()

	snap := ProgressSnapshot{Thinking: "想想"}
	if err := driver.push(ctx, "sesn-1", snap); err != nil {
		t.Fatalf("第一次推送失败: %v", err)
	}
	before, _, _ := cot.snapshot()

	if err := driver.push(ctx, "sesn-1", snap); err != nil {
		t.Fatalf("第二次推送失败: %v", err)
	}
	after, _, _ := cot.snapshot()

	if len(after) != len(before) {
		t.Errorf("内容没变不该再推，事件从 %d 条变成 %d 条", len(before), len(after))
	}
}

func TestDriverFinishCompletesCOT(t *testing.T) {
	cot := &fakeCOT{}
	driver, store := driverFixture(t, cot, nil)
	ctx := context.Background()

	driver.push(ctx, "sesn-1", ProgressSnapshot{
		Thinking: "查一下",
		Tools:    []ProgressToolCall{{ID: "t1", Name: "bash", Args: "ls"}}, // deliberately not finished
	})
	if err := driver.finish(ctx, "sesn-1", ProgressSnapshot{
		Thinking: "查完了",
		Tools:    []ProgressToolCall{{ID: "t1", Name: "bash", Args: "ls"}},
		Terminal: true, Outcome: "done",
	}); err != nil {
		t.Fatalf("封口失败: %v", err)
	}

	events, reasons, _ := cot.snapshot()
	// Unclosed tools must be closed, or the client keeps showing them as spinning.
	if !hasEvent(events, lark.COTEventToolCallEnd, "t1") {
		t.Errorf("终止该补 TOOL_CALL_END: %s", dumpEvents(events))
	}
	if !hasType(events, lark.COTEventRunFinished) {
		t.Errorf("该发 RUN_FINISHED: %s", dumpEvents(events))
	}
	if len(reasons) != 1 || reasons[0] != lark.COTCompleteDone {
		t.Errorf("该以 done 封口，得到 %v", reasons)
	}

	d, _ := store.GetProgressDelivery(ctx, "sesn-1")
	if !d.TerminalApplied || d.Status != ProgressCompleted {
		t.Errorf("该进 completed 且标记终态已推: %+v", d)
	}
	// The ordering gate must let it through.
	if !d.ReadyForFinalReply() {
		t.Error("封口后该让最终回复放行")
	}
}

func TestDriverFinishUsesErrorReason(t *testing.T) {
	cot := &fakeCOT{}
	driver, _ := driverFixture(t, cot, nil)
	ctx := context.Background()

	driver.push(ctx, "sesn-1", ProgressSnapshot{Thinking: "试试"})
	driver.finish(ctx, "sesn-1", ProgressSnapshot{
		Terminal: true, Outcome: "error", ErrorMessage: "模型挂了",
	})

	events, reasons, _ := cot.snapshot()
	if len(reasons) != 1 || reasons[0] != lark.COTCompleteError {
		t.Errorf("出错该以 error 封口，得到 %v", reasons)
	}
	// **Must not use timeout**: the client only renders the inner state on done.
	for _, r := range reasons {
		if r == lark.COTCompleteTimeout {
			t.Error("不该用 timeout 封口——会把被取消的 run 显示成超时")
		}
	}
	if !hasType(events, lark.COTEventRunError) {
		t.Errorf("该发 RUN_ERROR: %s", dumpEvents(events))
	}
}

// **The most important one in this file**: only degrade when definitely rejected.
func TestDriverFallsBackOnConfirmedRejection(t *testing.T) {
	cot := &fakeCOT{createErr: &lark.APIError{HTTPStatusCode: 403, Code: "99991672", Message: "no permission"}}
	sender := &fakeSender{}
	driver, store := driverFixture(t, cot, sender)
	ctx := context.Background()

	if err := driver.push(ctx, "sesn-1", ProgressSnapshot{Thinking: "想想"}); err != nil {
		t.Fatalf("该降级成功而不是报错: %v", err)
	}

	if len(sender.sent()) == 0 {
		t.Fatal("CoT 建不出来该降级发卡片")
	}
	d, _ := store.GetProgressDelivery(ctx, "sesn-1")
	if d.Transport != TransportCard {
		t.Errorf("该记下已降级，得到 %q", d.Transport)
	}
	if d.RemoteMessageID == "" {
		t.Error("降级后的卡片 ID 该落库")
	}
}

// An uncertain result **does not degrade**: the CoT may already have been created, and another card would mean two in the chat.
func TestDriverDoesNotFallBackOnUncertainOutcome(t *testing.T) {
	cot := &fakeCOT{createErr: &lark.APIError{HTTPStatusCode: 503, Code: "1", Message: "unavailable"}}
	sender := &fakeSender{}
	driver, store := driverFixture(t, cot, sender)
	ctx := context.Background()

	// Using the meaning of classifyCOTError: a 503 is wrapped by shared/infra/lark as "result uncertain".
	// Here we build that error directly to check the driver's branch.
	cot.createErr = errFromUnknown()

	if err := driver.push(ctx, "sesn-1", ProgressSnapshot{Thinking: "想想"}); err != nil {
		t.Fatalf("该安静记状态而不是报错: %v", err)
	}

	if len(sender.sent()) != 0 {
		t.Error("结果不确定时不该降级发卡片——那条 CoT 可能已经建出来了，会变成两条")
	}
	d, _ := store.GetProgressDelivery(ctx, "sesn-1")
	if d.Status != ProgressCreateUncertain {
		t.Errorf("该进 create_uncertain，得到 %q", d.Status)
	}
	if d.CreateAttemptedAt == nil {
		t.Error("该记下尝试时刻")
	}
	// And the ordering gate must let it through, or the final reply would wait for a CoT that never gets closed.
	if !d.ReadyForFinalReply() {
		t.Error("终态该让最终回复放行")
	}
}

func errFromUnknown() error {
	return errors.Join(lark.ErrCOTOutcomeUnknown, errors.New("网关超时"))
}

// Give up after reaching the consecutive failure limit; do not retry forever.
func TestDriverGivesUpAfterMaxFailures(t *testing.T) {
	cot := &fakeCOT{}
	driver, store := driverFixture(t, cot, nil)
	ctx := context.Background()

	// Create it first, then make appends keep failing.
	driver.push(ctx, "sesn-1", ProgressSnapshot{Thinking: "第一段"})
	cot.mu.Lock()
	cot.appendErr = &lark.APIError{HTTPStatusCode: 400, Code: "10001", Message: "bad"}
	cot.mu.Unlock()

	// The loop needs one extra round: the first push succeeds (no error injected yet),
	// and the failure count only starts growing from the next round.
	for i := 0; i < ProgressMaxFailures+1; i++ {
		snap := ProgressSnapshot{Thinking: strings.Repeat("x", i+1)}
		if err := driver.push(ctx, "sesn-1", snap); err != nil {
			t.Fatalf("第 %d 次推送不该返回错误（失败要记进状态）: %v", i, err)
		}
		// Clear the backoff so the next attempt can go on (this tests the failure count, not the backoff).
		d, _ := store.GetProgressDelivery(ctx, "sesn-1")
		if d.Status == ProgressFailed {
			break
		}
		d.NextRetryAt = nil
		_ = store.SaveProgressDelivery(ctx, d, "tok-a")
	}

	d, _ := store.GetProgressDelivery(ctx, "sesn-1")
	if d.Status != ProgressFailed {
		t.Errorf("连续失败 %d 次该放弃，实际状态 %q（失败计数 %d）",
			ProgressMaxFailures, d.Status, d.ConsecutiveFailureCount)
	}
	if !d.ReadyForFinalReply() {
		t.Error("放弃后该让最终回复放行，不然用户永远等不到答案")
	}
}

// When the lease is lost it must stop right away, and not keep pouring events into the same CoT.
func TestDriverStopsWhenLeaseLost(t *testing.T) {
	cot := &fakeCOT{}
	driver, store := driverFixture(t, cot, nil)
	ctx := context.Background()
	driver.push(ctx, "sesn-1", ProgressSnapshot{Thinking: "第一段"})

	// Simulate being taken over by another instance: swap the lease directly.
	d, _ := store.GetProgressDelivery(ctx, "sesn-1")
	_ = store.ReleaseProgressLease(ctx, "sesn-1", "tok-a")
	if _, ok, _ := store.ClaimProgressDelivery(ctx, "sesn-1", "tok-other", time.Hour); !ok {
		t.Fatal("接管失败")
	}
	_ = d

	err := driver.push(ctx, "sesn-1", ProgressSnapshot{Thinking: "第二段"})
	if !errors.Is(err, ErrProgressLeaseLost) {
		t.Fatalf("租约丢了该返回 ErrProgressLeaseLost，得到 %v", err)
	}
}

// Closing when nothing was ever created remotely: just finish, do not force-create an empty chain of thought just to close it.
func TestDriverFinishWithoutRemoteJustCloses(t *testing.T) {
	cot := &fakeCOT{createErr: errFromUnknown()}
	driver, store := driverFixture(t, cot, nil)
	ctx := context.Background()
	_ = driver.push(ctx, "sesn-1", ProgressSnapshot{Thinking: "想想"})

	// The previous step already went into create_uncertain, so finish should return quietly.
	if err := driver.finish(ctx, "sesn-1", ProgressSnapshot{Terminal: true, Outcome: "done"}); err != nil {
		t.Fatalf("该安静收工: %v", err)
	}
	_, reasons, creates := cot.snapshot()
	if len(reasons) != 0 {
		t.Errorf("没建出来就不该封口，得到 %v", reasons)
	}
	if creates != 1 {
		t.Errorf("不该为了封口再建一条，实际建了 %d 次", creates)
	}
	d, _ := store.GetProgressDelivery(ctx, "sesn-1")
	if !d.Finished() {
		t.Error("该处于终态")
	}
}

// ---------------------------------------------------------------------------
// Rendering the fallback card
// ---------------------------------------------------------------------------

func TestRenderProgressCardShowsState(t *testing.T) {
	running := renderProgressCard(ProgressSnapshot{
		Tools: []ProgressToolCall{
			{ID: "t1", Name: "bash", Done: true},
			{ID: "t2", Name: "read"},
		},
	})
	if !strings.Contains(running, "思考中") {
		t.Errorf("跑着的时候该显示思考中: %q", running)
	}
	if !strings.Contains(running, "bash") || !strings.Contains(running, "完成") {
		t.Errorf("该列出已完成的工具: %q", running)
	}
	if !strings.Contains(running, "read") || !strings.Contains(running, "执行中") {
		t.Errorf("该列出执行中的工具: %q", running)
	}

	done := renderProgressCard(ProgressSnapshot{Terminal: true, Outcome: "done"})
	if !strings.Contains(done, "已完成") {
		t.Errorf("终态该显示已完成: %q", done)
	}

	failed := renderProgressCard(ProgressSnapshot{
		Terminal: true, Outcome: "error", ErrorMessage: "炸了",
	})
	if !strings.Contains(failed, "失败") || !strings.Contains(failed, "炸了") {
		t.Errorf("出错该显示原因: %q", failed)
	}
}

func TestShouldFallbackToCard(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"确认被拒（4xx）": {&lark.APIError{HTTPStatusCode: 403, Code: "1"}, true},
		"结果不确定":     {errFromUnknown(), false},
		"nil":       {nil, false},
		"普通错误":      {errors.New("随便什么"), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := shouldFallbackToCard(tc.err); got != tc.want {
				t.Errorf("期望 %v，得到 %v", tc.want, got)
			}
		})
	}
}
