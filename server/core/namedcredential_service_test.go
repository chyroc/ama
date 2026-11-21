package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/persistmodel"
)

type namedCredentialTestStore struct {
	records map[string]NamedCredentialRecord
	puts    int
}

func (s *namedCredentialTestStore) List(_ context.Context, actor string) ([]NamedCredentialRecord, error) {
	out := make([]NamedCredentialRecord, 0)
	for _, record := range s.records {
		if record.Actor == actor {
			out = append(out, record)
		}
	}
	return out, nil
}
func (s *namedCredentialTestStore) Get(_ context.Context, id string) (NamedCredentialRecord, error) {
	record, ok := s.records[id]
	if !ok {
		return NamedCredentialRecord{}, persistmodel.ErrNotFound
	}
	return record, nil
}
func (s *namedCredentialTestStore) Put(_ context.Context, record NamedCredentialRecord) error {
	if s.records == nil {
		s.records = map[string]NamedCredentialRecord{}
	}
	if old, ok := s.records[record.ID]; ok {
		record.PublicID = old.PublicID
	}
	s.records[record.ID] = record
	s.puts++
	return nil
}
func (s *namedCredentialTestStore) Delete(_ context.Context, id string) error {
	delete(s.records, id)
	return nil
}

func newNamedCredentialTestProvider(
	store NamedCredentialStore, verify func(context.Context, NamedCredentialRecord) NamedCredentialVerification,
) *NamedCredentialProvider {
	return NewNamedCredentialProvider(NamedCredentialHooks{
		Store: store, Verify: verify, Provider: "example", NewID: func() string { return "credential_example" },
		Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	})
}

func TestNamedCredentialUpdateCannotChangePublicIdentity(t *testing.T) {
	store := &namedCredentialTestStore{}
	provider := newNamedCredentialTestProvider(store, func(context.Context, NamedCredentialRecord) NamedCredentialVerification {
		return NamedCredentialVerification{Valid: true}
	})
	view, err := provider.Add(context.Background(), "owner@example.com", "read only", "application_one", "secret_one")
	if err != nil {
		t.Fatal(err)
	}
	before := store.records[view.ID]
	if _, err := provider.Update(context.Background(), "owner@example.com", view.ID, "read only", "application_two", "secret_two"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("identity replacement error = %v, want invalid argument", err)
	}
	if after := store.records[view.ID]; after != before {
		t.Fatalf("rejected update changed the record: before=%#v after=%#v", before, after)
	}
}

func TestNamedCredentialRenameOnlyKeepsPreviousVerification(t *testing.T) {
	store := &namedCredentialTestStore{records: map[string]NamedCredentialRecord{
		"credential_example": {
			ID: "credential_example", Actor: "owner@example.com", Name: "read only",
			PublicID: "application_one", Secret: "secret_one", LastVerifyError: "previous verification failed",
			LastVerifiedAt: time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC),
		},
	}}
	verifications := 0
	provider := newNamedCredentialTestProvider(store, func(context.Context, NamedCredentialRecord) NamedCredentialVerification {
		verifications++
		return NamedCredentialVerification{Valid: true}
	})
	updated, err := provider.Update(context.Background(), "owner@example.com", "credential_example", "renamed", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if updated.StatusReason != "previous verification failed" {
		t.Fatalf("rename erased previous verification: %#v", updated)
	}
	if verifications != 0 || store.puts != 1 {
		t.Fatalf("rename-only must not invoke verification: verifications = %d, puts = %d", verifications, store.puts)
	}
}

func TestNamedCredentialMutationChecksOwnership(t *testing.T) {
	store := &namedCredentialTestStore{}
	provider := newNamedCredentialTestProvider(store, nil)
	view, err := provider.Add(context.Background(), "owner@example.com", "read only", "application_one", "secret_one")
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Delete(context.Background(), "other@example.com", view.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner's delete error = %v, want not found", err)
	}
	if _, err := provider.Update(context.Background(), "other@example.com", view.ID, "renamed", "", "new_secret"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other owner's update error = %v, want not found", err)
	}
	if _, ok := store.records[view.ID]; !ok {
		t.Fatal("unauthorized mutation deleted the record")
	}
}

func TestNamedCredentialLimitedAndUnknownAreNotInvalid(t *testing.T) {
	store := &namedCredentialTestStore{}
	provider := newNamedCredentialTestProvider(store, func(context.Context, NamedCredentialRecord) NamedCredentialVerification {
		return NamedCredentialVerification{Valid: true, Limited: true}
	})
	view, err := provider.Add(context.Background(), "owner@example.com", "limited", "application_one", "secret_one")
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "limited" {
		t.Fatalf("limited credential status = %q", view.Status)
	}
}
