// All tests in this file are offline: a real ServeMux with real routes, but Feishu and SSO are fake.
//
// What this layer checks is "are routing, authentication and error mapping wired correctly". The
// business rules are already tested in the feishu / auth packages; here we only care about the
// HTTP side: does the path match, is the status code right, are unauthenticated requests blocked.
//
// # Identity moved from "request header" to "cookie + context"
//
// This used to be a placeholder identity scheme (middleware rewriting the X-AMA-Actor header); it
// is now real authentication: the caller sends a cookie or Bearer, and the middleware checks it and
// puts the Identity into the context. So wherever this file needs to "send a request as some
// identity", it uses authedRequest to build a request with a credential.

package httpd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/authz"
	"github.com/chyroc/ama/server/feishu"
	"github.com/chyroc/ama/server/model/apimodel"
)

// testAPIKey is the fixed API Key used in tests.
//
// An API Key instead of going through an OAuth redirect: this layer checks the wiring of routing
// and authentication; the login flow itself is fully tested in the auth package, and running it
// again here would only be noise.
const (
	testAPIKey   = "test-api-key"
	testAPIEmail = "tester@example.com"
	otherAPIKey  = "other-api-key"
)

// newTestAuth builds an auth.Handler that only accepts testAPIKey.
func newTestAuth(t *testing.T) *auth.Handler {
	t.Helper()
	svc, err := auth.New(auth.Config{
		Store:  auth.NewMemStore(),
		Local:  auth.LocalConfig{Enabled: true},
		APIKey: auth.APIKey{Key: testAPIKey, Email: testAPIEmail},
	})
	if err != nil {
		t.Fatalf("建 auth.Service 失败: %v", err)
	}
	// PublicURL must be set: httptest.NewRequest's default Host is example.com (not loopback), and
	// the OIDC redirect_uri **deliberately does not trust request headers** (that is the entry for
	// account takeover).
	h, err := auth.NewHandler(auth.HandlerConfig{
		Service:   svc,
		PublicURL: "https://ama.example.com",
	})
	if err != nil {
		t.Fatalf("建 auth.Handler 失败: %v", err)
	}
	return h
}

// testMembership is fake membership for authz.Guard.
//
// The tests in this file only wire the Feishu path (CoreHandler is nil) and check HTTP-level things
// like routing, authentication and 405, not the role matrix, which is covered by server/authz's own
// cases. So everything is allowed here; it only makes sure Guard can be wired in without changing
// the existing status code behavior.
type testMembership struct{}

func (testMembership) RoleOf(
	_ context.Context, _, _ string,
) (apimodel.WorkspaceRole, bool, error) {
	return apimodel.RoleOwner, true, nil
}

func (testMembership) WorkspaceOf(_ context.Context, _ authz.ResourceKind, _ string) (string, error) {
	return "ws-1", nil
}

// newTestAuthz builds a Guard that allows everything.
func newTestAuthz(t *testing.T) *authz.Guard {
	t.Helper()
	g, err := authz.New(authz.Config{Membership: testMembership{}})
	if err != nil {
		t.Fatalf("建 authz.Guard 失败: %v", err)
	}
	return g
}

// allowAllForTest is an allow-all authorizer for the **inner** requireManage of feishu.Service.
//
// It is different from testMembership above: that one feeds authz.Guard (the HTTP boundary), this
// one feeds feishu.Config.Authorizer (the few requireManage calls inside Service). Both layers must
// allow, so the cases in this file reach the routes and status codes they really check.
type allowAllForTest struct{}

func (allowAllForTest) CanAccessWorkspace(context.Context, string, string) (bool, error) {
	return true, nil
}

func (allowAllForTest) CanManageWorkspace(context.Context, string, string) (bool, error) {
	return true, nil
}

// newTestServer builds a Server pointing at a fake Feishu.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newTestServerWithLogger(t, nil)
}

// newTestServerWithLogger is like newTestServer, but takes a logger so log content can be asserted.
// Pass nil for the default logger.
func newTestServerWithLogger(t *testing.T, log *slog.Logger) *Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/v1/app/registration", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("action") == "begin" {
			_, _ = w.Write([]byte(`{"device_code":"dc","verification_uri_complete":"https://open.feishu.cn/p?user_code=X","expires_in":3600,"interval":5}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	})
	fake := httptest.NewServer(mux)
	t.Cleanup(fake.Close)

	svc, err := feishu.New(feishu.Config{
		Store:       feishu.NewMemStore(),
		AccountsURL: fake.URL,
		OpenURL:     fake.URL,
		// Allow: this file checks routing/authentication/405, not roles. feishu's production
		// default is deny; without this, every Feishu endpoint stops at 403.
		Authorizer: allowAllForTest{},
	})
	if err != nil {
		t.Fatalf("建 Service 失败: %v", err)
	}
	srv, err := New(Config{
		Addr: ":0", FeishuService: svc,
		AuthHandler: newTestAuth(t), Authz: newTestAuthz(t),
		Logger: log,
	})
	if err != nil {
		t.Fatalf("建 Server 失败: %v", err)
	}
	return srv
}

// authedRequest builds a request with a credential.
func authedRequest(method, path string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	return req
}

func TestConfigValidate(t *testing.T) {
	svc, err := feishu.New(feishu.Config{Store: feishu.NewMemStore()})
	if err != nil {
		t.Fatalf("建 Service 失败: %v", err)
	}
	authHandler := newTestAuth(t)
	guard := newTestAuthz(t)
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"最小合法配置", Config{Addr: ":8080", FeishuService: svc, AuthHandler: authHandler, Authz: guard}, false},
		{"缺监听地址", Config{FeishuService: svc, AuthHandler: authHandler, Authz: guard}, true},
		{"缺业务模块", Config{Addr: ":8080", AuthHandler: authHandler, Authz: guard}, true},
		// Missing the authentication module means the whole service runs unprotected; it must be
		// stopped at wiring time, not discovered at run time.
		{"缺鉴权模块", Config{Addr: ":8080", FeishuService: svc, Authz: guard}, true},
		// Same for a missing authorization module: all endpoints respond as usual, only anyone can
		// operate on any workspace. That **fails silently**, so better not to start.
		{"缺授权模块", Config{Addr: ":8080", FeishuService: svc, AuthHandler: authHandler}, true},
		{"通配来源不允许", Config{Addr: ":8080", FeishuService: svc, AuthHandler: authHandler, Authz: guard, AllowedOrigin: "*"}, true},
		{"明确来源可以", Config{Addr: ":8080", FeishuService: svc, AuthHandler: authHandler, Authz: guard, AllowedOrigin: "http://localhost:5273"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate()
			if tc.wantErr && err == nil {
				t.Fatal("期望报错，实际通过")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("期望通过，实际报错: %v", err)
			}
		})
	}
}

// The health check must be public: probes cannot carry credentials.
func TestHealthz(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码期望 200，实际 %d", rec.Code)
	}
}

// Probe hits must leave log lines, and must be sampled.
//
// **These two go together; missing either one falls back into a trap we already paid for:**
//   - Log none → "the probe did not arrive" and "the probe arrived and succeeded" look the same in
//     the logs, while the fixes are opposite. That 260-second crash loop in production took two
//     rounds of debugging without closing because of this.
//   - Log every one → once every 10 seconds floods the logs, which is why it was skipped at first.
//
// So pin it: **the first 3 must all be there** (in a crash loop the process only lives 229 seconds,
// too short for a low-frequency line), and **after that it must be clearly sparse**.
func TestHealthzLogsSampled(t *testing.T) {
	var buf bytes.Buffer
	srv := newTestServerWithLogger(t, slog.New(slog.NewTextHandler(&buf, nil)))

	const hits = 70
	for range hits {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码期望 200，实际 %d", rec.Code)
		}
	}

	n := strings.Count(buf.String(), "健康检查命中")
	// First 3 + the 60th = 4 lines. Written as a range instead of equality: the sampling
	// parameters are tunable; what is pinned here is the property "present but sparse", not
	// those two exact numbers.
	if n == 0 {
		t.Fatal("探针命中一条日志都没打 —— 那会让「探针有没有到达」无法判断，见 logHealthz 注释")
	}
	if n >= hits {
		t.Fatalf("探针命中每次都打（%d/%d 条）—— 10 秒一次会淹掉日志", n, hits)
	}
	// The first few must be dense, otherwise a short-lived process gets none at all.
	if !strings.Contains(buf.String(), "hits=1") {
		t.Error("第 1 次命中没打日志 —— 崩溃循环里进程只活 229 秒，等不到低频的那一条")
	}
}

// **Unauthenticated requests always get 401**, and code must be unauthenticated: the frontend relies
// on it to go to the login page.
func TestUnauthenticatedIsRejected(t *testing.T) {
	srv := newTestServer(t)
	paths := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/workspaces/ws-1/feishu/registrations"},
		{http.MethodGet, "/api/feishu/apps/x"},
	}
	for _, p := range paths {
		t.Run(p.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(p.method, p.path, nil))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("期望 401，实际 %d，body=%s", rec.Code, rec.Body.String())
			}
			var out struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			if out.Code != "unauthenticated" {
				t.Errorf("code = %q，前端靠它跳登录页", out.Code)
			}
		})
	}
}

// A forged X-AMA-Actor header must have no effect: this is exactly why we moved from "rewriting
// request headers" to the context.
func TestSpoofedActorHeaderIsIgnored(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/ws-1/feishu/registrations", nil)
	req.Header.Set("X-AMA-Actor", "victim@example.com")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("伪造身份头期望 401，实际 %d", rec.Code)
	}
}

// Login endpoints must be public: requiring "logged in before you can log in" is a deadlock.
func TestAuthEndpointsArePublic(t *testing.T) {
	srv := newTestServer(t)
	cases := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		// Starting a login without OIDC configured is also a 303 (back to the home page with
		// ?login_error=): it is a browser navigation, not XHR.
		{"发起 OIDC 登录", http.MethodGet, auth.PathOIDCLogin, http.StatusSeeOther},
		// A callback without state is also a 303, **not 4xx**: the user arrives with the whole tab.
		{"OIDC 回调缺参数", http.MethodGet, auth.PathOIDCCallback, http.StatusSeeOther},
		{"登录方式探测", http.MethodGet, auth.PathProviders, http.StatusOK},
		// Password login without JSON returns 400, not a 401 from the middleware.
		{"密码登录", http.MethodPost, auth.PathLogin, http.StatusBadRequest},
		{"注册", http.MethodPost, auth.PathSignup, http.StatusBadRequest},
		// With GitHub off, starting is 404 github_disabled and polling without JSON is 400; neither
		// is a 401 from the middleware.
		{"发起 GitHub 登录", http.MethodPost, auth.PathGitHubDevice, http.StatusNotFound},
		{"轮询 GitHub 登录", http.MethodPost, auth.PathGitHubPoll, http.StatusBadRequest},
		// me returning 401 when not logged in is an **expected result** (the frontend uses it to
		// probe the login state), not being blocked by the middleware.
		{"未登录问 me", http.MethodGet, "/api/auth/me", http.StatusUnauthorized},
		{"登出", http.MethodPost, "/api/auth/logout", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != tc.want {
				t.Errorf("期望 %d，实际 %d，body=%s", tc.want, rec.Code, rec.Body.String())
			}
		})
	}
}

// With a credential, business endpoints work, and the identity really reaches the business layer.
func TestAuthenticatedRequestSucceeds(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authedRequest(http.MethodPost, "/api/workspaces/ws-1/feishu/registrations"))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		SessionID       string `json:"sessionId"`
		VerificationURL string `json:"verificationUrl"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if out.SessionID == "" || out.VerificationURL == "" {
		t.Errorf("响应缺字段: %s", rec.Body.String())
	}
}

// The cookie channel must work too: browsers use it (including SSE).
func TestCookieCredentialWorks(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/ws-1/feishu/registrations", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: testAPIKey})
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Cookie 凭据期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}
}

// Identities must really be isolated: an unknown credential does not even get in the door.
func TestUnknownCredentialRejected(t *testing.T) {
	srv := newTestServer(t)

	// First start a registration session with a valid identity.
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, authedRequest(http.MethodPost, "/api/workspaces/ws-1/feishu/registrations"))
	if rec.Code != http.StatusOK {
		t.Fatalf("发起失败: %d %s", rec.Code, rec.Body.String())
	}
	var challenge struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &challenge)

	// The starter polling it themselves should work.
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, authedRequest(http.MethodGet,
		"/api/workspaces/ws-1/feishu/registrations/"+challenge.SessionID))
	if rec2.Code != http.StatusOK {
		t.Errorf("发起人轮询期望 200，实际 %d %s", rec2.Code, rec2.Body.String())
	}

	// Switch to an unknown credential: it does not even get in the door.
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet,
		"/api/workspaces/ws-1/feishu/registrations/"+challenge.SessionID, nil)
	req3.Header.Set("Authorization", "Bearer "+otherAPIKey)
	srv.Handler().ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusUnauthorized {
		t.Errorf("陌生凭据期望 401，实际 %d", rec3.Code)
	}
}

// Mapping of sentinel errors to status codes.
func TestErrorMapping(t *testing.T) {
	srv := newTestServer(t)
	cases := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{"会话不存在", http.MethodGet, "/api/workspaces/ws-1/feishu/registrations/nope", http.StatusNotFound},
		{"应用不存在", http.MethodGet, "/api/feishu/apps/nope", http.StatusNotFound},
		{"删不存在的应用", http.MethodDelete, "/api/feishu/apps/nope", http.StatusNotFound},
		{"重检不存在的应用", http.MethodPost, "/api/feishu/apps/nope/readiness/refresh", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, authedRequest(tc.method, tc.path))
			if rec.Code != tc.want {
				t.Fatalf("期望 %d，实际 %d，body=%s", tc.want, rec.Code, rec.Body.String())
			}
			// The error body must carry both a message for people and a code for programs.
			var out struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("解析错误体失败: %v", err)
			}
			if out.Message == "" || out.Code == "" {
				t.Errorf("错误体应同时含 message 与 code，实际 %s", rec.Body.String())
			}
		})
	}
}

// On a method mismatch ServeMux should return 405, not handle GET as POST.
//
// **This is also the regression test for "where the authentication middleware sits".** If
// authentication wrapped the whole mux, this request would be stopped with 401 first, and the 405
// would never come back. So authentication is wrapped per route inside routing (see protectedMux
// in server.go).
func TestMethodRouting(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	// The registration endpoint only accepts POST. **Send a valid credential** so "405 or 401" only
	// depends on where the middleware sits, not on the identity.
	srv.Handler().ServeHTTP(rec, authedRequest(http.MethodPut, "/api/workspaces/ws-1/feishu/registrations"))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("期望 405，实际 %d——鉴权中间件包错位置会让它变成 401", rec.Code)
	}
}

// Without a credential, a request with the wrong method should also be 405, not 401: routing is
// decided before authentication, which is a precondition of the standard library's 405 behavior.
func TestMethodRoutingWithoutCredential(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/workspaces/ws-1/feishu/registrations", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("期望 405，实际 %d", rec.Code)
	}
}

// CORS headers are only sent for the configured origin, and never as a wildcard.
func TestCORS(t *testing.T) {
	mux := http.NewServeMux()
	fake := httptest.NewServer(mux)
	t.Cleanup(fake.Close)
	svc, err := feishu.New(feishu.Config{Store: feishu.NewMemStore(), AccountsURL: fake.URL, OpenURL: fake.URL, Authorizer: allowAllForTest{}})
	if err != nil {
		t.Fatalf("建 Service 失败: %v", err)
	}
	srv, err := New(Config{
		Addr: ":0", FeishuService: svc, AuthHandler: newTestAuth(t), Authz: newTestAuthz(t),
		AllowedOrigin: "http://localhost:5273",
	})
	if err != nil {
		t.Fatalf("建 Server 失败: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/api/feishu/apps/x", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("预检期望 204，实际 %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5273" {
		t.Errorf("来源头期望具体地址，实际 %q", got)
	}
	// Authorization must be allowed, otherwise callers with Bearer cannot pass the preflight.
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Error("应放开 Authorization 头，否则脚本带不了凭据")
	}
	// **Credentials must be allowed cross-origin**, otherwise the browser does not send the cookie
	// and the login state does not get through.
	if rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Error("跨域必须允许带凭据，否则 Cookie 发不过来")
	}
	if rec.Header().Get("Vary") != "Origin" {
		t.Error("应带 Vary: Origin，否则中间层可能跨站缓存响应")
	}
	// ⭐ **logid must be exposed explicitly, otherwise the frontend cannot read it cross-origin.**
	//
	// By default browsers only let JS read 7 CORS-safelisted response headers; all others **silently
	// return null**, with no error and no warning. Problem reports need to collect this id
	// (errorFrom in web/src/data/http.ts reads it); without this header that feature silently breaks
	// in deployments where "the frontend points to another host", showing up as "the logid is
	// sometimes there and sometimes not", and nothing points to CORS.
	//
	// The literal is pinned instead of comparing with requestid.Header: that would make both sides
	// come from the same source; changing the constant wrongly would keep both sides equal and the
	// test green, while production can no longer link them (the same mutation-test conclusion as in
	// the comment in logid_header_test.go).
	if got := rec.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, "X-Request-Id") {
		t.Errorf("必须 expose logid 头，否则跨域下 JS 读不到、问题反馈采不到它。实际 %q", got)
	}
}

// A panic must be caught and return 500 instead of dropping the connection.
func TestRecoverPanic(t *testing.T) {
	srv := newTestServer(t)
	handler := srv.recoverPanic(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("故意炸一下")
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("期望 500，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "服务内部错误") {
		t.Errorf("应返回可读的错误体，实际 %s", rec.Body.String())
	}
}

// SSE relies on Flush to send frame by frame. If any layer of the middleware chain hides
// http.Flusher, the chat page shows "spinning forever, then the whole reply pops out at once", with
// no error; it just looks like a slow backend.
func TestFlusherSurvivesMiddleware(t *testing.T) {
	srv := newTestServer(t)
	var flushable bool
	handler := srv.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, flushable = w.(http.Flusher)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if !flushable {
		t.Error("中间件链吃掉了 http.Flusher，SSE 会攒帧")
	}
}

// When ctx is canceled Run must exit gracefully and not hang.
func TestRunShutsDownOnContextCancel(t *testing.T) {
	srv := newTestServer(t)
	// Listen on a random port to avoid clashing with ports already in use on this machine.
	srv.http.Addr = "127.0.0.1:0"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	cancel()

	if err := <-done; err != nil {
		t.Fatalf("优雅退出应无错误，实际 %v", err)
	}
}

// TestStatusRecorderSupportsHijack pins "the logging middleware does not block the WebSocket upgrade".
//
// # Why it deserves its own test
//
// statusRecorder wraps the ResponseWriter, which hides the underlying http.Hijacker. Without the
// pass-through method, `gorilla/websocket`'s Upgrade gets "Hijack not supported"; **that error
// points at the websocket library, not at "the logging middleware added a wrapper"**, and the
// whole webshell terminal path depends on this upgrade.
//
// This is the same kind of problem as Flush (SSE once kept spinning because of it), so they are
// tested together.
func TestStatusRecorderSupportsHijack(t *testing.T) {
	var (
		hijackable bool
		flushable  bool
	)
	// Build the w the middleware wraps directly and ask whether it supports these two things.
	srvObj := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := srvObj.logRequest(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, hijackable = w.(http.Hijacker)
		_, flushable = w.(http.Flusher)
	}))

	// httptest.ResponseRecorder itself does not implement Hijacker, so a real server is needed:
	// only the ResponseWriter of a real connection has Hijack; testing with a recorder would always
	// give false, and the test would be just for show.
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	_ = resp.Body.Close()

	if !hijackable {
		t.Error("包过一层之后不支持 Hijack —— WebSocket 升级会失败，" +
			"而报错指向 websocket 库、不指向这里")
	}
	if !flushable {
		t.Error("包过一层之后不支持 Flush —— SSE 会攒在缓冲区里，页面表现为一直转圈")
	}
}
