package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/gopkg/arkframe"
)

// RunWatcherStore provides persistent run reads and claim release. Claimed runs
// are claimed before this lifecycle starts, so only one instance owns the rescue path.
type RunWatcherStore interface {
	GetRun(context.Context, string) (apimodel.Run, error)
	SaveRun(context.Context, apimodel.Run) (apimodel.Run, error)
	ReleaseRunWatch(context.Context, string) error
}

// RunWatchLifecycleHooks connects one claimed subscription to the downstream
// event stream, pending queue, terminal-frame handling, and last-resort rescue.
type RunWatchLifecycleHooks struct {
	Store         RunWatcherStore
	Stream        func(context.Context, string, func([]byte) error) error
	Drain         func(context.Context, string, string) bool
	Rescue        func(context.Context, string)
	RetireMissing func(context.Context, string)
	IsNotFound    func(error) bool
	TurnTimeout   time.Duration
}

// WatchClaimedRun subscribes to a claimed run, releases its persistent claim on exit,
// and stops at a real turn end. User interrupts close the stream without draining the
// next queued message. A missing remote session is retired instead of retried forever.
func WatchClaimedRun(
	ctx context.Context, runID, owner string, logger *slog.Logger, hooks RunWatchLifecycleHooks,
) {
	if logger == nil {
		logger = slog.Default()
	}
	if hooks.Store != nil {
		defer func() {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := hooks.Store.ReleaseRunWatch(releaseCtx, runID); err != nil {
				logger.Warn("释放会话订阅租约失败，下次要等它过期", slog.String("run", runID), slog.Any("err", err))
			}
		}()
	}
	if hooks.Stream == nil {
		return
	}
	timeout := hooks.TurnTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	logger.Info("开始常驻订阅", slog.String("run", runID))
	if hooks.Rescue != nil {
		go hooks.Rescue(runCtx, runID)
	}
	drained := false
	err := hooks.Stream(runCtx, runID, func(raw []byte) error {
		if arkframe.TurnEnded(raw) {
			if drainAllowed(raw) && hooks.Drain != nil {
				drained = hooks.Drain(runCtx, runID, owner)
			}
			cancel()
		}
		return nil
	})
	if err != nil && runCtx.Err() == nil {
		if hooks.IsNotFound != nil && hooks.IsNotFound(err) && hooks.RetireMissing != nil {
			hooks.RetireMissing(ctx, runID)
			return
		}
		logger.Warn("常驻订阅异常结束", slog.String("run", runID), slog.Any("err", err))
		return
	}
	if drained {
		logger.Info("常驻订阅结束，已从队列发出下一条", slog.String("run", runID))
		return
	}
	logger.Info("常驻订阅结束", slog.String("run", runID))
}
