// Tests for user preferences: normalization and defaults in the Service layer.
//
// The Store contracts live in repository/dao/preferences_store_test.go, where a shared suite
// runs **once against Mem and once against PG**. The two implementations must have the same
// upsert behavior, and the difference "overwrite the whole row vs merge" does not show up in
// a test against only one of them.

package core

import (
	"context"
	"errors"
	"testing"
)

const (
	prefsAlice = "alice@example.com"
	prefsBob   = "bob@example.com"
)

// TestPreferencesDefaultsWhenNeverSet checks that we return the defaults when nothing was
// ever set: not an error, not an empty string.
//
// **This is the existing-data case**: accounts from before the upgrade have no row at all in
// user_preferences, and what we read must be exactly the same as today's behavior
// (zh-CN + system).
//
// The cost of an empty string is concrete: on the frontend MESSAGES[""] is undefined, t()
// falls back to returning the key as is, and **the whole screen shows strings like
// "nav.overview"**. The page does not error; all the text is just gone.
func TestPreferencesDefaultsWhenNeverSet(t *testing.T) {
	svc, _ := newService(t)

	got, err := svc.GetUserPreferences(prefsCtx(prefsAlice))
	if err != nil {
		t.Fatalf("读没设置过的偏好失败：%v——「还没设置过」是正常情况，不该走错误通道", err)
	}
	if got.Language != LanguageZhCN || got.Appearance != AppearanceSystem {
		t.Errorf("默认档是 %+v，期望 {zh-CN system}——"+
			"存量账号（升级前就有、没有这一行）必须继续按现状工作", got)
	}
}

// TestPreferencesRoundTrip checks that values can be saved and read back.
func TestPreferencesRoundTrip(t *testing.T) {
	svc, _ := newService(t)
	ctx := prefsCtx(prefsAlice)

	saved, err := svc.SaveUserPreferences(ctx, UserPreferences{
		Language: LanguageEnUS, Appearance: AppearanceDark,
	})
	if err != nil {
		t.Fatalf("存偏好失败：%v", err)
	}
	if saved.Language != LanguageEnUS || saved.Appearance != AppearanceDark {
		t.Errorf("存完返回 %+v，期望 {en-US dark}", saved)
	}

	got, err := svc.GetUserPreferences(ctx)
	if err != nil {
		t.Fatalf("回读失败：%v", err)
	}
	if got != saved {
		t.Errorf("回读得到 %+v，与存进去的 %+v 不一致", got, saved)
	}
}

// TestPreferencesCanReturnToSystem checks that "switch from dark back to follow system" sticks.
//
// # This one watches the merge behavior of upsert
//
// system is the **default value**, so under any merge style of "keep the old value when the
// input is missing" (for example copying the coalesce from PutOnboarding), it looks exactly
// like "no new value given", and the change is silently dropped: the user clicks "follow
// system", refreshes, and it is still dark, with no error.
//
// The language side is the same: going from en-US back to zh-CN is "change back to the
// default", the same trap.
func TestPreferencesCanReturnToSystem(t *testing.T) {
	svc, _ := newService(t)
	ctx := prefsCtx(prefsAlice)

	if _, err := svc.SaveUserPreferences(ctx, UserPreferences{
		Language: LanguageEnUS, Appearance: AppearanceDark,
	}); err != nil {
		t.Fatalf("先存一份非默认的失败：%v", err)
	}

	// Change both fields back to the default.
	if _, err := svc.SaveUserPreferences(ctx, UserPreferences{
		Language: LanguageZhCN, Appearance: AppearanceSystem,
	}); err != nil {
		t.Fatalf("改回默认档失败：%v", err)
	}

	got, err := svc.GetUserPreferences(ctx)
	if err != nil {
		t.Fatalf("回读失败：%v", err)
	}
	if got.Appearance != AppearanceSystem {
		t.Errorf("❌ 外观是 %q，期望 system——"+
			"「改回默认值」被合并语义当成「没提供新值」丢掉了，而这个失败零报错", got.Appearance)
	}
	if got.Language != LanguageZhCN {
		t.Errorf("❌ 语言是 %q，期望 zh-CN——同上", got.Language)
	}
}

// TestPreferencesNormalizesBadValues checks that bad values fall back to the defaults, and
// that this happens **per field**.
//
// Bad values have real sources: the frontend migrates values that sat in localStorage for a
// long time as is, and these may hold things written by old versions or broken by hand.
func TestPreferencesNormalizesBadValues(t *testing.T) {
	svc, _ := newService(t)
	ctx := prefsCtx(prefsAlice)

	// Language is bad, appearance is fine: **only the language should fall back**.
	got, err := svc.SaveUserPreferences(ctx, UserPreferences{
		Language: "kl-KL", Appearance: AppearanceDark,
	})
	if err != nil {
		t.Fatalf("存带脏值的偏好失败：%v——脏值该被归一化，不该报错"+
			"（否则用户会「偏好怎么也存不上」而不知道是哪个旧值在作祟）", err)
	}
	if got.Language != LanguageZhCN {
		t.Errorf("脏语言归一化成 %q，期望 zh-CN", got.Language)
	}
	if got.Appearance != AppearanceDark {
		t.Errorf("❌ 外观被连坐成了 %q，期望 dark——"+
			"用户只是语言填错了，不该把他设好的深色一起冲掉", got.Appearance)
	}

	// The other way round: appearance bad, language fine.
	got, err = svc.SaveUserPreferences(ctx, UserPreferences{
		Language: LanguageEnUS, Appearance: "neon",
	})
	if err != nil {
		t.Fatalf("存失败：%v", err)
	}
	if got.Appearance != AppearanceSystem || got.Language != LanguageEnUS {
		t.Errorf("逐字段归一化不对：%+v，期望 {en-US system}", got)
	}

	// An empty request body (both fields missing) must also get the full defaults, not empty
	// strings.
	got, err = svc.SaveUserPreferences(ctx, UserPreferences{})
	if err != nil {
		t.Fatalf("存空偏好失败：%v", err)
	}
	if got.Language == "" || got.Appearance == "" {
		t.Errorf("❌ 归一化后仍有空串：%+v——空串会让前端整页文案退化成翻译键", got)
	}
}

// TestPreferencesAreIsolatedPerAccount checks that A's preferences can be neither read nor
// changed by B.
//
// The current user comes from the context, so this also checks that "we picked the right
// person".
func TestPreferencesAreIsolatedPerAccount(t *testing.T) {
	svc, _ := newService(t)

	if _, err := svc.SaveUserPreferences(prefsCtx(prefsAlice), UserPreferences{
		Language: LanguageEnUS, Appearance: AppearanceDark,
	}); err != nil {
		t.Fatalf("存 alice 的偏好失败：%v", err)
	}

	// bob never set anything and must get the defaults, not alice's values.
	bob, err := svc.GetUserPreferences(prefsCtx(prefsBob))
	if err != nil {
		t.Fatalf("读 bob 的偏好失败：%v", err)
	}
	if bob.Language != LanguageZhCN || bob.Appearance != AppearanceSystem {
		t.Errorf("❌ bob 读到了 %+v，期望默认档——A 的偏好串到了 B 那里", bob)
	}

	// After bob saves his own, alice's are not affected.
	if _, err := svc.SaveUserPreferences(prefsCtx(prefsBob), UserPreferences{
		Language: LanguageZhCN, Appearance: AppearanceLight,
	}); err != nil {
		t.Fatalf("存 bob 的偏好失败：%v", err)
	}
	alice, err := svc.GetUserPreferences(prefsCtx(prefsAlice))
	if err != nil {
		t.Fatalf("回读 alice 失败：%v", err)
	}
	if alice.Language != LanguageEnUS || alice.Appearance != AppearanceDark {
		t.Errorf("❌ bob 的保存改到了 alice 的偏好：%+v，期望 {en-US dark}", alice)
	}
}

// TestPreferencesEmailIsCaseInsensitive checks that the same person with different letter
// case shares one record.
//
// Member emails are typed in by admins and may be written as Alice@example.com. A primary key
// that differs in case gives the same person two records, which shows up as "my preferences
// sometimes change back". Such on-and-off problems are very hard to track down.
func TestPreferencesEmailIsCaseInsensitive(t *testing.T) {
	svc, _ := newService(t)

	if _, err := svc.SaveUserPreferences(prefsCtx("Alice@Example.com"), UserPreferences{
		Language: LanguageEnUS, Appearance: AppearanceDark,
	}); err != nil {
		t.Fatalf("存失败：%v", err)
	}
	got, err := svc.GetUserPreferences(prefsCtx(prefsAlice))
	if err != nil {
		t.Fatalf("读失败：%v", err)
	}
	if got.Language != LanguageEnUS || got.Appearance != AppearanceDark {
		t.Errorf("❌ 大小写不同的同一个人拿到了 %+v，期望 {en-US dark}——"+
			"主键没小写化，同一个人有了两条记录", got)
	}
}

// TestPreferencesRequireIdentity checks that it returns ErrInvalidArgument when there is no
// identity.
func TestPreferencesRequireIdentity(t *testing.T) {
	svc, _ := newService(t)

	if _, err := svc.GetUserPreferences(context.Background()); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("无身份读偏好得到 %v，期望 ErrInvalidArgument", err)
	}
	if _, err := svc.SaveUserPreferences(context.Background(), UserPreferences{}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("无身份存偏好得到 %v，期望 ErrInvalidArgument", err)
	}
}
