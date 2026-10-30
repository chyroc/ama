package dockertools

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/chyroc/ama/server/shared/infra/docker"
)

// RunReaper periodically reaps idle session containers and workspaces, blocking until ctx is
// canceled.
//
// # What gets reaped
//
//	container  last activity older than ContainerTTL → remove the container
//	           (the volume stays; the next message rebuilds it and the files are all there)
//	volume     last activity older than WorkspaceTTL → remove the volume
//	           (this is the real "workspace reset")
//
// "Last activity" is the time in the activity ledger (PG). When the ledger has no entry for the
// session (recording failed, or it was created before the ledger existed), it falls back to the
// container / volume's own creation time.
//
// # Why take a lock
//
// Every instance starts this loop, but only one instance should scan a given daemon. Otherwise N
// instances list containers and delete at the same time, adding load on the daemon for nothing, and
// the logs get N copies of "reaped". The lock is per daemon id: instances on different daemons scan
// their own and do not block each other.
//
// When leases is nil (no Redis configured) it **still runs**: removal itself is idempotent (404
// counts as success), so several instances scanning again is only waste and never deletes the wrong
// thing. The criterion is "idle for hours", not "nobody is using it right now".
func (p *Provider) RunReaper(ctx context.Context, leases Leases) {
	if p.cfg.ContainerTTL <= 0 && p.cfg.WorkspaceTTL <= 0 {
		p.log.Info("Docker 执行端未开回收（containerTTL 与 workspaceTTL 都 <= 0），闲置容器会一直留着")
		return
	}
	if leases == nil {
		p.log.Warn("Docker 回收巡检没有选主锁（未配置 Redis），每个实例都会扫一遍（删除幂等，结果正确）")
	}
	ticker := time.NewTicker(p.cfg.ReapInterval)
	defer ticker.Stop()
	for {
		p.reapOnce(ctx, leases)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// reapOnce takes the lock and scans once. It returns the number of containers and volumes removed
// (for tests).
func (p *Provider) reapOnce(ctx context.Context, leases Leases) (int, int) {
	if leases != nil {
		key := p.cfg.Prefix + ":docker-reaper:" + p.DaemonID()
		// The lock TTL is shorter than the interval: if the lock holder dies, another instance
		// picks up next round.
		lease, ok, err := leases.Acquire(ctx, key, p.cfg.ReapInterval*9/10)
		if err != nil {
			p.log.Warn("Docker 回收巡检抢锁失败，本轮跳过", slog.Any("err", err))
			return 0, 0
		}
		if !ok {
			return 0, 0
		}
		defer func() {
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = lease.Release(rctx)
		}()
	}
	return p.reap(ctx)
}

func (p *Provider) reap(ctx context.Context) (int, int) {
	now := p.now()
	managed := []string{p.labelManaged() + "=true"}

	containers, err := p.engine.ContainerList(ctx, append(managed, p.labelKind()+"=session"))
	if err != nil {
		p.log.Warn("Docker 回收巡检列容器失败，本轮跳过", slog.Any("err", err))
		return 0, 0
	}
	volumes, err := p.engine.VolumeList(ctx, append(managed, p.labelKind()+"=workspace"))
	if err != nil {
		p.log.Warn("Docker 回收巡检列 volume 失败，本轮跳过", slog.Any("err", err))
		return 0, 0
	}

	ids := map[string]bool{}
	for _, c := range containers {
		if id := c.Labels[p.labelSession()]; id != "" {
			ids[id] = true
		}
	}
	for _, v := range volumes {
		if id := v.Labels[p.labelSession()]; id != "" {
			ids[id] = true
		}
	}
	activity := map[string]time.Time{}
	p.mu.Lock()
	ledger := p.activity
	p.mu.Unlock()
	if ledger != nil && len(ids) > 0 {
		list := make([]string, 0, len(ids))
		for id := range ids {
			list = append(list, id)
		}
		got, err := ledger.SessionActivity(ctx, list)
		if err != nil {
			// If the ledger cannot be read, **do not delete**: using the creation time as last
			// activity would wrongly delete old sessions still in use.
			p.log.Warn("Docker 回收巡检读活动账本失败，本轮跳过", slog.Any("err", err))
			return 0, 0
		}
		activity = got
	}

	removedC := 0
	if p.cfg.ContainerTTL > 0 {
		for _, c := range containers {
			sid := c.Labels[p.labelSession()]
			last, ok := activity[sid]
			if !ok {
				last = time.Unix(c.Created, 0)
			}
			if now.Sub(last) <= p.cfg.ContainerTTL {
				continue
			}
			if err := p.engine.ContainerRemove(ctx, c.ID, true); err != nil {
				p.log.Warn("回收闲置容器失败", slog.String("container", containerName(c)), slog.Any("err", err))
				continue
			}
			removedC++
			p.log.Info("已回收闲置容器（工作区 volume 保留，下次来消息自动重建）",
				slog.String("container", containerName(c)), slog.String("ark_session", sid),
				slog.Duration("idle", now.Sub(last).Round(time.Minute)))
		}
	}

	removedV := 0
	if p.cfg.WorkspaceTTL > 0 {
		for _, v := range volumes {
			sid := v.Labels[p.labelSession()]
			last, ok := activity[sid]
			if !ok {
				t, err := time.Parse(time.RFC3339Nano, v.CreatedAt)
				if err != nil {
					continue // cannot understand the creation time, so leave it alone
				}
				last = t
			}
			if now.Sub(last) <= p.cfg.WorkspaceTTL {
				continue
			}
			// The daemon cannot remove a volume still mounted on a container (409), so remove that
			// container first.
			if sid != "" {
				if err := p.engine.ContainerRemove(ctx, p.ContainerName(sid), true); err != nil {
					p.log.Warn("回收工作区前删容器失败", slog.String("volume", v.Name), slog.Any("err", err))
					continue
				}
			}
			if err := p.engine.VolumeRemove(ctx, v.Name); err != nil {
				if docker.IsConflict(err) {
					p.log.Info("工作区 volume 仍被占用，下轮再试", slog.String("volume", v.Name))
					continue
				}
				p.log.Warn("回收闲置工作区失败", slog.String("volume", v.Name), slog.Any("err", err))
				continue
			}
			removedV++
			p.log.Info("已回收闲置工作区 volume（这个会话再来消息时工作区是空的）",
				slog.String("volume", v.Name), slog.String("ark_session", sid),
				slog.Duration("idle", now.Sub(last).Round(time.Minute)))
		}
	}
	return removedC, removedV
}

func containerName(c docker.ContainerSummary) string {
	if len(c.Names) > 0 {
		return strings.TrimPrefix(c.Names[0], "/")
	}
	return c.ID
}
