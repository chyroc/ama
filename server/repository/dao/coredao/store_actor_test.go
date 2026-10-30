// Storage-layer round trip for the two fields "who created it / who changed it".
//
// # Why this deserves its own test instead of relying on the Service-side tests
//
// The Service tests (core/entityactor_test.go) only run the in-memory implementation and prove "the stamping logic is right";
// this one proves **what is stored can be read back**, which is a different thing. All four entities marshal the whole struct
// into `doc jsonb`, so new fields come "for free", but **nothing tests that they come for free**.
// If anyone later pulls a doc field out into its own column (this repo already did it: workspaces pulled out
// description / created_at / updated_at), forgetting these two fields drops them silently,
// while all the ordering and counting tests stay green.
//
// ⚠️ The three people's names for the three entities are **deliberately all different**, and CreatedBy differs from UpdatedBy:
// with the same value for both, a wrong implementation that "also writes updatedBy into createdBy" would still pass.
package coredao

import (
	"context"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

// testActorFieldsRoundTrip: CreatedBy / UpdatedBy of the three entities can be stored and read back.
func testActorFieldsRoundTrip(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	// -- Skill --------------------------------------------------------------
	sk, err := s.CreateSkill(ctx, apimodel.Skill{
		ID:            "skill_actor",
		WorkspaceID:   "ws_demo",
		Name:          "actor-probe",
		Source:        apimodel.SkillCustom,
		LatestVersion: "1",
		Tags:          []string{},
		CreatedAt:     apimodel.NowISO(),
		UpdatedAt:     apimodel.NowISO(),
		CreatedBy:     "skill-creator@example.com",
		UpdatedBy:     "skill-editor@example.com",
	})
	if err != nil {
		t.Fatalf("建技能失败：%v", err)
	}
	if sk.CreatedBy != "skill-creator@example.com" || sk.UpdatedBy != "skill-editor@example.com" {
		t.Errorf("建技能的返回值就丢了：createdBy=%q updatedBy=%q", sk.CreatedBy, sk.UpdatedBy)
	}
	gotSk, err := s.GetSkill(ctx, "skill_actor")
	if err != nil {
		t.Fatalf("读回技能失败：%v", err)
	}
	if gotSk.CreatedBy != "skill-creator@example.com" {
		t.Errorf("技能 createdBy 没存住：实际 %q", gotSk.CreatedBy)
	}
	if gotSk.UpdatedBy != "skill-editor@example.com" {
		t.Errorf("技能 updatedBy 没存住：实际 %q", gotSk.UpdatedBy)
	}

	// -- Schedule -----------------------------------------------------------
	sch := newTestSchedule("sch_actor", apimodel.NowISO())
	sch.CreatedBy = "sched-creator@example.com"
	sch.UpdatedBy = "sched-editor@example.com"
	if _, err := s.CreateSchedule(ctx, sch); err != nil {
		t.Fatalf("建定时任务失败：%v", err)
	}
	gotSch, err := s.GetSchedule(ctx, "sch_actor")
	if err != nil {
		t.Fatalf("读回定时任务失败：%v", err)
	}
	if gotSch.CreatedBy != "sched-creator@example.com" {
		t.Errorf("定时任务 createdBy 没存住：实际 %q", gotSch.CreatedBy)
	}
	if gotSch.UpdatedBy != "sched-editor@example.com" {
		t.Errorf("定时任务 updatedBy 没存住：实际 %q", gotSch.UpdatedBy)
	}

	// -- Workspace ----------------------------------------------------------
	//
	// ⚠️ The workspace case is the most worth pinning: its createdBy / updatedBy are **separate columns pulled out of doc**
	// (doc no longer writes them, see CreateWorkspace). "Pulling out columns" is exactly the action that drops fields,
	// and dropping them does not error; "who created it" just never shows up.
	ws, err := s.CreateWorkspace(ctx, apimodel.Workspace{
		ID:        "ws_actor",
		Name:      "留痕探针",
		Key:       "actor-probe",
		CreatedAt: apimodel.NowISO(),
		UpdatedAt: apimodel.NowISO(),
		CreatedBy: "ws-creator@example.com",
		UpdatedBy: "ws-editor@example.com",
		Members: []apimodel.WorkspaceMember{{
			ID: "m_actor", Name: "建的人", Email: "ws-creator@example.com",
			Role: apimodel.RoleOwner, JoinedAt: apimodel.NowISO(),
		}},
	})
	if err != nil {
		t.Fatalf("建空间失败：%v", err)
	}
	if ws.CreatedBy != "ws-creator@example.com" {
		t.Errorf("建空间的返回值就丢了 createdBy：实际 %q", ws.CreatedBy)
	}
	gotWS, err := s.GetWorkspace(ctx, "ws_actor")
	if err != nil {
		t.Fatalf("读回空间失败：%v", err)
	}
	if gotWS.CreatedBy != "ws-creator@example.com" {
		t.Errorf("空间 createdBy 没存住：实际 %q", gotWS.CreatedBy)
	}
	if gotWS.UpdatedBy != "ws-editor@example.com" {
		t.Errorf("空间 updatedBy 没存住：实际 %q", gotWS.UpdatedBy)
	}

	// Lookup by key is another read path (shares getWorkspaceBy but has a different entry point); it must read them too.
	byKey, err := s.GetWorkspaceByKey(ctx, "actor-probe")
	if err != nil {
		t.Fatalf("按 key 读回空间失败：%v", err)
	}
	if byKey.CreatedBy != "ws-creator@example.com" {
		t.Errorf("按 key 读空间丢了 createdBy：实际 %q", byKey.CreatedBy)
	}
}
