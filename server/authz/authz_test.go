// Unit tests for authz: the role matrix, letter case, and those two guardrails.
//
// The most important assertions here are not the "viewer cannot write" ones (those are obvious), but
// **the two-way completeness of the route table** and **an unresolvable resource returns 404, not 403**: breaking either reports no error:
// the former lets new routes allow silently, the latter calls "the resource was deleted" "you have no permission".

package authz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
)

// fakeMembership is a programmable membership.
type fakeMembership struct {
	// roles is workspaceID|actor → role. Not in the table means not a member.
	roles map[string]apimodel.WorkspaceRole
	// owners is resource kind|id → workspaceID. Not in the table means the resource does not exist.
	owners map[string]string
	// When roleErr / ownerErr are non-nil the matching method errors directly, to test "database jitter is 500, not 403".
	roleErr  error
	ownerErr error
}

func (f *fakeMembership) RoleOf(
	_ context.Context, workspaceID, actor string,
) (apimodel.WorkspaceRole, bool, error) {
	if f.roleErr != nil {
		return "", false, f.roleErr
	}
	role, ok := f.roles[workspaceID+"|"+strings.ToLower(actor)]
	return role, ok, nil
}

func (f *fakeMembership) WorkspaceOf(_ context.Context, kind ResourceKind, id string) (string, error) {
	if f.ownerErr != nil {
		return "", f.ownerErr
	}
	ws, ok := f.owners[fmt.Sprintf("%d|%s", kind, id)]
	if !ok {
		return "", fmt.Errorf("%w: 没这个东西", ErrResourceNotFound)
	}
	return ws, nil
}

const (
	testWS     = "ws_demo"
	ownerMail  = "owner@example.com"
	adminMail  = "admin@example.com"
	memberMail = "member@example.com"
	viewerMail = "viewer@example.com"
	strangerML = "stranger@example.com"
)

// newFake builds a membership with "one of each of the four roles", the same shape as the seed workspace.
func newFake() *fakeMembership {
	return &fakeMembership{
		roles: map[string]apimodel.WorkspaceRole{
			testWS + "|" + ownerMail:  apimodel.RoleOwner,
			testWS + "|" + adminMail:  apimodel.RoleAdmin,
			testWS + "|" + memberMail: apimodel.RoleMember,
			testWS + "|" + viewerMail: apimodel.RoleViewer,
		},
		owners: map[string]string{
			fmt.Sprintf("%d|agent_1", KindAgent):  testWS,
			fmt.Sprintf("%d|skill_1", KindSkill):  testWS,
			fmt.Sprintf("%d|run_1", KindRun):      testWS,
			fmt.Sprintf("%d|sch_1", KindSchedule): testWS,
			fmt.Sprintf("%d|fs_1", KindFeishuApp): testWS,
			// A resource belonging to another workspace, to test cross-workspace privilege escalation.
			fmt.Sprintf("%d|agent_other", KindAgent): "ws_other",
			// A run in another workspace (along with its container), to test "people in workspace A cannot get files from workspace B's container".
			fmt.Sprintf("%d|run_other", KindRun): "ws_other",
		},
	}
}

// TestLevelOfRoles: the role → level mapping. owner and admin must be the same level.
func TestLevelOfRoles(t *testing.T) {
	cases := []struct {
		role apimodel.WorkspaceRole
		want Level
	}{
		{apimodel.RoleOwner, LevelAdmin},
		{apimodel.RoleAdmin, LevelAdmin},
		{apimodel.RoleMember, LevelMember},
		{apimodel.RoleViewer, LevelViewer},
		// A misspelled role in the database **must get the lowest permission** (fail-closed),
		// not be treated as some "close enough" level like member.
		{apimodel.WorkspaceRole("Owner"), LevelNone},
		{apimodel.WorkspaceRole(""), LevelNone},
		{apimodel.WorkspaceRole("god"), LevelNone},
	}
	for _, tc := range cases {
		t.Run(string(tc.role), func(t *testing.T) {
			if got := levelOf(tc.role); got != tc.want {
				t.Errorf("levelOf(%q) = %d，期望 %d", tc.role, got, tc.want)
			}
		})
	}
}

// TestActionLadder: what level each of the three action levels requires, and the transitivity of viewer<member<admin.
func TestActionLadder(t *testing.T) {
	if requiredLevel(ActionRead) != LevelViewer {
		t.Error("Read 应该 viewer 起")
	}
	if requiredLevel(ActionWrite) != LevelMember {
		t.Error("Write 应该 member 起")
	}
	if requiredLevel(ActionManage) != LevelAdmin {
		t.Error("Manage 应该 admin 起")
	}
	// A higher level must cover the actions of lower levels: member can Read, admin can Write.
	if levelOf(apimodel.RoleMember) < requiredLevel(ActionRead) {
		t.Error("member 应该能读")
	}
	if levelOf(apimodel.RoleAdmin) < requiredLevel(ActionWrite) {
		t.Error("admin 应该能写")
	}
	// Not the other way around.
	if levelOf(apimodel.RoleViewer) >= requiredLevel(ActionWrite) {
		t.Error("viewer 不该能写")
	}
	if levelOf(apimodel.RoleMember) >= requiredLevel(ActionManage) {
		t.Error("member 不该能管理")
	}
}

// serve wraps one route with Guard, calls it once as some identity, and returns the response.
func serve(t *testing.T, fake *fakeMembership, pattern, method, target, actor string) *httptest.ResponseRecorder {
	t.Helper()
	g, err := New(Config{Membership: fake})
	if err != nil {
		t.Fatalf("建 Guard 失败: %v", err)
	}
	mux := http.NewServeMux()
	g.Wrap(muxAdapter{mux}).HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	req := httptest.NewRequest(method, target, nil)
	if actor != "" {
		req = req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{Email: actor}))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// muxAdapter exposes *http.ServeMux as the Mux interface.
type muxAdapter struct{ mux *http.ServeMux }

func (m muxAdapter) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.mux.HandleFunc(pattern, h)
}

// bodyCode gets the code from the response envelope.
func bodyCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON：%s", rec.Body.String())
	}
	return out.Code
}

func bodyMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Message
}

// TestNonMemberIsRejected: non-members accessing workspace-level endpoints always get 403, and the message must say clearly "you are not in this workspace".
func TestNonMemberIsRejected(t *testing.T) {
	rec := serve(t, newFake(),
		"GET /api/workspaces/{workspaceId}/agents",
		http.MethodGet, "/api/workspaces/"+testWS+"/agents", strangerML)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d，期望 403，body=%s", rec.Code, rec.Body.String())
	}
	if code := bodyCode(t, rec); code != "access_denied" {
		t.Errorf("code = %q，期望 access_denied", code)
	}
	if msg := bodyMessage(t, rec); msg != MsgNotMember {
		t.Errorf("文案 = %q，期望 %q", msg, MsgNotMember)
	}
}

// TestViewerCannotWrite: viewer can read but not write; this is the most relied-on rule of the whole matrix.
func TestViewerCannotWrite(t *testing.T) {
	fake := newFake()

	read := serve(t, fake, "GET /api/workspaces/{workspaceId}/agents",
		http.MethodGet, "/api/workspaces/"+testWS+"/agents", viewerMail)
	if read.Code != http.StatusOK {
		t.Errorf("viewer 读期望 200，实际 %d", read.Code)
	}

	write := serve(t, fake, "POST /api/workspaces/{workspaceId}/agents",
		http.MethodPost, "/api/workspaces/"+testWS+"/agents", viewerMail)
	if write.Code != http.StatusForbidden {
		t.Fatalf("viewer 写期望 403，实际 %d", write.Code)
	}
	// **The message must be "insufficient permission", not "you are not a member"**: viewer is a member,
	// and telling them "you are not a member" would make them apply to join a workspace they are already in.
	if msg := bodyMessage(t, write); msg != MsgForbidden {
		t.Errorf("文案 = %q，期望 %q", msg, MsgForbidden)
	}
}

// TestMemberAndAdminPass: member can write but not manage; admin/owner pass everything.
func TestMemberAndAdminPass(t *testing.T) {
	fake := newFake()
	cases := []struct {
		name    string
		pattern string
		method  string
		target  string
		actor   string
		want    int
	}{
		{"member 发起对话", "POST /api/workspaces/{workspaceId}/conversations",
			http.MethodPost, "/api/workspaces/" + testWS + "/conversations", memberMail, http.StatusOK},
		{"member 建 agent", "POST /api/workspaces/{workspaceId}/agents",
			http.MethodPost, "/api/workspaces/" + testWS + "/agents", memberMail, http.StatusOK},
		// Member management is Manage: member can change agents, but not people.
		{"member 加成员被拒", "POST /api/workspaces/{workspaceId}/members",
			http.MethodPost, "/api/workspaces/" + testWS + "/members", memberMail, http.StatusForbidden},
		{"member 改空间设置被拒", "PATCH /api/workspaces/{workspaceId}",
			http.MethodPatch, "/api/workspaces/" + testWS, memberMail, http.StatusForbidden},
		{"admin 加成员", "POST /api/workspaces/{workspaceId}/members",
			http.MethodPost, "/api/workspaces/" + testWS + "/members", adminMail, http.StatusOK},
		{"admin 存方舟凭证", "PUT /api/workspaces/{workspaceId}/ark-credential",
			http.MethodPut, "/api/workspaces/" + testWS + "/ark-credential", adminMail, http.StatusOK},
		{"owner 删空间", "DELETE /api/workspaces/{workspaceId}",
			http.MethodDelete, "/api/workspaces/" + testWS, ownerMail, http.StatusOK},
		// viewer can see the credential (only the mask is returned), but cannot change it.
		{"viewer 看凭证", "GET /api/workspaces/{workspaceId}/ark-credential",
			http.MethodGet, "/api/workspaces/" + testWS + "/ark-credential", viewerMail, http.StatusOK},
		{"viewer 改凭证被拒", "PUT /api/workspaces/{workspaceId}/ark-credential",
			http.MethodPut, "/api/workspaces/" + testWS + "/ark-credential", viewerMail, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(t, fake, tc.pattern, tc.method, tc.target, tc.actor)
			if rec.Code != tc.want {
				t.Fatalf("状态码 = %d，期望 %d，body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestEmailCaseInsensitive: member emails are typed in by admins and may contain uppercase; identities are lowercased.
//
// A case-sensitive comparison would make that person **never find their own role**: it shows as "I am clearly a member,
// but it says I am not", and only for a few people.
func TestEmailCaseInsensitive(t *testing.T) {
	fake := &fakeMembership{
		// Note fakeMembership.RoleOf does ToLower itself; what this tests is that the actor Guard passes down
		// is not damaged, and both spellings with different case match.
		roles: map[string]apimodel.WorkspaceRole{
			testWS + "|bob@example.com": apimodel.RoleAdmin,
		},
	}
	for _, actor := range []string{"bob@example.com", "Bob@example.com", "BOB@EXAMPLE.COM"} {
		t.Run(actor, func(t *testing.T) {
			rec := serve(t, fake, "POST /api/workspaces/{workspaceId}/members",
				http.MethodPost, "/api/workspaces/"+testWS+"/members", actor)
			if rec.Code != http.StatusOK {
				t.Errorf("%q 期望 200，实际 %d", actor, rec.Code)
			}
		})
	}
}

// TestFlatPathResolvesWorkspace: flat paths must resolve ownership by resource id, and cross-workspace access must be blocked.
func TestFlatPathResolvesWorkspace(t *testing.T) {
	fake := newFake()

	ok := serve(t, fake, "GET /api/agents/{id}", http.MethodGet, "/api/agents/agent_1", viewerMail)
	if ok.Code != http.StatusOK {
		t.Errorf("本空间的 agent 期望 200，实际 %d", ok.Code)
	}

	// agent_other belongs to ws_other, and viewerMail is not a member of that workspace → 403.
	// This is the "reading resources of someone else's workspace" case, the one flat paths most easily miss.
	cross := serve(t, fake, "GET /api/agents/{id}", http.MethodGet, "/api/agents/agent_other", viewerMail)
	if cross.Code != http.StatusForbidden {
		t.Fatalf("跨空间读期望 403，实际 %d，body=%s", cross.Code, cross.Body.String())
	}
}

// TestMissingResourceIsNotFound: an unresolvable resource must give 404 + code=not_found, **not 403**.
//
// Six nullable getters in the frontend rely on code == "not_found" to turn the response into null. Returning 403 would turn
// "this agent has been deleted" into "no access": the user would go ask an admin for permission,
// while that agent is long gone.
func TestMissingResourceIsNotFound(t *testing.T) {
	rec := serve(t, newFake(), "GET /api/agents/{id}",
		http.MethodGet, "/api/agents/agent_gone", ownerMail)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404，body=%s", rec.Code, rec.Body.String())
	}
	if code := bodyCode(t, rec); code != "not_found" {
		t.Errorf("code = %q，期望 not_found——前端靠它转 null", code)
	}
}

// TestListRunEventsAllowsMissingRun: the "list endpoints never 404" exception.
//
// When the run does not exist Guard must let it through to the handler, which returns 200 + []. If Guard jumped in with 404
// it would turn the frontend's empty list into a thrown exception.
func TestListRunEventsAllowsMissingRun(t *testing.T) {
	rec := serve(t, newFake(), "GET /api/runs/{runId}/events",
		http.MethodGet, "/api/runs/run_gone/events", ownerMail)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（放行给 handler），body=%s", rec.Code, rec.Body.String())
	}
}

// TestStoreErrorIsInternal: a database lookup failure must be 500, not described as 403.
//
// Saying "you are not a member of this workspace" would make the user go ask an admin for permission, while the admin sees they are clearly the owner:
// a false statement neither the user nor the admin can deal with.
func TestStoreErrorIsInternal(t *testing.T) {
	fake := newFake()
	fake.roleErr = errors.New("库连不上")

	rec := serve(t, fake, "GET /api/workspaces/{workspaceId}/agents",
		http.MethodGet, "/api/workspaces/"+testWS+"/agents", ownerMail)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500，body=%s", rec.Code, rec.Body.String())
	}
}

// TestExemptRoutesSkipAuthz: global and personal endpoints get no workspace authorization.
//
// They still require login (guaranteed by auth.Protect), but must not be blocked by membership:
// if blocked, "list my workspaces" would become chicken-and-egg: with no workspace you could never list workspaces.
func TestExemptRoutesSkipAuthz(t *testing.T) {
	fake := &fakeMembership{} // empty table: nobody is a member of any workspace
	cases := []struct {
		pattern string
		method  string
		target  string
	}{
		{"GET /api/workspaces", http.MethodGet, "/api/workspaces"},
		{"POST /api/workspaces", http.MethodPost, "/api/workspaces"},
		{"GET /api/workspace-by-key", http.MethodGet, "/api/workspace-by-key?key=growth"},
		{"GET /api/workspace-key-taken", http.MethodGet, "/api/workspace-key-taken?key=x"},
		// The four authorization management routes: account-level, unrelated to workspaces. **Non-members must get through too**:
		// a newcomer who has not joined any workspace needs to authorize here first more than anyone,
		// and requiring membership would lock them out. See that comment in matrix.go.
		{"GET /api/integrations", http.MethodGet, "/api/integrations"},
		{"GET /api/integrations/{provider}", http.MethodGet, "/api/integrations/ama"},
		{"POST /api/integrations/{provider}/authorize", http.MethodPost, "/api/integrations/ama/authorize"},
		{"DELETE /api/integrations/{provider}", http.MethodDelete, "/api/integrations/ama"},
	}
	for _, tc := range cases {
		t.Run(tc.pattern, func(t *testing.T) {
			rec := serve(t, fake, tc.pattern, tc.method, tc.target, strangerML)
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200（豁免），body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestUndeclaredRoutePanics: registering a route without declared permissions must panic.
//
// **This is the most important guardrail of this package.** Without the panic, new routes would allow silently: all tests pass,
// the endpoint responds as usual, only any logged-in user can call it. This repo once had an incident of "a purely new module never wired into the main path",
// the same kind of problem.
func TestUndeclaredRoutePanics(t *testing.T) {
	g, err := New(Config{Membership: newFake()})
	if err != nil {
		t.Fatalf("建 Guard 失败: %v", err)
	}
	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("注册未声明权限的路由竟然没 panic——新增路由会静默放行")
		}
		if !strings.Contains(fmt.Sprint(v), "没有声明权限") {
			t.Errorf("panic 信息看不出原因：%v", v)
		}
	}()
	g.Wrap(muxAdapter{http.NewServeMux()}).HandleFunc(
		"GET /api/brand-new-thing", func(http.ResponseWriter, *http.Request) {})
}

// TestVerifyCatchesDeclaredButUnregistered: the guardrail in the opposite direction: declared but not registered.
//
// This happens when a pattern in matrix.go has a typo (workspaceld vs workspaceId):
// that declaration never matches any request, and the matching real route panics for "not declared":
// the error points to that real route, making people think a declaration is missing, while actually the declaration has a typo.
// Checking only one direction cannot catch this.
func TestVerifyCatchesDeclaredButUnregistered(t *testing.T) {
	g, err := New(Config{Membership: newFake()})
	if err != nil {
		t.Fatalf("建 Guard 失败: %v", err)
	}
	// Register nothing; Verify should report all non-optional routes.
	err = g.Verify()
	if err == nil {
		t.Fatal("一条路由都没注册，Verify 竟然通过了")
	}
	if !strings.Contains(err.Error(), "/api/workspaces/{workspaceId}/agents") {
		t.Errorf("错误信息里没点名漏了哪条：%v", err)
	}
}

// TestVerifyPassesWhenAllRegistered: Verify must pass once all declared routes are registered.
//
// It also proves: the two _debug ones **do not count as missing when not registered** (they are only registered when DevRoutes is on,
// which is off in production; without the exemption production startup would panic for "declared but not registered").
func TestVerifyPassesWhenAllRegistered(t *testing.T) {
	g, err := New(Config{Membership: newFake()})
	if err != nil {
		t.Fatalf("建 Guard 失败: %v", err)
	}
	wrapped := g.Wrap(muxAdapter{http.NewServeMux()})
	for pattern := range routePermissions {
		if optionalRoutes[pattern] {
			continue // deliberately skipped, to test that they really are exempt
		}
		wrapped.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
	}
	if err := g.Verify(); err != nil {
		t.Fatalf("全部注册后 Verify 仍报错：%v", err)
	}
}

// TestMissingIdentityIsUnauthorized: Guard not getting an identity means the wiring is wrong (wrapped outside authentication).
//
// Then it must reply 401 to make it visible, instead of 403 as a non-member: the latter would disguise a wiring bug
// as a normal permission denial.
func TestMissingIdentityIsUnauthorized(t *testing.T) {
	rec := serve(t, newFake(), "GET /api/workspaces/{workspaceId}/agents",
		http.MethodGet, "/api/workspaces/"+testWS+"/agents", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401", rec.Code)
	}
}

// TestGuardPreserves405: with authorization wrapped on, requests with the wrong method still get 405.
//
// This is the same constraint as auth.Protect: authorization must happen **after routing**. If wrapped outside the mux,
// a GET on an endpoint that only accepts POST would get 403 instead of 405, and the error would not point to the real cause at all.
func TestGuardPreserves405(t *testing.T) {
	g, err := New(Config{Membership: newFake()})
	if err != nil {
		t.Fatalf("建 Guard 失败: %v", err)
	}
	mux := http.NewServeMux()
	g.Wrap(muxAdapter{mux}).HandleFunc("POST /api/workspaces/{workspaceId}/agents",
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	rec := httptest.NewRecorder()
	// Even without any identity it should still be 405: routing comes before everything.
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/workspaces/"+testWS+"/agents", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d，期望 405——授权包错位置会让它变成 403/401", rec.Code)
	}
}

// TestFeishuAuthorizerMatchesMatrix: the two Feishu methods must use the same checks as the matrix.
//
// If the two layers disagree you get "Guard allows, Service rejects", and the error cannot point to the real cause.
func TestFeishuAuthorizerMatchesMatrix(t *testing.T) {
	a := NewFeishuAuthorizer(newFake())
	ctx := context.Background()

	cases := []struct {
		name   string
		actor  string
		access bool
		manage bool
	}{
		{"owner", ownerMail, true, true},
		{"admin", adminMail, true, true},
		// member can see the Feishu app list, but cannot connect a new app (that binds an external identity to the workspace).
		{"member", memberMail, true, false},
		{"viewer", viewerMail, true, false},
		{"非成员", strangerML, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := a.CanAccessWorkspace(ctx, tc.actor, testWS)
			if err != nil {
				t.Fatalf("CanAccessWorkspace 报错: %v", err)
			}
			if got != tc.access {
				t.Errorf("CanAccessWorkspace = %v，期望 %v", got, tc.access)
			}
			got, err = a.CanManageWorkspace(ctx, tc.actor, testWS)
			if err != nil {
				t.Fatalf("CanManageWorkspace 报错: %v", err)
			}
			if got != tc.manage {
				t.Errorf("CanManageWorkspace = %v，期望 %v", got, tc.manage)
			}
		})
	}
}

// TestFeishuAuthorizerRejectsEmpty: an empty identity or empty workspace is always rejected (fail-closed).
//
// The old AllowAllAuthorizer allowed as soon as both were non-empty; now non-empty is only a precondition.
func TestFeishuAuthorizerRejectsEmpty(t *testing.T) {
	a := NewFeishuAuthorizer(newFake())
	ctx := context.Background()
	for _, tc := range []struct{ actor, ws string }{
		{"", testWS}, {ownerMail, ""}, {"   ", testWS}, {"", ""},
	} {
		ok, err := a.CanAccessWorkspace(ctx, tc.actor, tc.ws)
		if err != nil || ok {
			t.Errorf("actor=%q ws=%q 期望拒绝，实际 ok=%v err=%v", tc.actor, tc.ws, ok, err)
		}
	}
}

// TestFeishuAuthorizerPropagatesError: a database lookup failure must be returned upward, not treated as "no permission".
//
// Returning false would make feishu's requireManage turn it into 403, so database jitter would show as
// "you have no permission", the user would go to an admin, and the admin would see they are clearly the owner.
func TestFeishuAuthorizerPropagatesError(t *testing.T) {
	fake := newFake()
	fake.roleErr = errors.New("库连不上")
	a := NewFeishuAuthorizer(fake)

	if _, err := a.CanAccessWorkspace(context.Background(), ownerMail, testWS); err == nil {
		t.Fatal("查库失败竟然没报错——那会让库故障表现成 403")
	}
}

// -- Session container (execution container) -----------------------------------------------------

// TestSandboxCrossWorkspaceDenied is the **privilege escalation baseline** of this group:
// people in workspace A cannot see files in workspace B's container.
//
// The container holds the agent's whole workspace: code, data, intermediate outputs written by the agent. Seeing it across workspaces
// is the same as reading files across workspaces, and this leak **reports no error at all** (the endpoint honestly returns 200 + the file list).
//
// The three routes are tested together: miss any one, and that one is a backdoor.
func TestSandboxCrossWorkspaceDenied(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		method  string
		target  string
	}{
		{"看状态", "GET /api/runs/{runId}/sandbox",
			http.MethodGet, "/api/runs/run_other/sandbox"},
		{"列文件", "GET /api/runs/{runId}/sandbox/files",
			http.MethodGet, "/api/runs/run_other/sandbox/files?path=/"},
		{"恢复容器", "POST /api/runs/{runId}/sandbox/resume",
			http.MethodPost, "/api/runs/run_other/sandbox/resume"},
		{"webshell", "POST /api/runs/{runId}/sandbox/shell",
			http.MethodPost, "/api/runs/run_other/sandbox/shell"},
		{"终端 WS", "GET /api/runs/{runId}/sandbox/terminal",
			http.MethodGet, "/api/runs/run_other/sandbox/terminal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// owner identity: the most permission in **its own** workspace, but this run belongs to ws_other.
			// Using owner instead of viewer is deliberate: it proves what is blocked is "cross-workspace",
			// not something that happened to be blocked by the role level.
			rec := serve(t, newFake(), tc.pattern, tc.method, tc.target, ownerMail)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("状态码 = %d，期望 403——A 空间的 owner 不该看到 B 空间的容器，"+
					"body=%s", rec.Code, rec.Body.String())
			}
			if msg := bodyMessage(t, rec); msg != MsgNotMember {
				t.Errorf("文案 = %q，期望 %q", msg, MsgNotMember)
			}
		})
	}
}

// TestSandboxRoleGates pins the level of each route.
//
// Viewing state and listing files are Read (viewer and up); resuming the container is Write (member and up).
//
// Why resume cannot be given to viewer: it brings the container up again, taking a slot of the **global** concurrency quota
// (capped at 20, shared by the pool and real sessions). Clicking repeatedly could use up the quota, and then other people's chats
// could not create containers and agents would get stuck in requires_action, a fault that does not point to "someone is clicking resume" at all.
func TestSandboxRoleGates(t *testing.T) {
	cases := []struct {
		name     string
		pattern  string
		method   string
		target   string
		actor    string
		wantCode int
	}{
		{"viewer 能看状态", "GET /api/runs/{runId}/sandbox",
			http.MethodGet, "/api/runs/run_1/sandbox", viewerMail, http.StatusOK},
		{"viewer 能列文件", "GET /api/runs/{runId}/sandbox/files",
			http.MethodGet, "/api/runs/run_1/sandbox/files?path=/", viewerMail, http.StatusOK},
		{"viewer 不能恢复容器", "POST /api/runs/{runId}/sandbox/resume",
			http.MethodPost, "/api/runs/run_1/sandbox/resume", viewerMail, http.StatusForbidden},
		{"member 能恢复容器", "POST /api/runs/{runId}/sandbox/resume",
			http.MethodPost, "/api/runs/run_1/sandbox/resume", memberMail, http.StatusOK},
		{"非成员一律拒", "GET /api/runs/{runId}/sandbox",
			http.MethodGet, "/api/runs/run_1/sandbox", strangerML, http.StatusForbidden},

		// The webshell threshold. ⚠️ **member must be blocked too**: this is its biggest difference from the others.
		// The container holds all of the agent's outputs and the credentials injected by authorization; getting into the shell means reading and writing them,
		// which is acting with the owner's identity. That is identity borrowing, not "a bit more permission".
		{"viewer 进不了 webshell", "POST /api/runs/{runId}/sandbox/shell",
			http.MethodPost, "/api/runs/run_1/sandbox/shell", viewerMail, http.StatusForbidden},
		{"member 也进不了 webshell", "POST /api/runs/{runId}/sandbox/shell",
			http.MethodPost, "/api/runs/run_1/sandbox/shell", memberMail, http.StatusForbidden},
		{"admin 能进 webshell", "POST /api/runs/{runId}/sandbox/shell",
			http.MethodPost, "/api/runs/run_1/sandbox/shell", adminMail, http.StatusOK},
		{"owner 能进 webshell", "POST /api/runs/{runId}/sandbox/shell",
			http.MethodPost, "/api/runs/run_1/sandbox/shell", ownerMail, http.StatusOK},

		// ⚠️ The terminal is a **GET** (a WebSocket handshake is a GET with the Upgrade header), but the ability it gives
		// is exactly the same as POST shell. **The easiest mistake is "GET is usually a read" → make it Read**,
		// which would let any viewer run arbitrary commands in the container. These three are here to pin that down.
		{"viewer 进不了终端", "GET /api/runs/{runId}/sandbox/terminal",
			http.MethodGet, "/api/runs/run_1/sandbox/terminal", viewerMail, http.StatusForbidden},
		{"member 也进不了终端", "GET /api/runs/{runId}/sandbox/terminal",
			http.MethodGet, "/api/runs/run_1/sandbox/terminal", memberMail, http.StatusForbidden},
		{"admin 能进终端", "GET /api/runs/{runId}/sandbox/terminal",
			http.MethodGet, "/api/runs/run_1/sandbox/terminal", adminMail, http.StatusOK},

		// Upstream (keystrokes) is at the same level as downstream. **Setting it lower would bypass the downstream gate**:
		// downstream alone only lets you watch; being able to type is what really runs commands.
		{"viewer 不能往终端写", "POST /api/runs/{runId}/sandbox/terminal/input",
			http.MethodPost, "/api/runs/run_1/sandbox/terminal/input", viewerMail, http.StatusForbidden},
		{"member 也不能往终端写", "POST /api/runs/{runId}/sandbox/terminal/input",
			http.MethodPost, "/api/runs/run_1/sandbox/terminal/input", memberMail, http.StatusForbidden},
		{"admin 能往终端写", "POST /api/runs/{runId}/sandbox/terminal/input",
			http.MethodPost, "/api/runs/run_1/sandbox/terminal/input", adminMail, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(t, newFake(), tc.pattern, tc.method, tc.target, tc.actor)
			if rec.Code != tc.wantCode {
				t.Errorf("状态码 = %d，期望 %d，body=%s",
					rec.Code, tc.wantCode, rec.Body.String())
			}
		})
	}
}

// serveCanceled has the same shape as serve, but the request's ctx is already cancelled before entering Guard:
// it simulates the middleware reaching the database lookup after "the user closed the page / SSE disconnected".
func serveCanceled(
	t *testing.T, fake *fakeMembership, pattern, method, target, actor string,
) *httptest.ResponseRecorder {
	t.Helper()
	g, err := New(Config{Membership: fake})
	if err != nil {
		t.Fatalf("建 Guard 失败: %v", err)
	}
	mux := http.NewServeMux()
	g.Wrap(muxAdapter{mux}).HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ctx, cancel := context.WithCancel(context.Background())
	if actor != "" {
		ctx = auth.WithIdentity(ctx, auth.Identity{Email: actor})
	}
	cancel() // the connection is already gone

	req := httptest.NewRequest(method, target, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestClientDisconnectIsNotInternalError: a client disconnect must not be treated as a server failure.
//
// This was the second problem found while investigating a production issue (the first was a 42703 during a rolling deploy):
// the SSE route drops whenever a user closes the page, the database lookup is cancelled with it, and Guard treated that as a database failure and returned 500.
// The result was the frontend **automatically reporting a production problem**: "the user closed a page" became a 5xx alert.
//
// ⚠️ The check is "not 500" instead of "equals a specific status code": with the connection gone, nothing written reaches
// the other end; what matters is **not entering 5xx stats and not triggering a frontend report**.
func TestClientDisconnectIsNotInternalError(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		target  string
		setup   func(*fakeMembership)
	}{
		{
			// Workspace-level route: the workspace comes straight from the path, and the failure happens in RoleOf.
			name:    "查成员时断开",
			pattern: "GET /api/workspaces/{workspaceId}/schedules",
			target:  "/api/workspaces/" + testWS + "/schedules",
			setup:   func(f *fakeMembership) { f.roleErr = context.Canceled },
		},
		{
			// Resource-level route: the owning workspace must be resolved first, and the failure happens in WorkspaceOf.
			// The production route /api/runs/{runId}/events/stream takes exactly this branch.
			name:    "反解归属空间时断开",
			pattern: "GET /api/runs/{runId}/events/stream",
			target:  "/api/runs/sesn-20260101000009-ex009/events/stream",
			setup:   func(f *fakeMembership) { f.ownerErr = context.Canceled },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFake()
			tc.setup(fake)

			rec := serveCanceled(t, fake, tc.pattern, http.MethodGet, tc.target, ownerMail)

			if rec.Code == http.StatusInternalServerError {
				t.Fatalf("客户端断开被当成了 500（会触发前端上报），body=%s", rec.Body.String())
			}
			if rec.Body.Len() != 0 {
				t.Errorf("连接已断还写了响应体：%s", rec.Body.String())
			}
		})
	}
}

// TestRealStoreErrorStillInternal: real failures must still be 500.
//
// Paired with the one above: adding only "a disconnect is not a failure" can easily let **all** RoleOf failures through by accident,
// and then a really broken database would also be let through silently (for read endpoints that is privilege escalation). This pins the other side.
func TestRealStoreErrorStillInternal(t *testing.T) {
	fake := newFake()
	fake.roleErr = errors.New("库连不上")

	// Note this uses serve (ctx not cancelled); the only difference from serveCanceled is this one variable.
	rec := serve(t, fake, "GET /api/workspaces/{workspaceId}/schedules",
		http.MethodGet, "/api/workspaces/"+testWS+"/schedules", ownerMail)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", rec.Code)
	}
	if got := bodyCode(t, rec); got != "internal" {
		t.Errorf("code = %q，期望 internal", got)
	}
}
