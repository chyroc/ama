// Normalization and the save gate for executor drafts (cloud container / host machine).
//
// The shape copies the authorization gate in the same file domain (ensureIntegrationsAllowed): the
// two host sources
// are the same pair of opposites as "personal authorization / service account authorization":
//
//	HostSourceWorkspace (workspace assignment): the machine is bound in workspace_hosts, and
//	    workspace admins can assign it
//	    to any agent in the workspace (the machine owner can also pick one they bound in); it stops
//	    working once unbound from the workspace.
//	HostSourcePersonal (personal direct connect): the machine belongs to the agent's creator, not
//	    going through workspace binding;
//	    only agent.CreatedBy can change this choice (not even admins), and it is unaffected by
//	    workspace unbinding.
//
// So the rules in this file must mirror those in wsintegration word for word; when changing one side,
// keep the other in mind.
package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// executorTriple is the normalized executor choice. The three fields are always checked and assigned
// together.
type executorTriple struct {
	ExecType string
	HostID   string
	Source   string
}

// normalizeExecutor only normalizes shape and validates literals; it does not query the database or
// check roles.
//
// An empty executorType is always a Docker container (also wiping a contradictory hostId, keeping
// the old resolveExecutor's
// tolerance); host must carry a hostId; an empty/unknown hostSource is normalized to workspace,
// the side that needs admin permission, so defaulting there does not widen permissions (existing v1
// rows all have this meaning too).
func normalizeExecutor(d apimodel.AgentDraft) (executorTriple, error) {
	execType := strings.TrimSpace(d.ExecutorType)
	hostID := strings.TrimSpace(d.HostID)
	switch execType {
	case "":
		// "No host selected but a hostId is carried" is a contradictory frontend state; wiping it is
		// friendlier than rejecting.
		return executorTriple{}, nil
	case "host":
		if hostID == "" {
			return executorTriple{}, fmt.Errorf(
				"%w: 选择了宿主机执行端，但没有指定具体的宿主机",
				persistmodel.ErrInvalidArgument)
		}
		return executorTriple{
			ExecType: "host",
			HostID:   hostID,
			Source:   apimodel.EffectiveHostSource(execType, d.HostSource),
		}, nil
	default:
		return executorTriple{}, fmt.Errorf(
			"%w: 执行端类型 %q 不被支持（只能为空或 host）",
			persistmodel.ErrInvalidArgument, execType)
	}
}

// ErrPersonalHostNeedsCreator is the on-screen text for "only the creator can change a personal
// direct-connect machine".
// Same three things as ErrPersonalIntegrationNeedsCreator: why, whom to ask (%s), and the
// alternative.
const ErrPersonalHostNeedsCreator = "这台宿主机是创建者（%s）的个人直连机器，只有创建者本人能更换或移除它。" +
	"它跑在创建者自己的机器上，不属于这个空间，所以空间管理员也不能代为修改。" +
	"你可以请创建者本人来改；如果要让空间统一分配执行机器，请改用「空间指派」。"

// ErrPersonalHostNoCreator is the text for an existing agent (no creator recorded) trying the
// personal direct-connect branch.
// Ownership and terminal permissions of personal direct connect all hang on agent.CreatedBy; with no
// creator there is nothing to hang them on;
// having an admin recreate the agent (making them the creator) is safer than hanging someone else's
// machine on an ownerless agent.
const ErrPersonalHostNoCreator = "这个 Agent 没有记录创建者（它建于本功能上线之前），" +
	"无法配置「个人直连」宿主机——个人直连必须属于 Agent 的创建者。" +
	"请空间管理员改用「空间指派」，或重建一个 Agent 后由创建者直连自己的机器。"

// ErrWorkspaceHostNeedsAdminOrOwner is the 403 text for the workspace assignment level.
const ErrWorkspaceHostNeedsAdminOrOwner = "把空间里的宿主机指派给 Agent，需要空间管理员权限，" +
	"或者这台机器就是你本人注册并绑进空间的。"

// ensureExecutorAllowed is the single rule for "is this executor change allowed", shared by create
// and update.
//
// current is the stored one; on create the caller passes an empty agent with only WorkspaceID +
// CreatedBy=actor filled
// (a fake current, for the same reason as the long comment at the call site of
// ensureIntegrationsAllowed).
//
// ⚠️ **Check "did it change", not "was it sent"**: the frontend's toAgentDraft always sends back the
// full draft,
// and a rule of "block if hostId was sent" would stop non-creators from saving even a rename, while
// the error would talk about
// executor permissions, not pointing at what they were doing at all. This is pinned by a mutation
// test.
func (s *Service) ensureExecutorAllowed(
	ctx context.Context, current apimodel.Agent, d apimodel.AgentDraft,
) (executorTriple, error) {
	want, err := normalizeExecutor(d)
	if err != nil {
		return executorTriple{}, err
	}
	have := executorTriple{
		ExecType: strings.TrimSpace(current.ExecutorType),
		HostID:   strings.TrimSpace(current.HostID),
		Source: apimodel.EffectiveHostSource(
			current.ExecutorType, current.HostSource),
	}
	// Nothing changed, so let it through: non-creators changing the title and members changing the
	// prompt all take this branch.
	if want == have {
		return want, nil
	}

	// Let it through when ctx has no identity: unit tests and system-driven paths (worker
	// inspection, Feishu inbound and other identity-less
	// calls), the same basis as actorIsAdmin.
	actor := strings.TrimSpace(auth.ActorFrom(ctx))
	if actor == "" {
		return want, nil
	}
	if want.ExecType == "host" && s.hosts == nil {
		return executorTriple{}, fmt.Errorf(
			"%w: 服务端未启用宿主机执行端，无法保存", persistmodel.ErrPrecondition)
	}

	personalTouched := have.Source == string(apimodel.HostSourcePersonal) ||
		want.Source == string(apimodel.HostSourcePersonal)

	if personalTouched {
		return s.ensurePersonalExecutorAllowed(ctx, current, actor, have, want)
	}
	return s.ensureWorkspaceExecutorAllowed(ctx, current, actor, have, want)
}

// -- 1. Personal direct connect: only agent.CreatedBy can change it --------------------------------
func (s *Service) ensurePersonalExecutorAllowed(
	ctx context.Context, current apimodel.Agent, actor string,
	have, want executorTriple,
) (executorTriple, error) {
	creator := strings.TrimSpace(current.CreatedBy)
	isCreator := false
	switch {
	case creator == "":
		// An existing agent with no creator recorded. Personal direct connect's "only the creator
		// can change it, only the owner can open the terminal"
		// all hangs on CreatedBy, with nobody to hang it on, so **the personal direct-connect level
		// is rejected outright** (the same migration decision as personal
		// authorization's "have an admin recreate the agent"); the workspace level is unaffected,
		// and admins still use workspace assignment.
		if want.Source == string(apimodel.HostSourcePersonal) {
			return executorTriple{}, fmt.Errorf("%w: %s",
				persistmodel.ErrAccessDenied, ErrPersonalHostNoCreator)
		}
		ok, err := s.actorIsAdmin(ctx, current.WorkspaceID)
		if err != nil {
			return executorTriple{}, err
		}
		if !ok {
			return executorTriple{}, fmt.Errorf("%w: %s",
				persistmodel.ErrAccessDenied, ErrPersonalHostNoCreator)
		}
	case !strings.EqualFold(creator, actor):
		// EqualFold rather than a byte comparison: Actor is already lowercased, but the stored
		// CreatedBy may not be
		// (the same lesson as in ensureIntegrationsAllowed).
		return executorTriple{}, fmt.Errorf("%w: "+ErrPersonalHostNeedsCreator,
			persistmodel.ErrAccessDenied, creator)
	default:
		isCreator = true
	}

	// want is a Docker container (removing the personal machine): passing the identity gate above is
	// enough, no machine lookup needed.
	if want.ExecType != "host" {
		return want, nil
	}

	if want.Source == string(apimodel.HostSourcePersonal) {
		// Personal direct connect can only hang your own machine (admins falling back on old data
		// cannot use this level to hang someone else's either).
		if _, err := s.hosts.ResolvePersonalHost(ctx, actor, want.HostID); err != nil {
			// Do not distinguish "machine does not exist / is not yours"; one message for both,
			// to avoid host id probing.
			return executorTriple{}, fmt.Errorf(
				"%w: 宿主机不存在或不是你本人注册的机器：%s",
				persistmodel.ErrNotFound, want.HostID)
		}
		return want, nil
	}

	// Switching to workspace assignment: the machine must really be bound in this workspace; admins
	// (including old-data fallback) can assign any bound
	// machine, and the creator can only pick the one they own.
	h, err := s.hosts.ResolveBoundHost(ctx, current.WorkspaceID, want.HostID)
	if err != nil {
		return executorTriple{}, fmt.Errorf(
			"%w: 宿主机不存在或未绑定到该空间：%s",
			persistmodel.ErrNotFound, want.HostID)
	}
	if isCreator && !strings.EqualFold(h.OwnerEmail, actor) {
		ok, err := s.actorIsAdmin(ctx, current.WorkspaceID)
		if err != nil {
			return executorTriple{}, err
		}
		if !ok {
			return executorTriple{}, fmt.Errorf("%w: %s",
				persistmodel.ErrAccessDenied, ErrWorkspaceHostNeedsAdminOrOwner)
		}
	}
	return want, nil
}

// -- 2. Workspace assignment: admins can assign any bound machine; the machine owner can change their own ----------------
func (s *Service) ensureWorkspaceExecutorAllowed(
	ctx context.Context, current apimodel.Agent, actor string,
	have, want executorTriple,
) (executorTriple, error) {
	isAdmin, err := s.actorIsAdmin(ctx, current.WorkspaceID)
	if err != nil {
		return executorTriple{}, err
	}

	// Removing the executor (host → container): admins can always remove it (when the machine has
	// been unbound, an admin is needed to rescue the agent);
	// non-admins can only remove it while the old machine is still bound in the workspace and they
	// own the old machine.
	// When the host domain is not wired, owner/binding cannot be verified, and "switch back to cloud
	// container" is itself a narrowing of permissions, so let it through
	// (the want=host case was already rejected explicitly above, and never gets here).
	if want.ExecType != "host" {
		if isAdmin || s.hosts == nil {
			return want, nil
		}
		h, err := s.hosts.ResolveBoundHost(ctx, current.WorkspaceID, have.HostID)
		if err != nil {
			return executorTriple{}, fmt.Errorf("%w: %s",
				persistmodel.ErrAccessDenied, ErrWorkspaceHostNeedsAdminOrOwner)
		}
		if !strings.EqualFold(h.OwnerEmail, actor) {
			return executorTriple{}, fmt.Errorf("%w: %s",
				persistmodel.ErrAccessDenied, ErrWorkspaceHostNeedsAdminOrOwner)
		}
		return want, nil
	}

	// Picking/changing a workspace-assigned machine: it must still be bound in this workspace, and
	// the actor must be an admin or the machine owner.
	h, err := s.hosts.ResolveBoundHost(ctx, current.WorkspaceID, want.HostID)
	if err != nil {
		return executorTriple{}, fmt.Errorf(
			"%w: 宿主机不存在或未绑定到该空间：%s",
			persistmodel.ErrNotFound, want.HostID)
	}
	if !isAdmin && !strings.EqualFold(h.OwnerEmail, actor) {
		return executorTriple{}, fmt.Errorf("%w: %s",
			persistmodel.ErrAccessDenied, ErrWorkspaceHostNeedsAdminOrOwner)
	}
	return want, nil
}
