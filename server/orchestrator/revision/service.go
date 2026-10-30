// Orchestration layer: revision history and rollback.

package revision

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Service is the set of methods handlers really use in this domain, **listing only the ones
// used**.
//
// This is a "narrow interface from the consumer's view": new methods added to the domain package
// (still core.Service for now) are invisible to handlers by default; to expose one you have to come
// to this file and add a line explicitly. What this saves is exactly those 161 empty forwarding
// methods: an interface only needs signatures, not function bodies.
//
// ⚠️ **The name is just Service; the package name carries the domain** (`issue.Service` /
// `ark.Service`). Previously nine interfaces were crammed into one package orchestrator and could
// only be told apart by prefix (IssueService / ArkService); after splitting into
// orchestrator/<domain>/ that prefix became redundant: the package name already says the domain.
//
// ⚠️ What the package name guards is **between domain interface declarations**: previously the
// nine interfaces shared a package, and a file in one domain could reference another domain's
// interface name with no error; now referencing it requires importing the sibling package, which
// is visible. **Blocking cross-domain access on the handler side does not rely on this**; that is
// handled by "field types are narrow interfaces"; see the comments on the ten Handler fields in
// apihandler/handler.go.
//
// ⚠️ **Signatures always use apimodel.X, never core.X.** This is not style but a hard rule: if
// this package imported core, core would show up in `go list -deps ./orchestrator/...`, the
// orchestration layer would depend again on the package it is supposed to hide, and inserting this
// layer would be pointless (full argument in the header of orchestrator/routes.go). Moving these
// types into domain in step 1 was exactly so these signatures could be written.
type Service interface {
	GetRevision(
		ctx context.Context, kind apimodel.RevisionKind, entityID, revisionID string,
	) (apimodel.Revision, error)
	ListRevisions(
		ctx context.Context, kind apimodel.RevisionKind, entityID string,
	) ([]apimodel.Revision, error)
	RollbackRevision(
		ctx context.Context, kind apimodel.RevisionKind, entityID, revisionID string,
	) (any, error)
}
