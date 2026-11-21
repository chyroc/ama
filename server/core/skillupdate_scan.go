package core

import (
	"context"
	"log/slog"
	"time"
)

const (
	// DefaultSkillUpdateScanInterval avoids a full upstream scan on every rolling restart.
	DefaultSkillUpdateScanInterval = 30 * time.Minute
	// DefaultSkillUpdateLeaseTTL allows another instance to take over after a stopped scan.
	DefaultSkillUpdateLeaseTTL = 5 * time.Minute
	// DefaultSkillUpdateMaxPerScan bounds one pass; a stable list order resumes the remainder.
	DefaultSkillUpdateMaxPerScan = 50
)

// SkillUpdateItem is the source metadata needed for logs and an implementation-specific
// update. Credentials and provider clients stay in the downstream implementation.
type SkillUpdateItem struct {
	ID          string
	WorkspaceID string
	Name        string
	Identifier  string
	InstalledBy string
	Version     string
	// Value is the full downstream skill. It is deliberately opaque to this package.
	Value any
}

// SkillUpdateScanHooks wires one optional Redis lease to persistent listing and updates.
// The lease only avoids duplicate downloads. Update must use an optimistic write or
// another persistent guard, because two instances may both scan after a lease failure.
type SkillUpdateScanHooks struct {
	Leases     Leases
	LeaderKey  string
	LeaderTTL  time.Duration
	MaxPerScan int
	List       func(context.Context) ([]SkillUpdateItem, error)
	// Update returns the new source version and whether the installed content changed.
	// It must leave the installed content unchanged when it returns an error.
	Update func(context.Context, SkillUpdateItem) (newVersion string, changed bool, err error)
	// RecordError may persist LastError without replacing the last usable skill version.
	RecordError func(context.Context, SkillUpdateItem, error)
	Logger      *slog.Logger
}

// ScanSkillUpdates runs one serial skill update pass. An unavailable lease service does
// not disable updates: doing so would make Redis a new single point of failure. A lease
// held by another instance does skip the pass. Items above the limit are explicitly logged.
func ScanSkillUpdates(ctx context.Context, hooks SkillUpdateScanHooks) {
	logger := hooks.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ttl := hooks.LeaderTTL
	if ttl <= 0 {
		ttl = DefaultSkillUpdateLeaseTTL
	}
	if hooks.Leases != nil {
		lease, ok, err := hooks.Leases.Acquire(ctx, hooks.LeaderKey, ttl)
		if err != nil {
			logger.Warn("抢技能更新锁失败，本轮照常扫（重复由乐观并发兜住）", slog.Any("err", err))
		} else if !ok {
			return
		} else if lease != nil {
			defer func() {
				if err := lease.Release(context.WithoutCancel(ctx)); err != nil {
					logger.Warn("释放技能更新锁失败，下一拍要等它过期", slog.Any("err", err))
				}
			}()
		}
	}
	if hooks.List == nil || hooks.Update == nil {
		logger.Error("技能自动更新巡检缺少存储或更新实现，本轮跳过")
		return
	}
	skills, err := hooks.List(ctx)
	if err != nil {
		logger.Warn("列自动更新技能失败，下个周期重试", slog.Any("err", err))
		return
	}
	limit := hooks.MaxPerScan
	if limit <= 0 {
		limit = DefaultSkillUpdateMaxPerScan
	}
	if len(skills) > limit {
		logger.Info("待检查的技能超过单轮上限，剩下的下一轮继续",
			slog.Int("total", len(skills)), slog.Int("本轮处理", limit),
			slog.Int("推迟", len(skills)-limit))
		skills = skills[:limit]
	}
	var checked, updated, failed int
	for _, skill := range skills {
		if ctx.Err() != nil {
			return
		}
		checked++
		newVersion, changed, err := hooks.Update(ctx, skill)
		if err != nil {
			failed++
			if hooks.RecordError != nil {
				hooks.RecordError(ctx, skill, err)
			}
			logger.Warn("技能自动更新失败，保留旧版本，下轮重试",
				slog.String("skill", skill.ID), slog.String("workspace", skill.WorkspaceID),
				slog.String("identifier", skill.Identifier), slog.String("installed_by", skill.InstalledBy),
				slog.Any("err", err))
			continue
		}
		if !changed {
			continue
		}
		updated++
		logger.Info("技能已自动更新到新版",
			slog.String("skill", skill.ID), slog.String("workspace", skill.WorkspaceID),
			slog.String("name", skill.Name), slog.String("from", skill.Version),
			slog.String("to", newVersion))
	}
	if updated > 0 || failed > 0 {
		logger.Info("技能自动更新巡检完成",
			slog.Int("检查", checked), slog.Int("更新", updated), slog.Int("失败", failed))
	}
}
