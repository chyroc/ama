// HTTP endpoints and the authentication middleware of auth.
//
// # Nine endpoints that need no login (registered on the bare mux, not in authz/matrix.go)
//
//	GET    /api/auth/providers       which login methods are enabled
//	POST   /api/auth/login           email + password login, sets the cookie
//	POST   /api/auth/signup          register a local account and log in, sets the cookie
//	GET    /api/auth/oidc/login      303 to the IdP authorization page
//	GET    /api/auth/oidc/callback   receive the IdP callback, set the cookie, then 303 back into the site
//	POST   /api/auth/github/device   start a GitHub device flow, return the user_code, set the binding cookie
//	POST   /api/auth/github/poll     poll the GitHub authorization result, set the session cookie on success
//	GET    /api/auth/me              the current logged-in user (401 when not logged in, not an error)
//	POST   /api/auth/logout          log out, clear the cookie
//
// These **must be outside the authentication middleware**: requiring "logged in before you can
// log in" is a deadlock.
//
// One more needs login: `POST /api/admin/users` (a platform admin creates a local account for
// someone). It goes through the full Protect → Guard chain and is declared in the matrix; see
// RegisterAccounts.
//
// # The two OIDC routes are "browser navigations", not XHR
//
// The response is a 303, not JSON, because the user goes down this path **with the whole tab**. So
//
//   - Failures must also be 303 (back to the home page with `?login_error=`). Returning JSON would
//     leave the user staring at a screen of `{"error":...}`, which they cannot understand and have
//     no way out of.
//   - `SameSite=Lax` on the cookie is **required**, not a preference: the callback is a cross-site
//     top-level navigation; Lax sends the cookie, `Strict` does not. Switching to Strict would make
//     login fail silently.
//
// # The two password routes only accept JSON
//
// They require `Content-Type: application/json`: a cross-site page using `<form>` can only send
// three "simple" Content-Types; sending JSON needs a CORS preflight, which we reject. So other
// websites cannot submit a login on the user's behalf (login CSRF: logging the victim into the
// attacker's account).
//
// # The credential goes into a cookie
//
//  1. **It is the only option for SSE.** The chat stream uses the native EventSource, which
//     **cannot send custom request headers**.
//  2. **With HttpOnly, JS cannot read it**, so XSS cannot steal it.
//  3. Frontend and backend are same-origin (the frontend build is go:embed-ed into the binary), so
//     none of the cross-site cookie problems apply.
//
// **Authorization: Bearer is still supported as well**, for callers without a cookie jar (scripts,
// probes, CI).
//
// # Where the middleware sits: it must be after routing
//
// If the authentication middleware wrapped the whole mux, a request with the wrong method would be
// stopped with 401 first instead of getting 405, and httpd's TestMethodRouting fails right away.
// So the middleware is provided by Handler.Protect and **wrapped route by route by each module
// when it registers routes**. See the wiring in httpd/server.go.

package auth

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/gopkg/selfurl"
)

// SessionCookieName is the name of the session cookie.
const SessionCookieName = "ama_session"

// PathMe is "who am I". It is registered on the bare mux (not through Protect), so it is **not in
// authz/matrix.go**.
//
// It is exported for the tests of the openapi package: /llms.txt recommends this endpoint as the
// first step "check that your key is right", and every path mentioned in that document must be
// checkable against the routes really registered. With a hard-coded literal, if this endpoint is
// renamed one day, the document silently becomes a lie (agents following it get 404, and the error
// does not point to the rename).
const PathMe = "/api/auth/me"

// Login routes that need no login. Also registered on the bare mux, **not in authz/matrix.go**.
//
// PathOIDCCallback has a second role: it is the path part of the `redirect_uri` sent to the IdP,
// and it must be registered on the IdP exactly. **Changing this constant changes the
// redirect_uri**, and that failure happens after the user clicks consent at the IdP (the IdP
// reports redirect_uri mismatch); locally, every try looks like "started successfully".
const (
	PathProviders    = "/api/auth/providers"
	PathLogin        = "/api/auth/login"
	PathSignup       = "/api/auth/signup"
	PathOIDCLogin    = "/api/auth/oidc/login"
	PathOIDCCallback = "/api/auth/oidc/callback"
	PathLogout       = "/api/auth/logout"
	PathGitHubDevice = "/api/auth/github/device"
	PathGitHubPoll   = "/api/auth/github/poll"
)

// githubCookiePath is the Path of the binding cookie: only requests under /api/auth/github/ carry it.
const githubCookiePath = "/api/auth/github"

// PathAdminUsers is for a platform admin creating a local account for someone. **Needs login**;
// declared as ActionNone in the matrix (account-level, no workspace); whether the caller is an
// admin is decided by the handler itself (see Service.CreateLocalUser).
const PathAdminUsers = "/api/admin/users"

// Credentials are also accepted in these two places, for different callers.
const (
	// authorizationHeader is the standard Bearer header, for scripts / CI / probes.
	authorizationHeader = "Authorization"

	// apiKeyQueryParam is an escape hatch kept for **SSE**.
	//
	// EventSource cannot send custom headers. Same-origin, it sends the cookie automatically, so
	// browsers do not need this; but **non-browser SSE clients** (probes, integration tests) have no
	// cookie jar and can only use the query.
	//
	// ⚠️ Only an **API Key** is accepted, not a session token: query parameters end up in access logs
	// and browser history, and putting a user's session token there writes the login state into
	// logs. An API Key is a machine credential set by operators and rotatable at any time, a
	// different kind of risk.
	apiKeyQueryParam = "api_key"
)

// Handler is the HTTP adapter of auth.
type Handler struct {
	svc *Service
	log *slog.Logger

	// secureCookie decides whether the cookie has the Secure flag.
	//
	// Local development is http://; with Secure the browser **does not store the cookie at all**.
	// The symptom is "login succeeded, but after a refresh I must log in again", with no console
	// error. So it follows the deployment: cmd/amad decides by whether AMA_PUBLIC_URL is https.
	secureCookie bool

	// publicURL is the public address of this service, used to build the OIDC redirect_uri.
	//
	// **Do not derive it from request headers instead**: Host can be changed by the caller, and
	// using it as the redirect_uri would let an attacker send the one-time code to their own domain
	// (account takeover). When empty, only requests from this machine can log in; see
	// selfurl.Callback.
	publicURL string
}

// HandlerConfig is the configuration needed to build a Handler.
type HandlerConfig struct {
	// Service is required.
	Service *Service

	// SecureCookie must be on in production (https) and off locally (http). See Handler.secureCookie.
	SecureCookie bool

	// PublicURL is the public address of this service (like https://ama.example.com).
	//
	// When empty, **only requests from this machine can log in** (selfurl.Callback only falls back
	// for loopback addresses); empty in production means nobody can log in. The wiring code must
	// say this clearly in the startup log.
	PublicURL string

	// Logger defaults to the slog default logger.
	Logger *slog.Logger
}

// NewHandler builds a Handler from the config.
func NewHandler(cfg HandlerConfig) (*Handler, error) {
	if cfg.Service == nil {
		return nil, errors.New("auth: Service 不能为 nil")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Handler{
		svc:          cfg.Service,
		log:          log,
		secureCookie: cfg.SecureCookie,
		publicURL:    strings.TrimRight(strings.TrimSpace(cfg.PublicURL), "/"),
	}, nil
}

// Register registers the no-login routes of auth on mux.
//
// **None of them go through Protect**: requiring "logged in before you can log in" is a deadlock.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+PathProviders, h.providers)
	mux.HandleFunc("POST "+PathLogin, h.passwordLogin)
	mux.HandleFunc("POST "+PathSignup, h.signup)
	mux.HandleFunc("GET "+PathOIDCLogin, h.oidcLogin)
	mux.HandleFunc("GET "+PathOIDCCallback, h.oidcCallback)
	mux.HandleFunc("POST "+PathGitHubDevice, h.githubDevice)
	mux.HandleFunc("POST "+PathGitHubPoll, h.githubPoll)
	mux.HandleFunc("GET "+PathMe, h.me)
	mux.HandleFunc("POST "+PathLogout, h.logout)
}

// RegisterAccounts mounts the account management routes that "need login" (mux is the one httpd
// wrapped with Protect + Guard). Kept separate from Register for the same reason as
// RegisterAPIKeys.
func (h *Handler) RegisterAccounts(mux Mux) {
	mux.HandleFunc("POST "+PathAdminUsers, h.createUser)
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// Protect wraps a handler and requires the caller to be logged in.
//
// On success it puts the Identity into the context; downstream code reads it with
// auth.ActorFrom(ctx). On failure it always returns 401 + code=unauthenticated, and the frontend
// sends the user to the login page.
//
// **Wrap per route, do not wrap around the mux**; see "Where the middleware sits" in the file
// header.
func (h *Handler) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, err := h.svc.Resolve(r.Context(), credentialFrom(r))
		if err != nil {
			if errors.Is(err, ErrUnauthenticated) {
				h.failWith(w, http.StatusUnauthorized, "请先登录", "unauthenticated")
				return
			}
			// A database hiccup is a 500, not a 401: calling it "not logged in" would make users
			// log in again and again, while the real cause is that the database is unreachable.
			h.log.ErrorContext(r.Context(), "校验身份失败",
				slog.String("path", r.URL.Path), slog.String("error", err.Error()))
			h.failWith(w, http.StatusInternalServerError, "服务内部错误", "internal")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), identity)))
	})
}

// ProtectFunc is the HandlerFunc version of Protect, handier when registering a single route.
func (h *Handler) ProtectFunc(next http.HandlerFunc) http.HandlerFunc {
	return h.Protect(next).ServeHTTP
}

// credentialFrom gets the credential from the request, trying three places in priority order.
func credentialFrom(r *http.Request) string {
	// 1. Authorization: Bearer <token>
	if raw := strings.TrimSpace(r.Header.Get(authorizationHeader)); raw != "" {
		if token, ok := strings.CutPrefix(raw, "Bearer "); ok {
			if t := strings.TrimSpace(token); t != "" {
				return t
			}
		}
	}
	// 2. Cookie (browsers use this, including SSE)
	if c, err := r.Cookie(SessionCookieName); err == nil {
		if v := strings.TrimSpace(c.Value); v != "" {
			return v
		}
	}
	// 3. ?api_key= (non-browser SSE clients). Only an API Key belongs here; see the constant's comment.
	return strings.TrimSpace(r.URL.Query().Get(apiKeyQueryParam))
}

// ---------------------------------------------------------------------------
// Endpoints
// ---------------------------------------------------------------------------

// providers tells the frontend how this deployment can log in.
func (h *Handler) providers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, h.svc.Providers(r.Context()))
}

// maxAuthBody is the size limit of login/sign-up request bodies. A few hundred bytes are enough;
// 16KB stops people from sending huge bodies.
const maxAuthBody = 16 << 10

type passwordRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
	// Admin is only used by /api/admin/users.
	Admin bool `json:"admin"`
}

// decodeJSONBody requires a JSON Content-Type and decodes the body; see "only accept JSON" in the
// file header for why.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if mt, _, _ := strings.Cut(ct, ";"); strings.TrimSpace(mt) != "application/json" {
		return errors.New("Content-Type 必须是 application/json")
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAuthBody))
	return dec.Decode(dst)
}

// passwordLogin logs in with email + password. On success it returns {user} and sets the cookie.
func (h *Handler) passwordLogin(w http.ResponseWriter, r *http.Request) {
	var req passwordRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		h.failWith(w, http.StatusBadRequest, "请求格式不正确："+err.Error(), "invalid_argument")
		return
	}
	res, err := h.svc.LoginWithPassword(r.Context(), req.Email, req.Password)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.setSessionCookie(w, res.SessionToken)
	writeJSON(w, http.StatusOK, map[string]any{"user": res.Identity})
}

// signup registers a local account and logs in right away.
func (h *Handler) signup(w http.ResponseWriter, r *http.Request) {
	var req passwordRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		h.failWith(w, http.StatusBadRequest, "请求格式不正确："+err.Error(), "invalid_argument")
		return
	}
	res, err := h.svc.Signup(r.Context(), req.Email, req.Password, req.Name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.setSessionCookie(w, res.SessionToken)
	writeJSON(w, http.StatusOK, map[string]any{"user": res.Identity})
}

// createUser lets a platform admin create a local account for someone. **It does not change the
// caller's session**.
func (h *Handler) createUser(w http.ResponseWriter, r *http.Request) {
	var req passwordRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		h.failWith(w, http.StatusBadRequest, "请求格式不正确："+err.Error(), "invalid_argument")
		return
	}
	identity, err := h.svc.CreateLocalUser(r.Context(), ActorFrom(r.Context()), req.Email, req.Password, req.Name, req.Admin)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": identity})
}

// oidcRedirectURI builds the OIDC callback URL. **Do not build it from request headers** (Host can
// be changed by the caller; using it as the redirect_uri would send the one-time code to the
// attacker's domain). selfurl.Callback prefers the configured publicURL and only falls back to
// deriving from the request for requests from this machine. Start and callback must compute the
// same value.
func (h *Handler) oidcRedirectURI(r *http.Request) (string, error) {
	return selfurl.Callback(h.publicURL, r, PathOIDCCallback)
}

// oidcLogin sends the browser to the IdP authorization page.
//
// 302/303 instead of returning JSON and letting the frontend redirect itself: `next` (where to go
// after login) is an entry for open redirects, and keeping it on the server means there is only
// one gate (sanitizeReturnTo).
func (h *Handler) oidcLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	redirectURI, err := h.oidcRedirectURI(r)
	if err != nil {
		h.log.ErrorContext(r.Context(), "推不出 OIDC 回调地址，无法发起登录",
			slog.String("host", r.Host), slog.String("error", err.Error()))
		// This is a server config problem (server.publicURL not set); retrying a hundred times
		// will not help the user.
		h.redirectWithLoginError(w, r, loginErrorUnavailable)
		return
	}
	authorizeURL, err := h.svc.StartOIDCLogin(r.Context(), redirectURI, sanitizeReturnTo(r.URL.Query().Get("next")))
	if err != nil {
		if !errors.Is(err, ErrOIDCDisabled) {
			h.log.ErrorContext(r.Context(), "发起 OIDC 登录失败", slog.String("error", err.Error()))
		}
		h.redirectWithLoginError(w, r, loginErrorUnavailable)
		return
	}
	http.Redirect(w, r, authorizeURL, http.StatusSeeOther)
}

// oidcCallback receives the IdP callback: checks state, exchanges the token, sets the cookie, and
// sends the user back into the site.
//
// The callback looks like `/api/auth/oidc/callback?code=...&state=...`; when the user declines, the
// IdP sends `?error=access_denied&state=...` (RFC 6749 4.1.2.1).
//
// ⚠️ **Nothing in the query is a trusted identity field**; identity only comes from the ID token
// the server got by exchanging the code and whose signature it verified.
func (h *Handler) oidcCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	w.Header().Set("Cache-Control", "no-store")

	if e := strings.TrimSpace(q.Get("error")); e != "" {
		// The user clicked decline, or a config problem on the IdP side (unauthorized_client etc.).
		// The former is not a failure.
		h.log.InfoContext(r.Context(), "IdP 回调带了错误", slog.String("error", e),
			slog.String("description", q.Get("error_description")))
		if e == "access_denied" {
			h.redirectWithLoginError(w, r, loginErrorDenied)
		} else {
			h.redirectWithLoginError(w, r, loginErrorUnavailable)
		}
		return
	}

	redirectURI, err := h.oidcRedirectURI(r)
	if err != nil {
		h.log.ErrorContext(r.Context(), "推不出 OIDC 回调地址", slog.String("error", err.Error()))
		h.redirectWithLoginError(w, r, loginErrorUnavailable)
		return
	}

	res, err := h.svc.CompleteOIDCLogin(r.Context(), redirectURI, q.Get("state"), q.Get("code"))
	if err != nil {
		switch {
		case errors.Is(err, ErrLoginExpired), errors.Is(err, ErrInvalidArgument):
			h.log.InfoContext(r.Context(), "OIDC 回调无效，引导重新登录", slog.String("error", err.Error()))
			h.redirectWithLoginError(w, r, loginErrorExpired)
		case errors.Is(err, ErrLoginNoIdentity):
			h.redirectWithLoginError(w, r, loginErrorNoIdentity)
		case errors.Is(err, ErrEmailUnverified):
			h.redirectWithLoginError(w, r, loginErrorEmailUnverified)
		default:
			h.log.ErrorContext(r.Context(), "完成 OIDC 登录失败", slog.String("error", err.Error()))
			h.redirectWithLoginError(w, r, loginErrorUnavailable)
		}
		return
	}

	h.setSessionCookie(w, res.SessionToken)
	// ReturnTo already went through sanitizeReturnTo before being stored; run it again here:
	// **defense in depth**.
	http.Redirect(w, r, sanitizeReturnTo(res.ReturnTo), http.StatusSeeOther)
}

// githubDevice starts a GitHub device flow.
//
// The response has **no device_code** (it is only in the database), only the user_code for people
// and the flow id needed for polling; the plain binding cookie value only appears in Set-Cookie.
// See "The flow id cannot be claimed by someone else" in github.go.
func (h *Handler) githubDevice(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	start, err := h.svc.StartGitHubLogin(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	maxAge := int(time.Until(start.ExpiresAt).Seconds())
	if maxAge <= 0 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     GitHubCookiePrefix + start.FlowID,
		Value:    start.BindingToken,
		Path:     githubCookiePath,
		HttpOnly: true,
		Secure:   h.secureCookie,
		// Strict: polling is a same-origin XHR and Strict does not affect it; requests started
		// cross-site never carry it.
		SameSite: http.SameSiteStrictMode,
		MaxAge:   maxAge,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"flowId":          start.FlowID,
		"userCode":        start.UserCode,
		"verificationUri": start.VerificationURI,
		"interval":        int(start.Interval / time.Second),
		"expiresAt":       apimodel.FormatISO(start.ExpiresAt),
	})
}

type githubPollRequest struct {
	FlowID string `json:"flowId"`
}

// githubPoll polls once. Still waiting: 200 {status:"pending", interval}; success: 200
// {status:"ok", user} and sets the session cookie; everything else goes through fail (410 / 403 /
// 502).
//
// Only accepts JSON (same as password login, to block login CSRF).
func (h *Handler) githubPoll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req githubPollRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		h.failWith(w, http.StatusBadRequest, "请求格式不正确："+err.Error(), "invalid_argument")
		return
	}
	flowID := strings.TrimSpace(req.FlowID)
	binding := ""
	if flowID != "" {
		if c, err := r.Cookie(GitHubCookiePrefix + flowID); err == nil {
			binding = c.Value
		}
	}
	res, err := h.svc.PollGitHubLogin(r.Context(), flowID, binding)
	if err != nil {
		// Final failures (expired / denied / not admitted) also clear the binding cookie;
		// "GitHub temporarily unavailable" does not, so the user can keep polling.
		if flowID != "" && !errors.Is(err, ErrLoginUnavailable) {
			h.clearGitHubCookie(w, flowID)
		}
		h.fail(w, r, err)
		return
	}
	if res.Pending {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":   "pending",
			"interval": int(res.Interval / time.Second),
		})
		return
	}
	h.clearGitHubCookie(w, flowID)
	h.setSessionCookie(w, res.Login.SessionToken)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "user": res.Login.Identity})
}

func (h *Handler) clearGitHubCookie(w http.ResponseWriter, flowID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     GitHubCookiePrefix + flowID,
		Value:    "",
		Path:     githubCookiePath,
		HttpOnly: true,
		Secure:   h.secureCookie,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// Reason codes of login failures. **Only an enum for the frontend to choose a message**; it
// contains no details.
const (
	loginErrorDenied          = "denied"
	loginErrorExpired         = "expired"
	loginErrorNoIdentity      = "no_identity"
	loginErrorEmailUnverified = "email_unverified"
	loginErrorUnavailable     = "unavailable"
)

// redirectWithLoginError redirects back to the home page with a reason code.
func (h *Handler) redirectWithLoginError(w http.ResponseWriter, r *http.Request, reason string) {
	http.Redirect(w, r, "/?login_error="+url.QueryEscape(reason), http.StatusSeeOther)
}

// sanitizeReturnTo reduces "where to go after login" to an **in-site relative path**, or "/"
// otherwise.
//
// # This is the only gate against open redirects
//
// Without the check, `/api/auth/oidc/login?next=https://evil.com` would turn our domain into a
// redirector: a phishing link starts with our domain (the user sees it and clicks without worry),
// but lands on the attacker's site, which can look exactly like the login page.
//
// # The rule is an allow list, not a deny list
//
// **This deliberately does not list "which shapes are bad"**: that road does not work, and it was
// hit once during self review. The original rule was "starts with / + does not start with // +
// does not contain ://", which looks thorough, but `/\evil.com` passes all three. **Browsers
// normalize backslashes to forward slashes**, so it is equivalent to `//evil.com` (a
// protocol-relative URL) and still goes off-site. Similar variants are `/\/evil.com`, and encoded
// ones like `%2f%2fevil.com`.
//
// So it became an allow list: **the second character must be a "path character"**, i.e. the x in
// `/xxx` can only be a normal path character other than letters, digits, `-`, `_`, `.`, `~`, `/`.
// A simpler and more reliable approach is to let url.Parse decide "does it have a scheme or a
// host"; if either is non-empty, it is not an in-site path.
//
// `/api/auth/` itself must also be blocked: redirecting back to the login endpoint forms a
// redirect loop.
func sanitizeReturnTo(next string) string {
	p := strings.TrimSpace(next)
	if p == "" || !strings.HasPrefix(p, "/") {
		return "/"
	}
	// **Block backslashes first**: url.Parse does not parse a Host out of `/\evil.com` (it is a
	// valid relative path), but browsers treat `\` as `/`. That difference is exactly where the
	// bypass comes from, so it can only be rejected right here before parsing; a backslash in a
	// path is not normal anyway.
	if strings.ContainsAny(p, `\`) {
		return "/"
	}
	u, err := url.Parse(p)
	// Parse failure, a scheme (`javascript:`, `https:`), or a host (`//evil.com`) all mean it is not
	// an in-site path. url.Parse instead of counting characters ourselves: encoded variants
	// (`%2f%2f`) go through the same check, with no need to list them one by one.
	if err != nil || u.Scheme != "" || u.Host != "" || !strings.HasPrefix(u.Path, "/") {
		return "/"
	}
	if strings.HasPrefix(u.Path, "/api/auth/") {
		return "/"
	}
	return p
}

// me returns the current logged-in user.
//
// **It is not wrapped in Protect**; it decides by itself: the frontend uses this route at startup
// to probe the login state, and not being logged in is an **expected result**, not an error. It
// returns 401 + code=unauthenticated, and the frontend shows the login page.
func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	identity, err := h.svc.Resolve(r.Context(), credentialFrom(r))
	if err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			h.failWith(w, http.StatusUnauthorized, "请先登录", "unauthenticated")
			return
		}
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": identity})
}

// logout ends the current session and clears the cookie.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Logout(r.Context(), credentialFrom(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	h.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// Cookie
// ---------------------------------------------------------------------------

func (h *Handler) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:  SessionCookieName,
		Value: token,
		Path:  "/",
		// HttpOnly: JS cannot read it, so XSS cannot steal it.
		HttpOnly: true,
		// Secure follows the deployment. Under local http:// the browser does not store a cookie
		// with it at all; the symptom is "login succeeded, but after a refresh I must log in
		// again", with no error. See Handler.secureCookie.
		Secure: h.secureCookie,
		// Lax: sent on all same-site requests (including top-level navigation), not on cross-site
		// POSTs. Frontend and backend are same-origin, so Strict would also work, but Lax is
		// friendlier to "coming back by clicking a link in Feishu".
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(h.svc.SessionTTL().Seconds()),
	})
}

func (h *Handler) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   h.secureCookie,
		SameSite: http.SameSiteLaxMode,
		// Only MaxAge < 0 means "delete now". 0 means a "session cookie" (gone only when the
		// browser closes), so after logout the current tab would still carry it, showing up as
		// "I clicked log out but I am still logged in".
		MaxAge: -1,
	})
}

// ---------------------------------------------------------------------------
// Error mapping
// ---------------------------------------------------------------------------

// fail maps a business error to an HTTP response.
//
// message is a sentence (in Chinese) for the user; code is a stable identifier for programs (the
// frontend picks the message by code).
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrWeakPassword):
		h.failWith(w, http.StatusBadRequest, ErrWeakPassword.Error()[len("auth: "):], "weak_password")
	case errors.Is(err, ErrInvalidArgument):
		h.failWith(w, http.StatusBadRequest, "请求参数不完整或格式不正确", "invalid_argument")
	case errors.Is(err, ErrUnauthenticated):
		h.failWith(w, http.StatusUnauthorized, "请先登录", "unauthenticated")
	case errors.Is(err, ErrInvalidCredentials):
		h.failWith(w, http.StatusUnauthorized, "邮箱或密码不正确", "invalid_credentials")
	case errors.Is(err, ErrTooManyAttempts):
		h.failWith(w, http.StatusTooManyRequests, "尝试次数过多，请 15 分钟后再试", "too_many_attempts")
	case errors.Is(err, ErrLocalDisabled):
		h.failWith(w, http.StatusNotFound, "这个部署没有启用邮箱密码登录", "local_disabled")
	case errors.Is(err, ErrOIDCDisabled):
		h.failWith(w, http.StatusNotFound, "这个部署没有启用 OIDC 登录", "oidc_disabled")
	case errors.Is(err, ErrSignupDisabled):
		h.failWith(w, http.StatusForbidden, "未开放注册，请联系平台管理员创建账号", "signup_disabled")
	case errors.Is(err, ErrForbidden):
		h.failWith(w, http.StatusForbidden, "只有平台管理员可以执行这个操作", "access_denied")
	case errors.Is(err, ErrEmailTaken):
		h.failWith(w, http.StatusConflict, "该邮箱已注册", "email_taken")
	case errors.Is(err, ErrGitHubDisabled):
		h.failWith(w, http.StatusNotFound, "这个部署没有启用 GitHub 登录", "github_disabled")
	case errors.Is(err, ErrGitHubFlowExpired):
		h.failWith(w, http.StatusGone, "这次 GitHub 登录已失效，请重新开始", "github_expired")
	case errors.Is(err, ErrGitHubDenied):
		h.failWith(w, http.StatusForbidden, "你在 GitHub 上取消了授权", "github_denied")
	case errors.Is(err, ErrGitHubNoVerifiedEmail):
		h.failWith(w, http.StatusForbidden, "这个 GitHub 账号没有已验证的主邮箱，请先在 GitHub 设置里验证邮箱", "github_no_verified_email")
	case errors.Is(err, ErrGitHubNotAllowed):
		h.failWith(w, http.StatusForbidden, "这个 GitHub 账号没有登录本平台的权限，请联系管理员", "github_not_allowed")
	case errors.Is(err, ErrLoginUnavailable):
		h.log.ErrorContext(r.Context(), "登录服务不可用",
			slog.String("path", r.URL.Path), slog.String("error", err.Error()))
		h.failWith(w, http.StatusBadGateway, "登录服务暂时不可用，请稍后重试", "login_unavailable")
	default:
		h.log.ErrorContext(r.Context(), "登录接口处理失败",
			slog.String("path", r.URL.Path), slog.String("error", err.Error()))
		h.failWith(w, http.StatusInternalServerError, "服务内部错误", "internal")
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
