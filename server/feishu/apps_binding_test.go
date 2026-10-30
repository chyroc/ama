// Behavior of the app-agent binding on the Feishu side.
//
// Two things, both of the "no error, just the wrong number on the page" kind:
//
//   - BoundAgentIDs is a derived field, computed on read. Before the change it was always [] in every response,
//     so the delete confirmation's "N agents are bound to it" always showed 0.
//   - Disconnecting an app must also clear the bindings on the agent side, or dangling references are left.

package feishu

import (
	"context"
	"errors"
	"testing"
)

// fakeUnbinder is a fake implementation of the core side.
//
// It only records "who is bound to whom", enough for these two tests. The source of truth for bindings is in core; this package only reads it.
type fakeUnbinder struct {
	// bound is feishuAppID → agentIDs.
	bound map[string][]string
	// boundErr makes AgentsBoundToFeishuApp return an error.
	boundErr error
	// unbound records the IDs of apps that were unbound.
	unbound []string
}

func (f *fakeUnbinder) UnbindFeishuApp(_ context.Context, feishuAppID string) error {
	f.unbound = append(f.unbound, feishuAppID)
	delete(f.bound, feishuAppID)
	return nil
}

func (f *fakeUnbinder) AgentsBoundToFeishuApp(
	_ context.Context, feishuAppID string,
) ([]string, error) {
	if f.boundErr != nil {
		return nil, f.boundErr
	}
	return f.bound[feishuAppID], nil
}

// newBindingService sets up a Service with an Unbinder, and pre-fills a connected app.
func newBindingService(t *testing.T, unbinder *fakeUnbinder) (*Service, *App) {
	t.Helper()
	store := NewMemStore()
	app := &App{
		ID:          "fsa_1",
		WorkspaceID: "ws_1",
		AppID:       "cli_1",
		Name:        "测试应用",
		Status:      StatusConnected,
	}
	if _, err := store.CreateApp(context.Background(), app, "secret"); err != nil {
		t.Fatalf("预置应用失败: %v", err)
	}
	svc, err := New(Config{Store: store, Unbinder: unbinder, Authorizer: allowAuthorizer{}})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return svc, app
}

// TestGetAppFillsBoundAgents: BoundAgentIDs is computed on the fly when reading an app.
//
// Before the change this field was always [] (Service had no dependency on core at all), so the frontend's delete confirmation
// always showed "0 agents bound to it", and users thought it was safe to delete.
func TestGetAppFillsBoundAgents(t *testing.T) {
	ctx := context.Background()
	unbinder := &fakeUnbinder{bound: map[string][]string{"fsa_1": {"agent_owner"}}}
	svc, _ := newBindingService(t, unbinder)

	got, err := svc.GetApp(ctx, "somebody@example.com", "fsa_1")
	if err != nil {
		t.Fatalf("取应用失败: %v", err)
	}
	if len(got.BoundAgentIDs) != 1 || got.BoundAgentIDs[0] != "agent_owner" {
		t.Errorf("BoundAgentIDs 该现算出 [agent_owner]，实际 %v", got.BoundAgentIDs)
	}
}

// TestBoundAgentsNeverPersisted: the derived field must not be stored.
//
// BoundAgentIDs lives in the App struct and is written back along with doc by SaveApp, so "fill it on read"
// and "do not store it" must both hold. Storing it would give the bindings a second copy in the store,
// updated only after some read, and from then on out of sync with the source of truth. The symptom is exactly what this change fixes,
// "the page shows it bound, but it is not". core/ark.go noted the same pitfall for Run.AgentDeleted.
func TestBoundAgentsNeverPersisted(t *testing.T) {
	ctx := context.Background()
	unbinder := &fakeUnbinder{bound: map[string][]string{"fsa_1": {"agent_owner"}}}
	svc, _ := newBindingService(t, unbinder)

	// Read once to trigger filling of the derived field.
	if _, err := svc.GetApp(ctx, "somebody@example.com", "fsa_1"); err != nil {
		t.Fatalf("取应用失败: %v", err)
	}

	// Ask Store for that record directly, bypassing Service, to see what is really stored.
	raw, err := svc.store.GetApp(ctx, "fsa_1")
	if err != nil {
		t.Fatalf("取库里的应用失败: %v", err)
	}
	if len(raw.BoundAgentIDs) != 0 {
		t.Errorf("BoundAgentIDs 是派生字段，绝不该落库，实际库里存着 %v", raw.BoundAgentIDs)
	}
}

// TestDeleteAppUnbindsAgents: disconnecting an app must also clear the bindings on the agent side.
//
// Without clearing, dangling feishuAppIds are left: the app is gone, but the agent detail page still shows "1 bound",
// and that binding points to an ID that cannot be found. The frontend LocalApiClient always did this cleanup; the server missed it.
func TestDeleteAppUnbindsAgents(t *testing.T) {
	ctx := context.Background()
	unbinder := &fakeUnbinder{bound: map[string][]string{"fsa_1": {"agent_owner"}}}
	svc, _ := newBindingService(t, unbinder)

	if err := svc.DeleteApp(ctx, "somebody@example.com", "fsa_1"); err != nil {
		t.Fatalf("断开应用失败: %v", err)
	}

	if len(unbinder.unbound) != 1 || unbinder.unbound[0] != "fsa_1" {
		t.Errorf("断开应用该解绑 fsa_1，实际解绑了 %v", unbinder.unbound)
	}
	// The app itself is really gone too.
	if _, err := svc.store.GetApp(ctx, "fsa_1"); err == nil {
		t.Error("应用该已被删除")
	}
}

// TestDeleteAppSucceedsWithoutUnbinder: disconnecting works as usual when Unbinder is nil.
//
// Many tests only care about the Feishu side and do not wire up core; they should not be forced to provide an Unbinder.
func TestDeleteAppSucceedsWithoutUnbinder(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	app := &App{ID: "fsa_1", WorkspaceID: "ws_1", AppID: "cli_1", Name: "测试应用"}
	if _, err := store.CreateApp(ctx, app, "secret"); err != nil {
		t.Fatalf("预置应用失败: %v", err)
	}
	// No Unbinder passed.
	svc, err := New(Config{Store: store, Authorizer: allowAuthorizer{}})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	if err := svc.DeleteApp(ctx, "somebody@example.com", "fsa_1"); err != nil {
		t.Fatalf("没有 Unbinder 时断开也该成功: %v", err)
	}
	if _, err := svc.store.GetApp(ctx, "fsa_1"); err == nil {
		t.Error("应用该已被删除")
	}
}

// TestListAppsToleratesBoundLookupFailure: failing to look up bindings should not stop the list page from opening.
//
// Missing a reverse reference only makes the number in the delete confirmation wrong, while a list that cannot open is a blank page.
// This matches how readiness check failures are handled (better one diagnosis fewer).
func TestListAppsToleratesBoundLookupFailure(t *testing.T) {
	ctx := context.Background()
	unbinder := &fakeUnbinder{boundErr: errors.New("core 挂了")}
	svc, _ := newBindingService(t, unbinder)

	apps, err := svc.ListApps(ctx, ListOption{
		ActorID:     "somebody@example.com",
		WorkspaceID: "ws_1",
	})
	if err != nil {
		t.Fatalf("反查绑定失败不该让列表页打不开: %v", err)
	}
	if len(apps) != 1 {
		t.Fatalf("该列出 1 个应用，实际 %d 个", len(apps))
	}
	// The contract requires this field to be [], not null; the frontend reads .length directly.
	if apps[0].BoundAgentIDs == nil {
		t.Error("BoundAgentIDs 不能是 nil（会编码成 null，前端读 .length 会炸）")
	}
}
