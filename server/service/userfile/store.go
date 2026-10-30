// The two outbound ports the userfile domain needs: metadata in PostgreSQL (Store), bytes in
// object storage (ObjectStore).
//
// Same convention as service/host: this package only declares interfaces. The real
// implementations are in repository/dao/coredao, repository/dao/coremem and the adapters in
// cmd/amad, and they are wired up in cmd/amad.
// **This package does not import core, nor any object storage SDK** (hard infra rule: leaf
// packages are not depended on directly by business packages; object storage errors are
// translated by the adapter into this package's ErrObjectNotFound).
package userfile

import (
	"context"
	"io"
	"time"

	"github.com/chyroc/ama/server/model/persistmodel"
)

// Store is the read/write port for the user_files table.
//
// ⚠️⚠️ **Except for Create, the first filter of every method must be owner_email.**
// These interfaces have no workspace, so Guard cannot check a workspace role; owner isolation
// lives entirely in the SQL. Someone else's id always returns persistmodel.ErrNotFound (404,
// does not reveal that it exists), never 403.
type Store interface {
	// CreateUserFile inserts a row. Returns persistmodel.ErrConflict when (owner_email,
	// object_key) hits the unique constraint (normally the random id never collides; the
	// constraint is only a safety net).
	CreateUserFile(ctx context.Context, row persistmodel.UserFile) error

	// GetUserFile gets a row by (owner, id); returns persistmodel.ErrNotFound both when it does
	// not belong to that owner and when it does not exist.
	GetUserFile(ctx context.Context, ownerEmail, id string) (persistmodel.UserFile, error)

	// ListCompletedUserFiles lists the owner's completed files, by created_at **descending**.
	// limit <= 0 is turned into a default value by the implementation.
	ListCompletedUserFiles(ctx context.Context, ownerEmail string, limit int) ([]persistmodel.UserFile, error)

	// MarkUserFileCompleted moves a row from uploading to completed and writes the size.
	//
	// ⚠️ The SQL must include a `status='uploading'` condition (CAS): a repeated callback or a
	// concurrent completion must not write an already completed row again. When 0 rows are
	// affected, return persistmodel.ErrNotFound (or ErrConflict; the implementation tells them
	// apart by "does the row exist").
	MarkUserFileCompleted(ctx context.Context, ownerEmail, id string, size int64) error

	// DeleteUserFile deletes a row; a missing row is not an error (idempotent).
	DeleteUserFile(ctx context.Context, ownerEmail, id string) error
}

// ObjectStore is the outbound port for byte storage (the wiring side provides the adapter).
//
// The methods come in two groups: whole object (Put/Delete/Size/PresignedGetURL/Open/Bucket,
// for simple mode and for delete/download) and the four multipart methods (for multipart
// mode).
type ObjectStore interface {
	// Bucket returns the current bucket name, stored in the bucket column.
	Bucket() string

	// Put writes the whole object in one go (simple mode, within one part size).
	Put(ctx context.Context, key string, content []byte, contentType string) error
	// Delete deletes the object; a missing object is not an error (idempotent).
	Delete(ctx context.Context, key string) error
	// Size gets the object's byte count; when the object does not exist, the error must be
	// recognized by errors.Is(err, ErrObjectNotFound) (used to read back the size after
	// complete).
	Size(ctx context.Context, key string) (int64, error)
	// PresignedGetURL signs a time-limited download link for the **browser** (the caller sets
	// the TTL).
	PresignedGetURL(key string, ttl time.Duration) (string, error)
	// Open streams the whole object (for downloads relayed through amad), returns the content
	// and byte count; the caller must Close it.
	// When the object does not exist, the error must be recognized by
	// errors.Is(err, ErrObjectNotFound).
	Open(ctx context.Context, key string) (io.ReadCloser, int64, error)

	// InitMultipartUpload starts a multipart upload and returns the object storage uploadId.
	InitMultipartUpload(ctx context.Context, key, contentType string) (string, error)
	// UploadPart streams one part; size must be the real byte count of content.
	UploadPart(ctx context.Context, key, uploadID string, partNumber int, size int64, content io.Reader) error
	// CompleteMultipartUpload merges the uploaded parts; the implementation fetches the part
	// list from object storage itself and assembles them.
	CompleteMultipartUpload(ctx context.Context, key, uploadID string) error
	// AbortMultipartUpload abandons a multipart upload; a missing uploadId is not an error
	// (idempotent).
	AbortMultipartUpload(ctx context.Context, key, uploadID string) error
}
