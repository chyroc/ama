package company

import (
	"context"
	"time"

	"github.com/chyroc/ama/server/app"
	publiccore "github.com/chyroc/ama/server/core"
)

// CredentialRow is a downstream-owned storage shape. Encryption at rest and SQL conditions
// remain private; the public service sees only this adapter's explicit field conversion.
type CredentialRow struct {
	ID                string
	Owner             string
	Label             string
	ApplicationID     string
	Secret            string
	MaskedSecret      string
	LastVerifiedAt    time.Time
	LastVerifyError   string
	LastVerifyLimited bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type CredentialStore interface {
	ListCredentials(context.Context, string) ([]CredentialRow, error)
	GetCredential(context.Context, string) (CredentialRow, error)
	PutCredential(context.Context, CredentialRow) error
	DeleteCredential(context.Context, string) error
}

// CredentialVerifier is implemented with the downstream platform SDK. Its result must not
// contain a plaintext secret. Unknown means a temporary inability to check, not a bad key.
type CredentialVerifier interface {
	Verify(context.Context, CredentialRow) publiccore.NamedCredentialVerification
}

func NewNamedCredentials(
	store CredentialStore, verifier CredentialVerifier, newID func() string,
) *publiccore.NamedCredentialProvider {
	return publiccore.NewNamedCredentialProvider(publiccore.NamedCredentialHooks{
		Store: namedStoreAdapter{store}, Provider: "example application directory",
		Verify: func(ctx context.Context, record publiccore.NamedCredentialRecord) publiccore.NamedCredentialVerification {
			if verifier == nil {
				return publiccore.NamedCredentialVerification{Unknown: true, Reason: "no verifier is configured"}
			}
			return verifier.Verify(ctx, privateCredential(record))
		},
		NewID: newID, PublicIDName: "Application ID", SecretName: "Application Secret",
	})
}

// NewBackgroundModule composes public lifecycle registration with tasks built by the
// downstream. An optional task must be a literal nil interface, not a typed nil pointer.
func NewBackgroundModule(tasks ...app.Task) app.Module {
	return app.NewTaskModule("company-background", tasks...)
}

type namedStoreAdapter struct{ store CredentialStore }

func (a namedStoreAdapter) List(ctx context.Context, owner string) ([]publiccore.NamedCredentialRecord, error) {
	rows, err := a.store.ListCredentials(ctx, owner)
	out := make([]publiccore.NamedCredentialRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, publicCredential(row))
	}
	return out, err
}
func (a namedStoreAdapter) Get(ctx context.Context, id string) (publiccore.NamedCredentialRecord, error) {
	row, err := a.store.GetCredential(ctx, id)
	return publicCredential(row), err
}
func (a namedStoreAdapter) Put(ctx context.Context, record publiccore.NamedCredentialRecord) error {
	return a.store.PutCredential(ctx, privateCredential(record))
}
func (a namedStoreAdapter) Delete(ctx context.Context, id string) error {
	return a.store.DeleteCredential(ctx, id)
}

func publicCredential(row CredentialRow) publiccore.NamedCredentialRecord {
	return publiccore.NamedCredentialRecord{
		ID: row.ID, Actor: row.Owner, Name: row.Label, PublicID: row.ApplicationID,
		Secret: row.Secret, SecretMasked: row.MaskedSecret,
		LastVerifiedAt: row.LastVerifiedAt, LastVerifyError: row.LastVerifyError,
		LastVerifyLimited: row.LastVerifyLimited, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}
func privateCredential(row publiccore.NamedCredentialRecord) CredentialRow {
	return CredentialRow{
		ID: row.ID, Owner: row.Actor, Label: row.Name, ApplicationID: row.PublicID,
		Secret: row.Secret, MaskedSecret: row.SecretMasked,
		LastVerifiedAt: row.LastVerifiedAt, LastVerifyError: row.LastVerifyError,
		LastVerifyLimited: row.LastVerifyLimited, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}
