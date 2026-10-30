// Tests for the Service layer: one test per invariant.
//
// These invariants come from LocalApiClient in the frontend's web/src/data/api.ts. Most of
// them are not "obviously correct" designs but existing behavior: a naive CRUD
// implementation would get every one of them wrong, and when wrong the page shows no error;
// the behavior just changes quietly. So each one needs a test pinning it down.

package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/repository/dao/coremem"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	arkcredsvc "github.com/chyroc/ama/server/service/arkcred"
)

// fakeVerifier is a programmable Ark verifier, so tests do not go over the network.
type fakeVerifier struct {
	result arkcredsvc.VerifyResult
	// When results is non-empty, entries are dequeued one per call in order (for tests where
	// the same Service must return different results at different stages, like "OK on save,
	// fails when clicking verify").
	results    []arkcredsvc.VerifyResult
	calls      int
	lastKey    string
	lastRegion apimodel.ArkRegion
}

func (f *fakeVerifier) Verify(_ context.Context, apiKey string, region apimodel.ArkRegion) arkcredsvc.VerifyResult {
	f.calls++
	f.lastKey = apiKey
	f.lastRegion = region
	if len(f.results) > 0 {
		r := f.results[0]
		f.results = f.results[1:]
		return r
	}
	// The zero value means "usable": &fakeVerifier{} is a verifier that always says OK,
	// so existing tests do not each need to set a result explicitly.
	if f.result.Status == "" {
		return arkcredsvc.VerifyResult{Status: arkcredsvc.VerifyStatusOK}
	}
	return f.result
}

// newService builds a Service **with object storage wired in**, used by most tests.
//
// ⚠️ **Since step 4a object storage is no longer optional, so it is wired in by default.**
// It used to be left out on purpose (`Config{Store: store}`), because back then "no object
// storage configured" was a normal setup: bytes fell back into the `skill_files.content`
// column. Step 4a removed that fallback: no file_id means an error, so any test that
// **uploads skill files** fails without blobs.
//
// That failure is correct (production would fail too), but it **cannot test what those tests
// want to test**: `TestScanMarksAlreadyInstalled` wants to test "imported ones are marked
// installed", but would die on "object storage not configured", an error that does not point
// at what it cares about. So a stand-in is wired in by default, letting those tests stay on
// their own topic.
//
// Tests that want to check "should fail when object storage is not configured" use
// newServiceWithoutBlobs. That is a **deliberate** setup and must be stated in the test.
func newService(t *testing.T) (*Service, *coremem.MemStore) {
	t.Helper()
	svc, store := newServiceWithoutBlobs(t)
	svc.blobs = newFakeBlobStore()
	return svc, store
}

// newServiceWithoutBlobs builds a Service **without object storage**.
//
// Only for tests that "deliberately check behavior without object storage configured" (since
// step 4a that is a failure setup; see the ErrPrecondition branches in putSkillFileBlobs and
// hydrateSkillFiles). Do not use it as the default: unrelated tests would die on "object
// storage not configured", which is not their topic.
func newServiceWithoutBlobs(t *testing.T) (*Service, *coremem.MemStore) {
	t.Helper()
	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败：%v", err)
	}
	svc, err := New(Config{Store: store})
	if err != nil {
		t.Fatalf("装配 Service 失败：%v", err)
	}
	return svc, store
}

const seedWS = "ws_demo"

// --------------------------------------------------------------------------
// Workspaces and members
// --------------------------------------------------------------------------

// TestCreateWorkspaceSeedsOwner checks that creating a workspace adds an owner automatically.
// The members page's "cannot remove the only owner" and the Ark credential's updatedBy both
// depend on it.
func TestCreateWorkspaceSeedsOwner(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	ws, err := svc.CreateWorkspace(ctx, WorkspaceInput{Name: "新空间", Key: "brand-new"})
	if err != nil {
		t.Fatalf("建空间失败：%v", err)
	}
	if len(ws.Members) != 1 {
		t.Fatalf("应自动塞一个成员，实际 %d", len(ws.Members))
	}
	m := ws.Members[0]
	if m.Role != apimodel.RoleOwner {
		t.Errorf("自动塞的成员应是 owner，实际 %s", m.Role)
	}
	// The default identity must match the person hard-coded in LocalApiClient, otherwise e2e
	// assertions fail
	if m.Email != "alice@example.com" {
		t.Errorf("默认 owner 邮箱与前端不一致：%s", m.Email)
	}
	if !strings.HasPrefix(ws.ID, "ws_") {
		t.Errorf("空间 ID 应以 ws_ 开头（前缀是可观测契约）：%s", ws.ID)
	}
}

// TestCreateWorkspaceOwnerIsLoggedInUser checks that the owner of a new workspace must be **the
// person currently logged in**.
//
// The previous test checks the fallback "when ctx has no identity" (only reached in unit
// tests and AMA_STORE=mem). In production every API sits behind the auth middleware, so ctx
// always has auth.Identity, and the owner should be the logged-in person, not that
// hard-coded name. Hard-coding it would make every workspace belong to the same person, with
// **no error**: the page looks fine, just with a stranger sitting in the member list.
func TestCreateWorkspaceOwnerIsLoggedInUser(t *testing.T) {
	svc, _ := newService(t)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{
		Email: "lisi@example.com",
		Name:  "李四",
	})

	ws, err := svc.CreateWorkspace(ctx, WorkspaceInput{Name: "我的空间", Key: "mine"})
	if err != nil {
		t.Fatalf("建空间失败：%v", err)
	}
	m := ws.Members[0]
	if m.Email != "lisi@example.com" {
		t.Errorf("owner 邮箱应是登录者，实际 %s", m.Email)
	}
	if m.Name != "李四" {
		t.Errorf("owner 名字应是登录者，实际 %s", m.Name)
	}
	if m.Role != apimodel.RoleOwner {
		t.Errorf("应是 owner，实际 %s", m.Role)
	}
}

// TestCreateWorkspaceOwnerNameFallsBackToEmail: the login provider (OIDC) sometimes gives no
// display name (only an email); then use the email as the name, do not leave it empty.
// An empty name would put a row with "nothing in it" in the member list.
func TestCreateWorkspaceOwnerNameFallsBackToEmail(t *testing.T) {
	svc, _ := newService(t)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{Email: "wangwu@example.com"})

	ws, err := svc.CreateWorkspace(ctx, WorkspaceInput{Name: "无名", Key: "noname"})
	if err != nil {
		t.Fatalf("建空间失败：%v", err)
	}
	if got := ws.Members[0].Name; got != "wangwu@example.com" {
		t.Errorf("没有展示名时应回落到邮箱，实际 %q", got)
	}
}

// TestUpdateWorkspaceSameKeyOK checks that saving your own key back unchanged is not a
// conflict. Otherwise every settings save would collide with itself.
func TestUpdateWorkspaceSameKeyOK(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	ws, _ := svc.GetWorkspaceByKey(ctx, "growth")
	name := "改个名字"
	key := ws.Key
	if _, err := svc.UpdateWorkspace(ctx, ws.ID, WorkspacePatch{Name: &name, Key: &key}); err != nil {
		t.Errorf("原样存回自己的 key 报错了：%v", err)
	}
}

// TestUpdateWorkspaceDescriptionCleared checks that a missing description key means clearing it.
//
// When the frontend clears the description, the body it sends has no such key
// (trim() || undefined is dropped by JSON.stringify), while LocalApiClient uses
// Object.assign, which copies undefined in and really clears it. So the HTTP side must read
// "missing key" as clearing, otherwise the two clients behave differently.
func TestUpdateWorkspaceDescriptionCleared(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	ws, _ := svc.GetWorkspaceByKey(ctx, "growth")
	if ws.Description == "" {
		t.Fatal("种子里这个空间本该有描述")
	}
	name := ws.Name
	// Send only name, not description
	got, err := svc.UpdateWorkspace(ctx, ws.ID, WorkspacePatch{Name: &name})
	if err != nil {
		t.Fatalf("更新失败：%v", err)
	}
	if got.Description != "" {
		t.Errorf("缺 description 键应清空，实际还是 %q", got.Description)
	}
}

// TestLastOwnerGuards: the two guards have **different** messages; do not merge them into one
// for DRY.
func TestLastOwnerGuards(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	ws, _ := svc.CreateWorkspace(ctx, WorkspaceInput{Name: "单人空间", Key: "solo"})
	owner := ws.Members[0]

	_, err := svc.UpdateMemberRole(ctx, ws.ID, owner.ID, apimodel.RoleViewer)
	if err == nil {
		t.Fatal("把唯一 owner 降级应该被拦住")
	}
	if !strings.Contains(err.Error(), "空间必须保留至少一个拥有者") {
		t.Errorf("改角色的文案不对：%v", err)
	}

	err = svc.RemoveMember(ctx, ws.ID, owner.ID)
	if err == nil {
		t.Fatal("移除唯一 owner 应该被拦住")
	}
	if !strings.Contains(err.Error(), "不能移除唯一的拥有者") {
		t.Errorf("移除成员的文案不对：%v", err)
	}
}

// TestOwnerGuardOnlyForLastOwner: with two owners, demoting one of them is allowed;
// demoting a non-owner is always allowed.
func TestOwnerGuardOnlyForLastOwner(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	ws, _ := svc.CreateWorkspace(ctx, WorkspaceInput{Name: "双 owner", Key: "duo"})
	second, err := svc.AddMember(ctx, ws.ID, MemberInput{
		Name: "另一个", Email: "other@example.com", Role: apimodel.RoleOwner,
	})
	if err != nil {
		t.Fatalf("加第二个 owner 失败：%v", err)
	}
	// Now there are two owners, so demoting one should be allowed
	if _, err := svc.UpdateMemberRole(ctx, ws.ID, second.ID, apimodel.RoleMember); err != nil {
		t.Errorf("有两个 owner 时降级其一应允许：%v", err)
	}
	// Demoting a non-owner is always allowed
	if _, err := svc.UpdateMemberRole(ctx, ws.ID, second.ID, apimodel.RoleViewer); err != nil {
		t.Errorf("降级非 owner 报错了：%v", err)
	}
}

// TestRemoveMissingMemberSilentlySucceeds: succeeds silently when the member does not exist,
// but reports "workspace does not exist" when the workspace does not exist. The three kinds of
// "not found" each behave differently.
func TestRemoveMissingMemberSilentlySucceeds(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	if err := svc.RemoveMember(ctx, seedWS, "m_不存在"); err != nil {
		t.Errorf("成员不存在应静默成功，实际报错：%v", err)
	}
	err := svc.RemoveMember(ctx, "ws_不存在", "m_1")
	if err == nil || !strings.Contains(err.Error(), "空间不存在") {
		t.Errorf("空间不存在应报「空间不存在」，实际：%v", err)
	}
	// When changing roles, both report "member does not exist"
	_, err = svc.UpdateMemberRole(ctx, "ws_不存在", "m_1", apimodel.RoleAdmin)
	if err == nil || !strings.Contains(err.Error(), "成员不存在") {
		t.Errorf("改角色时空间不存在也该报「成员不存在」，实际：%v", err)
	}
}

// TestAddMemberDuplicateEmail: email uniqueness is per workspace and case-sensitive.
func TestAddMemberDuplicateEmail(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	ws, _ := svc.GetWorkspaceByKey(ctx, "growth")
	existing := ws.Members[0].Email

	_, err := svc.AddMember(ctx, ws.ID, MemberInput{Name: "重复", Email: existing, Role: apimodel.RoleMember})
	if err == nil || !strings.Contains(err.Error(), "已经是该空间成员") {
		t.Errorf("重复邮箱应被拦住：%v", err)
	}
	// A different case should be **allowed** (the frontend does exact matching; do not
	// "helpfully" make it case-insensitive)
	upper := strings.ToUpper(existing)
	if upper != existing {
		if _, err := svc.AddMember(ctx, ws.ID, MemberInput{Name: "大写", Email: upper, Role: apimodel.RoleMember}); err != nil {
			t.Errorf("大小写不同的邮箱应被允许（前端是精确匹配）：%v", err)
		}
	}
}

// --------------------------------------------------------------------------
// Agent
// --------------------------------------------------------------------------

// TestUpdateAgentAlwaysBumpsVersion: every update adds 1, even an empty patch.
// Ark uses version for optimistic locking and the frontend sends it back, so incrementing is
// part of the API contract.
func TestUpdateAgentAlwaysBumpsVersion(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	a, err := svc.GetAgent(ctx, "agent_triage")
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	before := a.Version
	draft := draftFrom(a)

	for i := 1; i <= 2; i++ {
		got, err := svc.UpdateAgent(ctx, a.ID, draft)
		if err != nil {
			t.Fatalf("第 %d 次更新失败：%v", i, err)
		}
		if got.Version != before+i {
			t.Errorf("第 %d 次更新后 version 应是 %d，实际 %d", i, before+i, got.Version)
		}
	}
}

// TestDeleteSkillUnbindsWithoutBumping: deleting a skill must unbind it from all agents,
// but **must not** touch their version / updatedAt. Bumping them would make the frontend think
// someone edited the agent.
func TestDeleteSkillUnbindsWithoutBumping(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	// Find a skill that is referenced
	skills, _ := svc.ListSkills(ctx, seedWS)
	agents, _ := svc.ListAgents(ctx, seedWS)
	var target string
	before := map[string]apimodel.Agent{}
	for _, sk := range skills {
		for _, a := range agents {
			if contains(a.SkillIDs, sk.ID) {
				target = sk.ID
			}
		}
		if target != "" {
			break
		}
	}
	if target == "" {
		t.Fatal("种子里应该有被引用的技能")
	}
	for _, a := range agents {
		before[a.ID] = a
	}

	if err := svc.DeleteSkill(ctx, target); err != nil {
		t.Fatalf("删技能失败：%v", err)
	}

	after, _ := svc.ListAgents(ctx, seedWS)
	for _, a := range after {
		if contains(a.SkillIDs, target) {
			t.Errorf("agent %s 仍绑着已删的技能", a.ID)
		}
		old := before[a.ID]
		if a.Version != old.Version {
			t.Errorf("agent %s 的 version 被动了（%d → %d），删技能不该 bump",
				a.ID, old.Version, a.Version)
		}
		if a.UpdatedAt != old.UpdatedAt {
			t.Errorf("agent %s 的 updatedAt 被动了，删技能不该刷新", a.ID)
		}
	}
}

// TestUnbindFeishuAppNoBump: unbinding a Feishu app does not bump version either.
func TestUnbindFeishuAppNoBump(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	bound, err := svc.agents.AgentsBoundToFeishuApp(ctx, "fs_1")
	if err != nil {
		t.Fatalf("查绑定失败：%v", err)
	}
	if len(bound) == 0 {
		t.Fatal("种子里 fs_1 本该有绑定的 agent")
	}
	before, _ := svc.GetAgent(ctx, bound[0])

	if err := svc.agents.UnbindFeishuApp(ctx, "fs_1"); err != nil {
		t.Fatalf("解绑失败：%v", err)
	}
	after, _ := svc.GetAgent(ctx, bound[0])
	if contains(after.FeishuAppIDs, "fs_1") {
		t.Error("解绑后仍带着 fs_1")
	}
	if after.Version != before.Version {
		t.Errorf("解绑不该 bump version：%d → %d", before.Version, after.Version)
	}
	// After unbinding it can no longer be found
	if got, _ := svc.agents.AgentsBoundToFeishuApp(ctx, "fs_1"); len(got) != 0 {
		t.Errorf("解绑后仍查到 %v", got)
	}
}

// TestSkillVersionBump: latestVersion is a number in string form, and incrementing must be correct.
func TestSkillVersionBump(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	skills, _ := svc.ListSkills(ctx, seedWS)
	// **Take an editable skill**: ListSkills puts platform built-in skills first, and those
	// cannot be edited (core/skillbuiltin.go). Using skills[0] would get a built-in skill, and
	// this test would fail on "cannot be modified", which has nothing to do with the version
	// increment it wants to check.
	sk := firstEditableSkill(t, skills)
	before := sk.LatestVersion

	got, err := svc.UpdateSkill(ctx, sk.ID, apimodel.SkillDraft{
		Name: sk.Name, DisplayTitle: sk.DisplayTitle,
		Description: sk.Description, Instructions: sk.Instructions, Tags: sk.Tags,
	})
	if err != nil {
		t.Fatalf("更新技能失败：%v", err)
	}
	if got.LatestVersion == before {
		t.Errorf("latestVersion 没自增，仍是 %s", before)
	}
	if bumpVersion("3") != "4" {
		t.Errorf(`bumpVersion("3") 应是 "4"，实际 %q`, bumpVersion("3"))
	}
	// Bad data should not panic
	if got := bumpVersion("不是数字"); got != "1" {
		t.Errorf("脏版本号应重置为 1，实际 %q", got)
	}
}

// TestSkillNameUniquePerWorkspaceOnly: creating a skill checks duplicates per workspace;
// renaming **does not check**. This asymmetry is existing frontend behavior.
func TestSkillNameUniquePerWorkspaceOnly(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	skills, _ := svc.ListSkills(ctx, seedWS)
	// Same as TestSkillVersionBump: we need a skill created in the workspace; built-in skills
	// cannot be edited.
	editable := editableSkills(t, skills)
	taken := editable[0].Name

	_, err := svc.CreateSkill(ctx, seedWS, apimodel.SkillDraft{Name: taken})
	if err == nil || !strings.Contains(err.Error(), "在本空间已存在") {
		t.Errorf("建技能撞名应被拦住：%v", err)
	}

	// A rename collision is **allowed** (updateSkill does not check duplicates)
	other := editable[1]
	if _, err := svc.UpdateSkill(ctx, other.ID, apimodel.SkillDraft{
		Name: taken, DisplayTitle: other.DisplayTitle, Instructions: other.Instructions,
	}); err != nil {
		t.Errorf("改名撞车应被允许（前端 updateSkill 没查重）：%v", err)
	}
}

// --------------------------------------------------------------------------
// Ark credentials
// --------------------------------------------------------------------------

// TestArkCredentialEmptyShell: when not configured it returns an empty shell, not 404.
func TestArkCredentialEmptyShell(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	cred, err := svc.GetArkCredential(ctx, seedWS)
	if err != nil {
		t.Fatalf("未配置时不该报错：%v", err)
	}
	if cred.ApiKeyMasked != "" {
		t.Errorf("未配置时掩码应为空串，实际 %q", cred.ApiKeyMasked)
	}
	// region must be the default value, not an empty string, otherwise the frontend dropdown breaks
	if cred.Region != apimodel.RegionCNBeijing {
		t.Errorf("空壳的 region 应是 cn-beijing，实际 %q", cred.Region)
	}
	if cred.Check != apimodel.CheckUnchecked {
		t.Errorf("空壳的 check 应是 unchecked，实际 %q", cred.Check)
	}
}

// TestSaveArkCredentialMasks: after saving only the mask is returned; the plaintext never
// appears in the outward view.
func TestSaveArkCredentialMasks(t *testing.T) {
	ctx := context.Background()
	svc, store := newService(t)

	const key = "ARK0123456789abcdef0001"
	cred, err := svc.SaveArkCredential(ctx, seedWS, ArkCredentialDraft{
		APIKey: key, Region: apimodel.RegionCNBeijing,
	})
	if err != nil {
		t.Fatalf("保存凭证失败：%v", err)
	}
	if strings.Contains(cred.ApiKeyMasked, "23456789") {
		t.Error("掩码里漏出了明文中段")
	}
	if cred.Check != apimodel.CheckUnchecked {
		t.Error("刚保存的 key 应是 unchecked")
	}
	if cred.UpdatedBy == "" {
		t.Error("凭证类配置要留痕，updatedBy 不该为空")
	}
	// The plaintext really is stored (only in the Store)
	sec, err := store.GetArkSecret(ctx, seedWS)
	if err != nil || sec.APIKey != key {
		t.Error("明文没正确存下来")
	}
}

// TestMaskSecret: the masking algorithm must match the frontend byte for byte.
func TestMaskSecret(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"a", "*"},
		{"12345678", "********"}, // len ≤ 8 → same number of stars
		{"123456789", "1234*5678"[:4] + "*" + "6789"},
		{strings.Repeat("x", 24), "xxxx" + strings.Repeat("*", 16) + "xxxx"},
		{strings.Repeat("x", 40), "xxxx" + strings.Repeat("*", 16) + "xxxx"}, // caps at 24
	}
	for _, c := range cases {
		if got := maskSecret(c.in); got != c.want {
			t.Errorf("maskSecret(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
	// 9 chars: first 4 + 1 star + last 4
	if got := maskSecret("123456789"); got != "1234*6789" {
		t.Errorf(`maskSecret("123456789") = %q，期望 "1234*6789"`, got)
	}
}

// TestUpdateArkRegionResetsCheck: changing region must reset check and remove
// checkedAt/checkError, but **must not touch** updatedBy.
func TestUpdateArkRegionResetsCheck(t *testing.T) {
	ctx := context.Background()
	store, _ := coremem.NewSeededMemStore()
	fake := &fakeVerifier{}
	svc, _ := New(Config{Store: store, ArkVerifier: fake})

	_, err := svc.SaveArkCredential(ctx, seedWS, ArkCredentialDraft{
		APIKey: "ARK0123456789abcdef", Region: apimodel.RegionCNBeijing,
	})
	if err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	verified, err := svc.VerifyArkCredential(ctx, seedWS)
	if err != nil {
		t.Fatalf("校验失败：%v", err)
	}
	if verified.Check != apimodel.CheckValid || verified.CheckedAt == "" {
		t.Fatal("校验成功后应是 valid 且有 checkedAt")
	}
	updatedBy := verified.UpdatedBy

	got, err := svc.UpdateArkRegion(ctx, seedWS, apimodel.RegionAPSoutheast1)
	if err != nil {
		t.Fatalf("换区失败：%v", err)
	}
	if got.Check != apimodel.CheckUnchecked {
		t.Errorf("换区后应重置为 unchecked，实际 %q", got.Check)
	}
	if got.CheckedAt != "" {
		t.Errorf("换区后 checkedAt 应被删掉，实际 %q", got.CheckedAt)
	}
	if got.CheckError != "" {
		t.Errorf("换区后 checkError 应被删掉，实际 %q", got.CheckError)
	}
	if got.UpdatedBy != updatedBy {
		t.Errorf("换区不该动 updatedBy：%q → %q", updatedBy, got.UpdatedBy)
	}
}

// TestVerifyInvalidKeyIsNotAnError: an invalid key must be returned as 200 + check:"invalid",
// not as an HTTP error. The frontend has no try/catch, so throwing would become an unhandled
// rejection.
func TestVerifyInvalidKeyIsNotAnError(t *testing.T) {
	ctx := context.Background()
	store, _ := coremem.NewSeededMemStore()
	// The save must say OK (otherwise it cannot be stored at all); clicking "verify connectivity"
	// returns InvalidKey.
	fake := &fakeVerifier{results: []arkcredsvc.VerifyResult{
		{Status: arkcredsvc.VerifyStatusOK},
		{Status: arkcredsvc.VerifyStatusInvalidKey},
	}}
	svc, _ := New(Config{Store: store, ArkVerifier: fake})

	if _, err := svc.SaveArkCredential(ctx, seedWS, ArkCredentialDraft{
		APIKey: "ARK0123456789abcdef", Region: apimodel.RegionCNBeijing,
	}); err != nil {
		t.Fatalf("保存失败：%v", err)
	}

	got, err := svc.VerifyArkCredential(ctx, seedWS)
	if err != nil {
		t.Fatalf("key 无效不该返回错误，而应写进 check：%v", err)
	}
	if got.Check != apimodel.CheckInvalid {
		t.Errorf("应是 invalid，实际 %q", got.Check)
	}
	if got.CheckError == "" {
		t.Error("invalid 时要给用户看得懂的中文原因")
	}
}

// TestVerifyTransportErrorKeepsVerdict: a network failure does not change the verdict.
// "Could not verify" is not the same as "verification failed".
func TestVerifyTransportErrorKeepsVerdict(t *testing.T) {
	ctx := context.Background()
	store, _ := coremem.NewSeededMemStore()
	// Three calls: OK on save, OK on first verify, "could not verify" (Skipped) on second verify.
	fake := &fakeVerifier{results: []arkcredsvc.VerifyResult{
		{Status: arkcredsvc.VerifyStatusOK},
		{Status: arkcredsvc.VerifyStatusOK},
		{Status: arkcredsvc.VerifyStatusSkipped, Cause: errors.New("dial tcp: i/o timeout")},
	}}
	svc, _ := New(Config{Store: store, ArkVerifier: fake})

	if _, err := svc.SaveArkCredential(ctx, seedWS, ArkCredentialDraft{
		APIKey: "ARK0123456789abcdef", Region: apimodel.RegionCNBeijing,
	}); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	// First verify succeeds
	if _, err := svc.VerifyArkCredential(ctx, seedWS); err != nil {
		t.Fatalf("首次校验失败：%v", err)
	}
	// Third call (second verify click) has a network failure
	if _, err := svc.VerifyArkCredential(ctx, seedWS); err == nil {
		t.Fatal("传输故障应返回错误")
	}
	// The verdict should stay valid
	cur, _ := svc.GetArkCredential(ctx, seedWS)
	if cur.Check != apimodel.CheckValid {
		t.Errorf("传输故障不该改结论，应仍是 valid，实际 %q", cur.Check)
	}
}

// TestVerifyUsesStoredRegion: verification must use the stored region, not a hard-coded one.
func TestVerifyUsesStoredRegion(t *testing.T) {
	ctx := context.Background()
	store, _ := coremem.NewSeededMemStore()
	fake := &fakeVerifier{}
	svc, _ := New(Config{Store: store, ArkVerifier: fake})

	if _, err := svc.SaveArkCredential(ctx, seedWS, ArkCredentialDraft{
		APIKey: "ARK0123456789abcdef", Region: apimodel.RegionAPSoutheast1,
	}); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	if _, err := svc.VerifyArkCredential(ctx, seedWS); err != nil {
		t.Fatalf("校验失败：%v", err)
	}
	if fake.lastRegion != apimodel.RegionAPSoutheast1 {
		t.Errorf("应拿存下来的区域去验，实际 %q", fake.lastRegion)
	}
}

// TestVerifyWithoutCredential: without a credential it reports "please fill in the API Key
// first".
func TestVerifyWithoutCredential(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	_, err := svc.VerifyArkCredential(ctx, seedWS)
	if err == nil || !strings.Contains(err.Error(), "请先填写 API Key") {
		t.Errorf("应报「请先填写 API Key」，实际：%v", err)
	}
	// Same for changing region
	_, err = svc.UpdateArkRegion(ctx, seedWS, apimodel.RegionCNBeijing)
	if err == nil || !strings.Contains(err.Error(), "请先填写 API Key") {
		t.Errorf("换区也该报「请先填写 API Key」，实际：%v", err)
	}
}

// TestValidateArkAPIKey: validation messages must match the frontend exactly (e2e asserts the
// original strings).
func TestValidateArkAPIKey(t *testing.T) {
	if err := validateArkAPIKey(""); err == nil ||
		!strings.Contains(err.Error(), "API Key 不能为空") {
		t.Errorf("空 key 文案不对：%v", err)
	}
	if err := validateArkAPIKey("short"); err == nil ||
		!strings.Contains(err.Error(), "API Key 长度不足，请确认是否复制完整") {
		t.Errorf("短 key 文案不对：%v", err)
	}
	if err := validateArkAPIKey("ARK 0123456789abcdef"); err == nil ||
		!strings.Contains(err.Error(), "API Key 不应包含空格") {
		t.Errorf("带空格的 key 文案不对：%v", err)
	}
	if err := validateArkAPIKey("ARK0123456789abcdef"); err != nil {
		t.Errorf("合法 key 被拦了：%v", err)
	}
}

// draftFrom turns an agent into an equivalent draft, mimicking the frontend's toDraft.
func draftFrom(a apimodel.Agent) apimodel.AgentDraft {
	return apimodel.AgentDraft{
		Name: a.Name, Description: a.Description, System: a.System,
		Model: a.Model, BuiltinTools: a.BuiltinTools,
		SkillIDs: a.SkillIDs, FeishuAppIDs: a.FeishuAppIDs, Status: a.Status,
	}
}

// ---------------------------------------------------------------------------
// Exclusive 1:1 binding of Feishu apps
// ---------------------------------------------------------------------------

// TestFeishuAppBoundToSingleAgent: one Feishu app cannot be bound to two agents at the same time.
//
// This is the core test for this rule: in the seed, fs_1 belongs to agent_triage, and anyone
// else trying to take it must be rejected with ErrConflict. The error must name **the current
// holder** (users see names in the UI; given an ID they would have to look it up themselves).
func TestFeishuAppBoundToSingleAgent(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	// agent_reviewer has no app bound at first; have it try to take agent_triage's fs_1.
	reviewer, err := svc.GetAgent(ctx, "agent_reviewer")
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	draft := draftFrom(reviewer)
	draft.FeishuAppIDs = []string{"fs_1"}

	_, err = svc.UpdateAgent(ctx, reviewer.ID, draft)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("抢别人的飞书应用应返回 ErrConflict，实际：%v", err)
	}
	// The error must be shown to users as is, so it must include the holder's name.
	if !strings.Contains(err.Error(), "工单分诊助手") {
		t.Errorf("报错里应点出占用者的名字「工单分诊助手」，实际：%v", err)
	}

	// **The rejection must be complete**: "an error was reported but the binding was written
	// anyway" must not happen.
	after, _ := svc.GetAgent(ctx, "agent_reviewer")
	if len(after.FeishuAppIDs) != 0 {
		t.Errorf("被拒的更新不该留下绑定，实际：%v", after.FeishuAppIDs)
	}
	// The original holder is not affected.
	if bound, _ := svc.agents.AgentsBoundToFeishuApp(ctx, "fs_1"); len(bound) != 1 ||
		bound[0] != "agent_triage" {
		t.Errorf("fs_1 该仍然只归 agent_triage，实际：%v", bound)
	}
}

// TestCreateAgentRejectsTakenFeishuApp: creating an agent blocks taking an app too.
//
// It is a separate test because CreateAgent goes through a different validation entry
// (selfID is an empty string). Testing only UpdateAgent would miss the hole "it can be taken
// at creation time".
func TestCreateAgentRejectsTakenFeishuApp(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	_, err := svc.CreateAgent(ctx, seedWS, apimodel.AgentDraft{
		Name:         "抢别人应用的新 agent",
		Model:        apimodel.ModelConfig{ID: "doubao-seed-2-1-pro-260628"},
		FeishuAppIDs: []string{"fs_1"},
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("新建时抢占应返回 ErrConflict，实际：%v", err)
	}
	if bound, _ := svc.agents.AgentsBoundToFeishuApp(ctx, "fs_1"); len(bound) != 1 {
		t.Errorf("被拒的创建不该产生第二个绑定，实际：%v", bound)
	}
}

// TestUpdateAgentKeepsOwnFeishuApp: saving your own existing binding must not be blocked by
// yourself.
//
// This is the easiest one to get wrong: if validation does not exclude itself, the most
// common save, "just change the name and send the binding back unchanged", hits 409, which
// shows up as "this agent cannot be saved any more". The frontend sends back the full draft on
// every save, so every save goes through this path.
func TestUpdateAgentKeepsOwnFeishuApp(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	a, err := svc.GetAgent(ctx, "agent_triage")
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	if !contains(a.FeishuAppIDs, "fs_1") {
		t.Fatal("前提不成立：种子里 agent_triage 本该绑着 fs_1")
	}
	draft := draftFrom(a)
	draft.Name = "工单分诊助手（改名）"

	got, err := svc.UpdateAgent(ctx, a.ID, draft)
	if err != nil {
		t.Fatalf("保存自己已有的绑定不该失败：%v", err)
	}
	if !contains(got.FeishuAppIDs, "fs_1") {
		t.Errorf("保存后绑定丢了：%v", got.FeishuAppIDs)
	}
}

// TestFeishuAppRebindAfterUnbind: after the original holder unbinds, someone else can bind it.
//
// "Unbind first, then bind" is the path the error message tells users to take ("please
// unbind it over there first"), so it must really work, otherwise that hint is lying.
func TestFeishuAppRebindAfterUnbind(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	// The original holder unbinds: remove fs_1 from agent_triage.
	triage, _ := svc.GetAgent(ctx, "agent_triage")
	freeing := draftFrom(triage)
	freeing.FeishuAppIDs = []string{}
	if _, err := svc.UpdateAgent(ctx, triage.ID, freeing); err != nil {
		t.Fatalf("解绑失败：%v", err)
	}

	// Now agent_reviewer should be able to bind it.
	reviewer, _ := svc.GetAgent(ctx, "agent_reviewer")
	taking := draftFrom(reviewer)
	taking.FeishuAppIDs = []string{"fs_1"}
	if _, err := svc.UpdateAgent(ctx, reviewer.ID, taking); err != nil {
		t.Fatalf("原主人解绑后应该能绑上，实际：%v", err)
	}

	bound, _ := svc.agents.AgentsBoundToFeishuApp(ctx, "fs_1")
	if len(bound) != 1 || bound[0] != "agent_reviewer" {
		t.Errorf("fs_1 该只归 agent_reviewer，实际：%v", bound)
	}
}

// TestMemberChangesBumpUpdatedAt pins down "adding, removing, or changing members refreshes the
// workspace's updatedAt".
//
// # This one was also found by running the real service locally
//
// After UpdatedAt was added to apimodel.Workspace, the Store side wrote it correctly, but
// **none of the Service's four write paths refreshed it**, so after changing a member's role
// the updatedAt returned by the API was still the creation time. No error; the "last updated"
// column just never moved, looking as if the field was not wired up.
//
// The unit tests were all green at the time: they only asserted whether the role changed, and
// not one looked at updatedAt.
func TestMemberChangesBumpUpdatedAt(t *testing.T) {
	ctx := context.Background()
	svc, store := newService(t)

	ws, err := svc.CreateWorkspace(ctx, WorkspaceInput{Name: "时间戳空间", Key: "ts-ws"})
	if err != nil {
		t.Fatalf("建空间失败：%v", err)
	}
	if ws.UpdatedAt != ws.CreatedAt {
		t.Errorf("刚建好的空间 updatedAt 应等于 createdAt：%q vs %q", ws.UpdatedAt, ws.CreatedAt)
	}

	// apimodel.NowISO() only goes to milliseconds, so creating and changing within the same
	// millisecond gives the same string, and asserting "it changed" would be luck. Sleep past a
	// millisecond boundary before changing.
	time.Sleep(2 * time.Millisecond)

	added, err := svc.AddMember(ctx, ws.ID, MemberInput{
		Name: "新人", Email: "newbie@example.com", Role: apimodel.RoleMember,
	})
	if err != nil {
		t.Fatalf("加成员失败：%v", err)
	}
	afterAdd, err := store.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("取空间失败：%v", err)
	}
	if afterAdd.UpdatedAt == ws.UpdatedAt {
		t.Errorf("加成员没有刷新 updatedAt，仍是 %q", afterAdd.UpdatedAt)
	}

	time.Sleep(2 * time.Millisecond)
	if _, err := svc.UpdateMemberRole(ctx, ws.ID, added.ID, apimodel.RoleViewer); err != nil {
		t.Fatalf("改角色失败：%v", err)
	}
	afterRole, err := store.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("取空间失败：%v", err)
	}
	if afterRole.UpdatedAt == afterAdd.UpdatedAt {
		t.Errorf("改角色没有刷新 updatedAt，仍是 %q", afterRole.UpdatedAt)
	}

	time.Sleep(2 * time.Millisecond)
	if err := svc.RemoveMember(ctx, ws.ID, added.ID); err != nil {
		t.Fatalf("移除成员失败：%v", err)
	}
	afterRemove, err := store.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("取空间失败：%v", err)
	}
	if afterRemove.UpdatedAt == afterRole.UpdatedAt {
		t.Errorf("移除成员没有刷新 updatedAt，仍是 %q", afterRemove.UpdatedAt)
	}
}
