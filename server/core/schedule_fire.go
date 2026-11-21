package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
)

const DefaultScheduleFireTimeout = 10 * time.Minute

// FireScheduleHooks supplies one round's execution, delivery, and settlement.
type FireScheduleHooks struct {
	Run     func(context.Context, apimodel.Schedule, *slog.Logger) (string, string, error)
	Deliver func(context.Context, apimodel.Schedule, string, string, *slog.Logger) error
	Finish  func(context.Context, apimodel.Schedule, string, time.Time, error)
	Now     func() time.Time
	Timeout time.Duration
}

// FireSchedule runs one claimed task with an independent timeout. Delivery failure
// is a failed round too, and Finish receives the run ID even when execution fails.
func FireSchedule(ctx context.Context, sch apimodel.Schedule, logger *slog.Logger, hooks FireScheduleHooks) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("定时任务开始执行")
	timeout := hooks.Timeout
	if timeout <= 0 {
		timeout = DefaultScheduleFireTimeout
	}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	now := hooks.Now
	if now == nil {
		now = time.Now
	}
	started := now().UTC()
	runID, text, err := hooks.Run(runCtx, sch, logger)
	finished := now().UTC()
	if err != nil {
		logger.Error("定时任务执行失败", slog.Any("err", err))
		hooks.Finish(runCtx, sch, runID, finished, err)
		return
	}
	if hooks.Deliver != nil {
		if err := hooks.Deliver(runCtx, sch, text, runID, logger); err != nil {
			logger.Error("投递结果失败", slog.Any("err", err))
			hooks.Finish(runCtx, sch, runID, finished, err)
			return
		}
	}
	logger.Info("定时任务执行完成", slog.String("run", runID), slog.Duration("elapsed", finished.Sub(started)))
	hooks.Finish(runCtx, sch, runID, finished, nil)
}
