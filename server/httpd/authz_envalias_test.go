// Checks "only admin/owner can change credential aliases" under **real wiring**.
//
// The same reasoning as authz_integrations_test.go (see that file's header): core's unit tests put
// an identity into the context and cannot prove where the identity comes from in a real request;
// authz's unit tests use a fake core and cannot prove the patterns in the matrix match the real
// routes exactly. Only wiring them together and hitting real routes counts.
//
// # Why aliases are at the same level as "integrations"
//
// An alias **injects a platform credential once more** under another name, giving access to exactly
// the same things as the original credential. Letting a member change it means letting them decide
// "which credential variables appear in the container", the same kind of operation as ticking
// integrations, just through a different entry. Both holes must be closed; missing one means the
// restriction does not exist:
//
//	1. PATCH /api/agents/{id}                old hole: change one field and you are around it
//	2. POST /api/workspaces/{ws}/agents      the easiest to miss: not in the "modify" mental model
//
// ⚠️ There is no PUT .../env-aliases route: aliases are part of the existing agent write path, and
// real saves send the whole thing via PATCH (same as integrationIds today; see the comment in
// web/src/data/api.ts).

package httpd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// agentEnvAliases reads back an agent's current aliases as owner (who can always read them).
//
// Asserting "did it really change" must read it back: 403 means it was blocked, but 200 does not
// mean it was written.
func agentEnvAliases(t *testing.T, h http.Handler, agentID string) []map[string]string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodGet, "/api/agents/"+agentID, roleOwner, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("读 agent 失败：状态码 %d，body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		EnvAliases []map[string]string `json:"envAliases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解 agent 失败: %v", err)
	}
	return out.EnvAliases
}

// TestEnvAliasPatchBackdoorIsClosed: member sending envAliases via PATCH is rejected.
//
// Pinned together with a **control group**: the rule must be "did the value change", not "was it
// sent". The frontend's toAgentDraft always sends back the full draft; deciding by "was it sent"
// would stop a member from even saving a name change, and that error says "aliases need an admin",
// which does not point at all to what they actually did.
func TestEnvAliasPatchBackdoorIsClosed(t *testing.T) {
	h := newRBACServer(t)
	path := "/api/agents/" + rbacAgent

	// model is required (validateAgentDraft); without it the result is 400 instead of 403, and this
	// case would no longer test permissions.
	const model = `"model":{"id":"doubao-seed-2-1-pro-260628","thinking":"enabled",` +
		`"reasoning_effort":"high","service_tier":"default"}`
	// Sending it back unchanged must include integrationIds, otherwise it would first be stopped by
	// that gate (the value changed), and this case would test that restriction instead of aliases.
	// The seed has ["lark-user"].
	const keepIntegrations = `,"integrationIds":["` + rbacAgentIntegration + `"]`

	t.Run("member 加别名被拒", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(http.MethodPatch, path, roleMember,
			`{"name":"工单分诊助手",`+model+keepIntegrations+
				`,"envAliases":[{"key":"MY_AMA_KEY","from":"AMA_TOKEN"}]}`))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("member 从 PATCH 加上别名了：状态码 %d，body=%s\n"+
				"——别名与授权同档位，这条口子必须堵",
				rec.Code, rec.Body.String())
		}
		if _, code := envelope(t, rec); code != "access_denied" {
			t.Errorf("code = %q，期望 access_denied", code)
		}
		if got := agentEnvAliases(t, h, rbacAgent); len(got) != 0 {
			t.Errorf("被 403 了但值变了：%v", got)
		}
	})

	// ⚠️ Control group, **cannot be skipped**: without it someone would change the implementation to
	// "reject when the body has this key".
	t.Run("member 原样回传能保存", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(http.MethodPatch, path, roleMember,
			`{"name":"member 改的名字",`+model+keepIntegrations+`,"envAliases":[]}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("member 原样回传却存不下去：状态码 %d，body=%s\n"+
				"——判据必须是「值变没变」，不能是「传没传」（前端总是回传完整草稿）",
				rec.Code, rec.Body.String())
		}
	})

	t.Run("admin 能改", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(http.MethodPatch, path, roleAdmin,
			`{"name":"工单分诊助手",`+model+keepIntegrations+
				`,"envAliases":[{"key":"MY_AMA_KEY","from":"AMA_TOKEN"}]}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("admin 应当能改：状态码 %d，body=%s", rec.Code, rec.Body.String())
		}
		// Read back to confirm it was really written: 200 does not mean the write succeeded (the
		// handler might silently ignore it).
		got := agentEnvAliases(t, h, rbacAgent)
		if len(got) != 1 || got[0]["key"] != "MY_AMA_KEY" {
			t.Fatalf("admin 改完读回 = %v，期望一条 MY_AMA_KEY——回了 200 但没写进去", got)
		}
	})
}

// TestEnvAliasCreateBackdoorIsClosed: member creating an agent with aliases is rejected.
//
// ⚠️ **This is the easiest to miss**: it is not in the "modify" mental model. Without closing it a
// member only needs to "create a new one" to get around the whole restriction, however strict the
// PATCH check is.
func TestEnvAliasCreateBackdoorIsClosed(t *testing.T) {
	h := newRBACServer(t)
	const path = "/api/workspaces/ws_demo/agents"
	const model = `"model":{"id":"doubao-seed-2-1-pro-260628","thinking":"enabled",` +
		`"reasoning_effort":"high","service_tier":"default"}`

	t.Run("member 建 agent 带别名被拒", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(http.MethodPost, path, roleMember,
			`{"name":"偷偷带别名",`+model+
				`,"envAliases":[{"key":"MY_AMA_KEY","from":"AMA_TOKEN"}]}`))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("member 建了个带别名的 agent：状态码 %d，body=%s\n"+
				"——「建一个新的」是最容易漏的那条绕过", rec.Code, rec.Body.String())
		}
		if _, code := envelope(t, rec); code != "access_denied" {
			t.Errorf("code = %q，期望 access_denied", code)
		}
	})

	// ⚠️ Control group: **an empty list is the default of the create form**; creating an agent is a
	// member's right, and it must not be rejected just because the body has an empty array.
	t.Run("member 建不带别名的 agent 照常放行", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(http.MethodPost, path, roleMember,
			`{"name":"正常的 agent",`+model+`,"envAliases":[]}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("member 建普通 agent 被拦住了：状态码 %d，body=%s\n"+
				"——空列表是默认值，拦它等于禁止 member 建 agent",
				rec.Code, rec.Body.String())
		}
	})
}
