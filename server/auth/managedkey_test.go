// Tests for platform-managed API Keys; see managedkey.go.
//
// The core of this group is **determinism**: a random implementation passes assertions like "we
// get a key", but instances step on each other. So every assertion below looks at "do two calls /
// two instances compute the same string".
package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

const (
	testManagedSecret = "test-managed-secret-do-not-use-in-prod"
	testManagedEmail  = "alice@example.com"
)

// managedTestService builds a Service with the derivation secret set.
func managedTestService(t *testing.T) *Service {
	t.Helper()
	svc, err := New(Config{
		Store:            NewMemStore(),
		OIDC:             &fakeOIDC{},
		ManagedKeySecret: testManagedSecret,
	})
	if err != nil {
		t.Fatalf("建 Service 失败: %v", err)
	}
	return svc
}

func testSubject() ManagedKeySubject {
	return ManagedKeySubject{Email: testManagedEmail}
}

// TestEnsureManagedAPIKeyIsDeterministic pins "repeated calls for the same (workspace, person) give
// byte-identical plain text, and the database has only one row".
//
// # What this test guards against
//
// **It is the only way to tell the "derived" and "random" implementations apart.** Replace
// deriveManagedKey with crypto/rand and the first assertion below turns red at once. If it only
// asserted "we get a non-empty key", a random implementation would still pass everything, then
// blow up in production as "the token in the container changes on every exec, and every call gets
// 401".
func TestEnsureManagedAPIKeyIsDeterministic(t *testing.T) {
	svc := managedTestService(t)
	ctx := context.Background()

	first, err := svc.EnsureManagedAPIKey(ctx, testSubject())
	if err != nil {
		t.Fatalf("第一次注入失败: %v", err)
	}
	if !strings.HasPrefix(first, KeyPrefix) {
		t.Fatalf("推导出来的 key 没有 %q 前缀，looksLikeAPIKey 会认不出它: %q", KeyPrefix, first)
	}

	// Ask 4 more times in a row; every time it must be the same string.
	for i := 0; i < 4; i++ {
		again, err := svc.EnsureManagedAPIKey(ctx, testSubject())
		if err != nil {
			t.Fatalf("第 %d 次注入失败: %v", i+2, err)
		}
		if again != first {
			t.Fatalf("第 %d 次推导出了不同的 key，多实例下容器里的 token 会反复失效\n"+
				"first=%q again=%q", i+2, first, again)
		}
	}

	keys, err := svc.ListAPIKeys(ctx, testManagedEmail)
	if err != nil {
		t.Fatalf("列 key 失败: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("反复注入之后库里应当只有 1 行，实际 %d 行", len(keys))
	}
}

// TestManagedKeyConcurrentInjectionAcrossInstances simulates **two pods injecting into the same
// workspace at the same time**: both calls must get byte-identical tokens, and the database has
// only one row.
//
// # Why this needs its own test instead of relying on the one above
//
// The one above is sequential, so a "query, then create" implementation passes it too (the second
// call finds the row and returns it). Only under concurrency does the real race show: two
// goroutines query at the same time, both find nothing, both create.
//
// Then **the derived approach passes and the random one must fail**:
//
//   - Derived: both sides compute the same string → same key_hash → same id → the later
//     `on conflict (id) do update` is a harmless rewrite → 1 row in the database, both sides hold
//     the same token.
//   - Random: each side generates its own string → two different hashes → **2 rows in the
//     database**, and the two pods hold different tokens. After that any "clear the old platform
//     key" logic makes them step on each other.
//
// A shared Store instead of one Store per Service is exactly what makes this race really happen
// (two processes share the database, not memory). Two Service instances prove that **derivation
// does not depend on any in-process state**: they share no memory at all.
func TestManagedKeyConcurrentInjectionAcrossInstances(t *testing.T) {
	shared := NewMemStore()
	newInstance := func() *Service {
		svc, err := New(Config{
			Store:            shared,
			OIDC:             &fakeOIDC{},
			ManagedKeySecret: testManagedSecret,
		})
		if err != nil {
			t.Fatalf("建 Service 失败: %v", err)
		}
		return svc
	}
	// Two "instances", each its own Service (no shared memory), sharing one Store (the shared database).
	instances := []*Service{newInstance(), newInstance()}

	ctx := context.Background()
	const perInstance = 8
	tokens := make([]string, len(instances)*perInstance)
	errs := make([]error, len(instances)*perInstance)

	// Use a Barrier so all goroutines enter EnsureManagedAPIKey as close to the same time as
	// possible; otherwise the first one to finish creates the row, all the rest take the fast path,
	// and the race never happens.
	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for i, svc := range instances {
		for j := 0; j < perInstance; j++ {
			idx := i*perInstance + j
			done.Add(1)
			go func(idx int, svc *Service) {
				defer done.Done()
				start.Wait()
				tokens[idx], errs[idx] = svc.EnsureManagedAPIKey(ctx, testSubject())
			}(idx, svc)
		}
	}
	start.Done()
	done.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个并发注入失败: %v", i, err)
		}
	}
	// All tokens must be byte-identical.
	for i, tok := range tokens {
		if tok != tokens[0] {
			t.Fatalf("并发注入拿到了不同的 token（第 0 个与第 %d 个不同），"+
				"说明实现不是确定性推导——多实例下容器里的凭据会互相失效\n"+
				"[0]=%q [%d]=%q", i, tokens[0], i, tok)
		}
	}

	// The database may only have one row.
	keys, err := instances[0].ListAPIKeys(ctx, testManagedEmail)
	if err != nil {
		t.Fatalf("列 key 失败: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("%d 个并发注入之后库里应当只有 1 行，实际 %d 行——"+
			"随机生成的实现正是在这里建出多把 key", len(tokens), len(keys))
	}
}

// TestManagedKeyIsolatedByPerson pins "a different person is a different key".
//
// This is the only isolation dimension this key should have: it proves "who I am", so two people
// must get two keys. **There is deliberately no "different workspace" case**: the id has no
// workspace at all; see ManagedKeySubject for why: permissions are decided on the spot by authz
// from roles, and splitting by workspace gives no isolation.
func TestManagedKeyIsolatedByPerson(t *testing.T) {
	svc := managedTestService(t)
	ctx := context.Background()

	base, err := svc.EnsureManagedAPIKey(ctx, testSubject())
	if err != nil {
		t.Fatalf("注入失败: %v", err)
	}
	other, err := svc.EnsureManagedAPIKey(ctx, ManagedKeySubject{Email: "bob@example.com"})
	if err != nil {
		t.Fatalf("另一个人注入失败: %v", err)
	}
	if base == other {
		t.Fatal("两个人推导出了同一把 key —— 那把 key 就是身份，必须逐人不同")
	}
	// And each resolves back to its own identity; they must not get mixed up.
	for email, tok := range map[string]string{testManagedEmail: base, "bob@example.com": other} {
		ident, err := svc.Resolve(ctx, tok)
		if err != nil {
			t.Fatalf("%s 的 key 校验失败: %v", email, err)
		}
		if ident.Email != email {
			t.Fatalf("身份串了：%s 的 key 解析成 %s", email, ident.Email)
		}
	}
}

// TestManagedKeyValidatesViaExistingPath pins "the derived key goes through the existing check path".
//
// This proves it is **a real API Key**, not a separate kind of credential: Resolve accepts it, and
// the resolved identity is the person it belongs to. If the prefix or encoding is ever changed,
// looksLikeAPIKey rules it out first, and every call in the container gets 401.
func TestManagedKeyValidatesViaExistingPath(t *testing.T) {
	svc := managedTestService(t)
	ctx := context.Background()

	plaintext, err := svc.EnsureManagedAPIKey(ctx, testSubject())
	if err != nil {
		t.Fatalf("注入失败: %v", err)
	}

	ident, err := svc.Resolve(ctx, plaintext)
	if err != nil {
		t.Fatalf("推导出来的 key 走 Resolve 失败了，容器里的 ama 命令会全部 401: %v", err)
	}
	if ident.Email != testManagedEmail {
		t.Fatalf("身份不对：想要 %q，得到 %q", testManagedEmail, ident.Email)
	}
}

// TestManagedKeyRevokeThenReinjectRevives pins the behavior the requirement asks for: after a
// revoke, the next injection creates **the same** key again.
//
// ⚠️ This is also the runnable version of the warning: **revoking cannot invalidate a leaked
// token**. Asserting after == before is deliberate: it pins this limit in a test, so nobody thinks
// revoke equals rotate.
func TestManagedKeyRevokeThenReinjectRevives(t *testing.T) {
	svc := managedTestService(t)
	ctx := context.Background()

	before, err := svc.EnsureManagedAPIKey(ctx, testSubject())
	if err != nil {
		t.Fatalf("首次注入失败: %v", err)
	}
	if err := svc.RevokeManagedAPIKey(ctx, testSubject()); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}

	// After a revoke the card must show "revoked", so looking up the record must return ErrNotFound.
	if _, err := svc.ManagedAPIKeyRecord(ctx, testSubject()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("撤销之后查记录该回 ErrNotFound，得到 %v", err)
	}
	// And that token stops working at once.
	if _, err := svc.Resolve(ctx, before); err == nil {
		t.Fatal("撤销之后那把 token 仍然能通过校验，撤销没有生效")
	}

	after, err := svc.EnsureManagedAPIKey(ctx, testSubject())
	if err != nil {
		t.Fatalf("撤销后重新注入失败: %v", err)
	}
	if after != before {
		t.Fatalf("撤销后重新注入应当推导出同一把 key（需求：撤销后下次自动创建）\n"+
			"before=%q after=%q", before, after)
	}
}

// TestManagedKeyRecordCarriesNoPlaintext pins "the record shown to the card has no plain text".
//
// Being able to compute the plain text ≠ handing it out: the card is an HTTP response, and anything
// in it ends up in the browser, logs and screenshots.
func TestManagedKeyRecordCarriesNoPlaintext(t *testing.T) {
	svc := managedTestService(t)
	ctx := context.Background()

	plaintext, err := svc.EnsureManagedAPIKey(ctx, testSubject())
	if err != nil {
		t.Fatalf("注入失败: %v", err)
	}
	rec, err := svc.ManagedAPIKeyRecord(ctx, testSubject())
	if err != nil {
		t.Fatalf("查记录失败: %v", err)
	}
	// UserAPIKey has no plain-text field, so here we can only check that prefix does not give too
	// much: prefix must be a real prefix of the plain text, and **short enough that the rest cannot
	// be recovered**.
	if rec.Prefix != plaintext[:prefixDisplayLen] {
		t.Fatalf("prefix 与明文前缀不一致：%q vs %q", rec.Prefix, plaintext[:prefixDisplayLen])
	}
	if len(rec.Prefix) >= len(plaintext) {
		t.Fatalf("prefix 长度 %d 与明文 %d 相当，等于把整把 key 显示出来了",
			len(rec.Prefix), len(plaintext))
	}
	if rec.Name != ManagedKeyName {
		t.Fatalf("名字该是固定的 %q（让用户认出这是平台建的），得到 %q", ManagedKeyName, rec.Name)
	}
}

// TestManagedKeyUnavailableWhenUnconfigured pins "with no secret configured, it is a clear sentinel
// error", not an empty key or a panic.
//
// The wiring code uses ManagedKeyEnabled to decide whether to wire this feature, and at run time
// this error tells "deployment is missing config" apart from "the user revoked it"; the two are
// handled completely differently.
func TestManagedKeyUnavailableWhenUnconfigured(t *testing.T) {
	svc, err := New(Config{Store: NewMemStore(), OIDC: &fakeOIDC{}})
	if err != nil {
		t.Fatalf("建 Service 失败: %v", err)
	}
	if svc.ManagedKeyEnabled() {
		t.Fatal("没配密钥时 ManagedKeyEnabled 该是 false")
	}
	ctx := context.Background()
	if _, err := svc.EnsureManagedAPIKey(ctx, testSubject()); !errors.Is(err, ErrManagedKeyUnavailable) {
		t.Fatalf("该回 ErrManagedKeyUnavailable，得到 %v", err)
	}
	if _, err := svc.ManagedAPIKeyRecord(ctx, testSubject()); !errors.Is(err, ErrManagedKeyUnavailable) {
		t.Fatalf("查记录该回 ErrManagedKeyUnavailable，得到 %v", err)
	}
	if err := svc.RevokeManagedAPIKey(ctx, testSubject()); !errors.Is(err, ErrManagedKeyUnavailable) {
		t.Fatalf("撤销该回 ErrManagedKeyUnavailable，得到 %v", err)
	}
}

// TestManagedKeyDifferentSecretsDifferentKeys pins "a different secret means all keys change".
//
// This is the runnable version of the rotation comment: after replacing the secret all old tokens
// become invalid, and the next injection derives a new string. It is written as a test so that
// "the cost of rotation" is backed by evidence, not by trusting a comment.
func TestManagedKeyDifferentSecretsDifferentKeys(t *testing.T) {
	ctx := context.Background()
	mk := func(secret string) string {
		svc, err := New(Config{
			Store: NewMemStore(), OIDC: &fakeOIDC{},
			ManagedKeySecret: secret,
		})
		if err != nil {
			t.Fatalf("建 Service 失败: %v", err)
		}
		tok, err := svc.EnsureManagedAPIKey(ctx, testSubject())
		if err != nil {
			t.Fatalf("注入失败: %v", err)
		}
		return tok
	}
	if a, b := mk(testManagedSecret), mk(testManagedSecret+"-rotated"); a == b {
		t.Fatal("换了推导密钥却算出同一把 key，说明密钥没参与推导")
	}
}

// TestManagedKeyRejectsEmptySubject pins that Email is required.
//
// An empty email would derive a key for "a person who does not exist", and the key is the
// identity: when it calls an endpoint, authz has no way to decide permissions. It must be stopped
// at the entry point; do not HMAC an empty string.
func TestManagedKeyRejectsEmptySubject(t *testing.T) {
	svc := managedTestService(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		subj ManagedKeySubject
	}{
		{"缺人", ManagedKeySubject{}},
		{"只有空白", ManagedKeySubject{Email: "   "}},
	} {
		if _, err := svc.EnsureManagedAPIKey(ctx, tc.subj); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%s: 该回 ErrInvalidArgument，得到 %v", tc.name, err)
		}
	}
}

// TestManagedKeyEmailCaseInsensitive pins that the case of the email does not affect the derived
// result.
//
// The Actor is lower-cased elsewhere, but the email on the injection path comes from the workspace
// members table. Without normalizing the case, the same person would get two keys on two paths, and
// two rows in the database.
func TestManagedKeyEmailCaseInsensitive(t *testing.T) {
	svc := managedTestService(t)
	ctx := context.Background()

	lower, err := svc.EnsureManagedAPIKey(ctx, testSubject())
	if err != nil {
		t.Fatalf("注入失败: %v", err)
	}
	upper, err := svc.EnsureManagedAPIKey(ctx, ManagedKeySubject{Email: "Alice@Example.com"})
	if err != nil {
		t.Fatalf("大写邮箱注入失败: %v", err)
	}
	if lower != upper {
		t.Fatalf("同一个人大小写不同却算出两把 key\nlower=%q upper=%q", lower, upper)
	}
	keys, err := svc.ListAPIKeys(ctx, testManagedEmail)
	if err != nil {
		t.Fatalf("列 key 失败: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("大小写归一化之后库里应当只有 1 行，实际 %d 行", len(keys))
	}
}
