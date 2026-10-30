package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestHandler(t *testing.T, fake *fakeOIDC, key APIKey) (*Handler, *Service) {
	t.Helper()
	svc := newTestService(t, fake, key)
	h, err := NewHandler(HandlerConfig{Service: svc})
	if err != nil {
		t.Fatalf("NewHandler 失败: %v", err)
	}
	return h, svc
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解响应失败（body=%s）: %v", rec.Body.String(), err)
	}
	return out
}

func TestNewHandlerRejectsNilService(t *testing.T) {
	if _, err := NewHandler(HandlerConfig{}); err == nil {
		t.Error("Service 为 nil 应该被拒")
	}
}

func TestLoginRedirectsToAuthorizePage(t *testing.T) {
	fake := &fakeOIDC{}
	svc := newTestService(t, fake, APIKey{})
	// Set publicURL to simulate the production setup.
	h, err := NewHandler(HandlerConfig{Service: svc, PublicURL: "https://ama.example.com"})
	if err != nil {
		t.Fatalf("NewHandler 失败: %v", err)
	}
	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, PathOIDCLogin+"?next=/w/ws_1/chat", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("状态码 = %d, 期望 303；body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "https://idp.example.com/authorize?state="+fake.lastState {
		t.Errorf("Location = %q", loc)
	}
	// redirect_uri must be built from publicURL, **not from the request's Host** (which can be forged).
	if fake.lastRedirectURI != "https://ama.example.com"+PathOIDCCallback {
		t.Errorf("redirect_uri = %q", fake.lastRedirectURI)
	}
	// This redirect must not be cached: the code is different on every login.
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q，必须 no-store", cc)
	}
}

// ⚠️ **This one pins account takeover.**
//
// redirect_uri must never come from request headers: both `Host` and `X-Forwarded-Host` can be
// changed by the caller. An attacker gets an authorization link with `Host: evil.com`, tricks a
// victim into finishing it, and the one-time code lands on evil.com, where they exchange it for a
// token. So once publicURL is set, a forged Host must be ignored.
func TestLoginIgnoresSpoofedHostForRedirectURI(t *testing.T) {
	fake := &fakeOIDC{}
	svc := newTestService(t, fake, APIKey{})
	h, _ := NewHandler(HandlerConfig{Service: svc, PublicURL: "https://ama.example.com"})
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest(http.MethodGet, PathOIDCLogin, nil)
	req.Host = "evil.example.com"
	req.Header.Set("X-Forwarded-Host", "evil.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")
	mux.ServeHTTP(httptest.NewRecorder(), req)

	if strings.Contains(fake.lastRedirectURI, "evil") {
		t.Fatalf("redirect_uri 被请求头带跑了: %q —— 这是账号接管", fake.lastRedirectURI)
	}
	if fake.lastRedirectURI != "https://ama.example.com"+PathOIDCCallback {
		t.Errorf("redirect_uri = %q", fake.lastRedirectURI)
	}
}

// publicURL not set and the request is not from this machine → do not start the login; go back to
// the home page with unavailable (browser navigations do not get JSON).
func TestLoginWithoutPublicURLFromRemoteHost(t *testing.T) {
	svc := newTestService(t, &fakeOIDC{}, APIKey{})
	h, _ := NewHandler(HandlerConfig{Service: svc})
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest(http.MethodGet, PathOIDCLogin, nil)
	req.Host = "ama.example.com"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?login_error=unavailable" {
		t.Errorf("状态码 = %d, Location = %q，期望 303 到 /?login_error=unavailable", rec.Code, rec.Header().Get("Location"))
	}
}

// Requests from this machine may skip publicURL, so local development and worktrees (each on its
// own port) work out of the box. An attacker setting a loopback Host gets nothing (the code would
// be sent to their own machine).
func TestLoginFallsBackToLoopback(t *testing.T) {
	fake := &fakeOIDC{}
	svc := newTestService(t, fake, APIKey{})
	h, _ := NewHandler(HandlerConfig{Service: svc})
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest(http.MethodGet, PathOIDCLogin, nil)
	req.Host = "127.0.0.1:5347"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("状态码 = %d, 期望 303; body=%s", rec.Code, rec.Body.String())
	}
	if fake.lastRedirectURI != "http://127.0.0.1:5347"+PathOIDCCallback {
		t.Errorf("redirect_uri = %q", fake.lastRedirectURI)
	}
}

func TestCallbackSetsCookieAndRedirects(t *testing.T) {
	fake := &fakeOIDC{}
	svc := newTestService(t, fake, APIKey{})
	h, _ := NewHandler(HandlerConfig{Service: svc, PublicURL: "https://ama.example.com"})
	mux := http.NewServeMux()
	h.Register(mux)

	// Start once first to get a real state.
	mux.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, PathOIDCLogin+"?next=/preferences", nil))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		PathOIDCCallback+"?code=cb-code&state="+fake.lastState, nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("状态码 = %d, 期望 303; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/preferences" {
		t.Errorf("Location = %q，期望回到登录前那一页", loc)
	}
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			session = c
		}
	}
	if session == nil || session.Value == "" {
		t.Fatal("没有种下会话 Cookie")
	}
	if !session.HttpOnly {
		t.Error("会话 Cookie 必须 HttpOnly（XSS 偷不走）")
	}
	// ⚠️ Lax is **required**, not a preference: the callback is a cross-site top-level navigation,
	// Strict would not send the cookie, and switching to Strict would make login fail silently.
	if session.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v，必须是 Lax", session.SameSite)
	}
}

// Failures must also 302 back to the home page with a reason code, **not output JSON**: the user
// arrives with the whole tab, and a screen of `{"error":...}` is neither understandable nor gives a
// way out.
func TestCallbackFailuresRedirectWithReason(t *testing.T) {
	for _, tc := range []struct{ name, query, wantReason string }{
		{"用户点了拒绝", "?error=access_denied&state=x", "denied"},
		{"IdP 别的错误", "?error=server_error&state=x", "unavailable"},
		{"state 不认识", "?code=c&state=bogus", "expired"},
		{"没带 state", "?code=c", "expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := NewHandler(HandlerConfig{Service: newTestService(t, &fakeOIDC{}, APIKey{}), PublicURL: "https://ama.example.com"})
			mux := http.NewServeMux()
			h.Register(mux)

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, PathOIDCCallback+tc.query, nil))

			if rec.Code != http.StatusSeeOther {
				t.Fatalf("状态码 = %d, 期望 303; body=%s", rec.Code, rec.Body.String())
			}
			loc := rec.Header().Get("Location")
			if !strings.HasPrefix(loc, "/?login_error=") {
				t.Fatalf("Location = %q，期望带 login_error 回首页", loc)
			}
			if !strings.Contains(loc, tc.wantReason) {
				t.Errorf("Location = %q，期望原因 %q", loc, tc.wantReason)
			}
			// No cookie should be set on failure.
			for _, c := range rec.Result().Cookies() {
				if c.Name == SessionCookieName && c.Value != "" {
					t.Error("失败还种了会话 Cookie")
				}
			}
		})
	}
}

// ⚠️ **Regression lock for open redirects.**
//
// Without the check our domain would become a redirector: a phishing link starts with
// ama.example.com (the user sees a trusted domain and clicks without worry), but lands on the
// attacker's site.
//
// `//evil.com` is the easiest to miss: it does start with `/`, but browsers treat it as a
// protocol-relative cross-origin address.
func TestSanitizeReturnTo(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/w/ws_1/chat", "/w/ws_1/chat"},
		{"/preferences?tab=x", "/preferences?tab=x"},
		{"", "/"},
		{"   ", "/"},
		{"https://evil.com", "/"},
		{"//evil.com", "/"}, // protocol-relative URL
		// ⚠️ The backslash family. **During self review this was once written as "expected to pass
		// through unchanged", which was wrong**: browsers normalize `\` to `/`, so `/\evil.com` is
		// equivalent to `//evil.com` and still goes off-site. The implementation at the time (starts
		// with / + does not start with // + does not contain ://) passed all three, and this case
		// pinned that hole as correct behavior: **the test was vouching for the bug**.
		{"/\\evil.com", "/"},
		{"/\\/evil.com", "/"},
		{"/\\evil.com/path", "/"},
		{"javascript:alert(1)", "/"},
		{"HTTPS://evil.com", "/"}, // scheme is case-insensitive
		// Encoded slashes are **part of the path** (browsers do not decode them into separators),
		// so they pass.
		{"/%2f%2fevil.com", "/%2f%2fevil.com"},
		{"http://127.0.0.1/x", "/"},
		{PathOIDCLogin, "/"}, // redirecting back to the login endpoint would loop
		{PathLogin, "/"},
		{PathOIDCCallback + "?x=1", "/"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := sanitizeReturnTo(tc.in); got != tc.want {
				t.Errorf("sanitizeReturnTo(%q) = %q, 期望 %q", tc.in, got, tc.want)
			}
		})
	}
}

// Local is http://; with Secure the browser does not store this cookie at all. The symptom is
// "login succeeded, but after a refresh I must log in again", with no error.
func TestCookieSecureFollowsDeployment(t *testing.T) {
	for _, tc := range []struct {
		name   string
		secure bool
	}{{"本地 http", false}, {"线上 https", true}} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeOIDC{}
			svc := newTestService(t, fake, APIKey{})
			h, err := NewHandler(HandlerConfig{
				Service: svc, SecureCookie: tc.secure, PublicURL: "https://ama.example.com",
			})
			if err != nil {
				t.Fatalf("NewHandler 失败: %v", err)
			}
			mux := http.NewServeMux()
			h.Register(mux)

			mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, PathOIDCLogin, nil))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
				PathOIDCCallback+"?code=c&state="+fake.lastState, nil))

			for _, c := range rec.Result().Cookies() {
				if c.Name == SessionCookieName && c.Secure != tc.secure {
					t.Errorf("Secure = %v, 期望 %v", c.Secure, tc.secure)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// /api/auth/me
// ---------------------------------------------------------------------------

// Not being logged in is an **expected result**, not an error: the frontend probes the login state
// with this route at startup.
func TestMeUnauthenticated(t *testing.T) {
	h, _ := newTestHandler(t, &fakeOIDC{}, APIKey{})
	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/me", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d, 期望 401", rec.Code)
	}
	// **code must be unauthenticated**: the frontend relies on it to go to the login page.
	// If it were not_found, the frontend's "404 → null" rule would turn it into a silent blank page.
	if code := decode(t, rec)["code"]; code != "unauthenticated" {
		t.Errorf("code = %v, 期望 unauthenticated", code)
	}
}

func TestMeWithCookie(t *testing.T) {
	fake := &fakeOIDC{}
	h, svc := newTestHandler(t, fake, APIKey{})
	mux := http.NewServeMux()
	h.Register(mux)

	res := loginOnce(t, svc, fake)

	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: res.SessionToken})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	user, _ := decode(t, rec)["user"].(map[string]any)
	if user["email"] != "zhangsan@example.com" {
		t.Errorf("email = %v", user["email"])
	}
}

func TestMeWithBearer(t *testing.T) {
	h, _ := newTestHandler(t, &fakeOIDC{}, APIKey{Key: "secret-key", Email: "bot@example.com"})
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer secret-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	user, _ := decode(t, rec)["user"].(map[string]any)
	if user["email"] != "bot@example.com" {
		t.Errorf("email = %v", user["email"])
	}
	if user["viaApiKey"] != true {
		t.Errorf("viaApiKey = %v, 应该标出机器身份", user["viaApiKey"])
	}
}

// ---------------------------------------------------------------------------
// Logout
// ---------------------------------------------------------------------------

func TestLogoutClearsCookie(t *testing.T) {
	fake := &fakeOIDC{}
	h, svc := newTestHandler(t, fake, APIKey{})
	mux := http.NewServeMux()
	h.Register(mux)

	res := loginOnce(t, svc, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: res.SessionToken})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}

	var cleared *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			cleared = c
		}
	}
	if cleared == nil {
		t.Fatal("必须回一个清除 Cookie 的指令")
	}
	// **MaxAge must be negative**. 0 means a "session cookie" (gone only when the browser closes),
	// so after logout the current tab would still carry it, showing up as "I clicked log out but I
	// am still logged in".
	if cleared.MaxAge >= 0 {
		t.Errorf("MaxAge = %d, 必须是负数才会立刻删掉", cleared.MaxAge)
	}

	if _, err := svc.Resolve(context.Background(), res.SessionToken); err == nil {
		t.Error("服务端的会话也该没了")
	}
}

func TestProvidersEndpoint(t *testing.T) {
	cases := []struct {
		name       string
		cfg        Config
		wantLocal  bool
		wantSignup bool
		wantOIDC   bool
		wantAPIKey bool
	}{
		{"什么都没开", Config{}, false, false, false, false},
		{"本地账号关注册、空库", Config{Local: LocalConfig{Enabled: true}}, true, true, false, false},
		{"OIDC + API Key", Config{OIDC: &fakeOIDC{}, OIDCDisplayName: "Okta", APIKey: APIKey{Key: "k", Email: "bot@example.com"}}, false, false, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.Store = NewMemStore()
			svc, _ := New(tc.cfg)
			h, _ := NewHandler(HandlerConfig{Service: svc})
			mux := http.NewServeMux()
			h.Register(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, PathProviders, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d", rec.Code)
			}
			var p Providers
			if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
			if p.Local.Enabled != tc.wantLocal || p.Local.SignupOpen != tc.wantSignup ||
				p.OIDC.Enabled != tc.wantOIDC || p.APIKeyEnabled != tc.wantAPIKey {
				t.Errorf("providers = %+v", p)
			}
			if tc.wantOIDC && p.OIDC.DisplayName != "Okta" {
				t.Errorf("displayName = %q", p.OIDC.DisplayName)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Protect middleware
// ---------------------------------------------------------------------------

func TestProtectRejectsAnonymous(t *testing.T) {
	h, _ := newTestHandler(t, &fakeOIDC{}, APIKey{})
	protected := h.Protect(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("不该走到业务 handler")
	}))

	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/whatever", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d, 期望 401", rec.Code)
	}
	if code := decode(t, rec)["code"]; code != "unauthenticated" {
		t.Errorf("code = %v", code)
	}
}

func TestProtectInjectsIdentity(t *testing.T) {
	fake := &fakeOIDC{}
	h, svc := newTestHandler(t, fake, APIKey{})
	res := loginOnce(t, svc, fake)

	var gotActor string
	var gotIdentity Identity
	protected := h.Protect(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotActor = ActorFrom(r.Context())
		gotIdentity, _ = IdentityFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/whatever", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: res.SessionToken})
	protected.ServeHTTP(httptest.NewRecorder(), req)

	if gotActor != "zhangsan@example.com" {
		t.Errorf("ActorFrom = %q", gotActor)
	}
	if gotIdentity.Name != "张三" {
		t.Errorf("Identity.Name = %q", gotIdentity.Name)
	}
}

// All three credential places must be accepted.
func TestProtectAcceptsAllCredentialLocations(t *testing.T) {
	const key = "secret-key"
	cases := []struct {
		name  string
		apply func(*http.Request)
	}{
		{"Authorization 头", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+key) }},
		{"Cookie", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: key}) }},
		{"query 参数", func(r *http.Request) { r.URL.RawQuery = "api_key=" + key }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newTestHandler(t, &fakeOIDC{}, APIKey{Key: key, Email: "bot@example.com"})
			var ok bool
			protected := h.Protect(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				ok = ActorFrom(r.Context()) == "bot@example.com"
			}))
			req := httptest.NewRequest(http.MethodGet, "/api/whatever", nil)
			tc.apply(req)
			rec := httptest.NewRecorder()
			protected.ServeHTTP(rec, req)
			if !ok {
				t.Errorf("没认出身份，状态码 = %d", rec.Code)
			}
		})
	}
}

// A forged X-AMA-Actor header must have no effect at all: this is exactly why we moved from
// "rewriting request headers" to the context.
func TestProtectIgnoresSpoofedActorHeader(t *testing.T) {
	h, _ := newTestHandler(t, &fakeOIDC{}, APIKey{})
	protected := h.Protect(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("伪造的身份头不该放行")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/whatever", nil)
	req.Header.Set("X-AMA-Actor", "victim@example.com")
	rec := httptest.NewRecorder()
	protected.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("状态码 = %d, 期望 401", rec.Code)
	}
}

func TestProtectFunc(t *testing.T) {
	h, _ := newTestHandler(t, &fakeOIDC{}, APIKey{Key: "k", Email: "bot@example.com"})
	called := false
	fn := h.ProtectFunc(func(http.ResponseWriter, *http.Request) { called = true })

	req := httptest.NewRequest(http.MethodGet, "/api/whatever", nil)
	req.Header.Set("Authorization", "Bearer k")
	fn(httptest.NewRecorder(), req)

	if !called {
		t.Error("带了正确凭据应该放行")
	}
}

// **The middleware must be wrapped per route after routing, not around the mux.**
// Wrapped around it, a request with the wrong method would be stopped with 401 first instead of
// getting 405; httpd's TestMethodRouting fails right away, and the error does not point to the
// real cause.
func TestProtectAfterRoutingPreserves405(t *testing.T) {
	h, _ := newTestHandler(t, &fakeOIDC{}, APIKey{})

	mux := http.NewServeMux()
	mux.Handle("POST /api/thing", h.Protect(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })))

	// GET on a route that only accepts POST: should be 405, not 401.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/thing", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("状态码 = %d, 期望 405——中间件包错位置会让它变成 401", rec.Code)
	}
}

// Protect must not swallow Flush; otherwise every SSE frame piles up in the buffer, and the chat
// page shows "spinning forever, then the whole reply pops out at once".
func TestProtectPassesThroughFlusher(t *testing.T) {
	h, _ := newTestHandler(t, &fakeOIDC{}, APIKey{Key: "k", Email: "bot@example.com"})

	var flushable bool
	protected := h.Protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, flushable = w.(http.Flusher)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/stream", nil)
	req.Header.Set("Authorization", "Bearer k")
	protected.ServeHTTP(httptest.NewRecorder(), req)

	if !flushable {
		t.Error("Protect 把 http.Flusher 藏起来了，SSE 会攒帧")
	}
}

func TestCredentialFromPrefersBearer(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x?api_key=from-query", nil)
	req.Header.Set("Authorization", "Bearer from-header")
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "from-cookie"})

	if got := credentialFrom(req); got != "from-header" {
		t.Errorf("credentialFrom = %q, 期望优先用 Authorization 头", got)
	}
}

func TestCredentialFromIgnoresNonBearer(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "from-cookie"})

	if got := credentialFrom(req); got != "from-cookie" {
		t.Errorf("credentialFrom = %q, 非 Bearer 的头该被忽略", got)
	}
}

func TestCredentialFromEmpty(t *testing.T) {
	if got := credentialFrom(httptest.NewRequest(http.MethodGet, "/x", nil)); got != "" {
		t.Errorf("credentialFrom = %q, 期望空", got)
	}
}

func TestContextHelpersWithoutIdentity(t *testing.T) {
	ctx := context.Background()
	if _, ok := IdentityFrom(ctx); ok {
		t.Error("空 context 不该有身份")
	}
	if ActorFrom(ctx) != "" {
		t.Error("空 context 的 Actor 应该是空串")
	}
	// Putting in an invalid identity does not count either.
	if _, ok := IdentityFrom(WithIdentity(ctx, Identity{})); ok {
		t.Error("无效身份不该被认成已鉴权")
	}
}
