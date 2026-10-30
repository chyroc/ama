// Store contract tests for Issue, **run once each against MemStore and
// PGStore by runStoreSuite in store_test.go**.
//
// # Why this group must run on both sides
//
// Three things are achieved by **completely different mechanisms** in the two implementations:
//
//	           PG                          memory
//	ordering   order by ord desc/asc       walk the slice backwards/forwards
//	counts     select count(*) subquery    len(map[id])
//
// Checking only one side leaves drift on the other unwatched, and unit tests run exactly the in-memory version, which means
// **wrong behavior on PG is the easiest to slip through**. The skill_files case (the in-memory version stored the caller's
// Size as given, PG recomputed it as len(content)) was caught exactly like this, by SkillFilesRoundTrip.

package coredao

import (
	"context"
	"errors"
	"testing"

	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/model/apimodel"
)

// seedIssue builds an issue, stores it, and returns the stored object.
func seedIssue(t *testing.T, s core.Store, id, wsID, title string, status apimodel.IssueStatus) apimodel.Issue {
	t.Helper()
	iss, err := s.CreateIssue(context.Background(), apimodel.Issue{
		ID:          id,
		WorkspaceID: wsID,
		Title:       title,
		Body:        "正文 " + title,
		Status:      status,
		Priority:    apimodel.DefaultIssuePriority,
		Author:      "someone@example.com",
		CreatedAt:   apimodel.NowISO(),
		UpdatedAt:   apimodel.NowISO(),
	})
	if err != nil {
		t.Fatalf("建问题 %s 失败：%v", id, err)
	}
	return iss
}

// testIssueRoundTrip: basic store and read: create, read, update, delete.
func testIssueRoundTrip(t *testing.T, newStore storeFactory) {
	s := newStore(t, true)
	ctx := context.Background()

	created := seedIssue(t, s, "iss_1", seedWS, "登录后白屏", apimodel.IssueOpen)
	if created.Title != "登录后白屏" {
		t.Errorf("创建返回的标题不对：%q", created.Title)
	}

	got, err := s.GetIssue(ctx, "iss_1")
	if err != nil {
		t.Fatalf("读问题失败：%v", err)
	}
	if got.Title != "登录后白屏" || got.WorkspaceID != seedWS {
		t.Errorf("读回来的问题不对：%+v", got)
	}
	if got.Priority != apimodel.DefaultIssuePriority {
		t.Errorf("默认优先级应该是 %q，实际 %q", apimodel.DefaultIssuePriority, got.Priority)
	}

	// Update: both status and updated_at must be stored.
	got.Status = apimodel.IssueClosed
	got.Title = "改过的标题"
	got.UpdatedAt = "2026-09-01T00:00:00.000Z"
	if _, err := s.SaveIssue(ctx, got); err != nil {
		t.Fatalf("存问题失败：%v", err)
	}
	reread, err := s.GetIssue(ctx, "iss_1")
	if err != nil {
		t.Fatalf("重读失败：%v", err)
	}
	if reread.Status != apimodel.IssueClosed {
		t.Errorf("状态没存下去：%q", reread.Status)
	}
	if reread.Title != "改过的标题" {
		t.Errorf("标题没存下去：%q", reread.Title)
	}
	if reread.UpdatedAt != "2026-09-01T00:00:00.000Z" {
		t.Errorf("updated_at 没存下去：%q", reread.UpdatedAt)
	}

	// Updating something missing must return ErrNotFound, not succeed silently.
	_, err = s.SaveIssue(ctx, apimodel.Issue{ID: "iss_nope", WorkspaceID: seedWS})
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("改不存在的问题应报 ErrNotFound，实际 %v", err)
	}

	// Delete.
	if err := s.DeleteIssue(ctx, "iss_1"); err != nil {
		t.Fatalf("删问题失败：%v", err)
	}
	if _, err := s.GetIssue(ctx, "iss_1"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("删掉之后应报 ErrNotFound，实际 %v", err)
	}
	// Deleting something missing is not an error (idempotent, same as the other Deletes).
	if err := s.DeleteIssue(ctx, "iss_1"); err != nil {
		t.Errorf("重复删应该是幂等的，实际 %v", err)
	}
}

// testIssueListOrderAndFilter pins the list's ordering and filtering.
//
// **Descending** is the point here: in ascending order the newest issue would be on the last page, and that is exactly the most important one.
// The two implementations sort by completely different mechanisms (PG uses order by ord desc, the in-memory version walks the slice backwards),
// so each must run it.
func testIssueListOrderAndFilter(t *testing.T, newStore storeFactory) {
	s := newStore(t, true)
	ctx := context.Background()

	seedIssue(t, s, "iss_a", seedWS, "第一条", apimodel.IssueOpen)
	seedIssue(t, s, "iss_b", seedWS, "第二条", apimodel.IssueClosed)
	seedIssue(t, s, "iss_c", seedWS, "第三条", apimodel.IssueOpen)
	// One from another workspace; it must not appear in the list of the workspace above.
	seedIssue(t, s, "iss_other", "ws_other", "别的空间", apimodel.IssueOpen)

	all, err := s.ListIssues(ctx, seedWS, "")
	if err != nil {
		t.Fatalf("列问题失败：%v", err)
	}
	if len(all) != 3 {
		t.Fatalf("期望 3 条（不含别的空间），实际 %d 条", len(all))
	}
	// Descending: the last created comes first.
	if all[0].ID != "iss_c" || all[2].ID != "iss_a" {
		t.Errorf("列表不是倒序：%s, %s, %s", all[0].ID, all[1].ID, all[2].ID)
	}

	// Filter by status.
	open, err := s.ListIssues(ctx, seedWS, apimodel.IssueOpen)
	if err != nil {
		t.Fatalf("按状态列失败：%v", err)
	}
	if len(open) != 2 {
		t.Errorf("期望 2 条 open，实际 %d 条", len(open))
	}
	for _, iss := range open {
		if iss.Status != apimodel.IssueOpen {
			t.Errorf("筛 open 却返回了 %q", iss.Status)
		}
	}

	// Workspace isolation: issues of another workspace must never appear.
	for _, iss := range all {
		if iss.WorkspaceID != seedWS {
			t.Errorf("列表里混进了别的空间的问题：%+v", iss)
		}
	}

	// An empty result is a non-nil empty slice (the contract requires []; nil encodes as null and blows up the frontend's .map).
	none, err := s.ListIssues(ctx, "ws_empty", "")
	if err != nil {
		t.Fatalf("列空空间失败：%v", err)
	}
	if none == nil {
		t.Error("空结果必须是非 nil 空切片")
	}
}

// testIssueCountsAreDerived pins that the two counts are **computed**, not stored.
//
// Its value: if someone ever turns the counts into stored columns and forgets to update them somewhere,
// this goes red. In production that error shows as "the list says 3 comments, opening it shows only 2",
// with no error at all, and both numbers look plausible.
func testIssueCountsAreDerived(t *testing.T, newStore storeFactory) {
	s := newStore(t, true)
	ctx := context.Background()

	seedIssue(t, s, "iss_1", seedWS, "有评论的问题", apimodel.IssueOpen)

	// Right after creation both counts are 0.
	got, _ := s.GetIssue(ctx, "iss_1")
	if got.CommentCount != 0 || got.AttachmentCount != 0 {
		t.Errorf("新建的问题计数应该都是 0，实际 %d/%d",
			got.CommentCount, got.AttachmentCount)
	}

	// Add two comments and one attachment. **Note that no "update counts" method is called**:
	// the counts must keep up by themselves; that is what "derived" means.
	for _, id := range []string{"cmt_1", "cmt_2"} {
		if _, err := s.CreateIssueComment(ctx, apimodel.IssueComment{
			ID: id, IssueID: "iss_1", WorkspaceID: seedWS,
			Body: "一条评论", Author: "a@example.com",
			CreatedAt: apimodel.NowISO(), UpdatedAt: apimodel.NowISO(),
		}); err != nil {
			t.Fatalf("建评论失败：%v", err)
		}
	}
	if _, err := s.CreateIssueAttachment(ctx, apimodel.IssueAttachment{
		ID: "att_1", IssueID: "iss_1", WorkspaceID: seedWS,
		Filename: "a.png", ContentType: "image/png",
		Author: "a@example.com", CreatedAt: apimodel.NowISO(),
		FileID: "file_1",
	}); err != nil {
		t.Fatalf("建附件失败：%v", err)
	}

	got, _ = s.GetIssue(ctx, "iss_1")
	if got.CommentCount != 2 {
		t.Errorf("期望 2 条评论，实际 %d", got.CommentCount)
	}
	if got.AttachmentCount != 1 {
		t.Errorf("期望 1 个附件，实际 %d", got.AttachmentCount)
	}

	// The counts in the list must be right too (on the PG side the list and detail are two SQLs, easy to change only one).
	list, _ := s.ListIssues(ctx, seedWS, "")
	if len(list) != 1 || list[0].CommentCount != 2 || list[0].AttachmentCount != 1 {
		t.Errorf("列表里的计数与详情不一致：%+v", list)
	}

	// Delete one comment; the count must drop with it.
	if err := s.DeleteIssueComment(ctx, "cmt_1"); err != nil {
		t.Fatalf("删评论失败：%v", err)
	}
	got, _ = s.GetIssue(ctx, "iss_1")
	if got.CommentCount != 1 {
		t.Errorf("删掉一条后期望 1 条评论，实际 %d", got.CommentCount)
	}
}

// testIssueDeleteCascades pins "deleting an issue also deletes its comments and attachments".
//
// Missing it leaves orphan data: no read path will ever find it (reads always start from the issue, then fetch children),
// the database just keeps growing. **The attachments are especially costly, since they hold bytes.**
func testIssueDeleteCascades(t *testing.T, newStore storeFactory) {
	s := newStore(t, true)
	ctx := context.Background()

	seedIssue(t, s, "iss_1", seedWS, "要删的问题", apimodel.IssueOpen)
	if _, err := s.CreateIssueComment(ctx, apimodel.IssueComment{
		ID: "cmt_1", IssueID: "iss_1", WorkspaceID: seedWS,
		Body: "评论", Author: "a@example.com",
		CreatedAt: apimodel.NowISO(), UpdatedAt: apimodel.NowISO(),
	}); err != nil {
		t.Fatalf("建评论失败：%v", err)
	}
	if _, err := s.CreateIssueAttachment(ctx, apimodel.IssueAttachment{
		ID: "att_1", IssueID: "iss_1", WorkspaceID: seedWS,
		Filename: "a.png", ContentType: "image/png",
		Author: "a@example.com", CreatedAt: apimodel.NowISO(),
		FileID: "file_1",
	}); err != nil {
		t.Fatalf("建附件失败：%v", err)
	}

	if err := s.DeleteIssue(ctx, "iss_1"); err != nil {
		t.Fatalf("删问题失败：%v", err)
	}

	// Comments are gone.
	if _, err := s.GetIssueComment(ctx, "cmt_1"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("评论应该跟着删掉，实际 %v", err)
	}
	// Attachment metadata is gone.
	if _, err := s.GetIssueAttachment(ctx, "att_1"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("附件应该跟着删掉，实际 %v", err)
	}
}

// testIssueAttachmentMetadata pins that attachment metadata is stored and read back as is, and that a missing file_id is rejected
// (the bytes are only in object storage; an attachment without file_id cannot be read back).
func testIssueAttachmentMetadata(t *testing.T, newStore storeFactory) {
	s := newStore(t, true)
	ctx := context.Background()

	seedIssue(t, s, "iss_1", seedWS, "带附件的问题", apimodel.IssueOpen)
	created, err := s.CreateIssueAttachment(ctx, apimodel.IssueAttachment{
		ID: "att_1", IssueID: "iss_1", WorkspaceID: seedWS,
		Filename: "note.txt", ContentType: "text/plain", Size: 42, FileID: "file_abc",
		Author: "a@example.com", CreatedAt: apimodel.NowISO(),
	})
	if err != nil {
		t.Fatalf("建附件失败：%v", err)
	}
	if created.Size != 42 || created.FileID != "file_abc" {
		t.Errorf("返回值不对：%+v", created)
	}
	list, err := s.ListIssueAttachments(ctx, "iss_1")
	if err != nil || len(list) != 1 {
		t.Fatalf("列附件：%v %d", err, len(list))
	}
	if list[0].Size != 42 || list[0].FileID != "file_abc" {
		t.Errorf("列表里的元信息没存住：%+v", list[0])
	}
	got, err := s.GetIssueAttachment(ctx, "att_1")
	if err != nil || got.FileID != "file_abc" || got.WorkspaceID != seedWS {
		t.Errorf("Get 不对：%+v %v", got, err)
	}

	if _, err := s.CreateIssueAttachment(ctx, apimodel.IssueAttachment{
		ID: "att_2", IssueID: "iss_1", WorkspaceID: seedWS,
		Filename: "x.png", ContentType: "image/png",
		Author: "a@example.com", CreatedAt: apimodel.NowISO(),
	}); err == nil {
		t.Error("缺 file_id 的附件应当被拒绝")
	}
}

// testIssueCommentsAreOrdered pins that comments are **ascending**.
//
// The opposite of the issue list's descending order, on purpose: the issue list wants "newest first",
// while the discussion inside one issue should read top to bottom in the order it happened. Reversed, the conversation would show backwards,
// with replies appearing before what they reply to.
func testIssueCommentsAreOrdered(t *testing.T, newStore storeFactory) {
	s := newStore(t, true)
	ctx := context.Background()

	seedIssue(t, s, "iss_1", seedWS, "讨论", apimodel.IssueOpen)
	for _, id := range []string{"cmt_1", "cmt_2", "cmt_3"} {
		if _, err := s.CreateIssueComment(ctx, apimodel.IssueComment{
			ID: id, IssueID: "iss_1", WorkspaceID: seedWS,
			Body: "第 " + id + " 条", Author: "a@example.com",
			CreatedAt: apimodel.NowISO(), UpdatedAt: apimodel.NowISO(),
		}); err != nil {
			t.Fatalf("建评论 %s 失败：%v", id, err)
		}
	}

	list, err := s.ListIssueComments(ctx, "iss_1")
	if err != nil {
		t.Fatalf("列评论失败：%v", err)
	}
	if len(list) != 3 {
		t.Fatalf("期望 3 条评论，实际 %d", len(list))
	}
	if list[0].ID != "cmt_1" || list[2].ID != "cmt_3" {
		t.Errorf("评论必须是正序（对话从上往下读），实际 %s, %s, %s",
			list[0].ID, list[1].ID, list[2].ID)
	}

	// Update a comment: both body and updated_at must be stored.
	list[1].Body = "改过的评论"
	list[1].UpdatedAt = "2026-09-01T00:00:00.000Z"
	if _, err := s.SaveIssueComment(ctx, list[1]); err != nil {
		t.Fatalf("存评论失败：%v", err)
	}
	got, err := s.GetIssueComment(ctx, "cmt_2")
	if err != nil {
		t.Fatalf("重读评论失败：%v", err)
	}
	if got.Body != "改过的评论" {
		t.Errorf("评论正文没存下去：%q", got.Body)
	}
	if got.UpdatedAt != "2026-09-01T00:00:00.000Z" {
		t.Errorf("评论的 updated_at 没存下去：%q", got.UpdatedAt)
	}

	// An empty list is a non-nil empty slice.
	empty, err := s.ListIssueComments(ctx, "iss_nope")
	if err != nil {
		t.Fatalf("列不存在问题的评论失败：%v", err)
	}
	if empty == nil {
		t.Error("空结果必须是非 nil 空切片")
	}
}

// testDeleteWorkspaceCascadesIssues pins "deleting a workspace also deletes its issues, comments and attachments".
//
// A separate test instead of merging into the existing DeleteWorkspaceCascades: that one adds assertions on existing resources,
// while these three tables are new, and putting them together would make that test's failure messages hard to read.
func testDeleteWorkspaceCascadesIssues(t *testing.T, newStore storeFactory) {
	s := newStore(t, true)
	ctx := context.Background()

	seedIssue(t, s, "iss_1", seedWS, "会被级联删掉的问题", apimodel.IssueOpen)
	if _, err := s.CreateIssueComment(ctx, apimodel.IssueComment{
		ID: "cmt_1", IssueID: "iss_1", WorkspaceID: seedWS,
		Body: "评论", Author: "a@example.com",
		CreatedAt: apimodel.NowISO(), UpdatedAt: apimodel.NowISO(),
	}); err != nil {
		t.Fatalf("建评论失败：%v", err)
	}
	if _, err := s.CreateIssueAttachment(ctx, apimodel.IssueAttachment{
		ID: "att_1", IssueID: "iss_1", WorkspaceID: seedWS,
		Filename: "a.png", ContentType: "image/png",
		Author: "a@example.com", CreatedAt: apimodel.NowISO(),
		FileID: "file_1",
	}); err != nil {
		t.Fatalf("建附件失败：%v", err)
	}

	if err := s.DeleteWorkspace(ctx, seedWS); err != nil {
		t.Fatalf("删空间失败：%v", err)
	}

	if _, err := s.GetIssue(ctx, "iss_1"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("问题应该跟着空间删掉，实际 %v", err)
	}
	if _, err := s.GetIssueComment(ctx, "cmt_1"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("评论应该跟着空间删掉，实际 %v", err)
	}
	if _, err := s.GetIssueAttachment(ctx, "att_1"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("附件应该跟着空间删掉，实际 %v", err)
	}
}

// testIssueBotFingerprintDedup: issues produced by bots are deduped by origin + fingerprint,
// while human-written ones (both columns empty) **never take part**.
//
// # Why this must run on PG too
//
// The two Stores implement the same thing with completely different mechanisms:
//
//	PG       the partial unique index issues_bot_fingerprint_key (where origin <> '' ...)
//	memory   a piece of Go logic in CreateIssue
//
// Checking only the in-memory version, a wrong `where` on that PG index (typically missing `origin <> ”`,
// so human-written issues get pulled into dedup too) **goes unwatched**, and that breaks exactly
// the invariant in README-issue.md: "two people reporting the same thing should be two issues".
func testIssueBotFingerprintDedup(t *testing.T, newStore storeFactory) {
	s := newStore(t, true)
	ctx := context.Background()

	bot := func(id, fp, title string) (apimodel.Issue, error) {
		return s.CreateIssue(ctx, apimodel.Issue{
			ID:          id,
			WorkspaceID: seedWS,
			Title:       title,
			Body:        "机器人发现的",
			Status:      apimodel.IssueOpen,
			Priority:    apimodel.DefaultIssuePriority,
			Author:      "owner@example.com",
			CreatedAt:   apimodel.NowISO(),
			UpdatedAt:   apimodel.NowISO(),
			Origin:      apimodel.IssueOriginSchedule,
			Fingerprint: fp,
		})
	}

	if _, err := bot("iss_bot_1", "fp_aaa", "对话页 500"); err != nil {
		t.Fatalf("建第一条机器人问题失败：%v", err)
	}
	// A second one with the same fingerprint must be rejected: this is the correctness line of dedup (with multiple instances, check-then-create is TOCTOU).
	if _, err := bot("iss_bot_2", "fp_aaa", "对话页 500"); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("同 origin 同指纹的第二条该撞冲突，实际 err=%v", err)
	}
	// A different fingerprint gets in as usual (guards against over-merging).
	if _, err := bot("iss_bot_3", "fp_bbb", "登录超时"); err != nil {
		t.Fatalf("不同指纹该建得进去：%v", err)
	}

	// ⭐ The two human-written ones (both origin and fingerprint empty) **must both get in**.
	// This is the core of this test: if the unique index missed `where origin <> ''`, the second would be rejected,
	// and that would quietly overturn the design decision "two people reporting the same thing should be two issues".
	seedIssue(t, s, "iss_human_1", seedWS, "同一个现象", apimodel.IssueOpen)
	seedIssue(t, s, "iss_human_2", seedWS, "同一个现象", apimodel.IssueOpen)
}
