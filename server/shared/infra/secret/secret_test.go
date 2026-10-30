// Tests for the redaction primitives.
//
// This file pins a kind of **silent failure**: a credential ends up in logs / the database / the
// model context with no error at all, and it cannot be undone afterwards (the log platform keeps
// logs for a long time, RunEvent is a permanent record). So every assertion has to ask "if this
// were wrong, would anyone notice" — the answer is almost always "no", so there must be a test.

package secret

import (
	"strings"
	"testing"
)

// theJWT is a JWT with a realistic shape (the signature part carries an eye-catching phrase, to
// make it easy to assert "not even a fragment is left").
const theJWT = "eyJhbGciOiJIUzI1NiJ9.eyJ1c2VybmFtZSI6ImFsaWNlIn0.s1gn4tur3-do-not-log-me"

// ⭐ Mask **keeps only the tail, never the prefix**.
//
// The prefix is exactly what should least be exposed: `sa_g_` reveals the account type, `app-`
// reveals the tenant. Showing the prefix is a very common way to write it, so this must be pinned.
func TestMaskKeepsTailNeverPrefix(t *testing.T) {
	const s = "sa_g_secret_value_1234567890_TAIL"
	got := Mask(s, 4)

	if !strings.HasSuffix(got, "TAIL") {
		t.Errorf("应保留尾部 4 位，实际 %q", got)
	}
	if strings.Contains(got, "sa_g_") {
		t.Errorf("泄漏了前缀（那是账号类型信息）：%q", got)
	}
	if strings.Contains(got, "secret_value") {
		t.Errorf("泄漏了主体：%q", got)
	}
}

// Short values must be **masked entirely**, not returned as is: returning them as is means short
// secrets leak directly. And the mask width is fixed, so it does not reveal the length (the
// length cuts the brute-force space by an order of magnitude).
func TestMaskFullyHidesShortValuesAndLength(t *testing.T) {
	for _, in := range []string{"a", "abc", "short", "exactly6"} {
		got := Mask(in, 6)
		if got == in {
			t.Errorf("Mask(%q) 原样返回了——短值必须整个打码", in)
		}
		if strings.Contains(got, in) {
			t.Errorf("Mask(%q) = %q，泄漏了原值", in, got)
		}
	}

	// Short values of different lengths must mask to the same width, otherwise the length leaks.
	if Mask("abc", 6) != Mask("abcdefghij", 20) {
		t.Error("打码宽度随长度变化，泄漏了长度信息")
	}
	if Mask("", 4) != "" {
		t.Error("空值应返回空")
	}
}

// ⭐ Redact must recognize a JWT and **leave no recognizable fragment**.
func TestRedactRemovesJWTEntirely(t *testing.T) {
	in := "调用失败 Authorization=Bearer " + theJWT + " status=401"
	got := Redact(in)

	if strings.Contains(got, theJWT) {
		t.Errorf("JWT 明文仍在：%s", got)
	}
	// The signature part is the most sensitive part of a JWT; not even a fragment may remain.
	if strings.Contains(got, "s1gn4tur3-do-not-log-me") {
		t.Errorf("留下了签名片段：%s", got)
	}
	if !strings.Contains(got, Placeholder) {
		t.Errorf("应留占位符让人知道这里被抹了：%s", got)
	}
	// The surrounding debugging info must stay; erasing too aggressively makes logs useless.
	if !strings.Contains(got, "status=401") {
		t.Errorf("误伤了无关内容：%s", got)
	}
}

// ⭐ Redact must recognize the family where "the key name states 'this is a credential'
// explicitly".
//
// These shapes all come from real leak scenarios: the output of `env`, the command echo of
// `set -x`, and platform SDKs stuffing the whole request into an error message.
func TestRedactCoversCredentialShapedKeys(t *testing.T) {
	cases := map[string]string{
		"env 输出":        "SOME_CLI_USER_JWT=abcdef1234567890secret",
		"下划线 api_key":   "ark_api_key=sk-0123456789abcdef",
		"驼峰 Token":      "arkToken: 0123456789abcdefghij",
		"password":      "password=hunter2hunter2hunter2",
		"AK/SK":         "CLOUD_SECRET_ACCESS_KEY=WlhtbGJXRnVJam9pWVd4",
		"cookie":        "cookie=session=9f8e7d6c5b4a39281706",
		"Bearer 头":      "Authorization: Bearer 0123456789abcdefghijklmn",
		"带引号的值":         `app_secret="qwertyuiopasdfghjkl"`,
		"feishu secret": "FEISHU_APP_SECRET=zZzZzZzZzZzZzZzZzZzZ",
	}
	for name, in := range cases {
		got := Redact(in)
		if got == in {
			t.Errorf("%s：完全没抹——%s", name, got)
			continue
		}
		if !strings.Contains(got, Placeholder) {
			t.Errorf("%s：没留占位符——%s", name, got)
		}
	}
}

// ⚠️ **Bare hex must not be hit by mistake**: commit shas, logids and UUIDs are all bare hex, and
// they are exactly what matters most when debugging. Losing them also gives no error.
func TestRedactKeepsNonSecretIdentifiers(t *testing.T) {
	keep := []string{
		"commit=7a6f26bd1234567890abcdef1234567890abcdef",
		"logid=021787912024092fdbddc0100ff031d08049b4fb112f78b965947",
		"sandbox_id=550e8400-e29b-41d4-a716-446655440000",
		"user=alice space=default role=owner",
		"已连上 PostgreSQL host=10.0.0.1 port=5432 db=ama",
		"HTTP 服务启动 addr=:8080",
	}
	for _, in := range keep {
		if got := Redact(in); got != in {
			t.Errorf("误伤了排查必需的信息：\n  输入 %s\n  输出 %s", in, got)
		}
	}
}

// Key names must stay: the word `token` itself is not a secret, and when debugging you need to see
// "this field really had a value".
func TestRedactKeepsKeyNames(t *testing.T) {
	got := Redact("SOME_CLI_USER_JWT=abcdef1234567890secret")
	if !strings.Contains(got, "SOME_CLI_USER_JWT") {
		t.Errorf("键名不是秘密，不该抹：%s", got)
	}
}

// Scrubber erases exactly by known values, and every occurrence must be erased.
//
// Things like `env | grep JWT` repeat values; erasing only the first occurrence is as good as not
// erasing.
func TestScrubberReplacesEveryOccurrence(t *testing.T) {
	s := NewScrubber(theJWT)
	if !s.Active() {
		t.Fatal("给了长值，抹除器应当生效")
	}
	got := s.Text(theJWT + " middle " + theJWT + " end " + theJWT)
	if strings.Contains(got, theJWT) {
		t.Errorf("应抹掉全部出现：%s", got)
	}
	if n := strings.Count(got, Placeholder); n != 3 {
		t.Errorf("应有 3 个占位符，实际 %d：%s", n, got)
	}
}

// Values that are too short are not used for global replacement: a 3-character "abc" would ruin
// normal output.
func TestScrubberSkipsTooShortValues(t *testing.T) {
	s := NewScrubber("abc", "")
	if s.Active() {
		t.Error("短值不该参与替换，否则会把巧合的同名片段全打成占位符")
	}
	if got := s.Text("abcdef"); got != "abcdef" {
		t.Errorf("不该改动：%s", got)
	}
}

// ⭐ User message bodies **are not logged by default**; only the length and a short hash are.
//
// The body belongs to the user; it would go into the log platform, be kept for a long time and be
// searchable by the whole team, without the user knowing. And the three questions debugging really
// needs (was it received / how long / is it the same one) are all answered by length plus hash.
func TestFingerprintRevealsNoContent(t *testing.T) {
	const msg = "帮我查一下昨天那个订单为什么失败了"
	got := Fingerprint(msg)

	if strings.Contains(got, "订单") || strings.Contains(got, msg) {
		t.Errorf("泄漏了正文：%s", got)
	}
	if !strings.Contains(got, "len=") || !strings.Contains(got, "sha=") {
		t.Errorf("应给出长度与短哈希：%s", got)
	}
	// The fingerprint of the same message is stable ("is it the same one" relies on it).
	if Fingerprint(msg) != got {
		t.Error("指纹应稳定")
	}
	// Different messages have different fingerprints.
	if Fingerprint(msg+"!") == got {
		t.Error("不同消息的指纹应不同")
	}
	if Fingerprint("") != "len=0" {
		t.Error("空串应明确是 len=0")
	}
}

// ⚠️ **Object storage ak/sk must be recognized by the log layer.**
//
// This pins something that "fails with zero errors when missed": the sk of S3-style object storage
// is a 40-character base64 shape (`XAFMl3on...+bK44Jbpl9...`), **neither a three-part JWT nor bare
// hex**, so `Redact` cannot recognize it from the value alone; it can only rely on the **key
// name**.
//
// So those two config fields must have names like accessKey / secretKey. If someone renames them
// to something without key/secret/token (for example `sign`, `credentialsBlob`), redaction
// silently stops working: the value goes into logs as is, while existing tests all stay green (the
// fixtures do not include it).
//
// This is where the rule "newly injected credentials must also be added to the redaction list"
// lands in practice.
func TestObjectStorageCredentialKeysAreRedacted(t *testing.T) {
	// ⚠️ **Build fakes by shape only, never copy real credentials**: 40-character base64 (with +/
	// and non-alphanumerics) and 20 uppercase alphanumerics. Pasting a real ak/sk here as a
	// fixture would write the credential permanently into git history, since test files are
	// checked in, with **no error at all**. That is why a `git diff | grep <credential>`
	// self-check before committing is worth doing.
	const fakeSK = "Zm9vYmFyK3Rlc3RzZWNyZXQvbm90cmVhbEFCQ0RF"
	const fakeAK = "EXAMPLEAKID123456789"

	// 1. Erase by key name (the structured log path, RedactKV).
	for _, key := range []string{
		"storage.secretKey", "secretKey", "S3_SECRET_KEY", "storage.accessKey", "S3_ACCESS_KEY",
	} {
		if got := RedactKV(key, fakeSK); got != Placeholder {
			t.Errorf("键名 %s 应被判成凭据并整值抹掉，得到：%s", key, got)
		}
	}

	// 2. `key=value` forms in free text must also be erased (env echo, whole command lines being
	// logged).
	for _, line := range []string{
		"S3_SECRET_KEY=" + fakeSK,
		"storage.secretKey=" + fakeSK,
		"S3_ACCESS_KEY=" + fakeAK,
	} {
		got := Redact(line)
		if strings.Contains(got, fakeSK) || strings.Contains(got, fakeAK) {
			t.Errorf("凭据原样留在了输出里：%s", got)
		}
		if !strings.Contains(got, Placeholder) {
			t.Errorf("没留占位符：%s", got)
		}
	}

	// 3. **Bare values cannot be caught; this is a known limit**, so config field names must not be
	// changed casually. This is not a defect that "should be fixed", but a deliberate tradeoff of
	// shared/infra/secret (see the section "Redact deliberately does not recognize bare hex" in the
	// package comment): random strings with no recognizable shape can only be erased by key name
	// or by value.
	if got := Redact(fakeSK); got != fakeSK {
		t.Logf("裸 sk 被抹了（形状恰好命中某条规则），当前实现：%s", got)
	}
	// When you really need to erase by value, use Scrubber, which is exact.
	if got := NewScrubber(fakeSK).Text("上传失败 sk=" + fakeSK); strings.Contains(got, fakeSK) {
		t.Errorf("Scrubber 应按确切值抹掉 sk：%s", got)
	}
}
