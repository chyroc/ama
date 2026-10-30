// HTTP boundary: decode requests, call Service, map sentinel errors to status codes and Chinese messages.
//
// This layer is deliberately thin: no business decisions, only protocol conversion and error mapping. All business rules live in Service.
//
// # Responses never contain AppSecret
//
// Not because it is filtered here, but because the App struct has no such field (see app.go). The secret only flows inside Store
// and between check calls; the serialization path cannot reach it at all.

package feishu

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/chyroc/ama/server/auth"
)

// Handler exposes Service as HTTP endpoints.
type Handler struct {
	svc *Service
	log *slog.Logger
}

// NewHandler creates a Handler. When log is empty it uses the slog default logger.
func NewHandler(svc *Service, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{svc: svc, log: log}
}

// Register mounts this module's routes on mux.
//
// It uses Go 1.22+ method + wildcard routes, without a routing library.
//
// Paths come in two groups: those with a workspace are collection operations, and single-app operations only take appID; the latter match the
// existing shape of the frontend ApiClient (item-level methods only pass an opaque id).
// Mux is the route registration ability Register needs.
//
// It is not hard-coded as *http.ServeMux because of auth: httpd passes in a decorator that wraps the auth middleware,
// which wraps each handler in auth.Protect before handing it to the real mux. The reason is the same as core.Mux
// (auth must wrap each route; it cannot wrap the whole mux, or 405 would become 401);
// see the comment on Mux in core/handler.go.
type Mux interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

func (h *Handler) Register(mux Mux) {
	mux.HandleFunc("GET /api/workspaces/{workspaceId}/feishu/apps", h.listApps)
	// Chat (group) list: the dropdown data source when configuring "which chat to send to" for scheduled tasks.
	mux.HandleFunc("GET /api/workspaces/{workspaceId}/feishu/chats", h.listChats)
	mux.HandleFunc("POST /api/workspaces/{workspaceId}/feishu/registrations", h.beginRegistration)
	mux.HandleFunc("GET /api/workspaces/{workspaceId}/feishu/registrations/{sessionId}", h.pollRegistration)
	mux.HandleFunc("GET /api/feishu/apps/{id}", h.getApp)
	mux.HandleFunc("POST /api/feishu/apps/{id}/readiness/refresh", h.refreshReadinessEndpoint)
	mux.HandleFunc("POST /api/feishu/apps/{id}/admin-authorization", h.requestAdminAuthorization)
	// Trigger mode: PATCH instead of POST, since it changes a field on the app rather than starting an action
	// (compare readiness/refresh and admin-authorization, which both have remote side effects).
	mux.HandleFunc("PATCH /api/feishu/apps/{id}/trigger-mode", h.setTriggerMode)
	mux.HandleFunc("DELETE /api/feishu/apps/{id}", h.deleteApp)
}

// listChats lists the "chats the bot has seen", for scheduled tasks to pick a delivery target.
//
// Same shape as listApps (wrapped in {"chats": [...]}): all of these feishu endpoints return this way,
// and mixing in bare arrays would split the frontend's unwrapping logic.
//
// When no chat has been recorded yet it returns 200 + an empty array, **not 404**: that is the normal state of "the bot has not been @-mentioned in a chat yet",
// and the UI should show a hint instead of an error (same as "list endpoints never 404" on the core side).
func (h *Handler) listChats(w http.ResponseWriter, r *http.Request) {
	chats, err := h.svc.ListChats(r.Context(), ListOption{
		ActorID:     ActorFrom(r),
		WorkspaceID: r.PathValue("workspaceId"),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// Convert to the external shape. Two things to watch:
	//
	// 1. Internal records carry fields the frontend does not need, like workspaceId, and key names must be converted to camelCase.
	//
	// 2. **feishuAppId must be the AMA-side app ID, not Feishu's cli_xxx.**
	// ChatRecord.LarkAppID stores the latter (inbound messages only carry that), while the DeliveryTarget
	// field refers to feishu.App.ID: on delivery scheduleDeliverer uses it to call Store.GetApp.
	// Returning cli_xxx directly would mean the address saved by the scheduled task can never find the app,
	// showing up as **everything fine when configuring, failing only when delivery time comes**. So translate it here.
	byLarkID := map[string]string{}
	if apps, err := h.svc.ListApps(r.Context(), ListOption{
		ActorID:     ActorFrom(r),
		WorkspaceID: r.PathValue("workspaceId"),
	}); err == nil {
		for _, app := range apps {
			byLarkID[app.AppID] = app.ID
		}
	} else {
		// When it cannot be translated, **fail as a whole** instead of returning a list with wrong IDs: the latter would let users configure a task
		// that is bound to fail when it runs, and the failure would happen hours later, with no visible link to this configuration.
		h.fail(w, r, err)
		return
	}

	out := make([]map[string]any, 0, len(chats))
	for _, c := range chats {
		amaAppID, ok := byLarkID[c.LarkAppID]
		if !ok {
			// The app was disconnected but the chat record remains. Skip it: listing it would only let users configure a task that cannot work.
			continue
		}
		out = append(out, map[string]any{
			"chatId":      c.ChatID,
			"feishuAppId": amaAppID,
			"chatType":    c.ChatType,
			"name":        c.Name,
			"lastSeenAt":  c.LastSeenAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": out})
}

func (h *Handler) listApps(w http.ResponseWriter, r *http.Request) {
	apps, err := h.svc.ListApps(r.Context(), ListOption{
		ActorID:          ActorFrom(r),
		WorkspaceID:      r.PathValue("workspaceId"),
		RefreshReadiness: r.URL.Query().Get("refreshReadiness") == "true",
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": apps})
}

func (h *Handler) beginRegistration(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Region              string `json:"region"`
		AppID               string `json:"appId"`
		RequestGroupMessage bool   `json:"requestGroupMessage"`
	}
	// An empty body is legal: all three fields are optional. So only error when it really cannot be decoded.
	if err := decodeOptionalJSON(r, &body); err != nil {
		h.failWith(w, http.StatusBadRequest, "请求格式不正确", "invalid_payload")
		return
	}
	challenge, err := h.svc.Begin(r.Context(), BeginOption{
		ActorID:             ActorFrom(r),
		WorkspaceID:         r.PathValue("workspaceId"),
		Region:              body.Region,
		AppID:               body.AppID,
		RequestGroupMessage: body.RequestGroupMessage,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, challenge)
}

func (h *Handler) pollRegistration(w http.ResponseWriter, r *http.Request) {
	result, err := h.svc.Poll(r.Context(), PollOption{
		ActorID:     ActorFrom(r),
		WorkspaceID: r.PathValue("workspaceId"),
		SessionID:   r.PathValue("sessionId"),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) getApp(w http.ResponseWriter, r *http.Request) {
	app, err := h.svc.GetApp(r.Context(), ActorFrom(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func (h *Handler) refreshReadinessEndpoint(w http.ResponseWriter, r *http.Request) {
	app, err := h.svc.RefreshReadiness(r.Context(), ActorFrom(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func (h *Handler) requestAdminAuthorization(w http.ResponseWriter, r *http.Request) {
	app, err := h.svc.RequestAdminAuthorization(r.Context(), ActorFrom(r), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

// setTriggerMode changes the trigger mode and returns the whole app (one round trip is enough to update the page, consistent with refresh).
//
// The request body is {"triggerMode": "mention_only" | "topic_and_mention"}. **Values that cannot be decoded
// are not rejected in this layer**: Service normalizes them to the default mode; see Service.SetTriggerMode for why.
func (h *Handler) setTriggerMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TriggerMode string `json:"triggerMode"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		h.fail(w, r, err)
		return
	}
	app, err := h.svc.SetTriggerMode(
		r.Context(), ActorFrom(r), r.PathValue("id"), TriggerMode(body.TriggerMode))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, app)
}

func (h *Handler) deleteApp(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteApp(r.Context(), ActorFrom(r), r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id")})
}

// fail maps business errors to HTTP responses.
//
// message is a Chinese sentence for users (shown directly by the frontend), and code is a stable identifier for programs
// (when i18n is done later, translation keys map from it, without parsing Chinese).
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrInvalidArgument):
		h.failWith(w, http.StatusBadRequest, "请求参数不完整", "invalid_argument")
	case errors.Is(err, ErrAccessDenied):
		h.failWith(w, http.StatusForbidden, "你没有权限执行这个操作", "access_denied")
	case errors.Is(err, ErrSessionLost):
		h.failWith(w, http.StatusNotFound, "接入会话已失效，请重新发起", "session_lost")
	case errors.Is(err, ErrAppNotFound):
		h.failWith(w, http.StatusNotFound, "这个飞书应用不存在", "app_not_found")
	case errors.Is(err, ErrAppOwnedByOther):
		h.failWith(w, http.StatusConflict, "这个飞书应用已被其他人接入", "app_owned_by_other")
	default:
		// Upstream failures and code bugs all land here. Details only go into the log: the error message may carry
		// internal identifiers returned by Feishu, which should not be shown to users directly.
		h.log.ErrorContext(r.Context(), "飞书接口处理失败",
			slog.String("path", r.URL.Path), slog.String("error", err.Error()))
		h.failWith(w, http.StatusBadGateway, "飞书接口调用失败，请稍后重试", "upstream_failed")
	}
}

func (h *Handler) failWith(w http.ResponseWriter, status int, message, code string) {
	writeJSON(w, status, map[string]any{"message": message, "code": code})
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// decodeOptionalJSON decodes a request body that may be empty.
func decodeOptionalJSON(r *http.Request, out any) error {
	if r.Body == nil {
		return nil
	}
	err := json.NewDecoder(r.Body).Decode(out)
	if err != nil && strings.Contains(err.Error(), "EOF") {
		return nil
	}
	return err
}

// ActorFrom gets the caller identity of this request (employee email).
//
// **The identity comes from the Identity the auth middleware writes into context**, no longer from a request header.
//
// This used to read the X-AMA-Actor header, a placeholder from before there was a login state. It had a fatal weakness:
// **request headers are controlled by the caller**; if any middleware failed to overwrite the original value, users could send their own
// X-AMA-Actor and impersonate someone else. context can only be written by server code, so that weakness does not exist.
//
// The signature of this function did not change, so not a single call site in this file needs to change:
// that is exactly why it was gathered into one function in the first place.
func ActorFrom(r *http.Request) string {
	return auth.ActorFrom(r.Context())
}
