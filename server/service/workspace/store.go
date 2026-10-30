// The persistence contract of the Workspace domain: workspaces and members.
//
// # Declarations follow the domain, not left in core/store.go
//
// The reason is word for word the same as for the previous four domains (service/issue, suggestion, revision, schedule):
// core/store.go is a shared file every resource domain must pass through, and many concurrent changes touch it. Piling one domain's
// method declarations there costs not "a big file" but **conflict surface**: two changes each moving their own domain
// still collide in the same hunk of the diff.
//
// Now the declarations live here, and core/store.go only embeds one line, `workspacesvc.Store`.
package workspace

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// Store is all the persistence methods this domain needs, **listing only the ones needed**.
type Store interface {
	// -- Workspaces ---------------------------------------------------------------

	ListWorkspaces(ctx context.Context) ([]apimodel.Workspace, error)
	// GetWorkspace gets by ID; returns ErrNotFound if it does not exist.
	GetWorkspace(ctx context.Context, id string) (apimodel.Workspace, error)
	// GetWorkspaceByKey gets by URL key; returns ErrNotFound if it does not exist.
	GetWorkspaceByKey(ctx context.Context, key string) (apimodel.Workspace, error)
	// KeyTaken decides whether a key is already taken; when exceptID is non-empty that ID is excluded (editing yourself is not a conflict).
	KeyTaken(ctx context.Context, key, exceptID string) (bool, error)
	// CreateWorkspace stores a new workspace; returns ErrConflict if the key collides.
	CreateWorkspace(ctx context.Context, ws apimodel.Workspace) (apimodel.Workspace, error)
	// SaveWorkspace overwrites; returns ErrNotFound if it does not exist.
	SaveWorkspace(ctx context.Context, ws apimodel.Workspace) (apimodel.Workspace, error)
	// DeleteWorkspace deletes the workspace and cascades to clean everything under it; not existing **is not an error** (idempotent).
	DeleteWorkspace(ctx context.Context, id string) error

	// ListAuthzRoles is the query **dedicated to the permission check path**: it gets "this actor's member rows in this workspace"
	// in one go.
	//
	// # Why the permission check does not reuse GetWorkspace
	//
	// The permission check (RoleOf) **runs on every request** in authz/guard.go, **and deliberately has no cache at all**,
	// so every round trip on this path is multiplied by every endpoint of the site. GetWorkspace queries the workspace row +
	// **all** members, while the permission check only cares about **one row** of them: a workspace with 300 members would pull 300 rows for nothing on every request.
	//
	// # The contract (both implementations must match item by item; getting the permission check wrong is a security issue)
	//
	//   - actor comparison is **case-insensitive** (the email_key column is lowercased on the write side; see
	//     emailKeyOf in coredao). Same reason as memberOf: member emails are typed in by admins.
	//   - **When the workspace does not exist, return an empty slice**, not ErrNotFound. "The workspace is gone" and "you are not a member"
	//     are the same conclusion for authorization (no access).
	//   - Return **a non-nil empty slice**, like the other list methods in this file.
	ListAuthzRoles(ctx context.Context, workspaceID, actor string) ([]persistmodel.AuthzRole, error)
}
