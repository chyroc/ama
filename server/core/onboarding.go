// Onboarding completion state: account-level, not tied to any workspace.
//
// # Why this domain looks different from the other six
//
// The other six resource domains all hang under a workspace (`/api/workspaces/{id}/...`);
// this domain **deliberately does not**. "Have I seen the onboarding?" is a property of the
// person, not a piece of data inside some workspace. The same person sees the same state in
// every workspace, and may not even belong to any workspace yet (that is exactly the case
// for a newcomer's first login, which is the moment onboarding matters most).
//
// There is already a precedent with exactly the same shape: authorization management
// (integration.go). What they share is **account-level + the person is taken from context**,
// so the guard style below and the reasoning for the authz level both follow it.
//
// # Overreach cannot be expressed here (this is by design, not an oversight)
//
// The actor is taken only from auth.ActorFrom(ctx); **no email appears** in the path or the
// request body. So when logged-in user A calls these two endpoints, what A reads and changes
// is always A's own record. "Reading someone else's onboarding state" has no place to be
// expressed in the parameters at all.
//
// This is safer than "read an email from the body, then check it equals the current user",
// because if that check is ever missed it is a silent overreach, while the former **has no
// place to miss it at all**.
//
// ⚠️ Conversely: if one day we need "an admin checks whether someone has seen the
// onboarding", that **must be a new route with a new level**. Never add an optional email
// parameter to these two; that would break the invariant above in one stroke.
//
// # Why store it in the database, not localStorage
//
// See the top of schema_onboarding_pg.sql. In short: the requirement is "the same person is
// never onboarded twice", and localStorage is gone when you switch browsers, while
// auth_sessions is gone on logout.

package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// OnboardingRecord has moved to model/persistmodel (a Store input, not part of HTTP responses).

// GetOnboarding reads the onboarding state of the current logged-in user.
//
// **When there is no record it returns the zero value + nil, not ErrNotFound.** This follows
// the precedent of GetArkCredential: the frontend's nullable getter turns 404 into null, and
// it uses null to mean "still loading". So a new user would stay in the loading state
// forever, the onboarding would never appear, and no error would be reported.
// "This person has no record yet" is the most common normal case and should not go through
// the error path.
func (s *Service) GetOnboarding(ctx context.Context) (OnboardingState, error) {
	actor, err := onboardingActor(ctx)
	if err != nil {
		return OnboardingState{}, err
	}

	rec, err := s.store.GetOnboarding(ctx, actor)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// No record = brand-new user. The zero Version is 0, and the frontend uses it to decide "show onboarding".
			return OnboardingState{}, nil
		}
		return OnboardingState{}, err
	}
	return onboardingStateOf(rec), nil
}

// MarkOnboarding records that the current logged-in user "finished" or "skipped", and
// returns the state after recording.
//
// version is the **onboarding content version** sent by the frontend, stored as-is. The
// backend does not compare it; see the schema comment for why.
func (s *Service) MarkOnboarding(ctx context.Context, action OnboardingAction, version int) (OnboardingState, error) {
	actor, err := onboardingActor(ctx)
	if err != nil {
		return OnboardingState{}, err
	}

	now := time.Now().UTC()
	rec := persistmodel.OnboardingRecord{Email: actor, Version: version, UpdatedAt: now}
	switch action {
	case OnboardingComplete:
		rec.CompletedAt = now
	case OnboardingSkip:
		rec.SkippedAt = now
	default:
		// Reject unrecognized actions outright; **do not pick a default**. Defaulting to
		// complete would let a misspelled request permanently turn off a user's onboarding
		// without them seeing anything; defaulting to skip is the same.
		return OnboardingState{}, fmt.Errorf("%w: 引导动作只能是 complete 或 skip", ErrInvalidArgument)
	}
	// The version comes from the frontend; a negative number means the caller computed it
	// wrong. If stored, it would be smaller than any real version, so this person would be
	// **onboarded on every login**. That "it never seems to remember" symptom is very hard to
	// trace back to here.
	if version < 0 {
		return OnboardingState{}, fmt.Errorf("%w: 引导版本号不能是负数", ErrInvalidArgument)
	}

	// A single upsert, **not read-then-write**: production runs multiple instances, so two
	// clicks from the same person may land in two processes. Read-then-write is TOCTOU: both
	// sides see "no record", both insert, and the slower one hits the unique constraint and
	// returns 500, though the user did nothing wrong. Letting the database decide closes that
	// gap. See the implementation in store_pg.go.
	if err := s.store.PutOnboarding(ctx, rec); err != nil {
		return OnboardingState{}, err
	}

	// Read back instead of returning rec directly: the upsert does a coalesce on another
	// column (skipping first and finishing later keeps both times), and only reading back
	// gives the real merged result.
	saved, err := s.store.GetOnboarding(ctx, actor)
	if err != nil {
		return OnboardingState{}, err
	}
	return onboardingStateOf(saved), nil
}

// onboardingActor gets the current logged-in user, returning ErrInvalidArgument if not
// logged in.
//
// **It returns 400, not 401/403**, matching the four authorization management routes (see
// the guard in integration.go): these two routes already sit behind auth.Protect, so having
// no identity here means the caller did not go through HTTP (a test or an internal call),
// which is a parameter problem. The real "not logged in" is answered with 401 by the
// middleware further out.
func onboardingActor(ctx context.Context) (string, error) {
	actor := strings.TrimSpace(auth.ActorFrom(ctx))
	if actor == "" {
		return "", fmt.Errorf("%w: 需要先登录", ErrInvalidArgument)
	}
	// Lowercase once more. The auth entry already does this (auth/normalizeEmail); this is
	// **defense in depth**: a primary key with inconsistent case would give the same person
	// two records, showing up as "the onboarding occasionally appears again".
	return strings.ToLower(actor), nil
}

// onboardingStateOf converts the stored shape to the response shape.
func onboardingStateOf(rec persistmodel.OnboardingRecord) OnboardingState {
	return OnboardingState{
		CompletedAt: onboardingISO(rec.CompletedAt),
		SkippedAt:   onboardingISO(rec.SkippedAt),
		Version:     rec.Version,
	}
}

// onboardingISO converts a time to a fixed-length ISO string; the zero value becomes an empty string.
func onboardingISO(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return apimodel.FormatISO(t)
}
