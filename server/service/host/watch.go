package host

import (
	"context"
	"log/slog"
	"time"

	"github.com/chyroc/ama/server/shared/hostproto"
)

// SweepOfflineHosts implements core.HostWatchActions: one round of handling offline hosts.
//
// Why it is split into "list → CAS each host to offline → fail its pending execs" instead of one big SQL:
// only hosts hit by MarkHostOffline (really online and with the heartbeat evidence still stale) need the last two steps,
// and that ownership check must hold per host; setting offline and failing pending do not need atomicity across hosts
// (other instances / repeated sweeps in this round rely on their own CAS to avoid double handling).
func (s *Service) SweepOfflineHosts(
	ctx context.Context, before time.Time, limit int,
) ([]string, bool, error) {
	rows, err := s.store.ListHostsLastSeenBefore(ctx, before, limit)
	if err != nil {
		return nil, false, err
	}
	truncated := limit > 0 && len(rows) >= limit
	offline := make([]string, 0, len(rows))
	now := timeNow()
	failResult := string(hostproto.FailedExecResult(
		hostproto.ExecErrHostOffline,
		"宿主机离线（心跳超时），工具未执行或执行结果未知；请确认 ama daemon 正在运行"))
	for _, h := range rows {
		// Only flip rows that are online: machines that never came online (offline) have no in-flight calls to handle,
		// and repeated sweeps should not keep spamming the log. seenBefore=before guards against "heartbeat resumed right after listing".
		if h.Status != StatusOnline {
			continue
		}
		flipped, err := s.store.MarkHostOffline(ctx, h.ID, before, now)
		if err != nil {
			s.log.Error("宿主机置离线失败", slogHost(h.ID), errAttr(err))
			continue
		}
		if !flipped {
			continue
		}
		if n, err := s.store.FailPendingExecsForHost(ctx, h.ID, failResult, now); err != nil {
			s.log.Error("失败离线宿主机的在途调用出错", slogHost(h.ID), errAttr(err))
		} else if n > 0 {
			s.log.Info("离线宿主机的在途工具调用已按失败结算",
				slogHost(h.ID), slog.Int64("calls", n))
		}
		offline = append(offline, h.ID)
	}
	return offline, truncated, nil
}

// PurgeDoneTermDispatches implements core.HostWatchActions.
func (s *Service) PurgeDoneTermDispatches(ctx context.Context, before time.Time) (int64, error) {
	return s.store.PurgeOldTermDispatches(ctx, before)
}

func slogHost(id string) slog.Attr { return slog.String("host", id) }

// PurgeDoneExecDispatches implements core.HostWatchActions.
func (s *Service) PurgeDoneExecDispatches(ctx context.Context, before time.Time) (int64, error) {
	return s.store.PurgeDoneExecDispatches(ctx, before)
}
