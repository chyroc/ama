package app

import "log/slog"

// LoginAvailability describes actual login capabilities, not whether client pointers are non-nil.
// The implementation supplies these booleans after checking its own configuration.
type LoginAvailability struct {
	FixedAPIKey      bool
	ManagedKey       bool
	LoginMethod      string
	LoginConfigured  bool
	MissingLoginHint string
}

// LogLoginAvailability makes disabled authentication and managed-key injection visible at
// startup. A fixed API key is a privileged machine channel and should be reviewed explicitly.
func LogLoginAvailability(logger *slog.Logger, availability LoginAvailability) {
	if logger == nil {
		logger = slog.Default()
	}
	if availability.FixedAPIKey {
		logger.Warn("已启用固定 API Key 通道，请确认这是预期的（只给 CI/脚本用）")
	}
	if !availability.ManagedKey {
		logger.Warn("未配置托管 Key 推导密钥：需要平台凭据的 agent 在容器里运行 CLI 会失败（报没有凭据）")
	}
	if availability.LoginConfigured {
		logger.Info("登录方式已配置", slog.String("method", availability.LoginMethod))
	} else {
		hint := availability.MissingLoginHint
		if hint == "" {
			hint = "没有可用的登录方式，浏览器用户无法登录；固定或用户 API Key 仍可按已有权限使用"
		}
		logger.Error("登录不可用", slog.String("reason", hint))
	}
}

// LogTerminalWebSocket reports the actual WS origin allowlist. An empty allowlist is
// fail-closed for cross-origin requests, so the browser falls back to SSE+POST; it is not "enabled".
func LogTerminalWebSocket(logger *slog.Logger, origins []string) {
	if logger == nil {
		logger = slog.Default()
	}
	if len(origins) == 0 {
		logger.Warn("未配置终端 WebSocket 来源白名单：浏览器连接将被拒绝并回退到 SSE+POST（连打可能乱序）")
		return
	}
	logger.Info("终端 WebSocket 已启用", slog.Any("allowed_origins", origins))
}
