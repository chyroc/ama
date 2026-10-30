package core

import (
	"context"

	"github.com/chyroc/ama/server/shared/infra/filebytes"
)

// BlobStore is the outbound port for "where attachment bytes are stored".
//
// The implementation is S3-compatible object storage (shared/infra/s3, wired by the blobStore
// adapter in `cmd/amad`); AMA_STORE=mem and unit tests use coremem.MemBlobStore. **Bytes do not
// go into PostgreSQL**; PG stores only metadata (bucket + object_key in the files table).
// **Declared here instead of importing the implementation**: dependency inversion puts the
// interface on the consumer side (see model/persistmodel/README.md).
//
// **It can be nil** (when not wired in unit tests). Then all reads and writes of attachments and
// skill files fail with "no object storage wired"; there is no fallback to anywhere else.
type BlobStore interface {
	// Put writes one object.
	Put(ctx context.Context, key string, content []byte, contentType string) error
	// Get reads all bytes of one object. When the object does not exist, the returned error must
	// be recognized by IsBlobNotFound.
	Get(ctx context.Context, key string) ([]byte, error)
	// Delete removes one object; a missing object is not an error (idempotent).
	Delete(ctx context.Context, key string) error
	// Bucket returns the bucket name, recorded when saving to the database; see the comment on
	// the files.bucket column.
	Bucket() string
}

// ErrBlobNotFound is the sentinel a BlobStore implementation should wrap when "the object does
// not exist".
//
// The implementation is responsible for translating its own "not found" into this.
//
// **The real one is in shared/infra/filebytes**; this is only an alias with the same name.
// Reading and writing attachment bytes moved to that leaf package (see attachmentBytes below).
// The alias is **the same variable**, not a copy, so `errors.Is(err, core.ErrBlobNotFound)` is
// fully equal to the filebytes side.
//
// ⚠️⚠️ **Never errors.New a new one here.** Then the value the implementation returns and the
// value checked here are two different errors, `errors.Is` is always false, and the skill
// archive cache (skillArchiveSize) cannot recognize a miss: it treats every miss as a query error.
var ErrBlobNotFound = filebytes.ErrBlobNotFound

// isBlobNotFound checks "the object does not exist".
//
// It is a separate function so there is only one copy of the check: getting it wrong silently
// drops the fallback.
//
// The real one is also in shared/infra/filebytes; this is an alias.
var isBlobNotFound = filebytes.IsBlobNotFound

// blobEnabled reports whether attachment bytes should go to object storage.
//
// ⚠️ The check is **the interface value is non-nil**, which is the opposite of the always-true
// check this repo warns about: here `blobs` is only assigned when cmd/amad really builds an
// object storage client (in PG mode, amad refuses to start without object storage), and it is nil
// in unit tests with no fake wired. So it is not always true.
func (s *Service) blobEnabled() bool { return s.blobs != nil }

// attachmentObjectKey builds the object storage key for an attachment.
//
// **The real one is in shared/infra/filebytes (`filebytes.ObjectKey`)**; this is an alias.
// The shape `<purpose>/<yyyy>/<mm>/<fileID>` and the reason for each of the three parts are
// written there and in the object_key comment in files.sql.
//
// ⚠️ **Do not write a second implementation here.** Once an object key is written it cannot
// change (it is the only clue to find those bytes). If two implementations drift, old objects
// can never be found again.
var attachmentObjectKey = filebytes.ObjectKey

// Two purpose prefixes. **Separate instead of one shared attachments/**: the object storage side
// needs to count usage and set lifecycle rules separately, and splitting them later would mean
// moving objects.
const (
	blobPrefixIssueAttachment      = "issue-attachments"
	blobPrefixSuggestionAttachment = "suggestion-attachments"
)

// contentSHA256 computes the content hash as lowercase hex. For its use, see the content_sha256
// comment in files.sql.
//
// The real one is in shared/infra/filebytes; this is an alias.
var contentSHA256 = filebytes.ContentSHA256

// attachmentFiles wraps two things the Service holds (object storage, the files table) into a
// filebytes.Store, used by the two methods below.
//
// **Built fresh each time instead of stored as a field in New**: it only groups two existing
// pointers and has no state of its own. Storing a field adds a "forgot to build it at wiring time"
// failure path, and that path shows up as the whole attachment feature being silently unusable.
func (s *Service) attachmentFiles() *filebytes.Store {
	// ⚠️ When s.blobs is nil you **must pass nil in**, not an interface value wrapping nil.
	// The latter is non-nil, so filebytes.Enabled() is always true, and with no object storage
	// configured it still calls Put and panics on the spot (instead of returning "no object
	// storage wired"). This is exactly the "always-true check" the blobEnabled comment describes.
	if !s.blobEnabled() {
		return filebytes.New(nil, s.store, func() string { return s.nextID("file") })
	}
	return filebytes.New(s.blobs, s.store, func() string { return s.nextID("file") })
}

// putAttachmentBlob writes attachment bytes into object storage and saves a files record.
//
// **The real one is in shared/infra/filebytes (`Store.Put`)**; this only forwards. It was moved
// because **it spans resource domains**: the issue and suggestion domains each call it once, and
// core is being split into packages by resource domain. Without moving it, the first domain to
// move out would take it along and the other domain would stop compiling.
//
// The full argument for the "write the object first, then the database" order (the asymmetry of
// leaking vs corrupting) moved there too.
//
// The returned fileID goes into the file_id column of the business table and is never empty;
// it returns an error when no object storage is wired.
func (s *Service) putAttachmentBlob(
	ctx context.Context, prefix, filename, contentType, author string, content []byte,
) (string, error) {
	return s.attachmentFiles().Put(ctx, prefix, filename, contentType, author, content)
}

// attachmentBytes reads attachment bytes: it fetches them from object storage by fileID.
//
// **The real one is in shared/infra/filebytes (`Store.Read`)**; this only forwards, for the same
// reason as putAttachmentBlob: the issue and suggestion domains share it.
//
// There is no bytea fallback: the attachment table no longer has a content column, and the bytes
// live only in object storage.
func (s *Service) attachmentBytes(ctx context.Context, fileID string) ([]byte, error) {
	return s.attachmentFiles().Read(ctx, fileID)
}
