// Public shape of onboarding.

package apimodel

// OnboardingAction is the two values of the "mark" action.
type OnboardingAction string

const (
	// OnboardingComplete means the user went through all the steps.
	OnboardingComplete OnboardingAction = "complete"
	// OnboardingSkip means the user clicked skip.
	//
	// **Skipping is stored too**, as persistent as completing. If not stored, the user is bothered on every login,
	// even though they already said clearly they do not want to see it.
	OnboardingSkip OnboardingAction = "skip"
)

// OnboardingState is one person's onboarding state, used directly as the HTTP response body.
//
// # Do none of the three fields have omitempty?
//
// The two timestamps have it, Version does not, for these reasons:
//
//   - CompletedAt / SkippedAt are "optional": not completed means the key is absent. The frontend only checks truthiness
//     (`Boolean(state.completedAt)`), where an empty string and a missing key are the same, so omitempty
//     keeps the shape cleaner.
//   - **Version must not have it.** 0 is a valid value (people with no record are 0), and omitempty would drop
//     the whole key for 0, so the frontend gets undefined. `undefined < 1` is false,
//     so new users would **not** be onboarded. That is the one thing this feature must get right, and it fails with zero errors.
//     This is one case of the hard rule "numbers have no omitempty" at the top of model/apimodel.
type OnboardingState struct {
	// CompletedAt is when onboarding was completed; empty string if not. Fixed-length ISO, same shape as the frontend.
	CompletedAt string `json:"completedAt,omitempty"`
	// SkippedAt is when onboarding was skipped; empty string if not.
	SkippedAt string `json:"skippedAt,omitempty"`
	// Version is the onboarding content version this person has seen; 0 when there is no record. See the comment in the schema.
	Version int `json:"version"`
}

// Done means "no more onboarding needed". Either completed or skipped counts.
//
// It is a method instead of letting each caller check two fields: missing the SkippedAt check means **it keeps popping up after skip**,
// which is exactly the most annoying failure right after the user said they do not want to see it.
func (s OnboardingState) Done() bool {
	return s.CompletedAt != "" || s.SkippedAt != ""
}
