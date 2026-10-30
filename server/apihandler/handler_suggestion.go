// HTTP layer for product suggestions (Suggestion).
//
// It is its own file, next to handler_issue.go, for the same reason: it has a route that **returns bytes uploaded
// by users** (attachment download), whose caveats are completely different from the JSON ones,
// and mixed together it is easy to copy as if it were the same kind.
//
// ⚠️ **All routes in this group are ActionNone in authz/matrix.go, and that is not "forgot to set a level".**
// Suggestions are platform-level and have no workspace dimension; Guard's model (actor + workspace → role) cannot express them.
// **The real gate is in the Service** (suggestionForRead / isSuggestionStaff in core/suggestion.go).
// When adding a new route, do not copy kind/idParam from the issue group: that would make Guard look up the suggestion id in the issues
// table, and the result is a constant 404 with an error saying "resource does not exist".

package apihandler

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	skillsvc "github.com/chyroc/ama/server/service/skill"
	"github.com/chyroc/ama/server/shared/gopkg/attachment"
)

// registerSuggestionRoutes mounts the 11 routes of the suggestion domain.
//
// The path shape matches the issue group (the collection is at /api/suggestions, single-item operations on a sub-resource use a flat
// path /api/suggestion-replies/{id}), **but the collection is not mounted under a workspace**:
// it has no workspace. This is the only shape difference between the two groups, and the one that best shows what this entity is.
func (h *Handler) registerSuggestionRoutes(mux Mux) {
	mux.HandleFunc("GET /api/suggestions", h.listSuggestions)
	mux.HandleFunc("POST /api/suggestions", h.createSuggestion)
	mux.HandleFunc("GET /api/suggestions/{id}", h.getSuggestion)
	mux.HandleFunc("PATCH /api/suggestions/{id}", h.updateSuggestion)
	mux.HandleFunc("DELETE /api/suggestions/{id}", h.deleteSuggestion)

	mux.HandleFunc("GET /api/suggestions/{id}/replies", h.listSuggestionReplies)
	mux.HandleFunc("POST /api/suggestions/{id}/replies", h.createSuggestionReply)
	mux.HandleFunc("PATCH /api/suggestion-replies/{id}", h.updateSuggestionReply)
	mux.HandleFunc("DELETE /api/suggestion-replies/{id}", h.deleteSuggestionReply)

	mux.HandleFunc("GET /api/suggestions/{id}/attachments", h.listSuggestionAttachments)
	mux.HandleFunc("POST /api/suggestions/{id}/attachments", h.uploadSuggestionAttachment)

	mux.HandleFunc("GET /api/suggestion-attachments/{id}", h.serveSuggestionAttachment)
	mux.HandleFunc("DELETE /api/suggestion-attachments/{id}", h.deleteSuggestionAttachment)
}

// listSuggestions lists suggestions.
//
// ⚠️ **The `author` query param only works for platform staff**. If a normal user passes it, the Service
// unconditionally overrides it with themselves. Passing it through as is here is right: filtering happens in the Service,
// and checking again here would split authorization into two places, which sooner or later disagree.
func (h *Handler) listSuggestions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := persistmodel.SuggestionQuery{
		Author: strings.TrimSpace(q.Get("author")),
		Status: apimodel.SuggestionStatus(strings.TrimSpace(q.Get("status"))),
	}
	// A number that cannot be parsed is treated as not passed (uses the Service default), with no error: one mistyped limit
	// should not fail a whole data-pulling script, and the Service has a clear meaning for 0. Same as listFeedback.
	if n, err := strconv.Atoi(strings.TrimSpace(q.Get("limit"))); err == nil {
		query.Limit = n
	}
	if n, err := strconv.Atoi(strings.TrimSpace(q.Get("offset"))); err == nil {
		query.Offset = n
	}

	page, err := h.suggestion.ListSuggestions(r.Context(), query)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *Handler) createSuggestion(w http.ResponseWriter, r *http.Request) {
	var draft apimodel.SuggestionDraft
	if !h.decode(w, r, &draft) {
		return
	}
	sg, err := h.suggestion.CreateSuggestion(r.Context(), draft)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, sg)
}

func (h *Handler) getSuggestion(w http.ResponseWriter, r *http.Request) {
	sg, err := h.suggestion.GetSuggestion(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sg)
}

func (h *Handler) updateSuggestion(w http.ResponseWriter, r *http.Request) {
	var draft apimodel.SuggestionDraft
	if !h.decode(w, r, &draft) {
		return
	}
	sg, err := h.suggestion.UpdateSuggestion(r.Context(), r.PathValue("id"), draft)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sg)
}

func (h *Handler) deleteSuggestion(w http.ResponseWriter, r *http.Request) {
	if err := h.suggestion.DeleteSuggestion(r.Context(), r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// -- Replies --------------------------------------------------------------------

// suggestionReplyBody is the request body of a reply, and **has only one field**.
//
// It deliberately does not accept author or isStaff: the former comes from context (accepting it would allow impersonation),
// and the latter is decided by the server from an allowlist (accepting it would let anyone mark themselves as an official answer).
type suggestionReplyBody struct {
	Body string `json:"body"`
}

func (h *Handler) listSuggestionReplies(w http.ResponseWriter, r *http.Request) {
	list, err := h.suggestion.ListSuggestionReplies(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) createSuggestionReply(w http.ResponseWriter, r *http.Request) {
	var body suggestionReplyBody
	if !h.decode(w, r, &body) {
		return
	}
	reply, err := h.suggestion.CreateSuggestionReply(r.Context(), r.PathValue("id"), body.Body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, reply)
}

func (h *Handler) updateSuggestionReply(w http.ResponseWriter, r *http.Request) {
	var body suggestionReplyBody
	if !h.decode(w, r, &body) {
		return
	}
	reply, err := h.suggestion.UpdateSuggestionReply(r.Context(), r.PathValue("id"), body.Body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, reply)
}

func (h *Handler) deleteSuggestionReply(w http.ResponseWriter, r *http.Request) {
	if err := h.suggestion.DeleteSuggestionReply(r.Context(), r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// -- Attachments --------------------------------------------------------------------

func (h *Handler) listSuggestionAttachments(w http.ResponseWriter, r *http.Request) {
	list, err := h.suggestion.ListSuggestionAttachments(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// uploadSuggestionAttachment receives one attachment.
//
// It is **structurally identical, line for line,** to uploadIssueAttachment (including the two size limits and temp file cleanup);
// only the Service method it calls differs. The caveats for both are written in that one and not repeated here.
// When copying, copy the defer RemoveAll too; if it is missed, every upload leaves a copy on disk.
func (h *Handler) uploadSuggestionAttachment(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, attachment.MaxUploadBytes)

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

	// Reading one extra byte tells "exactly at the limit" apart from "over it".
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

	att, err := h.suggestion.CreateSuggestionAttachment(
		r.Context(), r.PathValue("id"), header.Filename, data)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, att)
}

// serveSuggestionAttachment returns attachment bytes to the browser.
//
// # ⚠️ The auth of this route is **not the same mechanism** as the issue one. This is the most important note in the file
//
// The issue one relies on authz: matrix declares KindIssueAttachment, and Guard resolves the owning workspace from the attachment id
// and then checks the member role. **That does not work here**: suggestions have no workspace, so Guard cannot resolve anything.
// So it is ActionNone in matrix, and auth is **entirely up to the Service**
// (SuggestionAttachmentContent looks up the owning suggestion and compares the author).
//
// If this route is set to ActionNone and you **forget to check in the Service**, anyone who gets the URL can
// see screenshots other people attached to their suggestions. URLs show up in shared links, browser history and logs,
// and this leak **produces no error, and no screen ever shows that it happened**.
// A real wiring test at the httpd layer pins down "someone else's attachment gets 403".
//
// Credentials go through the Cookie channel, same as the issue one: the browser does not send custom headers for subresource requests,
// but same-origin requests automatically include the HttpOnly Cookie, so the frontend needs no code at all.
//
// # None of the three response headers can be left out
//
// Identical to serveIssueAttachment (Content-Type / nosniff / Content-Disposition);
// how they split the work is written at the top of shared/gopkg/attachment/attachment.go. **Deleting any one of them gives no error**; the attack surface just comes back.
func (h *Handler) serveSuggestionAttachment(w http.ResponseWriter, r *http.Request) {
	att, content, err := h.suggestion.SuggestionAttachmentContent(r.Context(), r.PathValue("id"))
	if err != nil {
		// ⚠️ Failure must return **before any header is set**: setting headers first and then failing would make the error response carry
		// an image/png Content-Type, the browser would decode that JSON as an image and show a broken image,
		// and the real cause (403) would be completely hidden.
		h.fail(w, r, err)
		return
	}

	w.Header().Set("Content-Type", att.ContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", attachment.Disposition(att.ContentType, att.Filename))
	// private: this is authenticated content and must not enter any shared cache.
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (h *Handler) deleteSuggestionAttachment(w http.ResponseWriter, r *http.Request) {
	if err := h.suggestion.DeleteSuggestionAttachment(r.Context(), r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoContent(w)
}
