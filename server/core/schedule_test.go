// Business-layer tests for schedules.
//
// All offline, no credentials needed. Times are always fixed instants (`time.Date(...)`), so
// there is no sleep, no flakiness, and no need to mock the clock. That is exactly the payoff of
// ComputeNextRun taking after as a parameter.
//
// The two groups most worth reading:
//
//   - TestComputeNextRunCronRespectsTZ / DST: the time zone must be computed by the server
//     (we cannot rely on telling the agent in the prompt to convert to UTC by hand).
//     "Weekdays 9:30" as written is local time; getting it wrong shows up as **everything
//     shifted by 8 hours**, with no error.
//   - TestSetScheduleStatusResumeClearsFailures: consecutive failures auto-pause the task. If
//     resuming does not reset the count, the user clicks "enable" and the first failure stops
//     it again, which looks like a broken button.

package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
)

// mustLoadTZ loads a time zone, and skips the whole test case if the machine has no tzdata.
//
// Not using Fatal is on purpose: the container may really have no tzdata (alpine has none by
// default). Then the test should be skipped instead of turning the whole package red; how the
// business code handles that case is covered by other cases.
func mustLoadTZ(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("本机没有 tzdata（%v），跳过时区相关断言", err)
	}
	return loc
}

// TestComputeNextRunCronRespectsTZ: "weekdays 9:30" means local 9:30, not UTC 9:30.
//
// This one is the core of the file. Missing the `after.In(loc)` step gives no error; it just
// shifts every cron task by the time zone offset. In Beijing that is 8 hours, so the morning
// check runs in the afternoon.
func TestComputeNextRunCronRespectsTZ(t *testing.T) {
	sh := mustLoadTZ(t, "Asia/Shanghai")

	spec := apimodel.ScheduleSpec{
		Kind: apimodel.ScheduleCron,
		Cron: "30 9 * * 1-5", // weekdays 9:30
		TZ:   "Asia/Shanghai",
	}

	// 2026-08-27 is a Thursday. Starting at 08:00 Beijing time, the next one should be **09:30
	// Beijing on the same day** (= UTC 01:30), not 09:30 UTC that day (= 17:30 Beijing).
	after := time.Date(2026, 8, 27, 8, 0, 0, 0, sh)
	got, err := ComputeNextRun(spec, after)
	if err != nil {
		t.Fatalf("算下次触发失败：%v", err)
	}
	want := apimodel.FormatISO(time.Date(2026, 8, 27, 9, 30, 0, 0, sh))
	if got != want {
		t.Errorf("下次触发应是北京 9:30（%s），实际 %s —— 差一个时区就说明 after 没转到目标时区",
			want, got)
	}

	// Passing a UTC instant must give the same result: cron works on the absolute instant + the
	// target time zone, regardless of which Location the caller's time.Time carries.
	gotUTC, err := ComputeNextRun(spec, after.UTC())
	if err != nil {
		t.Fatalf("算下次触发失败：%v", err)
	}
	if gotUTC != want {
		t.Errorf("入参的 Location 不该影响结果：传 UTC 得到 %s，传本地得到 %s", gotUTC, want)
	}
}

// TestComputeNextRunCronSkipsWeekend: a weekday expression must skip the weekend.
func TestComputeNextRunCronSkipsWeekend(t *testing.T) {
	sh := mustLoadTZ(t, "Asia/Shanghai")
	spec := apimodel.ScheduleSpec{Kind: apimodel.ScheduleCron, Cron: "30 9 * * 1-5", TZ: "Asia/Shanghai"}

	// 2026-08-28 is a Friday; after 10:00 (that day's 9:30 has passed) → next is Monday 8-31.
	after := time.Date(2026, 8, 28, 10, 0, 0, 0, sh)
	got, err := ComputeNextRun(spec, after)
	if err != nil {
		t.Fatalf("算下次触发失败：%v", err)
	}
	want := apimodel.FormatISO(time.Date(2026, 8, 31, 9, 30, 0, 0, sh))
	if got != want {
		t.Errorf("周五之后应跳到周一（%s），实际 %s", want, got)
	}
}

// TestComputeNextRunCronDST: in a time zone with DST, cron still uses local wall clock time.
//
// Uses America/New_York instead of Asia/Shanghai: China has no DST now, so it cannot test this.
// It guards against "treating the time zone as a fixed offset": storing a UTC offset would turn
// "every day 9:30" into 8:30 or 10:30 after a DST switch, wrong twice a year, by an hour each
// time.
func TestComputeNextRunCronDST(t *testing.T) {
	ny := mustLoadTZ(t, "America/New_York")
	spec := apimodel.ScheduleSpec{Kind: apimodel.ScheduleCron, Cron: "30 9 * * *", TZ: "America/New_York"}

	// US DST in 2026 starts at 2 a.m. on March 8. After crossing it, "every day 9:30" should
	// still be **local** 9:30 (the UTC offset goes from -5 to -4, but wall clock time stays).
	after := time.Date(2026, 3, 8, 5, 0, 0, 0, ny)
	got, err := ComputeNextRun(spec, after)
	if err != nil {
		t.Fatalf("算下次触发失败：%v", err)
	}
	want := apimodel.FormatISO(time.Date(2026, 3, 8, 9, 30, 0, 0, ny))
	if got != want {
		t.Errorf("夏令时切换当天仍应是本地 9:30（%s），实际 %s —— "+
			"差一小时说明把时区当成了固定偏移", want, got)
	}
}

// TestComputeNextRunInterval: the interval counts from the given instant.
func TestComputeNextRunInterval(t *testing.T) {
	after := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	spec := apimodel.ScheduleSpec{Kind: apimodel.ScheduleInterval, EveryMs: 5 * 60 * 1000}

	got, err := ComputeNextRun(spec, after)
	if err != nil {
		t.Fatalf("算下次触发失败：%v", err)
	}
	want := apimodel.FormatISO(after.Add(5 * time.Minute))
	if got != want {
		t.Errorf("下次触发应是 %s，实际 %s", want, got)
	}

	// Below the minimum must be an error, not silently allowed: a 1-second interval would keep
	// the task forever piled up on "the previous run is still going".
	tooSmall := apimodel.ScheduleSpec{Kind: apimodel.ScheduleInterval, EveryMs: 1000}
	if _, err := ComputeNextRun(tooSmall, after); err == nil {
		t.Error("间隔小于 1 分钟应报错")
	}
}

// TestComputeNextRunOneshotKeepsPastTime: an expired oneshot is returned as is, not dropped.
//
// This is on purpose: if the service was down for an hour, a "remind me in 1 hour" that came
// due in that window should fire once on wake-up instead of silently vanishing. "Delete after
// running" is guaranteed by the scheduler, so it does not fire repeatedly.
func TestComputeNextRunOneshotKeepsPastTime(t *testing.T) {
	past := apimodel.FormatISO(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	spec := apimodel.ScheduleSpec{Kind: apimodel.ScheduleOneshot, FireAt: past}

	got, err := ComputeNextRun(spec, time.Now().UTC())
	if err != nil {
		t.Fatalf("算下次触发失败：%v", err)
	}
	if got != past {
		t.Errorf("过期的 oneshot 应原样返回 %s，实际 %s —— 丢弃它会让停机期间的提醒消失",
			past, got)
	}
}

// TestValidateScheduleSpec: required fields and invalid values for the three kinds.
func TestValidateScheduleSpec(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spec    apimodel.ScheduleSpec
		wantErr bool
	}{
		{"合法 cron", apimodel.ScheduleSpec{Kind: apimodel.ScheduleCron, Cron: "30 9 * * 1-5"}, false},
		{"cron 为空", apimodel.ScheduleSpec{Kind: apimodel.ScheduleCron}, true},
		{"cron 段数不对", apimodel.ScheduleSpec{Kind: apimodel.ScheduleCron, Cron: "30 9 * *"}, true},
		{"cron 字段越界", apimodel.ScheduleSpec{Kind: apimodel.ScheduleCron, Cron: "99 9 * * *"}, true},
		{"合法 interval", apimodel.ScheduleSpec{Kind: apimodel.ScheduleInterval, EveryMs: 60000}, false},
		{"interval 太小", apimodel.ScheduleSpec{Kind: apimodel.ScheduleInterval, EveryMs: 1}, true},
		{"interval 为零", apimodel.ScheduleSpec{Kind: apimodel.ScheduleInterval}, true},
		{"合法 oneshot", apimodel.ScheduleSpec{
			Kind: apimodel.ScheduleOneshot, FireAt: "2026-08-27T10:00:00.000Z"}, false},
		{"oneshot 缺时刻", apimodel.ScheduleSpec{Kind: apimodel.ScheduleOneshot}, true},
		{"oneshot 时刻格式不对", apimodel.ScheduleSpec{
			Kind: apimodel.ScheduleOneshot, FireAt: "2026-08-27 10:00"}, true},
		{"未知 kind", apimodel.ScheduleSpec{Kind: "weekly"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateScheduleSpec(tc.spec)
			if tc.wantErr && err == nil {
				t.Error("应该报错但没报")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("不该报错但报了：%v", err)
			}
			// Errors are always ErrInvalidArgument (the handler maps it to 400).
			if err != nil && !errors.Is(err, ErrInvalidArgument) {
				t.Errorf("错误应可判定为 ErrInvalidArgument，实际 %v", err)
			}
		})
	}
}

// newScheduleDraft builds a valid input.
func newScheduleDraft() apimodel.ScheduleDraft {
	return apimodel.ScheduleDraft{
		AgentID: "agent_triage",
		Name:    "每日巡检",
		Spec: apimodel.ScheduleSpec{
			Kind: apimodel.ScheduleCron,
			Cron: "30 9 * * 1-5",
			TZ:   apimodel.DefaultScheduleTZ,
		},
		Prompt:   "汇总昨天的告警",
		Delivery: apimodel.DeliveryTarget{Kind: apimodel.DeliveryRunOnly},
	}
}

// TestCreateScheduleComputesNextRun: right after creation there should be a next fire time.
//
// Without it the task is never claimed, while the UI looks fine; it just "never runs".
func TestCreateScheduleComputesNextRun(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	sch, err := svc.CreateSchedule(ctx, seedWS, newScheduleDraft())
	if err != nil {
		t.Fatalf("建任务失败：%v", err)
	}
	if sch.NextRunAt == "" {
		t.Error("建完就该算出 nextRunAt，否则这条任务永远不会被认领")
	}
	if sch.Status != apimodel.ScheduleActive {
		t.Errorf("默认应是 active，实际 %s", sch.Status)
	}
	if sch.Origin != apimodel.OriginUser {
		t.Errorf("界面建的任务 origin 应是 user，实际 %s", sch.Origin)
	}
	// AgentName is a snapshot; after the agent is deleted the list page still needs it to show
	// who the task belonged to.
	if sch.AgentName == "" {
		t.Error("应存下 agent 名字快照")
	}
	if sch.ID == "" {
		t.Error("应分配 ID")
	}
}

// TestCreatePausedScheduleHasNoNextRun: a task created as paused should have no next fire time.
//
// If one were kept, it might already be in the past when the user clicks "enable", so it would
// run right away; getting a report the moment you enable it is not what the user wanted.
func TestCreatePausedScheduleHasNoNextRun(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	draft := newScheduleDraft()
	draft.Status = apimodel.SchedulePaused
	sch, err := svc.CreateSchedule(ctx, seedWS, draft)
	if err != nil {
		t.Fatalf("建任务失败：%v", err)
	}
	if sch.NextRunAt != "" {
		t.Errorf("paused 的任务不该有 nextRunAt，实际 %q", sch.NextRunAt)
	}
}

// TestCreateScheduleRejectsBadInput: various invalid inputs.
func TestCreateScheduleRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	t.Run("空间不存在", func(t *testing.T) {
		if _, err := svc.CreateSchedule(ctx, "ws_nope", newScheduleDraft()); !errors.Is(err, ErrNotFound) {
			t.Errorf("应是 ErrNotFound，实际 %v", err)
		}
	})
	t.Run("agent 不存在", func(t *testing.T) {
		d := newScheduleDraft()
		d.AgentID = "agent_nope"
		if _, err := svc.CreateSchedule(ctx, seedWS, d); !errors.Is(err, ErrNotFound) {
			t.Errorf("应是 ErrNotFound，实际 %v", err)
		}
	})
	t.Run("任务名为空", func(t *testing.T) {
		d := newScheduleDraft()
		d.Name = "   "
		if _, err := svc.CreateSchedule(ctx, seedWS, d); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("应是 ErrInvalidArgument，实际 %v", err)
		}
	})
	t.Run("prompt 为空", func(t *testing.T) {
		d := newScheduleDraft()
		d.Prompt = ""
		if _, err := svc.CreateSchedule(ctx, seedWS, d); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("应是 ErrInvalidArgument，实际 %v", err)
		}
	})
	t.Run("投递到飞书但没给群", func(t *testing.T) {
		d := newScheduleDraft()
		d.Delivery = apimodel.DeliveryTarget{Kind: apimodel.DeliveryFeishu, FeishuAppID: "fs_1"}
		if _, err := svc.CreateSchedule(ctx, seedWS, d); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("应是 ErrInvalidArgument，实际 %v", err)
		}
	})
}

// TestCreateScheduleRejectsCrossWorkspaceAgent: an agent from another workspace must not be
// referenced.
//
// A cross-workspace reference would make the task run with another workspace's Ark
// credentials, which is privilege escalation.
func TestCreateScheduleRejectsCrossWorkspaceAgent(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	other, err := svc.CreateWorkspace(ctx, WorkspaceInput{Name: "另一个", Key: "other-ws"})
	if err != nil {
		t.Fatalf("建空间失败：%v", err)
	}
	// agent_triage belongs to seedWS; using it to create a task in another workspace must be
	// rejected.
	if _, err := svc.CreateSchedule(ctx, other.ID, newScheduleDraft()); !errors.Is(err, ErrNotFound) &&
		!errors.Is(err, ErrInvalidArgument) {
		t.Errorf("跨空间引用 agent 应被拒，实际 %v", err)
	}
}

// TestUpdateScheduleRecomputesOnSpecChange: changing the spec must recompute the next fire time.
//
// Without recomputing, after changing "every day 9:30" to "every 5 minutes" the next run still
// follows the old expression, and the user thinks the change did not take effect.
func TestUpdateScheduleRecomputesOnSpecChange(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	sch, err := svc.CreateSchedule(ctx, seedWS, newScheduleDraft())
	if err != nil {
		t.Fatalf("建任务失败：%v", err)
	}
	before := sch.NextRunAt

	draft := newScheduleDraft()
	draft.Spec = apimodel.ScheduleSpec{Kind: apimodel.ScheduleInterval, EveryMs: 5 * 60 * 1000}
	updated, err := svc.UpdateSchedule(ctx, sch.ID, draft)
	if err != nil {
		t.Fatalf("改任务失败：%v", err)
	}
	if updated.NextRunAt == before {
		t.Error("改了 spec 就该重算 nextRunAt，否则新表达式看起来没生效")
	}
	if updated.Spec.Kind != apimodel.ScheduleInterval {
		t.Errorf("spec 没改上：%+v", updated.Spec)
	}
}

// TestSetScheduleStatusResumeClearsFailures: resuming must reset the consecutive failure count
// and recompute the next fire time.
//
// Reaching the consecutive failure threshold auto-pauses the task (to stop a cascade). Without
// the reset, after the user clicks "enable" the first failure stops it again. It looks like the
// button does nothing, while the real cause hides in a counter the user cannot see.
func TestSetScheduleStatusResumeClearsFailures(t *testing.T) {
	ctx := context.Background()
	svc, store := newService(t)

	sch, err := svc.CreateSchedule(ctx, seedWS, newScheduleDraft())
	if err != nil {
		t.Fatalf("建任务失败：%v", err)
	}

	// Set up the state "hit the consecutive failure threshold, already auto-paused".
	sch.Status = apimodel.SchedulePaused
	sch.ConsecutiveFailures = apimodel.MaxConsecutiveFailures
	sch.LastError = "上次挂了"
	sch.NextRunAt = ""
	if _, err := store.SaveSchedule(ctx, sch); err != nil {
		t.Fatalf("造状态失败：%v", err)
	}

	resumed, err := svc.SetScheduleStatus(ctx, sch.ID, apimodel.ScheduleActive)
	if err != nil {
		t.Fatalf("启用失败：%v", err)
	}
	if resumed.ConsecutiveFailures != 0 {
		t.Errorf("启用要清零连续失败数，实际 %d —— 否则下次一失败就又被自动暂停",
			resumed.ConsecutiveFailures)
	}
	if resumed.LastError != "" {
		t.Errorf("启用要清掉上次的错误，实际 %q", resumed.LastError)
	}
	if resumed.NextRunAt == "" {
		t.Error("启用要重算 nextRunAt，否则这条任务再也不会被认领")
	}

	// Pausing must clear nextRunAt, so it no longer enters the set of claim candidates.
	paused, err := svc.SetScheduleStatus(ctx, sch.ID, apimodel.SchedulePaused)
	if err != nil {
		t.Fatalf("暂停失败：%v", err)
	}
	if paused.NextRunAt != "" {
		t.Errorf("暂停后 nextRunAt 应为空，实际 %q", paused.NextRunAt)
	}
}

// TestRunScheduleNowMarksPausedForTrialRun: a trial run must leave an explicit mark on a paused
// task.
//
// **Setting nextRunAt to now is not enough**: claiming only picks tasks that are "active or had
// a trial run requested". Without this mark, the API returns 200 and nextRunAt is updated, but
// the scheduler never claims it. It shows up as "clicked trial run, nothing happened", with zero
// errors. The whole path is verified in TestSchedulerRunsPausedScheduleAfterRunNow.
func TestRunScheduleNowMarksPausedForTrialRun(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	draft := newScheduleDraft()
	draft.Status = apimodel.SchedulePaused
	sch, err := svc.CreateSchedule(ctx, seedWS, draft)
	if err != nil {
		t.Fatalf("建任务失败：%v", err)
	}
	if sch.RunNowRequested {
		t.Fatal("刚建出来的任务不该自带试跑标记")
	}

	fired, err := svc.RunScheduleNow(ctx, sch.ID)
	if err != nil {
		t.Fatalf("试跑失败：%v", err)
	}
	if !fired.RunNowRequested {
		t.Error("试跑要标记「这一轮是人工要的」，否则 paused 的任务永远不会被认领")
	}
	if fired.NextRunAt == "" {
		t.Error("试跑要把 nextRunAt 推到现在，标记要与一个待触发的时刻成对才生效")
	}
	// **Never switch it to active along the way**: that would kill the "set it up first, do not
	// run automatically" use case. After one trial run it would start running on schedule, which
	// the user never asked for.
	if fired.Status != apimodel.SchedulePaused {
		t.Errorf("试跑不该改 status，实际变成了 %q", fired.Status)
	}
}

// TestSetScheduleStatusRejectsUnknown: only active / paused are accepted.
func TestSetScheduleStatusRejectsUnknown(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	sch, err := svc.CreateSchedule(ctx, seedWS, newScheduleDraft())
	if err != nil {
		t.Fatalf("建任务失败：%v", err)
	}
	if _, err := svc.SetScheduleStatus(ctx, sch.ID, "deleted"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("未知状态应被拒，实际 %v", err)
	}
}

// TestScheduleAgentDeletedIsDerived: "agent deleted" is computed at read time, not stored.
//
// Same shape as Run.AgentDeleted. Storing it would make it a stale snapshot that never updates
// (it would still say "deleted" after the agent was recreated), with no symptoms at all.
func TestScheduleAgentDeletedIsDerived(t *testing.T) {
	ctx := context.Background()
	svc, store := newService(t)

	sch, err := svc.CreateSchedule(ctx, seedWS, newScheduleDraft())
	if err != nil {
		t.Fatalf("建任务失败：%v", err)
	}
	if sch.AgentDeleted {
		t.Error("agent 还在，不该标记已删除")
	}

	if err := store.DeleteAgent(ctx, "agent_triage"); err != nil {
		t.Fatalf("删 agent 失败：%v", err)
	}

	got, err := svc.GetSchedule(ctx, sch.ID)
	if err != nil {
		t.Fatalf("查任务失败：%v", err)
	}
	if !got.AgentDeleted {
		t.Error("agent 删了应标记 agentDeleted")
	}

	list, err := svc.ListSchedules(ctx, seedWS)
	if err != nil {
		t.Fatalf("列任务失败：%v", err)
	}
	if len(list) != 1 || !list[0].AgentDeleted {
		t.Error("列表里也要标记 agentDeleted")
	}

	// The stored copy must still be false: on a schedule read straight from the Store this field
	// is always false.
	raw, err := store.GetSchedule(ctx, sch.ID)
	if err != nil {
		t.Fatalf("直接读库失败：%v", err)
	}
	if raw.AgentDeleted {
		t.Error("派生字段不该落库——落了就是个永不更新的陈旧快照")
	}
}

// TestListSchedulesUnknownWorkspace: an unknown workspace returns an empty slice, not 404.
//
// The frontend listSchedules return type is an array, not nullable; a 404 would make the page
// throw instead of showing an empty list.
func TestListSchedulesUnknownWorkspace(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	list, err := svc.ListSchedules(ctx, "ws_nope")
	if err != nil {
		t.Fatalf("列表接口不该报错：%v", err)
	}
	if list == nil {
		t.Error("应返回空切片而不是 nil（nil 会编码成 null，前端 .map() 会炸）")
	}
	if len(list) != 0 {
		t.Errorf("应为空，实际 %d 条", len(list))
	}
}

// TestGetScheduleNotFoundMessage: the not-found error must be recognized as ErrNotFound and have
// readable text.
func TestGetScheduleNotFoundMessage(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	_, err := svc.GetSchedule(ctx, "sch_nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("应是 ErrNotFound，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "定时任务不存在") {
		t.Errorf("错误文案要能直接给用户看，实际 %q", err.Error())
	}
}

// TestDeleteScheduleIsIdempotent: deleting a missing one is not an error.
func TestDeleteScheduleIsIdempotent(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	if err := svc.DeleteSchedule(ctx, "sch_nope"); err != nil {
		t.Errorf("删不存在的应当幂等，实际 %v", err)
	}
}
