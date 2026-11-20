package agent

import (
	"context"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
)

// AgentCredentialIDs contains named credential references selected by an Agent.
// Values are opaque ids; secrets must never be placed here.
type AgentCredentialIDs = apimodel.AgentCredentialIDs

// CredentialPolicy checks service-account authorization changes beyond the built-in
// personal-authorization rules. Implementations can enforce workspace bindings and
// role checks against their own registry. Nil preserves the original behavior.
type CredentialPolicy interface {
	AllowServiceCredentials(
		ctx context.Context, current, incoming apimodel.Agent,
		credentials apimodel.AgentCredentialIDs,
	) error
}

func credentialsOf(a apimodel.Agent) apimodel.AgentCredentialIDs {
	return apimodel.AgentCredentialIDs{
		apimodel.IntegrationCodebaseApp:    strings.TrimSpace(a.CodebaseAppCredentialID),
		apimodel.IntegrationServiceAccount: strings.TrimSpace(a.ServiceAccountCredentialID),
	}
}

func (s *Service) allowServiceCredentials(
	ctx context.Context, current, incoming apimodel.Agent, credentials apimodel.AgentCredentialIDs,
) error {
	if s.credentials == nil {
		return nil
	}
	return s.credentials.AllowServiceCredentials(ctx, current, incoming, credentials)
}
