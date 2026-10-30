// End-to-end authorization of product suggestions: real wiring (real core + real Guard + real auth)
// hitting real routes.
//
// # Why this layer cannot be skipped, even less than the feedback one
//
// `core/suggestion_test.go` already checks the Service's decisions, but **what really goes wrong is
// at the seams**, and this domain's seams are wider than elsewhere: in the matrix all its routes are
// `ActionNone`, i.e. **Guard does not guard these 13 routes at all**; it checks none of them. The
// whole security boundary is the Service layer alone.
//
// That gives two ways to break, both only visible at this layer:
//
//   - **The Service misses a check on one path** (e.g. attachment download forgets to look up the
//     suggestion it belongs to). Guard does not cover for it, so anyone with the URL can see the
//     screenshots other people attached, with no error and a perfectly normal UI. The same
//     oversight on the issue side would be stopped by Guard (that group has kind + idParam); here
//     it would not.
//   - **Someone "tidies up" by adding kind/idParam to these routes** so Guard also has a say. Then
//     Guard would resolve a suggestion id in the issues table, always fail → every route 404s,
//     while the error says "resource not found", which does not point to the real cause at all.
//
// So every case here goes through the whole chain with a real HTTP request + a real session token.
//
// # Three people, none of them optional
//
//	suggestionAuthorEmail   the person who submits suggestions. **Deliberately not a member of any
//	                        workspace**: suggestions are platform-level; submitting and reading them
//	                        should not require a workspace identity.
//	suggestionStaffEmail    the platform team (on the allow list), also not a member of any workspace.
//	roleOwner               the **owner** of the seed workspace, but **not** the author of the
//	                        suggestion, and not on the allow list.
//
// The last one proves "workspace roles have nothing to do with this domain": an owner can do
// anything in their own workspace, but cannot read suggestions others submitted. Testing with a
// viewer, both explanations "because the role is too low" and "because they are not the author"
// would hold, and the case could not single out the one it means to check.

package httpd

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/authz"
	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/feishu"
	"github.com/chyroc/ama/server/repository/dao/coremem"
)

const (
	// suggestionAuthorEmail submits suggestions; not a member of any workspace and not on the
	// platform team list.
	suggestionAuthorEmail = "amy@example.com"
	// suggestionOtherEmail is another normal user, used to check "you cannot see others' suggestions".
	suggestionOtherEmail = "eve@example.com"
	// suggestionStaffEmail is the platform team, on the allow list.
	suggestionStaffEmail = "devteam@example.com"
)

// newSuggestionServer wires a real service; the platform team list only has suggestionStaffEmail.
//
// **It deliberately does not reuse newFeedbackServer**: that function seeds sessions for only two
// people, while this needs four (author / another user / platform team / workspace owner). Reusing
// it and adding people on the way would quietly change the preconditions of the cases there.
func newSuggestionServer(t *testing.T) http.Handler {
	t.Helper()

	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败: %v", err)
	}
	coreSvc, err := core.New(core.Config{
		Store: store,
		// Production setup: bytes always go to object storage; an in-memory stand-in is used here.
		BlobStore: coremem.NewMemBlobStore(),
		// The platform team reuses the feedback allow list; see core/README-suggestion.md.
		FeedbackReaders: devReader{allow: suggestionStaffEmail},
	})
	if err != nil {
		t.Fatalf("建 core.Service 失败: %v", err)
	}
	coreHandler := newCoreRoutes(t, coreSvc, nil)

	feishuStore := feishu.NewMemStore()
	membership := rbacMembership{core: coreSvc, feishu: feishuStore}
	guard, err := authz.New(authz.Config{Membership: membership})
	if err != nil {
		t.Fatalf("建 authz.Guard 失败: %v", err)
	}
	feishuSvc, err := feishu.New(feishu.Config{
		Store:      feishuStore,
		Authorizer: authz.NewFeishuAuthorizer(membership),
	})
	if err != nil {
		t.Fatalf("建 feishu.Service 失败: %v", err)
	}

	authStore := auth.NewMemStore()
	for _, email := range []string{
		roleOwner, suggestionAuthorEmail, suggestionOtherEmail, suggestionStaffEmail,
	} {
		if err := authStore.PutSession(context.Background(), auth.Session{
			TokenHash: tokenHashFor(email),
			Email:     email,
			Name:      email,
			CreatedAt: time.Now(),
			ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("种会话失败: %v", err)
		}
	}
	authSvc, err := auth.New(auth.Config{Store: authStore})
	if err != nil {
		t.Fatalf("建 auth.Service 失败: %v", err)
	}
	authHandler, err := auth.NewHandler(auth.HandlerConfig{Service: authSvc})
	if err != nil {
		t.Fatalf("建 auth.Handler 失败: %v", err)
	}

	srv, err := New(Config{
		Addr: ":0", FeishuService: feishuSvc, CoreHandler: coreHandler,
		AuthHandler: authHandler, Authz: guard,
	})
	if err != nil {
		t.Fatalf("建 Server 失败: %v", err)
	}
	return srv.Handler()
}

// postSuggestion submits a suggestion as who and returns its id.
func postSuggestion(t *testing.T, h http.Handler, who, title, body string) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"title": title, "body": body})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("POST", "/api/suggestions", who, string(payload)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("%s 提建议要 201，实际 %d，体: %s", who, rec.Code, rec.Body.String())
	}
	var sg struct {
		ID     string `json:"id"`
		Author string `json:"author"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sg); err != nil {
		t.Fatalf("解响应失败: %v", err)
	}
	if sg.ID == "" {
		t.Fatalf("建议 id 是空的，体: %s", rec.Body.String())
	}
	return sg.ID
}

// TestSuggestionCanBeSubmittedByNonMember pins "people who belong to no workspace can still submit
// suggestions".
//
// ⚠️ This is not a formality. If someday someone moves these routes under a workspace
// (`/api/workspaces/{id}/...`) or gives them a level other than ActionNone, this scenario breaks
// first, **and those are exactly the people with the most to say** (a newcomer who cannot open a
// page and wants to give feedback).
func TestSuggestionCanBeSubmittedByNonMember(t *testing.T) {
	h := newSuggestionServer(t)

	// suggestionAuthorEmail is not a member of any workspace.
	id := postSuggestion(t, h, suggestionAuthorEmail, "希望支持深色模式", "晚上用眼睛疼")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("GET", "/api/suggestions/"+id, suggestionAuthorEmail, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("作者读自己的建议要 200，实际 %d，体: %s", rec.Code, rec.Body.String())
	}
	var sg struct {
		Author string `json:"author"`
		Status string `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sg)
	// The author comes from the login identity, not the request body.
	if sg.Author != suggestionAuthorEmail {
		t.Errorf("作者 = %q，想要 %q（必须取自登录身份）", sg.Author, suggestionAuthorEmail)
	}
	if sg.Status != "open" {
		t.Errorf("新建的状态 = %q，想要 open", sg.Status)
	}
}

// TestSuggestionAuthorCannotBeForged pins that author in the request body is not accepted.
//
// Accepting it would allow submitting suggestions under someone else's name, and suggestions are
// used to drive platform improvements; a wrong signature is worse than none.
func TestSuggestionAuthorCannotBeForged(t *testing.T) {
	h := newSuggestionServer(t)

	payload := `{"title":"冒名的建议","body":"x","author":"someone-else@example.com"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("POST", "/api/suggestions", suggestionAuthorEmail, payload))
	if rec.Code != http.StatusCreated {
		t.Fatalf("提建议要 201，实际 %d，体: %s", rec.Code, rec.Body.String())
	}
	var sg struct {
		Author string `json:"author"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sg)
	if sg.Author != suggestionAuthorEmail {
		t.Errorf("作者被请求体改成了 %q —— 那个字段必须被忽略", sg.Author)
	}
}

// TestSuggestionIsPrivateToAuthor is the most important case in this file: **others cannot read
// your suggestions**.
//
// It hits real routes with real session tokens. ⚠️ "Others" here includes **the workspace owner**,
// which proves "workspace roles have nothing to do with this domain".
func TestSuggestionIsPrivateToAuthor(t *testing.T) {
	h := newSuggestionServer(t)
	id := postSuggestion(t, h, suggestionAuthorEmail, "只有我能看", "私密内容")

	for _, who := range []string{suggestionOtherEmail, roleOwner} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole("GET", "/api/suggestions/"+id, who, ""))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s 读别人的建议要 403，实际 %d，体: %s",
				who, rec.Code, rec.Body.String())
		}
		_, code := envelope(t, rec)
		// code must be access_denied: the frontend relies on it to show "access denied".
		// not_found would be turned into null by nullable(), and the page would become a silent
		// empty state.
		if code != "access_denied" {
			t.Errorf("%s 拿到的 code = %q，想要 access_denied", who, code)
		}
	}
}

// TestSuggestionListIsScopedToAuthor pins that the list only returns your own.
//
// ⚠️ It also pins that the `?author=` parameter **has no effect for normal users**: that is how
// authorization works on this path, and accepting it would let anyone read anyone's suggestions.
func TestSuggestionListIsScopedToAuthor(t *testing.T) {
	h := newSuggestionServer(t)

	postSuggestion(t, h, suggestionAuthorEmail, "amy 的建议", "x")
	postSuggestion(t, h, suggestionOtherEmail, "bob 的建议一", "x")
	postSuggestion(t, h, suggestionOtherEmail, "bob 的建议二", "x")

	// amy only sees her own one.
	page := listSuggestions(t, h, suggestionAuthorEmail, "")
	if len(page.Suggestions) != 1 {
		t.Fatalf("amy 看到 %d 条，想要 1 条（列表必须按作者收窄）", len(page.Suggestions))
	}
	if page.Suggestions[0].Author != suggestionAuthorEmail {
		t.Errorf("amy 看到了 %s 的建议", page.Suggestions[0].Author)
	}
	if page.ViewerIsStaff {
		t.Error("amy 不是平台方，viewerIsStaff 该是 false")
	}

	// ⚠️ Adding ?author=bob does not help: the server always overwrites it with the caller.
	forged := listSuggestions(t, h, suggestionAuthorEmail, "author="+suggestionOtherEmail)
	if len(forged.Suggestions) != 1 || forged.Suggestions[0].Author != suggestionAuthorEmail {
		t.Errorf("amy 用 ?author=bob 读到了 %d 条别人的建议 —— "+
			"那个参数对普通用户必须无效", len(forged.Suggestions))
	}
}

// TestSuggestionStaffSeesEverything pairs with the previous case.
//
// Checking only "cannot see others'" would let an implementation where "nobody sees anything" pass,
// while the feature would actually be broken (the platform team could not read them either, and the
// whole requirement would be pointless).
func TestSuggestionStaffSeesEverything(t *testing.T) {
	h := newSuggestionServer(t)

	id := postSuggestion(t, h, suggestionAuthorEmail, "amy 的建议", "x")
	postSuggestion(t, h, suggestionOtherEmail, "bob 的建议", "x")

	page := listSuggestions(t, h, suggestionStaffEmail, "")
	if len(page.Suggestions) != 2 {
		t.Fatalf("平台方看到 %d 条，想要 2 条（全部）", len(page.Suggestions))
	}
	if !page.ViewerIsStaff {
		t.Error("平台方的 viewerIsStaff 该是 true —— 前端靠它决定渲染哪一套界面")
	}

	// It can read a single one too.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("GET", "/api/suggestions/"+id, suggestionStaffEmail, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("平台方读任意建议要 200，实际 %d", rec.Code)
	}

	// Filtering by person works for the platform team (the opposite of normal users).
	filtered := listSuggestions(t, h, suggestionStaffEmail, "author="+suggestionOtherEmail)
	if len(filtered.Suggestions) != 1 {
		t.Errorf("平台方按 author 筛拿到 %d 条，想要 1 条", len(filtered.Suggestions))
	}
}

// TestSuggestionStatusOnlyStaffCanChange pins "the author cannot change the status".
//
// The status says **how far the platform team has handled it**; letting the submitter click
// "resolved" themselves would make the field meaningless: looking at the list, the platform team
// could not tell "we resolved it" from "they gave up".
func TestSuggestionStatusOnlyStaffCanChange(t *testing.T) {
	h := newSuggestionServer(t)
	id := postSuggestion(t, h, suggestionAuthorEmail, "建议", "正文")

	// The author tries to mark their own as resolved: 403.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("PATCH", "/api/suggestions/"+id,
		suggestionAuthorEmail, `{"status":"resolved"}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("作者改状态要 403，实际 %d，体: %s", rec.Code, rec.Body.String())
	}

	// But editing the title and body is allowed.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("PATCH", "/api/suggestions/"+id,
		suggestionAuthorEmail, `{"title":"改过的标题","body":"改过的正文"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("作者改标题正文要 200，实际 %d，体: %s", rec.Code, rec.Body.String())
	}

	// The platform team changes the status: 200.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("PATCH", "/api/suggestions/"+id,
		suggestionStaffEmail, `{"status":"resolved"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("平台方改状态要 200，实际 %d，体: %s", rec.Code, rec.Body.String())
	}
	var sg struct {
		Status string `json:"status"`
		Title  string `json:"title"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sg)
	if sg.Status != "resolved" {
		t.Errorf("状态 = %q，想要 resolved", sg.Status)
	}
	// ⚠️ A PATCH with only status **must not clear the title and body**: that is the heuristic in
	// UpdateSuggestion "both empty = leave them alone this time" at work. Without it this would
	// return 400.
	if sg.Title != "改过的标题" {
		t.Errorf("只改状态却把标题冲掉了: %q", sg.Title)
	}
}

// TestSuggestionUnknownStatusIsRejected pins "anything unrecognized is rejected, not defaulted to
// open".
//
// The cost of silently correcting it: the caller misspells a status name, the endpoint returns 200,
// and the suggestion's status never becomes what they wanted. They think it worked, while the
// platform team still sees "unresolved".
func TestSuggestionUnknownStatusIsRejected(t *testing.T) {
	h := newSuggestionServer(t)
	id := postSuggestion(t, h, suggestionAuthorEmail, "建议", "正文")

	for _, bad := range []string{"resolve", "Resolved", "closed", "done"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole("PATCH", "/api/suggestions/"+id,
			suggestionStaffEmail, `{"status":"`+bad+`"}`))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("状态 %q 要被拒（400），实际 %d，体: %s",
				bad, rec.Code, rec.Body.String())
		}
	}

	// The suggestion's status was not touched.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("GET", "/api/suggestions/"+id, suggestionStaffEmail, ""))
	var sg struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sg)
	if sg.Status != "open" {
		t.Errorf("被拒之后状态变成了 %q，想要还是 open", sg.Status)
	}
}

// TestSuggestionRepliesAreTwoWay pins the "replies are supported" requirement itself: the platform
// team replies, the user follows up, both get in, and isStaff tells them apart.
func TestSuggestionRepliesAreTwoWay(t *testing.T) {
	h := newSuggestionServer(t)
	id := postSuggestion(t, h, suggestionAuthorEmail, "建议", "正文")

	// The platform team replies once.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("POST", "/api/suggestions/"+id+"/replies",
		suggestionStaffEmail, `{"body":"收到，下个版本做"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("平台方回复要 201，实际 %d，体: %s", rec.Code, rec.Body.String())
	}

	// The author follows up once.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("POST", "/api/suggestions/"+id+"/replies",
		suggestionAuthorEmail, `{"body":"大概什么时候？"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("作者追问要 201，实际 %d，体: %s", rec.Code, rec.Body.String())
	}

	// An unrelated person cannot reply.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("POST", "/api/suggestions/"+id+"/replies",
		suggestionOtherEmail, `{"body":"我也来说两句"}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("不相干的人回复要 403，实际 %d", rec.Code)
	}

	// List them: in order, two of them, isStaff tells them apart.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("GET", "/api/suggestions/"+id+"/replies", suggestionAuthorEmail, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("列回复要 200，实际 %d", rec.Code)
	}
	var replies []struct {
		Body    string `json:"body"`
		Author  string `json:"author"`
		IsStaff bool   `json:"isStaff"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &replies); err != nil {
		t.Fatalf("解回复失败: %v，体: %s", err, rec.Body.String())
	}
	if len(replies) != 2 {
		t.Fatalf("列出 %d 条回复，想要 2 条", len(replies))
	}
	// In order: the platform team's one first.
	if !replies[0].IsStaff {
		t.Error("第一条该是平台方的（isStaff=true），回复必须正序")
	}
	if replies[1].IsStaff {
		t.Error("第二条是作者写的，isStaff 该是 false")
	}
	// ⚠️ The author of the platform team's reply is **deliberately not asserted here**: it is empty
	// for normal users (the server wipes it on purpose; see
	// TestSuggestionStaffIdentityIsHiddenFromUsers). This case asks "both kinds of people can reply,
	// and isStaff tells them apart"; the author identity belongs to that other case.
	//
	// The user's own reply must have an author: that is the minimum evidence that "not everything is
	// made anonymous".
	if replies[1].Author != suggestionAuthorEmail {
		t.Errorf("用户自己那条的作者 = %q，不该被抹掉", replies[1].Author)
	}
}

// TestSuggestionStaffIdentityIsHiddenFromUsers pins that **platform team replies do not show an
// email**.
//
// Platform team replies speak **in the name of the platform**. Showing the email has two
// consequences: it reveals who builds the platform (organization information, which also changes
// when people move), and it ties promises to individuals (next time the user messages that person
// directly to chase progress, going around this channel).
//
// ⚠️ **It must be wiped on the server, not just hidden by the frontend.** The value would still be
// in the JSON, visible with F12 and fully collected by one script run; same as the README-feedback.md
// rule "'not putting an entry in the UI' is not access control". So this case asserts on **the bytes
// of the response body**.
//
// Three directions are pinned together; missing any one lets the implementation drift the wrong way:
//
//	a normal user viewing the platform team's reply  →  author is an empty string
//	a normal user viewing their own reply            →  author is still there (making it anonymous
//	                                                    would make the conversation look like talking
//	                                                    to oneself)
//	the platform team viewing their own reply        →  author is still there (they need to know who
//	                                                    followed up)
func TestSuggestionStaffIdentityIsHiddenFromUsers(t *testing.T) {
	h := newSuggestionServer(t)
	id := postSuggestion(t, h, suggestionAuthorEmail, "建议", "正文")

	// The platform team replies once, the author follows up once.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("POST", "/api/suggestions/"+id+"/replies",
		suggestionStaffEmail, `{"body":"收到，下个版本做"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("平台方回复要 201，实际 %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("POST", "/api/suggestions/"+id+"/replies",
		suggestionAuthorEmail, `{"body":"好的"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("作者追问要 201，实际 %d", rec.Code)
	}

	// -- 1. Normal user's view --
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("GET", "/api/suggestions/"+id+"/replies",
		suggestionAuthorEmail, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("列回复要 200，实际 %d", rec.Code)
	}

	// ⚠️ **First assert that the email is not in the raw bytes at all**, then decode the JSON and
	// look at fields. Decoding fields only would let an implementation that "moves the email into
	// another key" pass.
	if strings.Contains(rec.Body.String(), suggestionStaffEmail) {
		t.Errorf("响应体里出现了平台方的邮箱 %q —— "+
			"那暴露了平台由谁开发，且会让用户绕过这条链路去私聊他。\n体: %s",
			suggestionStaffEmail, rec.Body.String())
	}

	var replies []struct {
		Author  string `json:"author"`
		IsStaff bool   `json:"isStaff"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &replies); err != nil {
		t.Fatalf("解回复失败: %v", err)
	}
	if len(replies) != 2 {
		t.Fatalf("列出 %d 条回复，想要 2 条", len(replies))
	}
	if !replies[0].IsStaff {
		t.Fatal("第一条该是平台方的")
	}
	if replies[0].Author != "" {
		t.Errorf("平台方那条的 author = %q，想要空串", replies[0].Author)
	}
	// The user's own reply **keeps its author**: wiping it would make the conversation look like two
	// anonymous people talking to themselves.
	if replies[1].Author != suggestionAuthorEmail {
		t.Errorf("用户自己那条的 author = %q，不该被抹掉", replies[1].Author)
	}

	// -- 2. Platform team's view: the real name is visible --
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("GET", "/api/suggestions/"+id+"/replies",
		suggestionStaffEmail, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("平台方列回复要 200，实际 %d", rec.Code)
	}
	var asStaff []struct {
		Author  string `json:"author"`
		IsStaff bool   `json:"isStaff"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &asStaff)
	if len(asStaff) != 2 || asStaff[0].Author != suggestionStaffEmail {
		t.Errorf("平台方该看得到是谁回的（内部协作要用），实际 author = %q",
			asStaff[0].Author)
	}
}

// TestSuggestionReplyStaffFlagCannotBeForged pins that isStaff in the request body is not accepted.
//
// Accepting it would let anyone mark their own message as an official answer.
func TestSuggestionReplyStaffFlagCannotBeForged(t *testing.T) {
	h := newSuggestionServer(t)
	id := postSuggestion(t, h, suggestionAuthorEmail, "建议", "正文")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("POST", "/api/suggestions/"+id+"/replies",
		suggestionAuthorEmail, `{"body":"我是官方","isStaff":true,"author":"fake@example.com"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("回复要 201，实际 %d，体: %s", rec.Code, rec.Body.String())
	}
	var reply struct {
		Author  string `json:"author"`
		IsStaff bool   `json:"isStaff"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &reply)
	if reply.IsStaff {
		t.Error("请求体里的 isStaff 被采纳了 —— 任何人都能把自己标成官方答复")
	}
	if reply.Author != suggestionAuthorEmail {
		t.Errorf("作者被请求体改成了 %q", reply.Author)
	}
}

// TestSuggestionAttachmentIsPrivateToAuthor is **the easiest one to miss in this domain**.
//
// ⚠️ Attachment URLs go into `<img src>`, show up in shared links, browser history and logs. On the
// issue side authz blocks cross-workspace access by resolving the workspace; **here Guard does not
// guard at all** (ActionNone in the matrix), and the only door is the Service looking up the
// suggestion it belongs to.
//
// If that is missed, anyone with the URL can see screenshots others attached, and the leak **gives
// no error, and no UI will ever show that it happened**.
//
// The case **first asserts that the author can get it**, then checks others get 403: without the
// former, a 403 might only mean the attachment was never uploaded.
func TestSuggestionAttachmentIsPrivateToAuthor(t *testing.T) {
	h := newSuggestionServer(t)
	id := postSuggestion(t, h, suggestionAuthorEmail, "带图的建议", "见附件")

	// A minimal valid PNG (8-byte magic number + a bit of content) that passes server-side sniffing.
	png := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("x", 64))
	attID := uploadSuggestionAttachment(t, h, id, suggestionAuthorEmail, "截图.png", png)

	// 1. The author can get it, and gets the real bytes.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("GET", "/api/suggestion-attachments/"+attID,
		suggestionAuthorEmail, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("作者取自己的附件要 200，实际 %d，体: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), png) {
		t.Error("取到的字节与上传的不一致")
	}

	// 2. Others (including the workspace owner) get 403.
	for _, who := range []string{suggestionOtherEmail, roleOwner} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, asRole("GET", "/api/suggestion-attachments/"+attID, who, ""))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s 取别人建议的附件要 403，实际 %d（拿到 %d 字节）——"+
				"这条泄漏零报错，界面上完全看不出来",
				who, rec.Code, rec.Body.Len())
		}
	}

	// 3. The platform team can get it (they need to see the screenshot to handle it).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("GET", "/api/suggestion-attachments/"+attID,
		suggestionStaffEmail, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("平台方取附件要 200，实际 %d", rec.Code)
	}
}

// TestSuggestionAttachmentSecurityHeaders pins three security response headers.
//
// Their division of labor is written at the top of shared/gopkg/attachment/attachment.go. **Deleting
// any one gives no error**, the attack surface just comes back, so each of the three has its own
// assertion.
func TestSuggestionAttachmentSecurityHeaders(t *testing.T) {
	h := newSuggestionServer(t)
	id := postSuggestion(t, h, suggestionAuthorEmail, "带图的建议", "见附件")

	png := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("x", 64))
	attID := uploadSuggestionAttachment(t, h, id, suggestionAuthorEmail, "截图.png", png)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("GET", "/api/suggestion-attachments/"+attID,
		suggestionAuthorEmail, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("取附件要 200，实际 %d", rec.Code)
	}

	// 1. Content-Type is the real sniffed type, not a hard-coded json.
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q，想要 image/png", ct)
	}
	// 2. nosniff: forbid the browser from ignoring the type we set and guessing on its own.
	if v := rec.Header().Get("X-Content-Type-Options"); v != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q，想要 nosniff", v)
	}
	// 3. Images are inline (otherwise <img> cannot show them), and a Chinese file name must be
	// encoded per RFC 5987.
	cd := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "inline;") {
		t.Errorf("图片的 Content-Disposition 该是 inline，实际 %q", cd)
	}
	if !strings.Contains(cd, "filename*=UTF-8''") {
		t.Errorf("中文文件名要带 RFC 5987 编码，实际 %q", cd)
	}
}

// TestSuggestionAttachmentRejectsSVG pins that svg is not accepted.
//
// It can embed <script> and smuggle HTML in with <foreignObject>, and an image/svg+xml response is
// rendered as a vector image even with nosniff, with scripts running, in our own origin.
func TestSuggestionAttachmentRejectsSVG(t *testing.T) {
	h := newSuggestionServer(t)
	id := postSuggestion(t, h, suggestionAuthorEmail, "建议", "正文")

	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	rec := uploadSuggestionAttachmentRaw(t, h, id, suggestionAuthorEmail, "evil.svg", svg)
	if rec.Code == http.StatusCreated {
		t.Fatal("svg 被收下了 —— 它能内嵌脚本，跑在我们的同源里")
	}
}

// -- Helpers ------------------------------------------------------------------

type suggestionPageResp struct {
	Suggestions []struct {
		ID     string `json:"id"`
		Author string `json:"author"`
		Status string `json:"status"`
	} `json:"suggestions"`
	Limit         int  `json:"limit"`
	Offset        int  `json:"offset"`
	ViewerIsStaff bool `json:"viewerIsStaff"`
}

func listSuggestions(t *testing.T, h http.Handler, who, query string) suggestionPageResp {
	t.Helper()
	path := "/api/suggestions"
	if query != "" {
		path += "?" + query
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("GET", path, who, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s 列建议要 200，实际 %d，体: %s", who, rec.Code, rec.Body.String())
	}
	var page suggestionPageResp
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("解列表失败: %v，体: %s", err, rec.Body.String())
	}
	// List endpoints never output null.
	if page.Suggestions == nil {
		t.Error("suggestions 该是 []，实际是 null")
	}
	return page
}

// uploadSuggestionAttachmentRaw uploads an attachment and returns the raw response (no success
// assertion).
func uploadSuggestionAttachmentRaw(
	t *testing.T, h http.Handler, sugID, who, filename string, content []byte,
) *httptest.ResponseRecorder {
	t.Helper()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("造 multipart 失败: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("写 multipart 失败: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("关 multipart 失败: %v", err)
	}

	// ⚠️ asRole cannot be used: it only builds JSON requests (Content-Type hard-coded to
	// application/json), while this needs a multipart boundary. The token shape matches it.
	req := httptest.NewRequest("POST", "/api/suggestions/"+sugID+"/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+tokenTFor(who))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// uploadSuggestionAttachment uploads an attachment, asserts success, and returns the attachment id.
func uploadSuggestionAttachment(
	t *testing.T, h http.Handler, sugID, who, filename string, content []byte,
) string {
	t.Helper()
	rec := uploadSuggestionAttachmentRaw(t, h, sugID, who, filename, content)
	if rec.Code != http.StatusCreated {
		t.Fatalf("传附件要 201，实际 %d，体: %s", rec.Code, rec.Body.String())
	}
	var att struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &att); err != nil {
		t.Fatalf("解附件响应失败: %v", err)
	}
	if att.ID == "" {
		t.Fatalf("附件 id 是空的，体: %s", rec.Body.String())
	}
	// The URL is built by the server; the frontend puts it straight into <img src>.
	if att.URL != "/api/suggestion-attachments/"+att.ID {
		t.Errorf("附件 URL = %q，形状不对", att.URL)
	}
	return att.ID
}
