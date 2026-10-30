// User preferences (language and appearance): per account, not tied to any workspace.
//
// # Why this domain looks different from the other six
//
// Same shape as onboarding.go: per account, and the user comes from the context. The six
// resource domains all live under a workspace (`/api/workspaces/{id}/...`). This one
// **deliberately does not**: "I like dark mode" belongs to the person, not to some record
// inside a workspace. The person may not even belong to any workspace yet.
//
// # Access to someone else's data cannot be expressed here (by design, not an oversight)
//
// The actor comes only from auth.ActorFrom(ctx). **No email appears** in the path or the
// request body. So when logged-in user A calls these two endpoints, A always reads and
// writes A's own record.
// This is safer than "read the email from the body, then check it equals the current user".
// If that check is ever forgotten, it silently leaks access. Here **there is no place to
// forget it**.
//
// # ⚠️ Storing in the DB does not mean localStorage can go
//
// The frontend must apply the appearance to <html> before the first frame (otherwise it
// flashes light and then switches to dark). At that moment there is no login yet, let alone
// a network round trip. So localStorage stays, but **it is downgraded from the source of
// truth to a cache**: the DB is the cross-device source of truth, the cache handles the
// first frame. Both are written; reads check the cache first, then the DB.
//
// See the top of schema_prefs_pg.sql for details.

package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// Valid values for language and appearance. **They must match the union types in the
// frontend prefs/storage.ts exactly**. Otherwise the frontend can save a value, the backend
// reads it back and falls back to the default, and the user sees "I set it but it did not
// stick".
const (
	// LanguageZhCN is Simplified Chinese, the default.
	LanguageZhCN = "zh-CN"
	// LanguageEnUS is English.
	LanguageEnUS = "en-US"

	// AppearanceLight is light mode.
	AppearanceLight = "light"
	// AppearanceDark is dark mode.
	AppearanceDark = "dark"
	// AppearanceSystem follows the system setting, the default.
	AppearanceSystem = "system"
)

// UserPreferencesRecord has moved to model/persistmodel (same as OnboardingRecord).

// DefaultUserPreferences is the preferences value when no record exists.
//
// **It must match the frontend DEFAULT_PREFS exactly** (prefs/storage.ts). If not, a person
// who never set anything sees the UI jump between "first frame uses the localStorage
// default" and "the API response arrives", and nobody did anything to cause that jump.
func DefaultUserPreferences() UserPreferences {
	return UserPreferences{Language: LanguageZhCN, Appearance: AppearanceSystem}
}

// GetUserPreferences reads the preferences of the current logged-in user.
//
// **When no record exists it returns the defaults + nil, not ErrNotFound.** This follows
// GetOnboarding and GetArkCredential: the frontend's nullable getter turns 404 into null,
// and null there means "still loading". So a person who never set preferences would stay in
// the loading state forever, with no error at all.
// "This person has not saved preferences yet" is the most common normal case and should not
// go through the error path.
func (s *Service) GetUserPreferences(ctx context.Context) (UserPreferences, error) {
	actor, err := preferencesActor(ctx)
	if err != nil {
		return UserPreferences{}, err
	}

	rec, err := s.store.GetUserPreferences(ctx, actor)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return DefaultUserPreferences(), nil
		}
		return UserPreferences{}, err
	}
	// Normalize on the read path too: the DB may hold values written by an older version,
	// or someone may have edited the DB directly.
	// Read and write paths each normalize once and do not rely on each other, same as
	// feishu's NormalizeTriggerMode.
	return normalizeUserPreferences(UserPreferences{
		Language:   rec.Language,
		Appearance: rec.Appearance,
	}), nil
}

// SaveUserPreferences saves the preferences of the current logged-in user and returns the
// stored value.
//
// # Unknown values fall back to the default instead of returning an error
//
// This is **deliberately the opposite** of MarkOnboarding, which rejects unknown actions,
// because the cost of a mistake differs: a wrong onboarding action can turn off a feature
// for good (that must be blocked), while a wrong preference at worst shows an unexpected UI
// language, which the user sees right away and can change back.
//
// More importantly, there is a real source of bad values on this path: **the frontend
// uploads the old values from localStorage as is** (see the first sync in
// prefs/PrefsContext.tsx). That data has built up on the user's machine for a long time and
// may well contain values written by older versions or edited by hand. Returning 400 for one
// bad field would look like "preferences never save", and the user would have no idea that
// some old value is the cause.
func (s *Service) SaveUserPreferences(ctx context.Context, prefs UserPreferences) (UserPreferences, error) {
	actor, err := preferencesActor(ctx)
	if err != nil {
		return UserPreferences{}, err
	}

	normalized := normalizeUserPreferences(prefs)
	rec := persistmodel.UserPreferencesRecord{
		Email:      actor,
		Language:   normalized.Language,
		Appearance: normalized.Appearance,
		UpdatedAt:  time.Now().UTC(),
	}

	// A single upsert, **no read-then-write**: production runs many instances, and the same
	// person changing settings once in each of two tabs may hit two processes. Read-then-write
	// is TOCTOU: both see "no record", both insert, the slower one hits the primary key and
	// gets a 500, even though the user did nothing wrong.
	// Letting the database decide closes that gap. See the implementation in store_pg.go.
	if err := s.store.PutUserPreferences(ctx, rec); err != nil {
		return UserPreferences{}, err
	}
	// **Return the normalized value directly, without reading back.**
	//
	// Unlike MarkOnboarding: there the upsert coalesces two columns (merge behavior), so
	// only a read-back gets the merged result. Here the whole row is overwritten, so what we
	// write is what is stored. A read-back would only add a round trip, and when two tabs
	// save at the same time it could return the other tab's value to this one.
	return normalized, nil
}

// preferencesActor returns the current logged-in user; if there is none it returns
// ErrInvalidArgument.
//
// **It returns 400, not 401/403**, same as onboarding and access management: these two routes
// already sit behind auth.Protect, so reaching here without an identity means the caller did
// not come through HTTP (a test or an internal call), which is an argument problem. The real
// "not logged in" case gets a 401 from the middleware further out.
func preferencesActor(ctx context.Context) (string, error) {
	actor := strings.TrimSpace(auth.ActorFrom(ctx))
	if actor == "" {
		return "", fmt.Errorf("%w: 需要先登录", ErrInvalidArgument)
	}
	// Lowercase once more. The auth entry already did it (auth/normalizeEmail); this is
	// **defense in depth**: a primary key with mixed case gives the same person two records,
	// which shows up as "my preferences sometimes revert".
	return strings.ToLower(actor), nil
}

// normalizeUserPreferences maps any input to valid values; unknown ones fall back to the
// default.
//
// Each field is checked on its own, not "one bad field resets everything": if the user only
// got the language wrong, the dark mode they set should not be wiped too. This matches the
// per-field check in the frontend loadPrefs() (both sides need it: the backend keeps bad
// data out of the DB, the frontend guards against bad data already in the DB or the cache).
func normalizeUserPreferences(p UserPreferences) UserPreferences {
	out := DefaultUserPreferences()
	switch p.Language {
	case LanguageZhCN, LanguageEnUS:
		out.Language = p.Language
	}
	switch p.Appearance {
	case AppearanceLight, AppearanceDark, AppearanceSystem:
		out.Appearance = p.Appearance
	}
	return out
}
