// Persistence contract for the files domain (the files table).
//
// It lives in its own file, embedded by the Store interface in store.go, for exactly the same
// reason as store_issue.go: store.go is the shared file every resource domain must touch, and
// a dozen concurrent sessions pass through it.
//
// ⚠️ **This domain handles only metadata, not bytes.** The bytes live in object storage and are
// handled by BlobStore (core/blob.go); what is stored here is "which key in which bucket holds
// those bytes". The split between the two and the failure trade-offs are written in the file
// header of repository/schema/files.sql.
//
// The two implementations are in repository/dao/coremem (memory) and repository/dao/coredao (PG).
package core

import (
	"context"

	"github.com/chyroc/ama/server/model/persistmodel"
)

// fileStore is the persistence contract for the files table.
//
// **There is no Delete**, on purpose: deleting an attachment only drops the reference (deletes
// the row in the business table); the files row and the object storage object both stay. The
// reason is in the "consistency between object storage objects and DB rows" section of
// files.sql: failures in the two directions are not symmetric. A leak is a cost, corruption is
// an outage, so we prefer leaking.
// If we ever want to reclaim leaks, we add a reconciliation job; only then is Delete needed.
//
// **There is no List**: no page needs to list "all files".
type fileStore interface {
	// CreateFile stores one file metadata record.
	//
	// A duplicate id returns ErrConflict, but that does not happen on the normal path, since
	// the id is freshly generated.
	CreateFile(ctx context.Context, rec persistmodel.FileRecord) error

	// GetFile gets metadata by id; returns ErrNotFound if it does not exist.
	GetFile(ctx context.Context, id string) (persistmodel.FileRecord, error)

	// GetFiles gets metadata for a batch of ids, **all in one query**.
	//
	// # Why a batch version instead of calling GetFile in a loop
	//
	// The read path for skill files needs **all** files of one skill at once (the hot path
	// that injects into the container, see hydrateSkillFiles in core/blob.go). Looping over
	// GetFile means N PG round trips (a 15-file skill means 15), while this path only needs
	// one SQL statement. Replacing "N round trips" with "one SQL + N concurrent object reads"
	// removes the amplification on the PG side.
	//
	// # Missing ids are **not an error**
	//
	// The returned map only holds the ids that were found; the caller decides what missing
	// ones mean. This is deliberately different from GetFile: that one is "open one
	// attachment", where not found means 404; this one fills in a batch, and the caller
	// defines what a missing entry means (the skill-file side treats it as an error and refuses
	// to hand out an incomplete skill).
	// Failing at this layer would also throw away the ones that could be read.
	//
	// When ids is empty it returns an empty map without hitting the DB.
	GetFiles(ctx context.Context, ids []string) (map[string]persistmodel.FileRecord, error)
}
