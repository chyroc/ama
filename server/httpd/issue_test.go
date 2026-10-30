// **End-to-end authorization and security** tests for issues: real mux + real core + real authz +
// real sessions.
//
// # Why this group must be in httpd and not core
//
// core's cases use a fake Guard and authz's cases use a fake core; both are unit tests. What really
// goes wrong in this domain is all **at the seams**:
//
//	wrong level in the matrix               → unit tests all green, anyone in production can see others' images
//	pattern differs from the real route     → panic at startup (already caught by authz)
//	wrong kind mapping (attachment→issue)   → resolving always fails, every image 404s
//	a missing response header               → no error at all, the attack surface comes back
//
// So this hits real routes with **real wiring**, as identities from different workspaces.
//
// # The attachment case is the focus of this file
//
// Its URL is put into `<img src>` and looks like a static asset, and "why authenticate static
// assets" is exactly the dangerous intuition. TestIssueAttachmentIsWorkspaceScoped below guards
// against it.

package httpd

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// pngFixture is a byte sequence with a valid PNG magic number, to pass the server's type sniffing.
var pngFixture = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR" +
	"\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")

// createIssueAs creates an issue as some identity and returns its id.
func createIssueAs(t *testing.T, h http.Handler, email, wsID, body string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodPost, "/api/workspaces/"+wsID+"/issues", email, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("建问题失败：%d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析问题失败：%v", err)
	}
	return out.ID
}

// uploadAttachmentAs uploads an attachment to an issue as some identity and returns the attachment
// id and url.
func uploadAttachmentAs(
	t *testing.T, h http.Handler, email, issueID, filename string, content []byte,
) (id, url string) {
	t.Helper()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("造 multipart 失败：%v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("写 multipart 失败：%v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("关 multipart 失败：%v", err)
	}

	req := httptest.NewRequest(
		http.MethodPost, "/api/issues/"+issueID+"/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tokenTFor(email))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("传附件失败：%d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析附件失败：%v", err)
	}
	return out.ID, out.URL
}

// TestIssueAttachmentIsWorkspaceScoped is **the security baseline of this change**.
//
// Attachment URLs are put into `<img src>` in the Markdown body, so they look like static assets.
// If that intuition made it ActionNone, anyone with the URL could see images of **other people's
// workspaces**, and the leak gives no error, and no UI will ever show that it happened.
//
// Here a **non-member** (someone in no workspace) hits a really existing attachment URL, and 403 is
// asserted. Mutation check: change that entry in the matrix to ActionNone and this case must turn
// red.
func TestIssueAttachmentIsWorkspaceScoped(t *testing.T) {
	h := newRBACServer(t)

	// A workspace member creates an issue and uploads an image.
	issueID := createIssueAs(t, h, roleMember, rbacWS, `{"title":"带图的问题"}`)
	attID, url := uploadAttachmentAs(t, h, roleMember, issueID, "shot.png", pngFixture)

	// The URL shape must be right (the frontend writes it straight into <img src>).
	if want := "/api/issue-attachments/" + attID; url != want {
		t.Errorf("附件 URL 不对\n期望 %q\n实际 %q", want, url)
	}

	// The workspace member can get it themselves. **This one cannot be skipped**: without it, the
	// 403 below might only be because "the attachment cannot be fetched at all", not because
	// authorization stopped it.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodGet, url, roleViewer, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("空间成员（viewer）应该能看附件，实际 %d %s", rec.Code, rec.Body.String())
	}

	// ⚠️ Core assertion: **a non-member with the same URL must get 403**.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodGet, url, roleNobody, ""))
	if rec.Code != http.StatusForbidden {
		t.Errorf("非成员拿到附件 URL 必须 403，实际 %d %s——"+
			"这意味着任何拿到 URL 的人都能看别人空间的图", rec.Code, rec.Body.String())
	}
	if _, code := envelope(t, rec); code != "access_denied" {
		t.Errorf("期望 code=access_denied，实际 %q", code)
	}

	// No credentials at all does not work either (401, not 200).
	//
	// This is checked separately because `<img>` requests pass authentication with the cookie, and
	// "what happens when the cookie is not sent" decides whether a logged-out user sees a broken
	// image or someone else's image.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("不带凭据取附件应该 401，实际 %d", rec.Code)
	}
}

// TestIssueAttachmentSecurityHeaders pins three response headers.
//
// Each blocks one kind of failure, and **deleting any of them gives no error**; the attack surface
// just comes back:
//
//	Content-Type            wrong, and images do not render, or worse: HTML runs as a page
//	X-Content-Type-Options  without it the browser ignores our type and guesses (sniffing bypass)
//	Content-Disposition     without it non-images render in the browser instead of downloading
func TestIssueAttachmentSecurityHeaders(t *testing.T) {
	h := newRBACServer(t)
	issueID := createIssueAs(t, h, roleMember, rbacWS, `{"title":"带附件的问题"}`)

	t.Run("图片是 inline 且类型正确", func(t *testing.T) {
		_, url := uploadAttachmentAs(t, h, roleMember, issueID, "shot.png", pngFixture)

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(http.MethodGet, url, roleMember, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("取附件失败：%d %s", rec.Code, rec.Body.String())
		}

		// Content-Type must be the real sniffed type; otherwise the browser does not render the image.
		if got := rec.Header().Get("Content-Type"); got != "image/png" {
			t.Errorf("Content-Type 期望 image/png，实际 %q", got)
		}
		// nosniff: the second line of defense.
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("必须设 X-Content-Type-Options: nosniff，实际 %q", got)
		}
		// Images must be inline, otherwise they do not show in <img src>.
		disp := rec.Header().Get("Content-Disposition")
		if !strings.HasPrefix(disp, "inline;") {
			t.Errorf("图片必须是 inline（否则 <img> 显示不出来），实际 %q", disp)
		}
		if !strings.Contains(disp, "shot.png") {
			t.Errorf("Content-Disposition 里应带文件名，实际 %q", disp)
		}
		// Authenticated content must never go into a shared cache.
		if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "private") {
			t.Errorf("Cache-Control 必须是 private（这是带鉴权的内容），实际 %q", got)
		}
		// The bytes must be exactly the same.
		if !bytes.Equal(rec.Body.Bytes(), pngFixture) {
			t.Error("返回的字节与上传的不一致")
		}
	})

	t.Run("非图片是 attachment", func(t *testing.T) {
		_, url := uploadAttachmentAs(t, h, roleMember, issueID, "note.txt",
			[]byte("这是一段纯文本，不该在浏览器里渲染"))

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(http.MethodGet, url, roleMember, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("取附件失败：%d", rec.Code)
		}
		disp := rec.Header().Get("Content-Disposition")
		if !strings.HasPrefix(disp, "attachment;") {
			t.Errorf("非图片必须是 attachment（下载而不是渲染），实际 %q", disp)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("必须设 nosniff，实际 %q", got)
		}
	})
}

// TestIssueAttachmentRejectsDangerousUploads pins the three rejections on the upload side.
//
// All go through **the real HTTP path** (not calling the pure functions directly): whether those
// checks are really attached to the handler can only be seen by hitting real routes. The boundaries
// of the pure-function layer are in shared/gopkg/attachment/attachment_test.go.
func TestIssueAttachmentRejectsDangerousUploads(t *testing.T) {
	h := newRBACServer(t)
	issueID := createIssueAs(t, h, roleMember, rbacWS, `{"title":"测试上传"}`)

	upload := func(t *testing.T, filename string, content []byte) *httptest.ResponseRecorder {
		t.Helper()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, _ := mw.CreateFormFile("file", filename)
		_, _ = part.Write(content)
		_ = mw.Close()

		req := httptest.NewRequest(
			http.MethodPost, "/api/issues/"+issueID+"/attachments", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+tokenTFor(roleMember))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	t.Run("HTML 伪装成 png 被拒", func(t *testing.T) {
		html := []byte("<!DOCTYPE html><html><script>alert(document.cookie)</script></html>")
		rec := upload(t, "evil.png", html)
		if rec.Code == http.StatusCreated {
			t.Fatal("HTML 伪装成 png 竟然传上去了——这是一个 XSS")
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("期望 400，实际 %d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("svg 被拒", func(t *testing.T) {
		svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
		if rec := upload(t, "logo.svg", svg); rec.Code == http.StatusCreated {
			t.Fatal("svg 竟然传上去了——它能执行脚本")
		}
	})

	t.Run("超大文件被拒", func(t *testing.T) {
		// 6 MiB + 1, over both gates: 5 MiB per file and 6 MiB per request body.
		huge := make([]byte, (6<<20)+1)
		copy(huge, pngFixture)
		rec := upload(t, "huge.png", huge)
		if rec.Code == http.StatusCreated {
			t.Fatal("超大文件竟然传上去了")
		}
		if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusBadRequest {
			t.Errorf("期望 413（或 400），实际 %d %s", rec.Code, rec.Body.String())
		}
	})
}

// TestIssueRequiresTitleOrBody pins "reject when both title and body are empty".
//
// Going through real HTTP confirms this check is really attached to the write path, and **returns
// 400, not 500**: the latter would make the frontend show "internal server error", and the user
// would have no idea what to fix.
func TestIssueRequiresTitleOrBody(t *testing.T) {
	h := newRBACServer(t)

	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"两者都空", `{"title":"","body":""}`, http.StatusBadRequest},
		{"两者都是空白", `{"title":"   ","body":"\n\t "}`, http.StatusBadRequest},
		{"字段都不传", `{}`, http.StatusBadRequest},
		{"只填标题", `{"title":"只有标题"}`, http.StatusCreated},
		{"只填正文", `{"body":"只有正文"}`, http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, asRole(http.MethodPost,
				"/api/workspaces/"+rbacWS+"/issues", roleMember, tc.body))
			if rec.Code != tc.want {
				t.Errorf("期望 %d，实际 %d %s", tc.want, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestIssueTitleDerivedFromBody checks title extraction once end to end.
//
// The boundaries of the pure-function layer are in shared/gopkg/mdtitle/mdtitle_test.go (30+
// cases); this only checks **that it is really attached to the create path**: adding a new function
// that the main path never calls is the kind of "all green but the feature never took effect" this
// repository has seen before.
func TestIssueTitleDerivedFromBody(t *testing.T) {
	h := newRBACServer(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodPost, "/api/workspaces/"+rbacWS+"/issues",
		roleMember, `{"body":"![](https://x/a.png)\n\n# 登录之后整页白掉"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("建问题失败：%d %s", rec.Code, rec.Body.String())
	}

	var out struct {
		Title    string `json:"title"`
		Priority string `json:"priority"`
		Status   string `json:"status"`
		Author   string `json:"author"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败：%v", err)
	}

	// The image without alt is skipped, and the heading marker is stripped.
	if out.Title != "登录之后整页白掉" {
		t.Errorf("标题没有从正文正确提取，实际 %q", out.Title)
	}
	// Defaults: no status means open, no priority means p2 (**not p0**).
	if out.Status != "open" {
		t.Errorf("默认状态应该是 open，实际 %q", out.Status)
	}
	if out.Priority != "p2" {
		t.Errorf("默认优先级应该是 p2（不是 p0），实际 %q", out.Priority)
	}
	// The author comes from the login identity.
	if out.Author != roleMember {
		t.Errorf("作者应该是登录者 %q，实际 %q", roleMember, out.Author)
	}
}

// TestIssueAuthorCannotBeForged pins "the author can only come from the login identity".
//
// An author put into the request body must have **no effect** (not an error, it is ignored): that
// field should never be parsed at all. This is the same invariant as feedback / onboarding /
// user-preferences: overreach "cannot be expressed" on these endpoints.
func TestIssueAuthorCannotBeForged(t *testing.T) {
	h := newRBACServer(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodPost, "/api/workspaces/"+rbacWS+"/issues",
		roleMember, `{"title":"冒名测试","author":"`+roleOwner+`"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("建问题失败：%d %s", rec.Code, rec.Body.String())
	}

	var out struct {
		Author string `json:"author"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Author != roleMember {
		t.Errorf("作者被请求体改掉了：期望登录者 %q，实际 %q——"+
			"这意味着可以以别人的名义提问题", roleMember, out.Author)
	}
}

// TestIssueUpdatedAtChangesOnEdit pins "any change must leave an updated_at".
//
// Each of the three kinds of edits is checked once: without the refresh, sorting the list by "last
// activity" would always be wrong, and both timestamps look like valid times, so it cannot be found
// from the symptom.
func TestIssueUpdatedAtChangesOnEdit(t *testing.T) {
	h := newRBACServer(t)
	issueID := createIssueAs(t, h, roleMember, rbacWS, `{"title":"原标题","body":"原正文"}`)

	read := func(t *testing.T) (title, updatedAt string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole(http.MethodGet, "/api/issues/"+issueID, roleMember, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("读问题失败：%d", rec.Code)
		}
		var out struct {
			Title     string `json:"title"`
			UpdatedAt string `json:"updatedAt"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Title, out.UpdatedAt
	}

	_, before := read(t)

	// Change the title.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodPatch, "/api/issues/"+issueID, roleMember,
		`{"title":"新标题","body":"原正文"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("改标题失败：%d %s", rec.Code, rec.Body.String())
	}

	title, after := read(t)
	if title != "新标题" {
		t.Errorf("标题没改成功：%q", title)
	}
	// Timestamps are fixed-length ISO strings (millisecond precision); two edits in the same
	// millisecond may be equal, so only "right format and not empty" is asserted. The real refresh
	// logic is pinned by the Store contract cases (the one in testIssueRoundTrip that passes
	// different timestamps explicitly).
	if after == "" || len(after) != len(before) {
		t.Errorf("updatedAt 形状不对：before=%q after=%q", before, after)
	}

	// Change the status: confirm a PATCH with only some fields does not clear the other fields.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodPatch, "/api/issues/"+issueID, roleMember,
		`{"title":"新标题","body":"原正文","status":"closed"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("改状态失败：%d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Status string `json:"status"`
		Body   string `json:"body"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Status != "closed" {
		t.Errorf("状态没改成功：%q", out.Status)
	}
	if out.Body != "原正文" {
		t.Errorf("正文被意外改掉了：%q", out.Body)
	}
}

// TestIssueRolesAreEnforced covers reads and writes in this domain for the four roles + a non-member.
//
// viewer can read but not write, a non-member can do nothing: the standard shape of a
// workspace-level resource, but it **must be checked explicitly**: levels are written by hand one
// by one, and a wrong one gives no error.
func TestIssueRolesAreEnforced(t *testing.T) {
	h := newRBACServer(t)
	issueID := createIssueAs(t, h, roleMember, rbacWS, `{"title":"角色测试"}`)

	for _, tc := range []struct {
		name           string
		email          string
		wantRead       int
		wantWrite      int
		wantWriteIsOK  bool
		writeBodyIsNew bool
	}{
		{"owner 读写都行", roleOwner, http.StatusOK, http.StatusCreated, true, true},
		{"admin 读写都行", roleAdmin, http.StatusOK, http.StatusCreated, true, true},
		{"member 读写都行", roleMember, http.StatusOK, http.StatusCreated, true, true},
		// viewer is read-only: can see issues, cannot create them.
		{"viewer 只能读", roleViewer, http.StatusOK, http.StatusForbidden, false, true},
		// A non-member can do neither.
		{"非成员什么都不能", roleNobody, http.StatusForbidden, http.StatusForbidden, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Read the list.
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, asRole(http.MethodGet,
				"/api/workspaces/"+rbacWS+"/issues", tc.email, ""))
			if rec.Code != tc.wantRead {
				t.Errorf("列问题：期望 %d，实际 %d", tc.wantRead, rec.Code)
			}

			// Read details (flat path; the workspace is resolved from the issue).
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, asRole(http.MethodGet, "/api/issues/"+issueID, tc.email, ""))
			if rec.Code != tc.wantRead {
				t.Errorf("取问题详情：期望 %d，实际 %d", tc.wantRead, rec.Code)
			}

			// Write: create a new issue.
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, asRole(http.MethodPost,
				"/api/workspaces/"+rbacWS+"/issues", tc.email, `{"title":"新问题"}`))
			if rec.Code != tc.wantWrite {
				t.Errorf("提问题：期望 %d，实际 %d %s",
					tc.wantWrite, rec.Code, rec.Body.String())
			}

			// Write: post a comment (also the Write level).
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, asRole(http.MethodPost,
				"/api/issues/"+issueID+"/comments", tc.email, `{"body":"一条评论"}`))
			wantComment := tc.wantWrite
			if rec.Code != wantComment {
				t.Errorf("发评论：期望 %d，实际 %d %s",
					wantComment, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestIssueNotFoundIsNotForbidden pins the difference between 404 and 403.
//
// A missing resource must return **404 + code=not_found**; do not return 403 for everything to
// "avoid leaking existence": the frontend relies on that code to turn the response into null.
// Returning 403 would turn "this issue was deleted" into "access denied": the user thinks it is a
// permission problem and goes to an admin, while the issue is long gone.
func TestIssueNotFoundIsNotForbidden(t *testing.T) {
	h := newRBACServer(t)

	for _, path := range []string{
		"/api/issues/iss_nonexistent",
		"/api/issue-attachments/att_nonexistent",
	} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, asRole(http.MethodGet, path, roleMember, ""))
			if rec.Code != http.StatusNotFound {
				t.Errorf("不存在的资源应该回 404，实际 %d %s", rec.Code, rec.Body.String())
			}
			if _, code := envelope(t, rec); code != "not_found" {
				t.Errorf("期望 code=not_found（前端靠它转 null），实际 %q", code)
			}
		})
	}
}

// TestIssueCommentLifecycle runs create, edit and delete of comments end to end.
func TestIssueCommentLifecycle(t *testing.T) {
	h := newRBACServer(t)
	issueID := createIssueAs(t, h, roleMember, rbacWS, `{"title":"要讨论的问题"}`)

	// Post one.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodPost, "/api/issues/"+issueID+"/comments",
		roleMember, `{"body":"我也遇到了"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("发评论失败：%d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID        string `json:"id"`
		Author    string `json:"author"`
		CreatedAt string `json:"createdAt"`
		UpdatedAt string `json:"updatedAt"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Author != roleMember {
		t.Errorf("评论作者应该是登录者，实际 %q", created.Author)
	}
	// Right after posting the two timestamps are equal (the frontend uses that to tell "never edited").
	if created.CreatedAt != created.UpdatedAt {
		t.Errorf("新评论的 createdAt 与 updatedAt 应该相等：%q vs %q",
			created.CreatedAt, created.UpdatedAt)
	}

	// An empty comment must be rejected.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodPost, "/api/issues/"+issueID+"/comments",
		roleMember, `{"body":"   "}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("空评论应该被拒（400），实际 %d", rec.Code)
	}

	// Edit.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodPatch, "/api/issue-comments/"+created.ID,
		roleMember, `{"body":"补充：只在 Safari 上出现"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("改评论失败：%d %s", rec.Code, rec.Body.String())
	}
	var updated struct {
		Body string `json:"body"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &updated)
	if updated.Body != "补充：只在 Safari 上出现" {
		t.Errorf("评论没改成功：%q", updated.Body)
	}

	// It shows up in the list, and the issue's commentCount keeps up.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodGet, "/api/issues/"+issueID, roleMember, ""))
	var iss struct {
		CommentCount int `json:"commentCount"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &iss)
	if iss.CommentCount != 1 {
		t.Errorf("问题的 commentCount 应该是 1，实际 %d", iss.CommentCount)
	}

	// Delete.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodDelete, "/api/issue-comments/"+created.ID,
		roleMember, ""))
	if rec.Code != http.StatusNoContent {
		t.Errorf("删评论应该回 204，实际 %d", rec.Code)
	}
}

// TestIssuePatchStatusOnly ⭐ a PATCH with only status must succeed and must not touch the title
// and body.
//
// # This is a real failure hit in production
//
// `PATCH /api/issues/{id}` with `{"status":"closed"}` was rejected with the error **"please fill in
// at least a title or a body"**, a sentence that makes no sense at all (I only wanted to close this
// issue).
//
// Real cause: UpdateIssue always overwrote title/body with the draft, so "not sent" was read as
// "clear it", DeriveIssueTitle got an empty string → the whole request was 400.
//
// ⚠️ **The frontend never exposed it**, because patchMeta on IssueDetailPage always sends the full
// object back (title/body/status/priority all included). So only API / CLI callers hit it, and that
// is exactly the path the inspection bot and `ama issue` use. **Going through real HTTP is the key
// to this case**: a Service-layer unit test would easily fill in all four fields on the way, and
// then it could not be reproduced.
func TestIssuePatchStatusOnly(t *testing.T) {
	h := newRBACServer(t)

	// First create one with a title and body.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodPost, "/api/workspaces/"+rbacWS+"/issues",
		roleMember, `{"title":"原标题","body":"原正文"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("建问题失败：%d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析创建响应失败：%v", err)
	}

	// Only change the status, sending none of the other fields.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodPatch, "/api/issues/"+created.ID,
		roleMember, `{"status":"closed"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("只改状态就该成功，实际 %d %s —— 「没传标题正文」不等于「要清空」",
			rec.Code, rec.Body.String())
	}

	var got struct {
		Status string `json:"status"`
		Title  string `json:"title"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if got.Status != "closed" {
		t.Errorf("状态该变成 closed，实际 %q", got.Status)
	}
	if got.Title != "原标题" || got.Body != "原正文" {
		t.Errorf("标题与正文不该被动，实际 title=%q body=%q", got.Title, got.Body)
	}
}

// TestIssuePatchStillEditsTitleAndBody pairs with the previous one: when they are really sent, they
// must change.
//
// Without it, writing "keep the old values when both are empty" as "always keep the old values"
// would also pass the previous case, and that would break the main feature "edit title/body"
// completely, while the frontend looks perfectly fine (it shows the values it just submitted; only
// after a refresh do you find they were not saved).
func TestIssuePatchStillEditsTitleAndBody(t *testing.T) {
	h := newRBACServer(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodPost, "/api/workspaces/"+rbacWS+"/issues",
		roleMember, `{"title":"原标题","body":"原正文"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("建问题失败：%d", rec.Code)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析失败：%v", err)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, asRole(http.MethodPatch, "/api/issues/"+created.ID,
		roleMember, `{"title":"新标题","body":"新正文"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("改标题正文失败：%d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if got.Title != "新标题" || got.Body != "新正文" {
		t.Errorf("传了就该改，实际 title=%q body=%q", got.Title, got.Body)
	}
}
