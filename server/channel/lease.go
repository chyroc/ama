package channel

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// LeaseStore hands out exclusive, expiring locks. cmd/amad backs it with Redis (shared/infra/redis).
type LeaseStore interface {
	Acquire(ctx context.Context, key string, ttl time.Duration) (Lease, bool, error)
}

// Lease is a lock currently held.
type Lease interface {
	// Renew extends the lease. false means another holder took it over.
	Renew(ctx context.Context, ttl time.Duration) (bool, error)
	Release(ctx context.Context) error
}

// Lease timing. The TTL is four renew intervals, so the lock is lost only after three renewals in a
// row fail; one slow renewal (GC pause, network blip) must not cause a handover.
const (
	leaseTTL      = 20 * time.Second
	leaseRenew    = 5 * time.Second
	leaseRetryGap = 5 * time.Second
	backoffMin    = time.Second
	backoffMax    = 30 * time.Second
)

// errStop marks an error after which RunWithLease must not retry.
type errStop struct{ err error }

func (e errStop) Error() string { return e.err.Error() }
func (e errStop) Unwrap() error { return e.err }

// Stop wraps an error so RunWithLease returns it instead of retrying, for failures that retrying cannot
// fix (for example credentials the platform rejected; the user must connect the account again).
func Stop(err error) error {
	if err == nil {
		return nil
	}
	return errStop{err: err}
}

// RunWithLease keeps calling fn while this instance holds the lease for key, until ctx is done.
//
// # Why every receive loop needs it
//
// amad runs as several instances. A platform connection (a Feishu long connection, a WeChat long poll)
// must have exactly one consumer per account: two pollers of the same WeChat account would race on the
// sync cursor and each get half of the messages. So only the instance holding the lease runs fn; the
// others wait and take over within one TTL when the holder dies or is redeployed.
//
// fn gets a ctx that is cancelled as soon as the lease is lost. When fn returns nil the loop starts
// again right away (re-acquiring the lease); when it returns an error the loop backs off first; when it
// returns an error wrapped by Stop, RunWithLease returns that error.
func RunWithLease(
	ctx context.Context, leases LeaseStore, key string, log *slog.Logger, fn func(ctx context.Context) error,
) error {
	if log == nil {
		log = slog.Default()
	}
	backoff := backoffMin
	for ctx.Err() == nil {
		lease, ok, err := leases.Acquire(ctx, key, leaseTTL)
		if err != nil {
			log.Warn("抢锁失败，退避后重试", "lease", key, "error", err.Error(), "backoff", backoff.String())
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			backoff = nextBackoff(backoff)
			continue
		}
		if !ok {
			// Another instance holds it; that is the normal state for all but one instance.
			if !sleepCtx(ctx, leaseRetryGap) {
				return nil
			}
			continue
		}

		err = runHolding(ctx, lease, key, log, fn)
		if relErr := lease.Release(context.WithoutCancel(ctx)); relErr != nil {
			log.Warn("释放锁失败（会自己过期）", "lease", key, "error", relErr.Error())
		}
		if ctx.Err() != nil {
			return nil
		}
		var stop errStop
		if errors.As(err, &stop) {
			return stop.err
		}
		if err == nil {
			backoff = backoffMin
			continue
		}
		log.Warn("持锁的循环出错，退避后重来", "lease", key, "error", err.Error(), "backoff", backoff.String())
		if !sleepCtx(ctx, backoff) {
			return nil
		}
		backoff = nextBackoff(backoff)
	}
	return nil
}

// runHolding runs fn while renewing the lease in the background; fn's ctx ends when the lease is lost.
func runHolding(
	ctx context.Context, lease Lease, key string, log *slog.Logger, fn func(ctx context.Context) error,
) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(leaseRenew)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				ok, err := lease.Renew(runCtx, leaseTTL)
				if err != nil {
					log.Warn("续约失败，还会再试", "lease", key, "error", err.Error())
					continue
				}
				if !ok {
					log.Warn("锁已被别的实例接管，停止本实例的循环", "lease", key)
					cancel()
					return
				}
			}
		}
	}()
	defer wg.Wait()
	defer cancel()
	return fn(runCtx)
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > backoffMax {
		return backoffMax
	}
	return d
}
