package core

import "context"

// SessionCredentialSource binds one integration ID to its downstream injector. The public
// loop does not know the provider, credential subject, or variable names; those remain private.
type SessionCredentialSource struct {
	ID     string
	Inject func(context.Context) error
}

// InjectSessionCredentials applies enabled sources in the supplied order. A source failure
// returns the already computed values and the error: callers can log the problem and still
// inject user variables and successful credentials. Never discard partial results merely
// because an optional provider is unavailable.
func InjectSessionCredentials(
	ctx context.Context, integrationIDs []string, sources []SessionCredentialSource,
) error {
	for _, source := range sources {
		if source.Inject == nil || !IntegrationEnabled(integrationIDs, source.ID) {
			continue
		}
		if err := source.Inject(ctx); err != nil {
			return err
		}
	}
	return nil
}

// IntegrationEnabled uses explicit membership, not a legacy default-on boolean. An empty
// integration list means no credential is granted.
func IntegrationEnabled(ids []string, wanted string) bool {
	for _, id := range ids {
		if id == wanted {
			return true
		}
	}
	return false
}
