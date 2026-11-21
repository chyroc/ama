package core

import (
	"context"
	"log/slog"
	"time"
)

const (
	// DefaultToolResultCleanupInterval is the cleanup frequency for settled ledger rows.
	DefaultToolResultCleanupInterval = 10 * time.Minute
	// DefaultToolResultRetention exceeds a work item's lifetime and reclaim window.
	DefaultToolResultRetention = time.Hour
)

// CleanupOldToolResults removes ledger rows older than the retention period.
// A missing cleanup function means the downstream store has no ledger; deletion
// failures do not stop tool execution and are retried by the next scheduled pass.
func CleanupOldToolResults(
	ctx context.Context, deleteOld func(context.Context, time.Time) (int64, error),
	retention time.Duration, now time.Time, logger *slog.Logger,
) {
	if deleteOld == nil {
		return
	}
	if retention <= 0 {
		retention = DefaultToolResultRetention
	}
	if logger == nil {
		logger = slog.Default()
	}
	n, err := deleteOld(ctx, now.Add(-retention))
	if err != nil {
		logger.Warn("清理工具结果账本失败（不影响处理，下轮再试）", slog.Any("err", err))
		return
	}
	if n > 0 {
		logger.Info("清理陈年工具结果账本", slog.Int64("删除行数", n))
	}
}
