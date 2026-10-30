// End-to-end authorization: real mux + real core.Handler + real authz.Guard + real seed data.
//
// # Why this layer is needed
//
// The authz package's cases check the matrix and core's cases check filtering, but both are **unit**
// tests: each uses a fake of the other. What really goes wrong is at the seams: Guard wired in the
// wrong place, a pattern in the matrix differing from the real route by one character, resolving
// with the wrong parameter name. All of these are green in both unit tests.
//
// So this file runs with **real wiring**: as four roles + one non-member, it hits real routes and
// asserts status codes and messages. That also covers the four kinds of assertions in the
// requirement.
//
// Identities come from **seeding a real session for each role in auth's store**; requests carry the
// matching Bearer token. Not the "put an Identity into the context" trick: Protect overwrites it
// with the identity it resolves itself, so every request becomes 401, and the error does not point
// to the real cause at all (it looks like authorization blocked everyone).

package httpd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/repository/dao/coremem"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/authz"
	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/feishu"
	"github.com/chyroc/ama/server/model/apimodel"
)

// The four members of the seed workspace ws_demo (key=growth), one per role.
const (
	rbacWS     = "ws_demo"
	roleOwner  = "alice@example.com"
	roleAdmin  = "bob@example.com"
	roleMember = "carol@example.com"
	roleViewer = "dave@example.com"
	roleNobody = "outsider@example.com"
)

// newRBACServer builds a **complete** Server: real core + real authz + seed data.
//
// The difference from newTestServer is that it wires CoreHandler and a real Membership; those two
// are where authorization really takes effect.
func newRBACServer(t *testing.T) http.Handler {
	t.Helper()

	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败: %v", err)
	}
	coreSvc, err := core.New(core.Config{Store: store, BlobStore: coremem.NewMemBlobStore()})
	if err != nil {
		t.Fatalf("建 core.Service 失败: %v", err)
	}
	coreHandler := newCoreRoutes(t, coreSvc, nil)

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

	// Seed a real session for each of the five roles, so Protect can resolve the Bearer token into
	// the matching identity.
	authStore := auth.NewMemStore()
	for _, email := range []string{roleOwner, roleAdmin, roleMember, roleViewer, roleNobody} {
		if err := authStore.PutSession(context.Background(), auth.Session{
			TokenHash: tokenHashFor(email),
			Email:     email,
			Name:      email,
			CreatedAt: time.Now(),
			ExpiresAt: time.Now().Add(time.Hour),
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
		Addr: ":0", FeishuService: feishuSvc, CoreHandler: coreHandler,
		AuthHandler: authHandler, Authz: guard,
	})
	if err != nil {
		t.Fatalf("建 Server 失败: %v", err)
	}
	return srv.Handler()
}

// rbacMembership is the real membership: core looks up roles and three kinds of resources, feishu
// looks up apps.
//
// It has the same shape as authzMembership in cmd/amad. **It is deliberately rewritten in the test**
// instead of exporting the production one: that one is in the main package and cannot be exported;
// and this rewrite also happens to check that "this adapter is thin and hard to get wrong".
type rbacMembership struct {
	core   *core.Service
	feishu feishu.Store
}

func (m rbacMembership) RoleOf(
	ctx context.Context, workspaceID, actor string,
) (apimodel.WorkspaceRole, bool, error) {
	return m.core.RoleOf(ctx, workspaceID, actor)
}

func (m rbacMembership) WorkspaceOf(ctx context.Context, kind authz.ResourceKind, id string) (string, error) {
	var ck core.ResourceKind
	switch kind {
	case authz.KindAgent:
		ck = core.KindAgent
	case authz.KindSkill:
		ck = core.KindSkill
	case authz.KindRun:
		ck = core.KindRun
	case authz.KindSchedule:
		ck = core.KindSchedule
	case authz.KindIssue:
		ck = core.KindIssue
	case authz.KindIssueComment:
		ck = core.KindIssueComment
	case authz.KindIssueAttachment:
		ck = core.KindIssueAttachment
	case authz.KindFeishuApp:
		app, err := m.feishu.GetApp(ctx, id)
		if err != nil {
			return "", authz.ErrResourceNotFound
		}
		return app.WorkspaceID, nil
	default:
		return "", authz.ErrResourceNotFound
	}
	ws, err := m.core.WorkspaceOf(ctx, ck, id)
	if err != nil {
		return "", authz.ErrResourceNotFound
	}
	return ws, nil
}

// tokenTFor is the session token (plain text) of a role; tokenHashFor is its stored form.
//
// auth's store keeps the sha256, so it is computed here; hashToken is unexported in the auth package.
func tokenTFor(email string) string { return "session-token-" + email }

func tokenHashFor(email string) string {
	sum := sha256.Sum256([]byte(tokenTFor(email)))
	return hex.EncodeToString(sum[:])
}

// asRole builds a request "logged in as some role", carrying that person's session token.
//
// **It must use real credentials, not put an Identity into the context**: Protect overwrites the
// context with the identity it resolves itself, the one put in never reaches the handler, and the
// symptom is that every request gets 401.
func asRole(method, path, email, body string) *http.Request {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+tokenTFor(email))
	return req
}

// envelope decodes the response envelope.
func envelope(t *testing.T, rec *httptest.ResponseRecorder) (message, code string) {
	t.Helper()
	var out struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Message, out.Code
}

// TestRBACNonMemberIsForbidden: non-members get 403 on every workspace-scoped endpoint.
//
// Covers the first kind of assertion in the requirement. Note that **list endpoints are also 403**,
// not 200 + []: the rule "lists never 404" is about not existing, not about lacking permission.
func TestRBACNonMemberIsForbidden(t *testing.T) {
	h := newRBACServer(t)
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"列 agent", http.MethodGet, "/api/workspaces/" + rbacWS + "/agents"},
		{"列技能", http.MethodGet, "/api/workspaces/" + rbacWS + "/skills"},
		{"列运行历史", http.MethodGet, "/api/workspaces/" + rbacWS + "/runs"},
		{"看用量", http.MethodGet, "/api/workspaces/" + rbacWS + "/usage"},
		{"列定时任务", http.MethodGet, "/api/workspaces/" + rbacWS + "/schedules"},
		{"看方舟凭证", http.MethodGet, "/api/workspaces/" + rbacWS + "/ark-credential"},
		{"读单个 agent", http.MethodGet, "/api/agents/agent_triage"},
		{"读单个技能", http.MethodGet, "/api/skills/skill-20260801-triage"},
		// Read the content of a skill attachment. **This must be in the table**: it outputs file
		// bytes uploaded by users; without the permission declaration a non-member could get the
		// script content in another workspace's skill package.
		{"读技能附件", http.MethodGet, "/api/skills/skill-20260801-triage/file?path=a.md"},
		{"读单条运行", http.MethodGet, "/api/runs/run_1"},
		// Searching people must also be 403 for non-members: otherwise any logged-in user could make
		// up a workspace id and use it as a company-wide directory lookup.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, asRole(tc.method, tc.path, roleNobody, ""))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("状态码 = %d，期望 403，body=%s", rec.Code, rec.Body.String())
			}
			msg, code := envelope(t, rec)
			if code != "access_denied" {
				t.Errorf("code = %q，期望 access_denied", code)
			}
			if msg != authz.MsgNotMember {
				t.Errorf("文案 = %q，期望 %q", msg, authz.MsgNotMember)
			}
		})
	}
}

// TestRBACViewerCannotWrite: viewer can read, but every write is blocked.
//
// Covers the second kind of assertion in the requirement. The message must be "insufficient
// permission for this operation": a viewer is a member, and telling them "you are not a member"
// would make them request to join a workspace they are already in.
func TestRBACViewerCannotWrite(t *testing.T) {
	h := newRBACServer(t)

	// First confirm reading works, otherwise the 403s below might only mean "they cannot even read".
	read := httptest.NewRecorder()
	h.ServeHTTP(read, asRole(http.MethodGet, "/api/workspaces/"+rbacWS+"/agents", roleViewer, ""))
	if read.Code != http.StatusOK {
		t.Fatalf("viewer 读 agent 列表期望 200，实际 %d，body=%s", read.Code, read.Body.String())
	}

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"建 agent", http.MethodPost, "/api/workspaces/" + rbacWS + "/agents", `{"name":"x"}`},
		{"改 agent", http.MethodPatch, "/api/agents/agent_triage", `{"name":"x"}`},
		{"删 agent", http.MethodDelete, "/api/agents/agent_triage", ""},
		{"建技能", http.MethodPost, "/api/workspaces/" + rbacWS + "/skills", `{"name":"x"}`},
		{"改技能", http.MethodPatch, "/api/skills/skill-20260801-triage", `{"name":"x"}`},
		// Editing attachments is the same level as editing the body (write). Viewer is blocked,
		// non-members get 403.
		{"改技能附件", http.MethodPut, "/api/skills/skill-20260801-triage/file",
			`{"path":"a.md","content":"x"}`},
		{"发起对话", http.MethodPost, "/api/workspaces/" + rbacWS + "/conversations", `{"agentId":"agent_triage"}`},
		{"建定时任务", http.MethodPost, "/api/workspaces/" + rbacWS + "/schedules", `{"name":"x"}`},
		{"立刻触发", http.MethodPost, "/api/schedules/sch_x/run-now", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, asRole(tc.method, tc.path, roleViewer, tc.body))
			// The schedule of the run-now request does not exist and is first stopped by resolving
			// as 404; that is also **not passing**, but with a different status code, so here only
			// "not 2xx" is asserted.
			if rec.Code >= 200 && rec.Code < 300 {
				t.Fatalf("viewer 竟然写成功了：状态码 %d，body=%s", rec.Code, rec.Body.String())
			}
			if rec.Code == http.StatusForbidden {
				msg, _ := envelope(t, rec)
				if msg != authz.MsgForbidden {
					t.Errorf("文案 = %q，期望 %q", msg, authz.MsgForbidden)
				}
			}
		})
	}
}

// TestRBACViewerCannotManage: viewer cannot touch members or workspace settings either.
func TestRBACViewerCannotManage(t *testing.T) {
	h := newRBACServer(t)
	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"邀请成员", http.MethodPost, "/api/workspaces/" + rbacWS + "/members", `{"name":"a","email":"a@b.com","role":"member"}`},
		{"改成员角色", http.MethodPatch, "/api/workspaces/" + rbacWS + "/members/m_3", `{"role":"admin"}`},
		{"移除成员", http.MethodDelete, "/api/workspaces/" + rbacWS + "/members/m_3", ""},
		{"改空间设置", http.MethodPatch, "/api/workspaces/" + rbacWS, `{"name":"新名字"}`},
		{"删空间", http.MethodDelete, "/api/workspaces/" + rbacWS, ""},
		{"存方舟凭证", http.MethodPut, "/api/workspaces/" + rbacWS + "/ark-credential", `{"apiKey":"x","region":"cn-beijing"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, asRole(tc.method, tc.path, roleViewer, tc.body))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("状态码 = %d，期望 403，body=%s", rec.Code, rec.Body.String())
			}
			msg, _ := envelope(t, rec)
			if msg != authz.MsgForbidden {
				t.Errorf("文案 = %q，期望 %q", msg, authz.MsgForbidden)
			}
		})
	}
}

// TestRBACMemberCanWriteButNotManage: member is an "operator": can change content, not people or settings.
//
// Covers the third kind of assertion in the requirement (member side).
func TestRBACMemberCanWriteButNotManage(t *testing.T) {
	h := newRBACServer(t)

	// Can create an agent.
	create := httptest.NewRecorder()
	h.ServeHTTP(create, asRole(http.MethodPost, "/api/workspaces/"+rbacWS+"/agents",
		roleMember, `{"name":"member 建的","instructions":"hi","model":{"id":"doubao-seed-2-1-pro-260628","thinking":"enabled","reasoning_effort":"high","service_tier":"default"}}`))
	if create.Code != http.StatusCreated {
		t.Fatalf("member 建 agent 期望 201，实际 %d，body=%s", create.Code, create.Body.String())
	}

	// But cannot touch members or workspace settings.
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"邀请成员", http.MethodPost, "/api/workspaces/" + rbacWS + "/members", `{"name":"a","email":"a@b.com","role":"member"}`},
		{"改空间设置", http.MethodPatch, "/api/workspaces/" + rbacWS, `{"name":"新名字"}`},
		{"删空间", http.MethodDelete, "/api/workspaces/" + rbacWS, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, asRole(tc.method, tc.path, roleMember, tc.body))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("member 竟然能 %s：状态码 %d，body=%s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestRBACAdminAndOwnerCanManage: admin and owner have full permissions.
//
// Covers the third kind of assertion in the requirement (admin/owner side).
func TestRBACAdminAndOwnerCanManage(t *testing.T) {
	for _, actor := range []string{roleAdmin, roleOwner} {
		t.Run(actor, func(t *testing.T) {
			h := newRBACServer(t)

			// Invite a member (Manage)
			invite := httptest.NewRecorder()
			h.ServeHTTP(invite, asRole(http.MethodPost, "/api/workspaces/"+rbacWS+"/members",
				actor, `{"name":"新人","email":"newbie@example.com","role":"viewer"}`))
			if invite.Code != http.StatusCreated {
				t.Fatalf("邀请成员期望 201，实际 %d，body=%s", invite.Code, invite.Body.String())
			}

			// Change workspace settings (Manage)
			patch := httptest.NewRecorder()
			h.ServeHTTP(patch, asRole(http.MethodPatch, "/api/workspaces/"+rbacWS,
				actor, `{"name":"改过的名字"}`))
			if patch.Code != http.StatusOK {
				t.Fatalf("改空间设置期望 200，实际 %d，body=%s", patch.Code, patch.Body.String())
			}

			// Create an agent (Write)
			create := httptest.NewRecorder()
			h.ServeHTTP(create, asRole(http.MethodPost, "/api/workspaces/"+rbacWS+"/agents",
				actor, `{"name":"管理员建的","instructions":"hi","model":{"id":"doubao-seed-2-1-pro-260628","thinking":"enabled","reasoning_effort":"high","service_tier":"default"}}`))
			if create.Code != http.StatusCreated {
				t.Fatalf("建 agent 期望 201，实际 %d，body=%s", create.Code, create.Body.String())
			}

			// Read (Read)
			list := httptest.NewRecorder()
			h.ServeHTTP(list, asRole(http.MethodGet, "/api/workspaces/"+rbacWS+"/agents", actor, ""))
			if list.Code != http.StatusOK {
				t.Fatalf("列 agent 期望 200，实际 %d", list.Code)
			}
		})
	}
}

// TestRBACListWorkspacesIsScoped: the workspace list only returns workspaces you are in.
//
// Covers the fourth kind of assertion in the requirement. This is the only one that "does not block,
// but only shows your own": blocking it would mean new users could never list a workspace and so
// never get into the system.
func TestRBACListWorkspacesIsScoped(t *testing.T) {
	h := newRBACServer(t)

	// Members see the seed workspace.
	mine := httptest.NewRecorder()
	h.ServeHTTP(mine, asRole(http.MethodGet, "/api/workspaces", roleViewer, ""))
	if mine.Code != http.StatusOK {
		t.Fatalf("列空间期望 200，实际 %d", mine.Code)
	}
	var list []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(mine.Body.Bytes(), &list); err != nil {
		t.Fatalf("解响应失败：%v，body=%s", err, mine.Body.String())
	}
	if len(list) != 1 || list[0].ID != rbacWS {
		t.Fatalf("成员应该只看到 %s，实际 %+v", rbacWS, list)
	}

	// Non-members see an empty list (**not 403**, and not everything).
	none := httptest.NewRecorder()
	h.ServeHTTP(none, asRole(http.MethodGet, "/api/workspaces", roleNobody, ""))
	if none.Code != http.StatusOK {
		t.Fatalf("非成员列空间期望 200 + 空列表，实际 %d", none.Code)
	}
	var empty []any
	if err := json.Unmarshal(none.Body.Bytes(), &empty); err != nil {
		t.Fatalf("解响应失败：%v，body=%s", err, none.Body.String())
	}
	if len(empty) != 0 {
		t.Fatalf("非成员竟然看到 %d 个空间：%s", len(empty), none.Body.String())
	}
	// And it must be [], not null: the frontend calls .map() directly.
	if strings.TrimSpace(none.Body.String()) == "null" {
		t.Error("返回了 null，前端会 TypeError；契约要求 []")
	}
}

// TestRBACWorkspaceByKeyHidesOtherSpaces: a non-member looking up a workspace by key must get 404 +
// not_found.
//
// This is the one Guard cannot cover (the workspace comes from a query parameter); authorization
// happens in the Service. code must be not_found; the frontend's nullable() relies on it to turn it
// into null → go back to the workspace list.
func TestRBACWorkspaceByKeyHidesOtherSpaces(t *testing.T) {
	h := newRBACServer(t)

	ok := httptest.NewRecorder()
	h.ServeHTTP(ok, asRole(http.MethodGet, "/api/workspace-by-key?key=growth", roleViewer, ""))
	if ok.Code != http.StatusOK {
		t.Fatalf("成员按 key 查期望 200，实际 %d，body=%s", ok.Code, ok.Body.String())
	}

	denied := httptest.NewRecorder()
	h.ServeHTTP(denied, asRole(http.MethodGet, "/api/workspace-by-key?key=growth", roleNobody, ""))
	if denied.Code != http.StatusNotFound {
		t.Fatalf("非成员按 key 查期望 404，实际 %d，body=%s", denied.Code, denied.Body.String())
	}
	_, code := envelope(t, denied)
	if code != "not_found" {
		t.Errorf("code = %q，期望 not_found——前端靠它转 null 跳回空间列表", code)
	}
}

// TestRBACDeletedResourceIs404: a deleted resource must be 404, not 403.
//
// Returning 403 would turn "this agent was deleted" into "access denied"; the user would ask an
// admin for permission while the agent is long gone. The frontend's six nullable getters also rely
// on code=not_found to turn it into null.
func TestRBACDeletedResourceIs404(t *testing.T) {
	h := newRBACServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodGet, "/api/agents/agent_gone", roleOwner, ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404，body=%s", rec.Code, rec.Body.String())
	}
	_, code := envelope(t, rec)
	if code != "not_found" {
		t.Errorf("code = %q，期望 not_found", code)
	}
}

// TestRBACListRunEventsStillReturnsEmpty: when the run does not exist the event list still returns
// 200 + [].
//
// The only exception path of the rule "list endpoints never 404": when Guard cannot resolve the run
// it must let the request through to the handler, otherwise the frontend's empty list becomes a
// thrown exception.
func TestRBACListRunEventsStillReturnsEmpty(t *testing.T) {
	h := newRBACServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodGet, "/api/runs/run_gone/events", roleOwner, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200 + []，body=%s", rec.Code, rec.Body.String())
	}
}

// TestRBACMethodMismatchIsNotForbidden: requests with the wrong method **must not be swallowed by
// authorization**.
//
// The same constraint as auth.Protect: authorization must happen after routing. In the wrong place,
// a request with the wrong method would be stopped with 403/401 first, and the error does not point
// to the real cause at all.
//
// # Why assert 404 instead of 405
//
// With CoreHandler wired, **405 never appears anywhere on the mux**: core registers a "/api/"
// catch-all (for the frontend's "404 → null"; see the file header of core/handler.go), and the
// standard library's 405 relies on "no other pattern matches this path", while the catch-all matches
// every /api/ path. Measured: the same route returns 404 / 405 on muxes with / without the
// catch-all.
//
// This is **existing behavior, unrelated to authorization**: the two 405 cases in
// httpd/server_test.go use a Server with "only Feishu, no core", so they still see 405. What is
// pinned here is "not 403/401": the status code is not rewritten by the authorization layer.
func TestRBACMethodMismatchIsNotForbidden(t *testing.T) {
	h := newRBACServer(t)
	rec := httptest.NewRecorder()
	// The registration endpoint only accepts POST; hit it with PUT.
	h.ServeHTTP(rec, asRole(http.MethodPut, "/api/workspaces/"+rbacWS+"/feishu/registrations", roleOwner, ""))
	if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
		t.Fatalf("方法不匹配被授权层吃掉了：状态码 %d，body=%s——授权包错位置了", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404（core 的 /api/ 兜底吃掉了 405），body=%s", rec.Code, rec.Body.String())
	}
}
