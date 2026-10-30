// Automatic failover for stuck work: when a tool call has gone out and no result comes back
// for a long time, we decide that the executor of this work (the SDK's HandleItem) is stuck.
// We make it **stop heartbeating, and forbid it from reporting termination to Ark**, so the
// work is handed back to Ark's reclaim mechanism and re-dispatched after 45 seconds to a
// healthy worker that really runs it (threshold: see arkReclaimOlderThan; the upper limit is
// Ark's 90-second server-side stop line).
//
// # Why it is needed
//
// Under self_hosted, after the agent calls a tool the session stays in `requires_action`. A
// worker picks up the work, runs the tool in the container, and sends back
// `user.tool_result`. The SDK's tool event loop (SessionToolRunner) **has no overall timeout
// while waiting for results**: its "idle auto-exit" is only armed on end_turn, and never fires
// while the session is in requires_action. So if the runner gets stuck **before** it starts
// consuming events (GetSession / downloading skills in envinit / building the toolset /
// ListEvents keeps failing), this work hangs forever.
//
// On top of that, the heartbeat is a **separate goroutine** inside HandleItem, so it keeps
// beating while the main flow is stuck (every 15s, TTL 30s). Ark's reclaim
// (ReclaimOlderThanMS=45s, see arkReclaimOlderThan) only grabs work whose "heartbeat has
// expired". While the heartbeat is alive it thinks someone is handling it normally and never
// re-dispatches it. Net effect: the tool never runs, the session is stuck in requires_action
// forever, until reapStaleRuns ends the run 30 minutes later, and the user just waits. In
// production this has happened at least 6 times in a row on one environment, interleaved
// with successful runs.
//
// # Why "stop heartbeating and hand back for re-dispatch" instead of StopWork or a fake error result
//
//   - StopWork (the SDK's default action when workCtx is cancelled from outside: empty
//     cause → shouldStopItem returns true) puts the work into a final state, and Ark will not
//     re-dispatch it. That means this turn really fails.
//   - Adding an error tool_result (the closing reconciliation in workertoolaudit.go) lets Ark
//     move on, but what the agent receives is "the tool did not run", not that the tool really
//     ran. That is a last-resort fallback, not a success rate.
//   - Stop heartbeating: Ark uses the 45s reclaim threshold to re-dispatch the same work to a
//     healthy worker that is polling (at 90 seconds the server stops it for good, so this must
//     happen before then). When the new runner lists events and sees "a tool_use with no
//     tool_result", it **really runs it**. This is exactly the self-healing path already taken
//     when a pod is killed with kill -9; here we only turn it from "relying on the process
//     dying" into something we trigger on purpose.
//
// # Why it does not kill a tool that is "running normally"
//
// The threshold workToolProgressBound (180s) is **deliberately larger** than the single tool
// execution timeout (SDK DefaultToolTimeout=120s) plus container cold start (~20-30s). Once a
// tool really starts running, the SDK is sure to send its result (success or timeout error)
// back to Ark within ~150s at the latest, and that call is then no longer an orphan. So a
// tool_use that is still an orphan when failover fires never even reached the SDK's
// ResultStore.Begin (it was stuck before the runner), **and produced no side effects**.
// Rerunning is safe; no command runs twice.
package core

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

const (
	// workFailoverTick is how often the watcher checks "has the tool result come back".
	//
	// 15s: sparse enough not to add noticeable read load on Ark (4 ListEvents per minute per
	// active work), yet dense enough that detection delay is decided mainly by the threshold
	// below, not by the check interval.
	workFailoverTick = 15 * time.Second

	// workToolProgressBound is how long "the oldest tool call waiting for its result" may
	// wait at most; past that we fail over.
	//
	// ⚠️ **It must be larger than tool execution timeout (120s) + container cold start
	// (~30s)**, otherwise normal work where "the container is cold-starting and the tool is
	// about to run" would be judged stuck. This inequality is a correctness dependency,
	// written the same way as "lease TTL must be < arkReclaimOlderThan" in workerlease.go.
	// Changing either side means checking it again.
	//
	// 180s: leaves 30s of margin over the 150s worst normal path, and is still far below the
	// 10-minute turn timeout on the feishu side and the 30-minute reapStaleRuns, so the user
	// gets to "rerun on another worker and get a real answer" before those.
	workToolProgressBound = 180 * time.Second

	// workFailoverListTimeout is the timeout for one event fetch by the watcher. The watcher
	// must not stall because its own event read is stuck. It reads the unwrapped raw api, and
	// this adds a bounded timeout on top.
	workFailoverListTimeout = 10 * time.Second
)

// errWorkAbandoned is a non-fatal error returned if a heartbeat request still slips through
// after failover has let go of the work.
//
// Normally cancelWork has already been called when letting go, so the heartbeat loop's ctx
// is cancelled and no more heartbeats are sent; this is only a safeguard. It **must not be a
// 4xx** (SDK heartbeat takes the permanentFailure branch on a fatal 4xx). A plain error is
// enough: the heartbeat loop exits anyway once its ctx is cancelled.
var errWorkAbandoned = errors.New("work 已 failover 交回重派，本 worker 不再为它发心跳")

// workFailover wraps the per-work API (sendGuard) and adds a "this work has been let go"
// switch: after letting go it blocks StopWork (Ark must not put the work into a final state)
// and stops heartbeats (so Ark can reclaim it).
//
// Usage: pass it to newWorker as the selfhosted.API the SDK sees (the SDK's heartbeats and its
// closing StopWork both go through it); when the watcher judges the work stuck, call abandon()
// + cancelWork().
type workFailover struct {
	selfhosted.API
	workID string
	log    *slog.Logger

	abandoned atomic.Bool
}

func newWorkFailover(inner selfhosted.API, workID string, log *slog.Logger) *workFailover {
	return &workFailover{API: inner, workID: workID, log: log}
}

// abandon marks this work as let go. After that StopWork is swallowed and heartbeats stop.
func (f *workFailover) abandon() { f.abandoned.Store(true) }

func (f *workFailover) isAbandoned() bool { return f.abandoned.Load() }

// StopWork is **swallowed** after letting go: Ark must never put this work into a final
// state, otherwise reclaim will not re-dispatch it (work settled by StopWork is final and not
// eligible to be grabbed). This matches what shutdownSafeAPI suppresses when the process
// exits, but here it is per-work and triggered on purpose by the watcher.
func (f *workFailover) StopWork(ctx context.Context, req selfhosted.StopWorkRequest) error {
	if f.abandoned.Load() {
		f.log.Info("failover：已撒手这条 work，拦住 StopWork（交回方舟重派，不终止）",
			slog.String("work_id", req.WorkID))
		return nil
	}
	return f.API.StopWork(ctx, req)
}

// HeartbeatWork returns an error right away after letting go: it stops renewing the heartbeat
// for this work, so Ark's reclaim timer starts running. On the normal path letting go also
// cancels, so the heartbeat loop never reaches here again; this is a safeguard.
func (f *workFailover) HeartbeatWork(ctx context.Context, req selfhosted.HeartbeatWorkRequest) (*selfhosted.HeartbeatResponse, error) {
	if f.abandoned.Load() {
		return nil, errWorkAbandoned
	}
	return f.API.HeartbeatWork(ctx, req)
}

// watchWorkToolProgress is the per-work watchdog: if a tool call has gone out and there is
// still no result past the threshold, it fails over. It lives as long as workCtx, and exits
// when the work ends normally, the process exits, or it has already fired.
//
// api is the **unwrapped raw api** (the one in pollLoop), not sendGuard/workAPI: the watcher
// must still be able to read events when "the executor is stuck". The wrapper layers on the
// stuck path must not drag the watcher down. ListEvents is read-only, and here it runs once
// every 15s, so the volume is small.
func (p *WorkerPool) watchWorkToolProgress(
	ctx context.Context, f *workFailover, api selfhosted.API,
	item selfhosted.WorkItem, cancelWork context.CancelFunc, logger *slog.Logger,
) {
	p.watchWorkToolProgressWith(ctx, f, api, item, cancelWork, logger,
		workFailoverTick, workToolProgressBound)
}

// watchWorkToolProgressWith takes tick/bound as parameters so tests can drive it with small
// intervals.
func (p *WorkerPool) watchWorkToolProgressWith(
	ctx context.Context, f *workFailover, api selfhosted.API,
	item selfhosted.WorkItem, cancelWork context.CancelFunc, logger *slog.Logger,
	tick, bound time.Duration,
) {
	sessionID := item.SessionIDValue()
	// firstSeen records when each orphan call id (tool_use without tool_result) was first
	// seen. It is rebuilt each round from the current orphan set: answered calls are
	// removed from it automatically.
	firstSeen := map[string]time.Time{}

	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if f.isAbandoned() {
				return
			}
			listCtx, cancel := context.WithTimeout(ctx, workFailoverListTimeout)
			orphans, used, answered, err := listOrphanToolCalls(listCtx, api, sessionID)
			cancel()
			if err != nil {
				// If the watcher itself cannot read events it should neither misjudge nor
				// stall: skip this round and try again next round.
				logger.Debug("failover 监工拉事件失败，本轮跳过",
					slog.String("work_id", item.ID), slog.Any("err", err))
				continue
			}

			current := map[string]selfhosted.Event{}
			for _, ev := range orphans {
				current[toolCallID(ev)] = ev
			}
			// For calls that now have a result (no longer orphans), clear their first-seen time.
			for id := range firstSeen {
				if _, ok := current[id]; !ok {
					delete(firstSeen, id)
				}
			}
			// Newly appeared orphans start their timer from this round.
			for id := range current {
				if _, ok := firstSeen[id]; !ok {
					firstSeen[id] = now
				}
			}
			if len(current) == 0 {
				// The model is still thinking and has not called a tool, or all tool
				// results are back. That is normal; stay quiet.
				continue
			}

			// Find the orphan that has waited the longest.
			var oldestID string
			var oldestAt time.Time
			for id := range current {
				ts := firstSeen[id]
				if oldestID == "" || ts.Before(oldestAt) {
					oldestID, oldestAt = id, ts
				}
			}
			age := now.Sub(oldestAt).Round(time.Second)
			if age < bound {
				logger.Debug("工具结果待回传中，failover 监工继续等",
					slog.String("work_id", item.ID),
					slog.Int("待回传", len(current)),
					slog.Duration("最老等待", age))
				continue
			}

			// Judge the executor stuck: let go + stop. After stopping, the SDK's HandleItem
			// cleanup reaches StopWork, but workFailover swallows it. So Ark gets no
			// termination, only sees the heartbeat stop, and reclaim re-dispatches after
			// 45s (it must be picked up within 90s, or the server stops it automatically).
			logger.Warn("work 长时间没有工具结果，判定执行者卡死，交回方舟重派",
				slog.String("work_id", item.ID),
				slog.String("ark_session", sessionID),
				slog.String("最老工具调用", oldestID+":"+current[oldestID].Name),
				slog.Duration("已等待", age),
				slog.Duration("判定阈值", bound),
				slog.Int("发起", used),
				slog.Int("已回传", answered),
				slog.Int("待回传", len(current)))
			f.abandon()
			cancelWork()
			return
		}
	}
}
