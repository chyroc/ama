// Package devblob provides a staff-only proxy for a restricted object-storage prefix.
// Deployment-specific reader lists, prefix choices and storage clients are injected.
package devblob

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/chyroc/ama/server/model/persistmodel"
)

// BlobStore is the object storage required by the proxy.
type BlobStore interface {
	Put(ctx context.Context, key string, content []byte, contentType string) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}

// Readers decides who may use the proxy. Nil denies every caller.
type Readers interface {
	CanRead(actor string) bool
}

// Actor resolves the authenticated caller's stable id.
type Actor func(ctx context.Context) (string, error)

// BlobNotFound recognizes the storage adapter's missing-object error.
type BlobNotFound func(error) bool

// Service serves only keys under Prefix, never arbitrary object-storage keys.
type Service struct {
	objects      BlobStore
	readers      Readers
	actor        Actor
	prefix       string
	maxBytes     int
	blobNotFound BlobNotFound
	warn         func(ctx context.Context, message string, args ...any)
}

// Config wires a restricted object proxy. MaxBytes must be positive.
type Config struct {
	Objects      BlobStore
	Readers      Readers
	Actor        Actor
	Prefix       string
	MaxBytes     int
	BlobNotFound BlobNotFound
	Warn         func(ctx context.Context, message string, args ...any)
}

// New builds a proxy with all checks explicit.
func New(cfg Config) (*Service, error) {
	if cfg.Objects == nil {
		return nil, errors.New("devblob: Objects is required")
	}
	if cfg.Actor == nil {
		return nil, errors.New("devblob: Actor is required")
	}
	if cfg.Prefix == "" || !strings.HasSuffix(cfg.Prefix, "/") {
		return nil, errors.New("devblob: Prefix must end with /")
	}
	if cfg.MaxBytes <= 0 {
		return nil, errors.New("devblob: MaxBytes must be positive")
	}
	if cfg.BlobNotFound == nil {
		return nil, errors.New("devblob: BlobNotFound is required")
	}
	if cfg.Warn == nil {
		cfg.Warn = func(context.Context, string, ...any) {}
	}
	return &Service{
		objects: cfg.Objects, readers: cfg.Readers, actor: cfg.Actor,
		prefix: cfg.Prefix, maxBytes: cfg.MaxBytes,
		blobNotFound: cfg.BlobNotFound, warn: cfg.Warn,
	}, nil
}

// Guard verifies both the caller and key before any storage operation.
// It is exported for downstream wrappers that expose the same guard independently.
func (s *Service) Guard(ctx context.Context, key string) error { return s.guard(ctx, key) }

func (s *Service) guard(ctx context.Context, key string) error {
	actor, err := s.actor(ctx)
	if err != nil {
		return err
	}
	if s.readers == nil {
		s.warn(ctx, "object proxy reader list is not configured; deny", "actor", actor)
		return fmt.Errorf("%w: only staff may use the object proxy", persistmodel.ErrAccessDenied)
	}
	if !s.readers.CanRead(actor) {
		s.warn(ctx, "non-staff caller denied by object proxy", "actor", actor)
		return fmt.Errorf("%w: only staff may use the object proxy", persistmodel.ErrAccessDenied)
	}
	if !strings.HasPrefix(key, s.prefix) || strings.Contains(key, "..") {
		s.warn(ctx, "object proxy rejected an out-of-scope key", "actor", actor, "key", key)
		return fmt.Errorf("%w: object proxy only accepts keys under %s", persistmodel.ErrInvalidArgument, s.prefix)
	}
	return nil
}

// Get reads an object. A missing object maps to ErrNotFound for the storage-compatible 404.
func (s *Service) Get(ctx context.Context, key string) ([]byte, error) {
	if err := s.guard(ctx, key); err != nil {
		return nil, err
	}
	data, err := s.objects.Get(ctx, key)
	if err != nil {
		if s.blobNotFound(err) {
			return nil, fmt.Errorf("%w: object not found", persistmodel.ErrNotFound)
		}
		return nil, err
	}
	return data, nil
}

// Put writes one object within MaxBytes.
func (s *Service) Put(ctx context.Context, key string, content []byte, contentType string) error {
	if err := s.guard(ctx, key); err != nil {
		return err
	}
	if len(content) > s.maxBytes {
		return fmt.Errorf("%w: object exceeds %d bytes", persistmodel.ErrInvalidArgument, s.maxBytes)
	}
	return s.objects.Put(ctx, key, content, contentType)
}

// Delete removes an object. Missing objects are left to the idempotent storage implementation.
func (s *Service) Delete(ctx context.Context, key string) error {
	if err := s.guard(ctx, key); err != nil {
		return err
	}
	return s.objects.Delete(ctx, key)
}

// Head reports whether the object exists without treating absence as an error.
func (s *Service) Head(ctx context.Context, key string) (bool, error) {
	if err := s.guard(ctx, key); err != nil {
		return false, err
	}
	_, err := s.objects.Get(ctx, key)
	if err != nil {
		if s.blobNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
