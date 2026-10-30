// Contract tests for Store.
//
// **The same set of assertions runs once each on the in-memory and PG implementations** (TestStoreSuite), so any behavior
// drift between them fails right away. Same as in core / feishu: the in-memory implementation is the reference behavior, and the PG implementation follows it.
//
// How to get a clean PGStore, and how to skip when the database is unreachable, are both in store_pg_test.go.

package authdao

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/chyroc/ama/server/auth"
	"testing"
	"time"
)

// storeFactory builds an empty Store. Each of the two implementations has one.
type storeFactory func(t *testing.T) auth.Store

func memFactory(t *testing.T) auth.Store {
	t.Helper()
	return auth.NewMemStore()
}

// TestStoreSuite is the contract test of Store, run once each on the two implementations.
func TestStoreSuite(t *testing.T) {
	impls := []struct {
		name    string
		factory storeFactory
	}{
		{"mem", memFactory},
		{"pg", pgFactory},
	}
	for _, impl := range impls {
		t.Run(impl.name, func(t *testing.T) {
			t.Run("会话往返", func(t *testing.T) { testSessionRoundTrip(t, impl.factory) })
			t.Run("会话不存在", func(t *testing.T) { testSessionNotFound(t, impl.factory) })
			t.Run("会话覆盖", func(t *testing.T) { testSessionOverwrite(t, impl.factory) })
			t.Run("会话过期即不存在", func(t *testing.T) { testSessionExpiry(t, impl.factory) })
			t.Run("删会话", func(t *testing.T) { testDeleteSession(t, impl.factory) })
			t.Run("按邮箱删会话", func(t *testing.T) { testDeleteSessionsByEmail(t, impl.factory) })
			t.Run("OIDC 登录往返", func(t *testing.T) { testOIDCStateRoundTrip(t, impl.factory) })
			t.Run("OIDC 登录不存在", func(t *testing.T) { testOIDCStateNotFound(t, impl.factory) })
			t.Run("OIDC 登录只能消费一次", func(t *testing.T) { testOIDCStateSingleUse(t, impl.factory) })
			t.Run("OIDC 登录过期即不存在", func(t *testing.T) { testOIDCStateExpiry(t, impl.factory) })
			t.Run("空参数", func(t *testing.T) { testEmptyArguments(t, impl.factory) })
			t.Run("API Key 往返", func(t *testing.T) { testAPIKeyRoundTrip(t, impl.factory) })
			t.Run("API Key 按人列出且有序", func(t *testing.T) { testAPIKeyListOrdering(t, impl.factory) })
			t.Run("API Key 只能删自己的", func(t *testing.T) { testAPIKeyDeleteScoped(t, impl.factory) })
			t.Run("API Key 记使用时间", func(t *testing.T) { testAPIKeyTouch(t, impl.factory) })
			t.Run("用户档案往返", func(t *testing.T) { testUserRoundTrip(t, impl.factory) })
			t.Run("用户档案空值不覆盖", func(t *testing.T) { testUserUpsertKeepsNonEmpty(t, impl.factory) })
			t.Run("用户档案只有邮箱也能存", func(t *testing.T) { testUserEmailOnly(t, impl.factory) })
			t.Run("本地账号往返且邮箱唯一", func(t *testing.T) { testLocalAccountRoundTrip(t, impl.factory) })
			t.Run("第一个账号只能有一个", func(t *testing.T) { testFirstLocalAccount(t, impl.factory) })
			t.Run("已有 users 时不算第一个", func(t *testing.T) { testFirstLocalAccountBlockedByUsers(t, impl.factory) })
			t.Run("第一个账号并发只成一个", func(t *testing.T) { testFirstLocalAccountConcurrent(t, impl.factory) })
			t.Run("登录失败计数按窗口", func(t *testing.T) { testLoginFailureWindow(t, impl.factory) })
		})
	}
}

// sampleSession builds a session valid for one hour.
//
// Times are always UTC + truncated to seconds: PG's timestamptz normalizes the time zone, while Go's
// time.Time carries a monotonic clock reading, so comparing with reflect.DeepEqual directly would fail falsely.
func sampleSession(hash, email string) auth.Session {
	now := time.Now().UTC().Truncate(time.Second)
	return auth.Session{
		TokenHash:  hash,
		Email:      email,
		Name:       "张三",
		AvatarURL:  "https://x/a.png",
		EmployeeID: "12345",
		CreatedAt:  now,
		ExpiresAt:  now.Add(time.Hour),
	}
}

func sampleOIDCState(state string) auth.OIDCState {
	now := time.Now().UTC().Truncate(time.Second)
	return auth.OIDCState{
		State:        state,
		Nonce:        "nonce-" + state,
		CodeVerifier: "verifier-" + state,
		ReturnTo:     "/w/ws_1/chat",
		CreatedAt:    now,
		ExpiresAt:    now.Add(10 * time.Minute),
	}
}

func testSessionRoundTrip(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()
	want := sampleSession("hash-1", "grace@example.com")

	if err := store.PutSession(ctx, want); err != nil {
		t.Fatalf("PutSession 失败: %v", err)
	}
	got, err := store.GetSession(ctx, "hash-1")
	if err != nil {
		t.Fatalf("GetSession 失败: %v", err)
	}
	if got.Email != want.Email || got.Name != want.Name ||
		got.AvatarURL != want.AvatarURL || got.EmployeeID != want.EmployeeID {
		t.Errorf("读回来的会话不一致:\n got = %+v\nwant = %+v", got, want)
	}
	if !got.ExpiresAt.Equal(want.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, 期望 %v", got.ExpiresAt, want.ExpiresAt)
	}
}

func testSessionNotFound(t *testing.T, factory storeFactory) {
	store := factory(t)
	if _, err := store.GetSession(context.Background(), "nope"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("期望 ErrNotFound，得到 %v", err)
	}
}

func testSessionOverwrite(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()

	first := sampleSession("hash-1", "a@example.com")
	if err := store.PutSession(ctx, first); err != nil {
		t.Fatalf("PutSession 失败: %v", err)
	}
	second := sampleSession("hash-1", "b@example.com")
	second.Name = "李四"
	if err := store.PutSession(ctx, second); err != nil {
		t.Fatalf("覆盖写失败: %v", err)
	}

	got, err := store.GetSession(ctx, "hash-1")
	if err != nil {
		t.Fatalf("GetSession 失败: %v", err)
	}
	if got.Email != "b@example.com" || got.Name != "李四" {
		t.Errorf("覆盖没生效: %+v", got)
	}
}

// Expired sessions should disappear at the storage layer, so the upper layers do not have to remember to check every time.
func testSessionExpiry(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()

	expired := sampleSession("hash-expired", "a@example.com")
	expired.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	if err := store.PutSession(ctx, expired); err != nil {
		t.Fatalf("PutSession 失败: %v", err)
	}
	if _, err := store.GetSession(ctx, "hash-expired"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("过期会话应该读不到，得到 %v", err)
	}
}

func testDeleteSession(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()

	if err := store.PutSession(ctx, sampleSession("hash-1", "a@example.com")); err != nil {
		t.Fatalf("PutSession 失败: %v", err)
	}
	if err := store.DeleteSession(ctx, "hash-1"); err != nil {
		t.Fatalf("DeleteSession 失败: %v", err)
	}
	if _, err := store.GetSession(ctx, "hash-1"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("删完还能读到，得到 %v", err)
	}
	// Idempotent: deleting something missing is not an error.
	if err := store.DeleteSession(ctx, "hash-1"); err != nil {
		t.Errorf("重复删除应该幂等，得到 %v", err)
	}
}

func testDeleteSessionsByEmail(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()

	// One person with two devices, another with one.
	for _, h := range []string{"h1", "h2"} {
		if err := store.PutSession(ctx, sampleSession(h, "a@example.com")); err != nil {
			t.Fatalf("PutSession 失败: %v", err)
		}
	}
	if err := store.PutSession(ctx, sampleSession("h3", "b@example.com")); err != nil {
		t.Fatalf("PutSession 失败: %v", err)
	}

	n, err := store.DeleteSessionsByEmail(ctx, "a@example.com")
	if err != nil {
		t.Fatalf("DeleteSessionsByEmail 失败: %v", err)
	}
	if n != 2 {
		t.Errorf("删了 %d 条，期望 2", n)
	}
	// Other people's sessions must not be affected.
	if _, err := store.GetSession(ctx, "h3"); err != nil {
		t.Errorf("另一个人的会话被误删了: %v", err)
	}
}

func testOIDCStateRoundTrip(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()
	want := sampleOIDCState("state-1")

	if err := store.PutOIDCState(ctx, want); err != nil {
		t.Fatalf("PutOIDCState 失败: %v", err)
	}
	got, err := store.ConsumeOIDCState(ctx, "state-1")
	if err != nil {
		t.Fatalf("ConsumeOIDCState 失败: %v", err)
	}
	if got.State != want.State || got.ReturnTo != want.ReturnTo ||
		got.Nonce != want.Nonce || got.CodeVerifier != want.CodeVerifier {
		t.Errorf("读回来的登录流程不一致:\n got = %+v\nwant = %+v", got, want)
	}
	// After consuming, used_at must be set: it is a "one-time" credential, and also the only clue when debugging.
	if got.UsedAt.IsZero() {
		t.Error("消费之后 UsedAt 应该非零")
	}
}

func testOIDCStateNotFound(t *testing.T, factory storeFactory) {
	store := factory(t)
	if _, err := store.ConsumeOIDCState(context.Background(), "nope"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("期望 ErrNotFound，得到 %v", err)
	}
}

// ⚠️ **Both implementations must let only the first attempt succeed.**
//
// This is the core of replay protection: the callback URL stays in browser history, Referer and access logs, and being replayable means
// each of those places holds a usable key. The PG side guarantees it with a single `update ... where used_at is null`,
// and the in-memory side by running the whole function under the write lock. **Behavior drift shows up right here.**
func testOIDCStateSingleUse(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()

	if err := store.PutOIDCState(ctx, sampleOIDCState("state-1")); err != nil {
		t.Fatalf("PutOIDCState 失败: %v", err)
	}
	if _, err := store.ConsumeOIDCState(ctx, "state-1"); err != nil {
		t.Fatalf("第一次消费应该成功: %v", err)
	}
	if _, err := store.ConsumeOIDCState(ctx, "state-1"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("第二次消费必须回 ErrNotFound，得到 %v", err)
	}
}

func testOIDCStateExpiry(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()

	st := sampleOIDCState("state-expired")
	st.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	if err := store.PutOIDCState(ctx, st); err != nil {
		t.Fatalf("PutOIDCState 失败: %v", err)
	}
	// Expired is merged with "missing" and "already used" into the same error: callers handle them the same way, and telling them apart would
	// tell an attacker "this state exists, it just expired".
	if _, err := store.ConsumeOIDCState(ctx, "state-expired"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("过期的登录流程应该消费不了，得到 %v", err)
	}
}

func testEmptyArguments(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()

	if err := store.PutSession(ctx, auth.Session{}); !errors.Is(err, auth.ErrInvalidArgument) {
		t.Errorf("空 TokenHash 应该被拒，得到 %v", err)
	}
	if err := store.PutOIDCState(ctx, auth.OIDCState{}); !errors.Is(err, auth.ErrInvalidArgument) {
		t.Errorf("空 ID 应该被拒，得到 %v", err)
	}
	if _, err := store.GetSession(ctx, ""); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("空 hash 应该 ErrNotFound，得到 %v", err)
	}
	if _, err := store.ConsumeOIDCState(ctx, ""); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("空 ID 应该 ErrNotFound，得到 %v", err)
	}
	if _, err := store.DeleteSessionsByEmail(ctx, "  "); !errors.Is(err, auth.ErrInvalidArgument) {
		t.Errorf("空邮箱应该被拒，得到 %v", err)
	}
}

// ---------------------------------------------------------------------------
// Contract tests for API Keys
// ---------------------------------------------------------------------------
//
// **Both implementations run the same set**, for the same reason as above: behavior drift fails right away. This domain needs it especially,
// because "only delete your own" is implemented by the SQL where condition, while the in-memory implementation is a loop check in Go;
// getting either one wrong never fails compilation.

// testKeyHash builds a string shaped like an API Key hash (64 lowercase hex characters).
//
// It deliberately **does not reuse auth.hashAPIKey**: these tests only need "a stable and distinct
// hash string per id", not that specific algorithm. Exporting the production hashAPIKey for tests would widen the public surface
// just for directory shape, and the Store contract itself has no requirement on "how the hash is computed":
// it only stores and returns the string it is given as is.
func testKeyHash(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// sampleAPIKey builds an API Key record.
//
// Times are always UTC + truncated to seconds, for the same reason as sampleSession: PG's timestamptz normalizes the time zone,
// while Go's time.Time carries a monotonic clock reading, so comparing directly would fail falsely.
func sampleAPIKey(id, email, name string, createdAt time.Time) auth.APIKeyRecord {
	return auth.APIKeyRecord{
		ID:        id,
		KeyHash:   testKeyHash("plaintext-for-" + id),
		Email:     email,
		Name:      name,
		Prefix:    auth.KeyPrefix + id,
		CreatedAt: createdAt.UTC().Truncate(time.Second),
	}
}

func testAPIKeyRoundTrip(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()
	created := time.Now().UTC().Truncate(time.Second)

	want := sampleAPIKey("ak_1", "grace@example.com", "ci-deploy", created)
	if err := store.PutAPIKey(ctx, want); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	got, err := store.GetAPIKeyByHash(ctx, want.KeyHash)
	if err != nil {
		t.Fatalf("按 hash 读取失败: %v", err)
	}
	if got.ID != want.ID || got.Email != want.Email ||
		got.Name != want.Name || got.Prefix != want.Prefix {
		t.Errorf("读回来的记录不对:\n得到 %+v\n期望 %+v", got, want)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("createdAt 是 %v，期望 %v", got.CreatedAt, want.CreatedAt)
	}
	// **The LastUsedAt of a never-used key must be the zero value**, not a fake time.
	// The upper layer uses IsZero to decide whether to emit the lastUsedAt key.
	if !got.LastUsedAt.IsZero() {
		t.Errorf("没用过的 key 的 LastUsedAt 是 %v，期望零值", got.LastUsedAt)
	}

	// A missing hash is ErrNotFound, not an empty record.
	if _, err := store.GetAPIKeyByHash(ctx, testKeyHash("never-issued")); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("不存在的 hash 得到 %v，期望 ErrNotFound", err)
	}
	// An empty hash is also ErrNotFound (rather than querying the database with an empty string).
	if _, err := store.GetAPIKeyByHash(ctx, ""); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("空 hash 得到 %v，期望 ErrNotFound", err)
	}
	// Writes with an empty ID / empty hash must be rejected.
	if err := store.PutAPIKey(ctx, auth.APIKeyRecord{}); !errors.Is(err, auth.ErrInvalidArgument) {
		t.Errorf("空记录写入得到 %v，期望 ErrInvalidArgument", err)
	}
}

func testAPIKeyListOrdering(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	// Written out of order on purpose, with two rows having **the same creation time**, the case most likely to give a nondeterministic order.
	rows := []auth.APIKeyRecord{
		sampleAPIKey("ak_mid", "a@example.com", "中间", base.Add(-time.Hour)),
		sampleAPIKey("ak_new", "a@example.com", "最新", base),
		sampleAPIKey("ak_old", "a@example.com", "最老", base.Add(-2*time.Hour)),
		sampleAPIKey("ak_other", "b@example.com", "别人的", base),
	}
	for _, r := range rows {
		if err := store.PutAPIKey(ctx, r); err != nil {
			t.Fatalf("写入 %s 失败: %v", r.ID, err)
		}
	}

	got, err := store.ListAPIKeysByEmail(ctx, "a@example.com")
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("列出 %d 条，期望 3 条（别人的那条不该出现）", len(got))
	}
	// The contract requires **creation time descending** (newest first).
	wantOrder := []string{"ak_new", "ak_mid", "ak_old"}
	for i, want := range wantOrder {
		if got[i].ID != want {
			t.Errorf("第 %d 条是 %s，期望 %s（契约是创建时间倒序）", i, got[i].ID, want)
		}
	}
	for _, r := range got {
		if r.Email != "a@example.com" {
			t.Errorf("列表里出现了 %s 的 key", r.Email)
		}
	}

	// Email is case-insensitive.
	upper, err := store.ListAPIKeysByEmail(ctx, "A@Example.com")
	if err != nil {
		t.Fatalf("大写邮箱列表失败: %v", err)
	}
	if len(upper) != 3 {
		t.Errorf("用大写邮箱列出 %d 条，期望 3 条（邮箱应该小写化后比较）", len(upper))
	}

	// A person with no keys gets an empty result, not an error.
	none, err := store.ListAPIKeysByEmail(ctx, "nobody@example.com")
	if err != nil {
		t.Fatalf("空列表报错了: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("没有 key 的人列出了 %d 条", len(none))
	}
	// An empty email must be rejected.
	if _, err := store.ListAPIKeysByEmail(ctx, "  "); !errors.Is(err, auth.ErrInvalidArgument) {
		t.Errorf("空邮箱得到 %v，期望 ErrInvalidArgument", err)
	}
}

func testAPIKeyDeleteScoped(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	mine := sampleAPIKey("ak_mine", "me@example.com", "我的", now)
	theirs := sampleAPIKey("ak_theirs", "them@example.com", "别人的", now)
	for _, r := range []auth.APIKeyRecord{mine, theirs} {
		if err := store.PutAPIKey(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	// **Deleting someone else's must return false and change nothing**: ownership is part of the where condition.
	deleted, err := store.DeleteAPIKey(ctx, "me@example.com", theirs.ID)
	if err != nil {
		t.Fatalf("删除报错: %v", err)
	}
	if deleted {
		t.Error("删掉了别人的 key")
	}
	if _, err := store.GetAPIKeyByHash(ctx, theirs.KeyHash); err != nil {
		t.Errorf("别人的 key 被动了: %v", err)
	}

	// Deleting your own must succeed.
	deleted, err = store.DeleteAPIKey(ctx, "me@example.com", mine.ID)
	if err != nil {
		t.Fatalf("删自己的 key 报错: %v", err)
	}
	if !deleted {
		t.Error("删自己的 key 返回 false")
	}
	if _, err := store.GetAPIKeyByHash(ctx, mine.KeyHash); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("删完还能查到: %v", err)
	}

	// Deleting again returns false instead of an error (idempotent).
	deleted, err = store.DeleteAPIKey(ctx, "me@example.com", mine.ID)
	if err != nil {
		t.Errorf("重复删除报错: %v", err)
	}
	if deleted {
		t.Error("重复删除返回了 true")
	}

	// Empty arguments delete nothing.
	if deleted, err := store.DeleteAPIKey(ctx, "", theirs.ID); err != nil || deleted {
		t.Errorf("空邮箱删除得到 (%v, %v)，期望 (false, nil)", deleted, err)
	}
	if deleted, err := store.DeleteAPIKey(ctx, "them@example.com", ""); err != nil || deleted {
		t.Errorf("空 ID 删除得到 (%v, %v)，期望 (false, nil)", deleted, err)
	}
}

func testAPIKeyTouch(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()
	created := time.Now().UTC().Truncate(time.Second)

	rec := sampleAPIKey("ak_touch", "me@example.com", "k", created)
	if err := store.PutAPIKey(ctx, rec); err != nil {
		t.Fatal(err)
	}

	usedAt := created.Add(30 * time.Minute)
	if err := store.TouchAPIKey(ctx, rec.KeyHash, usedAt); err != nil {
		t.Fatalf("记使用时间失败: %v", err)
	}

	got, err := store.GetAPIKeyByHash(ctx, rec.KeyHash)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastUsedAt.Equal(usedAt) {
		t.Errorf("LastUsedAt 是 %v，期望 %v", got.LastUsedAt, usedAt)
	}
	// Touching again must overwrite with the newer time.
	later := usedAt.Add(time.Hour)
	if err := store.TouchAPIKey(ctx, rec.KeyHash, later); err != nil {
		t.Fatal(err)
	}
	got, _ = store.GetAPIKeyByHash(ctx, rec.KeyHash)
	if !got.LastUsedAt.Equal(later) {
		t.Errorf("第二次 touch 后 LastUsedAt 是 %v，期望 %v", got.LastUsedAt, later)
	}

	// **Touching a hash that does not exist is not an error**: on that path the key may have just been revoked,
	// and failing to record usage time should not fail the request.
	if err := store.TouchAPIKey(ctx, testKeyHash("gone"), later); err != nil {
		t.Errorf("touch 不存在的 key 报错了: %v", err)
	}
	if err := store.TouchAPIKey(ctx, "", later); err != nil {
		t.Errorf("touch 空 hash 报错了: %v", err)
	}
}

// -- User profiles ----------------------------------------------------------------

// sampleUser builds a profile. Times are always UTC + truncated to seconds, for the same reason as sampleSession.
func sampleUser(email, name, avatar string) auth.User {
	now := time.Now().UTC().Truncate(time.Second)
	return auth.User{Email: email, Name: name, AvatarURL: avatar, CreatedAt: now, UpdatedAt: now}
}

func testUserRoundTrip(t *testing.T, newStore storeFactory) {
	s := newStore(t)
	ctx := context.Background()

	want := sampleUser("frank@example.com", "李维", "https://example.com/a.png")
	if err := s.PutUser(ctx, want); err != nil {
		t.Fatalf("存用户档案失败：%v", err)
	}
	got, err := s.GetUser(ctx, "frank@example.com")
	if err != nil {
		t.Fatalf("取用户档案失败：%v", err)
	}
	if got.Email != want.Email || got.Name != want.Name || got.AvatarURL != want.AvatarURL {
		t.Errorf("档案不一致：%+v", got)
	}

	// Email is case-insensitive: Actor is already lowercased at the entry point, but callers may pass it as is.
	if _, err := s.GetUser(ctx, "Frank@Example.com"); err != nil {
		t.Errorf("按大小写不同的邮箱查不到：%v", err)
	}

	if _, err := s.GetUser(ctx, "nobody@example.com"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("查不存在的人应回 ErrNotFound，实际 %v", err)
	}
}

// testUserUpsertKeepsNonEmpty pins "empty values do not overwrite existing non-empty values".
//
// **This guards against silent data loss**: SSO does not guarantee name and picture every time,
// and if one login misses them and writes empty strings back, that person suddenly shows only an email in the member list,
// with no error at all, looking like "the avatar is broken".
func testUserUpsertKeepsNonEmpty(t *testing.T, newStore storeFactory) {
	s := newStore(t)
	ctx := context.Background()

	full := sampleUser("erin@example.com", "王朗", "https://example.com/w.png")
	if err := s.PutUser(ctx, full); err != nil {
		t.Fatalf("首次存档案失败：%v", err)
	}

	// The second login only got the email (name and avatar are both empty).
	blank := sampleUser("erin@example.com", "", "")
	blank.UpdatedAt = full.UpdatedAt.Add(time.Hour)
	if err := s.PutUser(ctx, blank); err != nil {
		t.Fatalf("二次存档案失败：%v", err)
	}

	got, err := s.GetUser(ctx, "erin@example.com")
	if err != nil {
		t.Fatalf("取档案失败：%v", err)
	}
	if got.Name != "王朗" {
		t.Errorf("空 name 把已有的真名冲掉了，现在是 %q", got.Name)
	}
	if got.AvatarURL != "https://example.com/w.png" {
		t.Errorf("空头像把已有的冲掉了，现在是 %q", got.AvatarURL)
	}
	// CreatedAt is "the first time this person was seen"; later logins must not refresh it.
	if !got.CreatedAt.Equal(full.CreatedAt) {
		t.Errorf("CreatedAt 被后续登录刷新了：%v → %v", full.CreatedAt, got.CreatedAt)
	}
}

// testUserEmailOnly pins "a row with only an email is valid".
//
// That is the state when someone who has never logged in is invited into a workspace. If it were designed as "a row only after login",
// that person could not even get a name in the member list.
func testUserEmailOnly(t *testing.T, newStore storeFactory) {
	s := newStore(t)
	ctx := context.Background()

	if err := s.PutUser(ctx, sampleUser("newcomer@example.com", "", "")); err != nil {
		t.Fatalf("只有邮箱的档案存不进去：%v", err)
	}
	got, err := s.GetUser(ctx, "newcomer@example.com")
	if err != nil {
		t.Fatalf("取档案失败：%v", err)
	}
	if got.Name != "" || got.AvatarURL != "" {
		t.Errorf("空档案不该被填上东西：%+v", got)
	}

	// An empty email must be rejected: that is a profile with no identity, and storing it would only create a garbage row that can never be found.
	if err := s.PutUser(ctx, sampleUser("", "无名", "")); err == nil {
		t.Error("空邮箱应该报错")
	}
}

func sampleAccount(email string) auth.LocalAccount {
	now := time.Now().UTC().Truncate(time.Second)
	return auth.LocalAccount{Email: email, PasswordHash: "$2a$04$hash-" + email, CreatedAt: now, UpdatedAt: now}
}

func testLocalAccountRoundTrip(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()
	acct := sampleAccount("Alice@Example.com")
	acct.Admin = true
	acct.CreatedBy = "root@example.com"
	ok, err := store.CreateLocalAccount(ctx, acct)
	if err != nil || !ok {
		t.Fatalf("CreateLocalAccount = %v, %v", ok, err)
	}
	got, err := store.GetLocalAccount(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("GetLocalAccount: %v", err)
	}
	if got.Email != "alice@example.com" || got.PasswordHash != acct.PasswordHash || !got.Admin ||
		got.Bootstrap || got.CreatedBy != "root@example.com" {
		t.Errorf("读回不一致: %+v", got)
	}
	// The same email (with different case) does not overwrite.
	again := sampleAccount("ALICE@example.com")
	again.PasswordHash = "other"
	ok, err = store.CreateLocalAccount(ctx, again)
	if err != nil || ok {
		t.Fatalf("重复邮箱应返回 (false, nil)，得到 %v, %v", ok, err)
	}
	got, _ = store.GetLocalAccount(ctx, "alice@example.com")
	if got.PasswordHash != acct.PasswordHash {
		t.Error("重复注册覆盖了原密码")
	}
	if _, err := store.GetLocalAccount(ctx, "nobody@example.com"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("不存在应 ErrNotFound，得到 %v", err)
	}
	any, err := store.HasAnyUser(ctx)
	if err != nil || !any {
		t.Errorf("HasAnyUser = %v, %v，期望 true", any, err)
	}
}

func testFirstLocalAccount(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()
	any, err := store.HasAnyUser(ctx)
	if err != nil || any {
		t.Fatalf("空库 HasAnyUser = %v, %v", any, err)
	}
	ok, err := store.CreateFirstLocalAccount(ctx, sampleAccount("first@example.com"))
	if err != nil || !ok {
		t.Fatalf("第一个账号应建成: %v, %v", ok, err)
	}
	got, _ := store.GetLocalAccount(ctx, "first@example.com")
	if !got.Bootstrap {
		t.Error("第一个账号应带 Bootstrap")
	}
	ok, err = store.CreateFirstLocalAccount(ctx, sampleAccount("second@example.com"))
	if err != nil || ok {
		t.Fatalf("第二个不该被当成第一个: %v, %v", ok, err)
	}
	// Normal creation is unaffected, and has no bootstrap.
	ok, err = store.CreateLocalAccount(ctx, sampleAccount("second@example.com"))
	if err != nil || !ok {
		t.Fatalf("普通创建失败: %v, %v", ok, err)
	}
	got, _ = store.GetLocalAccount(ctx, "second@example.com")
	if got.Bootstrap {
		t.Error("普通账号不该带 Bootstrap")
	}
}

func testFirstLocalAccountBlockedByUsers(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.PutUser(ctx, auth.User{Email: "oidc@example.com", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	ok, err := store.CreateFirstLocalAccount(ctx, sampleAccount("first@example.com"))
	if err != nil || ok {
		t.Fatalf("users 里已经有人时不算第一个: %v, %v", ok, err)
	}
}

// Two people racing to register the first account; only one may succeed. The PG side relies on the bootstrap unique index, the in-memory side on the write lock.
func testFirstLocalAccountConcurrent(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()
	const n = 8
	results := make(chan bool, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			ok, err := store.CreateFirstLocalAccount(ctx, sampleAccount(fmt.Sprintf("racer%d@example.com", i)))
			if err != nil {
				errs <- err
				return
			}
			results <- ok
		}(i)
	}
	created := 0
	for i := 0; i < n; i++ {
		select {
		case err := <-errs:
			t.Fatalf("并发抢注报错: %v", err)
		case ok := <-results:
			if ok {
				created++
			}
		}
	}
	if created != 1 {
		t.Fatalf("并发抢注第一个账号，建成了 %d 个，期望 1", created)
	}
}

func testLoginFailureWindow(t *testing.T, factory storeFactory) {
	store := factory(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	win := 15 * time.Minute
	for i := 1; i <= 3; i++ {
		n, err := store.RecordLoginFailure(ctx, "email:a@example.com", now, win)
		if err != nil || n != i {
			t.Fatalf("第 %d 次计数 = %d, %v", i, n, err)
		}
	}
	// Other keys do not affect each other.
	if n, _ := store.RecordLoginFailure(ctx, "email:b@example.com", now, win); n != 1 {
		t.Errorf("另一个 key 应从 1 开始，得到 %d", n)
	}
	// After the window passes, counting starts over.
	if n, _ := store.RecordLoginFailure(ctx, "email:a@example.com", now.Add(win+time.Second), win); n != 1 {
		t.Errorf("窗口过后应重置为 1，得到 %d", n)
	}
	if err := store.ClearLoginFailures(ctx, "email:a@example.com"); err != nil {
		t.Fatal(err)
	}
	if n, _ := store.RecordLoginFailure(ctx, "email:a@example.com", now.Add(win+2*time.Second), win); n != 1 {
		t.Errorf("清除后应从 1 开始，得到 %d", n)
	}
}
