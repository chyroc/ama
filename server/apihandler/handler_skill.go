// HTTP boundary for skills: decode the request, call the Service, map sentinel errors to status codes.
//
// It is its own file instead of staying in handler.go (the workspace domain): skill is an orchestration-layer
// domain package (orchestrator/skill). All skill routes are registered in this file, and all handlers go through
// skill.Service.

package apihandler

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
	skillsvc "github.com/chyroc/ama/server/service/skill"
)

// registerSkillRoutes registers all routes of this domain (10 core skill routes).
// Called by Handler.Register.
//
// ⚠️ **Each of these 16 must be declared in authz/matrix.go**. Miss one and the service panics on startup
// (that is on purpose, see authz/README.md: authorization not taking effect is invisible).
func (h *Handler) registerSkillRoutes(mux Mux) {
	mux.HandleFunc("GET /api/workspaces/{workspaceId}/skills", h.listSkills)
	mux.HandleFunc("POST /api/workspaces/{workspaceId}/skills", h.createSkill)
	// Upload a skill package (SKILL.md or zip). **It has an extra /upload instead of reusing the route above**:
	// one method+path can only have one handler, and this one takes multipart while the one above takes JSON.
	//
	// It does not clash with `GET /api/skills/{id}`: that one is under /api/skills, this one is under
	// /api/workspaces/{workspaceId}/skills, and the two subtrees do not overlap.
	// (Go 1.22's ServeMux panics at startup on ambiguous routes, see core/README.md.)
	mux.HandleFunc("POST /api/workspaces/{workspaceId}/skills/upload", h.uploadSkill)
	// Fork: copy a skill into a custom skill of this workspace.
	//
	// ⚠️ **It must be mounted under the workspace route, not as `POST /api/skills/{id}/fork`.**
	// For the latter, the authz Guard has to resolve the owning workspace from the skill id, and core/resource.go returns
	// ErrPlatformResource for the built-in skill prefix (authz turns that into "allow read, deny write"), so
	// **forking a built-in skill would always be 403**, and that 403 does not point at "the route shape is wrong" at all.
	// Mounted here, workspaceId is in the path and Guard does not need to resolve it. The source id goes in the body.
	mux.HandleFunc("POST /api/workspaces/{workspaceId}/skills/fork", h.forkSkill)
	mux.HandleFunc("GET /api/skills/{id}", h.getSkill)
	mux.HandleFunc("PATCH /api/skills/{id}", h.updateSkill)
	mux.HandleFunc("DELETE /api/skills/{id}", h.deleteSkill)
	// "Which entry is this skill in the Ark console." A key must be located first to answer it
	// (the Ark skill id depends on which key uploaded it); if none can be located, it returns an empty ref.
	// See GetSkillArkConsole in core/arkconsole.go.
	mux.HandleFunc("GET /api/skills/{id}/ark-console", h.getSkillArkConsole)
	// Read the content of one attachment, for the skill detail page.
	//
	// ⚠️ **The path goes in a query param, not a path segment** (`?path=scripts/init.sh`).
	// Attachment paths contain `/`. Writing it as `/api/skills/{id}/files/{path...}` has two costs:
	// first, wildcard routes have no precedent in this repo; second, the authz Guard resolves resource ownership from a **single**
	// PathValue(idParam), and an extra segment with slashes would mean changing that shared logic.
	// The precedent is `GET /api/runs/{runId}/sandbox/file?path=`; the shape is copied from it.
	mux.HandleFunc("GET /api/skills/{id}/file", h.getSkillFile)
	// Change the content of an existing attachment. **PUT, not PATCH**: the request body is the full new content of that file,
	// meaning a whole replacement, not a partial merge.
	mux.HandleFunc("PUT /api/skills/{id}/file", h.updateSkillFile)
}

func (h *Handler) listSkills(w http.ResponseWriter, r *http.Request) {
	list, err := h.skill.ListSkills(r.Context(), r.PathValue("workspaceId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) getSkill(w http.ResponseWriter, r *http.Request) {
	sk, err := h.skill.GetSkill(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sk)
}

func (h *Handler) createSkill(w http.ResponseWriter, r *http.Request) {
	var draft apimodel.SkillDraft
	if !h.decode(w, r, &draft) {
		return
	}
	sk, err := h.skill.CreateSkill(r.Context(), r.PathValue("workspaceId"), draft)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, sk)
}

func (h *Handler) updateSkill(w http.ResponseWriter, r *http.Request) {
	var draft apimodel.SkillDraft
	if !h.decode(w, r, &draft) {
		return
	}
	sk, err := h.skill.UpdateSkill(r.Context(), r.PathValue("id"), draft)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sk)
}

func (h *Handler) deleteSkill(w http.ResponseWriter, r *http.Request) {
	if err := h.skill.DeleteSkill(r.Context(), r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// getSkillFile reads the content of a skill attachment, for the detail page.
//
// The path goes in the query (`?path=`); for why, see the comment where the route is registered.
//
// **It returns JSON instead of raw bytes** (on purpose different from serveIssueAttachment for issue attachments):
// here we must tell the frontend both "it was truncated" and "this is binary", and a raw-bytes response
// has no place for those two: in a header the frontend has to parse them itself, and in the body you cannot tell content from metadata.
func (h *Handler) getSkillFile(w http.ResponseWriter, r *http.Request) {
	out, err := h.skill.SkillFileContent(
		r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// updateSkillFile changes the content of an existing attachment.
//
// # Both path and content go in the body, not the query
//
// On purpose different from the read route (`?path=`): a write carries content and already has a body, and putting the path there too is
// clearer than "half in the query, half in the body".
//
// # content is a **plain text string**, not base64
//
// It mirrors `apimodel.SkillFileContent.Content` on the response side (also a string). With base64,
// the frontend would have to encode on every save, and this endpoint is only for editing text files: binary attachments
// cannot be edited in the UI (decided by `binary: true` returned on read).
func (h *Handler) updateSkillFile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	sk, err := h.skill.UpdateSkillFile(
		r.Context(), r.PathValue("id"), body.Path, []byte(body.Content))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// Return the whole skill: the frontend needs the new `files[].size` to refresh the list, and latestVersion
	// to refresh the page header. Same response shape as PATCH /api/skills/{id}.
	writeJSON(w, http.StatusOK, sk)
}

// uploadSkill receives a skill package uploaded as multipart: parse → optionally save.
//
// # One route does two things, told apart by dryRun
//
// `?dryRun=1` only parses and returns the parse result, without saving; without it, it really creates. Why not split into two routes:
// splitting means keeping the parse result between the two requests (the frontend comes back with it to "confirm create"),
// so either the frontend sends the whole package again, or the server stores an intermediate state. Production runs many instances,
// so an in-process intermediate state would be lost when the second request lands on another instance (see CLAUDE.md).
// One route + the client sending again is the simplest stateless way: send once to preview, send again to confirm.
//
// # The size limit must come before reading
//
// http.MaxBytesReader wraps r.Body and fails **while reading** once the limit is exceeded.
// Doing ReadAll first and checking the length afterwards is pointless: a 10 GiB request would OOM the process first.
func (h *Handler) uploadSkill(w http.ResponseWriter, r *http.Request) {
	// Wrap the Body with a limit. Note that when over the limit, MaxBytesReader also marks w
	// so the connection is closed after the response. That is documented behavior, not a side effect.
	r.Body = http.MaxBytesReader(w, r.Body, skillsvc.MaxUploadBytes)

	// 32 KiB memory threshold: ParseMultipartForm writes anything beyond it to temp files.
	// This does not break "no storing on local disk": that rule is about data that **must live across requests**,
	// while the temp files here are RemoveAll'ed when this request ends (the defer below).
	if err := r.ParseMultipartForm(32 << 10); err != nil {
		if skillsvc.IsUploadTooLarge(err) {
			h.failWith(w, http.StatusRequestEntityTooLarge,
				userMessage(skillsvc.ErrUploadTooLarge), "upload_too_large")
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

	file, header, err := r.FormFile(uploadFieldName)
	if err != nil {
		h.failWith(w, http.StatusBadRequest,
			fmt.Sprintf("请选择要上传的文件（表单字段名 %s）", uploadFieldName), "invalid_payload")
		return
	}
	defer func() { _ = file.Close() }()

	// One more LimitReader: ParseMultipartForm already caps the whole request body,
	// but it does not care about the size of a single part. Reading one extra byte tells "exactly at the limit" apart from "over it".
	data, err := io.ReadAll(io.LimitReader(file, skillsvc.MaxUploadBytes+1))
	if err != nil {
		if skillsvc.IsUploadTooLarge(err) {
			h.failWith(w, http.StatusRequestEntityTooLarge,
				userMessage(skillsvc.ErrUploadTooLarge), "upload_too_large")
			return
		}
		h.failWith(w, http.StatusBadRequest, "读取上传内容失败", "invalid_payload")
		return
	}
	if len(data) > skillsvc.MaxUploadBytes {
		h.failWith(w, http.StatusRequestEntityTooLarge,
			userMessage(skillsvc.ErrUploadTooLarge), "upload_too_large")
		return
	}

	pkg, err := skillsvc.ParseSkillPackage(header.Filename, data)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	// Preview mode: return only the parse result, do not save. It gives the user a chance to notice a wrong parse before deciding to create.
	if isTruthy(r.URL.Query().Get("dryRun")) {
		writeJSON(w, http.StatusOK, pkg)
		return
	}

	// Zero-value opts = the original behavior (source=custom, no origin, version "1").
	// An uploaded skill is still **self-built**: upload is a way of creating, not a source.
	sk, err := h.skill.CreateSkillFromPackage(
		r.Context(), r.PathValue("workspaceId"), pkg, apimodel.SkillCreateOptions{})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, sk)
}

// uploadFieldName is the name of the file field in the upload form.
//
// FormData.append in the frontend api.ts must use the same name. When they do not match, the symptom is
// "please choose a file to upload": the error points at the user not choosing a file, while the field name is actually wrong.
const uploadFieldName = "file"

// isTruthy decides whether a query param means "yes".
//
// It accepts several spellings (1 / true / yes) because people will try this param by hand with curl;
// if only "1" were accepted, `?dryRun=true` would silently do a real create, a misunderstanding with side effects.
func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// getSkillArkConsole returns where this skill is in the Ark console (id + region),
// so the frontend can build a deep link. The agent and run versions have the same shape, in handler_agent.go / handler.go.
//
// **When the Ark side has not been created yet it returns an empty ref, not 404**: that is the normal lazy-creation state (the agent
// never had a chat, the skill was never used by any agent), and the frontend then does not render the link. Returning 404 would mix up "normal"
// and "resource does not exist". For the full reason see the file header of core/arkconsole.go.
func (h *Handler) getSkillArkConsole(w http.ResponseWriter, r *http.Request) {
	ref, err := h.skill.GetSkillArkConsole(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ref)
}

// ---- Fork ----

// forkSkill copies a skill into a custom skill of this workspace.
//
// The source id goes in the body instead of the path; for why, see the comment on that route in registerSkillRoutes
// (Guard resolution would make forking a built-in skill always 403, and the error would not point at the real cause).
func (h *Handler) forkSkill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SourceID string `json:"sourceId"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	sk, err := h.skill.ForkSkill(r.Context(), r.PathValue("workspaceId"), body.SourceID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, sk)
}
