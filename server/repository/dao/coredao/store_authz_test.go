// Contract tests for the authorization query (ListAuthzRoles); each of the two Store implementations runs them.
//
// In production authorization runs the PG SQL; if it drifts from mem, "unit tests green, wrong authorization in production".
package coredao

import (
	"context"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

// testAuthzRolesIsCaseInsensitive: member emails are **typed by hand** by admins and may contain uppercase,
// while the login identity is lowercased at the entry point (auth.normalizeEmail).
//
// Authorization compares "is this the same person", so it is case-insensitive (same as the comment on core.memberOf).
// If this is missed the symptom is "I am clearly a member but it says I'm not", and **only for a few people**.
func testAuthzRolesIsCaseInsensitive(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	// The seed stores everything in lowercase; query here with mixed case.
	got, err := s.ListAuthzRoles(ctx, "ws_demo", "Dave@Example.com")
	if err != nil {
		t.Fatalf("查判权角色失败：%v", err)
	}
	if len(got) != 1 {
		t.Fatalf("大写邮箱该命中同一个人（判权按「是不是同一个人」比），实际 %+v", got)
	}
	if got[0].Role != apimodel.RoleViewer {
		t.Errorf("角色该是 viewer，实际 %q", got[0].Role)
	}
}

// testAuthzRolesUnknownWorkspaceIsEmpty: when the workspace does not exist, return an **empty slice**, not ErrNotFound.
//
// "The workspace is gone" and "you are not a member" mean the same for authorization: no access. Otherwise deleting a workspace would
// turn authorization from 403 into 500.
//
// ⚠️ This also pins the `exists (select 1 from workspaces ...)` in the PG SQL:
// without it, leftover member rows would make authorization accept a workspace that no longer exists.
func testAuthzRolesUnknownWorkspaceIsEmpty(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	got, err := s.ListAuthzRoles(ctx, "ws_does_not_exist", "alice@example.com")
	if err != nil {
		t.Fatalf("空间不存在不该报错（「空间没了」与「你不是成员」是同一个结论）：%v", err)
	}
	if len(got) != 0 {
		t.Fatalf("不存在的空间该返回空，实际 %+v", got)
	}
	if got == nil {
		t.Error("该返回非 nil 空切片，同本包其它列表方法")
	}
}

// testAuthzRolesScopedToWorkspace: authorization **only looks at this workspace's rows**: a member of workspace A
// finds nothing in workspace B. Missing the workspace_id condition is cross-workspace privilege escalation, with zero errors.
func testAuthzRolesScopedToWorkspace(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, true)

	other, err := s.CreateWorkspace(ctx, apimodel.Workspace{
		ID: "ws_other", Key: "other", Name: "另一个空间",
		Members: []apimodel.WorkspaceMember{
			{ID: "m_other", Email: "outsider@example.com", Role: apimodel.RoleAdmin},
		},
	})
	if err != nil {
		t.Fatalf("建空间失败：%v", err)
	}

	got, err := s.ListAuthzRoles(ctx, "ws_demo", "outsider@example.com")
	if err != nil {
		t.Fatalf("查判权角色失败：%v", err)
	}
	if len(got) != 0 {
		t.Fatalf("这个人只在 ws_other 里，在 ws_demo 却命中了 %+v——跨空间越权", got)
	}

	// Positive control: it must match in the workspace they belong to, otherwise the check above passes falsely.
	got, err = s.ListAuthzRoles(ctx, other.ID, "outsider@example.com")
	if err != nil {
		t.Fatalf("查判权角色失败：%v", err)
	}
	if len(got) != 1 || got[0].Role != apimodel.RoleAdmin {
		t.Fatalf("在他所在的空间里该命中 admin 那一行，实际 %+v", got)
	}
}
