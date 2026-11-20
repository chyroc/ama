package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
)

type credentialTestOAuthProvider struct{ id string }

func (p *credentialTestOAuthProvider) ID() string          { return p.id }
func (p *credentialTestOAuthProvider) Name() string        { return p.id }
func (p *credentialTestOAuthProvider) Description() string { return p.id }
func (p *credentialTestOAuthProvider) State(context.Context, string) IntegrationState {
	return IntegrationState{}
}
func (p *credentialTestOAuthProvider) AuthorizeURL(context.Context, string) (string, error) {
	return "", ErrAuthorizeUnsupported
}
func (p *credentialTestOAuthProvider) Revoke(context.Context, string) error {
	return ErrRevokeUnsupported
}

type credentialTestProvider struct {
	gotActor  string
	gotName   string
	gotPublic string
	gotSecret string
}

func (p *credentialTestProvider) ID() string          { return "example" }
func (p *credentialTestProvider) Name() string        { return "Example" }
func (p *credentialTestProvider) Description() string { return "Example provider" }
func (p *credentialTestProvider) State(context.Context, string) IntegrationState {
	return IntegrationState{}
}
func (p *credentialTestProvider) AuthorizeURL(context.Context, string) (string, error) {
	return "", ErrAuthorizeUnsupported
}
func (p *credentialTestProvider) Revoke(context.Context, string) error {
	return ErrRevokeUnsupported
}

func (p *credentialTestProvider) ListCredentials(context.Context, string) ([]apimodel.IntegrationCredentialView, error) {
	return []apimodel.IntegrationCredentialView{}, nil
}
func (p *credentialTestProvider) AddCredential(_ context.Context, actor, name, publicID, secret string) (apimodel.IntegrationCredentialView, error) {
	p.gotActor, p.gotName, p.gotPublic, p.gotSecret = actor, name, publicID, secret
	return apimodel.IntegrationCredentialView{ID: "cred_example", SecretMasked: "***"}, nil
}
func (p *credentialTestProvider) UpdateCredential(context.Context, string, string, string, string, string) (apimodel.IntegrationCredentialView, error) {
	return apimodel.IntegrationCredentialView{}, nil
}
func (p *credentialTestProvider) DeleteCredential(context.Context, string, string) error { return nil }
func (p *credentialTestProvider) CredentialMeta(context.Context, string) (apimodel.IntegrationCredentialView, error) {
	return apimodel.IntegrationCredentialView{ID: "cred_example", Actor: "owner@example.com"}, nil
}

func TestCredentialCollectionUsesOptionalProviderCapabilities(t *testing.T) {
	provider := &credentialTestProvider{}
	registry := NewIntegrationRegistry(nil)
	registry.Register(provider)
	ctx := auth.WithIdentity(context.Background(), auth.Identity{Email: "caller@example.com"})
	if _, err := registry.ListCredentials(ctx, provider.ID()); err != nil {
		t.Fatal(err)
	}
	view, err := registry.AddCredential(ctx, provider.ID(), "example", "public-example", "secret-example")
	if err != nil {
		t.Fatal(err)
	}
	if provider.gotActor != "caller@example.com" || provider.gotName != "example" ||
		provider.gotPublic != "public-example" || provider.gotSecret != "secret-example" {
		t.Fatalf("provider got incorrect arguments: %+v", provider)
	}
	if view.SecretMasked != "***" {
		t.Fatalf("registry returned an unmasked secret: %+v", view)
	}
	if _, err := registry.UpdateCredential(ctx, provider.ID(), "cred_example", "renamed", "public-example", "new-secret"); err != nil {
		t.Fatal(err)
	}
	if err := registry.DeleteCredential(ctx, provider.ID(), "cred_example"); err != nil {
		t.Fatal(err)
	}
	meta, err := registry.CredentialMeta(context.Background(), provider.ID(), "cred_example")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Actor != "owner@example.com" {
		t.Fatalf("binding metadata lost its owner: %+v", meta)
	}
}

func TestCredentialCollectionRejectsMissingIdentityAndCapability(t *testing.T) {
	provider := &credentialTestProvider{}
	registry := NewIntegrationRegistry(nil)
	registry.Register(provider)
	if _, err := registry.ListCredentials(context.Background(), provider.ID()); err == nil {
		t.Fatal("anonymous credential listing must be rejected")
	}
	ctx := auth.WithIdentity(context.Background(), auth.Identity{Email: "caller@example.com"})
	registry.Register(&credentialTestOAuthProvider{id: "oauth-only"})
	if _, err := registry.ListCredentials(ctx, "oauth-only"); !errors.Is(err, ErrCredentialUnsupported) {
		t.Fatalf("got %v, want unsupported capability", err)
	}
	if _, err := registry.AddCredential(ctx, "oauth-only", "name", "public", "secret"); !errors.Is(err, ErrCredentialUnsupported) {
		t.Fatalf("got %v, want unsupported capability", err)
	}
}

func TestCredentialCollectionDoesNotPutSecretInRegistryError(t *testing.T) {
	registry := NewIntegrationRegistry(nil)
	registry.Register(&credentialTestOAuthProvider{id: "oauth-only"})
	ctx := auth.WithIdentity(context.Background(), auth.Identity{Email: "caller@example.com"})
	_, err := registry.AddCredential(ctx, "oauth-only", "name", "public", "must-not-appear")
	if err == nil || strings.Contains(err.Error(), "must-not-appear") {
		t.Fatalf("registry error leaked secret: %v", err)
	}
}
