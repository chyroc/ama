// HTTP layer for entity revision history.
//
// # Route shape: mounted under each entity, not one shared /api/revisions
//
// Each of the three entities has three routes (list history / get one / roll back), nine in total,
// instead of one catch-all route like `/api/revisions?kind=agent&entityId=...`. The reason is **authorization**:
//
// authz.Guard uses the ResourceKind + idParam declared on the route to work out which resource this
// request touches and which
// workspace it belongs to, then checks the role (see authz/matrix.go). It cannot read query params, so a shared route
// could only be declared as ActionNone, with the permission check done inside the Service. That is exactly what
// authz/README.md
// forbids (moving the check down into the Service bypasses the matrix, and the matrix is the only source of truth
// that machines can check against).
//
// What nine routes buy us: each one can declare in the matrix "reading needs viewer, rolling back needs member",
// and a missing declaration means **the service does not start at all**.
//
// # Why roll back is POST and not PUT/PATCH
//
// It is not "replace this resource with the request body". It is "perform an action": the request body is empty,
// and the effect is decided by the revisionId in the URL. Same as `POST /api/schedules/{id}/run-now`.
package apihandler

import (
	"net/http"

	"github.com/chyroc/ama/server/model/apimodel"
)

// registerRevisionRoutes mounts the nine revision history routes.
//
// ⚠️ **A new route must also declare its permission in authz/matrix.go**, or the service panics on startup
// (that is on purpose, see authz/guard.go). The declarations for these nine sit in that file next to their entities.
func (h *Handler) registerRevisionRoutes(mux Mux) {
	mux.HandleFunc("GET /api/agents/{id}/revisions", h.listAgentRevisions)
	mux.HandleFunc("GET /api/agents/{id}/revisions/{revisionId}", h.getAgentRevision)
	mux.HandleFunc("POST /api/agents/{id}/revisions/{revisionId}/rollback", h.rollbackAgentRevision)

	mux.HandleFunc("GET /api/skills/{id}/revisions", h.listSkillRevisions)
	mux.HandleFunc("GET /api/skills/{id}/revisions/{revisionId}", h.getSkillRevision)
	mux.HandleFunc("POST /api/skills/{id}/revisions/{revisionId}/rollback", h.rollbackSkillRevision)

	mux.HandleFunc("GET /api/schedules/{id}/revisions", h.listScheduleRevisions)
	mux.HandleFunc("GET /api/schedules/{id}/revisions/{revisionId}", h.getScheduleRevision)
	mux.HandleFunc("POST /api/schedules/{id}/revisions/{revisionId}/rollback", h.rollbackScheduleRevision)
}

// listRevisionsFor is the shared implementation of the three list handlers.
//
// **It returns `{"revisions": [...]}` instead of a bare array**: to add a field to a bare array later (such as
// "what the limit is" or "how many were cleaned up") you would have to change the response shape, while adding a
// field to an object is compatible.
// Same trade-off as setAgentIntegrations.
func (h *Handler) listRevisionsFor(
	w http.ResponseWriter, r *http.Request, kind apimodel.RevisionKind,
) {
	list, err := h.revision.ListRevisions(r.Context(), kind, r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"revisions": list,
		// Return the limit too, so the UI can say "keeps at most 20 versions" without hard-coding a
		// constant in the frontend that would go stale. The only source of truth is apimodel.MaxRevisionsPerEntity.
		"maxKept": apimodel.MaxRevisionsPerEntity,
	})
}

func (h *Handler) getRevisionFor(
	w http.ResponseWriter, r *http.Request, kind apimodel.RevisionKind,
) {
	rev, err := h.revision.GetRevision(r.Context(), kind, r.PathValue("id"), r.PathValue("revisionId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rev)
}

func (h *Handler) rollbackRevisionFor(
	w http.ResponseWriter, r *http.Request, kind apimodel.RevisionKind,
) {
	out, err := h.revision.RollbackRevision(r.Context(), kind, r.PathValue("id"), r.PathValue("revisionId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// What comes back is **the entity itself after the rollback** (Agent / Skill / Schedule),
	// so the frontend can refresh the detail page with it directly, without another GET.
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) listAgentRevisions(w http.ResponseWriter, r *http.Request) {
	h.listRevisionsFor(w, r, apimodel.RevisionKindAgent)
}

func (h *Handler) getAgentRevision(w http.ResponseWriter, r *http.Request) {
	h.getRevisionFor(w, r, apimodel.RevisionKindAgent)
}

func (h *Handler) rollbackAgentRevision(w http.ResponseWriter, r *http.Request) {
	h.rollbackRevisionFor(w, r, apimodel.RevisionKindAgent)
}

func (h *Handler) listSkillRevisions(w http.ResponseWriter, r *http.Request) {
	h.listRevisionsFor(w, r, apimodel.RevisionKindSkill)
}

func (h *Handler) getSkillRevision(w http.ResponseWriter, r *http.Request) {
	h.getRevisionFor(w, r, apimodel.RevisionKindSkill)
}

func (h *Handler) rollbackSkillRevision(w http.ResponseWriter, r *http.Request) {
	h.rollbackRevisionFor(w, r, apimodel.RevisionKindSkill)
}

func (h *Handler) listScheduleRevisions(w http.ResponseWriter, r *http.Request) {
	h.listRevisionsFor(w, r, apimodel.RevisionKindSchedule)
}

func (h *Handler) getScheduleRevision(w http.ResponseWriter, r *http.Request) {
	h.getRevisionFor(w, r, apimodel.RevisionKindSchedule)
}

func (h *Handler) rollbackScheduleRevision(w http.ResponseWriter, r *http.Request) {
	h.rollbackRevisionFor(w, r, apimodel.RevisionKindSchedule)
}
