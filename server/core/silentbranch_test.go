// Tests for "a failure branch must not share a silent return with the normal branch".
//
// # These assertions guard against one shape
//
// The trap this repo keeps falling into: `if err != nil { return }` shares one exit with "this
// thing simply does not exist". The failure then looks like the normal case, **with zero logs
// throughout**, and what the person debugging sees points the opposite way from the real cause.
// Three times we already paid for it:
//
//   - `if err != nil || len(bindings) == 0 { continue }` in worker.go (fixed): one DB hiccup
//     skipped a whole workspace, and every tool call in that workspace got stuck in requires_action;
//   - the scheduler that "stopped for 21 hours while four counters pretended nothing happened";
//   - Feishu CoT delivery failures with zero logs, leaving the user stuck at "thinking" forever.
//
// ⚠️ Each test below asserts on **log text**. Return values cannot catch this kind of problem:
// these functions all return void or nil, and "nothing happened" is exactly how they normally look.

package core

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/repository/dao/coremem"
	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

// captureCoreLogs builds a logger that writes logs into a buffer.
func captureCoreLogs() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// errGetSchedule is the injected failure. **Deliberately not ErrNotFound**: the two sharing one
// return is exactly the bug under test.
var errGetSchedule = errors.New("pg: connection reset by peer")

// flakyScheduleStore makes GetSchedule fail and passes other methods through.
type flakyScheduleStore struct {
	Store
	failGet bool
}

func (s *flakyScheduleStore) GetSchedule(ctx context.Context, id string) (apimodel.Schedule, error) {
	if s.failGet {
		return apimodel.Schedule{}, errGetSchedule
	}
	return s.Store.GetSchedule(ctx, id)
}

// ⭐ When the re-read fails while rescheduling to the next fire time, it must leave a trace.
//
// Reaching this return without rescheduling means the task's next_run_at stays empty →
// ClaimDueSchedules never picks it → **this scheduled task never runs again**, while the UI still
// shows status active and a failure count of 0.
//
// Worse, this function is only entered after the Warn above saying "the previous round is still
// running, skipping this trigger", so whoever reads the logs only sees "skipped, rescheduled".
// A silent return is not "one log line missing"; it is **the log asserting something that did
// not happen**.
//
// Mutation check: delete this Warn (keep the return) and this test turns red.
func TestRescheduleOnlyLogsWhenRereadFails(t *testing.T) {
	log, buf := captureCoreLogs()
	sch, svc, _, _, _, _ := newTestScheduler(t)
	sch.log = log

	seeded := seedSchedule(t, svc, nil)

	// Swap the store for the failing one.
	svc.store = &flakyScheduleStore{Store: svc.store, failGet: true}

	sch.rescheduleOnly(context.Background(), seeded)

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("重读失败必须留 Warn，否则这条任务永久停跑而日志说「已排回去」。实际输出：%s", out)
	}
	if !strings.Contains(out, seeded.ID) {
		t.Errorf("要带上是哪条任务，实际输出：%s", out)
	}
	// The consequence must be visible, not just "it failed".
	if !strings.Contains(out, "不会再被触发") {
		t.Errorf("日志要说清后果，否则看到的人不知道这条任务已经死了。实际输出：%s", out)
	}
}

// ⭐ A deleted task is the normal case, no log.
//
// This is the reverse control for the previous test. Without it, changing that branch to "Warn
// on any err" would also turn the previous test green, and since deleting tasks is an everyday
// action, that would keep producing noise. "Guarding against misses and against false alarms
// must come in pairs", same as the two stripRouteIDs tests in feedback.
func TestRescheduleOnlyIsSilentWhenScheduleDeleted(t *testing.T) {
	log, buf := captureCoreLogs()
	sch, _, _, _, _, _ := newTestScheduler(t)
	sch.log = log

	// No seed, just use an id that does not exist: the store returns ErrNotFound.
	sch.rescheduleOnly(context.Background(), apimodel.Schedule{ID: "sch_不存在"})

	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("任务已删是常态，不该打 Warn，否则日常删任务刷噪音。实际输出：%s", buf.String())
	}
}

// ⭐ A task paused by the user is not rescheduled, and no log either.
//
// Same as above, this is also the normal case: paused means "stop running".
func TestRescheduleOnlyIsSilentWhenPaused(t *testing.T) {
	log, buf := captureCoreLogs()
	sch, svc, store, _, _, _ := newTestScheduler(t)
	sch.log = log

	seeded := seedSchedule(t, svc, nil)
	cur, err := store.GetSchedule(context.Background(), seeded.ID)
	if err != nil {
		t.Fatalf("读任务失败：%v", err)
	}
	cur.Status = apimodel.SchedulePaused
	if _, err := store.SaveSchedule(context.Background(), cur); err != nil {
		t.Fatalf("暂停任务失败：%v", err)
	}

	sch.rescheduleOnly(context.Background(), seeded)

	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("用户暂停是常态，不该打 Warn。实际输出：%s", buf.String())
	}
}

// errGetRun is the injected failure, also deliberately not ErrNotFound.
var errGetRun = errors.New("pg: context deadline exceeded")

// flakyRunStore makes GetRun fail and passes other methods through.
type flakyRunStore struct {
	Store
	failGet bool
}

func (s *flakyRunStore) GetRun(ctx context.Context, id string) (apimodel.Run, error) {
	if s.failGet {
		return apimodel.Run{}, errGetRun
	}
	return s.Store.GetRun(ctx, id)
}

// ⭐ When the re-read before writing back the run fails, it must leave a trace.
//
// The comment on that return claims "the run was deleted", but GetRun's errors mix "PG hiccup /
// 5 second writeCtx timeout" with ErrNotFound. Reaching it means status / eventCount /
// lastActivityAt / usage are **all left un-updated**: the session stays at running forever, with
// 0 events and 0 tokens, and drops to the bottom of the list because lastActivityAt was not
// refreshed. The user sees "the conversation is running, but the history looks untouched".
//
// The upstream "failed to store event" Warn only covers the AppendRunEvent step; this step comes
// after it and is completely silent.
//
// Mutation check: delete this Warn and this test turns red.
func TestApplyEventToRunLogsWhenRereadFails(t *testing.T) {
	log, buf := captureCoreLogs()
	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败：%v", err)
	}
	svc, err := New(Config{Store: &flakyRunStore{Store: store, failGet: true}, Logger: log})
	if err != nil {
		t.Fatalf("装配 Service 失败：%v", err)
	}

	svc.applyEventToRun(context.Background(), "run_x", apimodel.RunEvent{
		Type: "session.status_idle", Sequence: 7,
	}, false)

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("重读失败必须留 Warn——否则会话永远停在 running 而日志零线索。实际输出：%s", out)
	}
	for _, want := range []string{"run_x", "session.status_idle"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺 %q，排查时要多猜一步。实际输出：%s", want, out)
		}
	}
}

// ⭐ A deleted run is the normal case, no log.
//
// Reverse control. The event stream is a high-frequency path; logging "run deleted" as a Warn
// too would flood the logs, and flooded logs are a failure in themselves.
func TestApplyEventToRunIsSilentWhenRunDeleted(t *testing.T) {
	log, buf := captureCoreLogs()
	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败：%v", err)
	}
	svc, err := New(Config{Store: store, Logger: log})
	if err != nil {
		t.Fatalf("装配 Service 失败：%v", err)
	}

	// This run does not exist → MemStore returns ErrNotFound.
	svc.applyEventToRun(context.Background(), "run_压根不存在", apimodel.RunEvent{
		Type: "session.status_idle", Sequence: 1,
	}, false)

	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("run 已删是常态，不该打 Warn，否则高频事件流刷屏。实际输出：%s", buf.String())
	}
}

// ⭐ User content does not go into this log.
//
// applyEventToRun holds the whole RunEvent, which may carry the user's message body.
// Only log the run id and the **event type enum**, not Raw.
func TestApplyEventToRunLogOmitsEventBody(t *testing.T) {
	log, buf := captureCoreLogs()
	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败：%v", err)
	}
	svc, err := New(Config{Store: &flakyRunStore{Store: store, failGet: true}, Logger: log})
	if err != nil {
		t.Fatalf("装配 Service 失败：%v", err)
	}

	const secret = "用户说的私密内容与 amak_live_secret_key"
	svc.applyEventToRun(context.Background(), "run_x", apimodel.RunEvent{
		Type:     "user.message",
		Sequence: 3,
		Raw:      []byte(`{"text":"` + secret + `"}`),
	}, false)

	out := buf.String()
	for _, leaked := range []string{secret, "amak_live_secret_key", "私密内容"} {
		if strings.Contains(out, leaked) {
			t.Errorf("事件正文 %q 泄漏进日志了。实际输出：%s", leaked, out)
		}
	}
	if !strings.Contains(out, "user.message") {
		t.Errorf("事件类型是枚举、该留着，实际输出：%s", out)
	}
}

// ---------------------------------------------------------------------------
// discardWork
// ---------------------------------------------------------------------------

// discardFakeAPI only implements the two methods discardWork uses.
//
// **Deliberately not reusing fakeWorkAPI from worker_poll_test.go**: that one grew out of
// pollLoop's timing assertions (long poll hanging, in-flight counts, reclaim inputs) and cannot
// fit the "make ack fail" need; adding a switch to it would change a pile of tests over there.
// All we need here are two controllable return values.
//
// It embeds the selfhosted.API interface (value nil) to satisfy the type: any unimplemented
// method panics as soon as it is called, which is exactly what we want. **It acts as an
// assertion that "discardWork should only touch these two methods".**
type discardFakeAPI struct {
	selfhosted.API

	ackErr  error
	stopErr error

	acked   bool
	stopped bool
}

func (f *discardFakeAPI) AckWork(context.Context, selfhosted.AckWorkRequest) error {
	f.acked = true
	return f.ackErr
}

func (f *discardFakeAPI) StopWork(context.Context, selfhosted.StopWorkRequest) error {
	f.stopped = true
	return f.stopErr
}

// fakeWorkItem builds a minimal work item.
func fakeWorkItem(id, envID string) selfhosted.WorkItem {
	return selfhosted.WorkItem{ID: id, EnvironmentID: envID}
}

// ⭐ When ack fails while discarding work, it must leave a trace.
//
// Before discarding, the caller logs "work has no session id, discarding", and that line
// **asserts it has already been discarded**. When ack fails it was not actually discarded, the
// next round picks up the same item, and the same Warn repeats every round. It looks like "Ark
// keeps sending the same bad work", while the real cause is that we never settled it.
// It points the exact opposite way, the most time-consuming kind of misdirection.
//
// Mutation check: delete this Warn and this test turns red.
func TestDiscardWorkLogsAckFailure(t *testing.T) {
	log, buf := captureCoreLogs()
	pool := &WorkerPool{log: slog.Default()}

	api := &discardFakeAPI{ackErr: errors.New("ark: 409 conflict")}
	pool.discardWork(context.Background(), api, fakeWorkItem("work_1", "env_1"), "worker_1", log)

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("ack 失败必须留 Warn，否则上游那条「丢弃」变成反复出现却不成立的断言。实际输出：%s", out)
	}
	for _, want := range []string{"work_1", "env_1", "409 conflict"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺 %q，实际输出：%s", want, out)
		}
	}
	if api.stopped {
		t.Error("ack 失败后不该再调 stop——既有语义，顺带钉住")
	}
}

// ⭐ When ack succeeds but stop fails, it must also leave a trace.
//
// Milder than the one above (the work only hangs until the reclaim window expires), but without a
// trace, "why did this work come back 90 seconds later" has no traceable source.
func TestDiscardWorkLogsStopFailure(t *testing.T) {
	log, buf := captureCoreLogs()
	pool := &WorkerPool{log: slog.Default()}

	api := &discardFakeAPI{stopErr: errors.New("ark: 500")}
	pool.discardWork(context.Background(), api, fakeWorkItem("work_2", "env_2"), "worker_1", log)

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("stop 失败要留痕，实际输出：%s", out)
	}
	if !strings.Contains(out, "reclaim") {
		t.Errorf("要说清它会怎么回来，实际输出：%s", out)
	}
}

// ⭐ No log when both steps succeed.
//
// Reverse control: a successful discard is the normal path, a Warn upstream already said so,
// and logging again here would just repeat it.
func TestDiscardWorkIsSilentOnSuccess(t *testing.T) {
	log, buf := captureCoreLogs()
	pool := &WorkerPool{log: slog.Default()}

	pool.discardWork(context.Background(), &discardFakeAPI{}, fakeWorkItem("work_3", "env_3"), "worker_1", log)

	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("丢弃成功不该打 Warn，上游已经说过了。实际输出：%s", buf.String())
	}
}
