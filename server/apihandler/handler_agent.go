// HTTP boundary for Agent: decode the request, call the Service, map sentinel errors to status codes.
//
// It is its own file instead of staying in handler.go (the workspace domain): agent is already the tenth
// orchestration-layer domain package (orchestrator/agent). Before, all 10 of these routes were in Register in handler.go,
// and all 9 handlers called workspace.Service (at the time the only mixed domain spanning several sources). After
// the split, writing `h.workspace.GetAgent(...)` in handler.go fails to compile right away.
//
// 9 handlers for 10 routes: the two sync-task routes are one GET query and one POST retry,
// not one method mounted two ways.

package apihandler

import (
	"errors"
	"net/http"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// registerAgentRoutes registers all routes of this domain. Called by Handler.Register.
//
// ⚠️ **Each of these 10 must be declared in authz/matrix.go**. Miss one and the service panics on startup
// (that is on purpose, see authz/README.md: authorization not taking effect is invisible).
func (h *Handler) registerAgentRoutes(mux Mux) {
	mux.HandleFunc("GET /api/workspaces/{workspaceId}/agents", h.listAgents)
	mux.HandleFunc("POST /api/workspaces/{workspaceId}/agents", h.createAgent)
	mux.HandleFunc("GET /api/agents/{id}", h.getAgent)
	mux.HandleFunc("PATCH /api/agents/{id}", h.updateAgent)
	mux.HandleFunc("DELETE /api/agents/{id}", h.deleteAgent)
	// Change "which integrations this agent may inject". **It is its own route because it is its own level**
	// (ActionManage, while PATCH /api/agents/{id} is Write).
	// For why PATCH cannot simply be raised to Manage, see the long comment in authz/matrix.go.
	mux.HandleFunc("PUT /api/agents/{id}/integrations", h.setAgentIntegrations)
	// "After saving, has Ark caught up yet?" The frontend polls this and only shows a message on failure.
	// See the file header of core/agentsync.go.
	mux.HandleFunc("GET /api/agents/{id}/sync-task", h.getAgentSyncTask)
	mux.HandleFunc("POST /api/agents/{id}/sync-task", h.retryAgentSync)
	// "Which entry is this agent in the Ark console." Ark resources are created lazily; when not created yet
	// it returns an empty ref (not 404), and the frontend then does not render the link. See core/arkconsole.go.
	mux.HandleFunc("GET /api/agents/{id}/ark-console", h.getAgentArkConsole)
}

func (h *Handler) listAgents(w http.ResponseWriter, r *http.Request) {
	list, err := h.agent.ListAgents(r.Context(), r.PathValue("workspaceId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) getAgent(w http.ResponseWriter, r *http.Request) {
	a, err := h.agent.GetAgent(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *Handler) createAgent(w http.ResponseWriter, r *http.Request) {
	var draft apimodel.AgentDraft
	if !h.decode(w, r, &draft) {
		return
	}
	a, err := h.agent.CreateAgent(r.Context(), r.PathValue("workspaceId"), draft)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

// updateAgent receives the full draft.
//
// The frontend updateAgent signature is Partial<AgentDraft>, but real calls always pass the full object
// (toDraft in AgentDetailPage). Using the full object instead of per-field pointers also avoids
// the ModelConfig null trap (see the comment on apimodel.ModelConfig).
func (h *Handler) updateAgent(w http.ResponseWriter, r *http.Request) {
	var draft apimodel.AgentDraft
	if !h.decode(w, r, &draft) {
		return
	}
	a, err := h.agent.UpdateAgent(r.Context(), r.PathValue("id"), draft)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *Handler) deleteAgent(w http.ResponseWriter, r *http.Request) {
	if err := h.agent.DeleteAgent(r.Context(), r.PathValue("id")); err != nil {
		h.fail(w, r, err)
		return
	}
	writeNoContent(w)
}

// getAgentSyncTask returns the result of this agent's latest "sync to Ark", for the frontend to poll.
//
// ⚠️ **With no record it returns 200 + status:"none", not 404.**
//
// On the frontend, 404 already means "this agent does not exist" (see the 403/404 distinction in
// authz/README.md), while "no sync record" here is a perfectly normal state: an existing agent was never saved,
// or the record passed its retention period and was cleaned up. Returning 404 would make the frontend render it as an error,
// and then every existing agent's detail page would show a red error line.
//
// This is the same rule as "list endpoints never return 404" (see the handler.go file header).
func (h *Handler) getAgentSyncTask(w http.ResponseWriter, r *http.Request) {
	task, err := h.agent.LatestAgentSyncTask(r.Context(), r.PathValue("id"))
	if errors.Is(err, persistmodel.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "none"})
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// retryAgentSync queues one more sync. It backs the "Retry" button next to the failure message.
//
// The difference from the save path is that **errors are returned upward**: the user clicked this, so if it cannot be queued they need to know.
func (h *Handler) retryAgentSync(w http.ResponseWriter, r *http.Request) {
	task, err := h.agent.RetryAgentSync(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, task)
}

// setAgentIntegrations changes "which integrations this agent may inject".
//
// It is its own route so it can have its own permission level (ActionManage). For the reason see
// that section in authz/matrix.go.
//
// It takes `{"integrationIds": [...]}` instead of a bare array: adding a field to a bare array later means changing the response shape,
// while adding a field to an object is compatible. An empty array is valid (= inject none), the same
// rule as on the UpdateAgent side.
func (h *Handler) setAgentIntegrations(w http.ResponseWriter, r *http.Request) {
	var body struct {
		// **Pointer**: we need to tell "an empty array was sent" (remove all) apart from "the key was not sent at all".
		// With a value type both are nil, so a caller that misspells the key name would silently clear every integration.
		IntegrationIDs *[]string `json:"integrationIds"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	if body.IntegrationIDs == nil {
		h.failWith(w, http.StatusBadRequest,
			"请求体缺少 integrationIds 字段", "invalid_argument")
		return
	}
	a, err := h.agent.SetAgentIntegrations(
		r.Context(), r.PathValue("id"), *body.IntegrationIDs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// getAgentArkConsole / getSkillArkConsole / getRunArkConsole return where this entity is in the Ark
// console (id + region), so the frontend can build a deep link.
//
// **When the Ark side has not been created yet it returns an empty ref, not 404**: that is the normal lazy-creation state (the agent
// never had a chat, the skill was never used by any agent), and the frontend then does not render the link. Returning 404 would mix up "normal"
// and "resource does not exist". For the full reason see the file header of core/arkconsole.go.
func (h *Handler) getAgentArkConsole(w http.ResponseWriter, r *http.Request) {
	ref, err := h.agent.GetAgentArkConsole(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ref)
}
