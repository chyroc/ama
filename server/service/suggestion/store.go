// Persistence contract of the product suggestion (Suggestion) domain.
//
// # Declarations follow the domain; they do not stay in core/store.go
//
// The reason is exactly the same as in service/issue/store.go: core/store.go is a shared file
// every resource domain goes through, and a dozen-plus concurrent sessions pass through it.
// Piling one domain's 16 method declarations there costs not "a big file" but **conflict
// surface**: two sessions each move their own domain, yet their diffs collide in the same hunk.
//
// Now the declarations live here, and core/store.go only embeds one line: `suggestionsvc.Store`.
//
// # ⚠️ The PG and in-memory implementations need not change a single line
//
// This relies on Go's implicit interfaces: both implementations (repository/dao/coredao, coremem)
// have the methods and satisfy this narrow interface automatically. **Do not add
// `var _ suggestion.Store = ...` assertions to them**: that would make dao import
// service/suggestion in reverse, gluing back the direction that was just split. Asserting at the
// wiring point (cmd/amad) is enough; that place already sees both sides.
package suggestion

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// Store is all the persistence methods this domain uses, **listing only the ones used**.
//
// ⚠️ **This group is not the same thing as issue.Store, nor as the "problem feedback" (Feedback)
// group**; do not mix their methods:
//
//	feedback group  machine-collected cross-workspace report stream (write only, deduped by a unique index)
//	issue.Store     human-written **workspace-level** entities (state transitions, comments, read by workspace)
//	Store (this)    human-written **platform-level** opinions (state, replies, read by author)
//
// The full comparison of the three is in core/README-suggestion.md.
//
// ⚠️⚠️ **The easiest one to get wrong: no method in this group takes a workspaceID argument.**
// Suggestions belong to no workspace, and their table does not even have a workspace_id column.
// So this domain also **does not** have the GetWorkspace that issue.Store has (that is issue's
// only "table owned by someone else"). Before adding a workspace-level filter argument, read
// that README first.
type Store interface {
	// ListSuggestions lists suggestions by condition, **in descending ord** (newest first).
	//
	// The filters are all in SuggestionQuery, and its Author field **is also the authorization
	// implementation**: non-platform users only get rows where Author = themselves. ⚠️ This does
	// not mean Store understands authorization: the decision is in Service; Store just filters by
	// the given author faithfully.
	ListSuggestions(ctx context.Context, q persistmodel.SuggestionQuery) ([]apimodel.Suggestion, error)
	GetSuggestion(ctx context.Context, id string) (apimodel.Suggestion, error)
	CreateSuggestion(ctx context.Context, sg apimodel.Suggestion) (apimodel.Suggestion, error)
	SaveSuggestion(ctx context.Context, sg apimodel.Suggestion) (apimodel.Suggestion, error)
	// DeleteSuggestion deletes a suggestion; not existing is not an error (idempotent).
	//
	// **Replies and attachments must be deleted with it**, otherwise orphan data stays in the DB
	// forever. The attachments table stores bytes, and a missed delete is completely invisible
	// (no page ever shows orphan attachments).
	DeleteSuggestion(ctx context.Context, id string) error

	// ListSuggestionReplies lists the replies under a suggestion, **in ascending ord** (a
	// conversation reads top to bottom).
	//
	// This is the opposite of ListSuggestions' descending order, on purpose, same as on the issue side.
	ListSuggestionReplies(ctx context.Context, suggestionID string) ([]apimodel.SuggestionReply, error)
	GetSuggestionReply(ctx context.Context, id string) (apimodel.SuggestionReply, error)
	CreateSuggestionReply(ctx context.Context, r apimodel.SuggestionReply) (apimodel.SuggestionReply, error)
	SaveSuggestionReply(ctx context.Context, r apimodel.SuggestionReply) (apimodel.SuggestionReply, error)
	// DeleteSuggestionReply deletes a reply; not existing is not an error (idempotent).
	DeleteSuggestionReply(ctx context.Context, id string) error

	// ListSuggestionAttachments lists attachment metadata, **without bytes**, in ascending ord.
	ListSuggestionAttachments(ctx context.Context, suggestionID string) ([]apimodel.SuggestionAttachment, error)
	// GetSuggestionAttachment gets an attachment's **metadata** (without bytes).
	//
	// Authorization checks use this: they only need to know which suggestion it hangs under, not
	// the bytes.
	GetSuggestionAttachment(ctx context.Context, id string) (apimodel.SuggestionAttachment, error)
	// CreateSuggestionAttachment only stores metadata; the bytes are already written to object
	// storage by PutBlob (the row's file_id points to them).
	CreateSuggestionAttachment(ctx context.Context, att apimodel.SuggestionAttachment) (apimodel.SuggestionAttachment, error)
	// DeleteSuggestionAttachment deletes an attachment; not existing is not an error (idempotent).
	DeleteSuggestionAttachment(ctx context.Context, id string) error
}
