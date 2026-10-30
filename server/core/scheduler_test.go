// Scheduler tests. All offline: Ark goes through fakeRunner in chat_test.go, delivery through
// this file's fakeDeliverer.
//
// Always step one tick by hand with `tick` instead of starting a ticker and waiting for it.
// What the tests assert is "scanned / not scanned, claimed / not claimed, what was written
// back"; letting real time take part only makes the cases slow and flaky.
//
// The three most worth reading:
//
//	TestSchedulerFireOnceWritesBackState  must write back next_run_at after a run, else "runs once, never again"
//	TestSchedulerAutoPausesAfterFailures  consecutive failures auto-pause, to stop a cascade
//	TestSchedulerOneshotDeletedAfterFire  a oneshot task is deleted after running, even on failure

package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/repository/dao/coremem"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// fakeDeliverer records every delivery for assertions.
type fakeDeliverer struct {
	mu    sync.Mutex
	calls []deliverCall
	err   error
}

type deliverCall struct {
	target apimodel.DeliveryTarget
	title  string
	text   string
	runID  string
}

func (d *fakeDeliverer) Deliver(
	_ context.Context, target apimodel.DeliveryTarget, title, text, runID string,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, deliverCall{target: target, title: title, text: text, runID: runID})
	return d.err
}

func (d *fakeDeliverer) snapshot() []deliverCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]deliverCall, len(d.calls))
	copy(out, d.calls)
	return out
}

// agentReplyFrames is the minimal frame sequence of one successful turn.
//
// Only agent.message and the closing status_idle: the former is "the result", the latter moves
// the run to a final state.
func agentReplyFrames(text string) [][]byte {
	return [][]byte{
		[]byte(fmt.Sprintf(
			`{"id":"sevt-1","type":"agent.message","content":[{"type":"text","text":%q}]}`, text)),
		[]byte(`{"id":"sevt-2","type":"session.status_idle","stop_reason":{"type":"end_turn"}}`),
	}
}

// newTestScheduler builds a scheduler wired with fakes.
//
// The returned now is a pointer to a changeable instant: everything the scheduler tests assert
// is a function of time, so advance time with it instead of sleeping (same as injecting Now in
// the feishu package).
func newTestScheduler(t *testing.T) (*Scheduler, *Service, *coremem.MemStore, *fakeRunner, *fakeDeliverer, *time.Time) {
	t.Helper()
	svc, store, runner := newChatService(t)
	deliverer := &fakeDeliverer{}
	now := time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC)
	clock := &now
	sch := NewScheduler(SchedulerConfig{
		Service:   svc,
		Deliverer: deliverer,
		Now:       func() time.Time { return *clock },
	})
	if sch == nil {
		t.Fatal("装配调度器失败")
	}
	return sch, svc, store, runner, deliverer, clock
}

// seedSchedule creates a due interval task (interval makes the next fire time easy to assert).
func seedSchedule(t *testing.T, svc *Service, mutate func(*apimodel.ScheduleDraft)) apimodel.Schedule {
	t.Helper()
	draft := apimodel.ScheduleDraft{
		AgentID:  "agent_triage",
		Name:     "每日巡检",
		Spec:     apimodel.ScheduleSpec{Kind: apimodel.ScheduleInterval, EveryMs: 5 * 60 * 1000},
		Prompt:   "汇总昨天的告警",
		Delivery: apimodel.DeliveryTarget{Kind: apimodel.DeliveryRunOnly},
	}
	if mutate != nil {
		mutate(&draft)
	}
	sch, err := svc.CreateSchedule(context.Background(), seedWS, draft)
	if err != nil {
		t.Fatalf("建任务失败：%v", err)
	}
	return sch
}

// makeDue sets a task's next fire time in the past, so it is due on this tick.
func makeDue(t *testing.T, store *coremem.MemStore, id string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	cur, err := store.GetSchedule(ctx, id)
	if err != nil {
		t.Fatalf("读任务失败：%v", err)
	}
	cur.NextRunAt = apimodel.FormatISO(at)
	if _, err := store.SaveSchedule(ctx, cur); err != nil {
		t.Fatalf("改到期时刻失败：%v", err)
	}
}

// TestSchedulerFireOnceWritesBackState: after one run the schedule state must be written back.
//
// **Writing back next_run_at is the weak point of this path**: it is cleared at claim time (to
// prevent a second claim during fire). If it is not set again after the run, the task is never
// claimed again. It shows up as "ran once, then never again", while the UI looks fine.
func TestSchedulerFireOnceWritesBackState(t *testing.T) {
	ctx := context.Background()
	sched, svc, store, runner, _, clock := newTestScheduler(t)
	runner.frames = agentReplyFrames("昨天有 3 条告警")

	sch := seedSchedule(t, svc, nil)
	makeDue(t, store, sch.ID, clock.Add(-time.Minute))

	sched.tick(ctx)
	sched.wg.Wait()

	got, err := store.GetSchedule(ctx, sch.ID)
	if err != nil {
		t.Fatalf("读任务失败：%v", err)
	}
	if got.NextRunAt == "" {
		t.Fatal("跑完要把 nextRunAt 排回去，否则这条任务再也不会被认领")
	}
	// interval counts from "the moment the run finished", so the next fire must be later than
	// now.
	if got.NextRunAt <= apimodel.FormatISO(*clock) {
		t.Errorf("下次触发应晚于现在，实际 %s（现在是 %s）",
			got.NextRunAt, apimodel.FormatISO(*clock))
	}
	if got.RunCount != 1 {
		t.Errorf("运行次数应为 1，实际 %d", got.RunCount)
	}
	if got.FailCount != 0 || got.ConsecutiveFailures != 0 {
		t.Errorf("成功一轮不该记失败：fail=%d consecutive=%d", got.FailCount, got.ConsecutiveFailures)
	}
	if got.LastError != "" {
		t.Errorf("成功一轮该清掉上次的错误，实际 %q", got.LastError)
	}
	if got.LastRunID == "" {
		t.Error("要记下这一轮的 run id，界面上靠它跳到运行详情")
	}
	if got.Status != apimodel.ScheduleActive {
		t.Errorf("成功一轮不该改状态，实际 %s", got.Status)
	}
}

// TestSchedulerRunHasScheduleTrigger: a Run created by a schedule must be marked schedule, not
// manual.
//
// **If mislabeled, that run shows up in the session list in the chat page's left column**
// (ChatPage filters on `trigger === "manual"`), and the user sees a pile of conversations they
// never started.
func TestSchedulerRunHasScheduleTrigger(t *testing.T) {
	ctx := context.Background()
	sched, svc, store, runner, _, clock := newTestScheduler(t)
	runner.frames = agentReplyFrames("好了")

	sch := seedSchedule(t, svc, nil)
	makeDue(t, store, sch.ID, clock.Add(-time.Minute))

	sched.tick(ctx)
	sched.wg.Wait()

	got, _ := store.GetSchedule(ctx, sch.ID)
	run, err := store.GetRun(ctx, got.LastRunID)
	if err != nil {
		t.Fatalf("读 run 失败：%v", err)
	}
	if run.Trigger != apimodel.TriggerSchedule {
		t.Errorf("触发来源应是 schedule，实际 %q —— manual 会让它混进对话页的会话列表",
			run.Trigger)
	}
	if run.TriggerDetail != sch.Name {
		t.Errorf("来源明细应是任务名（运行历史那一列直接显示它），实际 %q", run.TriggerDetail)
	}
	// ScheduleID is the only basis for the "run history" list on the detail page.
	//
	// **It is a different thing from TriggerDetail; assert both**: names can change and can
	// repeat, so looking up history by name alone breaks at the moment of a rename, and it
	// breaks with no error; the list just has fewer rows. Without this assertion, "forgot to
	// set the schedule id on the run" shows up in production as "the detail page never finds
	// any history", with zero errors.
	if run.ScheduleID != sch.ID {
		t.Errorf("run 应指回触发它的那条任务（%s），实际 %q —— 详情页的历史运行列表按它查",
			sch.ID, run.ScheduleID)
	}

	// The lookup step really finds this run. The check above only verified the field was set;
	// this one verifies the filter path works (the ScheduleID branch of ListRuns + the column on
	// the PG side).
	hist, err := store.ListRuns(ctx, sch.WorkspaceID, persistmodel.RunQuery{ScheduleID: sch.ID})
	if err != nil {
		t.Fatalf("按 scheduleId 反查失败：%v", err)
	}
	if len(hist) != 1 || hist[0].ID != run.ID {
		t.Errorf("按 scheduleId 应反查到刚跑那条 run，实际 %d 条", len(hist))
	}
}

// TestSchedulerDeliversToFeishu: with Feishu delivery configured, the final text must be sent.
func TestSchedulerDeliversToFeishu(t *testing.T) {
	ctx := context.Background()
	sched, svc, store, runner, deliverer, clock := newTestScheduler(t)
	runner.frames = agentReplyFrames("昨天有 3 条告警")

	sch := seedSchedule(t, svc, func(d *apimodel.ScheduleDraft) {
		d.Delivery = apimodel.DeliveryTarget{
			Kind:        apimodel.DeliveryFeishu,
			FeishuAppID: "fs_1",
			ChatID:      "oc_test",
			ThreadID:    "omt_test",
		}
	})
	makeDue(t, store, sch.ID, clock.Add(-time.Minute))

	sched.tick(ctx)
	sched.wg.Wait()

	calls := deliverer.snapshot()
	if len(calls) != 1 {
		t.Fatalf("应投递一次，实际 %d 次", len(calls))
	}
	if calls[0].text != "昨天有 3 条告警" {
		t.Errorf("投出去的应是 agent 的最终回复，实际 %q", calls[0].text)
	}
	if calls[0].target.ChatID != "oc_test" {
		t.Errorf("应投到冻结下来的那个群，实际 %q", calls[0].target.ChatID)
	}
	// "What came from a thread goes back to the thread": ThreadID must be carried all the way to
	// the delivery layer.
	if calls[0].target.ThreadID != "omt_test" {
		t.Errorf("话题 ID 要带过去，实际 %q", calls[0].target.ThreadID)
	}
	if calls[0].title != sch.Name {
		t.Errorf("卡片标题应是任务名，实际 %q", calls[0].title)
	}
}

// TestSchedulerRunOnlySkipsDelivery: run_only does not deliver.
func TestSchedulerRunOnlySkipsDelivery(t *testing.T) {
	ctx := context.Background()
	sched, svc, store, runner, deliverer, clock := newTestScheduler(t)
	runner.frames = agentReplyFrames("好了")

	sch := seedSchedule(t, svc, nil) // run_only by default
	makeDue(t, store, sch.ID, clock.Add(-time.Minute))

	sched.tick(ctx)
	sched.wg.Wait()

	if calls := deliverer.snapshot(); len(calls) != 0 {
		t.Errorf("run_only 不该投递，实际投了 %d 次", len(calls))
	}
	// But the Run must still be stored; that is the whole point of run_only.
	got, _ := store.GetSchedule(ctx, sch.ID)
	if got.LastRunID == "" {
		t.Error("run_only 也要留下一条 Run")
	}
}

// TestSchedulerDeliveryFailureCountsAsFailure: a delivery failure counts as a failed run.
//
// What the user wants is "see the report in the group at 9:30". If the agent run succeeds but
// the report is not sent, for them it is the same as not running, so it cannot count as a
// success.
func TestSchedulerDeliveryFailureCountsAsFailure(t *testing.T) {
	ctx := context.Background()
	sched, svc, store, runner, deliverer, clock := newTestScheduler(t)
	runner.frames = agentReplyFrames("报表")
	deliverer.err = errors.New("飞书拒收")

	sch := seedSchedule(t, svc, func(d *apimodel.ScheduleDraft) {
		d.Delivery = apimodel.DeliveryTarget{
			Kind: apimodel.DeliveryFeishu, FeishuAppID: "fs_1", ChatID: "oc_test",
		}
	})
	makeDue(t, store, sch.ID, clock.Add(-time.Minute))

	sched.tick(ctx)
	sched.wg.Wait()

	got, _ := store.GetSchedule(ctx, sch.ID)
	if got.FailCount != 1 {
		t.Errorf("投递失败应记一次失败，实际 fail=%d", got.FailCount)
	}
	if got.LastError == "" {
		t.Error("要留下失败原因，用户得知道是投递环节挂了")
	}
	// Schedule the next one even on failure: otherwise the task stops forever.
	if got.NextRunAt == "" {
		t.Error("失败也要排下一次触发")
	}
}

// TestSchedulerEmptyReplyIsFailure: an agent that replies with nothing counts as a failure.
//
// Sending an empty report is more confusing than sending none.
func TestSchedulerEmptyReplyIsFailure(t *testing.T) {
	ctx := context.Background()
	sched, svc, store, runner, deliverer, clock := newTestScheduler(t)
	// Only a status frame, no agent.message.
	runner.frames = [][]byte{
		[]byte(`{"id":"sevt-1","type":"session.status_idle","stop_reason":{"type":"end_turn"}}`),
	}

	sch := seedSchedule(t, svc, func(d *apimodel.ScheduleDraft) {
		d.Delivery = apimodel.DeliveryTarget{
			Kind: apimodel.DeliveryFeishu, FeishuAppID: "fs_1", ChatID: "oc_test",
		}
	})
	makeDue(t, store, sch.ID, clock.Add(-time.Minute))

	sched.tick(ctx)
	sched.wg.Wait()

	if calls := deliverer.snapshot(); len(calls) != 0 {
		t.Errorf("没有回复就不该投递，实际投了 %d 次", len(calls))
	}
	got, _ := store.GetSchedule(ctx, sch.ID)
	if got.FailCount != 1 {
		t.Errorf("空回复应算失败，实际 fail=%d", got.FailCount)
	}
}

// TestSchedulerAutoPausesAfterFailures: hitting the consecutive failure threshold auto-pauses.
//
// To stop a cascade: a task with a wrong prompt or a deleted agent fails every period, and each
// time it really creates an Ark session, burns tokens, and may post a failure card to the group.
func TestSchedulerAutoPausesAfterFailures(t *testing.T) {
	ctx := context.Background()
	sched, svc, store, runner, _, clock := newTestScheduler(t)
	// Session creation fails → every run fails.
	runner.sessionErr = errors.New("方舟挂了")

	sch := seedSchedule(t, svc, nil)

	for i := 0; i < apimodel.MaxConsecutiveFailures; i++ {
		makeDue(t, store, sch.ID, clock.Add(-time.Minute))
		sched.tick(ctx)
		sched.wg.Wait()
	}

	got, err := store.GetSchedule(ctx, sch.ID)
	if err != nil {
		t.Fatalf("读任务失败：%v", err)
	}
	if got.Status != apimodel.SchedulePaused {
		t.Errorf("连续失败 %d 次应自动暂停，实际状态 %s",
			apimodel.MaxConsecutiveFailures, got.Status)
	}
	if got.NextRunAt != "" {
		t.Errorf("暂停后不该再有下次触发，实际 %q", got.NextRunAt)
	}
	if got.FailCount != apimodel.MaxConsecutiveFailures {
		t.Errorf("失败次数应是 %d，实际 %d", apimodel.MaxConsecutiveFailures, got.FailCount)
	}
}

// TestSchedulerOneshotDeletedAfterFire: a oneshot task is deleted after it runs.
func TestSchedulerOneshotDeletedAfterFire(t *testing.T) {
	ctx := context.Background()
	sched, svc, store, runner, _, clock := newTestScheduler(t)
	runner.frames = agentReplyFrames("提醒你看发布结果")

	sch := seedSchedule(t, svc, func(d *apimodel.ScheduleDraft) {
		d.Spec = apimodel.ScheduleSpec{
			Kind:   apimodel.ScheduleOneshot,
			FireAt: apimodel.FormatISO(clock.Add(-time.Minute)),
		}
	})

	sched.tick(ctx)
	sched.wg.Wait()

	if _, err := store.GetSchedule(ctx, sch.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("一次性任务跑完该被删掉，实际 %v", err)
	}
}

// TestSchedulerOneshotDeletedEvenOnFailure: a oneshot task is deleted even when it fails.
//
// If kept, it would run and fail again next period: "remind me in 1 hour" would become a
// reminder every 30 seconds.
func TestSchedulerOneshotDeletedEvenOnFailure(t *testing.T) {
	ctx := context.Background()
	sched, svc, store, runner, _, clock := newTestScheduler(t)
	runner.sessionErr = errors.New("方舟挂了")

	sch := seedSchedule(t, svc, func(d *apimodel.ScheduleDraft) {
		d.Spec = apimodel.ScheduleSpec{
			Kind:   apimodel.ScheduleOneshot,
			FireAt: apimodel.FormatISO(clock.Add(-time.Minute)),
		}
	})

	sched.tick(ctx)
	sched.wg.Wait()

	if _, err := store.GetSchedule(ctx, sch.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("一次性任务失败也该被删掉（否则会反复重试），实际 %v", err)
	}
}

// TestSchedulerSkipsNotDue: tasks that are not due do not run.
func TestSchedulerSkipsNotDue(t *testing.T) {
	ctx := context.Background()
	sched, svc, store, runner, _, clock := newTestScheduler(t)
	runner.frames = agentReplyFrames("不该跑")

	sch := seedSchedule(t, svc, nil)
	makeDue(t, store, sch.ID, clock.Add(time.Hour)) // due only in an hour

	sched.tick(ctx)
	sched.wg.Wait()

	got, _ := store.GetSchedule(ctx, sch.ID)
	if got.RunCount != 0 {
		t.Errorf("没到点不该跑，实际跑了 %d 次", got.RunCount)
	}
}

// TestSchedulerSkipsPaused: paused tasks do not run.
func TestSchedulerSkipsPaused(t *testing.T) {
	ctx := context.Background()
	sched, svc, store, runner, _, clock := newTestScheduler(t)
	runner.frames = agentReplyFrames("不该跑")

	sch := seedSchedule(t, svc, func(d *apimodel.ScheduleDraft) {
		d.Status = apimodel.SchedulePaused
	})
	// Even with the due time forced into the past, a paused task must not be claimed.
	cur, _ := store.GetSchedule(ctx, sch.ID)
	cur.NextRunAt = apimodel.FormatISO(clock.Add(-time.Minute))
	if _, err := store.SaveSchedule(ctx, cur); err != nil {
		t.Fatalf("造状态失败：%v", err)
	}

	sched.tick(ctx)
	sched.wg.Wait()

	got, _ := store.GetSchedule(ctx, sch.ID)
	if got.RunCount != 0 {
		t.Errorf("暂停的任务不该跑，实际跑了 %d 次", got.RunCount)
	}
}

// TestSchedulerRunsPausedScheduleAfterRunNow: a paused task with a trial run requested really
// runs once.
//
// **This pins the whole path, not one layer.** RunScheduleNow used to only set nextRunAt to
// now, while the first condition of the claim SQL is `status = 'active'`. So the API returned
// 200 and the frontend row's nextRunAt updated, **but the run never happened**: zero errors,
// looking exactly like success.
//
// The assertion uses RunCount: finish only adds 1 after a run really completes.
func TestSchedulerRunsPausedScheduleAfterRunNow(t *testing.T) {
	ctx := context.Background()
	sched, svc, store, runner, _, clock := newTestScheduler(t)
	runner.frames = agentReplyFrames("试跑跑完了")

	// Created as paused: exactly the "set it up, click once to check it, then enable" shape.
	sch := seedSchedule(t, svc, func(d *apimodel.ScheduleDraft) {
		d.Status = apimodel.SchedulePaused
	})

	// Go through the real business entry point instead of setting the mark by hand: what we
	// verify is whether the run-now path works.
	fired, err := svc.RunScheduleNow(ctx, sch.ID)
	if err != nil {
		t.Fatalf("试跑失败：%v", err)
	}

	// **Advance the fake clock past that fire time.** RunScheduleNow uses real time
	// (apimodel.NowISO) while the scheduler uses the injected clock; in production they are the
	// same clock, here they are not. Advancing it matches the real order in production: the user
	// clicks, and the next tick picks it up.
	due, perr := time.Parse(persistmodel.ISOLayout, fired.NextRunAt)
	if perr != nil {
		t.Fatalf("试跑回执里的 nextRunAt 解不动（%q）：%v", fired.NextRunAt, perr)
	}
	*clock = due.Add(time.Second)

	sched.tick(ctx)
	sched.wg.Wait()

	got, err := store.GetSchedule(ctx, sch.ID)
	if err != nil {
		t.Fatalf("读任务失败：%v", err)
	}
	if got.RunCount != 1 {
		t.Fatalf("暂停的任务点了试跑就该真的跑一轮，实际跑了 %d 次 —— "+
			"run-now 返回 200 却零效果是最难查的一类故障", got.RunCount)
	}
	if got.LastRunID == "" {
		t.Error("跑完要回写 LastRunID，否则用户点不进去看这一轮的结果")
	}

	// **A trial run must not make it active**, nor schedule a next run: the user only wanted to
	// "check it once", not to have the task start running on schedule.
	if got.Status != apimodel.SchedulePaused {
		t.Errorf("试跑不该改 status，实际变成了 %q —— 那会让「先配好、别自动跑」这个用法消失",
			got.Status)
	}
	if got.NextRunAt != "" {
		t.Errorf("仍是 paused 就不该排下一次，实际 %q", got.NextRunAt)
	}
	// The mark was consumed by the claim, so another tick does not run it again.
	if got.RunNowRequested {
		t.Error("试跑标记该在认领时被消费，实际仍是 true")
	}

	sched.tick(ctx)
	sched.wg.Wait()
	after, _ := store.GetSchedule(ctx, sch.ID)
	if after.RunCount != 1 {
		t.Errorf("一次试跑只该换来一轮，再推一拍后跑了 %d 次", after.RunCount)
	}
}

// TestSchedulerPausedWithLeftoverNextRunNotClaimed: a paused task without a trial run request is
// not run.
//
// **Paired with the previous one**, it guards against "over-fixing": deleting the status
// condition from the claim entirely would also make the previous test pass, but then every task
// would run right away **at the moment it is paused**, because the nextRunAt left from before
// pausing is still in the past.
//
// The leftover state (paused + due nextRunAt) is built by hand on purpose, because on the normal
// path SetScheduleStatus clears nextRunAt, so it alone cannot reach this combination.
func TestSchedulerPausedWithLeftoverNextRunNotClaimed(t *testing.T) {
	ctx := context.Background()
	sched, svc, store, runner, _, clock := newTestScheduler(t)
	runner.frames = agentReplyFrames("不该跑")

	sch := seedSchedule(t, svc, nil)
	cur, _ := store.GetSchedule(ctx, sch.ID)
	cur.Status = apimodel.SchedulePaused
	cur.NextRunAt = apimodel.FormatISO(clock.Add(-time.Minute)) // the one left from before pausing
	cur.RunNowRequested = false                                 // nobody requested a trial run
	if _, err := store.SaveSchedule(ctx, cur); err != nil {
		t.Fatalf("造状态失败：%v", err)
	}

	sched.tick(ctx)
	sched.wg.Wait()

	got, _ := store.GetSchedule(ctx, sch.ID)
	if got.RunCount != 0 {
		t.Errorf("没点试跑的 paused 任务不该被跑，实际跑了 %d 次 —— "+
			"否则每条任务在暂停那一刻都会被立刻跑一次", got.RunCount)
	}
}

// TestSchedulerPausedDuringRunNotRescheduled: if paused while this run is going, no next run is
// scheduled.
func TestSchedulerPausedDuringRunNotRescheduled(t *testing.T) {
	ctx := context.Background()
	sched, svc, store, runner, _, clock := newTestScheduler(t)
	runner.frames = agentReplyFrames("跑完了")

	sch := seedSchedule(t, svc, nil)
	makeDue(t, store, sch.ID, clock.Add(-time.Minute))

	// Pause it after the claim and before the write-back: finish re-reads, so it should see this
	// change.
	claimed, err := store.ClaimDueSchedules(ctx, "test", *clock, claimLease, 0)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("认领失败：err=%v n=%d", err, len(claimed))
	}
	cur, _ := store.GetSchedule(ctx, sch.ID)
	cur.Status = apimodel.SchedulePaused
	if _, err := store.SaveSchedule(ctx, cur); err != nil {
		t.Fatalf("暂停失败：%v", err)
	}

	sched.finish(ctx, claimed[0], "sesn-x", *clock, nil)

	got, _ := store.GetSchedule(ctx, sch.ID)
	if got.NextRunAt != "" {
		t.Errorf("跑完发现已被暂停，就不该排下一次，实际 %q", got.NextRunAt)
	}
}

// TestSchedulerFinishRereadsSchedule: re-read before writing back, so changes the user made in
// the meantime are not overwritten.
//
// Several minutes may pass between claim and write-back, and the user may change the prompt in
// the UI. Writing back the in-memory copy from when fire started would **silently overwrite**
// the change: the user thinks they changed it, but the next run still uses the old one.
func TestSchedulerFinishRereadsSchedule(t *testing.T) {
	ctx := context.Background()
	sched, svc, store, _, _, clock := newTestScheduler(t)

	sch := seedSchedule(t, svc, nil)
	stale := sch // the copy from when fire started

	// The user changed the prompt in the meantime.
	cur, _ := store.GetSchedule(ctx, sch.ID)
	cur.Prompt = "改过的指令"
	if _, err := store.SaveSchedule(ctx, cur); err != nil {
		t.Fatalf("改 prompt 失败：%v", err)
	}

	sched.finish(ctx, stale, "sesn-x", *clock, nil)

	got, _ := store.GetSchedule(ctx, sch.ID)
	if got.Prompt != "改过的指令" {
		t.Errorf("回写不该覆盖用户中途的改动，实际 prompt=%q", got.Prompt)
	}
}

// TestSchedulerPromptCarriesContextSummary: the context summary must be put before the prompt.
//
// Every fire is a brand new session with zero history. Self wake-up needs it most: when the
// agent schedules "check CI in 10 minutes" it has a whole turn of context in mind, and the new
// session that wakes up knows nothing.
func TestSchedulerPromptCarriesContextSummary(t *testing.T) {
	sch := apimodel.Schedule{
		Prompt:         "看 CI 过了没",
		ContextSummary: "在给 PR #123 修测试，失败的是 store_test",
	}
	raw, err := scheduleUserMessage(sch, sch.Prompt)
	if err != nil {
		t.Fatalf("拼指令失败：%v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "PR #123") {
		t.Error("上下文摘要要进 prompt，否则醒来那轮不知道在说什么")
	}
	if !strings.Contains(text, "看 CI 过了没") {
		t.Error("原指令也要在")
	}
	// Summary first, instruction after: the model reads the background before the task.
	if strings.Index(text, "PR #123") > strings.Index(text, "看 CI 过了没") {
		t.Error("摘要应拼在指令前面")
	}
}

// TestSchedulerNoSummaryKeepsPromptClean: without a summary the prompt is the original text, with
// no prefix.
func TestSchedulerNoSummaryKeepsPromptClean(t *testing.T) {
	raw, err := scheduleUserMessage(apimodel.Schedule{Prompt: "汇总告警"}, "汇总告警")
	if err != nil {
		t.Fatalf("拼指令失败：%v", err)
	}
	if strings.Contains(string(raw), "上下文") {
		t.Errorf("没有摘要就不该加前缀，实际 %s", raw)
	}
}

// TestNewSchedulerNilServiceReturnsNil: with a nil Service it returns nil and lets the caller
// decide what to do.
//
// Same convention as NewWorkerPool: return nil when the config is incomplete, and cmd/amad then
// states the consequence loudly.
func TestNewSchedulerNilServiceReturnsNil(t *testing.T) {
	if NewScheduler(SchedulerConfig{}) != nil {
		t.Error("Service 为空应返回 nil")
	}
}

// fakeLeases is a fake implementation of gate one.
type fakeLeases struct {
	mu       sync.Mutex
	acquired int
	grant    bool
	err      error
	released int
}

func (l *fakeLeases) Acquire(_ context.Context, _ string, _ time.Duration) (Lease, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.acquired++
	if l.err != nil {
		return nil, false, l.err
	}
	if !l.grant {
		return nil, false, nil
	}
	return &fakeLease{parent: l}, true, nil
}

type fakeLease struct{ parent *fakeLeases }

func (l *fakeLease) Renew(_ context.Context, _ time.Duration) (bool, error) { return true, nil }
func (l *fakeLease) Release(_ context.Context) error {
	l.parent.mu.Lock()
	defer l.parent.mu.Unlock()
	l.parent.released++
	return nil
}

// TestSchedulerSkipsScanWhenNotLeader: if the lock cannot be taken, skip the scan this tick.
//
// That is all gate one does: save N-1 instances a useless scan. Correctness does not rely on it.
func TestSchedulerSkipsScanWhenNotLeader(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)
	runner.frames = agentReplyFrames("不该跑")
	now := time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC)
	leases := &fakeLeases{grant: false}
	sched := NewScheduler(SchedulerConfig{
		Service: svc,
		Leases:  leases,
		Now:     func() time.Time { return now },
	})

	sch := seedSchedule(t, svc, nil)
	makeDue(t, store, sch.ID, now.Add(-time.Minute))

	sched.tick(ctx)
	sched.wg.Wait()

	if leases.acquired != 1 {
		t.Errorf("应尝试抢锁一次，实际 %d 次", leases.acquired)
	}
	got, _ := store.GetSchedule(ctx, sch.ID)
	if got.RunCount != 0 {
		t.Errorf("抢不到锁不该扫，实际跑了 %d 次", got.RunCount)
	}
}

// TestSchedulerScansAnywayWhenRedisFails: when taking the lock errors, scan as usual.
//
// **It must not fall back to not scanning**: then all schedules would stop whenever Redis is
// down, while correctness never depended on it (duplicates are caught by the PG claim).
func TestSchedulerScansAnywayWhenRedisFails(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)
	runner.frames = agentReplyFrames("照常跑")
	now := time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC)
	leases := &fakeLeases{err: errors.New("redis 连不上")}
	sched := NewScheduler(SchedulerConfig{
		Service: svc,
		Leases:  leases,
		Now:     func() time.Time { return now },
	})

	sch := seedSchedule(t, svc, nil)
	makeDue(t, store, sch.ID, now.Add(-time.Minute))

	sched.tick(ctx)
	sched.wg.Wait()

	got, _ := store.GetSchedule(ctx, sch.ID)
	if got.RunCount != 1 {
		t.Errorf("Redis 挂了也该照常跑（重复由 PG 认领兜住），实际跑了 %d 次", got.RunCount)
	}
}

// TestSchedulerLeaderReleasesLock: after taking the lock and scanning, release it.
//
// Without releasing, the next tick has to wait for the TTL to expire, which stretches the scan
// period to leaderTTL.
func TestSchedulerLeaderReleasesLock(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newChatService(t)
	now := time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC)
	leases := &fakeLeases{grant: true}
	sched := NewScheduler(SchedulerConfig{
		Service: svc,
		Leases:  leases,
		Now:     func() time.Time { return now },
	})

	sched.tick(ctx)
	sched.wg.Wait()

	leases.mu.Lock()
	defer leases.mu.Unlock()
	if leases.released != 1 {
		t.Errorf("扫完应释放锁，实际释放 %d 次 —— 不释放会让下一拍等 TTL", leases.released)
	}
}

// contains / idxOf are already defined elsewhere in this package (contains in agent.go is the
// slice version), so use the strings ones directly here instead of making same-named helpers.

// TestLostRoundDetection: the check for "the previous run was lost" reports only real zombies,
// not normal runs.
//
// This guards **discoverability**, not correctness: zombie recovery (see store_pg_schedule.go)
// makes a stuck task heal itself, so "why it got stuck" loses every trace. That run writes no
// RunCount, no FailCount, no LastError. This Warn is the only clue.
//
// ⚠️ Both directions must be asserted together: **a missed report** means going back to a
// past incident (stopped for 21 hours without anyone noticing), and **a false report** is
// worse: a Warn that fires every run gets filtered out as noise, which is the same as turning
// the alert off.
func TestLostRoundDetection(t *testing.T) {
	now := time.Date(2026, 8, 30, 16, 41, 0, 0, time.UTC)
	sched := NewScheduler(SchedulerConfig{
		Service: &Service{},
		Now:     func() time.Time { return now },
	})

	hourly := apimodel.ScheduleSpec{Kind: apimodel.ScheduleInterval, EveryMs: 3600_000}

	cases := []struct {
		name string
		sch  apimodel.Schedule
		want bool
	}{{
		// The real shape seen in production: every 60 minutes, yet not run for 21 hours.
		name: "僵尸：距上次跑完 21 小时，远超一个周期加一个租约",
		sch:  apimodel.Schedule{Spec: hourly, LastRunAt: apimodel.FormatISO(now.Add(-21 * time.Hour))},
		want: true,
	}, {
		name: "正常：刚好到点被领走",
		sch:  apimodel.Schedule{Spec: hourly, LastRunAt: apimodel.FormatISO(now.Add(-time.Hour))},
		want: false,
	}, {
		// Being a little late is normal (scan interval + the previous run took a while); do not
		// report it.
		name: "正常：迟到几分钟，还在租约余量内",
		sch:  apimodel.Schedule{Spec: hourly, LastRunAt: apimodel.FormatISO(now.Add(-70 * time.Minute))},
		want: false,
	}, {
		// **A newly created task has naturally never run**; reporting this kind would make every
		// new task log a Warn.
		name: "不判：从没跑过",
		sch:  apimodel.Schedule{Spec: hourly, LastRunAt: ""},
		want: false,
	}, {
		name: "不判：oneshot 没有周期可比",
		sch: apimodel.Schedule{
			Spec:      apimodel.ScheduleSpec{Kind: apimodel.ScheduleOneshot, FireAt: apimodel.FormatISO(now)},
			LastRunAt: apimodel.FormatISO(now.Add(-21 * time.Hour)),
		},
		want: false,
	}, {
		name: "不判：LastRunAt 形状不对，不拿它当证据",
		sch:  apimodel.Schedule{Spec: hourly, LastRunAt: "上周二"},
		want: false,
	}, {
		name: "僵尸：cron 任务按「上次跑完之后的下一次」倒推周期",
		sch: apimodel.Schedule{
			Spec: apimodel.ScheduleSpec{
				Kind: apimodel.ScheduleCron, Cron: "*/10 * * * *", TZ: apimodel.DefaultScheduleTZ,
			},
			LastRunAt: apimodel.FormatISO(now.Add(-5 * time.Hour)),
		},
		want: true,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := sched.lostRoundBefore(tc.sch, now)
			if got != tc.want {
				t.Errorf("lostRoundBefore = %v，期望 %v", got, tc.want)
			}
		})
	}
}
