package app

import (
	"context"
	"fmt"
	"log/slog"
)

// BlobStoreSelection is the object storage returned by a downstream factory. Client is an
// opaque implementation-owned value; this module never serializes credentials or configuration.
type BlobStoreSelection struct {
	Client any
	Close  func(context.Context) error
}

// BlobStoreModuleConfig chooses the availability rule. Required is appropriate for a
// multi-instance deployment that has no shared byte fallback; Optional allows the downstream
// to keep a documented legacy fallback. Neither choice silently invents a storage backend.
type BlobStoreModuleConfig struct {
	Name     string
	Required bool
	Open     func(context.Context) (BlobStoreSelection, error)
	Logger   *slog.Logger
}

type BlobStoreModule struct{ cfg BlobStoreModuleConfig }

func NewBlobStoreModule(cfg BlobStoreModuleConfig) *BlobStoreModule {
	return &BlobStoreModule{cfg: cfg}
}

func (m *BlobStoreModule) Name() string {
	if m == nil || m.cfg.Name == "" {
		return "object-storage"
	}
	return m.cfg.Name
}

func (m *BlobStoreModule) Setup(ctx context.Context, runtime *Runtime) error {
	if m == nil || m.cfg.Open == nil {
		return fmt.Errorf("app: object storage module has no factory")
	}
	selection, err := m.cfg.Open(ctx)
	if err != nil {
		return fmt.Errorf("app: open object storage: %w", err)
	}
	if m.cfg.Required && selection.Client == nil {
		return fmt.Errorf("app: object storage is required but not configured")
	}
	if selection.Close != nil {
		runtime.OnClose("object-storage", selection.Close)
	}
	return nil
}
