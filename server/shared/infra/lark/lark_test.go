// All tests in this file are offline: no connection to Feishu, no credentials needed.
//
// What can be tested offline is the "protocol translation" layer: config validation, response
// parsing, error code mapping, URL and addons encoding. Real connectivity checks need a person
// to click "agree" on the authorization page; see the README and example.
//
// The fake Feishu runs on httptest and is injected through Config.AccountsURL / Config.OpenURL.
// The main reason these two fields exist is to let tests redirect requests here.

package lark

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// newFakeLark starts a fake Feishu; each test provides its own handler.
func newFakeLark(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(Config{AccountsURL: srv.URL, OpenURL: srv.URL})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return c
}

// newFakeRegistration starts a fake registration endpoint that only returns a fixed JSON, and
// records the request count and the last action received.
func newFakeRegistration(t *testing.T, body string) (*Client, *int32, *string) {
	t.Helper()
	var calls int32
	lastAction := new(string)
	mux := http.NewServeMux()
	// POST /oauth/v1/app/registration: shared by begin and poll
	mux.HandleFunc(RegistrationPath, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_ = r.ParseForm()
		*lastAction = r.FormValue("action")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	return newFakeLark(t, mux), &calls, lastAction
}

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr error
	}{
		{"零值合法，默认国内域", Config{}, nil},
		{"显式国内", Config{Region: RegionFeishu}, nil},
		{"显式国际", Config{Region: RegionLark}, nil},
		{"未知区域", Config{Region: Region("mars")}, ErrInvalidConfig},
		{"覆盖账号域地址", Config{AccountsURL: "http://127.0.0.1:1"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("期望通过校验，实际报错: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("期望 %v，实际 %v", tc.wantErr, err)
			}
		})
	}
}

// The region decides which domain is called. Empty must fall back to China, or users in China get sent to the international site.
func TestRegionDefaultsAndHosts(t *testing.T) {
	cases := []struct {
		name         string
		region       Region
		wantAccounts string
		wantOpen     string
	}{
		{"留空按国内", "", DefaultAccountsURLFeishu, DefaultOpenURLFeishu},
		{"国内", RegionFeishu, DefaultAccountsURLFeishu, DefaultOpenURLFeishu},
		{"国际", RegionLark, DefaultAccountsURLLark, DefaultOpenURLLark},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(Config{Region: tc.region})
			if err != nil {
				t.Fatalf("New 失败: %v", err)
			}
			if c.accounts != tc.wantAccounts {
				t.Errorf("accounts 期望 %q，实际 %q", tc.wantAccounts, c.accounts)
			}
			if c.open != tc.wantOpen {
				t.Errorf("open 期望 %q，实际 %q", tc.wantOpen, c.open)
			}
		})
	}
}

func TestNormalizeRegionAndOther(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  Region
	}{
		{"空串按国内", "", RegionFeishu},
		{"大小写不敏感", "LARK", RegionLark},
		{"带空格", "  lark  ", RegionLark},
		{"无法识别按国内", "mars", RegionFeishu},
		{"国内", "feishu", RegionFeishu},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeRegion(tc.input); got != tc.want {
				t.Errorf("期望 %v，实际 %v", tc.want, got)
			}
		})
	}
	if RegionFeishu.Other() != RegionLark || RegionLark.Other() != RegionFeishu {
		t.Error("Other 应该在两个域之间互换")
	}
}

// The expiry field has two spellings: testing returns expires_in, the protocol docs say expire_in.
// Accepting only one would read the expiry as 0, and the session would be judged expired at once.
func TestBeginAcceptsBothExpiryFieldNames(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"expires_in（实测的写法）", `{"device_code":"dc","verification_uri_complete":"https://open.feishu.cn/p?user_code=X","expires_in":3600,"interval":5}`, 3600},
		{"expire_in（文档的写法）", `{"device_code":"dc","verification_uri_complete":"https://open.feishu.cn/p?user_code=X","expire_in":1800,"interval":5}`, 1800},
		{"两个都给时取正数", `{"device_code":"dc","verification_uri_complete":"https://open.feishu.cn/p?user_code=X","expires_in":0,"expire_in":900,"interval":5}`, 900},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, action := newFakeRegistration(t, tc.body)
			ch, err := c.Begin(context.Background(), BeginSpec{})
			if err != nil {
				t.Fatalf("Begin 失败: %v", err)
			}
			if ch.ExpiresIn != tc.want {
				t.Errorf("ExpiresIn 期望 %d，实际 %d", tc.want, ch.ExpiresIn)
			}
			if *action != "begin" {
				t.Errorf("action 期望 begin，实际 %q", *action)
			}
		})
	}
}

// When begin is missing key fields it must return an error, not an empty Challenge that cannot be polled.
func TestBeginRejectsIncompleteResponse(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"缺 device_code", `{"verification_uri_complete":"https://open.feishu.cn/p"}`},
		{"缺授权地址", `{"device_code":"dc"}`},
		{"只有 verification_uri 但没 user_code", `{"device_code":"dc","verification_uri":"https://open.feishu.cn/p"}`},
		{"协议返回错误", `{"error":"invalid_request","error_description":"bad archetype"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := newFakeRegistration(t, tc.body)
			if _, err := c.Begin(context.Background(), BeginSpec{}); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("期望 %v，实际 %v", ErrInvalidResponse, err)
			}
		})
	}
}

// verification_uri + user_code can also build the authorization URL, as a fallback when complete is missing.
func TestBeginFallsBackToUserCode(t *testing.T) {
	body := `{"device_code":"dc","verification_uri":"https://open.feishu.cn/page/launcher/","user_code":"7V47-EWMA","expires_in":3600,"interval":5}`
	c, _, _ := newFakeRegistration(t, body)
	ch, err := c.Begin(context.Background(), BeginSpec{})
	if err != nil {
		t.Fatalf("Begin 失败: %v", err)
	}
	if !strings.Contains(ch.VerificationURL, "user_code=7V47-EWMA") {
		t.Errorf("授权地址应带上 user_code，实际 %q", ch.VerificationURL)
	}
}

// Mapping of the six protocol error codes to PollStatus, and which ones are final.
// invalid_grant matters most: it must be the final state PollInvalid, not fall into
// PollUnknown. The latter triggers a retry on the other domain, and the domain is not the problem.
func TestPollStatusMapping(t *testing.T) {
	cases := []struct {
		name         string
		protocolErr  string
		want         PollStatus
		wantTerminal bool
	}{
		{"空错误即成功", "", PollOK, false},
		{"等用户授权", "authorization_pending", PollPending, false},
		{"轮询过快", "slow_down", PollSlowDown, false},
		{"设备码过期", "expired_token", PollExpired, true},
		{"用户拒绝", "access_denied", PollDenied, true},
		{"设备码非法", "invalid_grant", PollInvalid, true},
		{"协议外的错误码", "teapot", PollUnknown, false},
		{"带空格也能识别", "  slow_down  ", PollSlowDown, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pollStatusFor(tc.protocolErr)
			if got != tc.want {
				t.Fatalf("期望 %v，实际 %v", tc.want, got)
			}
			if got.Terminal() != tc.wantTerminal {
				t.Errorf("Terminal 期望 %v，实际 %v", tc.wantTerminal, got.Terminal())
			}
		})
	}
}

// On a successful poll we must get the credentials and tenant_brand (the latter is what the two-domain correction relies on).
func TestPollParsesCredentialAndBrand(t *testing.T) {
	body := `{"client_id":"cli_abc","client_secret":"secret-xyz","user_info":{"tenant_brand":"lark"}}`
	c, calls, action := newFakeRegistration(t, body)
	ticket, err := c.Poll(context.Background(), "dc")
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}
	if ticket.Status != PollOK {
		t.Errorf("Status 期望 %v，实际 %v", PollOK, ticket.Status)
	}
	if ticket.AppID != "cli_abc" || ticket.AppSecret != "secret-xyz" {
		t.Errorf("凭证解析错了：%+v", ticket)
	}
	if ticket.TenantBrand != "lark" {
		t.Errorf("TenantBrand 期望 lark，实际 %q", ticket.TenantBrand)
	}
	if *action != "poll" {
		t.Errorf("action 期望 poll，实际 %q", *action)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("期望只请求 1 次，实际 %d", got)
	}
}

// An empty device_code should be stopped locally, not waste a network round trip.
func TestPollRejectsEmptyDeviceCode(t *testing.T) {
	c, calls, _ := newFakeRegistration(t, `{}`)
	for _, code := range []string{"", "   "} {
		if _, err := c.Poll(context.Background(), code); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("期望 %v，实际 %v", ErrInvalidArgument, err)
		}
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Errorf("期望不发请求，实际发了 %d 次", got)
	}
}

// addons encoding: Go's gzip writes a zero mtime, so the same input gives byte-identical output and can be asserted directly.
func TestEncodeAddons(t *testing.T) {
	t.Run("两者皆空时不带该参数", func(t *testing.T) {
		got, err := encodeAddons(nil, nil)
		if err != nil {
			t.Fatalf("编码失败: %v", err)
		}
		if got != "" {
			t.Errorf("期望空串，实际 %q", got)
		}
	})

	t.Run("同样输入编码结果确定", func(t *testing.T) {
		a, err := encodeAddons([]string{ScopeGroupMessage}, []string{CallbackCardAction})
		if err != nil {
			t.Fatalf("编码失败: %v", err)
		}
		b, err := encodeAddons([]string{ScopeGroupMessage}, []string{CallbackCardAction})
		if err != nil {
			t.Fatalf("编码失败: %v", err)
		}
		if a != b {
			t.Fatalf("同样输入两次编码结果不同：\n%q\n%q", a, b)
		}
		// base64url must not contain the three characters + / =, or they get escaped again inside the query.
		if strings.ContainsAny(a, "+/=") {
			t.Errorf("编码结果含非 URL 安全字符: %q", a)
		}
	})

	t.Run("去重且忽略空串", func(t *testing.T) {
		withDup, err := encodeAddons([]string{ScopeGroupMessage, "", ScopeGroupMessage}, nil)
		if err != nil {
			t.Fatalf("编码失败: %v", err)
		}
		clean, err := encodeAddons([]string{ScopeGroupMessage}, nil)
		if err != nil {
			t.Fatalf("编码失败: %v", err)
		}
		if withDup != clean {
			t.Errorf("去重后应与干净输入一致：\n%q\n%q", withDup, clean)
		}
	})
}

// The authorization page URL must get the display parameters filled in, but must not overwrite values Feishu already gave.
func TestBuildVerificationURL(t *testing.T) {
	t.Run("补齐参数", func(t *testing.T) {
		res := beginResponse{VerificationURIComplete: "https://open.feishu.cn/page/launcher?user_code=X"}
		got, err := buildVerificationURL(res, BeginSpec{
			AppName:      "AMA 助手",
			Source:       "ama",
			AppID:        "cli_old",
			TenantScopes: []string{ScopeGroupMessage},
		})
		if err != nil {
			t.Fatalf("构造失败: %v", err)
		}
		for _, want := range []string{"user_code=X", "from=sdk", "tp=sdk", "source=ama", "clientID=cli_old", "addons="} {
			if !strings.Contains(got, want) {
				t.Errorf("期望 URL 含 %q，实际 %q", want, got)
			}
		}
	})

	t.Run("不覆盖飞书已给的参数", func(t *testing.T) {
		res := beginResponse{VerificationURIComplete: "https://open.feishu.cn/p?user_code=X&from=lark&source=other"}
		got, err := buildVerificationURL(res, BeginSpec{Source: "ama"})
		if err != nil {
			t.Fatalf("构造失败: %v", err)
		}
		if !strings.Contains(got, "from=lark") || strings.Contains(got, "from=sdk") {
			t.Errorf("不该覆盖已有的 from，实际 %q", got)
		}
		if !strings.Contains(got, "source=other") {
			t.Errorf("不该覆盖已有的 source，实际 %q", got)
		}
	})
}

// Protocol states come **with HTTP 400**: in testing, authorization_pending and invalid_grant
// both return 400, not 200. Judging success by status code would treat "still waiting for
// authorization" as a failed request and break the whole polling flow. This bug only shows up
// when you really connect to Feishu once, so the real status codes lock it in here.
func TestPollTreatsHTTP400AsProtocolStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   PollStatus
	}{
		{"400 + authorization_pending（实测）", http.StatusBadRequest, `{"error":"authorization_pending","code":20094}`, PollPending},
		{"400 + invalid_grant（实测）", http.StatusBadRequest, `{"error":"invalid_grant","code":20079}`, PollInvalid},
		{"400 + expired_token", http.StatusBadRequest, `{"error":"expired_token"}`, PollExpired},
		{"400 + slow_down", http.StatusBadRequest, `{"error":"slow_down"}`, PollSlowDown},
		{"200 + 成功也要照旧能用", http.StatusOK, `{"client_id":"cli_a","client_secret":"s"}`, PollOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc(RegistrationPath, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			c := newFakeLark(t, mux)
			ticket, err := c.Poll(context.Background(), "dc")
			if err != nil {
				t.Fatalf("协议状态不该被当成传输错误，实际 %v", err)
			}
			if ticket.Status != tc.want {
				t.Errorf("期望 %v，实际 %v", tc.want, ticket.Status)
			}
		})
	}
}

// Only a response body that is not JSON at all (e.g. the gateway returns HTML) counts as a transport error.
func TestPollSurfacesNonJSONError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(RegistrationPath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`<html><body>502 Bad Gateway</body></html>`))
	})
	c := newFakeLark(t, mux)
	if _, err := c.Poll(context.Background(), "dc"); err == nil {
		t.Fatal("非 JSON 的错误响应应当报错")
	} else if StatusCode(err) != http.StatusBadGateway {
		t.Errorf("状态码期望 502，实际 %d", StatusCode(err))
	}
}

func TestMissingValues(t *testing.T) {
	cases := []struct {
		name     string
		required []string
		actual   []string
		want     []string
	}{
		{"全缺", []string{"a", "b"}, nil, []string{"a", "b"}},
		{"全有", []string{"a", "b"}, []string{"b", "a"}, []string{}},
		{"缺一个", []string{"a", "b"}, []string{"a"}, []string{"b"}},
		{"actual 多出来的不影响", []string{"a"}, []string{"a", "z"}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MissingValues(tc.required, tc.actual)
			// Must be non-nil, or it serializes to JSON null instead of [].
			if got == nil {
				t.Fatal("返回值不应为 nil")
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("期望 %v，实际 %v", tc.want, got)
			}
		})
	}
}

// The check must split grant_status into two groups, "granted" and "awaiting approval"; this
// split is what lets readiness give actionable advice.
func TestInspectAppSplitsGrantedAndUnapproved(t *testing.T) {
	mux := http.NewServeMux()
	// GET /bot/v3/info/: liveness check, also tells whether the bot is enabled
	mux.HandleFunc("/bot/v3/info/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"bot":{"open_id":"ou_1","app_name":"AMA"}}`))
	})
	// GET /application/v6/scopes: permission list
	mux.HandleFunc("/application/v6/scopes", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"scopes":[
			{"scope_name":"application:application:self_manage","grant_status":1},
			{"scope_name":"im:chat","grant_status":1},
			{"scope_name":"im:message.group_msg","grant_status":0}
		]}}`))
	})
	// GET /application/v6/applications/me: app details
	mux.HandleFunc("/application/v6/applications/me", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"app":{"app_name":"AMA 助手","online_version_id":"v1",
			"callback_info":{"callback_type":"websocket","subscribed_callbacks":["card.action.trigger"]}}}}`))
	})
	// GET .../app_versions/v1: events subscribed by the published version
	mux.HandleFunc("/application/v6/applications/me/app_versions/v1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"app_version":{"events":["im.message.receive_v1"]}}}`))
	})

	c := newFakeLark(t, mux)
	cfg, err := c.InspectApp(context.Background(), "token")
	if err != nil {
		t.Fatalf("InspectApp 失败: %v", err)
	}
	if !cfg.Inspectable {
		t.Error("有 self_manage 时应能读到应用详情")
	}
	if !cfg.BotEnabled {
		t.Error("能读到机器人信息就应判为已启用")
	}
	if !cfg.Published {
		t.Error("有 online_version_id 就应判为已发布")
	}
	if !ContainsValue(cfg.GrantedScopes, "im:chat") {
		t.Errorf("grant_status=1 应进已授权，实际 %v", cfg.GrantedScopes)
	}
	if !ContainsValue(cfg.UnapprovedScopes, ScopeGroupMessage) {
		t.Errorf("grant_status=0 应进等审批，实际 %v", cfg.UnapprovedScopes)
	}
	if ContainsValue(cfg.GrantedScopes, ScopeGroupMessage) {
		t.Error("等审批的权限不该同时出现在已授权里")
	}
	if !ContainsValue(cfg.SubscribedEvents, "im.message.receive_v1") {
		t.Errorf("应读到已发布版本的事件，实际 %v", cfg.SubscribedEvents)
	}
}

// Without self_manage the app details cannot be read. Then Inspectable must honestly be false,
// instead of treating "not seen" as "the user did not configure it" and reporting missing events.
func TestInspectAppWithoutSelfManageIsNotInspectable(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/bot/v3/info/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":99991672,"msg":"no permission"}`))
	})
	mux.HandleFunc("/application/v6/scopes", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"scopes":[{"scope_name":"im:chat","grant_status":1}]}}`))
	})
	var detailCalls int32
	mux.HandleFunc("/application/v6/applications/me", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&detailCalls, 1)
		_, _ = w.Write([]byte(`{"code":0,"data":{"app":{}}}`))
	})

	c := newFakeLark(t, mux)
	cfg, err := c.InspectApp(context.Background(), "token")
	if err != nil {
		t.Fatalf("InspectApp 失败: %v", err)
	}
	if cfg.Inspectable {
		t.Error("没有 self_manage 时 Inspectable 应为 false")
	}
	if cfg.BotEnabled {
		t.Error("读不到机器人信息时不应判为已启用")
	}
	if got := atomic.LoadInt32(&detailCalls); got != 0 {
		t.Errorf("没有 self_manage 就不该去读应用详情，实际读了 %d 次", got)
	}
	if len(cfg.SubscribedEvents) != 0 {
		t.Errorf("读不到就该是空，而不是编一个，实际 %v", cfg.SubscribedEvents)
	}
}

// Argument validation and failure handling when exchanging for a tenant token.
func TestTenantAccessToken(t *testing.T) {
	t.Run("入参为空时本地拦下", func(t *testing.T) {
		c := newFakeLark(t, http.NewServeMux())
		if _, err := c.TenantAccessToken(context.Background(), "", "s"); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("appID 为空：期望 %v，实际 %v", ErrInvalidArgument, err)
		}
		if _, err := c.TenantAccessToken(context.Background(), "cli_a", " "); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("appSecret 为空：期望 %v，实际 %v", ErrInvalidArgument, err)
		}
	})

	t.Run("拿到 token", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t-abc"}`))
		})
		c := newFakeLark(t, mux)
		got, err := c.TenantAccessToken(context.Background(), "cli_a", "secret")
		if err != nil {
			t.Fatalf("换 token 失败: %v", err)
		}
		if got != "t-abc" {
			t.Errorf("期望 t-abc，实际 %q", got)
		}
	})

	t.Run("凭证错误时报错", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/auth/v3/tenant_access_token/internal", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"code":10003,"msg":"invalid app_secret"}`))
		})
		c := newFakeLark(t, mux)
		if _, err := c.TenantAccessToken(context.Background(), "cli_a", "bad"); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("期望 %v，实际 %v", ErrInvalidResponse, err)
		}
	})
}

// "An approval request is already pending" and "no permission to request" both count as
// success: the user clicked the button, and the request is already with the admin. An error
// would make people think it was not submitted.
func TestRequestAdminAuthorizationTreatsBenignCodesAsSuccess(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		status  int
		wantErr bool
	}{
		{"成功", `{"code":0}`, http.StatusOK, false},
		{"已有待审批申请", `{"code":212004,"msg":"already pending"}`, http.StatusOK, false},
		{"没有要申请的权限", `{"code":212002,"msg":"nothing to apply"}`, http.StatusOK, false},
		{"HTTP 错误但业务码无害", `{"code":212004,"msg":"already pending"}`, http.StatusBadRequest, false},
		{"真的失败", `{"code":10001,"msg":"boom"}`, http.StatusOK, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/application/v6/scopes/apply", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			c := newFakeLark(t, mux)
			err := c.RequestAdminAuthorization(context.Background(), "token")
			if tc.wantErr && err == nil {
				t.Fatal("期望报错，实际成功")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("期望成功，实际报错: %v", err)
			}
		})
	}
}

// HTTP >= 300 must become a structured error that keeps the status code, business code and log_id.
func TestAPIErrorClassification(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		wantAuth    bool
		wantLimited bool
	}{
		{"401 是鉴权错误", http.StatusUnauthorized, `{"code":99991663,"msg":"invalid token","log_id":"lg1"}`, true, false},
		{"403 也是鉴权错误", http.StatusForbidden, `{"code":99991672,"msg":"no permission"}`, true, false},
		{"429 是限流", http.StatusTooManyRequests, `{"code":99991400,"msg":"too many"}`, false, true},
		{"500 两者都不是", http.StatusInternalServerError, `{"msg":"boom"}`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/bot/v3/info/", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			c := newFakeLark(t, mux)
			_, err := c.BotInfo(context.Background(), "token")
			if err == nil {
				t.Fatal("期望报错，实际成功")
			}
			apiErr, ok := AsAPIError(err)
			if !ok {
				t.Fatalf("期望结构化错误，实际 %v", err)
			}
			if apiErr.HTTPStatusCode != tc.status {
				t.Errorf("状态码期望 %d，实际 %d", tc.status, apiErr.HTTPStatusCode)
			}
			if StatusCode(err) != tc.status {
				t.Errorf("StatusCode 期望 %d，实际 %d", tc.status, StatusCode(err))
			}
			if IsAuthError(err) != tc.wantAuth {
				t.Errorf("IsAuthError 期望 %v，实际 %v", tc.wantAuth, IsAuthError(err))
			}
			if IsRateLimited(err) != tc.wantLimited {
				t.Errorf("IsRateLimited 期望 %v，实际 %v", tc.wantLimited, IsRateLimited(err))
			}
			// The error message must not be an empty shell; at least it should show it came from Feishu.
			if !strings.Contains(apiErr.Error(), "飞书返回") {
				t.Errorf("错误信息可读性不足: %q", apiErr.Error())
			}
		})
	}
}

// log_id is the most useful field when reporting a problem; it must be retrievable.
func TestLogIDExtraction(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/bot/v3/info/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":1,"msg":"boom","log_id":"lg-xyz"}`))
	})
	c := newFakeLark(t, mux)
	_, err := c.BotInfo(context.Background(), "token")
	if got := LogID(err); got != "lg-xyz" {
		t.Errorf("期望 lg-xyz，实际 %q", got)
	}
	if got := LogID(errors.New("普通错误")); got != "" {
		t.Errorf("非飞书错误应返回空串，实际 %q", got)
	}
}
