// Tests for the server-side resident subscriptions.
//
// They watch four things that **fail without reporting an error**:
//
//   - wrong scope: subscribing to idle sessions too, wasting connections and interfering
//     with container pausing;
//   - missed subscription: requires_action treated as terminal, so what the agent says next
//     is never stored;
//   - duplicate subscription: each instance subscribes once, using N times the connections;
//   - no unsubscribe: Ark does not close the stream, so without recognizing the terminal frame
//     a subscription hangs for the full fallback timeout.

package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/repository/dao/coremem"
	"github.com/chyroc/ama/server/shared/infra/ma"
)

// arkLikeRunner replaces fakeRunner's Stream with **Ark's real behavior: it does not return
// after sending its frames**.
//
// # Why it is needed
//
// `fakeRunner.Stream` returns once its frames are sent, so the bug "forgot to unsubscribe on
// the terminal frame" **cannot be seen** on it: the subscription still ends, just because the
// fake itself ended. Verified: deleting the whole turnEnded part kept every test green.
//
// Real Ark **does not close the stream** (it keeps a long connection per session waiting for
// the next user message; see the postmortems in shared/infra/ma and feishu inbound.go), so in
// production that bug shows up as every subscription hanging for the full fallback timeout.
// This repo has already been bitten by fakes that differ from real behavior (core/README.md:
// "this pitfall only shows up when running against real Ark").
//
// After sending its frames this fake blocks until ctx is canceled, and only returns when the
// subscriber disconnects itself.
type arkLikeRunner struct {
	*fakeRunner
}

func (r arkLikeRunner) Stream(
	ctx context.Context, key ArkBindingKey, sessionID string, onFrame func(raw []byte) error,
) error {
	for _, frame := range r.frames {
		if ctx.Err() != nil {
			return nil
		}
		if err := onFrame(frame); err != nil {
			return err
		}
	}
	// **Do not return after the frames are sent**: this is Ark's behavior, and the whole reason
	// this fake exists.
	<-ctx.Done()
	return nil
}

// newWatcherService builds a Service with a fakeRunner and a watcher without leader election.
func newWatcherService(t *testing.T) (*RunWatcher, *Service, *fakeRunner) {
	t.Helper()
	svc, _, runner := newChatService(t)
	w := NewRunWatcher(RunWatcherConfig{Service: svc})
	if w == nil {
		t.Fatal("装配 RunWatcher 失败（Service 或 runner 为空？）")
	}
	return w, svc, runner
}

// TestRunWatcherOnlySubscribesActiveRuns: subscribe only to sessions "still running", not to
// idle or terminal ones.
//
// # Both directions must be asserted
//
// Checking only "active ones are subscribed" is not enough: a "subscribe to everything"
// implementation would pass, and it would attach a long connection to every idle session,
// wasting resources and possibly making containers look alive so they do not pause (that is
// a cost-saving mechanism).
//
// Checking only "idle ones are not subscribed" is not enough either: an implementation that
// subscribes to nothing passes, which means the feature was never built.
func TestRunWatcherOnlySubscribesActiveRuns(t *testing.T) {
	ctx := context.Background()
	w, svc, _ := newWatcherService(t)

	// One run for each of the five states. The value set matches feishu's isTerminalRunStatus.
	cases := []struct {
		status    apimodel.RunStatus
		wantWatch bool
	}{
		{apimodel.RunRunning, true},
		{apimodel.RunRescheduling, true},
		// **requires_action is not terminal**: the agent is waiting for a tool result and
		// continues after the worker returns it. Treating it as terminal means the reply after
		// that is never stored.
		{apimodel.RunRequiresAction, true},
		{apimodel.RunIdle, false},
		{apimodel.RunTerminated, false},
	}
	for i, tc := range cases {
		run := apimodel.Run{
			ID:          string(tc.status) + "-run",
			WorkspaceID: seedWS,
			AgentID:     "agent_triage",
			Status:      tc.status,
			Trigger:     apimodel.TriggerManual,
			StartedAt:   apimodel.FormatISO(time.Now().Add(time.Duration(i) * time.Second)),
		}
		if _, err := svc.store.CreateRun(ctx, run); err != nil {
			t.Fatalf("造 run 失败：%v", err)
		}
	}

	active, err := svc.store.ListActiveRuns(ctx)
	if err != nil {
		t.Fatalf("列活跃 run 失败：%v", err)
	}
	got := map[string]bool{}
	for _, r := range active {
		got[r.ID] = true
	}
	for _, tc := range cases {
		id := string(tc.status) + "-run"
		if got[id] != tc.wantWatch {
			t.Errorf("状态 %s：应当被订阅=%v，实际=%v", tc.status, tc.wantWatch, got[id])
		}
	}

	// Also pin the ordering contract. **Do not just check "two results are the same"**: that
	// assertion is always true when the input is already ordered, and stays green even if the
	// order by is deleted (small tables cannot reveal a missing order by). The data here is
	// built by started_at, so check ascending order directly.
	for i := 1; i < len(active); i++ {
		if active[i-1].StartedAt > active[i].StartedAt {
			t.Errorf("应按 started_at 升序（先开始的先订），实际 %q 排在 %q 前面",
				active[i-1].StartedAt, active[i].StartedAt)
		}
	}
	_ = w
}

// TestRunWatcherClaimIsExclusive: a session can only be claimed once, and can be taken over
// after the lease expires.
//
// # This CAS is resource correctness under multiple instances
//
// Every pod scans the same set of active sessions. Without it, 5 pods each attach one
// subscription: **data does not get corrupted** (events are deduped by id), but Ark long
// connections are wasted 5x.
//
// The lease half must be checked too: when the holder is rebuilt nobody releases it, and only
// expiry lets someone else take over. Without checking, a "never expires" implementation
// passes the first half, and it would leave the session **never subscribed again** after the
// original holder dies, which is exactly the symptom this feature fixes.
func TestRunWatcherClaimIsExclusive(t *testing.T) {
	ctx := context.Background()
	_, svc, _ := newWatcherService(t)

	run := apimodel.Run{
		ID: "run-claim", WorkspaceID: seedWS, AgentID: "agent_triage",
		Status: apimodel.RunRunning, Trigger: apimodel.TriggerManual,
		StartedAt: apimodel.NowISO(),
	}
	if _, err := svc.store.CreateRun(ctx, run); err != nil {
		t.Fatalf("造 run 失败：%v", err)
	}

	now := time.Now()
	ok, err := svc.store.ClaimRunWatch(ctx, run.ID, "pod-a", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("第一个实例应当认领成功：ok=%v err=%v", ok, err)
	}
	// A second instance cannot claim it while the lease is valid.
	ok, err = svc.store.ClaimRunWatch(ctx, run.ID, "pod-b", now, time.Minute)
	if err != nil {
		t.Fatalf("认领出错：%v", err)
	}
	if ok {
		t.Error("租约还有效时第二个实例不该认领到——那会让同一条会话被订两遍")
	}
	// After the lease expires it can be taken over (the original holder may have been
	// rebuilt).
	ok, err = svc.store.ClaimRunWatch(ctx, run.ID, "pod-b", now.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatalf("认领出错：%v", err)
	}
	if !ok {
		t.Error("租约过期后应当能接手，否则原主一死这条会话就再也没人订了")
	}

	// After release it can be claimed again right away. Without releasing, one has to wait a
	// full lease, which shows up as "after closing the page, events are still not stored for a
	// while".
	if err := svc.store.ReleaseRunWatch(ctx, run.ID); err != nil {
		t.Fatalf("释放租约失败：%v", err)
	}
	ok, err = svc.store.ClaimRunWatch(ctx, run.ID, "pod-c", now.Add(2*time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("释放后应当能立刻认领：ok=%v err=%v", ok, err)
	}
}

// TestRunWatcherClaimSkipsTerminalRuns: terminal sessions cannot be claimed.
//
// Pairs with the previous test: the claim itself must also block terminal sessions, not rely
// only on the caller filtering first. A session can finish at any time between "scanned" and
// "claimed"; a successful claim in that window would attach a connection that never receives
// events to a finished session, until the fallback timeout.
func TestRunWatcherClaimSkipsTerminalRuns(t *testing.T) {
	ctx := context.Background()
	_, svc, _ := newWatcherService(t)

	for _, st := range []apimodel.RunStatus{apimodel.RunIdle, apimodel.RunTerminated} {
		run := apimodel.Run{
			ID: "run-" + string(st), WorkspaceID: seedWS, AgentID: "agent_triage",
			Status: st, Trigger: apimodel.TriggerManual, StartedAt: apimodel.NowISO(),
		}
		if _, err := svc.store.CreateRun(ctx, run); err != nil {
			t.Fatalf("造 run 失败：%v", err)
		}
		ok, err := svc.store.ClaimRunWatch(ctx, run.ID, "pod-a", time.Now(), time.Minute)
		if err != nil {
			t.Fatalf("认领出错：%v", err)
		}
		if ok {
			t.Errorf("%s 是终态，不该认领到——会挂一条永远收不到事件的连接", st)
		}
	}
}

// TestRunWatcherTickSubscribesAndReleases: one scan round subscribes, unsubscribes after the
// terminal frame, and releases the lease.
//
// # The "unsubscribe" half is the point
//
// **Ark does not close SSE after end_turn** (it keeps a long connection per session waiting
// for the next user message), so the subscriber must recognize the terminal frame and
// disconnect itself. Without that step every subscription hangs for the full fallback timeout
// (10 minutes); under a resident scan that quickly piles up goroutines and connections, and
// the logs only show "subscription started", never the end. feishu's resume.go misses
// exactly this step.
//
// The check is "the lease was released", not "the goroutine finished": the latter would need
// sleep to guess timing, while the former is the real output of this path, and it also proves
// the cleanup in the defer actually ran.
func TestRunWatcherTickSubscribesAndReleases(t *testing.T) {
	ctx := context.Background()
	w, svc, runner := newWatcherService(t)
	// **Switch to the "does not return after frames" fake**, otherwise this test proves
	// nothing: the default fake ends by itself, so "forgot to unsubscribe" is still green. See
	// arkLikeRunner.
	svc.runner = arkLikeRunner{fakeRunner: runner}

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}
	// Set to running by hand: after CreateConversation and before any message, the status is
	// not active.
	stored, _ := svc.store.GetRun(ctx, run.ID)
	stored.Status = apimodel.RunRunning
	if _, err := svc.store.SaveRun(ctx, stored); err != nil {
		t.Fatalf("置 running 失败：%v", err)
	}

	// One normal event + one terminal frame. The latter should make the subscription
	// disconnect by itself.
	runner.frames = [][]byte{
		[]byte(`{"id":"sevt-w1","type":"agent.message","content":[{"type":"text","text":"没人看着我也在答"}]}`),
		[]byte(`{"id":"sevt-w2","type":"session.status_idle","stop_reason":{"type":"end_turn"}}`),
	}

	w.tick(ctx)
	// Wait for that subscription goroutine to finish. **No sleep**: wg exists for exactly this.
	w.wg.Wait()

	// 1. Events really got stored: that is the purpose of the whole feature (without a
	// subscriber they would be lost).
	events, err := svc.store.ListRunEvents(ctx, run.ID, persistmodel.EventQuery{})
	if err != nil {
		t.Fatalf("列事件失败：%v", err)
	}
	if len(events) != len(runner.frames) {
		t.Errorf("没人看着也该把 %d 条事件落库，实际 %d 条",
			len(runner.frames), len(events))
	}

	// 2. The turn ended: the terminal frame set the run back to idle.
	//
	// ⚠️ **This assertion must not be written as "it can be claimed again now"**. The first
	// version was written that way and went red, for a reason unrelated to unsubscribing: the
	// terminal frame makes the run idle, and ClaimRunWatch already rejects terminal states
	// (correctly; see TestRunWatcherClaimSkipsTerminalRuns). Pick the output that really
	// corresponds to "unsubscribed", not one affected by another rule.
	final, err := svc.store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("读 run 失败：%v", err)
	}
	if final.Status != apimodel.RunIdle {
		t.Errorf("收到 end_turn 后 run 应当是 idle，实际 %s", final.Status)
	}

	// 3. **The lease really was released.** Read the lease directly, not through the indirect
	//    "can it be claimed again" path that is affected by status rules. If not released,
	//    this session still holds the slot after the original holder unsubscribes, and other
	//    instances must wait a full lease to take over.
	if held := watchLeaseHeld(t, svc, run.ID); held {
		t.Error("退订后应当释放租约，否则这条会话要等满一个租约才有人接手")
	}
}

// watchLeaseHeld asks "is this session's subscription lease still held by someone".
//
// It reads MemStore's internal state instead of "try to claim again": the latter is disturbed
// by the "terminal states cannot be claimed" rule, so it returns false whether or not the
// lease was released, and the assertion always holds. That would be a false green.
func watchLeaseHeld(t *testing.T, svc *Service, runID string) bool {
	t.Helper()
	mem, ok := svc.store.(*coremem.MemStore)
	if !ok {
		t.Fatalf("这条用例依赖 MemStore，实际是 %T", svc.store)
	}
	return mem.RunWatchLeaseHeld(runID)
}

// TestRunWatcherTwoInstancesSubscribeOnce: two instances scan at once, and one session is
// subscribed only once.
//
// # This checks gate two (PG claim), not gate one (Redis leader election)
//
// The Redis lock can be left out entirely (nil when Redis is not configured), and then
// **every instance scans**. Correctness must rest on the claim: without it, 5 pods each
// attach a subscription, wasting 5x the Ark long connections.
//
// The two watchers share one Service (that is, one store), simulating two pods seeing the
// same DB. The check is "the Ark side was subscribed only once": count how many times Stream
// was called directly, the only honest output of this (logs and leases may be affected by
// other rules).
//
// ⚠️ Mutation check: deleting the whole ClaimRunWatch part in tick turns this red (measured
// 2 != 1). Without it, none of the earlier tests can catch "duplicate subscription".
func TestRunWatcherTwoInstancesSubscribeOnce(t *testing.T) {
	ctx := context.Background()
	_, svc, runner := newWatcherService(t)
	// Do not return after the frames, so the subscription stays open: exactly the state "two
	// instances subscribe at once" needs to observe.
	counting := newCountingRunner(arkLikeRunner{fakeRunner: runner})
	svc.runner = counting

	run := apimodel.Run{
		ID: "run-two-pods", WorkspaceID: seedWS, AgentID: "agent_triage",
		Status: apimodel.RunRunning, Trigger: apimodel.TriggerManual,
		StartedAt: apimodel.NowISO(),
	}
	if _, err := svc.store.CreateRun(ctx, run); err != nil {
		t.Fatalf("造 run 失败：%v", err)
	}
	// Only one frame that does not end by itself: the subscription stays open and does not
	// enter the unsubscribe path.
	runner.frames = [][]byte{
		[]byte(`{"id":"sevt-two","type":"agent.message","content":[{"type":"text","text":"在答"}]}`),
	}

	// Each of the two "pods" scans one round. They share a store, so the claims really
	// compete.
	podA := NewRunWatcher(RunWatcherConfig{Service: svc, Owner: "pod-a"})
	podB := NewRunWatcher(RunWatcherConfig{Service: svc, Owner: "pod-b"})
	podA.tick(ctx)
	podB.tick(ctx)

	// Let both goroutines get into Stream.
	//
	// **Do not just wg.Wait()**: the subscription stays open on purpose, so waiting for it
	// means waiting for the full fallback timeout. Here we wait for "the call count to settle",
	// by polling instead of a fixed sleep.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && counting.calls(run.ID) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // give the second instance a real window to squeeze in

	if got := counting.calls(run.ID); got != 1 {
		t.Errorf("同一条会话只该被订阅 1 次，实际 %d 次 —— "+
			"多实例下这是 N 倍的方舟长连接", got)
	}

	// Cleanup: wake the open subscriptions so they do not hold up the test process.
	cancelAll(podA, podB)
}

// countingRunner counts how many times Stream was called for **the given session**.
//
// ⚠️ **Counts must be kept per sessionID, not just a total.** The seed already has 3 active
// runs (run_4 running / run_5 requires_action / run_8 rescheduling), and the scan subscribes
// to them too: the total is 4, not 1, and those 3 have nothing to do with what this test
// checks. The first version did exactly that and reported "got 4 calls", which looked like a
// broken claim, but the assertion was counting the wrong thing.
type countingRunner struct {
	arkLikeRunner
	mu sync.Mutex
	n  map[string]int
}

func newCountingRunner(inner arkLikeRunner) *countingRunner {
	return &countingRunner{arkLikeRunner: inner, n: map[string]int{}}
}

func (r *countingRunner) Stream(
	ctx context.Context, key ArkBindingKey, sessionID string, onFrame func(raw []byte) error,
) error {
	r.mu.Lock()
	r.n[sessionID]++
	r.mu.Unlock()
	return r.arkLikeRunner.Stream(ctx, key, sessionID, onFrame)
}

func (r *countingRunner) calls(sessionID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n[sessionID]
}

// cancelAll stops the open subscriptions so they do not drag on until the fallback timeout.
func cancelAll(ws ...*RunWatcher) {
	for _, w := range ws {
		w.watching.Range(func(k, _ any) bool {
			w.watching.Delete(k)
			return true
		})
	}
}

// TestRunWatcherRetiresMissingSession: a session that no longer exists on the Ark side must be
// settled on the spot, not resubscribed forever.
//
// # This came from a real run, not from reasoning
//
// The first time resident subscriptions ran locally, the log immediately showed the same 404
// (`InvalidSession.NotFound`) over and over, one round every 30 seconds, never stopping. Two
// sources:
//
//   - fake session ids in the seed data (run_4 / run_5);
//   - **a kind that really exists in production**: a session idle longer than the container
//     TTL (1 hour) is reclaimed by the platform, the Ark-side session is gone, while the
//     status in our DB still says running, because with no subscriber nobody pushes it to a
//     terminal state, the very gap this feature fills.
//     So the feature became a victim of that gap itself.
//
// The fix pushes it to terminated. **Skipping is not enough**: if the status does not change,
// every scan picks it up and subscribes again.
func TestRunWatcherRetiresMissingSession(t *testing.T) {
	ctx := context.Background()
	w, svc, runner := newWatcherService(t)
	// Make the subscription hit 404, with the same shape as the real one.
	svc.runner = notFoundRunner{fakeRunner: runner}

	run := apimodel.Run{
		ID: "run-gone", WorkspaceID: seedWS, AgentID: "agent_triage",
		Status: apimodel.RunRunning, Trigger: apimodel.TriggerManual,
		StartedAt: apimodel.NowISO(),
	}
	if _, err := svc.store.CreateRun(ctx, run); err != nil {
		t.Fatalf("造 run 失败：%v", err)
	}

	w.tick(ctx)
	w.wg.Wait()

	after, err := svc.store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("读 run 失败：%v", err)
	}
	// 1. Pushed to a terminal state: the only guarantee that "the next round will not pick it
	// up".
	if after.Status != apimodel.RunTerminated {
		t.Errorf("方舟侧已无此会话时应当标记 terminated，实际 %s —— "+
			"status 不改的话每 30 秒会再订一次，无限刷 404", after.Status)
	}
	// 2. Give the user a message they can understand, and **do not call it a service fault**:
	//    the execution environment was reclaimed, and sending a new message to start a new
	//    session is enough.
	if after.ErrorSummary == "" {
		t.Error("应当留一句说明，否则用户对着一个不能用的会话不知道发生了什么")
	}

	// 3. The next round does not pick it up: check directly that "it is no longer in the
	// active list".
	active, err := svc.store.ListActiveRuns(ctx)
	if err != nil {
		t.Fatalf("列活跃 run 失败：%v", err)
	}
	for _, r := range active {
		if r.ID == run.ID {
			t.Error("已终止的会话不该再出现在活跃列表里")
		}
	}
}

// notFoundRunner makes Stream fail with "the Ark side has no such session".
//
// Same shape as the real one: HTTP 404 + code=InvalidSession.NotFound.
type notFoundRunner struct {
	*fakeRunner
}

func (r notFoundRunner) Stream(
	context.Context, ArkBindingKey, string, func(raw []byte) error,
) error {
	return &ma.APIError{
		Code:           "InvalidSession.NotFound",
		Message:        "The session does not exist or you do not have access to it.",
		Type:           "Not Found",
		HTTPStatusCode: 404,
	}
}

// TestRunWatcherSkipsRunsWithActiveStream: sessions someone is already watching are not
// subscribed again.
//
// While the browser has the chat page open, events are already being stored; another
// resident subscription would only waste a connection.
func TestRunWatcherSkipsRunsWithActiveStream(t *testing.T) {
	ctx := context.Background()
	w, svc, _ := newWatcherService(t)

	run := apimodel.Run{
		ID: "run-watched", WorkspaceID: seedWS, AgentID: "agent_triage",
		Status: apimodel.RunRunning, Trigger: apimodel.TriggerManual,
		StartedAt: apimodel.NowISO(),
	}
	if _, err := svc.store.CreateRun(ctx, run); err != nil {
		t.Fatalf("造 run 失败：%v", err)
	}
	// Pretend the browser has the chat page open.
	release := svc.markStreamActive(run.ID)
	defer release()

	w.tick(ctx)
	w.wg.Wait()

	// No subscription started → the lease was never taken, so it can be claimed now.
	ok, err := svc.store.ClaimRunWatch(ctx, run.ID, "pod-x", time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("认领出错：%v", err)
	}
	if !ok {
		t.Error("已经有人在看的会话不该被常驻订阅认领（白占一条连接）")
	}
}

// TestReapStaleRunsRetiresZombies: stuck sessions must be pushed to a terminal state, and not
// a single live one may be touched.
//
// # This guards against a production incident
//
// In production, about one in ten runs stayed stuck in requires_action for good, the oldest
// for days, and the number was growing. The root cause was not a special tool (the pending
// tools were all ordinary bash / read), but **`user.tool_result` never came back because the
// execution environment was replaced, and nothing was responsible for cleaning up**.
//
// Three costs: zombies pile up in run history; the UI thinks the session is still waiting
// for someone; **resident subscriptions resubscribe to it every 30 seconds, each time hanging
// for the full 10-minute fallback timeout**, so the more pile up, the more Ark connections
// are wasted.
//
// # The negative half matters more than the positive half
//
// Checking only "zombies were retired" would let a "retire all active sessions"
// implementation pass, and it would cut off sessions doing normal work. The symptom looks
// exactly like a real zombie (a terminated run), which in turn makes people think the fallback
// did not work. So "fresh ones must not be touched" must be asserted too.
func TestReapStaleRunsRetiresZombies(t *testing.T) {
	ctx := context.Background()
	w, svc, _ := newWatcherService(t)

	now := time.Now().UTC()
	// Older than runStaleAfter = should be retired; recently active = should not.
	longAgo := apimodel.FormatISO(now.Add(-2 * runStaleAfter))
	justNow := apimodel.FormatISO(now.Add(-time.Second))

	cases := []struct {
		id         string
		status     apimodel.RunStatus
		lastActive string
		wantReaped bool
	}{
		{"zombie-ra", apimodel.RunRequiresAction, longAgo, true},
		{"zombie-running", apimodel.RunRunning, longAgo, true},
		{"fresh-ra", apimodel.RunRequiresAction, justNow, false},
		{"fresh-running", apimodel.RunRunning, justNow, false},
		{"already-idle", apimodel.RunIdle, longAgo, false},
	}
	for _, tc := range cases {
		run := apimodel.Run{
			ID: tc.id, WorkspaceID: seedWS, AgentID: "agent_triage",
			Status: tc.status, Trigger: apimodel.TriggerSchedule,
			StartedAt: tc.lastActive, LastActivityAt: tc.lastActive,
		}
		if _, err := svc.store.CreateRun(ctx, run); err != nil {
			t.Fatalf("造 run %s 失败：%v", tc.id, err)
		}
	}

	w.reapStaleRuns(ctx)

	for _, tc := range cases {
		got, err := svc.store.GetRun(ctx, tc.id)
		if err != nil {
			t.Fatalf("查 run %s 失败：%v", tc.id, err)
		}
		if tc.wantReaped {
			if got.Status != apimodel.RunTerminated {
				t.Errorf("run %s（%s，很久没动静）应被回收成 terminated，实际 %q —— "+
					"不收的话它会被每 30 秒重订一次，永远占着一条方舟连接",
					tc.id, tc.status, got.Status)
			}
			// Cleanup must fill in the end time, or it shows "in progress" in run history
			// forever.
			if got.EndedAt == "" || got.DurationMs == nil {
				t.Errorf("run %s 回收后要补上 endedAt/durationMs，实际 %+v", tc.id, got)
			}
			// It must leave a readable explanation. Without one, the user stares at a blank
			// "terminated" and guesses why.
			if got.ErrorSummary == "" {
				t.Errorf("run %s 回收后要写明原因", tc.id)
			}
			continue
		}
		if got.Status != tc.status {
			t.Errorf("run %s（%s）不该被动，实际变成了 %q —— "+
				"把正在跑的会话收掉就是把用户的回答腰斩，而症状与真僵尸一样",
				tc.id, tc.status, got.Status)
		}
		if got.EndedAt != "" {
			t.Errorf("run %s 没被回收，不该有 endedAt，实际 %q", tc.id, got.EndedAt)
		}
	}
}

// TestReapRunsBeforeSubscribing: retiring must happen before subscribing, or this round
// subscribes to a zombie for nothing.
//
// The wrong order does not report an error; it just makes every round first attach a
// subscription to the zombie that is sure to receive no events, hanging for the full
// watchTurnTimeout (10 minutes). The check is "after tick the zombie is terminal, and it was
// not claimed for subscription": the lease being claimable shows the subscribe step did not
// touch it.
func TestReapRunsBeforeSubscribing(t *testing.T) {
	ctx := context.Background()
	w, svc, _ := newWatcherService(t)

	longAgo := apimodel.FormatISO(time.Now().UTC().Add(-2 * runStaleAfter))
	run := apimodel.Run{
		ID: "zombie-order", WorkspaceID: seedWS, AgentID: "agent_triage",
		Status: apimodel.RunRequiresAction, Trigger: apimodel.TriggerSchedule,
		StartedAt: longAgo, LastActivityAt: longAgo,
	}
	if _, err := svc.store.CreateRun(ctx, run); err != nil {
		t.Fatalf("造 run 失败：%v", err)
	}

	w.tick(ctx)
	w.wg.Wait()

	got, err := svc.store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("查 run 失败：%v", err)
	}
	if got.Status != apimodel.RunTerminated {
		t.Fatalf("tick 应当先回收僵尸，实际状态 %q", got.Status)
	}
	// Not claimed for subscription. **The check cannot be "claim again and see if it
	// works"**: terminal states cannot be claimed anyway, so that assertion returns false
	// whether or not it was subscribed, a false green. Use RunWatchLeaseHeld to read the lease
	// directly (same reason as TestRunWatcherReleasesLeaseOnFinish).
	type leaseReader interface{ RunWatchLeaseHeld(string) bool }
	if lr, ok := svc.store.(leaseReader); ok && lr.RunWatchLeaseHeld(run.ID) {
		t.Error("僵尸已经被回收了，不该再给它挂订阅——那会白挂满 10 分钟兜底超时")
	}
}

// TestReapDoesNotKillRevivedRun: if the session comes back to life during retirement, it must
// not be killed again.
//
// There is a real window between listing candidates and writing back, and the late
// tool_result may well arrive right then. A "read then write" implementation would kill the
// just-revived session again: the user sees the conversation suddenly "terminated", and the
// DB only has a terminated row, with no log saying it ever came back.
//
// Here the window is created by "reviving it after listing and before writing back".
func TestReapDoesNotKillRevivedRun(t *testing.T) {
	ctx := context.Background()
	_, svc, _ := newWatcherService(t)

	now := time.Now().UTC()
	longAgo := apimodel.FormatISO(now.Add(-2 * runStaleAfter))
	run := apimodel.Run{
		ID: "revived", WorkspaceID: seedWS, AgentID: "agent_triage",
		Status: apimodel.RunRequiresAction, Trigger: apimodel.TriggerSchedule,
		StartedAt: longAgo, LastActivityAt: longAgo,
	}
	if _, err := svc.store.CreateRun(ctx, run); err != nil {
		t.Fatalf("造 run 失败：%v", err)
	}

	staleBefore := apimodel.FormatISO(now.Add(-runStaleAfter))
	stale, err := svc.store.ListStaleActiveRuns(ctx, staleBefore)
	if err != nil {
		t.Fatalf("列卡死的会话失败：%v", err)
	}
	// The seed already has a few unfinished runs, so we cannot assert "only 1"; pick the one we
	// built.
	var snapshot apimodel.Run
	for _, r := range stale {
		if r.ID == run.ID {
			snapshot = r
			break
		}
	}
	if snapshot.ID == "" {
		t.Fatalf("造的那条卡死会话应当被列出来，实际列出 %d 条但不含它", len(stale))
	}

	// The window: a late tool_result arrives, the run goes back to running and its activity
	// time is refreshed.
	revived := run
	revived.Status = apimodel.RunRunning
	revived.LastActivityAt = apimodel.FormatISO(now)
	if _, err := svc.store.SaveRun(ctx, revived); err != nil {
		t.Fatalf("模拟复活失败：%v", err)
	}

	// Retire using the snapshot from before the window, exactly what the scan would have.
	doomed := snapshot
	doomed.Status = apimodel.RunTerminated
	ok, err := svc.store.RetireStaleRun(ctx, doomed, staleBefore)
	if err != nil {
		t.Fatalf("回收出错：%v", err)
	}
	if ok {
		t.Error("会话已经活过来了，不该被回收")
	}
	after, err := svc.store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("查 run 失败：%v", err)
	}
	if after.Status != apimodel.RunRunning {
		t.Errorf("复活的会话应保持 running，实际 %q —— "+
			"先读后写会把它重新按死，且零报错", after.Status)
	}
}

// TestRequiresActionDoesNotCloseRun: requires_action is not an end and must not write
// endedAt.
//
// # This pins the "looks self-contradictory" shape from that incident
//
// Under self_hosted, Ark pushes an `idle + requires_action` frame for **every tool call**, many
// times in one turn. The old code called closeRun unconditionally in the status_idle branch,
// so the run carried both "an end time" and "a status that has not ended".
//
// Two real costs: the detail page shows an "end time" at a moment when the agent was actually
// still running; and when debugging zombies, a set `endedAt` leads people to "it all ended, so
// why didn't the status settle", **which is the wrong direction**: the real problem is that the
// tool_result never came back. The contract itself also says these two fields are "missing
// when not ended" (DurationMs is a pointer precisely to tell "0 ms" from "not ended").
//
// **Both directions must be asserted**: checking only that requires_action does not write
// endedAt would let a "never writes it" implementation pass, and that would make truly ended
// sessions show "in progress" forever.
func TestRequiresActionDoesNotCloseRun(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}

	// The idle frame in the middle of a tool call: the status changes, but **it is not an
	// end**.
	runner.frames = [][]byte{
		[]byte(`{"type":"session.status_idle","id":"sevt-ra","stop_reason":{"type":"requires_action","event_ids":["call_x"]}}`),
	}
	if err := svc.StreamRunEvents(ctx, run.ID, func([]byte) error { return nil }); err != nil {
		t.Fatalf("订阅失败：%v", err)
	}
	mid, _ := store.GetRun(ctx, run.ID)
	if mid.Status != apimodel.RunRequiresAction {
		t.Fatalf("状态应为 requires_action，实际 %q", mid.Status)
	}
	if mid.EndedAt != "" || mid.DurationMs != nil {
		t.Errorf("requires_action 是「正在等工具结果」，不该写 endedAt/durationMs，"+
			"实际 endedAt=%q durationMs=%v —— 这个自相矛盾的形状会把排查引向反方向",
			mid.EndedAt, mid.DurationMs)
	}

	// Reverse: the frame of a real end (end_turn) must fill it in, or run history shows "in
	// progress" forever.
	runner.frames = [][]byte{
		[]byte(`{"type":"session.status_idle","id":"sevt-done","stop_reason":{"type":"end_turn"}}`),
	}
	if err := svc.StreamRunEvents(ctx, run.ID, func([]byte) error { return nil }); err != nil {
		t.Fatalf("订阅失败：%v", err)
	}
	done, _ := store.GetRun(ctx, run.ID)
	if done.Status != apimodel.RunIdle {
		t.Fatalf("end_turn 后状态应为 idle，实际 %q", done.Status)
	}
	if done.EndedAt == "" || done.DurationMs == nil {
		t.Errorf("真正结束时要补上 endedAt/durationMs，实际 %+v", done)
	}
}

// TestRunStaleAfterExceedsLiveWorkSilence: the threshold must not fall within "the silence a
// live work item may have".
//
// # Why write a test for a constant
//
// Setting this too small is **worse than not doing it at all**: it pushes sessions that are
// working normally but slow on this step to a terminal state, and the symptom looks exactly
// like a real zombie (a terminated run), which in turn makes people think the fallback did not
// work.
//
// The lower bound is three parts added up, each with a source: the reclaim window (90 seconds)
// + the tool execution ceiling (120 seconds) + one subscription's fallback length (10 minutes).
// The assertion pins **this lower bound**, not a specific value: writing `== 30 minutes` would
// force anyone who tunes it to change the test, and the assertion would only restate the
// implementation.
func TestRunStaleAfterExceedsLiveWorkSilence(t *testing.T) {
	// The longest silence a live work item may have.
	maxLiveSilence := arkReclaimOlderThan + 120*time.Second + watchTurnTimeout
	if runStaleAfter <= maxLiveSilence {
		t.Errorf("runStaleAfter=%s 必须明显大于活着的 work 的最长静默期 %s，"+
			"否则会把正在跑的会话误判成僵尸并腰斩",
			runStaleAfter, maxLiveSilence)
	}
}

// TestRunWatcherNilWithoutRunner: without Ark wired in it returns nil, and the caller then does
// not start it.
//
// Return nil rather than an error: without Ark this scan has nothing to do, and letting it
// "quietly not exist" is more correct than keeping the service from starting. The caller must
// log a warning stating the consequence; see cmd/amad.
func TestRunWatcherNilWithoutRunner(t *testing.T) {
	if w := NewRunWatcher(RunWatcherConfig{}); w != nil {
		t.Error("Service 为空时应当返回 nil")
	}
	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败：%v", err)
	}
	svc, err := New(Config{Store: store}) // deliberately no ArkRunner
	if err != nil {
		t.Fatalf("装配 Service 失败：%v", err)
	}
	if w := NewRunWatcher(RunWatcherConfig{Service: svc}); w != nil {
		t.Error("没接方舟（runner 为 nil）时应当返回 nil")
	}
}

// TestReapSettlesDurationAtLastActivity: retirement must settle the duration at "the last
// activity", not at now.
//
// # This guards the second half of that incident
//
// Fallback retirement happens **long after** the session really stopped: zombie detection needs
// runStaleAfter (30 minutes) of silence, and older ones may have piled up for days. Settling
// with `now` then records not "how long this run ran" but "how long until someone noticed it
// was stuck".
//
// A batch retired in production had exactly this shape: sessions really active for seconds
// were recorded as running **for more than a day**. `durationMs` is a stored field, and the
// "duration" column of RunsPage and OverviewPage renders it directly (formatDuration), so run
// history showed a row of scan tasks that ran for tens of hours. **Zero errors**: the numbers
// were always there, just all wrong, and wrong in a way that looks like "these tasks really did
// run a long time".
//
// # The check pins "matches the real active interval", not a specific number
//
// Writing the assertion as `durationMs == 90000` would require changing it whenever the data
// changes, which only restates the implementation. Here we compare against
// `lastActivityAt - startedAt`, the interval the session actually ran.
//
// The upper-bound half especially cannot be dropped: a `now` implementation computes "more than
// twice runStaleAfter" in this test, three orders of magnitude off the real interval, exactly
// what production showed.
func TestReapSettlesDurationAtLastActivity(t *testing.T) {
	ctx := context.Background()
	w, svc, _ := newWatcherService(t)

	now := time.Now().UTC()
	// Build a session that "went quiet 90 seconds after starting, then stayed stuck for a long
	// time".
	// ⚠️ startedAt and lastActivityAt **must differ**: with the same value the real duration is
	// ≈0, and both "settle at now" and "settle at activity time" would pass an assertion that
	// only checks the lower bound.
	started := now.Add(-3 * runStaleAfter)
	lastActive := started.Add(90 * time.Second)
	wantMS := lastActive.Sub(started).Milliseconds()

	run := apimodel.Run{
		ID: "zombie-duration", WorkspaceID: seedWS, AgentID: "agent_triage",
		Status: apimodel.RunRequiresAction, Trigger: apimodel.TriggerSchedule,
		StartedAt: apimodel.FormatISO(started), LastActivityAt: apimodel.FormatISO(lastActive),
	}
	if _, err := svc.store.CreateRun(ctx, run); err != nil {
		t.Fatalf("造 run 失败：%v", err)
	}

	w.reapStaleRuns(ctx)

	got, err := svc.store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("查 run 失败：%v", err)
	}
	if got.Status != apimodel.RunTerminated {
		t.Fatalf("前提不成立：这条应当被回收，实际状态 %q", got.Status)
	}
	if got.DurationMs == nil {
		t.Fatal("回收后应当有时长——缺 durationMs 会让前端把已终止的 run 显示成「还没结束」")
	}
	if *got.DurationMs != wantMS {
		t.Errorf("回收的时长应按最后活跃时刻结算 = %d ms（%.0f 秒），实际 %d ms（%.1f 小时）——"+
			"按 now 结算记下的是「隔了多久才被发现」而不是「跑了多久」，"+
			"运行历史的时长列会出现一排几十小时的巡检任务",
			wantMS, float64(wantMS)/1000, *got.DurationMs, float64(*got.DurationMs)/3600000)
	}
	// endedAt must also be "the moment it actually stopped", or the end time on the detail page
	// is still wrong.
	if got.EndedAt != apimodel.FormatISO(lastActive) {
		t.Errorf("回收的结束时刻应当是最后活跃时刻 %q，实际 %q",
			apimodel.FormatISO(lastActive), got.EndedAt)
	}
}

// TestCloseRunAtSettlesAtGivenInstant: closeRunAt settles at the given instant and does not
// peek at now.
//
// Division of work with the test above: this one pins **the primitive itself** (there will be
// other callers besides the retirement path), the one above pins **that the retirement path
// really uses it**. Without this one, an implementation that ignores closeRunAt's argument and
// still uses now inside would only turn the test above red, and the error would point to the
// retirement logic, not to the settlement primitive.
func TestCloseRunAtSettlesAtGivenInstant(t *testing.T) {
	_, svc, _ := newWatcherService(t)

	started := time.Date(2026, 8, 30, 3, 10, 30, 0, time.UTC)
	endedAt := started.Add(31 * time.Second)
	run := apimodel.Run{ID: "close-at", StartedAt: apimodel.FormatISO(started)}

	svc.closeRunAt(&run, endedAt)

	if run.EndedAt != apimodel.FormatISO(endedAt) {
		t.Errorf("endedAt 应当是传入的时刻 %q，实际 %q", apimodel.FormatISO(endedAt), run.EndedAt)
	}
	if run.DurationMs == nil || *run.DurationMs != 31_000 {
		t.Errorf("时长应当按传入时刻算出 31000 ms，实际 %v", run.DurationMs)
	}
}
