// Business logic for issues (Issue): create/read/update/delete + comments + attachments.
//
// # It is not the same thing as "problem feedback" (core/feedback.go)
//
// The names and domains look alike (both are "something is wrong"), but they are **exactly
// opposite on four axes**, and merging them would break feedback's core invariant. The full
// comparison is in core/README-issue.md and at the top of schema_issue_pg.sql; here is only a
// one-line test:
//
//	feedback  collected by machine, content rebuilt from an allowlist, cross-workspace, deduplicated by unique index, write-only
//	issue     written by people, free text stored as is, workspace-level, cannot be deduplicated, has status flow and comments
//
// # Three rules that run through this file
//
// **1. The author only comes from the context.** The request body has no author field, and should
// not: the body is something the caller can change, and accepting it would allow "filing an issue
// in someone else's name". This is the same rule as feedback / onboarding / user-preferences (see
// authz/README.md).
//
// **2. Timestamps are set by Service, never trusted from the caller.** Changing title/body/
// status/priority all refresh UpdatedAt; letting the handler or frontend pass it means one missed
// spot gives "it was changed but the list still shows the old time".
//
// **3. Validation lives in this layer, not in the handler.** The handler only converts the
// protocol, so HTTP and any future entry point share the same checks.

package issue

import (
	"context"
	"fmt"
	"strings"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/gopkg/attachment"
	"github.com/chyroc/ama/server/shared/gopkg/humanize"
	"github.com/chyroc/ama/server/shared/gopkg/mdtitle"
)

// maxBodyLen and maxCommentLen are the length limits for body and comments.
//
// **They are required**: these two fields are free user input; without a limit, a single request
// could stuff hundreds of MB into PG.
//
// The truncation policy is **deliberately the opposite of feedback's: here we reject, we do not
// truncate**. feedback truncates because it is reported automatically, and losing part of the
// debug info still leaves it useful; here it is something the user wrote by hand, and quietly
// cutting half of it destroys their input while they think the submit succeeded.
const (
	maxBodyLen    = 64 * 1024
	maxCommentLen = 16 * 1024
)

// blobPrefix is the purpose prefix of attachments in object storage.
//
// ⚠️ **This string must not change.** It is part of the object key
// (`<purpose>/<yyyy>/<mm>/<fileID>`); after changing it, old attachments can never be found
// again, with no error at all, just "object does not exist" on download.
//
// It is separate from suggestion's "suggestion-attachments" instead of sharing one attachments/:
// the storage side must be able to measure usage and configure lifecycle separately, and once
// merged, splitting again means moving objects.
//
// ⚠️ core/blob.go has another copy with the same value, blobPrefixIssueAttachment, used by the
// attachment migration (core/migrateblob.go). **The two must match**; changing one breaks half.
const blobPrefix = "issue-attachments"

// actorFrom takes the caller's email from the context.
//
// Like feedback / suggestion, it lowercases once more (defense in depth): the auth entry already
// lowercases, but the author field is compared with the member table ("did I file this one"), and
// a case mismatch would make the same person look like two people.
func actorFrom(ctx context.Context) (string, error) {
	actor := strings.TrimSpace(auth.ActorFrom(ctx))
	if actor == "" {
		return "", fmt.Errorf("%w: 需要先登录", persistmodel.ErrInvalidArgument)
	}
	return strings.ToLower(actor), nil
}

// ListIssues lists the issues of a workspace. An empty status means no filter.
func (s *Service) ListIssues(
	ctx context.Context, workspaceID string, status apimodel.IssueStatus,
) ([]apimodel.Issue, error) {
	if status != "" && !apimodel.ValidIssueStatus(status) {
		return nil, fmt.Errorf("%w: 未知的状态 %q", persistmodel.ErrInvalidArgument, status)
	}
	// When the workspace does not exist, **return an empty list, not 404**: "list endpoints never
	// 404", see the file header of core/handler.go. Returning 404 would make the page throw instead
	// of showing an empty list.
	if _, err := s.store.GetWorkspace(ctx, workspaceID); err != nil {
		return []apimodel.Issue{}, nil
	}
	return s.store.ListIssues(ctx, workspaceID, status)
}

// GetIssue fetches one issue. Returns ErrNotFound if missing (the handler turns it into 404 + code=not_found).
func (s *Service) GetIssue(ctx context.Context, id string) (apimodel.Issue, error) {
	return s.store.GetIssue(ctx, id)
}

// CreateIssue creates an issue.
//
// **At least one of title and body is required**; this is the only required-field rule in this
// domain: with both empty, the record has no content at all, and the list gets a row that shows
// nothing when opened.
func (s *Service) CreateIssue(
	ctx context.Context, workspaceID string, draft apimodel.IssueDraft,
) (apimodel.Issue, error) {
	actor, err := actorFrom(ctx)
	if err != nil {
		return apimodel.Issue{}, err
	}
	if _, err := s.store.GetWorkspace(ctx, workspaceID); err != nil {
		return apimodel.Issue{}, fmt.Errorf("%w: 空间不存在", persistmodel.ErrNotFound)
	}

	title, err := validateNew(draft)
	if err != nil {
		return apimodel.Issue{}, err
	}

	status := draft.Status
	if status == "" {
		status = apimodel.IssueOpen
	}
	priority := draft.Priority
	if priority == "" {
		// Defaults to P2 if not set. **Not P0 by default**: defaulting to the top level means every
		// new issue is shouting "most urgent", and P0 loses its meaning right away. See
		// apimodel.DefaultIssuePriority.
		priority = apimodel.DefaultIssuePriority
	}

	now := apimodel.NowISO()
	return s.store.CreateIssue(ctx, apimodel.Issue{
		ID:          s.nextID("iss"),
		WorkspaceID: workspaceID,
		Title:       title,
		Body:        strings.TrimSpace(draft.Body),
		Status:      status,
		Priority:    priority,
		Author:      actor,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
}

// UpdateIssue changes an issue. All four fields are optional (changing only the status is the
// most common operation).
//
// # Telling "do not change this" from "clear this"
//
//	Status / Priority empty string  =  do not change (the enum has no valid "empty" value)
//	Title / Body empty string       =  really clear it
//
// The latter brings a case that must be handled: **when the title is cleared, derive it again from
// the body**, instead of leaving an empty title. Likewise, when the body is cleared and the title
// is also empty, reject.
//
// # ⚠️ But "neither was passed" does not mean "clear both"
//
// The rule above is right for **the frontend** (it always sends back the full object, see
// patchMeta in IssueDetailPage), but not for API / CLI callers: `PATCH {"status":"closed"}` is the
// most natural way to write it, and the original implementation read it as "clear both title and
// body". mdtitle.Derive then got an empty string, the whole request was rejected, and the error
// was **"please fill in at least a title or a body"**, a message that makes no sense at all (I
// just wanted to close this issue).
//
// This cannot be expressed with pointer fields on IssueDraft (it is a value type, see the
// trade-off on the domain side), so here the check is "both are empty strings": in that case
// **keep the original values** instead of clearing. The cost is that "clear title and body at the
// same time" can no longer be expressed, but it would have been rejected anyway (an empty record
// is not valid), so no real use case is hurt.
func (s *Service) UpdateIssue(
	ctx context.Context, id string, draft apimodel.IssueDraft,
) (apimodel.Issue, error) {
	if _, err := actorFrom(ctx); err != nil {
		return apimodel.Issue{}, err
	}
	cur, err := s.store.GetIssue(ctx, id)
	if err != nil {
		return apimodel.Issue{}, err
	}

	next := cur
	next.Title = strings.TrimSpace(draft.Title)
	next.Body = strings.TrimSpace(draft.Body)

	// Neither passed = leave title and body alone this time (typically only status / priority
	// changes), see the function comment. **Must not be read as "clear both"**, which would reject
	// a pure status change.
	if next.Title == "" && next.Body == "" {
		next.Title, next.Body = cur.Title, cur.Body
	}

	// If the title is empty, derive it again from the (new) body; reject if both are empty.
	title := mdtitle.Derive(next.Title, next.Body)
	if title == "" {
		return apimodel.Issue{}, errEmpty()
	}
	next.Title = title

	if err := checkBodyLen(next.Body); err != nil {
		return apimodel.Issue{}, err
	}

	if draft.Status != "" {
		if !apimodel.ValidIssueStatus(draft.Status) {
			return apimodel.Issue{}, fmt.Errorf(
				"%w: 未知的状态 %q", persistmodel.ErrInvalidArgument, draft.Status)
		}
		next.Status = draft.Status
	}
	if draft.Priority != "" {
		if !apimodel.ValidIssuePriority(draft.Priority) {
			return apimodel.Issue{}, fmt.Errorf(
				"%w: 未知的优先级 %q", persistmodel.ErrInvalidArgument, draft.Priority)
		}
		next.Priority = draft.Priority
	}

	// Changing anything refreshes UpdatedAt, set in one place by this layer.
	next.UpdatedAt = apimodel.NowISO()
	return s.store.SaveIssue(ctx, next)
}

// DeleteIssue deletes an issue (comments and attachments are deleted by Store in cascade).
func (s *Service) DeleteIssue(ctx context.Context, id string) error {
	return s.store.DeleteIssue(ctx, id)
}

// validateNew validates create input and returns the final title.
func validateNew(draft apimodel.IssueDraft) (string, error) {
	body := strings.TrimSpace(draft.Body)
	if err := checkBodyLen(body); err != nil {
		return "", err
	}
	// mdtitle.Derive returns an empty string when both are empty (it deliberately has no fallback, see shared/gopkg/mdtitle/mdtitle.go).
	title := mdtitle.Derive(draft.Title, body)
	if title == "" {
		return "", errEmpty()
	}
	if draft.Status != "" && !apimodel.ValidIssueStatus(draft.Status) {
		return "", fmt.Errorf("%w: 未知的状态 %q", persistmodel.ErrInvalidArgument, draft.Status)
	}
	if draft.Priority != "" && !apimodel.ValidIssuePriority(draft.Priority) {
		return "", fmt.Errorf("%w: 未知的优先级 %q", persistmodel.ErrInvalidArgument, draft.Priority)
	}
	return title, nil
}

// errEmpty is the error for "both title and body are empty".
//
// It has its own constructor so this message exists in one place only: it appears on both the
// create and update paths, and LocalApiClient copies it word for word (the frontend shows
// e.message directly, and e2e asserts the exact text).
func errEmpty() error {
	return fmt.Errorf("%w: 请至少填写标题或正文", persistmodel.ErrInvalidArgument)
}

func checkBodyLen(body string) error {
	if len(body) > maxBodyLen {
		return fmt.Errorf("%w: 正文超过 %s 上限",
			persistmodel.ErrInvalidArgument, humanize.HumanBytes(maxBodyLen))
	}
	return nil
}

// -- Comments -----------------------------------------------------------------

// ListIssueComments lists the comments under an issue.
//
// When the issue does not exist, **return an empty list, not 404** ("list endpoints never 404").
func (s *Service) ListIssueComments(
	ctx context.Context, issueID string,
) ([]apimodel.IssueComment, error) {
	if _, err := s.store.GetIssue(ctx, issueID); err != nil {
		return []apimodel.IssueComment{}, nil
	}
	return s.store.ListIssueComments(ctx, issueID)
}

// CreateIssueComment adds a comment.
func (s *Service) CreateIssueComment(
	ctx context.Context, issueID, body string,
) (apimodel.IssueComment, error) {
	actor, err := actorFrom(ctx)
	if err != nil {
		return apimodel.IssueComment{}, err
	}
	// The issue must exist: the comment hangs under it, and workspace_id is inherited from it
	// (**not taken from the request**: that is something the caller can change, and taking it would
	// allow putting comments into someone else's workspace).
	iss, err := s.store.GetIssue(ctx, issueID)
	if err != nil {
		return apimodel.IssueComment{}, err
	}

	body = strings.TrimSpace(body)
	if err := validateCommentBody(body); err != nil {
		return apimodel.IssueComment{}, err
	}

	now := apimodel.NowISO()
	return s.store.CreateIssueComment(ctx, apimodel.IssueComment{
		ID:          s.nextID("cmt"),
		IssueID:     issueID,
		WorkspaceID: iss.WorkspaceID,
		Body:        body,
		Author:      actor,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
}

// UpdateIssueComment changes a comment's body and refreshes UpdatedAt (the frontend uses it to show "edited").
func (s *Service) UpdateIssueComment(
	ctx context.Context, id, body string,
) (apimodel.IssueComment, error) {
	if _, err := actorFrom(ctx); err != nil {
		return apimodel.IssueComment{}, err
	}
	cur, err := s.store.GetIssueComment(ctx, id)
	if err != nil {
		return apimodel.IssueComment{}, err
	}

	body = strings.TrimSpace(body)
	if err := validateCommentBody(body); err != nil {
		return apimodel.IssueComment{}, err
	}

	cur.Body = body
	cur.UpdatedAt = apimodel.NowISO()
	return s.store.SaveIssueComment(ctx, cur)
}

// DeleteIssueComment deletes a comment.
func (s *Service) DeleteIssueComment(ctx context.Context, id string) error {
	return s.store.DeleteIssueComment(ctx, id)
}

// validateCommentBody validates a comment body.
//
// **Empty comments are rejected**: unlike an issue, a comment has no title to fall back on, and an
// empty comment on the timeline is just an empty bubble that only makes people think the UI is
// broken.
func validateCommentBody(body string) error {
	if body == "" {
		return fmt.Errorf("%w: 评论内容不能为空", persistmodel.ErrInvalidArgument)
	}
	if len(body) > maxCommentLen {
		return fmt.Errorf("%w: 评论超过 %s 上限",
			persistmodel.ErrInvalidArgument, humanize.HumanBytes(maxCommentLen))
	}
	return nil
}

// -- Attachments --------------------------------------------------------------

// ListIssueAttachments lists the attachment metadata of an issue (without bytes).
func (s *Service) ListIssueAttachments(
	ctx context.Context, issueID string,
) ([]apimodel.IssueAttachment, error) {
	if _, err := s.store.GetIssue(ctx, issueID); err != nil {
		return []apimodel.IssueAttachment{}, nil
	}
	list, err := s.store.ListIssueAttachments(ctx, issueID)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].URL = AttachmentURL(list[i].ID)
	}
	return list, nil
}

// CreateIssueAttachment stores an attachment.
//
// content is **untrusted input**: the type is decided by server-side sniffing (the client's claim
// is not trusted), and both size and count have limits. The reasons for the three checks are at
// the top of shared/gopkg/attachment/attachment.go.
func (s *Service) CreateIssueAttachment(
	ctx context.Context, issueID, filename string, content []byte,
) (apimodel.IssueAttachment, error) {
	actor, err := actorFrom(ctx)
	if err != nil {
		return apimodel.IssueAttachment{}, err
	}
	// Same as comments: workspace_id is inherited from the issue, not taken from the request.
	iss, err := s.store.GetIssue(ctx, issueID)
	if err != nil {
		return apimodel.IssueAttachment{}, err
	}

	if len(content) > attachment.MaxBytes {
		return apimodel.IssueAttachment{}, attachment.ErrTooLarge
	}

	// Count limit. **Check-then-write is acceptable here**: under concurrency across instances the
	// worst result is an issue with 21 attachments, which affects neither correctness nor security.
	// Letting a unique constraint decide would need numbering "which attachment this is", adding
	// real complexity for an unimportant constraint.
	existing, err := s.store.ListIssueAttachments(ctx, issueID)
	if err != nil {
		return apimodel.IssueAttachment{}, err
	}
	if len(existing) >= attachment.MaxCount {
		return apimodel.IssueAttachment{}, attachment.ErrTooMany
	}

	contentType, err := attachment.DetectType(filename, content)
	if err != nil {
		return apimodel.IssueAttachment{}, err
	}

	// The bytes go into object storage first, and the returned fileID is stored in the business
	// row (PG only holds metadata).
	//
	// ⚠️ **The order is object first, database second**; for the trade-off on the failure
	// direction, see filebytes.Store.Put.
	fileID, err := s.putBlob(ctx, blobPrefix, filename, contentType, actor, content)
	if err != nil {
		return apimodel.IssueAttachment{}, err
	}

	att, err := s.store.CreateIssueAttachment(ctx, apimodel.IssueAttachment{
		ID:          s.nextID("att"),
		IssueID:     issueID,
		WorkspaceID: iss.WorkspaceID,
		FileID:      fileID,
		// The file name is stored as is (trim only): cleaning happens **when it is sent out**
		// (attachment.Disposition), not when it is stored. Cleaning on store would make the user
		// see a changed name in the list without knowing why.
		Filename:    strings.TrimSpace(filename),
		ContentType: contentType,
		Size:        len(content),
		Author:      actor,
		CreatedAt:   apimodel.NowISO(),
	})
	if err != nil {
		return apimodel.IssueAttachment{}, err
	}
	att.URL = AttachmentURL(att.ID)
	return att, nil
}

// GetIssueAttachment fetches attachment metadata (without bytes). Authorization resolution and the pre-download check both use it.
func (s *Service) GetIssueAttachment(
	ctx context.Context, id string,
) (apimodel.IssueAttachment, error) {
	att, err := s.store.GetIssueAttachment(ctx, id)
	if err != nil {
		return apimodel.IssueAttachment{}, err
	}
	att.URL = AttachmentURL(att.ID)
	return att, nil
}

// IssueAttachmentContent fetches the attachment bytes, returned together with the metadata.
//
// Returned together instead of making the caller query twice: the handler uses the metadata to
// set response headers (Content-Type, Content-Disposition). Two queries would add a round trip,
// and the attachment could be deleted in between, so the headers are set but the content is
// empty.
func (s *Service) IssueAttachmentContent(
	ctx context.Context, id string,
) (apimodel.IssueAttachment, []byte, error) {
	att, err := s.store.GetIssueAttachment(ctx, id)
	if err != nil {
		return apimodel.IssueAttachment{}, nil, err
	}
	// The bytes only live in object storage; fetch them by file_id (the real implementation is in shared/infra/filebytes).
	content, err := s.readBlob(ctx, att.FileID)
	if err != nil {
		return apimodel.IssueAttachment{}, nil, err
	}
	att.URL = AttachmentURL(att.ID)
	return att, content, nil
}

// DeleteIssueAttachment deletes an attachment.
func (s *Service) DeleteIssueAttachment(ctx context.Context, id string) error {
	return s.store.DeleteIssueAttachment(ctx, id)
}

// AttachmentURL builds the access URL of an attachment.
//
// # Why the server builds it, not the frontend
//
// This path is the backend's business. If the frontend built it, changing the route would mean
// changing two places, and **missing the frontend one shows up as "all images are broken"**, a
// pure frontend symptom that cannot point at the real cause, "the backend changed the route".
//
// # Why the path has only the id, not the file name
//
// With the file name we would have to handle path traversal and encoding (`../../etc/passwd`,
// non-ASCII characters, spaces), and it adds nothing for <img src>. Putting the file name in
// Content-Disposition is enough.
//
// # It can go straight into <img src>
//
// Same-origin requests automatically carry the HttpOnly cookie (ama_session, Path=/,
// SameSite=Lax), so authentication just works, and the frontend needs no code at all.
// **This does not make the route public**: it declares ActionRead in authz/matrix.go and resolves
// the workspace from the attachment, so someone in workspace A who gets a URL from workspace B
// receives 403.
func AttachmentURL(id string) string {
	return "/api/issue-attachments/" + id
}
