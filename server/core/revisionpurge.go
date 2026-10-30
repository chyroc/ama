// Scheduled cleanup of history revisions over the limit.
//
// # Why cleanup is not done on the write path
//
// "Save a new snapshot and delete everything after the 20th while we are at it" looks the
// most natural, but that path has two problems, and the second is a correctness problem:
//
//  1. **It adds write latency.** Every agent save gets one more delete, and saving is a path
//     the user is waiting on. Cleanup has no latency needs at all; deleting an hour later
//     makes no difference.
//  2. **With many instances it is itself a race.** "Find the ones after the 20th, then delete
//     them" is check-then-write. Two instances saving the same entity at once each compute
//     their own list and delete the overlap. delete is idempotent, so nothing wrong is
//     deleted, but the work is wasted, and more so the higher the concurrency.
//
// So it runs as a scheduled task: once an hour, the instance that wins the lock scans the
// whole table and keeps apimodel.MaxRevisionsPerEntity rows per entity.
//
// **The cost is that the table may briefly go over the limit** (for up to an hour). That is
// fine: the limit is there to "keep junk data from growing forever", not to "never exceed
// 20 at any moment". The read API also only takes the first N rows, so users never see the
// extra ones.
//
// # The shape of this file copies shared/infra/pgmq/purge.go
//
// That is the earlier example of this kind of task in this repo. Three choices made to match
// it on purpose are explained in their own comments below: do not run when the lock is not
// won, still run when taking the lock **errors**, and run once at startup instead of waiting
// a full interval.
package core

import (
	"context"
	"log/slog"
	"time"
)

const (
	// revisionPurgeEvery is the cleanup interval.
	//
	// 1 hour rather than shorter: the extra snapshots are **ones nobody will look at anymore**
	// (the list only takes the first 20), and deleting an hour later has no visible effect.
	// A shorter interval only makes N instances scan, more often, a table that most of the
	// time has nothing to delete.
	revisionPurgeEvery = 1 * time.Hour

	// revisionPurgeLeaderKey is the lock key used to pick a leader.
	//
	// ⚠️ **It differs from the scheduler / runwatcher / pgmq keys**: sharing one lock would
	// let unrelated jobs starve each other. If the revision cleanup holds the lock, the
	// scheduler skips that tick for nothing.
	revisionPurgeLeaderKey = "revision:purge:leader"

	// revisionPurgeLeaderTTL is how long the lock is held.
	//
	// 5 minutes: far longer than one delete takes, and far shorter than revisionPurgeEvery.
	// The latter ensures that "the instance holding the lock died" makes the next tick wait
	// at most 5 minutes, instead of staying empty until the next hour.
	revisionPurgeLeaderTTL = 5 * time.Minute
)

// RevisionPurgerConfig configures the purger.
type RevisionPurgerConfig struct {
	// Service is required; cleanup goes through its Store.
	Service *Service
	// Leases may be nil: then there is no leader election and every instance cleans.
	//
	// That is normal in local development and has no correctness problem (delete is
	// idempotent); it just wastes work.
	Leases Leases
	// Keep is how many rows to keep per entity; empty means apimodel.MaxRevisionsPerEntity.
	Keep int
	// Every is the cleanup interval; empty means revisionPurgeEvery. For injection in tests.
	Every time.Duration
	// Logger empty means the Service's own logger.
	Logger *slog.Logger
}

// RevisionPurger periodically deletes each entity's history revisions over the limit.
type RevisionPurger struct {
	svc    *Service
	leases Leases
	keep   int
	every  time.Duration
	log    *slog.Logger
}

// NewRevisionPurger creates a purger. **It returns nil when Service is nil**, and the caller
// then does not start it.
//
// It returns nil instead of an error: the wiring layer already has branches like "do not
// start in mem mode", and letting it check in the same shape
// (`if p := New...(); p != nil`) is clearer than one more error path.
func NewRevisionPurger(cfg RevisionPurgerConfig) *RevisionPurger {
	if cfg.Service == nil {
		return nil
	}
	keep := cfg.Keep
	if keep <= 0 {
		keep = defaultRevisionKeep()
	}
	every := cfg.Every
	if every <= 0 {
		every = revisionPurgeEvery
	}
	log := cfg.Logger
	if log == nil {
		log = cfg.Service.log()
	}
	return &RevisionPurger{svc: cfg.Service, leases: cfg.Leases, keep: keep, every: every, log: log}
}

// Run cleans until ctx ends; it returns nil on a normal end.
//
// ⚠️ **Run once at startup, do not wait a full interval.** With only
// `for { <-ticker.C; tick() }`, a restarted instance waits a full hour before the first
// cleanup, and right after a restart the table often holds extra snapshots left from the
// last round, so the restart would achieve nothing.
func (p *RevisionPurger) Run(ctx context.Context) error {
	p.log.Info("历史版本清理已启动",
		slog.Duration("间隔", p.every),
		slog.Int("每个实体保留", p.keep),
		slog.Bool("选主", p.leases != nil))

	p.tick(ctx)

	ticker := time.NewTicker(p.every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.tick(ctx)
		}
	}
}

// tick runs one round of cleanup.
func (p *RevisionPurger) tick(ctx context.Context) {
	if p.leases != nil {
		lease, ok, err := p.leases.Acquire(ctx, revisionPurgeLeaderKey, revisionPurgeLeaderTTL)
		switch {
		case err != nil:
			// ⚠️ When taking the lock fails, **still clean**; do not fall back to not cleaning.
			//
			// These two go in opposite directions on purpose, and getting them backwards
			// gives no error:
			//   not won (ok=false)      → someone else is cleaning, I don't ← normal leader result
			//   lock error (err!=nil)   → Redis glitched, I clean anyway    ← lock unavailable
			//                             does not mean the work should not be done
			//
			// Correctness never depended on this lock (delete is idempotent). Written as
			// fail-closed, **the leak comes right back whenever Redis is down**, also with zero
			// errors. Same as the pgmq one.
			p.log.Warn("抢历史清理锁失败，本轮照常清（重复删除无副作用）", slog.Any("err", err))
		case !ok:
			// Another instance is cleaning. This is a normal result, not an error.
			return
		default:
			defer func() {
				// Pass a ctx that will not be canceled: on the shutdown path the original ctx
				// is often already canceled, and using it would fail to release the lock, so
				// the next instance would wait a full TTL for nothing.
				if err := lease.Release(context.WithoutCancel(ctx)); err != nil {
					p.log.Warn("释放历史清理锁失败，下一拍要等它过期", slog.Any("err", err))
				}
			}()
		}
	}

	// ⚠️ Go through the domain package's Purge, **do not reach back into p.svc.store**: that
	// path compiles (same package), but it bypasses service/revision and opens another
	// entry point outside the domain. The cleanup rules (keep<=0 deletes nothing, group by
	// entity) should live in only one place.
	n, err := p.svc.revisions.Purge(ctx, p.keep)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down, not a failure
		}
		p.log.Warn("清理超额历史版本失败，下一拍再试", slog.Any("err", err))
		return
	}
	// ⚠️ **Do not log when 0 rows were deleted.** In steady state it is 0 every hour, and
	// logging it would only bury the few lines that actually carry information. Same as the
	// pgmq one.
	if n > 0 {
		p.log.Info("清理了超额的历史版本", slog.Int64("条数", n))
	}
}
