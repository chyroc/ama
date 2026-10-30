// Poll leader election for the self_hosted worker: **only one instance polls an environment at a
// time**.
//
// # Why this layer exists
//
// Polling is the biggest source of idle traffic. Ark caps block_ms at 999 milliseconds (see
// arkPollBlockMS), so a "long poll" hangs for at most 1 second, and when idle that loop is a timer,
// not "hang until there is work".
//
// Example scale: 5 instances, 7 Ark environments → **35 poll loops**. With jitter [1s,3s) (mean 2
// seconds → 30 req/min per loop) that is **~1050 RPM**, while the work picked up when idle is 0 and
// tools run 0 times: all idle spinning.
//
// ⚠️ Count the instances actually running, not "how many replicas are configured": instances in a
// canary or a rolling deploy all run the same code and poll just the same.
//
// What gets hit is the **account-level** RPM quota (original error text
// `The RPM (Requests Per Minute) limit for the API on your account has been exceeded`), shared by
// all workers, so "everyone polls, everyone gets rate limited".
//
// ⚠️ **The actual quota number can't be found**: Ark doesn't give the limit in the error, and no
// RPM/quota number can be found in the official docs either. So the acceptance check can only be
// "429s drop clearly relative to the baseline", not "below some absolute value".
//
// The 5 loops on each environment are **fully redundant**: the work queue is split by environment,
// the same env is polled once by each of 5 instances, and they get the same batch of work, just with
// four extra useless requests. So elect a leader per env, let one instance own polling, and idle RPM
// is directly **divided by the instance count**: 1050 → ~210 RPM. Adding stepped backoff
// (workerbackoff.go) brings it further down to single digits.
//
// # ⚠️ This layer is an **optimization**, not correctness
//
// Don't treat it as the mechanism that "prevents two pods from handling the same work": **that is
// already handled elsewhere**: `AckWork` is a racing queued→starting operation, and the loser doesn't
// get the work (see the comment on ack failure in pollLoop). This decides how it fails:
//
//	no Redis configured → every pod polls = the behavior before this version; fully correct,
//	                      just uses more quota
//	Redis is down       → same as above, degrade-open; it must **not** stop the worker from starting
//	lock not obtained   → **not an error**, a normal result of leader election; don't flood with Error
//
// Same argument as the leader election in core.Scheduler (that one relies on atomic claims in PG as
// the backstop, this one relies on AckWork). The reverse also holds: **don't** make it fail-closed
// to be "safer"; then one Redis hiccup would stop polling for the whole environment, turning a pure
// optimization into a new single point of failure.
//
// # ⚠️⚠️ The TTL must be less than arkReclaimOlderThan (45 seconds), which must be less than Ark's 90-second stop line
//
// These three constants have an **implicit chain of dependencies**; read it before changing any of
// them:
//
//	arkWorkServerStopAfter = 90s  Ark server's hard stop line: 90 seconds after the last heartbeat,
//	                              the server itself sets the work to stopped (final state, can
//	                              never be polled again)
//	arkReclaimOlderThan    = 45s  "after how long without a heartbeat poll can grab it back"
//	workerLeaseTTL         = 20s  after the holder dies, how long until another pod can take over polling
//
// When the holder is killed with kill -9 (pod eviction, OOM, host failure), no one heartbeats the
// in-flight work it held. **After 45 seconds the reclaim window opens, but only a pod currently
// polling this env can reclaim it**, and with leader election that is only the lease holder. The
// takeover must happen before second 45:
//
//	takeover time = TTL (wait for the dead lock to expire) + retryGap (another pod's next attempt)
//	              = 20 + 5 = 25 seconds  <  45 seconds ✓ (20 seconds of margin)
//
// And the reclaim threshold must be early enough before Ark's 90-second stop line: within the window
// (90-45=45 seconds) poll must fire at least once; an environment with in-flight work keeps its
// ladder at the 1~10 second steps, so 45 seconds is enough for several polls.
// ⚠️ **The threshold was once set to 90 seconds, a zero-size window**: poll checks the timeout only
// once at the start of a request, so the poll at second 89.9 decides "not quite yet" and returns
// empty, and by the next poll the server has already stopped the work, so it can never be picked up
// again. A production session got stuck in requires_action forever exactly this way (during a
// rolling deploy, four works were all measured stopped by the server at exactly 90.00x seconds).
//
// Conversely, if the TTL is set above 45 seconds, it opens a **stuck window**: the work at second
// 45 can already be grabbed, but the new lease holder only appears at second TTL; in between no one
// is polling, until the server stops it for good at second 90. **Zero errors**; the user sees "the
// reply vanished halfway", and the logs say nothing.
//
// ⚠️ The most natural move when changing this is "raise the TTL a bit to reduce leader flapping";
// **that one change walks right into it**. `TestWorkerPollLeaseFitsReclaimWindow` pins this chain of
// inequalities, so raising it turns the test red.
//
// # Parameters copied from shared/infra/lark/longconn.go
//
// That is the leader election this repo already runs in production (the Feishu long connection),
// 20s / 5s / 5s, with the comment "4x margin: the lock is only lost after three renewals fail in a
// row". **Deliberately not defining another set**: two leader-election rhythms in one repo would
// make "how long until the lock drops" something no one can say for sure. A 20-second TTL also
// naturally satisfies the < 45 seconds above.
package core

import (
	"context"
	"log/slog"
	mrand "math/rand"
	"time"
)

const (
	// workerLeaseTTL is the lease duration of the poll right. **It must be much smaller than arkReclaimOlderThan**; see the file header.
	workerLeaseTTL = 20 * time.Second
	// workerLeaseRenew is the renewal interval. 1/4 of the TTL: the lock is only lost after three renewals fail in a row.
	workerLeaseRenew = 5 * time.Second
	// workerLeaseRetryGap is the retry interval when the lock can't be obtained.
	//
	// A fixed value instead of exponential backoff: not getting the lock is **the normal case** (N-1
	// pods fail every round), and backoff would make "how long until someone takes over after the
	// holder exits" unpredictable, while that value is part of the argument for the inequality chain
	// in the file header.
	workerLeaseRetryGap = 5 * time.Second
	// workerLeaseSpread is the upper bound of the random delay before the **first** lock attempt.
	//
	// Without it, the first pod to start grabs the leases for all envs at once: leader election works,
	// but all 7 environments sit on one pod. That becomes a de facto single point of failure (if it
	// dies, 7 environments wait for takeover together), the other 4 pods sit idle, and that pod's
	// container quota pressure is concentrated in one place.
	//
	// The value only makes sense on the same scale as retryGap: if the delay is much smaller than
	// retryGap, pods still arrive almost at once and the first one still takes everything. With 5
	// production pods each picking a random time in [0,5s), each env's winner is roughly uniform, and 7
	// environments are expected to spread at 1.4 per pod.
	//
	// ⚠️ It only helps when pods **start at the same time** (new release, group restart). During a
	// rolling deploy the pod that is alive holds all the leases, and new pods only get them after it
	// exits. That is the inherent shape of lease-based leader election, not a bug, and not worth adding
	// an "actively step down" mechanism for (stepping down would create extra takeover gaps during a
	// deploy).
	workerLeaseSpread = 5 * time.Second
)

// workerPollLeaseKey is the lock key for an environment's poll right.
//
// ⚠️ **Don't share it with the lock for the reclaim sweep** (`<prefix>:docker-reaper:<daemon id>`, see
// shared/infra/dockertools/reaper.go). Both are called "leader election", but the cost of failure is
// completely different: losing that one just skips one round of reclaiming (the next round scans
// again, users don't notice), while losing this one means **no one polls** this environment and
// conversations get stuck. Sharing one key would let the two starve each other.
//
// The granularity doesn't match either: that one is **one per daemon**, this one is **one per env**;
// a pod may hold the poll right for several environments at once, or for none.
func workerPollLeaseKey(envID string) string {
	return "ama:core:workerpoll:" + envID
}

// SetPollLeases installs the poll leader-election ability.
//
// # Why a setter and not a NewWorkerPool parameter
//
// It is decided by the wiring order: the worker pool is built early in `cmd/amad` (step 8), while
// the Redis client only connects when the Feishu long connection is wired (step 9). Putting Leases
// into WorkerConfig would mean moving the whole "connect Redis" step earlier, which changes the
// startup order, and the startup order already has a chain of interdependent preconditions (config →
// store → encryptor → each provider → worker → Redis → each long-running goroutine). Reordering it
// for an **optional optimization** is not worth the risk.
//
// ⚠️ **It must be called before Run**. Calling it after Run doesn't error, but by then refresh has
// already started a batch of workers, each holding the config from that moment; it shows up as
// "leader election is configured but some environments still have everyone polling", with zero
// errors. In `cmd/amad` it is called right before `workerPool.Run`.
//
// When p is nil (worker not enabled) it does nothing, same as RunWarmPool.
func (p *WorkerPool) SetPollLeases(leases Leases) {
	if p == nil {
		return
	}
	p.cfg.Leases = leases
}

// leaseSpreadDelay returns a random delay in [0, workerLeaseSpread); see the comment on that constant.
func (p *WorkerPool) leaseSpreadDelay() time.Duration {
	if p.leaseSpread <= 0 {
		return 0
	}
	return time.Duration(mrand.Int63n(int64(p.leaseSpread)))
}

// acquirePollLease tries to take this environment's poll right.
//
// Three kinds of return, which the caller must handle separately:
//
//	(lease, true,  nil)  got it; go poll, Release when done
//	(nil,   true,  nil)  **no Redis configured**, degrade-open: poll as usual (see the file header)
//	(nil,   false, nil)  the lock is held by another pod: **normal, not an error**
//	(nil,   false, err)  Redis error; back off and retry
func (p *WorkerPool) acquirePollLease(ctx context.Context, envID string) (Lease, bool, error) {
	if p.cfg.Leases == nil {
		// degrade-open: without leader election every pod polls. Correct, just uses more quota.
		return nil, true, nil
	}
	return p.cfg.Leases.Acquire(ctx, workerPollLeaseKey(envID), p.leaseTTL)
}

// keepPollLease renews the lease periodically, and cancels this round of polling if renewal fails.
//
// Copies the shape of lark.Receiver.keepLease. **Losing the lock must stop polling immediately**;
// otherwise two pods poll the same env at once, which is exactly what this layer is meant to save
// (it doesn't cause errors, but wastes quota, and "a leader was elected but RPM didn't drop" becomes
// a symptom whose cause no one can find).
//
// When lease is nil (degrade-open) it returns at once: there is no lock and nothing to guard, so
// polling runs until the outer ctx ends.
func (p *WorkerPool) keepPollLease(
	ctx context.Context, lease Lease, cancel context.CancelFunc, logger *slog.Logger,
) {
	if lease == nil {
		return
	}
	ticker := time.NewTicker(p.leaseRenew)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok, err := lease.Renew(ctx, p.leaseTTL)
			if err != nil {
				// One failed renewal is not fatal: the TTL is 4x the renewal interval, so there are two more chances.
				// Warn rather than Error: Redis hiccups are common, and this layer is a pure optimization.
				logger.Warn("poll 权续约失败，还会再试", slog.Any("err", err))
				continue
			}
			if !ok {
				// The lock is no longer ours (this machine stalled for a long time and another pod took it).
				// Stop polling right away and let the new holder own it.
				logger.Info("poll 权已被别的实例接管，本实例停止 poll 这个环境")
				cancel()
				return
			}
		}
	}
}
