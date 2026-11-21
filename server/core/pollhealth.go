package core

import (
	"context"
	"log/slog"
	"time"
)

const (
	// DefaultPollHealthTick is the watchdog's check interval.
	DefaultPollHealthTick = 10 * time.Second
	// DefaultPollStallBound exceeds the longest normal idle poll backoff.
	DefaultPollStallBound = 90 * time.Second
)

// PollHealth is the liveness heartbeat of one poll loop.
type PollHealth = pollHealth

// NewPollHealth starts a heartbeat at the supplied time.
func NewPollHealth(now time.Time) *PollHealth { return newPollHealth(now) }

// Beat records a completed PollWork call, including an empty poll.
func (h *PollHealth) Beat(now time.Time) { h.beat(now) }

// WorkStarted records a claimed work item occupying a concurrency slot.
func (h *PollHealth) WorkStarted() { h.workStarted() }

// WorkDone releases a work item's concurrency slot.
func (h *PollHealth) WorkDone() { h.workDone() }

// StalledAt reports whether no successful poll completed within bound.
func (h *PollHealth) StalledAt(now time.Time, bound time.Duration) bool {
	return h.stalledAt(now, bound)
}

// ActiveWork returns the number of claimed work items that have not finished.
func (h *PollHealth) ActiveWork() int64 { return h.activeWork.Load() }

// RunPollHealthWatchdog releases a stalled poller's lease and cancels its environment
// worker so it can be rebuilt. It runs independently of the possibly blocked poll loop.
func RunPollHealthWatchdog(
	ctx context.Context, health *PollHealth, environmentID, workerID string,
	lease Lease, envCancel context.CancelFunc,
	tick, bound time.Duration, now func() time.Time, logger *slog.Logger,
) {
	if tick <= 0 {
		tick = DefaultPollHealthTick
	}
	if bound <= 0 {
		bound = DefaultPollStallBound
	}
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	logger = logger.With(slog.String("watchdog", "poll-health"),
		slog.String("environment", environmentID), slog.String("worker_id", workerID))
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if health == nil || !health.StalledAt(now(), bound) {
				continue
			}
			logger.Error("环境 worker 持锁但长时间无法领活（pollLoop 卡死），主动放锁并重启本环境 worker",
				slog.String("environment", environmentID),
				slog.String("worker_id", workerID),
				slog.Int("在飞work数", int(health.ActiveWork())),
				slog.Duration("卡死判定", bound),
				slog.String("含义", "多半是并发槽被不响应取消的 work 占满；"+
					"已放锁让其它实例接管，本实例 ≤30s 内由 refresh 重建"))
			if lease != nil {
				relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				if err := lease.Release(relCtx); err != nil {
					logger.Warn("僵死放锁失败（锁会自行过期）", slog.Any("err", err))
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
