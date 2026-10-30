// HostWatcher: sweeps for host machines that went offline and handles their pending calls.
//
// # Which gap it fills
//
// The host daemon stays online through a 20s heartbeat (hosts.last_seen_at). When the user closes
// the laptop lid, loses network, or kills the process, nothing says "I am leaving", while a tool
// call may be waiting for it to run / return a result (service/host's HostExec blocks until the
// outer timeout). We cannot let each waiter time out and judge liveness on its own: with multiple
// instances that would handle the same thing N times, and the "host is offline" state has to be
// saved somewhere.
//
// Same two gates as runwatcher / scheduler:
//
//	Gate one (Redis lease, optional)  only the instance holding ama:hostwatch:leader scans, saving N-1 empty scans.
//	                                  Without Redis, every instance scans; correctness relies on the CAS below.
//	Gate two (PG CAS, required)       MarkHostOffline only updates rows that are still online;
//	                                  FailPendingExecsForHost only updates rows that are not done.
//	                                  A late heartbeat / late result makes the condition miss, so a host that
//	                                  came back is not marked dead again, and a result that already arrived is
//	                                  not overwritten.
//
// # Why the time constants are what they are
//
//   - Heartbeat interval is 20s and the long poll also hangs 20s, so normally last_seen is at most
//     about 40s old.
//   - Offline threshold is 90s: it tolerates two heartbeats in a row failing from network jitter,
//     without making the agent wait too long (the SDK-side ceiling for one tool call is 120s, so a
//     clear failure must come before that).
//   - Scan every 15s: worst-case delay from "really offline → marked dead" is 90+15s, which the
//     HostExec outer 130s cap still waits long enough to be woken.
package core

import (
	"context"
	"log/slog"
	"time"
)

const (
	// hostOfflineAfter: if last_seen_at is older than this, the host counts as offline.
	hostOfflineAfter = 90 * time.Second
	// hostWatchScanInterval: sweep interval.
	hostWatchScanInterval = 15 * time.Second
	// hostWatchLeaderKey / TTL: separate from runwatch and scheduler, no shared key
	// (sharing would starve each other; the runwatcher file header has the same comment).
	hostWatchLeaderKey = "ama:hostwatch:leader"
	hostWatchLeaderTTL = 30 * time.Second
	// hostWatchMaxPerTick: max hosts handled per round, so a mass network outage does not overload it at once.
	hostWatchMaxPerTick = 100
	// hostTermDispatchRetention: how long finished terminal frames are kept (only for debugging the
	// reconnect window; terminal frames carry no results, so losing them has no side effect). purge
	// only deletes done term_* rows.
	hostTermDispatchRetention = 10 * time.Minute
	// hostExecDispatchRetention: how long finished exec rows are kept. The row's payload contains
	// injected credentials, and result is stdout/stderr that may print plaintext secrets, so they
	// must not stay long; 7 days is enough to cover the daemon crash redelivery window and later
	// debugging.
	hostExecDispatchRetention = 7 * 24 * time.Hour
)

// HostWatchActions is everything the sweep needs from the storage layer, implemented by the host
// domain service.
//
// "List offline hosts → mark offline → fail their pending execs" is deliberately one Sweep call
// instead of three: the last two steps are only needed when MarkHostOffline's CAS **hits** (the
// host really was online). This ownership check and the actions must be done in one pass per host
// in the storage layer; orchestrating across methods would, with multiple instances, fail again
// hosts that someone else just handled.
type HostWatchActions interface {
	// SweepOfflineHosts finds hosts whose last_seen is older than before, and for each one:
	// CAS-marks it offline (hits only if currently online); on a hit, marks its unfinished exec
	// dispatches done with an offline error result. Returns the ids of hosts actually marked
	// offline this round. Truncated beyond limit (truncated=true); the rest wait for the next round.
	SweepOfflineHosts(ctx context.Context, before time.Time, limit int) (offlineHostIDs []string, truncated bool, err error)
	// PurgeDoneTermDispatches deletes term_* rows with state='done' and created_at older than
	// before.
	PurgeDoneTermDispatches(ctx context.Context, before time.Time) (int64, error)
	// PurgeDoneExecDispatches deletes finished exec rows past retention (they contain sensitive payload/result).
	PurgeDoneExecDispatches(ctx context.Context, before time.Time) (int64, error)
}

// HostWatcher is the offline-host sweep component; Run blocks until ctx is done.
type HostWatcher struct {
	actions HostWatchActions
	wakes   HostWakes
	leases  Leases
	log     *slog.Logger
	now     func() time.Time
}

// NewHostWatcher wires the sweeper. leases may be nil (then every instance scans, with PG CAS as the safety net).
func NewHostWatcher(actions HostWatchActions, wakes HostWakes, leases Leases, log *slog.Logger) *HostWatcher {
	if log == nil {
		log = slog.Default()
	}
	return &HostWatcher{actions: actions, wakes: wakes, leases: leases, log: log, now: time.Now}
}

// Run runs the sweep loop and blocks, like runwatcher: one sweep at startup, then on the ticker.
func (w *HostWatcher) Run(ctx context.Context) error {
	w.tick(ctx)
	t := time.NewTicker(hostWatchScanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			w.tick(ctx)
		}
	}
}

func (w *HostWatcher) tick(ctx context.Context) {
	// Gate one: Redis leader election. With no leases or a lock error, scan as usual (same
	// trade-off as runwatcher); correctness is guarded by the storage-layer CAS.
	if w.leases != nil {
		lease, ok, err := w.leases.Acquire(ctx, hostWatchLeaderKey, hostWatchLeaderTTL)
		if err != nil {
			w.log.Warn("宿主机巡检抢锁失败，本轮仍继续扫（正确性靠 PG CAS）", slog.Any("err", err))
		} else if !ok {
			return
		} else {
			defer func() {
				if err := lease.Release(context.WithoutCancel(ctx)); err != nil {
					w.log.Debug("释放宿主机巡检锁失败", slog.Any("err", err))
				}
			}()
		}
	}

	now := w.now()
	hostIDs, truncated, err := w.actions.SweepOfflineHosts(ctx, now.Add(-hostOfflineAfter), hostWatchMaxPerTick)
	if err != nil {
		w.log.Error("宿主机掉线巡检失败", slog.Any("err", err))
		return
	}
	if truncated {
		w.log.Warn("宿主机掉线巡检命中数超出单轮上限，剩余主机下一轮处理",
			slog.Int("limit", hostWatchMaxPerTick))
	}
	for _, id := range hostIDs {
		// Wake the heartbeat and exec waiters blocked on this host: they wake, read offline/done, and finish on their own.
		w.wakes.Wake(id)
		w.log.Info("宿主机心跳超时已置离线，未完成工具调用按离线失败", slog.String("host", id))
	}
	// Purge is done every round along the way.
	if n, err := w.actions.PurgeDoneTermDispatches(ctx, now.Add(-hostTermDispatchRetention)); err != nil {
		w.log.Debug("清理过期终端帧失败", slog.Any("err", err))
	} else if n > 0 {
		w.log.Debug("已清理过期终端帧", slog.Int64("rows", n))
	}
	if n, err := w.actions.PurgeDoneExecDispatches(ctx, now.Add(-hostExecDispatchRetention)); err != nil {
		w.log.Debug("清理过期 exec 调用失败", slog.Any("err", err))
	} else if n > 0 {
		w.log.Debug("已清理过期 exec 调用", slog.Int64("rows", n))
	}
}
