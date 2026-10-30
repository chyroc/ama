// Package filebytes reads and writes attachment bytes: **where the bytes are, and how to get them
// back**.
//
// # Split of work with the files table layer
//
// `core`'s `fileStore` (store_file.go) handles **metadata**: "which key in which bucket those
// bytes are at". This package handles **the bytes themselves**: actually writing to and reading
// from object storage. **Bytes live only in object storage; PG stores no attachment bytes at all**
// (the attachment table has no content column). The failure tradeoffs of the two are written in
// the file header of repository/schema/files.sql.
//
// # Why a separate package instead of dropping it into internal/util
//
// It **has business meaning**: it knows bytes and metadata are stored in two places, and that
// writes must go object first, database second. The rule for the util package is "knows nothing
// about this project"; this clearly does, so they cannot be mixed.
//
// # Why it was moved out of core (this is the whole reason the package exists)
//
// `Put` and `Read` are the two things in core that **really span resource domains**: the
// attachments of both the issue and suggestion domains go through them (core/issue.go and
// core/suggestion.go each call them once). When core is split by resource domain, unless they are
// taken out first, moving the first domain would take these two functions with it, and the other
// domain would stop compiling.
//
// The other places that use object storage (skill files, skill package zips, dev blobs, one-off
// migrations) **each belong to a single domain**, so they deliberately stay in core and were not
// moved along.
package filebytes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// ErrBlobNotFound is the sentinel that object storage implementations should wrap for "object
// does not exist".
//
// ⚠️⚠️ **There must be only one copy of this value; do not errors.New a synonym elsewhere.** The
// object storage implementation (the adapter layer in cmd/amad) must translate its "not found"
// into it, and the skill package cache (core.skillArchiveSize) recognizes it with `errors.Is` to
// detect a cache miss. Two different values would make that errors.Is **always false**, so every
// miss would be treated as "query error", silently throwing away the cache.
//
// The core side keeps an alias with the same name (`core.ErrBlobNotFound =
// filebytes.ErrBlobNotFound`); it is **the same variable**, not a copy, so errors.Is across
// packages still holds. This is the same approach as `core.ErrNotFound = persistmodel.ErrNotFound`.
//
// ⚠️ The "core:" prefix in the text is **kept on purpose**: that is what it was originally called;
// changing it to "filebytes:" would make the same kind of error inconsistent in production logs
// over time, and people searching logs would not know it is the same thing. Same as the note in
// storemodel/errors.go.
var ErrBlobNotFound = errors.New("core: 对象不存在")

// ErrNotConfigured means object storage is not wired. In PG mode amad refuses to start without
// object storage, so production never hits it; it only shows up in unit tests without a stand-in,
// or in wiring mistakes in mem mode.
//
// It wraps persistmodel.ErrPrecondition: the handler answers 412 instead of 500 based on it, and
// the error text says "object storage not configured" directly, so nobody has to guess.
var ErrNotConfigured = fmt.Errorf("%w: 没有装配对象存储（s3）", persistmodel.ErrPrecondition)

// IsBlobNotFound checks for "object does not exist".
func IsBlobNotFound(err error) bool { return errors.Is(err, ErrBlobNotFound) }

// Blobs is the outbound port for "where bytes are stored", with only the three methods this
// package uses.
//
// **Narrower than core.BlobStore** (no Delete; this package deletes nothing). The narrow
// interface is on purpose: a core.BlobStore passed in by the caller satisfies it implicitly, with
// no adapter code.
type Blobs interface {
	// Put writes an object.
	Put(ctx context.Context, key string, content []byte, contentType string) error
	// Get reads all bytes of an object. When the object does not exist, the returned error must
	// be recognized by IsBlobNotFound.
	Get(ctx context.Context, key string) ([]byte, error)
	// Bucket returns the bucket name, recorded when storing; see the comment on the files.bucket
	// column.
	Bucket() string
}

// Files is the metadata layer, again with only the two methods this package uses.
type Files interface {
	CreateFile(ctx context.Context, rec persistmodel.FileRecord) error
	GetFile(ctx context.Context, id string) (persistmodel.FileRecord, error)
}

// Store ties together "where bytes are stored" and "where metadata is stored", exposing only Put
// and Read.
//
// ⚠️ The Store in the name refers to **bytes**; it is not the same thing as core.Store (metadata,
// the whole persistence contract). See "Split of work with the files table layer" in this file's
// header.
type Store struct {
	blobs Blobs
	files Files
	// newID generates a new file id. It is supplied by the caller, because the id prefix is an
	// **observable contract** (see core's nextID) and should not be decided by this package.
	newID func() string
}

// New builds a Store.
//
// blobs **may be nil** (in unit tests without a stand-in); then Put / Read both return
// ErrNotConfigured. It **does not** quietly write bytes back to PG.
func New(blobs Blobs, files Files, newID func() string) *Store {
	return &Store{blobs: blobs, files: files, newID: newID}
}

// Enabled reports whether attachment bytes should go to object storage.
//
// ⚠️ The check is **the interface value is non-nil**, which is the opposite of the always-true
// kind of check this repo warns about: here blobs is only assigned when cmd/amad really built an
// object storage client, and is explicitly left nil when it could not be built (not configured,
// misconfigured). So it is not always true.
func (s *Store) Enabled() bool { return s.blobs != nil }

// ObjectKey builds the key of an attachment in object storage.
//
// Shape `<purpose>/<yyyy>/<mm>/<fileID>`; the reasons for the three parts are in the object_key
// comment in files.sql. Short version: the prefix lets the storage side set lifecycle rules per
// purpose, year/month sharding avoids piling up under one prefix, and the end is an unguessable id
// rather than **the file name the user typed** (that would need handling of path traversal and
// duplicate names).
//
// createdAt is an ISO string (apimodel.NowISO()) and year/month come from it; **not
// time.Now()**: that could make the createdAt of a record and the year/month in its key disagree
// at the instant a month rolls over.
//
// ⚠️ **There must be only one implementation of this shape.** Once an object key is written it
// cannot change (it is the only way to find those bytes); if two implementations drift apart, old
// objects can never be found again. The core side keeps `attachmentObjectKey` as an alias pointing
// to it, not a second implementation.
func ObjectKey(prefix, fileID, createdAt string) string {
	year, month := "0000", "00"
	// An ISO string always looks like 2026-09-01T10:00:00.000Z; the first 7 characters are
	// yyyy-mm.
	if len(createdAt) >= 7 && createdAt[4] == '-' {
		year, month = createdAt[0:4], createdAt[5:7]
	}
	return fmt.Sprintf("%s/%s/%s/%s", prefix, year, month, fileID)
}

// ContentSHA256 computes the content hash, lowercase hex. See the content_sha256 comment in
// files.sql for its purpose.
func ContentSHA256(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// Put writes attachment bytes to object storage and stores a files record.
//
// # Order: object first, then database. Not the other way round
//
// Failures in the two directions are not symmetric:
//
//	object first, then db, crash in between → an object nobody references in storage (**leak**, wastes a few MB)
//	db first, then object, crash in between → a record in the db whose bytes cannot be read (**corruption**, listed but 404 when opened)
//
// A leak is a cost; corruption is a failure users can see. So we prefer leaking. The full
// reasoning is in the file header of files.sql.
//
// The returned fileID goes into the file_id column of the business table and is **never empty**.
// Without object storage it returns ErrNotConfigured.
func (s *Store) Put(
	ctx context.Context, prefix, filename, contentType, author string, content []byte,
) (string, error) {
	if !s.Enabled() {
		return "", ErrNotConfigured
	}

	fileID := s.newID()
	createdAt := apimodel.NowISO()
	key := ObjectKey(prefix, fileID, createdAt)

	if err := s.blobs.Put(ctx, key, content, contentType); err != nil {
		return "", fmt.Errorf("core: 附件写对象存储失败: %w", err)
	}

	rec := persistmodel.FileRecord{
		ID:            fileID,
		Bucket:        s.blobs.Bucket(),
		ObjectKey:     key,
		Filename:      strings.TrimSpace(filename),
		ContentType:   contentType,
		Size:          len(content),
		ContentSHA256: ContentSHA256(content),
		Author:        author,
		CreatedAt:     createdAt,
	}
	if err := s.files.CreateFile(ctx, rec); err != nil {
		// The object is already written; a failure here leaves an orphan object. **We do not
		// delete it back**: the delete itself can fail too, and what matters most right now is to
		// report the error to the user truthfully, not to add another remote call on a path that
		// already failed.
		return "", fmt.Errorf("core: 附件元信息落库失败: %w", err)
	}
	return fileID, nil
}

// Read gets attachment bytes: it finds the files record by fileID, then fetches from object
// storage.
//
// **There is no bytea fallback**: bytes live only in object storage (the attachment table no
// longer has a content column). An empty fileID, a missing files row, or a missing object are all
// data inconsistencies, reported truthfully: the first two are ErrNotFound (attachment not
// readable), a missing object is ErrBlobNotFound (storage and database disagree).
func (s *Store) Read(ctx context.Context, fileID string) ([]byte, error) {
	if !s.Enabled() {
		return nil, ErrNotConfigured
	}
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return nil, fmt.Errorf("%w: 附件没有 file_id，找不到字节", persistmodel.ErrNotFound)
	}
	rec, err := s.files.GetFile(ctx, fileID)
	if err != nil {
		return nil, err
	}
	content, err := s.blobs.Get(ctx, rec.ObjectKey)
	if err != nil {
		return nil, fmt.Errorf("core: 读附件字节失败（file=%s）: %w", fileID, err)
	}
	return content, nil
}
