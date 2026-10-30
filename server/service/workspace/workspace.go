// Business logic of workspaces and members.
//
// The behavior baseline is LocalApiClient in web/src/data/api.ts, and error messages must match it word for word:
// the frontend shows e.message directly to users (FormModal.tsx / WorkspacesPage.tsx),
// and e2e also asserts some of the original strings.

package workspace

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// workspaceKeyPattern matches the frontend WORKSPACE_KEY_PATTERN.
var workspaceKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)

// The creator hard-coded in LocalApiClient (createWorkspace in api.ts).
// The 05-settings e2e asserts the remove button of this member is disabled, so the default must match it.
var defaultOwner = Owner{Name: "张三", Email: "alice@example.com"}

// ownerFrom gets "the person doing this operation", used to write the creator as the workspace owner.
//
// **The normal path is the current logged-in user**: every business endpoint is behind the auth middleware, so ctx always has
// auth.Identity, and the email is the Actor ID.
//
// Falling back to s.owner only happens in two cases: unit tests calling Service directly (not through HTTP), and local play
// with AMA_STORE=mem. **Production never reaches this fallback**: reaching it means some endpoint is missing auth,
// which is a bug, but this still gives a usable owner instead of leaving it empty, because "a workspace with no owner"
// would break a bunch of logic on the members page right away (cannot remove the only owner, updatedBy of Ark credentials).
func (s *Service) ownerFrom(ctx context.Context) Owner {
	identity, ok := auth.IdentityFrom(ctx)
	if !ok {
		return s.owner
	}
	name := strings.TrimSpace(identity.Name)
	if name == "" {
		name = identity.Email
	}
	return Owner{Name: name, Email: identity.Email}
}

// ActingOwner gets "the person doing this operation", for the two call sites still left in core
// (core/agentark.go and core/pendingmessage.go, which take UpdatedBy / Actor).
//
// It is the public version of ownerFrom: those two places and writing the owner when creating a workspace **must follow the same rule**;
// writing it separately would make "who changed it" give different answers on different endpoints, with zero errors.
func (s *Service) ActingOwner(ctx context.Context) Owner {
	return s.ownerFrom(ctx)
}

// ListWorkspaces lists **all** workspaces, without any filtering.
//
// ⚠️ **Do not expose it over HTTP**: that would show users other people's workspaces. The user-facing list goes through
// ListWorkspacesFor.
//
// An unfiltered version is kept because there is a kind of caller **with no notion of a "current user"**:
// the worker pool scan in core/worker.go scans the Ark bindings of all workspaces to start a worker for each environment.
// If this were changed to filter by identity, that path (a process-level ctx without auth.Identity) would find 0 workspaces,
// so every agent would get stuck in requires_action as soon as it calls a tool, **with no error**.
//
// So "filter or not" is an explicit choice at the call site, not decided implicitly by whether ctx has an identity:
// the latter would let identity-less system paths silently degrade to empty results, and such bugs are all green in single-instance tests.
func (s *Service) ListWorkspaces(ctx context.Context) ([]apimodel.Workspace, error) {
	return s.store.ListWorkspaces(ctx)
}

// ListWorkspacesFor only lists the workspaces actor is a member of. **This is the user-facing list.**
//
// actor is the employee email (the one from auth.ActorFrom). An empty actor returns an empty list, not everything:
// fail-closed: reaching here without an identity means some endpoint is missing auth, and then better to give nothing.
//
// Roles take no part in filtering: a viewer also sees the workspaces they are in, just read-only once inside.
func (s *Service) ListWorkspacesFor(ctx context.Context, actor string) ([]apimodel.Workspace, error) {
	who := strings.TrimSpace(actor)
	if who == "" {
		return []apimodel.Workspace{}, nil
	}
	all, err := s.store.ListWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	// Slice fields always return a non-nil empty slice, otherwise they encode as null while the contract requires [].
	out := make([]apimodel.Workspace, 0, len(all))
	for _, ws := range all {
		if _, ok := memberOf(ws, who); ok {
			out = append(out, ws)
		}
	}
	return out, nil
}

// RoleOf returns actor's role in a workspace. A false second return value means not a member.
//
// For wiring authz (it only declares needing this ability and does not know this package). When the workspace does not exist it returns
// (,"", false, nil): "the workspace is gone" and "you are not a member" are the same conclusion for authorization: no access.
//
// # One round trip; do not add a second
//
// This function **runs on every request** in authz/guard.go, **and deliberately has no cache at all**, so every
// database query here is multiplied by every endpoint of the site. The permission check only needs **that one row of this actor**, so it goes through
// store.ListAuthzRoles for a point lookup, instead of GetWorkspace pulling all members.
//
// ⚠️ **Never save this with a cache.** Role changes and member removals must take effect immediately.
// Adding a cache layer trades "immediately" for "after a while", and no UI would explain that.
func (s *Service) RoleOf(
	ctx context.Context, workspaceID, actor string,
) (apimodel.WorkspaceRole, bool, error) {
	who := strings.TrimSpace(actor)
	if who == "" {
		return "", false, nil
	}

	// When the workspace does not exist it returns an empty slice instead of ErrNotFound: "the workspace is gone" and "you are not a member"
	// are the same conclusion for authorization: no access.
	//
	// A database hiccup must be propagated: treating it as "not a member" would make a database failure show up as a 403
	// "您不是该工作区的成员" (you are not a member of this workspace), a false sentence the user cannot do anything about.
	rows, err := s.store.ListAuthzRoles(ctx, workspaceID, who)
	if err != nil {
		return "", false, err
	}

	// The unique index guarantees at most one row; it is still written as "take the highest permission", so that dirty data would never downgrade.
	best := apimodel.WorkspaceRole("")
	for _, r := range rows {
		if best == "" || roleRank(r.Role) > roleRank(best) {
			best = r.Role
		}
	}
	if best == "" {
		return "", false, nil
	}
	return best, true, nil
}

// memberOf finds a member in a workspace by email, **case-insensitively**.
//
// # Why it is case-insensitive here, while the dedup in AddMember is case-sensitive
//
// The two ask different questions:
//
//   - This function asks "is this the same person". auth.Identity.Email is lowercased by normalizeEmail at the entry
//     (auth/auth.go), while member emails are **typed in by admins** and may be written as
//     Bob@example.com. Compared case-sensitively, that person could never find their own role,
//     showing up as "I am clearly a member, but it says I am not", and only for a few people.
//   - AddMember's uniqueness check asks "does the frontend allow this addition". That one copies
//     LocalApiClient's exact match (see the comment on AddMember, "do not casually change it to case-insensitive"),
//     and changing it would block operations the frontend allows.
//
// So the two are deliberately inconsistent. The cost is that a@x and A@x can both exist as member records in the same workspace,
// and this function takes the first; that is a problem of the existing data model, and should not be quietly fixed here in authorization.
func memberOf(ws apimodel.Workspace, actor string) (apimodel.WorkspaceMember, bool) {
	for _, m := range ws.Members {
		if strings.EqualFold(strings.TrimSpace(m.Email), actor) {
			return m, true
		}
	}
	return apimodel.WorkspaceMember{}, false
}

// MemberOf is the public version of memberOf, for callers still left in core.
//
// ⚠️ **It is a pure function** (no database, no Service), so it is here only so that "how to decide it is the same
// person" has a single implementation. The core side used to have its own private function of the same name, and the direction the two copies would drift
// is exactly a security incident: the permission check recognizes this person while other places do not.
func MemberOf(ws apimodel.Workspace, actor string) (apimodel.WorkspaceMember, bool) {
	return memberOf(ws, actor)
}

// GetWorkspaceByKey gets a workspace by URL key; returns ErrNotFound if it does not exist.
//
// # This is the only place where "authorization is written in the Service layer"
//
// Authorization of the other workspace-level endpoints is done by authz.Guard at the HTTP boundary, but not this one:
// its workspace comes from a **query parameter** (?key=growth) rather than a path parameter, and Guard declares permissions by
// method + pattern, which cannot express "the workspace the key in the parameter refers to".
//
// **A non-member gets ErrNotFound (404), not ErrAccessDenied (403)**, for two reasons:
//
//   - the frontend WorkspaceGate uses nullable() to turn 404 into null → jump back to the workspace list; this is existing
//     behavior, and the frontend need not handle a new error shape for this endpoint;
//   - it also does not reveal "this key exists but you cannot get in".
func (s *Service) GetWorkspaceByKey(ctx context.Context, key string) (apimodel.Workspace, error) {
	ws, err := s.store.GetWorkspaceByKey(ctx, key)
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			return apimodel.Workspace{}, fmt.Errorf("%w: 空间不存在", persistmodel.ErrNotFound)
		}
		return apimodel.Workspace{}, err
	}
	// Non-members are treated as "does not exist"; see the long comment above.
	//
	// When no identity can be obtained **do not block**: this path is also called directly by unit tests (not through HTTP), and blocking would make a batch of
	// tests unrelated to authorization fail here. In production every endpoint is behind the auth middleware, so this branch is never reached.
	if actor := auth.ActorFrom(ctx); actor != "" {
		if _, ok := memberOf(ws, actor); !ok {
			return apimodel.Workspace{}, fmt.Errorf("%w: 空间不存在", persistmodel.ErrNotFound)
		}
	}
	return ws, nil
}

// IsWorkspaceKeyTaken decides whether a key is taken; when exceptID is non-empty that workspace is excluded.
//
// The key format is not validated: the frontend validates it itself before submitting (WorkspacesPage / SettingsPage),
// and here an invalid key returns false rather than 400: a 400 would be caught by the frontend and shown as
// the error text of the "name" field, which is worse.
func (s *Service) IsWorkspaceKeyTaken(ctx context.Context, key, exceptID string) (bool, error) {
	return s.store.KeyTaken(ctx, key, exceptID)
}

// CreateWorkspace creates a workspace, and automatically writes an owner member.
//
// Automatically adding the owner copies LocalApiClient: a workspace must have an owner; the members page's "cannot remove the only
// owner" and updatedBy of Ark credentials both depend on it.
//
// **The owner is the person currently logged in** (taken from auth.Identity in ctx; the email is the Actor ID).
// When it cannot be obtained it falls back to Config.DefaultOwner; only tests and local play with AMA_STORE=mem
// reach that path, and in production every endpoint is behind the auth middleware, so ctx always has an identity.
func (s *Service) CreateWorkspace(ctx context.Context, in apimodel.WorkspaceInput) (apimodel.Workspace, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return apimodel.Workspace{}, fmt.Errorf("%w: 空间名称不能为空", persistmodel.ErrInvalidArgument)
	}
	key := strings.TrimSpace(in.Key)
	if !workspaceKeyPattern.MatchString(key) {
		return apimodel.Workspace{}, fmt.Errorf("%w: 空间 Key 只能用小写字母、数字和连字符，且以字母或数字开头", persistmodel.ErrInvalidArgument)
	}

	now := apimodel.NowISO()
	owner := s.ownerFrom(ctx)
	ws := apimodel.Workspace{
		ID:          s.nextID("ws"),
		Name:        name,
		Key:         key,
		Description: strings.TrimSpace(in.Description),
		CreatedAt:   now,
		// On creation it has the same value as CreatedAt (the same now, not taken again: two calls could cross a millisecond
		// boundary, and a workspace just created would show as "edited").
		UpdatedAt: now,
		// Who created it. **Take auth.ActorFrom, not owner.Email**: the two have the same value right now, but
		// "the first owner" changes with transfers, while "who created it" does not; see
		// that part on apimodel.Workspace.CreatedBy. Using owner.Email passes the tests now,
		// but would silently become wrong once "transfer workspace" is added.
		CreatedBy: auth.ActorFrom(ctx),
		UpdatedBy: auth.ActorFrom(ctx),
		Members: []apimodel.WorkspaceMember{{
			ID:       s.nextID("m"),
			Name:     owner.Name,
			Email:    owner.Email,
			Role:     apimodel.RoleOwner,
			JoinedAt: now,
		}},
	}
	// A key collision gets ErrConflict from Store, with the message built there
	return s.store.CreateWorkspace(ctx, ws)
}

// UpdateWorkspace changes a workspace's name / key / description.
//
// # A missing description key means clearing it
//
// When the frontend clears the description, the body it sends **has no** description key: SettingsPage writes
// `description: description.trim() || undefined`, and JSON.stringify drops keys whose value is
// undefined entirely.
//
// While LocalApiClient uses Object.assign(ws, patch), where that key's value in patch is
// undefined (the object is passed in memory, without going through JSON), and Object.assign **does** copy it over,
// so the description really is cleared.
//
// So "the key is absent" must be interpreted as **clearing** over HTTP, to match LocalApiClient's behavior.
// Interpreted as "do not change", the same operation would give different results under the two clients: the localStorage version clears it,
// the HTTP version does not, and the page gives no error; users would just see the description come back after a while.
func (s *Service) UpdateWorkspace(ctx context.Context, id string, patch apimodel.WorkspacePatch) (apimodel.Workspace, error) {
	ws, err := s.store.GetWorkspace(ctx, id)
	if err != nil {
		return apimodel.Workspace{}, err
	}

	if patch.Name != nil {
		name := strings.TrimSpace(*patch.Name)
		if name == "" {
			return apimodel.Workspace{}, fmt.Errorf("%w: 空间名称不能为空", persistmodel.ErrInvalidArgument)
		}
		ws.Name = name
	}
	if patch.Key != nil {
		key := strings.TrimSpace(*patch.Key)
		if !workspaceKeyPattern.MatchString(key) {
			return apimodel.Workspace{}, fmt.Errorf("%w: 空间 Key 只能用小写字母、数字和连字符，且以字母或数字开头", persistmodel.ErrInvalidArgument)
		}
		ws.Key = key
	}
	// A missing key means clearing; see the long comment above
	if patch.Description != nil {
		ws.Description = strings.TrimSpace(*patch.Description)
	} else {
		ws.Description = ""
	}

	ws.UpdatedAt = apimodel.NowISO()
	// Who changed it. **Only UpdatedBy is touched; CreatedBy is the one read back above**; see
	// apimodel.Workspace.CreatedBy, and the same rule in UpdateAgent.
	ws.UpdatedBy = auth.ActorFrom(ctx)
	// Whether the key really changed, and the dedup excluding itself, are both in Store.SaveWorkspace
	return s.store.SaveWorkspace(ctx, ws)
}

// DeleteWorkspace deletes a workspace, cascading to clean everything under it. Deleting a non-existent id is not an error (idempotent).
func (s *Service) DeleteWorkspace(ctx context.Context, id string) error {
	return s.store.DeleteWorkspace(ctx, id)
}

// ---------------------------------------------------------------------------
// Members
// ---------------------------------------------------------------------------

var validRoles = map[apimodel.WorkspaceRole]bool{
	apimodel.RoleOwner:  true,
	apimodel.RoleAdmin:  true,
	apimodel.RoleMember: true,
	apimodel.RoleViewer: true,
}

// AddMember adds a member to a workspace.
//
// Email uniqueness is an exact match **per workspace** and **case-sensitive** (copied from api.ts:291):
// do not "casually" change it to case-insensitive or unique across workspaces; that would block operations the frontend allows.
// The new member's role is decided by the input, and **can be owner**.
func (s *Service) AddMember(ctx context.Context, workspaceID string, in apimodel.MemberInput) (apimodel.WorkspaceMember, error) {
	name := strings.TrimSpace(in.Name)
	email := strings.TrimSpace(in.Email)
	if name == "" || email == "" {
		return apimodel.WorkspaceMember{}, fmt.Errorf("%w: 成员姓名与邮箱都不能为空", persistmodel.ErrInvalidArgument)
	}
	if !validRoles[in.Role] {
		return apimodel.WorkspaceMember{}, fmt.Errorf("%w: 角色取值不合法", persistmodel.ErrInvalidArgument)
	}

	ws, err := s.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		// The frontend text here is "空间不存在" (workspace does not exist)
		return apimodel.WorkspaceMember{}, fmt.Errorf("%w: 空间不存在", persistmodel.ErrNotFound)
	}
	for _, m := range ws.Members {
		if m.Email == email {
			return apimodel.WorkspaceMember{}, fmt.Errorf("%w: %s 已经是该空间成员", persistmodel.ErrConflict, email)
		}
	}

	member := apimodel.WorkspaceMember{
		ID:    s.nextID("m"),
		Name:  name,
		Email: email,
		Role:  in.Role,
		// The avatar is stored as-is; empty is empty (omitempty makes it disappear from the JSON entirely).
		AvatarURL: strings.TrimSpace(in.AvatarURL),
		JoinedAt:  apimodel.NowISO(),
	}
	ws.Members = append(ws.Members, member)
	// Adding, removing, or changing members also counts as changing this workspace: "last updated" is about the workspace, not just its name.
	// Same for UpdatedBy: adding a person is a real user operation and must leave a trace (same as SetAgentIntegrations).
	ws.UpdatedAt = apimodel.NowISO()
	ws.UpdatedBy = auth.ActorFrom(ctx)
	if _, err := s.store.SaveWorkspace(ctx, ws); err != nil {
		return apimodel.WorkspaceMember{}, err
	}
	return member, nil
}

// UpdateMemberRole changes a member's role.
//
// The guard only fires when "changing **someone who is currently an owner** to a non-owner, and they are the last owner".
// Demoting a non-owner, or setting an owner to owner, is always allowed (copied from api.ts:314-319).
//
// Note the text: here it is "空间必须保留至少一个拥有者" (a workspace must keep at least one owner), while RemoveMember says
// "不能移除唯一的拥有者" (cannot remove the only owner): **two different sentences**; do not merge them for DRY.
// Also, when either the workspace or the member does not exist, the frontend reports "成员不存在" (member does not exist).
func (s *Service) UpdateMemberRole(
	ctx context.Context, workspaceID, memberID string, role apimodel.WorkspaceRole,
) (apimodel.WorkspaceMember, error) {
	if !validRoles[role] {
		return apimodel.WorkspaceMember{}, fmt.Errorf("%w: 角色取值不合法", persistmodel.ErrInvalidArgument)
	}

	ws, err := s.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return apimodel.WorkspaceMember{}, fmt.Errorf("%w: 成员不存在", persistmodel.ErrNotFound)
	}
	idx := -1
	for i, m := range ws.Members {
		if m.ID == memberID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return apimodel.WorkspaceMember{}, fmt.Errorf("%w: 成员不存在", persistmodel.ErrNotFound)
	}

	if ws.Members[idx].Role == apimodel.RoleOwner && role != apimodel.RoleOwner {
		if countOwners(ws.Members) == 1 {
			return apimodel.WorkspaceMember{}, fmt.Errorf("%w: 空间必须保留至少一个拥有者", persistmodel.ErrPrecondition)
		}
	}

	ws.Members[idx].Role = role
	ws.UpdatedAt = apimodel.NowISO()
	ws.UpdatedBy = auth.ActorFrom(ctx)
	if _, err := s.store.SaveWorkspace(ctx, ws); err != nil {
		return apimodel.WorkspaceMember{}, err
	}
	return ws.Members[idx], nil
}

// RemoveMember removes a member.
//
// The three kinds of "not found" behave differently, copied from LocalApiClient:
//   - workspace does not exist → report "空间不存在" (workspace does not exist)
//   - member does not exist → **succeed silently** (filter is a no-op)
//   - the target is the last owner → report "不能移除唯一的拥有者" (cannot remove the only owner)
func (s *Service) RemoveMember(ctx context.Context, workspaceID, memberID string) error {
	ws, err := s.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("%w: 空间不存在", persistmodel.ErrNotFound)
	}

	var target *apimodel.WorkspaceMember
	for i := range ws.Members {
		if ws.Members[i].ID == memberID {
			target = &ws.Members[i]
			break
		}
	}
	// A member that does not exist succeeds silently, without error
	if target == nil {
		return nil
	}
	if target.Role == apimodel.RoleOwner && countOwners(ws.Members) == 1 {
		return fmt.Errorf("%w: 不能移除唯一的拥有者", persistmodel.ErrPrecondition)
	}

	kept := make([]apimodel.WorkspaceMember, 0, len(ws.Members))
	for _, m := range ws.Members {
		if m.ID != memberID {
			kept = append(kept, m)
		}
	}
	ws.Members = kept
	ws.UpdatedAt = apimodel.NowISO()
	ws.UpdatedBy = auth.ActorFrom(ctx)
	_, err = s.store.SaveWorkspace(ctx, ws)
	return err
}

func countOwners(members []apimodel.WorkspaceMember) int {
	n := 0
	for _, m := range members {
		if m.Role == apimodel.RoleOwner {
			n++
		}
	}
	return n
}

// FirstOwnerEmail gets the email of the first owner, or an empty string if there is no owner.
//
// ⚠️ **A pure function**, here for the same reason as memberOf: the places on the injection path
// (credential injection in core/sessionenv.go, and core/ark.go taking the value for
// UpdatedBy) and WorkspaceOwnerOf **must be the same function**, otherwise the UI would say
// "A is in effect" while runtime uses B, with zero errors.
func FirstOwnerEmail(members []apimodel.WorkspaceMember) string {
	for _, m := range members {
		if m.Role == apimodel.RoleOwner {
			return m.Email
		}
	}
	return ""
}

// WorkspaceIDsFor returns the ids of the workspaces this person is in, in the same order as ListWorkspacesFor.
//
// # Why a separate method instead of letting the caller map
//
// It is for feishu.UserAppResolver (which needs to find Feishu apps by workspace), and **feishu cannot
// import concrete types of core** (that would form a cycle with the core → feishu direction). It returns []string rather than
// []apimodel.Workspace precisely so that the interface on that side only depends on standard library types.
//
// ⚠️ **It is called on the injection path** (once for every container command execution), so it deliberately makes no
// extra external calls.
func (s *Service) WorkspaceIDsFor(ctx context.Context, actor string) ([]string, error) {
	list, err := s.ListWorkspacesFor(ctx, actor)
	if err != nil {
		return nil, err
	}
	// **A non-nil empty slice**: the caller ranges over it directly, and nil and empty mean the same here,
	// but returning nil would add another way of writing the "are there any workspaces" check.
	out := make([]string, 0, len(list))
	for _, ws := range list {
		out = append(out, ws.ID)
	}
	return out, nil
}

// WorkspaceNameOf returns a workspace's display name, for display only.
//
// For the app picker of feishu.UserAppResolver ("which workspace does this Feishu app belong to"):
// when three apps share a name, the workspace name is often the only thing the user can tell them apart by (in practice all three were named "AMA 助手").
//
// ⚠️ **No authorization check**, the same rule as WorkspaceIDsFor: the workspace ids the caller has already come from
// the list of "workspaces this person is in", and the name is not more sensitive than the id. Adding a
// CanAccess check here would only add a database query and block nothing.
//
// ⚠️ **Not on the injection path**. Injection only needs app_id and secret, and looking up the name too would add a round trip to every bash
// command: that is exactly why ResolveApp deliberately goes through Store and not Service.
func (s *Service) WorkspaceNameOf(ctx context.Context, workspaceID string) (string, error) {
	ws, err := s.store.GetWorkspace(ctx, strings.TrimSpace(workspaceID))
	if err != nil {
		return "", err
	}
	return ws.Name, nil
}

// WorkspaceOwnerOf returns the email of this workspace's "first owner", whoever the injection path uses.
//
// # ⚠️ The rule must come from the same source as the injection path; do not write it again here
//
// It goes through FirstOwnerEmail, **literally the same function** as credential injection in sessionenv.go
// and ark.go taking the value for UpdatedBy. This is load-bearing: the purpose of this method
// is to tell users on the authorization card "whose authorization is actually in effect in this workspace", and once it differs from the person really injected,
// the UI would say "A is in effect" while runtime uses B, **with zero errors**, and while debugging nobody would suspect
// that the hint itself is wrong.
//
// When no owner can be found it returns **an empty string with a nil err**: RemoveMember prevents deleting down to zero owners,
// so that is an abnormal state, but it should only cost the card one hint, not turn the whole card into an error.
//
// ⚠️ **No authorization check**, same as WorkspaceIDsFor / WorkspaceNameOf: the caller (the provider layer)
// has already checked membership with ensureWorkspaceMember, and checking again here would only add a round trip.
func (s *Service) WorkspaceOwnerOf(ctx context.Context, workspaceID string) (string, error) {
	ws, err := s.store.GetWorkspace(ctx, strings.TrimSpace(workspaceID))
	if err != nil {
		return "", err
	}
	return FirstOwnerEmail(ws.Members), nil
}
