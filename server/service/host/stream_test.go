// Tests for host.Service's WS streaming session.
//
// Uses coremem's in-memory store (runs offline) + a fake Waiter that can be woken by hand.
// Note this file must be the external test package host_test: coremem imports host in turn
// (it implements the host.Store contract), so an in-package test would create a cycle.
package host_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/repository/dao/coremem"
	hostsvc "github.com/chyroc/ama/server/service/host"
	"github.com/chyroc/ama/server/shared/hostproto"
)

// fakeWaiter mimics core.HostWakes: Wait registers a chan with buffered=1, wake broadcasts
// by closing it; it unregisters automatically when ctx ends.
type fakeWaiter struct {
	mu    sync.Mutex
	chans map[string][]chan struct{}
	wakeN int
}

func newFakeWaiter() *fakeWaiter {
	return &fakeWaiter{chans: make(map[string][]chan struct{})}
}

func (w *fakeWaiter) Wait(ctx context.Context, hostID string) <-chan struct{} {
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	w.chans[hostID] = append(w.chans[hostID], ch)
	w.mu.Unlock()
	go func() {
		<-ctx.Done()
		w.mu.Lock()
		defer w.mu.Unlock()
		list := w.chans[hostID]
		for i, c := range list {
			if c == ch {
				w.chans[hostID] = append(list[:i], list[i+1:]...)
				break
			}
		}
	}()
	return ch
}

func (w *fakeWaiter) wake(hostID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ch := range w.chans[hostID] {
		close(ch)
	}
	delete(w.chans, hostID)
	w.wakeN++
}

type streamFixture struct {
	t      *testing.T
	svc    *hostsvc.Service
	store  *coremem.MemStore
	waiter *fakeWaiter
	host   persistmodel.Host
	now    time.Time
	nextID int
}

func newStreamFixture(t *testing.T) *streamFixture {
	t.Helper()
	store := coremem.NewMemStore()
	waiter := newFakeWaiter()
	svc, err := hostsvc.New(hostsvc.Config{Store: store, Wait: waiter})
	if err != nil {
		t.Fatalf("建 host service 失败: %v", err)
	}
	base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	resp, err := svc.Register(context.Background(), "a@example.com", hostproto.RegisterRequest{
		HostUUID: "uuid-1", Name: "dev-mac", OS: "darwin", Arch: "arm64", DaemonVersion: "test",
	}, base)
	if err != nil {
		t.Fatalf("注册主机失败: %v", err)
	}
	h, err := svc.AuthenticateToken(context.Background(), resp.Token)
	if err != nil {
		t.Fatalf("用新 token 换主机失败: %v", err)
	}
	return &streamFixture{t: t, svc: svc, store: store, waiter: waiter, host: h, now: base}
}

func (f *streamFixture) enqueueExec() hostproto.Dispatch {
	f.t.Helper()
	f.nextID++
	id := "htc_" + string(rune('a'+f.nextID-1)) + "001"
	seq, err := f.store.EnqueueDispatch(context.Background(), persistmodel.HostDispatch{
		ID: id, HostID: f.host.ID, Kind: hostproto.KindExec, SessionID: "run_1",
		Payload: `{"path":"/bin/true"}`, CreatedAt: f.now,
	})
	if err != nil {
		f.t.Fatalf("入队失败: %v", err)
	}
	return hostproto.Dispatch{ID: id, Seq: seq, Kind: hostproto.KindExec, SessionID: "run_1"}
}

func helloReq(seq int64) hostproto.HeartbeatRequest {
	return hostproto.HeartbeatRequest{LastSeq: seq, Name: "dev-mac", OS: "darwin", Arch: "arm64",
		DaemonVersion: "test", GNUToolsOK: boolPtr(true)}
}

func boolPtr(b bool) *bool { return &b }

// The first touch of BeginStream sets the machine back to online, refreshes last_seen_at, and claims the first batch of queued.
func TestStreamBeginTouchesAndClaims(t *testing.T) {
	f := newStreamFixture(t)
	want := f.enqueueExec()

	st, resp, err := f.svc.BeginStream(context.Background(), f.host, helloReq(0), f.now.Add(time.Second))
	if err != nil {
		t.Fatalf("BeginStream 失败: %v", err)
	}
	defer st.Close()
	if len(resp.Calls) != 1 || resp.Calls[0].ID != want.ID || resp.Calls[0].Seq != want.Seq {
		t.Fatalf("首批认领不对: %+v", resp.Calls)
	}
	got, err := f.store.GetHost(context.Background(), f.host.ID)
	if err != nil {
		t.Fatalf("回读主机失败: %v", err)
	}
	if got.Status != hostsvc.StatusOnline || !got.LastSeenAt.Equal(f.now.Add(time.Second)) {
		t.Fatalf("touch 未生效: status=%q lastSeen=%v", got.Status, got.LastSeenAt)
	}
}

// Cursor: after hello carries a new LastSeq, queued rows with seq≤cursor are no longer returned; a new row is picked up by Claim
// after NOTIFY wakes Wait.
func TestStreamCursorAndWakeClaim(t *testing.T) {
	f := newStreamFixture(t)
	d1 := f.enqueueExec()

	st, resp, err := f.svc.BeginStream(context.Background(), f.host, helloReq(0), f.now)
	if err != nil || len(resp.Calls) != 1 || resp.Calls[0].Seq != d1.Seq {
		t.Fatalf("首批不对: %+v err=%v", resp.Calls, err)
	}
	// The daemon is done; hello advances the cursor: nothing new.
	ack, err := st.Hello(context.Background(), helloReq(d1.Seq), f.now.Add(20*time.Second))
	if err != nil || len(ack.Calls) != 0 {
		t.Fatalf("游标推进后不应再拿到 d1: %+v err=%v", ack.Calls, err)
	}

	// One more row: Wait blocks first, and Claim gets it after wake.
	d2 := f.enqueueExec()
	woke := make(chan bool, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		woke <- st.Wait(ctx)
	}()
	// Give the goroutine a moment to register the waiter.
	time.Sleep(20 * time.Millisecond)
	f.waiter.wake(f.host.ID)
	if !<-woke {
		t.Fatal("Wait 应在 NOTIFY 唤醒后返回 true")
	}
	calls, err := st.Claim(context.Background(), f.now)
	if err != nil || len(calls) != 1 || calls[0].Seq != d2.Seq {
		t.Fatalf("唤醒后认领不对: %+v err=%v", calls, err)
	}
}

// Wait returns false when ctx is cancelled, and does not depend on the waiter (in the mem tier Wait=nil and the tick is the fallback;
// here a short ctx tests the cancel behavior directly).
func TestStreamWaitCancel(t *testing.T) {
	f := newStreamFixture(t)
	st, _, err := f.svc.BeginStream(context.Background(), f.host, helloReq(0), f.now)
	if err != nil {
		t.Fatalf("BeginStream 失败: %v", err)
	}
	defer st.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if st.Wait(ctx) {
		t.Fatal("ctx 结束时 Wait 应返回 false")
	}
}

// delivered rows whose claimed lease expired must be redelivered (the recovery path for daemon crash / lost results).
func TestStreamExpiredDeliveryRedelivery(t *testing.T) {
	f := newStreamFixture(t)
	d1 := f.enqueueExec()

	st, resp, err := f.svc.BeginStream(context.Background(), f.host, helloReq(0), f.now)
	if err != nil || len(resp.Calls) != 1 {
		t.Fatalf("首批不对: %+v err=%v", resp.Calls, err)
	}
	// The cursor advances to d1 and time passes the exec lease (150s): the old row is redelivered as expired and returned again,
	// regardless of the cursor; the daemon side relies on callID idempotency.
	ack, err := st.Hello(context.Background(), helloReq(d1.Seq), f.now.Add(151*time.Second))
	if err != nil {
		t.Fatalf("Hello 失败: %v", err)
	}
	if len(ack.Calls) != 1 || ack.Calls[0].ID != d1.ID {
		t.Fatalf("过期 delivered 行应重投: %+v", ack.Calls)
	}
}

// A new connection from the same host kicks out the old one: the old session's Done is closed and Replaced=true; after the old connection's
// Close the new connection is still registered (Done open, can keep sending Hello).
func TestStreamRegistryReplacesOld(t *testing.T) {
	f := newStreamFixture(t)
	old, _, err := f.svc.BeginStream(context.Background(), f.host, helloReq(0), f.now)
	if err != nil {
		t.Fatalf("第一个 BeginStream 失败: %v", err)
	}
	select {
	case <-old.Done():
		t.Fatal("旧会话不应一开始就关闭")
	default:
	}

	newSt, _, err := f.svc.BeginStream(context.Background(), f.host, helloReq(0), f.now.Add(time.Second))
	if err != nil {
		t.Fatalf("第二个 BeginStream 失败: %v", err)
	}
	defer newSt.Close()

	select {
	case <-old.Done():
	case <-time.After(time.Second):
		t.Fatal("新连接建立后旧会话 Done 应关闭")
	}
	if !old.Replaced() {
		t.Fatal("旧会话应报告 Replaced=true（close 4002）")
	}
	old.Close() // The old connection finishes normally; it must not remove the new connection.
	select {
	case <-newSt.Done():
		t.Fatal("旧连接 Close 不应摘掉新连接")
	default:
	}
	if _, err := newSt.Hello(context.Background(), helloReq(0), f.now.Add(2*time.Second)); err != nil {
		t.Fatalf("新连接在旧连接 Close 后应仍可用: %v", err)
	}
}

// On hello renewal the host has been deleted: returns an error wrapping ErrNotFound (the handler uses it for revoked).
func TestStreamHelloAfterHostDeleted(t *testing.T) {
	f := newStreamFixture(t)
	st, _, err := f.svc.BeginStream(context.Background(), f.host, helloReq(0), f.now)
	if err != nil {
		t.Fatalf("BeginStream 失败: %v", err)
	}
	if err := f.store.DeleteHost(context.Background(), f.host.ID); err != nil {
		t.Fatalf("删主机失败: %v", err)
	}
	_, err = st.Hello(context.Background(), helloReq(0), f.now.Add(time.Second))
	if !errors.Is(err, persistmodel.ErrNotFound) {
		t.Fatalf("主机已删应返回 ErrNotFound 包装，实际: %v", err)
	}
}
