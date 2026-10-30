// HTTP endpoints for API Keys.
//
// # Two path shapes, one handler
//
//	GET    /api/api-keys                            list **my own** keys
//	POST   /api/api-keys                            create one (the plain text is returned only this once)
//	GET    /api/workspaces/{workspaceId}/api-keys   same as above, workspace-scoped form
//	POST   /api/workspaces/{workspaceId}/api-keys   same as above
//	DELETE /api/api-keys/{id}                       revoke (only your own)
//
// **The resource is account-level**: the table has no workspace_id column, Service takes an
// email, and the SQL is `where id = $1 and email = $2`. So both path shapes return the same keys.
//
// The workspaceId in the two workspace-scoped routes **takes part in no filtering**. It has only
// one purpose there: letting authz.Guard resolve the workspace, so the caller must at least be a
// member of it. It is kept because it has been published in openapi.json; removing it would give
// a 404 to agents written against the old spec.
//
// The two account-level routes are for the "global settings" page (it is outside any workspace and
// has no workspaceId to fill in) and are declared ActionNone. See the comment on PathGlobalAPIKeys
// for why that is safe.
//
// # The plain text is never returned, except at creation
//
// The list only has the prefix (first few characters). This is not laziness: **the plain text
// does not exist**. The database only has the sha256, and the server cannot get it back either.
// So a "show my key again" feature cannot be built; you can only revoke and create a new one.

package auth

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
)

// Patterns of the four routes. Constants instead of literals: httpd registration, authz
// declarations and openapi generation all refer to the same string; a typo in one letter makes
// the service panic at startup (the better case) or makes the declaration never match.
const (
	// PathAPIKeys is the **workspace-scoped** form of list and create, kept for existing callers.
	PathAPIKeys = "/api/workspaces/{workspaceId}/api-keys"
	// PathGlobalAPIKeys is the **account-level** form of list and create, for the "global
	// settings" page.
	//
	// # Why this can be global now, although the comment above once said it could not
	//
	// The original worry was "a global pattern has no workspace → Guard can only declare
	// ActionNone → any logged-in user can call it", which moves the check down to the single
	// convention "the handler remembers to pick the right owner".
	//
	// That worry only holds if **"picking the right owner" is something that can go wrong**. Here
	// it is not: the actor is only taken from auth.ActorFrom(ctx), and **no email appears in the
	// path or the request body**, so "list someone else's keys" cannot even be expressed in the
	// parameters. It is not "checked", it is "impossible to say".
	// This is **the same pattern** as the four /api/integrations routes and the two
	// /api/user-onboarding routes; they have long been ActionNone for exactly the same reason
	// (see authz/matrix.go).
	//
	// And DELETE /api/api-keys/{id} **has been global + ActionNone from the start**. In other
	// words "revoke" has run this way for a long time, and only "list / create" still hung under
	// a workspace: three endpoints of the same domain in two shapes is itself an inconsistency.
	//
	// What really forced this change is the global settings page: it is outside any workspace
	// (account-level config should not depend on a workspace) and has no workspaceId. Making the
	// frontend first call listWorkspaces() and pick some workspace id would mean "a person who
	// belongs to no workspace cannot manage their own keys", and that person needs a key the most.
	PathGlobalAPIKeys = "/api/api-keys"
	// PathAPIKeyItem is revoke. **Not under a workspace**: revoking only needs the key's own id,
	// and ownership is decided by the SQL where clause (see Store.DeleteAPIKey).
	PathAPIKeyItem = "/api/api-keys/{id}"
)

// Mux is the route registration capability RegisterAPIKeys needs; same shape as core.Mux /
// feishu.Mux.
//
// **It is not hard-coded to *http.ServeMux because of authentication and authorization**: httpd
// passes in a decorator wrapping auth.Protect + authz.Guard. See the interface of the same name in
// core/handler.go.
type Mux interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// RegisterAPIKeys mounts the API Key routes.
//
// ⚠️ **Keeping it separate from Register is deliberate**: the five login routes of Register are
// registered on the **bare mux** (public; you cannot require "log in before logging in"), while
// these must go through the full Protect → Guard chain. Mixing them in one method would make "which
// routes are public" no longer visible in one place, and missing one gives an unauthorized entry
// point with no error at all.
//
// ⚠️ `/api/api-keys` and `/api/api-keys/{id}` **can coexist**: they have a different number of
// segments, so any concrete URL matches only one of them. What makes the Go 1.22 ServeMux panic at
// startup is "a literal segment and a wildcard segment at the same level" (see the note on
// integrations in core/handler.go).
//
// The two workspace-scoped routes and the two account-level routes **point to the same
// handler**: the handler does not read workspaceId at all, so the two shapes are the same to it.
func (h *Handler) RegisterAPIKeys(mux Mux) {
	mux.HandleFunc("GET "+PathAPIKeys, h.listAPIKeys)
	mux.HandleFunc("POST "+PathAPIKeys, h.createAPIKey)
	mux.HandleFunc("GET "+PathGlobalAPIKeys, h.listAPIKeys)
	mux.HandleFunc("POST "+PathGlobalAPIKeys, h.createAPIKey)
	mux.HandleFunc("DELETE "+PathAPIKeyItem, h.revokeAPIKey)
}

// listAPIKeys lists all keys of the current caller.
//
// **The owner is taken from the context, not from request parameters**: request parameters can be
// changed by the caller, and using them as the ownership condition would let anyone list anyone's
// keys. Only server code can write into the context.
func (h *Handler) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	actor := ActorFrom(r.Context())
	if actor == "" {
		// Getting here means Protect did not take effect (wired wrong). Return 401 to make it
		// visible, instead of treating it as "no keys" and returning an empty list, which would
		// be a silent error.
		h.failWith(w, http.StatusUnauthorized, "请先登录", "unauthenticated")
		return
	}
	keys, err := h.svc.ListAPIKeys(r.Context(), actor)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// keys is always a non-nil empty slice (guaranteed by Service), so this outputs [] not null.
	writeJSON(w, http.StatusOK, keys)
}

// createAPIKey creates a key; **the plain text appears only once, in this response**.
func (h *Handler) createAPIKey(w http.ResponseWriter, r *http.Request) {
	actor := ActorFrom(r.Context())
	if actor == "" {
		h.failWith(w, http.StatusUnauthorized, "请先登录", "unauthenticated")
		return
	}

	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		h.failWith(w, http.StatusBadRequest, "请求格式不正确", "invalid_argument")
		return
	}

	created, err := h.svc.CreateAPIKey(r.Context(), actor, body.Name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// ⚠️ **This is the only response with the plain text**. Do not add an access log that prints
	// the payload here: that writes the credential into log files, and logs are kept much longer
	// than the key lives.
	h.log.InfoContext(r.Context(), "已创建 API Key",
		slog.String("email", actor), slog.String("keyId", created.ID))
	writeJSON(w, http.StatusCreated, created)
}

// revokeAPIKey revokes a key.
//
// **Someone else's key returns 404, not 403**: a 403 would confirm "this id exists".
// For the caller both cases are handled the same way too (this key is not yours).
func (h *Handler) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	actor := ActorFrom(r.Context())
	if actor == "" {
		h.failWith(w, http.StatusUnauthorized, "请先登录", "unauthenticated")
		return
	}

	err := h.svc.RevokeAPIKey(r.Context(), actor, strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			h.failWith(w, http.StatusNotFound, "API Key 不存在", "not_found")
			return
		}
		h.fail(w, r, err)
		return
	}
	// 204: deleted, and there is no content to return. Same as the core delete endpoints.
	w.WriteHeader(http.StatusNoContent)
}
