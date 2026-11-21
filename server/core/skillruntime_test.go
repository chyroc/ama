package core

import (
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

func TestAppendBuiltinSkillEnvsKeepsUserOverridesAndScopesMemory(t *testing.T) {
	agent := apimodel.Agent{
		ID: "agent-1", WorkspaceID: "ws-1", OpenVikingEnabled: true,
		SkillIDs: []string{BuiltinSkillIDCUA, BuiltinSkillIDSeedanceMA, BuiltinSkillIDOpenVikingMemory},
		Envs:     []apimodel.AgentEnv{{Key: "ARK_SEEDANCE_API_KEY", Value: "user-value"}},
	}
	envs, secrets := AppendBuiltinSkillEnvs(agent, "ws-1", SkillRuntimeConfig{
		CUA:         CUASkillConfig{Enabled: true, GatewayURL: "https://cua.example.com/"},
		Seedance:    SeedanceSkillConfig{Enabled: true, APIKey: "server-value", AgentID: "fake-agent"},
		OpenViking:  OpenVikingServerConfig{Enabled: true, APIKey: "memory-key", ResourceID: "memory-resource"},
		ArkVaultIDs: []string{"fake-vault"},
	}, map[string]string{}, nil)
	if envs["ARK_SEEDANCE_API_KEY"] != "user-value" {
		t.Fatalf("用户配置不能被服务端覆盖: %q", envs["ARK_SEEDANCE_API_KEY"])
	}
	if envs["OPENVIKING_USER"] != "ws-1" || envs["OPENVIKING_AGENT_ID"] != "agent-1" {
		t.Fatalf("记忆范围必须隔离空间和 Agent: %#v", envs)
	}
	if envs["AP_CUA_SKILL_API_BASE_URL"] != "https://cua.example.com" {
		t.Fatalf("URL 末尾斜杠应去掉: %q", envs["AP_CUA_SKILL_API_BASE_URL"])
	}
	if len(secrets) != 1 || secrets[0] != "OPENVIKING_API_KEY" {
		t.Fatalf("只需标记实际注入的秘密: %#v", secrets)
	}
}

func TestAppendBuiltinSkillEnvsDisabledDoesNothing(t *testing.T) {
	envs, secrets := AppendBuiltinSkillEnvs(apimodel.Agent{SkillIDs: []string{BuiltinSkillIDCUA}}, "", SkillRuntimeConfig{}, nil, nil)
	if len(envs) != 0 || len(secrets) != 0 {
		t.Fatalf("未启用功能不得产生环境变量: %#v %#v", envs, secrets)
	}
}
