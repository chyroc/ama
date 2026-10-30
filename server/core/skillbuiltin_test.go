// Tests on the core side for platform built-in skills: they need the newChatService setup
// (create conversation, bind Ark Agent, check permissions), so they stay in core.
//
// # What these tests guard against is "failure with zero errors"
//
// The built-in skill chain has one thing in common: **whichever link breaks, nothing reports
// an error**.
//
//   - Not shown in the list → users think the platform lacks the ability and never report a bug;
//   - Shown but not attached to the Ark Agent → arkSkillsFor only logs a Warn,
//     and users see "I ticked the skill, but the model says it cannot use it";
//   - Authorization lookup returns an empty string → everyone can edit built-in skills, with
//     not a single log line.
//
// So these tests always assert the **final state** (what is really attached to the Ark Agent,
// what is really stored in the database), not "some function was called".
//
// ⚠️ **The two pure-function tests (built-in skills are valid themselves, the lark-cli body does
// not leak internal error codes) have moved with service/skill**; they need no setup. Do not
// write another copy here.
package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

// TestBuiltinSkillsVisibleInEveryWorkspace checks that built-in skills are listed in
// **every** workspace.
//
// That is the whole point of "platform built-in". It checks two workspaces, and the second
// one is newly created: checking only the seed workspace would miss a bug like "the merge
// logic is hard-wired to the seed workspace".
func TestBuiltinSkillsVisibleInEveryWorkspace(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newChatService(t)

	other, err := svc.CreateWorkspace(ctx, WorkspaceInput{
		Name: "另一个空间", Key: "other-ws",
	})
	if err != nil {
		t.Fatalf("建空间失败：%v", err)
	}

	for _, ws := range []string{seedWS, other.ID} {
		list, err := svc.ListSkills(ctx, ws)
		if err != nil {
			t.Fatalf("列 %s 的技能失败：%v", ws, err)
		}
		for _, want := range []string{BuiltinSkillIDLarkCLI, BuiltinSkillIDAMA} {
			if !hasSkillID(list, want) {
				t.Errorf("空间 %s 的技能列表里应当有内置技能 %s，实际只有 %v",
					ws, want, skillIDsOf(list))
			}
		}
	}

	// A new workspace should have nothing but the built-in skills (it has no skills of its own).
	// This also proves the merge did not bring in skills from other workspaces.
	list, err := svc.ListSkills(ctx, other.ID)
	if err != nil {
		t.Fatalf("列技能失败：%v", err)
	}
	if len(list) != len(BuiltinSkills()) {
		t.Errorf("新空间应当只有 %d 个内置技能，实际 %d 个：%v",
			len(BuiltinSkills()), len(list), skillIDsOf(list))
	}
}

// TestBuiltinSkillAttachedToArkAgent is the most important one: after ticking, **it is really
// attached on the Ark side**.
//
// It asserts runner.spec.Skills (the final content passed to Ark when creating the Agent),
// not "UploadSkill was called". A missing attachment gives zero errors; only checking the
// final state catches it.
func TestBuiltinSkillAttachedToArkAgent(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)
	bindSkills(t, store, seedChatAgent, BuiltinSkillIDLarkCLI)

	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("开对话失败：%v", err)
	}

	if len(runner.spec.Skills) != 1 {
		t.Fatalf("建方舟 Agent 时应当带上那个内置技能，实际 %d 个：%+v",
			len(runner.spec.Skills), runner.spec.Skills)
	}
	if runner.spec.Skills[0].ArkSkillID != "skill-ark-1" {
		t.Errorf("挂上去的应当是上传返回的 id，实际 %q", runner.spec.Skills[0].ArkSkillID)
	}

	// The uploaded package must be readable by our own parser, and **the body must really be in
	// it**. Asserting only "it was uploaded" would pass with an empty package, and Ark does not
	// validate the format or report errors.
	if len(runner.uploadedSkills) != 1 {
		t.Fatalf("应当上传 1 个技能包，实际 %d 个", len(runner.uploadedSkills))
	}
	up := runner.uploadedSkills[0]
	pkg, err := ParseSkillPackage(up.fileName, up.zip)
	if err != nil {
		t.Fatalf("上传的包必须是合法技能包：%v", err)
	}
	if pkg.Name != "lark-cli" {
		t.Errorf("包里的技能名应当是 lark-cli，实际 %q", pkg.Name)
	}
	// The body must contain the "do not run auth login" warning: that command blocks forever in
	// the container (tested in shared/infra/shelltools), and leaving it out makes the agent hang
	// a whole batch of tool calls.
	if !strings.Contains(pkg.Instructions, "auth login") {
		t.Error("lark-cli 正文里应当警告不要跑 auth login（容器里会永久卡住）")
	}
	// Zero attachments: the CLI is preinstalled in the container, so no files need to be written.
	if len(pkg.Files) != 0 {
		t.Errorf("内置技能应当零附件，实际 %d 个：%+v", len(pkg.Files), pkg.Files)
	}

	// The binding must record the fingerprint and the Ark skill id.
	b, err := store.GetArkBinding(ctx, seedWS, seedChatAgent)
	if err != nil {
		t.Fatalf("取绑定失败：%v", err)
	}
	if b.SkillsFingerprint == "" {
		t.Error("绑定里应当记下技能指纹，否则下次没法判断要不要更新")
	}
	if len(b.ArkSkillIDs) != 1 || b.ArkSkillIDs[0] != "skill-ark-1" {
		t.Errorf("绑定里应当记下方舟技能 id（供排查），实际 %v", b.ArkSkillIDs)
	}
}

// TestBuiltinSkillNotReuploadedAcrossAgents checks idempotency: when two agents in the same
// workspace each tick the skill, the skill package is uploaded only once.
//
// Ark has no delete API, so every extra upload is garbage that can **never** be cleaned up.
// fakeRunner.UploadSkill returns a new id each time (chat_test.go does this on purpose),
// so a repeated upload is caught here.
func TestBuiltinSkillNotReuploadedAcrossAgents(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	second, err := svc.CreateAgent(ctx, seedWS, apimodel.AgentDraft{
		Name:     "第二个 agent",
		Model:    apimodel.ModelConfig{ID: "doubao-seed-1.6"},
		SkillIDs: []string{BuiltinSkillIDLarkCLI},
	})
	if err != nil {
		t.Fatalf("建第二个 agent 失败：%v", err)
	}
	bindSkills(t, store, seedChatAgent, BuiltinSkillIDLarkCLI)

	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("开第一条对话失败：%v", err)
	}
	if _, err := svc.CreateConversation(ctx, seedWS, second.ID); err != nil {
		t.Fatalf("开第二条对话失败：%v", err)
	}

	if len(runner.uploadedSkills) != 1 {
		t.Fatalf("同一份内容只该传一次，实际传了 %d 次——方舟侧那些副本删不掉",
			len(runner.uploadedSkills))
	}
	// Both agents must get the **same** skill_id (reusing that upload record).
	if got := runner.spec.Skills; len(got) != 1 || got[0].ArkSkillID != "skill-ark-1" {
		t.Errorf("第二个 agent 应当复用同一个 skill_id，实际 %+v", got)
	}
}

// TestBuiltinSkillImmutable checks that users can neither edit nor delete built-in skills.
//
// ⚠️ The DeleteSkill part matters most: if it took the idempotent "deleting something missing
// is not an error" path, it would silently succeed and **remove every agent's binding** to
// it. The skill would still be in the list, but nobody's tick would remain, and there would be
// no error at all.
func TestBuiltinSkillImmutable(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)
	bindSkills(t, store, seedChatAgent, BuiltinSkillIDLarkCLI)

	_, err := svc.UpdateSkill(ctx, BuiltinSkillIDLarkCLI, apimodel.SkillDraft{
		Name: "lark-cli", DisplayTitle: "改个名", Description: "d", Instructions: "i",
	})
	if !errors.Is(err, ErrPrecondition) {
		t.Errorf("改内置技能应当回 ErrPrecondition，实际 %v", err)
	}

	if err := svc.DeleteSkill(ctx, BuiltinSkillIDLarkCLI); !errors.Is(err, ErrPrecondition) {
		t.Errorf("删内置技能应当回 ErrPrecondition，实际 %v", err)
	}

	// After the failed delete the binding must **still be there**. This is the silent failure
	// described in the comment above.
	a, err := store.GetAgent(ctx, seedChatAgent)
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	if !contains(a.SkillIDs, BuiltinSkillIDLarkCLI) {
		t.Error("删除被拒之后，agent 上的绑定不该被解掉")
	}

	// The body was not changed: GetSkill still returns the one from the constant.
	sk, err := svc.GetSkill(ctx, BuiltinSkillIDLarkCLI)
	if err != nil {
		t.Fatalf("取内置技能失败：%v", err)
	}
	if sk.DisplayTitle == "改个名" {
		t.Error("内置技能的正文被改掉了")
	}
	if sk.Source != apimodel.SkillBuiltin {
		t.Errorf("内置技能的 source 应当是 builtin，实际 %q", sk.Source)
	}
}

// TestBuiltinSkillWorkspaceOfIsPlatformResource checks that the authz reverse lookup for a
// built-in skill returns ErrPlatformResource.
//
// ⚠️ **Returning an empty string causes fail-open**: authz/guard.go lets the request through
// when workspaceID == "", RoleOf is never called, so any logged-in user could PATCH / DELETE
// built-in skills, and logDenied records nothing: privilege escalation with zero errors. This
// test pins that down.
func TestBuiltinSkillWorkspaceOfIsPlatformResource(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newChatService(t)

	ws, err := svc.WorkspaceOf(ctx, KindSkill, BuiltinSkillIDLarkCLI)
	if !errors.Is(err, ErrPlatformResource) {
		t.Errorf("内置技能应当回 ErrPlatformResource，实际 err=%v", err)
	}
	if ws != "" {
		t.Errorf("平台级资源不该给出空间，实际 %q", ws)
	}
	// The other way round: a normal skill must still resolve to its workspace; do not treat all
	// skills as platform-level.
	ws, err = svc.WorkspaceOf(ctx, KindSkill, "skill-20260801-triage")
	if err != nil {
		t.Fatalf("普通技能反解失败：%v", err)
	}
	if ws != seedWS {
		t.Errorf("普通技能应当反解出 %s，实际 %q", seedWS, ws)
	}
}

// TestBuiltinSkillNameCannotBeReused checks that users cannot create a skill with the same
// name as a built-in skill.
//
// Built-in skills are visible in every workspace. A name clash would put two lark-cli entries
// in the list, and in the binding UI you cannot tell them apart by name.
func TestBuiltinSkillNameCannotBeReused(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newChatService(t)

	_, err := svc.CreateSkill(ctx, seedWS, apimodel.SkillDraft{
		Name: "lark-cli", DisplayTitle: "我自己的", Description: "d", Instructions: "i",
	})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("与内置技能撞名应当回 ErrConflict，实际 %v", err)
	}
}

// TestBuiltinSkillCountsTowardArkLimit checks that built-in skills also count toward Ark's
// limit of 20.
//
// The limit must **return an error, not truncate**: truncating would make the 21st silently
// stop working, and users would see 21 bindings in the UI with only 20 taking effect.
func TestBuiltinSkillCountsTowardArkLimit(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	// First create 19 workspace skills; adding 2 built-in ones makes exactly 21.
	ids := make([]string, 0, maxArkSkillsPerAgent+1)
	for i := 0; i < maxArkSkillsPerAgent-1; i++ {
		sk, err := svc.CreateSkill(ctx, seedWS, apimodel.SkillDraft{
			Name:         "filler-" + string(rune('a'+i/26)) + string(rune('a'+i%26)),
			DisplayTitle: "填充", Description: "d", Instructions: "i",
		})
		if err != nil {
			t.Fatalf("建第 %d 个技能失败：%v", i, err)
		}
		ids = append(ids, sk.ID)
	}
	ids = append(ids, BuiltinSkillIDLarkCLI, BuiltinSkillIDLarkCLI)
	bindSkills(t, store, seedChatAgent, ids...)

	_, err := svc.CreateConversation(ctx, seedWS, seedChatAgent)
	if !errors.Is(err, ErrPrecondition) {
		t.Errorf("绑了 %d 个技能应当回 ErrPrecondition（而不是悄悄截断），实际 %v",
			len(ids), err)
	}
}

// hasSkillID reports whether the list contains a given skill id.
func hasSkillID(list []apimodel.Skill, id string) bool {
	for _, sk := range list {
		if sk.ID == id {
			return true
		}
	}
	return false
}

// skillIDsOf extracts all ids, only so they can be printed in error messages.
func skillIDsOf(list []apimodel.Skill) []string {
	out := make([]string, 0, len(list))
	for _, sk := range list {
		out = append(out, sk.ID)
	}
	return out
}

// editableSkills filters out platform built-in skills and keeps only the workspace's own.
//
// ListSkills puts built-in skills first, so **every test that takes "any skill" by index must
// go through this first**. Otherwise it gets a built-in skill and fails on "cannot be
// modified", which has nothing to do with what those tests want to check.
func editableSkills(t *testing.T, list []apimodel.Skill) []apimodel.Skill {
	t.Helper()
	out := make([]apimodel.Skill, 0, len(list))
	for _, sk := range list {
		if !IsBuiltinSkill(sk.ID) {
			out = append(out, sk)
		}
	}
	if len(out) < 2 {
		t.Fatalf("种子里至少要有 2 条可改的技能，实际 %d 条", len(out))
	}
	return out
}

// firstEditableSkill returns the first skill created in the workspace.
func firstEditableSkill(t *testing.T, list []apimodel.Skill) apimodel.Skill {
	t.Helper()
	return editableSkills(t, list)[0]
}
