// Tests for poll leader election, stepped backoff, and event wakeups.
//
// These tests **must be read as a group**. Any single one of them can be fooled by an
// implementation that is wrong in the opposite direction:
//
//	only ① (one pod polls)                  → "never poll" also satisfies it
//	only ② (takeover after a crash)         → "every pod polls" also satisfies it
//	only ③ (interval climbs step by step to 60s) → "once up it never comes back" also satisfies it
//	only ④ (getting work returns to fastest) → "always stay fastest" also satisfies it
//	only ⑤ (a wakeup wakes it)              → "fire one poll per signal" also satisfies it
//	only ⑥ (repeated wakeups are merged)    → "wakeups have no effect at all" also satisfies it
//
// The paired tests block each other's degenerate implementations, which is why they are written
// separately instead of merged into one.
package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// sharedLeases is a fake lock **shared across "pods"**, used to simulate Redis.
//
// The key is that, like a real lock, it lets only one holder succeed, and supports "the lock
// expires automatically after the holder dies suddenly", which is exactly what ② verifies.
type sharedLeases struct {
	mu sync.Mutex
	// held is the current holder's token; empty means nobody holds it.
	held map[string]string
	// dead records which tokens have "died suddenly" (no more renewals, and renewals always fail).
	dead map[string]bool
	// acquires records how many times each key was grabbed, to observe locking behavior.
	acquires map[string]int
	// When err is non-nil, Acquire always fails; used to verify degrade-open.
	err error
}

func newSharedLeases() *sharedLeases {
	return &sharedLeases{
		held:     map[string]string{},
		dead:     map[string]bool{},
		acquires: map[string]int{},
	}
}

func (l *sharedLeases) Acquire(_ context.Context, key string, _ time.Duration) (Lease, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.acquires[key]++
	if l.err != nil {
		return nil, false, l.err
	}
	if _, taken := l.held[key]; taken {
		return nil, false, nil
	}
	token := key + "#" + time.Now().Format(time.RFC3339Nano)
	l.held[key] = token
	return &sharedLease{parent: l, key: key, token: token}, true, nil
}

// expire makes the current holder's lock "expire", simulating the holder pod being kill -9'd:
// it never calls Release; the lock is just taken by someone else after it expires.
func (l *sharedLeases) expire(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if token, ok := l.held[key]; ok {
		l.dead[token] = true
		delete(l.held, key)
	}
}

func (l *sharedLeases) acquireCount(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.acquires[key]
}

type sharedLease struct {
	parent *sharedLeases
	key    string
	token  string
}

func (s *sharedLease) Renew(_ context.Context, _ time.Duration) (bool, error) {
	s.parent.mu.Lock()
	defer s.parent.mu.Unlock()
	if s.parent.dead[s.token] {
		// The lock has already been taken by someone else; this is what real Redis returns when
		// the Lua script's value comparison fails.
		return false, nil
	}
	return s.parent.held[s.key] == s.token, nil
}

func (s *sharedLease) Release(_ context.Context) error {
	s.parent.mu.Lock()
	defer s.parent.mu.Unlock()
	if s.parent.held[s.key] == s.token {
		delete(s.parent.held, s.key)
	}
	return nil
}

// countingPollAPI records the time of every PollWork; the queue is always empty.
//
// Same shape as idlePollAPI, but it also carries a "which pod is this" label, to verify "only
// one is polling".
type countingPollAPI struct {
	fakeWorkAPI

	mu    sync.Mutex
	calls []time.Time
}

func (a *countingPollAPI) PollWork(_ context.Context, _ selfhosted.PollWorkRequest) (*selfhosted.WorkItem, error) {
	a.mu.Lock()
	a.calls = append(a.calls, time.Now())
	a.mu.Unlock()
	return nil, nil
}

func (a *countingPollAPI) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

// fakeWakeups is the test double for WorkerWakeups, **shared across "pods"**, which is exactly
// the point under test: the one sending the signal and the one receiving it are usually not the
// same pod.
type fakeWakeups struct {
	mu sync.Mutex
	// subs are the subscriber callbacks on each env.
	subs map[string][]func(context.Context) error
	// woke records how many times each env was woken (sending side).
	woke map[string]int
	err  error
}

func newFakeWakeups() *fakeWakeups {
	return &fakeWakeups{
		subs: map[string][]func(context.Context) error{},
		woke: map[string]int{},
	}
}

func (f *fakeWakeups) Wake(ctx context.Context, envID string) error {
	f.mu.Lock()
	if f.err != nil {
		err := f.err
		f.mu.Unlock()
		return err
	}
	f.woke[envID]++
	subs := append([]func(context.Context) error(nil), f.subs[envID]...)
	f.mu.Unlock()
	for _, fn := range subs {
		_ = fn(ctx)
	}
	return nil
}

func (f *fakeWakeups) WatchWakeups(ctx context.Context, envID string, fn func(context.Context) error) error {
	f.mu.Lock()
	f.subs[envID] = append(f.subs[envID], fn)
	f.mu.Unlock()
	<-ctx.Done()
	return nil
}

// total is the total number of times all environments were woken, used as the check in "should
// it wake up" tests.
func (f *fakeWakeups) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.woke {
		n += c
	}
	return n
}

func (f *fakeWakeups) subscribed(envID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs[envID]) > 0
}

// leaseTestPool builds a WorkerPool configured only for leader election / wakeups, with
// durations shortened to milliseconds.
//
// ⚠️ **Durations must be injectable**: the real values are 20s/5s/5s, and running one test with
// those takes tens of seconds; slow tests get skipped with -short, which is the same as not
// having them. The scaling keeps the same ratios as production (TTL : renew : retry = 4 : 1 : 1),
// so the same shape is verified.
func leaseTestPool(leases Leases, wakeups WorkerWakeups) *WorkerPool {
	return &WorkerPool{
		cfg:        WorkerConfig{Concurrency: 2, Leases: leases, Wakeups: wakeups},
		log:        testLogger(),
		leaseTTL:   200 * time.Millisecond,
		leaseRenew: 50 * time.Millisecond,
		leaseRetry: 50 * time.Millisecond,
		// No spreading in tests: a random delay makes "how long until takeover" unpredictable,
		// and that is exactly the quantity ② measures.
		// Spreading itself is verified separately by TestWorkerLeaseSpreadIsRandom.
		leaseSpread: 0,
	}
}

// ---------------------------------------------------------------------------
// ① Two pods start at the same time → only one polls
// ---------------------------------------------------------------------------

// TestWorkerLeaseOnlyOnePodPolls is the **main goal** of this change: save N-1 copies of idle RPM.
//
// ⚠️ The check is "the other pod never polled even once", not "the total polls of the two pods
// went down". The latter can be fooled by "both are polling, each just half as fast", which
// saves no requests at all.
//
// ⚠️ Mutation check: when the acquirePollLease layer around runOne/pollLoop is removed (back to
// everyone polling), this one must go red.
func TestWorkerLeaseOnlyOnePodPolls(t *testing.T) {
	const envID = "env_shared"
	leases := newSharedLeases()

	apiA, apiB := &countingPollAPI{}, &countingPollAPI{}
	poolA, poolB := leaseTestPool(leases, nil), leaseTestPool(leases, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	for _, tc := range []struct {
		pool *WorkerPool
		api  *countingPollAPI
	}{{poolA, apiA}, {poolB, apiB}} {
		wg.Add(1)
		go func(p *WorkerPool, a *countingPollAPI) {
			defer wg.Done()
			p.runLeasedPoll(ctx, a, envID, "worker", 2, testLogger())
		}(tc.pool, tc.api)
	}

	// Wait until one really is polling (otherwise "the other is 0" might just mean neither has
	// started yet).
	waitFor(t, 3*time.Second, func() bool {
		return apiA.count() > 2 || apiB.count() > 2
	}, "至少有一个 pod 开始 poll")

	cancel()
	wg.Wait()

	a, b := apiA.count(), apiB.count()
	if a > 0 && b > 0 {
		t.Fatalf("两个 pod 都在 poll（A=%d, B=%d）——选主没生效。\n"+
			"线上 7 环境 × 3 pod = 21 条循环合成 ~630 RPM 全是空转，\n"+
			"这一层的全部意义就是把 ×pod 那一维去掉。\n"+
			"接线在 core/worker.go 的 runLeasedPoll → acquirePollLease", a, b)
	}
	if a == 0 && b == 0 {
		t.Fatal("两个 pod 都没 poll——选主把所有人都挡住了，这个环境的对话会全部卡死")
	}
}

// ---------------------------------------------------------------------------
// ② The holder dies → another takes over in < 45 seconds
// ---------------------------------------------------------------------------

// TestWorkerLeaseTakeoverAfterHolderDies is **the core of stability**.
//
// When the holder is kill -9'd (pod eviction / OOM) it does not Release; the lock can only wait
// for its TTL to expire. Until then **nobody is polling** this environment, so takeover must be
// faster than the line where Ark's reclaim window opens (arkReclaimOlderThan = 45 seconds);
// otherwise a stuck window opens: the work can already be reclaimed, but the loop that could
// reclaim it has not started yet; if it drags on to second 90, the server marks it stopped and
// it is beyond saving.
//
// ⚠️ Mutation check: when workerLeaseTTL is set to > 45 seconds,
// TestWorkerPollLeaseFitsReclaimWindow goes red (that one is pure arithmetic and does not depend
// on timing).
func TestWorkerLeaseTakeoverAfterHolderDies(t *testing.T) {
	const envID = "env_takeover"
	leases := newSharedLeases()

	apiA, apiB := &countingPollAPI{}, &countingPollAPI{}
	poolA, poolB := leaseTestPool(leases, nil), leaseTestPool(leases, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		poolA.runLeasedPoll(ctx, apiA, envID, "worker-a", 2, testLogger())
	}()
	go func() {
		defer wg.Done()
		poolB.runLeasedPoll(ctx, apiB, envID, "worker-b", 2, testLogger())
	}()

	waitFor(t, 3*time.Second, func() bool {
		return apiA.count() > 2 || apiB.count() > 2
	}, "有一个 pod 拿到 poll 权")

	// Note who the holder is, then make it "die suddenly" (no Release; the lock expires naturally).
	beforeA, beforeB := apiA.count(), apiB.count()
	loser := apiA
	if beforeA > beforeB {
		loser = apiB
	}
	leases.expire(workerPollLeaseKey(envID))

	// The other one must take over. **The time upper bound of this assertion is the key**:
	// after scaling TTL=200ms and retry=50ms, so takeover should finish within about (TTL+retry).
	// The 3-second allowance is to avoid flakiness, but it is still far below the 45 seconds you
	// get "scaling back to production".
	waitFor(t, 3*time.Second, func() bool {
		return loser.count() > 2
	}, "原持有者猝死后，另一个 pod 接管 poll")

	cancel()
	wg.Wait()
}

// TestWorkerPollLeaseFitsReclaimWindow pins down the chain of inequalities "worst-case takeover
// < reclaim threshold < Ark's stop line".
//
// # Why a pure arithmetic test
//
// ② is a timing test; it verifies "takeover really happens", but **cannot verify that the
// production values are right** (it scales durations down to milliseconds). And the changes that
// really cause trouble are exactly "raise TTL from 20 seconds" or "set the reclaim threshold back
// to 90 seconds", which ② cannot see at all.
//
// This test does not depend on any timing, only arithmetic, so it is stable, fast, and **always
// goes red when broken**.
//
// ⚠️ Mutation check: change workerLeaseTTL to 100 * time.Second, or change
// arkReclaimOlderThan back to 90 * time.Second, and this goes red immediately.
func TestWorkerPollLeaseFitsReclaimWindow(t *testing.T) {
	// Part 1: worst-case takeover time = waiting for the dead lock to expire (TTL) + another
	// pod's next attempt (retryGap).
	worst := workerLeaseTTL + workerLeaseRetryGap
	if worst >= arkReclaimOlderThan {
		t.Fatalf("最坏接管耗时 %v（TTL %v + retry %v）不小于 arkReclaimOlderThan %v。\n"+
			"这会开出一个**卡死窗口**：第 %v 那条在途 work 已被方舟判为无主、可以抢了，\n"+
			"而新的 lease 持有者要到第 %v 才出现——中间没有任何人在 poll 这个环境，\n"+
			"那条会话就停在 requires_action 不动，**且零报错**。\n"+
			"改 TTL 之前先读 core/workerlease.go 的文件头。",
			worst, workerLeaseTTL, workerLeaseRetryGap, arkReclaimOlderThan,
			arkReclaimOlderThan, worst)
	}

	// Part 2: the reclaim threshold must be **strictly less than** the Ark server's 90-second stop
	// line, and leave a window of at least one idle ladder cycle. A zero window is exactly an
	// earlier incident: threshold 90s = stop line 90s, the poll at second 89.9 judged "not quite
	// yet" and returned empty, and by the time the next one went out the server had already
	// stopped it, so it could never be claimed back.
	window := arkWorkServerStopAfter - arkReclaimOlderThan
	if window <= 0 {
		t.Fatalf("reclaim 阈值 %v 必须小于方舟服务端停止线 %v，否则 reclaim 窗口为零：\n"+
			"poll 永远抢不过服务端的自动 stopped，在途 work 永久卡在 requires_action。\n"+
			"见 arkReclaimOlderThan 注释里 sesn-20260101000012-ex012 的实测。",
			arkReclaimOlderThan, arkWorkServerStopAfter)
	}
	if window < 15*time.Second {
		t.Errorf("reclaim 窗口只有 %v（停止线 %v - 阈值 %v），至少要 15s：\n"+
			"poll 请求的超时判定只在请求开始时做一次，窗口窄于一个 poll 周期就等于零。",
			window, arkWorkServerStopAfter, arkReclaimOlderThan)
	}

	// Part 3: the threshold must be later than the old worker's heartbeat self-kill line (30s);
	// otherwise when the network just blips once and the old worker is still alive and about to
	// recover, the new worker has already taken the work: running twice.
	if arkReclaimOlderThan <= workerHeartbeatTTL {
		t.Errorf("arkReclaimOlderThan %v 必须大于心跳自杀线 workerHeartbeatTTL %v，\n"+
			"否则会把心跳慢一拍、其实还活着的 work 抢走，造成双容器双 tool_result。",
			arkReclaimOlderThan, workerHeartbeatTTL)
	}

	// Renewal needs margin: TTL is at least 3 times the renewal interval, otherwise one GC pause
	// loses the lock. Following the "4x margin: the lock is only lost after three consecutive
	// renewal failures" in shared/infra/lark/longconn.go.
	if workerLeaseTTL < 3*workerLeaseRenew {
		t.Errorf("TTL %v 不足续约间隔 %v 的 3 倍——一次网络抖动就会丢锁，"+
			"表现为频繁切主", workerLeaseTTL, workerLeaseRenew)
	}
}

// TestWorkerLeaseDegradesOpenWithoutRedis: with no Redis configured / Redis down, **poll as
// usual**.
//
// This layer is an optimization, not correctness (two pods claiming the same work is settled by
// AckWork). Making it fail-closed would let a Redis blip stop the whole environment from
// polling, turning a pure optimization into a new single point of failure, which is much worse
// than spending a bit more quota.
func TestWorkerLeaseDegradesOpenWithoutRedis(t *testing.T) {
	for _, tc := range []struct {
		name   string
		leases Leases
	}{
		{"没配 Redis", nil},
		{"Redis 出错", func() Leases { l := newSharedLeases(); l.err = errors.New("redis 连不上"); return l }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &countingPollAPI{}
			pool := leaseTestPool(tc.leases, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := make(chan struct{})
			go func() {
				defer close(done)
				pool.runLeasedPoll(ctx, api, "env_x", "worker", 2, testLogger())
			}()
			waitFor(t, 3*time.Second, func() bool { return api.count() > 0 },
				"没有选主能力时仍然照常 poll（degrade-open）")
			cancel()
			<-done
		})
	}
}

// ---------------------------------------------------------------------------
// ③ Consecutive empty polls → the interval climbs step by step to the 60s cap
// ---------------------------------------------------------------------------

// TestWorkerBackoffClimbsToCap verifies the ladder really climbs, and **caps at 60 seconds**.
//
// The check uses the ladder's pure logic instead of measured intervals: measuring would really
// wait 60 seconds, and such a test would only get skipped. The timing side is covered by
// TestPollLoopSleepsWhenQueueIsEmpty (it verifies "it really rested").
func TestWorkerBackoffClimbsToCap(t *testing.T) {
	var b backoffLadder

	// The first level should be the 1-second level (it does not start at 0.5 seconds; see the
	// comment on workerBackoffLevels).
	if got := workerBackoffLevels[0]; got != time.Second {
		t.Errorf("起步档 = %v，要 1s。改成更短会让活跃期 RPM 翻倍，"+
			"而那个场景已经被唤醒机制覆盖了", got)
	}

	seen := []time.Duration{}
	// Climbing the whole ladder takes (levels-1) × workerBackoffStep empty polls; run a few more
	// rounds to verify the cap.
	for i := 0; i < len(workerBackoffLevels)*workerBackoffStep*2; i++ {
		seen = append(seen, b.Next())
	}

	if b.Level() != len(workerBackoffLevels)-1 {
		t.Fatalf("连续空轮询 %d 次后停在第 %d 级，没爬到最慢那级（共 %d 级）",
			len(seen), b.Level(), len(workerBackoffLevels))
	}

	// Cap: those last intervals should all be around the 60-second level (with ±20% jitter).
	ceiling := time.Duration(float64(workerBackoffMax) * (1 + workerBackoffJitter))
	last := seen[len(seen)-1]
	if last > ceiling {
		t.Errorf("最慢一级 %v 超过上限 %v——空闲时接新 work 会太慢", last, ceiling)
	}
	floor := time.Duration(float64(workerBackoffMax) * (1 - workerBackoffJitter))
	if last < floor {
		t.Errorf("最慢一级只有 %v，没到 60 秒档（下界 %v）——降频没达到预期，"+
			"空闲 RPM 降不下来", last, floor)
	}

	// Non-decreasing: no level in the middle should be faster than the previous one (small swings
	// within the jitter are allowed, so it compares **levels**, not jittered values).
	var prev int
	b2 := backoffLadder{}
	for i := 0; i < len(workerBackoffLevels)*workerBackoffStep; i++ {
		b2.Next()
		if b2.Level() < prev {
			t.Fatalf("档位从 %d 掉回 %d——阶梯不该往回走（回到最快只能靠 reset）",
				prev, b2.Level())
		}
		prev = b2.Level()
	}
}

// TestWorkerBackoffNeedsConsecutiveIdlesToClimb: climbing needs **consecutive** empty polls, not
// a jump after one.
//
// Without this, "climb one level on every empty poll" would also pass ③, which would treat the
// normal pauses between conversation turns as "gone idle", and active sessions would be wrongly
// slowed down.
func TestWorkerBackoffNeedsConsecutiveIdlesToClimb(t *testing.T) {
	var b backoffLadder
	for i := 0; i < workerBackoffStep-1; i++ {
		b.Next()
		if b.Level() != 0 {
			t.Fatalf("只空轮询了 %d 次（阈值 %d）就升到第 %d 级——升级太激进，"+
				"对话间隙的正常停顿会把活跃环境降频",
				i+1, workerBackoffStep, b.Level())
		}
	}
	b.Next() // the workerBackoffStep-th time
	if b.Level() != 1 {
		t.Errorf("连续空轮询 %d 次后仍在第 %d 级，没升级——降频不会发生",
			workerBackoffStep, b.Level())
	}
}

// ---------------------------------------------------------------------------
// ④ Getting work → back to the fastest level right away
// ---------------------------------------------------------------------------

// TestWorkerBackoffResetsAfterWork guards against **the most hidden bug** in this change.
//
// The symptom of a missing reset is "an active environment gets slower the more it is used": an
// environment in frequent use climbs to the 60-second level through a few empty polls between
// conversation turns, and then **never comes back**: the next message waits a minute, while logs,
// monitoring and tests are all green. The zero-error kind.
//
// ⚠️ Mutation check: delete the `ladder.reset()` line after getting work in pollLoop, and this
// must go red.
func TestWorkerBackoffResetsAfterWork(t *testing.T) {
	var b backoffLadder
	// First climb to the top.
	for i := 0; i < len(workerBackoffLevels)*workerBackoffStep*2; i++ {
		b.Next()
	}
	if b.Level() == 0 {
		t.Fatal("没爬上去，用例本身没驱动起来")
	}

	b.Reset()

	if b.Level() != 0 {
		t.Fatalf("领到 work 之后档位还在第 %d 级，没回到最快——\n"+
			"症状是「活跃环境越用越慢」且**零报错**：一个频繁使用的环境爬到 60 秒档后\n"+
			"再也回不来，下一条消息要等一分钟。\n"+
			"修复在 core/worker.go 的 pollLoop：领到 work 后调 ladder.reset()",
			b.Level())
	}
	// After going back to the fastest level, the next interval should be the first level.
	if d := b.Next(); d > time.Duration(float64(workerBackoffLevels[0])*(1+workerBackoffJitter)) {
		t.Errorf("reset 之后第一次间隔 %v 仍然偏大（第一档是 %v）",
			d, workerBackoffLevels[0])
	}
}

// TestPollLoopResetsBackoffOnWork is the **end-to-end** version of ④: it runs the real pollLoop.
//
// The test above verifies the ladder data structure; this one verifies "pollLoop really calls
// it". Both are needed (a correct data structure that is not wired in is the kind of silent
// failure this repo has recorded again and again).
//
// # ⚠️ The first version of this test was fake; noted here so nobody changes it back
//
// The first version made work appear on poll number workerBackoffStep+1, then asserted "the
// interval afterwards is back to the first level". **In mutation testing it stayed green with
// ladder.reset() deleted**, for two reasons, and both had to be fixed:
//
//  1. the ladder used to have two 1-second levels (a duplicate level left after 0.5 seconds was
//     removed), so "climbed one level" could not be observed in time at all;
//  2. spinning only workerBackoffStep times only raises the level to level 1, which was also
//     1 second back then.
//
// So now: the ladder has no duplicate levels (see the comment on workerBackoffLevels),
// and **it first spins up to level 2 (the 10-second level) before handing out work**, so "was
// there a reset" shows in the interval as 1 second vs 10 seconds, an order of magnitude apart,
// which cannot be faked.
func TestPollLoopResetsBackoffOnWork(t *testing.T) {
	// Only hand out work after spinning to at least level 2 (the 10-second level): without a reset
	// the next interval is 10 seconds, with a reset it is 1 second, an order of magnitude apart.
	idlesBeforeWork := workerBackoffStep*2 + 1
	api := &idleThenWorkAPI{workAt: idlesBeforeWork}
	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 2}, log: testLogger()}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = pool.pollLoop(ctx, api, "env_test", "worker", 2, testLogger())
	}()

	// Wait until that work is claimed, then collect **two** more polls.
	//
	// ⚠️ Two, not one. After getting work pollLoop polls again **immediately** (no sleep), which is
	// deliberate (if there is work, claim the next one quickly), so the first "interval" after the
	// work is an immediate re-poll of tens of microseconds and does not reflect the level. What
	// really reflects ladder.next() is **the one after that**: it only sleeps after that empty
	// poll. The first version asserted on the first interval, so it was always green; it still
	// passed with ladder.reset() deleted, we measured it.
	//
	// The timeout must be long enough: reaching level 2 takes 2×step empty polls, the first few
	// 1 second each, then 10 seconds each; without a reset the sleep after the work can last up to
	// 20 seconds.
	waitFor(t, 120*time.Second, func() bool { return api.pollsAfterWork() >= 2 },
		"领到 work 之后又 poll 了至少两次")
	cancel()
	<-done

	gaps := api.gapsAfterWork()
	if len(gaps) < 2 {
		t.Fatalf("只采到 %d 个间隔，用例没驱动起来（要至少 2 个，见上面的说明）", len(gaps))
	}

	// Check: the **second** interval after the work (the first real empty-poll sleep) must be back
	// to the first level (1 second), not the level 2 it was at (10 seconds). The midpoint of the
	// two is the threshold, far from both sides, so it will not be flaky.
	first, second := workerBackoffLevels[0], workerBackoffLevels[1]
	threshold := (first + second) / 2
	if gaps[1] > threshold {
		t.Errorf("领到 work 之后第一次空轮询睡了 %v，仍然是降频后的档位"+
			"（门槛 %v，第一档 %v、第二档 %v）——\n"+
			"pollLoop 没有在领到 work 后调 ladder.reset()。\n"+
			"症状是「活跃环境越用越慢」且**零报错**：频繁使用的环境爬到 60 秒档后再也回不来。\n"+
			"修复在 core/worker.go 的 pollLoop：领到 work 后 ladder.reset()",
			gaps[1], threshold, first, second)
	}
}

// idleThenWorkAPI returns empty polls for the first workAt-1 times, hands out one work on poll
// workAt, then keeps returning empty polls.
type idleThenWorkAPI struct {
	fakeWorkAPI

	workAt int

	mu       sync.Mutex
	calls    []time.Time
	workIdx  int
	polls    int
	gotWork  bool
	workTime time.Time
}

func (a *idleThenWorkAPI) PollWork(_ context.Context, _ selfhosted.PollWorkRequest) (*selfhosted.WorkItem, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.polls++
	a.calls = append(a.calls, time.Now())
	if a.polls == a.workAt {
		a.gotWork = true
		a.workTime = time.Now()
		a.workIdx = len(a.calls)
		item := workItem("work_reset", "sesn_reset")
		return &item, nil
	}
	return nil, nil
}

func (a *idleThenWorkAPI) pollsAfterWork() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.gotWork {
		return 0
	}
	return len(a.calls) - a.workIdx
}

func (a *idleThenWorkAPI) gapsAfterWork() []time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.gotWork || a.workIdx >= len(a.calls) {
		return nil
	}
	var out []time.Duration
	prev := a.workTime
	for _, c := range a.calls[a.workIdx:] {
		out = append(out, c.Sub(prev))
		prev = c
	}
	return out
}

// ---------------------------------------------------------------------------
// ⑤ A wakeup signal arrives → the pod holding the lease polls right away (across pods)
// ---------------------------------------------------------------------------

// TestWorkerWakeupTriggersImmediatePoll verifies that after slowing down, responsiveness is
// recovered through wakeups.
//
// The check is "it polled again **soon** after the wakeup", and the upper bound for "soon" must be
// **far below** the current level's interval; otherwise "the timer just happened to fire" could
// also turn the test green, and nothing would be verified. So first push the level to the
// slowest (the 60-second level), then send the wakeup: if it polls within a second then, it can
// only be the wakeup.
func TestWorkerWakeupTriggersImmediatePoll(t *testing.T) {
	waker := newPollWaker()

	// Push the level to the slowest one.
	var b backoffLadder
	for i := 0; i < len(workerBackoffLevels)*workerBackoffStep*2; i++ {
		b.Next()
	}
	slow := b.Next() // ≈60 seconds
	if slow < 30*time.Second {
		t.Fatalf("没推到最慢档（当前 %v），用例前提不成立", slow)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	go func() {
		time.Sleep(30 * time.Millisecond)
		waker.signal()
	}()

	if !waker.wait(ctx, slow) {
		t.Fatal("wait 提前因 ctx 结束返回")
	}
	waited := time.Since(start)

	if waited > 5*time.Second {
		t.Fatalf("唤醒后等了 %v 才醒（当前档位 %v）——唤醒没生效，\n"+
			"降频就变成了「靠拉长间隔把问题藏起来」：用户发消息后最坏要等一分钟。\n"+
			"接线在 core/worker.go 的 pollLoop：waker.wait(ctx, ladder.next())",
			waited, slow)
	}
}

// TestWorkerWakeupCrossesPods is the real point of ⑤: **the one sending the signal and the one
// receiving it are not the same pod**.
//
// After leader election an env has only one poll loop globally, while requests that send
// messages land randomly through the load balancer. With an in-process chan this would go red,
// which is exactly the wrong implementation to guard against.
func TestWorkerWakeupCrossesPods(t *testing.T) {
	const envID = "env_cross"
	wakeups := newFakeWakeups()

	// pod A holds the lease and subscribes to wakeups.
	poolA := leaseTestPool(newSharedLeases(), wakeups)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	apiA := &countingPollAPI{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		poolA.runLeasedPoll(ctx, apiA, envID, "worker-a", 2, testLogger())
	}()

	waitFor(t, 3*time.Second, func() bool { return wakeups.subscribed(envID) },
		"持有 lease 的 pod 订阅了这个环境的唤醒信号")

	before := apiA.count()

	// pod B (another process) sends the wakeup; it has no poll loop itself.
	if err := wakeups.Wake(context.Background(), envID); err != nil {
		t.Fatalf("发唤醒失败: %v", err)
	}

	waitFor(t, 3*time.Second, func() bool { return apiA.count() > before },
		"另一个 pod 发的唤醒信号让持有 lease 的 pod 立刻 poll")

	cancel()
	<-done
}

// ---------------------------------------------------------------------------
// ⑥ A wakeup signal arrives repeatedly → merged, no extra polls
// ---------------------------------------------------------------------------

// TestWorkerWakeupCoalesces: when a conversation turn sends several messages in a row, the
// wakeups must be merged into one.
//
// Without merging, the requests saved by slowing down would be given right back by wakeups:
// 10 messages fire 10 polls.
//
// ⚠️ Mutation check: increase the capacity of pollWaker.ch from 1 (say to 16), or remove the
// non-blocking default branch in signal and make it a blocking send, and this must go red.
func TestWorkerWakeupCoalesces(t *testing.T) {
	waker := newPollWaker()

	// Send 50 signals in a row, simulating consecutive messages in one conversation turn.
	for i := 0; i < 50; i++ {
		waker.signal()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The first wait returns right away (there is a pending signal).
	if !waker.wait(ctx, time.Hour) {
		t.Fatal("第一次 wait 没被唤醒")
	}

	// **The second must wait for the timer**, not return right away: 50 signals should merge
	// into just one.
	start := time.Now()
	if !waker.wait(ctx, 150*time.Millisecond) {
		t.Fatal("第二次 wait 因 ctx 结束返回")
	}
	waited := time.Since(start)

	if waited < 100*time.Millisecond {
		t.Fatalf("第二次 wait 只等了 %v 就返回——50 个唤醒信号没有合并成一个。\n"+
			"一轮对话连发几条消息就会打出一串多余的 poll，把降频省下的请求原样吐回去。\n"+
			"修复在 core/workerwake.go：pollWaker.ch 容量必须是 1，signal 必须非阻塞",
			waited)
	}
}

// TestWorkerWakeupDrainAfterWork: after getting work, pending wakeups must be cleared.
//
// That round already goes back to poll right away; keeping the signal would only make the next
// wait return right away and fire a wasted poll.
func TestWorkerWakeupDrainAfterWork(t *testing.T) {
	waker := newPollWaker()
	waker.signal()
	waker.drain()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	waker.wait(ctx, 120*time.Millisecond)
	if waited := time.Since(start); waited < 80*time.Millisecond {
		t.Errorf("drain 之后 wait 只等了 %v 就返回——待处理的唤醒没清掉，"+
			"领到 work 后会白打一发 poll", waited)
	}
}

// TestWorkerWakerNilIsSafe: with no Wakeups configured (nil waker) it degrades to a plain timer.
//
// Every method on a nil receiver must be safe; otherwise "no wakeups configured" would become a
// panic, and that is the default path for local development and unit tests.
func TestWorkerWakerNilIsSafe(t *testing.T) {
	var waker *pollWaker // nil
	waker.signal()
	waker.drain()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	if !waker.wait(ctx, 80*time.Millisecond) {
		t.Fatal("nil waker 上 wait 应该正常等到定时器")
	}
	if waited := time.Since(start); waited < 50*time.Millisecond {
		t.Errorf("nil waker 上 wait 只等了 %v，没有真的等定时器", waited)
	}
}

// ---------------------------------------------------------------------------
// Helper assertions
// ---------------------------------------------------------------------------

// TestWorkerPollLeaseKeyDoesNotCollide: poll leader election and Docker reclaim-sweep leader
// election **must not share one lock**.
//
// Both are called "leader election", but the cost of failure is completely different: losing the
// reclaim lock just means idle containers are not cleaned this round (cleaned next round, users
// notice nothing), while losing the poll lock means nobody polls this environment and
// conversations get stuck. Sharing one key would let the two starve each other.
func TestWorkerPollLeaseKeyDoesNotCollide(t *testing.T) {
	key := workerPollLeaseKey("env_abc")
	if strings.Contains(key, "docker-reaper") {
		t.Fatal("poll 选主与 Docker 回收巡检选主共用了同一把锁——两者会互相饿死")
	}
	// Different environments must have different locks: the granularity is env, and one pod can
	// hold several at once.
	if workerPollLeaseKey("env_a") == workerPollLeaseKey("env_b") {
		t.Fatal("不同环境的 poll 锁撞了——会退化成「全局只有一个环境能被 poll」")
	}
}

// TestWorkerLeaseSpreadIsRandom: the spread delay before the first lock grab must be random.
//
// If it were not random, the first pod to start would grab the leases of all envs at once:
// leader election works, but all 7 environments pile onto one pod, which becomes a de facto
// single point of failure again.
func TestWorkerLeaseSpreadIsRandom(t *testing.T) {
	p := &WorkerPool{leaseSpread: workerLeaseSpread}
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		d := p.leaseSpreadDelay()
		if d < 0 || d >= workerLeaseSpread {
			t.Fatalf("散开延迟 %v 超出 [0, %v)", d, workerLeaseSpread)
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Error("散开延迟退化成了定值——所有 pod 会同时抢锁，" +
			"最先启动的那个把全部环境的 lease 一把抢光，又变成事实单点")
	}
	// When turned off it must really be 0 (tests need to pin down timing).
	zero := &WorkerPool{leaseSpread: 0}
	if d := zero.leaseSpreadDelay(); d != 0 {
		t.Errorf("leaseSpread=0 时延迟应为 0，实际 %v", d)
	}
}

// Compile-time check that the test doubles satisfy the interfaces. Without it the error would
// show up in the tests, pointing at "type mismatch" instead of here.
var (
	_ Leases        = (*sharedLeases)(nil)
	_ Lease         = (*sharedLease)(nil)
	_ WorkerWakeups = (*fakeWakeups)(nil)
)

// Placeholder for when atomic is used without a direct reference, so the import is not removed
// by mistake.
var _ = atomic.Int32{}
