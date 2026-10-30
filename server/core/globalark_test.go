// Tests for the "configure once globally, reference it from workspaces" mechanism.
//
// This file watches four things; when any of them is wrong it **reports no error**, the behavior
// is just quietly wrong:
//
//	inherit reads the global value              — wrong shows up as "referenced, but says no key is configured"
//	change global, inheriting workspaces follow — wrong shows up as "changed global, no effect"; this is the core need
//	override is not affected by global          — wrong shows up as "someone changed a key and broke my workspace"
//	existing data keeps working as it is        — wrong shows up as "after the upgrade every workspace stops working"
//
// The last one is the most dangerous, so it has its own test, which deliberately builds a row
// **without a source field**.

package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/repository/dao/coremem"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
)

// prefsCtx builds a ctx with an identity.
//
// The function with the same name in preferences_test.go is **a separate copy**: that file moved
// to the external test package (package core_test, because it belongs to the contract suite that
// builds dao.PGStore), and the internal package cannot reach it. For a three-line helper, a copy
// is cheaper than building a bridge for it.
func prefsCtx(email string) context.Context {
	return auth.WithIdentity(context.Background(), auth.Identity{Email: email})
}

const (
	arkOwner = "alice@example.com"
	arkOther = "bob@example.com"
)

// globalKey builds a fake key with a valid length (≥16).
func globalKey(tag string) string {
	return "ARKGLOBAL" + strings.Repeat("0", 16) + tag
}

// TestInheritReadsGlobalValue checks that an inheriting workspace reads the global value.
func TestInheritReadsGlobalValue(t *testing.T) {
	svc, _ := newService(t)
	ctx := prefsCtx(arkOwner)
	ws := seedWorkspaceID(t, svc, ctx)

	key := globalKey("AAAA")
	if _, err := svc.SaveGlobalArkCredential(ctx, ArkCredentialDraft{
		APIKey: key, Region: apimodel.RegionCNBeijing,
	}); err != nil {
		t.Fatalf("存全局凭证失败：%v", err)
	}

	// Reference explicitly: **there is no silent inheritance**, this step is required.
	if _, err := svc.UseGlobalArkCredential(ctx, ws); err != nil {
		t.Fatalf("引用全局凭证失败：%v", err)
	}

	got, err := svc.GetArkCredential(ctx, ws)
	if err != nil {
		t.Fatalf("读空间凭证失败：%v", err)
	}
	if got.Source != apimodel.SourceInherit {
		t.Errorf("source 是 %q，期望 inherit", got.Source)
	}
	if got.InheritFrom != arkOwner {
		t.Errorf("inheritFrom 是 %q，期望 %q", got.InheritFrom, arkOwner)
	}
	// ⚠️ The mask must be the global key's, **not an empty string**: an empty string makes the
	// frontend decide "not configured" and show the "enter API Key" empty state, while this
	// workspace actually works.
	if got.ApiKeyMasked != maskSecret(key) {
		t.Errorf("❌ 掩码是 %q，期望全局那把 key 的掩码 %q——"+
			"空串会让界面显示成「未配置」，用户会以为引用没生效然后再贴一遍 key",
			got.ApiKeyMasked, maskSecret(key))
	}

	// The plaintext must also resolve, otherwise starting a chat reports "please configure an Ark API Key first".
	sec, err := svc.resolveArkSecret(ctx, ws)
	if err != nil {
		t.Fatalf("解析明文失败：%v", err)
	}
	if sec.APIKey != key {
		t.Errorf("❌ 解析出的明文是 %q，期望 %q", sec.APIKey, key)
	}
}

// TestChangingGlobalPropagatesToInheritingWorkspace is **the core need of this feature**.
//
// Change the global one, and the workspaces referencing it must follow, with no sync step at all.
//
// If this fails, most likely someone changed the reference into "copy into the workspace". That
// change looks more direct (one less lookup when reading), but it breaks propagation entirely, and
// **nothing in the UI shows it**: the workspace settings page still says "from global settings",
// only the key is already stale.
func TestChangingGlobalPropagatesToInheritingWorkspace(t *testing.T) {
	svc, _ := newService(t)
	ctx := prefsCtx(arkOwner)
	ws := seedWorkspaceID(t, svc, ctx)

	oldKey := globalKey("OLDK")
	if _, err := svc.SaveGlobalArkCredential(ctx, ArkCredentialDraft{
		APIKey: oldKey, Region: apimodel.RegionCNBeijing,
	}); err != nil {
		t.Fatalf("存全局凭证失败：%v", err)
	}
	if _, err := svc.UseGlobalArkCredential(ctx, ws); err != nil {
		t.Fatalf("引用失败：%v", err)
	}

	// Switch to a new global key, **and change the region** (the region should follow too).
	newKey := globalKey("NEWK")
	if _, err := svc.SaveGlobalArkCredential(ctx, ArkCredentialDraft{
		APIKey: newKey, Region: apimodel.RegionAPSoutheast1,
	}); err != nil {
		t.Fatalf("换全局凭证失败：%v", err)
	}

	// **Nothing was done to the workspace**, and it should already see the new value.
	got, err := svc.GetArkCredential(ctx, ws)
	if err != nil {
		t.Fatalf("读空间凭证失败：%v", err)
	}
	if got.ApiKeyMasked != maskSecret(newKey) {
		t.Errorf("❌ 改全局后空间仍显示旧掩码 %q，期望 %q——"+
			"「改全局自动联动」是这个功能的核心诉求。多半是引用被改成了"+
			"「复制一份到空间」，那样改全局永远不会生效，且界面上看不出来",
			got.ApiKeyMasked, maskSecret(newKey))
	}
	if got.Region != apimodel.RegionAPSoutheast1 {
		t.Errorf("❌ 区域没跟着变：%q，期望 ap-southeast-1", got.Region)
	}

	// The plaintext matters even more: that is what chats actually use.
	sec, err := svc.resolveArkSecret(ctx, ws)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if sec.APIKey != newKey {
		t.Errorf("❌ 对话链路拿到的仍是旧 key——用户换了 key 但 agent 还在用旧的，"+
			"表现是「换完 key 还是 401」。得到 %q，期望 %q", sec.APIKey, newKey)
	}
}

// TestOverrideIsNotAffectedByGlobal checks that a customized workspace is not affected by global.
//
// The opposite direction matters just as much: once a workspace has its own key, it must not
// change when someone (or the owner) changes global, otherwise "customized for this workspace" is
// fake.
func TestOverrideIsNotAffectedByGlobal(t *testing.T) {
	svc, _ := newService(t)
	ctx := prefsCtx(arkOwner)
	ws := seedWorkspaceID(t, svc, ctx)

	ownKey := globalKey("OWNK")
	if _, err := svc.SaveArkCredential(ctx, ws, ArkCredentialDraft{
		APIKey: ownKey, Region: apimodel.RegionCNBeijing,
	}); err != nil {
		t.Fatalf("存空间凭证失败：%v", err)
	}

	// Configure a completely different global key.
	if _, err := svc.SaveGlobalArkCredential(ctx, ArkCredentialDraft{
		APIKey: globalKey("GLOB"), Region: apimodel.RegionAPSoutheast1,
	}); err != nil {
		t.Fatalf("存全局凭证失败：%v", err)
	}

	got, err := svc.GetArkCredential(ctx, ws)
	if err != nil {
		t.Fatalf("读失败：%v", err)
	}
	if got.Source != apimodel.SourceOverride {
		t.Errorf("source 是 %q，期望 override", got.Source)
	}
	if got.ApiKeyMasked != maskSecret(ownKey) {
		t.Errorf("❌ 自定义的空间被全局影响了：掩码是 %q，期望 %q",
			got.ApiKeyMasked, maskSecret(ownKey))
	}
	if got.Region != apimodel.RegionCNBeijing {
		t.Errorf("❌ 区域被全局那份顶掉了：%q", got.Region)
	}

	sec, err := svc.resolveArkSecret(ctx, ws)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if sec.APIKey != ownKey {
		t.Errorf("❌ 明文被全局那份顶掉了：得到 %q，期望 %q", sec.APIKey, ownKey)
	}
}

// TestLegacyRowWithoutSourceKeepsWorking is **the one for existing data**.
//
// # Why build this row by hand
//
// ark_secrets rows stored before the upgrade have no source key in doc at all, which deserializes
// to an empty string. Rows built through SaveArkCredential carry source=override, so **this shape
// cannot be tested that way**. So here a record with an empty Source is put straight into Store,
// simulating those old rows.
//
// If the empty string landed on the inherit side, after the upgrade **every configured workspace
// would suddenly switch to the global key**, without anyone touching any switch. Most likely the
// global one does not even exist, so chats in all those workspaces would report "please configure
// an Ark API Key first" at once. That is exactly the shape of a past incident of this kind.
func TestLegacyRowWithoutSourceKeepsWorking(t *testing.T) {
	svc, store := newService(t)
	ctx := prefsCtx(arkOwner)
	ws := seedWorkspaceID(t, svc, ctx)

	legacyKey := globalKey("LEGA")
	// Hand-insert a row **without a Source field** (zero-value empty string), like those stored before the upgrade.
	if err := store.PutArkSecret(ctx, apimodel.ArkSecret{
		ArkCredential: apimodel.ArkCredential{
			WorkspaceID:  ws,
			ApiKeyMasked: maskSecret(legacyKey),
			Region:       apimodel.RegionCNBeijing,
			Check:        apimodel.CheckUnchecked,
			// Source is deliberately left empty: this is what existing rows look like.
		},
		APIKey: legacyKey,
	}); err != nil {
		t.Fatalf("塞存量行失败：%v", err)
	}

	// Even if a global one exists (quite likely, since the same person may configure global later), it must not affect this.
	if _, err := svc.SaveGlobalArkCredential(ctx, ArkCredentialDraft{
		APIKey: globalKey("GLOB"), Region: apimodel.RegionAPSoutheast1,
	}); err != nil {
		t.Fatalf("存全局凭证失败：%v", err)
	}

	got, err := svc.GetArkCredential(ctx, ws)
	if err != nil {
		t.Fatalf("读存量行失败：%v", err)
	}
	if got.Source != apimodel.SourceOverride {
		t.Errorf("❌ 存量行（没有 source 键）被判成了 %q，期望 override——"+
			"空串必须收敛到现状，否则升级后所有已配置的空间会突然改用全局那把 key，"+
			"而没有任何人点过任何开关", got.Source)
	}
	if got.ApiKeyMasked != maskSecret(legacyKey) {
		t.Errorf("❌ 存量行读到的不是自己那把 key：%q", got.ApiKeyMasked)
	}

	// The chat path must also get the old key.
	sec, err := svc.resolveArkSecret(ctx, ws)
	if err != nil {
		t.Fatalf("解析存量行失败：%v", err)
	}
	if sec.APIKey != legacyKey {
		t.Errorf("❌ 存量空间的对话会用错 key：得到 %q，期望 %q", sec.APIKey, legacyKey)
	}
}

// TestGlobalArkIsPerAccount checks that A's global credential is neither readable by nor affects B.
func TestGlobalArkIsPerAccount(t *testing.T) {
	svc, _ := newService(t)

	aliceKey := globalKey("ALIC")
	if _, err := svc.SaveGlobalArkCredential(prefsCtx(arkOwner), ArkCredentialDraft{
		APIKey: aliceKey, Region: apimodel.RegionCNBeijing,
	}); err != nil {
		t.Fatalf("存 alice 的全局凭证失败：%v", err)
	}

	// bob never configured one, so he must get an empty shell, never alice's.
	bob, err := svc.GetGlobalArkCredential(prefsCtx(arkOther))
	if err != nil {
		t.Fatalf("读 bob 的全局凭证失败：%v", err)
	}
	if bob.ApiKeyMasked != "" {
		t.Errorf("❌ bob 看到了 alice 的凭证掩码 %q——账号级凭证串号了", bob.ApiKeyMasked)
	}

	// After bob configures his own, alice's is unaffected.
	if _, err := svc.SaveGlobalArkCredential(prefsCtx(arkOther), ArkCredentialDraft{
		APIKey: globalKey("BOBB"), Region: apimodel.RegionAPSoutheast1,
	}); err != nil {
		t.Fatalf("存 bob 的全局凭证失败：%v", err)
	}
	alice, err := svc.GetGlobalArkCredential(prefsCtx(arkOwner))
	if err != nil {
		t.Fatalf("回读 alice 失败：%v", err)
	}
	if alice.ApiKeyMasked != maskSecret(aliceKey) {
		t.Errorf("❌ bob 的保存改掉了 alice 的凭证：%q", alice.ApiKeyMasked)
	}
	if alice.Region != apimodel.RegionCNBeijing {
		t.Errorf("❌ bob 的区域串到了 alice：%q", alice.Region)
	}
}

// TestUseGlobalRequiresGlobalToExist checks that you cannot reference global when it is not
// configured.
//
// If allowed, the user clicks "use global", the UI shows success, and then starting a chat reports
// "credential does not exist". At that point they neither know where to configure it nor whether
// the reference took effect.
func TestUseGlobalRequiresGlobalToExist(t *testing.T) {
	svc, _ := newService(t)
	ctx := prefsCtx(arkOwner)
	ws := seedWorkspaceID(t, svc, ctx)

	_, err := svc.UseGlobalArkCredential(ctx, ws)
	if !errors.Is(err, ErrPrecondition) {
		t.Fatalf("没配全局就引用得到 %v，期望 ErrPrecondition", err)
	}
	if !strings.Contains(err.Error(), "全局设置") {
		t.Errorf("报错没指出该去哪配：%q", err.Error())
	}
}

// TestSaveOwnKeySwitchesBackToOverride checks that saving your own key switches from inherit back
// to override.
//
// This is the requirement "a workspace can still choose to customize". Without switching back, the
// user saves a key and it has no effect, because the read path still falls back to global.
func TestSaveOwnKeySwitchesBackToOverride(t *testing.T) {
	svc, _ := newService(t)
	ctx := prefsCtx(arkOwner)
	ws := seedWorkspaceID(t, svc, ctx)

	if _, err := svc.SaveGlobalArkCredential(ctx, ArkCredentialDraft{
		APIKey: globalKey("GLOB"), Region: apimodel.RegionCNBeijing,
	}); err != nil {
		t.Fatalf("存全局失败：%v", err)
	}
	if _, err := svc.UseGlobalArkCredential(ctx, ws); err != nil {
		t.Fatalf("引用失败：%v", err)
	}

	ownKey := globalKey("MINE")
	if _, err := svc.SaveArkCredential(ctx, ws, ArkCredentialDraft{
		APIKey: ownKey, Region: apimodel.RegionCNBeijing,
	}); err != nil {
		t.Fatalf("贴自己的 key 失败：%v", err)
	}

	got, err := svc.GetArkCredential(ctx, ws)
	if err != nil {
		t.Fatalf("读失败：%v", err)
	}
	if got.Source != apimodel.SourceOverride {
		t.Errorf("❌ 贴了自己的 key 之后 source 仍是 %q，期望 override", got.Source)
	}
	sec, err := svc.resolveArkSecret(ctx, ws)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if sec.APIKey != ownKey {
		t.Errorf("❌ 贴了自己的 key 却没生效，读路径还在回落到全局：得到 %q", sec.APIKey)
	}
}

// TestVerifyInheritDoesNotCopyKeyIntoWorkspace watches **the most hidden way to break this**.
//
// With inherit, the sec obtained during verification carries the global plaintext. If it were
// written back with PutArkSecret while at it, this workspace would get its own copy, and
// **changing global would never propagate again**, while the UI still says "from global
// settings". No error, looks right at the moment, and only days later do you find "why did
// changing global have no effect".
func TestVerifyInheritDoesNotCopyKeyIntoWorkspace(t *testing.T) {
	// We need a verifier that really returns a verdict: the one from newService has no verifier, and
	// VerifyArkCredential would return right at the "no verifier configured" step, never reaching
	// the write-back path.
	store, _ := coremem.NewSeededMemStore()
	svc, err := New(Config{Store: store, ArkVerifier: &fakeVerifier{}})
	if err != nil {
		t.Fatalf("建 Service 失败：%v", err)
	}
	ctx := prefsCtx(arkOwner)
	ws := seedWorkspaceID(t, svc, ctx)

	oldKey := globalKey("OLDV")
	if _, err := svc.SaveGlobalArkCredential(ctx, ArkCredentialDraft{
		APIKey: oldKey, Region: apimodel.RegionCNBeijing,
	}); err != nil {
		t.Fatalf("存全局失败：%v", err)
	}
	if _, err := svc.UseGlobalArkCredential(ctx, ws); err != nil {
		t.Fatalf("引用失败：%v", err)
	}

	// Verify once (the fake verifier says valid).
	if _, verr := svc.VerifyArkCredential(ctx, ws); verr != nil {
		t.Fatalf("校验失败：%v", verr)
	}

	// **The workspace row must still have no plaintext**.
	row, rerr := store.GetArkSecret(ctx, ws)
	if rerr != nil {
		t.Fatalf("读空间行失败：%v", rerr)
	}
	if row.APIKey != "" {
		t.Errorf("❌ 校验把全局那把明文复制进了空间行——引用机制被悄悄破坏了，"+
			"改全局从此不再联动，而界面上仍显示「来自全局设置」。得到 %q", row.APIKey)
	}

	// Propagation still holds: change global, and the workspace follows.
	newKey := globalKey("NEWV")
	if _, err := svc.SaveGlobalArkCredential(ctx, ArkCredentialDraft{
		APIKey: newKey, Region: apimodel.RegionCNBeijing,
	}); err != nil {
		t.Fatalf("换全局失败：%v", err)
	}
	sec, err := svc.resolveArkSecret(ctx, ws)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if sec.APIKey != newKey {
		t.Errorf("❌ 校验之后联动坏了：得到 %q，期望 %q", sec.APIKey, newKey)
	}
}

// TestDeletingGlobalLeavesInheritingWorkspaceWithGuidance checks the behavior after global is
// deleted.
//
// **No cascade** (see Service for why), so that workspace becomes "references a credential that
// does not exist". Requirements: it reads as not configured, but source is still inherit (so the UI
// gives the right guidance), and the error when starting a chat points at global settings, not
// workspace settings.
func TestDeletingGlobalLeavesInheritingWorkspaceWithGuidance(t *testing.T) {
	svc, _ := newService(t)
	ctx := prefsCtx(arkOwner)
	ws := seedWorkspaceID(t, svc, ctx)

	if _, err := svc.SaveGlobalArkCredential(ctx, ArkCredentialDraft{
		APIKey: globalKey("GONE"), Region: apimodel.RegionCNBeijing,
	}); err != nil {
		t.Fatalf("存全局失败：%v", err)
	}
	if _, err := svc.UseGlobalArkCredential(ctx, ws); err != nil {
		t.Fatalf("引用失败：%v", err)
	}
	if err := svc.DeleteGlobalArkCredential(ctx); err != nil {
		t.Fatalf("删全局失败：%v", err)
	}

	got, err := svc.GetArkCredential(ctx, ws)
	if err != nil {
		t.Fatalf("读失败：%v", err)
	}
	if got.ApiKeyMasked != "" {
		t.Errorf("全局删了之后掩码应为空，得到 %q", got.ApiKeyMasked)
	}
	// ⚠️ source must honestly be inherit: if it were judged override, the UI would say "customized
	// for this workspace, not configured" and guide the user to save a workspace key, while what
	// they most likely want is to reconfigure global.
	if got.Source != apimodel.SourceInherit {
		t.Errorf("❌ 全局删了之后 source 变成了 %q，期望仍是 inherit——"+
			"否则界面会给错引导", got.Source)
	}

	// The error when starting a chat must point at global settings.
	_, err = svc.arkKey(ctx, ws, "agent_x")
	if !errors.Is(err, ErrPrecondition) {
		t.Fatalf("期望 ErrPrecondition，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "全局设置") {
		t.Errorf("❌ 报错没指向全局设置，用户会按错的引导走一遍：%q", err.Error())
	}
}

// seedWorkspaceID creates a workspace and returns its id.
func seedWorkspaceID(t *testing.T, svc *Service, ctx context.Context) string {
	t.Helper()
	ws, err := svc.CreateWorkspace(ctx, WorkspaceInput{Name: "凭证测试", Key: "cred-test"})
	if err != nil {
		t.Fatalf("建空间失败：%v", err)
	}
	return ws.ID
}

// TestConversationPathResolvesInheritedKey checks that **the chat path** also goes through
// reference resolution.
//
// # Why this is written separately instead of relying on the ones above
//
// The settings page reads GetArkCredential, and chat reads arkKey: **the two paths are separate**.
// If arkKey skipped the resolveArkSecret call, an inheriting workspace would get a credential with
// an empty APIKey, and then **without any error** call Ark with it. Ark returns 401, and the user
// sees "Ark rejected this API Key", while the key on the settings page is perfectly fine.
//
// This bug really happened (when this mechanism was written, arkKey was in chat.go and got
// missed). The "deleted global gives guidance" test above can hit it, but its error points at the
// guidance text, not at "the chat got an empty key". So this one asserts the plaintext directly.
func TestConversationPathResolvesInheritedKey(t *testing.T) {
	svc, _ := newService(t)
	ctx := prefsCtx(arkOwner)
	ws := seedWorkspaceID(t, svc, ctx)

	key := globalKey("CONV")
	if _, err := svc.SaveGlobalArkCredential(ctx, ArkCredentialDraft{
		APIKey: key, Region: apimodel.RegionAPSoutheast1,
	}); err != nil {
		t.Fatalf("存全局失败：%v", err)
	}
	if _, err := svc.UseGlobalArkCredential(ctx, ws); err != nil {
		t.Fatalf("引用失败：%v", err)
	}

	bind, err := svc.arkKey(ctx, ws, "agent_x")
	if err != nil {
		t.Fatalf("❌ 对话链路取凭据失败：%v——"+
			"inherit 的空间在设置页上显示着一把可用的 key，对话却说没配", err)
	}
	if bind.APIKey != key {
		t.Errorf("❌ 对话链路拿到的明文是 %q，期望 %q——"+
			"空串会被原样发给方舟，回 401，用户看到「方舟拒绝了这把 API Key」，"+
			"而设置页上那把 key 明明是好的", bind.APIKey, key)
	}
	if bind.Region != apimodel.RegionAPSoutheast1 {
		t.Errorf("❌ 区域没跟着全局那份走：%q——会打到错误的方舟端点", bind.Region)
	}
}
