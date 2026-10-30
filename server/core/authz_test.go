// Tests for workspace isolation and role lookup.
//
// These two are the **data side** of authorization: Guard decides "is the level high enough"
// using RoleOf in this file; the workspace list not leaking other people's workspaces relies on
// ListWorkspacesFor.
//
// The easiest thing to break is the split between ListWorkspaces and ListWorkspacesFor: if the
// former also filters, the worker pool sees 0 workspaces, and then every agent hangs as soon as
// it calls a tool, **with no error**. So there is a test pinning that too.

package core

import (
	"context"
	"errors"
	"testing"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
)

// The four members of seed workspace ws_demo, one per role.
const (
	seedOwner  = "alice@example.com"
	seedAdmin  = "bob@example.com"
	seedMember = "carol@example.com"
	seedViewer = "dave@example.com"
	outsider   = "nobody@example.com"
)

// TestListWorkspacesForFiltersByMembership: the workspace list only returns workspaces you are in.
//
// This is the "data scope isolation" half: without it, any logged-in user can see the names and
// members of all workspaces, and nothing on the page looks wrong; there are just a few workspaces
// that should not be there.
func TestListWorkspacesForFiltersByMembership(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	// Create one more workspace with only outsider, so both sides have "visible" and "not visible" ones.
	other, err := svc.CreateWorkspace(
		auth.WithIdentity(ctx, auth.Identity{Email: outsider, Name: "路人"}),
		WorkspaceInput{Name: "别人的空间", Key: "other-space"})
	if err != nil {
		t.Fatalf("建空间失败：%v", err)
	}

	cases := []struct {
		name  string
		actor string
		want  []string // workspace IDs expected to be visible
	}{
		{"种子 owner 只看到种子空间", seedOwner, []string{seedWS}},
		{"种子 viewer 也能看到（只读不影响可见性）", seedViewer, []string{seedWS}},
		{"路人只看到自己建的", outsider, []string{other.ID}},
		{"谁都不是的人什么都看不到", "ghost@example.com", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			list, err := svc.ListWorkspacesFor(ctx, tc.actor)
			if err != nil {
				t.Fatalf("列空间失败：%v", err)
			}
			if len(list) != len(tc.want) {
				t.Fatalf("看到 %d 个空间，期望 %d 个：%+v", len(list), len(tc.want), ids(list))
			}
			for i, id := range tc.want {
				if list[i].ID != id {
					t.Errorf("第 %d 个是 %q，期望 %q", i, list[i].ID, id)
				}
			}
		})
	}
}

// TestListWorkspacesForEmptyActor: an empty identity returns an empty list (fail-closed), not
// everything.
//
// Reaching here with no identity means some endpoint is missing authentication. Return an empty
// list then, not everything; the other way around turns an auth bug directly into a data leak.
func TestListWorkspacesForEmptyActor(t *testing.T) {
	svc, _ := newService(t)
	list, err := svc.ListWorkspacesFor(context.Background(), "")
	if err != nil {
		t.Fatalf("列空间失败：%v", err)
	}
	if len(list) != 0 {
		t.Fatalf("空身份竟然看到 %d 个空间，必须是 0", len(list))
	}
	// And it must be a non-nil empty slice: nil encodes as null, and the contract requires [].
	if list == nil {
		t.Error("返回了 nil，会编码成 null，契约要求 []")
	}
}

// TestListWorkspacesStaysUnfiltered: the original method **must still return everything**.
//
// The worker pool patrol in core/worker.go uses it to scan Ark bindings of all workspaces. That
// path runs on a process-level ctx with no identity. If this also filtered by identity, it would
// see 0 workspaces → every agent gets stuck in requires_action as soon as it calls a tool, and
// the logs show nothing.
//
// **Before changing this, think through what happens to the worker pool.**
func TestListWorkspacesStaysUnfiltered(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	if _, err := svc.CreateWorkspace(
		auth.WithIdentity(ctx, auth.Identity{Email: outsider}),
		WorkspaceInput{Name: "别人的空间", Key: "other-space"}); err != nil {
		t.Fatalf("建空间失败：%v", err)
	}

	// Without even an identity, both workspaces must still be visible.
	all, err := svc.workspaces.ListWorkspaces(ctx)
	if err != nil {
		t.Fatalf("列空间失败：%v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListWorkspaces 返回 %d 个，期望 2 个（它不该过滤）：%+v", len(all), ids(all))
	}
}

// TestRoleOf looks up roles: one of each of the four roles; non-members and missing workspaces are both false.
func TestRoleOf(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	cases := []struct {
		name     string
		ws       string
		actor    string
		wantRole apimodel.WorkspaceRole
		wantOK   bool
	}{
		{"owner", seedWS, seedOwner, apimodel.RoleOwner, true},
		{"admin", seedWS, seedAdmin, apimodel.RoleAdmin, true},
		{"member", seedWS, seedMember, apimodel.RoleMember, true},
		{"viewer", seedWS, seedViewer, apimodel.RoleViewer, true},
		{"非成员", seedWS, outsider, "", false},
		{"空身份", seedWS, "", "", false},
		// A missing workspace and "you are not a member" give the same authorization answer, and
		// **must not return an error**: Guard maps errors to 500, and this is just a deleted workspace.
		{"空间不存在", "ws_gone", seedOwner, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			role, ok, err := svc.RoleOf(ctx, tc.ws, tc.actor)
			if err != nil {
				t.Fatalf("查角色报错：%v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("isMember = %v，期望 %v", ok, tc.wantOK)
			}
			if role != tc.wantRole {
				t.Errorf("角色 = %q，期望 %q", role, tc.wantRole)
			}
		})
	}
}

// TestRoleOfIsCaseInsensitive: member emails are typed by hand and may contain uppercase; login
// identities are lowercased.
//
// A case-sensitive comparison would mean that person can never find their own role. It shows up
// as "I am clearly a member but it says I am not", and only for the few people with uppercase
// letters in their email, which is very hard to track down.
//
// ⚠️ Note that AddMember's uniqueness check is **still case-sensitive** (copied from
// LocalApiClient). The two places differ on purpose; see the comment on memberOf for why.
func TestRoleOfIsCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	// An admin typed an email with uppercase letters.
	if _, err := svc.AddMember(ctx, seedWS, MemberInput{
		Name: "大写", Email: "MiXeD.Case@Example.com", Role: apimodel.RoleMember,
	}); err != nil {
		t.Fatalf("加成员失败：%v", err)
	}

	// The login identity is in lowercased form (the output of auth.normalizeEmail).
	role, ok, err := svc.RoleOf(ctx, seedWS, "mixed.case@example.com")
	if err != nil {
		t.Fatalf("查角色报错：%v", err)
	}
	if !ok {
		t.Fatal("大小写不同就查不到成员了——那个人会被永久拒在门外")
	}
	if role != apimodel.RoleMember {
		t.Errorf("角色 = %q，期望 member", role)
	}
}

// TestGetWorkspaceByKeyHidesNonMemberSpace: a non-member looking up a workspace by key must get
// "does not exist".
//
// The authorization for this one is **written in Service** (the only place in the whole design):
// its workspace comes from a query parameter, and Guard declares permissions by pattern, which
// cannot express that.
//
// Returning ErrNotFound instead of ErrAccessDenied is on purpose: the frontend WorkspaceGate uses
// nullable() to turn 404 into null → back to the workspace list, which is existing behavior; it
// also avoids leaking "this key exists but you cannot get in".
func TestGetWorkspaceByKeyHidesNonMemberSpace(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	// A member can find it.
	memberCtx := auth.WithIdentity(ctx, auth.Identity{Email: seedViewer})
	if _, err := svc.GetWorkspaceByKey(memberCtx, "growth"); err != nil {
		t.Fatalf("成员查自己的空间竟然失败：%v", err)
	}

	// A non-member looking up the same key must get "workspace does not exist".
	outCtx := auth.WithIdentity(ctx, auth.Identity{Email: outsider})
	_, err := svc.GetWorkspaceByKey(outCtx, "growth")
	if err == nil {
		t.Fatal("非成员竟然查到了别人的空间")
	}
	if !isNotFound(err) {
		t.Fatalf("期望 ErrNotFound（前端靠它转 null），实际 %v", err)
	}
}

// TestGetWorkspaceByKeyWithoutIdentityStillWorks: no blocking when there is no identity.
//
// This path is also called directly by many unit tests (not through HTTP); blocking would make a
// batch of tests unrelated to authorization fail here. In production every endpoint sits behind
// the auth middleware, so this branch is never reached.
func TestGetWorkspaceByKeyWithoutIdentityStillWorks(t *testing.T) {
	svc, _ := newService(t)
	if _, err := svc.GetWorkspaceByKey(context.Background(), "growth"); err != nil {
		t.Fatalf("无身份查空间失败：%v，会连带弄坏一批与授权无关的用例", err)
	}
}

// TestWorkspaceOfResolvesResources: flat paths must be able to resolve the owning workspace from a resource id.
func TestWorkspaceOfResolvesResources(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	cases := []struct {
		name string
		kind ResourceKind
		id   string
	}{
		{"agent", KindAgent, "agent_triage"},
		{"skill", KindSkill, "skill-20260801-triage"},
		{"run", KindRun, "run_1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws, err := svc.WorkspaceOf(ctx, tc.kind, tc.id)
			if err != nil {
				t.Fatalf("反解失败：%v", err)
			}
			if ws != seedWS {
				t.Errorf("归属空间 = %q，期望 %q", ws, seedWS)
			}
		})
	}
}

// TestWorkspaceOfMissingIsNotFound: a resource that cannot be resolved must return ErrNotFound.
//
// authz relies on it to return 404 instead of 403. If the translation is lost, "this agent was
// deleted" becomes "access denied", the user goes to an admin asking for permission, and the
// agent has long been gone.
func TestWorkspaceOfMissingIsNotFound(t *testing.T) {
	svc, _ := newService(t)
	for _, kind := range []ResourceKind{KindAgent, KindSkill, KindRun, KindSchedule} {
		if _, err := svc.WorkspaceOf(context.Background(), kind, "gone"); !isNotFound(err) {
			t.Errorf("kind=%d 反解不存在的资源，期望 ErrNotFound，实际 %v", kind, err)
		}
	}
}

// ids flattens a workspace list into an ID slice, so error messages show which ones.
func ids(list []apimodel.Workspace) []string {
	out := make([]string, 0, len(list))
	for _, ws := range list {
		out = append(out, ws.ID)
	}
	return out
}

// isNotFound reports whether this is a "resource does not exist" kind of error.
func isNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
