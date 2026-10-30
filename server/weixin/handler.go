package weixin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/chyroc/ama/server/auth"
)

// Handler is the HTTP side of the WeChat channel. Every route is under a workspace, so authz checks the
// caller's role there (see authz/matrix.go); the handler only makes sure the resource is in that workspace.
type Handler struct {
	svc *Service
	log *slog.Logger
}

// NewHandler creates a Handler.
func NewHandler(svc *Service, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{svc: svc, log: log}
}

// Mux is the part of http.ServeMux the handler needs.
type Mux interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// Register mounts the routes. Each one must also be declared in authz/matrix.go.
func (h *Handler) Register(mux Mux) {
	mux.HandleFunc("GET /api/workspaces/{workspaceId}/weixin/accounts", h.listAccounts)
	mux.HandleFunc("PATCH /api/workspaces/{workspaceId}/weixin/accounts/{accountId}", h.updateAccount)
	mux.HandleFunc("DELETE /api/workspaces/{workspaceId}/weixin/accounts/{accountId}", h.deleteAccount)
	mux.HandleFunc("POST /api/workspaces/{workspaceId}/weixin/logins", h.startLogin)
	mux.HandleFunc("GET /api/workspaces/{workspaceId}/weixin/logins/{loginId}", h.pollLogin)
	mux.HandleFunc("POST /api/workspaces/{workspaceId}/weixin/logins/{loginId}/verify-code", h.submitVerifyCode)
}

func (h *Handler) listAccounts(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.ListAccounts(r.Context(), r.PathValue("workspaceId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// updateAccount changes the bound agent: {"agentId": "..."}; "" unbinds.
func (h *Handler) updateAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AgentID *string `json:"agentId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.AgentID == nil {
		h.fail(w, r, ErrInvalidArgument)
		return
	}
	out, err := h.svc.BindAgent(r.Context(), r.PathValue("workspaceId"), r.PathValue("accountId"), *body.AgentID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) deleteAccount(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteAccount(r.Context(), r.PathValue("workspaceId"), r.PathValue("accountId")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) startLogin(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.StartLogin(r.Context(), r.PathValue("workspaceId"), auth.ActorFrom(r.Context()))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handler) pollLogin(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.PollLogin(r.Context(), r.PathValue("workspaceId"), auth.ActorFrom(r.Context()), r.PathValue("loginId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) submitVerifyCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		h.fail(w, r, ErrInvalidArgument)
		return
	}
	out, err := h.svc.SubmitVerifyCode(r.Context(), r.PathValue("workspaceId"), auth.ActorFrom(r.Context()),
		r.PathValue("loginId"), body.Code)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// fail maps service errors to {"message", "code"}: message is shown to the user, code is for programs.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrInvalidArgument):
		failWith(w, http.StatusBadRequest, "请求参数不完整", "invalid_argument")
	case errors.Is(err, ErrAccountNotFound):
		failWith(w, http.StatusNotFound, "这个微信账号不存在", "weixin_account_not_found")
	case errors.Is(err, ErrLoginNotFound):
		failWith(w, http.StatusNotFound, "登录已失效，请重新扫码", "weixin_login_not_found")
	case errors.Is(err, ErrVerifyCodeNotAsked):
		failWith(w, http.StatusConflict, "当前不需要输入验证码", "weixin_verify_code_not_asked")
	case errors.Is(err, ErrAgentNotFound):
		failWith(w, http.StatusNotFound, "Agent 不存在", "agent_not_found")
	case errors.Is(err, ErrAgentInOtherSpace):
		failWith(w, http.StatusBadRequest, "Agent 不属于该空间", "agent_in_other_workspace")
	case errors.Is(err, ErrBotInOtherSpace):
		failWith(w, http.StatusConflict, "这个微信机器人已接入别的空间", "weixin_bot_in_other_workspace")
	case errors.Is(err, ErrUpstream):
		h.log.WarnContext(r.Context(), "调用微信接口失败", "path", r.URL.Path, "error", err.Error())
		failWith(w, http.StatusBadGateway, "微信接口暂时不可用，请稍后再试", "weixin_upstream")
	default:
		h.log.ErrorContext(r.Context(), "微信接口处理失败", "path", r.URL.Path, "error", err.Error())
		failWith(w, http.StatusInternalServerError, "服务内部错误，请稍后再试", "internal")
	}
}

func failWith(w http.ResponseWriter, status int, message, code string) {
	writeJSON(w, status, map[string]any{"message": message, "code": code})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
