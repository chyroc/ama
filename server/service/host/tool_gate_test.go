package host_test

import (
	"context"
	"testing"
	"time"

	hostsvc "github.com/chyroc/ama/server/service/host"
	"github.com/chyroc/ama/server/shared/hostproto"
	"github.com/chyroc/ama/server/shared/hosttools"
)

// The two pre-check gates of ToolCall: host offline, and daemon does not support the native protocol. Both must
// return an IsError result before enqueueing, instead of making the call wait for the 130s timeout.
func TestToolCallGate(t *testing.T) {
	fx := newStreamFixture(t)
	svc := fx.svc
	ctx := context.Background()
	native := true
	old := false

	// Online + old daemon (NativeTools=false) → rejected by the capability gate.
	if err := fx.store.TouchHost(ctx, fx.host.ID, hostsvc.HostHeartbeatSnapshot{
		NativeTools: &old,
	}, "online", time.Now()); err != nil {
		t.Fatalf("touch old: %v", err)
	}
	res, err := svc.ToolCall(ctx, fx.host.ID, "run_1", hosttools.ToolCall{Name: hosttools.NameBash})
	if err != nil {
		t.Fatalf("ToolCall 不该返 Go error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("旧 daemon 应回 IsError，实际 %+v", res)
	}

	// Supports the native protocol but heartbeat expired (offline) → rejected by the online gate.
	if err := fx.store.TouchHost(ctx, fx.host.ID, hostsvc.HostHeartbeatSnapshot{
		NativeTools: &native,
	}, "online", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("touch stale: %v", err)
	}
	res, err = svc.ToolCall(ctx, fx.host.ID, "run_1", hosttools.ToolCall{Name: hosttools.NameRead})
	if err != nil || !res.IsError {
		t.Fatalf("离线主机应直接失败: %+v %v", res, err)
	}

	// Online + new daemon: pre-check passes (enters waiting; with no daemon returning a result it waits until ctx is cancelled;
	// a short ctx verifies it really "passed the gate and was enqueued" instead of returning a capability error at once).
	if err := fx.store.TouchHost(ctx, fx.host.ID, hostsvc.HostHeartbeatSnapshot{
		NativeTools: &native,
	}, "online", time.Now()); err != nil {
		t.Fatalf("touch new: %v", err)
	}
	shortCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	res, err = svc.ToolCall(shortCtx, fx.host.ID, "run_1", hosttools.ToolCall{Name: hosttools.NameBash})
	if err != nil {
		t.Fatalf("新 daemon 预检应放行: %v", err)
	}
	if !res.IsError || res.Text == "" {
		t.Fatalf("等不到结果应回取消类失败文本: %+v", res)
	}
}

// An empty HostUUID must be rejected: otherwise several machines of the same owner collapse into one row by empty uuid.
func TestRegisterRejectsEmptyUUID(t *testing.T) {
	fx := newStreamFixture(t)
	_, err := fx.svc.Register(context.Background(), "a@example.com",
		hostproto.RegisterRequest{HostUUID: "   ", Name: "x", OS: "darwin"}, time.Now())
	if err == nil {
		t.Fatal("空 HostUUID 应注册失败")
	}
}
