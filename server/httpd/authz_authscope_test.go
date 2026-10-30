// Checks "personal integrations only follow the creator; service accounts must first be bound in the
// workspace" under **real wiring**.
//
// # Why these must be in httpd and not core
//
// core's unit tests run by putting an identity into the context with `auth.WithIdentity`. That
// checks the business rules, but **cannot prove "where the identity comes from in a real
// request"**: if the implementation one day read a request header (or trusted createdBy in the
// request body), not a single core case would turn red.
//
// This repository has fallen into that trap: unit tests that put things into the context cannot
// prove a forged header has no effect, because `Protect` is not on that chain at all.
//
// So this file hits **real routes** with **real session tokens**, and does one more thing core
// cannot: it **also** sends a forged identity header and confirms not a word of it counts (see
// TestPersonalIntegrationIgnoresForgedIdentityHeaders).
//
// # Split with authz_integrations_test.go
//
//	that file   the **role** levels of "who can change integrations" (Guard + admin gate)
//	this file   the **classification** rules of "personal vs service account" (creator / workspace binding)
//
// The two sets of rules sit on the same routes, so they are written in two files, so that each
// one's failure messages point clearly.
package httpd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// createAgentAs has someone create an agent and returns its id.
//
// **After creating it, createdBy is read back once** to confirm it really is this person: every later
// "creator" check depends on it, and if CreateAgent ever stopped taking this value from the context,
// this blows up right here instead of letting the later cases pass on a wrong premise.
func createAgentAs(t *testing.T, h http.Handler, actor, name string) string {
	t.Helper()
	const model = `"model":{"id":"doubao-seed-2-1-pro-260628","thinking":"enabled",` +
		`"reasoning_effort":"high","service_tier":"default"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodPost, "/api/workspaces/"+rbacWS+"/agents", actor,
		`{"name":"`+name+`","system":"hi",`+model+`}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("建 agent 失败：状态码 %d，body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID        string `json:"id"`
		CreatedBy string `json:"createdBy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解 agent 失败：%v", err)
	}
	if out.CreatedBy != actor {
		t.Fatalf("createdBy 是 %q，期望 %q——createdBy 必须从 context 取，"+
			"否则「谁是创建者」这件事就不可信了", out.CreatedBy, actor)
	}
	return out.ID
}

// patchAgent PATCHes an agent as someone and returns the status code and body.
//
// ⚠️ The request body is the **full draft** (including integrationIds), the same as the frontend's
// toAgentDraft. That is the precondition for the case "a non-creator renaming is allowed".
func patchAgent(
	t *testing.T, h http.Handler, agentID, actor, name string, integrationIDs []string,
) (int, string) {
	t.Helper()
	ids, err := json.Marshal(integrationIDs)
	if err != nil {
		t.Fatalf("编 integrationIds 失败：%v", err)
	}
	const model = `"model":{"id":"doubao-seed-2-1-pro-260628","thinking":"enabled",` +
		`"reasoning_effort":"high","service_tier":"default"}`
	body := `{"name":"` + name + `","system":"hi",` + model +
		`,"integrationIds":` + string(ids) + `}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodPatch, "/api/agents/"+agentID, actor, body))
	return rec.Code, rec.Body.String()
}

// TestPersonalIntegrationFollowsCreatorOverHTTP: four of ①②③⑤ under real wiring.
//
// The four run in one case because they share the state of one agent as it evolves (create →
// creator injects → someone else renames → someone else tries to change integrations); split up,
// each would need its own fixture, and "what happened on the same agent, in order" would be lost.
func TestPersonalIntegrationFollowsCreatorOverHTTP(t *testing.T) {
	h := newRBACServer(t)
	// Zhang San = member, Li Si = admin.
	//
	// ⚠️ Li Si is deliberately an **admin**: this rule is stricter than the admin level (personal
	// credentials are not something the workspace owns, and an admin cannot inject them on someone's
	// behalf). With a member as Li Si, "it was actually blocked by the admin gate" would take away
	// these cases' power to tell things apart.
	agentID := createAgentAs(t, h, roleMember, "张三的 agent")

	t.Run("① 创建者注入自己的个人授权 → 200", func(t *testing.T) {
		code, body := patchAgent(t, h, agentID, roleMember, "张三的 agent",
			[]string{"lark-user"})
		if code != http.StatusOK {
			t.Fatalf("创建者注入自己的个人授权应当成功：状态码 %d，body=%s", code, body)
		}
	})

	t.Run("② 非创建者注入个人授权 → 403 且文案可行动", func(t *testing.T) {
		// Li Si tries to add another personal integration (ama is also a personal integration).
		code, body := patchAgent(t, h, agentID, roleAdmin, "张三的 agent",
			[]string{"lark-user", "ama"})
		if code != http.StatusForbidden {
			t.Fatalf("非创建者注入个人授权必须 403：状态码 %d，body=%s\n"+
				"——这是本次要堵的那个洞", code, body)
		}
		// The message must name the creator, otherwise the user does not know whom to ask.
		if !strings.Contains(body, roleMember) {
			t.Errorf("报错文案里要点名创建者（%s），实际：%s", roleMember, body)
		}
		if !strings.Contains(body, "服务账号") {
			t.Errorf("文案要给出可行动的替代方案，实际：%s", body)
		}
	})

	t.Run("③ 非创建者改标题（授权原样回传）→ 200", func(t *testing.T) {
		// ⚠️ This is the easiest one in the group to break: deciding by "was it sent" would make this
		// 403, and the error would say "only the creator can change personal integrations", which
		// does not point at all to what they actually did (rename).
		code, body := patchAgent(t, h, agentID, roleAdmin, "李四改的标题",
			[]string{"lark-user"})
		if code != http.StatusOK {
			t.Fatalf("非创建者改标题（授权未变）应当成功：状态码 %d，body=%s\n"+
				"——这正是「member 连改名字都保存不了」那个 bug 的症状", code, body)
		}
		if !strings.Contains(body, "李四改的标题") {
			t.Errorf("标题没改上：%s", body)
		}
	})

	t.Run("⑤ 非创建者移除已有的个人授权 → 403", func(t *testing.T) {
		code, body := patchAgent(t, h, agentID, roleAdmin, "李四改的标题", []string{})
		if code != http.StatusForbidden {
			t.Fatalf("非创建者移除个人授权必须 403：状态码 %d，body=%s\n"+
				"——只拦「新增」的实现会让这条漏过去，而静默允许移除会让 agent "+
				"悄悄失去能力", code, body)
		}
	})

	// Read back to confirm the stored copy **was not touched**: 403 means it was blocked, but we must
	// confirm it was not "written first, then errored".
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodGet, "/api/agents/"+agentID, roleOwner, ""))
	var out struct {
		IntegrationIDs []string `json:"integrationIds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解 agent 失败：%v", err)
	}
	if len(out.IntegrationIDs) != 1 || out.IntegrationIDs[0] != "lark-user" {
		t.Errorf("授权被动了：%v（应当仍是 [lark-user]）", out.IntegrationIDs)
	}
}

// TestPersonalIntegrationIgnoresForgedIdentityHeaders ⚠️ forged identity headers count for nothing.
//
// # This case is the biggest reason this file exists
//
// The input of "who is the creator" must come from **the actor in the context** (resolved by the
// Protect middleware from the session cookie/Bearer), not from any request header or body field.
//
// core's unit tests **cannot prove this**: they call `auth.WithIdentity(ctx, ...)` directly, and
// there are no headers to forge. So here a real session (Li Si) + a pile of forged headers (all
// claiming to be Zhang San) are sent, and the result must still be decided as **Li Si**, i.e. 403.
//
// If someday someone changes the rule to read a header like `X-Ama-Actor`, this one turns red, and
// no other case would.
func TestPersonalIntegrationIgnoresForgedIdentityHeaders(t *testing.T) {
	h := newRBACServer(t)
	agentID := createAgentAs(t, h, roleMember, "张三的 agent")

	// Zhang San first injects his own personal integration (a legitimate operation).
	if code, body := patchAgent(t, h, agentID, roleMember, "张三的 agent",
		[]string{"lark-user"}); code != http.StatusOK {
		t.Fatalf("创建者注入失败：状态码 %d，body=%s", code, body)
	}

	// Li Si, with a **real session** (his own) + a pile of headers forged as Zhang San, tries to
	// change the integrations.
	const model = `"model":{"id":"doubao-seed-2-1-pro-260628","thinking":"enabled",` +
		`"reasoning_effort":"high","service_tier":"default"}`
	req := asRole(http.MethodPatch, "/api/agents/"+agentID, roleAdmin,
		`{"name":"张三的 agent","system":"hi",`+model+
			// ⚠️ Also put a createdBy into the request body, and confirm it is not trusted either.
			`,"createdBy":"`+roleMember+`","integrationIds":["lark-user","ama"]}`)
	for _, k := range []string{
		"X-Ama-Actor", "X-Ama-Email", "X-Actor", "X-User-Email",
		"X-Forwarded-User", "X-Forwarded-Email", "Remote-User",
	} {
		req.Header.Set(k, roleMember)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("带伪造身份头仍然必须按真实会话（%s）判 → 403，实际状态码 %d，body=%s\n"+
			"——身份只能来自 context（Protect 从会话解出来的那个），"+
			"任何请求头/请求体字段都不能参与「谁是创建者」的判断",
			roleAdmin, rec.Code, rec.Body.String())
	}
	// The message still names the **real** creator, not the one in the forged headers (here the two
	// happen to have the same name, which is all the more reason to confirm the status is 403: that
	// is the evidence it was "decided as Li Si").
	if !strings.Contains(rec.Body.String(), roleMember) {
		t.Errorf("文案要点名真正的创建者，实际：%s", rec.Body.String())
	}
}
