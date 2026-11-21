package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

type workspaceIntegrationTestStore struct {
	mu    sync.Mutex
	binds map[string]persistmodel.WorkspaceIntegration
}

func (s *workspaceIntegrationTestStore) ListWorkspaceIntegrations(
	_ context.Context, workspaceID string,
) ([]persistmodel.WorkspaceIntegration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]persistmodel.WorkspaceIntegration, 0)
	for _, bind := range s.binds {
		if bind.WorkspaceID == workspaceID {
			out = append(out, bind)
		}
	}
	return out, nil
}

func (s *workspaceIntegrationTestStore) PutWorkspaceIntegration(
	_ context.Context, bind persistmodel.WorkspaceIntegration,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.binds == nil {
		s.binds = map[string]persistmodel.WorkspaceIntegration{}
	}
	for id, old := range s.binds {
		if old.WorkspaceID == bind.WorkspaceID && old.ProviderID == bind.ProviderID &&
			old.CredentialID == bind.CredentialID {
			old.UpdatedAt = bind.UpdatedAt
			s.binds[id] = old
			return nil
		}
	}
	s.binds[bind.ID] = bind
	return nil
}

func (s *workspaceIntegrationTestStore) GetWorkspaceIntegration(
	_ context.Context, id string,
) (persistmodel.WorkspaceIntegration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bind, ok := s.binds[id]
	if !ok {
		return persistmodel.WorkspaceIntegration{}, ErrNotFound
	}
	return bind, nil
}

func (s *workspaceIntegrationTestStore) DeleteWorkspaceIntegration(
	_ context.Context, id string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.binds, id)
	return nil
}

type workspaceIntegrationTestCredentials struct {
	name string
	err  error
}

func (c *workspaceIntegrationTestCredentials) CredentialMeta(
	context.Context, string, string,
) (apimodel.IntegrationCredentialView, error) {
	return apimodel.IntegrationCredentialView{
		Name: c.name, PublicID: "public_example", Actor: "owner@example.com",
	}, c.err
}

func newWorkspaceIntegrationTestService(
	store *workspaceIntegrationTestStore, credentials WorkspaceIntegrationCredentialLookup,
) *WorkspaceIntegrationService {
	ids := 0
	return NewWorkspaceIntegrationService(WorkspaceIntegrationHooks{
		Store: store, Credentials: credentials,
		WorkspaceExists: func(context.Context, string) error { return nil },
		Actor:           func(context.Context) string { return "admin@example.com" },
		NewID: func() string {
			ids++
			return "binding_test_" + time.Unix(int64(ids), 0).Format("150405")
		},
	})
}

func TestWorkspaceIntegrationRejectsPersonalAndUnknownProviders(t *testing.T) {
	svc := newWorkspaceIntegrationTestService(&workspaceIntegrationTestStore{}, nil)
	for _, provider := range []string{apimodel.IntegrationAMA, "unknown_example"} {
		if _, err := svc.Bind(context.Background(), "ws_example", apimodel.WorkspaceIntegrationInput{
			ProviderID: provider,
		}); !errors.Is(err, ErrPrecondition) {
			t.Errorf("provider %q: error = %v, want precondition rejection", provider, err)
		}
	}
}

func TestWorkspaceIntegrationReadsLiveMetadataAndKeepsBrokenReference(t *testing.T) {
	credentials := &workspaceIntegrationTestCredentials{name: "first name"}
	svc := newWorkspaceIntegrationTestService(&workspaceIntegrationTestStore{}, credentials)
	bound, err := svc.Bind(context.Background(), "ws_example", apimodel.WorkspaceIntegrationInput{
		ProviderID: apimodel.IntegrationServiceAccount, CredentialID: "cred_example",
	})
	if err != nil {
		t.Fatal(err)
	}
	credentials.name = "renamed globally"
	list, err := svc.List(context.Background(), "ws_example")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].CredentialName != "renamed globally" {
		t.Fatalf("List = %#v, want live renamed credential", list)
	}
	credentials.err = ErrNotFound
	list, err = svc.List(context.Background(), "ws_example")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != bound.ID || list[0].Available {
		t.Fatalf("deleted credential must remain a visible broken reference: %#v", list)
	}
	credentials.err = errors.New("temporary read failure")
	list, err = svc.List(context.Background(), "ws_example")
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Unavailable == "" || list[0].Unavailable == "这条凭据已被它的主人删除。请让管理员移除这条绑定，或重新添加一条可用的凭据。" {
		t.Fatalf("temporary failure was reported as deleted: %#v", list[0])
	}
}

func TestWorkspaceIntegrationDuplicateReadReturnsExistingID(t *testing.T) {
	svc := newWorkspaceIntegrationTestService(&workspaceIntegrationTestStore{},
		&workspaceIntegrationTestCredentials{})
	input := apimodel.WorkspaceIntegrationInput{ProviderID: apimodel.IntegrationCodebaseApp}
	first, err := svc.Bind(context.Background(), "ws_example", input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Bind(context.Background(), "ws_example", input)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.CreatedAt != first.CreatedAt {
		t.Fatalf("duplicate binding returned a new ID or creation time: first=%#v second=%#v", first, second)
	}
}

func TestWorkspaceIntegrationUnbindIsScoped(t *testing.T) {
	store := &workspaceIntegrationTestStore{}
	svc := newWorkspaceIntegrationTestService(store, nil)
	bound, err := svc.Bind(context.Background(), "ws_example", apimodel.WorkspaceIntegrationInput{
		ProviderID: apimodel.IntegrationServiceAccount,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Unbind(context.Background(), "another_workspace", bound.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace delete error = %v, want not found", err)
	}
	if err := svc.Unbind(context.Background(), "ws_example", bound.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Unbind(context.Background(), "ws_example", bound.ID); err != nil {
		t.Fatalf("duplicate delete should be harmless: %v", err)
	}
}
