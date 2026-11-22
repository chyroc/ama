// Store contract for user preferences: the shared suite in store_test.go runs it **once each against Mem and PG**.
//
// The two implementations must upsert the same way, and the difference "overwrite whole row vs merge" does not show
// in a one-sided test. Service-layer normalization and defaults are still tested in core/preferences_test.go.

package coredao

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/model/persistmodel"
)

const (
	prefsAlice = "alice@example.com"
	prefsBob   = "bob@example.com"
)

// -- Store contract (run once each against Mem and PG by store_test.go) ---------------------

// testUserPreferencesRoundTrip: what is stored can be read back; with no record, ErrNotFound.
func testUserPreferencesRoundTrip(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)

	if _, err := s.GetUserPreferences(ctx, prefsAlice); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("没有记录时得到 %v，期望 ErrNotFound", err)
	}

	at := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	rec := persistmodel.UserPreferencesRecord{
		Email: prefsAlice, Language: core.LanguageEnUS, Appearance: core.AppearanceDark, UpdatedAt: at,
	}
	if err := s.PutUserPreferences(ctx, rec); err != nil {
		t.Fatalf("存失败：%v", err)
	}

	got, err := s.GetUserPreferences(ctx, prefsAlice)
	if err != nil {
		t.Fatalf("读失败：%v", err)
	}
	if got.Language != core.LanguageEnUS || got.Appearance != core.AppearanceDark {
		t.Errorf("读回 %+v，与存进去的不一致", got)
	}
	if !got.UpdatedAt.Equal(at) {
		t.Errorf("updatedAt 读回 %v，期望 %v", got.UpdatedAt, at)
	}
}

// testUserPreferencesOverwrites checks that upsert **overwrites the whole row**, with no merging.
//
// This is the exact opposite of testOnboardingMergesBothTimestamps, and the two must not be "unified":
// the two onboarding columns each record a separate event that happened, while these two preference columns are always submitted together.
// Adding coalesce here would lose "change back to the default" (see TestPreferencesCanReturnToSystem).
func testUserPreferencesOverwrites(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)

	at := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	if err := s.PutUserPreferences(ctx, persistmodel.UserPreferencesRecord{
		Email: prefsAlice, Language: core.LanguageEnUS, Appearance: core.AppearanceDark, UpdatedAt: at,
	}); err != nil {
		t.Fatalf("第一次存失败：%v", err)
	}

	later := at.Add(time.Hour)
	if err := s.PutUserPreferences(ctx, persistmodel.UserPreferencesRecord{
		Email: prefsAlice, Language: core.LanguageZhCN, Appearance: core.AppearanceSystem, UpdatedAt: later,
	}); err != nil {
		t.Fatalf("覆盖失败：%v", err)
	}

	got, err := s.GetUserPreferences(ctx, prefsAlice)
	if err != nil {
		t.Fatalf("读失败：%v", err)
	}
	if got.Language != core.LanguageZhCN || got.Appearance != core.AppearanceSystem {
		t.Errorf("❌ 覆盖后读到 %+v，期望 {zh-CN system}——"+
			"upsert 必须整行覆盖；带上 coalesce 会让「改回默认值」被丢掉", got)
	}
	if !got.UpdatedAt.Equal(later) {
		t.Errorf("updatedAt 没跟着更新：%v", got.UpdatedAt)
	}
}

// testUserPreferencesIsPerAccount checks that the Store layer already isolates by email.
func testUserPreferencesIsPerAccount(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)

	at := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	if err := s.PutUserPreferences(ctx, persistmodel.UserPreferencesRecord{
		Email: prefsAlice, Language: core.LanguageEnUS, Appearance: core.AppearanceDark, UpdatedAt: at,
	}); err != nil {
		t.Fatalf("存 alice 失败：%v", err)
	}

	if _, err := s.GetUserPreferences(ctx, prefsBob); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("❌ alice 存完之后 bob 竟然也有记录了（err=%v）——偏好串号了", err)
	}

	if err := s.PutUserPreferences(ctx, persistmodel.UserPreferencesRecord{
		Email: prefsBob, Language: core.LanguageZhCN, Appearance: core.AppearanceLight, UpdatedAt: at,
	}); err != nil {
		t.Fatalf("存 bob 失败：%v", err)
	}
	alice, err := s.GetUserPreferences(ctx, prefsAlice)
	if err != nil {
		t.Fatalf("读 alice 失败：%v", err)
	}
	if alice.Language != core.LanguageEnUS || alice.Appearance != core.AppearanceDark {
		t.Errorf("❌ bob 的写入改到了 alice：%+v", alice)
	}
}
