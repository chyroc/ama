package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
)

// ScheduleTickHooks supplies persistent claims, optional lease leadership, and
// the in-flight/reschedule behavior for one scheduler scan.
type ScheduleTickHooks struct {
	Leases     Leases
	LeaderKey  string
	LeaderTTL  time.Duration
	Claim      func(context.Context, string, time.Time) ([]apimodel.Schedule, error)
	LostRound  func(apimodel.Schedule, time.Time) (bool, time.Duration)
	InFlight   func(string) bool
	Fire       func(apimodel.Schedule)
	Reschedule func(apimodel.Schedule)
	Owner      string
	Now        func() time.Time
	Logger     *slog.Logger
}

// ScanSchedules performs one scheduler scan. Redis leadership only reduces
// duplicate scans; the persistent claim is the correctness gate. A busy local round
// is rescheduled rather than silently left with an empty next_run_at.
func ScanSchedules(ctx context.Context, hooks ScheduleTickHooks) {
	logger := hooks.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if hooks.Leases != nil {
		lease, ok, err := hooks.Leases.Acquire(ctx, hooks.LeaderKey, hooks.LeaderTTL)
		if err != nil {
			logger.Warn("抢调度锁失败，本轮照常扫（重复由 PG 认领兜住）", slog.Any("err", err))
		} else if !ok {
			return
		} else {
			defer func() {
				if err := lease.Release(context.WithoutCancel(ctx)); err != nil {
					logger.Warn("释放调度锁失败，下一拍要等它过期", slog.Any("err", err))
				}
			}()
		}
	}
	now := hooks.Now
	if now == nil {
		now = time.Now
	}
	claimed, err := hooks.Claim(ctx, hooks.Owner, now().UTC())
	if err != nil {
		logger.Warn("认领定时任务失败，下个周期重试", slog.Any("err", err))
		return
	}
	if len(claimed) == 0 {
		return
	}
	logger.Info("认领到到期的定时任务", slog.Int("count", len(claimed)))
	for _, sch := range claimed {
		if hooks.LostRound != nil {
			if lost, since := hooks.LostRound(sch, now()); lost {
				logger.Warn("这条任务上一轮没跑完就断了（进程被重建？），本次是补领",
					slog.String("schedule", sch.ID), slog.String("last_run_at", sch.LastRunAt),
					slog.Duration("since_last_run", since))
			}
		}
		if hooks.InFlight != nil && hooks.InFlight(sch.ID) {
			logger.Warn("上一轮还在跑，跳过本次触发", slog.String("schedule", sch.ID))
			if hooks.Reschedule != nil {
				hooks.Reschedule(sch)
			}
			continue
		}
		if hooks.Fire != nil {
			hooks.Fire(sch)
		}
	}
}
