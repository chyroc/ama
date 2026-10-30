// Three things about credential aliases on the core side: write-path validation, permission
// level, and **never entering the system prompt**.
//
// Injection itself and masking are tested in shared/infra/shelltools, at the wire level;
// this file covers "can it be stored, who can store it, and does a stored one leak into the
// prompt".

package core

import (
	"context"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/infra/shelltools"
)

// aliasAgent creates an agent with aliases and returns the create error for the caller to assert.
func aliasAgent(
	t *testing.T, svc *Service, aliases []apimodel.AgentEnvAlias, envs []apimodel.AgentEnv,
) (apimodel.Agent, error) {
	t.Helper()
	return svc.CreateAgent(context.Background(), seedWS, apimodel.AgentDraft{
		Name:       "带别名的 agent",
		System:     "你是一个助手。",
		Model:      apimodel.ModelConfig{ID: "doubao-pro"},
		Envs:       envs,
		EnvAliases: aliases,
	})
}

// ⭐⭐ 1. Aliases must **never** enter the system prompt.
//
// # Why this case exists on its own instead of relying on the "secrets stay out of the prompt" case
//
// That filter checks `AgentEnv.Secret`, but an alias is not an AgentEnv at all; it is
// **a different field** on Agent. renderEnvPrompt only walks Envs, so aliases naturally
// cannot get in.
//
// This case pins down exactly that "naturally": if someone one day merges aliases into Envs
// (which looks simpler), their values would go into the `<harness:env>` block → into the
// model context → into the permanent RunEvent record → pushed to the browser. And an alias
// value is a **real credential**, not some config the user typed casually.
func TestEnvAliasNeverEntersSystemPrompt(t *testing.T) {
	svc, _ := newService(t)
	a, err := aliasAgent(t, svc, []apimodel.AgentEnvAlias{
		{Key: "LARK_TOKEN", From: shelltools.EnvLarkUserAccessToken},
	}, []apimodel.AgentEnv{
		{Key: "DEPLOY_REGION", Value: "cn-east"},
	})
	if err != nil {
		t.Fatalf("建 agent 失败: %v", err)
	}

	spec := arkSpecFor(a)

	// Normal envs still go in (proves prompt rendering itself works, so this is not a false pass).
	if !strings.Contains(spec.System, "DEPLOY_REGION=cn-east") {
		t.Fatalf("前提不成立：普通 env 都没进提示词，这条用例白测了\n%s", spec.System)
	}
	// Neither the alias **name nor its source name** should appear.
	//
	// The value cannot appear (this layer never has the value), so assert on names; that is
	// exactly the fingerprint of the wrong implementation "the alias was rendered as a normal env".
	if strings.Contains(spec.System, "LARK_TOKEN") {
		t.Errorf("❌ 别名进了系统提示词。它的值是**真凭据**，一旦进去就会"+
			"落 RunEvent 永久记录、被模型复述、推给浏览器。\n"+
			"→ 别名必须是 Agent 上的独立字段，不能并进 Envs\n%s", spec.System)
	}
	if strings.Contains(spec.System, shelltools.EnvLarkUserAccessToken) {
		t.Errorf("❌ 别名的源名进了系统提示词：\n%s", spec.System)
	}
}

// 2. Write-path validation: invalid aliases cannot be stored.
//
// Every error is one **the user can follow to fix**. This is the point of "block at write
// time" rather than "silently ignore at runtime": "configured but no effect" is much harder
// to debug than "rejected while configuring".
func TestEnvAliasValidation(t *testing.T) {
	cases := []struct {
		name    string
		aliases []apimodel.AgentEnvAlias
		envs    []apimodel.AgentEnv
		wantErr string
	}{
		{
			name:    "变量名非法（数字开头）",
			aliases: []apimodel.AgentEnvAlias{{Key: "2LARK", From: shelltools.EnvLarkUserAccessToken}},
			wantErr: "不能以数字开头",
		},
		{
			// ⚠️ This is a security boundary: if allowed, the user could declare
			// `LARKSUITE_CLI_USER_ACCESS_TOKEN ← AMA_TOKEN` and replace the identity in the container.
			name: "目标名是平台保留键",
			aliases: []apimodel.AgentEnvAlias{
				{Key: shelltools.EnvLarkUserAccessToken, From: shelltools.EnvAMAToken},
			},
			wantErr: "平台保留",
		},
		{
			name:    "源不在可别名清单里",
			aliases: []apimodel.AgentEnvAlias{{Key: "MY_WS", From: "AMA_WORKSPACE_ID"}},
			wantErr: "不是可引用的平台凭据",
		},
		{
			name: "两条别名重名",
			aliases: []apimodel.AgentEnvAlias{
				{Key: "LARK_TOKEN", From: shelltools.EnvLarkUserAccessToken},
				{Key: "LARK_TOKEN", From: shelltools.EnvAMAToken},
			},
			wantErr: "重复",
		},
		{
			// On a name clash, "who wins" depends on merge order, which is undefined behavior; it must be rejected at write time.
			name:    "别名与自定义 env 撞名",
			aliases: []apimodel.AgentEnvAlias{{Key: "SHARED", From: shelltools.EnvLarkUserAccessToken}},
			envs:    []apimodel.AgentEnv{{Key: "SHARED", Value: "x"}},
			wantErr: "只能留一个",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newService(t)
			_, err := aliasAgent(t, svc, tc.aliases, tc.envs)
			if err == nil {
				t.Fatalf("应当被拒但存进去了")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("报错要能让用户照着改。期望含 %q，实际 %q", tc.wantErr, err)
			}
		})
	}
}

// 3. Valid aliases can be stored, and their shape is stable when read back.
//
// This also pins down **non-nil**: nil encodes as null, and the frontend's isDirty is a plain
// JSON.stringify comparison, so null != [] would keep the save button lit forever.
func TestEnvAliasRoundTrip(t *testing.T) {
	svc, _ := newService(t)
	a, err := aliasAgent(t, svc, []apimodel.AgentEnvAlias{
		{Key: "LARK_TOKEN", From: shelltools.EnvLarkUserAccessToken},
	}, nil)
	if err != nil {
		t.Fatalf("合法的别名应当存得进去: %v", err)
	}
	if len(a.EnvAliases) != 1 || a.EnvAliases[0].Key != "LARK_TOKEN" {
		t.Fatalf("别名没存对: %+v", a.EnvAliases)
	}

	// An agent without aliases must also be [] (not nil).
	plain, err := svc.CreateAgent(context.Background(), seedWS, apimodel.AgentDraft{
		Name: "没有别名", System: "x", Model: apimodel.ModelConfig{ID: "doubao-pro"},
	})
	if err != nil {
		t.Fatalf("建 agent 失败: %v", err)
	}
	if plain.EnvAliases == nil {
		t.Error("❌ EnvAliases 是 nil——会编码成 null，让前端保存按钮永久亮着")
	}
}
