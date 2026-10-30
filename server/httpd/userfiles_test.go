// End-to-end tests of the account-level personal file endpoints: real mux + real Protect + real
// Guard + real userfile.Service + real coremem storage; object storage is an in-process fake (bytes
// are joined in memory).
//
// What is checked is the seams: are the routes protected by Protect (401), does the matrix's
// ActionNone let requests through, owner isolation (others get 404), the two auth channels API Key
// and cookie, and the HTTP shape of the two upload protocols simple and multipart.
package httpd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/apihandler"
	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/authz"
	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/feishu"
	"github.com/chyroc/ama/server/orchestrator"
	"github.com/chyroc/ama/server/repository/dao/coremem"
	"github.com/chyroc/ama/server/service/userfile"
)

// fakeObjects is an in-memory userfile.ObjectStore: whole objects in a map, parts stored by uploadId
// and joined by part number on complete. Its behavior matches the key semantics of the real object
// storage.
type fakeObjects struct {
	mu      sync.Mutex
	bucket  string
	objects map[string][]byte
	uploads map[string]map[int][]byte
	seq     int
}

func newFakeObjects() *fakeObjects {
	return &fakeObjects{
		bucket:  "ama-test",
		objects: map[string][]byte{},
		uploads: map[string]map[int][]byte{},
	}
}

func (o *fakeObjects) Bucket() string { return o.bucket }

func (o *fakeObjects) Put(_ context.Context, key string, content []byte, _ string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.objects[key] = append([]byte(nil), content...)
	return nil
}

func (o *fakeObjects) Delete(_ context.Context, key string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.objects, key)
	return nil
}

func (o *fakeObjects) Size(_ context.Context, key string) (int64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	b, ok := o.objects[key]
	if !ok {
		return 0, userfile.ErrObjectNotFound
	}
	return int64(len(b)), nil
}

func (o *fakeObjects) Open(_ context.Context, key string) (io.ReadCloser, int64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	b, ok := o.objects[key]
	if !ok {
		return nil, 0, userfile.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), b...))), int64(len(b)), nil
}

func (o *fakeObjects) PresignedGetURL(key string, ttl time.Duration) (string, error) {
	return "https://objects.example.com/" + o.bucket + "/" + key + "?sig=1&ttl=" + ttl.String(), nil
}

func (o *fakeObjects) InitMultipartUpload(
	_ context.Context, key, _ string,
) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seq++
	up := fmt.Sprintf("up-%d", o.seq)
	o.uploads[up] = map[int][]byte{}
	return up, nil
}

func (o *fakeObjects) UploadPart(
	_ context.Context, _, uploadID string, partNumber int, size int64, content io.Reader,
) error {
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return fmt.Errorf("size 不符：声明 %d 实到 %d", size, len(data))
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	parts, ok := o.uploads[uploadID]
	if !ok {
		return fmt.Errorf("未知 uploadId %s", uploadID)
	}
	parts[partNumber] = append([]byte(nil), data...)
	return nil
}

func (o *fakeObjects) CompleteMultipartUpload(
	_ context.Context, key, uploadID string,
) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	parts, ok := o.uploads[uploadID]
	if !ok || len(parts) == 0 {
		return fmt.Errorf("没有分片")
	}
	nums := make([]int, 0, len(parts))
	for n := range parts {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	var all []byte
	for _, n := range nums {
		all = append(all, parts[n]...)
	}
	o.objects[key] = all
	delete(o.uploads, uploadID)
	return nil
}

func (o *fakeObjects) AbortMultipartUpload(
	_ context.Context, _, uploadID string,
) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.uploads, uploadID)
	return nil
}

// newUserFileServer builds a complete server with the personal file domain on (coremem + real
// userfile svc).
func newUserFileServer(t *testing.T, ob userfile.ObjectStore) http.Handler {
	t.Helper()

	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败: %v", err)
	}
	coreSvc, err := core.New(core.Config{Store: store, BlobStore: coremem.NewMemBlobStore()})
	if err != nil {
		t.Fatalf("建 core.Service 失败: %v", err)
	}
	ufSvc, err := userfile.New(userfile.Config{Store: store, Objects: ob})
	if err != nil {
		t.Fatalf("建 userfile.Service 失败: %v", err)
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
		t.Fatalf("建 orchestrator 失败: %v", err)
	}
	coreHandler, err := apihandler.NewHandler(apihandler.HandlerConfig{
		Orchestrator: orch,
		UserFiles:    ufSvc,
	})
	if err != nil {
		t.Fatalf("建 apihandler 失败: %v", err)
	}

	feishuStore := feishu.NewMemStore()
	membership := rbacMembership{core: coreSvc, feishu: feishuStore}
	guard, err := authz.New(authz.Config{Membership: membership})
	if err != nil {
		t.Fatal(err)
	}
	feishuSvc, err := feishu.New(feishu.Config{
		Store:      feishuStore,
		Authorizer: authz.NewFeishuAuthorizer(membership),
	})
	if err != nil {
		t.Fatal(err)
	}

	authStore := auth.NewMemStore()
	for _, email := range []string{roleOwner, roleNobody} {
		if err := authStore.PutSession(context.Background(), auth.Session{
			TokenHash: tokenHashFor(email),
			Email:     email,
			Name:      email,
			CreatedAt: time.Now(),
			ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	authSvc, err := auth.New(auth.Config{Store: authStore})
	if err != nil {
		t.Fatal(err)
	}
	authHandler, err := auth.NewHandler(auth.HandlerConfig{Service: authSvc})
	if err != nil {
		t.Fatal(err)
	}

	srv, err := New(Config{
		Addr: ":0", FeishuService: feishuSvc, CoreHandler: coreHandler,
		AuthHandler: authHandler, Authz: guard,
	})
	if err != nil {
		t.Fatalf("建 Server 失败: %v", err)
	}
	return srv.Handler()
}

// do sends a request and reads back the response body.
func do(srv http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func bearer(token, method, path string, body io.Reader, ct string) *http.Request {
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer "+token)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	return req
}

type userFileJSON struct {
	ID               string `json:"id"`
	Filename         string `json:"filename"`
	Status           string `json:"status"`
	UploadMode       string `json:"uploadMode"`
	PartSize         int64  `json:"partSize"`
	Size             int64  `json:"size"`
	DownloadURL      string `json:"downloadUrl"`
	ExpiresInSeconds int    `json:"expiresInSeconds"`
}

func TestUserFilesRequireAuth(t *testing.T) {
	srv := newUserFileServer(t, newFakeObjects())

	// No credential must give 401: ActionNone means "logged in, no workspace check", not "no login".
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/files"},
		{http.MethodPost, "/api/files"},
		{http.MethodGet, "/api/files/uf_x"},
		{http.MethodDelete, "/api/files/uf_x"},
	} {
		rec := do(srv, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s 无凭据得到 %d，要 401", tc.method, tc.path, rec.Code)
		}
	}
}

func TestUserFilesDisabledReturns503(t *testing.T) {
	// Reuse newRBACServer: it does not wire UserFiles (nil), so the file endpoints should return 503,
	// not 404.
	srv := newRBACServer(t)
	rec := do(srv, asRole(http.MethodGet, "/api/files", roleOwner, ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未装配文件域应回 503，得到 %d：%s", rec.Code, rec.Body.String())
	}
}

func TestUserFilesSimpleFlowEndToEnd(t *testing.T) {
	ob := newFakeObjects()
	srv := newUserFileServer(t, ob)

	// Start (small file → simple).
	body := `{"filename":"hello.txt","contentType":"text/plain","size":5}`
	rec := do(srv, asRole(http.MethodPost, "/api/files", roleOwner, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("发起上传得到 %d：%s", rec.Code, rec.Body.String())
	}
	var f userFileJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &f); err != nil {
		t.Fatal(err)
	}
	if f.UploadMode != "simple" || f.Status != "uploading" {
		t.Fatalf("模式不对：%+v", f)
	}

	// Upload the content.
	rec = do(srv, bearer(tokenTFor(roleOwner), http.MethodPut,
		"/api/files/"+f.ID+"/content", bytes.NewReader([]byte("hello")),
		"application/octet-stream"))
	if rec.Code != http.StatusOK {
		t.Fatalf("传内容得到 %d：%s", rec.Code, rec.Body.String())
	}

	// The details include a download URL.
	rec = do(srv, asRole(http.MethodGet, "/api/files/"+f.ID, roleOwner, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("详情得到 %d：%s", rec.Code, rec.Body.String())
	}
	json.Unmarshal(rec.Body.Bytes(), &f)
	if f.Status != "completed" || f.Size != 5 {
		t.Fatalf("完成态不对：%+v", f)
	}
	if !strings.HasPrefix(f.DownloadURL, "https://objects.example.com/") || f.ExpiresInSeconds != 3600 {
		t.Fatalf("下载字段不对：url=%q ttl=%d", f.DownloadURL, f.ExpiresInSeconds)
	}

	// Download relayed through amad (used by the CLI): the bytes are right, and it is always served
	// as an attachment, never with the uploaded type (same-origin XSS).
	rec = do(srv, asRole(http.MethodGet, "/api/files/"+f.ID+"/content", roleOwner, ""))
	if rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Fatalf("中转下载不对：%d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("中转下载的类型应固定 octet-stream，实际 %s", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Errorf("中转下载应是 attachment，实际 %s", cd)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("中转下载缺 nosniff")
	}
	// Someone else downloading with the same id: 404 (does not reveal existence).
	rec = do(srv, asRole(http.MethodGet, "/api/files/"+f.ID+"/content", roleNobody, ""))
	if rec.Code != http.StatusNotFound {
		t.Errorf("他人中转下载应 404，实际 %d", rec.Code)
	}

	// The list has this one.
	rec = do(srv, asRole(http.MethodGet, "/api/files", roleOwner, ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), f.ID) {
		t.Fatalf("列表不对：%d %s", rec.Code, rec.Body.String())
	}

	// Delete.
	rec = do(srv, asRole(http.MethodDelete, "/api/files/"+f.ID, roleOwner, ""))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("删除得到 %d：%s", rec.Code, rec.Body.String())
	}
	rec = do(srv, asRole(http.MethodGet, "/api/files/"+f.ID, roleOwner, ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("删后应 404，得到 %d", rec.Code)
	}
	if _, ok := ob.objects["user-files/"+roleOwner+"/"+f.ID]; ok {
		t.Error("对象存储对象没被删")
	}
}

func TestUserFilesMultipartFlowEndToEnd(t *testing.T) {
	ob := newFakeObjects()
	srv := newUserFileServer(t, ob)

	// No size → multipart.
	rec := do(srv, asRole(http.MethodPost, "/api/files", roleOwner,
		`{"filename":"big.bin","contentType":"application/octet-stream"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("发起得到 %d：%s", rec.Code, rec.Body.String())
	}
	var f userFileJSON
	json.Unmarshal(rec.Body.Bytes(), &f)
	if f.UploadMode != "multipart" || f.PartSize != 8*1024*1024 {
		t.Fatalf("应走 8MiB 分片：%+v", f)
	}

	// Two parts, out of order (2 first, then 1).
	p1 := bytes.Repeat([]byte("A"), 12)
	p2 := []byte("tail")
	putPart := func(n int, data []byte) *httptest.ResponseRecorder {
		return do(srv, bearer(tokenTFor(roleOwner), http.MethodPut,
			fmt.Sprintf("/api/files/%s/parts/%d", f.ID, n),
			bytes.NewReader(data), "application/octet-stream"))
	}
	if rec := putPart(2, p2); rec.Code != http.StatusOK {
		t.Fatalf("传片 2 得到 %d：%s", rec.Code, rec.Body.String())
	}
	if rec := putPart(1, p1); rec.Code != http.StatusOK {
		t.Fatalf("传片 1 得到 %d：%s", rec.Code, rec.Body.String())
	}
	// Invalid part number → 400.
	if rec := putPart(0, []byte("x")); rec.Code != http.StatusBadRequest {
		t.Fatalf("片号 0 应 400，得到 %d", rec.Code)
	}

	// Complete.
	rec = do(srv, asRole(http.MethodPost,
		"/api/files/"+f.ID+"/complete", roleOwner, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("完成得到 %d：%s", rec.Code, rec.Body.String())
	}
	json.Unmarshal(rec.Body.Bytes(), &f)
	if f.Size != 16 {
		t.Fatalf("完成大小 = %d，要 16", f.Size)
	}
	// In object storage the bytes are joined by part number.
	if got := ob.objects["user-files/"+roleOwner+"/"+f.ID]; !bytes.Equal(got, append(p1, p2...)) {
		t.Fatalf("拼接字节不对：%q", got)
	}
}

func TestUserFilesAbortEndToEnd(t *testing.T) {
	srv := newUserFileServer(t, newFakeObjects())

	rec := do(srv, asRole(http.MethodPost, "/api/files", roleOwner,
		`{"filename":"big.bin","size":9000000}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("发起得到 %d：%s", rec.Code, rec.Body.String())
	}
	var f userFileJSON
	json.Unmarshal(rec.Body.Bytes(), &f)

	rec = do(srv, asRole(http.MethodPost, "/api/files/"+f.ID+"/abort", roleOwner, ""))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("abort 得到 %d：%s", rec.Code, rec.Body.String())
	}
	// After abort the row is gone, and the list does not include it.
	rec = do(srv, asRole(http.MethodGet, "/api/files", roleOwner, ""))
	if strings.Contains(rec.Body.String(), f.ID) {
		t.Fatal("abort 后列表里不应再有该文件")
	}
}

func TestUserFilesOwnerIsolation(t *testing.T) {
	srv := newUserFileServer(t, newFakeObjects())

	// A uploads a file.
	rec := do(srv, asRole(http.MethodPost, "/api/files", roleOwner,
		`{"filename":"secret.txt","size":3}`))
	var f userFileJSON
	json.Unmarshal(rec.Body.Bytes(), &f)
	do(srv, bearer(tokenTFor(roleOwner), http.MethodPut,
		"/api/files/"+f.ID+"/content", bytes.NewReader([]byte("abc")),
		"application/octet-stream"))

	// B (non-member, but logged in) reading / deleting should get 404, not 403 or 200.
	rec = do(srv, asRole(http.MethodGet, "/api/files/"+f.ID, roleNobody, ""))
	if rec.Code != http.StatusNotFound {
		t.Errorf("他人读应 404，得到 %d：%s", rec.Code, rec.Body.String())
	}
	rec = do(srv, asRole(http.MethodDelete, "/api/files/"+f.ID, roleNobody, ""))
	if rec.Code != http.StatusNotFound {
		t.Errorf("他人删应 404，得到 %d", rec.Code)
	}
	// B's list is empty, and A's file is still there.
	rec = do(srv, asRole(http.MethodGet, "/api/files", roleNobody, ""))
	if strings.Contains(rec.Body.String(), f.ID) {
		t.Error("B 的列表里不该有 A 的文件")
	}
	rec = do(srv, asRole(http.MethodGet, "/api/files/"+f.ID, roleOwner, ""))
	if rec.Code != http.StatusOK {
		t.Errorf("A 自己的文件应仍可读，得到 %d", rec.Code)
	}
}

func TestUserFilesAPIKeyAuth(t *testing.T) {
	srv := newUserFileServer(t, newFakeObjects())

	// Create an amak_ key through the real endpoint (as roleOwner).
	created := createAPIKeyAs(t, srv, roleOwner, "file-ci")

	// Run a full simple upload with this key (no cookie), proving it works by API key.
	rec := do(srv, bearer(created.Key, http.MethodPost, "/api/files",
		strings.NewReader(`{"filename":"k.txt","size":2}`), "application/json"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("API Key 发起上传得到 %d：%s", rec.Code, rec.Body.String())
	}
	var f userFileJSON
	json.Unmarshal(rec.Body.Bytes(), &f)
	rec = do(srv, bearer(created.Key, http.MethodPut,
		"/api/files/"+f.ID+"/content", bytes.NewReader([]byte("ok")),
		"application/octet-stream"))
	if rec.Code != http.StatusOK {
		t.Fatalf("API Key 传内容得到 %d：%s", rec.Code, rec.Body.String())
	}
	// The file belongs to the key's creator, and the session identity can read it.
	rec = do(srv, asRole(http.MethodGet, "/api/files/"+f.ID, roleOwner, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("同属主会话读不到 key 传的文件：%d", rec.Code)
	}
}

func TestUserFilesRejectsChunkedAndOversize(t *testing.T) {
	srv := newUserFileServer(t, newFakeObjects())

	// Start a file with simple.
	rec := do(srv, asRole(http.MethodPost, "/api/files", roleOwner,
		`{"filename":"f","size":1}`))
	var f userFileJSON
	json.Unmarshal(rec.Body.Bytes(), &f)

	// chunked (no Content-Length) → 411.
	req := bearer(tokenTFor(roleOwner), http.MethodPut,
		"/api/files/"+f.ID+"/content", strings.NewReader("x"), "application/octet-stream")
	req.ContentLength = -1
	if rec := do(srv, req); rec.Code != http.StatusLengthRequired {
		t.Errorf("chunked 应 411，得到 %d", rec.Code)
	}

	// Over 8 MiB → 413.
	big := bytes.Repeat([]byte("z"), 8*1024*1024+10)
	req = bearer(tokenTFor(roleOwner), http.MethodPut,
		"/api/files/"+f.ID+"/content", bytes.NewReader(big), "application/octet-stream")
	if rec := do(srv, req); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("超限应 413，得到 %d（body=%s）", rec.Code, rec.Body.String())
	}
}
