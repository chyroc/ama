package company

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/chyroc/ama/server/app"
	publiccore "github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/model/persistmodel"
)

func TestNamedCredentialAdapterKeepsIdentityAndOwnership(t *testing.T) {
	store := &exampleCredentialStore{rows: map[string]CredentialRow{}}
	provider := NewNamedCredentials(store, exampleVerifier{}, func() string { return "credential_example" })
	view, err := provider.Add(context.Background(), "owner@example.com", "read only", "application_example", "secret_example")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Update(context.Background(), "owner@example.com", view.ID, "renamed", "another_application", "new_secret"); !errors.Is(err, publiccore.ErrInvalidArgument) {
		t.Fatalf("identity replacement error = %v", err)
	}
	if err := provider.Delete(context.Background(), "other@example.com", view.ID); !errors.Is(err, publiccore.ErrNotFound) {
		t.Fatalf("other owner delete error = %v", err)
	}
	if store.rows[view.ID].ApplicationID != "application_example" {
		t.Fatalf("public identity changed: %#v", store.rows[view.ID])
	}
}

func TestNewBackgroundModuleSkipsMissingOptionalTasks(t *testing.T) {
	module := NewBackgroundModule(nil)
	if module == nil {
		t.Fatal("background module must be usable without optional tasks")
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	process := app.New(app.Config{Logger: logger}).AddModule(module)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := process.Run(ctx); err != nil {
		t.Fatalf("empty optional task module failed: %v", err)
	}
}

type exampleVerifier struct{}

func (exampleVerifier) Verify(context.Context, CredentialRow) publiccore.NamedCredentialVerification {
	return publiccore.NamedCredentialVerification{Unknown: true, Reason: "offline example"}
}

type exampleCredentialStore struct{ rows map[string]CredentialRow }

func (s *exampleCredentialStore) ListCredentials(_ context.Context, owner string) ([]CredentialRow, error) {
	rows := make([]CredentialRow, 0, len(s.rows))
	for _, row := range s.rows {
		if row.Owner == owner {
			rows = append(rows, row)
		}
	}
	return rows, nil
}
func (s *exampleCredentialStore) GetCredential(_ context.Context, id string) (CredentialRow, error) {
	row, ok := s.rows[id]
	if !ok {
		return CredentialRow{}, persistmodel.ErrNotFound
	}
	return row, nil
}
func (s *exampleCredentialStore) PutCredential(_ context.Context, row CredentialRow) error {
	if s.rows == nil {
		s.rows = map[string]CredentialRow{}
	}
	if old, ok := s.rows[row.ID]; ok {
		row.ApplicationID = old.ApplicationID
	}
	s.rows[row.ID] = row
	return nil
}
func (s *exampleCredentialStore) DeleteCredential(_ context.Context, id string) error {
	delete(s.rows, id)
	return nil
}
