// Service of the issue domain: what it needs in order to work.
//
// # Why the three dependencies look like this
//
// Besides its own Store, this domain needs three things from elsewhere. They are all injected as
// **functions**, rather than letting this package import core:
//
//	NextID      issues ids. The prefixes iss/cmt/att are decided by this package; the sequence is given by the wiring side
//	PutBlob     writes attachment bytes to object storage
//	ReadBlob    reads attachment bytes back from object storage by fileID
//
// The real implementations of the last two are in shared/infra/filebytes; core only forwards
// them too. **This package deliberately does not import filebytes directly**: that would drag in
// wiring details such as BlobStore and the files table, while this domain only cares about
// "store these bytes and get them back".
//
// # ⚠️ None of these three may be nil
//
// New checks explicitly and returns an error. Missing any one would show up as, respectively: a
// panic when issuing an id, a panic on attachment upload, a panic on attachment download. All of
// them are **on the request path**, not at startup. A wiring mistake must blow up at startup.

package issue

import (
	"context"
	"errors"

	"github.com/chyroc/ama/server/shared/gopkg/resource"
)

// Config is what is needed to build a Service.
type Config struct {
	// Store is this domain's persistence. Required.
	Store Store

	// NextID issues a new id with a prefix. Required.
	//
	// ⚠️ **The prefix is an observable contract**: iss_/cmt_/att_ show up in URLs, logs and
	// user screenshots, and are asserted by e2e. Changing the prefix means a whole new set of
	// ids; all old links break, with zero errors.
	NextID func(prefix string) string

	// PutBlob writes attachment bytes into object storage and returns a fileID that goes into the
	// file_id column of the business row (never empty).
	//
	// The bytes live only in object storage; the attachments table has no content column. When
	// object storage is not wired, it returns an error.
	PutBlob func(
		ctx context.Context, prefix, filename, contentType, author string, content []byte,
	) (string, error)

	// ReadBlob fetches attachment bytes from object storage by fileID.
	ReadBlob func(ctx context.Context, fileID string) ([]byte, error)
}

// Service is the business logic of the issue domain.
type Service struct {
	store    Store
	nextID   func(prefix string) string
	putBlob  func(ctx context.Context, prefix, filename, contentType, author string, content []byte) (string, error)
	readBlob func(ctx context.Context, fileID string) ([]byte, error)
}

// New creates a Service. None of the four dependencies may be missing; see the file header for why.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("issue: Store 不能为 nil")
	}
	if cfg.NextID == nil {
		return nil, errors.New("issue: NextID 不能为 nil")
	}
	if cfg.PutBlob == nil {
		return nil, errors.New("issue: PutBlob 不能为 nil")
	}
	if cfg.ReadBlob == nil {
		return nil, errors.New("issue: ReadBlob 不能为 nil")
	}
	return &Service{
		store:    cfg.Store,
		nextID:   cfg.NextID,
		putBlob:  cfg.PutBlob,
		readBlob: cfg.ReadBlob,
	}, nil
}

// RegisterResources fills this domain's three resources into the lookup table, so the
// authorization layer can find the owning workspace by id.
//
// Each domain fills its own entries. This used to be a 7-branch switch in core/resource.go that
// had to be edited every time a domain moved out. See the shared/gopkg/resource package header
// for why.
func (s *Service) RegisterResources(reg *resource.Registry) {
	reg.Register(resource.KindIssue, func(ctx context.Context, id string) (string, error) {
		iss, err := s.store.GetIssue(ctx, id)
		if err != nil {
			return "", resource.NotFound(err, "问题")
		}
		return iss.WorkspaceID, nil
	})
	reg.Register(resource.KindIssueComment, func(ctx context.Context, id string) (string, error) {
		c, err := s.store.GetIssueComment(ctx, id)
		if err != nil {
			return "", resource.NotFound(err, "评论")
		}
		return c.WorkspaceID, nil
	})
	reg.Register(resource.KindIssueAttachment, func(ctx context.Context, id string) (string, error) {
		// ⚠️ Use GetIssueAttachment (metadata only), **not** IssueAttachmentContent:
		// authorization only needs the owning workspace; fetching bytes from object storage would
		// move several MB for nothing, and do it on **every** image request; a page with 20
		// images means 20 times.
		att, err := s.store.GetIssueAttachment(ctx, id)
		if err != nil {
			return "", resource.NotFound(err, "附件")
		}
		return att.WorkspaceID, nil
	})
}
