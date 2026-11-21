package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
)

// RetireMissingRunHooks supplies the persistent read/update and settlement logic
// for a session that no longer exists on Ark. Read failures are not treated as termination.
type RetireMissingRunHooks struct {
	Get   func(context.Context, string) (apimodel.Run, error)
	Save  func(context.Context, apimodel.Run) (apimodel.Run, error)
	Close func(*apimodel.Run, time.Time)
}

// RetireMissingRun marks a remotely missing session terminated so the watcher does
// not subscribe again forever. It preserves the last real activity time where possible.
func RetireMissingRun(ctx context.Context, runID string, logger *slog.Logger, hooks RetireMissingRunHooks) {
	if logger == nil {
		logger = slog.Default()
	}
	if hooks.Get == nil || hooks.Save == nil || hooks.Close == nil {
		return
	}
	logger.Info("方舟侧已无此会话，标记终止并停止订阅", slog.String("run", runID))
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	run, err := hooks.Get(writeCtx, runID)
	if err != nil {
		return
	}
	run.Status = apimodel.RunTerminated
	if run.ErrorSummary == "" {
		run.ErrorSummary = "会话的执行环境已被回收，这条会话不能再继续了；重新发起一次对话即可。"
	}
	hooks.Close(&run, RetireRunEndedAt(run, time.Now().UTC()))
	if _, err := hooks.Save(writeCtx, run); err != nil {
		logger.Warn("标记会话终止失败，下一轮还会再试", slog.String("run", runID), slog.Any("err", err))
	}
}
