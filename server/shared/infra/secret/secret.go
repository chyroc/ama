// Package secret is the **one and only** implementation in this repo for handling "things that
// must not be exposed in plaintext".
//
// # Why this package exists
//
// Before it, the same job had two implementations in the repo that evolved separately:
//
//	core/integration.go   maskCredential(s, keepTail)  for display: keep only the tail
//	shared/infra/shelltools/scrub.go  scrubber                for output: erase by known values
//
// Both were correct, but they **did not know about each other**. Now a third user (the log layer)
// is being added, and another copy would make three; then "which one is right" has no answer.
// And once redaction misses something, the result is permanent plaintext (in the database, in
// the model context, in logs), with **zero errors**.
//
// So the two primitives are gathered here, and the two original places now forward to them. When
// users are added, only users are added, not implementations.
//
// # Three primitives, each doing one job (do not mix them up)
//
//	Mask(s, keepTail)  "I want to show a credential to a person"   → keep only the tail, mask short values entirely
//	Scrubber           "I know the exact value of the secret"      → erase them from text one by one
//	Redact(text)       "I do not know whether there is a secret"   → recognize by shape, then erase
//
// The first two are exact; the third is a heuristic **safety net**. The order is also the order of
// trust: if you can use the first two, do not use the third.
//
// # ⚠️ Redact deliberately does not recognize "bare hex"
//
// It is natural to think "a high-entropy hex string is a key, erase it". That cannot be done
// unconditionally: the repo is full of legitimate bare hex: git commit shas, sha256 checksums,
// request logids, UUIDs. Erasing all of them would leave debugging completely blind ("which
// commit" and "which logid" are exactly the two things that matter most when debugging), and that
// loss also gives no error.
//
// So Redact only recognizes things whose **shape cannot be mistaken**: the three parts of a JWT,
// and forms like `key=value` / `Bearer xxx` that state "this is a credential" explicitly. What it
// cannot recognize must be handled at the call site with the first two primitives.
package secret

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

// Placeholder is the trace left behind after erasing.
//
// **It is a human sentence on purpose, not an empty string**: both the model and people debugging
// need to know "something was here and we erased it", otherwise they would think the value never
// existed and then go "fix" something that is not broken.
// No fragment of the original value is kept either (not even its length).
const Placeholder = "[已抹除：凭据不进日志/不出容器]"

// MinLen is the lower bound below which a value is "too short to be worth erasing by value".
//
// Why it is needed: when a token is absurdly short (a test stub, a platform error returning one
// or two characters), using it for global replacement would turn every coincidental matching
// fragment in the output into the placeholder; a 3-character "abc" can ruin normal output.
// Real JWTs are over a thousand bytes, so this threshold does not affect real cases.
const MinLen = 16

// Mask redacts a credential-like value to "ellipsis + a few tail characters", for display only.
//
// ⚠️ **Keep only the tail, never the prefix.**
// The prefix is often exactly the part that should least be exposed: `sa_g_` reveals the account
// type, `app-` reveals the tenant, while a few tail characters are already enough for people to
// tell "is this the one I configured". Showing the prefix is a very common way to write it, but it
// gives away "who this credential belongs to" for free.
//
// When a value is too short to redact, **mask it entirely** instead of returning it as is:
// returning it as is means "short secrets leak directly", and those are exactly the ones that most
// need hiding.
func Mask(s string, keepTail int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if keepTail <= 0 || len(s) <= keepTail {
		// **Fixed width of three asterisks; do not use strings.Repeat("*", len(s)).**
		//
		// Printing one asterisk per character leaks len(s) exactly, which hurts short secrets the
		// most: the length cuts the brute-force space by an order of magnitude. With a fixed width,
		// "how short" is hidden too.
		return "***"
	}
	return "…" + s[len(s)-keepTail:]
}

// Scrubber erases text by a set of known secret values; the zero value is usable (erases
// nothing).
//
// **It does not own the tokens**: use it and throw it away; do not make it a field of a
// long-lived object. That is exactly the shape we want to avoid: "keeping a plaintext copy for as
// long as the container lives".
type Scrubber struct {
	secrets []string
}

// NewScrubber builds a scrubber from a set of secret values; values that are too short are
// skipped automatically (see MinLen).
func NewScrubber(values ...string) Scrubber {
	var secrets []string
	for _, v := range values {
		v = strings.TrimSpace(v)
		if len(v) < MinLen {
			continue
		}
		secrets = append(secrets, v)
	}
	return Scrubber{secrets: secrets}
}

// Active reports whether this scrubber has anything to do.
func (s Scrubber) Active() bool { return len(s.secrets) > 0 }

// Text erases all secret values from a piece of text.
func (s Scrubber) Text(in string) string {
	if !s.Active() || in == "" {
		return in
	}
	out := in
	for _, secret := range s.secrets {
		if secret == "" {
			continue
		}
		out = strings.ReplaceAll(out, secret, Placeholder)
	}
	return out
}

// jwtPattern recognizes the three parts of a JWT: base64url.base64url.base64url.
//
// Why this shape is safe to erase: all three parts are base64url, separated by dots, and the
// first part decodes to a JSON header; normal business text does not look like this. The minimum
// part lengths are deliberately not small, to avoid misjudging plain identifiers like `a.b.c`
// (domain fragments, package paths, version numbers).
var jwtPattern = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}`)

// kvSecretPattern recognizes key=value forms that "state 'this is a credential' explicitly".
//
// It only matches key names containing words like token / secret / password / apikey / jwt /
// credential, and the value runs until whitespace or a quote. That covers the output of `env` and
// the command echo of `set -x`, without touching normal fields like `user=alice`.
//
// ⚠️ **camelCase and snake_case must be listed separately**: `access_key` and `accesskey` are two
// words, and listing the former does not match the latter. Object storage config fields are
// often called `storage.accessKey` (camelCase); this gap was only found when adding it. Before
// that, `accessKey=xxx` **went into logs as is**, with no error at all.
var kvSecretPattern = regexp.MustCompile(
	`(?i)\b([A-Za-z0-9_.\-]*(?:token|secret|password|passwd|apikey|api_key|jwt|credential|access_key|accesskey|secretkey|secret_key|cookie)[A-Za-z0-9_.\-]*)\s*[=:]\s*("[^"]*"|'[^']*'|[^\s,;"']+)`)

// bearerPattern recognizes the Authorization header family.
var bearerPattern = regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+([A-Za-z0-9._~+/=-]{8,})`)

// Redact recognizes credentials in text by shape and erases them, for cases where "we do not know
// whether there is a secret inside".
//
// This is a **safety net**, not the main tool: if you can use Mask / Scrubber, do not rely on it.
// It guards against the real and frequent case of "accidentally dumping a whole environment /
// whole command line / whole error object into a log".
//
// ⚠️ What it cannot do must be stated clearly: encoded values (base64/rev), values split and
// concatenated, and random strings with no special shape are all beyond it. Guarding against those
// assumes "the person writing the log is not trusted", and then the right thing is not to pass
// that value in at all.
func Redact(text string) string {
	if text == "" {
		return text
	}
	// Cheap pre-check: the vast majority of log lines contain no suspicious marker; return
	// directly, avoiding three regexes per line.
	if !mightContainSecret(text) {
		return text
	}
	out := jwtPattern.ReplaceAllString(text, Placeholder)
	out = kvSecretPattern.ReplaceAllString(out, "$1="+Placeholder)
	out = bearerPattern.ReplaceAllString(out, "$1 "+Placeholder)
	return out
}

// RedactKV decides in cases where "key name and value are separate": if the key name says it is
// a credential, the whole value is erased.
//
// # Why this is needed, and Redact alone is not enough
//
// `Redact` only looks at one piece of text, while structured logs keep keys and values apart:
//
//	slog.String("api_key", "AKLTZjM4YmY0N2Y...")
//
// The value has no trace of "this is a credential" (not JWT-shaped, no `key=` prefix), so
// `Redact(value)` cannot recognize it, and the credential goes into the log as is. **But the key
// name already says what it is.**
//
// So the key name is checked first: if it matches a credential word, the whole value is erased,
// without trying to find a shape in the value. If the key name does not match, it falls back to
// `Redact`, letting the value's own shape speak (for example a JWT inside a message).
//
// The key name itself is **always kept**, which is the caller's job: when debugging you need to
// see "this field really had a value".
func RedactKV(key, value string) string {
	if value == "" {
		return value
	}
	if keyNamesACredential(key) {
		// Empty values need no erasing (returned above); any non-empty value becomes the
		// placeholder: **no tail kept**. This differs from Mask's tradeoff: Mask is "a person
		// actively wants to see a credential", while here it is "a log accidentally carried it",
		// and there is no reason to keep any fragment.
		return Placeholder
	}
	return Redact(value)
}

// credentialKeyPattern recognizes the family of "key names that state 'this is a credential'
// explicitly".
//
// Its word list is kept the same as kvSecretPattern on purpose, so the two do not drift into
// "this one matches, that one does not".
// ⚠️ When changing this, also change the one above and suspiciousMarkers below: **change all
// three together**.
var credentialKeyPattern = regexp.MustCompile(
	`(?i)(token|secret|password|passwd|apikey|api_key|jwt|credential|access_key|accesskey|secretkey|secret_key|cookie|authorization)`)

// keyNamesACredential reports whether a field name says "my value is a credential".
func keyNamesACredential(key string) bool {
	if key == "" {
		return false
	}
	return credentialKeyPattern.MatchString(key)
}

// suspiciousMarkers are the keywords that make Redact run its regexes.
//
// A hit means there **may** be a secret (the regexes still need to confirm); no hit means there
// is definitely none of the shapes we recognize.
//
// ⚠️ `eyJ` must be here: **the key name of a bare JWT may be elsewhere**. A slog attribute is
// `slog.String("jwt", <bare token>)`; the value itself is just `eyJ...`, without the word "jwt".
// An early version missed it, so "bare JWTs in attribute values were not erased", and that is
// exactly the most common way to write it. This hole was caught by the tests on the log reporting
// side.
var suspiciousMarkers = []string{
	"token", "secret", "password", "passwd", "apikey", "api_key",
	"jwt", "credential", "access_key", "accesskey", "secretkey", "secret_key",
	"cookie", "bearer", "basic ", "eyj",
}

// mightContainSecret does one cheap lowercase contains check.
func mightContainSecret(text string) bool {
	lower := strings.ToLower(text)
	for _, m := range suspiciousMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// Fingerprint turns a piece of **user data** into "length + short hash", for logging.
//
// # Why user message bodies must not go into logs
//
// That belongs to the user, not to us. It would go into the log platform, be kept for a long time,
// and be searchable by the whole team, without the user knowing. It also adds almost no value for
// debugging: debugging needs "was it received", "how long", "is it the same one", and length plus
// hash answers all three.
//
// The hash keeps only the first 8 hex characters: enough to tell "is it the same message" (which
// is what debugging redelivery, idempotency and deduplication really needs), but not enough to
// recover the original text.
func Fingerprint(s string) string {
	if s == "" {
		return "len=0"
	}
	sum := sha256.Sum256([]byte(s))
	return "len=" + strconv.Itoa(len(s)) + " sha=" + hex.EncodeToString(sum[:4])
}
