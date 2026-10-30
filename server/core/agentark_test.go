// Fallback order and invariants of the three-level Ark credentials (agent → workspace → account).
//
// # What this group of tests guards
//
// The two most likely mistakes with three-level configuration both **report no error**:
//
//	wrong fallback order   some agent's chat runs on another key (billed to someone else), and it works
//	lower level keeps copy changing the upper level no longer propagates, while the UI still shows "follow workspace settings"
//
// Both are only found days later during billing reconciliation, so they must be pinned down.
//
// All offline (MemStore + fake verifier), because what we test is our own resolution logic.

package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/repository/dao/coremem"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	arkcredsvc "github.com/chyroc/ama/server/service/arkcred"
)

// threeTierFixture builds "one workspace + one agent + one logged-in identity".
func threeTierFixture(t *testing.T) (*Service, *coremem.MemStore, context.Context, string, string) {
	t.Helper()
	store := coremem.NewMemStore()
	svc, err := New(Config{Store: store})
	if err != nil {
		t.Fatalf("建 Service 失败: %v", err)
	}
	// Attach an identity: account-level credentials are stored and loaded by actor.
	ctx := auth.WithIdentity(context.Background(), auth.Identity{
		Email: "me@example.com", Name: "我",
	})

	ws, err := store.CreateWorkspace(ctx, apimodel.Workspace{
		ID: "ws_tier", Name: "三级", Key: "tier",
		Members: []apimodel.WorkspaceMember{{
			ID: "m1", Email: "me@example.com", Role: apimodel.RoleOwner,
		}},
	})
	if err != nil {
		t.Fatalf("建空间失败: %v", err)
	}
	agent, err := store.CreateAgent(ctx, apimodel.Agent{
		ID: "agent_tier", WorkspaceID: ws.ID, Name: "a",
		Model:        apimodel.ModelConfig{ID: "doubao-pro"},
		BuiltinTools: []string{}, SkillIDs: []string{}, FeishuAppIDs: []string{},
		Status: apimodel.StatusActive,
	})
	if err != nil {
		t.Fatalf("建 agent 失败: %v", err)
	}
	return svc, store, ctx, ws.ID, agent.ID
}

// Three key constants: the assertions must make it obvious which level was resolved.
const (
	keyAgentLevel  = "ark-agent-level-key-000000001"
	keySpaceLevel  = "ark-space-level-key-000000002"
	keyGlobalLevel = "ark-global-level-key-00000003"
)

func putGlobal(t *testing.T, svc *Service, ctx context.Context, key string) {
	t.Helper()
	if _, err := svc.SaveGlobalArkCredential(ctx, ArkCredentialDraft{
		APIKey: key, Region: apimodel.RegionCNBeijing,
	}); err != nil {
		t.Fatalf("存账号级凭证失败: %v", err)
	}
}

func putSpace(t *testing.T, svc *Service, ctx context.Context, ws, key string) {
	t.Helper()
	if _, err := svc.SaveArkCredential(ctx, ws, ArkCredentialDraft{
		APIKey: key, Region: apimodel.RegionCNBeijing,
	}); err != nil {
		t.Fatalf("存空间级凭证失败: %v", err)
	}
}

func putAgent(t *testing.T, svc *Service, ctx context.Context, agentID, key string) {
	t.Helper()
	if _, err := svc.SaveAgentArkCredential(ctx, agentID, ArkCredentialDraft{
		APIKey: key, Region: apimodel.RegionCNBeijing,
	}); err != nil {
		t.Fatalf("存 agent 级凭证失败: %v", err)
	}
}

// ⭐ **Fallback order: agent → workspace → account; the more specific, the higher priority.**
//
// This is the core contract of the whole three-level configuration. A wrong order shows up as
// "some agent's chat used another key": it works, it is just billed to someone else, and nothing
// in the UI shows it.
func TestThreeTierFallbackOrder(t *testing.T) {
	svc, store, ctx, ws, agentID := threeTierFixture(t)

	// With all three levels configured, it should resolve to the most specific one.
	putGlobal(t, svc, ctx, keyGlobalLevel)
	putSpace(t, svc, ctx, ws, keySpaceLevel)
	putAgent(t, svc, ctx, agentID, keyAgentLevel)

	must := func(stage, wantKey string, wantLevel apimodel.ArkLevel) {
		t.Helper()
		sec, err := svc.resolveArkSecretForAgent(ctx, ws, agentID)
		if err != nil {
			t.Fatalf("%s：解析失败 %v", stage, err)
		}
		if sec.APIKey != wantKey {
			t.Errorf("%s：应当解析到 %q，实际 %q", stage, wantKey, sec.APIKey)
		}
		// The effective source must match too: the UI uses it to tell the user "which level is in use now".
		eff := svc.arkEffective(ctx, ws, agentID)
		if eff.Level != wantLevel {
			t.Errorf("%s：生效级别应当是 %q，实际 %q", stage, wantLevel, eff.Level)
		}
	}

	must("三级都配", keyAgentLevel, apimodel.LevelAgent)

	// Remove the agent level → fall back to the workspace level.
	if err := svc.DeleteAgentArkCredential(ctx, agentID); err != nil {
		t.Fatalf("删 agent 级失败: %v", err)
	}
	must("去掉 agent 级", keySpaceLevel, apimodel.LevelWorkspace)

	// Change the workspace to reference the account level → fall back to global.
	if _, err := svc.UseGlobalArkCredential(ctx, ws); err != nil {
		t.Fatalf("空间引用全局失败: %v", err)
	}
	must("空间引用全局", keyGlobalLevel, apimodel.LevelGlobal)

	// None of the three levels → ErrNotFound (callers use it to show "please configure first").
	if err := svc.DeleteGlobalArkCredential(ctx); err != nil {
		t.Fatalf("删账号级失败: %v", err)
	}
	if _, err := svc.resolveArkSecretForAgent(ctx, ws, agentID); !errors.Is(err, ErrNotFound) {
		t.Errorf("三级都没配应当回 ErrNotFound，实际 %v", err)
	}
	if eff := svc.arkEffective(ctx, ws, agentID); eff.Level != "" {
		t.Errorf("三级都没配时生效级别应当是空串，实际 %q", eff.Level)
	}
	_ = store
}

// An agent that "follows the workspace" **must not keep a copy**; that is the whole
// implementation of propagation.
//
// ⚠️ With a copy, changing the workspace-level key no longer propagates, while the UI still shows
// that agent as "follow workspace settings". This is the most hidden way the three-level
// mechanism breaks: no error, looks right at the moment, and only days later do you find the
// change did not take effect.
func TestFollowingAgentKeepsNoCopy(t *testing.T) {
	svc, store, ctx, ws, agentID := threeTierFixture(t)
	putSpace(t, svc, ctx, ws, keySpaceLevel)

	// This agent was never given a key, so the agent table must not have a row for it.
	if _, err := store.GetAgentArkSecret(ctx, agentID); !errors.Is(err, ErrNotFound) {
		t.Errorf("跟随空间的 agent 不该有自己的行，实际 err=%v", err)
	}

	// Read the view once (this step used to be the high-risk spot for "write a copy while at it").
	cred, err := svc.GetAgentArkCredential(ctx, agentID)
	if err != nil {
		t.Fatalf("读 agent 凭证视图失败: %v", err)
	}
	if cred.Custom {
		t.Error("没配过的 agent 的 custom 应当是 false")
	}
	if cred.Effective.Level != apimodel.LevelWorkspace {
		t.Errorf("生效级别应当是 workspace，实际 %q", cred.Effective.Level)
	}
	if _, err := store.GetAgentArkSecret(ctx, agentID); !errors.Is(err, ErrNotFound) {
		t.Error("读一次视图之后 agent 侧出现了副本 —— 联动会静默失效")
	}

	// **Change the workspace-level key; agents that follow it must get the new value right away.**
	const rotated = "ark-space-rotated-key-0000009"
	putSpace(t, svc, ctx, ws, rotated)
	sec, err := svc.resolveArkSecretForAgent(ctx, ws, agentID)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if sec.APIKey != rotated {
		t.Errorf("改了空间级 key，跟随它的 agent 应当立刻用上新的：想要 %q 得到 %q",
			rotated, sec.APIKey)
	}
}

// Verify must write the result back to **the workspace row** when "following the workspace", and
// must not leave a copy on the agent side.
//
// Same shape as TestVerifyInheritDoesNotCopyKeyIntoWorkspace: that one guards workspace→global,
// this one guards agent→workspace. The same break, one level more.
func TestVerifyFollowingDoesNotCopyKeyIntoAgent(t *testing.T) {
	store := coremem.NewMemStore()
	svc, err := New(Config{Store: store, ArkVerifier: okArkVerifier{}})
	if err != nil {
		t.Fatalf("建 Service 失败: %v", err)
	}
	ctx := auth.WithIdentity(context.Background(), auth.Identity{Email: "me@example.com"})
	if _, err := store.CreateWorkspace(ctx, apimodel.Workspace{
		ID: "ws_v", Name: "v", Key: "v",
		Members: []apimodel.WorkspaceMember{{ID: "m1", Email: "me@example.com", Role: apimodel.RoleOwner}},
	}); err != nil {
		t.Fatalf("建空间失败: %v", err)
	}
	if _, err := store.CreateAgent(ctx, apimodel.Agent{
		ID: "agent_v", WorkspaceID: "ws_v", Name: "a",
		Model:        apimodel.ModelConfig{ID: "doubao-pro"},
		BuiltinTools: []string{}, SkillIDs: []string{}, FeishuAppIDs: []string{},
		Status: apimodel.StatusActive,
	}); err != nil {
		t.Fatalf("建 agent 失败: %v", err)
	}
	putSpace(t, svc, ctx, "ws_v", keySpaceLevel)

	cred, err := svc.VerifyAgentArkCredential(ctx, "agent_v")
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if cred.Effective.Check != apimodel.CheckValid {
		t.Errorf("校验结果应当是 valid，实际 %q", cred.Effective.Check)
	}
	// **No row may appear on the agent side because of this.**
	if _, err := store.GetAgentArkSecret(ctx, "agent_v"); !errors.Is(err, ErrNotFound) {
		t.Error("校验把上级的 key 复制进 agent 行了 —— 改上级从此不联动，且界面看不出来")
	}
	// The result must be written back to the workspace row.
	if row, err := store.GetArkSecret(ctx, "ws_v"); err != nil {
		t.Fatalf("读空间行失败: %v", err)
	} else if row.Check != apimodel.CheckValid {
		t.Errorf("校验结果应当写回空间那一行，实际 %q", row.Check)
	}
}

// An agent that follows the workspace cannot change the region: the region is a property of the
// key, and it uses the upper level's key.
//
// Allowing it would write a record with "a region but no key", while resolution still falls
// back: the user changes it and nothing happens, with no error. Same handling as
// UpdateArkRegion with inherit.
func TestUpdateAgentRegionRejectedWhenFollowing(t *testing.T) {
	svc, store, ctx, ws, agentID := threeTierFixture(t)
	putSpace(t, svc, ctx, ws, keySpaceLevel)

	_, err := svc.UpdateAgentArkRegion(ctx, agentID, apimodel.RegionAPSoutheast1)
	if !errors.Is(err, ErrPrecondition) {
		t.Errorf("跟随空间时改区域应当被拒（ErrPrecondition），实际 %v", err)
	}
	// No row may be written because of this.
	if _, err := store.GetAgentArkSecret(ctx, agentID); !errors.Is(err, ErrNotFound) {
		t.Error("被拒的改区域仍然写出了一行「有区域没有 key」的记录")
	}
}

// Plaintext must never go into the public view: the type has no such field; here we pin the serialized result.
func TestAgentArkCredentialNeverLeaksPlaintext(t *testing.T) {
	svc, _, ctx, _, agentID := threeTierFixture(t)
	putAgent(t, svc, ctx, agentID, keyAgentLevel)

	cred, err := svc.GetAgentArkCredential(ctx, agentID)
	if err != nil {
		t.Fatalf("读凭证视图失败: %v", err)
	}
	raw, err := json.Marshal(cred)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	blob := string(raw)
	if strings.Contains(blob, keyAgentLevel) {
		t.Errorf("对外视图里出现了明文 key：%s", blob)
	}
	// The mask must be present (the frontend uses it to tell "is it configured").
	if cred.ApiKeyMasked == "" {
		t.Error("掩码不该是空串")
	}
	if !cred.Custom {
		t.Error("配过 key 的 agent 的 custom 应当是 true")
	}
}

// When an agent is deleted, its own key must be deleted with it.
//
// Agent ids are generated by an in-process counter, so delete-and-recreate may hit the same id.
// The new agent would then **silently inherit** the previous agent's credential: it works, just
// billed to someone else, and the UI does not show it.
func TestDeleteAgentDropsItsArkSecret(t *testing.T) {
	svc, store, ctx, _, agentID := threeTierFixture(t)
	putAgent(t, svc, ctx, agentID, keyAgentLevel)

	if err := store.DeleteAgent(ctx, agentID); err != nil {
		t.Fatalf("删 agent 失败: %v", err)
	}
	if _, err := store.GetAgentArkSecret(ctx, agentID); !errors.Is(err, ErrNotFound) {
		t.Error("删 agent 之后它那把 key 还在 —— 同 id 的新 agent 会静默继承它")
	}
}

// okArkVerifier is a fake verifier that always says "this key is usable".
type okArkVerifier struct{}

func (okArkVerifier) Verify(context.Context, string, apimodel.ArkRegion) arkcredsvc.VerifyResult {
	return arkcredsvc.VerifyResult{Status: arkcredsvc.VerifyStatusOK}
}

// User A cannot change user B's personal key: **overreach cannot be expressed**.
//
// The test is not "the caller was verified", but "you cannot name someone else": the actor only
// comes from the context, and no email appears in the path or request body. This test pins that
// invariant: **switch the identity and you read a different credential**; no input exists that
// can point at someone else's.
//
// ⚠️ This tests the Service layer. The real wiring (forged request headers have no effect) is
// guarded by the identity tests on the httpd side. Both are needed: a unit test that stuffs the
// context cannot prove Protect is in the chain.
func TestGlobalArkCredentialIsPerActor(t *testing.T) {
	store := coremem.NewMemStore()
	svc, err := New(Config{Store: store})
	if err != nil {
		t.Fatalf("建 Service 失败: %v", err)
	}

	ctxA := auth.WithIdentity(context.Background(), auth.Identity{Email: "a@example.com"})
	ctxB := auth.WithIdentity(context.Background(), auth.Identity{Email: "b@example.com"})

	const keyA = "ark-belongs-to-a-00000000001"
	const keyB = "ark-belongs-to-b-00000000002"
	putGlobal(t, svc, ctxA, keyA)
	putGlobal(t, svc, ctxB, keyB)

	// Each reads their own: A cannot read B's key.
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"A 读到自己的", ctxA, keyA},
		{"B 读到自己的", ctxB, keyB},
	} {
		// Go through the Service's public view (that is what the endpoint really returns).
		cred, err := svc.GetGlobalArkCredential(tc.ctx)
		if err != nil {
			t.Fatalf("%s：读失败 %v", tc.name, err)
		}
		if cred.ApiKeyMasked != maskSecret(tc.want) {
			t.Errorf("%s：应当读到自己那把的掩码 %q，实际 %q",
				tc.name, maskSecret(tc.want), cred.ApiKeyMasked)
		}
	}

	// A deletes their own; **it must not affect B**.
	if err := svc.DeleteGlobalArkCredential(ctxA); err != nil {
		t.Fatalf("A 删自己的失败: %v", err)
	}
	credB, err := svc.GetGlobalArkCredential(ctxB)
	if err != nil {
		t.Fatalf("B 读失败: %v", err)
	}
	if credB.ApiKeyMasked != maskSecret(keyB) {
		t.Error("A 删自己的凭证影响到了 B —— 那是跨用户的数据破坏")
	}
	// A's own should now be empty.
	credA, err := svc.GetGlobalArkCredential(ctxA)
	if err != nil {
		t.Fatalf("A 读失败: %v", err)
	}
	if credA.ApiKeyMasked != "" {
		t.Errorf("A 删完之后还读到 %q", credA.ApiKeyMasked)
	}
}
