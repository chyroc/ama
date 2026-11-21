package core

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
)

// PersonalCredentialSubject returns the agent creator for personal credentials.
// Only an existing agent with no recorded creator falls back to the workspace owner,
// preserving legacy behavior and leaving an explicit log for privilege investigations.
func PersonalCredentialSubject(
	ctx context.Context, agent apimodel.Agent, workspaceID string,
	loadOwner func(context.Context, string) (string, error),
	precondition error, logger *slog.Logger,
) (string, error) {
	if creator := strings.TrimSpace(agent.CreatedBy); creator != "" {
		return creator, nil
	}
	if loadOwner == nil {
		return "", fmt.Errorf("%w: 空间没有 owner，取不到可用身份", precondition)
	}
	owner, err := loadOwner(ctx, workspaceID)
	if err != nil {
		return "", fmt.Errorf("空间不存在: %w", err)
	}
	if owner == "" {
		return "", fmt.Errorf("%w: 空间没有 owner，取不到可用身份", precondition)
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "agent 没有记录创建者，个人授权回落到空间 owner（存量行为）",
		"agent", agent.ID, "workspace", workspaceID, "owner", owner)
	return owner, nil
}

// AppendUserEnvs adds user-defined environment entries without allowing platform
// reserved names to overwrite computed credentials. Secret names are returned
// separately for output scrubbing. Invalid entries are logged, not silently accepted.
func AppendUserEnvs(
	ctx context.Context, agentID string, entries []apimodel.AgentEnv,
	envs map[string]string, isReserved func(string) bool, logger *slog.Logger,
) []string {
	if logger == nil {
		logger = slog.Default()
	}
	var secretKeys []string
	for _, entry := range entries {
		key := strings.TrimSpace(entry.Key)
		if key == "" {
			continue
		}
		if isReserved != nil && isReserved(key) {
			logger.WarnContext(ctx, "自定义环境变量用了平台保留名，已忽略这一条", "agent", agentID, "key", key)
			continue
		}
		envs[key] = entry.Value
		if entry.Secret {
			secretKeys = append(secretKeys, key)
		}
	}
	return secretKeys
}
