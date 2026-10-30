// Store contract tests for entity history versions, **run once each on Mem and PG**.
//
// # Why these cannot be checked only on the core side with the in-memory Store
//
// The two implementations work completely differently, and every SQL on the PG side has its own failure mode:
//
//	numbering            PG is "max+1 + unique constraint catches it and retries", memory is a slice walk
//	list without body    PG relies on `'' as snapshot` in the select column list, memory sets nil by hand
//	no limit             PG relies on the meaning of `limit NULL`, memory on `if limit > 0`
//	excess cleanup       PG is a window function row_number() over (partition by ...), memory computes a threshold per group
//
// Checking only the in-memory version, the right column above is never executed once, **and coredao's tests are still all green**,
// because the green ones are other people's tests. This is exactly the known lesson "a brand-new module can be all green yet not in effect".
package coredao

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

// mkRevision builds a snapshot. The snapshot holds a recognizable value, to check byte-for-byte fidelity.
func mkRevision(id, entityID, name string) apimodel.Revision {
	return apimodel.Revision{
		ID:          id,
		WorkspaceID: "ws_demo",
		Kind:        apimodel.RevisionKindAgent,
		EntityID:    entityID,
		Snapshot:    json.RawMessage(`{"name":"` + name + `","system":"提示词"}`),
		Summary:     name,
		CreatedAt:   "2026-09-01T10:00:00.000Z",
		CreatedBy:   "alice@example.com",
		Reason:      apimodel.RevisionReasonEdit,
	}
}

// testRevisionRoundTrip stores one, reads it back and compares field by field.
//
// ⚠️ **snapshot must be byte-for-byte identical**. That is the whole reason for choosing text over jsonb:
// jsonb reorders keys and squeezes whitespace, which would make the UI diff show a false "changed" out of nothing.
// This test guards that choice: if someone ever switches the column type to jsonb, it turns red as soon as key order changes.
func testRevisionRoundTrip(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)

	in := mkRevision("rev_1", "agent_a", "第一版")
	saved, err := s.AppendEntityRevision(ctx, in)
	if err != nil {
		t.Fatalf("存快照失败：%v", err)
	}
	if saved.No != 1 {
		t.Errorf("第一条快照的序号应是 1，实际 %d", saved.No)
	}

	got, err := s.GetEntityRevision(ctx, "rev_1")
	if err != nil {
		t.Fatalf("读快照失败：%v", err)
	}
	if string(got.Snapshot) != string(in.Snapshot) {
		t.Errorf("snapshot 必须逐字节保真\n存进去：%s\n读回来：%s——"+
			"键被重排/空白被压掉说明这一列不是 text（那会让界面 diff 出假的改动）",
			in.Snapshot, got.Snapshot)
	}
	if got.WorkspaceID != in.WorkspaceID || got.Kind != in.Kind || got.EntityID != in.EntityID {
		t.Errorf("归属字段没对上：%+v", got)
	}
	if got.Summary != in.Summary || got.CreatedAt != in.CreatedAt || got.CreatedBy != in.CreatedBy {
		t.Errorf("展示字段没对上：%+v", got)
	}
	if got.Reason != apimodel.RevisionReasonEdit {
		t.Errorf("reason 应是 edit，实际 %q", got.Reason)
	}

	// A missing id must return ErrNotFound, not an empty struct.
	if _, err := s.GetEntityRevision(ctx, "rev_nope"); err == nil {
		t.Error("读不存在的快照应回 ErrNotFound，实际没报错")
	}
}

// testRevisionNumbersAreSequential: numbers of the same entity increase one by one; different entities count separately.
//
// ⚠️ "Count separately" is the check for partition: with a global auto-increment, two agents changing in turn
// would get numbers like 1/2/4/6. No error, but "version N" in the UI becomes a string of skipped numbers.
func testRevisionNumbersAreSequential(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)

	for i := 1; i <= 3; i++ {
		rev := mkRevision("rev_a"+strconv.Itoa(i), "agent_a", "a"+strconv.Itoa(i))
		saved, err := s.AppendEntityRevision(ctx, rev)
		if err != nil {
			t.Fatalf("存 agent_a 第 %d 条失败：%v", i, err)
		}
		if saved.No != i {
			t.Errorf("agent_a 第 %d 条的序号应是 %d，实际 %d", i, i, saved.No)
		}
	}
	// Another entity starts again from 1.
	saved, err := s.AppendEntityRevision(ctx, mkRevision("rev_b1", "agent_b", "b1"))
	if err != nil {
		t.Fatalf("存 agent_b 失败：%v", err)
	}
	if saved.No != 1 {
		t.Errorf("另一个实体的序号应从 1 重新开始，实际 %d——"+
			"序号必须按实体分组，不是全局自增", saved.No)
	}

	// ⚠️ **The No passed in must be ignored**: only the Store may decide the number; otherwise under concurrency two instances
	// each pass a number they computed themselves, and two snapshots end up with the same revision_no.
	forced := mkRevision("rev_a99", "agent_a", "a99")
	forced.No = 999
	saved, err = s.AppendEntityRevision(ctx, forced)
	if err != nil {
		t.Fatalf("存失败：%v", err)
	}
	if saved.No != 4 {
		t.Errorf("传进来的 No 应被忽略、按 max+1 分配（应为 4），实际 %d", saved.No)
	}
}

// testRevisionListOmitsSnapshot: lists do not include the body; only single reads do.
//
// One agent snapshot is a few KB, 20 of them tens of KB, while the list only renders names and times.
// ⚠️ Both implementations must match: if the in-memory version returns one more field, "the frontend gets the snapshot"
// works locally and vanishes in production, and that kind of difference only shows up against a real database.
func testRevisionListOmitsSnapshot(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)

	for i := 1; i <= 3; i++ {
		if _, err := s.AppendEntityRevision(ctx, mkRevision("rev_"+strconv.Itoa(i), "agent_a", "v"+strconv.Itoa(i))); err != nil {
			t.Fatalf("存第 %d 条失败：%v", i, err)
		}
	}

	list, err := s.ListEntityRevisions(ctx, apimodel.RevisionKindAgent, "agent_a", 0)
	if err != nil {
		t.Fatalf("列快照失败：%v", err)
	}
	if len(list) != 3 {
		t.Fatalf("limit=0 应不限量、返回 3 条，实际 %d 条", len(list))
	}
	// Newest first.
	if list[0].No != 3 || list[2].No != 1 {
		t.Errorf("应按序号倒序（最新在前），实际 %d,%d,%d", list[0].No, list[1].No, list[2].No)
	}
	for _, rev := range list {
		if len(rev.Snapshot) != 0 {
			t.Errorf("列表不该带 snapshot 正文（第 %d 条带了 %d 字节）", rev.No, len(rev.Snapshot))
		}
	}

	// limit takes effect.
	limited, err := s.ListEntityRevisions(ctx, apimodel.RevisionKindAgent, "agent_a", 2)
	if err != nil {
		t.Fatalf("列快照失败：%v", err)
	}
	if len(limited) != 2 {
		t.Errorf("limit=2 应返回 2 条，实际 %d 条", len(limited))
	}

	// An entity without snapshots returns an **empty slice, not nil**, and not an error either.
	empty, err := s.ListEntityRevisions(ctx, apimodel.RevisionKindAgent, "agent_none", 0)
	if err != nil {
		t.Fatalf("列空实体的快照不该报错：%v", err)
	}
	if empty == nil {
		t.Error("应返回空切片而不是 nil（nil 会编码成 null，前端要多判一种形状）")
	}
	if len(empty) != 0 {
		t.Errorf("应是空的，实际 %d 条", len(empty))
	}
}

// testRevisionPurgeKeepsNewestPerEntity: excess cleanup groups by entity and keeps the newest N of each.
//
// Both checks are required: **what stays is the newest** (not the oldest),
// and **grouped by entity** (not N rows for the whole table).
func testRevisionPurgeKeepsNewestPerEntity(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)

	// agent_a stores 5, agent_b stores 2.
	for i := 1; i <= 5; i++ {
		if _, err := s.AppendEntityRevision(ctx, mkRevision("rev_a"+strconv.Itoa(i), "agent_a", "a"+strconv.Itoa(i))); err != nil {
			t.Fatalf("存 a 第 %d 条失败：%v", i, err)
		}
	}
	for i := 1; i <= 2; i++ {
		if _, err := s.AppendEntityRevision(ctx, mkRevision("rev_b"+strconv.Itoa(i), "agent_b", "b"+strconv.Itoa(i))); err != nil {
			t.Fatalf("存 b 第 %d 条失败：%v", i, err)
		}
	}

	// keep=0 deletes nothing (when misconfigured to 0, better not delete; that step cannot be undone).
	if n, err := s.PurgeExcessRevisions(ctx, 0); err != nil || n != 0 {
		t.Errorf("keep=0 应什么都不删，实际删了 %d 条（err=%v）", n, err)
	}

	n, err := s.PurgeExcessRevisions(ctx, 2)
	if err != nil {
		t.Fatalf("清理失败：%v", err)
	}
	if n != 3 {
		t.Errorf("a 有 5 条留 2 条应删 3 条（b 只有 2 条不动），实际删了 %d 条", n)
	}

	gotA, _ := s.ListEntityRevisions(ctx, apimodel.RevisionKindAgent, "agent_a", 0)
	if len(gotA) != 2 {
		t.Fatalf("a 应剩 2 条，实际 %d 条", len(gotA))
	}
	if gotA[0].No != 5 || gotA[1].No != 4 {
		t.Errorf("留下的必须是**最新**的两条（5、4），实际 %d、%d——"+
			"留成最老的那几条同样「有上限」，但用户想找的永远是刚才那几版",
			gotA[0].No, gotA[1].No)
	}
	gotB, _ := s.ListEntityRevisions(ctx, apimodel.RevisionKindAgent, "agent_b", 0)
	if len(gotB) != 2 {
		t.Errorf("b 只有 2 条、没到上限，不该被动，实际剩 %d 条——"+
			"清理必须按实体分组，不是全表留 N 条", len(gotB))
	}

	// Delete a whole entity's history.
	removed, err := s.DeleteEntityRevisions(ctx, apimodel.RevisionKindAgent, "agent_a")
	if err != nil {
		t.Fatalf("删 a 的历史失败：%v", err)
	}
	if removed != 2 {
		t.Errorf("应删掉 a 剩下的 2 条，实际 %d", removed)
	}
	if left, _ := s.ListEntityRevisions(ctx, apimodel.RevisionKindAgent, "agent_a", 0); len(left) != 0 {
		t.Errorf("a 的历史应清空，实际剩 %d 条", len(left))
	}
	// b is unaffected.
	if left, _ := s.ListEntityRevisions(ctx, apimodel.RevisionKindAgent, "agent_b", 0); len(left) != 2 {
		t.Errorf("删 a 的历史不该动到 b，实际 b 剩 %d 条", len(left))
	}
	// Deleting something missing is not an error (idempotent).
	if _, err := s.DeleteEntityRevisions(ctx, apimodel.RevisionKindAgent, "agent_none"); err != nil {
		t.Errorf("删不存在实体的历史不该报错：%v", err)
	}
}
