// HTTP boundary for issues: decode the request, call the Service, map sentinel errors to status codes.
//
// It is its own file instead of being stuffed into handler.go: that file is already 1600 lines, and this domain has a
// **shape found nowhere else**: a route that returns binary (attachment download). Its caveats are completely different from
// those JSON handlers, and mixed together it is easy to copy as if it were the same kind.
//
// # Attachment download is the first route in the repo that "returns bytes uploaded by users"
//
// The existing non-JSON responses are only three: SSE (streamRunEvents in core/handler.go),
// a self-describing doc (openapi), and frontend static assets (httpd/static.go). What they share is that
// **we produce the content ourselves**. This one does not: it returns user-uploaded bytes to the browser as is,
// so none of the three response headers can be left out. The reasons are written one by one on serveIssueAttachment.

package apihandler

import (
	"fmt"
	"io"
	"net/http"

	"github.com/chyroc/ama/server/model/apimodel"
	skillsvc "github.com/chyroc/ama/server/service/skill"
	"github.com/chyroc/ama/server/shared/gopkg/attachment"
)

// registerIssueRoutes registers all routes of this domain. Called by Handler.Register.
//
// ⚠️ **Each of these 11 must be declared in authz/matrix.go**. Miss one and the service panics on startup
// (that is on purpose, see authz/README.md: authorization not taking effect is invisible).
func (h *Handler) registerIssueRoutes(mux Mux) {
	mux.HandleFunc("GET /api/workspaces/{workspaceId}/issues", h.listIssues)
	mux.HandleFunc("POST /api/workspaces/{workspaceId}/issues", h.createIssue)
	mux.HandleFunc("GET /api/issues/{id}", h.getIssue)
	mux.HandleFunc("PATCH /api/issues/{id}", h.updateIssue)
	mux.HandleFunc("DELETE /api/issues/{id}", h.deleteIssue)

	// Comments. Mounted under issue; Guard resolves the workspace from the issue.
	mux.HandleFunc("GET /api/issues/{id}/comments", h.listIssueComments)
	mux.HandleFunc("POST /api/issues/{id}/comments", h.createIssueComment)
	// Editing/deleting a single comment uses a flat path (same shape as /api/agents/{id}): the comment id is already unique,
	// and repeating issueId in the path only makes the frontend store one more value, and when the two disagree nobody knows which to trust.
	mux.HandleFunc("PATCH /api/issue-comments/{id}", h.updateIssueComment)
	mux.HandleFunc("DELETE /api/issue-comments/{id}", h.deleteIssueComment)

	// Attachment upload (multipart).
	//
	// ⚠️ **It must be its own path segment**; it cannot share one with the JSON routes above:
	// one method+path can only have one handler (uploadSkill in handler.go notes the same thing).
	mux.HandleFunc("GET /api/issues/{id}/attachments", h.listIssueAttachments)
	mux.HandleFunc("POST /api/issues/{id}/attachments", h.uploadIssueAttachment)

	// Attachment download. **Flat path**: this URL goes into <img src>, so shorter is better,
	// and it should not expose the issue id (that identifies another resource).
	mux.HandleFunc("GET /api/issue-attachments/{id}", h.serveIssueAttachment)
	mux.HandleFunc("DELETE /api/issue-attachments/{id}", h.deleteIssueAttachment)
}

// -- Issues --------------------------------------------------------------------

func (h *Handler) listIssues(w http.ResponseWriter, r *http.Request) {
	// status comes from the query; empty means no filter. Invalid values are rejected by the Service (400),
	// not silently ignored here: if ignored, the user would get "all" and think it was filtered.
	status := apimodel.IssueStatus(r.URL.Query().Get("status"))
	list, err := h.issue.ListIssues(r.Context(), r.PathValue("workspaceId"), status)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) createIssue(w http.ResponseWriter, r *http.Request) {
	var draft apimodel.IssueDraft
	if !h.decode(w, r, &draft) {
		return
	}
	iss, err := h.issue.CreateIssue(r.Context(), r.PathValue("workspaceId"), draft)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, iss)
}

func (h *Handler) getIssue(w http.ResponseWriter, r *http.Request) {
	iss, err := h.issue.GetIssue(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, iss)
}

func (h *Handler) updateIssue(w http.ResponseWriter, r *http.Request) {
	var draft apimodel.IssueDraft
	if !h.decode(w, r, &draft) {
		return
	}
	iss, err := h.issue.UpdateIssue(r.Context(), r.PathValue("id"), draft)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, iss)
}

func (h *Handler) deleteIssue(w http.ResponseWriter, r *http.Request) {
	if err := h.issue.DeleteIssue(r.Context(), r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// -- Comments --------------------------------------------------------------------

// issueCommentBody is the request body of a comment.
//
// **It has only one field, body**: the author comes from context, the time is set by the Service,
// and issueId is in the path. The fewer things that can appear in the request body, the fewer ways to overstep permissions.
type issueCommentBody struct {
	Body string `json:"body"`
}

func (h *Handler) listIssueComments(w http.ResponseWriter, r *http.Request) {
	list, err := h.issue.ListIssueComments(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) createIssueComment(w http.ResponseWriter, r *http.Request) {
	var body issueCommentBody
	if !h.decode(w, r, &body) {
		return
	}
	c, err := h.issue.CreateIssueComment(r.Context(), r.PathValue("id"), body.Body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (h *Handler) updateIssueComment(w http.ResponseWriter, r *http.Request) {
	var body issueCommentBody
	if !h.decode(w, r, &body) {
		return
	}
	c, err := h.issue.UpdateIssueComment(r.Context(), r.PathValue("id"), body.Body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) deleteIssueComment(w http.ResponseWriter, r *http.Request) {
	if err := h.issue.DeleteIssueComment(r.Context(), r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// -- Attachments --------------------------------------------------------------------

func (h *Handler) listIssueAttachments(w http.ResponseWriter, r *http.Request) {
	list, err := h.issue.ListIssueAttachments(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// uploadIssueAttachment receives an attachment uploaded as multipart.
//
// # The size limit must come before reading
//
// http.MaxBytesReader wraps r.Body and fails **while reading** once the limit is exceeded.
// Doing ReadAll first and checking the length afterwards is pointless: a 10 GiB request would OOM the process first.
// This is the same approach as uploadSkill in handler.go.
func (h *Handler) uploadIssueAttachment(w http.ResponseWriter, r *http.Request) {
	// Note that when over the limit, MaxBytesReader also marks w so the connection is closed after the response.
	// That is documented behavior, not a side effect.
	r.Body = http.MaxBytesReader(w, r.Body, attachment.MaxUploadBytes)

	// 32 KiB memory threshold; anything beyond goes to temp files.
	// **This does not break "no storing on local disk"**: that rule is about data that must live across requests,
	// while the temp files here are RemoveAll'ed when this request ends (the defer below).
	if err := r.ParseMultipartForm(32 << 10); err != nil {
		if skillsvc.IsUploadTooLarge(err) {
			h.failWith(w, http.StatusRequestEntityTooLarge,
				userMessage(attachment.ErrTooLarge), "upload_too_large")
			return
		}
		h.failWith(w, http.StatusBadRequest,
			"上传内容不是合法的 multipart/form-data 表单", "invalid_payload")
		return
	}
	// Temp files must be cleaned up by us. Otherwise every upload leaves a copy on disk, and the longer an instance lives the more leaks.
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, header, err := r.FormFile(attachment.FieldName)
	if err != nil {
		h.failWith(w, http.StatusBadRequest,
			fmt.Sprintf("请选择要上传的文件（表单字段名 %s）", attachment.FieldName),
			"invalid_payload")
		return
	}
	defer func() { _ = file.Close() }()

	// One more LimitReader: ParseMultipartForm caps the whole request body, but it does not care about the size of
	// a single part. Reading one extra byte tells "exactly at the limit" apart from "over it".
	data, err := io.ReadAll(io.LimitReader(file, attachment.MaxBytes+1))
	if err != nil {
		if skillsvc.IsUploadTooLarge(err) {
			h.failWith(w, http.StatusRequestEntityTooLarge,
				userMessage(attachment.ErrTooLarge), "upload_too_large")
			return
		}
		h.failWith(w, http.StatusBadRequest, "读取上传内容失败", "invalid_payload")
		return
	}
	if len(data) > attachment.MaxBytes {
		h.failWith(w, http.StatusRequestEntityTooLarge,
			userMessage(attachment.ErrTooLarge), "upload_too_large")
		return
	}

	att, err := h.issue.CreateIssueAttachment(
		r.Context(), r.PathValue("id"), header.Filename, data)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, att)
}

// serveIssueAttachment returns attachment bytes to the browser.
//
// # This route is authenticated; do not treat it as a static asset
//
// The URL goes into <img src> and looks like a static asset, but it is **not public**:
// matrix declares ActionRead + KindIssueAttachment, and Guard resolves the owning workspace from the attachment id
// and then checks the member role. **Someone in workspace A who gets a URL from workspace B gets 403**,
// pinned down by a real wiring test at the httpd layer.
//
// Credentials go through the Cookie channel: the browser does not send custom headers for subresource requests, but same-origin requests automatically include
// the HttpOnly Cookie (ama_session, Path=/, SameSite=Lax),
// and that is exactly one of the three channels in auth.credentialFrom. So the frontend needs no code at all.
//
// # Three response headers, none can be left out
//
//	Content-Type            the real type found by sniffing. If wrong, images do not render (or worse,
//	                        HTML gets run as a page).
//	X-Content-Type-Options  nosniff. Stops the browser from ignoring our type and guessing on its own;
//	                        this is the second line of defense after "type detection is bypassed".
//	Content-Disposition     inline for images (otherwise <img> cannot show them), attachment for the rest
//	                        (download instead of render). The third line of defense.
//
// How the three split the work is written at the top of shared/gopkg/attachment/attachment.go. **Deleting any one of them gives no error**;
// the attack surface just comes back, so each has its own test.
func (h *Handler) serveIssueAttachment(w http.ResponseWriter, r *http.Request) {
	att, content, err := h.issue.IssueAttachmentContent(r.Context(), r.PathValue("id"))
	if err != nil {
		// ⚠️ Failure must exit before any header is set: once headers are sent, the status code can no longer change
		// (same as streamRunEvents). Here h.fail writes the standard JSON envelope.
		h.fail(w, r, err)
		return
	}

	w.Header().Set("Content-Type", att.ContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition",
		attachment.Disposition(att.ContentType, att.Filename))
	// private: this is authenticated content and **must never enter a shared cache** (CDN, corporate proxy).
	// With public, an intermediate cache would serve A's attachment to B, and that produces no error at all.
	w.Header().Set("Cache-Control", "private, max-age=300")
	// Set the length explicitly, so the browser can show download progress and chunked transfer is avoided.
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (h *Handler) deleteIssueAttachment(w http.ResponseWriter, r *http.Request) {
	if err := h.issue.DeleteIssueAttachment(r.Context(), r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoContent(w)
}
