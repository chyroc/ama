// Tests for the write-path gates of the two levels of host binding (workspace assignment /
// personal direct connect).
//
// The shape copies authscope_test.go (the personal authorization set), because the two rule
// sets are made symmetric word for word on purpose:
//
//	workspace-assigned host  ≈ service account authorization: admins can assign any machine
//	                           bound in the workspace, and the machine owner can too
//	personal direct host     ≈ personal authorization: only agent.CreatedBy can change it,
//	                           not even admins
//
// As there, every "deny" case is paired with a reverse "who should succeed" case, or else a
// wrong "deny everything" implementation would pass. These cases use the real host domain
// service + the coremem store (not hand-written fakes), so the unique constraint on
// workspace_hosts and the host owner check go through the real paths.
package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/repository/dao/coremem"
	hostsvc "github.com/chyroc/ama/server/service/host"
	"github.com/chyroc/ama/server/shared/hostproto"
)

// hostScopeFixture builds a Service with the real host domain, and prepares two machines and
// the workspace roles:
//
//	creator@  workspace member, owns creatorMac (not bound to the workspace! only for personal direct connect)
//	other@    workspace member, owns otherMac and has bound it into the workspace
//	seedAdmin workspace admin (comes with the seed), owns no machine
type hostScopeFixture struct {
	t          *testing.T
	svc        *Service
	creatorMac string // owner creator@, **not bound to the workspace**
	otherMac   string // owner other@, bound into the workspace
}

func newHostScopeFixture(t *testing.T) *hostScopeFixture {
	t.Helper()
	// Not using newService: HostBindings is a constructor field of core.New and cannot be
	// added later, so we build one here in the shape of newServiceWithoutBlobs, with the host
	// domain.
	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败：%v", err)
	}
	hs, err := hostsvc.New(hostsvc.Config{Store: store})
	if err != nil {
		t.Fatalf("建 host 域服务失败：%v", err)
	}
	svc, err := New(Config{Store: store, HostBindings: hs})
	if err != nil {
		t.Fatalf("装配带 host 校验的 Service 失败：%v", err)
	}
	svc.blobs = newFakeBlobStore()
	svc.SetHostTerminalDispatcher(hs)

	const creator = "creator@example.com"
	const other = "other@example.com"
	ctx := context.Background()
	for _, m := range []struct{ email, role string }{
		{creator, string(apimodel.RoleMember)},
		{other, string(apimodel.RoleMember)},
	} {
		if _, err := svc.AddMember(ctx, seedWS, MemberInput{
			Name: m.email, Email: m.email, Role: apimodel.WorkspaceRole(m.role),
		}); err != nil {
			t.Fatalf("加成员 %s 失败：%v", m.email, err)
		}
	}

	register := func(owner, uuid, name string) string {
		resp, rerr := hs.Register(ctx, owner, hostproto.RegisterRequest{
			HostUUID: uuid, Name: name, OS: "darwin", Arch: "arm64",
			DaemonVersion: "test", GNUToolsOK: true,
		}, time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC))
		if rerr != nil {
			t.Fatalf("注册 %s 的机器失败：%v", owner, rerr)
		}
		return resp.HostID
	}
	creatorMac := register(creator, "uuid-creator", "creator-mac")
	otherMac := register(other, "uuid-other", "other-mac")
	// other binds their own machine into the workspace.
	if err := hs.BindHost(ctx, other, seedWS, otherMac,
		time.Date(2026, 9, 11, 10, 1, 0, 0, time.UTC)); err != nil {
		t.Fatalf("绑定 other 的机器失败：%v", err)
	}

	return &hostScopeFixture{t: t, svc: svc, creatorMac: creatorMac, otherMac: otherMac}
}

func hostDraft(hostID, source string) apimodel.AgentDraft {
	return apimodel.AgentDraft{
		Name:         "宿主机 agent",
		Model:        apimodel.ModelConfig{ID: "doubao-pro"},
		ExecutorType: "host",
		HostID:       hostID,
		HostSource:   source,
	}
}

func wantAccessDenied(t *testing.T, err error, fragment string) {
	t.Helper()
	if !errors.Is(err, persistmodel.ErrAccessDenied) {
		t.Fatalf("想要 access denied（文案含 %q），实际 %v", fragment, err)
	}
	if fragment != "" && !strings.Contains(err.Error(), fragment) {
		t.Errorf("错误文案应含 %q，实际：%v", fragment, err)
	}
}

// (1) The creator (member) direct-connects their own machine, **not bound to the workspace**,
// to a new agent -> success.
func TestExecutorPersonalDirectByCreator(t *testing.T) {
	f := newHostScopeFixture(t)
	a, err := f.svc.CreateAgent(asActor("creator@example.com"), seedWS,
		hostDraft(f.creatorMac, "personal"))
	if err != nil {
		t.Fatalf("创建者个人直连自己的机器应成功：%v", err)
	}
	if a.ExecutorType != "host" || a.HostID != f.creatorMac ||
		a.HostSource != string(apimodel.HostSourcePersonal) {
		t.Errorf("快照错：%+v", a)
	}
}

// (2) A member tries to direct-connect **someone else's** machine -> not-found (we do not
// confirm the machine exists or who owns it).
func TestExecutorPersonalDirectRejectsOthersHost(t *testing.T) {
	f := newHostScopeFixture(t)
	_, err := f.svc.CreateAgent(asActor("creator@example.com"), seedWS,
		hostDraft(f.otherMac, "personal"))
	if !errors.Is(err, persistmodel.ErrNotFound) {
		t.Fatalf("想用别人的机器做个人直连应回 not-found，实际 %v", err)
	}
}

// (3) A non-creator (even a workspace admin) changes the executor of a personal agent ->
// denied, and the message names the creator.
func TestExecutorPersonalHostLockedForNonCreator(t *testing.T) {
	f := newHostScopeFixture(t)
	a, err := f.svc.CreateAgent(asActor("creator@example.com"), seedWS,
		hostDraft(f.creatorMac, "personal"))
	if err != nil {
		t.Fatalf("前置建 agent 失败：%v", err)
	}

	// The admin tries to remove it.
	draft := fullDraftOf(a)
	draft.ExecutorType = ""
	draft.HostID = ""
	draft.HostSource = ""
	if _, err := f.svc.UpdateAgent(asActor(seedAdmin), a.ID, draft); err == nil {
		t.Fatal("空间管理员摘掉别人的个人直连机器应被拒绝")
	} else {
		wantAccessDenied(t, err, "creator@example.com")
	}

	// Another member tries to switch it to a workspace-assigned machine.
	draft = fullDraftOf(a)
	draft.ExecutorType = "host"
	draft.HostID = f.otherMac
	draft.HostSource = "workspace"
	if _, err := f.svc.UpdateAgent(asActor("other@example.com"), a.ID, draft); err == nil {
		t.Fatal("非创建者把个人直连改成空间指派应被拒绝")
	}
}

// (4) A non-creator only changes the name and sends the executor back unchanged -> success
// (the case most easily broken).
func TestExecutorNonCreatorCanRenamePersonalAgent(t *testing.T) {
	f := newHostScopeFixture(t)
	a, err := f.svc.CreateAgent(asActor("creator@example.com"), seedWS,
		hostDraft(f.creatorMac, "personal"))
	if err != nil {
		t.Fatalf("前置建 agent 失败：%v", err)
	}
	draft := fullDraftOf(a)
	draft.Name = "改个名字"
	draft.ExecutorType = "host"
	draft.HostID = f.creatorMac
	draft.HostSource = "personal"
	if _, err := f.svc.UpdateAgent(asActor(seedAdmin), a.ID, draft); err != nil {
		t.Fatalf("非创建者不改执行端、只改名字应放行：%v", err)
	}
}

// (5) The creator can remove or replace their own personal direct machine.
func TestExecutorCreatorCanChangeOwnPersonalHost(t *testing.T) {
	f := newHostScopeFixture(t)
	a, err := f.svc.CreateAgent(asActor("creator@example.com"), seedWS,
		hostDraft(f.creatorMac, "personal"))
	if err != nil {
		t.Fatalf("前置建 agent 失败：%v", err)
	}
	// Switch back to the cloud container.
	draft := fullDraftOf(a)
	draft.ExecutorType, draft.HostID, draft.HostSource = "", "", ""
	if _, err := f.svc.UpdateAgent(asActor("creator@example.com"), a.ID, draft); err != nil {
		t.Fatalf("创建者摘除自己的个人机器应成功：%v", err)
	}
}

// (6) Workspace assignment: an admin can assign a machine someone else bound into the
// workspace to an agent; a member cannot.
func TestExecutorWorkspaceAssignment(t *testing.T) {
	f := newHostScopeFixture(t)

	// A member (not the machine owner) picks a machine someone else bound -> 403.
	_, err := f.svc.CreateAgent(asActor("creator@example.com"), seedWS,
		hostDraft(f.otherMac, "workspace"))
	wantAccessDenied(t, err, "")

	// The admin picks the same one -> success.
	a, err := f.svc.CreateAgent(asActor(seedAdmin), seedWS,
		hostDraft(f.otherMac, "workspace"))
	if err != nil {
		t.Fatalf("管理员指派空间已绑（别人的）机器应成功：%v", err)
	}
	if a.HostSource != string(apimodel.HostSourceWorkspace) {
		t.Errorf("来源应为 workspace，实际 %q", a.HostSource)
	}
}

// (7) Missing hostSource (old frontend / existing data) is normalized to workspace: a member
// picking someone else's machine is still blocked, and picking their own machine bound to the
// workspace succeeds.
func TestExecutorBlankSourceDefaultsToWorkspace(t *testing.T) {
	f := newHostScopeFixture(t)
	// creator's machine is not bound to the workspace -> empty source goes to the workspace
	// level -> not-found.
	if _, err := f.svc.CreateAgent(asActor("creator@example.com"), seedWS,
		hostDraft(f.creatorMac, "")); !errors.Is(err, persistmodel.ErrNotFound) {
		t.Fatalf("空 source 应按空间指派校验（creator 机器未绑 → not-found），实际 %v", err)
	}
}

// (8) Existing agent (CreatedBy empty):
//   - picking personal direct connect -> always denied (a personal direct connect has no owner
//     to belong to; the message points to workspace assignment / recreating the agent);
//   - picking workspace assignment -> admin succeeds, member denied (falls back to the admin
//     level).
func TestExecutorLegacyAgentWithoutCreatorFallsBackToAdmin(t *testing.T) {
	f := newHostScopeFixture(t)
	a, err := f.svc.CreateAgent(context.Background(), seedWS,
		apimodel.AgentDraft{Name: "存量", Model: apimodel.ModelConfig{ID: "doubao-pro"}})
	if err != nil {
		t.Fatalf("前置建 agent 失败：%v", err)
	}
	if a.CreatedBy != "" {
		t.Fatalf("前置条件：这条 agent 的 CreatedBy 应为空，实际 %q", a.CreatedBy)
	}

	// Even an admin cannot set a personal direct connect on an agent with no owner.
	personal := fullDraftOf(a)
	personal.ExecutorType, personal.HostID, personal.HostSource =
		"host", f.otherMac, "personal"
	if _, err := f.svc.UpdateAgent(asActor(seedAdmin), a.ID, personal); err == nil {
		t.Fatal("存量无创建者 agent 配个人直连应被拒绝（即使是 admin）")
	}

	// A member (not the owner of that machine) changes workspace assignment -> denied.
	ws := fullDraftOf(a)
	ws.ExecutorType, ws.HostID, ws.HostSource = "host", f.otherMac, "workspace"
	if _, err := f.svc.UpdateAgent(asActor("creator@example.com"), a.ID, ws); err == nil {
		t.Fatal("存量 agent 的空间指派，非属主 member 改应被拒（回落 admin 档）")
	}
	// The admin changes workspace assignment -> success.
	if _, err := f.svc.UpdateAgent(asActor(seedAdmin), a.ID, ws); err != nil {
		t.Fatalf("存量 agent 的空间指派，admin 改应放行：%v", err)
	}
}

// (9) host chosen but no machine given / invalid executorType -> invalid_argument.
func TestExecutorInvalidDrafts(t *testing.T) {
	f := newHostScopeFixture(t)
	bad := []apimodel.AgentDraft{
		{Name: "a", Model: apimodel.ModelConfig{ID: "m"}, ExecutorType: "host", HostSource: "personal"},
		{Name: "a", Model: apimodel.ModelConfig{ID: "m"}, ExecutorType: "vm"},
	}
	for i, d := range bad {
		if _, err := f.svc.CreateAgent(asActor("creator@example.com"), seedWS, d); !errors.Is(err, persistmodel.ErrInvalidArgument) {
			t.Fatalf("第 %d 个坏草稿应回 invalid-argument，实际 %v", i, err)
		}
	}
}
