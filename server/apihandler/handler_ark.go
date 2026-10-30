// HTTP boundary for Ark credentials: three levels of credentials (workspace / account / Agent) and "get a key from the cloud account".
//
// It is its own file instead of being stuffed into handler.go: that file was once 1800 lines, with 83 functions and 12 domains crammed
// together, and this domain alone had 18 handlers, the largest piece in it. The Service layer was split by domain long ago
// (ark.go / agentark.go / arkfetch.go), but the Handler layer had not followed, so one package had two standards.
//
// # Why this domain deserves its own file
//
// It has a shape found nowhere else: **three levels of credentials**: Agent level > workspace level > account level. Which level is used is
// decided by arkKey in core/chat.go (not here). Each level has its own set of CRUD, and the names differ only by a prefix
// (saveArkCredential / saveGlobalArkCredential / saveAgentArkCredential).
// Mixed in with 12 domains, it is the easiest place to change the wrong level, and changing the wrong level gives no error; it just makes another workspace's credential take effect.
//
// # ⚠️ The three ark-fetch endpoints return only masked values; plaintext never goes through the frontend
//
// After the user picks one, the save endpoint is called with {accountId, keyId}, and the server fetches the plaintext itself.
// See core/arkfetch.go. arkFetchRefFrom parses that reference.

package apihandler

import (
	"net/http"

	"github.com/chyroc/ama/server/model/apimodel"
)

// registerArkRoutes registers all routes of this domain. Called by Handler.Register.
//
// ⚠️ **Each of these 17 must be declared in authz/matrix.go**. Miss one and the service panics on startup
// (that is on purpose, see authz/README.md: authorization not taking effect is invisible).
func (h *Handler) registerArkRoutes(mux Mux) {
	// Ark credential (one per workspace)
	mux.HandleFunc("GET /api/workspaces/{workspaceId}/ark-credential", h.getArkCredential)
	mux.HandleFunc("PUT /api/workspaces/{workspaceId}/ark-credential", h.saveArkCredential)
	mux.HandleFunc("DELETE /api/workspaces/{workspaceId}/ark-credential", h.deleteArkCredential)
	mux.HandleFunc("PATCH /api/workspaces/{workspaceId}/ark-credential/region", h.updateArkRegion)
	mux.HandleFunc("POST /api/workspaces/{workspaceId}/ark-credential/verify", h.verifyArkCredential)
	// Make this workspace reference the account-level credential of **the current logged-in user**.
	//
	// This is the "one explicit configuration step": a workspace never silently inherits anything; someone has to
	// click once on purpose. See UseGlobalArkCredential in core/ark.go.
	mux.HandleFunc("POST /api/workspaces/{workspaceId}/ark-credential/use-global", h.useGlobalArkCredential)

	// Account-level Ark credential (the "configure once globally" one).
	//
	// Not mounted under {workspaceId}: it does not belong to any workspace, same as integrations / user-preferences.
	mux.HandleFunc("GET /api/global-ark-credential", h.getGlobalArkCredential)
	mux.HandleFunc("PUT /api/global-ark-credential", h.saveGlobalArkCredential)
	mux.HandleFunc("DELETE /api/global-ark-credential", h.deleteGlobalArkCredential)
	mux.HandleFunc("POST /api/global-ark-credential/verify", h.verifyGlobalArkCredential)

	// Three **read-only** endpoints for "get the Ark API Key from the cloud account automatically".
	//
	// None are mounted under {workspaceId}: they query **the cloud accounts of the current logged-in user**, unrelated to the workspace.
	//
	// ⚠️ **These three return only masked values and ids, no plaintext**. After the user picks one, the existing save endpoint
	// (PUT .../ark-credential or PUT /api/global-ark-credential) is called with
	// {accountId, keyId}, and the server fetches the plaintext itself. Plaintext never goes through the frontend; see core/arkfetch.go.
	mux.HandleFunc("GET /api/ark-fetch/status", h.getArkFetchStatus)
	mux.HandleFunc("GET /api/ark-fetch/accounts", h.listVolcAccounts)
	mux.HandleFunc("GET /api/ark-fetch/accounts/{accountId}/keys", h.listVolcArkKeys)

	// Agent-level Ark credential (the most specific of the three levels).
	//
	// Mounted under /api/agents/{id} (not under the workspace): the same subtree as GET/PATCH /api/agents/{id},
	// and authz resolves the owning workspace from the agent id.
	//
	// ⚠️ **"No row" means "follow the workspace"**, so there is no explicit reference endpoint like use-workspace;
	// DELETE is "switch to the workspace setting". For why, see the top of core/agentark.go.
	mux.HandleFunc("GET /api/agents/{id}/ark-credential", h.getAgentArkCredential)
	mux.HandleFunc("PUT /api/agents/{id}/ark-credential", h.saveAgentArkCredential)
	mux.HandleFunc("DELETE /api/agents/{id}/ark-credential", h.deleteAgentArkCredential)
	mux.HandleFunc("PATCH /api/agents/{id}/ark-credential/region", h.updateAgentArkRegion)
	mux.HandleFunc("POST /api/agents/{id}/ark-credential/verify", h.verifyAgentArkCredential)
}

func (h *Handler) getArkCredential(w http.ResponseWriter, r *http.Request) {
	// When not configured, the Service returns an empty shell and does not return persistmodel.ErrNotFound. The frontend uses an empty mask to tell
	// "not configured" and null to tell "loading"; returning 404 would leave it stuck on the skeleton screen forever.
	cred, err := h.ark.GetArkCredential(r.Context(), r.PathValue("workspaceId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cred)
}

func (h *Handler) saveArkCredential(w http.ResponseWriter, r *http.Request) {
	var body struct {
		APIKey string `json:"apiKey"`
		Region string `json:"region"`
		// A non-empty FetchRef means "take that key from the cloud account"; apiKey should then be empty.
		// The server fetches the plaintext itself; it does not go through the frontend. See the top of core/arkfetch.go.
		FetchRef *struct {
			AccountID string `json:"accountId"`
			KeyID     string `json:"keyId"`
		} `json:"fetchRef"`
		// ConfirmNotActivated: the user clicked "Save anyway" on the "account has not activated Managed Agents"
		// confirmation bar. It only applies to that case; a bad key ignores it. See arkcred.saveverify.go.
		ConfirmNotActivated bool `json:"confirmNotActivated"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	cred, err := h.ark.SaveArkCredential(r.Context(), r.PathValue("workspaceId"), apimodel.ArkCredentialDraft{
		APIKey: body.APIKey, Region: apimodel.ArkRegion(body.Region),
		Fetch: arkFetchRefFrom(body.FetchRef), ConfirmNotActivated: body.ConfirmNotActivated,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cred)
}

func (h *Handler) updateArkRegion(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Region string `json:"region"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	cred, err := h.ark.UpdateArkRegion(r.Context(),
		r.PathValue("workspaceId"), apimodel.ArkRegion(body.Region))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cred)
}

// verifyArkCredential checks the credential.
//
// **When the key is invalid it returns 200 + check:"invalid"**, not 401. The frontend calls this method without
// try/catch, so a thrown error becomes an unhandled rejection and the page shows nothing. The decision logic is in
// the Service; here we only write the response as usual.
func (h *Handler) verifyArkCredential(w http.ResponseWriter, r *http.Request) {
	cred, err := h.ark.VerifyArkCredential(r.Context(), r.PathValue("workspaceId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cred)
}

func (h *Handler) deleteArkCredential(w http.ResponseWriter, r *http.Request) {
	if err := h.ark.DeleteArkCredential(r.Context(), r.PathValue("workspaceId")); err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// getGlobalArkCredential reads the current logged-in user's account-level credential (masked only).
func (h *Handler) getGlobalArkCredential(w http.ResponseWriter, r *http.Request) {
	cred, err := h.ark.GetGlobalArkCredential(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cred)
}

// saveGlobalArkCredential saves the account-level credential.
//
// ⚠️ After saving, **every workspace that references it uses the new key immediately**. This is the biggest difference between this endpoint and
// the workspace-level one; the impact is wider.
func (h *Handler) saveGlobalArkCredential(w http.ResponseWriter, r *http.Request) {
	var body struct {
		APIKey string `json:"apiKey"`
		Region string `json:"region"`
		// Same as the workspace-level one: non-empty means take it from the cloud account; plaintext never goes through the frontend.
		FetchRef *struct {
			AccountID string `json:"accountId"`
			KeyID     string `json:"keyId"`
		} `json:"fetchRef"`
		// Same as the workspace-level one: after confirming the not-activated warning, still save.
		ConfirmNotActivated bool `json:"confirmNotActivated"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	cred, err := h.ark.SaveGlobalArkCredential(r.Context(), apimodel.ArkCredentialDraft{
		APIKey:              body.APIKey,
		Region:              apimodel.ArkRegion(body.Region),
		Fetch:               arkFetchRefFrom(body.FetchRef),
		ConfirmNotActivated: body.ConfirmNotActivated,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cred)
}

// deleteGlobalArkCredential deletes the account-level credential.
//
// **It does not cascade to the workspaces that reference it**. For why, see that comment in the Service.
func (h *Handler) deleteGlobalArkCredential(w http.ResponseWriter, r *http.Request) {
	if err := h.ark.DeleteGlobalArkCredential(r.Context()); err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// verifyGlobalArkCredential checks the account-level credential.
//
// Same as the workspace-level one: **when the key is invalid it returns 200 + check:"invalid"**, not 401.
// The frontend calls it without try/catch, so a thrown error becomes an unhandled rejection (the page shows nothing).
func (h *Handler) verifyGlobalArkCredential(w http.ResponseWriter, r *http.Request) {
	cred, err := h.ark.VerifyGlobalArkCredential(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cred)
}

// getAgentArkCredential reads an agent's credential view, including "which level is currently in effect".
//
// **When not configured it returns 200 + empty shell** (custom:false), not 404: the frontend uses apiKeyMasked to tell
// "does it have its own key" and null to tell "loading"; returning 404 would leave it stuck on the skeleton screen.
func (h *Handler) getAgentArkCredential(w http.ResponseWriter, r *http.Request) {
	cred, err := h.ark.GetAgentArkCredential(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cred)
}

// saveAgentArkCredential attaches this agent's own key.
//
// Same shape as the other two levels: the key is one of two (a typed apiKey, or a fetchRef pointing to an existing key in the cloud account).
func (h *Handler) saveAgentArkCredential(w http.ResponseWriter, r *http.Request) {
	var body struct {
		APIKey   string `json:"apiKey"`
		Region   string `json:"region"`
		FetchRef *struct {
			AccountID string `json:"accountId"`
			KeyID     string `json:"keyId"`
		} `json:"fetchRef"`
		// Same as the workspace-level one: after confirming the not-activated warning, still save.
		ConfirmNotActivated bool `json:"confirmNotActivated"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	cred, err := h.ark.SaveAgentArkCredential(r.Context(), r.PathValue("id"), apimodel.ArkCredentialDraft{
		APIKey:              body.APIKey,
		Region:              apimodel.ArkRegion(body.Region),
		Fetch:               arkFetchRefFrom(body.FetchRef),
		ConfirmNotActivated: body.ConfirmNotActivated,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cred)
}

// deleteAgentArkCredential switches this agent back to following the workspace setting.
//
// **This is the "switch to the workspace setting" action**: delete the agent's row, and resolution falls back to the workspace level naturally.
// There is no separate use-workspace endpoint: having no row itself means "follow".
func (h *Handler) deleteAgentArkCredential(w http.ResponseWriter, r *http.Request) {
	if err := h.ark.DeleteAgentArkCredential(r.Context(), r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// updateAgentArkRegion changes only the region of this agent's key.
//
// An agent that follows the workspace is **rejected** (the region belongs to the key it uses, and that key is not at this level).
func (h *Handler) updateAgentArkRegion(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Region string `json:"region"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	cred, err := h.ark.UpdateAgentArkRegion(r.Context(), r.PathValue("id"),
		apimodel.ArkRegion(body.Region))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cred)
}

// verifyAgentArkCredential makes one real Ark call with the key **actually in effect** for this agent.
//
// When following the workspace, the result is written back to the workspace row (no copy is kept on the agent row, or the link would silently break).
// **An invalid key still returns 200 + check:"invalid"**, same as the other two levels.
func (h *Handler) verifyAgentArkCredential(w http.ResponseWriter, r *http.Request) {
	cred, err := h.ark.VerifyAgentArkCredential(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cred)
}

// getArkFetchStatus checks whether this feature is available and whether the current user has authorized.
//
// **The three states are returned separately** (unavailable / unauthorized + auth page URL / available), because what the user must do is completely
// different. Merging into two states would show "fixable with one click" as "feature unavailable".
func (h *Handler) getArkFetchStatus(w http.ResponseWriter, r *http.Request) {
	st, err := h.ark.GetArkFetchStatus(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// listVolcAccounts lists the authorized cloud accounts of the current logged-in user, for them to pick one.
func (h *Handler) listVolcAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := h.ark.ListVolcAccounts(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, accounts)
}

// listVolcArkKeys lists the Ark API Keys under one cloud account.
//
// ⚠️ **Only masked values and ids are returned**. The save endpoint fetches the plaintext on the server; it never goes through the frontend.
func (h *Handler) listVolcArkKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.ark.ListVolcArkKeys(r.Context(), r.PathValue("accountId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

// arkFetchRefFrom converts the fetchRef in the request body into the Service input.
//
// Only when both fields are non-empty does it count as "fetch from the cloud". Giving only one is an obvious caller bug;
// treating it as nil would silently take the "typed in but the key is empty" path and report "API Key too short".
func arkFetchRefFrom(ref *struct {
	AccountID string `json:"accountId"`
	KeyID     string `json:"keyId"`
}) *apimodel.ArkKeyRef {
	if ref == nil {
		return nil
	}
	return &apimodel.ArkKeyRef{AccountID: ref.AccountID, KeyID: ref.KeyID}
}

// useGlobalArkCredential makes this workspace reference the current logged-in user's account-level credential.
//
// It returns the whole credential view (including the mask resolved from the global one), so the frontend can update the UI in one round trip.
func (h *Handler) useGlobalArkCredential(w http.ResponseWriter, r *http.Request) {
	cred, err := h.ark.UseGlobalArkCredential(r.Context(), r.PathValue("workspaceId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cred)
}
