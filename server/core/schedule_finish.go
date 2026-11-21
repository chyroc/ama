package core

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
)

// FinishScheduleHooks supplies persistent schedule state and claim cleanup.
type FinishScheduleHooks struct {
	Get      func(context.Context, string) (apimodel.Schedule, error)
	Delete   func(context.Context, string) error
	Save     func(context.Context, apimodel.Schedule) (apimodel.Schedule, error)
	Release  func(context.Context, string) error
	Next     func(apimodel.ScheduleSpec, time.Time) (string, error)
	NotFound error
}

// FinishSchedule records one completed or failed round. It rereads user edits,
// deletes one-shot tasks regardless of outcome, pauses after repeated failures,
// and releases the claim only after saving the next fire time.
func FinishSchedule(
	ctx context.Context, sch apimodel.Schedule, runID string, finished time.Time, fireErr error,
	logger *slog.Logger, hooks FinishScheduleHooks,
) {
	if logger == nil {
		logger = slog.Default()
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	current, err := hooks.Get(writeCtx, sch.ID)
	if err != nil {
		if hooks.NotFound == nil || !errors.Is(err, hooks.NotFound) {
			logger.Warn("回写调度状态前重读失败", slog.String("schedule", sch.ID), slog.Any("err", err))
		}
		return
	}
	if current.Spec.Kind == apimodel.ScheduleOneshot {
		if err := hooks.Delete(writeCtx, current.ID); err != nil {
			logger.Warn("删除一次性任务失败，它可能会被重复触发", slog.String("schedule", current.ID), slog.Any("err", err))
		}
		return
	}
	current.LastRunAt = apimodel.FormatISO(finished)
	current.UpdatedAt = apimodel.FormatISO(finished)
	if runID != "" {
		current.LastRunID = runID
	}
	current.RunCount++
	if fireErr != nil {
		current.FailCount++
		current.ConsecutiveFailures++
		current.LastError = fireErr.Error()
	} else {
		current.ConsecutiveFailures = 0
		current.LastError = ""
	}
	switch {
	case current.Status != apimodel.ScheduleActive:
		current.NextRunAt = ""
	case current.ConsecutiveFailures >= apimodel.MaxConsecutiveFailures:
		current.Status = apimodel.SchedulePaused
		current.NextRunAt = ""
		logger.Warn("连续失败次数达上限，自动暂停这条定时任务",
			slog.String("schedule", current.ID), slog.Int("failures", current.ConsecutiveFailures))
	default:
		next, err := hooks.Next(current.Spec, finished)
		if err != nil {
			current.Status = apimodel.SchedulePaused
			current.NextRunAt = ""
			current.LastError = "算不出下次触发时刻：" + err.Error()
		} else {
			current.NextRunAt = next
		}
	}
	if _, err := hooks.Save(writeCtx, current); err != nil {
		logger.Error("回写调度状态失败，这条任务可能不会再被触发", slog.String("schedule", current.ID), slog.Any("err", err))
	}
	if err := hooks.Release(writeCtx, current.ID); err != nil {
		logger.Warn("释放认领租约失败，这条任务要等租约过期才能再跑", slog.String("schedule", current.ID), slog.Any("err", err))
	}
}
