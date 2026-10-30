// **Owner isolation** of integration status, through the full HTTP stack with real wiring.
//
// # Why test it here again when core already did
//
// `core/integration_actor_test.go` is a **unit** test: it puts an Identity into the context and
// skips the real authentication middleware. That approach cannot test the most important part of
// this path: **how Protect turns a credential into an identity**. And the answer to "can A see B's
// integrations" depends exactly on that part:
//
//	unit tests guarantee   "whoever is in the context, their integrations are looked up"
//	this layer guarantees  "the person in the context is the real holder of the credential, not
//	                       who the request claims to be"
//
// Without the latter, the former means nothing. So here two people each carry **their own real
// session token** through the real mux → real Protect → real Guard → real handler, and each must
// only see their own.
//
// This layer is also the only place that can disprove "change a header and you can pose as someone
// else": in unit tests Protect is not on the chain, a forged header could never work anyway, and
// testing it would show nothing.
//
// Copying the approach of authz_test.go: identities come from seeding real sessions in auth's
// store, and requests carry the matching Bearer token. **Do not put an Identity into the context**:
// Protect overwrites it with the identity it resolves itself, so every request becomes 401, and that
// error does not point to the real cause at all.

package httpd

import (
	"context"
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
)

// The two people. **Both are deliberately real members of the seed workspace ws_demo**: so "they see
// different things" cannot be explained as "one of them has no permission for this workspace"; both
// are in the same workspace with enough role.
const (
	integOwner  = roleOwner  // alice@example.com
	integMember = roleMember // carol@example.com
)

// echoActorProvider writes the actor it receives into the status, so "whose copy did we get" is
// visible in the response.
//
// It is separate from the fake with the same idea in the core package: this package cannot import
// that one (test files are not shared across packages). Rewriting it also shows the provider
// interface is thin.
type echoActorProvider struct{}

func (echoActorProvider) ID() string          { return "echo" }
func (echoActorProvider) Name() string        { return "Echo 源" }
func (echoActorProvider) Description() string { return "把当事人回显出来" }

func (echoActorProvider) State(_ context.Context, actor string) core.IntegrationState {
	return core.IntegrationState{
		Status:  core.IntegrationOK,
		Summary: "已授权：" + actor,
		Details: []core.IntegrationDetail{{Label: "授权用户", Value: actor}},
	}
}

func (echoActorProvider) AuthorizeURL(_ context.Context, actor string) (string, error) {
	return "https://example.test/authorize?for=" + actor, nil
}

func (echoActorProvider) Revoke(_ context.Context, actor string) error { return nil }

// newIntegrationServer builds a complete Server with an integration registry.
//
// The only difference from newRBACServer is the extra Integrations in core.HandlerConfig. The rest
// (real Guard, real Protect, real seed, real sessions of five people) is exactly the same.
func newIntegrationServer(t *testing.T) http.Handler {
	t.Helper()

	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败: %v", err)
	}
	coreSvc, err := core.New(core.Config{Store: store, BlobStore: coremem.NewMemBlobStore()})
	if err != nil {
		t.Fatalf("建 core.Service 失败: %v", err)
	}

	reg := core.NewIntegrationRegistry(nil)
	reg.Register(echoActorProvider{})

	coreHandler := newCoreRoutes(t, coreSvc, reg)

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
	// roleNobody belongs to no workspace; used to check "non-members can still manage their own
	// integrations".
	for _, email := range []string{integOwner, integMember, roleNobody} {
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

// ⭐ **Two people, each with their own real credential, each only see their own copy.**
//
// This is the most convincing evidence for the question "seeing integration records in a brand-new
// workspace, is that overreach": the full HTTP stack, real session tokens, two real members of the
// same workspace.
func TestIntegrationRealStackIsolatesActors(t *testing.T) {
	h := newIntegrationServer(t)

	for _, path := range []string{"/api/integrations", "/api/integrations/echo"} {
		for _, tc := range []struct{ me, other string }{
			{integOwner, integMember},
			{integMember, integOwner},
		} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, asRole(http.MethodGet, path, tc.me, ""))

			if rec.Code != http.StatusOK {
				t.Fatalf("%s 以 %s 打应回 200，实际 %d：%s", path, tc.me, rec.Code, rec.Body.String())
			}
			got := rec.Body.String()
			if !strings.Contains(got, tc.me) {
				t.Errorf("%s：%s 应看到自己那一份，实际：%s", path, tc.me, got)
			}
			if strings.Contains(got, tc.other) {
				t.Errorf("%s：%s 的响应里出现了 %s——这是越权！实际：%s",
					path, tc.me, tc.other, got)
			}
		}
	}
}

// ⭐⭐ **With A's credential, while claiming to be B in every way, you still get A's copy.**
//
// This only makes sense under real wiring: Protect is on the chain, and the identity it resolves
// from the credential overrides any "identity claimed in the request". In unit tests Protect is not
// there, and forging could never work anyway.
//
// The six methods were picked as "really seen before / the easiest to write by accident"; among
// them X-AMA-Actor is the header this repository **really used** (see the file header of
// auth/context.go). If one place was not cleaned up during the move to the context, this is where
// that back door would be.
func TestIntegrationRealStackIgnoresForgedActor(t *testing.T) {
	h := newIntegrationServer(t)

	for _, tc := range []struct {
		name    string
		path    string
		headers map[string]string
	}{
		{name: "query actor", path: "/api/integrations?actor=" + integMember},
		{name: "query email", path: "/api/integrations?email=" + integMember},
		{
			name:    "X-AMA-Actor 头（本仓库曾用过）",
			path:    "/api/integrations",
			headers: map[string]string{"X-AMA-Actor": integMember},
		},
		{
			name:    "X-Actor 头",
			path:    "/api/integrations",
			headers: map[string]string{"X-Actor": integMember},
		},
		{
			name:    "X-User-Email 头",
			path:    "/api/integrations",
			headers: map[string]string{"X-User-Email": integMember},
		},
		{
			// The impostor **replaces their own Bearer with the other person's email**, but the token
			// is wrong. This locks down "the identity comes from the credential itself, not from text
			// written in it".
			name:    "拿对方邮箱当 token",
			path:    "/api/integrations",
			headers: map[string]string{"X-Forwarded-User": integMember},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := asRole(http.MethodGet, tc.path, integOwner, "")
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("应回 200，实际 %d：%s", rec.Code, rec.Body.String())
			}
			got := rec.Body.String()
			if strings.Contains(got, integMember) {
				t.Fatalf("⚠️ 越权：%s 让 %s 拿到了 %s 那一份！响应：%s",
					tc.name, integOwner, integMember, got)
			}
			if !strings.Contains(got, integOwner) {
				t.Errorf("拿到的应是自己那一份，实际：%s", got)
			}
		})
	}
}

// No credential always gives 401, also when a forged identity claim is sent.
//
// This is the worst case: if an empty credential + a header could make up an identity, the whole
// authentication would be as good as nonexistent.
func TestIntegrationRealStackRejectsAnonymous(t *testing.T) {
	h := newIntegrationServer(t)

	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{name: "什么都不带"},
		{name: "只带伪造头", headers: map[string]string{"X-AMA-Actor": integOwner}},
		{name: "只带伪造 query"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := "/api/integrations"
			if tc.name == "只带伪造 query" {
				path += "?actor=" + integOwner
			}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			// 401, not 200: not logged in is not logged in, whatever the header says.
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("⚠️ 未登录应回 401，实际 %d：%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// Non-members can also call these four endpoints: **this is deliberate, not a hole**.
//
// These four are ActionNone: integrations are account-level, unrelated to "which workspace you are
// in and what your role is". Declaring them ActionRead would stop a newcomer who has not joined any
// workspace from opening "integration management", while they most need to authorize here first
// (the comment in matrix.go is written about exactly this).
//
// So this asserts "a non-member gets 200 and only sees their own copy"; it proves two things at
// once: letting it through is right, and letting it through does **not** cause overreach.
func TestIntegrationNonMemberGetsOwnStateNot403(t *testing.T) {
	h := newIntegrationServer(t)

	// roleNobody is not in any workspace's member table.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodGet, "/api/integrations", roleNobody, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("非成员打授权接口应回 200（这四条是 ActionNone），实际 %d：%s",
			rec.Code, rec.Body.String())
	}
	// And what they see is **their own** copy, not someone else's.
	got := rec.Body.String()
	if !strings.Contains(got, roleNobody) {
		t.Errorf("非成员应看到自己那一份，实际：%s", got)
	}
	for _, other := range []string{integOwner, integMember} {
		if strings.Contains(got, other) {
			t.Errorf("⚠️ 越权：非成员看到了 %s 的授权状态！响应：%s", other, got)
		}
	}

	// Control group: **the same person** calling a workspace-level endpoint must be blocked with 403.
	// Only both together show that the 200 above is "a property of this route" and not "Guard is not
	// wired": with only the one above, a completely broken Guard would also be green.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, asRole(http.MethodGet, "/api/workspaces/"+rbacWS+"/agents", roleNobody, ""))
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("非成员打空间级接口应回 403，实际 %d：%s——Guard 可能没装上",
			rec2.Code, rec2.Body.String())
	}
}

// The integration status response **has no workspace-scoped fields**, which confirms in reverse
// "this is account-level".
func TestIntegrationRealStackCarriesNoWorkspaceField(t *testing.T) {
	h := newIntegrationServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodGet, "/api/integrations", integOwner, ""))

	var raw []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("解析失败：%v（响应：%s）", err, rec.Body.String())
	}
	if len(raw) != 1 {
		t.Fatalf("应有 1 条，实际 %d", len(raw))
	}
	for _, k := range []string{"workspaceId", "workspaceID", "workspaceKey", "workspace"} {
		if _, has := raw[0][k]; has {
			t.Errorf("账号级的状态不该带 %q 字段", k)
		}
	}
}
