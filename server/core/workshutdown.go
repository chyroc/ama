package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

// NewShutdownSafeAPI suppresses StopWork after the worker context is cancelled.
// The check uses the worker lifetime, not the per-call context: the SDK may use
// context.Background for cleanup. Leaving the work unsettled lets another instance reclaim it.
func NewShutdownSafeAPI(ctx context.Context, api selfhosted.API, logger *slog.Logger) selfhosted.API {
	return newShutdownSafeAPI(ctx, api, logger)
}

// DiscardWork settles an unusable work item with ack followed by stop. Ack failure
// leaves it queued; stop failure leaves it starting until reclaim. Both outcomes are
// logged explicitly rather than claiming a successful discard.
func DiscardWork(
	ctx context.Context, api selfhosted.API, item selfhosted.WorkItem,
	workerID string, timeout time.Duration, logger *slog.Logger,
) {
	if logger == nil {
		logger = slog.Default()
	}
	if item.ID == "" {
		return
	}
	if err := api.AckWork(ctx, selfhosted.AckWorkRequest{
		EnvironmentID: item.EnvironmentID, WorkID: item.ID, WorkerID: workerID,
	}); err != nil {
		logger.Warn("丢弃 work 时 ack 失败，它仍在队列里、下一轮会被再次领到",
			slog.String("work_id", item.ID), slog.String("environment_id", item.EnvironmentID), slog.Any("err", err))
		return
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if err := api.StopWork(stopCtx, selfhosted.StopWorkRequest{
		EnvironmentID: item.EnvironmentID, WorkID: item.ID,
	}); err != nil {
		logger.Warn("丢弃 work 时 stop 失败，它会卡到 reclaim 窗口过期后重来",
			slog.String("work_id", item.ID), slog.String("environment_id", item.EnvironmentID), slog.Any("err", err))
	}
}
