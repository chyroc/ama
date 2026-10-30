// Connection routes for the ama daemon.
//
//	POST /api/host-daemon/register   goes through the full auth chain (authorized mux, ActionNone):
//	                                 the daemon registers with the user's AMA_TOKEN and gets back a host_token.
//	POST /api/host-daemon/heartbeat  bare mux, authenticates itself with Bearer host_token; long poll 20s.
//	POST /api/host-daemon/results    bare mux, authenticates itself with Bearer host_token.
//	GET  /api/host-daemon/ws         bare mux, authenticates itself with Bearer host_token (handshake header);
//	                                 WebSocket streaming send/receive. New daemons prefer it, and
//	                                 on handshake/keepalive failure the daemon falls back to the two above by itself.
//
// # Why heartbeat/results/WS do not go through Protect
//
// host_token is not a user identity (it does not map to auth_api_keys/auth_sessions), and a heartbeat every 20s
// should not trigger the write amplification of TouchAPIKey on the user API key. It is a "device token", the same kind as the
// terminal WS
// one-time ticket: it authenticates itself on a separate registration surface. So these three, like RegisterTerminalWS,
// are mounted on the bare mux and not in authz/matrix.go (adding them would trigger the reverse "declared but not registered" panic).
// Security is pinned down by this file's auth middleware and hostdaemon_test.go / hostdaemon_ws_test.go.
package apihandler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/persistmodel"
	hostsvc "github.com/chyroc/ama/server/service/host"
	"github.com/chyroc/ama/server/shared/hostproto"
)

// HostDaemonServices is the domain capability the daemon routes need (host.Service satisfies it).
type HostDaemonServices interface {
	Register(ctx context.Context, ownerEmail string, req hostproto.RegisterRequest, now time.Time) (hostproto.RegisterResponse, error)
	AuthenticateToken(ctx context.Context, token string) (persistmodel.Host, error)
	Heartbeat(ctx context.Context, h persistmodel.Host, req hostproto.HeartbeatRequest, now time.Time) (hostproto.HeartbeatResponse, error)
	SubmitResults(ctx context.Context, req hostproto.ResultsRequest) error
	// BeginStream starts a WS session (register the connection / kick the old one / first touch+claim), and returns the session and
	// the first batch of calls (written in hello_ack). When the host has been deleted, the error matches errors.Is(_,
	// persistmodel.ErrNotFound), and the handler then replies revoked.
	BeginStream(ctx context.Context, h persistmodel.Host, req hostproto.HeartbeatRequest, now time.Time) (hostsvc.StreamSession, hostproto.HeartbeatResponse, error)
}

type hostContextKey struct{}

// hostFromContext returns the host this request was authenticated as via host_token.
func hostFromContext(ctx context.Context) (persistmodel.Host, bool) {
	h, ok := ctx.Value(hostContextKey{}).(persistmodel.Host)
	return h, ok
}

// registerHostDaemonRegisterRoute mounts "register" (user identity). Register calls it on the
// authorized mux. The route is always registered (same policy as integrations): when the host
// domain is not wired in, the handler returns a clear 503-style error. Registration must not be skipped: that would make authz Verify
// (it is declared in matrix) panic on startup.
func (h *Handler) registerHostDaemonRegisterRoute(mux Mux) {
	mux.HandleFunc("POST /api/host-daemon/register", h.registerHostDaemon)
}

// RegisterHostDaemon mounts heartbeat/results/WS on the bare mux (httpd calls it outside Protect,
// same as RegisterTerminalWS).
func (h *Handler) RegisterHostDaemon(mux Mux) {
	mux.HandleFunc("POST /api/host-daemon/heartbeat", h.hostTokenAuth(h.hostDaemonHeartbeat))
	mux.HandleFunc("POST /api/host-daemon/results", h.hostTokenAuth(h.hostDaemonResults))
	mux.HandleFunc("GET /api/host-daemon/ws", h.hostTokenAuth(h.hostDaemonWS))
}

func (h *Handler) registerHostDaemon(w http.ResponseWriter, r *http.Request) {
	if h.hostDaemon == nil {
		h.failWith(w, http.StatusServiceUnavailable, "服务端未启用宿主机执行端", "host_disabled")
		return
	}
	var req hostproto.RegisterRequest
	if !h.decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.HostUUID) == "" {
		h.failWith(w, http.StatusBadRequest, "hostUuid 不能为空", "invalid_argument")
		return
	}
	resp, err := h.hostDaemon.Register(r.Context(), auth.ActorFrom(r.Context()), req, time.Now())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// hostTokenAuth is device-token auth on the bare mux: Bearer host_token → host row → ctx.
//
// Every failure is a bare 401, with no distinction between "missing / bad format / revoked", so we do not leak whether a token ever existed.
// There is no constant-time compare here: AuthenticateToken "hashes the token with sha256 and looks it up in an indexed column",
// so a hit or miss is decided by the database, not by a string compare. The timing side channel is the same as looking up a user by primary key
// (the auth path has always done this).
func (h *Handler) hostTokenAuth(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.hostDaemon == nil {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		raw := strings.TrimSpace(r.Header.Get("Authorization"))
		token, ok := strings.CutPrefix(raw, "Bearer ")
		if !ok || token == "" {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		hostRow, err := h.hostDaemon.AuthenticateToken(r.Context(), strings.TrimSpace(token))
		if err != nil {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), hostContextKey{}, hostRow)))
	}
}

func (h *Handler) hostDaemonHeartbeat(w http.ResponseWriter, r *http.Request) {
	hostRow, ok := hostFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	var req hostproto.HeartbeatRequest
	// Heartbeat allows an empty body (first cursor is 0). Body parse failure is only an error when "there is content but it is not JSON".
	if r.ContentLength != 0 {
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&req); err != nil {
			h.failWith(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error(), "invalid_argument")
			return
		}
	}
	req.HostID = hostRow.ID
	resp, err := h.hostDaemon.Heartbeat(r.Context(), hostRow, req, time.Now())
	if err != nil {
		// Client disconnected during the long poll: finish quietly, the response can no longer be written.
		if r.Context().Err() != nil {
			return
		}
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) hostDaemonResults(w http.ResponseWriter, r *http.Request) {
	hostRow, ok := hostFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	var req hostproto.ResultsRequest
	if !h.decode(w, r, &req) {
		return
	}
	// hostId comes from the host authenticated by the token; do not trust what the body claims (prevents using A's token to answer B's calls).
	req.HostID = hostRow.ID
	if err := h.hostDaemon.SubmitResults(r.Context(), req); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
