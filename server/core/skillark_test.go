// Tests for the chain that syncs skills to Ark.
//
// # What is tested here is "when to upload and when to update", not "what Ark received"
//
// The latter needs a real key + a real project (see the probe suggestion at the end of that
// section in core/README.md). This file uses fakeRunner to record upload and update calls, and
// asserts three things that can be tested offline and are the easiest to get wrong:
//
//  1. **Dedup**: the same content is uploaded only once. Ark has no delete API, so every extra upload is permanent garbage.
//  2. **Catch up**: when the binding already exists and the skill bindings change, the Ark side must update too. This is exactly the symptom being fixed.
//  3. **Degrade**: a failure on the Ark side must not stop the conversation from starting.
package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/repository/dao/coremem"

	"github.com/chyroc/ama/server/model/apimodel"
)

// seedChatAgent is the agent in the seed used for conversations, the same one as in chat_test.go.
const seedChatAgent = "agent_triage"

// seedSkillWithFiles attaches two files to the first skill in the seed and returns its id.
//
// Only skills with attachments reach the "build a zip" path; hand-written skills have no
// attachments but must be uploaded too (the body itself is the skill content).
//
// ⚠️ **Bytes go through object storage, not straight into the content column.** After 4a, rows
// with an empty file_id cannot be read (see hydrateSkillFiles). Calling store.PutSkillFiles
// directly would make these tests die on "refusing to hand over an incomplete skill", which has
// nothing to do with "syncing skills to Ark" that they test.
func seedSkillWithFiles(t *testing.T, svc *Service, store *coremem.MemStore) string {
	t.Helper()
	ctx := context.Background()
	skills, err := store.ListSkills(ctx, seedWS)
	if err != nil || len(skills) == 0 {
		t.Fatalf("种子里应当有技能：%v", err)
	}
	id := skills[0].ID

	files := []apimodel.SkillFile{
		{Path: "scripts/init.sh", Content: []byte("echo hi")},
		{Path: "references/rules.md", Content: []byte("# 规则\n")},
	}
	putSkillFilesViaBlobs(t, ctx, svc, id, files)
	// The metadata must also be recorded in doc, otherwise skillinject cannot see "does it have attachments".
	sk, _ := store.GetSkill(ctx, id)
	sk.Files = []apimodel.SkillFile{
		{Path: "references/rules.md", Size: len(files[1].Content)},
		{Path: "scripts/init.sh", Size: len(files[0].Content)},
	}
	if _, err := store.SaveSkill(ctx, sk); err != nil {
		t.Fatalf("存技能失败：%v", err)
	}
	return id
}

// bindSkills binds a set of skills to the seed agent.
func bindSkills(t *testing.T, store *coremem.MemStore, agentID string, skillIDs ...string) {
	t.Helper()
	ctx := context.Background()
	a, err := store.GetAgent(ctx, agentID)
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	a.SkillIDs = skillIDs
	if _, err := store.SaveAgent(ctx, a); err != nil {
		t.Fatalf("存 agent 失败：%v", err)
	}
}

// TestArkSkillsUploadedOnceAndAttachedToAgent is the core positive case: on the first
// conversation the skill is uploaded and attached to the Ark Agent.
func TestArkSkillsUploadedOnceAndAttachedToAgent(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)
	skillID := seedSkillWithFiles(t, svc, store)
	bindSkills(t, store, seedChatAgent, skillID)

	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("开对话失败：%v", err)
	}

	// Uploaded once, and what was uploaded is a real zip.
	if len(runner.uploadedSkills) != 1 {
		t.Fatalf("应当上传 1 个技能包，实际 %d 个", len(runner.uploadedSkills))
	}
	up := runner.uploadedSkills[0]
	if !strings.HasSuffix(up.fileName, ".zip") {
		t.Errorf("文件名要带 .zip（方舟据此识别格式），实际 %q", up.fileName)
	}
	// The bundle can be read back by our own parser, so its shape is right (frontmatter and top-level directory are there).
	pkg, err := ParseSkillPackage(up.fileName, up.zip)
	if err != nil {
		t.Fatalf("上传的包必须是合法技能包：%v", err)
	}
	if len(pkg.Files) != 2 {
		t.Errorf("包里应当有 2 个附件，实际 %d 个", len(pkg.Files))
	}

	// **The skill must be attached to the Ark Agent**: this is what this fix is about.
	// It is included when the Agent is created, so no extra UpdateAgentSkills is needed.
	if len(runner.spec.Skills) != 1 {
		t.Fatalf("建方舟 Agent 时应当带上 1 个技能，实际 %d 个：%+v",
			len(runner.spec.Skills), runner.spec.Skills)
	}
	if runner.spec.Skills[0].ArkSkillID != "skill-ark-1" {
		t.Errorf("挂上去的应当是上传返回的那个 id，实际 %q", runner.spec.Skills[0].ArkSkillID)
	}
	if runner.updateSkillsCalls != 0 {
		t.Errorf("建 Agent 时已经带上了技能，不该再打一次 UpdateAgent，实际 %d 次",
			runner.updateSkillsCalls)
	}

	// The binding must record the fingerprint, for comparison next time.
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

// TestArkSkillNotReuploadedWhenUnchanged pins down dedup.
//
// Ark has no delete API, so every extra upload is garbage that can **never** be cleaned up.
//
// Mutation check: remove the early return on a GetArkSkillUpload hit in ensureArkSkill
// → the second conversation uploads again, and this test turns red.
func TestArkSkillNotReuploadedWhenUnchanged(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)
	skillID := seedSkillWithFiles(t, svc, store)
	bindSkills(t, store, seedChatAgent, skillID)

	for i := 0; i < 3; i++ {
		if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
			t.Fatalf("第 %d 次开对话失败：%v", i+1, err)
		}
	}

	if len(runner.uploadedSkills) != 1 {
		t.Errorf("内容没变，应当只上传 1 次，实际 %d 次（每多一次就是方舟侧一份清不掉的垃圾）",
			len(runner.uploadedSkills))
	}
	// The binding hits and the fingerprint is unchanged → not a single UpdateAgent should be sent.
	if runner.updateSkillsCalls != 0 {
		t.Errorf("指纹没变不该打 UpdateAgent，实际 %d 次", runner.updateSkillsCalls)
	}
}

// TestArkSkillReuploadedWhenContentChanges pins down "a changed body must be re-uploaded".
//
// Ark has no "edit skill" API; re-uploading means a new skill_id, and then the reference on the Agent must be swapped.
func TestArkSkillReuploadedWhenContentChanges(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)
	skillID := seedSkillWithFiles(t, svc, store)
	bindSkills(t, store, seedChatAgent, skillID)

	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("开对话失败：%v", err)
	}
	if len(runner.uploadedSkills) != 1 {
		t.Fatalf("首次应当上传 1 次，实际 %d 次", len(runner.uploadedSkills))
	}

	// Change the skill body (through Service, the same as the user editing in the UI).
	sk, _ := store.GetSkill(ctx, skillID)
	if _, err := svc.UpdateSkill(ctx, skillID, apimodel.SkillDraft{
		Name:         sk.Name,
		DisplayTitle: sk.DisplayTitle,
		Description:  sk.Description,
		Instructions: sk.Instructions + "\n## 新增一节\n",
		Tags:         sk.Tags,
	}); err != nil {
		t.Fatalf("改技能失败：%v", err)
	}

	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("再开对话失败：%v", err)
	}

	if len(runner.uploadedSkills) != 2 {
		t.Errorf("正文改了应当重传，实际共上传 %d 次（不重传的话模型按旧内容干活，且零报错）",
			len(runner.uploadedSkills))
	}
	// And the reference on the Agent must be swapped to the new one.
	if runner.updateSkillsCalls != 1 {
		t.Fatalf("重传之后要更新 Agent 上的引用，实际 UpdateAgent %d 次", runner.updateSkillsCalls)
	}
	if len(runner.updatedSkills) != 1 || runner.updatedSkills[0].ArkSkillID != "skill-ark-2" {
		t.Errorf("Agent 上应当换成新传的那个 id，实际 %+v", runner.updatedSkills)
	}
}

// TestArkSkillsSyncedWhenBindingAlreadyExists is the regression test for **the symptom this fix
// targets**.
//
// Scenario: chat once (the binding and the Ark Agent are both created), then bind one more skill.
// The Ark Agent is cached forever (ensureBinding reuses it on a hit). Without an active sync the
// Ark side never learns about the new skill: the user sees "I bound a skill, but the agent acts
// like it doesn't exist", with zero errors.
//
// Mutation check: replace syncBindingSkills in ensureBinding with `return existing, nil`
// → this test turns red.
func TestArkSkillsSyncedWhenBindingAlreadyExists(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	// First conversation: the agent has no skills bound yet.
	bindSkills(t, store, seedChatAgent) // clear the bindings
	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("首次开对话失败：%v", err)
	}
	if len(runner.spec.Skills) != 0 {
		t.Fatalf("此时还没绑技能，方舟 Agent 上不该有，实际 %+v", runner.spec.Skills)
	}
	agentsBefore := runner.createdAgents

	// Only now bind a skill.
	skillID := seedSkillWithFiles(t, svc, store)
	bindSkills(t, store, seedChatAgent, skillID)

	// Start another conversation.
	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("再开对话失败：%v", err)
	}

	// The skill must be uploaded and attached to the **same** Ark Agent.
	if len(runner.uploadedSkills) != 1 {
		t.Errorf("新绑的技能应当被上传，实际 %d 次", len(runner.uploadedSkills))
	}
	if runner.updateSkillsCalls != 1 {
		t.Fatalf("绑定已存在时要靠 UpdateAgent 追上去，实际 %d 次（0 次就是本次要修的 bug）",
			runner.updateSkillsCalls)
	}
	if len(runner.updatedSkills) != 1 {
		t.Errorf("应当把新技能挂上去，实际 %+v", runner.updatedSkills)
	}
	// **The Ark Agent must not be recreated**: recreating leaks one more (Ark allows duplicate names
	// and they cannot be deleted), and it changes ArkAgentID, so old sessions point at an Agent that is no longer updated.
	if runner.createdAgents != agentsBefore {
		t.Errorf("不该重建方舟 Agent（会泄漏且换掉 id），建了 %d 个",
			runner.createdAgents-agentsBefore)
	}

	// The fingerprint must be written back, so the third conversation does not update again.
	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("第三次开对话失败：%v", err)
	}
	if runner.updateSkillsCalls != 1 {
		t.Errorf("指纹已回写，第三次不该再更新，实际共 %d 次", runner.updateSkillsCalls)
	}
}

// TestArkSkillsClearedWhenUnbound pins down "unbinding the last skill must really clear the Ark side".
//
// Ark's skills field replaces the whole list, and "not passing it" and "passing an empty array"
// are two different things: the former keeps what was there. So after unbinding we must clear it
// explicitly, otherwise the model keeps using that skill while the UI no longer shows it.
func TestArkSkillsClearedWhenUnbound(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)
	skillID := seedSkillWithFiles(t, svc, store)
	bindSkills(t, store, seedChatAgent, skillID)

	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("开对话失败：%v", err)
	}

	// Unbind everything.
	bindSkills(t, store, seedChatAgent)
	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("再开对话失败：%v", err)
	}

	if runner.updateSkillsCalls != 1 {
		t.Fatalf("解绑之后要更新方舟侧，实际 %d 次", runner.updateSkillsCalls)
	}
	if len(runner.updatedSkills) != 0 {
		t.Errorf("解绑之后方舟侧应当是空的，实际 %+v", runner.updatedSkills)
	}

	// The fingerprint must go back to the empty string (the same state as "never bound").
	b, _ := store.GetArkBinding(ctx, seedWS, seedChatAgent)
	if b.SkillsFingerprint != "" {
		t.Errorf("全解绑后指纹应当是空串，实际 %q", b.SkillsFingerprint)
	}
}

// TestConversationSurvivesSkillUploadFailure pins down degrading.
//
// Conversations work today without skills, so "the skill could not be uploaded" must never stop a
// conversation from starting: that would trade the main feature for an add-on.
//
// Mutation check: change that `continue` in arkSkillsFor to `return err`
// → this test turns red (the conversation cannot start).
func TestConversationSurvivesSkillUploadFailure(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)
	skillID := seedSkillWithFiles(t, svc, store)
	bindSkills(t, store, seedChatAgent, skillID)
	runner.uploadErr = errors.New("方舟炸了")

	run, err := svc.CreateConversation(ctx, seedWS, seedChatAgent)
	if err != nil {
		t.Fatalf("技能上传失败不该让对话开不起来：%v", err)
	}
	if run.ID == "" {
		t.Error("对话应当正常建出来")
	}
	// This round the model does not know about the skill, but the conversation is usable.
	if len(runner.spec.Skills) != 0 {
		t.Errorf("上传失败时不该挂上任何技能，实际 %+v", runner.spec.Skills)
	}
}

// TestConversationSurvivesSkillUpdateFailure is the same as above, but the failure happens in UpdateAgent.
//
// It also pins one more thing: **the fingerprint must not be written back on failure**. Writing it back would make the next
// conversation hit the fingerprint directly with no retry at all, so that skill would never take effect (with zero errors).
func TestConversationSurvivesSkillUpdateFailure(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	bindSkills(t, store, seedChatAgent)
	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("首次开对话失败：%v", err)
	}

	skillID := seedSkillWithFiles(t, svc, store)
	bindSkills(t, store, seedChatAgent, skillID)
	runner.updateSkillsErr = errors.New("方舟拒了")

	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("更新技能失败不该让对话开不起来：%v", err)
	}

	// The fingerprint must **not** be written back.
	b, _ := store.GetArkBinding(ctx, seedWS, seedChatAgent)
	if b.SkillsFingerprint != "" {
		t.Errorf("更新失败时不该回写指纹（否则下次连重试都没有），实际 %q", b.SkillsFingerprint)
	}

	// The next conversation must **try again**.
	runner.updateSkillsErr = nil
	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("第三次开对话失败：%v", err)
	}
	if runner.updateSkillsCalls != 2 {
		t.Errorf("上次失败了，这次应当重试，实际共 %d 次", runner.updateSkillsCalls)
	}
	b, _ = store.GetArkBinding(ctx, seedWS, seedChatAgent)
	if b.SkillsFingerprint == "" {
		t.Error("这次成功了，指纹该回写了")
	}
}

// TestArkSkillsRejectsTooMany pins down the limit of 20.
//
// Going over makes Ark return an English InvalidParameter, which shows up as "this agent cannot
// start a conversation" and does not point at "you bound 21 skills" at all. So we stop it here and
// return a Chinese error.
//
// **Error when creating the binding** (the user is configuring, so seeing the error helps), while
// an existing binding only degrades (this runs on every conversation start, and blocking it would
// make people think conversations are broken). The latter is pinned by the next test.
func TestArkSkillsRejectsTooMany(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	ids := make([]string, 0, maxArkSkillsPerAgent+1)
	for i := 0; i <= maxArkSkillsPerAgent; i++ {
		sk, err := svc.CreateSkill(ctx, seedWS, apimodel.SkillDraft{
			Name:         "bulk-" + string(rune('a'+i%26)) + strings.Repeat("x", i/26+1),
			DisplayTitle: "批量",
			Description:  "凑数用",
			Instructions: "# 凑数\n",
		})
		if err != nil {
			t.Fatalf("建第 %d 个技能失败：%v", i, err)
		}
		ids = append(ids, sk.ID)
	}
	bindSkills(t, store, seedChatAgent, ids...)

	_, err := svc.CreateConversation(ctx, seedWS, seedChatAgent)
	if err == nil {
		t.Fatal("绑了超过 20 个技能应当被挡住")
	}
	if !errors.Is(err, ErrPrecondition) {
		t.Errorf("应当是 ErrPrecondition（422），实际 %v", err)
	}
	if !strings.Contains(err.Error(), "最多") {
		t.Errorf("报错要说清上限，实际：%v", err)
	}
	// Nothing should be uploaded: block first, then act; do not upload half and then fail.
	if len(runner.uploadedSkills) != 0 {
		t.Errorf("超限时不该上传任何技能，实际 %d 个", len(runner.uploadedSkills))
	}
}

// TestArkSkillsTooManyDoesNotBreakExistingConversation pins down "with an existing binding, going over the limit only degrades".
//
// It complements the previous test: that one is "creating a new binding must error", this one is "an existing binding must not error".
// An error would make users find "after adding a skill I can't even start a conversation", while the conversation itself has nothing to do with skills.
func TestArkSkillsTooManyDoesNotBreakExistingConversation(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	// First create the binding while under the limit.
	bindSkills(t, store, seedChatAgent)
	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("首次开对话失败：%v", err)
	}

	// Now bind past the limit.
	ids := make([]string, 0, maxArkSkillsPerAgent+1)
	for i := 0; i <= maxArkSkillsPerAgent; i++ {
		sk, err := svc.CreateSkill(ctx, seedWS, apimodel.SkillDraft{
			Name:         "over-" + string(rune('a'+i%26)) + strings.Repeat("y", i/26+1),
			DisplayTitle: "超限",
			Description:  "凑数用",
			Instructions: "# 凑数\n",
		})
		if err != nil {
			t.Fatalf("建技能失败：%v", err)
		}
		ids = append(ids, sk.ID)
	}
	bindSkills(t, store, seedChatAgent, ids...)

	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Errorf("绑定已存在时超限只该降级，不该让对话开不起来：%v", err)
	}
}

// TestArkSkillRejectsCrossWorkspace pins down that cross-workspace references are blocked.
//
// # This is not a dirty data problem, it is data crossing a boundary
//
// DeleteWorkspace deliberately does not clean up dangling references from agents in other
// workspaces (store_pg.go has a comment on this, to behave the same as LocalApiClient), so
// agent.SkillIDs **really can** contain skill ids from another workspace.
//
// Without the block: workspace A's Ark key would upload the body and attachments of workspace B's
// skill, B's content would appear in A's project, and A's members could see it directly in the Ark
// console.
//
// Mutation check: remove the sk.WorkspaceID != key.WorkspaceID check in ensureArkSkill
// → this test turns red (the skill gets uploaded).
func TestArkSkillRejectsCrossWorkspace(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	// Create a skill in another workspace.
	other, err := svc.CreateWorkspace(ctx, WorkspaceInput{Name: "别的空间", Key: "other"})
	if err != nil {
		t.Fatalf("建另一个空间失败：%v", err)
	}
	foreign, err := svc.CreateSkill(ctx, other.ID, apimodel.SkillDraft{
		Name:         "foreign-skill",
		DisplayTitle: "别空间的技能",
		Description:  "不该被 ws_demo 用到",
		Instructions: "# 机密内容\n",
	})
	if err != nil {
		t.Fatalf("建跨空间技能失败：%v", err)
	}

	// Build a cross-workspace reference by hand (the normal path cannot create one, but deleting a workspace leaves such dangling references).
	bindSkills(t, store, seedChatAgent, foreign.ID)

	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("跨空间引用应当被跳过而不是让对话失败：%v", err)
	}

	// Key assertion: **not a single byte should be uploaded**.
	if len(runner.uploadedSkills) != 0 {
		t.Errorf("跨空间技能绝不该被上传（那是把 B 空间的内容泄露进 A 的 project），实际传了 %d 个",
			len(runner.uploadedSkills))
	}
	if len(runner.spec.Skills) != 0 {
		t.Errorf("跨空间技能不该挂到方舟 Agent 上，实际 %+v", runner.spec.Skills)
	}
}

// TestBindingKeepsNoFingerprintWhenArkDidNotAttach pins down "if Ark did not attach it, do not write back the fingerprint".
//
// # It blocks the half where "the code is fixed but production still isn't"
//
// That incident had two injuries: the literal in the ma layer forgot to pass Skills (the skills
// were not attached), and ensureBinding wrote back the fingerprint **unconditionally** (blocking
// the repair path). The second injury alone is enough to defeat the fix for the first: the binding
// claims "this set is already attached", so on the next conversation syncBindingSkills sees the
// same fingerprint and returns directly, and the UpdateAgentSkills that should correct it is never
// called.
//
// So this test simulates "Ark only accepts 0 skills" (exactly what the real forgot-to-pass case
// looks like), and requires the fingerprint in the binding to **stay empty**. Empty means the next
// conversation will retry for sure.
func TestBindingKeepsNoFingerprintWhenArkDidNotAttach(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)
	skillID := seedSkillWithFiles(t, svc, store)
	bindSkills(t, store, seedChatAgent, skillID)

	// Ark accepted the request but did not attach a single skill.
	zero := 0
	runner.createAgentSkillCount = &zero

	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		// Failing to attach skills should not stop the conversation; degrading is deliberate.
		t.Fatalf("技能没挂上时对话仍应能开：%v", err)
	}

	b, err := store.GetArkBinding(ctx, seedWS, seedChatAgent)
	if err != nil {
		t.Fatalf("取绑定失败：%v", err)
	}
	if b.SkillsFingerprint != "" {
		t.Errorf("方舟没挂上技能时**不能**回写指纹，实际 %q。\n"+
			"回写了会让下次开对话命中指纹直接跳过，那个技能就永久缺失且零报错"+
			"——光把漏传的字段补上也救不回来。", b.SkillsFingerprint)
	}
	if len(b.ArkSkillIDs) != 0 {
		t.Errorf("方舟没挂上技能时不该记 arkSkillIds（那是假账），实际 %v", b.ArkSkillIDs)
	}
}

// TestSecondConversationRetriesAttachAfterArkDidNotAttach is the second half of the previous one:
// **after leaving the fingerprint empty, the next conversation really does attach again**.
//
// The previous test only checks "no dirty data was written", while the user wants "the skill is
// usable in the end". Only the two together show that the self-healing path works: checking only
// the former, an implementation that "always leaves the fingerprint empty but never re-attaches"
// would still pass.
func TestSecondConversationRetriesAttachAfterArkDidNotAttach(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)
	skillID := seedSkillWithFiles(t, svc, store)
	bindSkills(t, store, seedChatAgent, skillID)

	// First time: Ark did not attach it (the shape of the forgot-to-pass case).
	zero := 0
	runner.createAgentSkillCount = &zero
	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("第一次开对话失败：%v", err)
	}
	if runner.updateSkillsCalls != 0 {
		t.Fatalf("建 Agent 那一次不该走 UpdateAgent，实际 %d 次", runner.updateSkillsCalls)
	}

	// Second time: hits the existing binding, and the fingerprint is empty ⇒ must go through UpdateAgentSkills once to re-attach.
	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("第二次开对话失败：%v", err)
	}
	if runner.updateSkillsCalls != 1 {
		t.Fatalf("指纹留空时第二次开对话必须补挂一次技能，实际打了 %d 次 UpdateAgent。\n"+
			"一次都不打说明自愈通道没通——用户会永远等不到技能生效。",
			runner.updateSkillsCalls)
	}
	if len(runner.updatedSkills) != 1 || runner.updatedSkills[0].ArkSkillID != "skill-ark-1" {
		t.Errorf("补挂的应当是那个已上传技能，实际 %+v", runner.updatedSkills)
	}
	// Write back the fingerprint only after re-attaching succeeds; after that it should hit steadily and not update again.
	b, err := store.GetArkBinding(ctx, seedWS, seedChatAgent)
	if err != nil {
		t.Fatalf("取绑定失败：%v", err)
	}
	if b.SkillsFingerprint == "" {
		t.Error("补挂成功后应当回写指纹，否则每次开对话都会白打一次 UpdateAgent")
	}
	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("第三次开对话失败：%v", err)
	}
	if runner.updateSkillsCalls != 1 {
		t.Errorf("指纹已经写对了，第三次不该再更新，实际累计 %d 次", runner.updateSkillsCalls)
	}
}
