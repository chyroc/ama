// Tests for pollLoop: concurrent claiming, and the timing trap of "StopWork while still running".
//
// These cases touch neither real Ark nor containers; only a fake selfhosted.API drives the
// scheduling logic. Container-side behavior is tested in shared/infra/dockertools.
package core

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chyroc/ama/server/shared/infra/dockertools"
	"github.com/volcengine/ark-runtime-go/arkruntime/lib/environments"
	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
	"github.com/volcengine/ark-runtime-go/arkruntime/tools/agenttoolset"
)

// fakeWorkAPI is a test double for selfhosted.API.
//
// It only implements the methods pollLoop really calls; the rest can return zero values, **but
// must not panic**. HandleItem calls GetSession internally, and a panic would be swallowed by
// the recover in the goroutine, showing up as "test mysteriously times out" instead of a
// failure.
type fakeWorkAPI struct {
	mu sync.Mutex

	// queue is the work to hand out, taken in order. Once empty, PollWork blocks until ctx ends
	// (simulating a long poll with no new work), so pollLoop does not spin and flood the log.
	queue []selfhosted.WorkItem

	// acked / stopped record when each work item was acked / stopped, to assert ordering.
	acked   []string
	stopped []string

	// getSessionGate is waited on inside GetSession, to hold work in the "being processed"
	// state. No hold when nil. HandleItem calls GetSession right away, so it is the most
	// reliable place to hold.
	getSessionGate chan struct{}

	// inFlight records how many work items are past GetSession and not finished at the same
	// time.
	inFlight    atomic.Int32
	maxInFlight atomic.Int32

	// pollErr makes the first n PollWork calls return errors, to verify the split between
	// backoff and fatal errors.
	pollErrs []error
	pollN    int

	// pollReqs records each PollWork's input, to assert whether reclaim was really passed.
	pollReqs []selfhosted.PollWorkRequest
}

func (f *fakeWorkAPI) PollWork(ctx context.Context, req selfhosted.PollWorkRequest) (*selfhosted.WorkItem, error) {
	f.mu.Lock()
	f.pollReqs = append(f.pollReqs, req)
	if f.pollN < len(f.pollErrs) {
		err := f.pollErrs[f.pollN]
		f.pollN++
		f.mu.Unlock()
		return nil, err
	}
	if len(f.queue) > 0 {
		item := f.queue[0]
		f.queue = f.queue[1:]
		f.mu.Unlock()
		return &item, nil
	}
	f.mu.Unlock()
	// Queue is empty: simulate a long poll hanging until the test ends.
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *fakeWorkAPI) AckWork(_ context.Context, req selfhosted.AckWorkRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acked = append(f.acked, req.WorkID)
	return nil
}

func (f *fakeWorkAPI) StopWork(_ context.Context, req selfhosted.StopWorkRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, req.WorkID)
	return nil
}

func (f *fakeWorkAPI) HeartbeatWork(_ context.Context, _ selfhosted.HeartbeatWorkRequest) (*selfhosted.HeartbeatResponse, error) {
	return &selfhosted.HeartbeatResponse{State: "running", TTLSeconds: 60}, nil
}

func (f *fakeWorkAPI) GetSession(ctx context.Context, req selfhosted.GetSessionRequest) (*selfhosted.Session, error) {
	cur := f.inFlight.Add(1)
	for {
		max := f.maxInFlight.Load()
		if cur <= max || f.maxInFlight.CompareAndSwap(max, cur) {
			break
		}
	}
	defer f.inFlight.Add(-1)

	if f.getSessionGate != nil {
		select {
		case <-f.getSessionGate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	// Return an error so HandleItem wraps up quickly: we are testing scheduling, not session
	// handling. Returning a nil session makes the SDK report "session response is empty" and run
	// its defer, which is exactly the "this work item is done" we want.
	return nil, errors.New("fake: 到此为止")
}

func (f *fakeWorkAPI) ListEvents(_ context.Context, _ selfhosted.ListEventsRequest) (*selfhosted.ListEventsResponse, error) {
	return &selfhosted.ListEventsResponse{}, nil
}

func (f *fakeWorkAPI) SendEvent(_ context.Context, _ selfhosted.SendEventRequest) error {
	return nil
}

func (f *fakeWorkAPI) OpenSkill(_ context.Context, _ selfhosted.OpenSkillRequest) (*selfhosted.SkillContent, error) {
	return nil, errors.New("fake: 不支持")
}

func (f *fakeWorkAPI) snapshot() (acked, stopped []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.acked...), append([]string(nil), f.stopped...)
}

// pollCount is how many times PollWork was called.
//
// **Must take the lock**: PollWork writes pollReqs in the poll goroutine under test, and the case
// reads it in its own goroutine; reading the field directly reports a data race under -race.
func (f *fakeWorkAPI) pollCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pollReqs)
}

// workItem builds a work item that SessionIDValue can recognize.
func workItem(id, sessionID string) selfhosted.WorkItem {
	return selfhosted.WorkItem{
		ID:            id,
		EnvironmentID: "env_test",
		Data:          selfhosted.WorkData{ID: sessionID, Type: "session"},
	}
}

// discardWriter swallows all writes; used as the output of the test logger.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestPollLoopNeverStopsWorkStillRunning is the reason this whole change exists.
//
// The first line of the SDK's WorkPoller.Next() is runPendingStop(), which sends StopWork for
// **the previous** work item. In a serial model that is right (the previous one has finished),
// but under concurrency the second Next marks the still-running first one as final. After that
// our postResult sends the result to a work item that has already ended; the agent gets stuck
// in requires_action, while both calls return success with zero errors.
//
// So this case holds the first work item in GetSession, then confirms that when the second one
// is claimed, **the first one has not been StopWork'ed**.
//
// ⚠️ Mutation check: this must go red if pollLoop is changed back to
// environments.NewWorkPoller.
func TestPollLoopNeverStopsWorkStillRunning(t *testing.T) {
	gate := make(chan struct{})
	api := &fakeWorkAPI{
		queue: []selfhosted.WorkItem{
			workItem("work_1", "sesn_1"),
			workItem("work_2", "sesn_2"),
		},
		getSessionGate: gate,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 4}, log: testLogger()}
	// pollLoop now needs **a factory that builds a worker per session** (ToolsFunc must know
	// which Ark session this item is; see dockertools.Provider.ToolsFuncForSession).
	// The tests do not care about the session, so ignore the input and give the same fake
	// worker.
	installFakeProcessor(pool)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = pool.pollLoop(ctx, api, "env_test", "worker_test", 4, testLogger())
	}()

	// Wait until both are acked: that means the second one was claimed while the first is still
	// held on the gate.
	waitFor(t, 2*time.Second, func() bool {
		acked, _ := api.snapshot()
		return len(acked) == 2
	}, "两条 work 都被 ack")

	// Key assertion: the first one is still running, so there must be no StopWork.
	_, stopped := api.snapshot()
	if len(stopped) != 0 {
		t.Fatalf("有 work 在仍然运行时被 StopWork：%v\n"+
			"这正是 WorkPoller.runPendingStop 的行为——并发下它会把在跑的 work 标记终态，"+
			"结果回传给一个已结束的 work，agent 卡在 requires_action 且零报错", stopped)
	}

	// Release, and let both finish normally.
	close(gate)
	cancel()
	<-done
}

// TestPollLoopRunsWorkConcurrently verifies that concurrency really happens.
//
// With a serial implementation maxInFlight is always 1. That was the behavior before this
// change, and the cause of "a coworker is chatting, and my message gets no response".
func TestPollLoopRunsWorkConcurrently(t *testing.T) {
	gate := make(chan struct{})
	api := &fakeWorkAPI{
		queue: []selfhosted.WorkItem{
			workItem("work_1", "sesn_1"),
			workItem("work_2", "sesn_2"),
			workItem("work_3", "sesn_3"),
		},
		getSessionGate: gate,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 3}, log: testLogger()}
	// pollLoop now needs **a factory that builds a worker per session** (ToolsFunc must know
	// which Ark session this item is; see dockertools.Provider.ToolsFuncForSession).
	// The tests do not care about the session, so ignore the input and give the same fake
	// worker.
	installFakeProcessor(pool)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = pool.pollLoop(ctx, api, "env_test", "worker_test", 3, testLogger())
	}()

	waitFor(t, 2*time.Second, func() bool {
		return api.maxInFlight.Load() >= 3
	}, "三条 work 同时在处理")

	close(gate)
	cancel()
	<-done

	if got := api.maxInFlight.Load(); got < 3 {
		t.Fatalf("同时在处理的 work 数 = %d，要 >= 3；串行实现下这个值恒为 1", got)
	}
}

// TestPollLoopRespectsConcurrencyLimit verifies that the limit holds.
//
// The limit is about quota: each running work item may hold its own execution container, and
// the concurrent Session quota is global (e.g. 20) and shared with the warm pool. Without a
// limit, one user spamming messages could use up the whole quota, and other workspaces would
// suffer.
func TestPollLoopRespectsConcurrencyLimit(t *testing.T) {
	gate := make(chan struct{})
	items := make([]selfhosted.WorkItem, 0, 6)
	for i := range 6 {
		items = append(items, workItem("work_"+string(rune('a'+i)), "sesn_"+string(rune('a'+i))))
	}
	api := &fakeWorkAPI{queue: items, getSessionGate: gate}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const limit = 2
	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: limit}, log: testLogger()}
	// pollLoop now needs **a factory that builds a worker per session** (ToolsFunc must know
	// which Ark session this item is; see dockertools.Provider.ToolsFuncForSession).
	// The tests do not care about the session, so ignore the input and give the same fake
	// worker.
	installFakeProcessor(pool)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = pool.pollLoop(ctx, api, "env_test", "worker_test", limit, testLogger())
	}()

	// Wait until it is full, then wait a bit more: if the limit did not work, this is enough
	// time for a third one to squeeze in.
	waitFor(t, 2*time.Second, func() bool {
		return api.inFlight.Load() >= limit
	}, "并发达到上限")
	time.Sleep(200 * time.Millisecond)

	if got := api.maxInFlight.Load(); got > limit {
		t.Fatalf("同时在处理 %d 条，超过上限 %d", got, limit)
	}
	// The ack count should not run far ahead either: a slot is taken before polling, so at most
	// a few more than the limit are in flight.
	acked, _ := api.snapshot()
	if len(acked) > limit {
		t.Fatalf("ack 了 %d 条但上限是 %d——说明 work 被领走后卡在等槽位上，"+
			"那时 lease 在手却不干活，会被别的 worker reclaim（白重试一轮）", len(acked), limit)
	}

	close(gate)
	cancel()
	<-done
}

// TestPollLoopDiscardsWorkWithoutSession verifies that a work item without a session id is
// settled.
//
// If not settled, it stays queued and is claimed again next round: an endless loop hammering
// the API and flooding the log.
func TestPollLoopDiscardsWorkWithoutSession(t *testing.T) {
	bad := selfhosted.WorkItem{ID: "work_bad", EnvironmentID: "env_test"} // no session id
	api := &fakeWorkAPI{queue: []selfhosted.WorkItem{bad}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 2}, log: testLogger()}
	// pollLoop now needs **a factory that builds a worker per session** (ToolsFunc must know
	// which Ark session this item is; see dockertools.Provider.ToolsFuncForSession).
	// The tests do not care about the session, so ignore the input and give the same fake
	// worker.
	installFakeProcessor(pool)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = pool.pollLoop(ctx, api, "env_test", "worker_test", 2, testLogger())
	}()

	waitFor(t, 2*time.Second, func() bool {
		_, stopped := api.snapshot()
		return len(stopped) == 1
	}, "无效 work 被结算")

	acked, stopped := api.snapshot()
	if len(acked) != 1 || acked[0] != "work_bad" {
		t.Fatalf("无效 work 要先 ack 才能结算（只 stop 不 ack 的话它还在 queued），得到 %v", acked)
	}
	if len(stopped) != 1 || stopped[0] != "work_bad" {
		t.Fatalf("无效 work 要被 StopWork，得到 %v", stopped)
	}

	cancel()
	<-done
}

// TestPollLoopReturnsOnFatalStatus verifies that config-type errors bubble up to reconnect,
// instead of looping in place forever.
//
// A 4xx (except 408/409/429) means the environment does not exist or the key lacks permission;
// retrying ten thousand times gives the same result. Let it return, and the runOne layer retries
// after 5 seconds and logs it, so a mistyped envID does not become an endless loop hammering the
// API.
func TestPollLoopReturnsOnFatalStatus(t *testing.T) {
	api := &fakeWorkAPI{
		pollErrs: []error{&selfhosted.APIError{StatusCode: 403, Message: "forbidden"}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 2}, log: testLogger()}
	installFakeProcessor(pool)
	err := pool.pollLoop(ctx, api, "env_test", "worker_test", 2, testLogger())
	if err == nil {
		t.Fatal("403 要作为错误返回让上层重连，不能原地退避重试")
	}
}

// TestArkFatalStatusClassification pins the error classification, which copies the SDK's
// isFatal4xx.
//
// 408/409/429 are transient and succeed after backoff; treating them as fatal would turn one
// rate limit into "the worker reconnects once". The other way round, treating 403 as transient
// means an endless loop hammering the API.
func TestArkFatalStatusClassification(t *testing.T) {
	cases := []struct {
		code  int
		fatal bool
	}{
		{400, true}, {401, true}, {403, true}, {404, true},
		{408, false}, // request timeout: retry
		{409, false}, // conflict: someone else got it, normal competition
		{429, false}, // rate limited: back off
		{500, false}, {502, false}, {503, false},
	}
	for _, c := range cases {
		err := &selfhosted.APIError{StatusCode: c.code}
		if got := arkFatalStatus(err); got != c.fatal {
			t.Errorf("状态码 %d：arkFatalStatus = %v，要 %v", c.code, got, c.fatal)
		}
	}
	// Anything that is not an APIError (network errors and such) is never fatal.
	if arkFatalStatus(errors.New("dial tcp: i/o timeout")) {
		t.Error("网络错误不该被当成致命的——那会让一次抖动触发 worker 重连")
	}
}

// TestArkBackoffCurve pins the backoff curve; the values match the SDK's backoff.
func TestArkBackoffCurve(t *testing.T) {
	if got := arkBackoff(0); got != time.Second {
		t.Errorf("arkBackoff(0) = %v，要 1s", got)
	}
	if got := arkBackoff(1); got != 2*time.Second {
		t.Errorf("arkBackoff(1) = %v，要 2s", got)
	}
	// There must be a cap: without one, consecutive failures would back off to hours, and the
	// worker would effectively be dead.
	if got := arkBackoff(100); got != arkBackoffCap {
		t.Errorf("arkBackoff(100) = %v，要封顶在 %v", got, arkBackoffCap)
	}
	for n := 1; n <= 20; n++ {
		if arkBackoff(n) > arkBackoffCap {
			t.Fatalf("arkBackoff(%d) 超过上限 %v", n, arkBackoffCap)
		}
	}
}

// TestArkWorkerIDIsUnique verifies that worker ids do not collide.
//
// A collision would make two workers reclaim each other's work: one turn handled twice, two
// containers, two tool_results.
func TestArkWorkerIDIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		id := arkWorkerID()
		if id == "" {
			t.Fatal("worker id 不能为空")
		}
		if seen[id] {
			t.Fatalf("worker id 重复：%s", id)
		}
		seen[id] = true
	}
}

// waitFor polls until a condition holds, and calls t.Fatal on timeout.
//
// Polling instead of a fixed sleep: in concurrency tests a fixed sleep is either slow or fails
// now and then (flaky), and a flaky test is worse than no test: it trains people to ignore red.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等「%s」超时（%v）", what, timeout)
}

// installFakeProcessor installs a test work processor on the pool: it behaves exactly like the
// old pollLoop calling fakeEnvWorker(...).HandleItem directly (the real SDK HandleItem, running
// heartbeat and SessionToolRunner; these cases all finish at the GetSession stage and never reach
// real tools).
//
// After the pollLoop refactor, "process one work item" goes through p.processWork (in
// production it is our own orchestration with a PG ledger). A fake is injected here so the
// existing poll/ack/concurrency/gate tests need not care about the tool set or the ledger.
func installFakeProcessor(pool *WorkerPool) {
	pool.processWork = func(ctx context.Context, item selfhosted.WorkItem, api selfhosted.API, _ string, _ *slog.Logger) error {
		return fakeEnvWorker(api).HandleItem(ctx, environments.HandleItemOptions{
			WorkID:            item.ID,
			EnvironmentID:     item.EnvironmentID,
			SessionID:         item.SessionIDValue(),
			LatestHeartbeatAt: item.LatestHeartbeatAt,
		})
	}
}

// installRecordingProcessor is like installFakeProcessor, but first sends the API handed down by
// pollLoop (wrapped with failover/sendGuard/rate limiting) into handed, so "wiring" tests can
// get it and assert on it. handed should be buffered; one send per work item.
func installRecordingProcessor(pool *WorkerPool, handed chan<- selfhosted.API) {
	pool.processWork = func(ctx context.Context, item selfhosted.WorkItem, api selfhosted.API, _ string, _ *slog.Logger) error {
		select {
		case handed <- api:
		default:
		}
		return fakeEnvWorker(api).HandleItem(ctx, environments.HandleItemOptions{
			WorkID:            item.ID,
			EnvironmentID:     item.EnvironmentID,
			SessionID:         item.SessionIDValue(),
			LatestHeartbeatAt: item.LatestHeartbeatAt,
		})
	}
}

// fakeEnvWorker builds a **real** EnvironmentWorker bound to the fake API.
//
// A real one instead of another double: pollLoop calls worker.HandleItem, which contains the
// heartbeat and StopWork timing logic, and a double would replace exactly what we want to verify.
// NewEnvironmentWorker takes the selfhosted.API interface directly, so this costs nothing.
//
// Workdir cannot point at a fixed path outside t.TempDir (HandleItem creates session
// directories inside it), so it goes to a temp dir; os.MkdirTemp here keeps each case
// independent.
func fakeEnvWorker(api selfhosted.API) *environments.EnvironmentWorker {
	dir, err := os.MkdirTemp("", "ama-worker-test-")
	if err != nil {
		dir = os.TempDir()
	}
	return environments.NewEnvironmentWorker(api, environments.EnvironmentWorkerOptions{
		EnvironmentID: "env_test",
		Workdir:       dir,
		// No ToolsFunc: every case finishes at GetSession and never reaches the step that
		// builds the tool set. The SDK would use its default tool set (local execution), but
		// those tools are never called.
		Logger: log.New(discardWriter{}, "", 0),
	})
}

// TestPollLoopPassesSessionIDToWorkerFactory is **the only case that can stop that production
// incident**.
//
// # That incident, and why none of the existing tests caught it
//
// After the worker pool became concurrent it switched to the SDK's `HandleItem`, which hardcodes
// `useWorkdirAsSession` to true internally, so workdir changed from `<root>/<sessionID>` to
// `<root>`:
//
//	Run        → handleItem(ctx, item, false) → <root>/sesn-xxx   ✅
//	HandleItem → handleItem(ctx, item, true)  → <root>            ❌
//
// And the tool layer at the time derived the Ark session id from `filepath.Base(workdir)`. So
// **every tool call failed, not a single container could be created, and conversations got
// stuck in requires_action**.
//
// ⚠️ **All tests were green at the time**, including the concurrency cases in this file, because
// they stubbed the worker factory to "ignore the input", and the tool layer's cases only tested
// the function itself. **Each side was right on its own, the wiring was wrong, and nobody looked
// in between.**
//
// So this case does not test some function's behavior, but **whether this wire is connected**:
// pollLoop must hand the session id in `WorkItem` to the factory that builds the worker.
func TestPollLoopPassesSessionIDToWorkerFactory(t *testing.T) {
	api := &fakeWorkAPI{
		queue: []selfhosted.WorkItem{
			workItem("work_1", "sesn-20260101000004-ex004"),
			workItem("work_2", "sesn-20260101000005-ex005"),
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var got []string

	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 4}, log: testLogger()}
	// With our own orchestration, pollLoop passes the session id to processWork as the 4th
	// argument; install a fake processor here that captures it (no real tools; it finishes at
	// the GetSession step on the fake API).
	pool.processWork = func(_ context.Context, item selfhosted.WorkItem, _ selfhosted.API, sid string, _ *slog.Logger) error {
		mu.Lock()
		got = append(got, sid)
		mu.Unlock()
		return nil // no real tools; this test only checks whether the session id reached the processor
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = pool.pollLoop(ctx, api, "env_test", "worker_test", 4, testLogger())
	}()

	waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 2
	}, "两条 work 都造过 worker")
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	seen := map[string]bool{}
	for _, id := range got {
		if id == "" {
			t.Fatal("工厂收到空 session id —— ToolsFunc 会退回去猜 workdir，" +
				"而在 HandleItem 下那条线索是断的（末段是根目录名），" +
				"表现为每次工具调用都失败、容器建不出来")
		}
		if !strings.HasPrefix(id, "sesn-") {
			t.Errorf("工厂收到的不像方舟 session id：%q", id)
		}
		seen[id] = true
	}
	// **Each work item gets its own id**; they must not all get the same one. Otherwise
	// containers get mixed up under concurrency, and two sessions sharing one container is no
	// isolation.
	if !seen["sesn-20260101000004-ex004"] || !seen["sesn-20260101000005-ex005"] {
		t.Errorf("两条 work 没有各自拿到自己的 session id，实际收到：%v", got)
	}
}

// TestPollLoopPassesOwnLoggerToProcessor pins "each work item gets the logger of its own
// environment".
//
// There is only one p.processWork for the whole pool, but one pollLoop per environment. The
// logger used to be bound when the closure was wired (the wiring point is in runOne, one per
// environment), so a later environment overwrote earlier ones, and the work logs of all
// environments were tagged with the last environment's workspace. When debugging in
// production, searching logs by workspace would not find your own.
// Here two environments share one pool and each polls one work item; assert that the processor
// gets each one's own logger.
func TestPollLoopPassesOwnLoggerToProcessor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	got := map[string]*slog.Logger{}

	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 2}, log: testLogger()}
	pool.processWork = func(_ context.Context, _ selfhosted.WorkItem, _ selfhosted.API, sid string, logger *slog.Logger) error {
		mu.Lock()
		got[sid] = logger
		mu.Unlock()
		return nil
	}

	envs := []struct {
		env, sid string
		logger   *slog.Logger
	}{
		{"env_a", "sesn-20261013000000-aaaaa", testLogger()},
		{"env_b", "sesn-20261013000000-bbbbb", testLogger()},
	}
	var wg sync.WaitGroup
	for _, e := range envs {
		api := &fakeWorkAPI{queue: []selfhosted.WorkItem{workItem("work_"+e.env, e.sid)}}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = pool.pollLoop(ctx, api, e.env, "worker_"+e.env, 2, e.logger)
		}()
	}

	waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == len(envs)
	}, "两个环境的 work 都交给了处理器")
	cancel()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for _, e := range envs {
		if got[e.sid] != e.logger {
			t.Errorf("%s 的 work 拿到的不是本环境 pollLoop 的 logger——"+
				"日志会标成别的空间（线上实测过：ws_a 的 work 标着 ws_b）", e.env)
		}
	}
}

// TestWorkerPassesRootWorkdirNotSessionDir pins "the workdir we give the SDK is the root
// directory".
//
// The case above tests "was the session id passed through"; this one tests **why it must be**:
// the `Workdir` we give the SDK is a root directory (`/tmp/ama-worker-sessions`), and
// `HandleItem` does not add a session subdirectory under it (`handleItem(ctx, item, true)`, the
// opposite of `Run`'s false).
//
// So there is **no** session id to guess from workdir. dockertools no longer derives it from
// workdir and errors out when no id is given. This case pins that with a real
// `dockertools.Provider`: no session id, only the root directory, **must fail**.
// If it goes red, someone changed "error when there is no id" into a silent fallback (e.g.
// going back to guessing from workdir, or landing on a shared container), which would mix
// different sessions into the same container.
func TestWorkerPassesRootWorkdirNotSessionDir(t *testing.T) {
	// Give no explicit session id: simulates the old "can only guess from workdir" path.
	fn := dockertools.New(nil, dockertools.Config{}, testLogger()).
		ToolsFuncForSession(context.Background(), "")

	_, err := fn(&agenttoolset.AgentToolContext{Workdir: workerRootDir})
	if err == nil {
		t.Fatalf("没给 session id、只给根目录 %q 竟然拿到了工具集 —— "+
			"dockertools 把「没有 id 就报错」改成了静默降级（会话会串容器）",
			workerRootDir)
	}
	// The error must point the way, otherwise the next person has to trace this chain again.
	if !strings.Contains(err.Error(), "session id") {
		t.Errorf("报错没提示缺的是 session id：%v", err)
	}
}

// TestPollLoopDoesNotStopWorkOnShutdown guards against "restart once, conversation stuck forever".
//
// # The real failure
//
// Session sesn-20260101000006-ex006 stopped for good at its 47th tool call. The database had 47
// agent.tool_use but only 46 user.tool_result, and the last event was
// session.status_idle{stop_reason: requires_action}. Within three seconds the logs showed:
//
//	13:07:45.799  worker stopped        ← ./dev.sh restart
//	13:07:48.929  agent.tool_use        ← the model wants a tool again, nobody is listening
//	13:07:50.970  worker pool started   ← the new process is up, but never gets this one back
//
// The root cause is the SDK's handleItem defer: once ctx is canceled, heartbeatLoop just
// returns **without calling markStopped**, so cause is empty and `shouldStopItem("")` takes the
// default and returns true. A graceful exit is treated as "this work item is done", and
// StopWork{Force:true} is sent. That work item becomes final on the Ark side, and the new
// process can never claim it.
//
// # This tests the wiring, not a function
//
// Testing shutdownSafeAPI.StopWork alone is not enough; that is exactly the lesson of the
// workdir incident: **each side was right, the wiring was wrong, nobody looked in between**. So
// this goes through the real pollLoop + the real EnvironmentWorker, lets the SDK's own defer
// run, and asserts that the StopWork it sends is blocked. If the guard is not wired (e.g.
// someone changes newWorker back to building its own API), this must go red.
func TestPollLoopDoesNotStopWorkOnShutdown(t *testing.T) {
	gate := make(chan struct{})
	api := &fakeWorkAPI{
		queue:          []selfhosted.WorkItem{workItem("work_1", "sesn-20260101000006-ex006")},
		getSessionGate: gate,
	}

	ctx, cancel := context.WithCancel(context.Background())
	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 4}, log: testLogger()}
	installFakeProcessor(pool)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = pool.pollLoop(ctx, api, "env_test", "worker_test", 4, testLogger())
	}()

	// Wait until this work item is really "being processed": right now it is held on
	// GetSession, matching "conversation halfway through" in production.
	waitFor(t, 3*time.Second, func() bool {
		acked, _ := api.snapshot()
		return len(acked) == 1
	}, "work 已被 ack 并开始处理")

	// Simulate process exit: cancel ctx first, then release so the SDK's defer runs.
	// The order must not be reversed: reversed it becomes "finished normally", and we would not
	// be testing the exit path.
	cancel()
	close(gate)
	<-done

	_, stopped := api.snapshot()
	if len(stopped) != 0 {
		t.Fatalf("进程退出时把在途 work 结算掉了：%v\n"+
			"方舟侧那条 work 会进终态，新实例 reclaim 也领不回来——"+
			"用户看到的是「对话说到一半就没了」，且全程零报错。\n"+
			"根因：SDK 的 handleItem defer 在 ctx 取消时 cause 为空，"+
			"shouldStopItem(\"\") 返回 true，于是发 StopWork{Force:true}。"+
			"护栏见 core.shutdownSafeAPI", stopped)
	}
}

// TestPollLoopStillStopsWorkWhenRunning is the counter case of the one above, so the guard does
// not block too much.
//
// StopWork should only be blocked when "the process is exiting". A work item that finished
// normally must be settled as usual, otherwise it keeps holding the lease until the reclaim
// timeout lets someone take it: every conversation would wait 90 seconds for nothing. If the
// guard is written as an unconditional return nil, this must go red.
func TestPollLoopStillStopsWorkWhenRunning(t *testing.T) {
	api := &fakeWorkAPI{
		queue: []selfhosted.WorkItem{workItem("work_1", "sesn-20260101000006-ex006")},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 4}, log: testLogger()}
	installFakeProcessor(pool)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = pool.pollLoop(ctx, api, "env_test", "worker_test", 4, testLogger())
	}()

	// Do not cancel ctx: this work item wraps up on its own after GetSession returns an error.
	// That is "finished normally", and the SDK's defer should send StopWork as usual.
	waitFor(t, 3*time.Second, func() bool {
		_, stopped := api.snapshot()
		return len(stopped) == 1
	}, "正常结束的 work 被 StopWork 结算")

	cancel()
	<-done
}

// TestPollWorkCarriesReclaim pins that the poll request carries reclaim.
//
// This is the other half of the fix. The guard only covers graceful exits that "ran the
// defer"; when the process is kill -9'ed (or the machine just disappears) the defer never runs,
// and the work item stays "claimed but no heartbeat". **Only a poll with reclaim_older_than_ms
// can take it back.**
//
// Both are required:
//
//	graceful exit → guard sends no StopWork → new process reclaims it
//	hard kill     → none was sent anyway    → new process reclaims it
//
// Without this parameter, both cases show up as "after a restart the conversation stays in
// requires_action forever".
func TestPollWorkCarriesReclaim(t *testing.T) {
	api := &fakeWorkAPI{}

	ctx, cancel := context.WithCancel(context.Background())
	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 1}, log: testLogger()}
	installFakeProcessor(pool)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = pool.pollLoop(ctx, api, "env_test", "worker_test", 1, testLogger())
	}()

	waitFor(t, 3*time.Second, func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		return len(api.pollReqs) > 0
	}, "至少 poll 过一次")
	cancel()
	<-done

	api.mu.Lock()
	req := api.pollReqs[0]
	api.mu.Unlock()

	if req.ReclaimOlderThanMS <= 0 {
		t.Fatalf("poll 没带 reclaim_older_than_ms（实际 %d）——"+
			"上一个进程没跑完的 work 永远没人接手，"+
			"那条会话会一直停在 requires_action", req.ReclaimOlderThanMS)
	}
	// Lower bound: must be greater than the SDK's heartbeat TTL (30 seconds), otherwise it would
	// steal work that is running and just one heartbeat late: two containers, two tool_results,
	// worse than being stuck.
	if req.ReclaimOlderThanMS <= 30_000 {
		t.Errorf("reclaim 阈值 %d ms 不大于 SDK 心跳 TTL(30s)，"+
			"会把还活着的 work 抢走造成双跑", req.ReclaimOlderThanMS)
	}
}

// fakeWorkerFactory is the factory pollLoop needs.
//
// ⚠️ **Must use the api passed in, not capture the outer one in a closure**: what pollLoop hands
// down is the one wrapped with the guard (shutdownSafeAPI). Capturing the outer bare api
// bypasses the guard, and TestPollLoopDoesNotStopWorkOnShutdown would go red as "the guard did
// not work", while the real cause is that the test itself is wired wrong.
func fakeWorkerFactory(_ string, api selfhosted.API) *environments.EnvironmentWorker {
	return fakeEnvWorker(api)
}

// idlePollAPI is a "queue always empty" double: every PollWork returns nil, nil right away
// (the shape of an Ark long poll that timed out with no work), and records the time of each
// call.
//
// **fakeWorkAPI cannot be reused**: when its queue is empty it hangs on `<-ctx.Done()`, which
// does the waiting the code under test should do itself. Real Ark hangs at most 999ms (the
// block_ms limit) and returns.
// Using a double that "hangs on its own" to test "does the caller pause" is a fake test that is
// always green.
type idlePollAPI struct {
	fakeWorkAPI

	mu    sync.Mutex
	calls []time.Time
}

func (a *idlePollAPI) PollWork(_ context.Context, _ selfhosted.PollWorkRequest) (*selfhosted.WorkItem, error) {
	a.mu.Lock()
	a.calls = append(a.calls, time.Now())
	a.mu.Unlock()
	return nil, nil
}

func (a *idlePollAPI) gaps() []time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []time.Duration
	for i := 1; i < len(a.calls); i++ {
		out = append(out, a.calls[i].Sub(a.calls[i-1]))
	}
	return out
}

// TestPollLoopSleepsWhenQueueIsEmpty guards against the 4159 429s seen in production.
//
// # The real failure
//
// In a one-hour window in production there were 4159 `poll work failed ... status 429 The RPM
// ... exceeded`, across 7 workspaces and 21 poll loops, **and in the same window 0 "work
// claimed" and 0 tool executions**. No work was done at all; idle spinning alone used up the
// Ark account's RPM quota (self-inflicted).
//
// # The root cause was one missing sleep here
//
// Ark's `block_ms` limit is 999 (the Ark docs on long-polling for work say "1-999
// milliseconds"), so a "long poll" hangs for at most 1 second. The SDK itself also does
// `sleep(ctx, jitter(1s, 3s))` when the queue is empty (`lib/environments/poller.go`). When we
// copied its loop we **missed that line**, so when idle it became a busy loop firing once a
// second.
//
// # The check is "the gap between two polls", not "is there a sleep call"
//
// Assert a lower bound on the measured gap. Using half of the first level leaves margin for
// jitter and scheduling noise: the first level is 1s with ±20% jitter and never goes below
// 800ms, but half as the lower bound keeps occasional timing glitches on CI from making this
// flaky.
//
// ⚠️ Mutation check: this must go red if the `waker.wait(ctx, ladder.next())` in pollLoop is
// deleted (i.e. back to "just continue").
func TestPollLoopSleepsWhenQueueIsEmpty(t *testing.T) {
	api := &idlePollAPI{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 4}, log: testLogger()}
	installFakeProcessor(pool)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = pool.pollLoop(ctx, api, "env_test", "worker_test", 4, testLogger())
	}()

	// ⚠️ **Wait until "enough samples", do not sleep a hardcoded duration.**
	//
	// The first version had `time.Sleep(2500 * time.Millisecond)` here, while the jitter upper
	// bound is 3000ms. When jitter drew ≥2.5s, the second poll never happened and the case failed
	// with "fewer than two polls". The odds are exactly (3.0-2.5)/(3.0-1.0) = **25%**, measured
	// 7/50, and it blocked two releases in a row. The logic under test was right all along; the
	// window arithmetic was what broke.
	//
	// So the window is **derived from constants** instead of typed by hand: when the ladder
	// changes it follows automatically and will not quietly become flaky again.
	//
	// ⚠️ With ladder backoff, **the first wantPolls polls all fall in the first level** (it takes
	// workerBackoffStep empty polls in a row to step up), so the window could use the first
	// level; but to avoid coupling to workerBackoffStep's value, use the larger of the first two
	// levels as the upper bound.
	const wantPolls = 3 // 3 calls = 2 gaps, more stable than looking at just 1 gap
	idleMax := time.Duration(float64(workerBackoffLevels[1]) * (1 + workerBackoffJitter))
	deadline := wantPolls*idleMax + 3*time.Second
	waitFor(t, deadline, func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		return len(api.calls) >= wantPolls
	}, "队列为空时至少 poll 到 3 次")
	cancel()
	<-done

	gaps := api.gaps()
	if len(gaps) == 0 {
		t.Fatal("poll 不足两次，用例本身没驱动起来")
	}

	floor := workerBackoffLevels[0] / 2
	for i, g := range gaps {
		if g < floor {
			t.Fatalf("第 %d 次与第 %d 次 poll 只隔了 %v（下界 %v）——\n"+
				"队列为空时没有歇一下，空闲的 worker 变成每秒一发的 busy-loop。\n"+
				"线上 21 条这样的循环合成 ~1200 RPM，把方舟账号配额打满，\n"+
				"表现为一小时 4159 条 429，而同期一条 work 都没领到。\n"+
				"修复在 core.pollLoop 的空队列分支：waker.wait(ctx, ladder.next())",
				i, i+1, g, floor)
		}
	}

	// Upper bound: do not make the idle gap too long, or picking up new work when idle gets
	// noticeably slower. arkReclaimOlderThan(90s) is a hard constraint; use a value far more
	// conservative than it here.
	//
	// ⚠️ With ladder backoff **the gap is supposed to step up** (that is the point of this
	// change), so the upper bound must use "the slowest level", not the first one; otherwise
	// this would report falsely once the ladder kicks in.
	// What really guards against "too slow" is the workerBackoffMax constant itself, plus the
	// wake-up mechanism.
	ceiling := time.Duration(float64(workerBackoffMax) * (1 + workerBackoffJitter))
	for i, g := range gaps {
		if g > ceiling {
			t.Errorf("第 %d 次间隔 %v 超过阶梯上限 %v——"+
				"空闲时接新 work 会变慢", i, g, ceiling)
		}
	}
}

// pollGateProbeAPI hands out one work item on the first PollWork (so pollLoop hands the
// gate-wrapped API to the worker factory, which is how we get it), and returns 429 every time
// after that.
//
// ListEvents simply succeeds. **This is the key**: then "ListEvents slowed down" can only come
// from the gate being closed by PollWork's 429, never from ListEvents hitting a rate limit
// itself.
type pollGateProbeAPI struct {
	fakeWorkAPI

	mu    sync.Mutex
	polls int
}

func (a *pollGateProbeAPI) PollWork(ctx context.Context, _ selfhosted.PollWorkRequest) (*selfhosted.WorkItem, error) {
	a.mu.Lock()
	a.polls++
	n := a.polls
	a.mu.Unlock()
	if n == 1 {
		item := workItem("work_1", "sesn-pollgate")
		return &item, nil
	}
	return nil, arkRateLimitErr()
}

// TestPollWork429ClosesGateForWholeEnvironment pins "PollWork must also go through the rate
// limit gate".
//
// # Why it was not wrapped before
//
// workerratelimit.go originally skipped PollWork on purpose, reasoning "it has arkBackoff, and
// stacking another layer would conflict". **That reasoning only holds in a single process**:
// `failures` is a local variable, one per pollLoop, while production runs "environments × pods"
// loops (measured 7 environments × 3 pods = 21). Each backing off on its own still adds up to
// one steady high-frequency stream, exactly the shape the workerratelimit.go file header itself
// describes; PollWork just was not counted back then.
//
// # Why the check cannot be "the gap between two polls"
//
// That measures nothing: `arkBackoff` already spaces retries by 2 seconds, **so the gap exists
// even when the gate is bypassed**, and the assertion is always true. We must pick an effect
// only the gate can cause.
//
// The one picked here is **spreading across call types**: a 429 hit by PollWork must make
// ListEvents in the same environment wait too. That is the observable form of the design "rate
// limits are per account, so backoff is shared per environment", and arkBackoff can never
// produce it (it only governs its own poll loop).
//
// ⚠️ Mutation check: must go red if pollWorkThroughGate in pollLoop is changed back to
// api.PollWork.
func TestPollWork429ClosesGateForWholeEnvironment(t *testing.T) {
	api := &pollGateProbeAPI{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 4}, log: testLogger()}

	// Capture the API pollLoop hands to the worker. **It must be this one**: capturing the outer
	// bare api bypasses the gate (same lesson as in the fakeWorkerFactory comment).
	handed := make(chan selfhosted.API, 1)
	installRecordingProcessor(pool, handed)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = pool.pollLoop(ctx, api, "env_test", "worker_test", 4, testLogger())
	}()

	var workAPI selfhosted.API
	select {
	case workAPI = <-handed:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("pollLoop 没把 API 交给 worker 工厂")
	}

	// Use **the gate's observable effect** directly as the check: poll ListEvents until it gets
	// held up.
	//
	// ⚠️ Cannot wait on the "PollWork returned 429" flag: the probe sets it inside PollWork,
	// while gate.Observe, which closes the gate, only runs in pollWorkThroughGate **after**
	// PollWork returns, so there is a window between them. Under heavy CI load (-race over all
	// packages) ListEvents passes instantly before the gate closes (measured 20µs, lower bound
	// 125ms); locally the window is tiny and it almost never reproduces.
	//
	// pollLoop must keep running: it is its Observe that closes the gate; once measured, the
	// defer cancel wraps up. ListEvents here uses Background instead of the canceled ctx: the
	// gate lets calls through right away when ctx is canceled, and then nothing could be
	// measured.
	floor := arkRateLimitBackoffStart / 4
	deadline := time.Now().Add(5 * time.Second)
	var waited time.Duration
	for time.Now().Before(deadline) {
		start := time.Now()
		if _, err := workAPI.ListEvents(context.Background(), selfhosted.ListEventsRequest{}); err != nil {
			cancel()
			<-done
			t.Fatalf("ListEvents 本身不该失败：%v", err)
		}
		waited = time.Since(start)
		if waited >= floor {
			break
		}
		// The gate is not closed yet (ListEvents returned instantly); give pollLoop a moment to
		// hit the 429 and Observe it.
		time.Sleep(10 * time.Millisecond)
	}

	if waited < floor {
		cancel()
		<-done
		t.Fatalf("PollWork 撞了 429，但同环境的 ListEvents 始终秒过（最后一次只等 %v，下界 %v）——\n"+
			"PollWork 没过限流闸门，撞了限流只有自己那条循环退避。\n"+
			"线上 21 条循环各退各的仍合成一条稳定高频流：一小时 4159 条 429，\n"+
			"而同期一条 work 都没领到。\n"+
			"接线在 core.pollLoop：pollWorkThroughGate(ctx, api, gate, ...)",
			waited, floor)
	}
}

// TestArkPollIdleJitterStaysInRange pins the range of jitter values.
//
// The range itself is a promise: the lower bound makes sure the rate really drops, the upper
// bound makes sure picking up new work when idle is not too slow.
// It also proves it is **not a constant**: a fixed interval makes multiple instances march in
// step and stack requests into periodic spikes, hitting the rate limit even when the average
// RPM is fine.
//
// ⚠️ With ladder backoff (workerbackoff.go), jitter changed from "fixed range [1s,3s)" to "±20%
// per level". **What this case guards is unchanged**: still "must not degrade into a constant".
// So the check became "sampling at each level must give several different values", instead of
// pinning the old range.
func TestArkPollIdleJitterStaysInRange(t *testing.T) {
	for _, base := range workerBackoffLevels {
		seen := map[time.Duration]bool{}
		lo := time.Duration(float64(base) * (1 - workerBackoffJitter))
		hi := time.Duration(float64(base) * (1 + workerBackoffJitter))
		for i := 0; i < 200; i++ {
			d := jitterAround(base)
			if d < lo || d > hi {
				t.Fatalf("jitterAround(%v) = %v，超出 ±%.0f%% 的 [%v, %v]",
					base, d, workerBackoffJitter*100, lo, hi)
			}
			seen[d] = true
		}
		if len(seen) < 2 {
			t.Errorf("在 %v 这一级上 200 次取样只得到 %d 个不同值——jitter 退化成了定值，"+
				"同一级的多条循环会齐步走，把请求叠成周期性尖峰", base, len(seen))
		}
	}
}
