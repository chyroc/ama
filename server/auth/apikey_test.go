// Unit tests for API Keys.
//
// They pin three things, all of the kind "when wrong there is no error, it is just insecure":
//
//	plain text not stored       the database only has the sha256; no field anywhere holds the plain text
//	revoke really works         right after a revoke the key no longer yields an identity
//	only your own keys          others' keys can be neither listed nor revoked, and 404 is returned, not 403
//
// The contract tests of the Store layer are in store_test.go (run against both the memory and PG
// implementations).

package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// newAPIKeyService builds a usable Service (memory Store).
func newAPIKeyService(t *testing.T) (*Service, *MemStore) {
	t.Helper()
	store := NewMemStore()
	svc, err := New(Config{Store: store, OIDC: &fakeOIDC{}})
	if err != nil {
		t.Fatalf("装配 Service 失败: %v", err)
	}
	return svc, store
}

const apiKeyOwner = "zhangsan@example.com"

// TestCreateAPIKeyReturnsPlaintextOnce checks that creation returns the plain text and that it has
// the right prefix.
func TestCreateAPIKeyReturnsPlaintextOnce(t *testing.T) {
	svc, _ := newAPIKeyService(t)
	ctx := context.Background()

	created, err := svc.CreateAPIKey(ctx, apiKeyOwner, "my-agent")
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	if !strings.HasPrefix(created.Key, KeyPrefix) {
		t.Errorf("明文 key 是 %q，应该以 %q 开头（密钥扫描器靠这个前缀识别泄露）",
			created.Key, KeyPrefix)
	}
	// 32 random bytes in base64 give 43 characters, plus the prefix. Shorter means too little entropy.
	if len(created.Key) < len(KeyPrefix)+40 {
		t.Errorf("明文 key 只有 %d 个字符，太短了（应该是 32 字节随机数）", len(created.Key))
	}
	if created.Name != "my-agent" {
		t.Errorf("name 是 %q，期望 my-agent", created.Name)
	}
	if !strings.HasPrefix(created.Key, created.Prefix) {
		t.Errorf("prefix %q 不是明文 %q 的前缀", created.Prefix, created.Key)
	}
	// prefix should only be a short piece and must not equal the plain text; that would mean the key
	// was stored in the display field.
	if created.Prefix == created.Key {
		t.Error("prefix 等于整个明文 key——那样列表接口就把凭据吐出去了")
	}
	if created.ID == "" || created.CreatedAt == "" {
		t.Errorf("id 或 createdAt 为空: %+v", created.UserAPIKey)
	}
	// A freshly created key has never been used; this field must be **missing**, not an empty string
	// or a fake time.
	if created.LastUsedAt != "" {
		t.Errorf("刚创建的 key 的 lastUsedAt 是 %q，应该为空（表示从没用过）", created.LastUsedAt)
	}
}

// TestAPIKeyPlaintextNeverStored is a **security assertion**: the plain text does not appear in any
// field in the database.
//
// It guards against "also storing the plain text on the side": such a change turns no test red
// (everything works), but it breaks the guarantee "a database leak does not yield keys".
//
// The check **goes through every string field of the record**, instead of looking at one
// particular field: the latter only protects that known field and misses new ones.
func TestAPIKeyPlaintextNeverStored(t *testing.T) {
	svc, store := newAPIKeyService(t)
	ctx := context.Background()

	created, err := svc.CreateAPIKey(ctx, apiKeyOwner, "ci-deploy")
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	plaintext := created.Key

	// Go through every row and every string field of the memory Store directly.
	store.mu.RLock()
	defer store.mu.RUnlock()
	if len(store.apiKeys) != 1 {
		t.Fatalf("库里有 %d 条记录，期望 1 条", len(store.apiKeys))
	}

	for hash, rec := range store.apiKeys {
		// The hash as a map key must be checked too, in case someone used the plain text as the key.
		fields := map[string]string{
			"map 的键":  hash,
			"KeyHash": rec.KeyHash,
			"ID":      rec.ID,
			"Email":   rec.Email,
			"Name":    rec.Name,
			"Prefix":  rec.Prefix,
		}
		for name, value := range fields {
			if value == plaintext {
				t.Errorf("❌ %s 字段等于明文 key——明文绝不能落库", name)
			}
			if strings.Contains(value, plaintext) {
				t.Errorf("❌ %s 字段包含完整明文 key", name)
			}
		}

		// Conversely, confirm that what is stored **really is** the hash: it must not pass falsely
		// because "nothing was stored".
		if rec.KeyHash != hashAPIKey(plaintext) {
			t.Errorf("KeyHash 不是明文的 sha256，校验路径会认不出这把 key")
		}
		if len(rec.KeyHash) != 64 {
			t.Errorf("KeyHash 是 %d 个字符，sha256 的 hex 应该是 64 个", len(rec.KeyHash))
		}
		// prefix should only be a short piece.
		if len(rec.Prefix) >= len(plaintext) {
			t.Errorf("Prefix 有 %d 个字符（明文 %d 个），太长了", len(rec.Prefix), len(plaintext))
		}
	}
}

// TestAPIKeyResolvesToCreatorIdentity checks that calling an endpoint with a key is the creator.
//
// **This is where the "key inherits the creator's permissions" design lands.** With the wrong
// identity, those 56 route declarations apply to the wrong person, and the symptom is "some
// endpoints return 403 for no reason", which does not point to the real cause.
func TestAPIKeyResolvesToCreatorIdentity(t *testing.T) {
	svc, _ := newAPIKeyService(t)
	ctx := context.Background()

	created, err := svc.CreateAPIKey(ctx, apiKeyOwner, "agent")
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	identity, err := svc.Resolve(ctx, created.Key)
	if err != nil {
		t.Fatalf("用刚创建的 key 换身份失败: %v", err)
	}
	if identity.Email != apiKeyOwner {
		t.Errorf("身份是 %q，期望创建者 %q", identity.Email, apiKeyOwner)
	}
	if !identity.ViaAPIKey {
		t.Error("ViaAPIKey 应该为真（让日志与前端能区分机器身份）")
	}
}

// TestAPIKeyEmailIsNormalized checks that emails differing in case map to the same person.
//
// Without normalizing, a key created by "Zhang.San@" would be bound to an Actor different from
// "zhang.san@", so it would not be a member of any workspace; the symptom is "the key was clearly
// created but it can access nothing".
func TestAPIKeyEmailIsNormalized(t *testing.T) {
	svc, _ := newAPIKeyService(t)
	ctx := context.Background()

	created, err := svc.CreateAPIKey(ctx, "ZhangSan@Example.com", "k")
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	identity, err := svc.Resolve(ctx, created.Key)
	if err != nil {
		t.Fatalf("换身份失败: %v", err)
	}
	if identity.Email != "zhangsan@example.com" {
		t.Errorf("身份是 %q，期望小写化后的 zhangsan@example.com", identity.Email)
	}
	// It must also be listed with the lower-cased email.
	keys, err := svc.ListAPIKeys(ctx, "zhangsan@example.com")
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(keys) != 1 {
		t.Errorf("用小写邮箱列出 %d 把 key，期望 1 把（大小写没归一）", len(keys))
	}
}

// TestRevokeAPIKeyTakesEffectImmediately checks that a key cannot be used right after it is revoked.
//
// **This is the whole reason for choosing "store a hash and look it up" over JWT** (see the auth.go
// package comment): a JWT cannot be taken back once signed, while here deleting the row is a real
// revoke.
func TestRevokeAPIKeyTakesEffectImmediately(t *testing.T) {
	svc, _ := newAPIKeyService(t)
	ctx := context.Background()

	created, err := svc.CreateAPIKey(ctx, apiKeyOwner, "temp")
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	// Usable before the revoke.
	if _, err := svc.Resolve(ctx, created.Key); err != nil {
		t.Fatalf("吊销前应该能用: %v", err)
	}

	if err := svc.RevokeAPIKey(ctx, apiKeyOwner, created.ID); err != nil {
		t.Fatalf("吊销失败: %v", err)
	}

	// Unusable right after the revoke.
	if _, err := svc.Resolve(ctx, created.Key); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("吊销后换身份得到 %v，期望 ErrUnauthenticated", err)
	}
	// It should not appear in the list any more either.
	keys, err := svc.ListAPIKeys(ctx, apiKeyOwner)
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("吊销后列表里还有 %d 把 key", len(keys))
	}
}

// TestRevokeOthersAPIKeyReturnsNotFound checks that you cannot revoke someone else's key, and that
// the result means 404.
//
// **Returning ErrNotFound instead of a permission error is deliberate**: answering "access denied"
// would confirm that this id exists.
func TestRevokeOthersAPIKeyReturnsNotFound(t *testing.T) {
	svc, store := newAPIKeyService(t)
	ctx := context.Background()

	victim, err := svc.CreateAPIKey(ctx, "victim@example.com", "victim-key")
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	err = svc.RevokeAPIKey(ctx, "attacker@example.com", victim.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("吊销别人的 key 得到 %v，期望 ErrNotFound（回 403 会确认这个 id 存在）", err)
	}

	// The key point: that key is still there, and still works.
	store.mu.RLock()
	remaining := len(store.apiKeys)
	store.mu.RUnlock()
	if remaining != 1 {
		t.Errorf("被攻击者的 key 被删掉了，库里剩 %d 条", remaining)
	}
	if _, err := svc.Resolve(ctx, victim.Key); err != nil {
		t.Errorf("受害者的 key 不能用了: %v", err)
	}
}

// TestListAPIKeysOnlyReturnsOwn checks that the list only returns your own keys.
func TestListAPIKeysOnlyReturnsOwn(t *testing.T) {
	svc, _ := newAPIKeyService(t)
	ctx := context.Background()

	if _, err := svc.CreateAPIKey(ctx, "a@example.com", "a-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateAPIKey(ctx, "a@example.com", "a-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateAPIKey(ctx, "b@example.com", "b-1"); err != nil {
		t.Fatal(err)
	}

	keys, err := svc.ListAPIKeys(ctx, "a@example.com")
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("a 有 %d 把 key，期望 2 把", len(keys))
	}
	for _, k := range keys {
		if k.Name == "b-1" {
			t.Error("列表里出现了别人的 key")
		}
	}
}

// TestListAPIKeysNeverReturnsPlaintext checks that what is in the list is not enough to recover a key.
//
// The UserAPIKey struct has no plain-text field at all, so what this test really guards against is
// "someone changes prefix to the whole plain text", which would not cause a compile error.
func TestListAPIKeysNeverReturnsPlaintext(t *testing.T) {
	svc, _ := newAPIKeyService(t)
	ctx := context.Background()

	created, err := svc.CreateAPIKey(ctx, apiKeyOwner, "k")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := svc.ListAPIKeys(ctx, apiKeyOwner)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("期望 1 把 key，得到 %d", len(keys))
	}

	for _, field := range []string{keys[0].ID, keys[0].Name, keys[0].Prefix} {
		if field == created.Key || strings.Contains(field, created.Key) {
			t.Errorf("❌ 列表返回的字段 %q 含有明文 key", field)
		}
	}
	// Using the prefix from the list as a credential must not yield an identity.
	if _, err := svc.Resolve(ctx, keys[0].Prefix); !errors.Is(err, ErrUnauthenticated) {
		t.Error("❌ 用列表里的 prefix 就能换到身份——那说明 prefix 泄露了太多")
	}
}

// TestListAPIKeysReturnsEmptySliceNotNil checks that with no keys the result is an empty slice, not nil.
//
// nil encodes as null, but the contract requires []: the frontend calls .map() directly (see rule
// one of domain).
func TestListAPIKeysReturnsEmptySliceNotNil(t *testing.T) {
	svc, _ := newAPIKeyService(t)
	keys, err := svc.ListAPIKeys(context.Background(), "nobody@example.com")
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if keys == nil {
		t.Error("空列表返回了 nil，会编码成 null；契约要求 []")
	}
	if len(keys) != 0 {
		t.Errorf("期望 0 把 key，得到 %d", len(keys))
	}
}

// TestAPIKeyLastUsedIsRecorded checks that lastUsedAt has a value after use.
//
// This field is the only basis for users to decide "which key can be revoked"; without it, facing a
// list of keys, they would not dare to delete any.
func TestAPIKeyLastUsedIsRecorded(t *testing.T) {
	svc, _ := newAPIKeyService(t)
	ctx := context.Background()

	// A fixed clock, so the assertion is about "was it recorded", not "is the time accurate".
	at := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return at }

	created, err := svc.CreateAPIKey(ctx, apiKeyOwner, "k")
	if err != nil {
		t.Fatal(err)
	}
	// Not there right after creation.
	before, _ := svc.ListAPIKeys(ctx, apiKeyOwner)
	if before[0].LastUsedAt != "" {
		t.Errorf("还没用过就有 lastUsedAt: %q", before[0].LastUsedAt)
	}

	if _, err := svc.Resolve(ctx, created.Key); err != nil {
		t.Fatalf("换身份失败: %v", err)
	}

	after, _ := svc.ListAPIKeys(ctx, apiKeyOwner)
	if after[0].LastUsedAt == "" {
		t.Fatal("用过一次之后 lastUsedAt 仍然为空")
	}
	// The shape must match apimodel.NowISO (exactly three decimals + Z), otherwise frontend sorting breaks.
	if want := "2026-08-28T10:00:00.000Z"; after[0].LastUsedAt != want {
		t.Errorf("lastUsedAt 是 %q，期望 %q（固定三位小数的 ISO 串）", after[0].LastUsedAt, want)
	}
}

// TestCreateAPIKeyRejectsEmptyName checks that the name is required.
//
// Allowing empty names would put several "nameless" keys in the list; users could not tell which
// is which, so nobody would dare to revoke any.
func TestCreateAPIKeyRejectsEmptyName(t *testing.T) {
	svc, _ := newAPIKeyService(t)
	ctx := context.Background()

	for _, name := range []string{"", "   ", "\t"} {
		if _, err := svc.CreateAPIKey(ctx, apiKeyOwner, name); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("名字为 %q 时得到 %v，期望 ErrInvalidArgument", name, err)
		}
	}
	// The email is required too.
	if _, err := svc.CreateAPIKey(ctx, "", "k"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("邮箱为空时得到 %v，期望 ErrInvalidArgument", err)
	}
}

// TestEveryAPIKeyIsUnique checks that keys created back to back do not collide.
//
// On a collision the unique constraint would make the second creation fail, and in the UI that
// error is "creation failed, please retry", which does not point to "the random numbers are not
// random enough" at all.
func TestEveryAPIKeyIsUnique(t *testing.T) {
	svc, _ := newAPIKeyService(t)
	ctx := context.Background()

	seen := map[string]bool{}
	ids := map[string]bool{}
	const n = 50
	for i := 0; i < n; i++ {
		created, err := svc.CreateAPIKey(ctx, apiKeyOwner, "k")
		if err != nil {
			t.Fatalf("第 %d 次创建失败: %v", i, err)
		}
		if seen[created.Key] {
			t.Fatalf("第 %d 次创建撞了同一把 key", i)
		}
		if ids[created.ID] {
			t.Fatalf("第 %d 次创建撞了同一个 id", i)
		}
		seen[created.Key] = true
		ids[created.ID] = true
	}
	if len(seen) != n {
		t.Errorf("%d 次创建只产出 %d 把不同的 key", n, len(seen))
	}
}

// TestFixedAPIKeyStillWorks checks that the **existing** global API Key channel is not broken.
//
// e2e and CI use that channel (AMA_API_KEY + AMA_API_KEY_EMAIL). The easiest mistake when adding
// user-level keys is to bend Resolve's branches so the fixed key goes down the database lookup
// path; then all of e2e fails, with the error "not logged in".
func TestFixedAPIKeyStillWorks(t *testing.T) {
	store := NewMemStore()
	svc, err := New(Config{
		Store: store,
		OIDC:  &fakeOIDC{},
		APIKey: APIKey{
			Key:   "fixed-ci-key",
			Email: "ci@example.com",
		},
	})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}

	identity, err := svc.Resolve(context.Background(), "fixed-ci-key")
	if err != nil {
		t.Fatalf("固定 API Key 换身份失败: %v（e2e / CI 靠这条通道）", err)
	}
	if identity.Email != "ci@example.com" {
		t.Errorf("身份是 %q，期望 ci@example.com", identity.Email)
	}
	if !identity.ViaAPIKey {
		t.Error("ViaAPIKey 应该为真")
	}
}

// TestUserAPIKeyCannotBeLoggedOut checks that user-level keys are not affected by logout.
//
// Letting logout delete a key on the way would be worse: that key may be in use by CI on another
// machine, and "who deleted it" cannot be found in the logs. Disabling it must go through an
// explicit revoke.
func TestUserAPIKeyCannotBeLoggedOut(t *testing.T) {
	svc, _ := newAPIKeyService(t)
	ctx := context.Background()

	created, err := svc.CreateAPIKey(ctx, apiKeyOwner, "k")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Logout(ctx, created.Key); err != nil {
		t.Fatalf("拿 API Key 调 logout 报错了: %v（应该静默无事发生）", err)
	}
	if _, err := svc.Resolve(ctx, created.Key); err != nil {
		t.Errorf("logout 之后 API Key 不能用了: %v——停用 key 只能走吊销", err)
	}
}

// TestLooksLikeAPIKeyDoesNotMatchSessionTokens checks that the prefix check does not take a session
// token for a key.
//
// If it did, session tokens would be looked up in the api_keys table, not found, and get 401; the
// symptom is "logged in but keeps getting logged out".
func TestLooksLikeAPIKeyDoesNotMatchSessionTokens(t *testing.T) {
	// A session token is a base64url random string without a prefix.
	for i := 0; i < 200; i++ {
		token, err := newToken()
		if err != nil {
			t.Fatal(err)
		}
		if looksLikeAPIKey(token) {
			t.Fatalf("会话 token %q 被认成了 API Key", token)
		}
	}
	// Conversely, a real key must be recognized.
	plaintext, _, err := newAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if !looksLikeAPIKey(plaintext) {
		t.Errorf("真 API Key %q 没被认出来", plaintext)
	}
}

// TestSessionTokenStillWorksAfterAPIKeyAdded checks that the session channel is not broken.
func TestSessionTokenStillWorksAfterAPIKeyAdded(t *testing.T) {
	svc, _ := newAPIKeyService(t)
	ctx := context.Background()

	token, err := svc.issueSession(ctx, Identity{Email: apiKeyOwner, Name: "张三"})
	if err != nil {
		t.Fatalf("签发会话失败: %v", err)
	}
	identity, err := svc.Resolve(ctx, token)
	if err != nil {
		t.Fatalf("会话 token 换身份失败: %v", err)
	}
	if identity.Email != apiKeyOwner {
		t.Errorf("身份是 %q，期望 %q", identity.Email, apiKeyOwner)
	}
	if identity.ViaAPIKey {
		t.Error("会话身份的 ViaAPIKey 应该为假")
	}
}
