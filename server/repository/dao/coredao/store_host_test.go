// Store contract tests for the three host executor tables.
//
// A separate file like issue_store_test.go: store_test.go is the shared test entry point, and this workspace
// has concurrent sessions, so stuffing large functions into it easily causes diff clashes. Test function signatures match the other domains,
// `func(t, newStore storeFactory)`, registered in runStoreSuite in store_test.go,
// so MemStore and PGStore each run them once. What drifts most easily in this domain is the claim SQL CTE vs
// the CAS under the mem lock, and the NULL/zero-value convention of nullable time columns; checking only one side is the same as not checking.

package coredao

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/service/host"
)

// boolPtr builds values for the *bool fields of the heartbeat snapshot (nil and false mean two different things and must be distinguishable).
func boolPtr(b bool) *bool { return &b }

// testHostRegistration checks three things including idempotent registration:
//  1. the first time inserted=true and token_hash is stored;
//  2. the second time for the same owner+uuid inserted=false, id and token_hash unchanged, snapshot columns refreshed;
//  3. different owners registering the same machine uuid get two rows (a shared dev machine is a valid case).
func testHostRegistration(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)

	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	first := persistmodel.Host{
		ID: "hst_a", OwnerEmail: "a@example.com", Name: "A 的 Mac",
		OS: "darwin", Arch: "arm64", HostUUID: "uuid-shared",
		TokenHash: "sha-first", Status: "online",
		DaemonVersion: "v1", GNUToolsOK: false, Toolcheck: "缺 gstat",
		CreatedAt: base,
	}
	saved, inserted, err := s.UpsertRegisteredHost(ctx, first)
	if err != nil || !inserted {
		t.Fatalf("首次注册应 inserted=true，实际 inserted=%v err=%v", inserted, err)
	}
	if saved.TokenHash != "sha-first" || saved.ID != "hst_a" {
		t.Errorf("首次注册落库行不对：%+v", saved)
	}
	if got, err := s.GetHostByOwnerUUID(ctx, "a@example.com", "uuid-shared"); err != nil ||
		got.TokenHash != "sha-first" {
		t.Fatalf("按 owner+uuid 回读失败：%+v err=%v", got, err)
	}

	// Registering the same machine again: the input carries **a new id and a new hash**, and both must be ignored.
	again := first
	again.ID = "hst_should_be_ignored"
	again.TokenHash = "sha-second"
	again.Name = "A 的 Mac 改名了"
	again.OS = "darwin"
	again.Arch = "amd64" // e.g. the machine reported a different architecture
	again.DaemonVersion = "v2"
	again.GNUToolsOK = true
	again.Toolcheck = ""
	saved2, inserted2, err := s.UpsertRegisteredHost(ctx, again)
	if err != nil || inserted2 {
		t.Fatalf("重复注册应 inserted=false，实际 inserted=%v err=%v", inserted2, err)
	}
	if saved2.ID != "hst_a" {
		t.Errorf("重复注册不该换 id：想要 hst_a，实际 %q", saved2.ID)
	}
	if saved2.TokenHash != "sha-first" {
		t.Errorf("重复注册不该重置 token_hash：想要 sha-first，实际 %q", saved2.TokenHash)
	}
	// ⚠️ Use Equal, not ==: the time.Time read back from PG timestamptz carries the local time zone Location
	// (same UTC instant, different Location), so == gives false negatives.
	if !saved2.CreatedAt.Equal(base) {
		t.Errorf("重复注册不该动 created_at：想要 %v，实际 %v", base, saved2.CreatedAt)
	}
	// Snapshot columns must be refreshed.
	if saved2.Name != "A 的 Mac 改名了" || saved2.Arch != "amd64" ||
		saved2.DaemonVersion != "v2" || !saved2.GNUToolsOK || saved2.Toolcheck != "" {
		t.Errorf("重复注册应刷新注册快照列，实际：%+v", saved2)
	}

	// A different owner registers the same shared machine: its own row, its own token.
	b := first
	b.ID = "hst_b"
	b.OwnerEmail = "b@example.com"
	b.Name = "B 视角的机器"
	b.TokenHash = "sha-b"
	savedB, insertedB, err := s.UpsertRegisteredHost(ctx, b)
	if err != nil || !insertedB || savedB.ID != "hst_b" {
		t.Fatalf("不同属主同 uuid 应是新行，实际 inserted=%v id=%q err=%v",
			insertedB, savedB.ID, err)
	}
	if hosts, err := s.ListHostsByOwner(ctx, "a@example.com"); err != nil ||
		len(hosts) != 1 || hosts[0].ID != "hst_a" {
		t.Errorf("A 应只看到自己那台，实际 %+v err=%v", hosts, err)
	}

	// An empty hash must not match any machine (an empty Authorization must not match a row).
	if _, err := s.GetHostByTokenHash(ctx, ""); !errors.Is(err, persistmodel.ErrNotFound) {
		t.Errorf("空 token_hash 应 ErrNotFound，实际 %v", err)
	}
	if got, err := s.GetHostByTokenHash(ctx, "sha-first"); err != nil || got.ID != "hst_a" {
		t.Errorf("按 token_hash 回读失败：got=%+v err=%v", got, err)
	}
}

// testHostHeartbeatAndOffline checks heartbeat refresh and the offline CAS (including the seenBefore race gate).
func testHostHeartbeatAndOffline(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)

	t0 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	h := persistmodel.Host{
		ID: "hst_hb", OwnerEmail: "o@example.com", HostUUID: "u-hb",
		TokenHash: "sha-hb", CreatedAt: t0,
	}
	if _, _, err := s.UpsertRegisteredHost(ctx, h); err != nil {
		t.Fatalf("注册失败：%v", err)
	}

	// Never sent a heartbeat: the sweep list must include it (on the SQL side, last_seen_at is null), but the CAS
	// must not set it offline: it was never online.
	listed, err := s.ListHostsLastSeenBefore(ctx, t0.Add(time.Minute), 0)
	if err != nil {
		t.Fatalf("列巡检主机失败：%v", err)
	}
	found := false
	for _, x := range listed {
		if x.ID == "hst_hb" {
			found = true
		}
	}
	if !found {
		t.Error("从没心跳过的机器也应出现在 ListHostsLastSeenBefore 里")
	}
	if hit, err := s.MarkHostOffline(ctx, "hst_hb", t0.Add(time.Minute), t0); err != nil || hit {
		t.Errorf("非 online 机器不应被 CAS 命中，实际 hit=%v err=%v", hit, err)
	}

	// Heartbeat goes online: snapshot fields are refreshed only when non-empty/non-nil; both the Toolcheck and GNUToolsOK pointers are included.
	tc := "ok"
	if err := s.TouchHost(ctx, "hst_hb", host.HostHeartbeatSnapshot{
		DaemonVersion: "v9", GNUToolsOK: boolPtr(true), Toolcheck: &tc,
	}, "online", t0); err != nil {
		t.Fatalf("心跳失败：%v", err)
	}
	got, err := s.GetHost(ctx, "hst_hb")
	if err != nil || got.Status != "online" || !got.LastSeenAt.Equal(t0) ||
		got.DaemonVersion != "v9" || !got.GNUToolsOK {
		t.Fatalf("心跳后回读不对：%+v err=%v", got, err)
	}

	// 90s sweep: last_seen before the threshold → marked offline.
	hit, err := s.MarkHostOffline(ctx, "hst_hb", t0.Add(90*time.Second), t0.Add(90*time.Second))
	if err != nil || !hit {
		t.Fatalf("过期 online 机器应被置离线，hit=%v err=%v", hit, err)
	}
	got, _ = s.GetHost(ctx, "hst_hb")
	if got.Status != "offline" {
		t.Errorf("置离线后 status=%q", got.Status)
	}
	// Marking again does not match (already offline).
	if hit, _ := s.MarkHostOffline(ctx, "hst_hb", t0.Add(90*time.Second), t0); hit {
		t.Error("offline 机器第二次 CAS 不该命中")
	}

	// After coming back online, marking with an earlier threshold must miss (simulating "heartbeat recovered right after listing").
	// This tick's snapshot only carries the version, which also checks that empty/nil fields do not overwrite old values.
	if err := s.TouchHost(ctx, "hst_hb", host.HostHeartbeatSnapshot{
		DaemonVersion: "v10",
	}, "online", t0.Add(100*time.Second)); err != nil {
		t.Fatalf("二次心跳失败：%v", err)
	}
	if hit, _ := s.MarkHostOffline(ctx, "hst_hb", t0.Add(90*time.Second), t0.Add(200*time.Second)); hit {
		t.Error("心跳已恢复（last_seen 不早于 seenBefore），CAS 必须落空")
	}
	got, _ = s.GetHost(ctx, "hst_hb")
	if got.Status != "online" {
		t.Errorf("落空的 CAS 不该改 status，实际 %q", got.Status)
	}
	// The second tick carries the version but no probe results: the version must be refreshed, GNUToolsOK/Toolcheck must be kept.
	if got.DaemonVersion != "v10" {
		t.Errorf("非空快照字段应刷新，daemonVersion=%q 想要 v10", got.DaemonVersion)
	}
	if !got.GNUToolsOK || got.Toolcheck != "ok" {
		t.Errorf("nil 快照字段不该覆盖旧值，gnuToolsOK=%v toolcheck=%q",
			got.GNUToolsOK, got.Toolcheck)
	}

	// Missing machine: UPDATE matches 0 rows, hit=false and no error.
	if hit, err := s.MarkHostOffline(ctx, "hst_nope", t0, t0); err != nil || hit {
		t.Errorf("不存在机器应 hit=false nil，实际 hit=%v err=%v", hit, err)
	}
	if err := s.TouchHost(ctx, "hst_nope", host.HostHeartbeatSnapshot{},
		"online", t0); !errors.Is(err, persistmodel.ErrNotFound) {
		t.Errorf("对不存在机器心跳应 ErrNotFound，实际 %v", err)
	}
}

// testWorkspaceHostBinding checks idempotent binding, join list order, unbinding, and cascade on host deletion.
func testWorkspaceHostBinding(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)
	t0 := time.Date(2026, 9, 1, 11, 0, 0, 0, time.UTC)

	mkHost := func(id, name string, createdAt time.Time) {
		t.Helper()
		if _, _, err := s.UpsertRegisteredHost(ctx, persistmodel.Host{
			ID: id, OwnerEmail: "o@example.com", Name: name, HostUUID: "u-" + id,
			TokenHash: "sha-" + id, CreatedAt: createdAt,
		}); err != nil {
			t.Fatalf("建 %s 失败：%v", id, err)
		}
	}
	mkHost("hst_1", "机器一", t0)
	mkHost("hst_2", "机器二", t0.Add(time.Second))
	mkHost("hst_3", "别的空间的机器", t0.Add(2*time.Second))

	bind1 := persistmodel.WorkspaceHost{
		ID: "wsh_2", WorkspaceID: "ws_1", HostID: "hst_2",
		AddedBy: "o@example.com", CreatedAt: t0.Add(2 * time.Second),
	}
	bind2 := persistmodel.WorkspaceHost{
		ID: "wsh_1", WorkspaceID: "ws_1", HostID: "hst_1",
		AddedBy: "o@example.com", CreatedAt: t0,
	}
	// Insert bind1, which has a later created_at, first on purpose, to check the list is ordered by binding time and not insertion order.
	if err := s.BindWorkspaceHost(ctx, bind1); err != nil {
		t.Fatalf("绑定 hst_2 失败：%v", err)
	}
	if err := s.BindWorkspaceHost(ctx, bind2); err != nil {
		t.Fatalf("绑定 hst_1 失败：%v", err)
	}
	// Repeated binding is idempotent: no error, nothing added.
	if err := s.BindWorkspaceHost(ctx, bind2); err != nil {
		t.Fatalf("重复绑定应幂等 nil，实际 %v", err)
	}

	hosts, err := s.ListWorkspaceHosts(ctx, "ws_1")
	if err != nil {
		t.Fatalf("列绑定失败：%v", err)
	}
	if len(hosts) != 2 || hosts[0].ID != "hst_1" || hosts[1].ID != "hst_2" {
		t.Fatalf("列表应按绑定 (created_at,id) 升序得 [hst_1 hst_2]，实际 %+v", hosts)
	}
	// What the join returns is the full host row + three binding columns: machine name/token_hash come from hosts,
	// AddedBy/BoundAt come from workspace_hosts (the binding page shows "who bound it").
	if hosts[0].Name != "机器一" || hosts[0].TokenHash != "sha-hst_1" {
		t.Errorf("join 的主机行内容不对：%+v", hosts[0])
	}
	if hosts[0].BindingID != "wsh_1" || hosts[0].AddedBy != "o@example.com" ||
		!hosts[0].BoundAt.Equal(t0) {
		t.Errorf("join 没把绑定列带回来：%+v", hosts[0])
	}

	if b, err := s.GetWorkspaceHost(ctx, "ws_1", "hst_1"); err != nil || b.ID != "wsh_1" {
		t.Fatalf("取绑定失败：%+v err=%v", b, err)
	}
	if _, err := s.GetWorkspaceHost(ctx, "ws_1", "hst_3"); !errors.Is(err, persistmodel.ErrNotFound) {
		t.Errorf("未绑定的组合应 ErrNotFound，实际 %v", err)
	}

	// Binding a missing host: must error (PG gives foreign key 23503, mem gives ErrNotFound;
	// the upper layer translates them uniformly; this only pins "not nil, and no extra binding").
	if err := s.BindWorkspaceHost(ctx, persistmodel.WorkspaceHost{
		ID: "wsh_x", WorkspaceID: "ws_1", HostID: "hst_nope", CreatedAt: t0,
	}); err == nil {
		t.Error("绑定不存在的主机应报错（外键），实际 nil")
	}
	if hosts, _ := s.ListWorkspaceHosts(ctx, "ws_1"); len(hosts) != 2 {
		t.Errorf("失败的绑定不该留下行，实际 %d 条", len(hosts))
	}

	// Unbinding is idempotent.
	if err := s.UnbindWorkspaceHost(ctx, "ws_1", "hst_1"); err != nil {
		t.Fatalf("解绑失败：%v", err)
	}
	if err := s.UnbindWorkspaceHost(ctx, "ws_1", "hst_1"); err != nil {
		t.Errorf("重复解绑应幂等 nil，实际 %v", err)
	}
	if hosts, _ := s.ListWorkspaceHosts(ctx, "ws_1"); len(hosts) != 1 || hosts[0].ID != "hst_2" {
		t.Errorf("解绑后应只剩 hst_2，实际 %+v", hosts)
	}

	// Delete the host: the bindings disappear via the foreign key cascade (the mem side matches it by hand).
	if err := s.DeleteHost(ctx, "hst_2"); err != nil {
		t.Fatalf("删主机失败：%v", err)
	}
	if err := s.DeleteHost(ctx, "hst_2"); err != nil {
		t.Errorf("删不存在主机应幂等 nil，实际 %v", err)
	}
	if hosts, _ := s.ListWorkspaceHosts(ctx, "ws_1"); len(hosts) != 0 {
		t.Errorf("删主机后绑定应级联消失，实际 %+v", hosts)
	}
	if _, err := s.GetHost(ctx, "hst_2"); !errors.Is(err, persistmodel.ErrNotFound) {
		t.Errorf("删掉的主机应 ErrNotFound，实际 %v", err)
	}
}

// testHostDispatchQueue checks the full state machine of the downstream queue: seq assignment, cursor claim, lease redelivery,
// CAS completion without overwriting, term ack.
func testHostDispatchQueue(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)
	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	const hostID = "hst_q"
	enqueue := func(id, kind string, createdAt time.Time) int64 {
		t.Helper()
		seq, err := s.EnqueueDispatch(ctx, persistmodel.HostDispatch{
			ID: id, HostID: hostID, Kind: kind, Payload: "payload-" + id,
			SessionID: "run-1", CreatedAt: createdAt,
		})
		if err != nil {
			t.Fatalf("入队 %s 失败：%v", id, err)
		}
		return seq
	}
	if seq := enqueue("hsp_1", "exec", t0); seq != 1 {
		t.Errorf("第一条 seq 应为 1，实际 %d", seq)
	}
	if seq := enqueue("hsp_2", "exec", t0.Add(time.Second)); seq != 2 {
		t.Errorf("第二条 seq 应为 2，实际 %d", seq)
	}
	if seq := enqueue("hsp_3", "term_input", t0.Add(2*time.Second)); seq != 3 {
		t.Errorf("第三条 seq 应为 3，实际 %d", seq)
	}

	// First claim with cursor 0: 3 rows in ascending seq; exec lease 150s, term 30s.
	got, err := s.ClaimPendingDispatches(ctx, hostID, 0, t0, 10)
	if err != nil {
		t.Fatalf("认领失败：%v", err)
	}
	if len(got) != 3 || got[0].Seq != 1 || got[1].Seq != 2 || got[2].Seq != 3 {
		t.Fatalf("首次认领应得 seq 1,2,3，实际 %+v", got)
	}
	for _, d := range got {
		if d.State != "delivered" || !d.DeliveredAt.Equal(t0) {
			t.Errorf("认领后状态/时刻不对：%+v", d)
		}
	}
	if !got[0].ClaimedUntil.Equal(t0.Add(150 * time.Second)) {
		t.Errorf("exec 租约应为 150s，实际 %v", got[0].ClaimedUntil)
	}
	if !got[2].ClaimedUntil.Equal(t0.Add(30 * time.Second)) {
		t.Errorf("term 租约应为 30s，实际 %v", got[2].ClaimedUntil)
	}

	// Lease not expired + no new queued rows after the cursor: nothing to claim.
	got, _ = s.ClaimPendingDispatches(ctx, hostID, 999, t0.Add(10*time.Second), 10)
	if len(got) != 0 {
		t.Errorf("租约有效期内不应重投，实际领到 %d 条", len(got))
	}

	// The term frame's 30s expires first: at t0+60 only seq=3 is redelivered (cursor 999 blocks the queued branch,
	// leaving only the expired-delivered branch).
	got, _ = s.ClaimPendingDispatches(ctx, hostID, 999, t0.Add(60*time.Second), 10)
	if len(got) != 1 || got[0].Seq != 3 {
		t.Fatalf("t0+60s 应只重投 term seq=3，实际 %+v", got)
	}
	// The daemon acks the redelivered terminal frame right away (real behavior); otherwise it would expire again after 30s
	// and show up in the next claim, polluting the exec redelivery assertions.
	if err := s.AckDispatches(ctx, hostID, []int64{3}, t0.Add(61*time.Second)); err != nil {
		t.Fatalf("ack term 帧失败：%v", err)
	}

	// t0+200: the two exec rows have expired too; the redelivery must match the first delivery (same row, same payload).
	got, _ = s.ClaimPendingDispatches(ctx, hostID, 999, t0.Add(200*time.Second), 10)
	if len(got) != 2 || got[0].Seq != 1 || got[1].Seq != 2 {
		t.Fatalf("t0+200s 应重投 exec seq=1,2，实际 %+v", got)
	}
	if got[0].Payload != "payload-hsp_1" || got[0].Kind != "exec" ||
		got[0].SessionID != "run-1" || !got[0].DeliveredAt.Equal(t0.Add(200*time.Second)) {
		t.Errorf("重投行内容应不变、delivered_at 应刷新，实际 %+v", got[0])
	}

	// CAS completion: a repeated complete does not overwrite result.
	if err := s.CompleteDispatch(ctx, hostID, "hsp_1", `{"ok":true}`, t0.Add(201*time.Second)); err != nil {
		t.Fatalf("complete 失败：%v", err)
	}
	if err := s.CompleteDispatch(ctx, hostID, "hsp_1", `{"ok":false}`, t0.Add(202*time.Second)); err != nil {
		t.Fatalf("重复 complete 报错：%v", err)
	}
	done1, err := s.GetDispatch(ctx, "hsp_1")
	if err != nil {
		t.Fatalf("取 hsp_1 失败：%v", err)
	}
	if done1.State != "done" || done1.Result != `{"ok":true}` {
		t.Errorf("重复 complete 不该覆盖 result，实际 state=%s result=%q",
			done1.State, done1.Result)
	}
	// Report from the wrong host: hostID does not match, 0 rows updated, no error.
	if err := s.CompleteDispatch(ctx, "hst_other", "hsp_2", "x", t0); err != nil {
		t.Errorf("不匹配主机的 complete 应静默 0 行，实际 %v", err)
	}

	// The term frame was already acked: ack it again here to check idempotency, and that ack only accepts term_*:
	// seq=2 is exec and must not be touched by it.
	if err := s.AckDispatches(ctx, hostID, []int64{3}, t0.Add(203*time.Second)); err != nil {
		t.Fatalf("重复 ack 报错：%v", err)
	}
	if d, _ := s.GetDispatch(ctx, "hsp_3"); d.State != "done" || d.Result != "" {
		t.Errorf("term ack 后应 done 且无 result，实际 %+v", d)
	}
	// seq=2 is exec; ack must not touch it.
	if d, _ := s.GetDispatch(ctx, "hsp_2"); d.State == "done" {
		t.Errorf("AckDispatches 不该动 exec 行，hsp_2 state=%s", d.State)
	}

	// done rows / rows within their lease are not claimed again: seq=1 is done; seq=2 is delivered, but
	// after the t0+200 redelivery its lease was renewed to t0+350, so at t0+300 it has not expired; seq=3 is acked.
	got, _ = s.ClaimPendingDispatches(ctx, hostID, 0, t0.Add(300*time.Second), 10)
	if len(got) != 0 {
		t.Fatalf("t0+300 不应有可认领行（seq=2 租约到 t0+350），实际 %+v", got)
	}
	// Past t0+350: only seq=2 has expired (seq=1 done, seq=3 done).
	got, _ = s.ClaimPendingDispatches(ctx, hostID, 0, t0.Add(400*time.Second), 10)
	if len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("t0+400 应只重投未完成的 seq=2，实际 %+v", got)
	}

	// Newly queued rows are governed only by the cursor: seq=4, claimable with cursor=3.
	seq4 := enqueue("hsp_4", "exec", t0.Add(3*time.Second))
	if seq4 != 4 {
		t.Errorf("新行 seq 应为 4，实际 %d", seq4)
	}
	if got, _ := s.ClaimPendingDispatches(ctx, hostID, 3, t0.Add(300*time.Second), 10); len(got) != 1 ||
		got[0].Seq != 4 {
		t.Fatalf("游标=3 应只领到新 queued seq=4，实际 %+v", got)
	}
	if got, _ := s.ClaimPendingDispatches(ctx, hostID, 4, t0.Add(300*time.Second), 10); len(got) != 0 {
		t.Fatalf("游标=4 不该再领到 seq=4（seq>afterSeq 为假），实际 %+v", got)
	}
}

// testHostFailOnlyExecsAndPurge checks that the host-offline fallback only touches exec, and that old term rows are cleaned up.
func testHostFailOnlyExecsAndPurge(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)
	t0 := time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	const hostID = "hst_f"

	enqueue := func(id, kind string, createdAt time.Time) {
		t.Helper()
		if _, err := s.EnqueueDispatch(ctx, persistmodel.HostDispatch{
			ID: id, HostID: hostID, Kind: kind, CreatedAt: createdAt,
		}); err != nil {
			t.Fatalf("入队失败：%v", err)
		}
	}
	enqueue("hsp_e1", "exec", t0)
	enqueue("hsp_e2", "exec", t0.Add(time.Second))
	enqueue("hsp_t1", "term_open", t0.Add(2*time.Second))
	enqueue("hsp_t2", "term_input", t0.Add(3*time.Second))
	// Claim first so they are all delivered (both the queued and delivered branches must be covered by fail;
	// here all are delivered; another brand-new queued exec is built separately below).
	if _, err := s.ClaimPendingDispatches(ctx, hostID, 0, t0, 10); err != nil {
		t.Fatalf("认领失败：%v", err)
	}
	enqueue("hsp_e3", "exec", t0.Add(4*time.Second)) // still queued

	n, err := s.FailPendingExecsForHost(ctx, hostID, `{"error":"host offline"}`, t0.Add(10*time.Second))
	if err != nil {
		t.Fatalf("fail 失败：%v", err)
	}
	if n != 3 {
		t.Errorf("应有 3 条 exec（2 delivered + 1 queued）被置 done，实际 %d", n)
	}
	for _, id := range []string{"hsp_e1", "hsp_e2", "hsp_e3"} {
		d, _ := s.GetDispatch(ctx, id)
		if d.State != "done" || d.Result != `{"error":"host offline"}` {
			t.Errorf("%s 应为 done 且带失败结果，实际 %+v", id, d)
		}
	}
	// term frames are untouched.
	for _, id := range []string{"hsp_t1", "hsp_t2"} {
		if d, _ := s.GetDispatch(ctx, id); d.State != "delivered" {
			t.Errorf("%s 是 term 帧，fail 不该动它，实际 state=%s", id, d.State)
		}
	}
	// Calling again: no exec in flight anymore, returns 0 and does not overwrite existing results.
	if n, _ := s.FailPendingExecsForHost(ctx, hostID, `{"error":"第二次"}`, t0.Add(11*time.Second)); n != 0 {
		t.Errorf("第二次 fail 应命中 0 行，实际 %d", n)
	}
	if d, _ := s.GetDispatch(ctx, "hsp_e1"); d.Result != `{"error":"host offline"}` {
		t.Errorf("重复 fail 不该覆盖 result，实际 %q", d.Result)
	}
	// Other hosts are unaffected.
	if _, err := s.EnqueueDispatch(ctx, persistmodel.HostDispatch{
		ID: "hsp_other", HostID: "hst_other", Kind: "exec", CreatedAt: t0,
	}); err != nil {
		t.Fatalf("给另一台主机入队失败：%v", err)
	}
	if n, _ := s.FailPendingExecsForHost(ctx, hostID, "x", t0.Add(12*time.Second)); n != 0 {
		t.Errorf("fail 不该圈进别的主机的指令，实际 %d", n)
	}
	if d, _ := s.GetDispatch(ctx, "hsp_other"); d.State != "queued" {
		t.Errorf("别的主机的 exec 应仍 queued，实际 %s", d.State)
	}

	// term cleanup: rows not done are not deleted; after being acked to done, only those with created_at before the threshold are deleted.
	if n, _ := s.PurgeOldTermDispatches(ctx, t0.Add(time.Hour)); n != 0 {
		t.Errorf("term 帧还没 done，不应被清理，实际删了 %d", n)
	}
	if err := s.AckDispatches(ctx, hostID, []int64{3, 4}, t0.Add(20*time.Second)); err != nil {
		t.Fatalf("ack term 失败：%v", err)
	}
	if n, _ := s.PurgeOldTermDispatches(ctx, t0.Add(time.Hour)); n != 2 {
		t.Errorf("两条旧 term done 行应被删除，实际 %d", n)
	}
	for _, id := range []string{"hsp_t1", "hsp_t2"} {
		if _, err := s.GetDispatch(ctx, id); !errors.Is(err, persistmodel.ErrNotFound) {
			t.Errorf("%s 应已被 purge，实际 err=%v", id, err)
		}
	}
	// exec rows are outside the term cleanup scope even when done.
	if d, err := s.GetDispatch(ctx, "hsp_e1"); err != nil || d.State != "done" {
		t.Errorf("exec 行不该被 term 清理删掉，d=%+v err=%v", d, err)
	}
}

// testAgentExecutorColumnsRoundTrip checks that the agent's executor_type / host_id columns round-trip consistently across
// the four touch points CreateAgent / SaveAgent / GetAgent / ListAgents. Runs on both mem and pg:
// the PG side uses real columns, the mem side stores the struct; the mechanisms differ, and drift is only visible when running both.
func testAgentExecutorColumnsRoundTrip(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)

	a := apimodel.Agent{
		ID: "ag_host", WorkspaceID: "ws_host", Name: "宿主机助手",
		ExecutorType: "host", HostID: "hst_x",
		BuiltinTools: []string{}, SkillIDs: []string{},
		FeishuAppIDs: []string{}, IntegrationIDs: []string{},
	}
	created, err := s.CreateAgent(ctx, a)
	if err != nil {
		t.Fatalf("建 agent 失败：%v", err)
	}
	if created.ExecutorType != "host" || created.HostID != "hst_x" {
		t.Errorf("CreateAgent 返回值没带住两列：%+v", created)
	}
	got, err := s.GetAgent(ctx, "ag_host")
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	if got.ExecutorType != "host" || got.HostID != "hst_x" {
		t.Errorf("GetAgent 往返失败：executorType=%q hostId=%q", got.ExecutorType, got.HostID)
	}
	list, err := s.ListAgents(ctx, "ws_host")
	if err != nil || len(list) != 1 || list[0].ExecutorType != "host" || list[0].HostID != "hst_x" {
		t.Fatalf("ListAgents 往返失败：list=%+v err=%v", list, err)
	}

	// Switch back to the default executor (Docker container): both columns are cleared (PG writes NULL) and both read back as empty strings.
	a.ExecutorType = ""
	a.HostID = ""
	if _, err := s.SaveAgent(ctx, a); err != nil {
		t.Fatalf("存 agent 失败：%v", err)
	}
	got, _ = s.GetAgent(ctx, "ag_host")
	if got.ExecutorType != "" || got.HostID != "" {
		t.Errorf("清空执行端后应读回空串，实际 executorType=%q hostId=%q",
			got.ExecutorType, got.HostID)
	}
}

// testHostDispatchSeqSurvivesPurge checks that the sequence counter is decoupled from the dispatch row lifecycle:
// after all done rows of a host are purged, the next command's seq must keep increasing,
// not wrap back to 1. The daemon's persisted last_seq cursor stays at the previous one (seq=2); after wrapping,
// `seq > last_seq` never holds, and new commands are silently never fetched (found in code review).
func testHostDispatchSeqSurvivesPurge(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)
	t0 := time.Date(2026, 9, 1, 14, 0, 0, 0, time.UTC)
	const hostID = "hst_purge"

	if _, _, err := s.UpsertRegisteredHost(ctx, persistmodel.Host{
		ID: hostID, OwnerEmail: "p@example.com", HostUUID: "uuid-purge",
		TokenHash: "x", CreatedAt: t0,
	}); err != nil {
		t.Fatalf("注册主机失败：%v", err)
	}
	enqueue := func(id string, createdAt time.Time) int64 {
		t.Helper()
		seq, err := s.EnqueueDispatch(ctx, persistmodel.HostDispatch{
			ID: id, HostID: hostID, Kind: "term_open", CreatedAt: createdAt,
		})
		if err != nil {
			t.Fatalf("入队 %s 失败：%v", id, err)
		}
		return seq
	}
	if seq := enqueue("hsp_p1", t0); seq != 1 {
		t.Fatalf("第一条 seq 应为 1，实际 %d", seq)
	}
	if seq := enqueue("hsp_p2", t0.Add(time.Second)); seq != 2 {
		t.Fatalf("第二条 seq 应为 2，实际 %d", seq)
	}

	// Claim and ack to done, then purge until no rows are left.
	if _, err := s.ClaimPendingDispatches(ctx, hostID, 0, t0, 10); err != nil {
		t.Fatalf("认领失败：%v", err)
	}
	if err := s.AckDispatches(ctx, hostID, []int64{1, 2}, t0.Add(time.Second)); err != nil {
		t.Fatalf("ack 失败：%v", err)
	}
	if n, err := s.PurgeOldTermDispatches(ctx, t0.Add(time.Hour)); err != nil || n != 2 {
		t.Fatalf("应 purge 两条，实际 n=%d err=%v", n, err)
	}
	if _, err := s.GetDispatch(ctx, "hsp_p1"); !errors.Is(err, persistmodel.ErrNotFound) {
		t.Fatalf("行应已删干净，实际 err=%v", err)
	}

	// The key check: after clearing, the enqueue must be 3, not 1; the counter on hosts must also read back as 3.
	if seq := enqueue("hsp_p3", t0.Add(2*time.Second)); seq != 3 {
		t.Fatalf("purge 清空后 seq 不应回卷，应为 3，实际 %d", seq)
	}
	h, err := s.GetHost(ctx, hostID)
	if err != nil {
		t.Fatalf("回读主机失败：%v", err)
	}
	if h.LastDispatchSeq != 3 {
		t.Fatalf("hosts.last_dispatch_seq 应为 3，实际 %d", h.LastDispatchSeq)
	}
	// A daemon with its cursor at 2 must be able to fetch seq=3.
	got, err := s.ClaimPendingDispatches(ctx, hostID, 2, t0.Add(3*time.Second), 10)
	if err != nil {
		t.Fatalf("清空后认领失败：%v", err)
	}
	if len(got) != 1 || got[0].Seq != 3 || got[0].ID != "hsp_p3" {
		t.Fatalf("游标 2 应只领到 seq=3 的新指令，实际 %+v", got)
	}
}
