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
	"errors"
	"fmt"
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
	// Gate one: if Redis is configured, grab the lock first; if we cannot, skip this tick.
	//
	// The lock is held **only for this scan** and released when the scan ends, not held long term:
	// holding it long term ties "who is leader" to one instance, and if it dies others must wait for the
	// TTL to take over; the claim itself is already atomic, and leader election only saves duplicate
	// scans, so one tick is a fine enough granularity.
	if s.cfg.Leases != nil {
		lease, ok, err := s.cfg.Leases.Acquire(ctx, leaderKey, leaderTTL)
		if err != nil {
			// Failing to grab the lock (Redis hiccup) **must not degrade into not scanning**: then
			// every scheduled task would stop whenever Redis is down, while correctness never depended
			// on it. So log a warning and scan as usual; the PG claim covers duplicates.
			s.log.Warn("抢调度锁失败，本轮照常扫（重复由 PG 认领兜住）", slog.Any("err", err))
		} else if !ok {
			// Another instance is scanning. This is a normal result, not an error.
			return
		} else {
			defer func() {
				if err := lease.Release(context.WithoutCancel(ctx)); err != nil {
					s.log.Warn("释放调度锁失败，下一拍要等它过期", slog.Any("err", err))
				}
			}()
		}
	}

	now := s.now().UTC()
	claimed, err := s.cfg.Service.store.ClaimDueSchedules(ctx, s.owner, now, claimLease, claimBatch)
	if err != nil {
		s.log.Warn("认领定时任务失败，下个周期重试", slog.Any("err", err))
		return
	}
	if len(claimed) == 0 {
		return
	}
	s.log.Info("认领到到期的定时任务", slog.Int("count", len(claimed)))

	for _, sch := range claimed {
		// **Leave a Warn when a zombie is reclaimed.** The NextRunAt returned by the claim is always
		// empty (claiming clears it), so here we cannot tell "normally due" from "zombie", but LastRunAt
		// plus the task period can: a task running normally is never claimed much more than one period
		// after it last finished.
		//
		// This log line is the **only trace** that "the previous round was lost": that round wrote
		// no RunCount, no FailCount, no LastError; all four counters act as if nothing happened (that
		// is exactly what a past production incident looked like: the task stopped for many hours while
		// status stayed active and consecutiveFailures stayed 0).
		// Without it, reclaiming makes the failure **fixed more thoroughly and also more hidden**: the
		// task heals itself, and nobody ever learns "why it broke".
		if lost, since := s.lostRoundBefore(sch, now); lost {
			s.log.Warn("这条任务上一轮没跑完就断了（进程被重建？），本次是补领",
				slog.String("schedule", sch.ID),
				slog.String("last_run_at", sch.LastRunAt),
				slog.Duration("since_last_run", since))
		}
		if _, busy := s.inflight.LoadOrStore(sch.ID, struct{}{}); busy {
			// The previous round is still running (the narrow window where the lease expired but the
			// work is not done). Skip it, but schedule it back, otherwise this task's next_run_at stays
			// empty and it is never claimed again.
			s.log.Warn("上一轮还在跑，跳过本次触发", slog.String("schedule", sch.ID))
			s.rescheduleOnly(ctx, sch)
			continue
		}
		s.wg.Add(1)
		go func(sch apimodel.Schedule) {
			defer s.wg.Done()
			defer s.inflight.Delete(sch.ID)
			s.fireOne(ctx, sch)
		}(sch)
	}
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
	log := s.log.With(
		slog.String("schedule", sch.ID),
		slog.String("agent", sch.AgentID),
		slog.String("schedule_name", sch.Name))

	// Log at start: creating the session / sending the message may hang a long time before the
	// success or failure log (creating a session builds Ark resources and starts a container); without
	// this line you could only guess from "claimed N tasks" whether this round actually started.
	log.Info("定时任务开始执行")

	// This round's lifetime is decoupled from tick; see details two and three.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fireTimeout)
	defer cancel()

	started := s.now().UTC()
	runID, text, err := s.runOnce(runCtx, sch, log)
	finished := s.now().UTC()

	if err != nil {
		log.Error("定时任务执行失败", slog.Any("err", err))
		s.finish(runCtx, sch, runID, finished, err)
		return
	}

	// Delivery. **A delivery failure also counts as this round failing**: what the user wants is "see
	// the report in the group at 9:30"; if the agent succeeds but the report is not sent, to them it is
	// the same as not running.
	if derr := s.deliver(runCtx, sch, text, runID, log); derr != nil {
		log.Error("投递结果失败", slog.Any("err", derr))
		s.finish(runCtx, sch, runID, finished, derr)
		return
	}

	log.Info("定时任务执行完成",
		slog.String("run", runID),
		slog.Duration("elapsed", finished.Sub(started)))
	s.finish(runCtx, sch, runID, finished, nil)
}

// runOnce creates a session, sends the prompt, and collects the agent's final text reply.
//
// Returns (runID, reply text, error). **Return runID whenever possible, even on error**: that Run
// has already been created, and only by writing it back to schedule.LastRunID can the user click in
// to see what failed.
func (s *Scheduler) runOnce(
	ctx context.Context, sch apimodel.Schedule, log *slog.Logger,
) (string, string, error) {
	run, err := s.cfg.Service.CreateConversation(ctx, sch.WorkspaceID, sch.AgentID)
	if err != nil {
		return "", "", fmt.Errorf("建会话失败: %w", err)
	}

	// Change trigger to schedule.
	//
	// CreateConversation hardcodes manual (that is the meaning for the chat page path), so change it
	// once here. **Without the change this run would show up in the chat page's left-hand conversation
	// list** (ChatPage filters by `trigger === "manual"`), and users would see a pile of conversations
	// they never opened.
	trigger := apimodel.TriggerSchedule
	if stored, gerr := s.cfg.Service.store.GetRun(ctx, run.ID); gerr == nil {
		stored.Trigger = trigger
		// TriggerDetail holds the task name: that column in the run history list shows it directly,
		// so users can see at a glance which scheduled task produced this run.
		stored.TriggerDetail = sch.Name
		// ScheduleID is **the half meant for machines**, alongside the name above rather than instead
		// of it: names can change and repeat, and after a rename looking up history by name breaks
		// (and breaking reports no error; the list just has fewer rows).
		// The detail page's "past runs" list queries by this ID; see apimodel.Run.ScheduleID.
		stored.ScheduleID = sch.ID
		if sch.Name != "" {
			stored.Title = sch.Name
		}
		if _, serr := s.cfg.Service.store.SaveRun(ctx, stored); serr != nil {
			// Failing to tag it is not fatal: this round runs as usual, only the source label in history
			// is wrong.
			log.Warn("回写触发来源失败", slog.Any("err", serr))
		}
	}

	// Subscribe first (detail one). Event storage hangs off this path.
	var (
		mu      sync.Mutex
		builder strings.Builder
	)
	// **We must decide "this round is over" ourselves and close the stream.**
	//
	// Ark **does not close the SSE connection** after end_turn (it keeps a long connection per session,
	// waiting for the next user message), so StreamRunEvents never returns on its own. The measured
	// symptom: the agent finished replying in 5 seconds, but this goroutine hung until fireTimeout (10
	// minutes) before waking. The lease was held the whole time, so an "every 5 minutes" task actually
	// became "every 10 minutes", and the log only ever showed "claimed task", never "finished". **This
	// trap only shows up against real Ark**: fakeRunner's Stream returns after sending all frames, so in
	// tests streamDone closes right away.
	//
	// The check uses session.status_idle, with the same meaning as applyEventToRun in core/chat.go.
	// An idle with requires_action **does not count as over** (the agent is waiting for confirmation and
	// will continue), so stop_reason must be checked.
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()

	streamDone := make(chan struct{})
	var streamErr error
	go func() {
		defer close(streamDone)
		streamErr = s.cfg.Service.StreamRunEvents(streamCtx, run.ID, func(raw []byte) error {
			if text, ok := agentMessageText(raw); ok && text != "" {
				mu.Lock()
				builder.WriteString(text)
				mu.Unlock()
			}
			if turnEnded(raw) {
				// Cancel ctx so StreamRunEvents returns. **Do not return an error here**:
				// that would be logged as "subscriber error" with a warning, while this is actually a
				// normal shutdown.
				stopStream()
			}
			return nil
		})
	}()

	// Give the subscription a moment to set up. No complex handshake: StreamRunEvents fills in past
	// events first, so even sending early loses nothing (same handling as feishu/inbound.go).
	time.Sleep(50 * time.Millisecond)

	event, err := scheduleUserMessage(sch, sch.Prompt)
	if err != nil {
		return run.ID, "", err
	}
	if _, err := s.cfg.Service.AppendRunEvents(ctx, run.ID, []json.RawMessage{event}); err != nil {
		return run.ID, "", fmt.Errorf("发送指令失败: %w", err)
	}

	<-streamDone
	if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
		log.Warn("事件流异常结束", slog.Any("err", streamErr))
	}

	mu.Lock()
	text := strings.TrimSpace(builder.String())
	mu.Unlock()

	if text == "" {
		// Not a single word produced: the agent may be stuck on a tool, or timed out. This counts as a
		// failure: an empty report is more confusing than no report.
		return run.ID, "", fmt.Errorf("这一轮没有产生任何回复（可能超时或执行环境有问题）")
	}
	return run.ID, text, nil
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
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	// **Re-read** before changing: between the claim and now, the user may have changed this task in
	// the UI (edited the prompt, clicked pause). Writing back the in-memory copy from when fire started
	// would overwrite those changes.
	cur, err := s.cfg.Service.store.GetSchedule(writeCtx, sch.ID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			s.log.Warn("回写调度状态前重读失败", slog.String("schedule", sch.ID), slog.Any("err", err))
		}
		// The task was deleted: this round's result has nowhere to go; end normally.
		return
	}

	// oneshot is deleted after running (success or failure).
	//
	// **Success or failure** is deliberate: if a failed one were kept, it would run and fail again
	// next cycle, and "remind me in 1 hour" would become a reminder every 30 seconds.
	//
	// Once deleted there is no lease to release: the row is gone, and so are the two lease columns.
	if cur.Spec.Kind == apimodel.ScheduleOneshot {
		if err := s.cfg.Service.store.DeleteSchedule(writeCtx, cur.ID); err != nil {
			s.log.Warn("删除一次性任务失败，它可能会被重复触发",
				slog.String("schedule", cur.ID), slog.Any("err", err))
		}
		return
	}

	cur.LastRunAt = apimodel.FormatISO(finished)
	cur.UpdatedAt = apimodel.FormatISO(finished)
	if runID != "" {
		cur.LastRunID = runID
	}
	cur.RunCount++
	if fireErr != nil {
		cur.FailCount++
		cur.ConsecutiveFailures++
		cur.LastError = fireErr.Error()
	} else {
		cur.ConsecutiveFailures = 0
		cur.LastError = ""
	}

	switch {
	case cur.Status != apimodel.ScheduleActive:
		// The user paused it while this round ran; do not schedule the next one.
		cur.NextRunAt = ""
	case cur.ConsecutiveFailures >= apimodel.MaxConsecutiveFailures:
		// Auto-pause after repeated failures (anti-avalanche): a task with a broken prompt or a deleted
		// agent fails every cycle, each time really creating an Ark session, burning tokens, and maybe
		// posting a failure card to the group.
		cur.Status = apimodel.SchedulePaused
		cur.NextRunAt = ""
		s.log.Warn("连续失败次数达上限，自动暂停这条定时任务",
			slog.String("schedule", cur.ID),
			slog.Int("failures", cur.ConsecutiveFailures))
	default:
		// Count the next one from **the time it finished**, which is exactly what "interval does not use a
		// fixed grid" comes down to.
		next, nerr := ComputeNextRun(cur.Spec, finished)
		if nerr != nil {
			// The expression can no longer compute the next time (broken by a manual edit?). Pause it so
			// it does not fail forever.
			cur.Status = apimodel.SchedulePaused
			cur.NextRunAt = ""
			cur.LastError = "算不出下次触发时刻：" + nerr.Error()
		} else {
			cur.NextRunAt = next
		}
	}

	if _, err := s.cfg.Service.store.SaveSchedule(writeCtx, cur); err != nil {
		// The cost of this write failing is "next_run_at stays empty -> never claimed again", so report it
		// explicitly.
		s.log.Error("回写调度状态失败，这条任务可能不会再被触发",
			slog.String("schedule", cur.ID), slog.Any("err", err))
	}

	// **Release the lease after SaveSchedule.** The other way around, for a moment next_run_at is still
	// the old (due) value while the lease is already gone, and another instance would grab it right away
	// and run it again: the same task runs two rounds back to back and the user gets two results.
	//
	// Not releasing has an equally concrete cost: the lease takes 15 minutes to expire, and tasks with
	// a shorter interval ("every 5 minutes") cannot be claimed until then, showing up as **the interval
	// silently stretched to 15 minutes**, with nothing visible in the log.
	if err := s.cfg.Service.store.ReleaseScheduleClaim(writeCtx, cur.ID); err != nil {
		s.log.Warn("释放认领租约失败，这条任务要等租约过期才能再跑",
			slog.String("schedule", cur.ID), slog.Any("err", err))
	}
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
	if sch.LastRunAt == "" || sch.Spec.Kind == apimodel.ScheduleOneshot {
		return false, 0
	}
	last, err := apimodel.ParseISO(sch.LastRunAt)
	if err != nil {
		return false, 0
	}
	since := now.Sub(last)

	// One period: interval uses EveryMs directly; cron works back from "the next one after it last
	// finished".
	var period time.Duration
	switch sch.Spec.Kind {
	case apimodel.ScheduleInterval:
		period = time.Duration(sch.Spec.EveryMs) * time.Millisecond
	default:
		nextISO, nerr := ComputeNextRun(sch.Spec, last)
		if nerr != nil {
			return false, 0
		}
		next, perr := apimodel.ParseISO(nextISO)
		if perr != nil {
			return false, 0
		}
		period = next.Sub(last)
	}
	if period <= 0 {
		return false, 0
	}
	return since > period+claimLease, since
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
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	cur, err := s.cfg.Service.store.GetSchedule(writeCtx, sch.ID)
	if err != nil {
		// A deleted task is normal (no need to schedule back); failing to read is a fault and must be
		// reported. Copy the check from finish; do not let "the DB hiccupped" and "the task is gone"
		// share one silent return.
		if !errors.Is(err, ErrNotFound) {
			s.log.Warn("排回下次触发前重读失败，这条任务的 next_run_at 会一直空着、不会再被触发",
				slog.String("schedule", sch.ID), slog.Any("err", err))
		}
		return
	}
	if cur.Status != apimodel.ScheduleActive {
		// The user paused/archived it, so it should not be scheduled: normal path, silent.
		return
	}
	next, err := ComputeNextRun(cur.Spec, s.now().UTC())
	if err != nil {
		// The expression cannot compute the next time: this is a **fault**, with consequences as bad
		// as the one above. Sharing one return with "the user paused it" would make it fully invisible.
		s.log.Warn("算不出下次触发时刻，这条任务的 next_run_at 会一直空着、不会再被触发",
			slog.String("schedule", cur.ID), slog.Any("err", err))
		return
	}
	cur.NextRunAt = next
	cur.UpdatedAt = apimodel.NowISO()
	// ⚠️ **Deliberately do not touch UpdatedBy**: the scheduler is a machine path, `auth.ActorFrom` is
	// always an empty string here, and writing it would only blank out "who changed it last" without
	// any error. Same as finish and applyOriginUpdate.
	if _, err := s.cfg.Service.store.SaveSchedule(writeCtx, cur); err != nil {
		s.log.Warn("排回下次触发失败", slog.String("schedule", cur.ID), slog.Any("err", err))
	}
	// Same as finish: schedule the time back first, then release the lease; the reverse order would let
	// another instance grab and run it right away.
	if err := s.cfg.Service.store.ReleaseScheduleClaim(writeCtx, cur.ID); err != nil {
		s.log.Warn("释放认领租约失败", slog.String("schedule", cur.ID), slog.Any("err", err))
	}
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
	if summary := strings.TrimSpace(sch.ContextSummary); summary != "" {
		prompt = "# 这条任务创建时的上下文\n\n" + summary +
			"\n\n---\n\n# 本次要做的事\n\n" + prompt
	}
	raw, err := json.Marshal(map[string]any{
		"type": "user.message",
		"content": []map[string]any{
			{"type": "text", "text": prompt},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("拼指令失败: %w", err)
	}
	return raw, nil
}

// turnEnded decides whether this frame means "this round is finished".
//
// Only session.status_idle counts, and only when stop_reason is not requires_action. The latter is
// indeed idle on the Ark side, but it means "stuck waiting for user confirmation"; the agent will
// continue, and treating it as over would collect only half the result.
// Same check as applyEventToRun in core/chat.go.
//
// session.status_terminated also counts as over (the session was terminated; no more events will
// come).
func turnEnded(raw []byte) bool {
	switch frameType(raw) {
	case "session.status_idle":
		return stopReasonType(raw) != "requires_action"
	case "session.status_terminated":
		return true
	}
	return false
}

// agentMessageText extracts the text the agent said from a frame.
//
// Only agent.message counts: thinking, tool calls, and status changes are not "results", and posting
// them to the group is noise. Delta frames (event_delta) and [DONE] are quietly skipped here: the
// former's type is not agent.message, and the latter is not JSON.
func agentMessageText(raw []byte) (string, bool) {
	var ev struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return "", false
	}
	if ev.Type != "agent.message" {
		return "", false
	}
	var b strings.Builder
	for _, c := range ev.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String(), true
}
