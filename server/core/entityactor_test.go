// The "who created it / who changed it" stamping boundaries of three entities: Skill / Workspace
// / Schedule.
//
// The Agent one is in agentactor_test.go; this file adds the other three following its pattern.
// **They were not merged into one table-driven test**: the write paths of the four entities have
// very different shapes (Skill has two creation exits and an identity-less patrol path,
// Workspace's "changes" include adding/removing/changing members, Schedule has a self-wake path
// that never goes through Service at all). A table would squash each entity's most important
// boundary into a single row that explains nothing.
//
// Each entity has three fixed assertions, plus its own special boundary:
//
//  1. On create, both CreatedBy and UpdatedBy are the creator;
//  2. After someone else changes it, CreatedBy stays and UpdatedBy becomes the changer (the
//     easiest one to get wrong);
//  3. With no identity, leave an empty string instead of a fake value (after omitempty the key
//     disappears = "unknown who").
package core

import (
	"context"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

// ---------------------------------------------------------------------------
// Skill
// ---------------------------------------------------------------------------

// TestCreateSkillStampsActor: creating a skill records who created it.
func TestCreateSkillStampsActor(t *testing.T) {
	svc, _ := newService(t)

	sk, err := svc.CreateSkill(asActor("alice@example.com"), seedWS, apimodel.SkillDraft{
		Name:         "report-writer",
		Instructions: "你负责写周报。",
	})
	if err != nil {
		t.Fatalf("建技能失败：%v", err)
	}
	if sk.CreatedBy != "alice@example.com" {
		t.Errorf("createdBy 应为 alice@example.com，实际 %q", sk.CreatedBy)
	}
	if sk.UpdatedBy != "alice@example.com" {
		t.Errorf("updatedBy 应为 alice@example.com，实际 %q", sk.UpdatedBy)
	}
}

// TestUpdateSkillKeepsCreatedByButChangesUpdatedBy: after someone else changes it, "who created
// it" must not be overwritten.
//
// ⚠️ Same failure mode as the Agent one: UpdateSkill changes fields on **the copy read back**,
// and also assigning CreatedBy while at it is an easy slip. After that the two fields collapse
// into one and nothing reports an error.
func TestUpdateSkillKeepsCreatedByButChangesUpdatedBy(t *testing.T) {
	svc, _ := newService(t)

	created, err := svc.CreateSkill(asActor("alice@example.com"), seedWS, apimodel.SkillDraft{
		Name:         "report-writer",
		Instructions: "你负责写周报。",
	})
	if err != nil {
		t.Fatalf("建技能失败：%v", err)
	}

	updated, err := svc.UpdateSkill(asActor("bob@example.com"), created.ID, apimodel.SkillDraft{
		Name:         "report-writer",
		Instructions: "你负责写周报，并且附一张图。",
	})
	if err != nil {
		t.Fatalf("改技能失败：%v", err)
	}
	if updated.CreatedBy != "alice@example.com" {
		t.Errorf("createdBy 应仍是 alice（建的人），实际 %q", updated.CreatedBy)
	}
	if updated.UpdatedBy != "bob@example.com" {
		t.Errorf("updatedBy 应变成 bob（改的人），实际 %q", updated.UpdatedBy)
	}
}

// TestCreateSkillWithoutIdentityLeavesActorEmpty: with no identity, leave it empty instead of a fake value.
func TestCreateSkillWithoutIdentityLeavesActorEmpty(t *testing.T) {
	svc, _ := newService(t)

	sk, err := svc.CreateSkill(context.Background(), seedWS, apimodel.SkillDraft{
		Name:         "nobody-owns-this",
		Instructions: "……",
	})
	if err != nil {
		t.Fatalf("建技能失败：%v", err)
	}
	if sk.CreatedBy != "" {
		t.Errorf("无身份时 createdBy 应为空串，实际 %q", sk.CreatedBy)
	}
}

// TestCreateSkillFromPackageStampsActor: upload / install from market / import from repo / fork
// must stamp too.
//
// These four paths share the single exit CreateSkillFromPackage, and all are triggered by users.
// It is pinned separately because it is **not** CreateSkill: if only the latter were tested,
// removing the stamping from this exit would keep all tests green, and every skill installed from
// the market would then have no creator.
func TestCreateSkillFromPackageStampsActor(t *testing.T) {
	svc, _ := newService(t)

	sk, err := svc.CreateSkillFromPackage(
		asActor("alice@example.com"), seedWS,
		apimodel.SkillPackage{
			Name:         "packaged-skill",
			Instructions: "来自一个技能包。",
		},
		SkillCreateOptions{},
	)
	if err != nil {
		t.Fatalf("从技能包建技能失败：%v", err)
	}
	if sk.CreatedBy != "alice@example.com" {
		t.Errorf("createdBy 应为 alice@example.com，实际 %q", sk.CreatedBy)
	}
	if sk.UpdatedBy != "alice@example.com" {
		t.Errorf("updatedBy 应为 alice@example.com，实际 %q", sk.UpdatedBy)
	}
}

// ---------------------------------------------------------------------------
// Workspace
// ---------------------------------------------------------------------------

// TestCreateWorkspaceStampsActor: creating a workspace records who created it.
func TestCreateWorkspaceStampsActor(t *testing.T) {
	svc, _ := newService(t)

	ws, err := svc.CreateWorkspace(asActor("alice@example.com"), WorkspaceInput{
		Name: "记账组",
		Key:  "accounting",
	})
	if err != nil {
		t.Fatalf("建空间失败：%v", err)
	}
	if ws.CreatedBy != "alice@example.com" {
		t.Errorf("createdBy 应为 alice@example.com，实际 %q", ws.CreatedBy)
	}
	if ws.UpdatedBy != "alice@example.com" {
		t.Errorf("updatedBy 应为 alice@example.com，实际 %q", ws.UpdatedBy)
	}
}

// TestUpdateWorkspaceKeepsCreatedByButChangesUpdatedBy: changing a workspace must not overwrite "who created it".
func TestUpdateWorkspaceKeepsCreatedByButChangesUpdatedBy(t *testing.T) {
	svc, _ := newService(t)

	created, err := svc.CreateWorkspace(asActor("alice@example.com"), WorkspaceInput{
		Name: "记账组",
		Key:  "accounting",
	})
	if err != nil {
		t.Fatalf("建空间失败：%v", err)
	}

	newName := "记账与报表组"
	updated, err := svc.UpdateWorkspace(asActor("bob@example.com"), created.ID, WorkspacePatch{
		Name: &newName,
	})
	if err != nil {
		t.Fatalf("改空间失败：%v", err)
	}
	if updated.CreatedBy != "alice@example.com" {
		t.Errorf("createdBy 应仍是 alice（建的人），实际 %q", updated.CreatedBy)
	}
	if updated.UpdatedBy != "bob@example.com" {
		t.Errorf("updatedBy 应变成 bob（改的人），实际 %q", updated.UpdatedBy)
	}
}

// TestAddMemberStampsWorkspaceUpdatedBy: adding/removing/changing members also counts as changing
// the workspace.
//
// Pinned separately for the same reason as CreateSkillFromPackage: the three member write paths
// (add / change role / remove) each call SaveWorkspace. If only UpdateWorkspace were tested,
// removing their stamping would keep everything green, and an admin could change the permissions
// of a whole room of people with no trace left at all.
func TestAddMemberStampsWorkspaceUpdatedBy(t *testing.T) {
	svc, store := newService(t)

	created, err := svc.CreateWorkspace(asActor("alice@example.com"), WorkspaceInput{
		Name: "记账组",
		Key:  "accounting",
	})
	if err != nil {
		t.Fatalf("建空间失败：%v", err)
	}

	if _, err := svc.AddMember(asActor("bob@example.com"), created.ID, MemberInput{
		Name:  "查理",
		Email: "charlie@example.com",
		Role:  apimodel.RoleMember,
	}); err != nil {
		t.Fatalf("加成员失败：%v", err)
	}

	// Read back to check, instead of trusting AddMember's return value: it returns the member,
	// not the workspace. Service has no single-workspace read method (list + Store is the real
	// read path), so go through store.
	after, err := store.GetWorkspace(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("读回空间失败：%v", err)
	}
	if after.CreatedBy != "alice@example.com" {
		t.Errorf("createdBy 应仍是 alice（建的人），实际 %q", after.CreatedBy)
	}
	if after.UpdatedBy != "bob@example.com" {
		t.Errorf("加成员的是 bob，updatedBy 应变成他，实际 %q", after.UpdatedBy)
	}
}

// TestCreateWorkspaceWithoutIdentityLeavesActorEmpty: with no identity, leave it empty.
//
// ⚠️ This also guards against something easy to get backwards: `CreateWorkspace` has an owner
// member right at hand (built by `ownerFrom(ctx)`). Using `owner.Email` as CreatedBy would pass
// the earlier tests today, but it is wrong: ownership can be transferred, "who created it" cannot.
// See apimodel.Workspace.CreatedBy.
func TestCreateWorkspaceWithoutIdentityLeavesActorEmpty(t *testing.T) {
	svc, _ := newService(t)

	ws, err := svc.CreateWorkspace(context.Background(), WorkspaceInput{
		Name: "没人认领的空间",
		Key:  "orphan",
	})
	if err != nil {
		t.Fatalf("建空间失败：%v", err)
	}
	if ws.CreatedBy != "" {
		t.Errorf("无身份时 createdBy 应为空串，实际 %q", ws.CreatedBy)
	}
}

// ---------------------------------------------------------------------------
// Schedule
// ---------------------------------------------------------------------------

// testScheduleDraft builds a minimal usable task input.
func testScheduleDraft(t *testing.T, svc *Service) apimodel.ScheduleDraft {
	t.Helper()
	agents, err := svc.ListAgents(context.Background(), seedWS)
	if err != nil {
		t.Fatalf("列 agent 失败：%v", err)
	}
	if len(agents) == 0 {
		t.Fatal("种子里应该有 agent")
	}
	return apimodel.ScheduleDraft{
		AgentID: agents[0].ID,
		Name:    "每日周报",
		Spec: apimodel.ScheduleSpec{
			Kind:    apimodel.ScheduleInterval,
			EveryMs: apimodel.MinScheduleIntervalMs,
		},
		Prompt: "写一份日报。",
	}
}

// TestCreateScheduleStampsActor: configuring a scheduled task records who configured it.
func TestCreateScheduleStampsActor(t *testing.T) {
	svc, _ := newService(t)
	draft := testScheduleDraft(t, svc)

	sch, err := svc.CreateSchedule(asActor("alice@example.com"), seedWS, draft)
	if err != nil {
		t.Fatalf("建定时任务失败：%v", err)
	}
	if sch.CreatedBy != "alice@example.com" {
		t.Errorf("createdBy 应为 alice@example.com，实际 %q", sch.CreatedBy)
	}
	if sch.UpdatedBy != "alice@example.com" {
		t.Errorf("updatedBy 应为 alice@example.com，实际 %q", sch.UpdatedBy)
	}
}

// TestUpdateScheduleKeepsCreatedByButChangesUpdatedBy: changing a task must not overwrite "who created it".
func TestUpdateScheduleKeepsCreatedByButChangesUpdatedBy(t *testing.T) {
	svc, _ := newService(t)
	draft := testScheduleDraft(t, svc)

	created, err := svc.CreateSchedule(asActor("alice@example.com"), seedWS, draft)
	if err != nil {
		t.Fatalf("建定时任务失败：%v", err)
	}

	draft.Prompt = "写一份日报，并且附上昨天的对比。"
	updated, err := svc.UpdateSchedule(asActor("bob@example.com"), created.ID, draft)
	if err != nil {
		t.Fatalf("改定时任务失败：%v", err)
	}
	if updated.CreatedBy != "alice@example.com" {
		t.Errorf("createdBy 应仍是 alice（建的人），实际 %q", updated.CreatedBy)
	}
	if updated.UpdatedBy != "bob@example.com" {
		t.Errorf("updatedBy 应变成 bob（改的人），实际 %q", updated.UpdatedBy)
	}
}

// TestSetScheduleStatusStampsUpdatedBy: enabling/disabling is also a real user action.
func TestSetScheduleStatusStampsUpdatedBy(t *testing.T) {
	svc, _ := newService(t)
	draft := testScheduleDraft(t, svc)

	created, err := svc.CreateSchedule(asActor("alice@example.com"), seedWS, draft)
	if err != nil {
		t.Fatalf("建定时任务失败：%v", err)
	}

	paused, err := svc.SetScheduleStatus(
		asActor("bob@example.com"), created.ID, apimodel.SchedulePaused)
	if err != nil {
		t.Fatalf("暂停定时任务失败：%v", err)
	}
	if paused.CreatedBy != "alice@example.com" {
		t.Errorf("createdBy 应仍是 alice，实际 %q", paused.CreatedBy)
	}
	if paused.UpdatedBy != "bob@example.com" {
		t.Errorf("暂停的是 bob，updatedBy 应变成他，实际 %q", paused.UpdatedBy)
	}
}

// TestCreateScheduleWithoutIdentityLeavesActorEmpty: with no identity, leave it empty.
func TestCreateScheduleWithoutIdentityLeavesActorEmpty(t *testing.T) {
	svc, _ := newService(t)
	draft := testScheduleDraft(t, svc)

	sch, err := svc.CreateSchedule(context.Background(), seedWS, draft)
	if err != nil {
		t.Fatalf("建定时任务失败：%v", err)
	}
	if sch.CreatedBy != "" {
		t.Errorf("无身份时 createdBy 应为空串，实际 %q", sch.CreatedBy)
	}
}
