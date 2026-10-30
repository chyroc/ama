// Tests for the two gates against zombie workers: stop sending when sends cannot get through, stop running when the run is dead.
//
// # What these tests guard against
//
// That production incident: within ten minutes two workers logged 2995 lines of
// `tool result send hit permanent 4xx` + `worker api status 400`,
// while maxing out that Ark account's RPM, so the same workers' ListEvents got 1132 429s
// (self-inflicted). Four sessions each hit 400 562~1252 times, while their runs had long finished;
// `sesn-20260101000008-ex008` **kept hammering for about 40 more minutes** after its run finished.
//
// The mechanism is three things stacked together (see the workersendguard.go file header): the SDK's
// `retrySendEvent` gives up "this delivery attempt", not "this pending result", so the result stays in
// `pendingResults`; our API wrapper downgrades the SDK to list mode at one round per 500ms;
// and that pending result also holds down the idle timer forever (`hasIdleBlockers`),
// so `MaxIdle` never fires once.
package core

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	coremem "github.com/chyroc/ama/server/repository/dao/coremem"
	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

// sendProbeAPI records every SendEvent and ListEvents and returns errors as scripted.
//
// Only these two are implemented; the rest come from embedding selfhosted.API (the value is nil, so calling one panics,
// which is the right reaction to "the test touched a path it should not", and must not be swallowed silently).
type sendProbeAPI struct {
	selfhosted.API

	mu sync.Mutex
	// err is the error every SendEvent returns (nil means success).
	err error
	// calls is the number of times SendEvent **really reached the layer below**; that is the check.
	calls int
	// listCalls is the number of times ListEvents really reached the layer below.
	//
	// **Counted separately from calls**: the gate must block both reads and writes, and one combined number could not show "writes blocked,
	// reads still running wild", a half-fixed state that was exactly the source of those 15 429s in production.
	listCalls int
}

func (p *sendProbeAPI) SendEvent(_ context.Context, _ selfhosted.SendEventRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.err
}

func (p *sendProbeAPI) ListEvents(
	_ context.Context, _ selfhosted.ListEventsRequest,
) (*selfhosted.ListEventsResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listCalls++
	return &selfhosted.ListEventsResponse{}, nil
}

func (p *sendProbeAPI) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *sendProbeAPI) listCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listCalls
}

// arkPayloadInvalidErr builds the real shape of that production 400.
//
// **It must be *selfhosted.APIError**: the check uses errors.As (arkFatalStatus);
// a string stand-in like errors.New("400") would silently break the whole chain: tests all green, production still hammering.
// The real one is `worker api status 400: Request payload is invalid`.
func arkPayloadInvalidErr() error {
	return &selfhosted.APIError{
		StatusCode: http.StatusBadRequest,
		Message:    "Request payload is invalid",
		RequestID:  "req-zombie-probe",
	}
}

// TestWorkSendGuardStopsAfterPermanentFailure is the reason this whole change exists.
//
// The check is "**the number of times it reached the layer below**", not "whether an error was returned": the latter is always true (errors were already
// passed through), and deleting the gate would still be green. Here it sends three times, and the layer below should get only one.
func TestWorkSendGuardStopsAfterPermanentFailure(t *testing.T) {
	probe := &sendProbeAPI{err: arkPayloadInvalidErr()}
	guard := newWorkSendGuard(probe, "sesn-20260101000008-ex008", nil, testLogger())

	ctx := context.Background()
	for i := range 3 {
		err := guard.SendEvent(ctx, selfhosted.SendEventRequest{})
		if err == nil {
			t.Fatalf("第 %d 次投递该返回错误（假装成功会在 ResultStore 里记一条假的「已送达」）", i+1)
		}
		// The error must be kept as is: it is the only useful information when debugging.
		var apiErr *selfhosted.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
			t.Fatalf("第 %d 次的错误形状变了：%v", i+1, err)
		}
	}

	if got := probe.count(); got != 1 {
		t.Errorf("永久 4xx 之后又打了 %d 次网络（该只有 1 次）——\n"+
			"投递闸没生效。SDK 的 retrySendEvent 放弃的只是「这一次投递」，\n"+
			"那条结果仍留在 pendingResults 里，而 list 模式每 500ms flush 一轮，\n"+
			"于是线上一条 session 刷了 1214 次 400、持续约 40 分钟。\n"+
			"闸在 core.workSendGuard.SendEvent", got)
	}
}

// TestWorkSendGuardKeepsRetryingRateLimit is the counterexample of the test above, so backoff is not broken along with it.
//
// ⚠️ **This is not an extra; it is required.** 429 is exactly the one that should back off and retry: treating it as permanent
// would make one rate limit throw away a tool result that should have been delivered, trading "slow for a while" for
// "this turn stuck in requires_action forever", worse than the original bug. Same for 408/409.
//
// The check is again "the number of times it reached the layer below": all three sends must really go out.
func TestWorkSendGuardKeepsRetryingRecoverableErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		// This one is the point: rate limiting must keep retrying (backoff is handled by arkRateLimitGate).
		{"限流 429", arkRateLimitErr()},
		{"请求超时 408", &selfhosted.APIError{StatusCode: http.StatusRequestTimeout}},
		{"冲突 409", &selfhosted.APIError{StatusCode: http.StatusConflict}},
		{"服务端 502", &selfhosted.APIError{StatusCode: http.StatusBadGateway}},
		{"网络错误", errors.New("dial tcp: connection refused")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := &sendProbeAPI{err: tc.err}
			// Give it a cancel that could be triggered by mistake: a recoverable error must never stop this work.
			cancelled := false
			guard := newWorkSendGuard(probe, "sesn-recoverable", func() { cancelled = true }, testLogger())

			ctx := context.Background()
			for range 3 {
				_ = guard.SendEvent(ctx, selfhosted.SendEventRequest{})
			}

			if got := probe.count(); got != 3 {
				t.Errorf("%s 被当成永久错误了（只打了 %d 次，该 3 次）——\n"+
					"429 是要退避重试的那一个，当成永久错误会丢掉一条本该送达的"+
					"工具结果，让这一轮永久卡在 requires_action，比原 bug 更糟。\n"+
					"分类见 core.arkFatalStatus（照抄 SDK 的 isFatal4xxStatus）", tc.name, got)
			}
			if cancelled {
				t.Errorf("%s 把这条 work 叫停了——可恢复错误不该结束 work", tc.name)
			}
		})
	}
}

// TestWorkSendGuardStopsListingAfterPermanentFailure pins "the gate must block reads too".
//
// # Why reads must be blocked too
//
// The wrap-up reconciliation (auditToolCalls) first calls ListEvents to find orphans, then sends one SendEvent for each.
// This session permanently accepts no events, so not one orphan found can be delivered; **that round of reads
// is pure waste**, and it really does hit the quota: in production, session sesn-20260101000010-ex010, while the gate
// only wrapped SendEvent, this fallback ListEvents alone produced 15 429s.
//
// ⚠️ Those 15 were the only calls in this case that really hit rate limiting: the 400/500 on the write side are rejected by Ark directly and
// do not count toward RPM, while **the read side does**. So "blocking only writes" is a half-done fix that looks finished.
//
// The check is "the number of times it reached the layer below": after hitting 400, none of three ListEvents should go out.
func TestWorkSendGuardStopsListingAfterPermanentFailure(t *testing.T) {
	probe := &sendProbeAPI{err: arkPayloadInvalidErr()}
	guard := newWorkSendGuard(probe, "sesn-20260101000010-ex010", nil, testLogger())

	ctx := context.Background()
	// Trip the gate first (this is the production order: the runner hits 400 first, the fallback reconciliation comes after).
	if err := guard.SendEvent(ctx, selfhosted.SendEventRequest{}); err == nil {
		t.Fatal("驱动用的 SendEvent 该返回 400，替身没配对")
	}

	for i := range 3 {
		resp, err := guard.ListEvents(ctx, selfhosted.ListEventsRequest{})
		if err == nil {
			t.Fatalf("第 %d 次拉取该返回错误——\n"+
				"返回空结果会让 listOrphanToolCalls 得出「一个孤儿都没有」，"+
				"于是对账悄悄跳过、posted=0、一条日志都不打，"+
				"那正是「兜底没生效」与「本来就没有孤儿」共用一条零日志路径的形状", i+1)
		}
		if resp != nil {
			t.Errorf("第 %d 次拉取返回了非 nil 的结果，调用方可能把它当成真数据", i+1)
		}
	}

	if got := probe.listCount(); got != 0 {
		t.Errorf("永久 4xx 之后 ListEvents 又打了 %d 次网络（该是 0 次）——\n"+
			"闸只包了 SendEvent，读那侧还在裸奔。对账拉出来的孤儿一条都送不进去，"+
			"这些请求是纯浪费，而且**读会计入方舟账号 RPM**："+
			"线上 sesn-20260101000010-ex010 的 15 条 429 全是它打的。\n"+
			"闸在 core.workSendGuard.ListEvents", got)
	}
}

// TestWorkSendGuardKeepsListingBeforePermanentFailure is the counterexample of the test above:
// **while the gate has not tripped, ListEvents must hit the network as usual.**
//
// ⚠️ This is not an extra; it is required. Without it, writing ListEvents as "always return an error" would also make
// the test above all green, and that would break every work's wrap-up reconciliation (orphans could never be found again),
// bringing back the original failure in this file's header, "the whole turn stuck in requires_action".
func TestWorkSendGuardKeepsListingBeforePermanentFailure(t *testing.T) {
	probe := &sendProbeAPI{}
	guard := newWorkSendGuard(probe, "sesn-healthy", nil, testLogger())

	ctx := context.Background()
	for i := range 3 {
		if _, err := guard.ListEvents(ctx, selfhosted.ListEventsRequest{}); err != nil {
			t.Fatalf("第 %d 次拉取报错了：%v", i+1, err)
		}
	}

	if got := probe.listCount(); got != 3 {
		t.Errorf("闸没触发时 ListEvents 只打了 %d 次（该 3 次）——\n"+
			"无条件挡住读会让收尾对账再也判不出孤儿，"+
			"那一轮永久停在 requires_action（正是这道闸本该防的那个故障）", got)
	}
}

// TestWorkSendGuardKeepsListingAfterRecoverableFailure is the read-side version of the rate limit test.
//
// 429 should only back off (handled by arkRateLimitGate), **not stop reads too**: that would turn "slow for a while"
// into "this work's wrap-up reconciliation is skipped", so real orphans go undetected and that turn is stuck forever.
func TestWorkSendGuardKeepsListingAfterRecoverableFailure(t *testing.T) {
	probe := &sendProbeAPI{err: arkRateLimitErr()}
	guard := newWorkSendGuard(probe, "sesn-ratelimited", nil, testLogger())

	ctx := context.Background()
	// Hit one 429: recoverable, the gate must not trip.
	_ = guard.SendEvent(ctx, selfhosted.SendEventRequest{})

	if _, err := guard.ListEvents(ctx, selfhosted.ListEventsRequest{}); err != nil {
		t.Fatalf("撞过 429 之后拉取被挡了：%v——\n"+
			"429 只该退避，把它当永久错误会让收尾对账不做、真孤儿判不出来", err)
	}
	if got := probe.listCount(); got != 1 {
		t.Errorf("撞过 429 之后 ListEvents 打了 %d 次（该 1 次）", got)
	}
}

// auditGuardAPI reproduces the full shape of that production session: it can dispatch work, GetSession hangs,
// the event history has orphans, and this session's events **can never be delivered** (400).
//
// The counters count **the times it really reached the layer below**, reads and writes separately; all the checks rest on them.
type auditGuardAPI struct {
	fakeWorkAPI

	mu sync.Mutex
	// events is this session's event history: three bash calls started but without results.
	events []selfhosted.Event
	// listCalls / sendCalls are the times it reached the layer below.
	listCalls int
	sendCalls int

	// entered is closed when GetSession is called (the work is being handled).
	entered chan struct{}
	// released is closed when GetSession returns because ctx was canceled (this work was stopped).
	released  chan struct{}
	enterOnce sync.Once
}

func newAuditGuardAPI(items ...selfhosted.WorkItem) *auditGuardAPI {
	return &auditGuardAPI{
		fakeWorkAPI: fakeWorkAPI{queue: items},
		events: []selfhosted.Event{
			toolUseEvent("call_ztpih6gn1t9a179v3bgn9m11", "bash"),
			toolUseEvent("call_wutu6fcyym5fbma4lwalc37u", "bash"),
			toolUseEvent("call_0nthhszqhtgddd12bm4fxylr", "bash"),
		},
		entered:  make(chan struct{}),
		released: make(chan struct{}),
	}
}

func (a *auditGuardAPI) GetSession(
	ctx context.Context, _ selfhosted.GetSessionRequest,
) (*selfhosted.Session, error) {
	a.enterOnce.Do(func() { close(a.entered) })
	<-ctx.Done()
	close(a.released)
	return nil, ctx.Err()
}

func (a *auditGuardAPI) ListEvents(
	_ context.Context, _ selfhosted.ListEventsRequest,
) (*selfhosted.ListEventsResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.listCalls++
	return &selfhosted.ListEventsResponse{
		Events: append([]selfhosted.Event(nil), a.events...),
	}, nil
}

// SendEvent always returns that production 400: this session's events can never be delivered.
func (a *auditGuardAPI) SendEvent(_ context.Context, _ selfhosted.SendEventRequest) error {
	a.mu.Lock()
	a.sendCalls++
	a.mu.Unlock()
	return arkPayloadInvalidErr()
}

func (a *auditGuardAPI) counts() (list, send int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.listCalls, a.sendCalls
}

// lockedBuffer is a concurrency-safe log sink.
//
// **It must be locked**: the pollLoop work writes logs in its own goroutine, while the test reads in the main
// goroutine. A bare bytes.Buffer reports a data race under `-race` right away.
type lockedBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// TestPollLoopWiresSendGuardIntoToolAudit is **the test for the second production incident (2026-09-05)**:
// the wrap-up reconciliation must go through the send gate, not slip under it.
//
// # That incident, and why none of the existing tests caught it
//
// The gate is built **outside** workAPI (`newWorkSendGuard(workAPI, ...)`), so passing workAPI to reconciliation
// bypasses it. In the same work the gate worked for the runner but not for the fallback, and **each side was right on its own**:
// the tests above prove "the gate itself is right" and "the API the runner gets is wrapped by the gate", but not one proves
// "the API the fallback gets is wrapped too".
//
// In production (session sesn-20260101000010-ex010) the cost was: the runner hit 400 and tripped the gate,
// the work was stopped, but the fallback sent the results again unchanged through workAPI. Ark redelivered this work every 2.2 seconds,
// and in 372 seconds it produced **210 posts that were bound to fail** + 15 429s (those 15 came from the fallback's ListEvents).
// The evidence is hard: in that window "skipping this delivery" appeared **0 times** and "failed to post tool result" 210 times;
// not one fallback request went through the gate.
//
// # How this test reproduces that order
//
//  1. the work hangs on GetSession (matching "the conversation is halfway");
//  2. the test sends one SendEvent through the API pollLoop **actually hands to the worker factory** → 400
//     → the gate trips and calls cancelWork (matching the moment the production runner hit 400);
//  3. GetSession returns, HandleItem wraps up, **and then comes the wrap-up reconciliation**;
//  4. assert that during reconciliation **neither reads nor writes reached the layer below**.
//
// ⚠️ The log assertion in the check cannot be dropped: it is the positive evidence that "reconciliation really ran". Without it,
// deleting the whole reconciliation, or letting it be blocked by `workerCtx.Err()`, would make this test **falsely green**
// (zero calls of course satisfy "did not reach the layer below").
func TestPollLoopWiresSendGuardIntoToolAudit(t *testing.T) {
	api := newAuditGuardAPI(workItem("work_1", "sesn-20260101000010-ex010"))

	logBuf := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 4}, log: testLogger()}

	handed := make(chan selfhosted.API, 1)
	installRecordingProcessor(pool, handed)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = pool.pollLoop(ctx, api, "env_test", "worker_test", 4, logger)
	}()

	var workAPI selfhosted.API
	select {
	case workAPI = <-handed:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("pollLoop 没把 API 交给 worker 工厂")
	}

	// Wait until this work is really "being handled".
	select {
	case <-api.entered:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("work 没进入处理（GetSession 没被调到），用例没驱动起来")
	}

	// That production moment: the runner hits 400, the gate trips and stops this work.
	// **Must use the API handed down by pollLoop**; capturing the bare api outside would bypass the gate.
	if err := workAPI.SendEvent(context.Background(), selfhosted.SendEventRequest{}); err == nil {
		t.Fatal("驱动用的 SendEvent 该返回 400，替身没配对")
	}

	// Record the lower-layer counts at this moment: everything added after this **comes from reconciliation**.
	listBefore, sendBefore := api.counts()

	// Wait for this work to be stopped (GetSession returns); the wrap-up reconciliation comes right after.
	select {
	case <-api.released:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("闸置位之后这条 work 没被叫停，用例没驱动到对账那一步")
	}

	// Wait for reconciliation to finish. **The check is that log line**, not "wait a while".
	//
	// ⚠️ Both shapes must be accepted, otherwise this test would fail with "wait timed out" instead of "wrong count":
	//   - gate works: ListEvents is blocked → logs the reconciliation-failed line;
	//   - gate bypassed (the bug to catch): ListEvents succeeds and finds orphans →
	//     logs the tool-call-returned-no-result line.
	// Waiting only for the first, a mutation check would report just a timeout, not pointing at the real cause;
	// accepting both lets the two count assertions below be the only check, so the error message is clear.
	waitFor(t, 5*time.Second, func() bool {
		out := logBuf.String()
		return strings.Contains(out, "工具调用对账失败") ||
			strings.Contains(out, "工具调用没有回传结果")
	}, "收尾对账已经跑过")

	cancel()
	<-done

	listAfter, sendAfter := api.counts()

	if got := sendAfter - sendBefore; got != 0 {
		t.Errorf("对账期间又往底层发了 %d 条事件（该是 0 条）——\n"+
			"兜底没走投递闸，从闸底下钻过去了。闸建在 workAPI **外面**，"+
			"所以 auditToolCalls 传 workAPI 就等于绕过它：\n"+
			"runner 撞 400 之后闸已置位，兜底那几发却照旧打网络、照旧必然失败。\n"+
			"线上 sesn-20260101000010-ex010 因此在 372 秒里打出 210 条必然失败的补发。\n"+
			"接线在 core.pollLoop：p.auditToolCalls(auditCtx, sendGuard, ...)", got)
	}
	if got := listAfter - listBefore; got != 0 {
		t.Errorf("对账期间又往底层拉了 %d 次事件（该是 0 次）——\n"+
			"闸没挡住读。这条 session 已经永久不收事件了，对账出来的孤儿一条都"+
			"送不进去，而**读会计入方舟账号 RPM**：\n"+
			"线上那 15 条 429 全是兜底的这个 ListEvents 打的。\n"+
			"闸在 core.workSendGuard.ListEvents", got)
	}

	// Also pin "do not pretend success": when reconciliation is blocked it must not log "tool result posted",
	// which would give a false answer when debugging (see the SendEvent comment in workersendguard.go).
	if strings.Contains(logBuf.String(), "已补发工具结果") {
		t.Error("对账被闸挡住了，日志却说「已补发工具结果」——\n" +
			"那是一条假的「已送达」，而「工具结果到底有没有回到方舟」正是排查 " +
			"requires_action 卡死时唯一有用的信息")
	}
}

// TestWorkSendGuardCancelsWork pins "besides short-circuiting, this work must also be stopped".
//
// ⚠️ **Just not sending is not enough**; this separate assertion was forced by mutation testing: the result that cannot be sent
// stays in the SDK's pendingResults, and `hasIdleBlockers()` is true as long as it is non-empty,
// so `armIdle` only ever sets idleArmPending, `idleArmedAt` stays at the zero value, while
// `idleExpired()` requires it to be non-zero. **The very result that makes it a zombie also turns off the only timer
// that could let it exit on its own**, and MaxIdle(60s) never fires once.
//
// Short-circuiting without stopping lowers the request volume, but that goroutine (with the execution container and
// work lease it holds) keeps spinning until the process exits, turning into a **quiet** leak, and quiet leaks are harder to find.
func TestWorkSendGuardCancelsWork(t *testing.T) {
	probe := &sendProbeAPI{err: arkPayloadInvalidErr()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newWorkSendGuard(probe, "sesn-cancel-probe", cancel, testLogger())

	if err := ctx.Err(); err != nil {
		t.Fatal("用例开始时 ctx 就已取消，驱动不起来")
	}
	_ = guard.SendEvent(context.Background(), selfhosted.SendEventRequest{})

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("永久 4xx 之后没有叫停这条 work——\n" +
			"那条结果留在 SDK 的 pendingResults 里，而它同时压住了 idle 计时器\n" +
			"（hasIdleBlockers 非空 → armIdle 只置 pending → idleExpired 恒假），\n" +
			"所以 MaxIdle(60s) 一次都不会触发，goroutine 与 执行容器会一直占到进程退出。\n" +
			"叫停在 core.workSendGuard.SendEvent 里的 g.cancel()")
	}
}

// TestPollLoopWiresSendGuardIntoWorkAPI pins the **wiring**, not a function.
//
// ⚠️ This is the kind of pitfall this repo hits again and again ("a brand-new module can be all green yet not in effect"): the tests above
// build newWorkSendGuard directly, proving "the gate itself is right", not "the gate is really hooked onto the
// API the worker gets". Removing that newWorkSendGuard layer in pollLoop
// (`itemWorker := newWorker(it.SessionIDValue(), workAPI)`) leaves them all green.
//
// The check: take the API pollLoop **actually hands to the worker factory**, make it hit 400 twice,
// and see whether the second is short-circuited. Removing the wiring must turn this red.
func TestPollLoopWiresSendGuardIntoWorkAPI(t *testing.T) {
	probe := &sendProbeAPI{err: arkPayloadInvalidErr()}
	driver := &sendGuardDriverAPI{
		fakeWorkAPI: fakeWorkAPI{queue: []selfhosted.WorkItem{workItem("work_1", "sesn-sendguard-wiring")}},
		probe:       probe,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 4}, log: testLogger()}

	// Capture the API handed down by pollLoop. **It must be used**; capturing the driver outside
	// would bypass the wiring (the same lesson as in the fakeWorkerFactory comment).
	handed := make(chan selfhosted.API, 1)
	installRecordingProcessor(pool, handed)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = pool.pollLoop(ctx, driver, "env_test", "worker_test", 4, testLogger())
	}()

	var workAPI selfhosted.API
	select {
	case workAPI = <-handed:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("pollLoop 没把 API 交给 worker 工厂")
	}
	cancel()
	<-done

	callCtx := context.Background()
	for range 3 {
		_ = workAPI.SendEvent(callCtx, selfhosted.SendEventRequest{})
	}

	if got := probe.count(); got != 1 {
		t.Errorf("work 用的那个 API 撞了永久 4xx 之后又打了 %d 次网络（该只有 1 次）——\n"+
			"投递闸没接到 pollLoop 上：闸自己是对的，但 work 拿到的 API 没包它，\n"+
			"于是线上那 2995 条 400 照旧。\n"+
			"接线在 core.pollLoop 的 sendGuard 那一行（newWorkSendGuard）", got)
	}
}

// sendGuardDriverAPI lets pollLoop run (dispatching work, ack, heartbeat go through fakeWorkAPI),
// but forwards SendEvent to probe, so the times it really reaches the layer below can be counted.
//
// **fakeWorkAPI alone is not enough**: its SendEvent always succeeds, so short-circuiting cannot be counted;
// probe alone is not enough either: it has no PollWork/AckWork, so pollLoop cannot run.
type sendGuardDriverAPI struct {
	fakeWorkAPI
	probe *sendProbeAPI
}

func (d *sendGuardDriverAPI) SendEvent(ctx context.Context, req selfhosted.SendEventRequest) error {
	return d.probe.SendEvent(ctx, req)
}

// blockingSessionAPI's GetSession **blocks until its own ctx is canceled**, and records that.
//
// # Why the check must rest here
//
// This is the shared pivot of the two "wiring" tests. `GetSession` is the first call that hits the network after entering `HandleItem`,
// so the ctx it gets **is the one HandleItem received** (the SDK derives one layer in
// `handleItem` with `workCtx, cancel := context.WithCancel(ctx)`).
//
// So "was this work stopped" gets an observable form that does not depend on SDK internals:
// let it hang on GetSession, then see **whether it was released**.
//   - wiring right: cancelWork() → workCtx canceled → returns here → the whole work wraps up;
//   - wiring wrong (HandleItem used the outer ctx / the watcher did not start): **hangs forever**, until the test times out.
//
// ⚠️ This is exactly the gap exposed by mutations 5 and 6: asserting only "SendEvent no longer hits the network",
// changing `HandleItem(workCtx, ...)` back to `HandleItem(ctx, ...)`, or deleting the whole line
// `go p.watchRunTerminated(...)`, **leaves all tests green**. The gate itself is right,
// but it cannot stop anything, so that goroutine, with the execution container and work lease it holds,
// keeps spinning until the process exits. A quiet leak with zero errors.
type blockingSessionAPI struct {
	fakeWorkAPI

	// sendErr is the error SendEvent returns. nil means success (the terminal-state gate test takes this branch:
	// the shape it wants is exactly "delivery has always been fine/recoverable, but the run is already dead").
	sendErr error

	// entered is closed when GetSession is called, letting the test know "the work is already running".
	entered chan struct{}
	// released is closed when GetSession returns because ctx was canceled; **this is the check**.
	released  chan struct{}
	enterOnce sync.Once
}

func newBlockingSessionAPI(items ...selfhosted.WorkItem) *blockingSessionAPI {
	return &blockingSessionAPI{
		fakeWorkAPI: fakeWorkAPI{queue: items},
		entered:     make(chan struct{}),
		released:    make(chan struct{}),
	}
}

func (a *blockingSessionAPI) GetSession(
	ctx context.Context, req selfhosted.GetSessionRequest,
) (*selfhosted.Session, error) {
	a.enterOnce.Do(func() { close(a.entered) })
	<-ctx.Done()
	close(a.released)
	return nil, ctx.Err()
}

// SendEvent returns errors as scripted.
//
// **It must be implemented here**: the embedded fakeWorkAPI.SendEvent always succeeds, so the send gate would never
// trip, and the test would go red claiming "the gate did not work", while the real cause is a mismatched stand-in.
func (a *blockingSessionAPI) SendEvent(_ context.Context, _ selfhosted.SendEventRequest) error {
	return a.sendErr
}

// TestPollLoopSendGuardActuallyEndsTheWork pins "stopping really reaches HandleItem".
//
// ⚠️ **This was forced by mutation testing**: changing `HandleItem(workCtx, ...)` in pollLoop
// back to `HandleItem(ctx, ...)` left all the tests above (including the wiring one,
// TestPollLoopWiresSendGuardIntoWorkAPI) **all green**: SendEvent indeed no longer hit the
// network, but that work kept running. The request volume dropped and the leak went quiet, and quiet leaks are harder to find.
//
// The check is "was the work hanging on GetSession released"; see blockingSessionAPI.
func TestPollLoopSendGuardActuallyEndsTheWork(t *testing.T) {
	api := newBlockingSessionAPI(workItem("work_1", "sesn-20260101000008-ex008"))
	// The real shape from production: this session's events can never be delivered (400 payload invalid).
	api.sendErr = arkPayloadInvalidErr()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 4}, log: testLogger()}

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

	// Wait until this work is really "being handled": at this point it hangs on GetSession,
	// matching "the conversation is halfway" in production.
	select {
	case <-api.entered:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("work 没进入处理（GetSession 没被调到），用例没驱动起来")
	}

	// Simulate that production moment: this session's events can no longer be delivered (400 payload invalid).
	// **Must use the API handed down by pollLoop**; capturing the bare api outside would bypass the gate.
	if err := workAPI.SendEvent(context.Background(), selfhosted.SendEventRequest{}); err == nil {
		t.Fatal("驱动用的 SendEvent 该返回 400，替身没配对")
	}

	select {
	case <-api.released:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("工具结果永久投递失败之后，这条 work 仍然在跑——\n" +
			"投递闸只短路了发送，没能叫停这条 work。那条投不出去的结果留在 SDK 的\n" +
			"pendingResults 里，而它同时压住了 idle 计时器（hasIdleBlockers 非空 →\n" +
			"armIdle 只置 pending → idleExpired 恒假），所以 MaxIdle(60s) 一次都不会触发：\n" +
			"goroutine、执行容器、work lease 会一直占到进程退出（安静的泄漏，零报错）。\n" +
			"接线在 core.pollLoop：workCtx/cancelWork 与 HandleItem(workCtx, ...)")
	}

	cancel()
	<-done
}

// TestPollLoopStopsWorkWhenRunTerminated pins that the second gate **is hooked onto pollLoop**.
//
// ⚠️ Also forced by mutation testing: deleting the whole line `go p.watchRunTerminated(...)`
// left TestWatchRunTerminatedStopsWorkAfterRunEnded above green: it calls that
// function directly, proving "the watcher itself is right", not "the watcher is really started".
// That is exactly the recorded lesson of this repo, "a brand-new module can be all green yet not in effect".
//
// This one goes through the real pollLoop: that run in the database is already terminated while the work still hangs on GetSession
// (matching the shape "the tool is stuck and the errors keep being recoverable"), and asserts it gets stopped.
func TestPollLoopStopsWorkWhenRunTerminated(t *testing.T) {
	const sessionID = "sesn-20260101000009-ex009"

	store := coremem.NewMemStore()
	svc, err := New(Config{Store: store})
	if err != nil {
		t.Fatalf("建 Service 失败: %v", err)
	}
	// The real shape from production: the run was already finished, while the worker kept running.
	if _, err := store.CreateRun(context.Background(), apimodel.Run{
		ID:          sessionID,
		WorkspaceID: "ws_zombie",
		Status:      apimodel.RunTerminated,
		StartedAt:   "2026-09-01T23:03:57.000Z",
		EndedAt:     "2026-09-01T23:07:34.879Z",
	}); err != nil {
		t.Fatalf("造 run 失败: %v", err)
	}

	restore := workRunCheckInterval
	workRunCheckInterval = 10 * time.Millisecond
	defer func() { workRunCheckInterval = restore }()

	api := newBlockingSessionAPI(workItem("work_1", sessionID))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := &WorkerPool{svc: svc, cfg: WorkerConfig{Concurrency: 4}, log: testLogger()}
	installFakeProcessor(pool)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = pool.pollLoop(ctx, api, "env_test", "worker_test", 4, testLogger())
	}()

	select {
	case <-api.entered:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("work 没进入处理（GetSession 没被调到），用例没驱动起来")
	}

	select {
	case <-api.released:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("这条 session 的 run 已是 terminated，work 却还在跑——\n" +
			"终态闸没接到 pollLoop 上（巡检自己是对的，但没人起它）。\n" +
			"当 SendEvent 的错误一直是可恢复的（比如持续 429）时投递闸不触发，\n" +
			"而那条 run 早已被 reapStaleRuns 收尾、没人要这个结果了，\n" +
			"于是它继续退避重试——那些请求正是限流本身的来源。\n" +
			"接线在 core.pollLoop：go p.watchRunTerminated(workCtx, ...)")
	}

	cancel()
	<-done
}

// TestWatchRunTerminatedStopsWorkAfterRunEnded pins the second gate: when the run is dead, stop running.
//
// # It covers the half the send gate cannot
//
// The send gate trips when "the error is permanent". But the same incident had another shape: the errors stayed
// **recoverable** (the environment was being rate limited, SendEvent kept getting 429), so the send gate did not trip,
// while that run had long been marked terminated by reapStaleRuns; nobody wanted this result anymore.
//
// # Why the check is "ctx was canceled" and not "did it query the database"
//
// "It queried the database" is always true (this function is a query loop), so asserting it checks nothing. What must be asserted is
// **after finding the terminal state it really stopped this work**.
func TestWatchRunTerminatedStopsWorkAfterRunEnded(t *testing.T) {
	store := coremem.NewMemStore()
	svc, err := New(Config{Store: store})
	if err != nil {
		t.Fatalf("建 Service 失败: %v", err)
	}

	const sessionID = "sesn-20260101000008-ex008"
	// The real shape from production: the run was finished, with endedAt 40 minutes ago.
	if _, err := store.CreateRun(context.Background(), apimodel.Run{
		ID:          sessionID,
		WorkspaceID: "ws_zombie",
		Status:      apimodel.RunTerminated,
		StartedAt:   "2026-09-01T19:56:57.000Z",
		EndedAt:     "2026-09-01T19:57:55.506Z",
	}); err != nil {
		t.Fatalf("造 run 失败: %v", err)
	}

	// Make the watch interval small, otherwise the test would wait 30 seconds.
	restore := workRunCheckInterval
	workRunCheckInterval = 10 * time.Millisecond
	defer func() { workRunCheckInterval = restore }()

	pool := &WorkerPool{svc: svc, cfg: WorkerConfig{Concurrency: 1}, log: testLogger()}
	installFakeProcessor(pool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go pool.watchRunTerminated(ctx, sessionID, cancel, testLogger())

	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("run 已经是 terminated，这条 work 却没被叫停——\n" +
			"当错误一直是可恢复的（比如持续 429）时投递闸不触发，\n" +
			"而那条 run 已经被 reapStaleRuns 收尾、没人再要这个结果了，\n" +
			"继续退避重试是纯浪费，且它贡献的请求量正是限流本身的来源。\n" +
			"闸在 core.WorkerPool.watchRunTerminated")
	}
}

// TestWatchRunTerminatedLeavesLiveRunAlone is the counterexample of the test above, so normal conversations are not killed.
//
// ⚠️ **This one is critical**; it blocks three kinds of "overkill", each worse than the original bug:
//
//	idle       after a turn's end_turn the run is idle, and the SDK deliberately waits MaxIdle(60s) more,
//	           so the user's next message can continue directly (otherwise the container must pause and resume, ~16 seconds).
//	           Treating idle as a stop signal = tearing down the container after every turn, a real experience regression.
//	run missing  probes, and paths that do not store a Run, find nothing. Treating that as terminal would kill them,
//	           with the symptom "tool calls fail for no reason".
//	running    running / requires_action are normal active states.
func TestWatchRunTerminatedLeavesLiveRunAlone(t *testing.T) {
	// A turn that just ended: endedAt is now, well within workIdleRunGrace.
	// **Use relative times instead of hard-coded literals**: a hard-coded one would naturally "age" some day,
	// and this test would quietly turn from "check the follow-up window" into "check the zombie rule", the direction reversed and still green.
	justEnded := apimodel.FormatISO(time.Now().UTC())
	// Old enough to pass the threshold. For the rows with active states: their time condition is met,
	// and **the only thing stopping them is the status check**, which is how to verify the status half really works.
	longEnded := apimodel.FormatISO(time.Now().UTC().Add(-2 * workIdleRunGrace))

	cases := []struct {
		name    string
		status  apimodel.RunStatus
		endedAt string
		create  bool
	}{
		// No endedAt means "this turn has not been settled yet", and it must not be killed however old startedAt is:
		// old data, and sessions whose settlement frame has not arrived yet, look like this.
		{"空闲但从没结算过（endedAt 为空，等下一条消息）", apimodel.RunIdle, "", true},
		{"空闲且刚答完一轮（正在 MaxIdle 续聊窗口里）", apimodel.RunIdle, justEnded, true},
		// endedAt is old enough to pass the threshold, but the status is still active; these are handled by reapStaleRuns,
		// and stepping in here would cut off sessions waiting for tool results.
		{"在跑", apimodel.RunRunning, longEnded, true},
		{"等工具结果", apimodel.RunRequiresAction, longEnded, true},
		{"重排中", apimodel.RunRescheduling, longEnded, true},
		{"库里查不到（探针 / 不落 Run 的链路）", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := coremem.NewMemStore()
			svc, err := New(Config{Store: store})
			if err != nil {
				t.Fatalf("建 Service 失败: %v", err)
			}
			const sessionID = "sesn-live-probe"
			if tc.create {
				if _, err := store.CreateRun(context.Background(), apimodel.Run{
					ID: sessionID, WorkspaceID: "ws_live", Status: tc.status,
					StartedAt: "2026-09-01T19:56:57.000Z", EndedAt: tc.endedAt,
				}); err != nil {
					t.Fatalf("造 run 失败: %v", err)
				}
			}

			restore := workRunCheckInterval
			workRunCheckInterval = 5 * time.Millisecond
			defer func() { workRunCheckInterval = restore }()

			pool := &WorkerPool{svc: svc, cfg: WorkerConfig{Concurrency: 1}, log: testLogger()}
			installFakeProcessor(pool)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var stopped atomic.Bool
			go pool.watchRunTerminated(ctx, sessionID, func() {
				stopped.Store(true)
				cancel()
			}, testLogger())

			// Give it plenty of ticks to make a mistake (one tick per 5ms, at least dozens in 200ms).
			time.Sleep(200 * time.Millisecond)

			if stopped.Load() {
				t.Errorf("run 状态 %q（endedAt=%q）被当成终态、把这条 work 叫停了——\n"+
					"只有 terminated、以及「答完一轮且已过 workIdleRunGrace」才该叫停。\n"+
					"把刚 idle 的会话当停止信号会让每轮对话结束就立刻拆掉容器"+
					"（恢复要 ~16 秒），而把「查不到」当终态会掐死探针与不落 Run 的链路。\n"+
					"判据见 core.idleRunAbandoned", tc.status, tc.endedAt)
			}
		})
	}
}

// TestWatchRunStopsAbandonedIdleWork pins the real shape of that 429 incident in 2026-09:
// **the run answered a turn normally, but the work did not exit for twenty-odd hours.**
//
// # Why the existing terminated rule cannot cover this
//
// All 9 of those production runs were `status=idle` with `endedAt` set, the oldest dead for 23 hours.
// Their status was **completely normal**: a finished conversation turn is idle. So watching only terminated,
// this batch of work would never get a stop signal: `reapStaleRuns` has no idle in its rule (which is right;
// treating idle as a zombie would break follow-ups), so nothing anywhere would push them to terminated.
//
// And on the SDK's fallback polling path they called ListEvents every 3 seconds, maxing out the account's RPM.
// Worse, **they do not heal themselves**: the SDK's idle timer needs "reading a new end_turn event" once
// to arm, and these runs had long ended with no new events coming; even after rate limiting fully recovered they could not exit.
//
// # Why the check is that ctx was canceled
//
// Same as TestWatchRunTerminatedStopsWorkAfterRunEnded: "queried the database" is always true;
// what must be asserted is that **after finding it, this work really was stopped**.
func TestWatchRunStopsAbandonedIdleWork(t *testing.T) {
	store := coremem.NewMemStore()
	svc, err := New(Config{Store: store})
	if err != nil {
		t.Fatalf("建 Service 失败: %v", err)
	}

	// The real shape of production sesn-20260101000011-ex011: idle + endedAt written, dead for 23 hours.
	// **Use relative time**, otherwise this would silently stop working some day when workIdleRunGrace is raised.
	const sessionID = "sesn-20260101000011-ex011"
	endedLongAgo := apimodel.FormatISO(time.Now().UTC().Add(-23 * time.Hour))
	if _, err := store.CreateRun(context.Background(), apimodel.Run{
		ID:          sessionID,
		WorkspaceID: "ws_inspect",
		Status:      apimodel.RunIdle,
		Trigger:     apimodel.TriggerSchedule,
		StartedAt:   apimodel.FormatISO(time.Now().UTC().Add(-23*time.Hour - time.Minute)),
		EndedAt:     endedLongAgo,
	}); err != nil {
		t.Fatalf("造 run 失败: %v", err)
	}

	restore := workRunCheckInterval
	workRunCheckInterval = 10 * time.Millisecond
	defer func() { workRunCheckInterval = restore }()

	pool := &WorkerPool{svc: svc, cfg: WorkerConfig{Concurrency: 1}, log: testLogger()}
	installFakeProcessor(pool)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go pool.watchRunTerminated(ctx, sessionID, cancel, testLogger())

	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("run 早就答完一轮并结算了（idle + endedAt 在 23 小时前），\n" +
			"这条 work 却没被叫停——它会在 SDK 的轮询路径里每 3 秒打一次 ListEvents，\n" +
			"把账号 RPM 打满，且**不会自愈**（装弹要靠读到新事件，而这条 run 不会再有）。\n" +
			"只盯 terminated 兜不住这个形状：这些 run 的状态是正常的，\n" +
			"没有任何一处会把它们推成 terminated。判据见 core.idleRunAbandoned")
	}
}

// TestIdleRunAbandonedBoundary pins the three boundaries of the rule itself directly.
//
// The two tests above go through the full goroutine chain (slow enough that they can only check a few representative points);
// this one takes the pure function out to check boundaries, **one point on each side of the threshold**, to catch reversed errors like writing `>=` as `<=`
// (which would still be all green with tests that only check "long ago" and "just now").
func TestIdleRunAbandonedBoundary(t *testing.T) {
	now := time.Now().UTC()
	iso := func(d time.Duration) string { return apimodel.FormatISO(now.Add(d)) }

	cases := []struct {
		name    string
		status  apimodel.RunStatus
		endedAt string
		want    bool
		why     string
	}{
		{"刚过阈值一分钟", apimodel.RunIdle, iso(-workIdleRunGrace - time.Minute), true,
			"超过阈值就该判僵尸，否则线上那批永远收不掉"},
		{"差一分钟到阈值", apimodel.RunIdle, iso(-workIdleRunGrace + time.Minute), false,
			"没到阈值不能动——这一侧误判是把用户的对话掐断"},
		{"endedAt 为空", apimodel.RunIdle, "", false,
			"没结算过就谈不上「早就答完了」，老数据与结算帧未到都是这个形状"},
		{"endedAt 形状怪（解析不出来）", apimodel.RunIdle, "2026/09/04 18:30", false,
			"解析失败必须默认不杀：误杀掐断对话，漏杀只是多烧一会儿配额"},
		{"terminated 由既有判据管", apimodel.RunTerminated, iso(-24 * time.Hour), false,
			"这个函数只管 idle 那一支，terminated 走 status 判据"},
		{"requires_action 再老也不归它管", apimodel.RunRequiresAction, iso(-24 * time.Hour), false,
			"卡住的会话由 reapStaleRuns 收，这里插一脚会把等工具结果的腰斩"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := apimodel.Run{ID: "sesn-x", Status: tc.status, EndedAt: tc.endedAt}
			if got := idleRunAbandoned(run, now); got != tc.want {
				t.Errorf("idleRunAbandoned(status=%q, endedAt=%q) = %v，应为 %v —— %s",
					tc.status, tc.endedAt, got, tc.want, tc.why)
			}
		})
	}
}

// TestWorkIdleRunGraceBounds pins the lower and upper bounds of workIdleRunGrace (see its comment for the chosen value).
//
// Lower bound: it must cover the SDK's MaxIdle follow-up window plus one check interval, otherwise it would stop normal work
// before the SDK exits on its own.
//
// Upper bound: it must be clearly less than the container TTL. After stopping, the container is paused; past the TTL the platform has already reclaimed it,
// and the pause always gets 400 `session is not resumable`. It used to be 2 hours, and every production hit arrived just after
// the container was already gone (measured 19/19 on 2026-10-13). Half the TTL is left as margin because containers in the warm pool have
// already lived part of their life before being taken.
func TestWorkIdleRunGraceBounds(t *testing.T) {
	// Container TTL.
	const containerTTL = time.Hour

	if floor := selfhosted.DefaultMaxIdle + workRunCheckInterval; workIdleRunGrace <= floor {
		t.Errorf("workIdleRunGrace = %v，必须大于 MaxIdle + 检查间隔（%v），"+
			"否则会抢在 SDK 自己退出之前掐掉正常续聊的 work", workIdleRunGrace, floor)
	}
	if workIdleRunGrace > containerTTL/2 {
		t.Errorf("workIdleRunGrace = %v，必须不超过容器 TTL 的一半（%v）："+
			"叫停之后要暂停容器，晚于 TTL 暂停必然失败（session is not resumable），"+
			"容器白白按运行态计费到被回收", workIdleRunGrace, containerTTL/2)
	}
}
