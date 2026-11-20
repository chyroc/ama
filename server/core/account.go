package core

import (
	"context"

	"github.com/chyroc/ama/server/model/persistmodel"
)

// AccountStore is the narrow persistence contract shared by onboarding and preferences.
// It deliberately excludes sessions, agents and login providers so downstreams can reuse
// account-level behavior without implementing the full core Store.
type AccountStore interface {
	GetOnboarding(ctx context.Context, email string) (persistmodel.OnboardingRecord, error)
	PutOnboarding(ctx context.Context, rec persistmodel.OnboardingRecord) error
	GetUserPreferences(ctx context.Context, email string) (persistmodel.UserPreferencesRecord, error)
	PutUserPreferences(ctx context.Context, rec persistmodel.UserPreferencesRecord) error
}

// AccountService owns account-level onboarding and preferences.
type AccountService struct{ store AccountStore }

// NewAccountService builds the service with only the methods it needs.
func NewAccountService(store AccountStore) *AccountService {
	return &AccountService{store: store}
}
