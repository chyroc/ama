package auth

import (
	"context"
	"strings"
	"testing"
)

func TestAPIKeyManagerWorksWithoutLoginService(t *testing.T) {
	store := NewMemStore()
	manager := NewAPIKeyManager(store, "example-managed-secret", nil)
	ctx := context.Background()
	created, err := manager.CreateAPIKey(ctx, "owner@example.com", "example")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Key, KeyPrefix) {
		t.Fatalf("unexpected plaintext shape: %q", created.Key)
	}
	record, err := store.GetAPIKeyByHash(ctx, hashAPIKey(created.Key))
	if err != nil {
		t.Fatal(err)
	}
	if record.KeyHash == created.Key {
		t.Fatal("database must store only the hash")
	}
	managed, err := manager.EnsureManagedKey(ctx, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	again, err := manager.EnsureManagedKey(ctx, "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if managed != again {
		t.Fatal("managed keys must be deterministic across calls")
	}
	if err := manager.RevokeAPIKey(ctx, "owner@example.com", created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetAPIKeyByHash(ctx, hashAPIKey(created.Key)); err == nil {
		t.Fatal("revocation must remove the stored key")
	}
}
