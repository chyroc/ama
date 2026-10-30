// Business logic for product suggestions (Suggestion): platform-level ideas and suggestions.
//
// ⚠️ **Read the first section of core/README-suggestion.md before touching this file.** This is
// the third kind of "something is wrong" thing in this repo. It is neither issues (workspace-level
// to-dos) nor feedback_reports (machine-collected error stream), and all three names look alike.
// The README has the full comparison on four axes.
//
// # Authorization is all in this package; authz does not cover it
//
// **This is the third place in this repo where "authorization is written in the Service layer"**
// (the first two: GetWorkspaceByKey, ListFeedback). The reason is the same kind as ListFeedback
// but goes further: authz's model is "actor + workspace → role", and suggestions **have no
// workspace dimension at all**; the table does not even have a workspace_id column. So this whole
// batch is ActionNone in the matrix, **and that is not "forgot to set a level"**: Guard cannot
// express it. The real gate is suggestionForRead / s.isStaff in this file.
//
// There are only two authorization rules, written here so they do not scatter:
//
//	the person who filed it  can read and change **their own** (including replies and attachments)
//	platform staff           can read and change **all of them**, and only staff can change status
//
// "Platform staff" reuses the feedback allowlist (injected by the wiring side through
// Config.IsStaff, see the file header of service.go). The reasoning, together with the objection
// "this couples the authorization of two things", is in the README. Short version: **both are the
// same group of people doing the same job** (reading what users report, and changing the platform
// based on it). The real consequence of two lists is someone gets added to one and forgotten in
// the other, then "they can see feedback but not suggestions", and that fault reports no error.

package suggestion

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/gopkg/attachment"
	"github.com/chyroc/ama/server/shared/gopkg/humanize"
	"github.com/chyroc/ama/server/shared/gopkg/mdtitle"
)

// Length limits for body and replies.
//
// Same values as the two in issue, and also **rejected when too long instead of quietly
// truncated**: this is something the user wrote by hand; cutting half of it destroys their input
// while they think the submit succeeded.
const (
	maxBodyLen  = 64 * 1024
	maxReplyLen = 16 * 1024
)

// Default and maximum page size for lists, same approach as the two feedback constants: when the
// caller gives 0 or a negative number, use the default; above the maximum, clamp to the maximum
// instead of returning an error. Script authors pulling data should not have to read our
// constants first just to paginate.
const (
	defaultLimit = 50
	maxPageSize  = 200
)

// blobPrefix is the purpose prefix of attachments in object storage.
//
// ⚠️ **This string must not change.** It is part of the object key
// (`<purpose>/<yyyy>/<mm>/<fileID>`); after changing it, old attachments can never be found
// again, with no error at all, just "object does not exist" on download.
//
// It is separate from issue's "issue-attachments" instead of sharing one attachments/: the
// storage side must be able to measure usage and configure lifecycle separately, and once merged,
// splitting again means moving objects.
//
// ⚠️ core/blob.go has another copy with the same value, blobPrefixSuggestionAttachment, used by
// the attachment migration (core/migrateblob.go). **The two must match**; changing one breaks
// half.
const blobPrefix = "suggestion-attachments"

// actorFrom takes the caller's email from the context.
//
// Like the functions with the same name in issue / feedback, it lowercases once more (defense in
// depth): the auth entry already lowercases, but this value is compared with the author column
// ("did I file this one"), and a case mismatch would make the same person look like two people.
// That shows up as "I cannot see the suggestion I filed", and the list is just empty, with no
// error.
func actorFrom(ctx context.Context) (string, error) {
	actor := strings.TrimSpace(auth.ActorFrom(ctx))
	if actor == "" {
		return "", fmt.Errorf("%w: 需要先登录", persistmodel.ErrInvalidArgument)
	}
	return strings.ToLower(actor), nil
}

// ListSuggestions lists suggestions.
//
// **One endpoint serves two kinds of people; the returned set is decided by identity**:
//
//	platform staff  everything (can filter by author)
//	everyone else   only their own, **q.Author is forcibly overwritten**
//
// ⚠️ That "forcibly overwritten" is how this path implements authorization, not parameter
// cleanup. When the request carries `author=someone-else@example.com`, it **must be ignored**;
// accepting it would let anyone read anyone's suggestions. So this is an unconditional
// assignment, not "fill in only when q.Author is empty": the latter would let that request take
// effect as is.
func (s *Service) ListSuggestions(
	ctx context.Context, q persistmodel.SuggestionQuery,
) (apimodel.SuggestionPage, error) {
	actor, err := actorFrom(ctx)
	if err != nil {
		return apimodel.SuggestionPage{}, err
	}

	staff := s.isStaff(actor)
	if !staff {
		// **Unconditional overwrite**, see above.
		q.Author = actor
	} else if q.Author != "" {
		// Staff filtering by person: lowercase to match the stored form, otherwise querying Foo@x would not find the foo@x rows.
		q.Author = strings.ToLower(strings.TrimSpace(q.Author))
	}

	if q.Status != "" && !apimodel.ValidSuggestionStatus(q.Status) {
		return apimodel.SuggestionPage{}, errUnknownStatus(q.Status)
	}

	// Pagination parameters are normalized here, **the caller is not trusted** (same as ListFeedback).
	if q.Limit <= 0 {
		q.Limit = defaultLimit
	}
	if q.Limit > maxPageSize {
		q.Limit = maxPageSize
	}
	if q.Offset < 0 {
		q.Offset = 0
	}

	list, err := s.store.ListSuggestions(ctx, q)
	if err != nil {
		return apimodel.SuggestionPage{}, err
	}
	return apimodel.SuggestionPage{
		Suggestions: list,
		Limit:       q.Limit,
		Offset:      q.Offset,
		// ViewerIsStaff tells the frontend which UI to render (status controls, author column).
		// **Computed by the server instead of guessed by the frontend**: the frontend cannot see
		// the allowlist, and a guess ends up as "there is a change-status button, and clicking it
		// gives 403".
		ViewerIsStaff: staff,
	}, nil
}

// GetSuggestion fetches one suggestion, **with authorization check**.
func (s *Service) GetSuggestion(ctx context.Context, id string) (apimodel.Suggestion, error) {
	sg, _, err := s.suggestionForRead(ctx, id)
	return sg, err
}

// suggestionForRead fetches a suggestion and checks read permission, returning the suggestion and
// "is the caller platform staff".
//
// Read permission = you filed it, or you are platform staff.
//
// ⚠️ **Not found and no permission return different errors**: not found returns ErrNotFound,
// someone else's returns ErrAccessDenied. This is the same rule as the one in authz/README.md,
// "return 404 when the resource does not exist, otherwise 'deleted' gets reported as 'no
// permission'". Reporting no permission as "does not exist" gains nothing extra here: suggestion
// ids are random strings that cannot be enumerated, and merging the two into one message would
// make users think it is a permission problem when their own suggestion was deleted, and go ask
// an admin.
func (s *Service) suggestionForRead(
	ctx context.Context, id string,
) (apimodel.Suggestion, bool, error) {
	actor, err := actorFrom(ctx)
	if err != nil {
		return apimodel.Suggestion{}, false, err
	}
	sg, err := s.store.GetSuggestion(ctx, id)
	if err != nil {
		return apimodel.Suggestion{}, false, err
	}
	staff := s.isStaff(actor)
	if !staff && sg.Author != actor {
		return apimodel.Suggestion{}, false, errNotYours()
	}
	return sg, staff, nil
}

// errNotYours is the "this one is not yours" message, **defined in one place**.
//
// Pulled out because it appears on seven or eight paths (read, change, delete, reply, upload
// attachment, download attachment). Writing it in each would slowly make the wording drift, and
// users see this exact text (the handler shows it directly).
func errNotYours() error {
	return fmt.Errorf("%w: 只能查看和修改自己提的建议", persistmodel.ErrAccessDenied)
}

// errUnknownStatus is the "unrecognized status" message.
//
// ⚠️ **Anything unrecognized is rejected, never quietly changed to open** (same rule as
// ValidIssueStatus): a silent correction makes the caller think the change worked, while the
// suggestion's status never became what they wanted.
func errUnknownStatus(s apimodel.SuggestionStatus) error {
	return fmt.Errorf("%w: 未知的状态 %q（只能是 open 或 resolved）",
		persistmodel.ErrInvalidArgument, s)
}

// errEmpty is the "both title and body are empty" message, **matching the frontend mock word for word**.
func errEmpty() error {
	return fmt.Errorf("%w: 请至少填写标题或正文", persistmodel.ErrInvalidArgument)
}

// CreateSuggestion files a suggestion. **Any logged-in user can file one**, no permission needed.
//
// This deliberately has no barrier, the same approach as POST /api/feedback: people who hit a
// problem or have an idea should be able to say it. Putting it under a workspace would stop "new
// people who have not joined any workspace yet" from filing suggestions, and they are exactly the
// group with the most to say.
func (s *Service) CreateSuggestion(
	ctx context.Context, draft apimodel.SuggestionDraft,
) (apimodel.Suggestion, error) {
	actor, err := actorFrom(ctx)
	if err != nil {
		return apimodel.Suggestion{}, err
	}

	// The title is optional and derived from the body, **sharing shared/gopkg/mdtitle with issue**.
	// Why not copy it: that function has a chain of order-sensitive Markdown stripping rules
	// (images before links, bold before italic, truncation by rune). A second copy would slowly
	// drift, and drift only shows up as "the title looks a bit odd sometimes", which nobody
	// investigates.
	title := mdtitle.Derive(draft.Title, draft.Body)
	if title == "" {
		return apimodel.Suggestion{}, errEmpty()
	}
	body := strings.TrimSpace(draft.Body)
	if err := checkBodyLen(body); err != nil {
		return apimodel.Suggestion{}, err
	}

	// Status: defaults to open if not set. If set, it must be recognized; anything unrecognized is rejected.
	status := apimodel.SuggestionOpen
	if draft.Status != "" {
		if !apimodel.ValidSuggestionStatus(draft.Status) {
			return apimodel.Suggestion{}, errUnknownStatus(draft.Status)
		}
		status = draft.Status
	}

	now := apimodel.NowISO()
	return s.store.CreateSuggestion(ctx, apimodel.Suggestion{
		ID:     s.nextID("sug"),
		Title:  title,
		Body:   body,
		Status: status,
		// **Only from the context**; the request body has no such field. Accepting it would allow filing suggestions under someone else's name.
		Author:    actor,
		CreatedAt: now,
		UpdatedAt: now,
	})
}

// UpdateSuggestion changes a suggestion.
//
// # The two kinds of people can change different things
//
//	the author        changes title and body (**cannot change status**)
//	platform staff    can change everything, including status
//
// "The author cannot change status" is on purpose: the status says **how far platform staff have
// got with it**. Letting the person who filed it click "resolved" themselves would make the field
// meaningless (staff looking at the list could not tell "we solved it" from "they gave up"). An
// author who wants to withdraw should delete it.
//
// # Both title and body empty = leave them alone this time
//
// The same heuristic as UpdateIssue, word for word: the typical case is staff only changing the
// status (PATCH {"status":"resolved"}); then Title/Body in the draft are zero values and must not
// be treated as "clear". The cost is that "really clear both title and body" cannot be expressed,
// but that would be an empty record anyway and would be rejected.
func (s *Service) UpdateSuggestion(
	ctx context.Context, id string, draft apimodel.SuggestionDraft,
) (apimodel.Suggestion, error) {
	cur, staff, err := s.suggestionForRead(ctx, id)
	if err != nil {
		return apimodel.Suggestion{}, err
	}

	next := cur
	next.Title = strings.TrimSpace(draft.Title)
	next.Body = strings.TrimSpace(draft.Body)
	if next.Title == "" && next.Body == "" {
		next.Title, next.Body = cur.Title, cur.Body
	}

	title := mdtitle.Derive(next.Title, next.Body)
	if title == "" {
		return apimodel.Suggestion{}, errEmpty()
	}
	next.Title = title
	if err := checkBodyLen(next.Body); err != nil {
		return apimodel.Suggestion{}, err
	}

	if draft.Status != "" {
		if !apimodel.ValidSuggestionStatus(draft.Status) {
			return apimodel.Suggestion{}, errUnknownStatus(draft.Status)
		}
		// ⚠️ Only platform staff can change status, see above.
		if !staff && draft.Status != cur.Status {
			return apimodel.Suggestion{}, fmt.Errorf(
				"%w: 只有平台方可以改建议的状态", persistmodel.ErrAccessDenied)
		}
		next.Status = draft.Status
	}

	next.UpdatedAt = apimodel.NowISO()
	return s.store.SaveSuggestion(ctx, next)
}

// DeleteSuggestion deletes a suggestion, together with its replies and attachments (guaranteed by
// Store).
//
// Authors can delete their own; platform staff can delete any.
func (s *Service) DeleteSuggestion(ctx context.Context, id string) error {
	if _, _, err := s.suggestionForRead(ctx, id); err != nil {
		return err
	}
	return s.store.DeleteSuggestion(ctx, id)
}

func checkBodyLen(body string) error {
	if len(body) > maxBodyLen {
		return fmt.Errorf("%w: 正文超过 %s 上限", persistmodel.ErrInvalidArgument,
			humanize.HumanBytes(maxBodyLen))
	}
	return nil
}

// -- Replies ------------------------------------------------------------------

// ListSuggestionReplies lists the replies under a suggestion, oldest first. Authorization is the same as reading the suggestion.
func (s *Service) ListSuggestionReplies(
	ctx context.Context, suggestionID string,
) ([]apimodel.SuggestionReply, error) {
	_, staff, err := s.suggestionForRead(ctx, suggestionID)
	if err != nil {
		return nil, err
	}
	list, err := s.store.ListSuggestionReplies(ctx, suggestionID)
	if err != nil {
		return nil, err
	}
	return hideStaffIdentity(list, staff), nil
}

// hideStaffIdentity blanks out the author email of staff replies, **unless the viewer is platform
// staff themselves**.
//
// # Why it must be blanked
//
// Staff replies are made **in the name of the platform** ("got it, coming in the next version"),
// not as one person's promise. Showing the email has two consequences, neither of which we want:
//
//   - **It reveals who builds the platform.** That is organizational information that users do
//     not need and should not learn from a product suggestion; the list also changes as people
//     move teams, which amounts to hanging the internal team roster on the product UI.
//   - **It ties the promise to a person.** Next time the user messages that person directly to
//     chase progress, bypassing this channel, which is exactly what this feature tries to avoid
//     (recorded, traceable, continues when someone else takes over).
//
// On the other hand, **the user's own follow-up keeps its author**: it is the user themselves, and
// blanking it would make the conversation look like two anonymous people talking to themselves.
// So the check is IsStaff, not "always blank".
//
// # Why blank it here, not just skip rendering it on the frontend
//
// "Not shown in the UI" is not hiding: the value is still in the JSON response, visible with F12,
// and a script gets all of it in one fetch. This is **the same mistake** as the one in
// README-feedback.md, "'not putting an entry in the UI' is not access control". If it is to be
// blanked, blank it on the server so it never leaves.
//
// # Staff themselves see real names
//
// They need to know "who replied to this" to work together internally (who followed up, whom to
// ask for details). So staff is passed in as the check: one endpoint, two kinds of people see two
// different things, the same approach as ListSuggestions deciding the returned set by identity.
func hideStaffIdentity(list []apimodel.SuggestionReply, viewerIsStaff bool) []apimodel.SuggestionReply {
	if viewerIsStaff {
		return list
	}
	out := make([]apimodel.SuggestionReply, 0, len(list))
	for _, r := range list {
		if r.IsStaff {
			// Set to empty instead of making up a fake name ("platform team" or the like): a fake
			// name in a valid email shape would be rendered by the frontend as a real address, and
			// aggregated by scripts as a real person. An empty string has a clear meaning, "this
			// reply has no personal signature", and the frontend renders it as the organization.
			r.Author = ""
		}
		out = append(out, r)
	}
	return out
}

// CreateSuggestionReply adds a reply. **Both the author and platform staff can reply**; that is
// what "supports conversation" means.
//
// IsStaff is decided **at write time** and stored, not computed at read time. See
// apimodel.SuggestionReply.IsStaff for why: the allowlist changes, and computing it on read would
// make the identity of old replies drift with it.
func (s *Service) CreateSuggestionReply(
	ctx context.Context, suggestionID, body string,
) (apimodel.SuggestionReply, error) {
	actor, err := actorFrom(ctx)
	if err != nil {
		return apimodel.SuggestionReply{}, err
	}
	// Authorization goes the same way: whoever can read this suggestion can reply.
	if _, _, err := s.suggestionForRead(ctx, suggestionID); err != nil {
		return apimodel.SuggestionReply{}, err
	}

	body = strings.TrimSpace(body)
	if err := validateReplyBody(body); err != nil {
		return apimodel.SuggestionReply{}, err
	}

	now := apimodel.NowISO()
	return s.store.CreateSuggestionReply(ctx, apimodel.SuggestionReply{
		ID:           s.nextID("sre"),
		SuggestionID: suggestionID,
		Body:         body,
		Author:       actor,
		IsStaff:      s.isStaff(actor),
		// Empty Origin = written by a person. The agent auto-reply path will be told apart here later.
		CreatedAt: now,
		UpdatedAt: now,
	})
}

// UpdateSuggestionReply changes a reply. **Only the author can change their own reply.**
//
// ⚠️ Platform staff have **no** special rights here, deliberately the opposite of "staff can
// change suggestion status": changing status is part of the handling process, while changing
// what someone else wrote is tampering with their words. To correct something, staff can only
// reply again.
func (s *Service) UpdateSuggestionReply(
	ctx context.Context, id, body string,
) (apimodel.SuggestionReply, error) {
	actor, err := actorFrom(ctx)
	if err != nil {
		return apimodel.SuggestionReply{}, err
	}
	cur, err := s.store.GetSuggestionReply(ctx, id)
	if err != nil {
		return apimodel.SuggestionReply{}, err
	}
	if cur.Author != actor {
		return apimodel.SuggestionReply{}, fmt.Errorf(
			"%w: 只能修改自己写的回复", persistmodel.ErrAccessDenied)
	}

	body = strings.TrimSpace(body)
	if err := validateReplyBody(body); err != nil {
		return apimodel.SuggestionReply{}, err
	}

	next := cur
	next.Body = body
	next.UpdatedAt = apimodel.NowISO()
	return s.store.SaveSuggestionReply(ctx, next)
}

// DeleteSuggestionReply deletes a reply. The author, or platform staff.
//
// Staff **do** have special rights here (unlike changing): deleting is a moderation action
// (removing junk), changing is tampering with someone's words. The two have different rules.
func (s *Service) DeleteSuggestionReply(ctx context.Context, id string) error {
	actor, err := actorFrom(ctx)
	if err != nil {
		return err
	}
	cur, err := s.store.GetSuggestionReply(ctx, id)
	if err != nil {
		// Not found is not an error (idempotent), but first confirm it really does not exist, rather than the lookup failing.
		if errors.Is(err, persistmodel.ErrNotFound) {
			return nil
		}
		return err
	}
	if cur.Author != actor && !s.isStaff(actor) {
		return fmt.Errorf("%w: 只能删除自己写的回复", persistmodel.ErrAccessDenied)
	}
	return s.store.DeleteSuggestionReply(ctx, id)
}

func validateReplyBody(body string) error {
	if body == "" {
		return fmt.Errorf("%w: 回复内容不能为空", persistmodel.ErrInvalidArgument)
	}
	if len(body) > maxReplyLen {
		return fmt.Errorf("%w: 回复超过 %s 上限", persistmodel.ErrInvalidArgument,
			humanize.HumanBytes(maxReplyLen))
	}
	return nil
}

// -- Attachments --------------------------------------------------------------

// ListSuggestionAttachments lists attachment metadata. Authorization is the same as reading the suggestion.
func (s *Service) ListSuggestionAttachments(
	ctx context.Context, suggestionID string,
) ([]apimodel.SuggestionAttachment, error) {
	if _, _, err := s.suggestionForRead(ctx, suggestionID); err != nil {
		return nil, err
	}
	list, err := s.store.ListSuggestionAttachments(ctx, suggestionID)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].URL = AttachmentURL(list[i].ID)
	}
	return list, nil
}

// CreateSuggestionAttachment uploads an attachment.
//
// The three layers of defense are **shared with issue through shared/gopkg/attachment**: type
// allowlist + server-side sniffing, nosniff, and everything that is not an image served as
// attachment. That logic has nothing to do with the entity; a copy would only make the two
// slowly drift, and what drifts is a security hole (e.g. one side later dropping the guard "do
// not accept image/ when sniffing has no opinion", so any bytes renamed to .png get rendered by
// <img>).
func (s *Service) CreateSuggestionAttachment(
	ctx context.Context, suggestionID, filename string, content []byte,
) (apimodel.SuggestionAttachment, error) {
	actor, err := actorFrom(ctx)
	if err != nil {
		return apimodel.SuggestionAttachment{}, err
	}
	if _, _, err := s.suggestionForRead(ctx, suggestionID); err != nil {
		return apimodel.SuggestionAttachment{}, err
	}

	if len(content) > attachment.MaxBytes {
		return apimodel.SuggestionAttachment{}, attachment.ErrTooLarge
	}
	existing, err := s.store.ListSuggestionAttachments(ctx, suggestionID)
	if err != nil {
		return apimodel.SuggestionAttachment{}, err
	}
	if len(existing) >= attachment.MaxCount {
		return apimodel.SuggestionAttachment{}, attachment.ErrTooMany
	}

	contentType, err := attachment.DetectType(filename, content)
	if err != nil {
		return apimodel.SuggestionAttachment{}, err
	}

	// The bytes go into object storage first, and the returned fileID is stored in the business
	// row (PG only holds metadata). Same reasoning as the issue side, word for word; the real
	// implementation is in shared/infra/filebytes.
	fileID, err := s.putBlob(ctx, blobPrefix, filename, contentType, actor, content)
	if err != nil {
		return apimodel.SuggestionAttachment{}, err
	}

	att, err := s.store.CreateSuggestionAttachment(ctx, apimodel.SuggestionAttachment{
		ID:           s.nextID("sat"),
		SuggestionID: suggestionID,
		FileID:       fileID,
		// The original file name is stored as is; **cleaning happens when the response header is
		// written** (attachment.Disposition), not when it is stored. Storing the cleaned value
		// would make the downloaded file name differ from what the user uploaded.
		Filename:    strings.TrimSpace(filename),
		ContentType: contentType,
		Size:        len(content),
		Author:      actor,
		CreatedAt:   apimodel.NowISO(),
	})
	if err != nil {
		return apimodel.SuggestionAttachment{}, err
	}
	att.URL = AttachmentURL(att.ID)
	return att, nil
}

// SuggestionAttachmentContent fetches the metadata and bytes of an attachment, **with
// authorization check**.
//
// ⚠️ **The authorization on this path is the easiest to forget, and forgetting it reports no
// error at all.** Attachment URLs go into <img src>, and show up in shared links, browser history
// and logs. Without a check, anyone who gets the URL can see the screenshots someone else attached
// to their suggestion, and that leak produces no error and no UI shows it ever happened. The issue
// side is protected by authz resolving the workspace; here there is no workspace to resolve, so
// **it can only be checked here**: look up the owning suggestion and compare author.
func (s *Service) SuggestionAttachmentContent(
	ctx context.Context, id string,
) (apimodel.SuggestionAttachment, []byte, error) {
	att, err := s.store.GetSuggestionAttachment(ctx, id)
	if err != nil {
		return apimodel.SuggestionAttachment{}, nil, err
	}
	// Authorization goes through the owning suggestion: only people who can read that suggestion can fetch its attachments.
	if _, _, err := s.suggestionForRead(ctx, att.SuggestionID); err != nil {
		return apimodel.SuggestionAttachment{}, nil, err
	}
	// The bytes only live in object storage; fetch them by file_id (the real implementation is in shared/infra/filebytes).
	content, err := s.readBlob(ctx, att.FileID)
	if err != nil {
		return apimodel.SuggestionAttachment{}, nil, err
	}
	att.URL = AttachmentURL(att.ID)
	return att, content, nil
}

// DeleteSuggestionAttachment deletes an attachment. Authorization is the same as reading the suggestion.
func (s *Service) DeleteSuggestionAttachment(ctx context.Context, id string) error {
	att, err := s.store.GetSuggestionAttachment(ctx, id)
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			// Not found is not an error (idempotent).
			return nil
		}
		return err
	}
	if _, _, err := s.suggestionForRead(ctx, att.SuggestionID); err != nil {
		return err
	}
	return s.store.DeleteSuggestionAttachment(ctx, id)
}

// AttachmentURL builds the access URL of an attachment.
//
// Built by the server instead of the frontend (same as issue.AttachmentURL): when the route
// changes and the frontend is not updated, it shows up as "all images are broken", a pure
// frontend symptom that cannot point at the real cause.
func AttachmentURL(id string) string {
	return "/api/suggestion-attachments/" + id
}
