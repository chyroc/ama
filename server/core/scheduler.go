// The scheduled-task scheduler: resident scan -> atomic claim -> drive one agent round -> deliver
// the result.
//
// # Where it sits in the whole chain
//
//	ticker (this file)
//	  ↓ ClaimDueSchedules: atomic claim, exactly one wins across instances (see store_pg_schedule.go)
//	  ↓
//	Service.CreateConversation  creates the Ark Session and a Run with trigger=schedule
//	  ↓ StreamRunEvents (subscribe first!) + AppendRunEvents (then send the prompt)
//	  ↓
//	Deliverer.Deliver  sends the final reply back to where it came from in Feishu; skipped for run_only
//	  ↓
//	write back next_run_at / counters / auto-pause on failure; oneshot is deleted after running
//
// # Two gates, both required
//
// amad runs as multiple instances in production (see CLAUDE.md at the repo root), so "who runs
// this task" must have an answer:
//
//	gate one (Redis lease, **optional**)   the whole scheduler grabs a lock and scans only if it
//	                                      gets it. Saves N-1 instances from useless scans; it is an
//	                                      **optimization**.
//	gate two (PG atomic claim, **required**) Store.ClaimDueSchedules. It is **correctness**.
//
// Gate one alone is not enough: the Redis section can be left empty entirely (the service still
// starts, and the Feishu long connection is just not enabled). If correctness depended on it, "no
// Redis configured" would mean "N times duplicate delivery", and that failure reports no error; it
// shows up as users getting several copies of the same report. So correctness rests entirely on
// that PG SQL, and Redis only saves resources.
//
// # Why fire is written again instead of reusing feishu/inbound.go
//
// The shapes are indeed similar (both "create session -> subscribe first -> send message -> wait
// for the stream to end -> deliver"), but there are three real differences: a scheduled task has no
// original message to reply to (it needs SendCard, not ReplyCard), whether to deliver depends on
// Delivery, and after running it must write back the schedule state. A shared function would need
// parameters for all three, making it harder to read than writing each one. **But the three details
// that cannot be skipped must be copied**; see fireOne.
package core

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
)

const (
	// scanInterval is the interval for scanning "are any tasks due".
	//
	// 30 seconds is a compromise: cron's smallest unit is 1 minute, so scanning more often is pointless
	// (two scans within the same minute, the second claims nothing); scanning less often means "remind
	// me in 1 minute" could wait up to 1.5 minutes in the worst case, and users can feel the delay.
	scanInterval = 30 * time.Second

	// claimLease is the length of the claim lease.
	//
	// **It must be clearly longer than one agent round** (fireTimeout is 10 minutes), otherwise the
	// lease expires while the task is still running, another instance grabs it and runs it again: the
	// same task runs two rounds in parallel and the user gets two results.
	// It must not be too long either: when an instance is rebuilt (release / scale-down / failed health
	// check) nobody releases the lease, and only expiry makes the task runnable again, so the task can be
	// stuck for as long as the lease in the worst case.
	claimLease = 15 * time.Minute

	// fireTimeout is the upper limit for one round, the same value as runTimeout on the feishu side.
	//
	// The agent may call many tools and think for a long time, but it cannot hold on forever: without
	// a per-fire timeout, one stuck turn would halt that task permanently (the running mark is never
	// released).
	fireTimeout = 10 * time.Minute

	// claimBatch is the most tasks claimed in one round.
	//
	// The cap keeps a single scan from overwhelming Ark: each one creates a Session and starts a
	// container. What is left over is picked up next cycle (their next_run_at is still in the past, so
	// they are still due).
	claimBatch = 20

	// leaderKey is the Redis lock name for gate one.
	leaderKey = "ama:scheduler:leader"

	// leaderTTL / leaderRenew are the leader-election lease and the renewal interval.
	//
	// The renewal interval is a third of the TTL: one failed renewal still leaves two more chances, so a
	// single network hiccup does not give up leadership. Same choice as on the feishu long-connection
	// side.
	leaderTTL   = 45 * time.Second
	leaderRenew = 15 * time.Second
)

// Deliverer delivers the result of one round.
//
// It is an interface instead of a direct dependency on the feishu package: core and feishu are
// deliberately coupled only by ID strings (see the comment in core/agent.go), and a reverse import
// would break that decoupling. cmd/amad passes the implementation in at wiring time, symmetric with
// feishu.Conversations.
type Deliverer interface {
	// Deliver sends a piece of text to target.
	//
	// title is used for the card title (the task name). runID lets the implementation build an
	// idempotency key: on Feishu-side retries the value stays the same, so it can tell it is the same
	// message, not a new one.
	Deliver(ctx context.Context, target apimodel.DeliveryTarget, title, text, runID string) error
}

// Leases is the leader-election capability gate one needs, provided by shared/infra/redis.
//
// Same shape as lark.LeaseStore (also an interface + a cmd/amad adapter there), for the same reason:
// core should not depend back on shared/infra/redis; that is the job of the infrastructure wiring
// layer.
type Leases interface {
	// Acquire grabs the lock. If it cannot, it returns (nil, false, nil): **that is not an error**, it
	// is a normal leader-election result.
	Acquire(ctx context.Context, key string, ttl time.Duration) (Lease, bool, error)
}

// Lease is a lock currently held.
type Lease interface {
	// Renew extends it. false means the lock is no longer ours, and the caller must **stop its work
	// immediately**.
	Renew(ctx context.Context, ttl time.Duration) (bool, error)
	// Release releases it. Pass a ctx that will not be cancelled (on the shutdown path the original ctx
	// is often already cancelled).
	Release(ctx context.Context) error
}

// SchedulerConfig wires up the scheduler.
type SchedulerConfig struct {
	// Service is required; used to create sessions, send messages, and subscribe to events.
	Service *Service
	// Deliverer may be nil: then results only go into run history, and tasks configured for Feishu
	// delivery log a warning.
	Deliverer Deliverer
	// Leases may be nil: then there is no leader election and every instance scans (PG claims
	// guarantee no duplicates).
	Leases Leases
	// Logger defaults to slog.Default() when empty.
	Logger *slog.Logger
	// Now defaults to time.Now when empty; tests can inject it.
	//
	// Copied from the feishu package's convention (its InboundConfig.Now). Everywhere else core calls
	// time.Now() directly, but scheduling is **a function of time**, and without injection it could only
	// be tested with sleep.
	Now func() time.Time
	// Owner identifies this instance and goes into the claim record, only for troubleshooting.
	// Defaults to the hostname.
	Owner string
	// ScanInterval defaults to scanInterval when empty. Tests make it smaller.
	ScanInterval time.Duration
}

// Scheduler is the resident scheduled-task scheduler.
type Scheduler struct {
	cfg   SchedulerConfig
	log   *slog.Logger
	now   func() time.Time
	owner string
	scan  time.Duration

	// inflight prevents the same task from running two rounds concurrently **within this instance**.
	//
	// This is not the correctness defense (that is the PG claim + lease); it only saves wasted work:
	// the claim already guarantees cross-instance mutual exclusion, and within this instance next_run_at
	// is already cleared between two scans, so it could not be claimed a second time anyway.
	// It is kept for the narrow window of "the lease expired but the previous round is actually still
	// running".
	inflight sync.Map // scheduleID → struct{}
	wg       sync.WaitGroup
}

// NewScheduler creates a scheduler.
//
// It returns nil when Service is nil; callers use that to decide whether to start, and explain the
// consequence in the log (same convention as NewWorkerPool).
func NewScheduler(cfg SchedulerConfig) *Scheduler {
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
		// The hostname is enough to tell instances apart, and on container orchestration platforms it is
		// the pod name. If it cannot be read, use a fixed string: owner only goes into logs, and claim
		// correctness does not depend on it being unique.
		if h, err := os.Hostname(); err == nil {
			owner = h
		} else {
			owner = "unknown"
		}
	}
	scan := cfg.ScanInterval
	if scan <= 0 {
		scan = scanInterval
	}
	return &Scheduler{cfg: cfg, log: log, now: now, owner: owner, scan: scan}
}

// Run starts and blocks until ctx is cancelled.
//
// Same shape as WorkerPool.Run / feishu.Supervisor.Run (a single blocking `Run(ctx) error`, with
// cleanup done before returning), so on the cmd/amad side one g.Go line is enough to hook it up.
func (s *Scheduler) Run(ctx context.Context) error {
	s.log.Info("定时任务调度器启动",
		slog.String("owner", s.owner),
		slog.Duration("scan_interval", s.scan),
		slog.Bool("leader_election", s.cfg.Leases != nil),
		slog.Bool("delivery", s.cfg.Deliverer != nil))

	ticker := time.NewTicker(s.scan)
	defer ticker.Stop()

	// Scan once at startup: after a restart, tasks that "came due while it was down" should catch up
	// as soon as possible instead of waiting a whole scan cycle.
	s.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			// Wait for running rounds to finish. **Do not return right away**: those goroutines are
			// writing to the database (writing back next_run_at, counters); exiting the process directly
			// would cut them off midway, showing up as a task's run_count missing one, or next_run_at
			// not being scheduled back.
			s.log.Info("定时任务调度器停止，等在跑的任务收尾")
			s.wg.Wait()
			return nil
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// tick scans one round: (optionally) elect a leader -> claim -> start a goroutine per task.
func (s *Scheduler) tick(ctx context.Context) {
	ScanSchedules(ctx, ScheduleTickHooks{
		Leases:    s.cfg.Leases,
		LeaderKey: leaderKey,
		LeaderTTL: leaderTTL,
		Claim: func(ctx context.Context, owner string, now time.Time) ([]apimodel.Schedule, error) {
			return s.cfg.Service.store.ClaimDueSchedules(ctx, owner, now, claimLease, claimBatch)
		},
		LostRound: s.lostRoundBefore,
		InFlight: func(id string) bool {
			_, busy := s.inflight.LoadOrStore(id, struct{}{})
			return busy
		},
		Fire: func(sch apimodel.Schedule) {
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer s.inflight.Delete(sch.ID)
				s.fireOne(ctx, sch)
			}()
		},
		Reschedule: func(sch apimodel.Schedule) { s.rescheduleOnly(ctx, sch) },
		Owner:      s.owner,
		Now:        s.now,
		Logger:     s.log,
	})
}

// fireOne runs one round of a task.
//
// # Three details that cannot be skipped, all learned the hard way in feishu/inbound.go
//
// One, **subscribe first, then send the message**. core events are only stored when someone is
// subscribed (see the chat.go file header and "what is still missing" in core/README.md); the other
// way around, the first part of the reply is neither seen nor stored. Scheduled tasks hit this gap
// especially hard, since by nature there is no browser watching them.
//
// Two, **ctx must be WithoutCancel, then wrapped with its own timeout**. This round must keep
// running after tick returns; using tick's ctx would cut the stream off when the next tick arrives
// (or when the process gets a signal).
//
// Three, **there must be a per-fire timeout**. Without it, one stuck turn halts that task
// permanently.
func (s *Scheduler) fireOne(ctx context.Context, sch apimodel.Schedule) {
	FireSchedule(ctx, sch, s.log.With(
		slog.String("schedule", sch.ID), slog.String("agent", sch.AgentID), slog.String("schedule_name", sch.Name)), FireScheduleHooks{
		Run:     s.runOnce,
		Deliver: s.deliver,
		Finish:  s.finish,
		Now:     s.now,
		Timeout: fireTimeout,
	})
}

// runOnce creates a session, sends the prompt, and collects the agent's final text reply.
//
// Returns (runID, reply text, error). **Return runID whenever possible, even on error**: that Run
// has already been created, and only by writing it back to schedule.LastRunID can the user click in
// to see what failed.
func (s *Scheduler) runOnce(
	ctx context.Context, sch apimodel.Schedule, log *slog.Logger,
) (string, string, error) {
	return RunScheduleOnce(ctx, sch, RunScheduleOnceHooks{
		Create: func(ctx context.Context, sch apimodel.Schedule) (apimodel.Run, error) {
			return s.cfg.Service.CreateConversation(ctx, sch.WorkspaceID, sch.AgentID)
		},
		Tag: func(ctx context.Context, sch apimodel.Schedule, run apimodel.Run) error {
			stored, err := s.cfg.Service.store.GetRun(ctx, run.ID)
			if err != nil {
				return nil
			}
			stored.Trigger = apimodel.TriggerSchedule
			stored.TriggerDetail = sch.Name
			stored.ScheduleID = sch.ID
			if sch.Name != "" {
				stored.Title = sch.Name
			}
			_, err = s.cfg.Service.store.SaveRun(ctx, stored)
			return err
		},
		Stream: s.cfg.Service.StreamRunEvents,
		Append: func(ctx context.Context, runID string, events []json.RawMessage) error {
			_, err := s.cfg.Service.AppendRunEvents(ctx, runID, events)
			return err
		},
		Logger: log,
	})
}

// deliver sends the result out according to the delivery address.
//
// # Inspection is not on this path: the agent creates issues itself
//
// Inspection-type tasks use run_only and return nil in the first branch below. **That is exactly the
// meaning they need**: "found no problem this round" must count as success, otherwise a healthy
// platform would be auto-paused after 5 rounds in a row by the MaxConsecutiveFailures anti-avalanche
// rule, showing up as "the healthier the platform, the sooner inspection stops, and nobody knows it
// stopped".
//
// There used to be a delivery=issue: the server parsed a JSON array from the agent's output and
// created issues on its behalf with a fingerprint computed from a key. It was removed because that
// contract was too fragile: a JSON parse failure wasted the whole inspection round, and the cause (the
// model missed a bracket) had nothing to do with the inspection itself. Now the agent creates them
// itself with `ama issue create` / `ama issue comment`, deduplicated by a fixed marker prefix in the
// title (the rule is written in the agent's system prompt), so the failure mode "cannot parse, so the
// whole round fails" no longer exists.
func (s *Scheduler) deliver(
	ctx context.Context, sch apimodel.Schedule, text, runID string, log *slog.Logger,
) error {
	// run_only (including empty) does not send anywhere: the result is the Run in run history.
	if sch.Delivery.Kind == "" || sch.Delivery.Kind == apimodel.DeliveryRunOnly {
		return nil
	}

	if s.cfg.Deliverer == nil {
		// Delivery is configured but the process has no Deliverer. **Say it loudly**: the user set
		// "send to group" in the UI, yet nothing will happen, and the run in run history succeeded: the
		// hardest kind of inconsistency to track down.
		log.Warn("这条任务配了外部投递，但服务端没装投递能力，结果只落在运行历史里",
			slog.String("delivery", string(sch.Delivery.Kind)))
		return nil
	}
	title := sch.Name
	if title == "" {
		title = "定时任务"
	}
	return s.cfg.Deliverer.Deliver(ctx, sch.Delivery, title, text, runID)
}

// finish writes back the result of one round: counters, error, next fire time; oneshot is deleted
// after running.
//
// **Use a ctx that will not be cancelled**: this step happens when fire wraps up, and by then runCtx
// may already be cancelled by the timeout, but "this round ran" must be stored. Otherwise next_run_at
// stays empty and the task is never claimed again (showing up as "it ran once and never again").
func (s *Scheduler) finish(
	ctx context.Context, sch apimodel.Schedule, runID string, finished time.Time, fireErr error,
) {
	FinishSchedule(ctx, sch, runID, finished, fireErr, s.log, FinishScheduleHooks{
		Get:      s.cfg.Service.store.GetSchedule,
		Delete:   s.cfg.Service.store.DeleteSchedule,
		Save:     s.cfg.Service.store.SaveSchedule,
		Release:  s.cfg.Service.store.ReleaseScheduleClaim,
		Next:     ComputeNextRun,
		NotFound: ErrNotFound,
	})
}

// lostRoundBefore decides "is this claim catching up on a task whose last round did not finish",
// and returns how long since it last finished.
//
// # Why this is the only way to tell
//
// The NextRunAt returned by the claim is always empty (claiming clears it), and claimed_until is not
// in doc (it is runtime state and should not appear in HTTP responses), so the only evidence the
// caller has is LastRunAt.
//
// The check is "time since it last finished exceeds one period + a full lease". The lease margin is
// there to **only report real ones**: a normal round is late by at most one scan interval, while a
// zombie is always late by at least one lease (it is not reclaimed until the lease expires). Better
// to miss some than to report false ones: a Warn that fires every time is the same as no Warn.
//
// Three cases return false directly:
//   - LastRunAt is empty: it never ran. **A newly created task is naturally like this**; it is not
//     a zombie.
//   - Cannot parse: old data with the wrong shape; do not use it as evidence.
//   - oneshot: there is no "period" to compare, and it is deleted after running, so the stuck
//     window is only one round anyway.
func (s *Scheduler) lostRoundBefore(sch apimodel.Schedule, now time.Time) (bool, time.Duration) {
	return LostScheduleRoundBefore(sch, now, claimLease)
}

// rescheduleOnly only schedules the next fire time back, without touching the counters.
//
// Used in the skip branch for "this instance's previous round is still running": the claim already
// cleared next_run_at, and without scheduling it back this task would never fire again.
//
// ⚠️ **Each of the three early-return branches below must log.** Not scheduling it back means this
// task's `next_run_at` stays empty -> ClaimDueSchedules never picks it -> **this scheduled task
// never runs again**, while in the UI status is still active, consecutiveFailures is still 0, and
// lastError is still empty (the same as the past incident where "it stopped for many hours while the
// four counters acted as if nothing happened").
//
// Worse, **the log would lie**: entering this function is preceded by the line
// Warn log "previous round still running, skipping this fire", and
// someone reading the log only sees "skipped, scheduled back". So a silent return here is not
// "one log line fewer"; it is "the log asserts something that did not happen".
//
// finish, which does the same job, already logs this (at the re-read failure); two functions
// doing the same thing where one logs and one does not shows this was an omission, not a design.
func (s *Scheduler) rescheduleOnly(ctx context.Context, sch apimodel.Schedule) {
	RescheduleOnly(ctx, s.cfg.Service.store, sch.ID, s.now().UTC(), ErrNotFound, s.log)
}

// scheduleUserMessage builds the user.message sent to the agent when the time comes.
//
// The prompt is passed in by the caller instead of read from sch.Prompt: the call site is where "what
// exactly this round sends" is decided; this only builds the shape.
//
// **ContextSummary is put before the prompt**: every fire is a brand-new session with zero history,
// so the agent does not know in what context this task was scheduled. Self-wakeups need it most: when
// the agent schedules "check CI in 10 minutes" it has a whole round of context in mind (which PR,
// what to do on failure), and the new session that wakes up knows none of it.
func scheduleUserMessage(sch apimodel.Schedule, prompt string) (json.RawMessage, error) {
	return ScheduleUserMessage(sch, prompt)
}
