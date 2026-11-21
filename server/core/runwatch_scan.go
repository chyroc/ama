package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
)

const (
	// DefaultRunWatchMaxConcurrent bounds long-lived subscriptions on one instance.
	DefaultRunWatchMaxConcurrent = 64
	// DefaultRunWatchLeaseFor exceeds one subscription's fallback timeout.
	DefaultRunWatchLeaseFor = 15 * time.Minute
)

// RunWatchScanHooks connects one scan to a downstream run store and subscriber.
// Claim must be an atomic persistent operation: the optional Watching check is
// only a local optimization and cannot exclude another process.
type RunWatchScanHooks struct {
	Watching        func(string) bool
	HasActiveStream func(string) bool
	Claim           func(context.Context, string, string, time.Time, time.Duration) (bool, error)
	Watch           func(context.Context, string)
	Rescue          func(context.Context, apimodel.Run, time.Duration)
	MaxConcurrent   int
	LeaseFor        time.Duration
	Now             func() time.Time
}

// ScanRunWatches subscribes to active runs that have no local listener and can be
// claimed in persistent storage. A browser stream suppresses a duplicate local
// subscription, but stopped-work rescue still runs for it. Overflow is reported
// rather than silently presented as fully subscribed.
func ScanRunWatches(
	ctx context.Context, runs []apimodel.Run, owner string, logger *slog.Logger, hooks RunWatchScanHooks,
) {
	if logger == nil {
		logger = slog.Default()
	}
	if hooks.Claim == nil || hooks.Watch == nil {
		logger.Error("会话常驻订阅缺少存储认领或订阅装配，本轮不启动订阅")
		return
	}
	maxConcurrent := hooks.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = DefaultRunWatchMaxConcurrent
	}
	leaseFor := hooks.LeaseFor
	if leaseFor <= 0 {
		leaseFor = DefaultRunWatchLeaseFor
	}
	now := hooks.Now
	if now == nil {
		now = time.Now
	}
	started, skipped := 0, 0
	for _, run := range runs {
		if ctx.Err() != nil {
			return
		}
		if hooks.Watching != nil && hooks.Watching(run.ID) {
			continue
		}
		if hooks.HasActiveStream != nil && hooks.HasActiveStream(run.ID) {
			if hooks.Rescue != nil && run.Status == apimodel.RunRequiresAction {
				if age := ageSinceActivity(run); age >= rescueAfter {
					hooks.Rescue(ctx, run, age)
				}
			}
			continue
		}
		if started >= maxConcurrent {
			skipped++
			continue
		}
		claimed, err := hooks.Claim(ctx, run.ID, owner, now(), leaseFor)
		if err != nil {
			logger.Warn("认领会话订阅失败，跳过这条", slog.String("run", run.ID), slog.Any("err", err))
			continue
		}
		if !claimed {
			continue
		}
		started++
		hooks.Watch(ctx, run.ID)
	}
	if started > 0 || skipped > 0 {
		logger.Info("会话常驻订阅巡检完成",
			slog.Int("active", len(runs)), slog.Int("started", started), slog.Int("skipped_over_cap", skipped))
	}
}
