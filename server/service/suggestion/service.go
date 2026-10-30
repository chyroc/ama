// Service for the product suggestion (Suggestion) domain: what it needs to do its job.
//
// # Why the four dependencies look like this
//
// Besides its own Store, this domain needs three things from elsewhere. They are all injected as
// **functions**, instead of having this package import core:
//
//	NextID      issues ids. The sug/sre/sat prefixes are decided by this package; the sequence comes from the wiring side
//	PutBlob     writes attachment bytes to object storage
//	ReadBlob    reads attachment bytes back from object storage by fileID
//	IsStaff     whether this person is "platform staff" — **a fourth one only this domain has, see below**
//
// The real versions of the middle two are in shared/infra/filebytes; core also just forwards.
// **This package deliberately does not import filebytes directly**: that would drag in wiring
// details such as BlobStore and the files table, while this domain only cares about "store these
// bytes, then get them back".
//
// # ⚠️ IsStaff: the biggest difference from the issue domain
//
// Issue permission checks are done by authz at the HTTP boundary, per workspace; the domain
// package does not check. **Suggestions have no workspace** (see the Store package comment), so
// authz's "actor + workspace → role" model cannot express them. That is why this whole group is
// ActionNone in the matrix, and **the real gate is in this package** (suggestionForRead / only
// platform staff can change status). With the permission check in the domain package, it has to
// know "who is platform staff".
//
// **Injected as a function instead of moving the allowlist in here**, for two reasons:
//
//   - That list is shared with "problem feedback" (core.FeedbackReaders, wired from config by
//     cmd/amad). Moving it here would drag the feedback wiring along, and feedback has not moved
//     yet.
//   - **It must be asked fresh on every call.** The core-side implementation reads the
//     s.feedbackReaders field, and in tests that field is assigned after New
//     (core/migrateblob_test.go). Computing the answer in New and storing it would forever give
//     the "not wired" answer.
//
// ⚠️ **The fail-closed direction is guarded on the core side** (list not wired → nobody is
// platform staff). This package only asks yes or no and adds no fallback. Adding a "treat as true
// when unavailable" default here would turn "forgot to configure the list" into "everyone is
// platform staff", so anyone could read all suggestions submitted by everyone, with no error and
// a completely normal-looking UI.
//
// # ⚠️ None of these four may be nil
//
// New checks explicitly and returns an error. Missing one shows up, respectively, as: panic when
// issuing an id, panic on attachment upload, panic on attachment download, panic on the
// permission check — all **on the request path**, not at startup. Missing wiring must blow up at
// startup.

package suggestion

import (
	"context"
	"errors"
)

// Config holds what is needed to build a Service.
type Config struct {
	// Store is this domain's persistence. Required.
	Store Store

	// NextID issues a new id with a prefix. Required.
	//
	// ⚠️ **The prefix is an observable contract**: sug_/sre_/sat_ show up in URLs, logs, and user
	// screenshots. Changing the prefix means a new set of ids; all old links break with no error.
	NextID func(prefix string) string

	// PutBlob writes attachment bytes into object storage and returns the fileID that goes into
	// the business row's file_id column (never empty).
	//
	// Bytes live only in object storage; the attachment table has no content column. It returns
	// an error when no object storage is wired.
	PutBlob func(
		ctx context.Context, prefix, filename, contentType, author string, content []byte,
	) (string, error)

	// ReadBlob fetches attachment bytes from object storage by fileID.
	ReadBlob func(ctx context.Context, fileID string) ([]byte, error)

	// IsStaff reports whether this actor is platform staff. Required; see the file header for why.
	//
	// ⚠️ **It is called on every permission check**, so the implementation must handle that rate
	// (today it is one in-memory lookup). And because it is asked fresh, allowlist changes take
	// effect immediately — do not cache its answer in this package.
	IsStaff func(actor string) bool
}

// Service is the business logic of the product suggestion domain.
type Service struct {
	store    Store
	nextID   func(prefix string) string
	putBlob  func(ctx context.Context, prefix, filename, contentType, author string, content []byte) (string, error)
	readBlob func(ctx context.Context, fileID string) ([]byte, error)
	isStaff  func(actor string) bool
}

// New builds a Service. All four dependencies are required; see the file header for why.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("suggestion: Store 不能为 nil")
	}
	if cfg.NextID == nil {
		return nil, errors.New("suggestion: NextID 不能为 nil")
	}
	if cfg.PutBlob == nil {
		return nil, errors.New("suggestion: PutBlob 不能为 nil")
	}
	if cfg.ReadBlob == nil {
		return nil, errors.New("suggestion: ReadBlob 不能为 nil")
	}
	if cfg.IsStaff == nil {
		return nil, errors.New("suggestion: IsStaff 不能为 nil")
	}
	return &Service{
		store:    cfg.Store,
		nextID:   cfg.NextID,
		putBlob:  cfg.PutBlob,
		readBlob: cfg.ReadBlob,
		isStaff:  cfg.IsStaff,
	}, nil
}

// ⚠️ **This domain has no RegisterResources, and that is not an oversight.**
//
// The table in shared/gopkg/resource answers "which workspace does this id belong to", which authz
// uses to check membership. Suggestions are **platform-level** and have no workspace at all — its
// table does not even have a workspace_id column. So none of the three resource kinds
// (suggestion / reply / attachment) can be mapped back, and there is no slot to fill.
//
// ⚠️ **Do not register a slot that returns an empty string just to "match the issue shape"**:
// the resource package header states the one rule that would cause a security incident — an
// empty string makes authz/guard.go **let the request through**. This domain's gate is in the
// Service (suggestionForRead), not in authz; see the comment above the ActionNone group in
// authz/matrix.go.
