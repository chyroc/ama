// Tests for the three read-only "open in Ark console" endpoints.
//
// # What these guard
//
// Every failure mode of this feature **reports no error**: the link still renders, clicking it
// still opens a page, but that page is about something else. So the tests do not focus on "is
// there a return value", but on these points:
//
//   - Ark resources are **created lazily**; before they exist we must return **empty**, not a
//     guessed id;
//   - region must come from **the credential actually in effect** (three-level fallback), not a
//     constant;
//   - the skill id depends on "which key uploaded it"; if the key cannot be found, give nothing.
package core

import (
	"context"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/repository/dao/coremem"
)

// TestAgentArkConsoleEmptyBeforeFirstChat: an agent that has never chatted should not return an
// Ark agent id.
//
// # Why this is the most important case
//
// The Ark Agent is only created by `ensureBinding` on **the first chat**. A freshly created agent
// does not exist on the Ark side at all. If we return anything other than an empty string here
// (or the frontend builds a URL from the empty string), the user lands on
// `/managed-agents/agents/`, a 404 page.
//
// And "agent created but never chatted" is **the most common** state, not an edge case.
func TestAgentArkConsoleEmptyBeforeFirstChat(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newChatService(t)

	ref, err := svc.agents.GetAgentArkConsole(ctx, "agent_triage")
	if err != nil {
		t.Fatalf("取方舟控制台引用失败：%v", err)
	}
	if ref.ArkAgentID != "" {
		t.Errorf("还没开过对话就给出了方舟 agent id（会拼出一个 404 链接）：%q", ref.ArkAgentID)
	}
	// region must still be returned: it comes from the credential, unrelated to "whether the Ark side was created".
	if ref.Region != apimodel.RegionCNBeijing {
		t.Errorf("region 应当取自凭据，实际 %q", ref.Region)
	}
}

// TestAgentArkConsoleAfterBinding: after a chat, the real Ark agent id must be returned.
func TestAgentArkConsoleAfterBinding(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	if err := store.PutArkBinding(ctx, persistmodel.ArkBinding{
		WorkspaceID: seedWS,
		AgentID:     "agent_triage",
		// Use the real shape (taken from real production data) instead of "x": with the wrong
		// shape, problems on the frontend URL-building side would not show here.
		ArkAgentID:       "agent-20260831113008-frhdh",
		ArkEnvironmentID: "env-1",
	}); err != nil {
		t.Fatalf("写绑定失败：%v", err)
	}

	ref, err := svc.agents.GetAgentArkConsole(ctx, "agent_triage")
	if err != nil {
		t.Fatalf("取方舟控制台引用失败：%v", err)
	}
	if ref.ArkAgentID != "agent-20260831113008-frhdh" {
		t.Errorf("方舟 agent id 不对：%q", ref.ArkAgentID)
	}
}

// TestAgentArkConsoleRegionFollowsCredential: region must follow the credential, not be a constant.
//
// What hardcoding cn-beijing looks like: a user in the Singapore region clicks through and sees
// "resource does not exist". That page opens fine, so nothing reports an error.
func TestAgentArkConsoleRegionFollowsCredential(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	if err := store.PutArkSecret(ctx, apimodel.ArkSecret{
		ArkCredential: apimodel.ArkCredential{
			WorkspaceID: seedWS,
			Region:      apimodel.RegionAPSoutheast1,
		},
		APIKey: "ARK0123456789abcdef",
	}); err != nil {
		t.Fatalf("改凭据区域失败：%v", err)
	}

	ref, err := svc.agents.GetAgentArkConsole(ctx, "agent_triage")
	if err != nil {
		t.Fatalf("取方舟控制台引用失败：%v", err)
	}
	if ref.Region != apimodel.RegionAPSoutheast1 {
		t.Errorf("region 没跟着凭据走（硬编码了？）：%q", ref.Region)
	}
}

// TestAgentArkConsoleNoCredential: with no Ark credential configured, region is empty.
//
// An empty region is the signal to the frontend: if the URL cannot be built, do not render the
// entry. Falling back to a default region would build a link that **looks right but points at
// another region**.
func TestAgentArkConsoleNoCredential(t *testing.T) {
	ctx := context.Background()
	// Deliberately **do not use** newChatService: that helper writes a credential in, and this
	// case needs exactly "none at all".
	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败：%v", err)
	}
	svc, err := New(Config{Store: store})
	if err != nil {
		t.Fatalf("装配 Service 失败：%v", err)
	}

	ref, err := svc.agents.GetAgentArkConsole(ctx, "agent_triage")
	if err != nil {
		t.Fatalf("取方舟控制台引用失败：%v", err)
	}
	if ref.Region != "" {
		t.Errorf("没配凭据时 region 应为空（否则会拼出指向错区域的链接），实际 %q", ref.Region)
	}
	if ref.ArkAgentID != "" {
		t.Errorf("没配凭据也不该有方舟 agent id：%q", ref.ArkAgentID)
	}
}

// TestRunArkConsoleUsesRunID covers the session case: run.ID itself is the Ark session id.
func TestRunArkConsoleUsesRunID(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建会话失败：%v", err)
	}

	ref, err := svc.runs.GetRunArkConsole(ctx, run.ID)
	if err != nil {
		t.Fatalf("取方舟控制台引用失败：%v", err)
	}
	if ref.ArkSessionID != run.ID {
		t.Errorf("会话 id 应当就是 run.ID：想要 %q，得到 %q", run.ID, ref.ArkSessionID)
	}
	if ref.Region != apimodel.RegionCNBeijing {
		t.Errorf("region 不对：%q", ref.Region)
	}
}

// TestRunArkConsoleNotFound: a run that does not exist must return 404, not a fake ref that can build a URL.
func TestRunArkConsoleNotFound(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newChatService(t)

	if _, err := svc.runs.GetRunArkConsole(ctx, "sesn-does-not-exist"); err == nil {
		t.Error("不存在的运行应当报错，否则会给出一个指向不存在会话的链接")
	}
}

// TestSkillArkConsoleEmptyWithoutUpload: no id is returned when no agent has uploaded the skill.
//
// A skill is uploaded to Ark **only when an agent chats with it** (arkSkillsFor →
// ensureArkSkill). A skill that is created but never used does not exist on the Ark side, so any
// id given here would be fake.
func TestSkillArkConsoleEmptyWithoutUpload(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newChatService(t)

	skills, err := svc.ListSkills(ctx, seedWS)
	if err != nil {
		t.Fatalf("列技能失败：%v", err)
	}
	var target apimodel.Skill
	for _, sk := range skills {
		if sk.WorkspaceID == seedWS {
			target = sk
			break
		}
	}
	if target.ID == "" {
		t.Skip("种子里没有属于该空间的技能")
	}

	ref, err := svc.skills.GetSkillArkConsole(ctx, target.ID)
	if err != nil {
		t.Fatalf("取方舟控制台引用失败：%v", err)
	}
	if ref.ArkSkillID != "" {
		t.Errorf("技能还没传到方舟就给出了 id（会拼出一个 404 链接）：%q", ref.ArkSkillID)
	}
	// ⚠️ **region must still be returned** (found missing during self-review): the frontend uses
	// "region present but no id" to mean "you use Ark, this skill just has not been uploaded yet".
	// An all-empty ref would be read as "Ark was never configured", which is a different thing,
	// and the UI should say something different.
	if ref.Region != apimodel.RegionCNBeijing {
		t.Errorf("没找到上传记录时也该带上 region，实际 %q", ref.Region)
	}
}

// TestSkillArkConsoleAfterUpload: after an upload, the Ark skill id must be returned.
//
// # This case also pins down "fingerprint by current content"
//
// The upload record's primary key includes content_hash. The hash written here is computed on
// the spot **with the same algorithm as ensureArkSkill**. If the read path changed to look up by
// a different rule (e.g. ignore the hash and take the latest row), this case would still pass,
// but TestSkillArkConsoleStaleAfterEdit below would fail. Read the two together for the full picture.
func TestSkillArkConsoleAfterUpload(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	sk, agentID := seedSkillBoundToAgent(t, svc, ctx)
	// The agent needs a binding, otherwise "which key uploaded this skill" cannot be found.
	if err := store.PutArkBinding(ctx, persistmodel.ArkBinding{
		WorkspaceID: seedWS, AgentID: agentID,
		ArkAgentID: "agent-20260831113008-frhdh", ArkEnvironmentID: "env-1",
	}); err != nil {
		t.Fatalf("写绑定失败：%v", err)
	}

	files, err := svc.SkillFiles(ctx, sk.ID)
	if err != nil {
		t.Fatalf("取技能附件失败：%v", err)
	}
	if _, err := store.PutArkSkillUpload(ctx, persistmodel.ArkSkillUpload{
		WorkspaceID:    seedWS,
		SkillID:        sk.ID,
		ContentHash:    SkillFingerprint(sk, files),
		KeyFingerprint: ArkKeyFingerprint("ARK0123456789abcdef"),
		ArkSkillID:     "skill-20260901120830-px59g",
		ArkVersion:     "1",
		CreatedAt:      apimodel.NowISO(),
	}); err != nil {
		t.Fatalf("写上传记录失败：%v", err)
	}

	ref, err := svc.skills.GetSkillArkConsole(ctx, sk.ID)
	if err != nil {
		t.Fatalf("取方舟控制台引用失败：%v", err)
	}
	if ref.ArkSkillID != "skill-20260901120830-px59g" {
		t.Errorf("方舟技能 id 不对：%q", ref.ArkSkillID)
	}
	if ref.Region != apimodel.RegionCNBeijing {
		t.Errorf("region 不对：%q", ref.Region)
	}
}

// TestSkillArkConsoleStaleAfterEdit: after the skill content changes, it must no longer point at
// the old Ark skill.
//
// # This guards the most hidden failure in the whole feature
//
// Ark has no "update skill" API; `CreateSkill` produces a new id every time. So once a skill is
// edited, the id on the Ark side refers to **the old content**. If the read path does not look up
// by the fingerprint of the current content, the user clicks through and sees the version they
// already changed: **the page opens, the content is old, no error at all**.
//
// This is also the evidence for "do not add a field on the entity to store the id": that
// snapshot would be frozen at the moment it was written.
func TestSkillArkConsoleStaleAfterEdit(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	sk, agentID := seedSkillBoundToAgent(t, svc, ctx)
	if err := store.PutArkBinding(ctx, persistmodel.ArkBinding{
		WorkspaceID: seedWS, AgentID: agentID,
		ArkAgentID: "agent-1", ArkEnvironmentID: "env-1",
	}); err != nil {
		t.Fatalf("写绑定失败：%v", err)
	}
	files, err := svc.SkillFiles(ctx, sk.ID)
	if err != nil {
		t.Fatalf("取技能附件失败：%v", err)
	}
	if _, err := store.PutArkSkillUpload(ctx, persistmodel.ArkSkillUpload{
		WorkspaceID: seedWS, SkillID: sk.ID,
		ContentHash:    SkillFingerprint(sk, files),
		KeyFingerprint: ArkKeyFingerprint("ARK0123456789abcdef"),
		ArkSkillID:     "skill-old-content", ArkVersion: "1",
		CreatedAt: apimodel.NowISO(),
	}); err != nil {
		t.Fatalf("写上传记录失败：%v", err)
	}

	// Change the body: from now on, that id on the Ark side refers to the old content.
	if _, err := svc.UpdateSkill(ctx, sk.ID, apimodel.SkillDraft{
		Name:         sk.Name,
		DisplayTitle: sk.DisplayTitle,
		Description:  sk.Description,
		Instructions: sk.Instructions + "\n\n新加的一段。",
		Tags:         sk.Tags,
	}); err != nil {
		t.Fatalf("改技能失败：%v", err)
	}

	ref, err := svc.skills.GetSkillArkConsole(ctx, sk.ID)
	if err != nil {
		t.Fatalf("取方舟控制台引用失败：%v", err)
	}
	if ref.ArkSkillID == "skill-old-content" {
		t.Error("技能改过之后仍然指向旧的方舟技能——用户会看到自己已经改掉的版本，且零报错")
	}
}

// TestSkillArkConsoleBuiltinHasNoEntry: built-in skills get no entry.
//
// They do not belong to any workspace; "which copy is yours" depends on which workspace you look
// from, and the skill detail page has no such context. A guessed id is worse than none.
func TestSkillArkConsoleBuiltinHasNoEntry(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newChatService(t)

	builtin := BuiltinSkills()
	if len(builtin) == 0 {
		t.Skip("没有内置技能")
	}

	ref, err := svc.skills.GetSkillArkConsole(ctx, builtin[0].ID)
	if err != nil {
		t.Fatalf("内置技能不该报错（详情页要打得开）：%v", err)
	}
	if ref.ArkSkillID != "" {
		t.Errorf("内置技能不该给出方舟技能 id：%q", ref.ArkSkillID)
	}
}

// seedSkillBoundToAgent finds a skill that belongs to the seed workspace and is bound to some
// agent.
//
// Returns the skill and the id of the agent bound to it. If none is found, bind one on the spot:
// the test needs this precondition to hold, not to rely on luck about the seed data's shape.
func seedSkillBoundToAgent(t *testing.T, svc *Service, ctx context.Context) (apimodel.Skill, string) {
	t.Helper()

	skills, err := svc.ListSkills(ctx, seedWS)
	if err != nil {
		t.Fatalf("列技能失败：%v", err)
	}
	var sk apimodel.Skill
	for _, s := range skills {
		// Built-in skills have an empty WorkspaceID; exclude them.
		if s.WorkspaceID == seedWS {
			sk = s
			break
		}
	}
	if sk.ID == "" {
		t.Skip("种子里没有属于该空间的技能")
	}

	agents, err := svc.ListAgents(ctx, seedWS)
	if err != nil {
		t.Fatalf("列 agent 失败：%v", err)
	}
	for _, a := range agents {
		for _, id := range a.SkillIDs {
			if id == sk.ID {
				return sk, a.ID
			}
		}
	}
	// No existing binding, so create one.
	if len(agents) == 0 {
		t.Skip("种子里没有 agent")
	}
	a := agents[0]
	if _, err := svc.UpdateAgent(ctx, a.ID, apimodel.AgentDraft{
		Name:           a.Name,
		Description:    a.Description,
		System:         a.System,
		Model:          a.Model,
		BuiltinTools:   a.BuiltinTools,
		SkillIDs:       append(append([]string{}, a.SkillIDs...), sk.ID),
		FeishuAppIDs:   a.FeishuAppIDs,
		Status:         a.Status,
		IntegrationIDs: a.IntegrationIDs,
		Envs:           a.Envs,
		EnvAliases:     a.EnvAliases,
	}); err != nil {
		t.Fatalf("绑技能失败：%v", err)
	}
	return sk, a.ID
}
