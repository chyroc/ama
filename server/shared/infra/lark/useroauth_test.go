// Tests for user OAuth (device-code authorization + refresh).
//
// All offline: the fake Feishu runs on httptest and is injected through Config.AccountsURL /
// OpenURL.
//
// ⚠️ The most important group of cases here pins "**the body of an HTTP 400 response must be
// parsed**". These two APIs put protocol states in a 400 body (authorization_pending is 400),
// while the generic do() does not parse the body on 4xx. Using it would treat "the user has not
// agreed yet" as a hard error: **polling gives up on the first try and the user can never
// authorize**, while the error only says "400".
// That bug was actually written once during implementation, so these cases must not be deleted.
package lark

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fakeOAuth starts a fake Feishu and dispatches to the two APIs by path.
//
// status and body are both given by the caller, because the key of this protocol is "the
// combination of status code and body".
func fakeOAuth(t *testing.T, status int, body string) *Client {
	t.Helper()
	return newFakeLark(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestBeginUserAuthOK(t *testing.T) {
	c := fakeOAuth(t, http.StatusOK, `{
		"device_code": "dc_abc",
		"user_code": "ABCD-1234",
		"verification_uri": "https://accounts.feishu.cn/open-apis/oauth/v1/device",
		"verification_uri_complete": "https://accounts.feishu.cn/open-apis/oauth/v1/device?user_code=ABCD-1234",
		"expires_in": 3600,
		"interval": 5
	}`)

	ch, err := c.BeginUserAuth(context.Background(), "cli_x", "secret")
	if err != nil {
		t.Fatalf("BeginUserAuth 失败: %v", err)
	}
	if ch.DeviceCode != "dc_abc" {
		t.Errorf("device_code = %q，想要 dc_abc", ch.DeviceCode)
	}
	// **Prefer the complete one**: it includes user_code, so the user does not type it; typing it
	// by hand is the step in this flow that fails most often. Mutation check: swap the two arguments
	// of firstNonEmpty in the implementation, and this assertion must fail.
	if !strings.Contains(ch.VerificationURL, "user_code=ABCD-1234") {
		t.Errorf("授权地址该优先用 verification_uri_complete，实际 %q", ch.VerificationURL)
	}
	if ch.ExpiresIn != 3600 || ch.Interval != 5 {
		t.Errorf("expiresIn/interval = %d/%d，想要 3600/5", ch.ExpiresIn, ch.Interval)
	}
}

// When verification_uri_complete is missing, fall back to verification_uri instead of returning an empty string.
func TestBeginUserAuthFallsBackToPlainURI(t *testing.T) {
	c := fakeOAuth(t, http.StatusOK, `{
		"device_code": "dc_abc",
		"verification_uri": "https://example.com/device",
		"expires_in": 600
	}`)
	ch, err := c.BeginUserAuth(context.Background(), "cli_x", "secret")
	if err != nil {
		t.Fatalf("BeginUserAuth 失败: %v", err)
	}
	if ch.VerificationURL != "https://example.com/device" {
		t.Errorf("该退回 verification_uri，实际 %q", ch.VerificationURL)
	}
}

// expire_in (missing an s) must also be accepted: accepting only expires_in would read the expiry as 0.
func TestBeginUserAuthAcceptsExpireInAlias(t *testing.T) {
	c := fakeOAuth(t, http.StatusOK,
		`{"device_code":"dc","verification_uri":"https://e.com/d","expire_in":1800}`)
	ch, err := c.BeginUserAuth(context.Background(), "cli_x", "secret")
	if err != nil {
		t.Fatalf("BeginUserAuth 失败: %v", err)
	}
	if ch.ExpiresIn != 1800 {
		t.Errorf("expiresIn = %d，想要 1800（expire_in 别名没收到）", ch.ExpiresIn)
	}
}

// App does not exist: tested as 400 + invalid_client + code 20048. Must return an error that includes the code.
func TestBeginUserAuthReportsProtocolErrorOn400(t *testing.T) {
	c := fakeOAuth(t, http.StatusBadRequest,
		`{"error":"invalid_client","error_description":"The specified app does not exist.","code":20048}`)
	_, err := c.BeginUserAuth(context.Background(), "cli_gone", "secret")
	if err == nil {
		t.Fatal("应用不存在时该报错")
	}
	if !errors.Is(err, ErrInvalidResponse) {
		t.Errorf("该是 ErrInvalidResponse，实际 %v", err)
	}
	// The code must appear in the error: debugging relies on it to tell "app was deleted" from other failures.
	if !strings.Contains(err.Error(), "20048") {
		t.Errorf("错误里该带 code 20048，实际 %q", err.Error())
	}
}

// ⭐⭐ **client_secret must be in the request body**; this pins a pitfall hit on a real setup.
//
// Without it Feishu returns `20140 The auth method is not supported.`, which sounds like "**this
// app** does not support device code", so debugging goes completely the wrong way (checking the
// app type, archetype, open platform switches). Tested: lark-cli's own app_id, **known to work
// with device code**, returns the same error, and adding the secret makes it succeed at once.
//
// Mutation check: delete the "client_secret" line of the form in the implementation, and this must fail.
func TestBeginUserAuthSendsClientSecret(t *testing.T) {
	var gotID, gotSecret string
	c := newFakeLark(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("解析 form 失败: %v", err)
		}
		gotID = r.FormValue("client_id")
		gotSecret = r.FormValue("client_secret")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_code":"dc","verification_uri":"https://e.com/d"}`))
	}))

	if _, err := c.BeginUserAuth(context.Background(), "cli_x", "sec-must-be-sent"); err != nil {
		t.Fatalf("BeginUserAuth 失败: %v", err)
	}
	if gotID != "cli_x" {
		t.Errorf("client_id = %q", gotID)
	}
	if gotSecret != "sec-must-be-sent" {
		t.Errorf("❌ client_secret 没进请求体（实际 %q）——少了它飞书回 20140，"+
			"而那个错误指向「应用不支持」，会让排查完全跑偏", gotSecret)
	}
}

// A missing appSecret must be **stopped at the start step**.
//
// Two reasons: the device code step itself needs it (see above), and failing only when
// exchanging the token, after the user has scanned the code, would waste that scan.
func TestBeginUserAuthRequiresSecretUpfront(t *testing.T) {
	c := fakeOAuth(t, http.StatusOK, `{"device_code":"dc"}`)
	if _, err := c.BeginUserAuth(context.Background(), "cli_x", "  "); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("缺 appSecret 该回 ErrInvalidArgument，实际 %v", err)
	}
}

// ⭐ This is the core of this file: **HTTP 400 + authorization_pending is not an error**.
//
// It pins two things at once: the 4xx body is parsed, and pending maps to a non-final state.
// Mutation check: change postJSONAllowProtocolError in userTokenRequest back to postJSON, and
// this must fail (res.Error becomes an empty string, so it goes into the "transport error" branch).
func TestPollUserAuthPendingIsNotAnError(t *testing.T) {
	c := fakeOAuth(t, http.StatusBadRequest,
		`{"error":"authorization_pending","error_description":"waiting","code":20034}`)

	tok, status, err := c.PollUserAuth(context.Background(), "cli_x", "secret", "dc_abc")
	if err != nil {
		t.Fatalf("还在等用户同意不该是 error，实际 %v", err)
	}
	if status != PollPending {
		t.Errorf("status = %q，想要 %q", status, PollPending)
	}
	if status.Terminal() {
		t.Error("pending 不该是终态——是终态的话轮询会立刻放弃，用户永远授权不上")
	}
	if tok != nil {
		t.Errorf("pending 时不该有 token，实际 %+v", tok)
	}
}

// slow_down is likewise a non-final state.
func TestPollUserAuthSlowDownIsNotTerminal(t *testing.T) {
	c := fakeOAuth(t, http.StatusBadRequest, `{"error":"slow_down"}`)
	_, status, err := c.PollUserAuth(context.Background(), "cli_x", "s", "dc")
	if err != nil {
		t.Fatalf("slow_down 不该是 error，实际 %v", err)
	}
	if status != PollSlowDown || status.Terminal() {
		t.Errorf("status = %q terminal=%v，想要 slow_down 且非终态", status, status.Terminal())
	}
}

func TestPollUserAuthSuccess(t *testing.T) {
	c := fakeOAuth(t, http.StatusOK, `{
		"access_token": "u-at-real",
		"refresh_token": "ur-rt-real",
		"expires_in": 7200,
		"refresh_token_expires_in": 604800,
		"scope": "offline_access im:message.group_msg:get_as_user",
		"token_type": "Bearer"
	}`)

	tok, status, err := c.PollUserAuth(context.Background(), "cli_x", "secret", "dc")
	if err != nil {
		t.Fatalf("PollUserAuth 失败: %v", err)
	}
	if status != PollOK {
		t.Fatalf("status = %q，想要 ok", status)
	}
	if tok.AccessToken != "u-at-real" || tok.RefreshToken != "ur-rt-real" {
		t.Errorf("token 取错了: %+v", tok)
	}
	if tok.ExpiresIn != 7200 || tok.RefreshTokenExpiresIn != 604800 {
		t.Errorf("两个有效期 = %d/%d，想要 7200/604800",
			tok.ExpiresIn, tok.RefreshTokenExpiresIn)
	}
	// scope must be returned as is: the answer to "why can't I read group history" is often here.
	if !strings.Contains(tok.Scope, ScopeUserReadGroupMessage) {
		t.Errorf("scope 没带回来: %q", tok.Scope)
	}
}

// The user clicked deny / the code expired: final states, and they must be distinguishable from pending.
func TestPollUserAuthTerminalStatuses(t *testing.T) {
	cases := []struct {
		errCode string
		want    PollStatus
	}{
		{"access_denied", PollDenied},
		{"expired_token", PollExpired},
		{"invalid_grant", PollInvalid},
		// ⚠️ invalid_client must be final: the app was deleted on the Feishu side, and polling ten thousand more times will not help.
		{"invalid_client", PollInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.errCode, func(t *testing.T) {
			c := fakeOAuth(t, http.StatusBadRequest, `{"error":"`+tc.errCode+`","code":1}`)
			_, status, err := c.PollUserAuth(context.Background(), "cli_x", "s", "dc")
			if status != tc.want {
				t.Errorf("status = %q，想要 %q", status, tc.want)
			}
			if !status.Terminal() {
				t.Errorf("%s 该是终态", tc.errCode)
			}
			if err == nil {
				t.Errorf("%s 该报错（终态要让调用方停下来）", tc.errCode)
			}
		})
	}
}

// ⭐ On a successful refresh, **the new refresh_token must be returned**.
//
// Feishu rotates refresh_token. Storing only access means the next refresh uses the old one,
// which may already be invalid. The symptom is "after refreshing for a while, everything
// suddenly needs authorizing again".
func TestRefreshUserTokenReturnsRotatedRefreshToken(t *testing.T) {
	c := fakeOAuth(t, http.StatusOK, `{
		"access_token": "u-at-new",
		"refresh_token": "ur-rt-NEW",
		"expires_in": 7200,
		"refresh_token_expires_in": 604800
	}`)
	tok, status, err := c.RefreshUserToken(context.Background(), "cli_x", "s", "ur-rt-OLD")
	if err != nil || status != PollOK {
		t.Fatalf("续期失败: status=%q err=%v", status, err)
	}
	if tok.RefreshToken != "ur-rt-NEW" {
		t.Errorf("refresh_token = %q，想要轮换后的 ur-rt-NEW", tok.RefreshToken)
	}
}

// ⭐ A failed refresh must tell "authorization is gone" from "failed this time".
//
// This split decides whether the card shows an error or a warning (see the core side). Mixing
// them into one means a network blip gets reported as "invalid", and the user is asked to
// authorize again and again.
func TestRefreshUserTokenDistinguishesRevokedFromTransient(t *testing.T) {
	t.Run("invalid_grant 是真失效", func(t *testing.T) {
		c := fakeOAuth(t, http.StatusBadRequest,
			`{"error":"invalid_grant","error_description":"The refresh token provided is invalid.","code":20026}`)
		_, status, err := c.RefreshUserToken(context.Background(), "cli_x", "s", "rt")
		if status != PollInvalid {
			t.Errorf("status = %q，想要 invalid", status)
		}
		if !status.Terminal() {
			t.Error("invalid_grant 该是终态：必须让用户重新授权")
		}
		if err == nil {
			t.Error("该报错")
		}
	})

	t.Run("飞书 5xx 只是这次没打通", func(t *testing.T) {
		// 5xx with a body that is not a protocol error → transport error → PollUnknown (non-final).
		c := fakeOAuth(t, http.StatusBadGateway, `<html>502 Bad Gateway</html>`)
		_, status, err := c.RefreshUserToken(context.Background(), "cli_x", "s", "rt")
		if err == nil {
			t.Fatal("5xx 该回 error")
		}
		if status != PollUnknown {
			t.Errorf("status = %q，想要 unknown（这样上层才会显示 warning 而不是 error）", status)
		}
		if status.Terminal() {
			t.Error("5xx 不该是终态——把它当终态会让一次抖动毁掉一条好凭据")
		}
	})
}

// Neither error nor access_token: treat as pending and poll again; do not fail.
func TestUserTokenRequestTreatsEmptyAsPending(t *testing.T) {
	c := fakeOAuth(t, http.StatusOK, `{}`)
	tok, status, err := c.PollUserAuth(context.Background(), "cli_x", "s", "dc")
	if err != nil {
		t.Fatalf("空响应不该报错: %v", err)
	}
	if status != PollPending || tok != nil {
		t.Errorf("status=%q tok=%+v，想要 pending + nil", status, tok)
	}
}

// When starting authorization, scope must be built into the request body from UserAuthScopes,
// **space separated**.
//
// ⚠️ This pins "we really request the scope for reading group history". Missing it gives zero
// errors: authorization still succeeds, but the agent is denied as soon as it reads group
// history, and that denial points to permissions, not here.
func TestBeginUserAuthRequestsHistoryScopes(t *testing.T) {
	var gotScope string
	c := newFakeLark(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("解析 form 失败: %v", err)
		}
		gotScope = r.FormValue("scope")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_code":"dc","verification_uri":"https://e.com/d"}`))
	}))

	if _, err := c.BeginUserAuth(context.Background(), "cli_x", "secret"); err != nil {
		t.Fatalf("BeginUserAuth 失败: %v", err)
	}
	for _, want := range []string{ScopeOfflineAccess, ScopeUserReadGroupMessage} {
		if !strings.Contains(gotScope, want) {
			t.Errorf("申请的 scope 里少了 %q，实际 %q", want, gotScope)
		}
	}
	// Space separated (OAuth spec), not commas: commas make Feishu treat it as one whole scope name.
	if strings.Contains(gotScope, ",") {
		t.Errorf("scope 该用空格分隔，实际 %q", gotScope)
	}
}

// The token exchange request body must carry the right grant_type and client credentials.
func TestPollUserAuthSendsDeviceCodeGrant(t *testing.T) {
	var body map[string]string
	c := newFakeLark(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at","expires_in":7200}`))
	}))
	if _, _, err := c.PollUserAuth(context.Background(), "cli_x", "sec", "dc_1"); err != nil {
		t.Fatalf("PollUserAuth 失败: %v", err)
	}
	if got := body["grant_type"]; got != "urn:ietf:params:oauth:grant-type:device_code" {
		t.Errorf("grant_type = %q，不是设备码那个", got)
	}
	for k, want := range map[string]string{
		"client_id": "cli_x", "client_secret": "sec", "device_code": "dc_1",
	} {
		if body[k] != want {
			t.Errorf("%s = %q，想要 %q", k, body[k], want)
		}
	}
}

func TestRefreshUserTokenSendsRefreshGrant(t *testing.T) {
	var body map[string]string
	c := newFakeLark(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at","expires_in":7200}`))
	}))
	if _, _, err := c.RefreshUserToken(context.Background(), "cli_x", "sec", "rt_1"); err != nil {
		t.Fatalf("RefreshUserToken 失败: %v", err)
	}
	if body["grant_type"] != "refresh_token" {
		t.Errorf("grant_type = %q，想要 refresh_token", body["grant_type"])
	}
	if body["refresh_token"] != "rt_1" {
		t.Errorf("refresh_token = %q，想要 rt_1", body["refresh_token"])
	}
}

// ExpiryFrom conversion: passing now explicitly is what lets us pin the result.
func TestExpiryFrom(t *testing.T) {
	base := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	got := ExpiryFrom(base, 7200)
	want := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("ExpiryFrom = %v，想要 %v", got, want)
	}
}

// Empty arguments always give ErrInvalidArgument; do not send a request that is sure to fail.
func TestUserOAuthRejectsEmptyArgs(t *testing.T) {
	c := fakeOAuth(t, http.StatusOK, `{}`)
	ctx := context.Background()
	if _, err := c.BeginUserAuth(ctx, "", "s"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("空 appID 该被拦: %v", err)
	}
	if _, _, err := c.PollUserAuth(ctx, "cli_x", "s", ""); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("空 device_code 该被拦: %v", err)
	}
	if _, _, err := c.RefreshUserToken(ctx, "cli_x", "s", ""); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("空 refresh_token 该被拦: %v", err)
	}
}
