// The full GitHub device flow: httptest starts a fake GitHub (shared/infra/github/githubtest),
// and the real github.Client and real Handler run through it. Time is injected by the test
// (svc.now), so "not yet time to poll", "interval grows after slow_down" and "expired" are all
// deterministic.
//
// The concurrency cases on PG are in repository/dao/authdao/github_pg_test.go (tests in the auth
// package cannot import authdao).

package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/shared/infra/github"
	"github.com/chyroc/ama/server/shared/infra/github/githubtest"
)

// ghFixture is a set of "fake GitHub + Service + Handler + adjustable clock".
type ghFixture struct {
	t     *testing.T
	gh    *githubtest.Server
	svc   *Service
	store *MemStore
	mux   *http.ServeMux

	mu  sync.Mutex
	now time.Time

	// bodies collects **all** response bodies and headers; at the end we assert that the
	// device_code / access token never appeared.
	bodies []string
}

func newGHFixture(t *testing.T, gh *githubtest.Server, cfg GitHubConfig) *ghFixture {
	t.Helper()
	f := &ghFixture{t: t, gh: gh, store: NewMemStore(), now: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)}
	cfg.Client = gh.Client(t)
	svc, err := New(Config{Store: f.store, GitHub: cfg})
	if err != nil {
		t.Fatal(err)
	}
	svc.now = f.clock
	f.store.now = f.clock
	f.svc = svc
	h, err := NewHandler(HandlerConfig{Service: svc})
	if err != nil {
		t.Fatal(err)
	}
	f.mux = http.NewServeMux()
	h.Register(f.mux)
	t.Cleanup(f.assertNoSecretLeaked)
	return f
}

func (f *ghFixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *ghFixture) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func (f *ghFixture) record(rec *httptest.ResponseRecorder) {
	var hdr bytes.Buffer
	_ = rec.Header().Write(&hdr)
	f.mu.Lock()
	f.bodies = append(f.bodies, rec.Body.String(), hdr.String())
	f.mu.Unlock()
}

func (f *ghFixture) assertNoSecretLeaked() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.bodies {
		if strings.Contains(b, githubtest.DeviceCode) {
			f.t.Errorf("device_code 出现在了响应里：%s", b)
		}
		if strings.Contains(b, githubtest.AccessToken) {
			f.t.Errorf("GitHub access token 出现在了响应里：%s", b)
		}
	}
}

type ghStart struct {
	FlowID          string `json:"flowId"`
	UserCode        string `json:"userCode"`
	VerificationURI string `json:"verificationUri"`
	Interval        int    `json:"interval"`
	ExpiresAt       string `json:"expiresAt"`
	cookie          *http.Cookie
}

// start starts a login and returns the response and the binding cookie.
func (f *ghFixture) start() ghStart {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, PathGitHubDevice, nil))
	f.record(rec)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("发起 GitHub 登录: %d %s", rec.Code, rec.Body.String())
	}
	var out ghStart
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		f.t.Fatal(err)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == GitHubCookiePrefix+out.FlowID {
			out.cookie = c
		}
	}
	if out.cookie == nil || !out.cookie.HttpOnly || out.cookie.SameSite != http.SameSiteStrictMode ||
		out.cookie.Path != "/api/auth/github" {
		f.t.Fatalf("绑定 Cookie 不对: %+v", out.cookie)
	}
	if out.cookie.Value == out.FlowID {
		f.t.Fatal("绑定 Cookie 的值不能就是 flow id")
	}
	return out
}

// poll polls once. A nil cookie means no cookie is sent.
func (f *ghFixture) poll(flowID string, cookie *http.Cookie) (*httptest.ResponseRecorder, map[string]any) {
	f.t.Helper()
	body, _ := json.Marshal(map[string]string{"flowId": flowID})
	req := httptest.NewRequest(http.MethodPost, PathGitHubPoll, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	f.record(rec)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func wantPending(t *testing.T, rec *httptest.ResponseRecorder, out map[string]any, interval int) {
	t.Helper()
	if rec.Code != http.StatusOK || out["status"] != "pending" || out["interval"] != float64(interval) {
		t.Fatalf("期望 pending interval=%d，得到 %d %v", interval, rec.Code, out)
	}
	if sessionCookie(rec) != nil {
		t.Fatal("pending 时不该种会话 Cookie")
	}
}

func wantCode(t *testing.T, rec *httptest.ResponseRecorder, out map[string]any, status int, code string) {
	t.Helper()
	if rec.Code != status || out["code"] != code {
		t.Fatalf("期望 %d %s，得到 %d %v", status, code, rec.Code, out)
	}
	if sessionCookie(rec) != nil {
		t.Fatal("失败时不该种会话 Cookie")
	}
}

func allowOcto() GitHubConfig { return GitHubConfig{AllowedUsers: []string{"octo-cat"}} }

// Main path: pending → slow_down (interval +5, persisted) → success; every step checks "did we
// really ask GitHub".
func TestGitHubDeviceFlowFull(t *testing.T) {
	gh := githubtest.New(t)
	gh.Script = []string{githubtest.Pending, githubtest.SlowDown, githubtest.OK}
	f := newGHFixture(t, gh, allowOcto())

	st := f.start()
	if st.UserCode != githubtest.UserCode || st.VerificationURI != "https://github.com/login/device" ||
		st.Interval != 5 || st.ExpiresAt != "2026-10-30T10:15:00.000Z" {
		t.Fatalf("发起响应不对: %+v", st)
	}
	if got := gh.Scope(); got != "read:user user:email" {
		t.Errorf("默认 scope 应是 read:user user:email，得到 %q", got)
	}

	// Poll right away: the interval has not passed, so GitHub must not be asked.
	rec, out := f.poll(st.FlowID, st.cookie)
	wantPending(t, rec, out, 5)
	if n := gh.TokenCalls(); n != 0 {
		t.Fatalf("没到间隔就去问了 GitHub %d 次", n)
	}

	// Time is up: GitHub says authorization_pending.
	f.advance(5 * time.Second)
	rec, out = f.poll(st.FlowID, st.cookie)
	wantPending(t, rec, out, 5)

	// Time is up again: slow_down → interval 10.
	f.advance(5 * time.Second)
	rec, out = f.poll(st.FlowID, st.cookie)
	wantPending(t, rec, out, 10)
	if n := gh.TokenCalls(); n != 2 {
		t.Fatalf("应问了 GitHub 2 次，实际 %d", n)
	}
	// The interval is persisted: a poll at the old interval (5 seconds) still does not ask GitHub.
	f.advance(5 * time.Second)
	rec, out = f.poll(st.FlowID, st.cookie)
	wantPending(t, rec, out, 10)
	if n := gh.TokenCalls(); n != 2 {
		t.Fatalf("slow_down 后按旧间隔轮询不该再问 GitHub，实际问了 %d 次", n)
	}

	// The new interval has passed: success.
	f.advance(5 * time.Second)
	rec, out = f.poll(st.FlowID, st.cookie)
	if rec.Code != http.StatusOK || out["status"] != "ok" {
		t.Fatalf("应登录成功，得到 %d %v", rec.Code, out)
	}
	cookie := sessionCookie(rec)
	if cookie == nil {
		t.Fatal("成功了却没种会话 Cookie")
	}
	id, err := f.svc.Resolve(t.Context(), cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	if id.Email != "octo@example.com" || id.Name != "Octo Cat" || id.AvatarURL != "https://avatars.example/octo.png" {
		t.Errorf("身份不对（邮箱要取 primary+verified 并小写）: %+v", id)
	}
	// The binding cookie is cleared.
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == st.cookie.Name && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("成功后应清掉绑定 Cookie")
	}
	// The row is deleted: polling again is invalid, and GitHub is not asked again.
	f.advance(time.Minute)
	rec, out = f.poll(st.FlowID, st.cookie)
	wantCode(t, rec, out, http.StatusGone, "github_expired")
	if n := gh.TokenCalls(); n != 3 {
		t.Errorf("成功之后不该再问 GitHub，实际共 %d 次", n)
	}
}

// When GitHub gives a larger interval with slow_down, use it.
func TestGitHubSlowDownUsesServerInterval(t *testing.T) {
	gh := githubtest.New(t)
	gh.Script = []string{githubtest.SlowDown}
	gh.SlowDownInterval = 30
	f := newGHFixture(t, gh, allowOcto())
	st := f.start()
	f.advance(5 * time.Second)
	rec, out := f.poll(st.FlowID, st.cookie)
	wantPending(t, rec, out, 30)
}

// Wrong flow id / no cookie / wrong cookie / expired: always 410, and GitHub is never asked.
func TestGitHubPollRejectsBadFlow(t *testing.T) {
	gh := githubtest.New(t)
	f := newGHFixture(t, gh, allowOcto())
	st := f.start()
	f.advance(5 * time.Second)

	rec, out := f.poll("not-a-flow", st.cookie)
	wantCode(t, rec, out, http.StatusGone, "github_expired")
	rec, out = f.poll(st.FlowID, nil)
	wantCode(t, rec, out, http.StatusGone, "github_expired")
	// Someone else got the flow id, but the cookie they hold is made up.
	rec, out = f.poll(st.FlowID, &http.Cookie{Name: st.cookie.Name, Value: "attacker-guess"})
	wantCode(t, rec, out, http.StatusGone, "github_expired")
	// A cookie from another login cannot be used to poll this one either.
	other := f.start()
	rec, out = f.poll(st.FlowID, &http.Cookie{Name: st.cookie.Name, Value: other.cookie.Value})
	wantCode(t, rec, out, http.StatusGone, "github_expired")
	if n := gh.TokenCalls(); n != 0 {
		t.Fatalf("认不出本人时不该去问 GitHub，实际问了 %d 次", n)
	}

	// Expired.
	f.advance(15 * time.Minute)
	rec, out = f.poll(st.FlowID, st.cookie)
	wantCode(t, rec, out, http.StatusGone, "github_expired")

	// A poll request that is not JSON (cross-site form) gets 400 right away.
	req := httptest.NewRequest(http.MethodPost, PathGitHubPoll, strings.NewReader("flowId="+st.FlowID))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非 JSON 轮询应 400，得到 %d", rec.Code)
	}
}

func TestGitHubAccessDenied(t *testing.T) {
	gh := githubtest.New(t)
	gh.Script = []string{githubtest.Denied}
	f := newGHFixture(t, gh, allowOcto())
	st := f.start()
	f.advance(5 * time.Second)
	rec, out := f.poll(st.FlowID, st.cookie)
	wantCode(t, rec, out, http.StatusForbidden, "github_denied")
	// After a denial the flow is void.
	f.advance(5 * time.Second)
	rec, out = f.poll(st.FlowID, st.cookie)
	wantCode(t, rec, out, http.StatusGone, "github_expired")
}

func TestGitHubExpiredToken(t *testing.T) {
	gh := githubtest.New(t)
	gh.Script = []string{githubtest.Expired}
	f := newGHFixture(t, gh, allowOcto())
	st := f.start()
	f.advance(5 * time.Second)
	rec, out := f.poll(st.FlowID, st.cookie)
	wantCode(t, rec, out, http.StatusGone, "github_expired")
}

func TestGitHubNoVerifiedPrimaryEmail(t *testing.T) {
	cases := map[string][]github.Email{
		"主邮箱没验证":   {{Email: "a@example.com", Primary: true, Verified: false}, {Email: "b@example.com", Verified: true}},
		"没有任何邮箱":   {},
		"已验证但非主邮箱": {{Email: "b@example.com", Primary: false, Verified: true}},
	}
	for name, emails := range cases {
		t.Run(name, func(t *testing.T) {
			gh := githubtest.New(t)
			gh.Emails = emails
			f := newGHFixture(t, gh, allowOcto())
			st := f.start()
			f.advance(5 * time.Second)
			rec, out := f.poll(st.FlowID, st.cookie)
			wantCode(t, rec, out, http.StatusForbidden, "github_no_verified_email")
		})
	}
}

// Admission: all allow lists empty = nobody gets in; with any of them set, "matching any one lets
// you in".
func TestGitHubAllowlist(t *testing.T) {
	cases := []struct {
		name  string
		cfg   GitHubConfig
		orgs  map[string]bool
		allow bool
	}{
		{"没配白名单一律拒绝", GitHubConfig{}, nil, false},
		{"用户名不在名单", GitHubConfig{AllowedUsers: []string{"someone-else"}}, nil, false},
		{"用户名命中（大小写不敏感）", GitHubConfig{AllowedUsers: []string{"OCTO-CAT"}}, nil, true},
		{"邮箱域名命中", GitHubConfig{AllowedEmailDomains: []string{"@Example.com"}}, nil, true},
		{"邮箱域名不命中", GitHubConfig{AllowedEmailDomains: []string{"other.com"}}, nil, false},
		{"组织成员", GitHubConfig{AllowedOrgs: []string{"Acme"}}, map[string]bool{"acme": true}, true},
		{"不是组织成员", GitHubConfig{AllowedOrgs: []string{"acme"}}, map[string]bool{"other": true}, false},
		{"多项里命中一项即可", GitHubConfig{AllowedUsers: []string{"x"}, AllowedOrgs: []string{"acme"}}, map[string]bool{"acme": true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := githubtest.New(t)
			if tc.orgs != nil {
				gh.Orgs = tc.orgs
			}
			f := newGHFixture(t, gh, tc.cfg)
			st := f.start()
			f.advance(5 * time.Second)
			rec, out := f.poll(st.FlowID, st.cookie)
			if tc.allow {
				if rec.Code != http.StatusOK || out["status"] != "ok" || sessionCookie(rec) == nil {
					t.Fatalf("应放行，得到 %d %v", rec.Code, out)
				}
				return
			}
			wantCode(t, rec, out, http.StatusForbidden, "github_not_allowed")
		})
	}
}

// Concurrent polls of the same flow create only one session (memory implementation; the PG one is
// in authdao).
func TestGitHubConcurrentPollOneSession(t *testing.T) {
	gh := githubtest.New(t)
	f := newGHFixture(t, gh, allowOcto())
	st := f.start()
	f.advance(5 * time.Second)

	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec, out := f.poll(st.FlowID, st.cookie)
			if rec.Code == http.StatusOK && out["status"] == "ok" {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("并发 %d 次轮询建了 %d 个会话，期望 1", n, ok)
	}
	if c := gh.TokenCalls(); c != 1 {
		t.Errorf("并发轮询应只问 GitHub 一次，实际 %d 次", c)
	}
	f.store.mu.RLock()
	sessions := len(f.store.sessions)
	f.store.mu.RUnlock()
	if sessions != 1 {
		t.Errorf("库里有 %d 个会话，期望 1", sessions)
	}
}

func TestGitHubDisabledAndProviders(t *testing.T) {
	svc := newTestService(t, nil, APIKey{})
	h, _ := NewHandler(HandlerConfig{Service: svc})
	mux := http.NewServeMux()
	h.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, PathGitHubDevice, nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "github_disabled") {
		t.Errorf("没开 GitHub 时发起应 404 github_disabled，得到 %d %s", rec.Code, rec.Body.String())
	}
	if p := svc.Providers(t.Context()); p.GitHub.Enabled {
		t.Error("没开 GitHub 时 providers 不该说启用")
	}

	f := newGHFixture(t, githubtest.New(t), GitHubConfig{DisplayName: "", AllowedUsers: []string{"x"}})
	p := f.svc.Providers(t.Context())
	if !p.GitHub.Enabled || p.GitHub.DisplayName != "GitHub" {
		t.Errorf("providers 里 github 不对: %+v", p.GitHub)
	}
}

// When GitHub is unavailable (wrong client_id / device flow not enabled), starting returns 502, not 500.
func TestGitHubStartUnavailable(t *testing.T) {
	gh := githubtest.New(t)
	bad, err := github.New(github.Config{ClientID: "wrong", WebURL: gh.URL(), APIURL: gh.URL()})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(Config{Store: NewMemStore(), GitHub: GitHubConfig{Client: bad, AllowedUsers: []string{"x"}}})
	if err != nil {
		t.Fatal(err)
	}
	h, _ := NewHandler(HandlerConfig{Service: svc})
	mux := http.NewServeMux()
	h.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, PathGitHubDevice, nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("GitHub 不可用时应 502，得到 %d %s", rec.Code, rec.Body.String())
	}
}
