package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// ErrPersonalIntegrationNotShareable explains the safer alternative without naming a provider.
const ErrPersonalIntegrationNotShareable = "「%s」是个人授权，不能添加到空间级。" +
	"它的凭据代表你本人，注入容器后 agent 能做的事等于你能做的事，" +
	"绑到空间上等于把你的身份借给空间里所有人。" +
	"要让空间里的 agent 共用一份凭据，请改用代表应用的服务账号授权，" +
	"并在凭据提供方收窄它的权限。"

// WorkspaceIntegrationStore persists references and their unique key. Implementations
// must preserve the original ID and CreatedAt on a duplicate Put, and use a database
// unique constraint rather than a process-local check when several instances can write.
type WorkspaceIntegrationStore interface {
	ListWorkspaceIntegrations(context.Context, string) ([]persistmodel.WorkspaceIntegration, error)
	PutWorkspaceIntegration(context.Context, persistmodel.WorkspaceIntegration) error
	GetWorkspaceIntegration(context.Context, string) (persistmodel.WorkspaceIntegration, error)
	DeleteWorkspaceIntegration(context.Context, string) error
}

// WorkspaceIntegrationCredentialLookup returns secret-free live metadata. It must not
// return a saved name or status copied into the binding row.
type WorkspaceIntegrationCredentialLookup interface {
	CredentialMeta(context.Context, string, string) (apimodel.IntegrationCredentialView, error)
}

// WorkspaceIntegrationHooks supplies workspace existence, actor identity and provider names.
type WorkspaceIntegrationHooks struct {
	Store           WorkspaceIntegrationStore
	Credentials     WorkspaceIntegrationCredentialLookup
	WorkspaceExists func(context.Context, string) error
	Actor           func(context.Context) string
	ProviderName    func(string) string
	NewID           func() string
	Now             func() time.Time
}

// WorkspaceIntegrationService manages references to explicitly registered application
// credentials. It does not authorize HTTP callers or retrieve plaintext secrets.
type WorkspaceIntegrationService struct {
	hooks WorkspaceIntegrationHooks
}

func NewWorkspaceIntegrationService(hooks WorkspaceIntegrationHooks) *WorkspaceIntegrationService {
	return &WorkspaceIntegrationService{hooks: hooks}
}

func (s *WorkspaceIntegrationService) exists(ctx context.Context, id string) error {
	if s.hooks.WorkspaceExists == nil {
		return fmt.Errorf("%w: 服务端没有装配空间查询能力", ErrPrecondition)
	}
	if err := s.hooks.WorkspaceExists(ctx, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: 空间不存在", ErrNotFound)
		}
		return err
	}
	return nil
}

func (s *WorkspaceIntegrationService) providerName(id string) string {
	if s.hooks.ProviderName != nil {
		if name := s.hooks.ProviderName(id); name != "" {
			return name
		}
	}
	return id
}

// List returns every binding, including broken ones. One failed credential lookup does
// not hide other bindings; an unavailable lookup is not reported as a deleted credential.
func (s *WorkspaceIntegrationService) List(
	ctx context.Context, workspaceID string,
) ([]apimodel.WorkspaceIntegrationView, error) {
	if s.hooks.Store == nil {
		return nil, fmt.Errorf("%w: 服务端没有装配空间授权存储", ErrPrecondition)
	}
	if err := s.exists(ctx, workspaceID); err != nil {
		return nil, err
	}
	binds, err := s.hooks.Store.ListWorkspaceIntegrations(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := make([]apimodel.WorkspaceIntegrationView, 0, len(binds))
	for _, bind := range binds {
		out = append(out, s.view(ctx, bind))
	}
	return out, nil
}

func (s *WorkspaceIntegrationService) view(
	ctx context.Context, bind persistmodel.WorkspaceIntegration,
) apimodel.WorkspaceIntegrationView {
	view := apimodel.WorkspaceIntegrationView{
		ID: bind.ID, WorkspaceID: bind.WorkspaceID, ProviderID: bind.ProviderID,
		ProviderName: s.providerName(bind.ProviderID), CredentialID: bind.CredentialID,
		BoundBy: bind.BoundBy, CreatedAt: bind.CreatedAt.UTC().Format(persistmodel.ISOLayout),
		Available: true,
	}
	if strings.TrimSpace(bind.CredentialID) == "" {
		return view
	}
	if s.hooks.Credentials == nil {
		view.Available = false
		view.Unavailable = "服务端没有装配凭据查询能力，这次查不到这条凭据的状态"
		return view
	}
	meta, err := s.hooks.Credentials.CredentialMeta(ctx, bind.ProviderID, bind.CredentialID)
	switch {
	case errors.Is(err, ErrNotFound):
		view.Available = false
		view.Unavailable = "这条凭据已被它的主人删除。请让管理员移除这条绑定，或重新添加一条可用的凭据。"
		return view
	case err != nil:
		view.Available = false
		view.Unavailable = "这次没能查到这条凭据的状态（存储暂时不可用），不代表它已失效"
		return view
	}
	view.CredentialName = meta.Name
	view.CredentialPublicID = meta.PublicID
	view.CredentialOwner = meta.Actor
	return view
}

// Bind stores only provider and credential IDs. Unknown providers default to personal and
// are rejected. The database unique constraint decides concurrent duplicate writes; the
// subsequent read returns the existing ID rather than a newly generated, nonexistent one.
func (s *WorkspaceIntegrationService) Bind(
	ctx context.Context, workspaceID string, in apimodel.WorkspaceIntegrationInput,
) (apimodel.WorkspaceIntegrationView, error) {
	var zero apimodel.WorkspaceIntegrationView
	if s.hooks.Store == nil {
		return zero, fmt.Errorf("%w: 服务端没有装配空间授权存储", ErrPrecondition)
	}
	providerID := strings.TrimSpace(in.ProviderID)
	if providerID == "" {
		return zero, fmt.Errorf("%w: 授权源 id 不能为空", ErrInvalidArgument)
	}
	if !apimodel.IsServiceAccountIntegration(providerID) {
		return zero, fmt.Errorf("%w: "+ErrPersonalIntegrationNotShareable,
			ErrPrecondition, s.providerName(providerID))
	}
	if err := s.exists(ctx, workspaceID); err != nil {
		return zero, err
	}
	credentialID := strings.TrimSpace(in.CredentialID)
	if credentialID != "" {
		if s.hooks.Credentials == nil {
			return zero, fmt.Errorf("%w: 服务端没有装配凭据查询能力，暂时不能按凭据绑定", ErrPrecondition)
		}
		if _, err := s.hooks.Credentials.CredentialMeta(ctx, providerID, credentialID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return zero, fmt.Errorf("%w: 这条凭据不存在或已被删除，请重新选一条", ErrNotFound)
			}
			return zero, err
		}
	}
	now := time.Now()
	if s.hooks.Now != nil {
		now = s.hooks.Now()
	}
	now = now.UTC()
	id := ""
	if s.hooks.NewID != nil {
		id = s.hooks.NewID()
	}
	if id == "" {
		return zero, fmt.Errorf("%w: 服务端没有装配空间授权 ID 生成器", ErrPrecondition)
	}
	actor := ""
	if s.hooks.Actor != nil {
		actor = s.hooks.Actor(ctx)
	}
	if err := s.hooks.Store.PutWorkspaceIntegration(ctx, persistmodel.WorkspaceIntegration{
		ID: id, WorkspaceID: workspaceID, ProviderID: providerID,
		CredentialID: credentialID, BoundBy: actor, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		return zero, err
	}
	binds, err := s.hooks.Store.ListWorkspaceIntegrations(ctx, workspaceID)
	if err != nil {
		return zero, err
	}
	for _, bind := range binds {
		if bind.ProviderID == providerID && bind.CredentialID == credentialID {
			return s.view(ctx, bind), nil
		}
	}
	return zero, fmt.Errorf("core: 绑定后回读不到，空间 %s 授权源 %s", workspaceID, providerID)
}

// Unbind removes only the reference. An ID from another workspace looks absent rather
// than revealing its existence with a permission error. Concurrent duplicate deletes are OK.
func (s *WorkspaceIntegrationService) Unbind(ctx context.Context, workspaceID, id string) error {
	if s.hooks.Store == nil {
		return fmt.Errorf("%w: 服务端没有装配空间授权存储", ErrPrecondition)
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: 绑定 id 不能为空", ErrInvalidArgument)
	}
	bind, err := s.hooks.Store.GetWorkspaceIntegration(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if bind.WorkspaceID != workspaceID {
		return fmt.Errorf("%w: 这条绑定不存在", ErrNotFound)
	}
	return s.hooks.Store.DeleteWorkspaceIntegration(ctx, id)
}
