package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/repository/dao/coremem"
)

type policyRevisions struct{}

func (policyRevisions) Record(context.Context, apimodel.RevisionKind, string, string, string, any, apimodel.RevisionReason) {
}
func (policyRevisions) Drop(context.Context, apimodel.RevisionKind, string) {}

type policyMembers struct{}

func (policyMembers) RoleOf(context.Context, string, string) (apimodel.WorkspaceRole, bool, error) {
	return apimodel.RoleOwner, true, nil
}

type policyIntegrations struct{}

func (policyIntegrations) DisplayName(id string) string { return id }

type policySyncs struct{}

func (policySyncs) EnqueueAgentSync(context.Context, apimodel.Agent) {}

type policyCredentials struct{ calls int }

func (p *policyCredentials) AllowServiceCredentials(_ context.Context, _, _ apimodel.Agent, _ apimodel.AgentCredentialIDs) error {
	p.calls++
	return errors.New("named credential is not bound")
}

func TestCredentialPolicyRunsOnlyForServiceCredentialChanges(t *testing.T) {
	ctx := auth.WithIdentity(context.Background(), auth.Identity{Email: "owner@example.com"})
	store := coremem.NewMemStore()
	if _, err := store.CreateWorkspace(ctx, apimodel.Workspace{ID: "ws_test", Name: "Example", Key: "example"}); err != nil {
		t.Fatal(err)
	}
	policy := &policyCredentials{}
	service, err := New(Config{
		Store: store, NextID: func(string) string { return "agent_test" },
		Revisions: policyRevisions{}, Members: policyMembers{}, Integrations: policyIntegrations{}, Syncs: policySyncs{}, Credentials: policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.CreateAgent(ctx, "ws_test", apimodel.AgentDraft{Name: "original", Model: apimodel.ModelConfig{ID: "example-model"}})
	if err != nil {
		t.Fatal(err)
	}
	if policy.calls != 0 {
		t.Fatalf("ordinary create must not call service policy: %d", policy.calls)
	}
	if _, err := service.UpdateAgent(ctx, created.ID, apimodel.AgentDraft{Name: "renamed", Model: apimodel.ModelConfig{ID: "example-model"}}); err != nil {
		t.Fatal(err)
	}
	if policy.calls != 0 {
		t.Fatalf("rename must not call service policy: %d", policy.calls)
	}
	_, err = service.SetAgentCredentials(ctx, created.ID, []string{apimodel.IntegrationServiceAccount}, apimodel.AgentCredentialIDs{
		apimodel.IntegrationServiceAccount: "credential_test",
	})
	if err == nil || policy.calls != 1 {
		t.Fatalf("named credential selection must call policy once: %v calls=%d", err, policy.calls)
	}
}
