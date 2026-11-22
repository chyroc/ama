package core

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/repository/dao/coremem"
	"github.com/chyroc/ama/server/shared/infra/ma"
)

func TestAgentEnvironmentChoiceControlsBindingAndWorker(t *testing.T) {
	svc, store, runner, workspaceID, agentA, agentB := arkKeyFixture(t)
	ctx := context.Background()
	if err := store.PutArkSecret(ctx, apimodel.ArkSecret{
		ArkCredential: apimodel.ArkCredential{
			WorkspaceID: workspaceID, Region: apimodel.RegionCNBeijing,
			Source: apimodel.SourceOverride,
		},
		APIKey: "ark-environment-choice-test",
	}); err != nil {
		t.Fatal(err)
	}
	cloud, err := store.CreateAgent(ctx, apimodel.Agent{
		ID: "agent_cloud", WorkspaceID: workspaceID, Name: "cloud",
		Model: apimodel.ModelConfig{ID: "m"}, EnvironmentType: apimodel.ArkEnvironmentCloud,
		BuiltinTools: []string{"bash"}, SkillIDs: []string{}, FeishuAppIDs: []string{},
		Status: apimodel.StatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{agentA, cloud.ID, agentB} {
		if _, err := svc.CreateConversation(ctx, workspaceID, id); err != nil {
			t.Fatalf("agent %s: %v", id, err)
		}
	}
	if got, want := runner.environmentTypes, []string{apimodel.ArkEnvironmentSelfHosted, apimodel.ArkEnvironmentCloud}; !reflect.DeepEqual(got, want) {
		t.Fatalf("environment types=%v, want %v: only siblings of the same type may be reused", got, want)
	}
	b, err := store.GetArkBinding(ctx, workspaceID, cloud.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.EnvironmentType != apimodel.ArkEnvironmentCloud {
		t.Fatalf("binding type=%q", b.EnvironmentType)
	}
	if got := arkSpecFor(cloud, []ma.CustomToolSpec{{Name: "query_sql"}}); len(got.Tools) != 1 ||
		got.Tools[0] != ma.ToolsetDefault || got.CustomTools != nil || got.BaseAgent != "" {
		t.Fatalf("general cloud must use Ark's native toolset without AMA-side tools: %+v", got)
	}

	var ensured []string
	RefreshWorkers(ctx, RefreshWorkersHooks{
		ListWorkspaces: func(context.Context) ([]apimodel.Workspace, error) {
			return []apimodel.Workspace{{ID: workspaceID}}, nil
		},
		ListBindings: store.ListArkBindings,
		Ensure: func(_ context.Context, _, _, envID string) {
			ensured = append(ensured, envID)
		},
	})
	if len(ensured) != 1 || ensured[0] == b.ArkEnvironmentID {
		t.Fatalf("worker environments=%v, cloud environment %s must not be polled", ensured, b.ArkEnvironmentID)
	}
}

func TestCreateAgentPersistsEnvironmentAndRejectsChanges(t *testing.T) {
	f := newHostScopeFixture(t)
	draft := hostDraft(f.otherMac, "workspace")
	draft.EnvironmentType = apimodel.ArkEnvironmentCloud
	created, err := f.svc.CreateAgent(context.Background(), seedWS, draft)
	if err != nil {
		t.Fatal(err)
	}
	if created.EnvironmentType != apimodel.ArkEnvironmentCloud || created.ExecutorType != "" ||
		created.HostID != "" || created.HostSource != "" {
		t.Fatalf("cloud agent must not keep a host executor: %+v", created)
	}
	changed := hostDraft("", "")
	changed.EnvironmentType = apimodel.ArkEnvironmentSelfHosted
	if _, err := f.svc.UpdateAgent(context.Background(), created.ID, changed); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("environment change must be rejected: %v", err)
	}
	if _, err := f.svc.SetAgentIntegrations(context.Background(), created.ID, []string{apimodel.IntegrationAMA}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("cloud must reject credential injection: %v", err)
	}
	legacy := hostDraft(f.otherMac, "workspace")
	legacy.EnvironmentType = ""
	legacyAgent, err := f.svc.CreateAgent(context.Background(), seedWS, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if legacyAgent.EnvironmentType != apimodel.ArkEnvironmentSelfHosted {
		t.Fatalf("omitted type must remain self_hosted: %q", legacyAgent.EnvironmentType)
	}
	if _, err := f.svc.UpdateAgent(context.Background(), legacyAgent.ID, legacy); err != nil {
		t.Fatalf("an old client saving the same environment must be accepted: %v", err)
	}
}

func TestLegacyBindingWithoutTypeIsSelfHosted(t *testing.T) {
	store := coremem.NewMemStore()
	ctx := context.Background()
	if err := store.PutArkBinding(ctx, persistmodel.ArkBinding{
		WorkspaceID: "ws", AgentID: "a", ArkEnvironmentID: "env-legacy",
	}); err != nil {
		t.Fatal(err)
	}
	var ensured []string
	RefreshWorkers(ctx, RefreshWorkersHooks{
		ListWorkspaces: func(context.Context) ([]apimodel.Workspace, error) {
			return []apimodel.Workspace{{ID: "ws"}}, nil
		},
		ListBindings: store.ListArkBindings,
		Ensure: func(_ context.Context, _, _, envID string) {
			ensured = append(ensured, envID)
		},
	})
	if !reflect.DeepEqual(ensured, []string{"env-legacy"}) {
		t.Fatalf("legacy worker must keep running, got %v", ensured)
	}
}
