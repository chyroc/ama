package cryptobox

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// testKey generates a valid base64 key.
func testKey(t *testing.T) string {
	t.Helper()
	raw := make([]byte, KeySize)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestSealOpenRoundTrip(t *testing.T) {
	box, err := New(testKey(t))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if !box.Enabled() {
		t.Fatal("配了密钥，Enabled 应为 true")
	}

	const plain = "ark-40e1f0c2-1234-5678-9abc-def012345678"
	sealed, err := box.Seal(plain)
	if err != nil {
		t.Fatalf("Seal 失败: %v", err)
	}

	// Ciphertext must carry the version prefix; compatibility with existing data depends on it.
	if !strings.HasPrefix(sealed, Prefix) {
		t.Errorf("密文要以 %q 开头，实际 %q", Prefix, sealed)
	}
	// The plaintext must not appear in the ciphertext, otherwise the "encryption" is fake.
	if strings.Contains(sealed, plain) {
		t.Error("密文里出现了明文")
	}

	got, err := box.Open(sealed)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if got != plain {
		t.Errorf("往返后不一致：想要 %q，得到 %q", plain, got)
	}
}

// Sealing the same plaintext twice must give different ciphertexts (the nonce is random).
//
// A fixed nonce is catastrophic under GCM (reusing a nonce with the same key can recover the
// plaintext), yet it **works perfectly fine functionally**: round trips still pass and tests stay
// green. So it must be pinned separately.
func TestSealUsesFreshNonce(t *testing.T) {
	box, err := New(testKey(t))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	const plain = "ark-same-input-twice"
	a, err := box.Seal(plain)
	if err != nil {
		t.Fatalf("第一次 Seal 失败: %v", err)
	}
	b, err := box.Seal(plain)
	if err != nil {
		t.Fatalf("第二次 Seal 失败: %v", err)
	}
	if a == b {
		t.Error("同一明文两次 Seal 得到相同密文——nonce 被复用了")
	}
}

// Existing plaintext rows (no prefix) must be read back as is.
//
// This is the whole basis for "no migration needed on release". If it goes red, every configured
// Ark credential fails at once on upgrade, and the error says "API key invalid".
func TestOpenPassesThroughLegacyPlaintext(t *testing.T) {
	const legacy = "ark-legacy-plaintext-in-db"

	// Both modes must read existing plaintext: instances with a key and instances without.
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"配了密钥", testKey(t)},
		{"降级模式", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box, err := New(tc.key)
			if err != nil {
				t.Fatalf("New 失败: %v", err)
			}
			got, err := box.Open(legacy)
			if err != nil {
				t.Fatalf("读存量明文失败: %v", err)
			}
			if got != legacy {
				t.Errorf("存量明文要原样返回：想要 %q，得到 %q", legacy, got)
			}
		})
	}
}

// In degrade mode (no key), Seal returns its input as is, and Enabled is false.
//
// Enabled is the only way ops can tell "is encryption actually on": the two modes work exactly the
// same.
func TestDisabledBoxPassesThrough(t *testing.T) {
	box, err := New("")
	if err != nil {
		t.Fatalf("空密钥应当降级而不是报错，得到: %v", err)
	}
	if box.Enabled() {
		t.Error("没配密钥，Enabled 应为 false")
	}
	const plain = "ark-no-key-configured"
	sealed, err := box.Seal(plain)
	if err != nil {
		t.Fatalf("降级模式 Seal 失败: %v", err)
	}
	if sealed != plain {
		t.Errorf("降级模式要原样返回：想要 %q，得到 %q", plain, sealed)
	}
}

// When the database has ciphertext but this process has no key, it must return an error instead
// of returning the ciphertext.
//
// Returned ciphertext would be sent to Ark as a key, the 401 says "invalid credential", while the
// real cause is that this instance is missing the key. Especially hidden with multiple instances:
// fully configured instances work fine, and only some requests fail.
func TestDisabledBoxRefusesCiphertext(t *testing.T) {
	enc, err := New(testKey(t))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	sealed, err := enc.Seal("ark-secret")
	if err != nil {
		t.Fatalf("Seal 失败: %v", err)
	}

	plainBox, err := New("")
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	got, err := plainBox.Open(sealed)
	if !errors.Is(err, ErrCorrupted) {
		t.Errorf("没有密钥读密文要回 ErrCorrupted，得到 err=%v", err)
	}
	if got != "" {
		t.Errorf("失败时不能返回任何内容，得到 %q", got)
	}
}

// With a different key it cannot be decrypted, and the error is distinguishable (it does not
// silently return garbage).
func TestOpenWithWrongKeyFails(t *testing.T) {
	a, err := New(testKey(t))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	b, err := New(testKey(t))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	sealed, err := a.Seal("ark-written-with-key-a")
	if err != nil {
		t.Fatalf("Seal 失败: %v", err)
	}
	if _, err := b.Open(sealed); !errors.Is(err, ErrCorrupted) {
		t.Errorf("换密钥解密要回 ErrCorrupted，得到 %v", err)
	}
}

// A misconfigured key must report ErrBadKey at construction time; it must not be treated as "not
// configured" and silently degrade.
//
// The consequence of a silent degrade is ops thinking encryption is on while the database is all
// plaintext.
func TestBadKeyIsNotSilentlyDowngraded(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"不是 base64", "这不是 base64!!!"},
		{"长度不对（16 字节）", base64.StdEncoding.EncodeToString(make([]byte, 16))},
		{"长度不对（33 字节）", base64.StdEncoding.EncodeToString(make([]byte, 33))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box, err := New(tc.key)
			if !errors.Is(err, ErrBadKey) {
				t.Errorf("配错密钥要回 ErrBadKey，得到 err=%v", err)
			}
			if box != nil {
				t.Error("出错时不该返回可用的 Box——那会被当成降级模式继续跑")
			}
		})
	}
}

// All four base64 variants must be accepted: keys are copied by people from a console, and the
// error for pasting the wrong variant (illegal base64 data) does not point at the real cause at
// all.
func TestAcceptsBase64Variants(t *testing.T) {
	raw := make([]byte, KeySize)
	for i := range raw {
		// Bytes chosen on purpose to trigger the +/ vs -_ difference.
		raw[i] = byte(i*7 + 251)
	}
	for _, enc := range []struct {
		name string
		enc  *base64.Encoding
	}{
		{"StdEncoding", base64.StdEncoding},
		{"URLEncoding", base64.URLEncoding},
		{"RawStdEncoding", base64.RawStdEncoding},
		{"RawURLEncoding", base64.RawURLEncoding},
	} {
		t.Run(enc.name, func(t *testing.T) {
			box, err := New(enc.enc.EncodeToString(raw))
			if err != nil {
				t.Fatalf("%s 形式的密钥应当被接受，得到: %v", enc.name, err)
			}
			if !box.Enabled() {
				t.Error("Enabled 应为 true")
			}
		})
	}
}

// An empty string is returned as is: it means "no key at this level" (inherited workspace rows
// are empty strings). Encrypting an empty string would make "not configured" and "configured as
// empty" look different in the database.
func TestSealEmptyStaysEmpty(t *testing.T) {
	box, err := New(testKey(t))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	sealed, err := box.Seal("")
	if err != nil {
		t.Fatalf("Seal 空串失败: %v", err)
	}
	if sealed != "" {
		t.Errorf("空串要原样返回，得到 %q", sealed)
	}
}

// Truncated / tampered ciphertext must be an error; it must not return partial plaintext.
func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	box, err := New(testKey(t))
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	sealed, err := box.Seal("ark-tamper-me")
	if err != nil {
		t.Fatalf("Seal 失败: %v", err)
	}

	for _, tc := range []struct {
		name  string
		input string
	}{
		{"整段截断", sealed[:len(sealed)/2]},
		{"只有前缀", Prefix},
		{"前缀 + 非 base64", Prefix + "!!!not-base64!!!"},
		{"前缀 + 太短的 blob", Prefix + base64.StdEncoding.EncodeToString([]byte{1, 2, 3})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := box.Open(tc.input)
			if !errors.Is(err, ErrCorrupted) {
				t.Errorf("坏密文要回 ErrCorrupted，得到 err=%v", err)
			}
			if got != "" {
				t.Errorf("失败时不能返回内容，得到 %q", got)
			}
		})
	}
}

// A nil receiver must not panic: callers may use it before it is initialized (for example on the
// path where config loading failed).
func TestNilBoxIsSafe(t *testing.T) {
	var box *Box
	if box.Enabled() {
		t.Error("nil Box 的 Enabled 应为 false")
	}
}
