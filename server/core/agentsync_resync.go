package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/chyroc/ama/server/model/persistmodel"
)

// AgentSyncResyncStore reads the latest task after a sync attempt and requeues it
// when a newer save requested another pass. The implementation must use persistent storage.
type AgentSyncResyncStore interface {
	LatestAgentSyncTask(context.Context, string) (persistmodel.AgentSyncTaskRow, error)
	RequeueAgentSyncTask(context.Context, string, time.Time) error
}

// RequeueAgentSyncIfRequested prevents a successful older sync from hiding a newer save.
// It must reread the task: the resync flag is set after the claimed snapshot was taken.
// A failed requeue returns false so the caller can leave recovery to the existing lease.
func RequeueAgentSyncIfRequested(
	ctx context.Context, store AgentSyncResyncStore, task persistmodel.AgentSyncTaskRow,
	now time.Time, logger *slog.Logger,
) bool {
	if store == nil {
		return false
	}
	current, err := store.LatestAgentSyncTask(ctx, task.AgentID)
	if err != nil || current.ID != task.ID || !current.ResyncRequested {
		return false
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("同步期间 agent 又被保存过，再跑一轮")
	if err := store.RequeueAgentSyncTask(ctx, task.ID, now); err != nil {
		logger.Warn("重排同步任务失败，等租约过期后回收", slog.Any("err", err))
		return false
	}
	return true
}
