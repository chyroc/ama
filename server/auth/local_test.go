package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// newLocalService builds a Service with local accounts on. bcrypt is set to the lowest cost,
// otherwise each hash takes several hundred milliseconds under -race.
func newLocalService(t *testing.T, cfg LocalConfig) (*Service, *MemStore) {
	t.Helper()
	cfg.Enabled = true
	store := NewMemStore()
	svc, err := New(Config{Store: store, Local: cfg})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	svc.bcryptCost = bcrypt.MinCost
	return svc, store
}

func TestSignupFirstUserBecomesAdminEvenWhenSignupClosed(t *testing.T) {
	svc, store := newLocalService(t, LocalConfig{AllowSignup: false})
	ctx := context.Background()

	if !svc.Providers(ctx).Local.SignupOpen {
		t.Fatal("空库时应该显示注册入口（允许注册第一个账号）")
	}
	res, err := svc.Signup(ctx, "Root@Example.com", "correct horse", "管理员")
	if err != nil {
		t.Fatalf("第一个账号应能注册: %v", err)
	}
	if res.Identity.Email != "root@example.com" || res.Identity.Name != "管理员" || res.SessionToken == "" {
		t.Errorf("注册结果不对: %+v", res)
	}
	acct, err := store.GetLocalAccount(ctx, "root@example.com")
	if err != nil || !acct.Admin || !acct.Bootstrap {
		t.Errorf("第一个账号应是平台管理员: %+v, %v", acct, err)
	}
	// The plain text is not stored.
	if acct.PasswordHash == "correct horse" || !strings.HasPrefix(acct.PasswordHash, "$2") {
		t.Errorf("存的不是 bcrypt 哈希: %q", acct.PasswordHash)
	}
	if ok, _ := svc.IsPlatformAdmin(ctx, "root@example.com"); !ok {
		t.Error("IsPlatformAdmin 应为 true")
	}

	// Second person: sign-up is closed → rejected.
	if _, err := svc.Signup(ctx, "bob@example.com", "password123", ""); !errors.Is(err, ErrSignupDisabled) {
		t.Errorf("关闭注册后第二个人应 ErrSignupDisabled，得到 %v", err)
	}
	if svc.Providers(ctx).Local.SignupOpen {
		t.Error("已有用户且关着注册时不该显示注册入口")
	}
}

func TestSignupOpen(t *testing.T) {
	svc, store := newLocalService(t, LocalConfig{AllowSignup: true})
	ctx := context.Background()
	if _, err := svc.Signup(ctx, "a@example.com", "password123", ""); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Signup(ctx, "b@example.com", "password123", "")
	if err != nil {
		t.Fatalf("开放注册时第二个人应能注册: %v", err)
	}
	if res.Identity.Name != "b" {
		t.Errorf("名字留空应退化成邮箱前缀，得到 %q", res.Identity.Name)
	}
	if acct, _ := store.GetLocalAccount(ctx, "b@example.com"); acct.Admin {
		t.Error("后来注册的不该是管理员")
	}
	if _, err := svc.Signup(ctx, "B@example.com", "password456", ""); !errors.Is(err, ErrEmailTaken) {
		t.Errorf("重复邮箱应 ErrEmailTaken，得到 %v", err)
	}
}

func TestSignupValidation(t *testing.T) {
	svc, _ := newLocalService(t, LocalConfig{AllowSignup: true})
	ctx := context.Background()
	for _, tc := range []struct {
		name, email, pw string
		want            error
	}{
		{"邮箱格式", "not-an-email", "password123", ErrInvalidArgument},
		{"带显示名", "张三 <a@example.com>", "password123", ErrInvalidArgument},
		{"密码太短", "a@example.com", "short", ErrWeakPassword},
		{"密码超过 72 字节", "a@example.com", strings.Repeat("x", 73), ErrWeakPassword},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Signup(ctx, tc.email, tc.pw, ""); !errors.Is(err, tc.want) {
				t.Errorf("期望 %v，得到 %v", tc.want, err)
			}
		})
	}
}

func TestLocalDisabled(t *testing.T) {
	svc := newTestService(t, nil, APIKey{})
	if _, err := svc.Signup(context.Background(), "a@example.com", "password123", ""); !errors.Is(err, ErrLocalDisabled) {
		t.Errorf("Signup 期望 ErrLocalDisabled，得到 %v", err)
	}
	if _, err := svc.LoginWithPassword(context.Background(), "a@example.com", "password123"); !errors.Is(err, ErrLocalDisabled) {
		t.Errorf("Login 期望 ErrLocalDisabled，得到 %v", err)
	}
}

func TestLoginWithPassword(t *testing.T) {
	svc, _ := newLocalService(t, LocalConfig{})
	ctx := context.Background()
	if _, err := svc.Signup(ctx, "alice@example.com", "password123", "Alice"); err != nil {
		t.Fatal(err)
	}

	res, err := svc.LoginWithPassword(ctx, " ALICE@example.com ", "password123")
	if err != nil {
		t.Fatalf("正确密码应能登录: %v", err)
	}
	if res.Identity.Email != "alice@example.com" || res.Identity.Name != "Alice" {
		t.Errorf("身份不对: %+v", res.Identity)
	}
	if id, err := svc.Resolve(ctx, res.SessionToken); err != nil || id.Email != "alice@example.com" {
		t.Errorf("新会话解析不出来: %+v, %v", id, err)
	}

	// A wrong password and a missing account **are the same error** (no way to enumerate emails).
	if _, err := svc.LoginWithPassword(ctx, "alice@example.com", "wrong-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("错密码期望 ErrInvalidCredentials，得到 %v", err)
	}
	if _, err := svc.LoginWithPassword(ctx, "nobody@example.com", "password123"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("不存在的账号期望 ErrInvalidCredentials，得到 %v", err)
	}
	if _, err := svc.LoginWithPassword(ctx, "", "x"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("空邮箱期望 ErrInvalidArgument，得到 %v", err)
	}
}

func TestLoginRateLimit(t *testing.T) {
	svc, store := newLocalService(t, LocalConfig{MaxFailures: 3, FailureWindow: 15 * time.Minute})
	ctx := context.Background()
	if _, err := svc.Signup(ctx, "alice@example.com", "password123", ""); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return base }

	for i := 0; i < 3; i++ {
		if _, err := svc.LoginWithPassword(ctx, "alice@example.com", "bad"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("第 %d 次错密码期望 ErrInvalidCredentials，得到 %v", i+1, err)
		}
	}
	// Over the limit, **even the correct password is not let in**: otherwise rate limiting only
	// slows guessing down, and a right guess still gets in.
	if _, err := svc.LoginWithPassword(ctx, "alice@example.com", "password123"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("超限后期望 ErrTooManyAttempts，得到 %v", err)
	}
	// Rate limiting is per email: others are not affected.
	if _, err := svc.LoginWithPassword(ctx, "other@example.com", "bad"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("别的邮箱不该被一起锁住，得到 %v", err)
	}
	// The count is in the Store, not inside the Service process (only then is it the same budget
	// across instances).
	store.mu.RLock()
	f := store.failures[failureKey("alice@example.com")]
	store.mu.RUnlock()
	if f.failures < 4 {
		t.Errorf("Store 里的计数 = %d，期望 ≥ 4", f.failures)
	}

	// It recovers by itself after the window; a successful login clears it.
	svc.now = func() time.Time { return base.Add(16 * time.Minute) }
	if _, err := svc.LoginWithPassword(ctx, "alice@example.com", "password123"); err != nil {
		t.Fatalf("窗口过后应能登录: %v", err)
	}
	store.mu.RLock()
	_, still := store.failures[failureKey("alice@example.com")]
	store.mu.RUnlock()
	if still {
		t.Error("登录成功后计数应被清掉")
	}
}

// When several Service instances (= several amad processes) share one Store, the budget is shared.
func TestLoginRateLimitSharedAcrossInstances(t *testing.T) {
	store := NewMemStore()
	mk := func() *Service {
		s, _ := New(Config{Store: store, Local: LocalConfig{Enabled: true, MaxFailures: 2}})
		s.bcryptCost = bcrypt.MinCost
		return s
	}
	a, b := mk(), mk()
	ctx := context.Background()
	if _, err := a.Signup(ctx, "alice@example.com", "password123", ""); err != nil {
		t.Fatal(err)
	}
	_, _ = a.LoginWithPassword(ctx, "alice@example.com", "bad")
	_, _ = b.LoginWithPassword(ctx, "alice@example.com", "bad")
	if _, err := a.LoginWithPassword(ctx, "alice@example.com", "password123"); !errors.Is(err, ErrTooManyAttempts) {
		t.Errorf("两个实例的失败应合并计数，得到 %v", err)
	}
}

func TestLogoutRevokesPasswordSession(t *testing.T) {
	svc, _ := newLocalService(t, LocalConfig{})
	ctx := context.Background()
	res, err := svc.Signup(ctx, "alice@example.com", "password123", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Logout(ctx, res.SessionToken); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Resolve(ctx, res.SessionToken); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("登出后会话应失效，得到 %v", err)
	}
}

func TestCreateLocalUserRequiresPlatformAdmin(t *testing.T) {
	svc, _ := newLocalService(t, LocalConfig{})
	ctx := context.Background()
	if _, err := svc.Signup(ctx, "root@example.com", "password123", ""); err != nil {
		t.Fatal(err)
	}
	id, err := svc.CreateLocalUser(ctx, "root@example.com", "bob@example.com", "password123", "Bob", false)
	if err != nil || id.Email != "bob@example.com" {
		t.Fatalf("管理员应能建账号: %+v, %v", id, err)
	}
	if _, err := svc.CreateLocalUser(ctx, "bob@example.com", "carol@example.com", "password123", "", false); !errors.Is(err, ErrForbidden) {
		t.Errorf("非管理员应 ErrForbidden，得到 %v", err)
	}
	if _, err := svc.CreateLocalUser(ctx, "root@example.com", "bob@example.com", "password123", "", false); !errors.Is(err, ErrEmailTaken) {
		t.Errorf("重复邮箱应 ErrEmailTaken，得到 %v", err)
	}
	// The created person can log in with that password.
	if _, err := svc.LoginWithPassword(ctx, "bob@example.com", "password123"); err != nil {
		t.Errorf("被建的账号应能登录: %v", err)
	}
	// An admin from the config (with no local account row) also counts.
	svc.admins["ops@example.com"] = true
	if _, err := svc.CreateLocalUser(ctx, "ops@example.com", "dave@example.com", "password123", "", false); err != nil {
		t.Errorf("配置名单里的管理员应能建账号: %v", err)
	}
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

func postJSON(mux http.Handler, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName && c.Value != "" {
			return c
		}
	}
	return nil
}

func TestPasswordHandlersEndToEnd(t *testing.T) {
	svc, _ := newLocalService(t, LocalConfig{})
	h, _ := NewHandler(HandlerConfig{Service: svc})
	mux := http.NewServeMux()
	h.Register(mux)

	// Register the first account → logged in directly (cookie set).
	rec := postJSON(mux, PathSignup, `{"email":"root@example.com","password":"password123","name":"Root"}`)
	if rec.Code != http.StatusOK || sessionCookie(rec) == nil {
		t.Fatalf("注册: %d %s", rec.Code, rec.Body.String())
	}
	// The second person's sign-up is rejected.
	rec = postJSON(mux, PathSignup, `{"email":"bob@example.com","password":"password123"}`)
	if rec.Code != http.StatusForbidden || decode(t, rec)["code"] != "signup_disabled" {
		t.Errorf("第二个注册: %d %s", rec.Code, rec.Body.String())
	}
	// Wrong password gives 401 + invalid_credentials, no cookie.
	rec = postJSON(mux, PathLogin, `{"email":"root@example.com","password":"nope-nope"}`)
	if rec.Code != http.StatusUnauthorized || decode(t, rec)["code"] != "invalid_credentials" || sessionCookie(rec) != nil {
		t.Errorf("错密码: %d %s", rec.Code, rec.Body.String())
	}
	// Log in with the correct password.
	rec = postJSON(mux, PathLogin, `{"email":"root@example.com","password":"password123"}`)
	cookie := sessionCookie(rec)
	if rec.Code != http.StatusOK || cookie == nil {
		t.Fatalf("登录: %d %s", rec.Code, rec.Body.String())
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("Cookie 属性不对: %+v", cookie)
	}
	// me recognizes the user.
	req := httptest.NewRequest(http.MethodGet, PathMe, nil)
	req.AddCookie(cookie)
	me := httptest.NewRecorder()
	mux.ServeHTTP(me, req)
	if user, _ := decode(t, me)["user"].(map[string]any); user["email"] != "root@example.com" {
		t.Errorf("me = %s", me.Body.String())
	}
	// Logout revokes: the same cookie on me now gives 401.
	if rec := postJSON(mux, PathLogout, ``, cookie); rec.Code != http.StatusOK {
		t.Fatalf("登出: %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, PathMe, nil)
	req.AddCookie(cookie)
	me = httptest.NewRecorder()
	mux.ServeHTTP(me, req)
	if me.Code != http.StatusUnauthorized {
		t.Errorf("登出后旧 Cookie 应失效，得到 %d", me.Code)
	}
}

// Submissions that are not JSON are always rejected: a cross-site <form> can only send simple
// Content-Types, so this blocks login CSRF.
func TestPasswordLoginRequiresJSON(t *testing.T) {
	svc, _ := newLocalService(t, LocalConfig{})
	h, _ := NewHandler(HandlerConfig{Service: svc})
	mux := http.NewServeMux()
	h.Register(mux)
	req := httptest.NewRequest(http.MethodPost, PathLogin, strings.NewReader("email=a@example.com&password=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("表单提交应被拒，得到 %d", rec.Code)
	}
}

func TestPasswordLoginTooManyAttemptsHTTP(t *testing.T) {
	svc, _ := newLocalService(t, LocalConfig{MaxFailures: 1})
	h, _ := NewHandler(HandlerConfig{Service: svc})
	mux := http.NewServeMux()
	h.Register(mux)
	_ = postJSON(mux, PathLogin, `{"email":"a@example.com","password":"bad"}`)
	rec := postJSON(mux, PathLogin, `{"email":"a@example.com","password":"bad"}`)
	if rec.Code != http.StatusTooManyRequests || decode(t, rec)["code"] != "too_many_attempts" {
		t.Errorf("期望 429 too_many_attempts，得到 %d %s", rec.Code, rec.Body.String())
	}
}
