package core

import (
	"context"
	"log/slog"
	mrand "math/rand"
	"time"
)

const (
	// DefaultWorkerLeaseTTL must stay below the Ark work reclaim threshold.
	DefaultWorkerLeaseTTL = 20 * time.Second
	// DefaultWorkerLeaseRenew leaves three failed renewals before the lease expires.
	DefaultWorkerLeaseRenew = 5 * time.Second
	// DefaultWorkerLeaseRetryGap bounds takeover time after a holder exits.
	DefaultWorkerLeaseRetryGap = 5 * time.Second
	// DefaultWorkerLeaseSpread distributes environments when instances start together.
	DefaultWorkerLeaseSpread = 5 * time.Second
)

// WorkerPollLeaseKey returns the per-environment poll lease key.
// It is separate from cleanup and container warm-pool leases.
func WorkerPollLeaseKey(environmentID string) string {
	return "ama:core:workerpoll:" + environmentID
}

// LeaseSpreadDelay returns a random delay in [0, limit).
func LeaseSpreadDelay(limit time.Duration) time.Duration {
	if limit <= 0 {
		return 0
	}
	return time.Duration(mrand.Int63n(int64(limit)))
}

// AcquirePollLease acquires the environment's poll right. A nil lease store deliberately
// degrades to polling on every instance; AckWork still decides which instance handles a work item.
func AcquirePollLease(ctx context.Context, leases Leases, environmentID string, ttl time.Duration) (Lease, bool, error) {
	if leases == nil {
		return nil, true, nil
	}
	return leases.Acquire(ctx, WorkerPollLeaseKey(environmentID), ttl)
}

// KeepPollLease renews a poll lease and cancels polling when another instance owns it.
// A transient renewal failure is retried; a lost lease stops polling immediately.
// A nil lease means polling was deliberately running without leader election.
func KeepPollLease(
	ctx context.Context, lease Lease, ttl, renew time.Duration,
	cancel context.CancelFunc, logger *slog.Logger,
) {
	if lease == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	ticker := time.NewTicker(renew)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok, err := lease.Renew(ctx, ttl)
			if err != nil {
				logger.Warn("poll 权续约失败，还会再试", slog.Any("err", err))
				continue
			}
			if !ok {
				logger.Info("poll 权已被别的实例接管，本实例停止 poll 这个环境")
				cancel()
				return
			}
		}
	}
}
