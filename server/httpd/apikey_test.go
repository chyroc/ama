// End-to-end tests of the three API Key endpoints: real mux + real Protect + real Guard + real seed.
//
// # Why this layer cannot be skipped
//
// The unit tests in the auth package check Service logic; there are no routes and no Guard there.
// What really goes wrong with these three endpoints is at the seams:
//
//	path has {workspaceId} but does not filter → relies on Guard to block non-members; if Guard is off, it is overreach
//	wrong owner (from parameters instead of context) → can list someone else's keys
//	revoke hangs on ActionNone → the SQL where condition must enforce ownership
//
// All of these are green in unit tests. So this hits real routes with **real wiring**.

package httpd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/auth"
)

// createdKey is the response shape of the create endpoint.
type createdKey struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Prefix     string `json:"prefix"`
	Key        string `json:"key"`
	CreatedAt  string `json:"createdAt"`
	LastUsedAt string `json:"lastUsedAt"`
}

// createAPIKeyAs creates a key as some role and returns the response.
func createAPIKeyAs(t *testing.T, srv http.Handler, email, name string) createdKey {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodPost,
		"/api/workspaces/"+rbacWS+"/api-keys", email, `{"name":"`+name+`"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建 API Key 得到 %d，期望 201。body=%s", rec.Code, rec.Body.String())
	}
	var out createdKey
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析创建响应失败: %v", err)
	}
	return out
}

// TestAPIKeyEndToEnd runs the full path: create → call a real endpoint with it → list → revoke →
// invalid.
//
// **This is the most convincing one**: it proves "calling an endpoint with a key" really equals
// "the person calling it while logged in".
func TestAPIKeyEndToEnd(t *testing.T) {
	srv := newRBACServer(t)

	created := createAPIKeyAs(t, srv, roleOwner, "my-agent")
	if created.Key == "" {
		t.Fatal("创建响应里没有明文 key")
	}
	if !strings.HasPrefix(created.Key, auth.KeyPrefix) {
		t.Errorf("明文 key 是 %q，应该以 %q 开头", created.Key, auth.KeyPrefix)
	}

	// Call a **real business endpoint** with this key (not one of the three API Key endpoints).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/"+rbacWS+"/agents", nil)
	req.Header.Set("Authorization", "Bearer "+created.Key)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("用 API Key 调 /agents 得到 %d，期望 200。body=%s", rec.Code, rec.Body.String())
	}
	// What comes back must be real data (ws_demo in the seed has agents), not an empty array: an
	// empty array would make "permissions not working" look like "no agents".
	if body := rec.Body.String(); !strings.Contains(body, "agent_") {
		t.Errorf("用 API Key 拿到的 agent 列表里没有种子数据: %s", body)
	}

	// It shows up in the list, with **no plain text**.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodGet, "/api/workspaces/"+rbacWS+"/api-keys", roleOwner, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("列表得到 %d，期望 200", rec.Code)
	}
	listBody := rec.Body.String()
	if strings.Contains(listBody, created.Key) {
		t.Error("❌ 列表接口返回了明文 key")
	}
	if !strings.Contains(listBody, created.ID) {
		t.Errorf("列表里没有刚创建的 key: %s", listBody)
	}

	// Revoke.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodDelete, "/api/api-keys/"+created.ID, roleOwner, ""))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("吊销得到 %d，期望 204。body=%s", rec.Code, rec.Body.String())
	}

	// After the revoke the key stops working at once, **with 401, not 403**: it is no longer a
	// recognizable identity, not "right identity but insufficient permission".
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/workspaces/"+rbacWS+"/agents", nil)
	req.Header.Set("Authorization", "Bearer "+created.Key)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("吊销后用那把 key 得到 %d，期望 401", rec.Code)
	}
}

// TestAPIKeyInheritsCreatorPermissions checks that a key strictly inherits its creator's role.
//
// **This is the core assertion of the "key bound to a person, not a workspace" design.** A key
// created by a viewer can only read, not write: if it could write, the credential would bypass the
// role, with no error at all.
func TestAPIKeyInheritsCreatorPermissions(t *testing.T) {
	srv := newRBACServer(t)

	viewerKey := createAPIKeyAs(t, srv, roleViewer, "viewer-key")

	// Can read.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/"+rbacWS+"/agents", nil)
	req.Header.Set("Authorization", "Bearer "+viewerKey.Key)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("viewer 的 key 读 /agents 得到 %d，期望 200", rec.Code)
	}

	// **Cannot write**: viewer is read-only.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/"+rbacWS+"/agents",
		strings.NewReader(`{"name":"x","system":"s","model":{"id":"m"}}`))
	req.Header.Set("Authorization", "Bearer "+viewerKey.Key)
	req.Header.Set("Content-Type", "application/json")
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("❌ viewer 的 key 建 agent 得到 %d，期望 403——"+
			"key 必须严格继承创建者权限，否则凭据就绕过了角色", rec.Code)
	}

	// A non-member's key always gets 403 (cannot even read).
	//
	// Note that a non-member **cannot create** a key (those two are ActionRead and Guard blocks
	// them), so what is checked here is "a non-member cannot even create one".
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodPost,
		"/api/workspaces/"+rbacWS+"/api-keys", roleNobody, `{"name":"sneaky"}`))
	if rec.Code != http.StatusForbidden {
		t.Errorf("非成员创建 key 得到 %d，期望 403", rec.Code)
	}
}

// TestAPIKeyListIsPerAccountNotPerWorkspace checks that keys are account-level.
//
// Hitting the list endpoint through the path of **another workspace** should show the same keys;
// that is exactly what "account-level" means. If it filtered by workspace, a key the user created in
// workspace A would disappear in workspace B, and nothing in the UI would explain it.
func TestAPIKeyListIsPerAccountNotPerWorkspace(t *testing.T) {
	srv := newRBACServer(t)

	// **Create a second workspace ourselves instead of finding one in the seed**: the owner in the
	// seed may only be in one workspace, and then this test would t.Skip, and a skipped test checks
	// nothing. Creating a workspace is ActionNone (anyone can do it and becomes its owner), so this
	// step always succeeds.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodPost, "/api/workspaces", roleOwner,
		`{"name":"第二个空间","key":"second-space"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("建第二个空间得到 %d，期望 201。body=%s", rec.Code, rec.Body.String())
	}
	var second struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatalf("解析新空间失败: %v", err)
	}
	if second.ID == "" || second.ID == rbacWS {
		t.Fatalf("新空间 id 不对: %q", second.ID)
	}

	created := createAPIKeyAs(t, srv, roleOwner, "cross-space")

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodGet, "/api/workspaces/"+second.ID+"/api-keys", roleOwner, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("从另一个空间列表得到 %d，期望 200。body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), created.ID) {
		t.Errorf("❌ 在 %s 建的 key 在 %s 看不到——key 应该是账号级的，"+
			"路径里的 workspaceId 只用来判成员身份、不参与过滤", rbacWS, second.ID)
	}

	// It must also hold the other way round: a key created in the new workspace is visible in the
	// original one.
	fromSecond := httptest.NewRecorder()
	srv.ServeHTTP(fromSecond, asRole(http.MethodPost,
		"/api/workspaces/"+second.ID+"/api-keys", roleOwner, `{"name":"born-in-second"}`))
	if fromSecond.Code != http.StatusCreated {
		t.Fatalf("在第二个空间建 key 得到 %d", fromSecond.Code)
	}
	var born createdKey
	if err := json.Unmarshal(fromSecond.Body.Bytes(), &born); err != nil {
		t.Fatal(err)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodGet, "/api/workspaces/"+rbacWS+"/api-keys", roleOwner, ""))
	if !strings.Contains(rec.Body.String(), born.ID) {
		t.Errorf("❌ 在 %s 建的 key 在 %s 看不到（反方向也该可见）", second.ID, rbacWS)
	}
}

// TestAPIKeyCannotSeeOrRevokeOthers checks that you can neither get nor revoke someone else's key.
func TestAPIKeyCannotSeeOrRevokeOthers(t *testing.T) {
	srv := newRBACServer(t)

	victim := createAPIKeyAs(t, srv, roleAdmin, "admin-key")

	// Another member does not see it in their list.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodGet, "/api/workspaces/"+rbacWS+"/api-keys", roleMember, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("列表得到 %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), victim.ID) {
		t.Errorf("❌ member 的列表里出现了 admin 的 key")
	}

	// Cannot revoke it, and **gets 404, not 403** (does not reveal that the id exists).
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodDelete, "/api/api-keys/"+victim.ID, roleMember, ""))
	if rec.Code != http.StatusNotFound {
		t.Errorf("吊销别人的 key 得到 %d，期望 404（回 403 会确认这个 id 存在）", rec.Code)
	}
	if _, code := envelope(t, rec); code != "not_found" {
		t.Errorf("错误 code 是 %q，期望 not_found", code)
	}

	// The key still works.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/"+rbacWS+"/agents", nil)
	req.Header.Set("Authorization", "Bearer "+victim.Key)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("受害者的 key 失效了（%d）", rec.Code)
	}
}

// TestAPIKeyRequiresAuth checks that these three endpoints require authentication themselves.
//
// They operate on long-lived credentials and **must** be behind Protect. Without it anyone could
// create a key for any email, a complete entry point for identity forgery.
func TestAPIKeyRequiresAuth(t *testing.T) {
	srv := newRBACServer(t)

	cases := []struct{ method, path, body string }{
		{http.MethodGet, "/api/workspaces/" + rbacWS + "/api-keys", ""},
		{http.MethodPost, "/api/workspaces/" + rbacWS + "/api-keys", `{"name":"x"}`},
		{http.MethodDelete, "/api/api-keys/ak_whatever", ""},
	}
	for _, tc := range cases {
		var req *http.Request
		if tc.body == "" {
			req = httptest.NewRequest(tc.method, tc.path, nil)
		} else {
			req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("❌ 不带凭据 %s %s 得到 %d，期望 401——"+
				"这三条操作长期凭据，必须要求已登录", tc.method, tc.path, rec.Code)
		}
	}
}

// TestAPIKeyCreateRejectsEmptyName checks that the name is required, with the error 400 +
// invalid_argument.
func TestAPIKeyCreateRejectsEmptyName(t *testing.T) {
	srv := newRBACServer(t)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodPost,
		"/api/workspaces/"+rbacWS+"/api-keys", roleOwner, `{"name":"  "}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("空名字得到 %d，期望 400", rec.Code)
	}
	if _, code := envelope(t, rec); code != "invalid_argument" {
		t.Errorf("错误 code 是 %q，期望 invalid_argument", code)
	}
}

// TestAPIKeyListReturnsEmptyArray checks that with no keys it returns [] and not null.
//
// null would make the frontend's .map() throw a TypeError (see rule one of model/apimodel).
func TestAPIKeyListReturnsEmptyArray(t *testing.T) {
	srv := newRBACServer(t)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodGet, "/api/workspaces/"+rbacWS+"/api-keys", roleViewer, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("列表得到 %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("没有 key 时回了 %q，期望 []（null 会让前端 .map() 抛异常）", got)
	}
}

// -- The two account-level routes ---------------------------------------------
//
// `/api/api-keys` is for the "global settings" page: that page is outside any workspace and has no
// workspaceId. The three cases below make sure it is **equivalent** to the two workspace-scoped
// routes, and check the extra capability it has (people in no workspace can manage their own keys).

// TestGlobalAPIKeyRoutesAreEquivalent checks that both path shapes return the same keys.
//
// **This asserts "only the path changed, not the resource".** If each side stored its own copy, a key
// created on the global settings page would not be visible on the workspace settings page, and
// nothing in the UI would explain it.
func TestGlobalAPIKeyRoutesAreEquivalent(t *testing.T) {
	srv := newRBACServer(t)

	// Create one through the **workspace-scoped** route.
	viaWorkspace := createAPIKeyAs(t, srv, roleOwner, "born-in-workspace")

	// And one through the **account-level** route.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodPost, "/api/api-keys", roleOwner, `{"name":"born-in-global"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/api-keys 得到 %d，期望 201。body=%s", rec.Code, rec.Body.String())
	}
	var viaGlobal createdKey
	if err := json.Unmarshal(rec.Body.Bytes(), &viaGlobal); err != nil {
		t.Fatalf("解析创建响应失败: %v", err)
	}
	if viaGlobal.Key == "" {
		t.Fatal("账号维度创建的响应里没有明文 key")
	}

	// Both list endpoints must see both keys.
	for _, path := range []string{"/api/api-keys", "/api/workspaces/" + rbacWS + "/api-keys"} {
		rec = httptest.NewRecorder()
		srv.ServeHTTP(rec, asRole(http.MethodGet, path, roleOwner, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s 得到 %d，期望 200。body=%s", path, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, viaWorkspace.ID) {
			t.Errorf("❌ GET %s 看不到空间维度建的 key（%s）——两种路径形状"+
				"必须是同一批资源，否则用户会以为 key 丢了", path, viaWorkspace.ID)
		}
		if !strings.Contains(body, viaGlobal.ID) {
			t.Errorf("❌ GET %s 看不到账号维度建的 key（%s）", path, viaGlobal.ID)
		}
	}
}

// TestGlobalAPIKeyRoutesWorkWithoutAnyWorkspace checks that people in no workspace can manage their
// own keys.
//
// **This is exactly why these two routes were added.** roleNobody is not a member of any workspace,
// so the two workspace-scoped routes always give them 403 (Guard requires at least membership). And
// they need a key the most: "not added to any workspace yet" and "wants to use the API" do not
// conflict.
//
// Declaring it ActionRead would turn this case red, which is exactly why it cannot be Read.
func TestGlobalAPIKeyRoutesWorkWithoutAnyWorkspace(t *testing.T) {
	srv := newRBACServer(t)

	// Precondition: they really cannot get into the workspace-scoped route (otherwise the comparison
	// below proves nothing).
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodGet, "/api/workspaces/"+rbacWS+"/api-keys", roleNobody, ""))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("前提不成立：非成员打空间维度的列表得到 %d，期望 403", rec.Code)
	}

	// Account-level: creation works.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodPost, "/api/api-keys", roleNobody, `{"name":"homeless"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("❌ 不属于任何空间的人建 key 得到 %d，期望 201——"+
			"账号级配置不该依赖某个空间。body=%s", rec.Code, rec.Body.String())
	}
	var mine createdKey
	if err := json.Unmarshal(rec.Body.Bytes(), &mine); err != nil {
		t.Fatal(err)
	}

	// Listing works too.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodGet, "/api/api-keys", roleNobody, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("非成员列自己的 key 得到 %d，期望 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), mine.ID) {
		t.Errorf("❌ 自己建的 key 自己看不到")
	}

	// ⚠️ But the key still inherits their **zero permissions**: using it on business endpoints still
	// gives 403. This cannot be skipped: it is the evidence that "ActionNone did not loosen
	// permissions".
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/workspaces/"+rbacWS+"/agents", nil)
	req.Header.Set("Authorization", "Bearer "+mine.Key)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("❌ 非成员用自己的 key 读别人空间的 agent 得到 %d，期望 403——"+
			"ActionNone 只是「这条路由不判空间」，绝不是「这把 key 有更多权限」", rec.Code)
	}
}

// TestGlobalAPIKeyRoutesStillRequireAuth checks that the two account-level routes also require
// authentication.
//
// They are ActionNone and **the easiest to mistake for "no login needed"**: ActionNone means "Guard
// does not check a workspace"; authentication is still done by Protect further out. Without it
// anyone could create a key for any email, a complete entry point for identity forgery.
func TestGlobalAPIKeyRoutesStillRequireAuth(t *testing.T) {
	srv := newRBACServer(t)

	cases := []struct{ method, path, body string }{
		{http.MethodGet, "/api/api-keys", ""},
		{http.MethodPost, "/api/api-keys", `{"name":"x"}`},
	}
	for _, tc := range cases {
		var req *http.Request
		if tc.body == "" {
			req = httptest.NewRequest(tc.method, tc.path, nil)
		} else {
			req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("❌ 不带凭据 %s %s 得到 %d，期望 401——ActionNone 不等于免鉴权",
				tc.method, tc.path, rec.Code)
		}
	}
}

// TestGlobalAPIKeyCannotSeeOthers checks that the account-level route also only returns your own keys.
//
// The workspace-scoped route has Guard behind it (non-members cannot get in); the account-level one
// **does not have that layer**, and ownership relies entirely on "the actor comes from the context".
// So it is checked separately.
func TestGlobalAPIKeyCannotSeeOthers(t *testing.T) {
	srv := newRBACServer(t)

	owners := createAPIKeyAs(t, srv, roleOwner, "owners-secret")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodGet, "/api/api-keys", roleMember, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("member 列自己的 key 得到 %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), owners.ID) {
		t.Errorf("❌ member 从 /api/api-keys 看到了 owner 的 key（%s）——"+
			"当事人必须从 context 取，绝不能从请求参数取", owners.ID)
	}
}

// TestGlobalAPIKeyIgnoresActorFromRequest checks that **the owner can only come from the context**.
//
// # This case was added after mutation testing; it watches a hole the other cases cannot see
//
// The cases above only check "A cannot see B's keys", and the requests they send **never carry any
// identity parameter at all**. So changing the handler to "request parameters take priority over
// the context":
//
//	actor := ActorFrom(r.Context())
//	if q := r.URL.Query().Get("email"); q != "" { actor = q }   // ← entry for overreach
//
// left **all those cases green** (measured), because nobody sent that parameter.
//
// This hole knocks down the only reason the two account-level ActionNone routes are acceptable:
// "overreach cannot be expressed in the parameters". Once it can be, and Guard does not check a
// workspace, any logged-in user can list and revoke anyone's keys, with **zero errors**.
//
// So here various identity parameters are put in on purpose, asserting none of them has any effect.
func TestGlobalAPIKeyIgnoresActorFromRequest(t *testing.T) {
	srv := newRBACServer(t)

	owners := createAPIKeyAs(t, srv, roleOwner, "owners-private-key")

	// member hits the list with all kinds of "I am the owner" hints; none should work.
	probes := []string{
		"/api/api-keys?email=" + roleOwner,
		"/api/api-keys?actor=" + roleOwner,
		"/api/api-keys?user=" + roleOwner,
		"/api/workspaces/" + rbacWS + "/api-keys?email=" + roleOwner,
	}
	for _, path := range probes {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, asRole(http.MethodGet, path, roleMember, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s 得到 %d，期望 200（参数应被忽略，而不是报错）", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), owners.ID) {
			t.Errorf("❌ member 用 %s 拿到了 owner 的 key（%s）——"+
				"当事人必须只从 context 取。请求参数是调用方能改的，"+
				"拿它当归属条件等于任何人都能列出任何人的 key，且零报错", path, owners.ID)
		}
	}

	// Same for create: with an email parameter it can still only create under its own name, not for
	// someone else.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodPost, "/api/api-keys?email="+roleOwner,
		roleMember, `{"name":"forged"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("带 email 参数创建得到 %d，期望 201", rec.Code)
	}

	// The key must appear in member's own list, **and not in owner's list**.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodGet, "/api/api-keys", roleOwner, ""))
	if strings.Contains(rec.Body.String(), "forged") {
		t.Errorf("❌ member 用 ?email= 把 key 建到了 owner 名下——" +
			"那等于任何人都能给任意邮箱造一把 key，是完整的身份伪造入口")
	}
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, asRole(http.MethodGet, "/api/api-keys", roleMember, ""))
	if !strings.Contains(rec.Body.String(), "forged") {
		t.Errorf("❌ 那把 key 没建到 member 自己名下")
	}
}
