// Store contract tests for product Suggestions, **run once each against MemStore and PGStore
// by runStoreSuite in store_test.go**.
//
// # Why this group must run on both sides
//
// Four things are achieved by **completely different mechanisms** in the two implementations:
//
//	           PG                          memory
//	ordering   order by ord desc/asc       walk the slice backwards/forwards
//	counts     select count(*) subquery    len(map[id])
//	paging     limit/offset                pageSlice slicing
//
// Checking only one side leaves drift on the other unwatched, and unit tests run exactly the in-memory version, which means
// **wrong behavior on PG is the easiest to slip through**.
//
// The last one (paging) is specific to this domain and the easiest to get backwards: **cutting the page first and then filtering by author**
// makes page two look almost empty for the user, and it does not error.
// testSuggestionAuthorFilterAppliesBeforePaging pins exactly this.

package coredao

import (
	"context"
	"errors"
	"testing"

	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// seedSuggestion builds a suggestion, stores it, and returns the stored object.
func seedSuggestion(
	t *testing.T, s core.Store, id, title, author string, status apimodel.SuggestionStatus,
) apimodel.Suggestion {
	t.Helper()
	sg, err := s.CreateSuggestion(context.Background(), apimodel.Suggestion{
		ID:        id,
		Title:     title,
		Body:      "正文 " + title,
		Status:    status,
		Author:    author,
		CreatedAt: apimodel.NowISO(),
		UpdatedAt: apimodel.NowISO(),
	})
	if err != nil {
		t.Fatalf("建建议 %s 失败: %v", id, err)
	}
	return sg
}

// testSuggestionRoundTrip stores one and reads one back, comparing field by field.
//
// Its point is not "test CRUD" but pinning that **every field is really stored and can be read back**:
// on the PG side the insert and select column lists are two hand-written places, and missing one column gives no error at all;
// that field just always reads as the zero value (feedback's log_id hit this).
func testSuggestionRoundTrip(t *testing.T, newStore storeFactory) {
	s := newStore(t, false)
	ctx := context.Background()

	want := seedSuggestion(t, s, "sug_1", "希望支持深色模式", "amy@example.com", apimodel.SuggestionOpen)

	got, err := s.GetSuggestion(ctx, "sug_1")
	if err != nil {
		t.Fatalf("取建议失败: %v", err)
	}
	if got.ID != want.ID || got.Title != want.Title || got.Body != want.Body {
		t.Errorf("标题正文没对上：got %+v want %+v", got, want)
	}
	if got.Status != apimodel.SuggestionOpen {
		t.Errorf("状态 = %q，想要 open", got.Status)
	}
	if got.Author != "amy@example.com" {
		t.Errorf("作者 = %q", got.Author)
	}
	if got.CreatedAt == "" || got.UpdatedAt == "" {
		t.Errorf("时间戳是空的: %+v", got)
	}

	// A missing one must return ErrNotFound, not a zero value + nil.
	// With a zero value the caller gets a suggestion whose author is an empty string, and then authorization decides
	// "empty string != you" and returns 403, an error pointing in completely the wrong direction.
	if _, err := s.GetSuggestion(ctx, "sug_nope"); !errors.Is(err, persistmodel.ErrNotFound) {
		t.Errorf("取不存在的建议 err = %v，想要 ErrNotFound", err)
	}
}

// testSuggestionListOrderAndFilter pins descending order and the two filters.
//
// ⚠️ **Ordering cannot be checked with "two calls give the same result"**: on a small table a sequential scan happens to equal insertion order,
// so it stays green even without order by. This asserts **the specific order itself**.
func testSuggestionListOrderAndFilter(t *testing.T, newStore storeFactory) {
	s := newStore(t, false)
	ctx := context.Background()

	seedSuggestion(t, s, "sug_1", "第一条", "amy@example.com", apimodel.SuggestionOpen)
	seedSuggestion(t, s, "sug_2", "第二条", "bob@example.com", apimodel.SuggestionResolved)
	seedSuggestion(t, s, "sug_3", "第三条", "amy@example.com", apimodel.SuggestionOpen)

	all, err := s.ListSuggestions(ctx, persistmodel.SuggestionQuery{Limit: 50})
	if err != nil {
		t.Fatalf("列建议失败: %v", err)
	}
	// **Newest first**: sug_3 → sug_2 → sug_1.
	wantOrder := []string{"sug_3", "sug_2", "sug_1"}
	if len(all) != len(wantOrder) {
		t.Fatalf("列出 %d 条，想要 %d 条", len(all), len(wantOrder))
	}
	for i, want := range wantOrder {
		if all[i].ID != want {
			t.Errorf("第 %d 条 = %s，想要 %s（列表必须按 ord 倒序）", i, all[i].ID, want)
		}
	}

	// Filter by author.
	mine, err := s.ListSuggestions(ctx, persistmodel.SuggestionQuery{
		Author: "amy@example.com", Limit: 50,
	})
	if err != nil {
		t.Fatalf("按作者列失败: %v", err)
	}
	if len(mine) != 2 {
		t.Fatalf("amy 的建议 %d 条，想要 2 条", len(mine))
	}
	for _, sg := range mine {
		if sg.Author != "amy@example.com" {
			t.Errorf("按作者筛却拿到了 %s 的建议", sg.Author)
		}
	}

	// Filter by status.
	open, err := s.ListSuggestions(ctx, persistmodel.SuggestionQuery{
		Status: apimodel.SuggestionOpen, Limit: 50,
	})
	if err != nil {
		t.Fatalf("按状态列失败: %v", err)
	}
	if len(open) != 2 {
		t.Fatalf("open 的建议 %d 条，想要 2 条", len(open))
	}

	// Both conditions together mean **and**, not or.
	both, err := s.ListSuggestions(ctx, persistmodel.SuggestionQuery{
		Author: "bob@example.com", Status: apimodel.SuggestionOpen, Limit: 50,
	})
	if err != nil {
		t.Fatalf("双条件列失败: %v", err)
	}
	if len(both) != 0 {
		t.Errorf("bob 没有 open 的建议，却列出 %d 条——两个条件被当成「或」了", len(both))
	}
}

// testSuggestionAuthorFilterAppliesBeforePaging pins "filter first, then cut the page".
//
// ⚠️ **The whole point of this test is to guard against one specific wrong implementation**: cut the page by offset/limit first,
// then filter by author inside that page. With that, a user who submitted only 1 suggestion,
// with 5 suggestions from others ranked before theirs in the database, would get **a completely empty** first page,
// while the endpoint returns 200 and the list returns [], with no error at all.
//
// How the data is built matters: the target user's one must **rank behind the others** (smaller ord),
// otherwise the first page naturally includes it and the wrong implementation stays green.
func testSuggestionAuthorFilterAppliesBeforePaging(t *testing.T, newStore storeFactory) {
	s := newStore(t, false)
	ctx := context.Background()

	// Create the target user's one first: it has the smallest ord and comes last in descending order.
	seedSuggestion(t, s, "sug_mine", "我的建议", "amy@example.com", apimodel.SuggestionOpen)
	// Then create 5 from others; they rank in front.
	for _, id := range []string{"sug_o1", "sug_o2", "sug_o3", "sug_o4", "sug_o5"} {
		seedSuggestion(t, s, id, "别人的 "+id, "bob@example.com", apimodel.SuggestionOpen)
	}

	// The first page asks for only 2. A wrong implementation that cuts the page first would get [sug_o5, sug_o4],
	// and after filtering return an empty list.
	page, err := s.ListSuggestions(ctx, persistmodel.SuggestionQuery{
		Author: "amy@example.com", Limit: 2, Offset: 0,
	})
	if err != nil {
		t.Fatalf("列建议失败: %v", err)
	}
	if len(page) != 1 {
		t.Fatalf("amy 的第一页有 %d 条，想要 1 条——"+
			"分页很可能作用在了筛选之前（先切页再过滤），"+
			"那会让用户看到一个空列表而接口仍回 200", len(page))
	}
	if page[0].ID != "sug_mine" {
		t.Errorf("拿到 %s，想要 sug_mine", page[0].ID)
	}
}

// testSuggestionPaging pins limit/offset themselves.
func testSuggestionPaging(t *testing.T, newStore storeFactory) {
	s := newStore(t, false)
	ctx := context.Background()

	for _, id := range []string{"sug_1", "sug_2", "sug_3", "sug_4", "sug_5"} {
		seedSuggestion(t, s, id, "建议 "+id, "amy@example.com", apimodel.SuggestionOpen)
	}

	first, err := s.ListSuggestions(ctx, persistmodel.SuggestionQuery{Limit: 2, Offset: 0})
	if err != nil {
		t.Fatalf("列第一页失败: %v", err)
	}
	if len(first) != 2 || first[0].ID != "sug_5" || first[1].ID != "sug_4" {
		t.Fatalf("第一页 = %v，想要 [sug_5 sug_4]", ids(first))
	}

	second, err := s.ListSuggestions(ctx, persistmodel.SuggestionQuery{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("列第二页失败: %v", err)
	}
	if len(second) != 2 || second[0].ID != "sug_3" || second[1].ID != "sug_2" {
		t.Fatalf("第二页 = %v，想要 [sug_3 sug_2]", ids(second))
	}

	// offset past the total: return an empty slice, **not an error and not nil**.
	// nil would encode as JSON null, while the contract requires [] (the frontend and scripts iterate it directly).
	beyond, err := s.ListSuggestions(ctx, persistmodel.SuggestionQuery{Limit: 2, Offset: 99})
	if err != nil {
		t.Fatalf("越界翻页不该报错: %v", err)
	}
	if beyond == nil {
		t.Error("越界翻页回了 nil，契约要求非 nil 空切片（nil 会编码成 null）")
	}
	if len(beyond) != 0 {
		t.Errorf("越界翻页拿到 %d 条", len(beyond))
	}
}

func ids(list []apimodel.Suggestion) []string {
	out := make([]string, 0, len(list))
	for _, sg := range list {
		out = append(out, sg.ID)
	}
	return out
}

// testSuggestionCountsAreDerived pins that the two counts are computed.
//
// Derived and not stored: storing them would mean updating them on every reply and attachment add/delete, and missing one spot shows as
// "the list says 3 replies, opening it shows only 2", with no error at all, and both numbers look plausible.
func testSuggestionCountsAreDerived(t *testing.T, newStore storeFactory) {
	s := newStore(t, false)
	ctx := context.Background()

	seedSuggestion(t, s, "sug_1", "建议", "amy@example.com", apimodel.SuggestionOpen)

	// Right after creation both counts are 0.
	got, _ := s.GetSuggestion(ctx, "sug_1")
	if got.ReplyCount != 0 || got.AttachmentCount != 0 {
		t.Fatalf("新建的建议计数不是 0: %+v", got)
	}

	for _, id := range []string{"sre_1", "sre_2"} {
		if _, err := s.CreateSuggestionReply(ctx, apimodel.SuggestionReply{
			ID: id, SuggestionID: "sug_1", Body: "回复 " + id,
			Author: "staff@example.com", IsStaff: true,
			CreatedAt: apimodel.NowISO(), UpdatedAt: apimodel.NowISO(),
		}); err != nil {
			t.Fatalf("建回复失败: %v", err)
		}
	}
	if _, err := s.CreateSuggestionAttachment(ctx, apimodel.SuggestionAttachment{
		ID: "sat_1", SuggestionID: "sug_1", Filename: "a.png",
		ContentType: "image/png", Author: "amy@example.com", CreatedAt: apimodel.NowISO(),
		FileID: "file_1",
	}); err != nil {
		t.Fatalf("建附件失败: %v", err)
	}

	got, _ = s.GetSuggestion(ctx, "sug_1")
	if got.ReplyCount != 2 {
		t.Errorf("ReplyCount = %d，想要 2（计数必须现算）", got.ReplyCount)
	}
	if got.AttachmentCount != 1 {
		t.Errorf("AttachmentCount = %d，想要 1", got.AttachmentCount)
	}

	// The counts in the list must be right too: List and Get are two paths; the PG side keeps them consistent with the same column list constant,
	// and the in-memory version with the same withSuggestionCountsLocked.
	list, _ := s.ListSuggestions(ctx, persistmodel.SuggestionQuery{Limit: 50})
	if len(list) != 1 || list[0].ReplyCount != 2 || list[0].AttachmentCount != 1 {
		t.Errorf("列表里的计数与详情不一致: %+v", list)
	}

	// Delete one reply; the count must drop with it.
	if err := s.DeleteSuggestionReply(ctx, "sre_1"); err != nil {
		t.Fatalf("删回复失败: %v", err)
	}
	got, _ = s.GetSuggestion(ctx, "sug_1")
	if got.ReplyCount != 1 {
		t.Errorf("删掉一条回复后 ReplyCount = %d，想要 1", got.ReplyCount)
	}
}

// testSuggestionRepliesAreOrdered pins that replies are **ascending** (the opposite of the suggestion list's descending order).
func testSuggestionRepliesAreOrdered(t *testing.T, newStore storeFactory) {
	s := newStore(t, false)
	ctx := context.Background()

	seedSuggestion(t, s, "sug_1", "建议", "amy@example.com", apimodel.SuggestionOpen)
	for _, id := range []string{"sre_1", "sre_2", "sre_3"} {
		if _, err := s.CreateSuggestionReply(ctx, apimodel.SuggestionReply{
			ID: id, SuggestionID: "sug_1", Body: "回复 " + id,
			Author:    "amy@example.com",
			CreatedAt: apimodel.NowISO(), UpdatedAt: apimodel.NowISO(),
		}); err != nil {
			t.Fatalf("建回复失败: %v", err)
		}
	}

	list, err := s.ListSuggestionReplies(ctx, "sug_1")
	if err != nil {
		t.Fatalf("列回复失败: %v", err)
	}
	want := []string{"sre_1", "sre_2", "sre_3"}
	if len(list) != len(want) {
		t.Fatalf("列出 %d 条回复，想要 %d 条", len(list), len(want))
	}
	for i, id := range want {
		if list[i].ID != id {
			t.Errorf("第 %d 条回复 = %s，想要 %s（对话必须正序，从上往下读）",
				i, list[i].ID, id)
		}
	}
}

// testSuggestionReplyStaffFlagPersists pins that is_staff is really stored and can be read back.
//
// ⚠️ This guards against an implementation that "compares author against the allowlist at read time": with it,
// the true stored here would become false when read (the test environment has no allowlist wired).
// And the real consequence is that **the identity on old replies drifts with config**: after someone changes role and is removed from the list,
// their official answer from back then turns into a user comment.
func testSuggestionReplyStaffFlagPersists(t *testing.T, newStore storeFactory) {
	s := newStore(t, false)
	ctx := context.Background()

	seedSuggestion(t, s, "sug_1", "建议", "amy@example.com", apimodel.SuggestionOpen)

	if _, err := s.CreateSuggestionReply(ctx, apimodel.SuggestionReply{
		ID: "sre_staff", SuggestionID: "sug_1", Body: "我们下个版本做",
		Author: "staff@example.com", IsStaff: true,
		CreatedAt: apimodel.NowISO(), UpdatedAt: apimodel.NowISO(),
	}); err != nil {
		t.Fatalf("建平台方回复失败: %v", err)
	}
	if _, err := s.CreateSuggestionReply(ctx, apimodel.SuggestionReply{
		ID: "sre_user", SuggestionID: "sug_1", Body: "好的，谢谢",
		Author: "amy@example.com", IsStaff: false,
		CreatedAt: apimodel.NowISO(), UpdatedAt: apimodel.NowISO(),
	}); err != nil {
		t.Fatalf("建用户回复失败: %v", err)
	}

	list, err := s.ListSuggestionReplies(ctx, "sug_1")
	if err != nil {
		t.Fatalf("列回复失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("列出 %d 条回复，想要 2 条", len(list))
	}
	if !list[0].IsStaff {
		t.Error("平台方那条读出来 IsStaff = false —— " +
			"这个标记必须是落库的真列，不能读取时现算")
	}
	if list[1].IsStaff {
		t.Error("用户那条读出来 IsStaff = true")
	}

	// Editing the body must not touch is_staff.
	got, err := s.GetSuggestionReply(ctx, "sre_staff")
	if err != nil {
		t.Fatalf("取回复失败: %v", err)
	}
	got.Body = "改了正文"
	got.UpdatedAt = apimodel.NowISO()
	if _, err := s.SaveSuggestionReply(ctx, got); err != nil {
		t.Fatalf("存回复失败: %v", err)
	}
	after, _ := s.GetSuggestionReply(ctx, "sre_staff")
	if !after.IsStaff {
		t.Error("改完正文之后 IsStaff 变成 false 了——那一列不该被 Save 覆盖")
	}
	if after.Body != "改了正文" {
		t.Errorf("正文没改成: %q", after.Body)
	}
}

// testSuggestionAttachmentMetadata pins that attachment metadata is stored and read back as is: Size and file_id are given by the caller
// (the bytes are already in object storage and the Store cannot reach them), and a missing file_id is rejected, since such an attachment cannot be read back.
func testSuggestionAttachmentMetadata(t *testing.T, newStore storeFactory) {
	s := newStore(t, false)
	ctx := context.Background()

	seedSuggestion(t, s, "sug_1", "建议", "amy@example.com", apimodel.SuggestionOpen)

	created, err := s.CreateSuggestionAttachment(ctx, apimodel.SuggestionAttachment{
		ID: "sat_1", SuggestionID: "sug_1", Filename: "截图.png",
		ContentType: "image/png", Size: 1234, FileID: "file_abc",
		Author: "amy@example.com", CreatedAt: apimodel.NowISO(),
	})
	if err != nil {
		t.Fatalf("传附件失败: %v", err)
	}
	if created.Size != 1234 || created.FileID != "file_abc" {
		t.Errorf("返回值不对：%+v", created)
	}
	list, err := s.ListSuggestionAttachments(ctx, "sug_1")
	if err != nil || len(list) != 1 {
		t.Fatalf("列附件：%v %d", err, len(list))
	}
	if list[0].Size != 1234 || list[0].FileID != "file_abc" || list[0].Filename != "截图.png" {
		t.Errorf("列表里的元信息没存住：%+v", list[0])
	}
	got, err := s.GetSuggestionAttachment(ctx, "sat_1")
	if err != nil || got.FileID != "file_abc" {
		t.Errorf("Get 的 file_id 不对：%+v %v", got, err)
	}

	if _, err := s.CreateSuggestionAttachment(ctx, apimodel.SuggestionAttachment{
		ID: "sat_2", SuggestionID: "sug_1", Filename: "x.png", ContentType: "image/png",
		Author: "amy@example.com", CreatedAt: apimodel.NowISO(),
	}); err == nil {
		t.Error("缺 file_id 的附件应当被拒绝——字节只在对象存储里，没有 file_id 就读不回来")
	}
}

// testSuggestionDeleteCascades pins that deleting a suggestion also deletes its replies and attachment metadata (the bytes are in object storage and, per files.sql, are not deleted).
//
// Orphan data is never found by any read path (reads always start from the suggestion, then fetch children); it just keeps taking up space,
// and **the attachments are especially costly** since they hold bytes.
func testSuggestionDeleteCascades(t *testing.T, newStore storeFactory) {
	s := newStore(t, false)
	ctx := context.Background()

	seedSuggestion(t, s, "sug_1", "建议", "amy@example.com", apimodel.SuggestionOpen)
	if _, err := s.CreateSuggestionReply(ctx, apimodel.SuggestionReply{
		ID: "sre_1", SuggestionID: "sug_1", Body: "回复",
		Author: "amy@example.com", CreatedAt: apimodel.NowISO(), UpdatedAt: apimodel.NowISO(),
	}); err != nil {
		t.Fatalf("建回复失败: %v", err)
	}
	if _, err := s.CreateSuggestionAttachment(ctx, apimodel.SuggestionAttachment{
		ID: "sat_1", SuggestionID: "sug_1", Filename: "a.png",
		ContentType: "image/png", Author: "amy@example.com", CreatedAt: apimodel.NowISO(),
		FileID: "file_1",
	}); err != nil {
		t.Fatalf("建附件失败: %v", err)
	}

	if err := s.DeleteSuggestion(ctx, "sug_1"); err != nil {
		t.Fatalf("删建议失败: %v", err)
	}

	if _, err := s.GetSuggestion(ctx, "sug_1"); !errors.Is(err, persistmodel.ErrNotFound) {
		t.Errorf("删完还能取到建议: %v", err)
	}
	if replies, _ := s.ListSuggestionReplies(ctx, "sug_1"); len(replies) != 0 {
		t.Errorf("删建议后还剩 %d 条孤儿回复", len(replies))
	}
	if atts, _ := s.ListSuggestionAttachments(ctx, "sug_1"); len(atts) != 0 {
		t.Errorf("删建议后还剩 %d 个孤儿附件", len(atts))
	}

	// Deleting something missing is not an error (idempotent).
	if err := s.DeleteSuggestion(ctx, "sug_nope"); err != nil {
		t.Errorf("删不存在的建议报错了: %v", err)
	}
}

// testSuggestionSaveDoesNotChangeAuthor pins that Save does not change author.
//
// ⚠️ author is **the basis of authorization** in this domain (non-staff can only read rows where author = themselves).
// Letting Save change it opens a privilege escalation path: put your own suggestion under someone else's name,
// or the other way around, claim someone else's as yours. Same as SaveIssue not updating workspace_id.
func testSuggestionSaveDoesNotChangeAuthor(t *testing.T, newStore storeFactory) {
	s := newStore(t, false)
	ctx := context.Background()

	seedSuggestion(t, s, "sug_1", "建议", "amy@example.com", apimodel.SuggestionOpen)

	sg, err := s.GetSuggestion(ctx, "sug_1")
	if err != nil {
		t.Fatalf("取建议失败: %v", err)
	}
	sg.Author = "attacker@example.com"
	sg.Title = "改过的标题"
	sg.Status = apimodel.SuggestionResolved
	sg.UpdatedAt = apimodel.NowISO()
	if _, err := s.SaveSuggestion(ctx, sg); err != nil {
		t.Fatalf("存建议失败: %v", err)
	}

	after, _ := s.GetSuggestion(ctx, "sug_1")
	if after.Author != "amy@example.com" {
		t.Errorf("author 被 Save 改成了 %q —— 那一列是授权的依据，不能被覆盖",
			after.Author)
	}
	// What should change must still change.
	if after.Title != "改过的标题" {
		t.Errorf("标题没改成: %q", after.Title)
	}
	if after.Status != apimodel.SuggestionResolved {
		t.Errorf("状态没改成: %q", after.Status)
	}
}

// testSuggestionsAreNeverDeduped pins that **dedup is deliberately not done**.
//
// ⚠️ This test guards against "copying the partial unique index from issue and adding one here too".
// Two people submitting the same suggestion are two separate requests; merging them loses "how many people want it",
// and that is exactly what priority is based on. Even the same person submitting two identical ones should get two.
func testSuggestionsAreNeverDeduped(t *testing.T, newStore storeFactory) {
	s := newStore(t, false)
	ctx := context.Background()

	// Two people submit word-for-word identical suggestions.
	seedSuggestion(t, s, "sug_1", "希望支持深色模式", "amy@example.com", apimodel.SuggestionOpen)
	seedSuggestion(t, s, "sug_2", "希望支持深色模式", "bob@example.com", apimodel.SuggestionOpen)
	// The same person submits another identical one.
	seedSuggestion(t, s, "sug_3", "希望支持深色模式", "amy@example.com", apimodel.SuggestionOpen)

	all, err := s.ListSuggestions(ctx, persistmodel.SuggestionQuery{Limit: 50})
	if err != nil {
		t.Fatalf("列建议失败: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("三条内容相同的建议只剩 %d 条——建议**刻意不去重**，"+
			"合并会丢掉「有几个人想要」，而那正是判优先级的依据", len(all))
	}
}
