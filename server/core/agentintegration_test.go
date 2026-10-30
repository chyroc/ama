// Tests for storing and reading the selected integrations (Agent.IntegrationIDs), plus test
// helpers shared by several injection-related tests.
package core

import (
	"context"
	"log/slog"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/repository/dao/coremem"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// seedRunFor creates a Run that belongs to the given workspace and agent, and returns its id
// (= the Ark session id).
func seedRunFor(t *testing.T, store *coremem.MemStore, workspaceID, agentID string) string {
	t.Helper()
	run, err := store.CreateRun(context.Background(), apimodel.Run{
		ID:          "sesn-test-" + agentID,
		WorkspaceID: workspaceID,
		AgentID:     agentID,
		Status:      apimodel.RunIdle,
		Trigger:     apimodel.TriggerManual,
		StartedAt:   apimodel.NowISO(),
		Threads:     []apimodel.RunThread{},
	})
	if err != nil {
		t.Fatalf("造 Run 失败: %v", err)
	}
	return run.ID
}

// ownerOf returns the email of the owner of the seed workspace.
func ownerOf(t *testing.T, store *coremem.MemStore, workspaceID string) string {
	t.Helper()
	ws, err := store.GetWorkspace(context.Background(), workspaceID)
	if err != nil {
		t.Fatalf("取空间失败: %v", err)
	}
	email := firstOwnerEmail(ws.Members)
	if email == "" {
		t.Fatal("种子空间应当有 owner")
	}
	return email
}

// seedAgentIn creates an agent in the given workspace, optionally with the integrations it has selected.
func seedAgentIn(t *testing.T, svc *Service, workspaceID string, integrations ...string) apimodel.Agent {
	t.Helper()
	a, err := svc.CreateAgent(context.Background(), workspaceID, apimodel.AgentDraft{
		Name:           "测试 agent",
		Model:          apimodel.ModelConfig{ID: "doubao-pro"},
		IntegrationIDs: integrations,
	})
	if err != nil {
		t.Fatalf("建 agent 失败: %v", err)
	}
	return a
}

// TestIntegrationIDsSurviveRoundTrip pins that this field can be stored, read, and
// **cleared**.
//
// "Can be cleared" is the point: UpdateAgent assigns IntegrationIDs **unconditionally**
// (the opposite of Status), because an empty list is a meaningful value here (= remove all
// integrations). Writing it the way Status is written would stop users from removing the
// last integration; it would look like "I removed it and it came back by itself".
func TestIntegrationIDsSurviveRoundTrip(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	agent := seedAgentIn(t, svc, seedWS, apimodel.IntegrationLarkUser, apimodel.IntegrationAMA)
	got, err := svc.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	// Sorted, so the order is fixed: ama < lark-user.
	if len(got.IntegrationIDs) != 2 ||
		got.IntegrationIDs[0] != apimodel.IntegrationAMA ||
		got.IntegrationIDs[1] != apimodel.IntegrationLarkUser {
		t.Fatalf("存不住或没排序: %v", got.IntegrationIDs)
	}

	// Remove all.
	updated, err := svc.UpdateAgent(ctx, agent.ID, apimodel.AgentDraft{
		Name:           got.Name,
		Model:          got.Model,
		IntegrationIDs: []string{},
	})
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	if len(updated.IntegrationIDs) != 0 {
		t.Errorf("应当能全部取消，实际还剩 %v ——"+
			"这说明 UpdateAgent 用了「空就跳过」判断，用户会取消不掉",
			updated.IntegrationIDs)
	}
	// ⚠️ Must be a non-nil empty slice; otherwise it encodes as null and the frontend isDirty stays dirty forever.
	if updated.IntegrationIDs == nil {
		t.Error("清空后必须是 []（非 nil），nil 会编码成 null 让页面永久「脏」")
	}
}
