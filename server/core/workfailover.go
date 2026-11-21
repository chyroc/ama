package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

const (
	// DefaultWorkFailoverTick is the tool-progress check interval.
	DefaultWorkFailoverTick = 15 * time.Second
	// DefaultWorkToolProgressBound exceeds tool timeout plus a cold start.
	DefaultWorkToolProgressBound = 180 * time.Second
	// DefaultWorkFailoverListTimeout bounds the watchdog's own event read.
	DefaultWorkFailoverListTimeout = 10 * time.Second
)

// WorkFailover is a per-work API wrapper that hands a stuck work item back to Ark.
type WorkFailover = workFailover

// NewWorkFailover wraps inner so Abandon can stop heartbeats and suppress StopWork.
func NewWorkFailover(inner selfhosted.API, workID string, logger *slog.Logger) *WorkFailover {
	return newWorkFailover(inner, workID, logger)
}

// Abandon lets Ark reclaim this work rather than receiving its termination.
func (f *WorkFailover) Abandon() { f.abandon() }

// Abandoned reports whether this work has been handed back for re-dispatch.
func (f *WorkFailover) Abandoned() bool { return f.isAbandoned() }

// WatchWorkToolProgress stops and hands back a work item whose oldest unanswered
// tool call has exceeded bound. api must be the unwrapped API so a blocked executor
// cannot block this watchdog too. List failures are retried, never treated as proof of a stall.
func WatchWorkToolProgress(
	ctx context.Context, failover *WorkFailover, api selfhosted.API,
	item selfhosted.WorkItem, cancelWork context.CancelFunc, logger *slog.Logger,
	tick, bound time.Duration,
) {
	if tick <= 0 {
		tick = DefaultWorkFailoverTick
	}
	if bound <= 0 {
		bound = DefaultWorkToolProgressBound
	}
	if logger == nil {
		logger = slog.Default()
	}
	watchWorkToolProgress(ctx, failover, api, item, cancelWork, logger, tick, bound)
}

func watchWorkToolProgress(
	ctx context.Context, f *workFailover, api selfhosted.API,
	item selfhosted.WorkItem, cancelWork context.CancelFunc, logger *slog.Logger,
	tick, bound time.Duration,
) {
	sessionID := item.SessionIDValue()
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
			listCtx, cancel := context.WithTimeout(ctx, DefaultWorkFailoverListTimeout)
			orphans, used, answered, err := listOrphanToolCalls(listCtx, api, sessionID)
			cancel()
			if err != nil {
				logger.Debug("failover 监工拉事件失败，本轮跳过",
					slog.String("work_id", item.ID), slog.Any("err", err))
				continue
			}
			current := map[string]selfhosted.Event{}
			for _, ev := range orphans {
				current[toolCallID(ev)] = ev
			}
			for id := range firstSeen {
				if _, ok := current[id]; !ok {
					delete(firstSeen, id)
				}
			}
			for id := range current {
				if _, ok := firstSeen[id]; !ok {
					firstSeen[id] = now
				}
			}
			if len(current) == 0 {
				continue
			}
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
					slog.String("work_id", item.ID), slog.Int("待回传", len(current)), slog.Duration("最老等待", age))
				continue
			}
			logger.Warn("work 长时间没有工具结果，判定执行者卡死，交回方舟重派",
				slog.String("work_id", item.ID), slog.String("ark_session", sessionID),
				slog.String("最老工具调用", oldestID+":"+current[oldestID].Name),
				slog.Duration("已等待", age), slog.Duration("判定阈值", bound),
				slog.Int("发起", used), slog.Int("已回传", answered), slog.Int("待回传", len(current)))
			f.abandon()
			cancelWork()
			return
		}
	}
}
