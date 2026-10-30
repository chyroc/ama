// Two gates for zombie workers: stop sending once a result can never be delivered, and stop
// running once the run reaches a final state.
//
// # The real failure
//
// In a ten-minute window in production, two workers logged 2995 lines of
// `tool result send hit permanent 4xx` + `worker api status 400: Request payload
// is invalid`, and at the same time **the same workers** logged 1132 429s. Grouped by
// worker_id and environment, the two sets match exactly: the 429s were not a lack of quota.
// These useless retries filled the account's RPM themselves, and then ListEvents hit the rate
// limit they had created (self-inflicted).
//
// Four sessions each hit 562~1252 400s, while their runs **had long finished**:
//
//	sesn-20260101000008-ex008  1214 x 400  run: idle, endedAt 19:57:55.506Z
//	sesn-20260101000009-ex009  1252 x 400  run: idle, endedAt 23:07:34.879Z
//
// `-2krdd` kept hammering for **about 40 more minutes after** its run finished.
//
// # Why the SDK's own "give up on permanent 4xx" did not catch it
//
// It does exist, but that giving up **applies only to one send, not to the pending result**:
//
//	retrySendEvent  hits isFatal4xxStatus, returns (false, err) at once   ← gives up this one send
//	flushResults    `continue` when posted is false                       ← result stays in the map
//
// Deletion only happens in `markAnswered`, which is only called when `posted == true`. So this
// undeliverable result **stays in `pendingResults` forever** and is flushed again next round.
//
// And "next round" comes very often, with two more things stacked on top:
//
//  1. **Our API wrapper layer pushed the SDK down to list polling mode.** The `selfhosted.API`
//     interface has no `StreamEvents`, and `shutdownSafeAPI` / `rateLimitedAPI` embed the
//     interface itself, so the wrapped object **no longer satisfies
//     `selfhosted.EventStreamer`** (confirmed by testing: the bare `ClientAPI` satisfies it,
//     one layer of wrapping and it does not). So `SessionToolRunner` skips
//     `consumeStreamLoop` and goes straight to `consumeList`. The first statement of that loop
//     body is `flushResults`, and the tick is `EventPollInterval`, which we do not set, so it
//     defaults to **500ms**.
//
//  2. **This work never reaches its idle timeout.** `hasIdleBlockers()` is true whenever
//     `len(pendingResults) > 0`, so `armIdle` only sets `idleArmPending` and leaves
//     `idleArmedAt` at its zero value, while `idleExpired()` requires `!idleArmedAt.IsZero()`.
//     **The pending result that made it a zombie also switched off the only timer that could
//     make it exit on its own.** `MaxIdle` (60 seconds) never fires on this path.
//
// Put the three together and you get those 40 minutes: one round every 500ms, one send per
// round, never deleted when it fails, and never timing out. The only way out is the process
// exiting (in production it was two rolling deploys that replaced them, so "fewer 429s
// lately" was an illusion caused by the deploys, not a fix).
//
// # The two gates added here
//
//	Gate one, send gate (workSendGuard):
//	    after a permanent 4xx, stop calling SendEvent for this session, and **stop this
//	    work** (just not sending is not enough, see below).
//	Gate two, final-state gate (watchRunTerminated):
//	    stop this work once the run is terminated in the database. This is the
//	    cross-instance backstop; see its own comment.
//	    ⚠️ It also covers a third shape: the run **finished a turn normally** (idle +
//	    endedAt written) but the work did not exit by itself; see workIdleRunGrace. That was
//	    the real shape of a later 429 incident: the run state was completely normal, so
//	    looking only at the run showed nothing.
//
// **Both are needed, but they are not backups of each other**: the first handles "cannot
// deliver but keeps sending" (the direct source of those 2995 400s), the second handles "the
// error is recoverable (e.g. constant 429) so the first gate never fires, but nobody wants
// that run any more".
//
// # Why the work must be stopped, not just the sending
//
// Because of item 2 above: while that result stays in `pendingResults`, the idle timer can
// never be armed. If only SendEvent were short-circuited, the request volume would drop, but
// that goroutine (with the container and work lease it holds) would keep spinning until the
// process exits. That trades it for a **quiet** leak, and quiet leaks are harder to find.
//
// # Why StopWork should be sent after stopping
//
// Stopping uses **each work's own ctx**, not the worker's ctx. So the
// `shutdownSafeAPI.StopWork` guard (which checks the worker ctx) still only blocks StopWork
// when **the process exits**, and is not affected here. And here it **should** be sent: this
// work really cannot continue, and settling it releases the lease right away, instead of
// making other instances wait a full reclaim cycle (45 seconds) for nothing.
package core

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

// workRunCheckInterval is the interval for "watching whether that run in the database has
// reached a final state".
//
// 30 seconds, the same pace as the other checks in this repo. The criterion is "how long a
// zombie may waste at most": at the measured ~2 req/s, 30 seconds is 60 useless requests,
// which is acceptable. The cost is one GetRun every 30 seconds per in-flight work; with a
// concurrency cap of 20 that totals ≤40 per minute, negligible for the database.
//
// **It is a var, not a const**, only so tests can make it smaller. Production code must not
// change it.
var workRunCheckInterval = 30 * time.Second

// workIdleRunGrace is "how long after a run has finished normally the work still hanging
// around is treated as a zombie".
//
// # It does not fix "wrong run state"; it fixes "the work outlives its run"
//
// In the 429 incident in production: 9 runs were all `status=idle` with `endedAt` set; the
// oldest had been dead for 23 hours, yet their work was still in the SDK's consumeList
// calling ListEvents every 3 seconds, filling the account's RPM. **Those runs' state was
// correct**: a finished conversation turn is idle, and that is the normal basis for
// multi-turn chat / self-wakeup / Feishu follow-ups (RunStatus in apimodel.go only has
// idle/running/terminated/rescheduling; there is no "completed" value).
// What was wrong was the work: it should exit after that turn ends, but lives on because the
// arming condition on the SDK's fallback polling path is never met (see "Why the SDK never
// exits by itself" below).
//
// So the check lives on the worker side and **does not change the run state**: changing the
// state would touch 182/200 historical runs in production (measured), and would also mark
// them as red "failed" in the UI (RunStatusChip checks non-empty `errorSummary` +
// terminated) and raise the failure rate on the overview page. Rewriting all of history to
// fix a problem that only affects in-flight work costs far too much.
//
// # Why the SDK never exits by itself
//
// On this path the SDK's `MaxIdle` is **not "not yet due"; it was never started**:
// `idleArmedAt` must be armed by `armIdle`, whose only call site is in
// `processListedEvents`, which requires a **successful** ListEvents that **reads new
// events**. These runs ended long ago, so there will be no new events. Even if the rate limit
// clears and ListEvents succeeds, all historical events are judged already seen by
// `markEventSeen` and skipped with `continue`, `touchedIdle` stays false, and **it is still
// not armed**. So `idleExpired()` is always false, the timer channel is always nil, and all
// three exits fail at once.
//
// ⚠️ **The conclusion is that these zombies do not heal themselves**, and waiting for the
// rate limit to ease does not help. Before this fix, the only way to clear them was
// restarting the pod. That is also why the check must come from our side.
//
// # Why endedAt is required; checking only idle is wrong
//
// `idle` is not a final state; it means "this turn is done, waiting for the user's next
// message". Checking only status would tear down the container right after every turn, and
// the next message would wait ~16 more seconds for it to resume. That trades a real
// experience regression for this bug fix (same as the "Why only terminated" part in
// watchRunTerminated below).
//
// Only a non-empty `endedAt` shows this turn **was settled** (only closeRun writes it, when
// it receives the final-state frame). A run that is settled and has been quiet past MaxIdle
// should already have been exited by the SDK. A real follow-up goes through
// AppendRunEvents, which pushes the state back to running and refreshes lastActivityAt, so
// the next tick no longer matches. Even if it is stopped right before a follow-up, the next
// message is picked up by a new work that Ark dispatches, exactly the same path as after a
// normal MaxIdle exit.
//
// # Value: 10 minutes (it used to be 2 hours, which was wrong)
//
// **Stopping the work does not tear down the container.** After cancel here, the work takes
// **the same** cleanup path as the SDK's own `ErrIdleTimeout` exit: "one work finished" →
// container idle → paused or stopped (idle in dockertools/session.go; the workspace volume is
// untouched). So stopping early is the same as "the SDK's MaxIdle took effect a few minutes
// late": the workspace is kept as is, and the user resumes normally on return.
//
// The lower bound only needs to cover **the normal follow-up window**:
//
//	60 seconds   SDK MaxIdle: the follow-up window deliberately kept after a turn; container not paused
//	30 seconds   workRunCheckInterval: this gate's own check interval
//	margin       for clock drift and platform-side queuing, rounded up to 10 minutes
//
// The upper bound is set by **the container TTL (1 hour)**, and must be **far below** it:
// after stopping, the container is to be paused, but past the TTL the platform has already
// reclaimed it, so pausing is sure to fail (400 `session is not resumable`), the container is
// billed as running for that whole last hour, and the workspace cannot be kept either.
//
// The old 2-hour value fell right into this trap. Measured in production over a 6-hour
// window: one workspace's scheduled task picked up 45 works, **19** did not exit after
// finishing a turn, and all were stopped by this gate **2h01m** after pickup; in the same
// window, 18 "pause container failed: session is not resumable" errors matched those 19 one
// by one. In other words, every time this gate fired it was just late enough that the
// container was already gone. By then stopping only served to "stop the useless requests";
// saving resources and keeping the workspace were both too late. Meanwhile each work also
// held an environment concurrency slot (steadily 5~6/20 in the logs), which at higher volume
// would crowd out normal conversations.
//
// ⚠️ **Do not reuse runStaleAfter (30 minutes) here.** That threshold detects "stuck
// sessions" (requires_action that never gets a tool result), and stopping there cuts a
// conversation turn in half. This one detects "work that already finished but is hanging
// around", and stopping only does early the cleanup the SDK should have done. They mean
// different things; do not merge them.
//
// ⚠️ This gate is a backstop, not a fix: the root cause of why the work never reaches
// MaxIdle after end_turn is still in the SDK's idle timer arming condition (see "Why the SDK
// never exits by itself" above). This only decides "how long it wastes once broken".
const workIdleRunGrace = 10 * time.Minute

// workSendGuard wraps SendEvent: after a permanent 4xx it stops sending to this session and
// stops this work.
//
// # Why the check reuses arkFatalStatus
//
// It is the same classification as the SDK's `isFatal4xxStatus` (4xx minus 408/409/429), and
// exactly the check `retrySendEvent` uses to log `tool result send hit permanent 4xx`.
// Reusing it guarantees "the SDK considers this a permanent failure" and "we give up on this
// session" are the same thing, and never split into two meanings.
//
// ⚠️ **429 must never count**. It is the one to back off and retry (backoff: see
// arkRateLimitGate). Treating it as permanent would make a single rate limit drop a tool
// result that should have been delivered. That trades "a bit slower" for "this turn is stuck
// in requires_action forever", which is worse than the original bug.
// Same for 408/409 (timeouts and races are temporary). TestWorkSendGuardKeepsRetryingRateLimit
// pins this down.
//
// # Why it is tracked per session, not per callID
//
// The permanent 4xx here comes from the session events endpoint and means "this session no
// longer accepts events" (two shapes seen in testing: 400 `Request payload is invalid` and
// 404 SessionExpired). That is a property of the whole session, not of one result. Also, once
// stopped this work is over, so splitting by callID makes no difference in behavior and only
// adds a layer of state.
//
// ⚠️ The remaining risk, stated plainly: if one day "the session is alive but one result's
// payload really is malformed", this gate ends the whole turn (the user sees "it stopped
// halfway") instead of dropping just that one. That would be our own serialization bug, and
// the SDK side would also give up sending (just without ending the work), so neither
// approach saves that turn. The only difference is that **this gate makes it show up faster
// and more visibly**, instead of quietly spinning until the process exits. This trade-off is
// deliberate.
type workSendGuard struct {
	selfhosted.API

	// sessionID is only for logs; duplicate detection does not rely on it (see "Why it is tracked
	// per session" above).
	sessionID string
	// cancel stops this work. **It is not the worker's cancel**; see the last section of the file
	// header.
	cancel context.CancelFunc
	log    *slog.Logger

	mu sync.Mutex
	// failed non-nil = this session can never be delivered to again; remember that error and
	// return it as is.
	failed error
	// skippedSend / skippedList count how many calls each short-circuited, only for logs.
	//
	// **Deliberately two counters**: besides making "the gate is really blocking" visible, it
	// must tell whether it blocked **writes** (SDK and fallback SendEvent) or **reads** (the
	// fallback reconciliation's ListEvents). With one number you could not tell whether those 15
	// 429s came from reads or writes, and the two are fixed differently.
	skippedSend int
	skippedList int
}

// newWorkSendGuard wraps a send gate. When api is nil it is returned as is, for the caller to
// handle.
//
// cancel may be nil (in tests that only want to check the short-circuit); then it only
// short-circuits and does not stop.
func newWorkSendGuard(
	api selfhosted.API, sessionID string, cancel context.CancelFunc, logger *slog.Logger,
) selfhosted.API {
	if api == nil {
		return api
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &workSendGuard{API: api, sessionID: sessionID, cancel: cancel, log: logger}
}

// SendEvent returns the remembered error directly, without touching the network, once this
// session has failed permanently.
//
// **It returns an error instead of pretending to succeed**, even though "pretending to
// succeed" would let the SDK remove it from pendingResults and then exit on idle by itself
// (the easier path). The reason: that would record a fake "delivered" in ResultStore, so the
// question "did the tool result really get back to Ark" would get a wrong answer in both the
// logs and the stored state, and that is exactly the only useful information when debugging
// a requires_action hang. Exiting is handled by cancel instead, which is honest and just as
// bounded.
func (g *workSendGuard) SendEvent(ctx context.Context, req selfhosted.SendEventRequest) error {
	if err := g.permanentErr(); err != nil {
		g.mu.Lock()
		g.skippedSend++
		n := g.skippedSend
		g.mu.Unlock()
		// Log only the first few times, then stay quiet: when really stuck this is called many times
		// (the SDK flushes every 500ms), and logging each one would drown the useful logs.
		if n <= 3 {
			g.log.Warn("这条 session 的事件已永久投递失败，跳过本次投递",
				slog.String("ark_session", g.sessionID),
				slog.Int("累计跳过", n),
				slog.Any("首次错误", err))
		}
		return err
	}

	err := g.API.SendEvent(ctx, req)
	if err == nil || !arkFatalStatus(err) {
		// Success, or a recoverable error (429 backoff, 408 timeout, 409 race, 5xx, network blips):
		// pass through as is. Retry behavior belongs to the SDK and the rate limit gate; this layer
		// stays out of it.
		return err
	}

	g.mu.Lock()
	first := g.failed == nil
	if first {
		g.failed = err
	}
	g.mu.Unlock()

	if first {
		// This one must **state the consequence plainly**: it is the only clue that "this
		// conversation turn ends here".
		g.log.Warn("工具结果永久投递失败（session 不再接收事件），停止重试并结束这条 work",
			slog.String("ark_session", g.sessionID),
			slog.Any("err", err),
			slog.String("说明", "常见于 run 已收尾/session 已过期；"+
				"继续重试只会烧方舟 RPM 配额，见 core/workersendguard.go 文件头"))
		if g.cancel != nil {
			g.cancel()
		}
	}
	return err
}

// ListEvents also returns the remembered error directly, without touching the network, once
// this session has failed permanently.
//
// # Why reads are blocked too, not only writes
//
// Two paths read this endpoint, and both are wasted calls:
//
//  1. **Closing reconciliation (auditToolCalls)**: it first calls ListEvents to find calls
//     that "were made but have no result", then sends a SendEvent for each. This session no
//     longer accepts events at all, so not one of the orphans found can be delivered: **that
//     round of reads is pure waste**.
//  2. **The SDK's own consumeList polling**: the runner also gets this gate (in `pollLoop`,
//     `newWorker(sessionID, sendGuard)`), and that loop runs every 500ms. There is a window
//     between stopping this work and it exiting; blocking reads saves the requests in that
//     window too.
//
// Measured in production (session sesn-20260101000010-ex010): when the gate only wrapped
// SendEvent, this fallback ListEvents alone produced 15 429s. It was the only call in this
// case that really hit the quota, because the 400s and 500s on the write side are rejected
// by Ark outright and not counted toward the rate limit, while **the read side is counted**.
//
// # Return an error, not an empty result
//
// An empty result (`&ListEventsResponse{}`) would make `listOrphanToolCalls` conclude "no
// orphans at all", so reconciliation is skipped silently, `posted=0`, and not a single log
// line is written. That is exactly the shape where "the fallback did not work" and "there were
// no orphans to begin with" share one log-free path, which the "reconciliation failed" Warn in
// auditToolCalls explicitly says to avoid. Returning an error reaches that Warn and says
// "this round was not reconciled because this session has been judged dead".
//
// ⚠️ **Do not stop the work** (only the SendEvent side stops it). This is only a read; its
// failure is no new evidence that "this session is over". The gate being set already means
// the write side stopped it long ago.
func (g *workSendGuard) ListEvents(
	ctx context.Context, req selfhosted.ListEventsRequest,
) (*selfhosted.ListEventsResponse, error) {
	if err := g.permanentErr(); err != nil {
		g.mu.Lock()
		g.skippedList++
		n := g.skippedList
		g.mu.Unlock()
		// Log only the first few times, for the same reason as SendEvent: the SDK's consumeList runs
		// every 500ms.
		if n <= 3 {
			g.log.Warn("这条 session 的事件已永久投递失败，跳过本次拉取（对账结果也送不进去）",
				slog.String("ark_session", g.sessionID),
				slog.Int("累计跳过", n),
				slog.Any("首次错误", err))
		}
		return nil, err
	}
	return g.API.ListEvents(ctx, req)
}

// permanentErr returns "whether this session has already failed permanently".
func (g *workSendGuard) permanentErr() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.failed
}

// watchRunTerminated watches that run in the database and stops this work as soon as the
// run reaches a final state.
//
// # Which gap it covers, and why the send gate alone is not enough
//
// The send gate fires when "the error is permanent". But the same incident had another
// shape: the error stays **recoverable** (e.g. the environment is being rate limited and
// SendEvent keeps getting 429), so the send gate never fires, while that run was long ago
// moved to terminated by `reapStaleRuns`. Nobody wants this result any more. Continuing to
// back off and retry is pure waste then, and the requests it adds are the very source of
// the rate limit.
//
// # Why it must "query the database" instead of using an in-process map
//
// **This is a distributed service** (see CLAUDE.md). The instance that closes the run and the
// instance running this work are **usually not the same**: `reapStaleRuns` is run by the
// **one** instance that wins the Redis lease, while the work is handled by whichever instance
// polled it, and the two have nothing to do with each other. So "cancel the in-process
// goroutine when setting the final state" finds nobody in production: a local single process
// is always right, production fails now and then, and the error does not point at the real
// cause.
//
// The database is the only fact visible across instances, so the check lives on the worker's
// own side: **is the run for my session still alive**. Then it does not matter who closed it
// or on which instance.
//
// # Why only terminated counts, not idle
//
// `idle` is a **normal** state: after an end_turn the run is idle, and the SDK deliberately
// waits `MaxIdle` (60 seconds) more, so when the user sends the next message the same work
// can continue directly without pausing and resuming the container (resume takes ~16
// seconds). Treating idle as a stop signal would tear down the container after every turn,
// trading a real experience regression for this bug fix, and this bug is the send gate's job.
//
// ⚠️ **But "still hanging around long after finishing a turn" is a different matter**; see
// idleRunAbandoned: that check requires `endedAt` to be written **and** workIdleRunGrace (10
// minutes) to have passed, an order of magnitude away from "just did end_turn, waiting for
// the next message" (60 seconds), so it does not hurt follow-ups.
//
// All three sources of `terminated` clearly mean "this session ends here":
// `session.status_terminated` (Ark says it ended), `retireMissingRun` (404 on the Ark side),
// `reapStaleRuns` (reclaimed after 30 minutes of silence). None of them needs the worker to
// keep running.
//
// # Not finding the run is a normal result, not an error
//
// The Ark sessionID is the `Run.ID` (CreateConversation in chat.go uses the id returned by
// CreateSession directly as Run.ID), but **not every session has a run**: probes and paths
// that do not store a Run will find nothing. Then do nothing: **never treat "not found" as a
// final state**, otherwise work from probes and old paths would be killed at once, with the
// symptom "tool calls fail for no clear reason".
func (p *WorkerPool) watchRunTerminated(
	ctx context.Context, arkSessionID string, cancel context.CancelFunc, logger *slog.Logger,
) {
	if p == nil || p.svc == nil || p.svc.store == nil || arkSessionID == "" || cancel == nil {
		return
	}
	ticker := time.NewTicker(workRunCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		run, err := p.svc.store.GetRun(ctx, arkSessionID)
		if err != nil {
			// Not found (probes, paths that do not store a Run) or a database blip: make no judgment.
			// Look again next tick; treating either as a final state would kill normal work.
			continue
		}
		abandoned := idleRunAbandoned(run, time.Now().UTC())
		if run.Status != apimodel.RunTerminated && !abandoned {
			continue
		}

		// Warn instead of Info: reaching here means a work is still running after its run was judged
		// dead. That is **evidence of a failure** (either a tool is stuck or a result keeps failing to
		// deliver), not everyday noise.
		//
		// The two match reasons must be reported separately: terminated is "the run was judged dead",
		// abandoned is "the run finished normally but the work did not exit by itself". The latter
		// points at the SDK's fallback polling path never exiting (see workIdleRunGrace). The
		// debugging direction is completely different, and one combined message would send people
		// the wrong way.
		reason := "run 已是终态"
		if abandoned {
			reason = "run 早已答完一轮并结算，这条 work 却还没退出（疑似卡在 SDK 的轮询路径）"
		}
		logger.Warn("结束仍在跑的 work："+reason,
			slog.String("ark_session", arkSessionID),
			slog.String("run_status", string(run.Status)),
			slog.String("ended_at", run.EndedAt),
			slog.String("说明", "继续跑只会对一条已收尾的会话重试投递、白烧方舟 RPM 配额，"+
				"见 core/workersendguard.go 文件头"))
		cancel()
		return
	}
}

// idleRunAbandoned decides "this run finished normally long ago, but the work is still
// hanging around".
//
// All three conditions are required; each one blocks one kind of wrong kill (see
// workIdleRunGrace for the values):
//
//	status == idle             runs still going (running / requires_action) are handled
//	                           elsewhere; stepping in here would cut off a session waiting
//	                           for a tool result
//	endedAt non-empty          only closeRun writes it, on the final-state frame. Empty = this
//	                           turn is not settled yet, so it cannot have "finished long ago"
//	endedAt before threshold   a just-finished run must be kept for follow-ups (the SDK's
//	                           MaxIdle window); killing it makes the next message wait ~16
//	                           more seconds for the container to resume
//
// ⚠️ **When endedAt cannot be parsed it returns false (no kill)**, the opposite choice from
// retireEndedAt: there, falling back to now on a parse failure is about "better to record a
// longer duration than lose endedAt", while here falling back to now would immediately judge
// an oddly shaped old record a zombie. The default answer of this check must be "leave it
// alone": a wrong kill cuts off the user's conversation, a missed kill only burns quota a
// bit longer, and those costs are not symmetric.
func idleRunAbandoned(run apimodel.Run, now time.Time) bool {
	if run.Status != apimodel.RunIdle || run.EndedAt == "" {
		return false
	}
	ended, err := time.Parse("2006-01-02T15:04:05.000Z", run.EndedAt)
	if err != nil {
		return false
	}
	return now.Sub(ended) >= workIdleRunGrace
}
