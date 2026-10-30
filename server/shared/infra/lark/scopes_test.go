// Building the permission-request page URL.
//
// These cases guard one thing: **when the user clicks the link, it must land on the right
// open platform, with the permissions already filled in**.
// Getting it wrong raises no error. The user either reaches a page that says the app does not
// exist, or reaches the page but has to find the permissions themselves. Both get read as
// "AMA's hint is wrong".

package lark

import (
	"net/url"
	"strings"
	"testing"
)

// The China domain and the international domain are **two separate open platforms**. With the
// wrong domain the user cannot find their own app over there.
//
// This one asserts region by region and is the most important test in this file: if the
// if branch in ScopeApplyURL is ever hard-coded to a single domain, only this test goes red.
func TestScopeApplyURLPicksHostByRegion(t *testing.T) {
	cases := []struct {
		name     string
		region   Region
		wantHost string
	}{
		{"国内飞书", RegionFeishu, "open.feishu.cn"},
		{"国际 Lark", RegionLark, "open.larksuite.com"},
		// Empty and garbage values both map to the China domain, same as NormalizeRegion.
		{"空 region 落默认域", Region(""), "open.feishu.cn"},
		{"脏值落默认域", Region("nonsense"), "open.feishu.cn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := ScopeApplyURL(tc.region, "cli_1", ScopeGroupMessage)
			parsed, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("拼出来的地址解析不了: %v（%q）", err, raw)
			}
			if parsed.Host != tc.wantHost {
				t.Errorf("期望落在 %s，实际 %s（%q）", tc.wantHost, parsed.Host, raw)
			}
			if parsed.Scheme != "https" {
				t.Errorf("必须是 https，实际 %q", parsed.Scheme)
			}
		})
	}
}

// Both query parameters must be present: clientID picks the app, scopes decides which items the
// page pre-checks.
//
// Without scopes the user has to dig for im:message.group_msg among hundreds of permissions.
// That is exactly what this link is meant to save.
func TestScopeApplyURLCarriesAppAndScopes(t *testing.T) {
	raw := ScopeApplyURL(RegionFeishu, "cli_abc", ScopeGroupMessage)
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := parsed.Query().Get("clientID"); got != "cli_abc" {
		t.Errorf("clientID 期望 cli_abc，实际 %q", got)
	}
	if got := parsed.Query().Get("scopes"); got != ScopeGroupMessage {
		t.Errorf("scopes 期望 %q，实际 %q", ScopeGroupMessage, got)
	}
}

// Multiple permissions are separated by a **literal comma**; it must not be escaped to %2C.
//
// The open platform splits this parameter on literal commas. If escaped, the whole thing is
// read as one permission name and the page checks nothing. Yet the address bar looks right,
// which is why this needs its own test.
func TestScopeApplyURLJoinsScopesWithLiteralComma(t *testing.T) {
	raw := ScopeApplyURL(RegionFeishu, "cli_1", ScopeGroupMessage, "im:chat")
	if strings.Contains(raw, "%2C") || strings.Contains(raw, "%2c") {
		t.Errorf("逗号被转义了，开放平台会把它当成一个权限名: %q", raw)
	}
	if !strings.Contains(raw, ScopeGroupMessage+","+"im:chat") {
		t.Errorf("两个权限该用逗号连起来，实际 %q", raw)
	}
}

// When no meaningful URL can be built, return an empty string, so the caller can decide
// "should this entry be shown at all".
//
// Returning a URL with missing parameters is worse: the UI shows a button that 404s when
// clicked, and the user thinks they did something wrong.
func TestScopeApplyURLEmptyWhenNothingToAsk(t *testing.T) {
	cases := []struct {
		name   string
		appID  string
		scopes []string
	}{
		{"没有 appID", "", []string{ScopeGroupMessage}},
		{"appID 只有空白", "   ", []string{ScopeGroupMessage}},
		{"没要任何权限", "cli_1", nil},
		{"权限全是空串", "cli_1", []string{"", "  "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ScopeApplyURL(RegionFeishu, tc.appID, tc.scopes...); got != "" {
				t.Errorf("该返回空串，实际 %q", got)
			}
		})
	}
}
