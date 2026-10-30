// Issues: 13 forwarding methods. **The business logic has already moved to service/issue.**
//
// # These empty shells are a transition state, not a design
//
// The logic lives in service/issue; this file only passes calls through unchanged. The only
// reason to keep them is that **methods the handler cannot reach must still be covered by
// core**: the orchestrator layer's fallback interface (orchestrator.Fallback) requires all
// 13 methods on *core.Service:
//
//	cmd/amad/orchestrator.go var _ orchestrator.Fallback = (*core.Service)(nil)
//
// Remove the methods from *Service and that assertion fails to compile.
//
// So this step deliberately moves only the logic and keeps the method set: **the value of
// this step is "issue business logic no longer lives in core"**, and the check is that
// `go list -deps ./service/issue` does not contain core.
// The 13 shells will be deleted together with the separate change that "moves the handlers
// out of core". By then the handler uses the *issue.Service injected by the orchestrator
// layer, and this whole file goes away.
//
// ⚠️ **Do not add logic here.** It looks like a normal issue.go, but any check written here
// bypasses service/issue: the two entry points give different answers for the same thing,
// and nothing reports an error. Add things to service/issue/issue.go.

package core

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
	issuesvc "github.com/chyroc/ama/server/service/issue"
)

// IssueAttachmentURL builds the access URL of an attachment. The real one is in service/issue.
//
// The name is kept because several places inside core (handler_issue.go) and in comments
// refer to it.
var IssueAttachmentURL = issuesvc.AttachmentURL

func (s *Service) ListIssues(
	ctx context.Context, workspaceID string, status apimodel.IssueStatus,
) ([]apimodel.Issue, error) {
	return s.issues.ListIssues(ctx, workspaceID, status)
}

func (s *Service) GetIssue(ctx context.Context, id string) (apimodel.Issue, error) {
	return s.issues.GetIssue(ctx, id)
}

func (s *Service) CreateIssue(
	ctx context.Context, workspaceID string, draft apimodel.IssueDraft,
) (apimodel.Issue, error) {
	return s.issues.CreateIssue(ctx, workspaceID, draft)
}

func (s *Service) UpdateIssue(
	ctx context.Context, id string, draft apimodel.IssueDraft,
) (apimodel.Issue, error) {
	return s.issues.UpdateIssue(ctx, id, draft)
}

func (s *Service) DeleteIssue(ctx context.Context, id string) error {
	return s.issues.DeleteIssue(ctx, id)
}

func (s *Service) ListIssueComments(
	ctx context.Context, issueID string,
) ([]apimodel.IssueComment, error) {
	return s.issues.ListIssueComments(ctx, issueID)
}

func (s *Service) CreateIssueComment(
	ctx context.Context, issueID, body string,
) (apimodel.IssueComment, error) {
	return s.issues.CreateIssueComment(ctx, issueID, body)
}

func (s *Service) UpdateIssueComment(
	ctx context.Context, id, body string,
) (apimodel.IssueComment, error) {
	return s.issues.UpdateIssueComment(ctx, id, body)
}

func (s *Service) DeleteIssueComment(ctx context.Context, id string) error {
	return s.issues.DeleteIssueComment(ctx, id)
}

func (s *Service) ListIssueAttachments(
	ctx context.Context, issueID string,
) ([]apimodel.IssueAttachment, error) {
	return s.issues.ListIssueAttachments(ctx, issueID)
}

func (s *Service) CreateIssueAttachment(
	ctx context.Context, issueID, filename string, content []byte,
) (apimodel.IssueAttachment, error) {
	return s.issues.CreateIssueAttachment(ctx, issueID, filename, content)
}

func (s *Service) IssueAttachmentContent(
	ctx context.Context, id string,
) (apimodel.IssueAttachment, []byte, error) {
	return s.issues.IssueAttachmentContent(ctx, id)
}

func (s *Service) DeleteIssueAttachment(ctx context.Context, id string) error {
	return s.issues.DeleteIssueAttachment(ctx, id)
}
