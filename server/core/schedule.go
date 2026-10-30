// Schedules: 7 forwarding methods + two aliases used by the scheduler / tests.
// **The business logic has already moved to service/schedule.**
//
// # Those 7 empty shells are a transition state, not a design
//
// The logic lives in service/schedule; this file only forwards calls unchanged. The only reason
// to keep them is that **methods the handler cannot reach still need core as the fallback**:
// the orchestration layer's fallback interface (orchestrator.Fallback) requires those 7
// methods on *core.Service:
//
//	cmd/amad/orchestrator.go var _ orchestrator.Fallback = (*core.Service)(nil)
//
// Remove the methods from *Service and that assertion fails to compile.
//
// So this step on purpose only moves the logic and leaves the method set alone: **the value of
// this step is "the schedule business logic no longer lives in core"**, and the check is that
// `go list -deps ./service/schedule` does not include core.
// The shells will be deleted together with the separate change that "moves the handler out of
// core".
//
// ⚠️ **Do not add logic here.** It looks like a normal schedule.go, but any check written here
// bypasses service/schedule: the two entry points give different answers for the same thing,
// and nothing reports an error. Add things in service/schedule/schedule.go.
//
// # ⚠️ The two aliases below are not a transition state; scheduler.go really uses them
//
// ComputeNextRun and ValidateScheduleSpec are **function value aliases** (not forwarding
// functions), so `core.ComputeNextRun` and `schedulesvc.ComputeNextRun` are the same thing.
// There is no way for "two implementations to drift apart".
//
// They have two real callers:
//
//	core/scheduler.go   computes the next fire time after a run (three places)
//	core/schedule_test.go the 6 pure-function cases (time zone / DST / interval / expired oneshot)
//
// The scheduler has not moved yet (see the file header of service/schedule/service.go for
// why), so these two aliases will go with it, not with the handler step.
package core

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
	schedulesvc "github.com/chyroc/ama/server/service/schedule"
)

// ComputeNextRun computes the next fire time. The real implementation is in service/schedule.
//
// **It is an alias, not a forwarding function**, so it cannot drift from the domain package's
// version.
var ComputeNextRun = schedulesvc.ComputeNextRun

// ValidateScheduleSpec validates the trigger expression. The real implementation is in
// service/schedule. Same as above, it is an alias.
var ValidateScheduleSpec = schedulesvc.ValidateScheduleSpec

// ---------------------------------------------------------------------------
// Forwarding: the 6 used by the handler (transition state)
// ---------------------------------------------------------------------------

func (s *Service) ListSchedules(ctx context.Context, workspaceID string) ([]apimodel.Schedule, error) {
	return s.schedules.ListSchedules(ctx, workspaceID)
}

func (s *Service) GetSchedule(ctx context.Context, id string) (apimodel.Schedule, error) {
	return s.schedules.GetSchedule(ctx, id)
}

func (s *Service) CreateSchedule(
	ctx context.Context, workspaceID string, draft apimodel.ScheduleDraft,
) (apimodel.Schedule, error) {
	return s.schedules.CreateSchedule(ctx, workspaceID, draft)
}

func (s *Service) UpdateSchedule(
	ctx context.Context, id string, draft apimodel.ScheduleDraft,
) (apimodel.Schedule, error) {
	return s.schedules.UpdateSchedule(ctx, id, draft)
}

func (s *Service) SetScheduleStatus(
	ctx context.Context, id string, status apimodel.ScheduleStatus,
) (apimodel.Schedule, error) {
	return s.schedules.SetScheduleStatus(ctx, id, status)
}

func (s *Service) DeleteSchedule(ctx context.Context, id string) error {
	return s.schedules.DeleteSchedule(ctx, id)
}

func (s *Service) RunScheduleNow(ctx context.Context, id string) (apimodel.Schedule, error) {
	return s.schedules.RunScheduleNow(ctx, id)
}

// ---------------------------------------------------------------------------
// Forwarding: used by the entry in this package's revision.go (goes with its caller, not
// deleted with the handler)
// ---------------------------------------------------------------------------

// updateSchedule is an update with a reason, used by the rollback entry. The real
// implementation is in service/schedule.
//
// ⚠️ **Rollback must go through it, not do recordRevision + SaveSchedule itself.** The reason
// is written on UpdateScheduleWithReason in service/schedule (it would record two history
// entries, and skip "recompute NextRunAt when the spec changed").
func (s *Service) updateSchedule(
	ctx context.Context, id string, draft apimodel.ScheduleDraft, reason apimodel.RevisionReason,
) (apimodel.Schedule, error) {
	return s.schedules.UpdateScheduleWithReason(ctx, id, draft, reason)
}
