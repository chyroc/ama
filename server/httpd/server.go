// Package httpd is the HTTP service skeleton of the AMA backend.
//
// It is modeled on an existing Feishu integration and reimplemented at AMA's scale.
//
// # Why there is no routing library
//
// Since Go 1.22 the standard library ServeMux supports "method + path wildcards", i.e.
// `mux.HandleFunc("GET /api/x/{id}", h)` with `r.PathValue("id")`. That is all the routing this
// project needs; bringing in chi / gin would only add a dependency.
//
// # What this layer is responsible for
//
// Only wiring and cross-cutting concerns: route registration, CORS, panic recovery, access logs.
// Business logic lives in its own modules, and this package knows no business concept, except for
// chaining the modules' Register calls in New.
//
// # Usage
//
//	srv, err := httpd.New(httpd.Config{Addr: ":8080", FeishuService: svc})
//	if err != nil {
//	    return err
//	}
//	return srv.Run(ctx)
package httpd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/authz"
	"github.com/chyroc/ama/server/feishu"
	"github.com/chyroc/ama/server/openapi"
	"github.com/chyroc/ama/server/orchestrator"
	"github.com/chyroc/ama/server/weixin"

	"github.com/chyroc/ama/server/shared/gopkg/requestid"
)

// Server read/write timeouts. Device code polling uses short requests, so this order of magnitude
// is enough.
//
// **writeTimeout is 0 (no limit)**, for SSE.
//
// The chat stream (GET /api/runs/{id}/events/stream) is a long-lived connection: the agent may think
// as long as it wants, and the user may keep the page open all day. net/http's WriteTimeout is a
// deadline for **the whole response**, not an idle limit "between two writes". Set to 60 seconds,
// any conversation longer than a minute would be cut hard at second 60, showing up as "the reply
// stops halfway", with nothing in the server logs.
//
// The cost is that a slow client can hold a connection forever. ReadHeaderTimeout is still there
// and stops scanners that connect without sending; a real write timeout should be per route
// (http.ResponseController.SetWriteDeadline), added when needed. Do not kill SSE along with it.
const (
	readHeaderTimeout = 10 * time.Second
	writeTimeout      = 0
	shutdownTimeout   = 10 * time.Second
)

// Config is the configuration needed to build a Server.
type Config struct {
	// Addr is the listen address, like ":8080". Required.
	Addr string

	// FeishuService is the Feishu integration module. Required.
	FeishuService *feishu.Service
	// WeixinService is the WeChat channel (server/weixin). Optional: when nil its routes still exist
	// (every route declared in authz/matrix.go must be registered) and answer 503.
	WeixinService *weixin.Service

	// CoreHandler is the set of routes of the core business module (workspaces / members / agents
	// / skills / Ark credentials / run history).
	//
	// Its type is a **narrow interface of the orchestration layer** (only Register /
	// RegisterTerminalWS), not *core.Handler: the wiring layer should not see the rest of the
	// handler, and should not be recompiled when core's signatures change. The implementation is
	// still *core.Handler, passed in by cmd/amad.
	//
	// If empty, the endpoints of those six domains are not registered; the Feishu path can still
	// run on its own.
	//
	// ⚠️ The nil check is `!= nil`, and this is now an **interface**: passing a typed nil
	// (`var h *core.Handler; cfg.CoreHandler = h`) is not detected; it goes into the register
	// branch and panics. All current call sites either pass a real handler or leave the field
	// unset; both are safe. This is the classic Go interface trap; core.NewHandler notes the same.
	CoreHandler orchestrator.Routes

	// AllowedOrigin is the origin allowed for cross-origin requests, like "http://localhost:5273".
	//
	// Empty means no CORS headers are sent. During development the frontend runs on 5273 and the
	// backend on another port, so it is needed; a same-origin production deployment does not need
	// it. **"*" is not supported**: a wildcard origin for credentialed requests opens them to every
	// site; to allow one, name that site explicitly.
	AllowedOrigin string

	// AuthHandler is the login and authentication module. Required.
	//
	// It provides two things: the five login endpoints (public), and the Protect middleware
	// (protecting all other endpoints).
	AuthHandler *auth.Handler

	// Authz is the authorization decorator. Required.
	//
	// It decides by "workspace member role" whether a logged-in request may pass. The split with
	// AuthHandler is **authentication vs authorization**: the former answers "who are you" (401 on
	// failure), the latter "may you" (403 on failure).
	//
	// Required instead of optional: empty would mean no authorization, and that **fails silently**:
	// all endpoints respond as usual, only anyone can operate on any workspace. Better not to start.
	Authz *authz.Guard

	// Logger defaults to the slog default logger.
	Logger *slog.Logger
}

func (c Config) validate() error {
	if strings.TrimSpace(c.Addr) == "" {
		return errors.New("httpd: Addr 不能为空")
	}
	if c.FeishuService == nil {
		return errors.New("httpd: FeishuService 不能为 nil")
	}
	if c.AuthHandler == nil {
		return errors.New("httpd: AuthHandler 不能为 nil")
	}
	if c.Authz == nil {
		return errors.New("httpd: Authz 不能为 nil")
	}
	if strings.TrimSpace(c.AllowedOrigin) == "*" {
		return errors.New("httpd: AllowedOrigin 不能是 *，请写明具体来源")
	}
	return nil
}

// Server is the HTTP service.
type Server struct {
	cfg  Config
	log  *slog.Logger
	http *http.Server
}

// New builds a Server from the config.
func New(cfg Config) (*Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	// Backend routes form their own mux, and **no "/" catch-all is registered on it**.
	//
	// The standard library's 405 relies on "no other pattern matches this path": once the mux has
	// "/", a PUT to an endpoint that only accepts POST is judged to match "/", and gets that
	// catch-all's response instead of 405. Measured (see the isBackendPath comment in static.go):
	// after registering "/", TestMethodRouting went from 405 to 200 + an HTML page.
	//
	// So the frontend static service lives **outside this mux**, and routeFrontend splits by path:
	// backend paths go to the mux (405 / core's JSON 404 both come from it), everything else goes to
	// the SPA.
	mux := http.NewServeMux()
	// Health check. **Probe hits are logged by sampling**; see the comment on logHealthz. That is
	// the only way to tell "did the probe actually reach the process".
	var healthzHits atomic.Uint64
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		logHealthz(log, r, healthzHits.Add(1))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	// Login endpoints are **not protected**: requiring "logged in before you can log in" is a
	// deadlock. All nine are under /api/auth/; see Register in auth/handler.go.
	cfg.AuthHandler.Register(mux)

	// WebSocket probe endpoint (`/websocket`). **Not protected and not in the matrix**; see the
	// comment on registerWebSocketProbe in wsprobe.go: authentication would stop the handshake
	// from happening at all, and a 401 could not tell whether the gateway swallowed the upgrade
	// header or we blocked it ourselves.
	registerWebSocketProbe(mux, log)

	// The terminal WebSocket endpoint. **Registered on the bare mux, deliberately not through
	// Protect/Guard.**
	//
	// It may be reached on another domain (a separate WS entry), and that connection **has no
	// cookie** (ama_session is host-only), while browser WebSockets cannot send custom request
	// headers, so the standard authentication chain does not apply here. Instead it redeems a
	// one-time ticket, issued by the same-origin endpoint
	// `POST /api/runs/{runId}/sandbox/terminal/ticket`, which has already passed authentication +
	// authz when issuing. The full argument is in the file header of core/terminalticket.go.
	//
	// ⚠️ **So it bypasses the gate "every new route must be declared in the matrix"** (declaring
	// it would trigger the reverse panic "declared but not registered"). Tests make up for the
	// bypassed gate: on the core side a group of cases pins "no ticket / expired ticket / used
	// ticket / someone else's run all fail to connect". Read those cases before touching this route.
	if cfg.CoreHandler != nil {
		cfg.CoreHandler.RegisterTerminalWS(mux)
		// Heartbeats/results of the ama daemon: also registered on the bare mux, self-authenticated
		// (host_token), not in the matrix. Same reason as the terminal WS; the full argument is in
		// the file header of apihandler/handler_hostdaemon.go; security is pinned by the auth
		// middleware and tests there.
		cfg.CoreHandler.RegisterHostDaemon(mux)
	}

	// The self-describing endpoints (OpenAPI document + /llms.txt) are **also not protected**, for
	// a variant of the same reason: an external agent must first understand this platform before
	// it can authenticate, and requiring "authenticate before you learn how to authenticate" is
	// also a deadlock.
	//
	// Only the **structure** is exposed (field names, paths, permission levels), no instance data:
	// those two handlers have no Store and know no workspace. The security boundary is described in
	// the file header of openapi/handler.go and pinned by TestPublicRoutesExposeNoTenantData.
	//
	// ⚠️ They are registered on the **bare mux**, not on authorized, so they **must not** be
	// written into authz/matrix.go: that would trigger the reverse panic "declared but not
	// registered".
	openapi.NewHandler().Register(mux)

	// All other business endpoints require login.
	//
	// **Authentication wraps each handler, not the whole mux.** guarded is a decorator: it passes
	// the handler through auth.Protect before giving it to the real mux, so the dozens of route
	// registrations in core / feishu need no change at all.
	//
	// Why not wrap the outside: the standard library's 405 relies on "match the route first, then
	// check the method". If authentication came before routing, a GET to an endpoint that only
	// accepts POST would get 401 instead of 405; TestMethodRouting fails right away, and the error
	// does not point to the real cause at all. For the measured comparison see
	// auth.TestProtectAfterRoutingPreserves405.
	guarded := &protectedMux{mux: mux, protect: cfg.AuthHandler.ProtectFunc}

	// Then add a layer of **authorization**. The chain from outside to inside is:
	//
	//	mux routing → Protect (authentication: who are you, 401 on failure) → Guard
	//	(authorization: may you, 403 on failure) → business handler
	//
	// Authorization must come after authentication (without an identity there is no role to
	// check), and after routing (otherwise a request with the wrong method gets 403 instead of 405,
	// same reason as protectedMux).
	//
	// Guard looks up a permission table by method + pattern, and **a route not declared in the
	// table panics at registration**. So any route added in the two Register calls below must also
	// be declared in authz/matrix.go.
	authorized := cfg.Authz.Wrap(guarded)

	feishu.NewHandler(cfg.FeishuService, log).Register(authorized)
	weixin.NewHandler(cfg.WeixinService, log).Register(authorized)
	// The three API Key routes: account-level resources, but through the full authentication +
	// authorization chain.
	//
	// They are registered separately from the five login endpoints above, and **this separation is
	// a security boundary**: those five must be public (callable before login), these three must be
	// protected (they operate on long-lived credentials). Mixing them in one Register would make
	// "which routes are public" no longer visible in one place.
	cfg.AuthHandler.RegisterAPIKeys(authorized)
	// A platform admin creates a local account for someone (POST /api/admin/users). Needs login;
	// whether the caller is an admin is decided by the handler.
	cfg.AuthHandler.RegisterAccounts(authorized)
	// The six core business domains. Its paths do not overlap with Feishu: Feishu is under
	// /api/feishu/ and /api/workspaces/{id}/feishu/, core is under /api/workspaces (including members
	// / agents / skills / ark-credential / runs), /api/agents, /api/skills, /api/runs. The standard
	// library mux panics at startup on conflicting paths, so a conflict cannot happen silently.
	if cfg.CoreHandler != nil {
		cfg.CoreHandler.Register(authorized)
	}

	// Reverse check for missing declarations: the panic above catches "registered but not
	// declared"; this catches the other direction, "declared but not registered", i.e. a typo in a
	// pattern in matrix.go, so that declaration never matches any request. Checking only one
	// direction cannot find this.
	//
	// **Skipped when CoreHandler is nil**: then the dozens of core routes are not registered, and
	// the check would fail for sure. That setup only appears in tests (the Feishu path running
	// alone).
	if cfg.CoreHandler != nil {
		if err := cfg.Authz.Verify(); err != nil {
			return nil, err
		}
	}

	srv := &Server{cfg: cfg, log: log}

	srv.http = &http.Server{
		Addr: cfg.Addr,
		// gzip **only wraps the frontend static asset branch**; the backend mux does not go through
		// it. So SSE / WebSocket (all under /api/) avoid compression through the routing structure,
		// not through "the filter should let them through"; the latter was measured not to hold,
		// see the file header of compress.go.
		Handler:           srv.wrap(routeFrontend(mux, compressStatic(spaHandler(staticRoot())))),
		ReadHeaderTimeout: readHeaderTimeout,
		WriteTimeout:      writeTimeout,
	}
	return srv, nil
}

// Handler returns the handler with middleware applied; tests can use it directly with httptest.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Run starts the service and blocks until ctx is canceled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("HTTP 服务启动", slog.String("addr", s.cfg.Addr))
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("HTTP 服务异常退出: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		// Give in-flight requests some time to finish instead of cutting them off.
		s.log.Info("收到退出信号，开始优雅关闭")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("优雅关闭失败: %w", err)
		}
		return nil
	}
}

// protectedMux is a decorator: every handler registered on it is wrapped with authentication
// first and then given to the real mux.
//
// **It exists so that authentication happens after routing.** It implements the interface that
// only has HandleFunc (orchestrator.Mux; core.Mux is an alias of it; feishu.Mux is still its own
// declaration with the same shape), so the route registration code of the business modules needs
// no change, and every route is protected.
//
// Switching to "wrap the whole mux in an authentication middleware" would break 405: then
// authentication happens before routing, and a GET to an endpoint that only accepts POST would be
// stopped with 401. See the comment in New.
type protectedMux struct {
	mux     *http.ServeMux
	protect func(http.HandlerFunc) http.HandlerFunc
}

func (p *protectedMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	p.mux.HandleFunc(pattern, p.protect(handler))
}

// wrap chains the cross-cutting middleware. From outside to inside: panic recovery → logid →
// access log → CORS.
//
// panic recovery is outermost, so it catches surprises in all the later middleware.
//
// **logid must be outside logRequest** (further out): `logRequest` writes the "request finished"
// line with `InfoContext`, and needs to read the logid from ctx. The other way round (logid inside)
// that line has no logid, and it is exactly the first line people look at when debugging; this
// mistake gives no error, it just silently breaks the linking.
//
// **Authentication is not in this chain**: it is wrapped per route inside the mux (see
// protectedMux), because it must happen after routing so as not to turn 405 into 401.
func (s *Server) wrap(next http.Handler) http.Handler {
	return s.recoverPanic(withLogID(s.logRequest(s.cors(next))))
}

// logHealthz logs health check hits by sampling; n is which hit this is in this process.
//
// # Why this must be logged (it was once deliberately not logged)
//
// `logRequest` has an `if r.URL.Path == "/healthz" { return }`, with a fair reason: the probe hits
// every 10 seconds, and logging all of them would flood the logs. **But the blind spot it left
// kept a production incident open through two rounds of debugging**: pods were killed every 260
// seconds (lived 229 seconds = initial_delay + 3×10s), and the logs showed neither `/healthz` nor
// any "request finished", so
//
//	"the probe never arrived"  and  "the probe arrived and succeeded, something else judged it dead"
//
// **look exactly the same in the logs, while the fixes are opposite**. Without this line the only
// option is guessing by changing the port and redeploying again and again; two guesses (two
// different port settings) both failed, wasting two release rounds.
//
// # Sampling
//
// The first 3 hits are always logged, then one in every 60 (probe every 10 seconds → about one line
// every 10 minutes).
//
// The first 3 are the key: **as soon as the 1st line appears after the probe window starts, "the
// probe did not arrive" is ruled out**, and in a crash loop the process only lives 229 seconds, too
// short to wait for a low-frequency line. In steady state one line every 10 minutes is negligible,
// and the original intent "do not flood the logs" is kept.
//
// What is logged is **the probe's source address and Host**: the address family (IPv4/IPv6) and
// the port were both suspects in that investigation, and `RemoteAddr` is the only thing on the
// server side that can confirm "where the probe actually came from and which address it hit".
func logHealthz(log *slog.Logger, r *http.Request, n uint64) {
	const (
		verboseHits = 3
		everyN      = 60
	)
	if n > verboseHits && n%everyN != 0 {
		return
	}
	log.InfoContext(r.Context(), "健康检查命中",
		slog.Uint64("hits", n),
		slog.String("remote", r.RemoteAddr),
		slog.String("host", r.Host),
		slog.String("proto", r.Proto))
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.ErrorContext(r.Context(), "请求处理 panic",
					slog.String("path", r.URL.Path), slog.Any("panic", v))
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"服务内部错误","code":"internal"}`))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder records the response status code for the access log.
//
// **It must pass Flush through.** After wrapping the ResponseWriter, the original http.Flusher is
// hidden inside; core's SSE handler doing w.(http.Flusher) would fail the assertion, so every frame
// piles up in the buffer, and the chat page shows "spinning forever, then the whole reply pops out
// at once". Problems like this give no error; they just look like a slow backend.
type statusRecorder struct {
	http.ResponseWriter
	status int

	// errBody collects a small piece of the response body only on 5xx; **its only use is to pull
	// out the `code` field**.
	//
	// ⚠️ The collected bytes **never go into the log**; only the error code enum parseErrorCode picks
	// out of them does (`internal` / `sso_unavailable` ...). This is an allow-list read, same as
	// feedback's sanitize: keys not on the list are never read, so the Chinese sentence in message,
	// and any field anyone adds to error responses in the future, cannot get out.
	//
	// Why it is worth collecting: the error code is worth more for finding the cause than the status
	// code (the same 500 may be `internal` or something else), and it is a low-cardinality enum,
	// useful for aggregation and alerts.
	errBody []byte
}

// errBodySniffLimit is the maximum number of bytes collected on 5xx to read the error code.
//
// Error responses are small objects like `{"message":"…","code":"…"}`; 512 bytes is plenty.
// The limit exists because a 5xx body is **not always** our small JSON: besides panic recovery, it
// may come from other middleware or the standard library, and collecting without a limit would
// copy a whole large response into memory.
const errBodySniffLimit = 512

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Write passes the write through, and on 5xx also keeps the first small piece (only to read the
// error code; see errBody).
//
// **Outside 5xx it is a pure pass-through** that copies no bytes: normal traffic and SSE all go
// this way, and must not pay for a field that is only used during failures.
func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status >= http.StatusInternalServerError && len(r.errBody) < errBodySniffLimit {
		n := errBodySniffLimit - len(r.errBody)
		if n > len(b) {
			n = len(b)
		}
		r.errBody = append(r.errBody, b[:n]...)
	}
	return r.ResponseWriter.Write(b)
}

// Flush passes the flush through to the underlying ResponseWriter so SSE can send frame by frame.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack passes connection takeover through so WebSocket can complete the protocol upgrade.
//
// **The same kind of problem as Flush, with a more hidden symptom.** After wrapping the
// ResponseWriter the underlying http.Hijacker is hidden, and `gorilla/websocket`'s Upgrade gets
// "Hijack not supported"; that error points at the websocket library, not at "the logging
// middleware added a wrapper".
//
// It is used by the webshell terminal proxy (core's sandbox shell WS). Before this method was
// added, that route could not upgrade no matter how it was written.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("httpd: 底层 ResponseWriter 不支持 Hijack")
	}
	// Record the status as 101: once the connection is taken over we can no longer see what it
	// writes, and leaving 0 would produce an odd "status=0" record in the access log.
	r.status = http.StatusSwitchingProtocols
	return h.Hijack()
}

// withLogID gives each request a request id (see shared/gopkg/requestid) and puts it into ctx.
//
// # It makes "the user reported an id" searchable
//
// It is the common basis of three things:
//
//  1. **amad's own logs carry it**: `logRequest` and every `*Context`-style call in business code
//     (`InfoContext` / `ErrorContext`) go through xlog.NewHandler, which takes the request id from
//     ctx and fills the `request_id` field; searching for it gives **all** log lines of this one
//     request;
//  2. **it is returned to the caller in a response header**: users/the frontend can give this id
//     directly when reporting a problem instead of describing "around what time";
//  3. **it is passed downstream**: the outgoing transport can put it into a request header of the
//     same name.
//
// Without any one of the three, cross-service debugging falls back to "guess by timestamp", and
// timestamps collide under concurrency.
//
// # Reuse the one from upstream first
//
// The gateway/caller may have generated one already. **Reuse it** instead of making another: the
// same call having one id on each side is as good as having no id, which is exactly the problem
// it is meant to solve.
func withLogID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(requestid.Header))
		if id == "" {
			id = requestid.New()
		}
		ctx := requestid.With(r.Context(), id)
		// Return it to the caller, who reports this id directly when there is a problem.
		w.Header().Set(requestid.Header, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		// Health checks are hit very often by probes; do not log them, or the log is all noise.
		//
		// ⚠️ **This return only holds for 2xx**: /healthz returning 5xx is a real incident and must
		// not be silenced just because "it is the probe path". logServerError below runs before
		// this return, so probe failures still leave an Error.
		logServerError(s.log, r, rec, time.Since(start))
		if r.URL.Path == "/healthz" {
			return
		}
		s.log.InfoContext(r.Context(), "请求完成",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			// host / remote were added to find "which port the probe actually hits"; they are not
			// decoration.
			//
			// In that 260-second crash loop, `/healthz` was never hit (zero logHealthz lines), but
			// **this log line did appear once**: `path=/debug/pprof/dynconf status=200`, proving
			// the process was reachable and an outside caller could connect to the port it listened
			// on. So the problem narrowed down to "the port/path the probe hits differs from what
			// the process serves".
			//
			// ⚠️ **But it could not be settled then, because Host was not logged here**: there was
			// no way to know which port the incoming request used, so no way to answer "is that the
			// port the probe should use". On that platform each pod got a different port, while the
			// service declaration said 8080; that mismatch was the main suspect, and `Host` is
			// exactly the field that tells at a glance.
			slog.String("host", r.Host),
			slog.String("remote", r.RemoteAddr),
			slog.Duration("cost", time.Since(start)))
	})
}

// logServerError guarantees that **every 5xx leaves one Error line in the log**.
//
// # Why this layer must catch it (logs in the business layer do not count)
//
// A page in production really returned 500, while in the same period amad's logs had **0**
// lines with `_level=Error`. So debugging fell back to "guess which handler did it": a 500 that
// leaves no Error throws away exactly what most needs checking.
//
// The business layer does log (the default branch of `core.Handler.fail`, the two places in
// `auth`), but that relies on **each call site remembering**, which does not cover everything and
// decays as new code is added:
//
//   - places that call `h.failWith(w, 500, …)` directly do not go through `fail` (the SSE spot in
//     core/handler.go is one), and log nothing at all;
//   - 5xx written by any middleware, the standard library or third-party handlers will certainly
//     not go through our `fail`;
//   - any future handler that forgets to log is another zero-log incident, and **forgetting gives
//     no error**.
//
// This layer is on the path of every response, so it is the only place that can give the
// **invariant** "5xx ⇒ there is always an Error". The business layer logs stay: they carry business
// context (which workspace, which run), more useful than the generic fields here; this layer makes
// sure "not a single one is missed".
//
// # Only ids and enums, no content
//
// Five things are logged: method / path (**the route path, without the query**) / status / code /
// cost. Why each one is not sensitive:
//
//   - `path` uses `r.URL.Path`, so **the query is excluded by structure**: not "remember to strip
//     it", that field simply has no query. The frontend once really put the identity into
//     `?actor=` (see data/http.ts in web), and any "log the full URL" approach would carry it away.
//   - `code` is our own error code enum (`internal` / `sso_unavailable` ...), low cardinality with
//     no user data, worth more for finding the cause than the status code.
//   - **The response body itself is not logged**; only the `code` key is picked out of it (see
//     parseErrorCode). The body has sentences for users (in Chinese), sometimes with entity names,
//     and it is "what the endpoint returned", exactly one of the things README-feedback.md says is
//     never collected at either level.
//   - `logid` need not be added explicitly: `ErrorContext` goes through xlog.NewHandler, which
//     fills `request_id` automatically, the same id as the "request finished" line, so the two
//     match up.
//
// Deliberately **not logged**: UA, body, headers, actor email. The first ones are either
// near-fingerprints or user data, and the actor can be found in other log lines through the logid.
//
// # Frequency
//
// 5xx is an abnormal path whose normal volume is zero, so no rate limit is needed: if it really
// floods, the flood itself is the incident to report.
// 4xx is **deliberately not logged as Error**: 403/404 are daily traffic (frontend probing,
// scanners, permission boundaries); logging them would turn this line into noise, and they already
// carry the status code in the "request finished" Info line.
func logServerError(log *slog.Logger, r *http.Request, rec *statusRecorder, cost time.Duration) {
	if rec.status < http.StatusInternalServerError {
		return
	}
	attrs := []any{
		slog.String("method", r.Method),
		// ⚠️ r.URL.Path, **not r.URL.String()**: the latter includes the query.
		slog.String("path", r.URL.Path),
		slog.Int("status", rec.status),
		slog.Duration("cost", cost),
	}
	if code := parseErrorCode(rec.errBody); code != "" {
		attrs = append(attrs, slog.String("code", code))
	}
	log.ErrorContext(r.Context(), "请求失败（5xx）", attrs...)
}

// parseErrorCode picks the `code` key out of an error response body and nothing else.
//
// **Allow-list read**: it decodes into a struct with only a `Code` field, so the Chinese sentence
// in `message`, and any field anyone adds to error responses in the future, is never read; same rule
// as feedback's sanitize (keys not on the list should never be read). A deny-list "delete a few
// keys" approach always misses something.
//
// If it cannot decode (not JSON, truncated, not a response we wrote at all) it returns an empty
// string, and the caller skips the field. **No fake value like "unknown"**: that is a valid string
// and would merge "could not decode" with "the error code really is called unknown" (same reason
// core/observe.go forbids fake logids).
func parseErrorCode(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return payload.Code
}

// cors sends cross-origin headers per config and only opens the explicitly configured origin.
func (s *Server) cors(next http.Handler) http.Handler {
	origin := strings.TrimSpace(s.cfg.AllowedOrigin)
	if origin == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		// PATCH / PUT are used by the six core domains (edit workspace, edit agent, save Ark
		// credentials). Without them the browser's preflight fails right away, and curl cannot
		// reproduce it: only browsers send preflights.
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
		// Authorization is for callers with Bearer (scripts / CI); browsers use the cookie and do
		// not need it.
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		// ⚠️ **logid must be exposed explicitly, otherwise the frontend cannot read it cross-origin.**
		//
		// By default browsers only let JS read 7 CORS-safelisted response headers (Cache-Control /
		// Content-Language / Content-Length / Content-Type / Expires / Last-Modified / Pragma); all
		// others **silently return null**, with no error and no warning.
		//
		// Right now "nothing goes wrong without it", because the default is same-origin (the
		// frontend build is go:embed-ed into the binary, API_BASE is `/api`), and same-origin JS can
		// read all response headers. But once someone points the frontend at another host with
		// `VITE_API_BASE` (exactly the case this cors middleware exists for), the logid in problem
		// reports **silently disappears in that deployment**; the symptom is "the logid is
		// sometimes there and sometimes not", and nothing points to CORS.
		w.Header().Set("Access-Control-Expose-Headers", requestid.Header)
		// **Credentials must be allowed**, otherwise the browser does not send the cookie
		// cross-origin and the login state does not get through. This is also why AllowedOrigin
		// cannot be "*": the browser rejects the two together.
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		// The origin varies by request header; without this, a middle layer might cache site A's
		// response for site B.
		w.Header().Add("Vary", "Origin")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
