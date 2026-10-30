// Package userfile is the "account-level personal files" business domain: a logged-in user stores
// their own files in object storage, isolated by email, independent of workspaces.
//
// # Boundaries
//
//   - This package depends only on model (persistmodel/apimodel) and the standard library, not on
//     core / apihandler / shared/infra.
//     Both byte storage and metadata storage are injected as interfaces (store.go).
//   - The HTTP layer is in apihandler (handler_userfiles.go); wiring is in cmd/amad.
//
// # Owner isolation
//
// The first argument of every method is owner (lowercase email), taken by the HTTP layer from
// auth.ActorFrom(ctx). **This package does not accept an email claimed in the request body.** Every
// SQL in Store filters on owner_email, and other people's files always give ErrNotFound (404).
//
// # Two upload paths
//
// If the declared total size is ≤ PartSize (8 MiB), use simple: one PUT of the whole object. Larger
// (or undeclared) uses multipart: init → several PUT part → complete. The in-progress state
// (including the object storage uploadId) is stored in PostgreSQL, so part requests can land on any
// instance and continue, with no in-process state.
package userfile

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/chyroc/ama/server/model/persistmodel"
)

const (
	// idPrefix is the file id prefix (starts with uf_). It shows up in URLs and logs, so it is an
	// observable contract.
	idPrefix = "uf"

	// keyPrefix is the fixed domain prefix of object keys: `user-files/<email>/<id>`. It separates
	// "files stored for people" from issue-attachments/, skill-files/ and so on, so the storage
	// side can set lifecycle rules and track usage separately.
	keyPrefix = "user-files"

	// PartSize is the fixed multipart part size, and also the largest whole object allowed in
	// simple mode. 8 MiB: the S3-compatible part minimum is 5 MiB (except the last part); 8 is a
	// power of 2 and above the minimum. A 2 GiB file = 256 parts, far below the S3-compatible limit
	// of 10000 parts.
	PartSize int64 = 8 * 1024 * 1024

	// MaxFileSize is the total size limit per file, 2 GiB. On complete, the size read back from
	// object storage is used for the final check; the client's declared size is not trusted.
	MaxFileSize int64 = 2 * 1024 * 1024 * 1024

	// downloadTTL is how long a signed download link stays valid.
	downloadTTL = time.Hour

	// Default and hard limit for list paging.
	defaultListLimit = 100
	maxListLimit     = 1000

	// maxFilenameBytes is the file name length limit (in bytes; both the database column and common
	// file systems can handle it).
	maxFilenameBytes = 255
)

// ErrObjectNotFound means the object does not exist in object storage.
//
// It is a separate sentinel (instead of reusing persistmodel.ErrNotFound): "the row exists in the
// database but the object is gone" and "no row in the database" are two different things. The first
// is a storage inconsistency (5xx is more honest); the second is 404. The adapter must translate
// the object storage SDK's not found into this value so errors.Is recognizes it.
var ErrObjectNotFound = errors.New("userfile: 对象存储对象不存在")

// Business sentinels always **wrap the persistmodel sentinel of the same name**: apihandler.fail
// only recognizes those persistmodel ones, and a bare errors.New falls into default and becomes 500
// (this is pinned in service/host's service.go).
var (
	ErrNotFound        = fmt.Errorf("userfile: 不存在: %w", persistmodel.ErrNotFound)
	ErrConflict        = fmt.Errorf("userfile: 冲突: %w", persistmodel.ErrConflict)
	ErrInvalidArgument = fmt.Errorf("userfile: 参数不合法: %w", persistmodel.ErrInvalidArgument)
	ErrAccessDenied    = fmt.Errorf("userfile: 拒绝访问: %w", persistmodel.ErrAccessDenied)
)

// Service is the business logic of the personal files domain.
type Service struct {
	store   Store
	objects ObjectStore
	log     *slog.Logger
}

// Config wires up Service. Store and Objects are both required. Missing one is not a degradation;
// the whole domain is unusable, so New returns an error right away (missing wiring must blow up at
// startup, not panic on a request path).
type Config struct {
	Store   Store
	Objects ObjectStore
	Log     *slog.Logger
}

// New creates a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("userfile: Store 不能为 nil")
	}
	if cfg.Objects == nil {
		return nil, errors.New("userfile: ObjectStore 不能为 nil")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: cfg.Store, objects: cfg.Objects, log: log}, nil
}

// newID generates a random id with no central source (uf_ + 32 hex). Multiple instances write, so
// an in-process counter cannot be used (the distributed rule in CLAUDE.md); same as
// service/host.newID.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand can only fail when the system entropy source is unavailable; there is no safe
		// id to hand out if we keep going.
		panic(fmt.Sprintf("userfile: 读系统随机数失败: %v", err))
	}
	return idPrefix + "_" + hex.EncodeToString(b[:])
}
