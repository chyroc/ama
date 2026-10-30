// Server-side resident subscriptions: **sessions nobody is watching still need a listener**.
//
// # Which gap this fills
//
// Event persistence hangs on the SSE path (forwarding and writing to the DB happen in the
// same place; see the file header of core/chat.go). So for a turn where the user "sends a
// message and closes the page", the agent's reply **does not get into history**: after a
// refresh they only see their own message, even though the agent did answer. This gap sat in
// core/README.md for a long time; that TODO said "to fill it you need a background subscriber,
// which first requires answering 'who owns it, and which instance does it run on'".
// This file is that answer.
//
// # Only subscribe to sessions "still running", not idle ones
//
//	running / rescheduling / requires_action   subscribe
//	idle / terminated                          do not subscribe
//
// The value set matches feishu's isTerminalRunStatus exactly. **requires_action in particular
// is not terminal**: the agent is waiting for a tool result and continues after the worker
// returns it. Treating it as terminal would disconnect just as the agent is about to go on,
// and that part of the reply would never be stored.
//
// Two reasons not to subscribe to idle sessions; the second matters more:
//
//  1. idle sessions have no events to receive, so a subscription only wastes a long
//     connection;
//  2. **a container pauses after 60 seconds idle to give back compute** (see
//     shared/infra/dockertools/README.md). Unsubscribing when a turn ends keeps resident
//     subscriptions from making the session "look still alive".
//
// # Coexisting with existing subscribers is safe; existing mechanisms already guarantee it
//
// One Ark session having several subscribers at once is **normal** (browser SSE, feishu
// inbound, feishu resume, scheduled tasks, and now this file). Duplicates do not create two
// rows: AppendRunEvent is idempotent by the Ark event id (PG via the run_events_event_id_idx
// unique index, MemStore via a scan before writing), and on a hit it returns **the existing
// row**, with the same sequence.
// TestConcurrentSubscribersDoNotDuplicateEvents pins this; behind it is a real production
// incident.
//
// ⚠️ Usage counting used to leak here (addUsage in applyEventToRun was `+=` without checking
// created). Shipping this feature would turn it from occasional into certain, so it was fixed
// first. Do not remove that gate.
//
// # This is a distributed task, written with scheduler.go's two gates
//
//	Gate one (Redis lease, **optional**)  the whole scan takes one lock and only scans when it
//	                                      wins. It saves N-1 instances useless scans; it is an
//	                                      **optimization**.
//	Gate two (PG atomic claim, **required**)  only one instance subscribes to a session at a
//	                                      time. It is **correctness**.
//
// Gate one alone is not enough: Redis can be left unconfigured entirely (the service still
// starts). If correctness depended on it, "no Redis configured" would mean N instances
// subscribing to the same session at once. That would **not** corrupt data (dedup covers it),
// but it would use N times the Ark connections, so gate two should still block it.
package core

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/ma"
)

// Default pace and lease parameters of the scan.
const (
	// watchScanInterval is the interval between two scans.
	//
	// **30 seconds**: this decides "after the user closes the page, how long until someone
	// takes over listening". Too long, and events in that gap are still lost (Ark SSE does not
	// replay; only SyncRunEvents can fill them in, and that waits until someone next opens
	// the page). Too short, and every round scans the DB for nothing.
	//
	// Matching the scheduler's 30 seconds is a coincidence; they are unrelated, and changing
	// one does not require changing the other.
	watchScanInterval = 30 * time.Second

	// watchLeaderKey is the Redis key used for leader election.
	//
	// **Different from the scheduler / skillupdate keys**: the three jobs are unrelated, and
	// sharing one key would make them starve each other, which shows up as "sometimes it does
	// not run", the hardest kind to debug.
	watchLeaderKey = "ama:runwatch:leader"

	// watchLeaderTTL is the leader lease duration.
	//
	// It only covers **one scan** (scan the DB + start goroutines, both fast), not the
	// subscriptions themselves: subscriptions run asynchronously and their mutual exclusion is
	// gate two's job. So this value does not need to be long.
	watchLeaderTTL = 45 * time.Second

	// watchMaxConcurrent is how many resident subscriptions this instance holds at once.
	//
	// Each subscription is one long-lived HTTP connection + one goroutine. The cap prevents
	// "hundreds of sessions active at once eat the connection pool", which would slow down
	// normal chat requests too.
	//
	// The overflow is picked up next round, **and the log must say how many were skipped**;
	// otherwise "silently truncated" reads as "all subscribed" (same as skillupdater's
	// maxPerTick).
	watchMaxConcurrent = 64

	// watchTurnTimeout is the fallback duration of one resident subscription.
	//
	// Normal exit relies on turnEnded (disconnect when a terminal frame is seen); this is only
	// a fallback: Ark does not close the stream on its own (it keeps a long connection per
	// session waiting for the next user message), so if the terminal frame somehow never
	// arrives, without this cap the goroutine hangs forever.
	//
	// 10 minutes; it only limits the length of this one resident subscription. While the
	// session is still running, later scans subscribe again, so it does not limit how long
	// feishu waits for a turn's reply.
	watchTurnTimeout = 10 * time.Minute

	// watchLeaseFor is the claim lease duration for one session subscription.
	//
	// **It must be clearly longer than watchTurnTimeout**; otherwise the lease expires while
	// the subscription is still open, and another instance subscribes to the same session
	// again (wasting a connection). 15 minutes: 10 minutes of fallback timeout plus 5 minutes
	// of margin for cleanup and clock drift.
	//
	// It cannot be too long either: when the holder is rebuilt nobody releases it, and only
	// expiry lets someone else take over. During that time nobody listens to this session.
	// Same as claimLease for schedules, with the same value.
	watchLeaseFor = 15 * time.Minute

	// runStaleAfter is "how long without activity before a session is considered stuck".
	//
	// # Setting this too small is worse than not doing it at all
	//
	// Too small, and sessions that are **working normally but slow on this step** get judged
	// as zombies and pushed to a terminal state: the user's answer is cut off, and the symptom
	// looks exactly like a real zombie (a terminated run), which in turn makes people think
	// the fallback did not work. So the lower bound must be "the longest silence a live work
	// item can have", three parts added up, each with a source:
	//
	//	45 s    arkReclaimOlderThan: the window from a work losing heartbeat to another worker
	//	        reclaiming it (must be earlier than Ark's 90-second server-side cutoff; see
	//	        that constant's comment)
	//	120 s   the tool execution ceiling (see the four timeout layers in core/worker.go; not
	//	        configurable)
	//	10 min  watchTurnTimeout: the fallback length of one subscription, possibly with no
	//	        frame at all
	//
	// About 12.75 minutes in total. We use **30 minutes**, leaving 2x margin for clock drift
	// and queueing on the Ark side.
	//
	// The upper bound comes from "how long users can tolerate": until a stuck session is
	// resolved it can neither send new messages (Ark returns 400 RequiresActionRejected) nor
	// stop holding a resident subscription that restarts every 10 minutes. Half an hour is a
	// value both sides can accept.
	//
	// ⚠️ **Do not use "one turn lasts at most 10 minutes" to argue for setting this to 10
	// minutes.** That cap applies to one subscription, not the whole session: a session can
	// run many turns in a row, with normal silence between them.
	runStaleAfter = 30 * time.Minute

	// reapMaxPerTick is the most sessions one scan retires.
	//
	// The cap prevents "hundreds of zombies piled up in the DB, all fetched in one round and
	// written one by one" from dragging this tick out. The rest is retired next round, **and
	// the log must say how many were cut**; otherwise "silently retired fewer" reads as "all
	// cleaned up" (same as watchMaxConcurrent and skillupdater's maxPerTick).
	reapMaxPerTick = 64
)

// RunWatcherConfig wires up the resident subscription scan.
type RunWatcherConfig struct {
	// Service is required.
	Service *Service
	// Leases may be empty: if empty there is no leader election and every instance scans
	// (correctness is covered by the PG claim).
	Leases Leases
	// Logger defaults to slog.Default() when empty.
	Logger *slog.Logger
	// ScanInterval defaults to watchScanInterval when empty; tests can inject a shorter value.
	ScanInterval time.Duration
	// Owner identifies this instance, used only for debugging. Defaults to the host name.
	Owner string
}

// RunWatcher is the resident scan that "subscribes to sessions still running".
type RunWatcher struct {
	cfg   RunWatcherConfig
	log   *slog.Logger
	owner string
	scan  time.Duration

	// watching holds sessions this instance already subscribes to, so one round does not
	// start duplicate goroutines.
	//
	// **It is not the correctness guard** (that is the PG claim): it cannot handle exclusion
	// across instances, and it is gone when the process restarts. It only saves useless work
	// inside this instance: while a subscription started last round is still running, this
	// round should not start another one for the same session.
	watching sync.Map // runID → struct{}

	// wg waits for all subscription goroutines to finish; used at shutdown.
	wg sync.WaitGroup
}

// NewRunWatcher creates a resident subscription scan.
//
// It returns nil when Service or runner is empty: without Ark wired in there is nothing for
// it to do, and the caller uses that to decide whether to start it (same convention as
// NewScheduler / NewSkillUpdater).
func NewRunWatcher(cfg RunWatcherConfig) *RunWatcher {
	if cfg.Service == nil || cfg.Service.runner == nil {
		return nil
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	owner := cfg.Owner
	if owner == "" {
		owner, _ = os.Hostname()
		if owner == "" {
			owner = "unknown"
		}
	}
	scan := cfg.ScanInterval
	if scan <= 0 {
		scan = watchScanInterval
	}
	return &RunWatcher{cfg: cfg, log: log, owner: owner, scan: scan}
}

// Run starts and blocks until ctx is canceled.
//
// Same shape as Scheduler.Run / SkillUpdater.Run (a single blocking `Run(ctx) error`), so
// cmd/amad can attach it with one g.Go line.
func (w *RunWatcher) Run(ctx context.Context) error {
	w.log.Info("会话常驻订阅巡检启动",
		slog.String("owner", w.owner),
		slog.Duration("scan_interval", w.scan),
		slog.Bool("leader_election", w.cfg.Leases != nil))

	ticker := time.NewTicker(w.scan)
	defer ticker.Stop()

	// **Scan once at startup.**
	//
	// Same as Scheduler, opposite of SkillUpdater: after a restart, the sessions the previous
	// process was subscribed to have no subscriber at all right now (in-process state dies
	// with the process), while those sessions **keep running** on the Ark side. Waiting one
	// scan period would leave the events from those 30 seconds to be filled in later by
	// SyncRunEvents.
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			// Wait for running subscriptions to finish. **Do not return right away**: those
			// goroutines are writing to the DB (storing events, updating run status), and
			// exiting the process directly would cut them off midway.
			w.log.Info("会话常驻订阅巡检停止，等在跑的订阅收尾")
			w.wg.Wait()
			return nil
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// tick runs one round: (optionally) elect a leader → list running sessions → subscribe to the
// ones nobody is listening to.
func (w *RunWatcher) tick(ctx context.Context) {
	// Gate one: if Redis is configured, try the lock first; if we do not get it, skip this
	// tick.
	//
	// The lock is held only **during this scan** (scan the DB + start goroutines), not for the
	// subscriptions: those are asynchronous and their exclusion is gate two's job. Holding it
	// long would tie "who is leader" to one instance, and if it dies others must wait for the
	// TTL to take over.
	if w.cfg.Leases != nil {
		lease, ok, err := w.cfg.Leases.Acquire(ctx, watchLeaderKey, watchLeaderTTL)
		if err != nil {
			// A failed lock attempt (Redis glitch) **must not turn into not scanning**: then
			// when Redis is down, the "unwatched sessions do not store events" gap comes back
			// entirely, while correctness never depended on Redis.
			w.log.Warn("抢订阅巡检锁失败，本轮照常扫（重复由 PG 认领兜住）",
				slog.Any("err", err))
		} else if !ok {
			// Another instance is scanning. This is a normal result, not an error.
			return
		} else {
			defer func() {
				if err := lease.Release(context.WithoutCancel(ctx)); err != nil {
					w.log.Warn("释放订阅巡检锁失败，下一拍要等它过期", slog.Any("err", err))
				}
			}()
		}
	}

	// Retire stuck sessions first, then subscribe to live ones.
	//
	// **The order must not be reversed**: retiring pushes zombies to a terminal state, so they
	// do not appear in ListActiveRuns below, which saves "subscribing to a session that will
	// never have events and hanging for the full 10 minutes". Reversed, every round would first
	// subscribe to the zombies, and this round's retirement would no longer help them.
	w.reapStaleRuns(ctx)

	runs, err := w.cfg.Service.store.ListActiveRuns(ctx)
	if err != nil {
		w.log.Warn("列出进行中的会话失败，本轮跳过", slog.Any("err", err))
		return
	}

	started, skipped := 0, 0
	for _, run := range runs {
		if ctx.Err() != nil {
			return
		}
		// This instance is already listening to this one (started last round, not done yet).
		if _, busy := w.watching.Load(run.ID); busy {
			continue
		}
		// Someone is already listening: the browser has the chat page open, the feishu turn is
		// running, or a scheduled task is waiting for the result.
		// No need to start another: events are already being stored, and one more only wastes
		// a connection.
		//
		// ⚠️ This check **is only valid within this process** (activeStreams is an in-process
		// reference count). Whether other instances have subscribers is invisible here; the PG
		// claim below decides that.
		if w.cfg.Service.hasActiveStream(run.ID) {
			continue
		}
		if started >= watchMaxConcurrent {
			skipped++
			continue
		}
		// Gate two: PG atomic claim. **Correctness lives here**, not in the Redis lock above.
		//
		// Failing to claim means another instance is subscribing to it (or it just became
		// terminal), so skip it. This is a normal result, not an error. If the lease expires
		// without renewal, whoever scans it next round takes over.
		claimed, err := w.cfg.Service.store.ClaimRunWatch(
			ctx, run.ID, w.owner, time.Now(), watchLeaseFor)
		if err != nil {
			w.log.Warn("认领会话订阅失败，跳过这条",
				slog.String("run", run.ID), slog.Any("err", err))
			continue
		}
		if !claimed {
			continue
		}
		started++
		w.watchOne(ctx, run.ID)
	}

	if started > 0 || skipped > 0 {
		// Log only when work was actually done; otherwise idle-loop logs drown out useful ones.
		w.log.Info("会话常驻订阅巡检完成",
			slog.Int("active", len(runs)),
			slog.Int("started", started),
			// **Say when it was truncated**: otherwise "silently subscribed fewer" reads as
			// "all subscribed".
			slog.Int("skipped_over_cap", skipped))
	}
}

// retireMissingRun pushes a session that "no longer exists on the Ark side" to a terminal
// state, so it is not subscribed to again.
//
// # Why it must be written to the DB, not just skipped
//
// If only skipped, this run's status stays running forever, so **every scan picks it up and
// subscribes again**: a 404 every 30 seconds, in an endless loop. This was hit on the very
// first real run.
//
// Set to terminated, not idle: idle means "this turn is answered, waiting for the next
// message", while this session **no longer exists**, and sending another message would only
// 404. terminated is the truth; the frontend then shows "terminated" instead of leaving the
// user waiting on a session that cannot send messages.
//
// Failures are only logged: the next round tries again, which is better than making the whole
// subscription path fail.
func (w *RunWatcher) retireMissingRun(ctx context.Context, runID string) {
	w.log.Info("方舟侧已无此会话，标记终止并停止订阅", slog.String("run", runID))

	// Use a ctx without cancellation: the process may be shutting down, and if this step is not
	// done the next startup hits the same thing again.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	run, err := w.cfg.Service.store.GetRun(writeCtx, runID)
	if err != nil {
		// The run is gone too (deleted), so there is even less to do.
		return
	}
	run.Status = apimodel.RunTerminated
	if run.ErrorSummary == "" {
		// Give the user a message they can understand. **Do not write "service error"**: this
		// is not an outage; the execution environment was reclaimed (container TTL 1 hour), and
		// sending a new message to start a new session is enough.
		run.ErrorSummary = "会话的执行环境已被回收，这条会话不能再继续了；重新发起一次对话即可。"
	}
	w.cfg.Service.closeRunAt(&run, retireEndedAt(run))
	if _, err := w.cfg.Service.store.SaveRun(writeCtx, run); err != nil {
		w.log.Warn("标记会话终止失败，下一轮还会再试",
			slog.String("run", runID), slog.Any("err", err))
	}
}

// retireEndedAt computes an honest end time for a session retired by the fallback.
//
// It uses `lastActivityAt` (falling back to `startedAt` if missing, same rule as
// persistmodel.LastActivityOf): the last moment the session actually did something, which is
// when it really stopped.
//
// **It cannot use `now`**: retirement happens long after the session stopped (zombie
// detection needs runStaleAfter of silence, and older ones may have piled up for days). Using
// now records "how long until it was noticed" rather than "how long it ran". A batch retired
// after a production incident had exactly this shape: a session active for a few seconds was
// recorded as lasting more than a day, and durationMs is a stored field that the "duration"
// column of run history and the overview page renders directly.
//
// If it cannot be parsed (old data with odd shapes), fall back to now: in that case a value
// that is too large is better than dropping endedAt entirely, because a missing endedAt makes
// the frontend treat this terminated run as "not finished yet".
func retireEndedAt(run apimodel.Run) time.Time {
	if t, err := time.Parse("2006-01-02T15:04:05.000Z", persistmodel.LastActivityOf(run)); err == nil {
		return t
	}
	return time.Now().UTC()
}

// reapStaleRuns pushes sessions that "have been quiet for a long time but still show an active
// status" to a terminal state.
//
// # Which gap it covers
//
// Each of the three active states has a normal way out, but **none is guaranteed to happen**:
// requires_action waits for the worker to return `user.tool_result`, and that result may
// never come: the worker's pod is replaced while a tool is halfway (rolling deploy, failed
// health check, scale-down), the Ark-side work is settled early by StopWork, or the container
// cannot be created because quota is tight. **None** of these paths has an owner responsible
// for pushing the run to a terminal state, so it stays there forever.
//
// A production incident was exactly this: about one in ten runs were stuck in
// requires_action, the pending tools were all ordinary container tools like bash / read (not
// a problem with one specific tool), and the oldest had been stuck for days. Each zombie was
// also picked up by ListActiveRuns above every 30 seconds, subscribed again, and held for the
// full 10-minute fallback timeout: **the more pile up, the more Ark long connections are
// wasted**.
//
// # Why it lives here instead of a new scan
//
// It and the resident subscriptions are two halves of the same problem ("who listens" and
// "who cleans up when nobody can hear"), sharing one scan, one leader lock, and one 30-second
// pace. A new goroutine would just add another Redis key to maintain, and three unrelated jobs
// sharing one key would starve each other (see the comment on watchLeaderKey).
//
// # Distributed safety rests on CAS, not on the leader lock
//
// The Redis lock above is an **optimization** (saving N-1 instances duplicate scans) and can
// be absent entirely. Correctness comes from RetireStaleRun's conditional update: the rule is
// in the UPDATE's WHERE, whoever changes it first wins, and later ones match no row. This also
// blocks the most important race, **a late tool_result arriving right at this moment**: it
// refreshes lastActivityAt, so this UPDATE misses, and the just-revived session is not killed
// again.
func (w *RunWatcher) reapStaleRuns(ctx context.Context) {
	// Write to the DB with a ctx without cancellation: the process may be shutting down, and if
	// this step is skipped the zombies stay until the next startup.
	// The read goes in too, so ctx does not become Done halfway through the read.
	reapCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	staleBefore := apimodel.FormatISO(time.Now().UTC().Add(-runStaleAfter))
	stale, err := w.cfg.Service.store.ListStaleActiveRuns(reapCtx, staleBefore)
	if err != nil {
		w.log.Warn("列出卡死的会话失败，本轮跳过回收", slog.Any("err", err))
		return
	}
	if len(stale) == 0 {
		return
	}

	reaped, skipped := 0, 0
	for _, run := range stale {
		if reaped >= reapMaxPerTick {
			skipped++
			continue
		}
		// Set terminated, not idle: idle means "this turn is answered, waiting for the next
		// message", while this session **was cut off while stuck**. The Ark side is also stuck
		// in requires_action, and a new message would get 400 RequiresActionRejected.
		// terminated is the truth; the frontend then shows "terminated" instead of leaving the
		// user waiting on a session that cannot send messages. Same choice as retireMissingRun.
		run.Status = apimodel.RunTerminated
		if run.ErrorSummary == "" {
			// Give the user a message they can understand and that **tells them what to do
			// next**. Do not write "service error": in most cases the execution environment was
			// replaced (deploy, scale-down) so the tool result could not be returned, and
			// starting a new turn fixes it.
			run.ErrorSummary = "这轮对话的工具结果没有返回（执行环境可能已被回收或替换），" +
				"会话已自动结束；重新发起一次对话即可。"
		}
		w.cfg.Service.closeRunAt(&run, retireEndedAt(run))

		ok, err := w.cfg.Service.store.RetireStaleRun(reapCtx, run, staleBefore)
		if err != nil {
			w.log.Warn("回收卡死的会话失败，下一轮还会再试",
				slog.String("run", run.ID), slog.Any("err", err))
			continue
		}
		if !ok {
			// Meanwhile it came back to life by itself (a late tool_result arrived), or another
			// instance retired it first. Both are normal results, not errors.
			continue
		}
		reaped++
		// Log each one: zombies are **evidence of a fault**, not daily noise. Stating the run
		// and how long it was stuck lets the next investigation answer "is this occasional or
		// growing".
		w.log.Warn("回收卡死的会话：工具结果一直没回来，已标记终止",
			slog.String("run", run.ID),
			slog.String("workspace", run.WorkspaceID),
			slog.String("last_activity", persistmodel.LastActivityOf(run)),
			slog.Duration("stale_after", runStaleAfter))
	}

	if reaped > 0 || skipped > 0 {
		w.log.Warn("卡死会话回收完成",
			slog.Int("found", len(stale)),
			slog.Int("reaped", reaped),
			// **Say when it was truncated**; otherwise "silently retired fewer" reads as "all
			// cleaned up".
			slog.Int("skipped_over_cap", skipped))
	}
}

// watchOne attaches a resident subscription to one session, running asynchronously.
//
// # Why one goroutine per session instead of serial
//
// Subscribing **blocks** (once attached it lasts a whole turn, possibly minutes). Serially, all
// later sessions would wait until the first finishes. That is exactly the current shape of
// feishu's ResumeInflightTurns; it is acceptable there because "resume after restart" is a
// rare one-time action with few sessions, while this scan runs all the time.
func (w *RunWatcher) watchOne(ctx context.Context, runID string) {
	w.watching.Store(runID, struct{}{})
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer w.watching.Delete(runID)
		// **Releasing the lease must happen**; otherwise this session waits a full lease (15
		// minutes) before being subscribed again, which shows up as "after closing the page,
		// events are still not stored for a while", the very symptom this feature fixes. Use a
		// ctx without cancellation: when the process is shutting down ctx is already Done, a
		// write with it fails right away, and the lease leaks in the DB (same as the
		// scheduler's finish).
		defer func() {
			relCtx, relCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer relCancel()
			if err := w.cfg.Service.store.ReleaseRunWatch(relCtx, runID); err != nil {
				w.log.Warn("释放会话订阅租约失败，下次要等它过期",
					slog.String("run", runID), slog.Any("err", err))
			}
		}()

		// The subscription's lifetime does not follow this tick: the stream must keep
		// listening after tick returns. For the fallback timeout see watchTurnTimeout.
		runCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), watchTurnTimeout)
		defer cancel()

		// It must be woken up when the process shuts down; otherwise wg.Wait() waits until the
		// fallback timeout.
		stop := context.AfterFunc(ctx, cancel)
		defer stop()

		w.log.Info("开始常驻订阅", slog.String("run", runID))

		// Last rescue: when the work was stopped by Ark and poll reclaim has no hope left, the
		// single holder of this PG claim sends the missing tool_result. For the rule and why it
		// lives here, see runrescue.go.
		go w.rescueTick(runCtx, runID)

		// **We must recognize the terminal frame ourselves and close the stream.**
		//
		// Ark **does not close SSE** after end_turn (it keeps a long connection per session,
		// waiting for the next user message), so StreamRunEvents does not return on its own.
		// Without this step every subscription hangs for the full watchTurnTimeout (10
		// minutes). Under a resident scan that quickly piles up goroutines and connections, and
		// the logs only show "subscription started", never the end.
		//
		// ⚠️ feishu's resume.go misses exactly this step (it relies on runTimeout as a
		// fallback); that path is rare, so the symptom is mild. **Do not copy it**; copy the
		// two places in inbound.go / scheduler.go instead.
		//
		// The rule is exactly the same as turnEnded: the idle of requires_action does not count
		// as ended.
		// drained records "whether one message was sent from the queue when this turn ended".
		//
		// If one was sent, a new turn has started and the session is still active: on cleanup
		// below the lease must be released right away so the next tick can subscribe again,
		// instead of waiting a scan period.
		drained := false
		err := w.cfg.Service.StreamRunEvents(runCtx, runID, func(raw []byte) error {
			if turnEnded(raw) {
				// **This moment is the only reliable signal of "the next message can be sent".**
				//
				// "The run becomes idle" cannot replace it: requires_action is also idle on the
				// Ark side, but it means "stuck waiting for a tool result" and the agent will
				// continue; a queued message inserted then would interrupt it. turnEnded already
				// makes that distinction.
				//
				// ⚠️ But turnEnded is one notch wider than draining: **a user interrupt also counts
				// as turnEnded** (stop_reason is user_interrupt), and then the queue head must
				// never be taken; see the notes on drainAllowed.
				//
				// Draining happens **before** closing the stream: the stream is still open, so the
				// events caused by the message just sent can be received by this subscription
				// directly, without waiting for the next tick to subscribe again.
				if drainAllowed(raw) {
					drained = w.cfg.Service.DrainPendingMessage(runCtx, runID, w.owner)
				}

				// Cancel ctx so StreamRunEvents returns. **Do not return an error here**: that
				// would be logged as a warning "subscriber failed", while this is a normal finish.
				//
				// ⚠️ Disconnect even if one message was just drained: that message makes Ark start
				// a new turn, and part of this subscription's fallback timeout is already used up.
				// Letting the next tick subscribe again with a full budget is cleaner than
				// extending it here forever.
				cancel()
			}
			return nil
		})
		// Our own disconnect is a normal finish, not a fault. runCtx expiring (fallback timeout)
		// does not count either: that path only means this turn was very long, and the events
		// were still stored.
		if err != nil && runCtx.Err() == nil {
			// **The Ark side has no such session at all**: retrying will never succeed, so it
			// must be settled on the spot; otherwise it is resubscribed every 30 seconds,
			// spamming WARN in an endless loop.
			//
			// This was hit as soon as it ran on a real machine (every round of the first run):
			// run_4 / run_5 in the seed data have fake session ids, and production has a real
			// source too: a session idle longer than the container TTL (1 hour) is reclaimed
			// by the platform, the Ark-side session is gone, while the status in our DB still
			// says running (with no subscriber nobody pushes it to a terminal state; that is
			// the very gap this feature fills, so it became a victim of that gap itself).
			if ma.IsNotFound(err) {
				w.retireMissingRun(ctx, runID)
				return
			}
			w.log.Warn("常驻订阅异常结束",
				slog.String("run", runID), slog.Any("err", err))
			return
		}
		if drained {
			// Say "another message was sent from the queue"; otherwise the log only shows one
			// subscription ending, while a new turn has actually started, which is confusing
			// when debugging "why does this session keep running". The lease is released in the
			// defer above, so the next tick can subscribe again.
			w.log.Info("常驻订阅结束，已从队列发出下一条", slog.String("run", runID))
			return
		}
		w.log.Info("常驻订阅结束", slog.String("run", runID))
	}()
}
