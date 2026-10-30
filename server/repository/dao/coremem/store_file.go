// In-memory implementation of the files table.
//
// The contract is declared in core/store_file.go; the PG implementation is in repository/dao/coredao/store_pg_file.go.
//
// Only metadata is stored here; the bytes live in BlobStore (MemBlobStore for mem, S3 for PG).
// This matches the split on the PG side, so both implementations must behave exactly the same for these two fileStore methods,
// which is what running runStoreSuite against both is there to guarantee.
package coremem

import (
	"context"
	"fmt"

	"github.com/chyroc/ama/server/model/persistmodel"
)

func (s *MemStore) CreateFile(_ context.Context, rec persistmodel.FileRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.data.files[rec.ID]; exists {
		return fmt.Errorf("%w: 文件已存在", persistmodel.ErrConflict)
	}
	s.data.files[rec.ID] = rec
	return nil
}

func (s *MemStore) GetFile(
	_ context.Context, id string,
) (persistmodel.FileRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rec, ok := s.data.files[id]
	if !ok {
		return persistmodel.FileRecord{}, fmt.Errorf("%w: 文件不存在", persistmodel.ErrNotFound)
	}
	return rec, nil
}

// GetFiles fetches metadata in bulk. Contract (including "missing ids are not an error") in core/store_file.go.
//
// ⚠️ **Missing ids are skipped silently**, deliberately different from GetFile's ErrNotFound.
// The contract requires this difference; it is not the two implementations drifting apart. The PG version behaves exactly the same.
func (s *MemStore) GetFiles(
	_ context.Context, ids []string,
) (map[string]persistmodel.FileRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string]persistmodel.FileRecord, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if rec, ok := s.data.files[id]; ok {
			out[id] = rec
		}
	}
	return out, nil
}
