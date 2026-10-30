// Vault: a credential vault.
// API: /api/v3/vaults
//
// A Vault lets an Agent call third-party services as the user (e.g. open a PR with the user's
// GitHub token). The platform injects the secret; it never enters the sandbox process. For the
// credentials themselves see credentials.go.

package ma

import (
	"context"
	"errors"

	"github.com/volcengine/ark-runtime-go/arkruntime"
	"github.com/volcengine/ark-runtime-go/arkruntime/model/vault"
)

// VaultService manages vaults and the credentials inside them. Get it via Instance.Vaults().
type VaultService struct {
	client *arkruntime.Client
}

// Create creates a vault. displayName is a display name of 1–255 characters.
func (s *VaultService) Create(ctx context.Context, displayName string) (*vault.Vault, error) {
	if displayName == "" {
		return nil, errors.New("ma: displayName 必填")
	}
	return s.CreateRaw(ctx, &vault.CreateVaultRequest{DisplayName: displayName})
}

// CreateRaw creates a vault using the SDK's own request body (metadata allowed).
func (s *VaultService) CreateRaw(
	ctx context.Context,
	req *vault.CreateVaultRequest,
) (*vault.Vault, error) {
	return s.client.CreateVault(ctx, req)
}

// Get returns vault details by ID.
func (s *VaultService) Get(ctx context.Context, vaultID string) (*vault.Vault, error) {
	return s.client.GetVault(ctx, vaultID)
}

// List lists vaults, page by page. Only Limit / Page are supported.
func (s *VaultService) List(ctx context.Context, page Page) (*vault.ListVaultsResponse, error) {
	return s.ListRaw(ctx, &vault.VaultsListParams{
		Limit: optVaultInt32(page.Limit),
		Page:  optVaultString(page.Page),
	})
}

// ListRaw paginates using the SDK's own query parameters.
func (s *VaultService) ListRaw(
	ctx context.Context,
	params *vault.VaultsListParams,
) (*vault.ListVaultsResponse, error) {
	return s.client.ListVaults(ctx, params)
}

// Update changes the vault's display name.
func (s *VaultService) Update(
	ctx context.Context,
	vaultID, displayName string,
) (*vault.Vault, error) {
	return s.UpdateRaw(ctx, vaultID, &vault.UpdateVaultRequest{
		DisplayName: optVaultString(displayName),
	})
}

// UpdateRaw updates a vault using the SDK's own request body.
func (s *VaultService) UpdateRaw(
	ctx context.Context,
	vaultID string,
	req *vault.UpdateVaultRequest,
) (*vault.Vault, error) {
	return s.client.UpdateVault(ctx, vaultID, req)
}

// Delete deletes a vault together with all credentials in it.
func (s *VaultService) Delete(
	ctx context.Context,
	vaultID string,
) (*vault.DeleteVaultResponse, error) {
	return s.client.DeleteVault(ctx, vaultID)
}
