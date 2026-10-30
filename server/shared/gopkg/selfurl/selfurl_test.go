package selfurl

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(host string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Host = host
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// ⚠️ **This group guards against account takeover.**
//
// redirect_uri must never come from request headers: both `Host` and `X-Forwarded-Host` can be
// changed by the caller. An attacker uses `Host: evil.com` to get an authorization link and tricks
// a victim into clicking through; the one-time code then lands on evil.com and he exchanges it for a
// token. So once publicURL is configured, forged headers must be ignored.
func TestCallbackPrefersPublicURLOverRequest(t *testing.T) {
	spoofed := map[string]string{
		"X-Forwarded-Host":  "evil.example.com",
		"X-Forwarded-Proto": "https",
	}
	got, err := Callback("https://ama.example.com", req("evil.example.com", spoofed), "/api/auth/callback")
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if got != "https://ama.example.com/api/auth/callback" {
		t.Fatalf("= %q —— 请求头把回调地址带跑了，这是账号接管", got)
	}
}

// A trailing slash on publicURL must not produce `//api/...` (that is the shape of a
// protocol-relative path, which easily causes other problems), so it is trimmed first.
func TestCallbackTrimsTrailingSlash(t *testing.T) {
	got, err := Callback("https://ama.example.com/", req("x", nil), "/api/auth/callback")
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if got != "https://ama.example.com/api/auth/callback" {
		t.Errorf("= %q", got)
	}
}

// Without publicURL, **only local requests** may fall back. This gap is safe: an attacker who sets
// a loopback Host gets nothing (the code is sent to his own machine), and in return local
// development and worktrees (each on a different port) work without configuration.
func TestCallbackFallsBackOnlyForLoopback(t *testing.T) {
	for _, tc := range []struct {
		name, host string
		want       string
	}{
		{"127.0.0.1 带端口", "127.0.0.1:5347", "http://127.0.0.1:5347/cb"},
		{"localhost", "localhost:3000", "http://localhost:3000/cb"},
		{"IPv6 回环", "[::1]:8080", "http://[::1]:8080/cb"},
		{"内网 10.x（开发机直连）", "10.1.2.3:5273", "http://10.1.2.3:5273/cb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Callback("", req(tc.host, nil), "/cb")
			if err != nil {
				t.Fatalf("本机请求应该允许回落: %v", err)
			}
			if got != tc.want {
				t.Errorf("= %q, 期望 %q", got, tc.want)
			}
		})
	}
}

// No publicURL and not a local request -> **return an error, do not guess one**.
//
// A wrong guessed redirect_uri only fails at the callback step after the user has authorized, and
// that error does not point at the config; failing right here lets the handler say "the server has
// no publicURL configured".
func TestCallbackRefusesUnknownHost(t *testing.T) {
	for _, tc := range []struct{ name, host string }{
		{"线上域名但没配 publicURL", "ama.example.com"},
		{"任意外部域名", "evil.example.com"},
		{"空 Host", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Callback("", req(tc.host, nil), "/cb")
			if err == nil {
				t.Fatalf("期望报错，却拿到 %q", got)
			}
			if got != "" {
				t.Errorf("报错时应返回空串，得到 %q", got)
			}
		})
	}
}

// FromRequest is for "display links" and may follow the request; that is exactly how it differs
// from Callback. This also locks in the behavior openapi relies on: http://127.0.0.1 locally,
// https in production.
func TestFromRequest(t *testing.T) {
	for _, tc := range []struct {
		name, host string
		headers    map[string]string
		want       string
	}{
		{"本机按 http", "127.0.0.1:5273", nil, "http://127.0.0.1:5273"},
		{
			// In production TLS terminates at the gateway and the process sees plain HTTP, so
			// **do not look at r.TLS**; the signal is X-Forwarded-Proto.
			name: "线上按网关给的 proto", host: "ama.example.com",
			headers: map[string]string{"X-Forwarded-Proto": "https"},
			want:    "https://ama.example.com",
		},
		{
			name: "非本机且网关没给 proto 时默认 https", host: "ama.example.com",
			want: "https://ama.example.com",
		},
		{
			// It may be a comma-separated list; take the first one (what the original client saw).
			name: "X-Forwarded-Host 取第一个", host: "internal:8080",
			headers: map[string]string{"X-Forwarded-Host": "ama.example.com, proxy.internal"},
			want:    "https://ama.example.com",
		},
		{"拿不到 Host 时返回空串（退化成相对路径）", "", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FromRequest(req(tc.host, tc.headers)); got != tc.want {
				t.Errorf("= %q, 期望 %q", got, tc.want)
			}
		})
	}
}

func TestIsLoopback(t *testing.T) {
	for _, h := range []string{
		"localhost", "127.0.0.1", "127.0.0.1:5273", "::1", "[::1]:8080",
		"0.0.0.0", "127.5.5.5", "192.168.1.9:5273", "10.1.2.3",
	} {
		if !IsLoopback(h) {
			t.Errorf("%q 应该算本机/内网", h)
		}
	}
	for _, h := range []string{
		"ama.example.com", "evil.example.com", "8.8.8.8",
		// ⚠️ Do not write the prefix check as "contains 10." or similar: these two are not private addresses.
		"110.1.2.3", "192.1681.1",
	} {
		if IsLoopback(h) {
			t.Errorf("%q 不该算本机/内网", h)
		}
	}
}
