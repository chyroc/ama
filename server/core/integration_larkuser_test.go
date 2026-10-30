// Tests for the Feishu user authorization provider.
//
// All offline: the fake Feishu runs on httptest (the two URLs in lark.Config are the injection
// points), and storage is an in-memory stub.
//
// The three most important groups of assertions, each aimed at a property only this provider has:
//
//  1. **Expiry triggers automatic refresh**, and the new refresh_token must be written back too
//     (Feishu rotates it);
//  2. **Refresh failures must be split into warning and error** — network jitter must not be
//     reported as "authorization has expired", which would make users scan the QR code again and
//     again (a credential's validity needs an "unknown" state);
//  3. **Credentials must not go into State** — that struct goes into the HTTP response as is.
package core

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/shared/infra/lark"
)

// ---------------------------------------------------------------------------
// Stubs
// ---------------------------------------------------------------------------

// fakeLarkTokenStore is an in-memory implementation of LarkUserTokenStore.
type fakeLarkTokenStore struct {
	tokens   map[string]persistmodel.LarkUserToken
	sessions map[string]persistmodel.LarkAuthSession
	// appPrefs is "which Feishu app I use by default", keyed by actor.
	appPrefs map[string]string
	// putErr, when non-nil, makes PutLarkUserToken fail; used to test "refresh succeeded but the database write failed".
	putErr error
	// puts counts the writes, used to assert "the refresh really was written back".
	puts int

	// now is the clock used to decide "has the pending auth session expired", and it **must come
	// from the same source as the fixed clock in the test**.
	//
	// ⚠️ This once used `time.Now()` directly, which was a time bomb: the test's fixed clock is
	// 2026-08-31T10:00Z and the session lasts 10 minutes, so **once real time passed 10:10Z,
	// GetLarkAuthSession treated this good session as expired**, and
	// TestLarkUserPollAuthorizationStoresToken was bound to fail after that moment — yet it
	// reported "this authorization flow has expired", which points at the code under test, not at
	// the fake store's clock. Zero value means use time.Now() (existing tests are not affected).
	now func() time.Time
}

// nowTime returns this fake store's clock, falling back to the real clock if none was injected.
func (f *fakeLarkTokenStore) nowTime() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

func newFakeLarkStore() *fakeLarkTokenStore {
	return &fakeLarkTokenStore{
		tokens:   map[string]persistmodel.LarkUserToken{},
		sessions: map[string]persistmodel.LarkAuthSession{},
		appPrefs: map[string]string{},
	}
}

func lkey(actor, app string) string { return actor + "\x00" + app }

func (f *fakeLarkTokenStore) GetLarkUserToken(
	_ context.Context, actor, app string,
) (persistmodel.LarkUserToken, error) {
	tok, ok := f.tokens[lkey(actor, app)]
	if !ok {
		return persistmodel.LarkUserToken{}, persistmodel.ErrNotFound
	}
	return tok, nil
}

func (f *fakeLarkTokenStore) PutLarkUserToken(_ context.Context, tok persistmodel.LarkUserToken) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.puts++
	f.tokens[lkey(tok.Actor, tok.LarkAppID)] = tok
	return nil
}

func (f *fakeLarkTokenStore) DeleteLarkUserToken(_ context.Context, actor, app string) error {
	delete(f.tokens, lkey(actor, app))
	return nil
}

func (f *fakeLarkTokenStore) ListLarkUserTokens(
	_ context.Context, actor string,
) ([]persistmodel.LarkUserToken, error) {
	out := []persistmodel.LarkUserToken{}
	for _, t := range f.tokens {
		if t.Actor == actor {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeLarkTokenStore) PutLarkAuthSession(
	_ context.Context, sess persistmodel.LarkAuthSession,
) error {
	f.sessions[lkey(sess.Actor, sess.LarkAppID)] = sess
	return nil
}

func (f *fakeLarkTokenStore) GetLarkAuthSession(
	_ context.Context, actor, app string,
) (persistmodel.LarkAuthSession, error) {
	sess, ok := f.sessions[lkey(actor, app)]
	if !ok {
		return persistmodel.LarkAuthSession{}, persistmodel.ErrNotFound
	}
	// Same as both real implementations: **expired counts as not existing**.
	// ⚠️ Use the injected clock, not time.Now() — see the fakeLarkTokenStore.now comment for why.
	if !sess.ExpiresAt.After(f.nowTime()) {
		return persistmodel.LarkAuthSession{}, persistmodel.ErrNotFound
	}
	return sess, nil
}

func (f *fakeLarkTokenStore) DeleteLarkAuthSession(_ context.Context, actor, app string) error {
	delete(f.sessions, lkey(actor, app))
	return nil
}

func (f *fakeLarkTokenStore) GetLarkUserAppPref(_ context.Context, actor string) (string, error) {
	appID, ok := f.appPrefs[actor]
	if !ok || appID == "" {
		return "", persistmodel.ErrNotFound
	}
	return appID, nil
}

func (f *fakeLarkTokenStore) PutLarkUserAppPref(_ context.Context, actor, appID string) error {
	f.appPrefs[actor] = appID
	return nil
}

// fakeAppResolver always returns the same pair of app credentials.
type fakeAppResolver struct {
	appID  string
	secret string
	err    error
}

func (f fakeAppResolver) ResolveApp(
	_ context.Context, _ apimodel.LarkAppQuery,
) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	return f.appID, f.secret, nil
}

func (f fakeAppResolver) ListUserApps(
	_ context.Context, _ string,
) ([]apimodel.LarkUserApp, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []apimodel.LarkUserApp{{AppID: f.appID, Name: "测试应用"}}, nil
}

// WorkspaceIDsFor makes this fake treat the actor as a member of every workspace — it is for the
// tests that "do not care about workspaces". Tests that care about membership checks use
// recordingAppResolver (its list is controllable).
func (f fakeAppResolver) WorkspaceIDsFor(_ context.Context, _ string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []string{"ws_a", "ws_b"}, nil
}

// recordingAppResolver records the query each ResolveApp call receives and returns a different
// app per workspace.
//
// Why it exists: **checking only that feishu.ResolveApp picks by workspace is not enough**. This
// flow has two levels (core's provider → feishu's resolver), and this bug was exactly in the
// **upper level** — the provider got workspaceID but did not pass it down. However correct the
// resolver side is, it does not help. (Lesson: a multi-level feature must be wired at every level.)
type recordingAppResolver struct {
	// byWorkspace maps workspaceID → appID. **The values are all different**, otherwise
	// "workspace passed" and "workspace not passed" return the same value and no difference shows.
	byWorkspace map[string]string
	// fallback is the one returned when there is no workspace context (or the workspace is unknown).
	fallback string
	// gotQueries records the query of each call, for assertions.
	gotQueries []apimodel.LarkAppQuery

	// memberOf is "which workspaces this person is a member of". **Empty means every key of
	// byWorkspace counts as a member**, so existing tests need not fill it in one by one; to test
	// "non-member is rejected", pass an explicit list that does not contain the target workspace.
	memberOf []string
	// membersErr makes WorkspaceIDsFor fail, to test "reject when it cannot be looked up".
	membersErr error
	// gotMemberChecks counts the membership checks, used to assert **the check happens before resolving**.
	gotMemberChecks int
}

func (r *recordingAppResolver) ResolveApp(
	_ context.Context, q apimodel.LarkAppQuery,
) (string, string, error) {
	r.gotQueries = append(r.gotQueries, q)
	if q.WorkspaceID != "" {
		appID, ok := r.byWorkspace[q.WorkspaceID]
		if !ok {
			// Same as the real implementation: **do not fall back to another workspace**.
			return "", "", errors.New("这个空间里没有可用的飞书应用")
		}
		return appID, "sec-" + appID, nil
	}
	// ⚠️ An empty fallback means "this person has no usable app at all", and then it **must return
	// an error**, same as the real implementation (feishu.ResolveApp returns ErrAppNotFound).
	// Returning ("", "", nil) would give the caller an empty app_id while thinking resolution
	// succeeded — the state would then go on to "look up the token for this empty app" and show
	// "not authorized yet", while the right answer is missing (an admin must connect an app first).
	if r.fallback == "" {
		return "", "", errors.New("这个人所在的空间里没有可用的飞书应用")
	}
	return r.fallback, "sec-" + r.fallback, nil
}

func (r *recordingAppResolver) ListUserApps(
	_ context.Context, _ string,
) ([]apimodel.LarkUserApp, error) {
	out := []apimodel.LarkUserApp{}
	for _, appID := range r.byWorkspace {
		out = append(out, apimodel.LarkUserApp{AppID: appID})
	}
	return out, nil
}

// WorkspaceIDsFor answers "which workspaces is this person in", for the membership check.
//
// When memberOf is empty, every key of byWorkspace counts as a member (the default expectation of
// existing tests).
func (r *recordingAppResolver) WorkspaceIDsFor(_ context.Context, _ string) ([]string, error) {
	r.gotMemberChecks++
	if r.membersErr != nil {
		return nil, r.membersErr
	}
	if r.memberOf != nil {
		return r.memberOf, nil
	}
	out := make([]string, 0, len(r.byWorkspace))
	for wsID := range r.byWorkspace {
		out = append(out, wsID)
	}
	return out, nil
}

// newLarkProviderWithFakeFeishu starts a fake Feishu and builds the provider.
//
// handler decides how Feishu responds; now fixes the clock (expiry logic is the core of this
// provider, and testing with the real clock only allows "roughly this number" assertions, which
// would not turn red even when the code is wrong).
//
// ⚠️ **The same now is also given to the fake store**: whether a pending auth session has expired
// is decided on the store side. If the two clocks come from different sources, the test suddenly
// starts failing after some real moment, and the error says "this authorization flow has
// expired" — pointing at the code under test, not at the clock.
func newLarkProviderWithFakeFeishu(
	t *testing.T, store LarkUserTokenStore, handler http.Handler, now time.Time,
) *LarkUserProvider {
	t.Helper()
	if fake, ok := store.(*fakeLarkTokenStore); ok && fake.now == nil {
		fake.now = func() time.Time { return now }
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client, err := lark.New(lark.Config{AccountsURL: srv.URL, OpenURL: srv.URL})
	if err != nil {
		t.Fatalf("建飞书客户端失败: %v", err)
	}
	p := NewLarkUserProvider(store,
		fakeAppResolver{appID: "cli_test", secret: "sec"}, client, nil, discardLogger())
	p.now = func() time.Time { return now }
	return p
}

// jsonHandler returns a fixed piece of JSON.
func jsonHandler(status int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

// ---------------------------------------------------------------------------
// The id and domain constants must match exactly
// ---------------------------------------------------------------------------

// ⭐ The provider's ID() and apimodel.IntegrationLarkUser **must match exactly**.
//
// A mismatch shows up as "checked but not injected": the agent stores the domain value, while
// injection decides by the provider's id — **with no error**; the user sees the checkbox on but
// the feature does not work.
func TestLarkUserProviderIDMatchesDomainConstant(t *testing.T) {
	p := NewLarkUserProvider(nil, nil, nil, nil, discardLogger())
	if p.ID() != apimodel.IntegrationLarkUser {
		t.Errorf("provider.ID() = %q，domain 常量 = %q，两者必须逐字相等",
			p.ID(), apimodel.IntegrationLarkUser)
	}
}

// ---------------------------------------------------------------------------
// The four states
// ---------------------------------------------------------------------------

// Not wired (Feishu not connected) → missing, and **no authorize button**.
//
// missing and "not authorized" are two different things: the former needs an admin to configure
// it, and the user clicking an authorize button does nothing.
func TestLarkUserStateMissingWhenNotConfigured(t *testing.T) {
	p := NewLarkUserProvider(nil, nil, nil, nil, discardLogger())
	st := p.State(context.Background(), "alice@example.com")
	if st.Status != IntegrationMissing {
		t.Errorf("status = %q，想要 missing", st.Status)
	}
	if st.CanAuthorize {
		t.Error("missing 时不该给「发起授权」按钮——点了只会 422，而用户无从得知要去配服务端")
	}
	// ⚠️ Even when missing it must be the injection kind: that way it shows up in the agent's
	// authorization checkbox area (disabled + a hint), instead of disappearing entirely.
	if !st.Injectable {
		t.Error("Injectable 必须为 true，否则这张卡片在 agent 表单里整个消失")
	}
}

// Wired but never authorized → error (**not** missing), with an authorize button.
//
// One click by the user fixes it → it should be error; missing means "clicking will not help".
func TestLarkUserStateErrorWhenNeverAuthorized(t *testing.T) {
	p := newLarkProviderWithFakeFeishu(t, newFakeLarkStore(),
		jsonHandler(http.StatusOK, `{}`), time.Now())
	st := p.State(context.Background(), "alice@example.com")
	if st.Status != IntegrationError {
		t.Errorf("status = %q，想要 error（用户点一下就能解决）", st.Status)
	}
	if !st.CanAuthorize {
		t.Error("没授权时必须给「发起授权」按钮——那是这个源的主路径")
	}
}

// Authorized and valid → ok, and can be revoked.
func TestLarkUserStateOKWhenValid(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	store := newFakeLarkStore()
	store.tokens[lkey("alice@example.com", "cli_test")] = persistmodel.LarkUserToken{
		Actor: "alice@example.com", LarkAppID: "cli_test",
		AccessToken: "u-at", RefreshToken: "u-rt",
		ExpiresAt:        now.Add(2 * time.Hour),
		RefreshExpiresAt: now.Add(7 * 24 * time.Hour),
		Scope:            lark.ScopeOfflineAccess + " " + lark.ScopeUserReadGroupMessage,
		UserName:         "Carol", OpenID: "ou_abc",
	}
	p := newLarkProviderWithFakeFeishu(t, store, jsonHandler(http.StatusOK, `{}`), now)

	st := p.State(context.Background(), "alice@example.com")
	if st.Status != IntegrationOK {
		t.Errorf("status = %q，想要 ok", st.Status)
	}
	// With credentials it can really revoke (after deletion OAuth must be redone), unlike the fake revoke of the AMA provider.
	if !st.CanRevoke {
		t.Error("有凭据时该能撤销")
	}
}

// ⭐⭐ **Credentials must never appear in any field of State.**
//
// State goes into the HTTP response and the browser as is. This test sets both tokens to
// easy-to-spot values, then scans every string field of State (including every item of Details).
func TestLarkUserStateNeverLeaksCredentials(t *testing.T) {
	const secretAccess = "u-at-SECRET-DO-NOT-LEAK-0001"
	const secretRefresh = "ur-rt-SECRET-DO-NOT-LEAK-0002"
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)

	store := newFakeLarkStore()
	store.tokens[lkey("alice@example.com", "cli_test")] = persistmodel.LarkUserToken{
		Actor: "alice@example.com", LarkAppID: "cli_test",
		AccessToken: secretAccess, RefreshToken: secretRefresh,
		ExpiresAt:        now.Add(2 * time.Hour),
		RefreshExpiresAt: now.Add(7 * 24 * time.Hour),
		UserName:         "Carol",
	}
	p := newLarkProviderWithFakeFeishu(t, store, jsonHandler(http.StatusOK, `{}`), now)
	st := p.State(context.Background(), "alice@example.com")

	fields := []string{st.Summary, st.FixHint, st.FixURL, st.Name, st.Description}
	for _, d := range st.Details {
		fields = append(fields, d.Label, d.Value)
	}
	for _, f := range fields {
		for _, bad := range []string{secretAccess, secretRefresh} {
			if strings.Contains(f, bad) {
				t.Errorf("❌ State 里出现了明文凭据（它会进 HTTP 响应与浏览器）：%q", f)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Automatic refresh
// ---------------------------------------------------------------------------

// ⭐ When the access_token is about to expire it is **refreshed automatically**, and the new
// refresh_token must be written back too.
//
// Feishu rotates the refresh_token. Storing only the access token means the next refresh uses the
// old one, which may already be invalid — it shows up as "after a while of refreshing, everything
// suddenly needs re-authorization".
func TestLarkUserAccessTokenRefreshesAndPersistsRotatedRefresh(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	store := newFakeLarkStore()
	store.tokens[lkey("alice@example.com", "cli_test")] = persistmodel.LarkUserToken{
		Actor: "alice@example.com", LarkAppID: "cli_test",
		AccessToken: "u-at-OLD", RefreshToken: "ur-rt-OLD",
		// **Already expired** (before now), must trigger a refresh.
		ExpiresAt:        now.Add(-time.Minute),
		RefreshExpiresAt: now.Add(7 * 24 * time.Hour),
	}
	p := newLarkProviderWithFakeFeishu(t, store, jsonHandler(http.StatusOK, `{
		"access_token": "u-at-NEW",
		"refresh_token": "ur-rt-NEW",
		"expires_in": 7200,
		"refresh_token_expires_in": 604800
	}`), now)

	token, appID, err := p.UserAccessToken(context.Background(), "alice@example.com", "")
	if err != nil {
		t.Fatalf("取凭据失败: %v", err)
	}
	if token != "u-at-NEW" {
		t.Errorf("返回的该是续期后的 token，实际 %q", token)
	}
	if appID != "cli_test" {
		t.Errorf("appID = %q，想要 cli_test（lark-cli 要它与 token 成对）", appID)
	}
	// Written back, and **the new refresh token was stored too**.
	saved := store.tokens[lkey("alice@example.com", "cli_test")]
	if saved.RefreshToken != "ur-rt-NEW" {
		t.Errorf("轮换后的 refresh_token 没写回：%q", saved.RefreshToken)
	}
	if !saved.ExpiresAt.Equal(now.Add(2 * time.Hour)) {
		t.Errorf("过期时刻该按 expires_in 重算，实际 %v", saved.ExpiresAt)
	}
	if saved.LastRefreshError != "" {
		t.Errorf("续期成功后该清掉上次的失败记录，实际 %q", saved.LastRefreshError)
	}
}

// Before the refresh window it **should not** call Feishu — this is the hot path every command goes through.
func TestLarkUserAccessTokenSkipsRefreshWhenFresh(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	store := newFakeLarkStore()
	store.tokens[lkey("alice@example.com", "cli_test")] = persistmodel.LarkUserToken{
		Actor: "alice@example.com", LarkAppID: "cli_test",
		AccessToken: "u-at-FRESH", RefreshToken: "ur-rt",
		ExpiresAt:        now.Add(2 * time.Hour),
		RefreshExpiresAt: now.Add(7 * 24 * time.Hour),
	}
	called := false
	p := newLarkProviderWithFakeFeishu(t, store,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"should-not-be-used","expires_in":7200}`))
		}), now)

	token, _, err := p.UserAccessToken(context.Background(), "alice@example.com", "")
	if err != nil {
		t.Fatalf("取凭据失败: %v", err)
	}
	if token != "u-at-FRESH" {
		t.Errorf("该直接用库里那把，实际 %q", token)
	}
	if called {
		t.Error("没到续期窗口不该打飞书——这条路径每次执行命令都会走")
	}
}

// ⭐⭐ **Refresh failures come in two kinds: terminal clears the credentials, non-terminal keeps them.**
//
// This is where the "credential validity needs an unknown state" lesson lands: treating network
// jitter as invalid would make users be asked again and again to rescan, and each time after
// scanning it "works again for a while".
func TestLarkUserAccessTokenDistinguishesRevokedFromTransient(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	expired := persistmodel.LarkUserToken{
		Actor: "alice@example.com", LarkAppID: "cli_test",
		AccessToken: "u-at-OLD", RefreshToken: "ur-rt-OLD",
		ExpiresAt:        now.Add(-time.Minute),
		RefreshExpiresAt: now.Add(7 * 24 * time.Hour),
	}

	t.Run("invalid_grant 是真失效：清掉凭据", func(t *testing.T) {
		store := newFakeLarkStore()
		store.tokens[lkey("alice@example.com", "cli_test")] = expired
		p := newLarkProviderWithFakeFeishu(t, store, jsonHandler(http.StatusBadRequest,
			`{"error":"invalid_grant","code":20026}`), now)

		if _, _, err := p.UserAccessToken(context.Background(), "alice@example.com", ""); err == nil {
			t.Fatal("该报错")
		}
		if _, ok := store.tokens[lkey("alice@example.com", "cli_test")]; ok {
			t.Error("授权真失效时该把凭据清掉，让用户重新授权")
		}
	})

	t.Run("网络抖动：保留凭据，只记一条失败原因", func(t *testing.T) {
		store := newFakeLarkStore()
		store.tokens[lkey("alice@example.com", "cli_test")] = expired
		// 5xx and the body is not a protocol error → transport error → non-terminal.
		p := newLarkProviderWithFakeFeishu(t, store,
			jsonHandler(http.StatusBadGateway, `<html>502</html>`), now)

		if _, _, err := p.UserAccessToken(context.Background(), "alice@example.com", ""); err == nil {
			t.Fatal("这次取不到，该报错")
		}
		saved, ok := store.tokens[lkey("alice@example.com", "cli_test")]
		if !ok {
			t.Fatal("❌ 一次网络抖动不该毁掉一条好凭据——那会让用户反复重新授权")
		}
		if saved.RefreshToken != "ur-rt-OLD" {
			t.Error("refresh_token 该原样留着")
		}
		if saved.LastRefreshError == "" {
			t.Error("该记下失败原因，好让卡片显示 warning")
		}
		// ⚠️ The failure reason must not contain credentials (it goes into the HTTP response via State).
		for _, bad := range []string{"ur-rt-OLD", "u-at-OLD"} {
			if strings.Contains(saved.LastRefreshError, bad) {
				t.Errorf("失败原因里不许有凭据：%q", saved.LastRefreshError)
			}
		}
	})
}

// ⭐ Last refresh failed (but refresh has not expired) → the card is **warning**, not error.
//
// The line between warning and error is "does the user need to act now". In this case they do
// not; the platform retries automatically next time. Showing error would send them to
// re-authorize, which is the wrong action.
func TestLarkUserStateWarningAfterTransientRefreshFailure(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	store := newFakeLarkStore()
	store.tokens[lkey("alice@example.com", "cli_test")] = persistmodel.LarkUserToken{
		Actor: "alice@example.com", LarkAppID: "cli_test",
		AccessToken: "u-at", RefreshToken: "u-rt",
		ExpiresAt:        now.Add(time.Hour),
		RefreshExpiresAt: now.Add(7 * 24 * time.Hour),
		LastRefreshError: "续期请求没打通，稍后会自动重试",
	}
	p := newLarkProviderWithFakeFeishu(t, store, jsonHandler(http.StatusOK, `{}`), now)

	st := p.State(context.Background(), "alice@example.com")
	if st.Status != IntegrationWarning {
		t.Errorf("status = %q，想要 warning——一次续期失败不该被说成「已失效」", st.Status)
	}
	// Still usable, so the user should not be pushed to act.
	if strings.Contains(st.FixHint, "必须") {
		t.Errorf("warning 的提示不该催用户动手：%q", st.FixHint)
	}
}

// The refresh_token has expired too → error, must re-authorize.
func TestLarkUserStateErrorWhenRefreshExpired(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	store := newFakeLarkStore()
	store.tokens[lkey("alice@example.com", "cli_test")] = persistmodel.LarkUserToken{
		Actor: "alice@example.com", LarkAppID: "cli_test",
		AccessToken: "u-at", RefreshToken: "u-rt",
		ExpiresAt:        now.Add(-time.Hour),
		RefreshExpiresAt: now.Add(-time.Minute), // also expired
	}
	p := newLarkProviderWithFakeFeishu(t, store, jsonHandler(http.StatusOK, `{}`), now)

	st := p.State(context.Background(), "alice@example.com")
	if st.Status != IntegrationError {
		t.Errorf("status = %q，想要 error（自动续期救不回来了）", st.Status)
	}
	if !st.CanAuthorize {
		t.Error("该给授权按钮")
	}
}

// ---------------------------------------------------------------------------
// Authorize and revoke
// ---------------------------------------------------------------------------

// Start authorization: return the authorization URL and **save** the device_code (with multiple instances, polling may land in another process).
func TestLarkUserAuthorizeURLPersistsDeviceCode(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	store := newFakeLarkStore()
	p := newLarkProviderWithFakeFeishu(t, store, jsonHandler(http.StatusOK, `{
		"device_code": "dc_abc",
		"user_code": "ABCD",
		"verification_uri_complete": "https://accounts.feishu.cn/device?user_code=ABCD",
		"expires_in": 3600,
		"interval": 5
	}`), now)

	url, err := p.AuthorizeURL(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("发起授权失败: %v", err)
	}
	if !strings.Contains(url, "user_code=ABCD") {
		t.Errorf("该返回带 user_code 的地址，实际 %q", url)
	}
	sess, ok := store.sessions[lkey("alice@example.com", "cli_test")]
	if !ok {
		t.Fatal("❌ device_code 必须落库：线上多实例，轮询那次请求可能落在别的进程上")
	}
	if sess.DeviceCode != "dc_abc" {
		t.Errorf("device_code = %q", sess.DeviceCode)
	}
	if !sess.ExpiresAt.After(now) {
		t.Error("过期时刻必须晚于现在，否则用户刚拿到链接就被判作废")
	}
}

// Polling and getting "still waiting" is not an error.
func TestLarkUserPollAuthorizationPendingIsNotAnError(t *testing.T) {
	now := time.Now()
	store := newFakeLarkStore()
	store.sessions[lkey("alice@example.com", "cli_test")] = persistmodel.LarkAuthSession{
		Actor: "alice@example.com", LarkAppID: "cli_test",
		DeviceCode: "dc", PollInterval: 5,
		ExpiresAt: now.Add(10 * time.Minute),
	}
	p := newLarkProviderWithFakeFeishu(t, store,
		jsonHandler(http.StatusBadRequest, `{"error":"authorization_pending"}`), now)

	st, err := p.PollAuthorization(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("还在等用户点同意不该是 error：%v", err)
	}
	if !st.Pending || st.Done {
		t.Errorf("该是 pending，实际 %+v", st)
	}
}

// ⭐ Polling succeeds: store the credentials and **clear the intermediate state**.
func TestLarkUserPollAuthorizationStoresToken(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	store := newFakeLarkStore()
	store.sessions[lkey("alice@example.com", "cli_test")] = persistmodel.LarkAuthSession{
		Actor: "alice@example.com", LarkAppID: "cli_test",
		DeviceCode: "dc", PollInterval: 5,
		ExpiresAt: now.Add(10 * time.Minute),
	}
	p := newLarkProviderWithFakeFeishu(t, store, jsonHandler(http.StatusOK, `{
		"access_token": "u-at-fresh",
		"refresh_token": "ur-rt-fresh",
		"expires_in": 7200,
		"refresh_token_expires_in": 604800,
		"scope": "offline_access im:message.group_msg:get_as_user"
	}`), now)

	st, err := p.PollAuthorization(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("轮询失败: %v", err)
	}
	if !st.Done {
		t.Errorf("该是 done，实际 %+v", st)
	}
	saved, ok := store.tokens[lkey("alice@example.com", "cli_test")]
	if !ok {
		t.Fatal("凭据没存下来")
	}
	if saved.AccessToken != "u-at-fresh" || saved.RefreshToken != "ur-rt-fresh" {
		t.Errorf("存错了：%+v", saved)
	}
	// Relative seconds must be converted to an absolute time.
	if !saved.ExpiresAt.Equal(now.Add(2 * time.Hour)) {
		t.Errorf("ExpiresAt = %v，想要 now+2h", saved.ExpiresAt)
	}
	// The intermediate state should be cleared.
	if _, still := store.sessions[lkey("alice@example.com", "cli_test")]; still {
		t.Error("授权完成后该清掉设备码中间态")
	}
}

// ⭐ When authorization succeeds but **the permission to read group messages was not granted**, say
// so right away.
//
// Otherwise the agent is rejected as soon as it reads group history, and that rejection points at
// permissions, not at "one item was missing when authorizing".
func TestLarkUserPollAuthorizationWarnsWhenGroupScopeMissing(t *testing.T) {
	now := time.Now()
	store := newFakeLarkStore()
	store.sessions[lkey("alice@example.com", "cli_test")] = persistmodel.LarkAuthSession{
		Actor: "alice@example.com", LarkAppID: "cli_test",
		DeviceCode: "dc", ExpiresAt: now.Add(10 * time.Minute),
	}
	// Only offline_access was granted, not im:message.group_msg:get_as_user.
	p := newLarkProviderWithFakeFeishu(t, store, jsonHandler(http.StatusOK, `{
		"access_token": "u-at", "refresh_token": "ur-rt",
		"expires_in": 7200, "refresh_token_expires_in": 604800,
		"scope": "offline_access"
	}`), now)

	st, err := p.PollAuthorization(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("轮询失败: %v", err)
	}
	if !st.Done {
		t.Fatal("授权本身是成功的")
	}
	if !strings.Contains(st.Message, "读群消息") {
		t.Errorf("缺读群权限时该就地说出来，实际 %q", st.Message)
	}
}

// When there is no pending authorization flow (or it expired), tell the user to "start again" instead of a vague error.
func TestLarkUserPollAuthorizationWithoutSessionTellsUserToRestart(t *testing.T) {
	p := newLarkProviderWithFakeFeishu(t, newFakeLarkStore(),
		jsonHandler(http.StatusOK, `{}`), time.Now())
	st, err := p.PollAuthorization(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("这不该是 error（用户只是没在授权流程里）：%v", err)
	}
	if st.Done || st.Pending {
		t.Errorf("既没完成也不该是 pending，实际 %+v", st)
	}
	if !strings.Contains(st.Message, "重新") {
		t.Errorf("该提示重新发起，实际 %q", st.Message)
	}
}

// Revoke is a real revoke: delete the row; after that re-authorization is required.
func TestLarkUserRevokeDeletesToken(t *testing.T) {
	store := newFakeLarkStore()
	store.tokens[lkey("alice@example.com", "cli_test")] = persistmodel.LarkUserToken{
		Actor: "alice@example.com", LarkAppID: "cli_test", AccessToken: "u-at",
	}
	p := newLarkProviderWithFakeFeishu(t, store, jsonHandler(http.StatusOK, `{}`), time.Now())

	if err := p.Revoke(context.Background(), "alice@example.com"); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if _, ok := store.tokens[lkey("alice@example.com", "cli_test")]; ok {
		t.Error("撤销后库里不该还有凭据")
	}
	// Idempotent: revoking again is not an error.
	if err := p.Revoke(context.Background(), "alice@example.com"); err != nil {
		t.Errorf("重复撤销该幂等，实际 %v", err)
	}
}

// Getting credentials when never authorized must return ErrPrecondition, with a hint pointing at what the user can do.
func TestLarkUserAccessTokenWithoutAuthorizationIsPrecondition(t *testing.T) {
	p := newLarkProviderWithFakeFeishu(t, newFakeLarkStore(),
		jsonHandler(http.StatusOK, `{}`), time.Now())
	_, _, err := p.UserAccessToken(context.Background(), "alice@example.com", "")
	if !errors.Is(err, ErrPrecondition) {
		t.Errorf("该是 ErrPrecondition，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "授权") {
		t.Errorf("提示该指向「去做授权」，实际 %q", err.Error())
	}
}

// ---------------------------------------------------------------------------
// ⭐⭐ workspaceID must be passed all the way to the app selection step
// ---------------------------------------------------------------------------

// This guards **the wiring of this level**, and is a pair with the group of tests on the feishu
// side; both are required:
//
//	feishu/apps_userresolver_test.go   the resolver picks by WorkspaceID once it receives it
//	here                                the provider really passes workspaceID down
//
// If the upper level does not pass it, however correct the lower level is, it does not help — and
// that is exactly the shape of this bug: addLarkUserEnvs clearly had workspaceID in its input, but
// only passed owner down. (Lesson: a multi-level feature must be wired at every level.)
//
// Mutation check: change `WorkspaceID: workspaceID` in UserAccessToken to `""` (or back to
// p.resolveApp(ctx, actor)), and this turns red with (translated):
//
//	getting the token in workspace ws_c should use cli_c, but cli_a was used
func TestUserAccessTokenPinsAppToRunWorkspace(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	const actor = "alice@example.com"

	resolver := &recordingAppResolver{
		byWorkspace: map[string]string{
			"ws_a": "cli_a",
			"ws_c": "cli_c",
		},
		// This is what you get when it "guesses" — the three values all differ, so a mismatch always shows.
		fallback: "cli_a",
	}
	store := newFakeLarkStore()
	// **Each of the two workspaces has authorized once**, with different token values: comparing
	// only appID would let a half-wrong implementation ("right app but wrong token row") pass.
	for ws, appID := range resolver.byWorkspace {
		_ = ws
		store.tokens[lkey(actor, appID)] = persistmodel.LarkUserToken{
			Actor: actor, LarkAppID: appID,
			AccessToken:      "token-for-" + appID,
			RefreshToken:     "refresh-for-" + appID,
			ExpiresAt:        now.Add(2 * time.Hour),
			RefreshExpiresAt: now.Add(7 * 24 * time.Hour),
		}
	}

	srv := httptest.NewServer(jsonHandler(http.StatusOK, `{}`))
	t.Cleanup(srv.Close)
	client, err := lark.New(lark.Config{AccountsURL: srv.URL, OpenURL: srv.URL})
	if err != nil {
		t.Fatalf("建飞书客户端失败: %v", err)
	}
	p := NewLarkUserProvider(store, resolver, client, nil, discardLogger())
	p.now = func() time.Time { return now }

	// ⭐ Run in workspace ws_c — it is **not** the fallback one.
	token, appID, err := p.UserAccessToken(context.Background(), actor, "ws_c")
	if err != nil {
		t.Fatalf("取 token 失败: %v", err)
	}
	if appID != "cli_c" {
		t.Errorf("在空间 ws_c 里取 token 应当用 cli_c，实际用了 %s\n"+
			"→ 这就是跨空间错配：lark-cli 会拿到 99991668 token invalid，"+
			"或者侥幸有效但读不到那个群，而两种报错都不指向「选错了应用」", appID)
	}
	if token != "token-for-cli_c" {
		t.Errorf("token 也要跟着应用走，应当是 token-for-cli_c，实际 %s", token)
	}

	// Assert directly on the query passed down: even if both checks above pass, that could come
	// from "workspace passed but unused downstream" plus "workspace not passed but guessed right".
	if len(resolver.gotQueries) == 0 {
		t.Fatal("provider 压根没调用 ResolveApp")
	}
	last := resolver.gotQueries[len(resolver.gotQueries)-1]
	if last.WorkspaceID != "ws_c" {
		t.Errorf("传给 ResolveApp 的 WorkspaceID 应当是 ws_c，实际 %q\n"+
			"→ 注入点手上有 workspaceID，丢掉它就等于让下游去猜", last.WorkspaceID)
	}
	if last.Actor != actor {
		t.Errorf("Actor 应当是 %s，实际 %q", actor, last.Actor)
	}
}

// "This workspace's app is not authorized yet" must return a clear error, and **must not fall
// back to another workspace's token**.
//
// Falling back turns a problem the user can fix with one click (go authorize) into a silent
// mismatch. And the error message must include the app_id: this person may already have
// authorized **another** app, and saying only "Feishu user authorization not done yet" would
// leave them confused in front of a card showing "authorized".
func TestUserAccessTokenDoesNotFallBackToAnotherWorkspaceToken(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	const actor = "alice@example.com"

	resolver := &recordingAppResolver{
		byWorkspace: map[string]string{"ws_a": "cli_a", "ws_c": "cli_c"},
		fallback:    "cli_a",
	}
	store := newFakeLarkStore()
	// **Only the ws_a app was authorized**; the ws_c one was not.
	store.tokens[lkey(actor, "cli_a")] = persistmodel.LarkUserToken{
		Actor: actor, LarkAppID: "cli_a",
		AccessToken:      "token-for-cli_a",
		RefreshToken:     "refresh-for-cli_a",
		ExpiresAt:        now.Add(2 * time.Hour),
		RefreshExpiresAt: now.Add(7 * 24 * time.Hour),
	}

	srv := httptest.NewServer(jsonHandler(http.StatusOK, `{}`))
	t.Cleanup(srv.Close)
	client, err := lark.New(lark.Config{AccountsURL: srv.URL, OpenURL: srv.URL})
	if err != nil {
		t.Fatalf("建飞书客户端失败: %v", err)
	}
	p := NewLarkUserProvider(store, resolver, client, nil, discardLogger())
	p.now = func() time.Time { return now }

	token, _, err := p.UserAccessToken(context.Background(), actor, "ws_c")
	if err == nil {
		t.Fatalf("ws_c 那个应用没授权过，应当明确失败，却拿到了 token %q\n"+
			"→ 回落到 ws_a 那把 token 是静默错配：它对 cli_c 无效", token)
	}
	if !errors.Is(err, ErrPrecondition) {
		t.Errorf("应当是 ErrPrecondition（用户去授权就能解决），实际 %v", err)
	}
	// ⭐ The error message must name which app it is.
	if !strings.Contains(err.Error(), "cli_c") {
		t.Errorf("错误信息里应当带上是哪个应用没授权，实际 %q\n"+
			"→ 这个人已经对 cli_a 授权过了，不说清的话他会对着一张"+
			"显示「已授权」的卡片发懵", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Start authorization / polling: pick the app by workspace
//
// This group guards something **symmetric** to the two UserAccessToken tests: injection is pinned
// by workspace, while **starting authorization used to ignore the workspace** (only the
// account-level default). So clicking "start authorization" on the agent page might authorize
// another workspace's app, while at runtime the error is "user authorization not done for Feishu
// app cli_xxx" — which does not point to "you authorized the wrong one".
// ---------------------------------------------------------------------------

// newScopedProvider builds a provider with a recordingAppResolver, for this group.
//
// The Feishu side always returns a successful device code response, so the tests only need to
// care about "which app was picked".
func newScopedProvider(
	t *testing.T, store *fakeLarkTokenStore, resolver *recordingAppResolver, now time.Time,
) *LarkUserProvider {
	t.Helper()
	// ⚠️ The field name is `verification_uri` (OAuth device code spec), **not `_url`**.
	// If misspelled, BeginUserAuth returns "response is missing the authorization URL", an error
	// that points at the code under test rather than the fixture.
	srv := httptest.NewServer(jsonHandler(http.StatusOK, `{
		"device_code": "dc-1",
		"verification_uri": "https://example.com/device",
		"user_code": "ABCD-1234",
		"expires_in": 600,
		"interval": 5
	}`))
	t.Cleanup(srv.Close)
	client, err := lark.New(lark.Config{AccountsURL: srv.URL, OpenURL: srv.URL})
	if err != nil {
		t.Fatalf("建飞书客户端失败: %v", err)
	}
	if store.now == nil {
		store.now = func() time.Time { return now }
	}
	p := NewLarkUserProvider(store, resolver, client, nil, discardLogger())
	p.now = func() time.Time { return now }
	return p
}

// twoWorkspaceResolver builds a resolver with "one app per workspace, two workspaces".
//
// ⚠️ The three values (the two workspaces' apps + fallback) **all differ**, otherwise "workspace
// passed" and "workspace not passed" would return the same value and a wrong implementation would
// still pass.
func twoWorkspaceResolver() *recordingAppResolver {
	return &recordingAppResolver{
		byWorkspace: map[string]string{"ws_a": "cli_a", "ws_b": "cli_b"},
		fallback:    "cli_a",
	}
}

// ⭐ Starting authorization picks the app by workspace, not by the account-level default.
//
// Mutation check: replace resolveAppScoped in AuthorizeURLForWorkspace with resolveApp (or pass
// WorkspaceID as ""), and this turns red with (translated):
//
//	starting authorization in workspace ws_b should use cli_b, but it was recorded on cli_a
func TestAuthorizeURLPinsAppToRequestedWorkspace(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	const actor = "alice@example.com"
	resolver := twoWorkspaceResolver()
	store := newFakeLarkStore()
	p := newScopedProvider(t, store, resolver, now)

	if _, err := p.AuthorizeURLForWorkspace(context.Background(), actor, "ws_b"); err != nil {
		t.Fatalf("发起授权失败: %v", err)
	}

	// Assert that the saved pending auth session is recorded on **workspace ws_b's app**.
	//
	// Check the session instead of only gotQueries: which app the device_code is recorded on
	// decides whether polling can find it again, and that is the real load-bearing point of this
	// flow.
	if _, ok := store.sessions[lkey(actor, "cli_b")]; !ok {
		var got []string
		for k := range store.sessions {
			got = append(got, k)
		}
		t.Errorf("在空间 ws_b 里发起授权应当用 cli_b，实际记在了 %v 上\n"+
			"→ 这就是「从 agent 页授权却授权了别的空间那个应用」：agent 仍然用不了 lark-cli，"+
			"而运行时报的是「还没对飞书应用 cli_b 做过用户授权」，那句话不指向「授权错了一个」", got)
	}
	if len(resolver.gotQueries) == 0 {
		t.Fatal("压根没调用 ResolveApp")
	}
	last := resolver.gotQueries[len(resolver.gotQueries)-1]
	if last.WorkspaceID != "ws_b" {
		t.Errorf("传给 ResolveApp 的 WorkspaceID 应当是 ws_b，实际 %q", last.WorkspaceID)
	}
}

// Without a workspace, the behavior is exactly the same as before — **the authorization management
// page's path must not be broken by this change**.
//
// That page is account-level (it has no workspace context at all), so it must keep using the
// fallback.
func TestAuthorizeURLWithoutWorkspaceStaysAccountLevel(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	const actor = "alice@example.com"
	resolver := twoWorkspaceResolver()
	store := newFakeLarkStore()
	p := newScopedProvider(t, store, resolver, now)

	// Use the method on the interface (IntegrationProvider.AuthorizeURL); it is a thin wrapper passing an empty string.
	if _, err := p.AuthorizeURL(context.Background(), actor); err != nil {
		t.Fatalf("发起授权失败: %v", err)
	}
	if _, ok := store.sessions[lkey(actor, "cli_a")]; !ok {
		t.Errorf("不带空间时应当用账号级默认的 cli_a，实际没记在它上面")
	}
	if len(resolver.gotQueries) == 0 {
		t.Fatal("压根没调用 ResolveApp")
	}
	if got := resolver.gotQueries[0].WorkspaceID; got != "" {
		t.Errorf("不带空间时 WorkspaceID 应当是空串，实际 %q\n"+
			"→ 凭空钉一个空间会让授权管理页在「用户不在那个空间」时整页报错", got)
	}
}

// ⭐⭐ Security assertion: naming a workspace **you are not a member of** must be rejected, and
// **it must never be used to query the database**.
//
// This route is ActionNone (Guard cannot check a workspace in the query; see authz/matrix.go),
// so this check is the only gate.
//
// Mutation check: move ensureWorkspaceMember in resolveAppScoped after ResolveApp (or delete it
// entirely), and this turns red with (translated):
//
//	a non-member's workspaceId should not be handed to ResolveApp, but it was called 1 time
func TestAuthorizeURLRejectsWorkspaceUserIsNotMemberOf(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	const actor = "alice@example.com"
	resolver := twoWorkspaceResolver()
	// They are only in ws_a; ws_b belongs to someone else.
	resolver.memberOf = []string{"ws_a"}
	store := newFakeLarkStore()
	p := newScopedProvider(t, store, resolver, now)

	_, err := p.AuthorizeURLForWorkspace(context.Background(), actor, "ws_b")
	if err == nil {
		t.Fatal("指名一个自己不是成员的空间应当被拒")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("应当是 ErrNotFound（404），实际 %v\n"+
			"→ 用 403 会给出预言机：非成员能借此分辨「这个空间不存在」与"+
			"「存在但还没接入飞书应用」", err)
	}
	// ⭐ The check must happen **before** resolving: calling ResolveApp means that id already entered the database query path.
	if len(resolver.gotQueries) != 0 {
		t.Errorf("非成员的 workspaceId 不该被交给 ResolveApp，实际调了 %d 次\n"+
			"→ feishu 那侧的 workspace 分支直接查库、不查成员（对可信的注入链路是刻意的），"+
			"所以顺序错了就等于让人拿别人的 workspaceId 去探", len(resolver.gotQueries))
	}
	// No pending authorization flow should be left either.
	if len(store.sessions) != 0 {
		t.Errorf("被拒之后不该留下待授权会话，实际有 %d 条", len(store.sessions))
	}
	// ⚠️ The message must not reveal anything about that workspace.
	if strings.Contains(err.Error(), "飞书应用") {
		t.Errorf("错误信息不该提「有没有飞书应用」，实际 %q", err.Error())
	}
}

// When the workspace has no usable app, fail clearly, **do not fall back to the account-level
// default**, and leave no pending authorization flow.
//
// Falling back is the most tempting and most wrong choice: it turns "this workspace has not
// connected a Feishu app yet" into a silent mismatch.
func TestAuthorizeURLWorkspaceWithoutAppDoesNotFallBack(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	const actor = "alice@example.com"
	resolver := twoWorkspaceResolver()
	// They are a member of ws_empty, but that workspace has no Feishu app.
	resolver.memberOf = []string{"ws_a", "ws_b", "ws_empty"}
	store := newFakeLarkStore()
	p := newScopedProvider(t, store, resolver, now)

	_, err := p.AuthorizeURLForWorkspace(context.Background(), actor, "ws_empty")
	if err == nil {
		t.Fatal("这个空间没有飞书应用，应当明确失败")
	}
	if !errors.Is(err, ErrPrecondition) {
		t.Errorf("应当是 ErrPrecondition，实际 %v", err)
	}
	// **There should be no pending auth session at all** — especially not one recorded on the fallback app.
	if len(store.sessions) != 0 {
		t.Errorf("不该发起任何授权流程，实际留下 %d 条\n"+
			"→ 回落到账号默认那个应用会让用户以为授权好了，"+
			"而这个空间的 agent 仍然用不了", len(store.sessions))
	}
	// The message must point to the **real** place to connect. It once said "workspace settings →
	// Feishu", while workspace settings has no Feishu item at all.
	if strings.Contains(err.Error(), "空间设置") {
		t.Errorf("别再指向「空间设置」——那里没有飞书这一项，实际 %q", err.Error())
	}
	if !strings.Contains(err.Error(), "侧栏") {
		t.Errorf("应当指向侧栏那个「飞书」入口，实际 %q", err.Error())
	}
}

// ⭐⭐ Start and poll must resolve **the same** app, otherwise the whole flow dies.
//
// Start records the device_code on `(actor, app_id)`, and polling needs the same app_id to find
// it. When the two sides disagree, GetLarkAuthSession finds nothing → returns "this authorization
// flow has expired", and its Pending is false, so the frontend treats it as **terminal** and stops
// polling on the first tick. So within 3 seconds of clicking authorize the user sees "expired",
// **while the start itself succeeded**.
//
// Mutation check: replace resolveAppScoped in PollAuthorizationForWorkspace with resolveApp, and
// this turns red (it gets that "expired" message).
func TestPollAuthorizationUsesSameWorkspaceAsAuthorize(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	const actor = "alice@example.com"
	resolver := twoWorkspaceResolver()
	store := newFakeLarkStore()

	// First start in ws_b: the device_code is recorded on cli_b.
	begin := newScopedProvider(t, store, resolver, now)
	if _, err := begin.AuthorizeURLForWorkspace(context.Background(), actor, "ws_b"); err != nil {
		t.Fatalf("发起授权失败: %v", err)
	}

	// Then poll in ws_b. This time Feishu returns a successful token.
	srv := httptest.NewServer(jsonHandler(http.StatusOK, `{
		"access_token": "u-at",
		"refresh_token": "u-rt",
		"expires_in": 7200,
		"refresh_token_expires_in": 604800,
		"scope": "offline_access im:message.group_msg:get_as_user"
	}`))
	t.Cleanup(srv.Close)
	client, err := lark.New(lark.Config{AccountsURL: srv.URL, OpenURL: srv.URL})
	if err != nil {
		t.Fatalf("建飞书客户端失败: %v", err)
	}
	poll := NewLarkUserProvider(store, resolver, client, nil, discardLogger())
	poll.now = func() time.Time { return now }

	st, err := poll.PollAuthorizationForWorkspace(context.Background(), actor, "ws_b")
	if err != nil {
		t.Fatalf("轮询失败: %v", err)
	}
	if !st.Done {
		t.Fatalf("该是 done，实际 %+v\n"+
			"→ 这正是「只接了一半」的症状：发起钉在空间的应用上、轮询按账号默认解，"+
			"于是查不到 session。而 Pending 为 false 会让前端当成终态停止轮询——"+
			"用户点完授权 3 秒内就看到「已失效」，而发起其实是成功的", st)
	}
	// The credentials must land under **that workspace's** app.
	if _, ok := store.tokens[lkey(actor, "cli_b")]; !ok {
		t.Errorf("token 应当存在 cli_b 名下，实际没有")
	}
	if _, ok := store.tokens[lkey(actor, "cli_a")]; ok {
		t.Errorf("不该往账号默认的 cli_a 上存 token——那就是错配本身")
	}
}

// Under "pinned by workspace", the preference is still **soft**: if it is not in this workspace it
// is ignored, not fatal.
//
// If it were hard, after the user picked workspace A's app on the authorization management page,
// none of workspace B's agents could be authorized.
func TestAuthorizeURLKeepsPreferenceSoftUnderWorkspacePin(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	const actor = "alice@example.com"
	resolver := twoWorkspaceResolver()
	store := newFakeLarkStore()
	// On the authorization management page they picked ws_a's app, and now they start authorization in ws_b.
	store.appPrefs[actor] = "cli_a"
	p := newScopedProvider(t, store, resolver, now)

	if _, err := p.AuthorizeURLForWorkspace(context.Background(), actor, "ws_b"); err != nil {
		t.Fatalf("偏好指向别的空间不该让授权失败: %v", err)
	}
	if _, ok := store.sessions[lkey(actor, "cli_b")]; !ok {
		t.Errorf("应当用 ws_b 那个应用（偏好是软的，跨空间时被忽略）")
	}
	// The preference must still be **passed down**: it is a valid tie-breaker when one workspace has several apps.
	if len(resolver.gotQueries) == 0 {
		t.Fatal("压根没调用 ResolveApp")
	}
	last := resolver.gotQueries[len(resolver.gotQueries)-1]
	if last.PreferAppID != "cli_a" {
		t.Errorf("偏好应当照样传给 ResolveApp（由它决定软硬），实际 %q\n"+
			"→ 不传的话，一个空间里有多个应用时用户在授权管理页选的那个会被无视",
			last.PreferAppID)
	}
}

// ---------------------------------------------------------------------------
// ⭐⭐ The status check must resolve the same app as starting authorization (the bug fixed here)
// ---------------------------------------------------------------------------

// ⭐⭐ This is the **end-to-end reproduction** of this defect: authorization succeeded, yet the card
// still said "not authorized yet".
//
// The shape was "only three of four paths were wired" — start / poll / list apps were all pinned
// by workspace, **while the status check stayed on the account-level default**. So authorizing on
// ws_b's agent page put the token on cli_b (right), while the card looked up cli_a (wrong) →
// showing "not authorized yet, click 'start authorization'". The user followed it; each time the
// authorization really succeeded and each time the card said not authorized, **with HTTP 200 on
// the whole chain**.
//
// The same pair observed in practice:
//
//	GET /api/integrations/lark-user                    → cli_example_app_a, not authorized
//	GET /api/integrations/lark-user/apps?workspaceId=… → cli_example_app_b, authorized:true
//
// Mutation check: replace resolveAppScoped in StateForWorkspace with resolveApp, and this turns
// red with (translated):
//
//	in workspace ws_b the status should be ok (the token is on cli_b), but got error / not authorized yet…
func TestStateUsesSameAppAsAuthorizeForWorkspace(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	const actor = "alice@example.com"
	resolver := twoWorkspaceResolver()
	store := newFakeLarkStore()
	p := newScopedProvider(t, store, resolver, now)
	ctx := context.Background()

	// Store the credentials through the real chain: start + poll in ws_b (exactly the path of the
	// user clicking the button), instead of stuffing a row into the store by hand — doing it by hand
	// would bypass "which app it is stored under", which is the load-bearing point of this bug.
	if _, err := p.AuthorizeURLForWorkspace(ctx, actor, "ws_b"); err != nil {
		t.Fatalf("发起授权失败: %v", err)
	}
	tokenSrv := httptest.NewServer(jsonHandler(http.StatusOK, `{
		"code": 0,
		"access_token": "uat-1",
		"refresh_token": "urt-1",
		"expires_in": 7200,
		"refresh_token_expires_in": 604800,
		"scope": "im:message.group_msg:get_as_user offline_access"
	}`))
	defer tokenSrv.Close()
	client, err := lark.New(lark.Config{AccountsURL: tokenSrv.URL, OpenURL: tokenSrv.URL})
	if err != nil {
		t.Fatalf("建飞书客户端失败: %v", err)
	}
	p.client = client
	if st, err := p.PollAuthorizationForWorkspace(ctx, actor, "ws_b"); err != nil || !st.Done {
		t.Fatalf("轮询该成功，实际 err=%v st=%+v", err, st)
	}
	// Precondition self-check: the credentials really landed under ws_b's app (not cli_a).
	if _, ok := store.tokens[lkey(actor, "cli_b")]; !ok {
		t.Fatalf("前提不成立：token 没落在 cli_b 上")
	}

	// ⭐ Now ask for the status — **for the same workspace**.
	st := p.StateForWorkspace(ctx, actor, "ws_b")
	if st.Status != IntegrationOK {
		t.Errorf("在空间 ws_b 里状态该是 ok（token 就在 cli_b 上），实际 %s / %s\n"+
			"→ 这就是用户报的那个 bug：他在 agent 页反复授权，每次都真的成功、"+
			"每次卡片都说「未授权」。根因是状态按**账号级默认**解应用，"+
			"而授权按**空间**解——四条路径只接了三条", st.Status, st.Summary)
	}
	// The "Feishu app" line on the card must be cli_b, otherwise the app the user sees is not the one actually used.
	var shownApp string
	for _, d := range st.Details {
		if d.Label == "飞书应用" {
			shownApp = d.Value
		}
	}
	if shownApp != "cli_b" {
		t.Errorf("卡片该显示 cli_b，实际 %q——显示的应用与判状态的应用必须是同一个", shownApp)
	}
}

// Without a workspace, the status still uses the account-level rule — **the authorization
// management page's path must not be broken by this change**.
//
// That page has no workspace context; pinning a workspace out of nowhere would make it report
// "not authorized" for an app the user does not care about.
func TestStateWithoutWorkspaceStaysAccountLevel(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	const actor = "alice@example.com"
	resolver := twoWorkspaceResolver()
	store := newFakeLarkStore()
	// The account-level default is cli_a; give it a valid credential; cli_b deliberately gets none.
	store.tokens[lkey(actor, "cli_a")] = persistmodel.LarkUserToken{
		Actor: actor, LarkAppID: "cli_a", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt:        now.Add(2 * time.Hour),
		RefreshExpiresAt: now.Add(7 * 24 * time.Hour),
	}
	p := newScopedProvider(t, store, resolver, now)

	// Use the method on the interface (IntegrationProvider.State); it is a thin wrapper passing an empty string.
	st := p.State(context.Background(), actor)
	if st.Status != IntegrationOK {
		t.Errorf("账号级口径下该是 ok（cli_a 有凭据），实际 %s / %s", st.Status, st.Summary)
	}
	if len(resolver.gotQueries) == 0 {
		t.Fatal("压根没调用 ResolveApp")
	}
	if got := resolver.gotQueries[0].WorkspaceID; got != "" {
		t.Errorf("不带空间时 WorkspaceID 该是空串，实际 %q", got)
	}
}

// When a non-member names a workspace, the status **must not say "not authorized yet"**.
//
// That message would send them to click "start authorization", and in this case that button is
// bound to 404. The error must point at the real cause: you may not be a member of this workspace.
func TestStateForWorkspaceRejectsNonMemberWithoutTellingThemToAuthorize(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	const actor = "alice@example.com"
	resolver := twoWorkspaceResolver()
	resolver.memberOf = []string{"ws_a"} // ws_b belongs to someone else
	store := newFakeLarkStore()
	p := newScopedProvider(t, store, resolver, now)

	st := p.StateForWorkspace(context.Background(), actor, "ws_b")
	if st.CanAuthorize {
		t.Error("非成员不该拿到「发起授权」按钮——点了必然 404")
	}
	if strings.Contains(st.Summary, "还没有授权") {
		t.Errorf("非成员的状态被说成「还没有授权」，那会让他一遍遍点授权按钮；实际应说清"+
			"「你可能不是这个空间的成员」。当前 summary=%q", st.Summary)
	}
	// **Never use an unchecked workspaceId to query the database** (same as the AuthorizeURL security assertion).
	for _, q := range resolver.gotQueries {
		if q.WorkspaceID == "ws_b" {
			t.Error("非成员的 workspaceId 不该被交给 ResolveApp")
		}
	}
}

// ⭐⭐ "This workspace has not connected a Feishu app yet" **must not mark the whole card missing**.
//
// # Why this is pinned on its own
//
// The agent form only renders sources with `status === "ok" || canAuthorize` (the listed filter in
// web/src/components/AgentRuntimeSections.tsx). So once this branch returns missing +
// CanAuthorize:false, the card **disappears entirely** from the agent page, and a dead end forms
// (hiding a control by default needs visibility derived from whether the user can fix it):
//
//	card is invisible → they do not know the "Feishu user authorization" ability exists
//	→ even less do they know they must first connect a Feishu app in the sidebar
//	→ yet that is exactly something they can do with two clicks (they are likely this workspace's owner)
//
// The rule, in the words of that lesson: **group by "can the user make it usable themselves"**,
// not by "is it usable right now". The workspace has no app connected = they can fix it → it must
// be visible.
//
// ⚠️ The account-level case (empty workspaceID) is still missing — then it is "no usable app at
// all", an admin must configure it, and the user clicking a button does nothing. The two are
// deliberately different, so this test asserts both sides.
//
// Mutation check: delete the workspace branch in StateForWorkspace (so it falls into the
// account-level missing + CanAuthorize=false), and this turns red with (translated):
//
//	with no app connected in the workspace, status should be error (the user can fix it), but got missing
//	CanAuthorize should be true, otherwise the agent form hides the whole card
func TestStateForWorkspaceWithoutAppStaysVisibleAndActionable(t *testing.T) {
	now := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	const actor = "alice@example.com"
	// ws_c is their workspace, but it has **not** connected any Feishu app (not in byWorkspace).
	resolver := twoWorkspaceResolver()
	resolver.memberOf = []string{"ws_a", "ws_b", "ws_c"}
	store := newFakeLarkStore()
	p := newScopedProvider(t, store, resolver, now)

	st := p.StateForWorkspace(context.Background(), actor, "ws_c")
	if st.Status != IntegrationError {
		t.Errorf("空间里没接入应用时 status 该是 error（用户自己能解决），实际 %s", st.Status)
	}
	if !st.CanAuthorize {
		t.Error("CanAuthorize 该是 true，否则 agent 表单会把整张卡片藏掉——" +
			"而那是「卡片看不见→不知道要去接入应用」的死局")
	}
	// The message must point to an **actionable next step**, not "not authorized yet" (clicking would not fix it).
	if !strings.Contains(st.Summary, "还没接入飞书应用") {
		t.Errorf("summary 要说清「这个空间还没接入飞书应用」，实际 %q", st.Summary)
	}
	if !strings.Contains(st.FixHint, "飞书") {
		t.Errorf("FixHint 要指向侧栏的「飞书」入口，实际 %q", st.FixHint)
	}

	// ⚠️ The account-level case is **still missing**: no usable app at all → an admin must configure it.
	noApps := &recordingAppResolver{byWorkspace: map[string]string{}, fallback: ""}
	p2 := newScopedProvider(t, newFakeLarkStore(), noApps, now)
	if st2 := p2.State(context.Background(), actor); st2.Status != IntegrationMissing {
		t.Errorf("账号级一个应用都没有时该是 missing（要管理员配），实际 %s / %s",
			st2.Status, st2.Summary)
	}
}
