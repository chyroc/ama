package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/shared/infra/oidc"
)

// fakeOIDC is a programmable OIDC stub so service-layer unit tests can cover the branches "IdP is
// down", "code rejected", "no email", "email not verified". The real code flow (including
// signature checks and PKCE) is run by oidc_flow_test.go against a fake IdP started with httptest.
type fakeOIDC struct {
	authorizeErr error

	claims      *oidc.Claims
	exchangeErr error

	// Record the last received arguments, to assert "state / nonce / verifier really were passed
	// down and read back unchanged".
	lastRedirectURI string
	lastState       string
	lastNonce       string
	lastVerifier    string
	gotVerifier     string
	gotNonce        string
	authorizeCalls  int
	exchangeCalls   int
}

func (f *fakeOIDC) AuthCodeURL(_ context.Context, redirectURI, state, nonce, verifier string) (string, error) {
	f.authorizeCalls++
	f.lastRedirectURI, f.lastState, f.lastNonce, f.lastVerifier = redirectURI, state, nonce, verifier
	if f.authorizeErr != nil {
		return "", f.authorizeErr
	}
	return "https://idp.example.com/authorize?state=" + state, nil
}

func (f *fakeOIDC) Exchange(_ context.Context, _, _, verifier, nonce string) (oidc.Claims, error) {
	f.exchangeCalls++
	f.gotVerifier, f.gotNonce = verifier, nonce
	if f.exchangeErr != nil {
		return oidc.Claims{}, f.exchangeErr
	}
	if f.claims != nil {
		return *f.claims, nil
	}
	return oidc.Claims{Subject: "u1", Email: "zhangsan@example.com", EmailVerified: true, Name: "张三"}, nil
}

func newTestService(t *testing.T, fake *fakeOIDC, key APIKey) *Service {
	t.Helper()
	cfg := Config{Store: NewMemStore(), APIKey: key}
	if fake != nil {
		cfg.OIDC = fake
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return svc
}

// loginOnce runs the whole OIDC login and returns the result. Session tests only care that "there
// is a valid session".
func loginOnce(t *testing.T, svc *Service, fake *fakeOIDC) LoginResult {
	t.Helper()
	if _, err := svc.StartOIDCLogin(context.Background(), "https://x/cb", ""); err != nil {
		t.Fatalf("StartOIDCLogin 失败: %v", err)
	}
	res, err := svc.CompleteOIDCLogin(context.Background(), "https://x/cb", fake.lastState, "code")
	if err != nil {
		t.Fatalf("CompleteOIDCLogin 失败: %v", err)
	}
	return res
}

func TestNewRejectsMissingDeps(t *testing.T) {
	if _, err := New(Config{OIDC: &fakeOIDC{}}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("缺 Store 应该被拒，得到 %v", err)
	}
	// It can be built even with both login methods off: the service still starts, and the fixed
	// API Key still works.
	if _, err := New(Config{Store: NewMemStore()}); err != nil {
		t.Errorf("不开任何登录方式也应该能建出来，得到 %v", err)
	}
}

// ---------------------------------------------------------------------------
// OIDC
// ---------------------------------------------------------------------------

func TestStartOIDCLoginPersistsStateNonceVerifier(t *testing.T) {
	fake := &fakeOIDC{}
	store := NewMemStore()
	svc, _ := New(Config{Store: store, OIDC: fake})

	url, err := svc.StartOIDCLogin(context.Background(), "https://ama.example.com/api/auth/oidc/callback", "/w/ws_1/chat")
	if err != nil {
		t.Fatalf("StartOIDCLogin 失败: %v", err)
	}
	if !strings.HasPrefix(url, "https://idp.example.com/") {
		t.Errorf("授权地址不对: %s", url)
	}
	if fake.lastRedirectURI != "https://ama.example.com/api/auth/oidc/callback" {
		t.Errorf("redirect_uri 没传下去: %q", fake.lastRedirectURI)
	}
	// The three random values differ from each other, and the verifier length satisfies RFC 7636's
	// 43~128.
	if fake.lastState == fake.lastNonce || fake.lastNonce == fake.lastVerifier || fake.lastState == fake.lastVerifier {
		t.Error("state / nonce / verifier 不该相同")
	}
	if n := len(fake.lastVerifier); n < 43 || n > 128 {
		t.Errorf("PKCE verifier 长度 %d 不在 43~128", n)
	}
	store.mu.RLock()
	st, ok := store.oidcStates[fake.lastState]
	store.mu.RUnlock()
	if !ok || st.Nonce != fake.lastNonce || st.CodeVerifier != fake.lastVerifier || st.ReturnTo != "/w/ws_1/chat" {
		t.Errorf("落库的登录流程不对: %+v", st)
	}
}

func TestStartOIDCLoginDisabledAndDown(t *testing.T) {
	svc := newTestService(t, nil, APIKey{})
	if _, err := svc.StartOIDCLogin(context.Background(), "https://x/cb", ""); !errors.Is(err, ErrOIDCDisabled) {
		t.Errorf("没配 OIDC 应 ErrOIDCDisabled，得到 %v", err)
	}
	down := newTestService(t, &fakeOIDC{authorizeErr: errors.New("connection refused")}, APIKey{})
	if _, err := down.StartOIDCLogin(context.Background(), "https://x/cb", ""); !errors.Is(err, ErrLoginUnavailable) {
		t.Errorf("IdP 不通应 ErrLoginUnavailable，得到 %v", err)
	}
	if _, err := down.StartOIDCLogin(context.Background(), " ", ""); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("空 redirectURI 应 ErrInvalidArgument，得到 %v", err)
	}
}

func TestCompleteOIDCLogin(t *testing.T) {
	fake := &fakeOIDC{claims: &oidc.Claims{Email: " ZhangSan@Example.COM ", EmailVerified: true, Name: "张三", Picture: "https://x/a.png"}}
	svc := newTestService(t, fake, APIKey{})
	if _, err := svc.StartOIDCLogin(context.Background(), "https://x/cb", "/preferences"); err != nil {
		t.Fatal(err)
	}
	res, err := svc.CompleteOIDCLogin(context.Background(), "https://x/cb", fake.lastState, "code")
	if err != nil {
		t.Fatalf("CompleteOIDCLogin 失败: %v", err)
	}
	if res.Identity.Email != "zhangsan@example.com" || res.Identity.Name != "张三" || res.Identity.AvatarURL != "https://x/a.png" {
		t.Errorf("身份不对: %+v", res.Identity)
	}
	if res.ReturnTo != "/preferences" {
		t.Errorf("ReturnTo = %q", res.ReturnTo)
	}
	// The token exchange sends the verifier / nonce pair generated at the start.
	if fake.gotVerifier != fake.lastVerifier || fake.gotNonce != fake.lastNonce {
		t.Error("Exchange 没拿到发起时落库的 verifier / nonce")
	}
	id, err := svc.Resolve(context.Background(), res.SessionToken)
	if err != nil || id.Email != "zhangsan@example.com" {
		t.Errorf("新会话解析不出身份: %+v, %v", id, err)
	}
}

func TestCompleteOIDCLoginStateChecks(t *testing.T) {
	fake := &fakeOIDC{}
	svc := newTestService(t, fake, APIKey{})
	if _, err := svc.StartOIDCLogin(context.Background(), "https://x/cb", ""); err != nil {
		t.Fatal(err)
	}
	state := fake.lastState
	// Unknown state: rejected, and **the token is not exchanged at all** (state is the CSRF gate and
	// must come first).
	if _, err := svc.CompleteOIDCLogin(context.Background(), "https://x/cb", "forged-state", "code"); !errors.Is(err, ErrLoginExpired) {
		t.Errorf("伪造 state 应 ErrLoginExpired，得到 %v", err)
	}
	if fake.exchangeCalls != 0 {
		t.Error("state 不对时不该去换 token")
	}
	if _, err := svc.CompleteOIDCLogin(context.Background(), "https://x/cb", state, "code"); err != nil {
		t.Fatalf("第一次应成功: %v", err)
	}
	if _, err := svc.CompleteOIDCLogin(context.Background(), "https://x/cb", state, "code"); !errors.Is(err, ErrLoginExpired) {
		t.Errorf("state 只能用一次，得到 %v", err)
	}
	if _, err := svc.CompleteOIDCLogin(context.Background(), "https://x/cb", "", "code"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("空 state 应 ErrInvalidArgument，得到 %v", err)
	}
}

func TestCompleteOIDCLoginErrors(t *testing.T) {
	cases := []struct {
		name  string
		fake  *fakeOIDC
		allow bool
		want  error
	}{
		{"IdP 拒绝 code", &fakeOIDC{exchangeErr: oidc.ErrRejected}, false, ErrLoginExpired},
		{"IdP 不通", &fakeOIDC{exchangeErr: errors.New("connection reset")}, false, ErrLoginUnavailable},
		{"没有邮箱", &fakeOIDC{claims: &oidc.Claims{Subject: "u1", EmailVerified: true}}, false, ErrLoginNoIdentity},
		{"邮箱未验证", &fakeOIDC{claims: &oidc.Claims{Email: "a@example.com"}}, false, ErrEmailUnverified},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := New(Config{Store: NewMemStore(), OIDC: tc.fake, OIDCAllowUnverifiedEmail: tc.allow})
			if _, err := svc.StartOIDCLogin(context.Background(), "https://x/cb", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.CompleteOIDCLogin(context.Background(), "https://x/cb", tc.fake.lastState, "code"); !errors.Is(err, tc.want) {
				t.Errorf("期望 %v，得到 %v", tc.want, err)
			}
		})
	}
	// After relaxing it explicitly, an unverified email can log in.
	fake := &fakeOIDC{claims: &oidc.Claims{Email: "a@example.com"}}
	svc, _ := New(Config{Store: NewMemStore(), OIDC: fake, OIDCAllowUnverifiedEmail: true})
	if _, err := svc.StartOIDCLogin(context.Background(), "https://x/cb", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteOIDCLogin(context.Background(), "https://x/cb", fake.lastState, "code"); err != nil {
		t.Errorf("放宽后应成功，得到 %v", err)
	}
}

// A failing post-login hook must not make the login fail (the session has already been issued).
func TestLoginHookFailureDoesNotFailLogin(t *testing.T) {
	fake := &fakeOIDC{}
	called := 0
	svc, _ := New(Config{Store: NewMemStore(), OIDC: fake, OnLogin: func(context.Context, string) error {
		called++
		return errors.New("boom")
	}})
	res := loginOnce(t, svc, fake)
	if res.SessionToken == "" || called != 1 {
		t.Errorf("钩子失败不该影响登录：token=%q called=%d", res.SessionToken, called)
	}
}

// ---------------------------------------------------------------------------
// Resolve / API Key
// ---------------------------------------------------------------------------

func TestResolveEmptyCredential(t *testing.T) {
	svc := newTestService(t, &fakeOIDC{}, APIKey{})
	if _, err := svc.Resolve(context.Background(), ""); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("期望 ErrUnauthenticated，得到 %v", err)
	}
}

func TestResolveUnknownToken(t *testing.T) {
	svc := newTestService(t, &fakeOIDC{}, APIKey{})
	if _, err := svc.Resolve(context.Background(), "bogus-token"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("期望 ErrUnauthenticated，得到 %v", err)
	}
}

func TestResolveAPIKey(t *testing.T) {
	key := APIKey{Key: "secret-key", Email: "Bot@Example.com"}
	svc := newTestService(t, &fakeOIDC{}, key)

	identity, err := svc.Resolve(context.Background(), "secret-key")
	if err != nil {
		t.Fatalf("Resolve 失败: %v", err)
	}
	if identity.Email != "bot@example.com" {
		t.Errorf("Email = %q, 应该小写", identity.Email)
	}
	if !identity.ViaAPIKey {
		t.Error("应该标成 ViaAPIKey")
	}
	if identity.Name != "bot" {
		t.Errorf("Name = %q, 缺省应该用邮箱前缀", identity.Name)
	}
}

func TestResolveAPIKeyWrongValue(t *testing.T) {
	svc := newTestService(t, &fakeOIDC{}, APIKey{Key: "secret-key", Email: "bot@example.com"})
	if _, err := svc.Resolve(context.Background(), "secret-ke"); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("错的 key 必须被拒，得到 %v", err)
	}
}

// Only the key without the email (or the other way round) = the channel does not exist. Production
// sets neither by default.
func TestAPIKeyDisabledWhenIncomplete(t *testing.T) {
	cases := []struct {
		name string
		key  APIKey
	}{
		{"两个都空", APIKey{}},
		{"只有 key", APIKey{Key: "k"}},
		{"只有邮箱", APIKey{Email: "bot@example.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t, &fakeOIDC{}, tc.key)
			if svc.APIKeyEnabled() {
				t.Error("配不全就该关闭")
			}
			if _, err := svc.Resolve(context.Background(), "k"); !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("通道关闭时不该放行，得到 %v", err)
			}
		})
	}
}

func TestAPIKeyCustomName(t *testing.T) {
	svc := newTestService(t, &fakeOIDC{}, APIKey{Key: "k", Email: "bot@example.com", Name: "自动化"})
	identity, err := svc.Resolve(context.Background(), "k")
	if err != nil {
		t.Fatalf("Resolve 失败: %v", err)
	}
	if identity.Name != "自动化" {
		t.Errorf("Name = %q", identity.Name)
	}
}

// ---------------------------------------------------------------------------
// Session lifecycle
// ---------------------------------------------------------------------------

func TestSessionExpires(t *testing.T) {
	store := NewMemStore()
	fake := &fakeOIDC{}
	svc, err := New(Config{Store: store, OIDC: fake, SessionTTL: time.Hour})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	res := loginOnce(t, svc, fake)
	if res.SessionToken == "" {
		t.Fatal("应该签发了 token")
	}

	// Move the clock past the lifetime.
	future := time.Now().Add(2 * time.Hour)
	store.now = func() time.Time { return future }

	if _, err := svc.Resolve(context.Background(), res.SessionToken); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("过期会话应该被拒，得到 %v", err)
	}
}

func TestLogout(t *testing.T) {
	fake := &fakeOIDC{}
	svc := newTestService(t, fake, APIKey{})
	res := loginOnce(t, svc, fake)

	if err := svc.Logout(context.Background(), res.SessionToken); err != nil {
		t.Fatalf("Logout 失败: %v", err)
	}
	if _, err := svc.Resolve(context.Background(), res.SessionToken); !errors.Is(err, ErrUnauthenticated) {
		t.Error("登出后 token 必须失效——这正是不用 JWT 的理由")
	}
}

func TestLogoutIsIdempotent(t *testing.T) {
	svc := newTestService(t, &fakeOIDC{}, APIKey{})
	if err := svc.Logout(context.Background(), "never-existed"); err != nil {
		t.Errorf("登出不存在的会话不该报错: %v", err)
	}
	if err := svc.Logout(context.Background(), ""); err != nil {
		t.Errorf("空凭据登出不该报错: %v", err)
	}
}

// An API Key is not a session; logging it out does nothing, and it still works afterwards.
func TestLogoutDoesNotKillAPIKey(t *testing.T) {
	svc := newTestService(t, &fakeOIDC{}, APIKey{Key: "k", Email: "bot@example.com"})
	if err := svc.Logout(context.Background(), "k"); err != nil {
		t.Fatalf("Logout 失败: %v", err)
	}
	if _, err := svc.Resolve(context.Background(), "k"); err != nil {
		t.Errorf("API Key 登出后仍应可用，得到 %v", err)
	}
}

func TestLogoutAll(t *testing.T) {
	fake := &fakeOIDC{}
	svc := newTestService(t, fake, APIKey{})

	// The same person logs in on two devices.
	var tokens []string
	for range 2 {
		tokens = append(tokens, loginOnce(t, svc, fake).SessionToken)
	}

	n, err := svc.LogoutAll(context.Background(), "zhangsan@example.com")
	if err != nil {
		t.Fatalf("LogoutAll 失败: %v", err)
	}
	if n != 2 {
		t.Errorf("删了 %d 条，期望 2", n)
	}
	for i, tk := range tokens {
		if _, err := svc.Resolve(context.Background(), tk); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("第 %d 个会话应该已被踢掉", i)
		}
	}
}

// The same person logged in on several devices; they do not affect each other: logging out on the
// laptop should not kick out the phone.
func TestMultipleSessionsPerUser(t *testing.T) {
	fake := &fakeOIDC{}
	svc := newTestService(t, fake, APIKey{})

	r1 := loginOnce(t, svc, fake)
	r2 := loginOnce(t, svc, fake)

	if r1.SessionToken == r2.SessionToken {
		t.Fatal("两次登录必须是不同的 token")
	}
	if err := svc.Logout(context.Background(), r1.SessionToken); err != nil {
		t.Fatalf("Logout 失败: %v", err)
	}
	if _, err := svc.Resolve(context.Background(), r2.SessionToken); err != nil {
		t.Errorf("另一台设备的会话不该被连累: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The token itself
// ---------------------------------------------------------------------------

// The database stores the hash, not the token: reading the database does not yield a usable credential.
func TestSessionStoresHashNotToken(t *testing.T) {
	store := NewMemStore()
	fake := &fakeOIDC{}
	svc, _ := New(Config{Store: store, OIDC: fake})

	res := loginOnce(t, svc, fake)

	store.mu.RLock()
	defer store.mu.RUnlock()
	for hash := range store.sessions {
		if hash == res.SessionToken {
			t.Fatal("库里直接存了明文 token")
		}
		if hash != hashToken(res.SessionToken) {
			t.Errorf("存的不是 token 的 sha256")
		}
	}
}

func TestNewTokenIsRandomAndURLSafe(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tk, err := newToken()
		if err != nil {
			t.Fatalf("newToken 失败: %v", err)
		}
		if seen[tk] {
			t.Fatal("token 重复了")
		}
		seen[tk] = true
		// It must go safely into cookies and queries, so it cannot have the three characters + / =.
		if strings.ContainsAny(tk, "+/=") {
			t.Errorf("token 含需要转义的字符: %q", tk)
		}
	}
}

func TestHashTokenIsStable(t *testing.T) {
	if hashToken("abc") != hashToken("abc") {
		t.Error("同样的输入必须得到同样的 hash")
	}
	if hashToken("abc") == hashToken("abd") {
		t.Error("不同输入不该撞")
	}
	if len(hashToken("abc")) != 64 {
		t.Errorf("sha256 的 hex 应该是 64 字符，得到 %d", len(hashToken("abc")))
	}
}

func TestConstantTimeEqual(t *testing.T) {
	if !constantTimeEqual("abc", "abc") {
		t.Error("相同应该相等")
	}
	if constantTimeEqual("abc", "abd") || constantTimeEqual("abc", "ab") {
		t.Error("不同不该相等")
	}
}

func TestNormalizeEmail(t *testing.T) {
	cases := map[string]string{
		"  ZhangSan@Example.com  ": "zhangsan@example.com",
		"a@b.com":                  "a@b.com",
		"":                         "",
	}
	for in, want := range cases {
		if got := normalizeEmail(in); got != want {
			t.Errorf("normalizeEmail(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func TestIdentityValid(t *testing.T) {
	if (Identity{}).Valid() {
		t.Error("空身份不该 Valid")
	}
	if (Identity{Email: "  "}).Valid() {
		t.Error("空白邮箱不该 Valid")
	}
	if !(Identity{Email: "a@b.com"}).Valid() {
		t.Error("有邮箱就该 Valid")
	}
}

// The expiry check uses "expired when the time is reached" (not "expired after the time is
// passed"), the same as Session.Expired.
func TestOIDCStateExpired(t *testing.T) {
	base := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	st := OIDCState{ExpiresAt: base}
	if st.Expired(base.Add(-time.Second)) {
		t.Error("还没到点不该算过期")
	}
	if !st.Expired(base) {
		t.Error("到点即过期")
	}
	if !st.Expired(base.Add(time.Second)) {
		t.Error("过点当然过期")
	}
}
