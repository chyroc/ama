package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

// WorkProcessingHooks supplies per-work guards, execution, health counting, and
// final tool-call reconciliation. Process receives a per-work context so cancelling
// one work item does not cancel other work in the same environment.
type WorkProcessingHooks struct {
	Started func()
	Done    func()
	Setup   func(context.CancelFunc) (
		api selfhosted.API, abandoned func() bool, progress func(context.Context, context.CancelFunc),
	)
	Process      func(context.Context, selfhosted.API) error
	Terminated   func(context.Context, context.CancelFunc)
	Audit        func(context.Context, selfhosted.API)
	AuditTimeout time.Duration
	Logger       *slog.Logger
}

// ProcessClaimedWork runs one work item with its own cancellation, progress and
// termination watchdogs, and final reconciliation. Audit is skipped when the worker
// is shutting down or failover abandoned the item: a new owner must really execute it.
func ProcessClaimedWork(
	workerCtx context.Context, item selfhosted.WorkItem, api selfhosted.API, hooks WorkProcessingHooks,
) {
	logger := hooks.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if hooks.Started != nil {
		hooks.Started()
	}
	if hooks.Done != nil {
		defer hooks.Done()
	}
	workCtx, cancelWork := context.WithCancel(workerCtx)
	defer cancelWork()
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.Error("处理 work panic", slog.String("work_id", item.ID), slog.Any("panic", recovered))
		}
	}()
	var guardAPI selfhosted.API
	abandoned := func() bool { return false }
	if hooks.Setup != nil {
		var progress func(context.Context, context.CancelFunc)
		guardAPI, abandoned, progress = hooks.Setup(cancelWork)
		if progress != nil {
			go progress(workCtx, cancelWork)
		}
	} else {
		guardAPI = api
	}
	if hooks.Terminated != nil {
		go hooks.Terminated(workCtx, cancelWork)
	}
	if hooks.Process != nil {
		if err := hooks.Process(workCtx, guardAPI); err != nil && workerCtx.Err() == nil {
			logger.Warn("处理 work 失败", slog.String("work_id", item.ID), slog.Any("err", err))
		}
	}
	if workerCtx.Err() == nil && !abandoned() && hooks.Audit != nil {
		timeout := hooks.AuditTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(workerCtx), timeout)
		hooks.Audit(auditCtx, guardAPI)
		cancel()
	}
}
