// Cases for the framework's own helpers: masking, expiry rendering, and the line between "unknown"
// and "broken".
//
// # Why these moved while the rest of integration_test.go did not
//
// The rule is **whether core's Handler is needed**. These test pure functions (maskCredential /
// ExpiryDetail / isTransient) unrelated to routing; the rest there (four-state serialization, route
// dispatch, an empty registry returning []) all need `NewHandler` to build a mux, and **the routing
// and handler step deliberately stays in core**.
//
// ⚠️ These are not optional format tests; each of the three pins a real failure: masking that showed
// the prefix (`sa_g_` gives away the account type), milliseconds parsed as seconds giving the year
// 58624, and calling a network hiccup "authorization expired", so the user wastes a trip
// re-authorizing.
package integration

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// maskCredential **keeps only the tail, not the prefix**, and masks short values entirely.
//
// Showing the prefix is a very common approach, but the prefix is often exactly what should not be
// shown: `sa_g_` gives away the account type, `app-` gives away the tenant. Returning short values as
// is would be worse: those are exactly the ones to hide.
func TestMaskCredentialKeepsTailNotPrefix(t *testing.T) {
	const secret = "sa_g_1234567890abcdef"
	got := maskCredential(secret, 4)

	if got == secret {
		t.Fatal("必须截断")
	}
	// The prefix must not appear: this is the key difference from approaches that "keep the first
	// few characters".
	if strings.Contains(got, "sa_g") {
		t.Errorf("不能露前缀（sa_g 点出了这是应用账号），实际 %q", got)
	}
	if !strings.HasSuffix(got, "cdef") {
		t.Errorf("应保留尾部 4 位便于分辨是哪一把，实际 %q", got)
	}

	// Values too short to mask are masked entirely; they must not be returned as is.
	for _, in := range []string{"abc", "secret", "s"} {
		masked := maskCredential(in, 6)
		if masked == in {
			t.Errorf("maskCredential(%q, 6) 原样返回了输入——短值必须整个打码", in)
		}
		if strings.Contains(masked, in) {
			t.Errorf("maskCredential(%q, 6) = %q，泄漏了原值", in, masked)
		}
	}
	if maskCredential("", 4) != "" {
		t.Error("空串应回空串")
	}
}

// Masking short values must have a **fixed width** and not leak the original length.
//
// strings.Repeat("*", len(s)) outputs exactly len(s) asterisks. That is most harmful for short
// secrets: the length cuts the brute-force space by an order of magnitude.
func TestMaskCredentialDoesNotLeakLength(t *testing.T) {
	short := maskCredential("abc", 6)
	longer := maskCredential("abcdefghij", 20)
	if short != longer {
		t.Errorf("不同长度的短值打码结果必须一样（否则泄漏长度）：%q vs %q", short, longer)
	}
	if len(short) != 3 {
		t.Errorf("应是定宽三星，实际 %q", short)
	}
}

// An absurd expiry time (mixed-up units) is **not shown**, instead of presenting a lie to the user.
//
// A typical failure: some platform's expire_at is in milliseconds; parsed as seconds it gives the
// year 58624, so that token "never expires", and after it really expires every call gets 401. The
// opposite mistake gives 1970. Both would be rendered faithfully by fmtExpiry as numbers like
// "20889000 days left".
func TestExpiryDetailRejectsImplausibleTimes(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		// Milliseconds parsed as seconds: about the year 58624.
		{"毫秒当秒解", time.Unix(1787829588975, 0)},
		// Seconds parsed as milliseconds: 1970.
		{"秒当毫秒解", time.UnixMilli(1787822427)},
	} {
		if d, ok := ExpiryDetail("凭证有效期至", tc.at); ok {
			t.Errorf("%s（%v）不该产出 detail，实际给了 %q", tc.name, tc.at.UTC().Year(), d.Value)
		}
	}
	// Values in the normal range must still be shown.
	if _, ok := ExpiryDetail("凭证有效期至", time.Now().Add(time.Hour)); !ok {
		t.Error("正常时刻应当显示")
	}
}

// ExpiryDetail must return false for the zero time, not format it as 0001-01-01.
//
// That value would be shown by the frontend as a real expiry time.
func TestExpiryDetailSkipsZeroTime(t *testing.T) {
	if _, ok := ExpiryDetail("有效期至", time.Time{}); ok {
		t.Error("零时间不该产出 detail——0001-01-01 会被当成真的过期时间显示")
	}
}

// The expiry must include "how long is left", not just a bare timestamp.
//
// Details are shown as plain text in the frontend; a bare timestamp makes the user work out "how far
// away is that", which is the only thing they want to know. Expired ones must also say "expired".
func TestExpiryDetailSaysHowLongLeft(t *testing.T) {
	future, ok := ExpiryDetail("凭证有效期至", time.Now().Add(14*24*time.Hour))
	if !ok {
		t.Fatal("非零时间应产出 detail")
	}
	if !strings.Contains(future.Value, "还有") {
		t.Errorf("未来时间要说「还有多久」，实际 %q", future.Value)
	}
	// Assert "has a day unit" instead of an exact number of days: 14 days minus a few nanoseconds
	// truncates to 13 days, and pinning the number would only give a case that turns red now and
	// then.
	if !strings.Contains(future.Value, "天") {
		t.Errorf("应给出人能读的时长（天），实际 %q", future.Value)
	}
	// It must also include the real time, not only the relative duration: users sometimes need to
	// match times with the platform console.
	if !strings.Contains(future.Value, time.Now().Format("2006")) {
		t.Errorf("应带上具体时刻，实际 %q", future.Value)
	}

	past, ok := ExpiryDetail("凭证有效期至", time.Now().Add(-2*time.Hour))
	if !ok {
		t.Fatal("过去时间也应产出 detail")
	}
	if !strings.Contains(past.Value, "已过期") {
		t.Errorf("过期的要明说「已过期」，实际 %q", past.Value)
	}
}

// ⭐ **Network hiccups / timeouts must not be called "unauthorized" or "unavailable".**
//
// "Unknown" gets its own handling. Calling it error would make the user authorize again, while their
// authorization is fine, and **that wasted authorization would also overwrite the credential that was
// fine on the platform side**.
//
// So these are always warning, FixHint must say plainly "no need to authorize again", and
// CanAuthorize is false. This pins the rule itself (isTransient); each provider's cases check its
// handling.
func TestTransientErrorsDoNotLookLikeRevoked(t *testing.T) {
	// The rule of isTransient itself: only the ctx and net kinds count, never the message text.
	if !isTransient(context.DeadlineExceeded) {
		t.Error("超时应认成 transient")
	}
	if !isTransient(context.Canceled) {
		t.Error("取消应认成 transient")
	}
	if !isTransient(&net.DNSError{Err: "no such host"}) {
		t.Error("网络错误应认成 transient")
	}
	// Business failures **must not** be treated as transient: that would turn "really not
	// authorized" into "try again later", and the user would never get the authorize button they
	// should click.
	if isTransient(errors.New("credential grant not found")) {
		t.Error("业务失败不该认成 transient")
	}
	if isTransient(nil) {
		t.Error("nil 不是 transient")
	}
}

// ---- Helpers ----------------------------------------------------------------

func hasDetail(details []IntegrationDetail, label, value string) bool {
	for _, d := range details {
		if d.Label == label && d.Value == value {
			return true
		}
	}
	return false
}

func hasDetailLabel(details []IntegrationDetail, label string) bool {
	for _, d := range details {
		if d.Label == label {
			return true
		}
	}
	return false
}
