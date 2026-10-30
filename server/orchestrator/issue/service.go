// Orchestrator layer: issues (workspace level): status changes, comments, attachments.

package issue

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Service is the set of methods the handler actually uses in this domain. **Only the ones
// it uses are listed.**
//
// This is a "narrow interface from the consumer's side": new methods on the domain package
// (still core.Service for now) are invisible to the handler by default; to expose one, you
// must add a line to this file explicitly. What this saves is the 161 empty forwarding
// methods: an interface only needs signatures, not function bodies.
//
// ⚠️ **The name is just Service; the domain is carried by the package name**
// (`issue.Service` / `ark.Service`). Before, nine interfaces were packed into the same
// package orchestrator and could only be told apart by prefix (IssueService / ArkService);
// after splitting into orchestrator/<domain>/ that prefix became repetition, since the
// package name already says which domain it is.
//
// ⚠️ The package name only guards **between domain interface declarations**: when the nine
// interfaces shared a package, a file of one domain could use another domain's interface
// name with no error; now it has to import the sibling package, which is visible.
// **Blocking cross-domain access on the handler side does not rely on it**: that is done by
// "the field type is a narrow interface"; see the comments on the ten Handler fields in
// apihandler/handler.go.
//
// ⚠️ **Signatures always use apimodel.X, never core.X.** This is not style, it is a hard
// rule: if this package imports core, core shows up in `go list -deps ./orchestrator/...`,
// the orchestrator layer depends again on the package it is supposed to hide, and the layer
// is pointless (full reasoning in the file header of orchestrator/routes.go). Step 1 moved
// these types into domain exactly so that these signatures can be written.
type Service interface {
	CreateIssue(
		ctx context.Context, workspaceID string, draft apimodel.IssueDraft,
	) (apimodel.Issue, error)
	CreateIssueAttachment(
		ctx context.Context, issueID, filename string, content []byte,
	) (apimodel.IssueAttachment, error)
	CreateIssueComment(
		ctx context.Context, issueID, body string,
	) (apimodel.IssueComment, error)
	DeleteIssue(ctx context.Context, id string) error
	DeleteIssueAttachment(ctx context.Context, id string) error
	DeleteIssueComment(ctx context.Context, id string) error
	GetIssue(ctx context.Context, id string) (apimodel.Issue, error)
	IssueAttachmentContent(
		ctx context.Context, id string,
	) (apimodel.IssueAttachment, []byte, error)
	ListIssueAttachments(
		ctx context.Context, issueID string,
	) ([]apimodel.IssueAttachment, error)
	ListIssueComments(
		ctx context.Context, issueID string,
	) ([]apimodel.IssueComment, error)
	ListIssues(
		ctx context.Context, workspaceID string, status apimodel.IssueStatus,
	) ([]apimodel.Issue, error)
	UpdateIssue(
		ctx context.Context, id string, draft apimodel.IssueDraft,
	) (apimodel.Issue, error)
	UpdateIssueComment(
		ctx context.Context, id, body string,
	) (apimodel.IssueComment, error)
}
