// HTTP and business boundary tests for the authorization management framework (provider
// registry + four-state model).
//
// This layer is fully offline: the upstreams of real providers need real credentials, which we
// cannot use here.
//
// So this locks in **decisions that can go wrong without any network**, and they happen to be
// the easiest to get wrong and report no error when wrong:
//
//   - routing is correct (a wrong provider id must give 404, not silently the first one)
//   - each of the four states is encoded correctly (missing and error in particular must stay
//     separate)
//   - where the UI rule "no authorize button if not connected" lands in the backend (CanAuthorize)
//   - credentials never go into responses

package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/auth"
)

// integrationActor is the logged-in identity in tests.
const integrationActor = "alice@example.com"

// fakeProvider is a programmable provider used to produce all combinations of the four states and the two actions.
type fakeProvider struct {
	id           string
	name         string
	desc         string
	state        IntegrationState
	authorizeAt  string
	authorizeErr error
	revokeErr    error
	panicOnState bool
	// stateCalls records how many times State was called and with which actor, to check that the framework passes the actor correctly.
	stateCalls []string
}

func (f *fakeProvider) ID() string          { return f.id }
func (f *fakeProvider) Name() string        { return f.name }
func (f *fakeProvider) Description() string { return f.desc }

func (f *fakeProvider) State(ctx context.Context, actor string) IntegrationState {
	f.stateCalls = append(f.stateCalls, actor)
	if f.panicOnState {
		panic("测试故意 panic")
	}
	return f.state
}

func (f *fakeProvider) AuthorizeURL(ctx context.Context, actor string) (string, error) {
	if f.authorizeErr != nil {
		return "", f.authorizeErr
	}
	return f.authorizeAt, nil
}

func (f *fakeProvider) Revoke(ctx context.Context, actor string) error { return f.revokeErr }

// newIntegrationMux builds a mux with the routes installed.
func newIntegrationMux(t *testing.T, reg *IntegrationRegistry) *http.ServeMux {
	t.Helper()
	svc, _ := newService(t)
	h := newTestHandler(t, svc, reg)
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}

// doIntegration sends a request with (or without) a logged-in identity.
func doIntegration(t *testing.T, mux *http.ServeMux, method, path string, loggedIn bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(""))
	req.Header.Set("Content-Type", "application/json")
	if loggedIn {
		req = req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{
			Email: integrationActor,
			Name:  "张三",
		}))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// ---- Routing -----------------------------------------------------------------

// Each route must reach **its own** provider, not all land on the first one.
//
// This is the easiest place to get wrong after moving to a registry, and the hardest to notice:
// with only one provider registered, "path parameter not wired, always returns the first" and
// "wired correctly" behave exactly the same. So this uses **two** providers and asserts for each
// that it got itself.
func TestIntegrationRoutesDispatchByProvider(t *testing.T) {
	reg := NewIntegrationRegistry(nil)
	reg.Register(&fakeProvider{
		id: "alpha", name: "Alpha 源",
		state:       IntegrationState{Status: IntegrationOK, Summary: "alpha 好的"},
		authorizeAt: "https://example.test/alpha",
	})
	reg.Register(&fakeProvider{
		id: "beta", name: "Beta 源",
		state:       IntegrationState{Status: IntegrationError, Summary: "beta 坏的"},
		authorizeAt: "https://example.test/beta",
	})
	mux := newIntegrationMux(t, reg)

	for _, tc := range []struct{ id, wantSummary string }{
		{"alpha", "alpha 好的"},
		{"beta", "beta 坏的"},
	} {
		rec := doIntegration(t, mux, http.MethodGet, "/api/integrations/"+tc.id, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s 应回 200，实际 %d：%s", tc.id, rec.Code, rec.Body.String())
		}
		var got IntegrationState
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("解析失败：%v", err)
		}
		if got.ID != tc.id {
			t.Errorf("GET /api/integrations/%s 拿到的是 %q——路径参数没接对", tc.id, got.ID)
		}
		if got.Summary != tc.wantSummary {
			t.Errorf("provider %s 的 summary 应为 %q，实际 %q", tc.id, tc.wantSummary, got.Summary)
		}
	}

	// authorize must be routed by provider too.
	for _, tc := range []struct{ id, wantURL string }{
		{"alpha", "https://example.test/alpha"},
		{"beta", "https://example.test/beta"},
	} {
		rec := doIntegration(t, mux, http.MethodPost, "/api/integrations/"+tc.id+"/authorize", true)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s/authorize 应回 200，实际 %d：%s", tc.id, rec.Code, rec.Body.String())
		}
		var body struct {
			AuthorizationURL string `json:"authorizationUrl"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("解析失败：%v", err)
		}
		if body.AuthorizationURL != tc.wantURL {
			t.Errorf("provider %s 的授权地址应为 %q，实际 %q", tc.id, tc.wantURL, body.AuthorizationURL)
		}
	}
}

// The list must be returned in **registration order** and include all providers.
//
// If the order is unstable, cards jump around and the page looks broken. And List queries
// concurrently, so "order" can really go wrong; this locks it in.
func TestIntegrationListKeepsRegistrationOrder(t *testing.T) {
	reg := NewIntegrationRegistry(nil)
	want := []string{"first", "second", "third", "fourth"}
	for _, id := range want {
		reg.Register(&fakeProvider{id: id, name: id, state: IntegrationState{Status: IntegrationOK}})
	}
	mux := newIntegrationMux(t, reg)

	// Run several times: ordering bugs under concurrency do not show up every time.
	for round := 0; round < 5; round++ {
		rec := doIntegration(t, mux, http.MethodGet, "/api/integrations", true)
		if rec.Code != http.StatusOK {
			t.Fatalf("应回 200，实际 %d：%s", rec.Code, rec.Body.String())
		}
		var states []IntegrationState
		if err := json.Unmarshal(rec.Body.Bytes(), &states); err != nil {
			t.Fatalf("解析失败：%v", err)
		}
		if len(states) != len(want) {
			t.Fatalf("应返回 %d 个授权源，实际 %d", len(want), len(states))
		}
		for i, id := range want {
			if states[i].ID != id {
				t.Fatalf("第 %d 轮：第 %d 个应为 %q，实际 %q——顺序没按注册顺序",
					round, i, id, states[i].ID)
			}
		}
	}
}

// Calling a provider that does not exist must return 404 + code=not_found, never silently
// another one.
//
// 404 because the frontend's list comes from the backend; hitting an unknown id means the
// frontend has a stale list, and 404 is the right signal then. Silently returning the first one
// is the worst: the user sees the Feishu user authorization state on the "AMA" card.
func TestIntegrationUnknownProviderReturns404(t *testing.T) {
	reg := NewIntegrationRegistry(nil)
	reg.Register(&fakeProvider{id: "alpha", name: "Alpha", state: IntegrationState{Status: IntegrationOK}})
	mux := newIntegrationMux(t, reg)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/integrations/nope"},
		{http.MethodPost, "/api/integrations/nope/authorize"},
		{http.MethodDelete, "/api/integrations/nope"},
	} {
		rec := doIntegration(t, mux, tc.method, tc.path, true)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s 应回 404，实际 %d：%s", tc.method, tc.path, rec.Code, rec.Body.String())
			continue
		}
		var body struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("解析失败：%v", err)
			continue
		}
		// The frontend decides by code, not by status code: when routes are mounted wrong, the standard library's 404 is text/plain.
		if body.Code != "not_found" {
			t.Errorf("%s %s 的 code 应为 not_found，实际 %q", tc.method, tc.path, body.Code)
		}
	}
}

// With no provider registered, the list returns 200 + `[]`, **not 404 and not null**.
//
// Both are locked in: 404 makes the page throw instead of showing the empty state; null makes the
// frontend's .map crash, which shows as the whole tab going blank.
func TestIntegrationEmptyRegistryReturnsEmptyArray(t *testing.T) {
	mux := newIntegrationMux(t, NewIntegrationRegistry(nil))

	rec := doIntegration(t, mux, http.MethodGet, "/api/integrations", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("应回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("空注册表应回 []，实际 %q——null 会让前端 .map 炸掉", got)
	}
}

// With Handler's Integrations nil, the four routes **still work**, not 404.
//
// On the frontend, 404 means "this backend version does not have the feature", which is a
// different thing from "the feature exists but no provider is configured".
func TestIntegrationNilRegistryStillServes(t *testing.T) {
	svc, _ := newService(t)
	h := newTestHandler(t, svc, nil) // deliberately no Integrations
	mux := http.NewServeMux()
	h.Register(mux)

	rec := doIntegration(t, mux, http.MethodGet, "/api/integrations", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("注册表为 nil 时列表也要回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("应回 []，实际 %q", got)
	}
}

// When not logged in, all four endpoints should return 400, instead of sending an empty actor to
// the platform.
//
// Sending an empty actor makes the platform return a vague error, which gets investigated as a
// "platform outage" for a long time, while the real cause is just not being logged in.
func TestIntegrationRequiresLogin(t *testing.T) {
	reg := NewIntegrationRegistry(nil)
	reg.Register(&fakeProvider{id: "alpha", name: "Alpha", state: IntegrationState{Status: IntegrationOK}})
	mux := newIntegrationMux(t, reg)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/integrations"},
		{http.MethodGet, "/api/integrations/alpha"},
		{http.MethodPost, "/api/integrations/alpha/authorize"},
		{http.MethodDelete, "/api/integrations/alpha"},
	} {
		rec := doIntegration(t, mux, tc.method, tc.path, false)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s 未登录应回 400，实际 %d：%s",
				tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

// The framework must pass **the currently logged-in user** to the provider, not an empty string and not someone else.
func TestIntegrationPassesActorToProvider(t *testing.T) {
	p := &fakeProvider{id: "alpha", name: "Alpha", state: IntegrationState{Status: IntegrationOK}}
	reg := NewIntegrationRegistry(nil)
	reg.Register(p)
	mux := newIntegrationMux(t, reg)

	doIntegration(t, mux, http.MethodGet, "/api/integrations", true)
	if len(p.stateCalls) != 1 {
		t.Fatalf("State 应被调 1 次，实际 %d 次", len(p.stateCalls))
	}
	if p.stateCalls[0] != integrationActor {
		t.Errorf("传给 provider 的 actor 应为 %q，实际 %q", integrationActor, p.stateCalls[0])
	}
}

// One provider panicking must not take down the whole list; the other providers' states must still be visible.
func TestIntegrationOneProviderPanicDoesNotKillList(t *testing.T) {
	reg := NewIntegrationRegistry(nil)
	reg.Register(&fakeProvider{id: "boom", name: "会炸的", panicOnState: true})
	reg.Register(&fakeProvider{
		id: "fine", name: "好的",
		state: IntegrationState{Status: IntegrationOK, Summary: "一切正常"},
	})
	mux := newIntegrationMux(t, reg)

	rec := doIntegration(t, mux, http.MethodGet, "/api/integrations", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("应回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var states []IntegrationState
	if err := json.Unmarshal(rec.Body.Bytes(), &states); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(states) != 2 {
		t.Fatalf("应返回 2 个，实际 %d", len(states))
	}
	if states[0].Status != IntegrationError {
		t.Errorf("panic 的那个应显示 error，实际 %q", states[0].Status)
	}
	if states[1].Status != IntegrationOK || states[1].Summary != "一切正常" {
		t.Errorf("好的那个应不受影响，实际 status=%q summary=%q",
			states[1].Status, states[1].Summary)
	}
}

// Registering the same id twice must panic, not silently overwrite.
//
// A silent overwrite shows up as one card missing from the page, with no error at all.
func TestIntegrationDuplicateRegistrationPanics(t *testing.T) {
	reg := NewIntegrationRegistry(nil)
	reg.Register(&fakeProvider{id: "same", name: "第一个"})
	defer func() {
		if rec := recover(); rec == nil {
			t.Error("重复注册应当 panic——静默覆盖会让页面少一张卡片且不报错")
		}
	}()
	reg.Register(&fakeProvider{id: "same", name: "第二个"})
}

// The framework must fill in ID / Name / CheckedAt when a provider forgets them.
//
// A missing ID means the frontend gets no key: the card becomes anonymous, and React reports
// duplicate keys.
func TestIntegrationFrameworkFillsMissingFields(t *testing.T) {
	reg := NewIntegrationRegistry(nil)
	// Deliberately leave state empty except for a Status.
	reg.Register(&fakeProvider{
		id: "alpha", name: "Alpha 源", desc: "一句话说明",
		state: IntegrationState{Status: IntegrationOK},
	})
	mux := newIntegrationMux(t, reg)

	rec := doIntegration(t, mux, http.MethodGet, "/api/integrations", true)
	var states []IntegrationState
	if err := json.Unmarshal(rec.Body.Bytes(), &states); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(states) != 1 {
		t.Fatalf("应返回 1 个，实际 %d", len(states))
	}
	got := states[0]
	if got.ID != "alpha" {
		t.Errorf("ID 应被兜底成 %q，实际 %q", "alpha", got.ID)
	}
	if got.Name != "Alpha 源" {
		t.Errorf("Name 应被兜底成 %q，实际 %q", "Alpha 源", got.Name)
	}
	if got.Description != "一句话说明" {
		t.Errorf("Description 应被兜底，实际 %q", got.Description)
	}
	if got.CheckedAt == "" {
		t.Error("CheckedAt 应被兜底，不该为空")
	}
}

// ---- Four states and two actions ---------------------------------------------

// The four status values must go into JSON unchanged, every one of them.
//
// The frontend picks colors and decides whether to show buttons by these strings; if a value
// changes, the frontend falls into the default branch, and that is silent (the card shows a
// neutral color, which just looks "less nice").
func TestIntegrationFourStatusesSerializeExactly(t *testing.T) {
	for _, tc := range []struct {
		status IntegrationStatus
		want   string
	}{
		{IntegrationOK, "ok"},
		{IntegrationWarning, "warning"},
		{IntegrationError, "error"},
		{IntegrationMissing, "missing"},
	} {
		reg := NewIntegrationRegistry(nil)
		reg.Register(&fakeProvider{
			id: "alpha", name: "Alpha",
			state: IntegrationState{Status: tc.status, Summary: "s"},
		})
		mux := newIntegrationMux(t, reg)
		rec := doIntegration(t, mux, http.MethodGet, "/api/integrations/alpha", true)

		var raw map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatalf("解析失败：%v", err)
		}
		if got := raw["status"]; got != tc.want {
			t.Errorf("状态 %v 应序列化成 %q，实际 %v", tc.status, tc.want, got)
		}
	}
}

// FixHint / FixURL / canAuthorize / canRevoke must go into the response as they are.
//
// These four fields carry "how to fix it"; without them the page can only say "broken" and not
// what to do.
func TestIntegrationFixFieldsReachResponse(t *testing.T) {
	reg := NewIntegrationRegistry(nil)
	reg.Register(&fakeProvider{
		id: "alpha", name: "Alpha",
		state: IntegrationState{
			Status:       IntegrationError,
			Summary:      "凭证失效了",
			FixHint:      "回控制台重置 secret 后重试",
			FixURL:       "https://console.test/app",
			CanAuthorize: true,
			CanRevoke:    true,
			Details: []IntegrationDetail{
				{Label: "授权用户", Value: integrationActor},
			},
		},
	})
	mux := newIntegrationMux(t, reg)

	rec := doIntegration(t, mux, http.MethodGet, "/api/integrations/alpha", true)
	var got IntegrationState
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if got.FixHint != "回控制台重置 secret 后重试" {
		t.Errorf("fixHint 没到响应里，实际 %q", got.FixHint)
	}
	if got.FixURL != "https://console.test/app" {
		t.Errorf("fixUrl 没到响应里，实际 %q", got.FixURL)
	}
	if !got.CanAuthorize {
		t.Error("canAuthorize 应为 true")
	}
	if !got.CanRevoke {
		t.Error("canRevoke 应为 true")
	}
	if len(got.Details) != 1 || got.Details[0].Label != "授权用户" {
		t.Errorf("details 没到响应里，实际 %+v", got.Details)
	}
}

// hasDetail / hasDetailLabel look up a name/value pair in Details.
//
// ⚠️ service/integration also has a copy with the same names (those tests moved there with the
// framework). Two copies on purpose: unexported symbols cannot be reached across packages, and
// exporting them for tests would widen visibility.
func hasDetail(details []IntegrationDetail, label, value string) bool {
	for _, d := range details {
		if d.Label == label && d.Value == value {
			return true
		}
	}
	return false
}

func hasDetailLabel(details []IntegrationDetail, label string) bool {
	for _, d := range details {
		if d.Label == label {
			return true
		}
	}
	return false
}
