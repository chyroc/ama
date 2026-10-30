// The **last-resort** rescue for stuck work: when Ark's server has already marked the work
// stopped and poll reclaim has no hope left, the patrol fills in the missing tool_result itself.
//
// # Which gap it covers
//
// A requires_action session normally heals itself through poll reclaim (45s after a worker dies,
// a new worker grabs the work back and really runs it, see arkReclaimOlderThan). But Ark's
// server has a hard line: once the last heartbeat is arkWorkServerStopAfter old (90s, measured,
// undocumented), the work is set to the terminal state stopped, and **PollWork never returns it
// again**. If the threshold is misconfigured, no poll happens to land inside the reclaim window,
// or Ark moves this line in the future, the session stays stuck in requires_action forever, until
// reapStaleRuns terminates it 30 minutes later. The user sees "the reply stopped halfway".
//
// Ark's documentation on self-hosted environment security says explicitly: **"after a Work Item
// enters the stopped state, a late Tool Result is still accepted by the server and drives the
// Session"**. So stopped work does not mean it cannot be saved: post the missing result, and that
// Ark turn continues. When the agent sees "the tool was not executed" it calls again, and that is
// a **new** work item, queued normally and really run by a polling worker.
//
// # Rule: both conditions must hold before we post
//
//  1. this run has been silent in our database for rescueAfter (= 90s stop line + 10s margin);
//  2. **GET work confirms state=stopped on the Ark side**.
//
// Condition 2 is what keeps it safe: a normal tool run can take 120 seconds (see the four
// timeout layers in worker.go). During that time the PG side is also silent, but the work is
// still active on Ark with heartbeats going. Posting on silence alone would collide with the real
// result (two results for the same call). Check first, then post; active/starting/queued/stopping
// are never touched.
//
// # Distributed safety
//
// It is called from watchOne, where the session has already been claimed via PG ClaimRunWatch,
// so only one instance in the whole system does this. Posting is also idempotent: each time,
// auditToolCalls first runs ListEvents and computes the call id set difference, so after one post
// the next round sees no orphans.
package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime"
	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// rescueAfter is "how long a run must be silent before the patrol is allowed to rescue it".
//
// It must be > arkWorkServerStopAfter (90s): before 90s the work may not yet be stopped by the
// server, and the normal reclaim path still has a chance to grab it back and really run it. The
// patrol should not step in before that; posting a "not executed" error result would waste a
// tool that could have really run. Plus a 10s margin for server clock skew and event persist
// delay.
const rescueAfter = arkWorkServerStopAfter + 10*time.Second

// rescueCheckEvery is how often to re-check while a subscription is alive.
// 30s, same pace as the patrol scan: after 100s of silence we act within 30s at the latest.
const rescueCheckEvery = 30 * time.Second

// rescueTick periodically checks "does this run need rescue" while a long-lived subscription is
// alive.
//
// No separate patrol pass: watchOne already holds the PG claim for this run (unique across the
// system), so hanging it inside there naturally avoids concurrent duplicate posts. When runCtx
// ends (subscription disconnected / safety timeout), the goroutine stops with it.
func (w *RunWatcher) rescueTick(ctx context.Context, runID string) {
	ticker := time.NewTicker(rescueCheckEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Re-read the database every time: lastActivityAt is refreshed by real progress, and once it is refreshed we should not rescue.
			run, err := w.cfg.Service.store.GetRun(ctx, runID)
			if err != nil {
				return // run is gone; the subscription side handles retire/cleanup
			}
			if run.Status != apimodel.RunRequiresAction {
				// running has not reached the tool yet, idle/terminated already ended; neither needs rescue.
				continue
			}
			age := ageSinceActivity(run)
			if age < rescueAfter {
				continue
			}
			w.rescueStoppedWork(ctx, run, age)
		}
	}
}

// ageSinceActivity computes how long since the run's last real activity; returns 0 if the time cannot be parsed (better not to rescue).
func ageSinceActivity(run apimodel.Run) time.Duration {
	at, err := time.Parse("2006-01-02T15:04:05.000Z", persistmodel.LastActivityOf(run))
	if err != nil {
		return 0
	}
	return time.Since(at)
}

// shouldRescue is the pure "should we post" check, pulled out so it is easy to test.
//
// All three conditions are required:
//   - our side really is stuck in requires_action (running/idle/terminated are not rescued);
//   - silence has passed Ark's stop line (age >= rescueAfter), so normal reclaim has no hope;
//   - the work can be found on Ark and its state is exactly stopped. Any live state such as
//     active must not be touched; not found (404) is not touched either (that key may not belong
//     to this resource).
func shouldRescue(status apimodel.RunStatus, age time.Duration, workState string, found bool) bool {
	return status == apimodel.RunRequiresAction &&
		age >= rescueAfter &&
		found && workState == arkWorkStateStopped
}

// rescueStoppedWork checks the Ark work state and, once it is confirmed stopped, posts the missing tool_result.
func (w *RunWatcher) rescueStoppedWork(ctx context.Context, run apimodel.Run, age time.Duration) {
	log := w.log.With(slog.String("run", run.ID), slog.String("workspace", run.WorkspaceID))

	binding, err := w.cfg.Service.store.GetArkBinding(ctx, run.WorkspaceID, run.AgentID)
	if err != nil {
		// ErrNotFound (no binding) / database read failure cannot be rescued, and should not flood
		// the logs: this loop ticks every 30s. Wait quietly for reapStaleRuns to clean up at 30 minutes.
		return
	}
	key, err := w.cfg.Service.arkKey(ctx, run.WorkspaceID, run.AgentID)
	if err != nil {
		log.Debug("救援时取不到方舟凭证，跳过", slog.Any("err", err))
		return
	}
	baseURL, err := arkBaseURL(key.Region)
	if err != nil {
		log.Warn("救援时解析方舟地址失败，跳过", slog.Any("err", err))
		return
	}

	state, found, err := fetchArkWorkState(
		ctx, baseURL, key.APIKey, binding.ArkEnvironmentID, run.ID)
	if err != nil {
		log.Warn("查询 work 状态失败，下一轮再试", slog.Any("err", err))
		return
	}
	if !shouldRescue(run.Status, age, state, found) {
		// Still active/queued/… means a worker is handling it or about to, so **never post**;
		// 404 means Ark no longer has this work, so a post has nowhere to go. Both are left to the normal path.
		return
	}

	log.Warn("work 已被方舟停止却仍卡 requires_action，巡检补发工具结果（最后一道救援）",
		slog.Duration("静默", age.Round(time.Second)),
		slog.String("environment", binding.ArkEnvironmentID))

	// Distributed counter: the 1st rescue is a normal fallback; from the 2nd on, it means the
	// failover→reclaim self-healing chain has failed for this run (the re-dispatched executor did
	// not really run the tool). The count must be read **before posting**: even if the post itself
	// fails, the fact "the re-dispatch did not catch it" still holds, so the count is not wasted.
	rescueN, countErr := w.cfg.Service.store.IncrRunRescueCount(ctx, run.ID)
	if countErr != nil {
		// A count failure does not block the rescue (the session is still stuck), but log it: without
		// the severity rule, debugging falls back to "a pile of Warns you cannot rank".
		log.Warn("自增救援计数失败，本轮仍照常补发", slog.Any("err", countErr))
	}

	client := arkruntime.NewClientWithApiKey(key.APIKey, arkruntime.WithBaseUrl(baseURL))
	rescueCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), toolAuditTimeout)
	defer cancel()
	posted := auditToolCalls(rescueCtx, selfhosted.NewClientAPI(client), run.ID, log)
	if posted > 0 {
		if rescueNeedsHuman(rescueN, countErr != nil) {
			// ⚠️ **Error, not Warn.** This round's "not executed" is pushed forward by the posted
			// error result, and the agent will most likely issue a new call, which goes through the
			// same broken chain again. Seen in production: one run failed 7 times in a row over 43
			// minutes with only Warns, and nobody noticed the self-healing chain was broken. At this
			// count a human should step in: check whether the executor pod / environment is stuck in
			// the preparation stage, and rebuild the instance if needed.
			log.Error("同一条会话已连续多次靠巡检兜底，failover 重派自愈链失效，需要人介入",
				slog.Int("第几次救援", rescueN),
				slog.Int("本轮补发", posted),
				slog.String("environment", binding.ArkEnvironmentID),
				slog.String("排查方向",
					"查该环境执行者 pod 在工具准备段（账本/凭据/建容器）是否卡死、是否需要重建实例"))
		} else {
			log.Warn("巡检补发完成，方舟那一轮可以继续；agent 若重试工具会作为新 work 被真执行",
				slog.Int("补发", posted),
				slog.Int("第几次救援", rescueN))
		}
	}
}

// repeatedRescueAlertAt is "from which rescue of the same run we raise an Error for a human".
//
// 1: normal fallback (one failover did not catch it; a re-dispatch or a slow tool can cause that).
// 2: already a strong signal that "failover→reclaim ran a full round and the new executor still
// did not run the tool". Posting quietly again only makes the agent fail round after round. 2 is
// the smallest number that separates "occasional" from "systemic".
const repeatedRescueAlertAt = 2

// rescueNeedsHuman decides whether the n-th rescue (n>=1) should be raised to Error for a human.
// With countFailed=true (counter store unavailable) it stays at Warn to be safe: a broken counter
// step should not turn every normal rescue into a false incident report.
func rescueNeedsHuman(n int, countFailed bool) bool {
	return !countFailed && n >= repeatedRescueAlertAt
}
