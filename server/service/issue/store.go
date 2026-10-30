// The persistence contract of the Issue domain.
//
// # Declarations live with the domain, not in core/store.go
//
// At first only the **implementation** was moved out; the **declarations** of the 20 methods
// stayed in the Store interface in core/store.go. That was the same as not moving: adding a
// method to issue still meant editing store.go (1700+ lines), a shared file that every
// resource domain passes through and that many concurrent sessions touch. The cost of
// changes landing there is not "the file is big"; it is the **conflict surface**: two
// sessions each add something to their own domain, yet their diffs collide in the same hunk.
//
// Now the declarations live here, and core/store.go no longer even needs the embed line.
//
// # ⚠️ Neither the PG nor the in-memory implementation needs a single line changed
//
// This works because of Go's implicit interfaces: both implementations
// (repository/dao/coredao, coremem) already have the methods, so they satisfy this narrow
// interface automatically. **Do not add `var _ issue.Store = ...` assertions to them**:
// that would make dao import service/issue in reverse and glue back the direction that
// was just split apart. Writing the assertion at the wiring point (cmd/amad) is enough;
// that place already sees both sides.
package issue

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Store is every persistence method this domain uses, **listing only the ones it uses**.
//
// It is **not the same thing** as the Feedback group; do not mix the two sets of methods:
// that group is a machine-collected cross-workspace report stream (write-only, deduplicated
// by a unique index); this group is a human-written workspace-level entity (has status
// transitions, comments, and gets edited).
// See core/README-issue.md for the reasons.
type Store interface {
	// GetWorkspace checks whether a workspace exists.
	//
	// ⚠️ **This is the only "table owned by someone else" in this domain**; it is here on
	// purpose, not out of laziness.
	//
	// Two places use it: ListIssues (a missing workspace returns an empty list, not 404) and
	// CreateIssue (a missing workspace must be rejected). It only **reads a table**; it does
	// not call the workspace domain's Service, so it does not break the "zero imports between
	// domain packages" rule. Going through the orchestration layer instead would mean having
	// the orchestrator look up the workspace first and pass the result in, which adds a
	// layer for a single existence check and is not worth it.
	//
	// Rule: **reading another domain's table** may go into your own Store; **calling another
	// domain's Service** (which has business rules and side effects) must go up through the
	// orchestrator.
	GetWorkspace(ctx context.Context, id string) (apimodel.Workspace, error)

	// ListIssues lists the issues of a workspace, **in descending ord** (newest first).
	//
	// An empty status means no filter. No pagination: a workspace has far fewer issues than
	// run history, and pagination would only cost one more query for the "total count" the
	// frontend displays.
	ListIssues(ctx context.Context, workspaceID string, status apimodel.IssueStatus) ([]apimodel.Issue, error)
	GetIssue(ctx context.Context, id string) (apimodel.Issue, error)
	CreateIssue(ctx context.Context, iss apimodel.Issue) (apimodel.Issue, error)
	SaveIssue(ctx context.Context, iss apimodel.Issue) (apimodel.Issue, error)
	// DeleteIssue deletes an issue; not existing is not an error (idempotent).
	//
	// **Comments and attachments must be deleted with it**, otherwise orphaned data keeps
	// taking up the database. The attachment table stores bytes, so a missed delete costs
	// far more than elsewhere, and it is completely invisible (no page shows orphaned
	// attachments).
	DeleteIssue(ctx context.Context, id string) error

	// ListIssueComments lists the comments under an issue, **in ascending ord** (the
	// conversation reads top to bottom).
	//
	// This is the opposite of ListIssues' descending order, on purpose: the issue list wants
	// "newest first", while the discussion inside one issue should read in the order it
	// happened.
	ListIssueComments(ctx context.Context, issueID string) ([]apimodel.IssueComment, error)
	GetIssueComment(ctx context.Context, id string) (apimodel.IssueComment, error)
	CreateIssueComment(ctx context.Context, c apimodel.IssueComment) (apimodel.IssueComment, error)
	SaveIssueComment(ctx context.Context, c apimodel.IssueComment) (apimodel.IssueComment, error)
	// DeleteIssueComment deletes a comment; not existing is not an error (idempotent).
	DeleteIssueComment(ctx context.Context, id string) error

	// ListIssueAttachments lists the attachment metadata of an issue, **without bytes**, in
	// ascending ord.
	//
	// Leaving out the bytes is the key point: one issue may have 20 attachments of 5 MiB
	// each. Reading them all just to show file names on the detail page would make one page
	// load move 100 MB.
	ListIssueAttachments(ctx context.Context, issueID string) ([]apimodel.IssueAttachment, error)

	// GetIssueAttachment gets an attachment's **metadata** (without bytes).
	//
	// Authorization lookup goes through this: authz only needs workspace_id and does not
	// need to touch the bytes.
	GetIssueAttachment(ctx context.Context, id string) (apimodel.IssueAttachment, error)

	// CreateIssueAttachment only stores metadata; the bytes are already written to object storage by PutBlob (the row's file_id points to them).
	CreateIssueAttachment(ctx context.Context, att apimodel.IssueAttachment) (apimodel.IssueAttachment, error)
	// DeleteIssueAttachment deletes an attachment; not existing is not an error (idempotent).
	DeleteIssueAttachment(ctx context.Context, id string) error
}
