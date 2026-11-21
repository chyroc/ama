package core

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/infra/shelltools"
)

// SkillRuntimeConfig contains the server-managed settings used by optional built-in skills.
// Disabled settings never change an agent's environment.
type SkillRuntimeConfig struct {
	CUA         CUASkillConfig
	Seedance    SeedanceSkillConfig
	OpenViking  OpenVikingServerConfig
	ArkVaultIDs []string
}

// AppendBuiltinSkillEnvs adds the settings needed by the selected optional skills.
// User-entered values win; secretKeys tells the executor which values to redact.
func AppendBuiltinSkillEnvs(
	agent apimodel.Agent, workspaceID string, cfg SkillRuntimeConfig,
	envs map[string]string, secretKeys []string,
) (map[string]string, []string) {
	if envs == nil {
		envs = make(map[string]string)
	}
	// Callers normally pass the user environment assembled by their own reserved-key filter.
	// AppendUserEnvs is a second filter for standalone users, not a license to inject reserved keys.
	AppendUserEnvs(context.Background(), agent.ID, agent.Envs, envs, shelltools.IsPlatformReservedEnv, nil)
	if contains(agent.SkillIDs, BuiltinSkillIDCUA) && cfg.CUA.Enabled {
		setEmptyEnv(envs, "AP_CUA_SKILL_API_BASE_URL", trimURL(cfg.CUA.GatewayURL))
		setEmptyEnv(envs, "CUA_CREDENTIAL_ARTIFACT_BASE_URL", trimURL(cfg.CUA.ArtifactBaseURL))
	}
	if contains(agent.SkillIDs, BuiltinSkillIDSeedanceMA) && cfg.Seedance.Enabled {
		if setEmptyEnv(envs, "ARK_SEEDANCE_API_KEY", cfg.Seedance.APIKey) {
			secretKeys = append(secretKeys, "ARK_SEEDANCE_API_KEY")
		}
		setEmptyEnv(envs, "ARK_SEEDANCE_API_BASE_URL", trimURL(cfg.Seedance.APIBaseURL))
		setEmptyEnv(envs, "ARK_SEEDANCE_AGENT_ID", cfg.Seedance.AgentID)
		setEmptyEnv(envs, "ARK_SEEDANCE_ENVIRONMENT_ID", cfg.Seedance.EnvironmentID)
		setEmptyEnv(envs, "ARK_SEEDANCE_VIDEO_MODEL", cfg.Seedance.VideoModel)
		setEmptyEnv(envs, "ARK_SEEDANCE_IMAGE_MODEL", cfg.Seedance.ImageModel)
		setJSONEnv(envs, "ARK_SEEDANCE_VAULT_IDS", cfg.ArkVaultIDs)
	}
	if contains(agent.SkillIDs, BuiltinSkillIDOpenVikingMemory) {
		ov := resolveOpenVikingConfig(cfg.OpenViking, workspaceID, agent)
		if ov.Enabled {
			setEmptyEnv(envs, "OPENVIKING_URL", ov.URL)
			if setEmptyEnv(envs, "OPENVIKING_API_KEY", ov.APIKey) {
				secretKeys = append(secretKeys, "OPENVIKING_API_KEY")
			}
			setEmptyEnv(envs, "OPENVIKING_RESOURCE_ID", ov.ResourceID)
			setEmptyEnv(envs, "OPENVIKING_USER", ov.User)
			setEmptyEnv(envs, "OPENVIKING_AGENT_ID", ov.AgentID)
			setEmptyEnv(envs, "OPENVIKING_ENABLED", "true")
		}
	}
	if len(cfg.ArkVaultIDs) > 0 {
		setJSONEnv(envs, "ARK_VAULT_IDS", cfg.ArkVaultIDs)
	}
	return envs, secretKeys
}

func setEmptyEnv(envs map[string]string, key, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.TrimSpace(envs[key]) != "" {
		return false
	}
	envs[key] = value
	return true
}

func setJSONEnv(envs map[string]string, key string, value any) {
	if strings.TrimSpace(envs[key]) != "" {
		return
	}
	if raw, err := json.Marshal(value); err == nil {
		envs[key] = string(raw)
	}
}

func trimURL(value string) string { return strings.TrimRight(strings.TrimSpace(value), "/") }
