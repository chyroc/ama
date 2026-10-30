// The full OIDC code flow: httptest starts a fake IdP (discovery + JWKS + authorize + token),
// and the real shared/infra/oidc client runs through it; signature, PKCE, nonce and state checks are all real.

package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/chyroc/ama/server/shared/infra/oidc"
)

// fakeIdP is a minimal OIDC provider.
type fakeIdP struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	signer jose.Signer

	mu sync.Mutex
	// codes: code → the challenge / nonce / redirect_uri sent when the login started.
	codes map[string]pendingCode
	// Failures that tests can set up.
	email         string
	emailVerified bool
	wrongNonce    bool
	tokenCalls    int
}

type pendingCode struct {
	challenge, nonce, redirectURI string
}

const (
	idpClientID     = "ama-test"
	idpClientSecret = "s3cret"
)

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{t: t, key: key, signer: signer, codes: map[string]pendingCode{},
		email: "Alice@Example.com", emailVerified: true}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("GET /jwks", f.jwks)
	mux.HandleFunc("GET /authorize", f.authorize)
	mux.HandleFunc("POST /token", f.token)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) discovery(w http.ResponseWriter, _ *http.Request) {
	u := f.srv.URL
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                u,
		"authorization_endpoint":                u + "/authorize",
		"token_endpoint":                        u + "/token",
		"jwks_uri":                              u + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
	})
}

func (f *fakeIdP) jwks(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &f.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig",
	}}})
}

// authorize simulates "the user logged in at the IdP and clicked consent": it 302s straight back
// to redirect_uri with a code.
func (f *fakeIdP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != idpClientID || q.Get("response_type") != "code" {
		http.Error(w, "bad client", http.StatusBadRequest)
		return
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "PKCE required", http.StatusBadRequest)
		return
	}
	if q.Get("nonce") == "" || q.Get("state") == "" || !strings.Contains(q.Get("scope"), "openid") {
		http.Error(w, "missing nonce/state/openid", http.StatusBadRequest)
		return
	}
	code := "code-" + q.Get("state")[:8]
	f.mu.Lock()
	f.codes[code] = pendingCode{challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), redirectURI: q.Get("redirect_uri")}
	f.mu.Unlock()
	back, _ := url.Parse(q.Get("redirect_uri"))
	bq := back.Query()
	bq.Set("code", code)
	bq.Set("state", q.Get("state"))
	back.RawQuery = bq.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (f *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.tokenCalls++
	f.mu.Unlock()
	_ = r.ParseForm()
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != idpClientID || secret != idpClientSecret {
		writeOAuthError(w, "invalid_client")
		return
	}
	code := r.PostForm.Get("code")
	f.mu.Lock()
	p, found := f.codes[code]
	delete(f.codes, code) // a code is one-time
	f.mu.Unlock()
	if !found || p.redirectURI != r.PostForm.Get("redirect_uri") {
		writeOAuthError(w, "invalid_grant")
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != p.challenge {
		writeOAuthError(w, "invalid_grant") // PKCE does not match
		return
	}
	nonce := p.nonce
	if f.wrongNonce {
		nonce = "some-other-nonce"
	}
	now := time.Now()
	claims, _ := json.Marshal(map[string]any{
		"iss": f.srv.URL, "sub": "user-1", "aud": idpClientID,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"nonce": nonce, "email": f.email, "email_verified": f.emailVerified,
		"name": "Alice", "picture": "https://idp.example/alice.png",
	})
	obj, err := f.signer.Sign(claims)
	if err != nil {
		f.t.Error(err)
		return
	}
	raw, _ := obj.CompactSerialize()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "at", "token_type": "Bearer", "expires_in": 300, "id_token": raw,
	})
}

func writeOAuthError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

// oidcFixture sets up the Handler + a real OIDC client pointing at the fake IdP.
func oidcFixture(t *testing.T, idp *fakeIdP) (*http.ServeMux, *Service) {
	t.Helper()
	client, err := oidc.New(oidc.Config{
		Issuer: idp.srv.URL, ClientID: idpClientID, ClientSecret: idpClientSecret,
		HTTPClient: idp.srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(Config{Store: NewMemStore(), OIDC: client, OIDCDisplayName: "Fake"})
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(HandlerConfig{Service: svc, PublicURL: "https://ama.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Register(mux)
	return mux, svc
}

// startFlow runs the first two steps: start login → browser goes to the IdP → IdP 302s back.
// Returns the path+query of the callback.
func startFlow(t *testing.T, mux *http.ServeMux, idp *fakeIdP, next string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, PathOIDCLogin+"?next="+url.QueryEscape(next), nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("发起登录: %d %s", rec.Code, rec.Body.String())
	}
	authURL := rec.Header().Get("Location")
	if !strings.HasPrefix(authURL, idp.srv.URL+"/authorize?") {
		t.Fatalf("没跳去 IdP: %s", authURL)
	}
	u, _ := url.Parse(authURL)
	if got := u.Query().Get("redirect_uri"); got != "https://ama.example.com"+PathOIDCCallback {
		t.Errorf("redirect_uri = %q", got)
	}
	// The plain verifier must never appear in the authorization URL; only the S256 digest may.
	if u.Query().Get("code_verifier") != "" {
		t.Error("授权地址里出现了 code_verifier 原文")
	}

	noRedirect := *idp.srv.Client()
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("IdP authorize: %d", resp.StatusCode)
	}
	back, _ := url.Parse(resp.Header.Get("Location"))
	if back.Path != PathOIDCCallback {
		t.Fatalf("IdP 回跳路径不对: %s", back)
	}
	return back.RequestURI()
}

func callback(mux *http.ServeMux, uri string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, uri, nil))
	return rec
}

func TestOIDCFullCodeFlow(t *testing.T) {
	idp := newFakeIdP(t)
	mux, svc := oidcFixture(t, idp)

	cb := startFlow(t, mux, idp, "/w/ws_1/chat")
	rec := callback(mux, cb)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/w/ws_1/chat" {
		t.Fatalf("回调: %d Location=%q body=%s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	cookie := sessionCookie(rec)
	if cookie == nil {
		t.Fatal("没种会话 Cookie")
	}
	id, err := svc.Resolve(t.Context(), cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	// The lower-cased email is the Actor; name / picture come from the ID token.
	if id.Email != "alice@example.com" || id.Name != "Alice" || id.AvatarURL != "https://idp.example/alice.png" {
		t.Errorf("身份不对: %+v", id)
	}

	// Replay the same callback: state is already consumed → expired.
	rec = callback(mux, cb)
	if loc := rec.Header().Get("Location"); loc != "/?login_error=expired" {
		t.Errorf("重放回调应 expired，得到 %q", loc)
	}
}

// ⚠️ This one pins CSRF: if the state in the callback is not the one we started, it must be
// rejected, **and the token must not be exchanged**.
func TestOIDCStateMismatchRejected(t *testing.T) {
	idp := newFakeIdP(t)
	mux, _ := oidcFixture(t, idp)

	cb := startFlow(t, mux, idp, "/")
	u, _ := url.Parse(cb)
	q := u.Query()
	q.Set("state", "attacker-chosen-state")
	u.RawQuery = q.Encode()

	rec := callback(mux, u.RequestURI())
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/?login_error=expired" {
		t.Fatalf("state 不匹配应 303 到 expired，得到 %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if sessionCookie(rec) != nil {
		t.Error("state 不匹配还种了 Cookie")
	}
	idp.mu.Lock()
	calls := idp.tokenCalls
	idp.mu.Unlock()
	if calls != 0 {
		t.Errorf("state 不匹配时不该去 IdP 换 token，实际换了 %d 次", calls)
	}
}

func TestOIDCNonceMismatchRejected(t *testing.T) {
	idp := newFakeIdP(t)
	idp.wrongNonce = true
	mux, _ := oidcFixture(t, idp)
	rec := callback(mux, startFlow(t, mux, idp, "/"))
	if loc := rec.Header().Get("Location"); loc != "/?login_error=expired" || sessionCookie(rec) != nil {
		t.Errorf("nonce 不符应拒绝，得到 %q", loc)
	}
}

func TestOIDCUnverifiedEmailRejected(t *testing.T) {
	idp := newFakeIdP(t)
	idp.emailVerified = false
	mux, _ := oidcFixture(t, idp)
	rec := callback(mux, startFlow(t, mux, idp, "/"))
	if loc := rec.Header().Get("Location"); loc != "/?login_error=email_unverified" || sessionCookie(rec) != nil {
		t.Errorf("未验证邮箱应拒绝，得到 %q", loc)
	}
}

// When the code is tampered with (or PKCE does not match) the IdP returns invalid_grant → the user
// sees expired, not unavailable.
func TestOIDCInvalidGrantIsExpired(t *testing.T) {
	idp := newFakeIdP(t)
	mux, _ := oidcFixture(t, idp)
	cb := startFlow(t, mux, idp, "/")
	u, _ := url.Parse(cb)
	q := u.Query()
	q.Set("code", "forged-code")
	u.RawQuery = q.Encode()
	rec := callback(mux, u.RequestURI())
	if loc := rec.Header().Get("Location"); loc != "/?login_error=expired" {
		t.Errorf("伪造 code 应 expired，得到 %q", loc)
	}
}

// The IdP is entirely unreachable: starting a login goes back to the home page with unavailable,
// and the service does not crash.
func TestOIDCIdPDown(t *testing.T) {
	idp := newFakeIdP(t)
	mux, _ := oidcFixture(t, idp)
	idp.srv.Close()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, PathOIDCLogin, nil))
	if loc := rec.Header().Get("Location"); loc != "/?login_error=unavailable" {
		t.Errorf("IdP 不可达应 unavailable，得到 %d %q", rec.Code, loc)
	}
}
