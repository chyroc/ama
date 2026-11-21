package apimodel

// WorkspaceIntegrationInput adds a reference to a named application credential.
// Personal credentials cannot be shared through this type.
type WorkspaceIntegrationInput struct {
	ProviderID   string
	CredentialID string
}

// WorkspaceIntegrationView contains live credential metadata, never a saved copy or secret.
// A deleted credential leaves a visible broken reference instead of a stale, apparently usable row.
type WorkspaceIntegrationView struct {
	ID                 string `json:"id"`
	WorkspaceID        string `json:"workspaceId"`
	ProviderID         string `json:"providerId"`
	ProviderName       string `json:"providerName"`
	CredentialID       string `json:"credentialId,omitempty"`
	CredentialName     string `json:"credentialName,omitempty"`
	CredentialPublicID string `json:"credentialPublicId,omitempty"`
	CredentialOwner    string `json:"credentialOwner,omitempty"`
	Available          bool   `json:"available"`
	Unavailable        string `json:"unavailable,omitempty"`
	BoundBy            string `json:"boundBy,omitempty"`
	CreatedAt          string `json:"createdAt"`
}
