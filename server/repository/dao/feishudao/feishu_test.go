// All tests in this file run offline: no connection to Feishu, no credentials needed.
//
// The fake Feishu is started with httptest and injected through Config.AccountsURL / Config.OpenURL. Time is injected with
// Config.Now, so expiry logic does not need real waiting.
//
// What is locked down here are a few constraints that do not show right away when broken: auth order, session ownership, dual-domain correction,
// idempotent registration, and "the secret never goes into a response".

package feishudao

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chyroc/ama/server/feishu"
	"github.com/chyroc/ama/server/feishu/feishumodel"
	"github.com/chyroc/ama/server/shared/infra/lark"
)

// fakeLark is a programmable fake Feishu.
type fakeLark struct {
	// pollBody is the response body the next poll returns, consumed in order; once used up the last one repeats.
	pollBodies []string
	pollCalls  int32
	// pollHosts records the Host each poll hit, to verify the domain switch.
	pollHosts []string
	server    *httptest.Server
}

// newFakeLark starts a fake Feishu where begin always succeeds and poll returns pollBodies in order.
func newFakeLark(t *testing.T, pollBodies ...string) *fakeLark {
	t.Helper()
	f := &fakeLark{pollBodies: pollBodies}
	mux := http.NewServeMux()
	// POST /oauth/v1/app/registration: shared by begin and poll
	mux.HandleFunc("/oauth/v1/app/registration", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.FormValue("action") == "begin" {
			_, _ = w.Write([]byte(`{"device_code":"dc-1","verification_uri_complete":"https://open.feishu.cn/p?user_code=X","expires_in":3600,"interval":5}`))
			return
		}
		n := int(atomic.AddInt32(&f.pollCalls, 1)) - 1
		f.pollHosts = append(f.pollHosts, r.Host)
		body := `{"error":"authorization_pending"}`
		if len(f.pollBodies) > 0 {
			if n >= len(f.pollBodies) {
				n = len(f.pollBodies) - 1
			}
			body = f.pollBodies[n]
		}
		// Real Feishu returns HTTP 400 for unfinished protocol states (measured: authorization_pending is
		// 400 + code 20094). The fake Feishu copies this behavior, otherwise tests would be looser than the real environment.
		if strings.Contains(body, `"error"`) {
			w.WriteHeader(http.StatusBadRequest)
		}
		_, _ = w.Write([]byte(body))
	})
	// For inspection: give a "complete" set of responses, so readiness can reach ready.
	mux.HandleFunc("/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t-1"}`))
	})
	mux.HandleFunc("/bot/v3/info/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"bot":{"open_id":"ou_1","app_name":"AMA 助手"}}`))
	})
	mux.HandleFunc("/application/v6/scopes", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(grantedScopesBody()))
	})
	mux.HandleFunc("/application/v6/applications/me", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"app":{"app_name":"AMA 助手","online_version_id":"v1",
			"callback_info":{"callback_type":"websocket","subscribed_callbacks":["card.action.trigger"]}}}}`))
	})
	mux.HandleFunc("/application/v6/applications/me/app_versions/v1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(allEventsBody()))
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// grantedScopesBody builds a response where "all basic scopes are granted".
func grantedScopesBody() string {
	var b strings.Builder
	b.WriteString(`{"code":0,"data":{"scopes":[`)
	for i, scope := range baseScopesForTest() {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"scope_name":"` + scope + `","grant_status":1}`)
	}
	b.WriteString(`]}}`)
	return b.String()
}

func allEventsBody() string {
	var b strings.Builder
	b.WriteString(`{"code":0,"data":{"app_version":{"events":[`)
	for i, e := range baseEventsForTest() {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"` + e + `"`)
	}
	b.WriteString(`]}}}`)
	return b.String()
}

// newTestService builds a Service pointing at the fake Feishu, with controllable time and the in-memory Store.
func newTestService(t *testing.T, f *fakeLark) (*feishu.Service, *time.Time) {
	t.Helper()
	return newTestServiceWith(t, f, feishu.NewMemStore())
}

// newTestServiceWith is the same as newTestService, but the caller picks the Store.
//
// It exists so the same set of assertions can run on both Store implementations: store_pg_test.go uses it to run the end-to-end
// flows below unchanged on PGStore. **There is only one copy of the assertions**, otherwise the two implementations would sooner or later each be green on their own.
//
// Note the injected clock only goes to the Service, **not the Store**. NewMemStore() and NewPGStore() both use
// real time internally, and following them keeps the two implementations equivalent. Really changing the Store's clock would send tests like
// "move time forward" down another branch: the session would already be gone at the Store layer, and the Service's
// `s.now().After(session.ExpiresAt)` would never get to run, so Poll would report
// ErrSessionLost instead of returning the expired status. The Store's own expiry behavior is covered separately by
// TestStoreExpiredSessionIsGone, which is the only place the Store's clock is injected.
func newTestServiceWith(t *testing.T, f *fakeLark, store feishu.Store) (*feishu.Service, *time.Time) {
	t.Helper()
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	clock := &now
	svc, err := feishu.New(feishu.Config{
		Store:       store,
		AccountsURL: f.server.URL,
		OpenURL:     f.server.URL,
		Now:         func() time.Time { return *clock },
		// Explicitly allow: this package's tests check Feishu business logic, not authorization. The production fallback is deny,
		// and without passing this every test would stop at 403 and never test what it means to. See allowAuthorizer.
		Authorizer: allowAuthorizer{},
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return svc, clock
}

const (
	testActor     = "u-alice"
	testWorkspace = "ws-1"
)

func mustBegin(t *testing.T, svc *feishu.Service) *feishu.Challenge {
	t.Helper()
	ch, err := svc.Begin(context.Background(), feishu.BeginOption{ActorID: testActor, WorkspaceID: testWorkspace})
	if err != nil {
		t.Fatalf("Begin 失败: %v", err)
	}
	return ch
}

func TestNewRequiresStore(t *testing.T) {
	if _, err := feishu.New(feishu.Config{}); !errors.Is(err, feishu.ErrInvalidArgument) {
		t.Fatalf("期望 %v，实际 %v", feishu.ErrInvalidArgument, err)
	}
}

// Starting a registration must get a high-entropy session ID and an authorization URL.
func TestBeginReturnsChallenge(t *testing.T) {
	svc, _ := newTestService(t, newFakeLark(t))
	ch := mustBegin(t, svc)
	if len(ch.SessionID) != feishu.SessionIDBytes {
		t.Errorf("会话 ID 长度期望 %d，实际 %d", feishu.SessionIDBytes, len(ch.SessionID))
	}
	if ch.PollIntervalSeconds != 5 {
		t.Errorf("轮询间隔期望 5，实际 %d", ch.PollIntervalSeconds)
	}
	if !strings.Contains(ch.VerificationURL, "user_code=X") {
		t.Errorf("授权地址不对: %q", ch.VerificationURL)
	}
}

// Two starts must give different session IDs: a predictable ID can be enumerated.
func TestBeginSessionIDsAreUnique(t *testing.T) {
	svc, _ := newTestService(t, newFakeLark(t))
	seen := make(map[string]struct{})
	for i := 0; i < 20; i++ {
		ch := mustBegin(t, svc)
		if _, dup := seen[ch.SessionID]; dup {
			t.Fatalf("会话 ID 重复: %q", ch.SessionID)
		}
		seen[ch.SessionID] = struct{}{}
	}
}

// When "group messages" is checked, **the two levels that include bots must be requested together**.
//
// # Why this test deserves to exist on its own
//
// 2026-09-02: the agent did not reply at all in the alert group, and it took two rounds of investigation. The real cause was that Feishu splits "receive group messages"
// into two levels, and we only requested `im:message.group_msg`; that level **explicitly excludes messages sent by
// bots**, so the alert group (43 of 44 messages sent by bots) received not a single event.
//
// This failure has **zero errors**: the long connection is fine, readiness is all green, the inbound table has not one row,
// and the logs read word for word like "everything is fine". So only pinning it here works.
//
// ⚠️ **The cost of a missing request is not "add it later"**: the addons in the authorization page URL are fixed once generated,
// and adding scopes requires the user to **go through the whole authorization again**. So the rule is "request this level fully in one go".
func TestBeginRequestsIncludeBotScopesWithGroupMessage(t *testing.T) {
	svc, _ := newTestService(t, newFakeLark(t))
	ch, err := svc.Begin(context.Background(), feishu.BeginOption{
		ActorID: testActor, WorkspaceID: testWorkspace, RequestGroupMessage: true,
	})
	if err != nil {
		t.Fatalf("Begin 失败: %v", err)
	}
	scopes := decodeTenantScopes(t, ch.VerificationURL)
	for _, want := range []string{
		// All three are needed: reading group history requires the first one by name (feishu/README.md),
		// and the last two are what make "push me the messages sent by bots too".
		lark.ScopeGroupMessage,
		lark.ScopeGroupMessageIncludeBot,
		lark.ScopeGroupAtMessageIncludeBot,
	} {
		if !slices.Contains(scopes, want) {
			t.Errorf("勾了群消息就该一并申请 %q，实际只申请了 %v\n"+
				"（少了 include_bot 那档，报警群里机器人发的告警飞书一条都不会推）",
				want, scopes)
		}
	}
}

// When group messages is not checked, **request no extra scopes at all**: asking for more makes admins hesitate to approve.
func TestBeginWithoutGroupMessageAsksNoExtraScopes(t *testing.T) {
	svc, _ := newTestService(t, newFakeLark(t))
	ch, err := svc.Begin(context.Background(), feishu.BeginOption{
		ActorID: testActor, WorkspaceID: testWorkspace,
	})
	if err != nil {
		t.Fatalf("Begin 失败: %v", err)
	}
	if scopes := decodeTenantScopes(t, ch.VerificationURL); len(scopes) != 0 {
		t.Errorf("没勾就不该申请任何额外权限，实际申请了 %v", scopes)
	}
}

// decodeTenantScopes decodes the addons parameter in the authorization URL back into the tenant scope list.
//
// addons is gzip + base64url JSON (see shared/infra/lark/addons.go). **Decode and check**
// instead of matching the string with Contains: after base64 the scope names never appear in plain text,
// so Contains would always be false; such an assertion looks like it tests something but is really always green.
func decodeTenantScopes(t *testing.T, authorizeURL string) []string {
	t.Helper()
	parsed, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatalf("授权地址解析失败: %v", err)
	}
	addons := parsed.Query().Get("addons")
	if addons == "" {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(addons)
	if err != nil {
		t.Fatalf("addons 不是合法的 base64url: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("addons 不是合法的 gzip: %v", err)
	}
	defer func() { _ = zr.Close() }()
	var payload struct {
		Scopes struct {
			Tenant []string `json:"tenant"`
		} `json:"scopes"`
	}
	if err := json.NewDecoder(zr).Decode(&payload); err != nil {
		t.Fatalf("addons 解 JSON 失败: %v", err)
	}
	return payload.Scopes.Tenant
}

// An empty identity or workspace must be rejected; an empty identity must not get all the way through.
func TestBeginRejectsMissingIdentity(t *testing.T) {
	svc, _ := newTestService(t, newFakeLark(t))
	cases := []struct {
		name        string
		actorID     string
		workspaceID string
	}{
		{"缺身份", "", testWorkspace},
		{"缺空间", testActor, ""},
		{"身份是空格", "   ", testWorkspace},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Begin(context.Background(), feishu.BeginOption{ActorID: tc.actorID, WorkspaceID: tc.workspaceID})
			if !errors.Is(err, feishu.ErrInvalidArgument) && !errors.Is(err, feishu.ErrAccessDenied) {
				t.Fatalf("期望被拒，实际 %v", err)
			}
		})
	}
}

// denyAuthorizer rejects everything, used to check the auth order.
type denyAuthorizer struct{ calls int32 }

func (d *denyAuthorizer) CanAccessWorkspace(ctx context.Context, actorID, workspaceID string) (bool, error) {
	atomic.AddInt32(&d.calls, 1)
	return false, nil
}

func (d *denyAuthorizer) CanManageWorkspace(ctx context.Context, actorID, workspaceID string) (bool, error) {
	atomic.AddInt32(&d.calls, 1)
	return false, nil
}

// allowAuthorizer allows everything, **only used in tests**.
//
// The production fallback is denyAllAuthorizer (always reject; see the "why the fallback is deny" part in feishu.go),
// so most tests in this package, which check **Feishu business logic** like idempotent registration, dual-domain correction and readiness diagnosis,
// not authorization, must explicitly pass an allowing authorizer; otherwise they all stop at 403 and never test what they mean to.
//
// Real authorization decisions are made by server/authz, which has its own tests covering the role matrix.
type allowAuthorizer struct{}

func (allowAuthorizer) CanAccessWorkspace(context.Context, string, string) (bool, error) {
	return true, nil
}

func (allowAuthorizer) CanManageWorkspace(context.Context, string, string) (bool, error) {
	return true, nil
}

// This is the most important test in this package: **polling must authorize first, then look up the session.**
//
// In the wrong order sessionID becomes a de facto credential: whoever guesses it can advance someone else's registration. This uses an
// always-reject Authorizer with a session that really exists: if the implementation looked up the session first, a non-member would
// get a normal response like pending instead of 403.
func TestPollAuthorizesBeforeTouchingSession(t *testing.T) {
	f := newFakeLark(t)
	svc, _ := newTestService(t, f)
	ch := mustBegin(t, svc)

	// Switch to an always-reject authorizer; the session still really exists.
	deny := &denyAuthorizer{}
	svc.SetAuthorizerForTest(deny)

	_, err := svc.Poll(context.Background(), feishu.PollOption{
		ActorID: "u-attacker", WorkspaceID: testWorkspace, SessionID: ch.SessionID,
	})
	if !errors.Is(err, feishu.ErrAccessDenied) {
		t.Fatalf("期望 %v，实际 %v", feishu.ErrAccessDenied, err)
	}
	if atomic.LoadInt32(&deny.calls) == 0 {
		t.Error("鉴权没被调用，说明实现绕过了权限校验")
	}
	// When authorization fails, Feishu must not be called.
	if got := atomic.LoadInt32(&f.pollCalls); got != 0 {
		t.Errorf("鉴权失败后仍调用了飞书 %d 次", got)
	}
}

// A session is bound to its starter: other people in the same workspace (even with admin rights) cannot take over someone else's registration.
func TestPollRejectsNonInitiator(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		svc, _ := newTestServiceWith(t, newFakeLark(t), store)
		ch := mustBegin(t, svc)

		_, err := svc.Poll(context.Background(), feishu.PollOption{
			ActorID: "u-bob", WorkspaceID: testWorkspace, SessionID: ch.SessionID,
		})
		if !errors.Is(err, feishu.ErrAccessDenied) {
			t.Fatalf("期望 %v，实际 %v", feishu.ErrAccessDenied, err)
		}
	})
}

// Polling workspace A's session from workspace B must be rejected.
func TestPollRejectsWorkspaceMismatch(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		svc, _ := newTestServiceWith(t, newFakeLark(t), store)
		ch := mustBegin(t, svc)

		_, err := svc.Poll(context.Background(), feishu.PollOption{
			ActorID: testActor, WorkspaceID: "ws-other", SessionID: ch.SessionID,
		})
		if !errors.Is(err, feishu.ErrAccessDenied) {
			t.Fatalf("期望 %v，实际 %v", feishu.ErrAccessDenied, err)
		}
	})
}

func TestPollRejectsUnknownSession(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		svc, _ := newTestServiceWith(t, newFakeLark(t), store)
		_, err := svc.Poll(context.Background(), feishu.PollOption{
			ActorID: testActor, WorkspaceID: testWorkspace, SessionID: "不存在的会话",
		})
		if !errors.Is(err, feishu.ErrSessionLost) {
			t.Fatalf("期望 %v，实际 %v", feishu.ErrSessionLost, err)
		}
	})
}

// After a session expires, polling returns expired instead of waiting forever.
func TestPollExpiresSession(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		svc, clock := newTestServiceWith(t, newFakeLark(t), store)
		ch := mustBegin(t, svc)

		*clock = clock.Add(feishumodel.SessionTTL + time.Second)

		res, err := svc.Poll(context.Background(), feishu.PollOption{
			ActorID: testActor, WorkspaceID: testWorkspace, SessionID: ch.SessionID,
		})
		if err != nil {
			t.Fatalf("Poll 失败: %v", err)
		}
		if res.Status != feishu.RegistrationExpired {
			t.Errorf("期望 %v，实际 %v", feishu.RegistrationExpired, res.Status)
		}
		if res.Reason == "" {
			t.Error("过期时应给出可读原因")
		}
	})
}

// Mapping of the various protocol states to external states.
func TestPollStatusMapping(t *testing.T) {
	cases := []struct {
		name string
		body string
		want feishu.RegistrationStatus
	}{
		{"等授权", `{"error":"authorization_pending"}`, feishu.RegistrationPending},
		{"过期", `{"error":"expired_token"}`, feishu.RegistrationExpired},
		{"被拒", `{"error":"access_denied"}`, feishu.RegistrationDenied},
		{"设备码非法", `{"error":"invalid_grant"}`, feishu.RegistrationInvalid},
		{"OK 但凭证还没下发", `{"client_id":"","client_secret":""}`, feishu.RegistrationPending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newTestService(t, newFakeLark(t, tc.body))
			ch := mustBegin(t, svc)
			res, err := svc.Poll(context.Background(), feishu.PollOption{
				ActorID: testActor, WorkspaceID: testWorkspace, SessionID: ch.SessionID,
			})
			if err != nil {
				t.Fatalf("Poll 失败: %v", err)
			}
			if res.Status != tc.want {
				t.Errorf("期望 %v，实际 %v", tc.want, res.Status)
			}
		})
	}
}

// slow_down must increase the poll interval, and write it back to the session so later polls use the new interval.
func TestPollSlowDownIncreasesInterval(t *testing.T) {
	f := newFakeLark(t, `{"error":"slow_down"}`, `{"error":"slow_down"}`)
	svc, _ := newTestService(t, f)
	ch := mustBegin(t, svc)
	ctx := context.Background()
	opt := feishu.PollOption{ActorID: testActor, WorkspaceID: testWorkspace, SessionID: ch.SessionID}

	first, err := svc.Poll(ctx, opt)
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}
	want := ch.PollIntervalSeconds + feishu.SlowDownIncrement
	if first.PollIntervalSeconds != want {
		t.Errorf("首次退避后期望 %d，实际 %d", want, first.PollIntervalSeconds)
	}

	second, err := svc.Poll(ctx, opt)
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}
	if second.PollIntervalSeconds != want+feishu.SlowDownIncrement {
		t.Errorf("再次退避后期望 %d，实际 %d", want+feishu.SlowDownIncrement, second.PollIntervalSeconds)
	}
}

// invalid_grant is a final state and has nothing to do with the account domain; it must never trigger a domain-switch retry.
//
// Without mapping this error code separately, it would fall into the "unknown" branch and switch domain to try another round, both wasted and
// covering the real cause with "protocol error".
func TestPollInvalidGrantDoesNotRetryOtherRegion(t *testing.T) {
	f := newFakeLark(t, `{"error":"invalid_grant","error_description":"The device_code is invalid"}`)
	svc, _ := newTestService(t, f)
	ch := mustBegin(t, svc)

	res, err := svc.Poll(context.Background(), feishu.PollOption{
		ActorID: testActor, WorkspaceID: testWorkspace, SessionID: ch.SessionID,
	})
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}
	if res.Status != feishu.RegistrationInvalid {
		t.Fatalf("期望 %v，实际 %v", feishu.RegistrationInvalid, res.Status)
	}
	// A final state must delete the session, not leave it for the frontend to keep polling.
	if _, err := svc.StoreForTest().GetSession(context.Background(), ch.SessionID); !errors.Is(err, feishu.ErrSessionLost) {
		t.Error("终态后会话应已删除")
	}
}

// Dual-domain correction: when poll reports the tenant is actually on the international domain, change the session's domain in place and keep polling, without making the user start over.
func TestPollCorrectsRegionFromTenantBrand(t *testing.T) {
	// First time: OK but no credentials, and it reports tenant_brand=lark (meaning we guessed the wrong domain).
	f := newFakeLark(t, `{"client_id":"","user_info":{"tenant_brand":"lark"}}`)
	svc, _ := newTestService(t, f)
	ch := mustBegin(t, svc)

	res, err := svc.Poll(context.Background(), feishu.PollOption{
		ActorID: testActor, WorkspaceID: testWorkspace, SessionID: ch.SessionID,
	})
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}
	// Still pending: keep waiting after the correction instead of erroring and making the user start over.
	if res.Status != feishu.RegistrationPending {
		t.Fatalf("期望 %v，实际 %v", feishu.RegistrationPending, res.Status)
	}
	session, err := svc.StoreForTest().GetSession(context.Background(), ch.SessionID)
	if err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	if session.Region != "lark" {
		t.Errorf("会话的区域期望被改成 lark，实际 %q", session.Region)
	}
}

// Also try to correct the domain on an unknown error code; when tenant_brand is missing or matches the current one, do not change anything.
func TestCorrectRegionOnlyWhenBrandDiffers(t *testing.T) {
	cases := []struct {
		name    string
		current string
		brand   string
		want    bool
	}{
		{"国内猜错了实际在国际", "feishu", "lark", true},
		{"国际猜错了实际在国内", "lark", "feishu", true},
		{"一致时不改", "feishu", "feishu", false},
		{"没给归属时不改", "feishu", "", false},
		{"给了无法识别的值时不改", "feishu", "mars", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newTestService(t, newFakeLark(t))
			session := feishu.Session{Region: tc.current, ExpiresAt: time.Now().Add(time.Minute)}
			got := svc.CorrectRegion(context.Background(), "s1", &session, tc.brand)
			if got != tc.want {
				t.Fatalf("期望 %v，实际 %v", tc.want, got)
			}
			if tc.want && session.Region == tc.current {
				t.Error("返回改了却没真的改区域")
			}
			if !tc.want && session.Region != tc.current {
				t.Errorf("不该改区域，实际改成了 %q", session.Region)
			}
		})
	}
}

// After authorization succeeds, the app must be stored, with both status and readiness filled in.
func TestPollCompletesAndPersistsApp(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		f := newFakeLark(t, `{"client_id":"cli_ok","client_secret":"s-1","user_info":{"tenant_brand":"feishu"}}`)
		svc, _ := newTestServiceWith(t, f, store)
		ch := mustBegin(t, svc)

		res, err := svc.Poll(context.Background(), feishu.PollOption{
			ActorID: testActor, WorkspaceID: testWorkspace, SessionID: ch.SessionID,
		})
		if err != nil {
			t.Fatalf("Poll 失败: %v", err)
		}
		if res.Status != feishu.RegistrationCompleted {
			t.Fatalf("期望 %v，实际 %v", feishu.RegistrationCompleted, res.Status)
		}
		if res.App == nil {
			t.Fatal("完成时应带上 App")
		}
		if res.App.AppID != "cli_ok" {
			t.Errorf("AppID 期望 cli_ok，实际 %q", res.App.AppID)
		}
		if res.App.CreatedByID != testActor {
			t.Errorf("接入人期望 %q，实际 %q", testActor, res.App.CreatedByID)
		}
		// The fake Feishu gives a "complete" config, so it should go straight to ready.
		if res.App.Readiness == nil || res.App.Readiness.Status != feishu.ReadinessReady {
			t.Errorf("期望 readiness 为 %s，实际 %+v", feishu.ReadinessReady, res.App.Readiness)
		}
		if res.App.Status != feishu.StatusConnected {
			t.Errorf("状态期望 %v，实际 %v", feishu.StatusConnected, res.App.Status)
		}
		// After completion the session must be deleted.
		if _, err := svc.StoreForTest().GetSession(context.Background(), ch.SessionID); !errors.Is(err, feishu.ErrSessionLost) {
			t.Error("完成后会话应已删除")
		}
	})
}

// Idempotent: the same person connecting the same Feishu app again counts as a successful reconnect, not a conflict.
func TestRegistrationIsIdempotentForSameOwner(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		f := newFakeLark(t, `{"client_id":"cli_same","client_secret":"s-new","user_info":{"tenant_brand":"feishu"}}`)
		svc, _ := newTestServiceWith(t, f, store)
		ctx := context.Background()

		first := mustBegin(t, svc)
		res1, err := svc.Poll(ctx, feishu.PollOption{ActorID: testActor, WorkspaceID: testWorkspace, SessionID: first.SessionID})
		if err != nil {
			t.Fatalf("首次 Poll 失败: %v", err)
		}

		// The same person connects once more.
		second := mustBegin(t, svc)
		res2, err := svc.Poll(ctx, feishu.PollOption{ActorID: testActor, WorkspaceID: testWorkspace, SessionID: second.SessionID})
		if err != nil {
			t.Fatalf("重连不该报错，实际 %v", err)
		}
		if res2.Status != feishu.RegistrationCompleted {
			t.Fatalf("重连期望 %v，实际 %v", feishu.RegistrationCompleted, res2.Status)
		}
		// It is the same record; no new app should be added.
		if res2.App.ID != res1.App.ID {
			t.Errorf("重连应回读同一条记录，首次 %q 重连 %q", res1.App.ID, res2.App.ID)
		}
		apps, err := svc.StoreForTest().ListApps(ctx, testWorkspace)
		if err != nil {
			t.Fatalf("列表失败: %v", err)
		}
		if len(apps) != 1 {
			t.Errorf("期望只有 1 个应用，实际 %d", len(apps))
		}
		// The secret must be replaced with the new one.
		secret, err := svc.StoreForTest().AppSecret(ctx, "cli_same")
		if err != nil {
			t.Fatalf("读密钥失败: %v", err)
		}
		if secret != "s-new" {
			t.Errorf("密钥期望被刷新为 s-new，实际 %q", secret)
		}
	})
}

// The other half of idempotency: when someone else already connected this app, it must really report a conflict.
func TestRegistrationRejectsOtherOwner(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		f := newFakeLark(t, `{"client_id":"cli_taken","client_secret":"s-1","user_info":{"tenant_brand":"feishu"}}`)
		svc, _ := newTestServiceWith(t, f, store)
		ctx := context.Background()

		first := mustBegin(t, svc)
		if _, err := svc.Poll(ctx, feishu.PollOption{ActorID: testActor, WorkspaceID: testWorkspace, SessionID: first.SessionID}); err != nil {
			t.Fatalf("首次 Poll 失败: %v", err)
		}

		// A different person connects the same Feishu app.
		other := "u-bob"
		ch, err := svc.Begin(ctx, feishu.BeginOption{ActorID: other, WorkspaceID: testWorkspace})
		if err != nil {
			t.Fatalf("Begin 失败: %v", err)
		}
		_, err = svc.Poll(ctx, feishu.PollOption{ActorID: other, WorkspaceID: testWorkspace, SessionID: ch.SessionID})
		if !errors.Is(err, feishu.ErrAppOwnedByOther) {
			t.Fatalf("期望 %v，实际 %v", feishu.ErrAppOwnedByOther, err)
		}
	})
}

// MissingScopes and UnapprovedScopes must be kept apart: one needs the user to change config, the other can only wait for an admin.
func TestReadinessSeparatesMissingFromUnapproved(t *testing.T) {
	f := newFakeLark(t, `{"client_id":"cli_p","client_secret":"s-1"}`)
	// Override the scope response: self_manage approved, group_msg awaiting approval, the other basic scopes never requested.
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/v1/app/registration", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("action") == "begin" {
			_, _ = w.Write([]byte(`{"device_code":"dc","verification_uri_complete":"https://open.feishu.cn/p?user_code=X","expires_in":3600,"interval":5}`))
			return
		}
		_, _ = w.Write([]byte(`{"client_id":"cli_p","client_secret":"s-1"}`))
	})
	mux.HandleFunc("/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t"}`))
	})
	mux.HandleFunc("/bot/v3/info/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"bot":{"open_id":"ou_1"}}`))
	})
	mux.HandleFunc("/application/v6/scopes", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"scopes":[
			{"scope_name":"application:application:self_manage","grant_status":1},
			{"scope_name":"im:chat","grant_status":0}
		]}}`))
	})
	mux.HandleFunc("/application/v6/applications/me", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"app":{"app_name":"A","online_version_id":"v1",
			"callback_info":{"callback_type":"websocket","subscribed_callbacks":["card.action.trigger"]}}}}`))
	})
	mux.HandleFunc("/application/v6/applications/me/app_versions/v1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(allEventsBody()))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	_ = f

	svc, err := feishu.New(feishu.Config{Store: feishu.NewMemStore(), AccountsURL: srv.URL, OpenURL: srv.URL, Authorizer: allowAuthorizer{}})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	ch := mustBegin(t, svc)
	res, err := svc.Poll(context.Background(), feishu.PollOption{
		ActorID: testActor, WorkspaceID: testWorkspace, SessionID: ch.SessionID,
	})
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}
	r := res.App.Readiness
	// im:chat is "requested, awaiting approval", reported as usual: this item comes from grant_status and is reliable in real time.
	if !containsStr(r.UnapprovedScopes, "im:chat") {
		t.Errorf("im:chat 应在等审批列表里，实际 %v", r.UnapprovedScopes)
	}
	if containsStr(r.MissingScopes, "im:chat") {
		t.Error("已申请等审批的权限不该同时算作缺失——那会让用户重复提交")
	}
	// **With the long connection, "missing scopes" is no longer reported.**
	//
	// This is the core of this round of changes, and the regression guard for the bug the user reported: it used to compare against the "published version snapshot",
	// while long-connection authorization takes effect at runtime and cannot be read from the snapshot, so a perfectly fine app was wrongly reported as
	// "missing 1 scope im:chat" and "missing 8 event subscriptions", forcing the user to request scopes that were already in effect again and again.
	// See the readiness.go file header.
	if len(r.MissingScopes) != 0 {
		t.Errorf("长连接模式下不该报缺权限（快照读不到运行时授权），实际 %v", r.MissingScopes)
	}
}

// In long-connection mode **missing events are not reported based on the published version snapshot**.
//
// This directly locks down the reported symptom: an app with no event subscriptions at all must not be reported as "missing 8 event subscriptions".
func TestReadinessDoesNotReportMissingEventsOnLongConnection(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/v1/app/registration", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("action") == "begin" {
			_, _ = w.Write([]byte(`{"device_code":"dc","verification_uri_complete":"https://open.feishu.cn/p?user_code=X","expires_in":3600,"interval":5}`))
			return
		}
		_, _ = w.Write([]byte(`{"client_id":"cli_p","client_secret":"s-1"}`))
	})
	mux.HandleFunc("/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t"}`))
	})
	mux.HandleFunc("/bot/v3/info/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"bot":{"open_id":"ou_1"}}`))
	})
	// All basic scopes are granted.
	mux.HandleFunc("/application/v6/scopes", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(grantedScopesBody()))
	})
	mux.HandleFunc("/application/v6/applications/me", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"app":{"app_name":"A","online_version_id":"v1",
			"callback_info":{"callback_type":"websocket","subscribed_callbacks":["card.action.trigger"]}}}}`))
	})
	// **The snapshot has no events at all**: this is exactly what a long-connection app really looks like.
	mux.HandleFunc("/application/v6/applications/me/app_versions/v1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"app_version":{"events":[]}}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	svc, err := feishu.New(feishu.Config{Store: feishu.NewMemStore(), AccountsURL: srv.URL, OpenURL: srv.URL, Authorizer: allowAuthorizer{}})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	ch := mustBegin(t, svc)
	res, err := svc.Poll(context.Background(), feishu.PollOption{
		ActorID: testActor, WorkspaceID: testWorkspace, SessionID: ch.SessionID,
	})
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}

	r := res.App.Readiness
	if len(r.MissingEvents) != 0 {
		t.Errorf("长连接模式下不该报缺事件订阅，实际 %v", r.MissingEvents)
	}
	// Scopes complete, callback right, probe passed → it should be ready, not stuck at "to be configured".
	if r.Status != feishu.ReadinessReady {
		t.Errorf("该判为就绪，实际 %q（缺权限 %v 缺事件 %v 缺回调 %v）",
			r.Status, r.MissingScopes, r.MissingEvents, r.MissingCallbacks)
	}
}

// When the callback type is not websocket, report a missing card callback: with the long connection, card buttons only respond with it.
//
// This is also the only consumer of AppConfiguration.CallbackType: it used to be parsed and assigned,
// and then nobody in the whole repo read it.
func TestReadinessReportsMissingCallbackWhenNotWebSocket(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/v1/app/registration", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("action") == "begin" {
			_, _ = w.Write([]byte(`{"device_code":"dc","verification_uri_complete":"https://open.feishu.cn/p?user_code=X","expires_in":3600,"interval":5}`))
			return
		}
		_, _ = w.Write([]byte(`{"client_id":"cli_p","client_secret":"s-1"}`))
	})
	mux.HandleFunc("/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t"}`))
	})
	mux.HandleFunc("/bot/v3/info/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"bot":{"open_id":"ou_1"}}`))
	})
	mux.HandleFunc("/application/v6/scopes", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(grantedScopesBody()))
	})
	// callback_type is http (callback URL mode) and the card callback is subscribed,
	// but it does not work with the long connection, so it must still be reported as missing.
	mux.HandleFunc("/application/v6/applications/me", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"app":{"app_name":"A","online_version_id":"v1",
			"callback_info":{"callback_type":"http","subscribed_callbacks":["card.action.trigger"]}}}}`))
	})
	mux.HandleFunc("/application/v6/applications/me/app_versions/v1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(allEventsBody()))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	svc, err := feishu.New(feishu.Config{Store: feishu.NewMemStore(), AccountsURL: srv.URL, OpenURL: srv.URL, Authorizer: allowAuthorizer{}})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	ch := mustBegin(t, svc)
	res, err := svc.Poll(context.Background(), feishu.PollOption{
		ActorID: testActor, WorkspaceID: testWorkspace, SessionID: ch.SessionID,
	})
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}
	if !containsStr(res.App.Readiness.MissingCallbacks, "card.action.trigger") {
		t.Errorf("回调类型不是 websocket 时该报缺卡片回调，实际 %v",
			res.App.Readiness.MissingCallbacks)
	}
}

// When the group message scope is not granted, readiness must carry the scope name and the authorization page URL; when granted the URL stays empty.
//
// # This guards against a real incident
//
// A user set the trigger mode to "auto-reply in topic groups" and sent messages in a topic group; **the bot did not reply at all,
// with no error whatsoever**. The root cause was not the trigger check (that part was right) but on the Feishu side: without
// the im:message.group_msg scope, **Feishu never pushes group messages that do not @ the bot**;
// the events never reach us, and the inbound table does not get a single row.
//
// The backend had actually already computed GroupMessageScopeGranted, but nothing in the frontend read it,
// so the UI showed ready (green), and the user had no clue. These two fields exist for the UI:
// the scope name keeps the hint text in sync with the backend constant, and an empty/non-empty URL works directly as the "show a hint or not" switch.
func TestReadinessExposesGroupMessageScopeAndAuthURL(t *testing.T) {
	// scopesBody builds a scope response: all basic scopes approved, and each of the two group message levels given or not by parameter.
	//
	// ⚠️ **The two levels must be controllable separately.** Feishu splits "receive group messages" into group_msg (humans only) and
	// group_msg.include_bot:read (including bots), and production really has the combination "the first yes, the second no";
	// the 2026-09-02 incident was exactly that combination. With one combined switch in the fixture,
	// that combination would never be tested, and it is exactly the hardest one to track down.
	scopesBody := func(groupGranted, botGranted bool) string {
		var b strings.Builder
		b.WriteString(`{"code":0,"data":{"scopes":[`)
		for _, scope := range baseScopesForTest() {
			b.WriteString(`{"scope_name":"` + scope + `","grant_status":1},`)
		}
		grant := func(ok bool) string {
			if ok {
				return "1"
			}
			return "0"
		}
		// Scope names come from constants, not copied literals: when a constant changes this must follow, and a copy would silently mismatch.
		b.WriteString(`{"scope_name":"` + lark.ScopeGroupMessage + `","grant_status":` + grant(groupGranted) + `},`)
		b.WriteString(`{"scope_name":"` + lark.ScopeGroupMessageIncludeBot + `","grant_status":` + grant(botGranted) + `}`)
		b.WriteString(`]}}`)
		return b.String()
	}

	for _, tc := range []struct {
		name         string
		groupGranted bool
		botGranted   bool
		wantURL      bool
		wantBotURL   bool
	}{
		{"两档都没授予：两个入口都要给", false, false, true, true},
		{"两档都已授予：不留任何残留提示", true, true, false, false},
		// ⭐ This one is the shape of the 2026-09-02 incident: group messages granted, the include-bot level not.
		// The UI looked completely normal (groupMessageScopeGranted=true), while the alert group
		// got not a single event for 20 hours. Without this case the root cause would not be tested.
		{"只给了真人那档：仍要提示缺「收机器人消息」", true, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/oauth/v1/app/registration", func(w http.ResponseWriter, r *http.Request) {
				_ = r.ParseForm()
				if r.FormValue("action") == "begin" {
					_, _ = w.Write([]byte(`{"device_code":"dc","verification_uri_complete":"https://open.feishu.cn/p?user_code=X","expires_in":3600,"interval":5}`))
					return
				}
				_, _ = w.Write([]byte(`{"client_id":"cli_p","client_secret":"s-1"}`))
			})
			mux.HandleFunc("/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t"}`))
			})
			mux.HandleFunc("/bot/v3/info/", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"code":0,"bot":{"open_id":"ou_1"}}`))
			})
			mux.HandleFunc("/application/v6/scopes", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(scopesBody(tc.groupGranted, tc.botGranted)))
			})
			mux.HandleFunc("/application/v6/applications/me", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"code":0,"data":{"app":{"app_name":"A","online_version_id":"v1",
					"callback_info":{"callback_type":"websocket","subscribed_callbacks":["card.action.trigger"]}}}}`))
			})
			mux.HandleFunc("/application/v6/applications/me/app_versions/v1", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(allEventsBody()))
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			svc, err := feishu.New(feishu.Config{Store: feishu.NewMemStore(), AccountsURL: srv.URL, OpenURL: srv.URL, Authorizer: allowAuthorizer{}})
			if err != nil {
				t.Fatalf("New 失败: %v", err)
			}
			ch := mustBegin(t, svc)
			res, err := svc.Poll(context.Background(), feishu.PollOption{
				ActorID: testActor, WorkspaceID: testWorkspace, SessionID: ch.SessionID,
			})
			if err != nil {
				t.Fatalf("Poll 失败: %v", err)
			}
			r := res.App.Readiness

			// The scope name **must be sent in both cases**: the UI builds its text with it, in sync with the backend constant.
			if r.GroupMessageScope != "im:message.group_msg" {
				t.Errorf("权限名该原样下发，实际 %q", r.GroupMessageScope)
			}
			if r.GroupMessageScopeGranted != tc.groupGranted {
				t.Errorf("授权状态期望 %v，实际 %v", tc.groupGranted, r.GroupMessageScopeGranted)
			}
			if tc.wantURL {
				if r.GroupMessageAuthURL == "" {
					t.Fatal("没授予时必须给出授权页地址，否则用户不知道去哪开——" +
						"这正是那次故障里缺的东西")
				}
				// The URL must be usable directly: it carries the app ID and the scopes to request, so clicking it lands right on the selection page.
				if !strings.Contains(r.GroupMessageAuthURL, "clientID=cli_p") {
					t.Errorf("授权地址该带上这个应用的 ID，实际 %q", r.GroupMessageAuthURL)
				}
				if !strings.Contains(r.GroupMessageAuthURL, "im:message.group_msg") {
					t.Errorf("授权地址该预填要申请的权限，实际 %q", r.GroupMessageAuthURL)
				}
			} else if r.GroupMessageAuthURL != "" {
				t.Errorf("已授予就不该再留授权入口（界面据此隐藏整块提示），实际 %q",
					r.GroupMessageAuthURL)
			}

			// -- The include-bot level, **checked separately** from the one above --
			//
			// The cost of combining them into one boolean is in the comment on the third case of tc: that is exactly the shape of the production incident,
			// "UI all green, zero events in the alert group".
			if r.BotMessageScope != lark.ScopeGroupMessageIncludeBot {
				t.Errorf("含机器人那档的权限名该原样下发，实际 %q", r.BotMessageScope)
			}
			if r.BotMessageScopeGranted != tc.botGranted {
				t.Errorf("含机器人那档的授权状态期望 %v，实际 %v",
					tc.botGranted, r.BotMessageScopeGranted)
			}
			if tc.wantBotURL {
				if r.BotMessageAuthURL == "" {
					t.Fatal("没授予「收机器人消息」时必须给出授权页地址——" +
						"报警群里全是机器人在说话，缺这档就一条消息都收不到")
				}
				if !strings.Contains(r.BotMessageAuthURL, "clientID=cli_p") {
					t.Errorf("授权地址该带上这个应用的 ID，实际 %q", r.BotMessageAuthURL)
				}
				// Both include_bot scopes are requested together; missing one means the user has to make another trip to the open platform.
				if !strings.Contains(r.BotMessageAuthURL, lark.ScopeGroupMessageIncludeBot) {
					t.Errorf("授权地址该预填免 @ 那档 include_bot，实际 %q", r.BotMessageAuthURL)
				}
				if !strings.Contains(r.BotMessageAuthURL, lark.ScopeGroupAtMessageIncludeBot) {
					t.Errorf("授权地址该一并预填 @ 那档 include_bot，实际 %q", r.BotMessageAuthURL)
				}
			} else if r.BotMessageAuthURL != "" {
				t.Errorf("已授予就不该再留授权入口，实际 %q", r.BotMessageAuthURL)
			}
		})
	}
}

// Readiness status derivation. Key point: while the user still has to act themselves, do not report "waiting for admin", otherwise the user just waits.
func TestDeriveStatus(t *testing.T) {
	cases := []struct {
		name string
		in   *feishu.Readiness
		want string
	}{
		{
			"全齐且连通",
			&feishu.Readiness{ConfigurationInspectable: true, BotEnabled: true, Published: true, RuntimeConnected: true},
			feishu.ReadinessReady,
		},
		{
			"全齐但连不上",
			&feishu.Readiness{ConfigurationInspectable: true, BotEnabled: true, Published: true},
			feishu.ReadinessRuntimeUnavailable,
		},
		{
			"只卡在审批上",
			&feishu.Readiness{ConfigurationInspectable: true, BotEnabled: true, Published: true,
				UnapprovedScopes: []string{"im:chat"}},
			feishu.ReadinessWaitingForAdmin,
		},
		{
			"既缺权限又有待审批：要用户先动手，不能说等管理员",
			&feishu.Readiness{ConfigurationInspectable: true, BotEnabled: true, Published: true,
				MissingScopes: []string{"im:chat"}, UnapprovedScopes: []string{"im:message:readonly"}},
			feishu.ReadinessSetupRequired,
		},
		{
			"没发布",
			&feishu.Readiness{ConfigurationInspectable: true, BotEnabled: true},
			feishu.ReadinessSetupRequired,
		},
		{
			"巡检失败",
			&feishu.Readiness{ConfigurationInspectable: true, BotEnabled: true, Published: true,
				RuntimeConnected: true, LastCheckError: "读不到"},
			feishu.ReadinessSetupRequired,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := feishu.DeriveStatus(tc.in); got != tc.want {
				t.Errorf("期望 %s，实际 %s", tc.want, got)
			}
		})
	}
}

// When credentials are invalid, report runtime_unavailable truthfully instead of pretending to be ready.
func TestReadinessReportsBadCredential(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/v1/app/registration", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("action") == "begin" {
			_, _ = w.Write([]byte(`{"device_code":"dc","verification_uri_complete":"https://o.cn/p?user_code=X","expires_in":3600,"interval":5}`))
			return
		}
		_, _ = w.Write([]byte(`{"client_id":"cli_bad","client_secret":"s-bad"}`))
	})
	// No token can be obtained, simulating a rotated secret.
	mux.HandleFunc("/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":10003,"msg":"invalid app_secret"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	svc, err := feishu.New(feishu.Config{Store: feishu.NewMemStore(), AccountsURL: srv.URL, OpenURL: srv.URL, Authorizer: allowAuthorizer{}})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	ch := mustBegin(t, svc)
	res, err := svc.Poll(context.Background(), feishu.PollOption{
		ActorID: testActor, WorkspaceID: testWorkspace, SessionID: ch.SessionID,
	})
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}
	if res.App.Readiness.Status != feishu.ReadinessRuntimeUnavailable {
		t.Errorf("期望 %s，实际 %s", feishu.ReadinessRuntimeUnavailable, res.App.Readiness.Status)
	}
	if res.App.Status != feishu.StatusError {
		t.Errorf("状态期望 %v，实际 %v", feishu.StatusError, res.App.Status)
	}
	if res.App.Readiness.LastRuntimeError == "" {
		t.Error("应给出可读的失败原因")
	}
}

// This locks down "the secret never goes into a response": serialize App once and assert the secret cannot be found in it.
//
// The App struct has no secret field at all, so this test should not fail normally. Its value is that it blows up right away
// when someday someone adds an AppSecret field to App to save effort.
func TestAppJSONNeverContainsSecret(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		const secret = "super-secret-value"
		f := newFakeLark(t, `{"client_id":"cli_j","client_secret":"`+secret+`"}`)
		svc, _ := newTestServiceWith(t, f, store)
		ch := mustBegin(t, svc)

		res, err := svc.Poll(context.Background(), feishu.PollOption{
			ActorID: testActor, WorkspaceID: testWorkspace, SessionID: ch.SessionID,
		})
		if err != nil {
			t.Fatalf("Poll 失败: %v", err)
		}

		// The whole poll result (including App and readiness) must not contain the secret after serialization.
		raw, err := json.Marshal(res)
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		if strings.Contains(string(raw), secret) {
			t.Fatalf("响应里出现了 AppSecret：%s", raw)
		}
		// The secret really was stored in the Store; it just does not leak.
		stored, err := svc.StoreForTest().AppSecret(context.Background(), "cli_j")
		if err != nil || stored != secret {
			t.Errorf("密钥应存在 Store 里，实际 %q err=%v", stored, err)
		}
	})
}

// Lists must be isolated by workspace.
func TestListAppsIsolatesWorkspaces(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		f := newFakeLark(t, `{"client_id":"cli_a","client_secret":"s"}`)
		svc, _ := newTestServiceWith(t, f, store)
		ctx := context.Background()

		ch := mustBegin(t, svc)
		if _, err := svc.Poll(ctx, feishu.PollOption{ActorID: testActor, WorkspaceID: testWorkspace, SessionID: ch.SessionID}); err != nil {
			t.Fatalf("Poll 失败: %v", err)
		}

		mine, err := svc.ListApps(ctx, feishu.ListOption{ActorID: testActor, WorkspaceID: testWorkspace})
		if err != nil {
			t.Fatalf("列表失败: %v", err)
		}
		if len(mine) != 1 {
			t.Fatalf("本空间期望 1 个，实际 %d", len(mine))
		}
		other, err := svc.ListApps(ctx, feishu.ListOption{ActorID: testActor, WorkspaceID: "ws-2"})
		if err != nil {
			t.Fatalf("列表失败: %v", err)
		}
		if len(other) != 0 {
			t.Errorf("别的空间期望 0 个，实际 %d", len(other))
		}
	})
}

// Disconnecting an app must clear its secret too, not leave it in memory.
func TestDeleteAppClearsSecret(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		f := newFakeLark(t, `{"client_id":"cli_d","client_secret":"s"}`)
		svc, _ := newTestServiceWith(t, f, store)
		ctx := context.Background()

		ch := mustBegin(t, svc)
		res, err := svc.Poll(ctx, feishu.PollOption{ActorID: testActor, WorkspaceID: testWorkspace, SessionID: ch.SessionID})
		if err != nil {
			t.Fatalf("Poll 失败: %v", err)
		}
		if err := svc.DeleteApp(ctx, testActor, res.App.ID); err != nil {
			t.Fatalf("断开失败: %v", err)
		}
		if _, err := svc.StoreForTest().GetApp(ctx, res.App.ID); !errors.Is(err, feishu.ErrAppNotFound) {
			t.Error("应用应已删除")
		}
		if _, err := svc.StoreForTest().AppSecret(ctx, "cli_d"); !errors.Is(err, feishu.ErrAppNotFound) {
			t.Error("密钥应随应用一起清掉")
		}
	})
}

// The Store must return copies: a caller changing one must not affect the stored data, otherwise the in-memory implementation and
// a future SQL implementation would behave differently.
// Empty slices must serialize as [], not null.
//
// This one really blew up: the frontend ReadinessCard reads r.missingScopes.length directly, and on null
// the whole page went blank (Cannot read properties of null). The root cause was that
// append([]string(nil), empty slice...) in Clone returns nil; an app that is "fully ready" (all four lists
// empty) triggers exactly this, while the normal path showed no problem.
//
// It also covers the read path of the PG implementation: old data in the database carries null, which decodes as nil, and must be
// caught by Clone on read; otherwise fixing the write path cannot save rows already stored.
func TestEmptySlicesMarshalAsArraysNotNull(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		// A "fully ready" app: the four missing lists and BoundAgentIDs are all empty.
		app := &feishu.App{
			ID: "a_ready", AppID: "cli_ready", WorkspaceID: testWorkspace,
			BoundAgentIDs: []string{},
			Readiness: &feishu.Readiness{
				Status: feishu.ReadinessReady, CheckedAt: time.Now(),
				MissingScopes: []string{}, UnapprovedScopes: []string{},
				MissingEvents: []string{}, MissingCallbacks: []string{},
				ConfigurationInspectable: true, BotEnabled: true,
				Published: true, RuntimeConnected: true,
			},
		}
		if _, err := store.CreateApp(ctx, app, "s"); err != nil {
			t.Fatalf("落库失败: %v", err)
		}

		got, err := store.GetApp(ctx, "a_ready")
		if err != nil {
			t.Fatalf("读取失败: %v", err)
		}
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		// Point them out one by one, so the error shows directly which field came back as null.
		for _, field := range []string{
			"boundAgentIds", "missingScopes", "unapprovedScopes",
			"missingEvents", "missingCallbacks",
		} {
			if strings.Contains(string(raw), `"`+field+`":null`) {
				t.Errorf("%s 序列化成了 null，前端会当场白屏：%s", field, raw)
			}
		}
	})
}

func TestStoreReturnsCopies(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		app := &feishu.App{ID: "a1", AppID: "cli_c", WorkspaceID: testWorkspace, BoundAgentIDs: []string{"ag1"}}
		if _, err := store.CreateApp(ctx, app, "s"); err != nil {
			t.Fatalf("落库失败: %v", err)
		}

		got, err := store.GetApp(ctx, "a1")
		if err != nil {
			t.Fatalf("读取失败: %v", err)
		}
		got.Name = "被改了"
		got.BoundAgentIDs[0] = "篡改"

		again, err := store.GetApp(ctx, "a1")
		if err != nil {
			t.Fatalf("读取失败: %v", err)
		}
		if again.Name == "被改了" {
			t.Error("改动返回值不该影响库里的数据")
		}
		if again.BoundAgentIDs[0] != "ag1" {
			t.Error("切片也要深拷贝，否则元素能被就地改掉")
		}
	})
}

func containsStr(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// baseScopesForTest / baseEventsForTest take their values from the real tables in shared/infra/lark.
//
// Deliberately not copying the constants again in the test: with a copy, if the real table changes and the test is forgotten, the test stays
// all green, checking its own copy instead of product behavior.
func baseScopesForTest() []string { return lark.BaseScopes }

func baseEventsForTest() []string { return lark.BaseEvents }

// ---------------------------------------------------------------------------
// Persistence contract of unfinished turns
// ---------------------------------------------------------------------------
//
// The assertions are written once, and eachStore runs them once each on the in-memory and PG implementations; otherwise the two implementations would sooner or later
// each be green on their own, while production runs the PG branch.

// Unfinished turns must be stored and read back, with not one field missing.
//
// Asserting field by field is not verbose: every column of this table is "needed to rebuild card updates after restart",
// and missing any one sends recovery down an incomplete path (for example, losing ReplyInThread would make the recovered
// card go to the group instead of the original thread).
func TestInflightTurnRoundTrip(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		started := time.Now().UTC().Truncate(time.Millisecond)
		want := feishu.InflightTurnRecord{
			RunID:         "sesn-rt",
			LarkAppID:     "cli_1",
			ChatID:        "oc_1",
			SrcMessageID:  "om_1",
			CardMessageID: "om_card_1",
			ReplyInThread: true,
			LastText:      "已经写了一半",
			Status:        feishumodel.InflightOpen,
			StartedAt:     started,
		}
		if err := store.PutInflightTurn(ctx, want); err != nil {
			t.Fatalf("写未结轮次失败: %v", err)
		}

		recs, err := store.ListInflightTurns(ctx)
		if err != nil {
			t.Fatalf("列未结轮次失败: %v", err)
		}
		if len(recs) != 1 {
			t.Fatalf("应有 1 条记录，实际 %d 条", len(recs))
		}
		got := recs[0]
		if got.RunID != want.RunID {
			t.Errorf("RunID 不对：want %q got %q", want.RunID, got.RunID)
		}
		if got.LarkAppID != want.LarkAppID {
			t.Errorf("LarkAppID 不对：want %q got %q", want.LarkAppID, got.LarkAppID)
		}
		if got.ChatID != want.ChatID {
			t.Errorf("ChatID 不对：want %q got %q", want.ChatID, got.ChatID)
		}
		if got.SrcMessageID != want.SrcMessageID {
			t.Errorf("SrcMessageID 不对：want %q got %q", want.SrcMessageID, got.SrcMessageID)
		}
		if got.CardMessageID != want.CardMessageID {
			t.Errorf("CardMessageID 不对（丢了它就没法 PATCH 原卡）：want %q got %q",
				want.CardMessageID, got.CardMessageID)
		}
		if got.ReplyInThread != want.ReplyInThread {
			t.Errorf("ReplyInThread 不对（丢了它恢复的卡片会跑错位置）：want %v got %v",
				want.ReplyInThread, got.ReplyInThread)
		}
		if got.LastText != want.LastText {
			t.Errorf("LastText 不对（丢了它会把用户已读的内容清掉重来）：want %q got %q",
				want.LastText, got.LastText)
		}
		if got.Status != want.Status {
			t.Errorf("Status 不对：want %q got %q", want.Status, got.Status)
		}
		if !got.StartedAt.Equal(started) {
			t.Errorf("StartedAt 不对（判超时要用它）：want %v got %v", started, got.StartedAt)
		}
	})
}

// An empty CardMessageID must read back empty as is: it means "card not created yet", a different recovery branch from "has a card".
func TestInflightTurnKeepsEmptyCardAnchor(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		if err := store.PutInflightTurn(ctx, feishu.InflightTurnRecord{
			RunID:        "sesn-nocard",
			LarkAppID:    "cli_1",
			SrcMessageID: "om_1",
			Status:       feishumodel.InflightOpen,
			StartedAt:    time.Now(),
		}); err != nil {
			t.Fatalf("写未结轮次失败: %v", err)
		}
		recs, err := store.ListInflightTurns(ctx)
		if err != nil {
			t.Fatalf("列未结轮次失败: %v", err)
		}
		if len(recs) != 1 || recs[0].CardMessageID != "" {
			t.Fatalf("没建卡的记录读回来该是空锚点，实际 %+v", recs)
		}
	})
}

// Claiming may succeed only once: the second time must be blocked, and the count must go up.
func TestClaimInflightTurnOnlyOnce(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		if err := store.PutInflightTurn(ctx, feishu.InflightTurnRecord{
			RunID:        "sesn-claim",
			LarkAppID:    "cli_1",
			SrcMessageID: "om_1",
			Status:       feishumodel.InflightOpen,
			StartedAt:    time.Now(),
		}); err != nil {
			t.Fatalf("写未结轮次失败: %v", err)
		}

		ok, err := store.ClaimInflightTurn(ctx, "sesn-claim")
		if err != nil || !ok {
			t.Fatalf("第一次认领该成功，实际 ok=%v err=%v", ok, err)
		}
		ok, err = store.ClaimInflightTurn(ctx, "sesn-claim")
		if err != nil {
			t.Fatalf("第二次认领报错: %v", err)
		}
		if ok {
			t.Error("第二次认领该被挡掉——两个实例同写一张卡会让内容来回跳")
		}

		recs, err := store.ListInflightTurns(ctx)
		if err != nil {
			t.Fatalf("列未结轮次失败: %v", err)
		}
		if len(recs) != 1 {
			t.Fatalf("应有 1 条记录，实际 %d 条", len(recs))
		}
		if recs[0].RetryCount != 1 {
			t.Errorf("认领该把次数 +1，实际 %d——涨不上去就永远撞不到放弃上限", recs[0].RetryCount)
		}
		if recs[0].Status != feishumodel.InflightResuming {
			t.Errorf("认领后状态该是 %q，实际 %q", feishumodel.InflightResuming, recs[0].Status)
		}
	})
}

// Claiming a turn that does not exist is not an error, it just claims nothing (it may have just finished and been deleted).
func TestClaimInflightTurnMissing(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ok, err := store.ClaimInflightTurn(context.Background(), "sesn-gone")
		if err != nil {
			t.Fatalf("认领不存在的轮次不该报错，实际 %v", err)
		}
		if ok {
			t.Error("不存在的轮次不该认领成功")
		}
	})
}

// Routine updates (refreshing card progress) must not push the claim status and count back.
//
// Pushing them back would let one card refresh make another instance claim it again, giving the same card two writers.
func TestPutInflightTurnKeepsClaimState(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		rec := feishu.InflightTurnRecord{
			RunID:        "sesn-keep",
			LarkAppID:    "cli_1",
			SrcMessageID: "om_1",
			Status:       feishumodel.InflightOpen,
			StartedAt:    time.Now(),
		}
		if err := store.PutInflightTurn(ctx, rec); err != nil {
			t.Fatalf("写未结轮次失败: %v", err)
		}
		if ok, err := store.ClaimInflightTurn(ctx, "sesn-keep"); err != nil || !ok {
			t.Fatalf("认领该成功，实际 ok=%v err=%v", ok, err)
		}

		// Simulate a card refresh during resume: Put again with the original copy (Status=inflight, RetryCount=0).
		rec.CardMessageID = "om_card_9"
		rec.LastText = "又写了一段"
		if err := store.PutInflightTurn(ctx, rec); err != nil {
			t.Fatalf("更新未结轮次失败: %v", err)
		}

		recs, err := store.ListInflightTurns(ctx)
		if err != nil {
			t.Fatalf("列未结轮次失败: %v", err)
		}
		if len(recs) != 1 {
			t.Fatalf("应有 1 条记录，实际 %d 条", len(recs))
		}
		if recs[0].Status != feishumodel.InflightResuming {
			t.Errorf("刷卡不该把认领状态冲回 %q，实际 %q——冲回去就会被重复认领",
				recs[0].Status, feishumodel.InflightResuming)
		}
		if recs[0].RetryCount != 1 {
			t.Errorf("刷卡不该把次数冲回去，实际 %d", recs[0].RetryCount)
		}
		// But the content and anchors must be updated.
		if recs[0].CardMessageID != "om_card_9" || recs[0].LastText != "又写了一段" {
			t.Errorf("刷卡该更新锚点与内容，实际 %+v", recs[0])
		}
	})
}

// The list is ordered by StartedAt ascending: what was interrupted first is recovered first, matching the order the user asked in.
func TestListInflightTurnsSortedByStart(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		base := time.Now().UTC()
		// Written out of order on purpose.
		for _, spec := range []struct {
			id    string
			offAt time.Duration
		}{
			{"sesn-c", 300 * time.Millisecond},
			{"sesn-a", 100 * time.Millisecond},
			{"sesn-b", 200 * time.Millisecond},
		} {
			if err := store.PutInflightTurn(ctx, feishu.InflightTurnRecord{
				RunID:        spec.id,
				LarkAppID:    "cli_1",
				SrcMessageID: "om_" + spec.id,
				Status:       feishumodel.InflightOpen,
				StartedAt:    base.Add(spec.offAt),
			}); err != nil {
				t.Fatalf("写未结轮次失败: %v", err)
			}
		}

		recs, err := store.ListInflightTurns(ctx)
		if err != nil {
			t.Fatalf("列未结轮次失败: %v", err)
		}
		var got []string
		for _, rec := range recs {
			got = append(got, rec.RunID)
		}
		want := []string{"sesn-a", "sesn-b", "sesn-c"}
		if len(got) != len(want) {
			t.Fatalf("应有 %d 条，实际 %v", len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("应按起点升序（先中断的先恢复），want %v got %v", want, got)
			}
		}
	})
}

// Deletion must be idempotent: deleting something missing is not an error. The recovery path calls it unconditionally.
func TestDeleteInflightTurnIsIdempotent(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		if err := store.DeleteInflightTurn(ctx, "sesn-none"); err != nil {
			t.Errorf("删不存在的轮次不该报错，实际 %v", err)
		}
		if err := store.PutInflightTurn(ctx, feishu.InflightTurnRecord{
			RunID:        "sesn-del",
			LarkAppID:    "cli_1",
			SrcMessageID: "om_1",
			Status:       feishumodel.InflightOpen,
			StartedAt:    time.Now(),
		}); err != nil {
			t.Fatalf("写未结轮次失败: %v", err)
		}
		if err := store.DeleteInflightTurn(ctx, "sesn-del"); err != nil {
			t.Fatalf("删除失败: %v", err)
		}
		recs, err := store.ListInflightTurns(ctx)
		if err != nil {
			t.Fatalf("列未结轮次失败: %v", err)
		}
		if len(recs) != 0 {
			t.Errorf("删完该一条不剩，实际 %d 条", len(recs))
		}
		// Deleting again is not an error either.
		if err := store.DeleteInflightTurn(ctx, "sesn-del"); err != nil {
			t.Errorf("重复删除不该报错，实际 %v", err)
		}
	})
}

// Listing an empty store must give an empty slice, not nil: callers range over it directly, and while nil can be ranged,
// by contract this module's list methods always return non-nil (see the Store interface comment).
func TestListInflightTurnsEmptyIsNotNil(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		recs, err := store.ListInflightTurns(context.Background())
		if err != nil {
			t.Fatalf("列未结轮次失败: %v", err)
		}
		if recs == nil {
			t.Error("空结果该是空切片而不是 nil")
		}
	})
}

// AgentID must round-trip, **otherwise restart recovery cannot ask "should this agent show its chain of thought"**.
//
// A separate test instead of merging into TestInflightTurnRoundTrip: that one uses a record with every field filled,
// while this one must also lock down "an empty AgentID (existing row) reads back as an empty string instead of an error":
// agent_id is a later-added nullable column, and scanning into a bare string fails right away on NULL,
// which would stop the whole recovery flow, with the symptom "after restart no unfinished turn was recovered".
func TestInflightTurnRoundTripsAgentID(t *testing.T) {
	eachStore(t, func(t *testing.T, store feishu.Store) {
		ctx := context.Background()
		started := time.Now().UTC().Truncate(time.Millisecond)

		withAgent := feishu.InflightTurnRecord{
			RunID: "sesn-with-agent", LarkAppID: "cli_1", ChatID: "oc_1",
			SrcMessageID: "om_1", AgentID: "agent_42",
			Status: feishumodel.InflightOpen, StartedAt: started,
		}
		// The one without AgentID simulates **an existing row**: the column was added later, and it is NULL in old data.
		legacy := feishu.InflightTurnRecord{
			RunID: "sesn-legacy", LarkAppID: "cli_1", ChatID: "oc_1",
			SrcMessageID: "om_2",
			Status:       feishumodel.InflightOpen, StartedAt: started.Add(time.Second),
		}
		for _, rec := range []feishu.InflightTurnRecord{withAgent, legacy} {
			if err := store.PutInflightTurn(ctx, rec); err != nil {
				t.Fatalf("写未结轮次失败: %v", err)
			}
		}

		recs, err := store.ListInflightTurns(ctx)
		if err != nil {
			t.Fatalf("列未结轮次失败: %v", err)
		}
		got := make(map[string]string, len(recs))
		for _, r := range recs {
			got[r.RunID] = r.AgentID
		}
		if got["sesn-with-agent"] != "agent_42" {
			t.Errorf("AgentID 该往返，得到 %q——丢了它重启后就问不出该不该展示思维链",
				got["sesn-with-agent"])
		}
		if got["sesn-legacy"] != "" {
			t.Errorf("存量行该读出空串，得到 %q", got["sesn-legacy"])
		}
	})
}
