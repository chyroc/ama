// Permission tests for the two-level host binding at two runtime entry points: "recheck on
// session creation" and "web terminal ticket".
//
// Paired with hostscope_test.go (write paths):
//   - Session creation: personal direct connection only checks the machine still exists
//     (unbinding the workspace has no effect); workspace assignment checks the workspace binding;
//   - Terminal ticket: container/workspace machine = workspace admin; personal machine = the
//     machine's owner in person (viewer is fine too).
package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/repository/dao/coremem"
	hostsvc "github.com/chyroc/ama/server/service/host"
	"github.com/chyroc/ama/server/shared/hostproto"
)

// hostRuntimeFixture sets up a Service with a real host domain, a fake Ark runner, and a workspace Ark key.
type hostRuntimeFixture struct {
	t     *testing.T
	svc   *Service
	store *coremem.MemStore
	hs    *hostsvc.Service
}

func newHostRuntimeFixture(t *testing.T) *hostRuntimeFixture {
	t.Helper()
	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败：%v", err)
	}
	hs, err := hostsvc.New(hostsvc.Config{Store: store})
	if err != nil {
		t.Fatalf("建 host 域失败：%v", err)
	}
	svc, err := New(Config{Store: store, ArkRunner: &fakeRunner{}, BlobStore: newFakeBlobStore(),
		HostBindings: hs})
	if err != nil {
		t.Fatalf("装配 Service 失败：%v", err)
	}
	svc.SetHostTerminalDispatcher(hs)
	if err := store.PutArkSecret(context.Background(), apimodel.ArkSecret{
		ArkCredential: apimodel.ArkCredential{WorkspaceID: seedWS, Region: apimodel.RegionCNBeijing},
		APIKey:        "ARK0123456789abcdef",
	}); err != nil {
		t.Fatalf("写方舟凭证失败：%v", err)
	}
	return &hostRuntimeFixture{t: t, svc: svc, store: store, hs: hs}
}

// registerHost registers a machine and marks it online (within the 90s heartbeat grace period).
func (f *hostRuntimeFixture) registerHost(owner, uuid string) persistmodel.Host {
	f.t.Helper()
	now := time.Now()
	resp, err := f.hs.Register(context.Background(), owner, hostproto.RegisterRequest{
		HostUUID: uuid, Name: uuid + "-mac", OS: "darwin", Arch: "arm64",
		DaemonVersion: "test", GNUToolsOK: true,
	}, now)
	if err != nil {
		f.t.Fatalf("注册机器失败：%v", err)
	}
	if err := f.store.TouchHost(context.Background(), resp.HostID,
		hostsvc.HostHeartbeatSnapshot{}, hostsvc.StatusOnline, now); err != nil {
		f.t.Fatalf("刷在线失败：%v", err)
	}
	h, err := f.hs.GetHost(context.Background(), resp.HostID)
	if err != nil {
		f.t.Fatalf("读回机器失败：%v", err)
	}
	return h
}

// seedHostRun stores a run with a host snapshot directly (skips Ark and the session recheck; tests terminal tickets only).
func (f *hostRuntimeFixture) seedHostRun(runID, hostID, source string) apimodel.Run {
	f.t.Helper()
	now := apimodel.NowISO()
	run := apimodel.Run{
		ID: runID, WorkspaceID: seedWS, AgentID: "agent_triage", AgentName: "x",
		Status: apimodel.RunRunning, Trigger: apimodel.TriggerManual,
		StartedAt: now, LastActivityAt: now,
		HostID: hostID, HostSource: source,
	}
	if _, err := f.store.CreateRun(context.Background(), run); err != nil {
		f.t.Fatalf("落 run 失败：%v", err)
	}
	return run
}

// ---- Session creation recheck -----------------------------------------------

// Personal direct: machine exists, even never bound to a workspace; session creation succeeds and the run snapshots personal.
func TestConversationPersonalHostNeedsNoBinding(t *testing.T) {
	f := newHostRuntimeFixture(t)
	h := f.registerHost(seedMember, "uuid-personal-run")

	a, err := f.svc.CreateAgent(asActor(seedMember), seedWS, apimodel.AgentDraft{
		Name: "个人直连会话", Model: apimodel.ModelConfig{ID: "doubao-pro"},
		ExecutorType: "host", HostID: h.ID, HostSource: "personal",
	})
	if err != nil {
		t.Fatalf("创建者个人直连建 agent 失败：%v", err)
	}
	run, err := f.svc.CreateConversation(asActor(seedMember), seedWS, a.ID)
	if err != nil {
		t.Fatalf("个人直连机器未绑空间也应能建会话：%v", err)
	}
	if run.HostSource != string(apimodel.HostSourcePersonal) || run.HostID != h.ID {
		t.Errorf("run 快照错：host=%q source=%q", run.HostID, run.HostSource)
	}
}

// Personal direct: after the owner revokes the machine, session creation is blocked.
func TestConversationPersonalHostDeletedBlocked(t *testing.T) {
	f := newHostRuntimeFixture(t)
	h := f.registerHost(seedMember, "uuid-personal-del")
	a, err := f.svc.CreateAgent(asActor(seedMember), seedWS, apimodel.AgentDraft{
		Name: "个人直连删机", Model: apimodel.ModelConfig{ID: "doubao-pro"},
		ExecutorType: "host", HostID: h.ID, HostSource: "personal",
	})
	if err != nil {
		t.Fatalf("前置建 agent 失败：%v", err)
	}
	if err := f.hs.DeleteHost(context.Background(), seedMember, h.ID); err != nil {
		t.Fatalf("吊销机器失败：%v", err)
	}
	if _, err := f.svc.CreateConversation(asActor(seedMember), seedWS, a.ID); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("机器删除后建会话应回 precondition，实际 %v", err)
	}
}

// Workspace assignment: after the workspace unbinds, session creation is blocked (v1 semantics do not fall back).
func TestConversationWorkspaceHostUnboundBlocked(t *testing.T) {
	f := newHostRuntimeFixture(t)
	h := f.registerHost(seedAdmin, "uuid-ws-run")
	if err := f.hs.BindHost(context.Background(), seedAdmin, seedWS, h.ID, time.Now()); err != nil {
		t.Fatalf("绑定失败：%v", err)
	}
	a, err := f.svc.CreateAgent(asActor(seedAdmin), seedWS, apimodel.AgentDraft{
		Name: "空间指派", Model: apimodel.ModelConfig{ID: "doubao-pro"},
		ExecutorType: "host", HostID: h.ID, HostSource: "workspace",
	})
	if err != nil {
		t.Fatalf("前置建 agent 失败：%v", err)
	}
	if err := f.hs.UnbindHost(context.Background(), seedWS, h.ID); err != nil {
		t.Fatalf("解绑失败：%v", err)
	}
	if _, err := f.svc.CreateConversation(asActor(seedAdmin), seedWS, a.ID); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("空间解绑后建会话应回 precondition，实际 %v", err)
	}
}

// ---- Terminal ticket levels -------------------------------------------------

// Personal direct machine: only the owner can get a ticket, even as a viewer; an admin cannot.
func TestTerminalTicketPersonalHostOwnerOnly(t *testing.T) {
	f := newHostRuntimeFixture(t)
	h := f.registerHost(seedViewer, "uuid-personal-term")
	f.seedHostRun("run_pterm", h.ID, "personal")

	if _, err := f.svc.IssueTerminalTicket(asActor(seedViewer), "run_pterm"); err != nil {
		t.Fatalf("机器属主（viewer 身份）应能开自己的个人终端：%v", err)
	}
	if _, err := f.svc.IssueTerminalTicket(asActor(seedAdmin), "run_pterm"); !errors.Is(err, persistmodel.ErrAccessDenied) {
		t.Fatalf("非属主 admin 应被拒，实际 %v", err)
	}
	if _, err := f.svc.IssueTerminalTicket(asActor(seedMember), "run_pterm"); !errors.Is(err, persistmodel.ErrAccessDenied) {
		t.Fatalf("非属主 member 应被拒，实际 %v", err)
	}
}

// Workspace-assigned machine: keeps the workspace admin bar; member is rejected.
func TestTerminalTicketWorkspaceHostNeedsAdmin(t *testing.T) {
	f := newHostRuntimeFixture(t)
	h := f.registerHost(seedMember, "uuid-ws-term")
	f.seedHostRun("run_wsterm", h.ID, "workspace")

	if _, err := f.svc.IssueTerminalTicket(asActor(seedAdmin), "run_wsterm"); err != nil {
		t.Fatalf("空间管理员应能开空间宿主机终端：%v", err)
	}
	if _, err := f.svc.IssueTerminalTicket(asActor(seedMember), "run_wsterm"); !errors.Is(err, persistmodel.ErrAccessDenied) {
		t.Fatalf("member 开空间宿主机终端应被拒，实际 %v", err)
	}
}

// Existing runs (empty host_source) are treated as workspace-assigned: admin required.
func TestTerminalTicketBlankSourceActsAsWorkspace(t *testing.T) {
	f := newHostRuntimeFixture(t)
	h := f.registerHost(seedMember, "uuid-blank-term")
	f.seedHostRun("run_blank", h.ID, "")

	if _, err := f.svc.IssueTerminalTicket(asActor(seedMember), "run_blank"); !errors.Is(err, persistmodel.ErrAccessDenied) {
		t.Fatalf("空 source run 应按空间档（member 拒），实际 %v", err)
	}
}

// Personal machine offline / deleted: even the owner cannot get one (412-like), rather than a ticket being signed quietly.
func TestTerminalTicketPersonalHostOfflineAndDeleted(t *testing.T) {
	f := newHostRuntimeFixture(t)

	// Offline: registered but no heartbeat.
	resp, err := f.hs.Register(context.Background(), seedMember, hostproto.RegisterRequest{
		HostUUID: "uuid-offline-term", Name: "offline", OS: "darwin", Arch: "arm64",
	}, time.Now())
	if err != nil {
		t.Fatalf("注册失败：%v", err)
	}
	f.seedHostRun("run_off", resp.HostID, "personal")
	if _, err := f.svc.IssueTerminalTicket(asActor(seedMember), "run_off"); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("离线机器应回 precondition，实际 %v", err)
	}

	// Deleted: after an online machine is deleted.
	h := f.registerHost(seedMember, "uuid-del-term")
	f.seedHostRun("run_del", h.ID, "personal")
	if err := f.hs.DeleteHost(context.Background(), seedMember, h.ID); err != nil {
		t.Fatalf("吊销失败：%v", err)
	}
	if _, err := f.svc.IssueTerminalTicket(asActor(seedMember), "run_del"); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("已删除机器应回 precondition，实际 %v", err)
	}
}
