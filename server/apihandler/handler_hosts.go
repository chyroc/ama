// Account-level and workspace-level HTTP routes for hosts (host executor).
//
// Account level (ActionNone, the identity is the person who registered it):
//
//	GET    /api/hosts                                      hosts I registered
//	DELETE /api/hosts/{hostId}                             revoke one
//
// Workspace level (GET is Read, because members need to see candidates in the Agent form; bind/unbind is Manage):
//
//	GET    /api/workspaces/{workspaceId}/hosts             hosts bound to the workspace
//	POST   /api/workspaces/{workspaceId}/hosts             bind (body: {hostId}; you can only bind your own machine)
//	DELETE /api/workspaces/{workspaceId}/hosts/{hostId}    unbind (an admin can remove one bound by anyone)
//
// Binding to a workspace only decides "which machines can be picked in this workspace". The
// right to assign a machine to a specific agent is checked in the agent save gate
// (service/agent.ensureExecutorAllowed): an admin can assign any bound machine, and the machine
// owner can also pick a machine they bound. On top of that, the creator can connect their own
// machine directly and personally, without going through the workspace.
//
// The register/heartbeat/result calls made by the daemon itself are in handler_hostdaemon.go.
package apihandler

import (
	"context"
	"net/http"
	"time"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// HostServices is the host-domain capability this file needs (host.Service satisfies it).
// The narrow interface is declared on the handler side, for the same reason as the other domains on Handler.
type HostServices interface {
	ListMyHosts(ctx context.Context, ownerEmail string) ([]persistmodel.Host, error)
	DeleteHost(ctx context.Context, ownerEmail, hostID string) error
	BindHost(ctx context.Context, actorEmail, workspaceID, hostID string, now time.Time) error
	ListWorkspaceHosts(ctx context.Context, workspaceID string) ([]persistmodel.BoundHost, error)
	UnbindHost(ctx context.Context, workspaceID, hostID string) error
}

// hostsOr503 has already written a 503 when it returns false (setups without the host domain wired in, such as some path tests).
func (h *Handler) hostsOr503(w http.ResponseWriter, r *http.Request) bool {
	if h.hosts == nil {
		h.failWith(w, http.StatusServiceUnavailable, "服务端未启用宿主机执行端", "host_disabled")
		return false
	}
	return true
}

func hostView(h persistmodel.Host, now time.Time) apimodel.Host {
	out := apimodel.Host{
		ID:            h.ID,
		Name:          h.Name,
		OS:            h.OS,
		Arch:          h.Arch,
		Status:        h.Status,
		Online:        h.Status == "online" && !h.LastSeenAt.IsZero() && now.Sub(h.LastSeenAt) <= 90*time.Second,
		GNUToolsOK:    h.GNUToolsOK,
		Toolcheck:     h.Toolcheck,
		NativeToolsOK: h.NativeTools,
		DaemonVersion: h.DaemonVersion,
		CreatedAt:     apimodel.FormatISO(h.CreatedAt),
	}
	if !h.LastSeenAt.IsZero() {
		out.LastSeenAt = apimodel.FormatISO(h.LastSeenAt)
	}
	return out
}

// registerHostsRoutes mounts the account-level and workspace-level host routes (called by Register).
func (h *Handler) registerHostsRoutes(mux Mux) {
	mux.HandleFunc("GET /api/hosts", h.listMyHosts)
	mux.HandleFunc("DELETE /api/hosts/{hostId}", h.deleteHost)

	mux.HandleFunc("GET /api/workspaces/{workspaceId}/hosts", h.listWorkspaceHosts)
	mux.HandleFunc("POST /api/workspaces/{workspaceId}/hosts", h.bindWorkspaceHost)
	mux.HandleFunc("DELETE /api/workspaces/{workspaceId}/hosts/{hostId}", h.unbindWorkspaceHost)
}

func (h *Handler) listMyHosts(w http.ResponseWriter, r *http.Request) {
	if !h.hostsOr503(w, r) {
		return
	}
	rows, err := h.hosts.ListMyHosts(r.Context(), auth.ActorFrom(r.Context()))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	now := time.Now()
	out := make([]apimodel.Host, 0, len(rows))
	for _, row := range rows {
		out = append(out, hostView(row, now))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) deleteHost(w http.ResponseWriter, r *http.Request) {
	if !h.hostsOr503(w, r) {
		return
	}
	if err := h.hosts.DeleteHost(
		r.Context(), auth.ActorFrom(r.Context()), r.PathValue("hostId")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listWorkspaceHosts(w http.ResponseWriter, r *http.Request) {
	if !h.hostsOr503(w, r) {
		return
	}
	rows, err := h.hosts.ListWorkspaceHosts(r.Context(), r.PathValue("workspaceId"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	now := time.Now()
	out := make([]apimodel.WorkspaceHost, 0, len(rows))
	for _, row := range rows {
		v := apimodel.WorkspaceHost{
			Host:         hostView(row.Host, now),
			OwnerEmail:   row.OwnerEmail,
			AddedByEmail: row.AddedBy,
		}
		// Existing bindings (from before the added_by column shipped) have a zero bind time. Leave it
		// empty so the frontend shows "not recorded", instead of rendering 1970.
		if !row.BoundAt.IsZero() {
			v.BoundAt = apimodel.FormatISO(row.BoundAt)
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) bindWorkspaceHost(w http.ResponseWriter, r *http.Request) {
	if !h.hostsOr503(w, r) {
		return
	}
	var body struct {
		HostID string `json:"hostId"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	if body.HostID == "" {
		h.failWith(w, http.StatusBadRequest, "hostId 不能为空", "invalid_argument")
		return
	}
	if err := h.hosts.BindHost(r.Context(), auth.ActorFrom(r.Context()),
		r.PathValue("workspaceId"), body.HostID, time.Now()); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) unbindWorkspaceHost(w http.ResponseWriter, r *http.Request) {
	if !h.hostsOr503(w, r) {
		return
	}
	if err := h.hosts.UnbindHost(
		r.Context(), r.PathValue("workspaceId"), r.PathValue("hostId")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
