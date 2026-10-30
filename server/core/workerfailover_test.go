// Tests for failover and end-to-end logging. Fully offline: a fake selfhosted.API, no real
// Ark/container/PG.
package core

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

// —— workFailover wrapper ——

func TestWorkFailoverPassesThroughBeforeAbandon(t *testing.T) {
	inner := &fakeWorkAPI{}
	log := testLogger()
	f := newWorkFailover(inner, "work_1", log)

	if f.isAbandoned() {
		t.Fatal("刚建出来不该是 abandoned")
	}
	if err := f.StopWork(context.Background(), selfhosted.StopWorkRequest{WorkID: "work_1"}); err != nil {
		t.Fatalf("撒手前 StopWork 应透传: %v", err)
	}
	_, stopped := inner.snapshot()
	if len(stopped) != 1 || stopped[0] != "work_1" {
		t.Fatalf("撒手前 StopWork 应到达底层，实际 stopped=%v", stopped)
	}
	if _, err := f.HeartbeatWork(context.Background(), selfhosted.HeartbeatWorkRequest{WorkID: "work_1"}); err != nil {
		t.Fatalf("撒手前 Heartbeat 应透传成功: %v", err)
	}
}

func TestWorkFailoverSuppressesStopAndHeartbeatAfterAbandon(t *testing.T) {
	inner := &fakeWorkAPI{}
	log := testLogger()
	f := newWorkFailover(inner, "work_1", log)

	f.abandon()
	if !f.isAbandoned() {
		t.Fatal("abandon() 后应置位")
	}

	// StopWork must be swallowed (return nil) and **must not reach the inner API**. Otherwise Ark
	// marks the work terminal and never reclaims and reassigns it, so the failover is wasted.
	if err := f.StopWork(context.Background(), selfhosted.StopWorkRequest{WorkID: "work_1"}); err != nil {
		t.Fatalf("撒手后 StopWork 应被吞掉返回 nil，实际 %v", err)
	}
	_, stopped := inner.snapshot()
	if len(stopped) != 0 {
		t.Fatalf("撒手后 StopWork 绝不能透传给方舟，实际 stopped=%v", stopped)
	}

	// Heartbeats must stop (return an error) so that Ark's reclaim timer starts running.
	if _, err := f.HeartbeatWork(context.Background(), selfhosted.HeartbeatWorkRequest{WorkID: "work_1"}); err == nil {
		t.Fatal("撒手后 Heartbeat 应回错、不再续租")
	}
}

// —— watchdog ——

func TestWatchdogFailsOverWhenOrphanNeverAnswered(t *testing.T) {
	api := &auditAPI{events: []selfhosted.Event{toolUseEvent("call_stuck", "read")}}
	p := &WorkerPool{}
	log := testLogger()

	workCtx, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()
	f := newWorkFailover(api, "work_stuck", log)
	item := workItem("work_stuck", "sesn-stuck")

	done := make(chan struct{})
	go func() {
		p.watchWorkToolProgressWith(workCtx, f, api, item, cancelWork, log,
			5*time.Millisecond, 30*time.Millisecond)
		close(done)
	}()

	// After the threshold it must let go and stop the work.
	deadline := time.After(2 * time.Second)
	for !f.isAbandoned() {
		select {
		case <-deadline:
			t.Fatal("orphan 持续没结果，看门狗应在阈值后 failover")
		case <-time.After(5 * time.Millisecond):
		}
	}
	select {
	case <-workCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("failover 应叫停这条 work（cancelWork）")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("看门狗触发后应退出")
	}
}

func TestWatchdogIdleWhenNoOrphan(t *testing.T) {
	// Both tool_use and tool_result are present -> no orphan -> never triggers.
	api := &auditAPI{events: []selfhosted.Event{
		toolUseEvent("call_ok", "bash"),
		toolResultEvent("call_ok"),
	}}
	runWatchdogAndAssertNoTrigger(t, api)
}

func TestWatchdogNoTriggerWhenOrphanGetsAnswered(t *testing.T) {
	// The first two fetches have an orphan (the tool just started), then the result comes back.
	// It is running normally and must not fail over.
	api := &flipOrphanAPI{answerAfterLists: 2}
	runWatchdogAndAssertNoTrigger(t, api)
}

// flipOrphanAPI returns only tool_use (an orphan) for the first answerAfterLists ListEvents calls,
// then adds tool_result (sent back). It simulates "the tool is a bit slow but produces a result
// within the threshold".
type flipOrphanAPI struct {
	fakeWorkAPI
	mu               sync.Mutex
	listCount        int
	answerAfterLists int
}

func (f *flipOrphanAPI) ListEvents(
	_ context.Context, _ selfhosted.ListEventsRequest,
) (*selfhosted.ListEventsResponse, error) {
	f.mu.Lock()
	n := f.listCount
	f.listCount++
	f.mu.Unlock()
	events := []selfhosted.Event{toolUseEvent("call_late", "bash")}
	if n >= f.answerAfterLists {
		events = append(events, toolResultEvent("call_late"))
	}
	return &selfhosted.ListEventsResponse{Events: events}, nil
}

func runWatchdogAndAssertNoTrigger(t *testing.T, api selfhosted.API) {
	t.Helper()
	p := &WorkerPool{}
	log := testLogger()
	workCtx, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()
	f := newWorkFailover(api, "work_ok", log)
	item := workItem("work_ok", "sesn-ok")

	go p.watchWorkToolProgressWith(workCtx, f, api, item, cancelWork, log,
		5*time.Millisecond, 30*time.Millisecond)

	// Run enough ticks (well past the threshold) and confirm it never lets go.
	time.Sleep(300 * time.Millisecond)
	if f.isAbandoned() {
		t.Fatal("工具结果在阈值内回来了，看门狗不该 failover")
	}
	if workCtx.Err() != nil {
		t.Fatal("正常在跑的 work 不该被看门狗叫停")
	}
}

// —— end-to-end redacted logging ——

func TestInstrumentedAPILogsSuccessAndFailureWithoutBody(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	inner := &instrumentedFake{}
	wrapped := newInstrumentedWorkerAPI(inner, log)

	// Success path: returns unchanged, no panic.
	if _, err := wrapped.ListEvents(context.Background(), selfhosted.ListEventsRequest{}); err != nil {
		t.Fatalf("ListEvents 成功路径: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, `"api":"ListEvents"`) {
		t.Fatalf("成功调用应记方法名，实际: %s", out)
	}

	// Failure path: passes the error through, and the log has the status code/method name but no
	// body content.
	inner.listErr = &selfhosted.APIError{StatusCode: 500, Message: "boom"}
	buf.Reset()
	if _, err := wrapped.ListEvents(context.Background(), selfhosted.ListEventsRequest{}); err == nil {
		t.Fatal("注入的错误应透传")
	}
	out := buf.String()
	if !strings.Contains(out, `"api":"ListEvents"`) {
		t.Fatalf("失败调用应记方法名，实际: %s", out)
	}
	if !strings.Contains(out, `"status":500`) {
		t.Fatalf("失败调用应记 HTTP 状态码 500，实际: %s", out)
	}

	// Redaction: never print body content such as tool names / commands into the log.
	if strings.Contains(out, "secret-command") {
		t.Fatalf("日志不该包含请求正文，实际: %s", out)
	}
}

type instrumentedFake struct {
	fakeWorkAPI
	listErr error
}

func (f *instrumentedFake) ListEvents(
	ctx context.Context, _ selfhosted.ListEventsRequest,
) (*selfhosted.ListEventsResponse, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	// Include a "body" event to verify it does not show up in the log.
	return &selfhosted.ListEventsResponse{
		Events: []selfhosted.Event{toolUseEvent("call_1", "secret-command")},
	}, nil
}
