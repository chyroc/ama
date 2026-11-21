package core

import (
	"context"
	"fmt"
	"time"

	"github.com/chyroc/ama/server/shared/infra/filebytes"
)

const DefaultSkillArchiveURLTTL = 30 * time.Minute

// SkillArchive is a content-addressed package and its byte count.
type SkillArchive struct {
	URL   string
	Bytes int
}

// SkillArchiveHooks provides the already assembled package and downstream storage.
// Size returning -1 means a cache miss; other negative values are not used.
type SkillArchiveHooks struct {
	Enabled  bool
	Hash     string
	BuildZip func() ([]byte, error)
	Size     func(context.Context, string) (int, error)
	Put      func(context.Context, string, []byte) error
	Sign     func(string, time.Duration) (string, error)
	NowISO   string
}

// EnsureSkillArchive reuses a content-addressed package or uploads the complete
// zip when absent, then returns a short-lived download link. A missing signing
// capability returns nil so the caller can fall back to per-file injection.
func EnsureSkillArchive(ctx context.Context, hooks SkillArchiveHooks) (*SkillArchive, error) {
	if !hooks.Enabled || hooks.Sign == nil {
		return nil, nil
	}
	if hooks.BuildZip == nil || hooks.Size == nil || hooks.Put == nil {
		return nil, fmt.Errorf("技能包存储装配不完整")
	}
	key := filebytes.ObjectKey("skill-archives", hooks.Hash+".zip", hooks.NowISO)
	size, err := hooks.Size(ctx, key)
	if err != nil {
		return nil, err
	}
	if size < 0 {
		zip, err := hooks.BuildZip()
		if err != nil {
			return nil, fmt.Errorf("打技能包失败: %w", err)
		}
		if err := hooks.Put(ctx, key, zip); err != nil {
			return nil, fmt.Errorf("技能包写对象存储失败: %w", err)
		}
		size = len(zip)
	}
	url, err := hooks.Sign(key, DefaultSkillArchiveURLTTL)
	if err != nil {
		return nil, fmt.Errorf("签技能包下载链接失败: %w", err)
	}
	return &SkillArchive{URL: url, Bytes: size}, nil
}

// SkillArchiveSize returns the cached byte count or -1 for a cache miss. A store
// without size support also returns -1, causing an idempotent rebuild and upload.
func SkillArchiveSize(
	ctx context.Context, size func(context.Context, string) (int64, error),
	notFound func(error) bool, key string,
) (int, error) {
	if size == nil {
		return -1, nil
	}
	n, err := size(ctx, key)
	if err != nil {
		if notFound != nil && notFound(err) {
			return -1, nil
		}
		return 0, fmt.Errorf("查技能包失败: %w", err)
	}
	return int(n), nil
}
