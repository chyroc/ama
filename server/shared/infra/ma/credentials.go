// Credentials: the actual secrets / OAuth grants stored in a Vault.
// API: /api/v3/vaults/{vault_id}/credentials
//
// The Auth field is a union (API Key, OAuth and other shapes), and the shapes are built very
// differently, so there is no convenience wrapper here; callers use the SDK request body directly.

package ma

import (
	"context"

	"github.com/volcengine/ark-runtime-go/arkruntime/model/vault"
)

// CreateCredential creates a credential in the given vault.
//
// Build Auth with the SDK's union constructors, e.g. vault.NewAPIKeyAuthCredentialAuthSum(...);
// see the SDK's vault package for the available shapes.
func (s *VaultService) CreateCredential(
	ctx context.Context,
	vaultID string,
	req *vault.CreateCredentialRequest,
) (*vault.Credential, error) {
	return s.client.CreateCredential(ctx, vaultID, req)
}

// GetCredential returns credential details. The result does not contain the secret in plain text.
func (s *VaultService) GetCredential(
	ctx context.Context,
	vaultID, credentialID string,
) (*vault.Credential, error) {
	return s.client.GetCredential(ctx, vaultID, credentialID)
}

// ListCredentials lists the credentials in a vault, page by page.
func (s *VaultService) ListCredentials(
	ctx context.Context,
	vaultID string,
	page Page,
) (*vault.ListCredentialsResponse, error) {
	return s.ListCredentialsRaw(ctx, vaultID, &vault.CredentialsListParams{
		VaultId: vaultID,
		Limit:   optVaultInt32(page.Limit),
		Page:    optVaultString(page.Page),
	})
}

// ListCredentialsRaw lists credentials using the SDK's own query parameters.
func (s *VaultService) ListCredentialsRaw(
	ctx context.Context,
	vaultID string,
	params *vault.CredentialsListParams,
) (*vault.ListCredentialsResponse, error) {
	return s.client.ListCredentials(ctx, vaultID, params)
}

// UpdateCredential updates a credential (replace the secret or change the display name).
func (s *VaultService) UpdateCredential(
	ctx context.Context,
	vaultID, credentialID string,
	req *vault.UpdateCredentialRequest,
) (*vault.Credential, error) {
	return s.client.UpdateCredential(ctx, vaultID, credentialID, req)
}

// DeleteCredential deletes a credential.
func (s *VaultService) DeleteCredential(
	ctx context.Context,
	vaultID, credentialID string,
) (*vault.DeleteCredentialResponse, error) {
	return s.client.DeleteCredential(ctx, vaultID, credentialID)
}

// ValidateCredential checks whether a credential is still valid (e.g. whether an MCP OAuth grant
// has expired).
func (s *VaultService) ValidateCredential(
	ctx context.Context,
	vaultID, credentialID string,
) (*vault.CredentialValidation, error) {
	return s.client.ValidateCredential(ctx, vaultID, credentialID)
}
