package auth

import (
	"context"
	"log/slog"
	"time"
)

// APIKeyStore is the narrow persistence contract needed by user and managed API Keys.
// Login providers and their additional state stores do not have to implement other Store methods.
type APIKeyStore interface {
	PutAPIKey(ctx context.Context, key APIKeyRecord) error
	GetAPIKeyByHash(ctx context.Context, keyHash string) (APIKeyRecord, error)
	ListAPIKeysByEmail(ctx context.Context, email string) ([]APIKeyRecord, error)
	DeleteAPIKey(ctx context.Context, email, id string) (bool, error)
}

// APIKeyManager owns user-created and platform-derived API Keys without requiring a
// particular login implementation. It uses the same rules as Service's existing methods.
type APIKeyManager struct {
	store            APIKeyStore
	managedKeySecret string
	log              *slog.Logger
	now              func() time.Time
}

// NewAPIKeyManager builds a key-only manager. managedKeySecret may be empty: user-created
// keys still work, while platform-derived keys report ErrManagedKeyUnavailable.
func NewAPIKeyManager(store APIKeyStore, managedKeySecret string, logger *slog.Logger) *APIKeyManager {
	manager := &APIKeyManager{
		store: store, managedKeySecret: managedKeySecret,
		log: logger, now: time.Now,
	}
	if manager.log == nil {
		manager.log = slog.Default()
	}
	return manager
}
