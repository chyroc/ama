package apimodel

// AgentCredentialIDs maps a provider id to the selected named credential id.
// Values are references only; plaintext credentials never belong in Agent or AgentDraft.
type AgentCredentialIDs map[string]string

// These keys identify provider slots used for optional named service credentials.
// Downstream registries may support other provider ids through the same map.
const (
	IntegrationCodebaseApp    = "codebase-app"
	IntegrationServiceAccount = "service-account"
)
