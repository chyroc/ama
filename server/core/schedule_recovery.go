package core

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
)

const DefaultScheduleClaimLease = 15 * time.Minute

// LostScheduleRoundBefore reports a claim recovered after an unfinished previous
// round. The extra full lease margin avoids calling ordinary scheduler delay a loss.
func LostScheduleRoundBefore(sch apimodel.Schedule, now time.Time, lease time.Duration) (bool, time.Duration) {
	if sch.LastRunAt == "" || sch.Spec.Kind == apimodel.ScheduleOneshot {
		return false, 0
	}
	last, err := apimodel.ParseISO(sch.LastRunAt)
	if err != nil {
		return false, 0
	}
	since := now.Sub(last)
	var period time.Duration
	switch sch.Spec.Kind {
	case apimodel.ScheduleInterval:
		period = time.Duration(sch.Spec.EveryMs) * time.Millisecond
	default:
		nextISO, err := ComputeNextRun(sch.Spec, last)
		if err != nil {
			return false, 0
		}
		next, err := apimodel.ParseISO(nextISO)
		if err != nil {
			return false, 0
		}
		period = next.Sub(last)
	}
	if period <= 0 {
		return false, 0
	}
	if lease <= 0 {
		lease = DefaultScheduleClaimLease
	}
	return since > period+lease, since
}

// RescheduleOnlyStore provides fresh schedule state and persistent claim cleanup.
type RescheduleOnlyStore interface {
	GetSchedule(context.Context, string) (apimodel.Schedule, error)
	SaveSchedule(context.Context, apimodel.Schedule) (apimodel.Schedule, error)
	ReleaseScheduleClaim(context.Context, string) error
}

// RescheduleOnly puts back the next fire time when this instance is still running
// the previous round. It rereads user edits, writes before releasing the claim, and
// leaves read/compute failures visible instead of silently stranding an active task.
func RescheduleOnly(
	ctx context.Context, store RescheduleOnlyStore, scheduleID string, now time.Time,
	notFound error, logger *slog.Logger,
) {
	if store == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	current, err := store.GetSchedule(writeCtx, scheduleID)
	if err != nil {
		if notFound == nil || !errors.Is(err, notFound) {
			logger.Warn("排回下次触发前重读失败，这条任务的 next_run_at 会一直空着、不会再被触发",
				slog.String("schedule", scheduleID), slog.Any("err", err))
		}
		return
	}
	if current.Status != apimodel.ScheduleActive {
		return
	}
	next, err := ComputeNextRun(current.Spec, now.UTC())
	if err != nil {
		logger.Warn("算不出下次触发时刻，这条任务的 next_run_at 会一直空着、不会再被触发",
			slog.String("schedule", scheduleID), slog.Any("err", err))
		return
	}
	current.NextRunAt = next
	current.UpdatedAt = apimodel.NowISO()
	if _, err := store.SaveSchedule(writeCtx, current); err != nil {
		logger.Warn("排回下次触发失败", slog.String("schedule", scheduleID), slog.Any("err", err))
	}
	if err := store.ReleaseScheduleClaim(writeCtx, current.ID); err != nil {
		logger.Warn("释放认领租约失败", slog.String("schedule", current.ID), slog.Any("err", err))
	}
}
