package coremem

import (
	"context"
	"strings"
	"sync"

	"github.com/chyroc/ama/server/shared/infra/filebytes"
)

// MemBlobStore is the in-memory implementation of core.BlobStore (this package does not import core, to avoid an import cycle in core's tests). Used for AMA_STORE=mem and unit tests.
//
// ⚠️ Lost on process restart, and each instance keeps its own copy. Same tier as the in-memory Store: only fit for a single local process.
type MemBlobStore struct {
	mu   sync.RWMutex
	objs map[string][]byte
}

// NewMemBlobStore creates an empty in-memory byte store.
func NewMemBlobStore() *MemBlobStore {
	return &MemBlobStore{objs: map[string][]byte{}}
}

// Put writes a copy of the bytes (copied, so later changes to the caller's slice do not affect it). Same key overwrites.
func (m *MemBlobStore) Put(_ context.Context, key string, content []byte, _ string) error {
	cp := append([]byte(nil), content...)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[strings.TrimSpace(key)] = cp
	return nil
}

// Get returns a copy of the bytes. Returns filebytes.ErrBlobNotFound (that is, core.ErrBlobNotFound) if missing.
func (m *MemBlobStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.objs[strings.TrimSpace(key)]
	if !ok {
		return nil, filebytes.ErrBlobNotFound
	}
	return append([]byte(nil), b...), nil
}

// Delete removes the bytes. Missing is not an error.
func (m *MemBlobStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, strings.TrimSpace(key))
	return nil
}

// Bucket is the label written into files.bucket.
func (m *MemBlobStore) Bucket() string { return "mem" }
