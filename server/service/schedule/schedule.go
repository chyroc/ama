// Business layer of scheduled tasks: CRUD invariants + how "when does it run next" is computed.
//
// "How it runs when the time comes" is not in this package but in core/scheduler.go: that is the
// long-running background part, and it drives the whole chat path (create an Ark Session, store a
// Run, deliver back to Feishu), while the run / chat / ark domains have not been split out yet.
// **This step deliberately only moves the entity's CRUD and validation**; see the passage at the end
// of this file for why.
//
// # Time is always passed in as a parameter; no clock interface
//
// ComputeNextRun(spec, after) takes "now" as a parameter instead of calling time.Now() inside. The
// reason: what this domain needs to assert (is the next 9:30 on a weekday a Monday or a Friday,
// where an interval starts from, what happens on a daylight saving day) **is all a function of
// time**; given a fixed moment it can be asserted, with no mock library and no sleep.
//
// # cron uses robfig/cron/v3, not a hand-written parser
//
// A hand-written 5-field parser would save one dependency, but it is not worth it, for two reasons:
//
// 1. **Hand-written parsers easily get real bugs**: e.g. writing
// `strings.ReplaceAll(dowField, "7", "0")` to normalize Sunday's 7 to 0 turns `*/7` into `*/0`.
// A hand-written parser would also have to maintain the OR semantics of dom/dow and a guard against
// the endless loop of "no match within a year".
//
// 2. **Time zones come for free**. robfig's ParseStandard with time.LoadLocation can compute the
// next local time, so there is no need to tell the agent in the system prompt to convert Beijing
// time to UTC by hand. "Weekdays at 9:30" is written in local time, and a hard-coded UTC offset
// would also be pushed off twice a year by daylight saving time.
//
// # ⚠️ Sentinel errors come from model/persistmodel, not core
//
// Domain packages do not import core (that is exactly what the package split is for). The few
// `ErrInvalidArgument` on the core side are **aliases with the same name** (the same variable), so
// the handler's mapping and the 400-odd `errors.Is` calls did not change at all.
package schedule

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// cronParser is the parser of standard 5-field cron (minute hour day month weekday).
//
// It deliberately **does not enable** cron.Descriptor (@daily and the like) or a seconds field:
//   - seconds: the scheduler scans on the order of seconds and one agent conversation takes
//     minutes; supporting seconds would only let users write tasks that pile up forever;
//   - @daily: whether it means UTC midnight or local midnight is a separate question, while
//     `0 0 * * *` says the same thing without ambiguity.
//
// ParseStandard itself is "5 fields + an optional @descriptor", so it is used here instead of
// building our own Parser: building one means listing the bits of five fields explicitly, and a
// wrong one gives no error, it just makes some kind of expression silently parse into something
// else.
var cronParser = cron.ParseStandard

// scheduleLocation resolves a time zone name; on failure it falls back to UTC and returns the
// reason.
//
// **The caller must know about a failure**; it must not silently fall back to UTC: the container
// image may lack tzdata (alpine does by default), and then LoadLocation("Asia/Shanghai") fails.
// Silently falling back shows up as "the weekday 9:30 task runs at 17:30", 8 hours off, and the
// error message does not point to the missing time zone data at all.
func scheduleLocation(tz string) (*time.Location, error) {
	name := strings.TrimSpace(tz)
	if name == "" {
		name = apimodel.DefaultScheduleTZ
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		// The library's error goes in the middle and the user-readable sentence last: the handler's
		// userMessage takes the part after the last ": ", and the other order would cut the message
		// down to a string of English (the trap described in the comment in
		// ValidateScheduleSpec).
		return time.UTC, fmt.Errorf("%w: 加载时区失败（%v），时区名 %q 可能不对，"+
			"或者容器里没装 tzdata", persistmodel.ErrInvalidArgument, err, name)
	}
	return loc, nil
}

// ValidateScheduleSpec validates the trigger expression.
//
// Each of the three kinds checks its own required fields; this is where to fail fast: if a task
// with a wrong cron could be stored, it would never run (or could never compute its next time in
// any period), and the user could not see in the UI what is wrong.
func ValidateScheduleSpec(spec apimodel.ScheduleSpec) error {
	switch spec.Kind {
	case apimodel.ScheduleCron:
		expr := strings.TrimSpace(spec.Cron)
		if expr == "" {
			return fmt.Errorf("%w: cron 表达式不能为空", persistmodel.ErrInvalidArgument)
		}
		if _, err := cronParser(expr); err != nil {
			// **Do not put the library's raw error into this sentence.** The handler's
			// userMessage takes the part after the last ": ", and robfig's errors contain colons
			// (`failed to parse int from 99: strconv.Atoi: ...`); including it would cut the message
			// the user sees in the middle, leaving something like "99) — five-field format is...".
			// This trap gives no compile error; the handler tests caught it.
			//
			// That English is also useless to the user (it mentions strconv), so an example of the
			// correct form is given instead: the user wants "how to write it right", not "at which
			// byte the parser failed".
			return fmt.Errorf("%w: cron 表达式无效，五段格式是「分 时 日 月 周」，"+
				"例如工作日 9:30 写成 30 9 * * 1-5", persistmodel.ErrInvalidArgument)
		}
		if _, err := scheduleLocation(spec.TZ); err != nil {
			return err
		}
	case apimodel.ScheduleInterval:
		if spec.EveryMs < apimodel.MinScheduleIntervalMs {
			return fmt.Errorf("%w: 间隔不能短于 1 分钟", persistmodel.ErrInvalidArgument)
		}
	case apimodel.ScheduleOneshot:
		if strings.TrimSpace(spec.FireAt) == "" {
			return fmt.Errorf("%w: 一次性任务要给触发时刻", persistmodel.ErrInvalidArgument)
		}
		if _, err := time.Parse(persistmodel.ISOLayout, spec.FireAt); err != nil {
			return fmt.Errorf("%w: 触发时刻格式不对（要形如 2026-08-27T10:00:00.000Z）",
				persistmodel.ErrInvalidArgument)
		}
	default:
		return fmt.Errorf("%w: 未知的触发类型 %q（只支持 cron / interval / oneshot）",
			persistmodel.ErrInvalidArgument, spec.Kind)
	}
	return nil
}

// ComputeNextRun computes the next trigger time after `after`, returned as a fixed-length ISO string.
//
// An empty string means **there is no next time** (only for oneshot: it should be deleted after it
// runs).
//
// The differences between the three kinds are worth remembering:
//
//	cron      computed from the expression in the **given time zone**; depends on after's absolute
//	          moment, not on its time zone
//	interval  EveryMs after `after`; **not a fixed grid**, see below
//	oneshot   just FireAt itself, even if it is already past (then the scheduler runs it once right away)
//
// **Why interval does not use a fixed grid**: the after passed in by the caller is "when the previous
// run finished", so the real period is EveryMs + the duration of that run. With a fixed grid (last
// planned time + EveryMs), when a run takes longer than the interval, the next one is due at once
// or even catches up several times in a row, eating the interval. For a need like "inspect every 5
// minutes", users want "5 minutes between two runs", not "a slot every 5 minutes".
func ComputeNextRun(spec apimodel.ScheduleSpec, after time.Time) (string, error) {
	switch spec.Kind {
	case apimodel.ScheduleCron:
		sched, err := cronParser(strings.TrimSpace(spec.Cron))
		if err != nil {
			// Same as above: do not put the library's error at the end, or the user sees truncated
			// English.
			return "", fmt.Errorf("%w: cron 表达式无效，五段格式是「分 时 日 月 周」",
				persistmodel.ErrInvalidArgument)
		}
		loc, err := scheduleLocation(spec.TZ)
		if err != nil {
			return "", err
		}
		// **after must be converted to the target time zone before handing it to robfig.**
		// cron.Schedule.Next does calendar arithmetic in the Location of the moment passed in (it
		// uses Hour() / Weekday() and so on); passing a UTC moment gives "9:30 UTC", 8 hours off.
		// Missing this step gives no error; it just shifts every cron task by the time zone offset.
		next := sched.Next(after.In(loc))
		if next.IsZero() {
			// robfig returns the zero value when "there is no match within a year" (e.g. February 30).
			return "", fmt.Errorf("%w: 这个 cron 表达式一年内不会触发（检查日和月是否矛盾）",
				persistmodel.ErrInvalidArgument)
		}
		return apimodel.FormatISO(next), nil

	case apimodel.ScheduleInterval:
		if spec.EveryMs < apimodel.MinScheduleIntervalMs {
			return "", fmt.Errorf("%w: 间隔不能短于 1 分钟", persistmodel.ErrInvalidArgument)
		}
		return apimodel.FormatISO(after.Add(time.Duration(spec.EveryMs) * time.Millisecond)), nil

	case apimodel.ScheduleOneshot:
		// Return it unchanged, **without checking whether it is already past**: an expired oneshot
		// should run once right away (e.g. the service was down for an hour, and a reminder due in
		// that time is sent when it comes back) instead of being silently dropped.
		// "Delete after running" is guaranteed by the scheduler, so it does not fire again and again.
		if strings.TrimSpace(spec.FireAt) == "" {
			return "", fmt.Errorf("%w: 一次性任务缺少触发时刻", persistmodel.ErrInvalidArgument)
		}
		return spec.FireAt, nil

	default:
		return "", fmt.Errorf("%w: 未知的触发类型 %q", persistmodel.ErrInvalidArgument, spec.Kind)
	}
}

// ---------------------------------------------------------------------------
// CRUD on the Service side
// ---------------------------------------------------------------------------

// ListSchedules lists the scheduled tasks of a workspace.
//
// A missing workspace returns an empty slice instead of 404, consistent with listAgents / listRuns
// (list endpoints never 404; those frontend methods return arrays, not nullables).
//
// It also marks "the related agent was deleted", exactly the same way as ListRuns (load all agents
// at once into a set, not one query each, which would be N+1).
func (s *Service) ListSchedules(ctx context.Context, workspaceID string) ([]apimodel.Schedule, error) {
	list, err := s.store.ListSchedules(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	agents, err := s.store.ListAgents(ctx, workspaceID)
	if err != nil {
		// fail-open: a database hiccup should not show up as "your agent was deleted", a false
		// statement the user cannot act on.
		s.log().Warn("列 agent 失败，定时任务暂不标记已删除",
			"workspace", workspaceID, "error", err.Error())
		return list, nil
	}
	alive := make(map[string]struct{}, len(agents))
	for _, a := range agents {
		alive[a.ID] = struct{}{}
	}
	for i := range list {
		if _, ok := alive[list[i].AgentID]; !ok {
			list[i].AgentDeleted = true
		}
	}
	return list, nil
}

// GetSchedule gets one scheduled task; returns ErrNotFound if it does not exist.
func (s *Service) GetSchedule(ctx context.Context, id string) (apimodel.Schedule, error) {
	sch, err := s.store.GetSchedule(ctx, id)
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			return apimodel.Schedule{}, fmt.Errorf("%w: 定时任务不存在", persistmodel.ErrNotFound)
		}
		return apimodel.Schedule{}, err
	}
	s.markScheduleAgentDeleted(ctx, &sch)
	return sch, nil
}

// markScheduleAgentDeleted computes "the related agent was deleted" on the spot.
//
// **Only ErrNotFound counts; other errors fail open**: saying "your agent was deleted" during a
// database hiccup is false, and would make the UI mark this task as unusable. Same discipline as
// markAgentDeleted.
func (s *Service) markScheduleAgentDeleted(ctx context.Context, sch *apimodel.Schedule) {
	_, err := s.store.GetAgent(ctx, sch.AgentID)
	switch {
	case err == nil:
	case errors.Is(err, persistmodel.ErrNotFound):
		sch.AgentDeleted = true
	default:
		s.log().Warn("查 agent 失败，本次不标记已删除",
			"schedule", sch.ID, "error", err.Error())
	}
}

// CreateSchedule creates a scheduled task.
//
// NextRunAt is computed right after creation, **unless it is created paused**: a paused task has no
// next trigger time; keeping one could make it already expired the moment it is enabled, so it runs
// once immediately (the user just clicked enable and gets a report, not what they wanted). It is
// recomputed on enable; see SetScheduleStatus.
func (s *Service) CreateSchedule(
	ctx context.Context, workspaceID string, draft apimodel.ScheduleDraft,
) (apimodel.Schedule, error) {
	if _, err := s.store.GetWorkspace(ctx, workspaceID); err != nil {
		return apimodel.Schedule{}, fmt.Errorf("%w: 空间不存在", persistmodel.ErrNotFound)
	}
	agent, err := s.validateScheduleDraft(ctx, workspaceID, draft)
	if err != nil {
		return apimodel.Schedule{}, err
	}

	status := draft.Status
	if status == "" {
		status = apimodel.ScheduleActive
	}

	now := apimodel.NowISO()
	actor := auth.ActorFrom(ctx)
	sch := apimodel.Schedule{
		ID:             s.nextID("sch"),
		WorkspaceID:    workspaceID,
		AgentID:        draft.AgentID,
		AgentName:      agent.Name,
		Name:           strings.TrimSpace(draft.Name),
		Spec:           draft.Spec,
		Prompt:         draft.Prompt,
		ContextSummary: draft.ContextSummary,
		Origin:         apimodel.OriginUser,
		Status:         status,
		Delivery:       draft.Delivery,
		CreatedAt:      now,
		UpdatedAt:      now,
		// Who configured it. **Only taken from the context**; ScheduleDraft has no such fields.
		//
		// This is the only entry point with Origin user, so the stamp always has a value (except for
		// callers with no identity). The self-wakeup path never goes through this function: it calls
		// store.CreateSchedule directly, so those two fields are naturally empty; see the passage on
		// apimodel.Schedule.CreatedBy.
		CreatedBy: actor,
		UpdatedBy: actor,
	}
	if status == apimodel.ScheduleActive {
		next, err := ComputeNextRun(sch.Spec, time.Now().UTC())
		if err != nil {
			return apimodel.Schedule{}, err
		}
		sch.NextRunAt = next
	}
	return s.store.CreateSchedule(ctx, sch)
}

// UpdateSchedule changes a scheduled task.
//
// **Changing the spec means recomputing NextRunAt**: without it, after changing "every day at 9:30"
// to "every hour", the next run would still be at the time of the old expression, and the user
// would think it did not take effect. The base for recomputing is "now", not the old NextRunAt,
// which belongs to the old expression.
func (s *Service) UpdateSchedule(
	ctx context.Context, id string, draft apimodel.ScheduleDraft,
) (apimodel.Schedule, error) {
	return s.UpdateScheduleWithReason(ctx, id, draft, apimodel.RevisionReasonEdit)
}

// UpdateScheduleWithReason is the implementation of UpdateSchedule, with an extra reason for the
// history.
//
// **It is public because rollback needs it**: when the revision domain writes a snapshot back into a
// task, it goes through here, with reason RevisionReasonRollback (see the schedule entry in
// core/revision.go).
// ⚠️ **Rollback must go through here, not recordRevision + SaveSchedule on its own**: that would
// record two history entries, and would bypass "recompute NextRunAt when the spec changed". Rolling
// back to an old spec is exactly a spec change; without recomputing, the task would keep running on
// the current expression while the UI shows the old one.
func (s *Service) UpdateScheduleWithReason(
	ctx context.Context, id string, draft apimodel.ScheduleDraft, reason apimodel.RevisionReason,
) (apimodel.Schedule, error) {
	cur, err := s.store.GetSchedule(ctx, id)
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			return apimodel.Schedule{}, fmt.Errorf("%w: 定时任务不存在", persistmodel.ErrNotFound)
		}
		return apimodel.Schedule{}, err
	}
	agent, err := s.validateScheduleDraft(ctx, cur.WorkspaceID, draft)
	if err != nil {
		return apimodel.Schedule{}, err
	}

	specChanged := cur.Spec != draft.Spec
	// Record a snapshot before the change, for rolling back after a mistake. It sits after
	// validation and before assignment, the same as UpdateAgent.
	//
	// ⚠️ The snapshot only has the ScheduleDraft fields; run-time state (NextRunAt / RunCount /
	// ConsecutiveFailures) never goes in; see the comment on Snapshot.
	if before := Snapshot(cur); !sameScheduleDraft(before, draft) {
		s.revisions.Record(ctx, apimodel.RevisionKindSchedule, cur.WorkspaceID, cur.ID,
			cur.Name, before, reason)
	}
	cur.AgentID = draft.AgentID
	cur.AgentName = agent.Name
	cur.Name = strings.TrimSpace(draft.Name)
	cur.Spec = draft.Spec
	cur.Prompt = draft.Prompt
	cur.ContextSummary = draft.ContextSummary
	cur.Delivery = draft.Delivery
	if draft.Status != "" {
		cur.Status = draft.Status
	}
	cur.UpdatedAt = apimodel.NowISO()
	// Who changed it. **Only UpdatedBy is touched; CreatedBy is the one read back above** (same as
	// UpdateAgent).
	//
	// ⚠️ Changing a self-wakeup task gives it an UpdatedBy without a CreatedBy; that shape is right,
	// truthfully saying "scheduled by an agent, later changed by a person". Do not fill in CreatedBy
	// here just to "look tidy".
	cur.UpdatedBy = auth.ActorFrom(ctx)

	switch {
	case cur.Status != apimodel.ScheduleActive:
		// Paused means no next trigger time.
		cur.NextRunAt = ""
	case specChanged || cur.NextRunAt == "":
		next, err := ComputeNextRun(cur.Spec, time.Now().UTC())
		if err != nil {
			return apimodel.Schedule{}, err
		}
		cur.NextRunAt = next
	}
	return s.store.SaveSchedule(ctx, cur)
}

// SetScheduleStatus enables or pauses a task.
//
// Enabling does three things; missing any of them causes visible strange problems:
//
//   - **Recompute NextRunAt**: there is none while paused; without computing it this task would
//     never be claimed again;
//   - **The base is "now"**, not the time before the pause: computing from the old time would make
//     it due at once, and the user would get a report right after clicking enable;
//   - **Reset ConsecutiveFailures to zero**: reaching the threshold of consecutive failures pauses
//     it automatically (to prevent a cascade); without resetting, the first failure after a manual
//     enable would pause it again, and the user would think the button does nothing.
func (s *Service) SetScheduleStatus(
	ctx context.Context, id string, status apimodel.ScheduleStatus,
) (apimodel.Schedule, error) {
	if status != apimodel.ScheduleActive && status != apimodel.SchedulePaused {
		return apimodel.Schedule{}, fmt.Errorf("%w: 状态只能是 active 或 paused",
			persistmodel.ErrInvalidArgument)
	}
	cur, err := s.store.GetSchedule(ctx, id)
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			return apimodel.Schedule{}, fmt.Errorf("%w: 定时任务不存在", persistmodel.ErrNotFound)
		}
		return apimodel.Schedule{}, err
	}

	cur.Status = status
	cur.UpdatedAt = apimodel.NowISO()
	// Enabling/pausing is a real user operation and must leave a trace (same as
	// SetAgentIntegrations).
	cur.UpdatedBy = auth.ActorFrom(ctx)
	if status == apimodel.ScheduleActive {
		next, err := ComputeNextRun(cur.Spec, time.Now().UTC())
		if err != nil {
			return apimodel.Schedule{}, err
		}
		cur.NextRunAt = next
		cur.ConsecutiveFailures = 0
		cur.LastError = ""
	} else {
		cur.NextRunAt = ""
	}
	return s.store.SaveSchedule(ctx, cur)
}

// DeleteSchedule deletes a scheduled task; a missing one is not an error (safe to repeat).
func (s *Service) DeleteSchedule(ctx context.Context, id string) error {
	if err := s.store.DeleteSchedule(ctx, id); err != nil {
		return err
	}
	// The task is gone, so its history is cleared too (same as DeleteAgent).
	s.revisions.Drop(ctx, apimodel.RevisionKindSchedule, id)
	return nil
}

// RunScheduleNow changes a task's next trigger time to now, so the scheduler runs it on its next
// tick.
//
// # Why "change the time" instead of "run one round right here"
//
// Running right here would make the HTTP handler wait synchronously for an agent conversation
// (possibly ten minutes); the request would surely time out while the task actually runs in the
// background: the user sees a failure while it succeeds, the worst kind of inconsistency.
//
// Changing the time hands "who runs it" to the scheduler, which gives three things for free: atomic
// claiming (with multiple instances a double click by the user does not run two rounds), failure
// counting and automatic pausing work as usual, and there is only one copy of the write-back logic
// after a run. The cost is that the user waits up to one scan period (30 seconds) before results
// start coming.
//
// **Paused tasks can also be test-run**: that is exactly the "configure it first, click once to see
// if it is right, then enable" use. A test run does not change Status: after the run, the finish
// side sees it is still paused and does not schedule a next one.
//
// # This promise only holds thanks to the RunNowRequested flag
//
// The claim SQL only claims tasks that are "active or clicked for a test run" (see
// ClaimDueSchedules in store_pg_schedule.go), so just setting next_run_at to now **has no effect at
// all on paused tasks**. This was hit while building the inspection loop: run-now returned 200 and
// the row's nextRunAt even updated in the frontend, looking exactly like success, while the
// scheduler never claimed it, and a whole scan period was wasted waiting.
//
// There were two possible fixes; "add a column as an explicit flag" was chosen over "return 422 for
// paused", because README-inspect.md had already recorded a workaround for this defect ("create the
// schedule as active directly"), and that workaround kills exactly the "configure first, do not run
// automatically" use, which is why paused exists. The other half of the reason is that the finish
// side was already right (it does not schedule a next one when it sees paused), so the whole promise
// was only one column away from holding.
//
// **The flag must be an explicit column, not inferred from "next_run_at is due"**: the next_run_at
// left over from before the pause stays in the past, and inferring from it would make every task
// run at once **the moment it is paused**.
//
// ⚠️ **Do not turn the paused task into active on the way**: that would remove the "configure first,
// do not run automatically" use; after one test run it would start running on schedule, which the
// user never asked for.
func (s *Service) RunScheduleNow(ctx context.Context, id string) (apimodel.Schedule, error) {
	cur, err := s.store.GetSchedule(ctx, id)
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			return apimodel.Schedule{}, fmt.Errorf("%w: 定时任务不存在", persistmodel.ErrNotFound)
		}
		return apimodel.Schedule{}, err
	}

	// Without the agent there can be no result, so say so plainly instead of letting it fail once
	// when it is due.
	//
	// **Use ErrPrecondition (422), not ErrNotFound**: the task itself exists; it is "this operation
	// is not allowed right now". With 404 the frontend would turn it into null and the page would
	// become a silent empty state (same as how AppendRunEvents handles frozen sessions).
	if _, aerr := s.store.GetAgent(ctx, cur.AgentID); aerr != nil {
		if errors.Is(aerr, persistmodel.ErrNotFound) {
			return apimodel.Schedule{}, fmt.Errorf(
				"%w: 关联的 Agent 已被删除，这条任务跑不出结果，请改绑一个 Agent",
				persistmodel.ErrPrecondition)
		}
		return apimodel.Schedule{}, aerr
	}

	cur.NextRunAt = apimodel.NowISO()
	// **Mark "this round was asked for by a person"**; it is consumed at the moment of claiming.
	// Paused tasks can only be claimed thanks to it; see this function's comment and
	// apimodel.Schedule.RunNowRequested. Active tasks do not really need it (the status side already
	// lets them through), but always setting it is simpler, and it also leaves a record in the
	// database of "who asked for this trigger".
	cur.RunNowRequested = true
	cur.UpdatedAt = apimodel.NowISO()
	// Test runs also leave a trace: the comment above says "leaves a record in the database of who
	// asked for this trigger", but RunNowRequested can only say "a person asked", not who; this line
	// fills in the second half.
	cur.UpdatedBy = auth.ActorFrom(ctx)
	return s.store.SaveSchedule(ctx, cur)
}

// validateScheduleDraft validates the input and returns the related agent.
//
// The agent must exist and belong to the same workspace: a cross-workspace reference would make a
// task run with another workspace's credentials, which is overreach. **The agent's name is taken
// out and stored as a snapshot**, so the list page can still say who it belonged to after the agent
// is deleted.
func (s *Service) validateScheduleDraft(
	ctx context.Context, workspaceID string, draft apimodel.ScheduleDraft,
) (apimodel.Agent, error) {
	if strings.TrimSpace(draft.Name) == "" {
		return apimodel.Agent{}, fmt.Errorf("%w: 任务名不能为空", persistmodel.ErrInvalidArgument)
	}
	if strings.TrimSpace(draft.Prompt) == "" {
		// A task without a prompt would not know what to do when due: the agent would get an empty
		// message and reply with something random.
		return apimodel.Agent{}, fmt.Errorf("%w: 要给 agent 的指令不能为空",
			persistmodel.ErrInvalidArgument)
	}
	if err := ValidateScheduleSpec(draft.Spec); err != nil {
		return apimodel.Agent{}, err
	}
	if err := validateDelivery(draft.Delivery); err != nil {
		return apimodel.Agent{}, err
	}
	if draft.Status != "" &&
		draft.Status != apimodel.ScheduleActive && draft.Status != apimodel.SchedulePaused {
		return apimodel.Agent{}, fmt.Errorf("%w: 状态只能是 active 或 paused",
			persistmodel.ErrInvalidArgument)
	}

	agent, err := s.store.GetAgent(ctx, draft.AgentID)
	if err != nil {
		return apimodel.Agent{}, fmt.Errorf("%w: Agent 不存在", persistmodel.ErrNotFound)
	}
	if agent.WorkspaceID != workspaceID {
		return apimodel.Agent{}, fmt.Errorf("%w: Agent 不属于该空间", persistmodel.ErrInvalidArgument)
	}
	return agent, nil
}

// validateDelivery validates the delivery target.
func validateDelivery(d apimodel.DeliveryTarget) error {
	switch d.Kind {
	case "", apimodel.DeliveryRunOnly:
		// Empty means run_only: nothing is pushed out, and the result is just a Run in the run history.
		return nil
	case apimodel.DeliveryFeishu:
		if strings.TrimSpace(d.FeishuAppID) == "" {
			return fmt.Errorf("%w: 投递到飞书要指定应用", persistmodel.ErrInvalidArgument)
		}
		if strings.TrimSpace(d.ChatID) == "" {
			return fmt.Errorf("%w: 投递到飞书要指定群", persistmodel.ErrInvalidArgument)
		}
		return nil
	default:
		// ⚠️ `issue` gets here and is rejected (400), **on purpose**: that path was removed; see the
		// passage on apimodel.DeliveryKind. Inspection now uses run_only + the agent creating an issue
		// itself.
		return fmt.Errorf("%w: 未知的投递方式 %q", persistmodel.ErrInvalidArgument, d.Kind)
	}
}
