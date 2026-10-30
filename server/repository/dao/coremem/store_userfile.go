// In-memory implementation of the user_files table; behaves the same as coredao/store_pg_userfile.go case by case:
// owner isolation, CAS to completed, ordered by (created_at, id) descending.
// Everything runs under the single lock s.mu (the mem version is single-process, so a lock is enough).
package coremem

import (
	"context"
	"sort"

	"github.com/chyroc/ama/server/model/persistmodel"
)

// CreateUserFile follows the PG contract: a clash on (owner, object_key) returns ErrConflict.
func (s *MemStore) CreateUserFile(_ context.Context, row persistmodel.UserFile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, old := range s.data.userFiles {
		if old.OwnerEmail == row.OwnerEmail && old.ObjectKey == row.ObjectKey {
			return persistmodel.ErrConflict
		}
	}
	s.data.userFiles = append(s.data.userFiles, row)
	return nil
}

// GetUserFile returns ErrNotFound both when the row belongs to another owner and when it does not exist (does not reveal existence).
func (s *MemStore) GetUserFile(
	_ context.Context, ownerEmail, id string,
) (persistmodel.UserFile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, f := range s.data.userFiles {
		if f.OwnerEmail == ownerEmail && f.ID == id {
			return f, nil
		}
	}
	return persistmodel.UserFile{}, persistmodel.ErrNotFound
}

// ListCompletedUserFiles returns only this owner's completed rows, ordered by (created_at, id) descending.
func (s *MemStore) ListCompletedUserFiles(
	_ context.Context, ownerEmail string, limit int,
) ([]persistmodel.UserFile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]persistmodel.UserFile, 0)
	for _, f := range s.data.userFiles {
		if f.OwnerEmail == ownerEmail && f.Status == persistmodel.UserFileStatusCompleted {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].ID > out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// MarkUserFileCompleted is a CAS: it only changes the row if it exists and is still uploading.
// When 0 rows are affected it tells 404 and 409 apart the same way as the PG version.
func (s *MemStore) MarkUserFileCompleted(
	_ context.Context, ownerEmail, id string, size int64,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.userFiles {
		f := &s.data.userFiles[i]
		if f.OwnerEmail != ownerEmail || f.ID != id {
			continue
		}
		if f.Status != persistmodel.UserFileStatusUploading {
			return persistmodel.ErrConflict
		}
		f.Status = persistmodel.UserFileStatusCompleted
		f.Size = size
		return nil
	}
	return persistmodel.ErrNotFound
}

// DeleteUserFile is idempotent: missing also counts as success.
func (s *MemStore) DeleteUserFile(_ context.Context, ownerEmail, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.data.userFiles[:0]
	for _, f := range s.data.userFiles {
		if f.OwnerEmail == ownerEmail && f.ID == id {
			continue
		}
		kept = append(kept, f)
	}
	s.data.userFiles = kept
	return nil
}
