// The invariant that Ark-derived resources (Environment / Agent / skill uploads) **belong to the
// key that created them**.
//
// # Why it needs its own test file
//
// Back when keys were only workspace-level, this invariant **held automatically** (same workspace
// means the same key), so ensureBinding always did "reuse the environment of any binding in the
// same workspace". Once agent-level credentials came in, that "automatically" was gone:
//
//	agent A builds env-1 with key1
//	agent B uses key2 and reuses env-1        ← uses key2 to access key1's project
//
// Ark's error says the credential is bad, while each of the two keys is fine on its own: **the
// error does not point at the real cause at all**. The table comment on ark_skill_uploads
// recorded the same kind of problem long ago ("not separating by workspace would give workspace B
// a skill_id that does not exist in its own project"); back then, separating by workspace was
// enough.
//
// All these cases are offline (through fakeRunner), because what we test is "how we reuse
// things ourselves", not how Ark responds.

package core

import (
	"context"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/repository/dao/coremem"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// arkKeyFixture builds a workspace + two agents, and gives each agent a different key.
//
// The returned svc has fakeRunner installed, so the whole chat path can run offline.
func arkKeyFixture(t *testing.T) (*Service, *coremem.MemStore, *fakeRunner, string, string, string) {
	t.Helper()
	store := coremem.NewMemStore()
	runner := &fakeRunner{sessionID: "sess-1"}
	svc, err := New(Config{Store: store, ArkRunner: runner})
	if err != nil {
		t.Fatalf("建 Service 失败: %v", err)
	}
	ctx := context.Background()

	ws, err := store.CreateWorkspace(ctx, apimodel.Workspace{
		ID: "ws_key", Name: "键", Key: "key-ws",
		Members: []apimodel.WorkspaceMember{{
			ID: "m1", Email: "owner@example.com", Role: apimodel.RoleOwner,
		}},
	})
	if err != nil {
		t.Fatalf("建空间失败: %v", err)
	}

	mk := func(id string) string {
		a, err := store.CreateAgent(ctx, apimodel.Agent{
			ID: id, WorkspaceID: ws.ID, Name: id,
			Model:        apimodel.ModelConfig{ID: "doubao-pro"},
			BuiltinTools: []string{}, SkillIDs: []string{}, FeishuAppIDs: []string{},
			Status: apimodel.StatusActive,
		})
		if err != nil {
			t.Fatalf("建 agent 失败: %v", err)
		}
		return a.ID
	}
	return svc, store, runner, ws.ID, mk("agent_a"), mk("agent_b")
}

// Two agents with the same key must reuse the same Environment (existing behavior, must not break).
func TestSameKeyReusesEnvironment(t *testing.T) {
	svc, store, runner, ws, agentA, agentB := arkKeyFixture(t)
	ctx := context.Background()

	if err := store.PutArkSecret(ctx, apimodel.ArkSecret{
		ArkCredential: apimodel.ArkCredential{
			WorkspaceID: ws, Region: apimodel.RegionCNBeijing,
			Source: apimodel.SourceOverride,
		},
		APIKey: "ark-the-only-key-0000000000",
	}); err != nil {
		t.Fatalf("存凭证失败: %v", err)
	}

	for _, agentID := range []string{agentA, agentB} {
		if _, err := svc.CreateConversation(ctx, ws, agentID); err != nil {
			t.Fatalf("开对话失败（agent=%s）: %v", agentID, err)
		}
	}
	if runner.createdEnvs != 1 {
		t.Errorf("同一把 key 应当只建一个环境，实际建了 %d 个", runner.createdEnvs)
	}
}

// ⚠️ **Two different keys must never reuse the same Environment.**
//
// If this fails, agent B is using key2 to access an environment in key1's project. Ark reports
// "invalid credential", while both of the user's keys are fine, and the real cause cannot be found.
func TestDifferentKeysDoNotShareEnvironment(t *testing.T) {
	svc, store, runner, ws, agentA, agentB := arkKeyFixture(t)
	ctx := context.Background()

	// agent A uses the workspace-level key.
	if err := store.PutArkSecret(ctx, apimodel.ArkSecret{
		ArkCredential: apimodel.ArkCredential{
			WorkspaceID: ws, Region: apimodel.RegionCNBeijing,
			Source: apimodel.SourceOverride,
		},
		APIKey: "ark-workspace-key-111111111",
	}); err != nil {
		t.Fatalf("存空间级凭证失败: %v", err)
	}
	if _, err := svc.CreateConversation(ctx, ws, agentA); err != nil {
		t.Fatalf("agent A 开对话失败: %v", err)
	}
	if runner.createdEnvs != 1 {
		t.Fatalf("第一次应当建一个环境，实际 %d", runner.createdEnvs)
	}

	// Replace the workspace-level key, simulating agent B having a different key in effect.
	//
	// (Agent-level storage is the next piece of work; swapping the workspace-level key has the
	// same effect here: ensureBinding sees "a key different from the existing binding".)
	if err := store.PutArkSecret(ctx, apimodel.ArkSecret{
		ArkCredential: apimodel.ArkCredential{
			WorkspaceID: ws, Region: apimodel.RegionCNBeijing,
			Source: apimodel.SourceOverride,
		},
		APIKey: "ark-another-key-2222222222",
	}); err != nil {
		t.Fatalf("换凭证失败: %v", err)
	}
	if _, err := svc.CreateConversation(ctx, ws, agentB); err != nil {
		t.Fatalf("agent B 开对话失败: %v", err)
	}

	if runner.createdEnvs != 2 {
		t.Errorf("换了 key 必须另建环境（不能复用别人 project 的），"+
			"实际只建了 %d 个", runner.createdEnvs)
	}

	// The two bindings must have different environment ids, and each records its own key's fingerprint.
	bindA, err := store.GetArkBinding(ctx, ws, agentA)
	if err != nil {
		t.Fatalf("读 agent A 的绑定失败: %v", err)
	}
	bindB, err := store.GetArkBinding(ctx, ws, agentB)
	if err != nil {
		t.Fatalf("读 agent B 的绑定失败: %v", err)
	}
	if bindA.ArkEnvironmentID == bindB.ArkEnvironmentID {
		t.Errorf("两把 key 共用了环境 %q", bindA.ArkEnvironmentID)
	}
	if bindA.KeyFingerprint == "" || bindB.KeyFingerprint == "" {
		t.Error("绑定要记下 key 指纹，否则下次判不出能不能复用")
	}
	if bindA.KeyFingerprint == bindB.KeyFingerprint {
		t.Error("两把不同的 key 指纹不该相同")
	}
	// The fingerprint must never contain plaintext.
	for _, b := range []persistmodel.ArkBinding{bindA, bindB} {
		if b.KeyFingerprint == "ark-workspace-key-111111111" ||
			b.KeyFingerprint == "ark-another-key-2222222222" {
			t.Error("指纹里出现了明文 key")
		}
	}
}

// After the key changes, that agent's Ark Agent must also be rebuilt.
//
// The old Ark Agent belongs to the old key's project. Keeping it makes Ark report "resource does
// not exist", and the error points at the credential, so the user will think their newly
// configured key is bad.
func TestChangingKeyRebuildsArkAgent(t *testing.T) {
	svc, store, runner, ws, agentA, _ := arkKeyFixture(t)
	ctx := context.Background()

	put := func(key string) {
		t.Helper()
		if err := store.PutArkSecret(ctx, apimodel.ArkSecret{
			ArkCredential: apimodel.ArkCredential{
				WorkspaceID: ws, Region: apimodel.RegionCNBeijing,
				Source: apimodel.SourceOverride,
			},
			APIKey: key,
		}); err != nil {
			t.Fatalf("存凭证失败: %v", err)
		}
	}

	put("ark-first-key-333333333333")
	if _, err := svc.CreateConversation(ctx, ws, agentA); err != nil {
		t.Fatalf("第一次开对话失败: %v", err)
	}
	firstAgents := runner.createdAgents

	// Chat again with the same key: must not rebuild.
	if _, err := svc.CreateConversation(ctx, ws, agentA); err != nil {
		t.Fatalf("第二次开对话失败: %v", err)
	}
	if runner.createdAgents != firstAgents {
		t.Errorf("同一把 key 不该重建方舟 Agent（%d → %d）", firstAgents, runner.createdAgents)
	}

	// Change the key: must rebuild.
	put("ark-second-key-4444444444444")
	if _, err := svc.CreateConversation(ctx, ws, agentA); err != nil {
		t.Fatalf("换 key 后开对话失败: %v", err)
	}
	if runner.createdAgents != firstAgents+1 {
		t.Errorf("换了 key 必须重建方舟 Agent（旧的属于旧 project），"+
			"实际 createdAgents=%d 期望 %d", runner.createdAgents, firstAgents+1)
	}
}

// Existing bindings (without a fingerprint) must keep being reused; we must not rebuild everyone's
// resources at the moment of upgrade.
//
// ⚠️ If this fails: after the upgrade, **Ark resources of every existing workspace get rebuilt**,
// and duplicate Environment names hit 409, so nobody can start a new chat right away. This is the
// most dangerous way this change can break, so it is pinned separately.
func TestLegacyBindingWithoutFingerprintIsReused(t *testing.T) {
	svc, store, runner, ws, agentA, agentB := arkKeyFixture(t)
	ctx := context.Background()

	if err := store.PutArkSecret(ctx, apimodel.ArkSecret{
		ArkCredential: apimodel.ArkCredential{
			WorkspaceID: ws, Region: apimodel.RegionCNBeijing,
			Source: apimodel.SourceOverride,
		},
		APIKey: "ark-legacy-space-key-5555555",
	}); err != nil {
		t.Fatalf("存凭证失败: %v", err)
	}

	// Hand-write a binding **without KeyFingerprint**, simulating data written before the upgrade.
	if err := store.PutArkBinding(ctx, persistmodel.ArkBinding{
		WorkspaceID:      ws,
		AgentID:          agentA,
		ArkAgentID:       "ark-agent-legacy",
		ArkEnvironmentID: "env-legacy",
		CreatedAt:        apimodel.NowISO(),
		// KeyFingerprint is deliberately left empty: that is the shape of existing rows.
	}); err != nil {
		t.Fatalf("写存量绑定失败: %v", err)
	}

	// agent A: hits the existing binding, should not rebuild anything.
	if _, err := svc.CreateConversation(ctx, ws, agentA); err != nil {
		t.Fatalf("agent A 开对话失败: %v", err)
	}
	if runner.createdEnvs != 0 || runner.createdAgents != 0 {
		t.Errorf("存量绑定必须原样复用，实际建了 %d 个环境、%d 个 Agent",
			runner.createdEnvs, runner.createdAgents)
	}

	// agent B: creates a new binding, but must **reuse the existing environment** (same key, the old record just has no fingerprint).
	if _, err := svc.CreateConversation(ctx, ws, agentB); err != nil {
		t.Fatalf("agent B 开对话失败: %v", err)
	}
	if runner.createdEnvs != 0 {
		t.Errorf("应当复用存量环境 env-legacy，实际又建了 %d 个", runner.createdEnvs)
	}
	bindB, err := store.GetArkBinding(ctx, ws, agentB)
	if err != nil {
		t.Fatalf("读 agent B 绑定失败: %v", err)
	}
	if bindB.ArkEnvironmentID != "env-legacy" {
		t.Errorf("agent B 该复用 env-legacy，实际是 %q", bindB.ArkEnvironmentID)
	}
}

// Two basic properties of ArkKeyFingerprint: no plaintext, and an empty key gives an empty string.
func TestArkKeyFingerprint(t *testing.T) {
	const plain = "ark-secret-value-must-not-leak"
	fp := ArkKeyFingerprint(plain)
	if fp == "" {
		t.Fatal("非空 key 的指纹不该是空串")
	}
	if strings.Contains(fp, plain) {
		t.Errorf("指纹里不能出现明文：%q", fp)
	}
	// The same key must produce the same result every time (it is used for equality checks).
	if ArkKeyFingerprint(plain) != fp {
		t.Error("指纹必须是确定性的，否则每次都判成「换了 key」")
	}
	// Different keys give different fingerprints.
	if ArkKeyFingerprint("ark-another-value-here-0000") == fp {
		t.Error("不同的 key 不该有相同指纹")
	}
	// An empty key gives an empty string: callers must be able to tell "no key" from "a key".
	for _, empty := range []string{"", "   "} {
		if got := ArkKeyFingerprint(empty); got != "" {
			t.Errorf("空 key 的指纹该是空串，实际 %q", got)
		}
	}
}
