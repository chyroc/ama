package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
)

const (
	DefaultWorkRunCheckInterval = 30 * time.Second
	DefaultWorkIdleRunGrace     = 10 * time.Minute
)

// IdleRunAbandoned reports a normally settled run whose work has outlived the
// follow-up grace window. Unsettled or unparseable runs are not killed on weak evidence.
func IdleRunAbandoned(run apimodel.Run, now time.Time) bool {
	if run.Status != apimodel.RunIdle || run.EndedAt == "" {
		return false
	}
	ended, err := time.Parse("2006-01-02T15:04:05.000Z", run.EndedAt)
	if err != nil {
		return false
	}
	return now.Sub(ended) >= DefaultWorkIdleRunGrace
}

// WatchRunTerminated stops a work item after its persistent run is terminated or
// an idle run has exceeded the follow-up grace window. A missing run or a store
// failure is never treated as a terminal state: terminal cleanup may happen on
// another instance, so the database, not an in-process map, is the cross-instance fact.
func WatchRunTerminated(
	ctx context.Context, sessionID string, cancel context.CancelFunc,
	getRun func(context.Context, string) (apimodel.Run, error),
	interval time.Duration, logger *slog.Logger,
) {
	if getRun == nil || sessionID == "" || cancel == nil {
		return
	}
	if interval <= 0 {
		interval = DefaultWorkRunCheckInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run, err := getRun(ctx, sessionID)
			if err != nil {
				continue
			}
			abandoned := IdleRunAbandoned(run, time.Now().UTC())
			if run.Status != apimodel.RunTerminated && !abandoned {
				continue
			}
			reason := "run 已是终态"
			if abandoned {
				reason = "run 早已答完一轮并结算，这条 work 却还没退出（疑似卡在 SDK 的轮询路径）"
			}
			logger.Warn("结束仍在跑的 work："+reason,
				slog.String("ark_session", sessionID), slog.String("run_status", string(run.Status)),
				slog.String("ended_at", run.EndedAt),
				slog.String("说明", "继续跑只会对一条已收尾的会话重试投递、白烧方舟 RPM 配额，"+
					"见 core/workersendguard.go 文件头"))
			cancel()
			return
		}
	}
}
