package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

const (
	DefaultRunStaleAfter  = 30 * time.Minute
	DefaultReapMaxPerTick = 64
)

// ReapStaleRunsHooks supplies the stale run query and conditional update.
// Retire must include the stale cutoff in its atomic predicate: a late tool result
// refreshes activity and makes the update lose instead of killing a revived run.
type ReapStaleRunsHooks struct {
	List   func(context.Context, string) ([]apimodel.Run, error)
	Retire func(context.Context, apimodel.Run, string) (bool, error)
	Close  func(*apimodel.Run, time.Time)
	Max    int
	After  time.Duration
	Now    func() time.Time
}

// ReapStaleRuns terminates stale active runs in bounded batches. It uses the
// database conditional update, not the optional leader lease, as the correctness gate.
func ReapStaleRuns(ctx context.Context, logger *slog.Logger, hooks ReapStaleRunsHooks) {
	if hooks.List == nil || hooks.Retire == nil || hooks.Close == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	now := hooks.Now
	if now == nil {
		now = time.Now
	}
	after := hooks.After
	if after <= 0 {
		after = DefaultRunStaleAfter
	}
	maxPerTick := hooks.Max
	if maxPerTick <= 0 {
		maxPerTick = DefaultReapMaxPerTick
	}
	reapCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	staleBefore := apimodel.FormatISO(now().UTC().Add(-after))
	stale, err := hooks.List(reapCtx, staleBefore)
	if err != nil {
		logger.Warn("列出卡死的会话失败，本轮跳过回收", slog.Any("err", err))
		return
	}
	reaped, skipped := 0, 0
	for _, run := range stale {
		if reaped >= maxPerTick {
			skipped++
			continue
		}
		run.Status = apimodel.RunTerminated
		if run.ErrorSummary == "" {
			run.ErrorSummary = "这轮对话的工具结果没有返回（执行环境可能已被回收或替换），" +
				"会话已自动结束；重新发起一次对话即可。"
		}
		hooks.Close(&run, RetireRunEndedAt(run, now().UTC()))
		ok, err := hooks.Retire(reapCtx, run, staleBefore)
		if err != nil {
			logger.Warn("回收卡死的会话失败，下一轮还会再试",
				slog.String("run", run.ID), slog.Any("err", err))
			continue
		}
		if !ok {
			continue
		}
		reaped++
		logger.Warn("回收卡死的会话：工具结果一直没回来，已标记终止",
			slog.String("run", run.ID), slog.String("workspace", run.WorkspaceID),
			slog.String("last_activity", persistmodel.LastActivityOf(run)), slog.Duration("stale_after", after))
	}
	if reaped > 0 || skipped > 0 {
		logger.Warn("卡死会话回收完成",
			slog.Int("found", len(stale)), slog.Int("reaped", reaped), slog.Int("skipped_over_cap", skipped))
	}
}
