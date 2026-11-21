package core

import (
	"context"
	"log/slog"
	"time"
)

// WorkerStartGateHooks supplies credential resolution and the auth backoff check.
type WorkerStartGateHooks struct {
	Resolve func(context.Context) (ArkBindingKey, error)
	Blocked func(string, time.Time) (bool, time.Duration)
	Logger  *slog.Logger
}

// WorkerStartAllowed resolves the binding's own credential and rejects a start
// while its fingerprint is in auth backoff. Key resolution happens on every refresh,
// so a newly configured key is released instead of retrying the old client forever.
func WorkerStartAllowed(ctx context.Context, workspaceID, environmentID string, hooks WorkerStartGateHooks) (ArkBindingKey, bool) {
	logger := hooks.Logger
	if logger == nil {
		logger = slog.Default()
	}
	key, err := hooks.Resolve(ctx)
	if err != nil {
		logger.Warn("空间没有可用的方舟凭证，worker 不启动（该空间的工具调用会卡住）",
			slog.String("workspace", workspaceID), slog.Any("err", err))
		return ArkBindingKey{}, false
	}
	if hooks.Blocked != nil {
		if blocked, left := hooks.Blocked(ArkKeyFingerprint(key.APIKey), time.Now()); blocked {
			logger.Debug("方舟凭据仍在认证退避期内，本轮跳过这个环境",
				slog.String("workspace", workspaceID), slog.String("environment", environmentID),
				slog.Duration("剩余", left.Round(time.Second)))
			return ArkBindingKey{}, false
		}
	}
	return key, true
}
