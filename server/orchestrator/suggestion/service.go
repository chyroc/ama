// Orchestration layer: product suggestions (users giving the platform suggestions; platform-level).

package suggestion

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// Service is the methods the handler really uses in this domain, **listing only the ones used**.
//
// This is a "narrow interface from the consumer's point of view": methods newly added to the domain
// package (still core.Service for now) are invisible to the handler by default; to open one you must
// add a line in this file explicitly. What this saves is those 161 empty forwarding methods: an
// interface only needs signatures, not function bodies.
//
// ⚠️ **The name is just Service; the domain is carried by the package name** (`issue.Service` /
// `ark.Service`). Before, nine interfaces were packed into the same package orchestrator and could
// only be told apart by prefixes (IssueService / ArkService); after splitting into
// orchestrator/<domain>/ the prefix became repetition, since the package name already says the
// domain.
//
// ⚠️ The package name only polices **between the domain interface declarations**: when the nine
// interfaces shared a package, one domain's file could reference another domain's interface name
// without any error; now that requires importing the sibling package, which is visible.
// **Cross-domain blocking on the handler side does not rely on it**: that is done by "field types are
// narrow interfaces"; see the comments on the ten fields of Handler in apihandler/handler.go.
//
// ⚠️ **Signatures always use apimodel.X, never core.X**. This is not style, it is a hard rule: if this
// package imported core, core would appear in `go list -deps ./orchestrator/...`, the orchestration
// layer would depend again on the package it is supposed to hide, and inserting this layer would be
// pointless (the full argument is in the file header of orchestrator/routes.go). Moving these types
// into domain earlier was done exactly so these signatures could be written.
type Service interface {
	CreateSuggestion(
		ctx context.Context, draft apimodel.SuggestionDraft,
	) (apimodel.Suggestion, error)
	CreateSuggestionAttachment(
		ctx context.Context, suggestionID, filename string, content []byte,
	) (apimodel.SuggestionAttachment, error)
	CreateSuggestionReply(
		ctx context.Context, suggestionID, body string,
	) (apimodel.SuggestionReply, error)
	DeleteSuggestion(ctx context.Context, id string) error
	DeleteSuggestionAttachment(ctx context.Context, id string) error
	DeleteSuggestionReply(ctx context.Context, id string) error
	GetSuggestion(ctx context.Context, id string) (apimodel.Suggestion, error)
	ListSuggestionAttachments(
		ctx context.Context, suggestionID string,
	) ([]apimodel.SuggestionAttachment, error)
	ListSuggestionReplies(
		ctx context.Context, suggestionID string,
	) ([]apimodel.SuggestionReply, error)
	ListSuggestions(
		ctx context.Context, q persistmodel.SuggestionQuery,
	) (apimodel.SuggestionPage, error)
	SuggestionAttachmentContent(
		ctx context.Context, id string,
	) (apimodel.SuggestionAttachment, []byte, error)
	UpdateSuggestion(
		ctx context.Context, id string, draft apimodel.SuggestionDraft,
	) (apimodel.Suggestion, error)
	UpdateSuggestionReply(
		ctx context.Context, id, body string,
	) (apimodel.SuggestionReply, error)
}
