// Product suggestions (Suggestion): 14 forwarding methods. **The business logic has already
// moved to service/suggestion.**
//
// # These empty shells are a transition state, not a design
//
// The logic lives in service/suggestion; this file only forwards calls unchanged. The only reason
// to keep them is that **core must still back the methods that handlers cannot reach**: the
// orchestration layer's fallback interface (orchestrator.Fallback) requires all 14 methods on
// *core.Service:
//
//	cmd/amad/orchestrator.go var _ orchestrator.Fallback = (*core.Service)(nil)
//
// Remove the methods from *Service and that assertion fails to compile.
//
// So this step deliberately moves only the logic and leaves the method set alone: **the value of
// this step is "suggestion business logic no longer lives in core"**, and the check is that core
// does not appear in `go list -deps ./service/suggestion`.
// The 14 shells will be deleted together with the separate MR that moves handlers out of core.
//
// ⚠️ **Do not add logic here.** It looks like a normal suggestion.go, but any check written here
// bypasses service/suggestion: the two entry points give different answers for the same thing,
// and nothing reports an error. **This matters even more in this domain than on the issue side:
// all permission checks live in the domain package**
// (authz treats suggestions as ActionNone; Guard cannot express platform-level resources),
// so adding a check here means adding a second set of authorization rules. Add things to
// service/suggestion.

package core

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	suggestionsvc "github.com/chyroc/ama/server/service/suggestion"
)

// SuggestionAttachmentURL builds the access URL for an attachment. The real one is in
// service/suggestion.
//
// The name is kept because many places inside core and in comments refer to it, same as
// IssueAttachmentURL.
var SuggestionAttachmentURL = suggestionsvc.AttachmentURL

// isSuggestionStaff reports whether this actor is platform staff. **It is injected for
// service/suggestion to use.**
//
// The permission check itself is in the domain package (there is no workspace to resolve there,
// so authz cannot cover it), but the list of "who is platform staff" lives in core: it is the same
// list used by issue feedback (s.feedbackReaders, wired by cmd/amad from config). The reason is in
// core/README-suggestion.md: both are the same group of people doing the same job, and the real
// result of keeping two lists is that someone gets added to one and forgotten in the other, a
// failure with zero errors.
//
// ⚠️ **When not wired it returns false**, meaning "nobody is platform staff". Everyone can still
// submit suggestions and see their own, but nobody can see all of them and nobody can change
// status. This fail-closed direction matches ListFeedback, and here it must not be reversed even
// more so: defaulting to true would mean "forgot to configure the list" equals **everyone is
// platform staff**, so anyone could read every suggestion submitted across the whole company,
// with zero errors and a UI that looks completely normal.
//
// ⚠️ **This method must not be inlined into a boolean computed in New**: it reads the
// s.feedbackReaders field, and in tests that field is assigned after New.
// Asking on every call is what makes allowlist changes take effect right away.
func (s *Service) isSuggestionStaff(actor string) bool {
	if s.feedbackReaders == nil {
		return false
	}
	return s.feedbackReaders.CanRead(actor)
}

func (s *Service) ListSuggestions(
	ctx context.Context, q persistmodel.SuggestionQuery,
) (apimodel.SuggestionPage, error) {
	return s.suggestions.ListSuggestions(ctx, q)
}

func (s *Service) GetSuggestion(ctx context.Context, id string) (apimodel.Suggestion, error) {
	return s.suggestions.GetSuggestion(ctx, id)
}

func (s *Service) CreateSuggestion(
	ctx context.Context, draft apimodel.SuggestionDraft,
) (apimodel.Suggestion, error) {
	return s.suggestions.CreateSuggestion(ctx, draft)
}

func (s *Service) UpdateSuggestion(
	ctx context.Context, id string, draft apimodel.SuggestionDraft,
) (apimodel.Suggestion, error) {
	return s.suggestions.UpdateSuggestion(ctx, id, draft)
}

func (s *Service) DeleteSuggestion(ctx context.Context, id string) error {
	return s.suggestions.DeleteSuggestion(ctx, id)
}

func (s *Service) ListSuggestionReplies(
	ctx context.Context, suggestionID string,
) ([]apimodel.SuggestionReply, error) {
	return s.suggestions.ListSuggestionReplies(ctx, suggestionID)
}

func (s *Service) CreateSuggestionReply(
	ctx context.Context, suggestionID, body string,
) (apimodel.SuggestionReply, error) {
	return s.suggestions.CreateSuggestionReply(ctx, suggestionID, body)
}

func (s *Service) UpdateSuggestionReply(
	ctx context.Context, id, body string,
) (apimodel.SuggestionReply, error) {
	return s.suggestions.UpdateSuggestionReply(ctx, id, body)
}

func (s *Service) DeleteSuggestionReply(ctx context.Context, id string) error {
	return s.suggestions.DeleteSuggestionReply(ctx, id)
}

func (s *Service) ListSuggestionAttachments(
	ctx context.Context, suggestionID string,
) ([]apimodel.SuggestionAttachment, error) {
	return s.suggestions.ListSuggestionAttachments(ctx, suggestionID)
}

func (s *Service) CreateSuggestionAttachment(
	ctx context.Context, suggestionID, filename string, content []byte,
) (apimodel.SuggestionAttachment, error) {
	return s.suggestions.CreateSuggestionAttachment(ctx, suggestionID, filename, content)
}

func (s *Service) SuggestionAttachmentContent(
	ctx context.Context, id string,
) (apimodel.SuggestionAttachment, []byte, error) {
	return s.suggestions.SuggestionAttachmentContent(ctx, id)
}

func (s *Service) DeleteSuggestionAttachment(ctx context.Context, id string) error {
	return s.suggestions.DeleteSuggestionAttachment(ctx, id)
}
