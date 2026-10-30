// Environment-level poller health watchdog.
//
// # It covers the fatal shape that workFailover can't reach
//
// The watchdog in workerfailover.go is **one per work**, and only watches "did this one's tool
// result come back". But pollLoop is shaped as "first take a concurrency slot (capacity = the
// concurrency, default 20), then call PollWork to pick up work":
//
//	sem <- {}            // ① take a slot
//	item = PollWork()    // ② pick up work
//	go processWork(...)  // ③ only releases the slot with <-sem when done
//
// When a work goroutine is stuck on **a block that ignores ctx** (some setup step without a
// timeout, an endless wait inside the SDK), it never runs `<-sem`. Once such stuck slots fill up,
// the main loop stays at ① forever and **never reaches ② PollWork**: the whole environment picks up
// no new work. Worse, the poll lease is still renewed every 5 seconds by keepPollLease, so:
//
//   - the only poller holding the lock for this environment no longer consumes the queue, yet
//     keeps renewing;
//   - other pods can't get the lock (runLeasedPoll only retries silently when it fails to get the
//     lock), so they can't take over;
//   - workFailover hands the stuck work back to Ark for redispatch, but after 45s **no healthy
//     poller picks it up**, and at 90s the server marks it stopped;
//   - after that, every new session (tool call) in this environment starves, with zero errors,
//     until reapStaleRuns ends it after 30 minutes.
//
// A production session had exactly this shape: runrescue tried to rescue it 11 times in a row and
// failed (the log said the failover redispatch self-healing chain is broken and a human is needed),
// and the tools of later sessions in the same environment were never run by anyone.
//
// # Approach
//
// In the same round that "holds the poll lock and enters pollLoop", start an **independent
// goroutine** in parallel (it can't live inside pollLoop: it must keep running when the main loop is
// stuck) that watches pollHealth:
//
//   - pollLoop beats once after every completed PollWork (including empty polls);
//   - if there is no successful poll for longer than pollStallBound, the lock holder is judged dead;
//   - handling: ① release the lease right away so another pod can grab the lock and take over (no
//     need to wait for the 20s TTL); ② cancel the whole environment worker so runOne exits and
//     refresh rebuilds a clean pollLoop (new semaphore) on this pod in the next cycle (≤30s);
//     ③ log an Error that clearly states "environment-level stall".
//
// The stuck old goroutines may still leak on the old semaphore (they can't be collected if they
// ignore cancel), but restoring service comes first: the new pollLoop has its own semaphore and picks
// up work as usual; the old leaks are at most the concurrency count and get cleaned up with the
// process on the next rolling deploy. Every trigger logs an Error, so leaks don't pile up silently.
package core

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

const (
	// pollHealthTick is the check interval of the environment health watchdog.
	pollHealthTick = 10 * time.Second

	// pollStallBound is "how long a lock holder can go without completing a PollWork before it
	// counts as dead".
	//
	// It must be clearly larger than pollLoop's longest normal idle gap. The idle path is: long-poll
	// block ≤1s + stepped backoff sleep (at most 60s, see workerbackoff.go). 90s is chosen: 30s of
	// margin over the idlest 60s sleep, yet far earlier than the 30-minute reapStaleRuns, so a
	// takeover happens in a window users barely notice.
	pollStallBound = 90 * time.Second
)

// pollHealth is the liveness heartbeat of one pollLoop instance, written by the main loop and read by
// the environment watchdog.
//
// lastPollOK: the time the last PollWork returned successfully (whether or not it got work).
// activeWork: the number of works picked up and not yet finished (slots in use), printed along with
// the stall, to tell "sem filled by stuck slots" apart from "the network-level polling itself hung".
type pollHealth struct {
	lastPollOK atomic.Int64 // unix nano
	activeWork atomic.Int64
}

func newPollHealth(now time.Time) *pollHealth {
	h := &pollHealth{}
	h.lastPollOK.Store(now.UnixNano())
	return h
}

// beat records one successful PollWork round trip.
func (h *pollHealth) beat(now time.Time) {
	if h != nil {
		h.lastPollOK.Store(now.UnixNano())
	}
}

func (h *pollHealth) workStarted() {
	if h != nil {
		h.activeWork.Add(1)
	}
}

func (h *pollHealth) workDone() {
	if h != nil {
		h.activeWork.Add(-1)
	}
}

// stalledAt reports "as of now, has it been longer than bound without a successful poll".
func (h *pollHealth) stalledAt(now time.Time, bound time.Duration) bool {
	last := h.lastPollOK.Load()
	if last == 0 {
		return false
	}
	return now.Sub(time.Unix(0, last)) > bound
}

// runPollHealthWatchdog watches the liveness of a pollLoop that holds the lock. When it stalls, it
// releases the lease and restarts this environment's worker. It exits when ctx is canceled.
//
// lease may be nil (fallback mode with no leader election): then there is no lock to release, but
// the environment worker must still be canceled to trigger a rebuild. envCancel is the switch that
// "restarts this environment's worker on this pod".
func (p *WorkerPool) runPollHealthWatchdog(
	ctx context.Context,
	h *pollHealth,
	envID, workerID string,
	lease Lease,
	envCancel context.CancelFunc,
	logger *slog.Logger,
) {
	p.runPollHealthWatchdogWithClock(ctx, h, envID, workerID, lease, envCancel,
		pollHealthTick, pollStallBound, time.Now)
}

// runPollHealthWatchdogWithClock pulls out tick / bound / now so unit tests can drive it.
func (p *WorkerPool) runPollHealthWatchdogWithClock(
	ctx context.Context,
	h *pollHealth,
	envID, workerID string,
	lease Lease,
	envCancel context.CancelFunc,
	tick, bound time.Duration,
	nowFunc func() time.Time,
) {
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	log := p.healthLogger(envID, workerID)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !h.stalledAt(nowFunc(), bound) {
				continue
			}

			active := h.activeWork.Load()
			log.Error("环境 worker 持锁但长时间无法领活（pollLoop 卡死），主动放锁并重启本环境 worker",
				slog.String("environment", envID),
				slog.String("worker_id", workerID),
				slog.Int("在飞work数", int(active)),
				slog.Duration("卡死判定", bound),
				slog.String("含义", "多半是并发槽被不响应取消的 work 占满；"+
					"已放锁让其它实例接管，本实例 ≤30s 内由 refresh 重建"))

			if lease != nil {
				relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				if err := lease.Release(relCtx); err != nil {
					log.Warn("僵死放锁失败（锁会自行过期）", slog.Any("err", err))
				}
				cancel()
			}
			if envCancel != nil {
				envCancel()
			}
			return
		}
	}
}

// healthLogger returns a logger with fixed fields; when WorkerPool is nil it uses the default logger
// (only unit tests call it with a nil pool).
func (p *WorkerPool) healthLogger(envID, workerID string) *slog.Logger {
	l := slog.Default()
	if p != nil && p.log != nil {
		l = p.log
	}
	return l.With(slog.String("watchdog", "poll-health"),
		slog.String("environment", envID), slog.String("worker_id", workerID))
}
