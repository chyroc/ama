// Credential lookup and request building for the Ark passthrough: the run scope resolves
// the key through three levels using the session's workspace/agent; the workspace scope
// resolves by workspace (empty agentID → falls back to global).
// The byte passthrough itself is covered by the tests in shared/infra/ma.

package core

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/repository/dao/coremem"
	"github.com/chyroc/ama/server/shared/infra/ma"
)

// proxyFixture reuses arkKeyFixture: two agents under ws_key, with one workspace-level key configured.
func proxyFixture(t *testing.T) (*Service, *fakeRunner, string, string) {
	t.Helper()
	svc, store, runner, ws, agentA, _ := arkKeyFixture(t)
	ctx := context.Background()
	if err := store.PutArkSecret(ctx, apimodel.ArkSecret{
		ArkCredential: apimodel.ArkCredential{
			WorkspaceID: ws, Region: apimodel.RegionCNBeijing,
			Source: apimodel.SourceOverride,
		},
		APIKey: "ark-workspace-key-000000000",
	}); err != nil {
		t.Fatalf("配空间 key 失败：%v", err)
	}
	// A run that belongs to agentA (the id is the Ark session id).
	run := apimodel.Run{
		ID: "sesn-run-1", WorkspaceID: ws, AgentID: agentA,
		Status: apimodel.RunIdle, Threads: []apimodel.RunThread{},
		StartedAt: apimodel.NowISO(), LastActivityAt: apimodel.NowISO(),
	}
	if _, err := store.CreateRun(ctx, run); err != nil {
		t.Fatalf("建 run 失败：%v", err)
	}
	return svc, runner, ws, run.ID
}

func TestArkProxyDoForRunResolvesSessionKeyAndPath(t *testing.T) {
	svc, runner, _, runID := proxyFixture(t)

	var gotKey ArkBindingKey
	var gotReq ma.RawRequest
	runner.rawDoFunc = func(key ArkBindingKey, req ma.RawRequest) (*ma.RawResponse, error) {
		gotKey = key
		gotReq = req
		return &ma.RawResponse{StatusCode: 200, Body: []byte(`{"data":[]}`)}, nil
	}

	resp, err := svc.ArkProxyDoForRun(context.Background(), runID, ma.RawRequest{
		Method: http.MethodPost,
		Path:   "/sessions/" + runID + "/resources",
		Body:   []byte(`{"type":"file"}`),
	})
	if err != nil {
		t.Fatalf("透传失败：%v", err)
	}
	if string(resp.Body) != `{"data":[]}` {
		t.Errorf("响应没原样返回：%s", resp.Body)
	}
	if gotKey.APIKey != "ark-workspace-key-000000000" {
		t.Errorf("用错了 key：%q", gotKey.APIKey)
	}
	if gotReq.Method != http.MethodPost || !strings.HasSuffix(gotReq.Path, "/resources") {
		t.Errorf("请求组装错：%+v", gotReq)
	}
}

func TestArkProxyDoForRunUsesAgentSpecificKey(t *testing.T) {
	svc, store, runner, ws, agentA, _ := arkKeyFixture(t)
	ctx := context.Background()
	// Workspace-level key
	if err := store.PutArkSecret(ctx, apimodel.ArkSecret{
		ArkCredential: apimodel.ArkCredential{
			WorkspaceID: ws, Region: apimodel.RegionCNBeijing, Source: apimodel.SourceOverride,
		},
		APIKey: "ark-workspace-key-000000000",
	}); err != nil {
		t.Fatal(err)
	}
	// agentA's own key (the three-level lookup should prefer it)
	if err := store.PutAgentArkSecret(ctx, apimodel.AgentArkSecret{
		AgentID:     agentA,
		WorkspaceID: ws,
		Region:      apimodel.RegionCNBeijing,
		APIKey:      "ark-agent-key-00000000000",
	}); err != nil {
		t.Fatalf("配 agent key 失败：%v", err)
	}
	run := apimodel.Run{
		ID: "sesn-run-agentkey", WorkspaceID: ws, AgentID: agentA,
		Status: apimodel.RunIdle, Threads: []apimodel.RunThread{},
		StartedAt: apimodel.NowISO(), LastActivityAt: apimodel.NowISO(),
	}
	if _, err := store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	runner.rawDoFunc = func(key ArkBindingKey, _ ma.RawRequest) (*ma.RawResponse, error) {
		return &ma.RawResponse{StatusCode: 200, Body: nil}, nil
	}
	if _, err := svc.ArkProxyDoForRun(ctx, run.ID, ma.RawRequest{Method: "GET", Path: "/x"}); err != nil {
		t.Fatal(err)
	}
	if runner.lastRawKey.APIKey != "ark-agent-key-00000000000" {
		t.Errorf("该用 agent 级 key，得到 %q", runner.lastRawKey.APIKey)
	}
}

func TestArkProxyDoForWorkspacePassesEmptyAgent(t *testing.T) {
	svc, runner, ws, _ := proxyFixture(t)
	runner.rawDoFunc = func(key ArkBindingKey, req ma.RawRequest) (*ma.RawResponse, error) {
		return &ma.RawResponse{StatusCode: 200, Body: []byte(`{}`)}, nil
	}
	if _, err := svc.ArkProxyDoForWorkspace(context.Background(), ws, ma.RawRequest{
		Method: http.MethodGet, Path: "/vaults",
	}); err != nil {
		t.Fatalf("workspace 作用域透传失败：%v", err)
	}
	if runner.lastRawKey.AgentID != "" {
		t.Errorf("workspace 作用域不该带 agentID：%q", runner.lastRawKey.AgentID)
	}
	if runner.lastRawKey.APIKey != "ark-workspace-key-000000000" {
		t.Errorf("key 解析错：%q", runner.lastRawKey.APIKey)
	}
}

func TestArkProxyForUnknownRunFails(t *testing.T) {
	svc, _, _, _ := proxyFixture(t)
	if _, err := svc.ArkProxyDoForRun(context.Background(), "sesn-missing", ma.RawRequest{
		Method: http.MethodGet, Path: "/x",
	}); err == nil {
		t.Fatal("不存在的 run 应当失败")
	}
}

// Without a runner (Ark not connected), return a clear error; do not panic.
func TestArkProxyWithoutRunner(t *testing.T) {

	svc, err := New(Config{Store: coremem.NewMemStore()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ArkProxyDoForWorkspace(context.Background(), "ws_x", ma.RawRequest{
		Method: http.MethodGet, Path: "/vaults",
	}); err == nil || !strings.Contains(err.Error(), "未接入方舟") {
		t.Fatalf("想要「未接入方舟」错误，得到 %v", err)
	}
}

// The key lookup for RawStream takes the same path; here we only check that it uses the run
// scope and returns a stream.
func TestArkProxyStreamForRun(t *testing.T) {
	svc, runner, _, runID := proxyFixture(t)
	runner.rawStreamFn = func(key ArkBindingKey, req ma.RawRequest) (*ma.RawStream, error) {
		return ma.NewRawStream(http.StatusOK, nil, io.NopCloser(strings.NewReader("data: {}\n\n"))), nil
	}
	stream, err := svc.ArkProxyStreamForRun(context.Background(), runID, ma.RawRequest{
		Method: http.MethodGet, Path: "/sessions/" + runID + "/threads/sthr-1/stream",
	})
	if err != nil {
		t.Fatalf("流式透传失败：%v", err)
	}
	defer stream.Close()
	if stream.StatusCode != 200 {
		t.Errorf("StatusCode = %d", stream.StatusCode)
	}
}
