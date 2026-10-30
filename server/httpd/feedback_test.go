// End-to-end authorization of problem feedback: real wiring (real core + real Guard + real auth)
// hitting real routes.
//
// # Why this layer cannot be skipped
//
// `core/feedback_test.go` already checks the Service's decisions, but **what really goes wrong is at
// the seams**:
//
//   - If the two declarations in the matrix are wrong (e.g. GET as ActionRead), Guard first checks
//     workspace membership, so a person who is "on the dev team but in no workspace" cannot read
//     it, and unit tests cannot see Guard;
//   - Conversely, if someday someone "tidies up" by moving the GET check into Guard, the gate in the
//     Service becomes dead code, and when the two disagree the symptom is "sometimes 403, sometimes
//     200".
//
// This file goes through the whole chain with real HTTP requests + real session tokens, so both of
// the above get caught.
//
// # The "normal user" here is an owner
//
// Picking owner instead of viewer or a non-member is deliberate: **it proves "workspace roles have
// nothing to do with this endpoint"**. An owner can do anything in their own workspace, yet still
// cannot read the feedback list, which shows this gate really is not workspace authorization but a
// separate mechanism. Testing with a viewer, both explanations "because the role is too low" and
// "because they are not on the allow list" would hold.

package httpd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/repository/dao/coremem"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/authz"
	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/feishu"
)

// devReader is the dev team allow list for tests. Production uses config.Feedback (it has a method
// with the same name).
type devReader struct{ allow string }

func (d devReader) CanRead(actor string) bool {
	return strings.EqualFold(actor, d.allow)
}

// feedbackDevEmail is the "dev team" person. **Deliberately not a member of any workspace**: feedback
// is cross-workspace, and reading it should not require a workspace identity; this also pins that.
const feedbackDevEmail = "devteam@example.com"

// newFeedbackServer wires a real service with a feedback allow list.
//
// Almost the same as newRBACServer, with one more FeedbackReaders. **It deliberately does not reuse
// that function**: the assertions there depend on "no allow list" (then the feedback endpoints are
// 403 for everyone), and putting an allow list in would quietly change its preconditions.
func newFeedbackServer(t *testing.T) http.Handler {
	t.Helper()

	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败: %v", err)
	}
	coreSvc, err := core.New(core.Config{
		Store:           store,
		FeedbackReaders: devReader{allow: feedbackDevEmail},
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

	// Seed two real sessions: an owner (owner of the seed workspace) and a dev team member.
	authStore := auth.NewMemStore()
	for _, email := range []string{roleOwner, feedbackDevEmail} {
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

// TestFeedbackReadDeniedForOrdinaryUser is the end-to-end evidence of "not open to users".
//
// ⚠️ **This is the assertion the requirement asked for by name.** It hits the real route with a real
// session token through the real Guard; in other words, the exact same request with curl also gets
// 403.
func TestFeedbackReadDeniedForOrdinaryUser(t *testing.T) {
	h := newFeedbackServer(t)

	rec := httptest.NewRecorder()
	// roleOwner is the **owner** of the seed workspace: they can do anything in their own workspace.
	h.ServeHTTP(rec, asRole("GET", "/api/feedback", roleOwner, ""))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("普通用户（即便是空间 owner）读反馈列表要 403，实际 %d，体: %s",
			rec.Code, rec.Body.String())
	}
	msg, code := envelope(t, rec)
	// code must be access_denied: the frontend relies on it to show "access denied".
	// not_found would be turned into null by nullable(), and the page would become a silent empty
	// state.
	if code != "access_denied" {
		t.Errorf("code 要是 access_denied，实际 %q", code)
	}
	if !strings.Contains(msg, "只有开发团队") {
		t.Errorf("文案要说清原因，实际 %q", msg)
	}
}

// TestFeedbackReadAllowedForDevTeam: people on the allow list can read it.
//
// Pairs with the previous one: checking only the rejection would let an implementation that
// "rejects everyone" pass, while the feature would actually be broken (the dev team could not read
// it either).
//
// It also pins that **reading feedback does not require workspace membership**: feedbackDevEmail is
// not a member of any workspace; if that matrix entry were wrongly declared ActionRead, Guard would
// block them with 403 first.
func TestFeedbackReadAllowedForDevTeam(t *testing.T) {
	h := newFeedbackServer(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("GET", "/api/feedback", feedbackDevEmail, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("开发团队成员该能读，实际 %d，体: %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Reports []map[string]any `json:"reports"`
		Total   int              `json:"total"`
		Limit   int              `json:"limit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("解响应失败: %v，体: %s", err, rec.Body.String())
	}
	// reports must be [], not null (list endpoints never output null).
	if page.Reports == nil {
		t.Error("reports 该是 []，实际是 null")
	}
	if page.Limit == 0 {
		t.Error("limit 该是收敛后的默认值，实际 0")
	}
}

// TestFeedbackSubmitAllowedForOrdinaryUser: submitting **can be done by any logged-in user**.
//
// The asymmetry between read and write permissions is deliberate: the people who hit problems are
// the ones who most need to give feedback. Restricting submission too would make the feature
// usable only by the dev team.
func TestFeedbackSubmitAllowedForOrdinaryUser(t *testing.T) {
	h := newFeedbackServer(t)

	rec := httptest.NewRecorder()
	body := `{"level":"user","routePattern":"/w/growth","errorCode":"internal","httpStatus":500}`
	h.ServeHTTP(rec, asRole("POST", "/api/feedback", roleOwner, body))

	// 204: there is nothing to return for a submission, and the record id is deliberately not
	// returned.
	if rec.Code != http.StatusNoContent {
		t.Fatalf("上报该回 204，实际 %d，体: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 不该带 body，实际: %s", rec.Body.String())
	}
}

// TestFeedbackRoundTrip: after a submission the dev team can really read it, and sensitive fields
// were not stored.
//
// This closes the whole loop: a normal user submits (with a pile of credentials) → the dev team
// reads it → the credentials are not in it. Each of the three parts has unit tests, but **only
// running them together proves nothing leaks at the seams**.
func TestFeedbackRoundTrip(t *testing.T) {
	h := newFeedbackServer(t)

	// A normal user submits, with the request body stuffed with credentials (simulating a careless
	// or malicious caller).
	rec := httptest.NewRecorder()
	body := `{
		"level": "user",
		"routePattern": "/w/growth/chat/{id}",
		"errorCode": "internal",
		"httpStatus": 500,
		"message": "加载失败，请稍后重试",
		"browser": "Chrome 120",
		"os": "macOS",
		"token": "leaked-token-abcdefghijklmnop",
		"cookie": "ama_session=leaked-cookie-value",
		"authorization": "Bearer leaked-bearer-token"
	}`
	h.ServeHTTP(rec, asRole("POST", "/api/feedback", roleOwner, body))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("上报失败: %d %s", rec.Code, rec.Body.String())
	}

	// The dev team reads.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, asRole("GET", "/api/feedback", feedbackDevEmail, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("读列表失败: %d %s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()

	// That record must be there.
	if !strings.Contains(raw, "/w/growth/chat/{id}") {
		t.Errorf("刚上报的那条应该读得到，实际响应: %s", raw)
	}
	// ⚠️ Not a single credential may be there. Check the whole response bytes instead of field by
	// field: those values could hide under any key of the payload.
	for _, secret := range []string{
		"leaked-token-abcdefghijklmnop",
		"ama_session=leaked-cookie-value",
		"leaked-bearer-token",
	} {
		if strings.Contains(raw, secret) {
			t.Errorf("敏感值出现在读取响应里: %q\n完整响应: %s", secret, raw)
		}
	}
}
