package core

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

// WorkItemHooks supplies the per-work runner, heartbeat, and cleanup policy.
// Prepare returns a close function for resources owned by this work item.
type WorkItemHooks struct {
	Prepare   func(context.Context) (func() error, error)
	Heartbeat func(context.Context, context.CancelFunc, func(string))
	Cleanup   func(context.Context, string, error)
}

// RunClaimedWork runs one claimed item with an independent heartbeat and waits for
// that heartbeat before cleanup. Cleanup receives the final heartbeat cause and the
// runner error, allowing each deployment to preserve its own StopWork policy.
func RunClaimedWork(
	ctx context.Context, item selfhosted.WorkItem, logger *slog.Logger, hooks WorkItemHooks,
) (retErr error) {
	if item.ID == "" {
		return errors.New("work item id must not be empty")
	}
	if logger == nil {
		logger = slog.Default()
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var heartbeatCause atomic.Value
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		if hooks.Heartbeat != nil {
			hooks.Heartbeat(workCtx, cancel, func(cause string) { heartbeatCause.Store(cause) })
		}
	}()
	defer func() {
		cancel()
		<-heartbeatDone
		cause, _ := heartbeatCause.Load().(string)
		if cause == HeartbeatCauseStopRequested && errors.Is(retErr, context.Canceled) {
			retErr = nil
		}
		if hooks.Cleanup != nil {
			hooks.Cleanup(workCtx, cause, retErr)
		}
	}()
	if hooks.Prepare == nil {
		return errors.New("work item prepare function is required")
	}
	close, err := hooks.Prepare(workCtx)
	if err != nil {
		return err
	}
	if close != nil {
		defer func() { _ = close() }()
	}
	return nil
}
