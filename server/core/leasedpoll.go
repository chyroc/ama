package core

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// LeasedPollHooks supplies the poll loop, lease acquisition/renewal, health watchdog,
// and timing knobs for one environment. The lease is an optimization, not the correctness gate.
type LeasedPollHooks struct {
	Spread        func() time.Duration
	RetryGap      time.Duration
	ReconnectGap  func() time.Duration
	Acquire       func(context.Context, string) (Lease, bool, error)
	Renew         func(context.Context, Lease, context.CancelFunc, *slog.Logger)
	PrepareHealth func(context.Context, Lease, context.CancelFunc, *slog.Logger) (*sync.WaitGroup, *pollHealth)
	Poll          func(context.Context, *pollHealth) error
	IsAuthError   func(error) bool
	Sleep         func(context.Context, time.Duration) bool
}

// RunLeasedPoll polls only after winning the environment lease. Losing the lease
// stops this round, renewal and health checks finish before release, and release uses
// an uncancelled context. Auth errors return to the caller so it can rebuild the client
// with a newly resolved credential instead of retrying forever with the same key.
func RunLeasedPoll(ctx context.Context, environmentID string, logger *slog.Logger, hooks LeasedPollHooks) error {
	if logger == nil {
		logger = slog.Default()
	}
	if hooks.Poll == nil || hooks.Acquire == nil {
		return nil
	}
	if hooks.Spread != nil {
		if d := hooks.Spread(); d > 0 && hooks.Sleep != nil && !hooks.Sleep(ctx, d) {
			return nil
		}
	}
	for {
		lease, ok, err := hooks.Acquire(ctx, environmentID)
		if err != nil {
			logger.Warn("抢 poll 权失败，本轮照常 poll（退化成每个实例都 poll）", slog.Any("err", err))
		} else if !ok {
			logger.Debug("poll 权在别的实例手上，本实例待命重试", slog.String("environment", environmentID))
			if hooks.Sleep != nil && !hooks.Sleep(ctx, hooks.RetryGap) {
				logger.Info("worker 停止")
				return nil
			}
			continue
		}
		leaseCtx, cancelLease := context.WithCancel(ctx)
		var leaseWG sync.WaitGroup
		if lease != nil && hooks.Renew != nil {
			logger.Info("拿到 poll 权，开始 poll 这个环境")
			leaseWG.Add(1)
			go func() {
				defer leaseWG.Done()
				hooks.Renew(leaseCtx, lease, cancelLease, logger)
			}()
		}
		var healthWG *sync.WaitGroup
		var health *pollHealth
		if hooks.PrepareHealth != nil {
			healthWG, health = hooks.PrepareHealth(leaseCtx, lease, cancelLease, logger)
		}
		err = hooks.Poll(leaseCtx, health)
		cancelLease()
		if healthWG != nil {
			healthWG.Wait()
		}
		cancelLease()
		leaseWG.Wait()
		if lease != nil {
			if releaseErr := lease.Release(context.WithoutCancel(ctx)); releaseErr != nil {
				logger.Warn("释放 poll 权失败（会自己过期）", slog.Any("err", releaseErr))
			}
		}
		if err != nil && ctx.Err() == nil {
			if hooks.IsAuthError != nil && hooks.IsAuthError(err) {
				return err
			}
			logger.Warn("worker 异常退出，5 秒后重连", slog.Any("err", err))
		}
		gap := 5 * time.Second
		if hooks.ReconnectGap != nil {
			gap = hooks.ReconnectGap()
		}
		select {
		case <-ctx.Done():
			logger.Info("worker 停止")
			return nil
		case <-time.After(gap):
		}
	}
}
