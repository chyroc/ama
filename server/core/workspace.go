// Workspaces and members: forwarded to service/workspace.
//
// # The business logic has moved out
//
// Step 7 of the package-split plan moved workspaces and members into service/workspace
// (check: `go list -deps ./service/workspace | grep -c 'ama/server/core$'` = 0).
// What remains here is all **forwarding**, the same shape as the previous four domains (issue / suggestion / revision / schedule).
//
// # ⚠️ Forwarding is temporary, but RoleOf stays a bit longer
//
// The orchestration layer's fallback interface requires *core.Service to keep these methods; the assertion is in
// cmd/amad/orchestrator.go (var _ orchestrator.Fallback = (*core.Service)(nil)).
// Each time a domain moves entirely out of core, the matching batch of forwarders is deleted with it.
//
// **But RoleOf and WorkspaceOf are not in that batch.** They are the implementation of authz.Membership
// (wired in authzMembership in cmd/amad/main.go), and authz and core deliberately do not import each other:
// authz declares what it needs, and cmd/amad satisfies it with core.Service. This step did **not** switch
// the implementer of Membership to *workspacesvc.Service; see the file header of service/workspace/service.go
// for why: the agent / skill / run cells that WorkspaceOf resolves are still in core's registry,
// and switching only half would make the two methods of Membership come from two different objects.
//
// So the authz side has **zero changes** in this step; the mechanical check is zero diff in roleCapabilities.ts.
//
// # ⚠️ The authorization rules moved too; do not reimplement them here
//
//	authorization and role ordering  → workspace.RoleOf
//
// This file should have **not a single check**. Adding one makes a second source of truth, and its drift leads to security incidents.

package core

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
	workspacesvc "github.com/chyroc/ama/server/service/workspace"
)

// ListWorkspacesFor lists only the workspaces actor is a member of. **This is the user-facing list.**
func (s *Service) ListWorkspacesFor(ctx context.Context, actor string) ([]apimodel.Workspace, error) {
	return s.workspaces.ListWorkspacesFor(ctx, actor)
}

// RoleOf returns actor's role in a workspace. A false second return value means not a member.
//
// ⚠️ **This is the entry point for authorization across the whole site**: authz/guard.go calls it on every request (through
// authzMembership in cmd/amad), and core/agent.go calls it when saving an agent too.
// So this forwarder lives longer than the others; see the file header.
//
// The authorization rules are in workspacesvc.RoleOf; **do not check again here**.
func (s *Service) RoleOf(
	ctx context.Context, workspaceID, actor string,
) (apimodel.WorkspaceRole, bool, error) {
	return s.workspaces.RoleOf(ctx, workspaceID, actor)
}

// GetWorkspaceByKey gets a workspace by URL key; returns ErrNotFound if missing.
//
// ⚠️ **This is the only place where "authorization is written in the Service layer"** (the workspace comes from a query parameter,
// which a Guard pattern cannot express); non-members get 404 instead of 403.
// The full reasoning is on workspacesvc.GetWorkspaceByKey; do not assume the auth check was forgotten.
func (s *Service) GetWorkspaceByKey(ctx context.Context, key string) (apimodel.Workspace, error) {
	return s.workspaces.GetWorkspaceByKey(ctx, key)
}

// IsWorkspaceKeyTaken checks whether key is taken; when exceptID is non-empty that workspace is excluded.
//
// ⚠️ **It must stay a global uniqueness check**, not filtered by membership; otherwise a user could create a workspace whose key is already
// taken by someone else, then hit the unique constraint on save.
func (s *Service) IsWorkspaceKeyTaken(ctx context.Context, key, exceptID string) (bool, error) {
	return s.workspaces.IsWorkspaceKeyTaken(ctx, key, exceptID)
}

// CreateWorkspace creates a workspace and automatically adds an owner member.
func (s *Service) CreateWorkspace(ctx context.Context, in WorkspaceInput) (apimodel.Workspace, error) {
	return s.workspaces.CreateWorkspace(ctx, in)
}

// UpdateWorkspace changes the workspace's name / key / description.
//
// ⚠️ **A missing description key means clear it** (matching LocalApiClient's Object.assign);
// see workspacesvc.UpdateWorkspace for why.
func (s *Service) UpdateWorkspace(ctx context.Context, id string, patch WorkspacePatch) (apimodel.Workspace, error) {
	return s.workspaces.UpdateWorkspace(ctx, id, patch)
}

// DeleteWorkspace deletes a workspace and cascades cleanup of everything under it. Deleting a missing id is not an error (idempotent).
func (s *Service) DeleteWorkspace(ctx context.Context, id string) error {
	return s.workspaces.DeleteWorkspace(ctx, id)
}

// AddMember adds a member to a workspace.
func (s *Service) AddMember(ctx context.Context, workspaceID string, in MemberInput) (apimodel.WorkspaceMember, error) {
	return s.workspaces.AddMember(ctx, workspaceID, in)
}

// UpdateMemberRole changes a member's role.
func (s *Service) UpdateMemberRole(
	ctx context.Context, workspaceID, memberID string, role apimodel.WorkspaceRole,
) (apimodel.WorkspaceMember, error) {
	return s.workspaces.UpdateMemberRole(ctx, workspaceID, memberID, role)
}

// RemoveMember removes a member.
func (s *Service) RemoveMember(ctx context.Context, workspaceID, memberID string) error {
	return s.workspaces.RemoveMember(ctx, workspaceID, memberID)
}

// memberOf finds a member in a workspace by email, **case-insensitively**.
//
// **It is an alias of workspacesvc.MemberOf** (a variable, not a forwarding function, so it cannot drift):
// this package has a few more call sites, and "what counts as the same person" must have only one implementation. Authorization recognizing someone
// while other places do not is a security incident. The full reasoning (and why AddMember's duplicate check is instead
// case-sensitive) is on workspacesvc.MemberOf.
var memberOf = workspacesvc.MemberOf

// firstOwnerEmail gets the first owner's email, or an empty string if there is no owner.
//
// Same as above, an alias of workspacesvc.FirstOwnerEmail. The three injections in sessionenv.go and
// the UpdatedBy value in ark.go both use it; **it must have the same source as WorkspaceOwnerOf**.
var firstOwnerEmail = workspacesvc.FirstOwnerEmail
