// Checks "only admin/owner can change integration injection" under **real wiring**.
//
// # Why these must be in httpd and not core
//
// The restriction is implemented by **two layers** together, and neither layer's unit tests can prove
// it holds:
//
//	authz.Guard   blocks PUT /api/agents/{id}/integrations (ActionManage, per route)
//	core.Service  blocks integrationIds in PATCH and POST bodies (per field; Guard cannot judge it)
//
// core's unit tests run by putting an identity into the context, which cannot prove "where the
// identity comes from in a real request"; authz's unit tests use a fake core, which cannot prove the
// patterns in the matrix match the real routes exactly. Only wiring both layers together, hitting
// real routes with real session tokens, proves "it is really blocked". Same as the passage in the
// file header of authz_test.go.
//
// # All three holes must be closed at the same time
//
// Leave any one open and the restriction does not exist (member uses the remaining one), with **no
// error at all**:
//
//	1. PUT  /api/agents/{id}/integrations        new route, blocked by Guard
//	2. PATCH /api/agents/{id}                    old hole: change one field and you are around it
//	3. POST /api/workspaces/{ws}/agents          the easiest to miss: it is not in the "modify" mental model

package httpd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chyroc/ama/server/authz"
)

// In the seed, agent_triage's integrations are ["lark-user"] (core/seed.json).
//
// **This value must match the seed**: the "sending it back unchanged is allowed" case below uses it;
// if it is wrong, that case gets 403 because "the value changed", and the error talks about
// permissions, not about a wrong fixture.
const (
	rbacAgent            = "agent_triage"
	rbacAgentIntegration = "lark-user"
)

// agentIntegrations reads back an agent's current integration list as owner (who can always read it).
//
// Asserting "did it really change" must read it back; do not just look at the write request's status
// code: 403 means it was blocked, but 200 does not mean it was written (the handler might silently
// ignore it).
func agentIntegrations(t *testing.T, h http.Handler, agentID string) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodGet, "/api/agents/"+agentID, roleOwner, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("读 agent 失败：状态码 %d，body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		IntegrationIDs []string `json:"integrationIds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解 agent 失败: %v", err)
	}
	return out.IntegrationIDs
}

// TestIntegrationsPutRequiresManage: member calling the new PUT endpoint gets 403 (the first hole).
//
// It also pins that viewer and non-members cannot get in, and that **admin can**: asserting only
// "member is blocked" would let an implementation that makes the whole route unusable for everyone
// pass.
func TestIntegrationsPutRequiresManage(t *testing.T) {
	h := newRBACServer(t)
	path := "/api/agents/" + rbacAgent + "/integrations"
	body := `{"integrationIds":["ama"]}`

	t.Run("member 被拦", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(http.MethodPut, path, roleMember, body))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("member 竟然改得动授权注入：状态码 %d，body=%s\n"+
				"——这条路由的档位必须是 ActionManage", rec.Code, rec.Body.String())
		}
		msg, code := envelope(t, rec)
		if code != "access_denied" {
			t.Errorf("code = %q，期望 access_denied", code)
		}
		// The message must say "insufficient permission for this operation", not "you are not a
		// member": a member is a member, and saying otherwise would make them request to join a
		// workspace they are already in.
		if msg != authz.MsgForbidden {
			t.Errorf("文案 = %q，期望 %q", msg, authz.MsgForbidden)
		}
		// It really was not written.
		if got := agentIntegrations(t, h, rbacAgent); len(got) != 1 || got[0] != rbacAgentIntegration {
			t.Errorf("被 403 了但值变了：%v——拦截发生在写之后", got)
		}
	})

	t.Run("viewer 被拦", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(http.MethodPut, path, roleViewer, body))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("viewer 状态码 = %d，期望 403，body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("非成员被拦", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(http.MethodPut, path, roleNobody, body))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("非成员状态码 = %d，期望 403，body=%s", rec.Code, rec.Body.String())
		}
	})

	// ⚠️ This one is the **control group** and cannot be skipped: the three above are all green
	// under a wrong implementation where "nobody can use the route". With this one, "blocking too
	// much" gets noticed.
	t.Run("admin 改得动", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(http.MethodPut, path, roleAdmin, `{"integrationIds":["ama","lark-user"]}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("admin 应当改得动：状态码 %d，body=%s", rec.Code, rec.Body.String())
		}
		got := agentIntegrations(t, h, rbacAgent)
		// normalize sorts, so the order is fixed: ama < lark-user.
		if len(got) != 2 || got[0] != "ama" || got[1] != "lark-user" {
			t.Fatalf("admin 改完读回 = %v，期望 [ama lark-user]——回了 200 但没写进去", got)
		}
	})
}

// TestIntegrationsPatchBackdoorIsClosed: member sending integrationIds via PATCH is rejected (the
// second hole).
//
// ⚠️ **This is the key point of the whole change**: splitting out a new route is only half of it;
// without closing the old hole a member can still change it in the body of
// `PATCH /api/agents/{id}`, a bypass with no error.
//
// It also pins "sending it back unchanged is allowed": the frontend's toAgentDraft **always** sends
// back the full draft (including integrationIds), and rejecting unconditionally would stop a member
// from even saving a name change.
func TestIntegrationsPatchBackdoorIsClosed(t *testing.T) {
	h := newRBACServer(t)
	path := "/api/agents/" + rbacAgent

	// Common fields for the rename: model is required (validateAgentDraft); without it the result
	// would be 400 instead of 403, and this case would no longer test permissions.
	const model = `"model":{"id":"doubao-seed-2-1-pro-260628","thinking":"enabled",` +
		`"reasoning_effort":"high","service_tier":"default"}`

	t.Run("member 改这个字段被拒", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(http.MethodPatch, path, roleMember,
			`{"name":"工单分诊助手",`+model+`,"integrationIds":["ama","lark-user"]}`))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("member 从 PATCH 绕过去了：状态码 %d，body=%s\n"+
				"——只把新路由定成 Manage 是不够的，旧口子必须一起堵",
				rec.Code, rec.Body.String())
		}
		if _, code := envelope(t, rec); code != "access_denied" {
			t.Errorf("code = %q，期望 access_denied", code)
		}
		if got := agentIntegrations(t, h, rbacAgent); len(got) != 1 || got[0] != rbacAgentIntegration {
			t.Errorf("被 403 了但值变了：%v", got)
		}
	})

	t.Run("member 清空也被拒", func(t *testing.T) {
		// An empty array is also a "change": removing integrations is also admin's job, otherwise a
		// member could turn off integrations others set up, again with no error (the agent just
		// suddenly loses its credentials).
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(http.MethodPatch, path, roleMember,
			`{"name":"工单分诊助手",`+model+`,"integrationIds":[]}`))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("member 清空授权成功了：状态码 %d，body=%s", rec.Code, rec.Body.String())
		}
	})

	// ⚠️ Control group, **this one cannot be skipped**: the rule is "did the value change", not "was
	// it sent". Without it someone would change the implementation to "reject when the body has
	// this key", which would stop a member from even saving a name change, and that error says
	// "integrations need an admin", which does not point at all to what they actually did.
	t.Run("member 原样回传能保存", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(http.MethodPatch, path, roleMember,
			`{"name":"member 改的名字",`+model+`,"integrationIds":["`+rbacAgentIntegration+`"]}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("member 原样回传却存不下去：状态码 %d，body=%s\n"+
				"——判据必须是「值变没变」，不能是「传没传」（前端总是回传完整草稿）",
				rec.Code, rec.Body.String())
		}
		var out struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if out.Name != "member 改的名字" {
			t.Errorf("名字没改成：%q", out.Name)
		}
	})

	t.Run("admin 从 PATCH 改得动", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(http.MethodPatch, path, roleAdmin,
			`{"name":"admin 改的",`+model+`,"integrationIds":["ama"]}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("admin 应当改得动：状态码 %d，body=%s", rec.Code, rec.Body.String())
		}
		if got := agentIntegrations(t, h, rbacAgent); len(got) != 1 || got[0] != "ama" {
			t.Fatalf("admin 改完读回 = %v，期望 [ama]", got)
		}
	})
}

// TestIntegrationsCreateBackdoorIsClosed: the hole of sending integrationIds when creating an agent
// (the third).
//
// ⚠️ **This is the easiest to miss, because it is not in the "modify" mental model.** POST to create
// an agent is ActionWrite (member can create agents anyway), and the create form can also tick
// integrations; without blocking it, "create a new one" goes around the whole restriction.
//
// # ⚠️⚠️ The rule changed with "personal integrations follow the creator"; do not read it the old way
//
// This used to assert "**member creating an agent with any integration always gets 403**" (only
// admins could configure integrations). That rule has been replaced by the requirement:
//
//	personal integrations   follow only the **creator**. A member ticking **their own** personal
//	                        credential on an agent **they create** is their right: the credential
//	                        is theirs anyway, and once injected it still represents themselves. So
//	                        it is now **allowed**.
//
// The groups below each pin one side of the new rule; missing any group lets a reversed wrong
// implementation stay green.
func TestIntegrationsCreateBackdoorIsClosed(t *testing.T) {
	h := newRBACServer(t)
	path := "/api/workspaces/" + rbacWS + "/agents"
	const model = `"model":{"id":"doubao-seed-2-1-pro-260628","thinking":"enabled",` +
		`"reasoning_effort":"high","service_tier":"default"}`

	// ⚠️ **The group where the rule changed**: member creating an agent with **their own personal
	// integration** is now allowed.
	//
	// Blocking it would be wrong: a member could not use their own credential for their own agent,
	// and the error would say "needs an admin", pointing to something an admin cannot help with (the
	// credential is the person's own).
	t.Run("member 带自己的个人授权建 → 放行（创建者本人）", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(http.MethodPost, path, roleMember,
			`{"name":"带个人授权的新 agent","system":"hi",`+model+`,"integrationIds":["lark-user"]}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("member 给自己新建的 agent 勾自己的个人授权应当放行：状态码 %d，body=%s\n"+
				"——那把凭据本来就是他的，拦住等于让他用不了自己的授权",
				rec.Code, rec.Body.String())
		}
	})

	// ⚠️ Control group: member **creating an agent without integrations must still work**. Without
	// it, an implementation that raises the whole POST to Manage would also pass, which is a
	// regression (members should be able to create agents).
	t.Run("member 不带授权照常能建", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(http.MethodPost, path, roleMember,
			`{"name":"普通新 agent","system":"hi",`+model+`,"integrationIds":[]}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("member 建普通 agent 应当能成：状态码 %d，body=%s\n"+
				"——空列表是新建表单的默认值，不能因为请求体里有这个键就拒",
				rec.Code, rec.Body.String())
		}
	})

	t.Run("admin 带授权能建", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(http.MethodPost, path, roleAdmin,
			`{"name":"admin 建的","system":"hi",`+model+`,"integrationIds":["lark-user"]}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("admin 应当能建：状态码 %d，body=%s", rec.Code, rec.Body.String())
		}
		var out struct {
			ID             string   `json:"id"`
			IntegrationIDs []string `json:"integrationIds"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if len(out.IntegrationIDs) != 1 || out.IntegrationIDs[0] != "lark-user" {
			t.Fatalf("admin 建出来的 agent 授权 = %v，期望 [lark-user]——回了 201 但字段被吃掉了",
				out.IntegrationIDs)
		}
	})
}

// TestMyRoleReflectsRealRole: the role returned by GET my-role uses the same rules as permission
// checks.
//
// The frontend relies on it to decide "draw the integrations block as editable or read-only", so if
// it is wrong the UI is wrong. **canManage is computed by the server**: if the frontend compared
// strings itself, a mismatch with authz.levelOf would give no error (owner and admin being the
// same level is the easiest thing to miss).
func TestMyRoleReflectsRealRole(t *testing.T) {
	h := newRBACServer(t)
	cases := []struct {
		email     string
		role      string
		canManage bool
	}{
		{roleOwner, "owner", true},
		// ⚠️ admin must be exactly the same as owner: when drawing the table it is easiest to give
		// owner a few extra ticks.
		{roleAdmin, "admin", true},
		{roleMember, "member", false},
		{roleViewer, "viewer", false},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, asRole(http.MethodGet,
				"/api/workspaces/"+rbacWS+"/my-role", tc.email, ""))
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200，body=%s", rec.Code, rec.Body.String())
			}
			var out struct {
				Role      string `json:"role"`
				CanManage bool   `json:"canManage"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("解响应失败: %v", err)
			}
			if out.Role != tc.role {
				t.Errorf("role = %q，期望 %q", out.Role, tc.role)
			}
			if out.CanManage != tc.canManage {
				t.Errorf("canManage = %v，期望 %v——界面会照这个画",
					out.CanManage, tc.canManage)
			}
		})
	}

	// A non-member should not even be able to ask (ActionRead already blocks it).
	t.Run("非成员 403", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(http.MethodGet,
			"/api/workspaces/"+rbacWS+"/my-role", roleNobody, ""))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("状态码 = %d，期望 403，body=%s", rec.Code, rec.Body.String())
		}
	})
}
