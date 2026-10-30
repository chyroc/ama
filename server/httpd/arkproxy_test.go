// End-to-end tests of the Ark pass-through endpoints: real mux + real Guard + real auth on a real
// HTTP path, but the Ark runner (ArkRunner) is fake: it only takes over the two pass-through methods
// RawDo/RawStream; the other methods come from an embedded nil interface and panic right away if
// called by mistake (tests should never reach them).
//
// What is checked:
//   - byte-level pass-through: request body/query reach the runner unchanged, and the Ark response
//     (including 4xx) goes back to the client unchanged;
//   - path expansion: {runId} is filled into the Ark path automatically;
//   - permission levels: viewer cannot write (writing session resources), member cannot write Vault
//     credentials (needs admin);
//   - the 64 MiB body limit;
//   - a missing run uses the AMA envelope, not an Ark error.

package httpd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/apihandler"
	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/authz"
	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/feishu"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/orchestrator"
	"github.com/chyroc/ama/server/repository/dao/coremem"
	"github.com/chyroc/ama/server/shared/infra/ma"
)

// arkProxyRunner only implements the two pass-through methods; the embedded interface supplies the
// other method signatures. The test paths do not go through the main chat path, so those methods
// stay nil.
type arkProxyRunner struct {
	core.ArkRunner
	doFunc     func(key core.ArkBindingKey, req ma.RawRequest) (*ma.RawResponse, error)
	streamFunc func(key core.ArkBindingKey, req ma.RawRequest) (*ma.RawStream, error)
	gotKey     core.ArkBindingKey
	gotReq     ma.RawRequest
}

func (r *arkProxyRunner) RawDo(_ context.Context, key core.ArkBindingKey, req ma.RawRequest) (*ma.RawResponse, error) {
	r.gotKey = key
	r.gotReq = req
	return r.doFunc(key, req)
}

func (r *arkProxyRunner) RawStream(_ context.Context, key core.ArkBindingKey, req ma.RawRequest) (*ma.RawStream, error) {
	return r.streamFunc(key, req)
}

const (
	proxyRunID = "sesn-proxy-1"
	proxyWS    = "ws_demo"
)

func newArkProxyServer(t *testing.T, runner *arkProxyRunner) http.Handler {
	t.Helper()
	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败: %v", err)
	}
	// A workspace-level Ark key, needed for resolving the pass-through (the seed has no credentials).
	if err := store.PutArkSecret(context.Background(), apimodel.ArkSecret{
		ArkCredential: apimodel.ArkCredential{
			WorkspaceID: proxyWS, Region: apimodel.RegionCNBeijing,
			Source: apimodel.SourceOverride,
		},
		APIKey: "ark-proxy-key-00000000000",
	}); err != nil {
		t.Fatalf("配空间 key 失败：%v", err)
	}
	if _, err := store.CreateRun(context.Background(), apimodel.Run{
		ID: proxyRunID, WorkspaceID: proxyWS, AgentID: "agent_demo_1",
		Status: apimodel.RunIdle, Threads: []apimodel.RunThread{},
		StartedAt: apimodel.NowISO(), LastActivityAt: apimodel.NowISO(),
	}); err != nil {
		t.Fatalf("建 run 失败：%v", err)
	}

	coreSvc, err := core.New(core.Config{Store: store, BlobStore: coremem.NewMemBlobStore(), ArkRunner: runner})
	if err != nil {
		t.Fatalf("建 core.Service 失败: %v", err)
	}
	orch, err := orchestrator.New(orchestrator.Config{
		Issue:      coreSvc.Issues(),
		Suggestion: coreSvc.Suggestions(),
		Revision:   coreSvc.Revisions(),
		ArkCred:    coreSvc.ArkCreds(),
		Workspace:  coreSvc.Workspaces(),
		Agent:      coreSvc.Agents(),
		Schedule:   coreSvc.Schedules(),
		Run:        coreSvc.Runs(),
		Skill:      coreSvc.Skills(),
		Fallback:   coreSvc,
	})
	if err != nil {
		t.Fatalf("建 orchestrator 失败：%v", err)
	}
	h, err := apihandler.NewHandler(apihandler.HandlerConfig{Orchestrator: orch})
	if err != nil {
		t.Fatalf("建 apihandler.Handler 失败：%v", err)
	}

	feishuStore := feishu.NewMemStore()
	membership := rbacMembership{core: coreSvc, feishu: feishuStore}
	guard, err := authz.New(authz.Config{Membership: membership})
	if err != nil {
		t.Fatalf("建 authz.Guard 失败: %v", err)
	}
	feishuSvc, err := feishu.New(feishu.Config{
		Store:      feishuStore,
		Authorizer: authz.NewFeishuAuthorizer(membership),
	})
	if err != nil {
		t.Fatalf("建 feishu.Service 失败: %v", err)
	}
	authStore := auth.NewMemStore()
	for _, email := range []string{roleOwner, roleAdmin, roleMember, roleViewer, roleNobody} {
		if err := authStore.PutSession(context.Background(), auth.Session{
			TokenHash: tokenHashFor(email), Email: email, Name: email,
			CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("种会话失败: %v", err)
		}
	}
	authSvc, err := auth.New(auth.Config{Store: authStore})
	if err != nil {
		t.Fatalf("建 auth.Service 失败: %v", err)
	}
	authHandler, err := auth.NewHandler(auth.HandlerConfig{Service: authSvc})
	if err != nil {
		t.Fatalf("建 auth.Handler 失败: %v", err)
	}
	srv, err := New(Config{
		Addr: ":0", FeishuService: feishuSvc, CoreHandler: h,
		AuthHandler: authHandler, Authz: guard,
	})
	if err != nil {
		t.Fatalf("建 Server 失败: %v", err)
	}
	return srv.Handler()
}

func TestArkProxyForwardsBytesAndPath(t *testing.T) {
	runner := &arkProxyRunner{
		doFunc: func(key core.ArkBindingKey, req ma.RawRequest) (*ma.RawResponse, error) {
			return &ma.RawResponse{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       []byte(`{"data":[{"id":"res_1","type":"file","new_ark_field":7}]}`),
			}, nil
		},
	}
	srv := newArkProxyServer(t, runner)

	req := asRole(http.MethodPost,
		"/api/runs/"+proxyRunID+"/ark/resources?limit=50",
		roleMember, `{"type":"file","file_id":"file-1","mount_path":"data/x"}`)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("想要 200，得到 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "new_ark_field") {
		t.Errorf("方舟响应没原样转发：%s", rec.Body.String())
	}
	// The path and query the runner receives are in Ark's shape (including the session id).
	if runner.gotReq.Path != "/sessions/"+proxyRunID+"/resources" {
		t.Errorf("方舟路径拼错：%q", runner.gotReq.Path)
	}
	if runner.gotReq.Query.Get("limit") != "50" {
		t.Errorf("query 没转发：%v", runner.gotReq.Query)
	}
	if !strings.Contains(string(runner.gotReq.Body), `"mount_path":"data/x"`) {
		t.Errorf("请求体没原样转发：%s", runner.gotReq.Body)
	}
	if runner.gotKey.APIKey != "ark-proxy-key-00000000000" {
		t.Errorf("反解出的 key 不对：%q", runner.gotKey.APIKey)
	}
}

// When Ark returns 4xx: the HTTP status code and Ark's error body are forwarded unchanged, not
// through the AMA envelope.
func TestArkProxyForwardsUpstreamError(t *testing.T) {
	runner := &arkProxyRunner{
		doFunc: func(key core.ArkBindingKey, req ma.RawRequest) (*ma.RawResponse, error) {
			return &ma.RawResponse{
				StatusCode: http.StatusNotFound,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       []byte(`{"error":{"code":"ResourceNotFound","message":"没这个资源"}}`),
			}, maErr404()
		},
	}
	srv := newArkProxyServer(t, runner)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodGet,
		"/api/runs/"+proxyRunID+"/ark/resources/res_missing", roleMember, ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("想要 404，得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ResourceNotFound") {
		t.Errorf("方舟错误体没原样转发：%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"code":"not_found"`) {
		t.Errorf("方舟错误不该被翻成 AMA 信封：%s", rec.Body.String())
	}
}

// viewer can only read, not write: adding a session resource is blocked by Guard with 403.
func TestArkProxySessionWriteRequiresMember(t *testing.T) {
	srv := newArkProxyServer(t, &arkProxyRunner{doFunc: func(_ core.ArkBindingKey, req ma.RawRequest) (*ma.RawResponse, error) {
		if req.Method != http.MethodGet {
			t.Fatalf("viewer 的写请求不该到 runner，得到 %s", req.Method)
		}
		return &ma.RawResponse{StatusCode: http.StatusOK, Body: []byte(`{"data":[]}`)}, nil
	}})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodPost,
		"/api/runs/"+proxyRunID+"/ark/resources", roleViewer, `{}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 写会话资源应 403，得到 %d：%s", rec.Code, rec.Body.String())
	}
	// Reads are allowed.
	ok := httptest.NewRecorder()
	srv.ServeHTTP(ok, asRole(http.MethodGet,
		"/api/runs/"+proxyRunID+"/ark/resources", roleViewer, ""))
	if ok.Code != http.StatusOK {
		t.Fatalf("viewer 读资源应 200，得到 %d：%s", ok.Code, ok.Body.String())
	}
}

// Writing Vault credentials needs Manage: member gets 403, admin goes through to the runner.
func TestArkProxyVaultCredentialWriteRequiresAdmin(t *testing.T) {
	called := false
	srv := newArkProxyServer(t, &arkProxyRunner{doFunc: func(_ core.ArkBindingKey, _ ma.RawRequest) (*ma.RawResponse, error) {
		called = true
		return &ma.RawResponse{StatusCode: http.StatusOK, Body: []byte(`{}`)}, nil
	}})

	memberRec := httptest.NewRecorder()
	srv.ServeHTTP(memberRec, asRole(http.MethodPost,
		"/api/workspaces/"+proxyWS+"/ark/vaults/v1/credentials", roleMember, `{}`))
	if memberRec.Code != http.StatusForbidden {
		t.Fatalf("member 建凭证应 403，得到 %d", memberRec.Code)
	}
	if called {
		t.Fatal("member 请求不该到 runner")
	}

	adminRec := httptest.NewRecorder()
	srv.ServeHTTP(adminRec, asRole(http.MethodPost,
		"/api/workspaces/"+proxyWS+"/ark/vaults/v1/credentials", roleAdmin, `{}`))
	if adminRec.Code != http.StatusOK {
		t.Fatalf("admin 建凭证应放行，得到 %d：%s", adminRec.Code, adminRec.Body.String())
	}
}

// Non-members of the workspace get 403 on every pass-through.
func TestArkProxyNonMemberForbidden(t *testing.T) {
	srv := newArkProxyServer(t, &arkProxyRunner{doFunc: func(core.ArkBindingKey, ma.RawRequest) (*ma.RawResponse, error) {
		t.Fatal("非成员请求不该到 runner")
		return nil, nil
	}})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodGet,
		"/api/workspaces/"+proxyWS+"/ark/vaults", roleNobody, ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非成员应 403，得到 %d", rec.Code)
	}
}

// Missing run: an AMA error of its own, so it goes through the AMA envelope as 404.
func TestArkProxyUnknownRunIsAMAEnvelope(t *testing.T) {
	srv := newArkProxyServer(t, &arkProxyRunner{doFunc: func(core.ArkBindingKey, ma.RawRequest) (*ma.RawResponse, error) {
		t.Fatal("run 不存在时不该到 runner")
		return nil, nil
	}})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodGet,
		"/api/runs/sesn-nope/ark/resources", roleMember, ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("想要 404，得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not_found") {
		t.Errorf("该走 AMA 信封：%s", rec.Body.String())
	}
}

// A request body over 64 MiB is stopped with 413 before reaching Ark.
func TestArkProxyBodyTooLarge(t *testing.T) {
	srv := newArkProxyServer(t, &arkProxyRunner{doFunc: func(core.ArkBindingKey, ma.RawRequest) (*ma.RawResponse, error) {
		t.Fatal("超大 body 不该到 runner")
		return nil, nil
	}})
	big := strings.Repeat("x", 64<<20+10)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodPost,
		"/api/runs/"+proxyRunID+"/ark/resources", roleMember, big))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("想要 413，得到 %d", rec.Code)
	}
}

// SSE pass-through: when upstream returns 200 the raw event bytes are relayed; when upstream returns
// 4xx the error body is returned with the 4xx.
func TestArkProxyStreamRelaysBytes(t *testing.T) {
	runner := &arkProxyRunner{
		streamFunc: func(_ core.ArkBindingKey, _ ma.RawRequest) (*ma.RawStream, error) {
			return ma.NewRawStream(http.StatusOK,
				http.Header{"Content-Type": []string{"text/event-stream"}},
				io.NopCloser(strings.NewReader(": ready\n\ndata: {\"type\":\"x\"}\n\n"))), nil
		},
	}
	srv := newArkProxyServer(t, runner)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodGet,
		"/api/runs/"+proxyRunID+"/ark/threads/sthr-1/stream", roleMember, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("想要 200，得到 %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "data: {\"type\":\"x\"}") {
		t.Errorf("SSE 字节没原样中继：%q", rec.Body.String())
	}
}

func TestArkProxyStreamUpstream4xx(t *testing.T) {
	runner := &arkProxyRunner{
		streamFunc: func(_ core.ArkBindingKey, _ ma.RawRequest) (*ma.RawStream, error) {
			return ma.NewRawStream(http.StatusBadRequest,
				http.Header{"Content-Type": []string{"application/json"}},
				io.NopCloser(strings.NewReader(`{"error":{"code":"BadRequest"}}`))), nil
		},
	}
	srv := newArkProxyServer(t, runner)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodGet,
		"/api/runs/"+proxyRunID+"/ark/threads/sthr-missing/stream", roleMember, ""))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("想要 400，得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "BadRequest") {
		t.Errorf("4xx 错误体没转发：%s", rec.Body.String())
	}
}

// maErr404 builds an Ark 404 structured error (the pass-through layer returns it along with the raw
// response).
func maErr404() error {
	return &ma.APIError{HTTPStatusCode: 404, Code: "ResourceNotFound", Message: "没这个资源"}
}

// Even if upstream returns text/html (a tainted response), a same-origin browser must not be able to
// render scripts from it: Content-Type must be forced to JSON.
func TestArkProxyDoesNotForwardHTMLContentType(t *testing.T) {
	runner := &arkProxyRunner{
		doFunc: func(_ core.ArkBindingKey, _ ma.RawRequest) (*ma.RawResponse, error) {
			return &ma.RawResponse{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
				Body:       []byte(`<script>alert(1)</script>`),
			}, nil
		},
	}
	srv := newArkProxyServer(t, runner)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodGet,
		"/api/runs/"+proxyRunID+"/ark/resources", roleMember, ""))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("text/html 必须被收敛成 JSON，得到 %q", ct)
	}
	if rec.Header().Get("Content-Disposition") == "" {
		t.Error("应有 Content-Disposition: attachment 防止导航渲染")
	}
}
