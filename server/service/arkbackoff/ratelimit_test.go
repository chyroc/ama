// Tests for rate-limit backoff: a 429 must back off, other errors are unaffected.
//
// These tests do not touch the real Ark: they drive the gate logic with a controlled
// selfhosted.API stand-in.
//
// # ⚠️ Why these are here, while the "wiring" one stays in core
//
// This file checks **whether the gate itself is right** (does backoff grow, does it reset, is it
// shared),
// and three of them read Gate's unexported fields (backoff / until), which only the same package can
// do.
//
// While `TestPollLoopWiresRateLimitGateIntoWorkAPI` checks **whether the gate is really wired onto
// the API that works use**; it needs `WorkerPool.pollLoop`, so it stays in core.
// That one cannot be dropped: remove the WrapAPI layer in pollLoop, and the tests in this file **all
// stay green**:
// they build the wrapper directly, proving "the gate itself is right", not "the gate is wired in".
// See the file header of core/workerratelimit_test.go.
//
// # ⚠️ When changing tests here, run mutation testing first; do not just look at "all green"
//
// As measured during the P1 fix: tests checking only middle levels **missed the real failing
// level**:
// changing the step-down lower bound from `RateLimitStart` to `0` left the whole package all green,
// while that mutation exactly
// recreated the fault itself. Five mutations and the tests each one should turn red:
//
//	observeSuccess back to `backoff = 0`       → DoesNotResetWhileStillRateLimited + ClimbsDespite…
//	step-down lower bound written as 0         → DoesNotResetWhileStillRateLimited (only the
//	                                             starting-level subtest catches it)
//	delete the HealthyAfter branch             → ResetsAfterHealthyPeriod
//	also wipe until when stepping down         → DoesNotResetWhileStillRateLimited
//	do not record lastHit (healthy check broken) → ResetsAfterHealthyPeriod
package arkbackoff

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// rateLimitProbeAPI records the time of each ListEvents / SendEvent, and returns errors per a
// script.
//
// Only these two methods are implemented; the rest come from embedding selfhosted.API (nil, never
// called in tests;
// calling one would panic, which is the right reaction to "the test touched a path it should not",
// and must not be swallowed silently).
type rateLimitProbeAPI struct {
	selfhosted.API

	mu sync.Mutex
	// listErrs is returned in call order; once used up it returns nil (success).
	listErrs []error
	listN    int
	// listAt is the entry time of each ListEvents, used to measure the gap between two calls.
	listAt []time.Time
	// sendErrs / sendAt likewise, for SendEvent.
	sendErrs []error
	sendN    int
	sendAt   []time.Time
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
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sendAt = append(p.sendAt, time.Now())
	if p.sendN < len(p.sendErrs) {
		err := p.sendErrs[p.sendN]
		p.sendN++
		return err
	}
	return nil
}

func (p *rateLimitProbeAPI) listGaps() []time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []time.Duration
	for i := 1; i < len(p.listAt); i++ {
		out = append(out, p.listAt[i].Sub(p.listAt[i-1]))
	}
	return out
}

// arkRateLimitErr builds a 429 with the real shape.
//
// **It must be a *selfhosted.APIError**: the gate uses selfhosted.IsStatus to detect rate limits,
// which goes through
// errors.As. A string stand-in like errors.New("429") would silently break the whole chain: tests all
// green,
// production still storming. The real error's shape is `worker api status 429: The RPM ... exceeded`
// (APIError.Error in types.go).
func arkRateLimitErr() error {
	return &selfhosted.APIError{
		StatusCode: http.StatusTooManyRequests,
		Message:    "The RPM (Requests Per Minute) limit for the API on your account has been exceeded",
		RequestID:  "req-ratelimit-probe",
	}
}

// TestRateLimitedAPIBacksOffOnTooManyRequests guards against that storm of 6087 429s.
//
// # The real fault
//
// In a local run, 15 concurrent works produced 6087 instances of
// `worker api status 429: The RPM ... exceeded`, steady at ~400 requests/minute,
// stopping only when the process exited. In the SDK's fallback consumeList loop, a failed ListEvents
// only logs a Warn and breaks, then goes back to the fixed ticker: **zero backoff**, so the rate
// limit was amplified into a storm.
//
// # The check is "the gap", not "was there an error"
//
// Assert that the second call came at least one backoff start after the first. Asserting only "an
// error was returned" is
// always true (errors pass through anyway); such an assertion stays green with backoff deleted.
func TestRateLimitedAPIBacksOffOnTooManyRequests(t *testing.T) {
	api := &rateLimitProbeAPI{listErrs: []error{arkRateLimitErr()}}
	wrapped := WrapAPI(api, NewGate(testLogger()))

	ctx := context.Background()
	// The first call hits 429: the error must pass through as is (the upper layer must classify it),
	// and the gate closes.
	if _, err := wrapped.ListEvents(ctx, selfhosted.ListEventsRequest{}); err == nil {
		t.Fatal("撞了 429 却没把错误透传上去——上层的错误分类会看不到限流")
	}
	// The second call should be held by the gate for a while before going out.
	if _, err := wrapped.ListEvents(ctx, selfhosted.ListEventsRequest{}); err != nil {
		t.Fatalf("退避之后那次不该失败：%v", err)
	}

	gaps := api.listGaps()
	if len(gaps) != 1 {
		t.Fatalf("要恰好两次调用（1 个间隔），实际 %d 个间隔", len(gaps))
	}
	// jitter takes [d/2, d), so the lower bound is half the start.
	min := RateLimitStart / 2
	if gaps[0] < min {
		t.Errorf("撞 429 之后隔了 %v 就重试，小于退避下界 %v——\n"+
			"零退避会把限流放大成风暴：实测 15 条并发 work 在 15 分钟里"+
			"打出 6087 条 429，稳定 ~400 请求/分钟。\n"+
			"退避见 arkbackoff.Gate", gaps[0], min)
	}
}

// TestRateLimitedAPIDoesNotBackOffOnOtherErrors is the counterexample of the one above, guarding
// against backing off too much.
//
// ⚠️ This is not an extra, it is **required**: backing off only on rate limits is a deliberate
// tradeoff. The 4xx classification in arkFatalStatus
// requires "4xx other than timeout/conflict/rate limit are config problems; retries do not help;
// surface to the upper layer to reconnect".
// If the gate backed off on all errors, those errors that should surface at once would be held for
// some seconds first,
// with the symptom "a wrong envID takes a long time to report an error", and the error not pointing
// at backoff.
//
// Checked once each with three non-rate-limit errors: a network error, a 5xx, and **a 4xx that is
// not 429**.
func TestRateLimitedAPIDoesNotBackOffOnOtherErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"网络错误", errors.New("dial tcp: connection refused")},
		{"服务端错误", &selfhosted.APIError{StatusCode: http.StatusBadGateway, Message: "upstream unavailable"}},
		{"非限流的 4xx", &selfhosted.APIError{StatusCode: http.StatusForbidden, Message: "key has no permission"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &rateLimitProbeAPI{listErrs: []error{tc.err}}
			wrapped := WrapAPI(api, NewGate(testLogger()))

			ctx := context.Background()
			if _, err := wrapped.ListEvents(ctx, selfhosted.ListEventsRequest{}); err == nil {
				t.Fatal("错误没有原样透传")
			}
			if _, err := wrapped.ListEvents(ctx, selfhosted.ListEventsRequest{}); err != nil {
				t.Fatalf("第二次不该失败：%v", err)
			}

			gaps := api.listGaps()
			if len(gaps) != 1 {
				t.Fatalf("要恰好两次调用，实际 %d 个间隔", len(gaps))
			}
			// The upper bound is half the backoff lower bound: non-rate-limit errors must retry at
			// once, not be held.
			// The wide margin is for scheduling jitter: a real backoff would be over 250ms, an order of
			// magnitude apart.
			if limit := RateLimitStart / 4; gaps[0] >= limit {
				t.Errorf("%s 也被退避了（隔了 %v，超过 %v）——\n"+
					"只有限流该退避。其它 4xx 是配置问题（环境不存在、key 无权限），"+
					"压住它们会让「打错 envID」很久才报错，且报错不指向退避。\n"+
					"分类见 arkbackoff.Gate.Observe", tc.name, gaps[0], limit)
			}
		})
	}
}

// TestRateLimitGateIsSharedAcrossConcurrentWorks pins the key point "shared per environment".
//
// # Why sharing is essential, not an implementation detail
//
// The rate limit is **account-level**. Each runner backing off on its own looks polite, but N
// staggered retries still add up to
// one steady high-frequency stream: 15 each backing off to once per second still add up to 900
// requests/minute,
// still hitting RPM. So when one runner hits a 429, the other runners in the same environment must
// wait together.
//
// Check: let A hit one 429, then see whether B, **which never hit one**, is held.
// Changing the gate to one per work would surely turn this red.
func TestRateLimitGateIsSharedAcrossConcurrentWorks(t *testing.T) {
	gate := NewGate(testLogger())

	// A and B are two concurrent works in the same environment, each with its own API stand-in,
	// sharing one gate.
	apiA := &rateLimitProbeAPI{listErrs: []error{arkRateLimitErr()}}
	apiB := &rateLimitProbeAPI{}
	workA := WrapAPI(apiA, gate)
	workB := WrapAPI(apiB, gate)

	ctx := context.Background()
	// A hits the rate limit, the gate closes.
	if _, err := workA.ListEvents(ctx, selfhosted.ListEventsRequest{}); err == nil {
		t.Fatal("A 撞 429 却没透传错误")
	}

	// B never hit it, but it should wait too: the account quota is shared.
	start := time.Now()
	if _, err := workB.ListEvents(ctx, selfhosted.ListEventsRequest{}); err != nil {
		t.Fatalf("B 不该失败：%v", err)
	}
	waited := time.Since(start)

	if min := RateLimitStart / 2; waited < min {
		t.Errorf("另一条 work 只等了 %v（下界 %v）——\n"+
			"gate 没有按环境共享。限流是账号级的：每条 runner 各退各的，"+
			"15 条错开重试合起来仍是 ~900 请求/分钟，照样撞 RPM。\n"+
			"gate 建在 pollLoop 里（每个 environment 一个），见 core.pollLoop", waited, min)
	}
}

// rewindGate moves the gate's time state back by d, to simulate "d has passed" without really
// sleeping.
//
// Both times move together, because real time passes for both together: moving only lastHit would
// create a
// state "quiet for 30 seconds, but the backoff window has not expired", impossible under a real
// clock
// (the window is at most 10 seconds); using it as the check would test a branch that does not exist.
func rewindGate(g *Gate, d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.lastHit.IsZero() {
		g.lastHit = g.lastHit.Add(-d)
	}
	if !g.until.IsZero() {
		g.until = g.until.Add(-d)
	}
}

// TestRateLimitGateResetsAfterHealthyPeriod: after a full healthy period of quiet, one success must
// reset fully.
//
// # ⚠️ This was changed from TestRateLimitGateResetsAfterSuccess; the reasons must be kept
//
// The old one asserted "**one** success resets backoff to zero and opens the gate", and
// **that was exactly the cause of the production P1**: 9 runners shared one gate, and any one
// success
// lifted the backoff for the other 8, so the backoff could never climb; in production 73.1% of
// waits stayed at
// the 250~500ms starting level, only 0.6% ever reached the 10-second cap, and 429s went on all day.
// See the comment on Gate.Observe. So this test **had to change**: it pinned the fault itself.
//
// # But what it originally worried about is right, and is kept as is
//
// The old comment said "without resetting, one occasional rate limit would make this environment
// slow **forever** (backoff stuck at
// the 10-second cap), showing up as 'the conversation gets slower the more it is used', with no
// error pointing at backoff"; that worry holds,
// and cannot be dropped just to fix the one above.
//
// What changed is only "how fast counts as recovered": from "recover at once on one success" to "the
// first success after **a full
// RateLimitHealthyAfter (30s)** of quiet recovers". Occasional rate limits still fully recover,
// just one healthy period later; continuous rate limiting can no longer earn a pardon.
// "No reset during the healthy period" is pinned by TestRateLimitGateDoesNotResetWhileStillRateLimited,
// and only the two together are the complete tradeoff.
func TestRateLimitGateResetsAfterHealthyPeriod(t *testing.T) {
	gate := NewGate(testLogger())
	gate.Observe(arkRateLimitErr())

	gate.mu.Lock()
	armed := gate.backoff
	gate.mu.Unlock()
	if armed <= 0 {
		t.Fatalf("撞 429 之后 backoff 该被置上，实际 %v", armed)
	}

	// Simulate "a healthy period of quiet after the rate limit": that is the shape of an occasional
	// rate limit: hit once, then gone.
	rewindGate(gate, RateLimitHealthyAfter+time.Second)
	gate.Observe(nil)

	gate.mu.Lock()
	after := gate.backoff
	until := gate.until
	gate.mu.Unlock()
	if after != 0 {
		t.Errorf("安静满 %v 之后 backoff 没复位（还是 %v）——"+
			"一次偶发限流会让这个环境永久停在退避上限，表现是「对话越用越卡」，"+
			"且没有任何报错指向退避", RateLimitHealthyAfter, after)
	}
	if !until.IsZero() {
		t.Errorf("安静满健康期之后闸门没打开（until=%v）", until)
	}

	// After the reset, calls should not be held.
	api := &rateLimitProbeAPI{}
	wrapped := WrapAPI(api, gate)
	start := time.Now()
	if _, err := wrapped.ListEvents(context.Background(), selfhosted.ListEventsRequest{}); err != nil {
		t.Fatalf("复位之后不该失败：%v", err)
	}
	if waited := time.Since(start); waited >= RateLimitStart/4 {
		t.Errorf("复位之后仍被拦了 %v", waited)
	}
}

// TestRateLimitGateDoesNotResetWhileStillRateLimited: a success during the healthy period can only
// step down one level, not reset.
//
// # This pins exactly that production P1
//
// Ark's RPM limit is a leaky bucket; **429 is never a 100% hit**: some requests always squeeze in.
// With a shared gate, "any one success resets fully" thus becomes a stable self-exciting loop:
// 8 hit 429 and raise the ladder, the 9th succeeds once and pushes it back to zero, and the next
// round starts over.
// The production fingerprint was the wait distribution: 73.1% at the starting level, only 0.6% at
// the cap,
// while exponential backoff should "wait longer the more it hits, quickly reaching the cap".
//
// The check picks "**which level backoff stays at after a success**" rather than "how long it
// waited":
// the latter would need really sleeping, and widened by jitter it cannot tell "stepped down one
// level" from "reset".
//
// # ⚠️ Why every level must be checked, rather than picking one middle level
//
// This first checked only the 2s level, and **mutation testing found the gap on the spot**: changing
// the step-down lower bound from
// `RateLimitStart` to `0` (one success at the starting level goes to zero), and the whole package
// **stayed all green**.
// And that mutation exactly recreated the fault itself: 73% of production waits stayed at the
// starting level,
// meaning the real back-and-forth **only happened on the lowest notch**, which no amount of careful
// middle-level checks can catch.
//
// So the starting level is a **must-check** level: it is the only level where "halving drops below
// the lower bound",
// and the only level directly pointed at by production data.
func TestRateLimitGateDoesNotResetWhileStillRateLimited(t *testing.T) {
	cases := []struct {
		name string
		hits int
		want time.Duration // which level it is expected to stay at "after one success"
	}{
		// ⚠️ This level was added by mutation testing; do not delete it: it is the only test that
		// catches "lower bound written as 0".
		{"起点档（线上 73% 的等待停在这里）", 1, RateLimitStart},
		{"中间档", 3, time.Second},
		{"封顶档", 8, RateLimitCap / 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := NewGate(testLogger())
			for i := 0; i < tc.hits; i++ {
				gate.Observe(arkRateLimitErr())
			}
			gate.mu.Lock()
			climbed := gate.backoff
			gate.mu.Unlock()

			// Right after, one success: rate limiting is still ongoing (no quiet period), so it can only
			// step down one level.
			gate.Observe(nil)

			gate.mu.Lock()
			after := gate.backoff
			until := gate.until
			gate.mu.Unlock()

			if after == 0 {
				t.Fatalf("持续限流期间一次成功就把退避清零了（%v → 0）——\n"+
					"这正是 2026-09-05 线上 P1 的成因：9 条 runner 共享一个闸门，"+
					"任意一条成功就替另外 8 条解除退避，而 429 从来不是 100%% 命中，"+
					"于是退避永远爬不上去（线上 73%% 的等待停在 500ms 起点档，"+
					"只有 0.6%% 到过 10 秒封顶）。\n"+
					"成功该退一档、且不低于 %v，清零只能靠安静满 %v。\n"+
					"见 Gate.observeSuccess", climbed, RateLimitStart, RateLimitHealthyAfter)
			}
			if after != tc.want {
				t.Errorf("%v 档上一次成功之后停在 %v，期望 %v——\n"+
					"成功该**减半**（下界是起点 %v）：退多了等于恢复旧的全量复位，"+
					"退少了则偶发限流迟迟降不下来", climbed, after, tc.want, RateLimitStart)
			}
			// An already scheduled wait window should not be wiped early by one success: that would make
			// the other runners in Wait
			// wake at once and hit together, exactly the spike being treated.
			if until.IsZero() {
				t.Error("成功把退避窗口整个抹掉了——同环境正在等的 runner 会立刻一起醒来重打")
			}
		})
	}
}

// TestRateLimitGateClimbsDespiteInterleavedSuccesses is the **scenario-level** check for this fix.
//
// The two above check single-step transitions; this one directly recreates the production shape:
// **one environment with 9 runners sharing
// a gate, each round 8 hit 429 and 1 squeezes in successfully** (that is normal under leaky-bucket
// rate limiting).
// The requirement is that after a few rounds the backoff really reaches high levels: "under
// continuous rate limiting the ladder must be able to climb".
//
// # Two checks guard against two kinds of degradation; neither can be dropped
//
//   - **The peak (after 8 429s) must reach the cap**: guards against "the ladder cannot grow at all".
//   - **The trough (after that success) must not be below half the cap**: guards exactly against
//     this bug.
//     ⚠️ Looking only at the peak **cannot catch** the old implementation: the old code started from
//     0 each round, and 8 429s in a row
//     (500ms→1s→2s→4s→8s→10s) still touched the cap, with an identical peak.
//     The difference is only in the trough: the old implementation gives **0**, the new one **5s**.
//
// ⚠️ The steady state is "peak 10s / trough 5s" rather than "stable at 10s"; that is the normal shape
// of the halving design,
// not an incomplete fix: doubling on hits and halving on successes lets the ladder find its own
// balance at the quota boundary:
// climbing while still over quota, coming down once below it. Compared with the 500ms during the
// production fault,
// request volume on this path drops to about 1/10~1/20.
func TestRateLimitGateClimbsDespiteInterleavedSuccesses(t *testing.T) {
	gate := NewGate(testLogger())

	const runners = 9
	var peaks, troughs []time.Duration
	for round := 0; round < 3; round++ {
		for i := 0; i < runners-1; i++ {
			gate.Observe(arkRateLimitErr())
		}
		gate.mu.Lock()
		peaks = append(peaks, gate.backoff)
		gate.mu.Unlock()

		// The 9th squeezed in. The old implementation reset the whole ladder at this step.
		gate.Observe(nil)

		gate.mu.Lock()
		troughs = append(troughs, gate.backoff)
		// Open the window by hand so the test does not really sleep (only affects until, not the
		// ladder).
		gate.until = time.Time{}
		gate.mu.Unlock()
	}

	if peak := peaks[len(peaks)-1]; peak < RateLimitCap {
		t.Errorf("每轮 %d 次 429 之后退避只到 %v，没压到封顶 %v——阶梯涨不动。\n"+
			"各轮峰值：%v", runners-1, peak, RateLimitCap, peaks)
	}
	if trough := troughs[len(troughs)-1]; trough < RateLimitCap/2 {
		t.Errorf("%d 条 runner 每轮 %d 次 429 + 1 次成功，跑了 %d 轮，"+
			"那次成功把退避打回 %v（不该低于 %v）——\n"+
			"各轮峰值：%v\n各轮谷值：%v\n"+
			"这正是 2026-09-05 线上 P1 的形状：共享闸门「共享了惩罚，也共享了赦免」，"+
			"而赦免只需要 9 条里的任意一条。旧实现这里是 0，于是退避永远爬不上去，"+
			"429 打满一整天（48 次/分钟，中位间隔 3 秒，73%% 的等待停在 500ms 起点档）。\n"+
			"见 Gate.Observe 的「成功为什么是退一档而不是清零」",
			runners, runners-1, len(troughs), trough, RateLimitCap/2, peaks, troughs)
	}
}

// TestRateLimitGateGrowsExponentially: consecutive rate-limit hits must wait longer and longer.
//
// Fixed-interval backoff is still a steady request stream under continuous rate limiting; only
// exponential growth really brings volume down.
func TestRateLimitGateGrowsExponentially(t *testing.T) {
	gate := NewGate(testLogger())

	var seen []time.Duration
	for i := 0; i < 3; i++ {
		gate.Observe(arkRateLimitErr())
		gate.mu.Lock()
		seen = append(seen, gate.backoff)
		// Open the gate by hand so the test does not really sleep.
		gate.until = time.Time{}
		gate.mu.Unlock()
	}

	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("第 %d 次退避 %v 没比上一次 %v 长——"+
				"固定间隔在持续限流下仍是一条稳定的高频流，压不下来。\n"+
				"实际序列：%v", i+1, seen[i], seen[i-1], seen)
		}
	}
	if last := seen[len(seen)-1]; last > RateLimitCap {
		t.Errorf("退避 %v 超过上限 %v——无上限增长会让恢复变得极慢", last, RateLimitCap)
	}
}

// TestRateLimitGateWaitHonorsContext: when ctx is cancelled the gate must let through at once.
//
// Otherwise process exit would be held up by backoff: the SDK's defer still has StopWork/container
// pause to do,
// and being stuck here would turn "stop the service" into "wait ten seconds before stopping".
func TestRateLimitGateWaitHonorsContext(t *testing.T) {
	gate := NewGate(testLogger())
	// Close the gate for far longer than the test runs.
	gate.mu.Lock()
	gate.until = time.Now().Add(time.Hour)
	gate.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan bool, 1)
	go func() { done <- gate.Wait(ctx) }()

	select {
	case ok := <-done:
		if ok {
			t.Error("ctx 已取消，Wait 该返回 false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消了 Wait 还在等——进程退出会被退避拖住，" +
			"SDK defer 里的 StopWork 与容器暂停都做不完")
	}
}
