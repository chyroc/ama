// The **wiring** test for rate-limit backoff: is the gate really attached to the API that work uses.
//
// # ⚠️ Why only this one is left here, and the other six are in service/arkbackoff
//
// The gate's own behavior (does the backoff grow, does it reset, is it shared, does ctx cancel let
// it through) moved with the implementation to `service/arkbackoff/ratelimit_test.go`; three of
// those tests read unexported fields of Gate, which only the same package can do.
//
// **This one can't move with them**, because it needs `WorkerPool.pollLoop`. And it can't be
// dropped either: those six check "the gate itself is correct", and if you remove the
// newRateLimitedAPI layer in pollLoop, they **all stay green**. That is exactly the case this repo
// has noted before: "a purely new module can be all green and still not take effect".
package core

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

// rateLimitProbeAPI records the time of every ListEvents / SendEvent and returns errors from a script.
//
// Only these two methods are implemented; the rest come from embedding selfhosted.API (the value is
// nil and the test never calls them; calling one panics, which is the right reaction to "the test
// touched a path it shouldn't" and should not be swallowed silently).
type rateLimitProbeAPI struct {
	selfhosted.API

	mu sync.Mutex
	// listErrs are returned in call order; once used up, nil (success) is returned.
	listErrs []error
	listN    int
	// listAt is the entry time of each ListEvents, used to measure the gap between two calls.
	listAt []time.Time
}

func (p *rateLimitProbeAPI) ListEvents(_ context.Context, _ selfhosted.ListEventsRequest) (*selfhosted.ListEventsResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listAt = append(p.listAt, time.Now())
	if p.listN < len(p.listErrs) {
		err := p.listErrs[p.listN]
		p.listN++
		if err != nil {
			return nil, err
		}
	}
	return &selfhosted.ListEventsResponse{}, nil
}

func (p *rateLimitProbeAPI) SendEvent(_ context.Context, _ selfhosted.SendEventRequest) error {
	return nil
}

// arkRateLimitErr builds a 429 with the real shape.
//
// **It must be a *selfhosted.APIError**: the gate uses selfhosted.IsStatus to detect rate limiting,
// and that uses errors.As. A string stand-in like errors.New("429") makes the whole chain silently
// stop working: tests all green, while production still gets the storm.
func arkRateLimitErr() error {
	return &selfhosted.APIError{
		StatusCode: http.StatusTooManyRequests,
		Message:    "The RPM (Requests Per Minute) limit for the API on your account has been exceeded",
		RequestID:  "req-ratelimit-probe",
	}
}

// TestPollLoopWiresRateLimitGateIntoWorkAPI pins the **wiring**, not a function.
//
// ⚠️ This test was forced by mutation testing: remove the newRateLimitedAPI layer in pollLoop
// (`workAPI := newShutdownSafeAPI(...)`), and the tests in service/arkbackoff **all stay green**.
// They build the wrapper directly, which proves "the gate itself is correct", not "the gate is
// really attached to the API that work uses". That is exactly the case this repo has noted before:
// "a purely new module can be all green and still not take effect": the feature doesn't work, with
// zero errors.
//
// Check: take the API that pollLoop **actually hands to the worker factory**, make it hit one 429,
// then see whether the second call is held back. Without the wiring this test must fail.
//
// # ⚠️ This test used to be flaky (measured baseline: 6/12 failures); don't revert the fix
//
// Symptom: occasional "hit 429 but the error was not passed through". The cause is **not** a problem
// in the code under test; the test itself had a race: `listErrs` was loaded with that one 429 at
// construction time, and inside `HandleItem` (the SDK's session-tool-runner) **ListEvents is also
// called**. That internal call and the explicit call in the test below fight over the same 429; when
// the internal call takes it, the explicit call gets nil, and the test fails with "the error was not
// passed through".
//
// The fix is to **load the 429 only after pollLoop has finished**: by then HandleItem is done, no one
// else will take it, and the explicit call is sure to get it. What is checked does not change at all
// (still "is the API handed down by pollLoop wired to the gate"); only the race is gone.
//
// ⚠️ Don't change it back to "load at construction time": that would turn this required CI gate back
// into a coin toss, and a flaky test is worse than no test: it trains people to ignore red.
func TestPollLoopWiresRateLimitGateIntoWorkAPI(t *testing.T) {
	// The probe does **not** load the error yet: HandleItem also calls ListEvents, and would take it now.
	probe := &rateLimitProbeAPI{}
	// The outer layer also has to drive pollLoop: borrow fakeWorkAPI to hand out one work,
	// but replace ListEvents/SendEvent with the probe's implementation.
	driver := &rateLimitDriverAPI{
		fakeWorkAPI: fakeWorkAPI{queue: []selfhosted.WorkItem{workItem("work_1", "sesn-ratelimit-wiring")}},
		probe:       probe,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 4}, log: testLogger()}

	// Capture the API handed down by pollLoop. **It must be used**: capturing the outer driver
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
		t.Fatal("pollLoop 没把 API 交给 worker 工厂")
	}
	cancel()
	<-done

	// pollLoop has finished, so all the ListEvents inside HandleItem are done. Only **now** load
	// that 429, so it can only be taken by the explicit call below. See the flaky note above.
	probe.mu.Lock()
	probe.listErrs = []error{arkRateLimitErr()}
	probe.listN = 0
	probe.mu.Unlock()

	// Now check it: the first call hits 429, the second should be held back by the gate.
	callCtx := context.Background()
	if _, err := workAPI.ListEvents(callCtx, selfhosted.ListEventsRequest{}); err == nil {
		t.Fatal("撞 429 却没透传错误")
	}
	start := time.Now()
	if _, err := workAPI.ListEvents(callCtx, selfhosted.ListEventsRequest{}); err != nil {
		t.Fatalf("退避之后那次不该失败：%v", err)
	}
	waited := time.Since(start)

	if min := arkRateLimitBackoffStart / 2; waited < min {
		t.Errorf("work 用的那个 API 撞 429 后只隔了 %v 就重试（下界 %v）——\n"+
			"限流退避没接到 pollLoop 上：闸门自己是对的，但 work 拿到的 API 没包它，\n"+
			"于是线上照旧零退避、6087 条 429 那场风暴照旧。\n"+
			"接线在 core.pollLoop 的 workAPI 那一行（newRateLimitedAPI）", waited, min)
	}
}

// rateLimitDriverAPI lets pollLoop run (handing out work, ack, heartbeat go through fakeWorkAPI),
// but forwards ListEvents / SendEvent to the probe, so the backoff gap can be measured.
//
// **fakeWorkAPI alone won't do**: its ListEvents always succeeds, so no backoff can be measured;
// the probe alone won't do either: it has no PollWork/AckWork, so pollLoop can't run.
type rateLimitDriverAPI struct {
	fakeWorkAPI
	probe *rateLimitProbeAPI
}

func (d *rateLimitDriverAPI) ListEvents(ctx context.Context, req selfhosted.ListEventsRequest) (*selfhosted.ListEventsResponse, error) {
	return d.probe.ListEvents(ctx, req)
}

func (d *rateLimitDriverAPI) SendEvent(ctx context.Context, req selfhosted.SendEventRequest) error {
	return d.probe.SendEvent(ctx, req)
}
