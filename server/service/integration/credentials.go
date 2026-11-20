package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// ErrCredentialUnsupported means a provider has no manually managed credential collection.
// It is different from OAuth/device authorization, which does not accept a pasted secret.
var ErrCredentialUnsupported = errors.New("这个授权源不接受手工填写的凭据")

// Optional interfaces for providers that manage a collection of named credentials. They stay
// separate from IntegrationProvider: OAuth-only providers do not need unsupported stubs.
type integrationCredentialLister interface {
	ListCredentials(ctx context.Context, actor string) ([]apimodel.IntegrationCredentialView, error)
}

type integrationCredentialAdder interface {
	AddCredential(ctx context.Context, actor, name, publicID, secret string) (apimodel.IntegrationCredentialView, error)
}

type integrationCredentialDeleter interface {
	DeleteCredential(ctx context.Context, actor, credentialID string) error
}

type integrationCredentialUpdater interface {
	UpdateCredential(
		ctx context.Context, actor, credentialID, name, publicID, secret string,
	) (apimodel.IntegrationCredentialView, error)
}

type integrationCredentialMetaReader interface {
	CredentialMeta(ctx context.Context, credentialID string) (apimodel.IntegrationCredentialView, error)
}

// ListCredentials lists secret-free credential metadata for the current actor.
func (r *IntegrationRegistry) ListCredentials(
	ctx context.Context, id string,
) ([]apimodel.IntegrationCredentialView, error) {
	actor := strings.TrimSpace(auth.ActorFrom(ctx))
	if actor == "" {
		return nil, fmt.Errorf("%w: 需要先登录", persistmodel.ErrInvalidArgument)
	}
	p, err := r.Get(id)
	if err != nil {
		return nil, err
	}
	lister, ok := p.(integrationCredentialLister)
	if !ok {
		return nil, fmt.Errorf("%w: 授权源 %q 不支持多条凭据", ErrCredentialUnsupported, id)
	}
	return lister.ListCredentials(ctx, actor)
}

// AddCredential validates and stores a new credential through the provider. The secret is passed
// only to that provider and is never included in registry errors or logs.
func (r *IntegrationRegistry) AddCredential(
	ctx context.Context, id, name, publicID, secret string,
) (apimodel.IntegrationCredentialView, error) {
	var zero apimodel.IntegrationCredentialView
	actor := strings.TrimSpace(auth.ActorFrom(ctx))
	if actor == "" {
		return zero, fmt.Errorf("%w: 需要先登录", persistmodel.ErrInvalidArgument)
	}
	p, err := r.Get(id)
	if err != nil {
		return zero, err
	}
	adder, ok := p.(integrationCredentialAdder)
	if !ok {
		return zero, fmt.Errorf("%w: 授权源 %q 不支持多条凭据", ErrCredentialUnsupported, id)
	}
	return adder.AddCredential(ctx, actor, name, publicID, secret)
}

// UpdateCredential changes the display name or secret, not the public identity of an existing
// credential. A binding must not silently start referring to another account.
func (r *IntegrationRegistry) UpdateCredential(
	ctx context.Context, id, credentialID, name, publicID, secret string,
) (apimodel.IntegrationCredentialView, error) {
	var zero apimodel.IntegrationCredentialView
	actor := strings.TrimSpace(auth.ActorFrom(ctx))
	if actor == "" {
		return zero, fmt.Errorf("%w: 需要先登录", persistmodel.ErrInvalidArgument)
	}
	p, err := r.Get(id)
	if err != nil {
		return zero, err
	}
	updater, ok := p.(integrationCredentialUpdater)
	if !ok {
		return zero, fmt.Errorf("%w: 授权源 %q 不支持就地修改凭据（删掉重加即可）",
			ErrCredentialUnsupported, id)
	}
	return updater.UpdateCredential(ctx, actor, credentialID, name, publicID, secret)
}

// DeleteCredential removes one of the current actor's credentials. Providers must return
// not-found rather than access-denied when the id belongs to another actor, so it is not disclosed.
func (r *IntegrationRegistry) DeleteCredential(ctx context.Context, id, credentialID string) error {
	actor := strings.TrimSpace(auth.ActorFrom(ctx))
	if actor == "" {
		return fmt.Errorf("%w: 需要先登录", persistmodel.ErrInvalidArgument)
	}
	p, err := r.Get(id)
	if err != nil {
		return err
	}
	deleter, ok := p.(integrationCredentialDeleter)
	if !ok {
		return fmt.Errorf("%w: 授权源 %q 不支持多条凭据", ErrCredentialUnsupported, id)
	}
	return deleter.DeleteCredential(ctx, actor, credentialID)
}

// CredentialMeta returns secret-free metadata for a binding reference. It deliberately does not
// require the caller to own the credential: workspace administrators may bind a colleague's
// read-only account. The provider decides whether the reference exists.
func (r *IntegrationRegistry) CredentialMeta(
	ctx context.Context, providerID, credentialID string,
) (apimodel.IntegrationCredentialView, error) {
	var zero apimodel.IntegrationCredentialView
	p, err := r.Get(providerID)
	if err != nil {
		return zero, err
	}
	reader, ok := p.(integrationCredentialMetaReader)
	if !ok {
		return zero, fmt.Errorf("%w: 授权源 %q 不支持多条凭据", ErrCredentialUnsupported, providerID)
	}
	return reader.CredentialMeta(ctx, credentialID)
}
