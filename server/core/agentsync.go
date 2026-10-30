// "Sync an agent to Ark right after it is saved": enqueueing, running and settling the
// background task.
//
// # Which gap this file fills
//
// An Ark Agent is created once and cached forever, and catching up **only happens at the
// moment a new chat is opened** (ensureBinding → syncBindingSkills / syncBindingSpec in
// chat.go). Meanwhile `Service.UpdateAgent` only writes the database and does `Version++`
// from start to end, and never touches the runner.
//
// So when a user edits the prompt and clicks save, PATCH returns 200 and the database really
// changes, but **nothing happens on the Ark side** until someone next opens a new chat. It
// looks like "I changed it and it acts like I didn't", with zero errors.
//
// This file makes that happen on save: enqueue a task → claimed in the background → run the
// same ensureBinding → write the result back to the task row for the frontend to query.
//
// # ⚠️ This **does not rewrite the sync logic**, it only adds a trigger for it
//
// The sync body, the fingerprint, and the vital invariant "the fingerprint is written back
// only after Ark confirms" are all already in chat.go. This file calls that same
// ensureBinding.
//
// Copying a "slimmed-down sync" is the easiest and most costly mistake here: get the timing of
// any one of those three fingerprint write-backs wrong (chat.go:486 / :565 / :640) and the
// repair path is blocked forever. Once the fingerprint is false, every later comparison
// matches, and that agent never catches up again, with zero errors.
//
// # Lives alongside lazy creation at chat time, not instead of it
//
// See the comment at the end of runOnce: both paths go through the same ensureBinding, the
// fingerprint decides, and so it is naturally idempotent. A chat **never waits** for this
// background task.
package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/ma"
)

const (
	// syncScanInterval is how often we scan for "agents waiting to sync".
	//
	// 15 seconds: the normal trigger for this path is Kick (at save time this instance scans
	// right away), and scanning is only a **fallback**. It covers two cases: "the instance that
	// enqueued is not the one that runs it" and "the Kick was lost". So it does not need to be
	// frequent, but it cannot be too rare either: on the fallback path the user is waiting
	// (the frontend is polling).
	syncScanInterval = 15 * time.Second

	// syncClaimLease is how long a claim lease lasts.
	//
	// **It must be clearly longer than one sync round** (syncRunTimeout is 2 minutes), or the
	// lease expires before the run is done and another instance takes it and runs it again.
	// It cannot be too long either: when an instance is recreated nobody releases the lease,
	// only expiry makes the task runnable again, so the worst-case stall equals the lease
	// length. Chosen the same way as the scheduler's claimLease (15min / 10min there).
	syncClaimLease = 5 * time.Minute

	// syncRunTimeout is the limit for one sync round.
	//
	// A sync makes three or four round trips to Ark (create environment / upload skills /
	// create or update Agent), and 2 minutes is plenty. Without it, one stuck call would hold
	// the lease until it expires, and the frontend keeps spinning.
	syncRunTimeout = 2 * time.Minute

	// syncClaimBatch is the max number of tasks claimed per round.
	syncClaimBatch = 20

	// syncMaxAttempts is the max number of runs for one task.
	//
	// When reached, the task ends in failed and the reason is left for the user. **It cannot
	// retry forever**: an agent that can never sync (key revoked, skills over the limit) would
	// be reclaimed again and again by the lease and rerun every cycle, flooding both Ark and
	// the logs. Same reason as pendingMaxAttempts for pending_messages.
	syncMaxAttempts = 3

	// syncLeaderKey is the Redis lock name for gate one.
	//
	// **Separate from the scheduler / runwatch keys**: sharing one would let two unrelated
	// scans starve each other.
	syncLeaderKey = "ama:agentsync:leader"
	syncLeaderTTL = 45 * time.Second

	// syncRetention is how long finished tasks are kept.
	//
	// One day: frontend polling only looks at the latest one; keeping them is for manual
	// checks of "did yesterday's save actually sync". Without cleanup this table only grows
	// (same lesson as pgmq: a cleanup function was written but nobody called it).
	syncRetention = 24 * time.Hour
)

// newAgentSyncTaskID generates a task ID.
//
// ⚠️ **Do not use s.nextID**: that is an **in-process** counter (idSeq in service.go) and will
// collide with many instances. A collision means an insert primary key conflict, and that
// insert is exactly where "two quick clicks run only once" is decided, so the conflict would
// be misread as "a task is already running", and this save's sync would be silently
// swallowed. Same technique as newTerminalID / arkWorkerID: a random suffix prevents
// collisions.
func newAgentSyncTaskID() string {
	var b [9]byte
	if _, err := rand.Read(b[:]); err != nil {
		// If random bytes are unavailable, fall back to a timestamp. **Do not panic**: a sync
		// task should not fail to enqueue because of this.
		return fmt.Sprintf("astask_%d", time.Now().UnixNano())
	}
	return "astask_" + hex.EncodeToString(b[:])
}

// EnqueueAgentSync queues one "sync to Ark" for an agent.
//
// Called by CreateAgent / UpdateAgent after a successful save, and when the frontend clicks
// "retry".
//
// # Every failure degrades; it never drags the save down with it
//
// An enqueue failure only logs a Warn and returns a zero value: **the save itself already
// succeeded**. Returning an error at that point would make the user see "save failed" while
// the database actually changed, a much worse lie than "not synced". The cost of not
// enqueueing is falling back to the old behavior: the lazy-creation path will catch it up the
// next time a chat is opened.
func (s *Service) EnqueueAgentSync(ctx context.Context, a apimodel.Agent) persistmodel.AgentSyncTaskRow {
	row, created, err := s.enqueueSyncOnce(ctx, a)
	if err != nil {
		s.log().Warn("给 agent 排同步任务失败，改动要等下次开对话才追平",
			slog.String("workspace", a.WorkspaceID), slog.String("agent", a.ID),
			slog.Any("err", err))
		return persistmodel.AgentSyncTaskRow{}
	}
	// ⚠️ **Zero value + no error** is that very narrow window: the insert hit an existing task,
	// and that task finished just before the following update (so neither statement matched).
	//
	// **We must try again here, not pretend nothing happened.** Returning directly would let
	// this save's change silently not sync, and it happens exactly at "the last round just
	// ended", the moment that most needs a sync, with zero errors and zero logs. The retry
	// always succeeds: the one in the way is already in a terminal state, and the unique index
	// no longer blocks.
	if err == nil && row.ID == "" {
		row, created, err = s.enqueueSyncOnce(ctx, a)
		if err != nil || row.ID == "" {
			s.log().Warn("排同步任务撞上窄窗口且重试仍未成功，改动要等下次开对话才追平",
				slog.String("workspace", a.WorkspaceID), slog.String("agent", a.ID),
				slog.Any("err", err))
			return persistmodel.AgentSyncTaskRow{}
		}
	}
	if created {
		// Kick only when a task was **really created**: if we hit an existing task, that one is
		// already running or queued, and another kick only wastes a scan.
		s.kickAgentSync()
	}
	return row
}

// enqueueSyncOnce enqueues once, with no retry.
func (s *Service) enqueueSyncOnce(
	ctx context.Context, a apimodel.Agent,
) (persistmodel.AgentSyncTaskRow, bool, error) {
	now := time.Now().UTC()
	return s.store.EnqueueAgentSyncTask(ctx, persistmodel.AgentSyncTaskRow{
		ID:          newAgentSyncTaskID(),
		WorkspaceID: a.WorkspaceID,
		AgentID:     a.ID,
		Status:      string(apimodel.AgentSyncPending),
		CreatedAt:   now,
		UpdatedAt:   now,
	})
}

// kickAgentSync makes this instance's syncer scan right away instead of waiting for the next
// tick.
//
// # It is only an optimization; correctness does not depend on it
//
// The channel is **buffered and non-blocking**: if nobody is receiving (syncer not started,
// or busy with the last scan), the kick is just dropped. Dropping it only delays by one
// syncScanInterval (15 seconds), and correctness rests on the atomic claim in PG. This is the
// same argument as the scheduler's "gate one is an optimization, gate two is correctness".
//
// ⚠️ **Do not change it to a blocking send**: that would tie the save API to the progress of a
// background goroutine, and if the syncer gets stuck the whole PATCH hangs.
func (s *Service) kickAgentSync() {
	s.syncKickMu.RLock()
	ch := s.syncKick
	s.syncKickMu.RUnlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// AgentSyncer is the long-running worker for "sync to Ark after an agent is saved".
//
// Same shape as Scheduler (a single blocking `Run(ctx) error`), so cmd/amad can hook it up
// with one g.Go line. The two gates are the same too:
//
//	gate one (Redis lease, **optional**)  saves N-1 instances from useless scans; an optimization.
//	gate two (PG atomic claim, **required**)  Store.ClaimDueAgentSyncTasks; correctness.
//
// **Gate one alone is not enough**: Redis can be left unset entirely (the service still
// starts), and if correctness depended on it, "no Redis configured" would mean "N instances
// syncing the same agent at once".
type AgentSyncer struct {
	svc    *Service
	leases Leases
	log    *slog.Logger
	now    func() time.Time
	owner  string
	scan   time.Duration

	wg sync.WaitGroup
	// inflight blocks "the same task run by two scans at once on this instance".
	//
	// It is only an in-process layer; across instances the PG claim blocks it (see
	// ClaimDueAgentSyncTasks).
	inflight sync.Map
}

// AgentSyncerConfig configures the worker.
type AgentSyncerConfig struct {
	// Service is required.
	Service *Service
	// Leases may be nil: then there is no leader election and every instance scans (the PG
	// claim prevents duplicates).
	Leases Leases
	// Logger empty means slog.Default().
	Logger *slog.Logger
	// Now empty means time.Now; tests can inject it.
	Now func() time.Time
	// Owner empty means the hostname (the pod name on a container platform). It only goes into
	// logs and claimed_by and does not affect correctness.
	Owner string
	// ScanInterval empty means syncScanInterval.
	ScanInterval time.Duration
}

// NewAgentSyncer creates a worker. It returns nil when Service is nil (same convention as
// NewScheduler).
func NewAgentSyncer(cfg AgentSyncerConfig) *AgentSyncer {
	if cfg.Service == nil {
		return nil
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	owner := strings.TrimSpace(cfg.Owner)
	if owner == "" {
		if h, err := os.Hostname(); err == nil {
			owner = h
		} else {
			owner = "unknown"
		}
	}
	scan := cfg.ScanInterval
	if scan <= 0 {
		scan = syncScanInterval
	}
	return &AgentSyncer{
		svc: cfg.Service, leases: cfg.Leases, log: log,
		now: now, owner: owner, scan: scan,
	}
}

// Run starts and blocks until ctx is canceled.
func (a *AgentSyncer) Run(ctx context.Context) error {
	a.log.Info("agent 方舟同步执行器启动",
		slog.String("owner", a.owner),
		slog.Duration("scan_interval", a.scan),
		slog.Bool("leader_election", a.leases != nil))

	// Register the kick channel so EnqueueAgentSync can make this instance scan right away.
	kick := make(chan struct{}, 1)
	a.svc.syncKickMu.Lock()
	a.svc.syncKick = kick
	a.svc.syncKickMu.Unlock()
	defer func() {
		a.svc.syncKickMu.Lock()
		a.svc.syncKick = nil
		a.svc.syncKickMu.Unlock()
	}()

	ticker := time.NewTicker(a.scan)
	defer ticker.Stop()

	// Scan once at startup: after a restart, tasks that "the previous instance claimed but did
	// not finish" should be taken over soon, not after waiting a full scan interval. Same
	// choice as Scheduler.Run.
	a.tick(ctx)
	purgeTicker := time.NewTicker(time.Hour)
	defer purgeTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Wait for the running rounds to finish. **Do not return directly**: those
			// goroutines are writing to the database (settling task state), and exiting the
			// process now would leave tasks in running until the lease expires.
			a.log.Info("agent 方舟同步执行器停止，等在跑的任务收尾")
			a.wg.Wait()
			return nil
		case <-ticker.C:
			a.tick(ctx)
		case <-kick:
			// Someone just saved an agent; scan now instead of waiting for the next tick.
			a.tick(ctx)
		case <-purgeTicker.C:
			a.purge(ctx)
		}
	}
}

// tick scans one round: (optionally) elect a leader → claim → start one goroutine per task.
func (a *AgentSyncer) tick(ctx context.Context) {
	// Gate one: if Redis is configured, take the lock first; if not won, skip this scan.
	if a.leases != nil {
		lease, ok, err := a.leases.Acquire(ctx, syncLeaderKey, syncLeaderTTL)
		if err != nil {
			// A lock failure (Redis glitch) **must not fall back to not scanning**: then every
			// agent would stop syncing whenever Redis is down, while correctness never depended
			// on it. Log a warning and scan as usual, with the PG claim covering duplicates.
			// Same handling as Scheduler.tick.
			a.log.Warn("抢 agent 同步锁失败，本轮照常扫（重复由 PG 认领兜住）", slog.Any("err", err))
		} else if !ok {
			// Another instance is scanning. This is a normal result, not an error.
			return
		} else {
			defer func() {
				if err := lease.Release(context.WithoutCancel(ctx)); err != nil {
					a.log.Warn("释放 agent 同步锁失败，下一拍要等它过期", slog.Any("err", err))
				}
			}()
		}
	}

	now := a.now().UTC()
	claimed, err := a.svc.store.ClaimDueAgentSyncTasks(ctx, a.owner, now, syncClaimLease, syncClaimBatch)
	if err != nil {
		a.log.Warn("认领 agent 同步任务失败，下个周期重试", slog.Any("err", err))
		return
	}
	if len(claimed) == 0 {
		return
	}
	a.log.Info("认领到待同步的 agent", slog.Int("count", len(claimed)))

	for _, task := range claimed {
		if task.AttemptCount > 1 {
			// This task was cut off before the last round finished (the process was
			// recreated); this is a reclaim.
			//
			// **This log line is the only trace that "the last round was lost"**, same as the
			// scheduler one: reclaiming fixes the failure more fully and also hides it better,
			// and without this line nobody would know it ever broke.
			a.log.Warn("这条同步任务上一轮没跑完（进程被重建？），本次是补领",
				slog.String("task", task.ID), slog.String("agent", task.AgentID),
				slog.Int("attempt", task.AttemptCount))
		}
		if _, busy := a.inflight.LoadOrStore(task.ID, struct{}{}); busy {
			// The last round is still running on this instance (the narrow window where the
			// lease expired but the work is not done). Skip it. No need to put it back: after
			// the lease expires it will naturally be claimed again.
			a.log.Warn("这条同步任务上一轮还在跑，跳过本次", slog.String("task", task.ID))
			continue
		}
		a.wg.Add(1)
		go func(task persistmodel.AgentSyncTaskRow) {
			defer a.wg.Done()
			defer a.inflight.Delete(task.ID)
			a.runOne(ctx, task)
		}(task)
	}
}

// purge deletes expired finished tasks, and expired terminal entry tickets.
//
// # Why terminal tickets ride along here instead of having their own scan
//
// Everything they need is already here: a low-frequency ticker, a background loop that is
// already running, and cleanup on exit. A separate goroutine + ticker would just write the
// same things a second time.
//
// ⚠️ **The two jobs are unrelated, so one failing must not block the other.** Each part below
// checks its own error and logs on its own, without a shared return. With a shared one, "sync
// task cleanup failed" would mean terminal tickets are never cleaned, and nobody could see
// that link.
func (a *AgentSyncer) purge(ctx context.Context) {
	before := a.now().UTC().Add(-syncRetention)
	if n, err := a.svc.store.PurgeFinishedAgentSyncTasks(ctx, before); err != nil {
		a.log.Warn("清理已完成的 agent 同步任务失败", slog.Any("err", err))
	} else if n > 0 {
		a.log.Info("清理了已完成的 agent 同步任务", slog.Int64("count", n))
	}

	// Terminal entry tickets: short-lived rows created often and useless after 30 seconds;
	// without cleanup this table only grows.
	//
	// ⚠️ **This call site is required, not an optional optimization.** This repo has already
	// hit "a cleanup function was written but nobody called it" once (the pgmq table was
	// missed that way). That miss gives zero errors, and the cost is index entries growing
	// without end. The comment on PurgeExpiredTerminalTickets once said "called by a
	// scheduled task" while there was no call site at all; it was only found in self-review.
	if n, err := a.svc.PurgeExpiredTerminalTickets(ctx); err != nil {
		a.log.Warn("清理过期终端入场票失败", slog.Any("err", err))
	} else if n > 0 {
		a.log.Info("清理了过期终端入场票", slog.Int64("count", n))
	}
}

// runOne runs one sync task and settles it.
func (a *AgentSyncer) runOne(ctx context.Context, task persistmodel.AgentSyncTaskRow) {
	// The limit for one round. Deriving with WithoutCancel **does not work**: then this round
	// would not be interrupted on process shutdown, and Run's wg.Wait() would wait until the
	// timeout. Use the parent ctx here: on shutdown this round is canceled, the task stays in
	// running, and lease expiry lets another instance take over (that is exactly what the
	// lease is for).
	runCtx, cancel := context.WithTimeout(ctx, syncRunTimeout)
	defer cancel()

	log := a.log.With(slog.String("task", task.ID),
		slog.String("workspace", task.WorkspaceID), slog.String("agent", task.AgentID))

	stage, err := a.svc.syncAgentToArk(runCtx, task.WorkspaceID, task.AgentID)

	// Settle with a ctx **without cancellation**: the process may be shutting down right when
	// the run finishes, and a canceled ctx would fail to write the result, leaving the task in
	// running until the lease expires. That is an avoidable "frontend keeps spinning". Same
	// choice as ReleaseAll in worker.go.
	settleCtx, settleCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer settleCancel()
	now := a.now().UTC()

	if err == nil {
		// It succeeded. But **first check whether the user saved again while this round was
		// running**: if so, this sync used the old agent, and marking it succeeded would
		// silently lose that change while the user sees "sync succeeded", the worst kind of
		// false green.
		if a.requeueIfResyncWanted(settleCtx, task, log) {
			return
		}
		if err := a.svc.store.FinishAgentSyncTask(
			settleCtx, task.ID, apimodel.AgentSyncSucceeded, "", "", now); err != nil {
			log.Warn("同步成功但结算失败，任务会被租约回收后重跑（幂等，无害）", slog.Any("err", err))
		}
		return
	}

	// Failed. If the limit is not reached, put it back to try again.
	if task.AttemptCount < syncMaxAttempts {
		log.Warn("同步 agent 到方舟失败，稍后重试",
			slog.String("stage", string(stage)), slog.Int("attempt", task.AttemptCount),
			slog.Any("err", err))
		if rerr := a.svc.store.RequeueAgentSyncTask(settleCtx, task.ID, now); rerr != nil {
			log.Warn("重排同步任务失败，等租约过期后回收", slog.Any("err", rerr))
		}
		return
	}

	log.Error("同步 agent 到方舟连续失败已放弃，改动要等下次开对话才追平",
		slog.String("stage", string(stage)), slog.Int("attempts", task.AttemptCount),
		slog.Any("err", err))
	if ferr := a.svc.store.FinishAgentSyncTask(
		settleCtx, task.ID, apimodel.AgentSyncFailed, userMessageOf(err), stage, now); ferr != nil {
		log.Warn("结算失败的同步任务时又失败了", slog.Any("err", ferr))
	}
}

// requeueIfResyncWanted handles "the user saved again while this round was running".
//
// It returns true when the task was put back in the queue; the caller must not settle it.
//
// ⚠️ **This step cannot be skipped, or that save's change is silently lost.** The timeline:
//
//	t0  user saves A → enqueue → claim → sync starts (reads A)
//	t1  user saves B → enqueue hits the running one → sets resync_requested
//	t2  sync done (Ark has A)
//
// If t2 just ends in succeeded, the user sees "sync succeeded" while Ark actually has A, and B
// waits until the next chat. That is exactly the symptom this whole mechanism should remove,
// and this time it also misleads the user once more.
func (a *AgentSyncer) requeueIfResyncWanted(
	ctx context.Context, task persistmodel.AgentSyncTaskRow, log *slog.Logger,
) bool {
	// Read it again: in the snapshot taken at claim time, resync_requested is always false
	// (the flag is set after that). **Not rereading is the same as this field not existing.**
	cur, err := a.svc.store.LatestAgentSyncTask(ctx, task.AgentID)
	if err != nil || cur.ID != task.ID || !cur.ResyncRequested {
		return false
	}
	log.Info("同步期间 agent 又被保存过，再跑一轮")
	if err := a.svc.store.RequeueAgentSyncTask(ctx, task.ID, a.now().UTC()); err != nil {
		log.Warn("重排同步任务失败，等租约过期后回收", slog.Any("err", err))
		return false
	}
	return true
}

// syncAgentToArk really syncs an agent to Ark, and returns which step failed.
//
// # It calls the same ensureBinding as the chat path
//
// **We deliberately do not write a separate "slimmed-down sync"**: get the timing of any one
// of the three fingerprint write-backs wrong (chat.go:486 / :565 / :640) and the repair path is
// blocked forever. Once the fingerprint is false, every later comparison matches, and that
// agent never catches up again, with zero errors. Reusing the same function keeps that
// invariant in one place.
//
// # When the fingerprint matches this costs almost nothing
//
// ensureBinding finds the existing binding → compares two fingerprints → returns right away
// if both match, with **zero network calls**. So the requirement "if it was just synced, don't
// sync again" is met by the existing mechanism, and no extra check is needed here (adding one
// would split from the check over there).
func (s *Service) syncAgentToArk(
	ctx context.Context, workspaceID, agentID string,
) (apimodel.AgentSyncStage, error) {
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		// The agent has been deleted. **This is not a failure**: a task outliving its agent is
		// normal (the delete happened while the sync was queued), and reporting an error would
		// only leave red text in the UI that nobody can fix.
		if errors.Is(err, ErrNotFound) {
			return "", nil
		}
		return apimodel.AgentSyncStageAgent, err
	}
	if agent.WorkspaceID != workspaceID {
		// The agent was moved to another workspace, so this task's coordinates are no longer
		// valid. Also not a failure.
		return "", nil
	}

	key, err := s.arkKey(ctx, workspaceID, agentID)
	if err != nil {
		// No key configured, or the referenced global config is gone. **The user can fix this
		// one themselves**, so stage is marked credential and the frontend points the user to
		// workspace settings.
		return apimodel.AgentSyncStageCredential, err
	}

	binding, err := s.ensureBinding(ctx, key, agent)
	if err != nil {
		return arkSyncStageOf(err), err
	}

	// ⚠️ **ensureBinding returning nil does not mean the sync really succeeded.**
	//
	// When syncBindingSkills / syncBindingSpec fail, they only log a Warn and return a usable
	// binding (the chat path does this on purpose: chatting must work even if sync fails). If
	// a nil error were taken as success, this would report "sync succeeded" when it did not
	// catch up, and that result is exactly what the user came to see.
	//
	// So compare the fingerprint once more: the fingerprint on the binding is "which version
	// Ark has right now", and if it differs from the expected value, one of those two
	// catch-ups did not work.
	if stage, ok := arkSyncShortfall(s, ctx, key, agent, binding); !ok {
		return stage, fmt.Errorf("方舟侧未追平（%s），已记录，下次开对话会重试", stage)
	}
	return "", nil
}

// arkSyncShortfall checks whether the fingerprint on the binding really caught up to the
// expected value.
//
// It returns (the step that failed, whether it caught up).
//
// # Why the check is the fingerprint and not ensureBinding's return value
//
// See above: when those two catch-ups fail they degrade, and the return value does not show
// it. The fingerprint means exactly "this is what is on the Ark side right now"; if it equals
// the expected value, Ark has confirmed it. This check and the three "write back only on
// success" points in chat.go are two sides of the same coin.
func arkSyncShortfall(
	s *Service, ctx context.Context, key ArkBindingKey,
	agent apimodel.Agent, b persistmodel.ArkBinding,
) (apimodel.AgentSyncStage, bool) {
	if b.SpecFingerprint != ArkSpecFingerprint(arkSpecFor(agent)) {
		return apimodel.AgentSyncStageSpec, false
	}
	// The skills side needs the expected fingerprint computed again. A failure (for example
	// over the limit) also counts as not caught up: in that case the user must see the
	// reason, not a silent success.
	_, want, err := s.arkSkillsFor(ctx, key, agent)
	if err != nil || b.SkillsFingerprint != want {
		return apimodel.AgentSyncStageSkills, false
	}
	return "", true
}

// arkSyncStageOf works out from ensureBinding's error which step it got stuck on.
//
// Ark's structured errors are checked by code first; anything not recognized is matched by
// the Chinese sentence in the error (built by arkFailure), because ensureBinding has no
// per-step error types. **Anything not recognized goes to the agent stage**: that is the
// most general one and will not send the user in the wrong direction.
func arkSyncStageOf(err error) apimodel.AgentSyncStage {
	// An account without Managed Agents activated is the same kind of issue as "invalid key"
	// (account/credential config, fixed in the Ark console), so it goes to credential.
	// arkFailure's message for it has **no** "failed to prepare the Ark runtime environment"
	// prefix, so string matching below alone would put it in the most general agent stage.
	if ma.IsManagedAgentNotOpen(err) {
		return apimodel.AgentSyncStageCredential
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "方舟运行环境"):
		return apimodel.AgentSyncStageEnvironment
	case strings.Contains(msg, "API Key"), strings.Contains(msg, "方舟 API Key"):
		return apimodel.AgentSyncStageCredential
	case strings.Contains(msg, "技能"):
		return apimodel.AgentSyncStageSkills
	default:
		return apimodel.AgentSyncStageAgent
	}
}

// userMessageOf takes the part of the error meant for the user.
//
// The core error message convention is "sentinel: Chinese sentence", and the frontend shows
// the part after the colon as is (same as userMessage in handler.go). This does the same,
// because this sentence goes into the failure_reason column and into the HTTP response as is.
func userMessageOf(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 && i+2 < len(msg) {
		return msg[i+2:]
	}
	return msg
}

// LatestAgentSyncTask gets the result of an agent's latest sync, for frontend polling.
//
// It returns ErrNotFound when there are no records at all, and **that is different from "sync
// failed"**: it only means this agent was never saved (or the records expired and were
// cleaned up), and should not be shown as an error.
func (s *Service) LatestAgentSyncTask(
	ctx context.Context, agentID string,
) (apimodel.AgentSyncTask, error) {
	row, err := s.store.LatestAgentSyncTask(ctx, agentID)
	if err != nil {
		return apimodel.AgentSyncTask{}, err
	}
	return apimodel.AgentSyncTask{
		ID:            row.ID,
		WorkspaceID:   row.WorkspaceID,
		AgentID:       row.AgentID,
		Status:        apimodel.AgentSyncStatus(row.Status),
		FailureReason: row.FailureReason,
		FailureStage:  apimodel.AgentSyncStage(row.FailureStage),
		Attempts:      row.AttemptCount,
		// Those two columns are timestamptz in the database (for sorting and timeout checks),
		// and must be turned into fixed-length ISO strings for the API. See "Timestamps: unit
		// and format" in CLAUDE.md.
		CreatedAt: apimodel.FormatISO(row.CreatedAt),
		UpdatedAt: apimodel.FormatISO(row.UpdatedAt),
	}, nil
}

// RetryAgentSync queues a sync for an agent again, for the frontend "retry" button.
//
// The only difference from EnqueueAgentSync is that it returns errors: the user clicked this
// **on purpose**, so if it cannot be queued they should know (while on the save path failing
// to queue only degrades and should not interrupt the save).
func (s *Service) RetryAgentSync(ctx context.Context, agentID string) (apimodel.AgentSyncTask, error) {
	a, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		return apimodel.AgentSyncTask{}, fmt.Errorf("%w: Agent 不存在", ErrNotFound)
	}
	row, created, err := s.enqueueSyncOnce(ctx, a)
	if err != nil {
		return apimodel.AgentSyncTask{}, fmt.Errorf("排同步任务失败: %w", err)
	}
	// Same as EnqueueAgentSync: on the narrow window, try once more (the one in the way is
	// already in a terminal state, so this always succeeds).
	if row.ID == "" {
		if row, created, err = s.enqueueSyncOnce(ctx, a); err != nil {
			return apimodel.AgentSyncTask{}, fmt.Errorf("排同步任务失败: %w", err)
		}
	}
	if created {
		s.kickAgentSync()
	}
	if row.ID == "" {
		// Still nothing after the retry. This should not happen, but we must not pretend it
		// succeeded (that would make the user think it is syncing).
		return apimodel.AgentSyncTask{}, fmt.Errorf("%w: 上一次同步刚刚结束，请再点一次", ErrConflict)
	}
	return s.LatestAgentSyncTask(ctx, agentID)
}
