package apimodel

// IntegrationCredentialView is the public, secret-free view of one named credential in a
// provider's collection. It is deliberately separate from the stored credential, which may
// contain a plaintext secret. Never add a plaintext secret field to this type.
type IntegrationCredentialView struct {
	// ID is the opaque credential identifier used by agent and workspace bindings.
	ID string `json:"id"`
	// Name is the human label distinguishing multiple credentials of one provider.
	Name string `json:"name"`
	// PublicID identifies the account or application publicly. It is stable after creation;
	// changing the secret must not silently turn an existing binding into another identity.
	PublicID string `json:"publicId"`
	// SecretMasked is the precomputed display mask, never the plaintext secret.
	SecretMasked string `json:"secretMasked"`
	// Status is one of CredentialStatusOK / Limited / Error / Unknown.
	Status string `json:"status"`
	// StatusReason is a secret-free explanation for a non-ok status.
	StatusReason string `json:"statusReason,omitempty"`
	// CreatedAt and LastVerifiedAt use the project's millisecond ISO timestamp format.
	// LastVerifiedAt is empty when no verification has succeeded; do not emit a zero time.
	CreatedAt      string `json:"createdAt"`
	LastVerifiedAt string `json:"lastVerifiedAt,omitempty"`
	// Actor is the owner who configured the credential. Workspace bindings may legitimately
	// refer to a credential owned by somebody else, so consumers need this attribution.
	Actor string `json:"actor,omitempty"`
}

// Credential verification statuses. Limited means the credential is valid but its permission
// scope does not cover the probe resource; it is not an invalid credential. Unknown means the
// check could not complete and must not be presented as proof that the credential is broken.
const (
	CredentialStatusOK      = "ok"
	CredentialStatusLimited = "limited"
	CredentialStatusError   = "error"
	CredentialStatusUnknown = "unknown"
)
