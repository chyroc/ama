// **Caller isolation** of authorization states: these tests lock in "I can only see my own".
//
// # Why a separate file
//
// integration_test.go locks in the four-state model and routing; when those break, the page shows
// the wrong thing. This file locks in the kind that **becomes overreach when wrong**. The two are
// handled completely differently on failure (the former means fixing text and branches; the
// latter is a security incident that must stop everything). Keeping them apart also lets the
// question "is authorization isolation tested" be answered at a glance, without searching through
// forty tests.
//
// # Which real misunderstanding these defend against
//
// "Authorization management" is a tab in **workspace settings**, so creating a new workspace and
// going in shows your own authorization records, which looks like a cross-workspace leak. It is
// actually **account-level**: states are looked up by actor, and the path has no workspaceID at
// all (see the four ActionNone entries in authz/matrix.go).
//
// Only tests can tell "looks like overreach" from "real overreach", so two things are pinned
// here:
//
//	1. A's response **contains nothing** of B's, even when both are in the same workspace
//	2. the caller can **only** come from the context; query / body / headers cannot affect it
//
// Point 2 is the premise of point 1: if the actor can be overridden by request parameters, point
// 1 being green is worthless. Anyone could set their actor to someone else's email and then
// legitimately get that person's data.

package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/chyroc/ama/server/auth"
)

// Two callers. **Both use real-shaped emails**: downstream code relies on the actor being an email
// (the provider normalizes it into the platform-side identifier), and "a" / "b" cannot test that
// normalization step.
const (
	actorAlice = "alice.zhang@example.com"
	actorBob   = "bob.li@example.com"
)

// actorEchoProvider encodes the actor it receives into its own state as is.
//
// This is the only way "isolation" can be observed: a provider's state normally lives on the
// platform side, which tests cannot reach, so the state is **determined by the actor**. That way
// "whose state did I get" is directly visible in the response body.
type actorEchoProvider struct {
	id string

	// mu protects seen: List queries providers concurrently, and the same provider instance may
	// also be hit by several requests at once (the concurrency test below does exactly that);
	// without the lock, -race fails.
	mu   sync.Mutex
	seen []string
}

func (p *actorEchoProvider) ID() string          { return p.id }
func (p *actorEchoProvider) Name() string        { return "Echo 源" }
func (p *actorEchoProvider) Description() string { return "把 actor 回显出来" }

func (p *actorEchoProvider) State(_ context.Context, actor string) IntegrationState {
	p.mu.Lock()
	p.seen = append(p.seen, actor)
	p.mu.Unlock()
	return IntegrationState{
		Status:  IntegrationOK,
		Summary: "已授权：" + actor,
		Details: []IntegrationDetail{{Label: "授权用户", Value: actor}},
	}
}

func (p *actorEchoProvider) AuthorizeURL(_ context.Context, actor string) (string, error) {
	p.mu.Lock()
	p.seen = append(p.seen, actor)
	p.mu.Unlock()
	return "https://example.test/authorize?for=" + actor, nil
}

func (p *actorEchoProvider) Revoke(_ context.Context, actor string) error {
	p.mu.Lock()
	p.seen = append(p.seen, actor)
	p.mu.Unlock()
	return nil
}

// actors returns a copy of the list of actors received.
func (p *actorEchoProvider) actors() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

// doAs sends a request as the given identity, and allows adding query / headers / body that "try
// to impersonate someone else".
//
// An empty actor means not logged in.
func doAs(t *testing.T, mux *http.ServeMux, method, path, actor string,
	headers map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if actor != "" {
		req = req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{
			Email: actor,
			Name:  actor,
		}))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// echoMux builds a mux with only the echo provider installed.
func echoMux(t *testing.T) (*http.ServeMux, *actorEchoProvider) {
	t.Helper()
	p := &actorEchoProvider{id: "echo"}
	reg := NewIntegrationRegistry(nil)
	reg.Register(p)
	return newIntegrationMux(t, reg), p
}

// ---- 1. A cannot see B --------------------------------------------------------

// ⭐ **Each person only sees their own authorization state.**
//
// This is the direct answer to "I see authorization records in a new workspace; is that
// overreach?": no. State is looked up per caller, and A's response contains no trace of B.
//
// The assertions have two halves, **both required**:
//
//	positive  what we got is our own (otherwise both might get empty results and still "not leak")
//	negative  the other person's email cannot be found anywhere in the response body (that is "no leak")
//
// With only the positive half, changing Summary to a constant would also pass; both would then
// see the same sentence, and "the same sentence" is exactly what we are guarding against.
func TestIntegrationStateIsPerActor(t *testing.T) {
	for _, path := range []string{"/api/integrations", "/api/integrations/echo"} {
		mux, _ := echoMux(t)

		for _, tc := range []struct{ me, other string }{
			{actorAlice, actorBob},
			{actorBob, actorAlice},
		} {
			rec := doAs(t, mux, http.MethodGet, path, tc.me, nil, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("%s 以 %s 打应回 200，实际 %d：%s", path, tc.me, rec.Code, rec.Body.String())
			}
			got := rec.Body.String()

			// Positive: this one is mine.
			if !strings.Contains(got, tc.me) {
				t.Errorf("%s：%s 应看到自己的状态，响应里却没有他的邮箱：%s", path, tc.me, got)
			}
			// Negative: **nobody else may appear in the response**. Search the whole response body
			// byte by byte instead of one field: a leak could happen in summary / details / fixHint.
			if strings.Contains(got, tc.other) {
				t.Errorf("%s：%s 的响应里出现了 %s 的信息——这是越权！响应：%s",
					path, tc.me, tc.other, got)
			}
		}
	}
}

// Two people in the same workspace also each see their own.
//
// The previous test has no workspace context, and could be read as "isolated because they are
// not in the same workspace". This one puts both in **the same workspace** (same seed data, same
// Service), and the conclusion holds, because authorization is not split by workspace at all; it
// is split by person.
//
// This also explains why these four routes are ActionNone: their answers do not depend on "which
// workspace you are in or what role you have".
func TestIntegrationStateIsPerActorEvenInSameWorkspace(t *testing.T) {
	// Both share the same Service, seed data and workspace, so "they see different things" cannot
	// be caused by "different data sources"; it can only be split by caller.
	svc, _ := newService(t)
	wss, err := svc.workspaces.ListWorkspaces(context.Background())
	if err != nil || len(wss) == 0 {
		t.Fatalf("种子里应有空间：err=%v len=%d", err, len(wss))
	}
	ws := wss[0]

	p := &actorEchoProvider{id: "echo"}
	reg := NewIntegrationRegistry(nil)
	reg.Register(p)
	h := newTestHandler(t, svc, reg)
	mux := http.NewServeMux()
	h.Register(mux)

	alice := doAs(t, mux, http.MethodGet, "/api/integrations", actorAlice, nil, "").Body.String()
	bob := doAs(t, mux, http.MethodGet, "/api/integrations", actorBob, nil, "").Body.String()

	if strings.Contains(alice, actorBob) || strings.Contains(bob, actorAlice) {
		t.Errorf("同一个空间（%s）里两人也不该看到对方的授权状态\nalice: %s\nbob:   %s",
			ws.Key, alice, bob)
	}
	if alice == bob {
		t.Errorf("两个人拿到了完全一样的响应——状态没有按当事人区分：%s", alice)
	}
}

// No mix-ups under concurrency either.
//
// List queries providers concurrently, and production runs multiple instances, with many requests
// running at once inside one process. Mix-up bugs are always green in single-request tests; only
// concurrency + -race catches them.
func TestIntegrationConcurrentRequestsDoNotCrossActors(t *testing.T) {
	mux, _ := echoMux(t)

	const rounds = 40
	var wg sync.WaitGroup
	errs := make(chan string, rounds*2)
	for i := 0; i < rounds; i++ {
		for _, tc := range []struct{ me, other string }{
			{actorAlice, actorBob},
			{actorBob, actorAlice},
		} {
			wg.Add(1)
			go func(me, other string) {
				defer wg.Done()
				got := doAs(t, mux, http.MethodGet, "/api/integrations", me, nil, "").Body.String()
				if !strings.Contains(got, me) || strings.Contains(got, other) {
					errs <- me + " 拿到的响应串号了：" + got
				}
			}(tc.me, tc.other)
		}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// ---- 2. The caller can only come from the context -----------------------------

// ⭐⭐ **Nothing in the request can change "who I am".**
//
// This is the most important test in the group. All the "A cannot see B" tests above become
// worthless if the actor can be overridden by the request: an attacker does not need to read B's
// data, they only need to declare their actor as B.
//
// The covered surface is picked by "actually happened before / easiest to write by accident":
//
//	X-AMA-Actor    this repo **really used** this header (see the file header in auth/context.go);
//	               if one spot was not cleaned up when moving to context, this is that back door
//	?actor= / ?email=  the "debug parameter" easiest to add while at it
//	actor in the body  the comment on markOnboarding calls this out: adding an email field
//	               opens a way to change someone else's state
//
// The assertion is on **the actor the provider receives**, not on what the response looks like:
// that is the end of this chain, and the only place an override can be observed when it happens.
func TestIntegrationActorCannotBeOverriddenByRequest(t *testing.T) {
	const victim = actorBob

	for _, tc := range []struct {
		name    string
		path    string
		headers map[string]string
		body    string
	}{
		{name: "query 里塞 actor", path: "/api/integrations?actor=" + victim},
		{name: "query 里塞 email", path: "/api/integrations?email=" + victim},
		{name: "query 里塞 user", path: "/api/integrations?user=" + victim},
		{
			name:    "X-AMA-Actor 头（本仓库曾用过的那个）",
			path:    "/api/integrations",
			headers: map[string]string{"X-AMA-Actor": victim},
		},
		{
			name:    "X-Actor / X-User-Email 头",
			path:    "/api/integrations",
			headers: map[string]string{"X-Actor": victim, "X-User-Email": victim},
		},
		{
			name: "请求体里塞 actor 与 email",
			path: "/api/integrations",
			body: `{"actor":"` + victim + `","email":"` + victim + `"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux, p := echoMux(t)
			// Log in as alice while trying to impersonate bob.
			rec := doAs(t, mux, http.MethodGet, tc.path, actorAlice, tc.headers, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("应回 200，实际 %d：%s", rec.Code, rec.Body.String())
			}

			seen := p.actors()
			if len(seen) != 1 {
				t.Fatalf("provider 应被调 1 次，实际 %d 次：%v", len(seen), seen)
			}
			if seen[0] == victim {
				t.Fatalf("⚠️ 越权：%s 被请求里的值覆盖成了 %s——当事人必须只从 context 取", tc.name, victim)
			}
			if seen[0] != actorAlice {
				t.Fatalf("provider 收到的当事人应为 %q，实际 %q", actorAlice, seen[0])
			}
			// The impersonated person should not appear in the response body either.
			if strings.Contains(rec.Body.String(), victim) {
				t.Errorf("响应里出现了 %s：%s", victim, rec.Body.String())
			}
		})
	}
}

// The three write actions cannot be overridden by the request either.
//
// A read endpoint leaking means "seeing someone else's state"; a write endpoint being overridden
// is worse: it means starting an authorization **in someone else's name** or revoking someone
// else's. So these three are pinned again on their own, instead of inferring from the read
// endpoint result above.
func TestIntegrationWriteActionsAlsoIgnoreRequestActor(t *testing.T) {
	const victim = actorBob

	for _, tc := range []struct{ name, method, path string }{
		{"发起授权", http.MethodPost, "/api/integrations/echo/authorize?actor=" + victim},
		{"解除授权", http.MethodDelete, "/api/integrations/echo?actor=" + victim},
		{"查单个状态", http.MethodGet, "/api/integrations/echo?actor=" + victim},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux, p := echoMux(t)
			rec := doAs(t, mux, tc.method, tc.path, actorAlice,
				map[string]string{"X-AMA-Actor": victim},
				`{"actor":"`+victim+`"}`)
			if rec.Code >= 500 {
				t.Fatalf("不该 5xx，实际 %d：%s", rec.Code, rec.Body.String())
			}
			seen := p.actors()
			if len(seen) != 1 {
				t.Fatalf("provider 应被调 1 次，实际 %d 次：%v", len(seen), seen)
			}
			if seen[0] != actorAlice {
				t.Fatalf("⚠️ %s 的当事人应为 %q，实际 %q——被请求里的值覆盖了就是越权",
					tc.name, actorAlice, seen[0])
			}
		})
	}
}

// When not logged in, putting an actor in the request **must not** make it logged in either.
//
// This is the extreme case of the previous one, and the worst: if an empty context + query
// parameters could produce an identity, the whole authentication would effectively not exist
// (nobody needs to log in, just add a parameter).
//
// Expected: 400 (missing caller), and the provider is never called.
func TestIntegrationAnonymousCannotForgeActor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		headers map[string]string
	}{
		{name: "query", path: "/api/integrations?actor=" + actorBob},
		{
			name:    "头",
			path:    "/api/integrations",
			headers: map[string]string{"X-AMA-Actor": actorBob},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux, p := echoMux(t)
			// Empty actor ⇒ not logged in.
			rec := doAs(t, mux, http.MethodGet, tc.path, "", tc.headers, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("未登录应回 400，实际 %d：%s", rec.Code, rec.Body.String())
			}
			if seen := p.actors(); len(seen) != 0 {
				t.Fatalf("⚠️ 未登录却把 %v 送到了 provider——请求里的值凑出了一个身份", seen)
			}
		})
	}
}

// The actor the framework passes to the provider must be the login email **unchanged**, with no
// processing.
//
// The provider normalizes it into the platform-side identifier (the part before @), so a change
// on the way would mean "looking up another person". That reports no error; the state just shows
// unauthorized forever.
func TestIntegrationActorReachesProviderVerbatim(t *testing.T) {
	mux, p := echoMux(t)
	doAs(t, mux, http.MethodGet, "/api/integrations", actorAlice, nil, "")

	seen := p.actors()
	if len(seen) != 1 || seen[0] != actorAlice {
		t.Fatalf("provider 收到的应为 %q，实际 %v", actorAlice, seen)
	}
}

// The responses of the four routes **must contain no trace of workspace**, which confirms "this
// is account-level".
//
// This one is for people reading the code: authorization state has nothing to do with
// workspaces, so the response has no workspaceId. If someday someone adds that field to State
// "to include the workspace while at it", this fails. At that point, first think through whether
// it really should become workspace-level, instead of just adding it.
func TestIntegrationStateCarriesNoWorkspaceDimension(t *testing.T) {
	mux, _ := echoMux(t)
	rec := doAs(t, mux, http.MethodGet, "/api/integrations", actorAlice, nil, "")

	var raw []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("应有 1 条，实际 %d", len(raw))
	}
	for _, k := range []string{"workspaceId", "workspaceID", "workspaceKey", "workspace"} {
		if _, has := raw[0][k]; has {
			t.Errorf("授权状态是账号级的，不该带 %q 字段——加之前先想清楚它是不是要改成空间级", k)
		}
	}
}
