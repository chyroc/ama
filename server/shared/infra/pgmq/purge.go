// Scheduled cleanup of expired messages.
//
// # Why it is needed: delete-on-consume **only reclaims the ones that went through**
//
// On the normal path a message lives no more than a few milliseconds (Consume takes it and
// deletes it). But one kind is never reclaimed: **messages that were sent but that nobody
// ever consumes**. The webshell case is concrete: the user typed a key, and the instance
// holding that PTY did not pick it up within the TTL (30 seconds). It may have just been
// rebuilt, or the user closed the terminal and left. After these rows expire, **no code
// deletes them**.
//
// This is not an immediate outage, it is a slow leak: payloads are only tens of bytes, so the
// table does not suddenly grow. What really degrades is the index: `term:<terminalID>` is one
// topic per terminal and only ever grows, rows of dead topics permanently hold entries in
// `mq_messages_due_idx`, and together with the dead tuples from deletes, the autovacuum load
// keeps rising.
//
// **This kind of problem is the hardest to notice**, because there is never a single day when
// things "clearly get worse".
//
// ⚠️ **The `PurgeExpired` function itself was written long ago (with tests), but nobody ever
// called it.** The sentence "the caller hooks it into its own scheduled check" described
// something that never happened. A classic case of "the function was written, the wiring was
// forgotten", with zero errors. This file is that wiring.
package pgmq

import (
	"context"
	"log/slog"
	"time"
)

const (
	// purgeEvery is the cleanup interval.
	//
	// 1 hour rather than shorter: the TTL is only 30 seconds, so whatever piles up is
	// **already completely dead**; there is no "something breaks if it is deleted a bit
	// later". A shorter interval only makes N instances scan a nearly empty table more often.
	purgeEvery = 1 * time.Hour

	// purgeLeaderKey is the lock key used for leader election.
	//
	// ⚠️ It **must differ** from the scheduler / runwatcher keys: sharing one lock would let
	// two unrelated jobs starve each other. If cleanup grabs the lock, the scheduler skips
	// that tick for nothing.
	purgeLeaderKey = "pgmq:purge:leader"

	// purgeLeaderTTL is how long the lock is held.
	//
	// 5 minutes: far longer than one delete takes (milliseconds), and far shorter than
	// purgeEvery. The latter means "the instance holding the lock died" only makes the next
	// tick wait up to 5 minutes, instead of skipping all the way to the next hour.
	purgeLeaderTTL = 5 * time.Minute
)

// Lease is a lock being held. Same shape as core.Lease, but **declared locally here**.
//
// Why not import core: no package under `infra/` in the whole repo imports `core`; this rule
// is strictly kept (see the "Layering" section in this package's pgmq.go).
type Lease interface {
	// Release releases it. Pass a ctx that will not be canceled: on the shutdown path the
	// original ctx is often already canceled, and using it would fail to release the lock, so
	// the next instance waits a full TTL for nothing.
	Release(ctx context.Context) error
}

// Leases is the leader election capability, injected by the caller.
type Leases interface {
	// Acquire tries to take the lock. If it cannot, it returns (nil, false, nil). **That is
	// not an error**; it is a normal result of leader election.
	Acquire(ctx context.Context, key string, ttl time.Duration) (Lease, bool, error)
}

// PurgerConfig holds the parameters for NewPurger.
type PurgerConfig struct {
	// DB reads and writes the message table. Required.
	DB DB

	// Leases may be empty: then **every instance cleans**.
	//
	// That is safe (delete is idempotent; deleting again has no side effect), it is just N
	// instances doing the same thing. Same handling as the scheduler: leader election here is
	// an optimization, not a correctness requirement.
	Leases Leases

	// Every is the cleanup interval. Defaults to purgeEvery when empty.
	Every time.Duration

	// Log: no logging when empty.
	Log *slog.Logger
}

// Purger is a scheduled cleaner that runs on its own.
type Purger struct {
	db     DB
	leases Leases
	every  time.Duration
	log    *slog.Logger
}

// NewPurger builds a cleaner. It returns nil when DB is nil. That is a wiring error, but it
// **should not keep the service from starting**: stopped cleanup is only slow degradation
// and affects no online feature.
func NewPurger(cfg PurgerConfig) *Purger {
	if cfg.DB == nil {
		return nil
	}
	every := cfg.Every
	if every <= 0 {
		every = purgeEvery
	}
	return &Purger{db: cfg.DB, leases: cfg.Leases, every: every, log: cfg.Log}
}

// Run keeps cleaning until ctx ends; a normal end returns nil.
//
// ⚠️ **Run once at startup, do not wait a full interval.** After an instance restarts, the
// table often has expired messages left from the previous round piled up; waiting an hour to
// clean them would make the restart achieve nothing.
func (p *Purger) Run(ctx context.Context) error {
	p.logInfo("mq_messages 过期清理已启动",
		slog.Duration("间隔", p.every),
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
func (p *Purger) tick(ctx context.Context) {
	// If Redis is configured, take the lock first; if we cannot, skip this tick.
	if p.leases != nil {
		lease, ok, err := p.leases.Acquire(ctx, purgeLeaderKey, purgeLeaderTTL)
		switch {
		case err != nil:
			// ⚠️ If taking the lock fails, **clean as usual**; it must not degrade into not
			// cleaning.
			//
			// Same check as in runwatcher: correctness never depended on this lock (delete is
			// idempotent), so a Redis blip should not stop cleanup. Otherwise, once Redis goes
			// down, the whole leak comes back, again with zero errors.
			p.logWarn("抢清理锁失败，本轮照常清（重复删除无副作用）", slog.Any("err", err))
		case !ok:
			// Another instance is cleaning. This is a normal result, not an error.
			return
		default:
			defer func() {
				if err := lease.Release(context.WithoutCancel(ctx)); err != nil {
					p.logWarn("释放清理锁失败，下一拍要等它过期", slog.Any("err", err))
				}
			}()
		}
	}

	n, err := PurgeExpired(ctx, p.db)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down, not a failure
		}
		p.logWarn("清理过期消息失败，下一拍再试", slog.Any("err", err))
		return
	}
	// ⚠️ **Do not log when 0 rows were deleted.** In steady state it is 0 every hour, and
	// logging it would only drown the lines that carry real information. A non-zero count is
	// worth knowing: it shows that some messages really were never consumed.
	if n > 0 {
		p.logInfo("清理了过期的队列消息", slog.Int64("条数", n))
	}
}

func (p *Purger) logInfo(msg string, args ...any) {
	if p.log != nil {
		p.log.Info(msg, args...)
	}
}

func (p *Purger) logWarn(msg string, args ...any) {
	if p.log != nil {
		p.log.Warn(msg, args...)
	}
}
