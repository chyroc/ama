package company

import (
	"context"
	"errors"
	"testing"

	publiccore "github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/model/apimodel"
)

func TestDownstreamCredentialBindingsKeepPrivateStorage(t *testing.T) {
	const provider = "example-application-directory"
	apimodel.RegisterServiceAccountIntegration(provider)
	store := &exampleBindingStore{rows: map[string]BindingRow{}}
	directory := &exampleCredentialDirectory{name: "initial name"}
	svc := NewCredentialBindings(store, directory,
		func(context.Context) string { return "admin@example.com" },
		func(id string) string { return id },
		func() string { return "binding_example" },
	)
	bound, err := svc.Bind(context.Background(), "workspace_example", apimodel.WorkspaceIntegrationInput{
		ProviderID: provider, CredentialID: "credential_example",
	})
	if err != nil {
		t.Fatal(err)
	}
	directory.name = "renamed in the directory"
	views, err := svc.List(context.Background(), "workspace_example")
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].CredentialName != directory.name || bound.ID != "binding_example" {
		t.Fatalf("live downstream metadata was not used: %#v", views)
	}
	if store.rows[bound.ID].BoundBy != "admin@example.com" {
		t.Fatalf("actor was not taken from downstream context: %#v", store.rows[bound.ID])
	}
}

func TestDownstreamPersonalCredentialCannotBeShared(t *testing.T) {
	svc := NewCredentialBindings(&exampleBindingStore{}, &exampleCredentialDirectory{},
		nil, nil, func() string { return "binding_example" })
	if _, err := svc.Bind(context.Background(), "workspace_example", apimodel.WorkspaceIntegrationInput{
		ProviderID: "example-personal-oauth",
	}); !errors.Is(err, publiccore.ErrPrecondition) {
		t.Fatalf("personal credential sharing error = %v, want precondition rejection", err)
	}
}

type exampleBindingStore struct{ rows map[string]BindingRow }

func (s *exampleBindingStore) ListBindings(_ context.Context, workspaceID string) ([]BindingRow, error) {
	rows := make([]BindingRow, 0, len(s.rows))
	for _, row := range s.rows {
		if row.WorkspaceID == workspaceID {
			rows = append(rows, row)
		}
	}
	return rows, nil
}
func (s *exampleBindingStore) UpsertBinding(_ context.Context, row BindingRow) error {
	if s.rows == nil {
		s.rows = map[string]BindingRow{}
	}
	s.rows[row.ID] = row
	return nil
}
func (s *exampleBindingStore) GetBinding(_ context.Context, id string) (BindingRow, error) {
	row, ok := s.rows[id]
	if !ok {
		return BindingRow{}, publiccore.ErrNotFound
	}
	return row, nil
}
func (s *exampleBindingStore) DeleteBinding(_ context.Context, id string) error {
	delete(s.rows, id)
	return nil
}

type exampleCredentialDirectory struct{ name string }

func (d exampleCredentialDirectory) Metadata(context.Context, string, string) (CredentialMetadata, error) {
	return CredentialMetadata{Name: d.name, PublicID: "application_example", Owner: "owner@example.com"}, nil
}
