package company

import (
	"context"
	"time"

	publiccore "github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// BindingRow belongs to the downstream storage layer. Its column names, unique constraints,
// and transaction implementation need not match AMA's own storage.
type BindingRow struct {
	ID           string
	WorkspaceID  string
	ProviderID   string
	CredentialID string
	BoundBy      string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// BindingStore is implemented by the downstream database. Upsert must let its unique
// constraint decide duplicate concurrent writes and retain the original ID and creation time.
type BindingStore interface {
	ListBindings(context.Context, string) ([]BindingRow, error)
	UpsertBinding(context.Context, BindingRow) error
	GetBinding(context.Context, string) (BindingRow, error)
	DeleteBinding(context.Context, string) error
}

// CredentialMetadata contains no secret. The downstream directory may use any private protocol.
type CredentialMetadata struct {
	Name     string
	PublicID string
	Owner    string
}

type CredentialDirectory interface {
	Metadata(context.Context, string, string) (CredentialMetadata, error)
}

// NewCredentialBindings adapts private storage and metadata to the public reference rules.
// The caller supplies a random ID generator, authenticated actor, and provider name lookup.
func NewCredentialBindings(
	store BindingStore, directory CredentialDirectory,
	actor func(context.Context) string, providerName func(string) string, newID func() string,
) *publiccore.WorkspaceIntegrationService {
	return publiccore.NewWorkspaceIntegrationService(publiccore.WorkspaceIntegrationHooks{
		Store: bindingStoreAdapter{store}, Credentials: credentialDirectoryAdapter{directory},
		WorkspaceExists: func(context.Context, string) error {
			// A real adapter calls its own workspace store and maps ErrNotFound here.
			return nil
		},
		Actor: actor, ProviderName: providerName, NewID: newID,
	})
}

type bindingStoreAdapter struct{ store BindingStore }

func (a bindingStoreAdapter) ListWorkspaceIntegrations(
	ctx context.Context, workspaceID string,
) ([]persistmodel.WorkspaceIntegration, error) {
	rows, err := a.store.ListBindings(ctx, workspaceID)
	out := make([]persistmodel.WorkspaceIntegration, 0, len(rows))
	for _, row := range rows {
		out = append(out, publicBinding(row))
	}
	return out, err
}

func (a bindingStoreAdapter) PutWorkspaceIntegration(
	ctx context.Context, bind persistmodel.WorkspaceIntegration,
) error {
	return a.store.UpsertBinding(ctx, BindingRow{
		ID: bind.ID, WorkspaceID: bind.WorkspaceID, ProviderID: bind.ProviderID,
		CredentialID: bind.CredentialID, BoundBy: bind.BoundBy,
		CreatedAt: bind.CreatedAt, UpdatedAt: bind.UpdatedAt,
	})
}

func (a bindingStoreAdapter) GetWorkspaceIntegration(
	ctx context.Context, id string,
) (persistmodel.WorkspaceIntegration, error) {
	row, err := a.store.GetBinding(ctx, id)
	return publicBinding(row), err
}

func (a bindingStoreAdapter) DeleteWorkspaceIntegration(ctx context.Context, id string) error {
	return a.store.DeleteBinding(ctx, id)
}

func publicBinding(row BindingRow) persistmodel.WorkspaceIntegration {
	return persistmodel.WorkspaceIntegration{
		ID: row.ID, WorkspaceID: row.WorkspaceID, ProviderID: row.ProviderID,
		CredentialID: row.CredentialID, BoundBy: row.BoundBy,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

type credentialDirectoryAdapter struct{ directory CredentialDirectory }

func (a credentialDirectoryAdapter) CredentialMeta(
	ctx context.Context, providerID, credentialID string,
) (apimodel.IntegrationCredentialView, error) {
	metadata, err := a.directory.Metadata(ctx, providerID, credentialID)
	return apimodel.IntegrationCredentialView{
		Name: metadata.Name, PublicID: metadata.PublicID, Actor: metadata.Owner,
	}, err
}
