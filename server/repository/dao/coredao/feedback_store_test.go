// Store contract tests: issue feedback.
//
// runStoreSuite in store_test.go runs them **once each against MemStore and PGStore**.
//
// # Why this group must run on both implementations
//
// Rate limiting is really implemented by PG's `on conflict (dedup_key) do update`, while unit tests run the in-memory version.
// Checking only one side misses drift in both directions:
//
//   - PG fixed but the in-memory version keeps piling up duplicates → the problem comes back as soon as `AMA_STORE=mem` is used,
//     and **unit tests use exactly the in-memory version**, so tests would never see this bug;
//   - in-memory version right but PG has no unique index → rate limiting silently stops working in production.
//
// Same reasoning as the precedent of dedup by event_id in run_events (see core/README.md).
//
// The PG run calls t.Skip as a whole when the database is unreachable, so `go test ./...` still runs offline for people without containers.

package coredao

import (
	"context"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// isoOf turns a time into the fixed-length ISO string FeedbackQuery wants.
func isoOf(t time.Time) string { return apimodel.FormatISO(t) }

// feedbackRec builds a feedback record.
//
// dedupKey is passed in explicitly instead of computed here: this group tests **the Store's dedup behavior**,
// while "how the key is computed" is the Service's job (feedbackDedupKey). Keeping them apart,
// changing the algorithm does not touch this group, and this group does not let a Store bug slide just because the algorithm is right.
func feedbackRec(id, level, actor, route, code, dedupKey string, at time.Time) persistmodel.FeedbackRecord {
	return persistmodel.FeedbackRecord{
		ID:           id,
		Level:        persistmodel.FeedbackLevel(level),
		Actor:        actor,
		RoutePattern: route,
		ErrorCode:    code,
		HTTPStatus:   500,
		Payload:      map[string]any{"message": "boom"},
		CreatedAt:    at,
		LastSeenAt:   at,
		Occurrences:  1,
		DedupKey:     dedupKey,
	}
}

// testFeedbackDedupesAutoLevel: one dedupKey keeps only one row, and occurrences accumulates.
func testFeedbackDedupesAutoLevel(t *testing.T, newStore storeFactory) {
	s := newStore(t, false)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	// Report the same key five times. **The id is different each time**: real callers generate a new id every time,
	// so an implementation deduping by id would pile up 5 rows, while dedup must be by dedupKey.
	for i := 0; i < 5; i++ {
		rec := feedbackRec(
			"fb_"+string(rune('a'+i)), "auto", "u@example.com",
			"/w/x/chat", "internal", "same-key", base.Add(time.Duration(i)*time.Second),
		)
		if err := s.PutFeedback(ctx, rec); err != nil {
			t.Fatalf("第 %d 次存反馈失败: %v", i, err)
		}
	}

	recs, total, err := s.ListFeedback(ctx, persistmodel.FeedbackQuery{Limit: 50})
	if err != nil {
		t.Fatalf("列反馈失败: %v", err)
	}
	if total != 1 || len(recs) != 1 {
		t.Fatalf("同一个 dedupKey 只该留 1 行，实际 total=%d len=%d", total, len(recs))
	}
	if recs[0].Occurrences != 5 {
		t.Errorf("occurrences 该累加到 5，实际 %d", recs[0].Occurrences)
	}
	// On conflict **only occurrences and last_seen_at change**: keeping the first one is deliberate,
	// the scene of the first occurrence is more valuable than the 5th.
	if !recs[0].CreatedAt.Equal(base) {
		t.Errorf("created_at 该保持首次的 %v，实际 %v", base, recs[0].CreatedAt)
	}
	if !recs[0].LastSeenAt.After(recs[0].CreatedAt) {
		t.Errorf("last_seen_at 该被推进到最后一次（%v），实际 %v",
			base.Add(4*time.Second), recs[0].LastSeenAt)
	}
}

// testFeedbackKeepsUserLevel: an empty dedupKey **is not deduped**; every one is kept.
//
// ⚠️ This pins the PG-side rule "dedup_key must be written as NULL, not an empty string".
// With an empty string, the unique index would treat the second as a duplicate of the first, so **of all feedback users submit themselves,
// only the first gets in**, and the rest all become +1 on the first. The user sees "received" while the database has nothing.
// This is the easiest point in the whole implementation to get wrong, and getting it wrong gives no error at all.
func testFeedbackKeepsUserLevel(t *testing.T, newStore storeFactory) {
	s := newStore(t, false)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	for i := 0; i < 3; i++ {
		rec := feedbackRec(
			"fbu_"+string(rune('a'+i)), "user", "u@example.com",
			"/w/x/chat", "internal", "", base.Add(time.Duration(i)*time.Second),
		)
		if err := s.PutFeedback(ctx, rec); err != nil {
			t.Fatalf("第 %d 次存反馈失败: %v", i, err)
		}
	}

	_, total, err := s.ListFeedback(ctx, persistmodel.FeedbackQuery{Limit: 50})
	if err != nil {
		t.Fatalf("列反馈失败: %v", err)
	}
	if total != 3 {
		t.Errorf("空 dedupKey 不该去重，3 条就该有 3 行，实际 %d", total)
	}
}

// testFeedbackLogIDRoundTrips: the server logid must be stored and read back as is, and empty must stay empty.
//
// # Why this goes into the Store contract (run once each on the two implementations)
//
// This is the step **most easily missed when adding a column, with zero errors when missed**: the PG side needs three changes at once
// (the insert column list, the number of `values` placeholders, the select column list and the Scan receivers).
// Missing the select half means it is written but cannot be read back; `LogID` is always an empty string.
// And the Service-layer tests run on MemStore (it clones the whole record, so it naturally misses nothing),
// **so only the PG run can reveal it**.
//
// ⚠️ The empty half matters just as much: `log_id` is a nullable column, and NULL must read as an empty string, not make Scan error.
// Every row of level one is NULL, so that path is taken even more than the one with a value.
// Scanning directly into `string` would make **the list endpoint fail entirely on the first auto record**.
func testFeedbackLogIDRoundTrips(t *testing.T, newStore storeFactory) {
	s := newStore(t, false)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	// Level two + logid (errors with a response, like 403/500).
	withLog := feedbackRec("fbl_a", "user", "u@example.com", "/w/x", "internal", "", base)
	withLog.LogID = "20260830120000-abcdef123456"
	// Level two + no logid (network_error: the request never reached the server, so no id was generated).
	noLog := feedbackRec("fbl_b", "user", "u@example.com", "/w/y", "network_error", "",
		base.Add(-time.Second))
	// Level one: logid is deliberately not collected, so this row's column is NULL.
	auto := feedbackRec("fbl_c", "auto", "u@example.com", "/w/z", "internal", "auto-key",
		base.Add(-2*time.Second))

	for _, rec := range []persistmodel.FeedbackRecord{withLog, noLog, auto} {
		if err := s.PutFeedback(ctx, rec); err != nil {
			t.Fatalf("存 %s 失败: %v", rec.ID, err)
		}
	}

	recs, _, err := s.ListFeedback(ctx, persistmodel.FeedbackQuery{Limit: 50})
	if err != nil {
		t.Fatalf("列反馈失败: %v", err)
	}
	got := map[string]string{}
	for _, r := range recs {
		got[r.ID] = r.LogID
	}

	if got["fbl_a"] != "20260830120000-abcdef123456" {
		t.Errorf("有 logid 的那条要原样读回来，实际 %q。\n"+
			"读不回来通常是 select 的列清单漏了 log_id——写进去了但读不出来，零报错。",
			got["fbl_a"])
	}
	// Both rows without a logid must read as an empty string (NULL on the PG side), not an error and not some placeholder value.
	for _, id := range []string{"fbl_b", "fbl_c"} {
		if got[id] != "" {
			t.Errorf("%s 没有 logid，该读成空串，实际 %q。\n"+
				"绝不能兜底成占位值：那会被当成真 logid 拿去搜，而它什么都搜不到。",
				id, got[id])
		}
	}
}

// testFeedbackListOrderAndPaging: stable order, correct paging, total is the full count.
func testFeedbackListOrderAndPaging(t *testing.T, newStore storeFactory) {
	s := newStore(t, false)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	// Five rows with increasing times. The list must return them by created_at **descending** (the newest problem matters most).
	for i := 0; i < 5; i++ {
		rec := feedbackRec(
			"fbo_"+string(rune('a'+i)), "user", "u@example.com",
			"/w/x", "internal", "", base.Add(time.Duration(i)*time.Minute),
		)
		if err := s.PutFeedback(ctx, rec); err != nil {
			t.Fatalf("存反馈失败: %v", err)
		}
	}

	page, total, err := s.ListFeedback(ctx, persistmodel.FeedbackQuery{Limit: 2})
	if err != nil {
		t.Fatalf("列反馈失败: %v", err)
	}
	// total is **the count of all rows matching the conditions**, not the number on this page.
	if total != 5 {
		t.Errorf("total 该是 5，实际 %d", total)
	}
	if len(page) != 2 {
		t.Fatalf("limit=2 该返回 2 条，实际 %d", len(page))
	}
	// Descending: the first should be the latest one (fbo_e).
	if page[0].ID != "fbo_e" {
		t.Errorf("该按 created_at 降序，第一条期望 fbo_e，实际 %s", page[0].ID)
	}
	if !page[0].CreatedAt.After(page[1].CreatedAt) {
		t.Errorf("相邻两条该是降序，实际 %v 不晚于 %v", page[0].CreatedAt, page[1].CreatedAt)
	}

	// Paging: offset=2 gets the 3rd and 4th, and **no overlap with page one**.
	// With an unstable sort there would be duplicates or gaps here, and that error never shows with small data.
	next, _, err := s.ListFeedback(ctx, persistmodel.FeedbackQuery{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("翻页失败: %v", err)
	}
	if len(next) != 2 {
		t.Fatalf("第二页该有 2 条，实际 %d", len(next))
	}
	for _, a := range page {
		for _, b := range next {
			if a.ID == b.ID {
				t.Errorf("翻页出现重复记录 %s——排序不稳定", a.ID)
			}
		}
	}

	// When offset is past the total, return an empty slice instead of an error, and **non-nil** (list endpoints never return null).
	empty, _, err := s.ListFeedback(ctx, persistmodel.FeedbackQuery{Limit: 2, Offset: 999})
	if err != nil {
		t.Fatalf("超范围 offset 该回空而不是报错: %v", err)
	}
	if empty == nil {
		t.Error("该回非 nil 的空切片")
	}
	if len(empty) != 0 {
		t.Errorf("超范围 offset 该回 0 条，实际 %d", len(empty))
	}
}

// testFeedbackFilters: filter by level / errorCode / time window.
func testFeedbackFilters(t *testing.T, newStore storeFactory) {
	s := newStore(t, false)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	// Four rows, **each with a value on every dimension different from the others**, so a wrong implementation that
	// "only looks at one dimension" cannot pass (same lesson as the mutation fixture).
	recs := []persistmodel.FeedbackRecord{
		feedbackRec("fbf_a", "auto", "a@example.com", "/r1", "internal", "k1", base),
		feedbackRec("fbf_b", "user", "b@example.com", "/r2", "access_denied", "", base.Add(time.Hour)),
		feedbackRec("fbf_c", "auto", "c@example.com", "/r3", "not_found", "k3", base.Add(2*time.Hour)),
		feedbackRec("fbf_d", "user", "d@example.com", "/r4", "network_error", "", base.Add(3*time.Hour)),
	}
	for _, rec := range recs {
		if err := s.PutFeedback(ctx, rec); err != nil {
			t.Fatalf("存反馈失败: %v", err)
		}
	}

	// level filter.
	got, total, err := s.ListFeedback(ctx, persistmodel.FeedbackQuery{Level: persistmodel.FeedbackAuto, Limit: 50})
	if err != nil {
		t.Fatalf("按 level 筛失败: %v", err)
	}
	if total != 2 || len(got) != 2 {
		t.Errorf("auto 级该有 2 条，实际 total=%d len=%d", total, len(got))
	}
	for _, r := range got {
		if r.Level != persistmodel.FeedbackAuto {
			t.Errorf("筛出了非 auto 的记录: %s (%s)", r.ID, r.Level)
		}
	}

	// errorCode filter.
	got, total, err = s.ListFeedback(ctx, persistmodel.FeedbackQuery{ErrorCode: "not_found", Limit: 50})
	if err != nil {
		t.Fatalf("按 errorCode 筛失败: %v", err)
	}
	if total != 1 || len(got) != 1 || got[0].ID != "fbf_c" {
		t.Errorf("按 errorCode 筛该只得 fbf_c，实际 total=%d got=%v", total, got)
	}

	// The time window is **half-open** [Since, Until).
	// Taking [base+1h, base+3h) should give fbf_b and fbf_c, without fbf_d (open on the right).
	got, total, err = s.ListFeedback(ctx, persistmodel.FeedbackQuery{
		Since: isoOf(base.Add(time.Hour)),
		Until: isoOf(base.Add(3 * time.Hour)),
		Limit: 50,
	})
	if err != nil {
		t.Fatalf("按时间窗筛失败: %v", err)
	}
	if total != 2 {
		t.Errorf("时间窗 [+1h, +3h) 该得 2 条，实际 %d", total)
	}
	for _, r := range got {
		if r.ID == "fbf_d" {
			t.Error("右开区间不该包含 until 那一刻的记录（fbf_d）——" +
				"闭区间会让边界那条同时落进相邻两页，脚本翻页时重复处理")
		}
		if r.ID == "fbf_a" {
			t.Error("时间窗之前的记录不该被筛出来（fbf_a）")
		}
	}

	// Both conditions together: level=auto and inside the window leaves only fbf_c.
	got, _, err = s.ListFeedback(ctx, persistmodel.FeedbackQuery{
		Level: persistmodel.FeedbackAuto,
		Since: isoOf(base.Add(time.Hour)),
		Limit: 50,
	})
	if err != nil {
		t.Fatalf("叠加条件筛失败: %v", err)
	}
	if len(got) != 1 || got[0].ID != "fbf_c" {
		t.Errorf("level+时间窗叠加该只得 fbf_c，实际 %v", got)
	}
}
