// HTTP routes for account-level personal files (user files): object storage multipart upload / signed download URL.
//
// # Routes (9, all account-level ActionNone)
//
//	POST   /api/files                          start an upload (picks simple/multipart by size)
//	PUT    /api/files/{id}/content             simple mode: send the whole file
//	PUT    /api/files/{id}/parts/{partNumber}  multipart mode: send one part
//	POST   /api/files/{id}/complete            multipart: merge the parts
//	POST   /api/files/{id}/abort               abandon an upload
//	GET    /api/files                          list my completed files (?limit=)
//	GET    /api/files/{id}                     file details (with a time-limited download URL when completed)
//	GET    /api/files/{id}/content             download relayed through amad (for the CLI; see userfile.Service.OpenFile)
//	DELETE /api/files/{id}                     delete a completed file
//
// # Security model: ActionNone does not mean no login
//
// These resources do not belong to any workspace, so Guard cannot check a member role, and matrix sets ActionNone
// (login required, no extra workspace check). Owner isolation has two layers:
//
//  1. The owner always comes from auth.ActorFrom(ctx). **Never accept an email claimed in the request body / query**;
//  2. Every SQL behind the Service includes owner_email, so other people's ids always get 404.
//
// # Why uploads are relayed through the server instead of signing a direct-upload URL
//
// It does not require the object storage to support "presigned upload" (signing details differ by vendor, and failures are very hard to debug);
// relaying through the server keeps auth in one place (amad) and stores the in-progress uploadId in the database (resumable across instances).
// The only cost is one extra network hop for the bytes. Each part is fixed at 8 MiB and **streamed through**; large files are never buffered in the process.
package apihandler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/gopkg/attachment"
)

// UserFileServices is the personal-file domain capability this file needs (*userfile.Service satisfies it).
// The narrow interface is declared on the handler side, for the same reason as the other domains on Handler.
type UserFileServices interface {
	StartUpload(ctx context.Context, owner string, in apimodel.StartUserFileUpload) (apimodel.UserFile, error)
	PutSimpleContent(ctx context.Context, owner, id string, size int64, content io.Reader) (apimodel.UserFile, error)
	UploadPart(ctx context.Context, owner, id string, partNumber int, size int64, content io.Reader) error
	CompleteUpload(ctx context.Context, owner, id string) (apimodel.UserFile, error)
	AbortUpload(ctx context.Context, owner, id string) error
	ListFiles(ctx context.Context, owner string, limit int) ([]apimodel.UserFile, error)
	GetFile(ctx context.Context, owner, id string) (apimodel.UserFile, error)
	OpenFile(ctx context.Context, owner, id string) (apimodel.UserFile, io.ReadCloser, error)
	DeleteFile(ctx context.Context, owner, id string) error
}

// simplePartLimit / partLimit are request body read limits: both the simple whole object and a single part are
// about 8 MiB, plus 1 byte so that "just over the limit" is caught by MaxBytesReader while reading.
//
// ⚠️ These two numbers must stay in sync with service/userfile.PartSize, but **must not import it**:
// the handler only sees the narrow interfaces it declares. 8 MiB is a protocol constant (written into openapi).
// If they drift, "the client splits at 8 MiB and the server rejects by some other number". A second check in the service layer
// and e2e tests pin it down.
const (
	userFileSimpleLimit = 8*1024*1024 + 1
	userFilePartLimit   = 8*1024*1024 + 1
)

func (h *Handler) userFilesOr503(w http.ResponseWriter, r *http.Request) bool {
	if h.userFiles == nil {
		h.failWith(w, http.StatusServiceUnavailable, "服务端未配置对象存储，文件功能不可用", "userfiles_disabled")
		return false
	}
	return true
}

func (h *Handler) registerUserFileRoutes(mux Mux) {
	mux.HandleFunc("POST /api/files", h.startUserFileUpload)
	mux.HandleFunc("PUT /api/files/{id}/content", h.putUserFileContent)
	mux.HandleFunc("PUT /api/files/{id}/parts/{partNumber}", h.putUserFilePart)
	mux.HandleFunc("POST /api/files/{id}/complete", h.completeUserFileUpload)
	mux.HandleFunc("POST /api/files/{id}/abort", h.abortUserFileUpload)
	mux.HandleFunc("GET /api/files", h.listUserFiles)
	mux.HandleFunc("GET /api/files/{id}", h.getUserFile)
	mux.HandleFunc("GET /api/files/{id}/content", h.getUserFileContent)
	mux.HandleFunc("DELETE /api/files/{id}", h.deleteUserFile)
}

// startUserFileUpload starts an upload.
func (h *Handler) startUserFileUpload(w http.ResponseWriter, r *http.Request) {
	if !h.userFilesOr503(w, r) {
		return
	}
	var body apimodel.StartUserFileUpload
	if !h.decode(w, r, &body) {
		return
	}
	f, err := h.userFiles.StartUpload(r.Context(), auth.ActorFrom(r.Context()), body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, f)
}

// putUserFileContent receives the whole file in simple mode: the body is the bytes, and Content-Type is decided by
// what was stored at StartUpload (not read here, to avoid disagreeing with the declaration).
func (h *Handler) putUserFileContent(w http.ResponseWriter, r *http.Request) {
	if !h.userFilesOr503(w, r) {
		return
	}
	size, ok := h.requireContentLength(w, r)
	if !ok {
		return
	}
	if size > 8*1024*1024 {
		h.failWith(w, http.StatusRequestEntityTooLarge,
			"simple 上传不能超过 8 MiB：大文件请走分片上传（POST /api/files 不传 size 或传更大值）",
			"upload_too_large")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, userFileSimpleLimit)
	f, err := h.userFiles.PutSimpleContent(
		r.Context(), auth.ActorFrom(r.Context()), r.PathValue("id"), size, r.Body)
	if err != nil {
		h.writeUploadErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// putUserFilePart receives one part of a multipart upload.
func (h *Handler) putUserFilePart(w http.ResponseWriter, r *http.Request) {
	if !h.userFilesOr503(w, r) {
		return
	}
	partNumber, err := strconv.Atoi(r.PathValue("partNumber"))
	if err != nil {
		h.failWith(w, http.StatusBadRequest, "片号必须是 1~10000 的整数", "invalid_part_number")
		return
	}
	size, ok := h.requireContentLength(w, r)
	if !ok {
		return
	}
	// 8 MiB is a protocol constant (same source as userfile.PartSize). The handler side pre-checks with a local copy,
	// to avoid importing the service package for one number; the service layer has a second, identical check as a backstop.
	if size > 8*1024*1024 {
		h.failWith(w, http.StatusRequestEntityTooLarge,
			"单片不能超过 8 MiB", "part_too_large")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, userFilePartLimit)
	if err := h.userFiles.UploadPart(
		r.Context(), auth.ActorFrom(r.Context()), r.PathValue("id"),
		partNumber, size, r.Body); err != nil {
		h.writeUploadErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apimodel.UserFilePart{PartNumber: partNumber})
}

func (h *Handler) completeUserFileUpload(w http.ResponseWriter, r *http.Request) {
	if !h.userFilesOr503(w, r) {
		return
	}
	f, err := h.userFiles.CompleteUpload(
		r.Context(), auth.ActorFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (h *Handler) abortUserFileUpload(w http.ResponseWriter, r *http.Request) {
	if !h.userFilesOr503(w, r) {
		return
	}
	if err := h.userFiles.AbortUpload(
		r.Context(), auth.ActorFrom(r.Context()), r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

func (h *Handler) listUserFiles(w http.ResponseWriter, r *http.Request) {
	if !h.userFilesOr503(w, r) {
		return
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			h.failWith(w, http.StatusBadRequest, "limit 必须是非负整数", "invalid_limit")
			return
		}
		limit = n
	}
	list, err := h.userFiles.ListFiles(r.Context(), auth.ActorFrom(r.Context()), limit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) getUserFile(w http.ResponseWriter, r *http.Request) {
	if !h.userFilesOr503(w, r) {
		return
	}
	f, err := h.userFiles.GetFile(
		r.Context(), auth.ActorFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// getUserFileContent downloads file content relayed through amad (streamed; the whole file is never buffered in the process).
//
// ⚠️ **Always served as an attachment download, with type always application/octet-stream**: this response shares the origin with the whole site,
// and the content is arbitrary user-uploaded bytes. Returning text/html because that was the declared type at upload would be a stored XSS
// (a same-origin page can read everything except the Cookie and call APIs as the user). nosniff stops the browser from guessing the type itself.
func (h *Handler) getUserFileContent(w http.ResponseWriter, r *http.Request) {
	if !h.userFilesOr503(w, r) {
		return
	}
	f, body, err := h.userFiles.OpenFile(
		r.Context(), auth.ActorFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", attachment.Disposition("application/octet-stream", f.Filename))
	w.Header().Set("Cache-Control", "private, no-store")
	if f.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	// Headers are already written; a disconnect midway can only be logged, the status code can no longer change.
	_, _ = io.Copy(w, body)
}

func (h *Handler) deleteUserFile(w http.ResponseWriter, r *http.Request) {
	if !h.userFilesOr503(w, r) {
		return
	}
	if err := h.userFiles.DeleteFile(
		r.Context(), auth.ActorFrom(r.Context()), r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// requireContentLength checks that the request has a real Content-Length (not chunked / unknown).
//
// The part protocol relies on it to pass the part size to object storage (the SDK sets Content-Length from it; if wrong it truncates or hangs),
// so Transfer-Encoding: chunked is not accepted. API/CLI clients sending fixed-length requests is the normal way.
// When it returns false, the response (411 / 400) has already been written.
func (h *Handler) requireContentLength(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if r.ContentLength < 0 {
		h.failWith(w, http.StatusLengthRequired,
			"必须用 Content-Length 声明字节数，不接受 chunked 请求", "content_length_required")
		return 0, false
	}
	if r.ContentLength == 0 {
		h.failWith(w, http.StatusBadRequest, "请求体为空", "empty_body")
		return 0, false
	}
	return r.ContentLength, true
}

// writeUploadErr maps errors on the upload path to status codes.
//
// The only difference from h.fail: when the request body exceeds the MaxBytesReader limit, the error is not
// a persistmodel sentinel (it is *http.MaxBytesError), so it must be turned into 413 separately;
// otherwise it falls into 500, while "file too large" is clearly a client problem.
func (h *Handler) writeUploadErr(w http.ResponseWriter, r *http.Request, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		h.failWith(w, http.StatusRequestEntityTooLarge,
			"上传内容超过 8 MiB 上限：大文件请走分片上传（先 POST /api/files 拿上传会话）",
			"upload_too_large")
		return
	}
	h.fail(w, r, err)
}
