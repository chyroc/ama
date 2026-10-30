// Tests for problem feedback.
//
// # The four things this file guards
//
// Each one is the "breaks without an error" kind, so each was mutation-checked (comment out the
// matching implementation and confirm the test really turns red). Security and rate limiting
// without a mutation check count as not verified.
//
//  1. **Ordinary users cannot read the feedback list** (the proof of "not open to users");
//  2. **Sensitive fields are not stored** (allowlist filtering works, rather than a denylist
//     deleting a few keys);
//  3. **Level-one automatic records are rate limited** (the same error N times does not create
//     N rows);
//  4. **Level-one and level-two collect different field sets** (level one has no ids and no trail).

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/chyroc/ama/server/repository/dao/coremem"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// -- Test helpers ------------------------------------------------------------

// isAccessDenied reports whether the error is ErrAccessDenied.
//
// It is split out because **this check is itself a contract**: the handler maps it to 403 +
// code=access_denied, and the frontend uses that code to show "no access" instead of turning the
// response into null.
func isAccessDenied(err error) bool { return errors.Is(err, ErrAccessDenied) }

// decodeJSONForTest goes through the real JSON decoding path.
//
// ⚠️ **The allowlist test must use it** and cannot build a FeedbackInput directly: what we verify
// is exactly "the struct has no token-like fields", and when building the struct you cannot even
// write that line (it does not compile), so the test would turn into "testing something that
// cannot happen".
func decodeJSONForTest(raw string, out any) error {
	return json.Unmarshal([]byte(raw), out)
}

// mustMarshalForTest serializes a record to a string, for the overall "does any sensitive value
// appear" check.
//
// We check the serialized bytes instead of asserting field by field: a sensitive value may hide
// in any key of the payload, and per-field assertions only cover "the fields I thought of" —
// missing one means it was not tested.
func mustMarshalForTest(t *testing.T, v any) string {
	t.Helper()
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return string(blob)
}

// utf8ValidForTest reports whether the string is valid UTF-8.
func utf8ValidForTest(s string) bool { return utf8.ValidString(s) }

// feedbackTestReaders is the allowlist implementation used in tests.
//
// It deliberately does not use config.Feedback: core does not import shared/infra/config (once
// that dependency exists, every core test has to build a Config). A three-line hand-written
// implementation is enough here; the real wiring is cmd/amad passing cfg.Feedback in.
type feedbackTestReaders struct {
	allow []string
}

func (r feedbackTestReaders) CanRead(actor string) bool {
	for _, a := range r.allow {
		if strings.EqualFold(a, actor) {
			return true
		}
	}
	return false
}

// newFeedbackService builds a Service with an allowlist. readers == nil means not wired.
func newFeedbackService(t *testing.T, readers FeedbackReaders) *Service {
	t.Helper()
	svc, err := New(Config{Store: coremem.NewMemStore(), FeedbackReaders: readers})
	if err != nil {
		t.Fatalf("建 Service 失败: %v", err)
	}
	return svc
}

// asActor builds a ctx with an identity. It uses auth.WithIdentity instead of setting a key by hand:
// that is the entry the auth middleware really uses, and a test that bypasses it cannot prove
// production is right too.
func asActor(email string) context.Context {
	return auth.WithIdentity(context.Background(), auth.Identity{Email: email})
}

// ---------------------------------------------------------------------------
// 1. Read permission: ordinary users always get 403
// ---------------------------------------------------------------------------

// TestListFeedbackDeniesOrdinaryUser is the core evidence for "not open to users".
//
// ⚠️ **This cannot rely only on "no entry point in the UI"** — that is not access control; anyone
// can hit it with curl. So this calls the Service directly, simulating a request that bypasses the
// UI.
func TestListFeedbackDeniesOrdinaryUser(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})

	// An ordinary user (not on the allowlist). **Note they can even be the owner of some
	// workspace** — workspace roles have nothing to do with this endpoint, which is the point of
	// the design: feedback spans workspaces.
	_, err := svc.ListFeedback(asActor("someone@example.com"), persistmodel.FeedbackQuery{})
	if err == nil {
		t.Fatal("普通用户竟然读到了反馈列表——「只给开发团队」这条边界没生效")
	}
	if !strings.Contains(err.Error(), "只有开发团队") {
		t.Errorf("错误文案要说清原因，实际是: %v", err)
	}
	// Must be ErrAccessDenied: the handler maps it to 403 + code=access_denied.
	// Another sentinel would make the frontend treat it as a different problem (for example
	// not_found gets turned into null, and the page becomes a silent empty state).
	if !isAccessDenied(err) {
		t.Errorf("要回 ErrAccessDenied（→403），实际是: %v", err)
	}
}

// TestListFeedbackAllowsWhitelistedActor: people on the allowlist can read.
//
// Paired with the previous one: testing only "deny" would let a "deny everyone" implementation
// pass, and then the feature would actually be broken (the dev team could not read either) while
// all tests are green.
func TestListFeedbackAllowsWhitelistedActor(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})

	page, err := svc.ListFeedback(asActor("dev@example.com"), persistmodel.FeedbackQuery{})
	if err != nil {
		t.Fatalf("白名单里的人应该能读，却失败了: %v", err)
	}
	// List endpoints never return null (see the core/handler.go file header).
	if page.Reports == nil {
		t.Error("Reports 必须是非 nil 的空切片，不能是 nil（前端会直接遍历它）")
	}
}

// TestListFeedbackWhitelistIsCaseInsensitive: email comparison ignores case.
//
// The allowlist is **typed into config by hand** and may be written as Dev@Example.com, while the
// actor was already lowercased at the auth entry. A mismatch shows up as "I am clearly on the list
// but cannot read", with no error at all.
func TestListFeedbackWhitelistIsCaseInsensitive(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"Dev@Example.com"}})
	if _, err := svc.ListFeedback(asActor("dev@example.com"), persistmodel.FeedbackQuery{}); err != nil {
		t.Errorf("大小写不同的同一个人应该能读: %v", err)
	}
}

// TestListFeedbackDeniesWhenReadersUnconfigured: with no allowlist wired, **nobody can read**.
//
// ⚠️ This pins fail-closed. With a let-through fallback, "forgot to wire it" would mean this gate
// does not exist at all, with no error — exactly why this repo deleted AllowAllAuthorizer.
func TestListFeedbackDeniesWhenReadersUnconfigured(t *testing.T) {
	svc := newFeedbackService(t, nil)
	if _, err := svc.ListFeedback(asActor("anyone@example.com"), persistmodel.FeedbackQuery{}); err == nil {
		t.Fatal("没装配白名单时必须拒绝所有人（fail-closed），实际放行了")
	}
}

// TestSubmitFeedbackAllowsAnyLoggedInUser: submitting is **not** limited by the allowlist.
//
// Read and write permissions are deliberately asymmetric: the person who hit the problem is the
// one who most needs to be able to report it. Restricting submission too would mean only the dev
// team could use this feature, which would make it pointless.
func TestSubmitFeedbackAllowsAnyLoggedInUser(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})
	err := svc.SubmitFeedback(asActor("someone@example.com"), FeedbackInput{
		Level: "user", RoutePattern: "/w/x", ErrorCode: "internal",
	})
	if err != nil {
		t.Fatalf("任何登录用户都该能上报，却失败了: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 2. Sensitive fields are not stored
// ---------------------------------------------------------------------------

// TestFeedbackNeverStoresCredentialFields pins the allowlist filtering.
//
// # Why this uses "raw JSON" instead of building a FeedbackInput
//
// Because what we verify is exactly **the struct has no such fields**. When building a
// FeedbackInput you cannot even write the `token:` line (it does not compile), so the test would
// turn into "testing something that cannot happen". JSON decoding is the real path: whatever the
// frontend (or any caller) sends is a piece of JSON.
func TestFeedbackNeverStoresCredentialFields(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})

	// A "malicious or careless" caller: stuffs a pile of credentials into the request body.
	raw := `{
		"level": "user",
		"routePattern": "/w/x/chat",
		"errorCode": "internal",
		"token": "super-secret-token-value-12345",
		"cookie": "ama_session=abcdef1234567890",
		"authorization": "Bearer aaaaaaaaaaaaaaaaaaaa",
		"apiKey": "amak_deadbeefcafe1234",
		"password": "hunter2hunter2",
		"responseBody": {"secret": "leaked-response-body"},
		"userMessage": "我的密码是 hunter2"
	}`
	var in FeedbackInput
	if err := decodeJSONForTest(raw, &in); err != nil {
		t.Fatalf("解请求体失败: %v", err)
	}
	if err := svc.SubmitFeedback(asActor("someone@example.com"), in); err != nil {
		t.Fatalf("上报失败: %v", err)
	}

	page, err := svc.ListFeedback(asActor("dev@example.com"), persistmodel.FeedbackQuery{})
	if err != nil {
		t.Fatalf("读列表失败: %v", err)
	}
	if len(page.Reports) != 1 {
		t.Fatalf("要有 1 条记录，实际 %d 条", len(page.Reports))
	}

	// Serialize the whole record and look for each sensitive value.
	//
	// We check the serialized bytes instead of asserting field by field: those values may hide in
	// any key of the payload, and per-field assertions only cover "the fields I thought of" —
	// missing one means it was not tested.
	blob := mustMarshalForTest(t, page.Reports[0])
	for _, secret := range []string{
		"super-secret-token-value-12345",
		"ama_session=abcdef1234567890",
		"aaaaaaaaaaaaaaaaaaaa",
		"amak_deadbeefcafe1234",
		"hunter2hunter2",
		"leaked-response-body",
		"我的密码是 hunter2",
	} {
		if strings.Contains(blob, secret) {
			t.Errorf("敏感值落库了: %q\n完整记录: %s", secret, blob)
		}
	}
}

// TestFeedbackScrubsEmbeddedCredentials pins the defense-in-depth layer: credentials **embedded in
// values**.
//
// The allowlist blocks "sending an extra token field", but not "pasting a JWT into message" —
// message is a field we actively collect. So we also scrub by shape.
func TestFeedbackScrubsEmbeddedCredentials(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})

	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	cases := []struct {
		name   string
		input  string
		secret string
	}{
		{"JWT", "调用失败，token=" + jwt, jwt},
		{"API Key", "用 amak_abcd1234efgh5678 调用失败", "amak_abcd1234efgh5678"},
		{"URL 里的 token", "GET /api/x?token=s3cr3tvalue123 失败", "s3cr3tvalue123"},
		{"Bearer 头", "Authorization: Bearer abcdefgh12345678 被拒", "abcdefgh12345678"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := svc.SubmitFeedback(asActor("u@example.com"), FeedbackInput{
				Level: "user", RoutePattern: "/w/x", Message: tc.input,
				// Each uses a different errorCode so they are not merged by dedup (user level is not
				// deduped anyway, but keeping cases independent of each other is safer).
				ErrorCode: tc.name,
			}); err != nil {
				t.Fatalf("上报失败: %v", err)
			}
			page, err := svc.ListFeedback(asActor("dev@example.com"), persistmodel.FeedbackQuery{ErrorCode: tc.name})
			if err != nil {
				t.Fatalf("读列表失败: %v", err)
			}
			if len(page.Reports) != 1 {
				t.Fatalf("要有 1 条，实际 %d", len(page.Reports))
			}
			blob := mustMarshalForTest(t, page.Reports[0])
			if strings.Contains(blob, tc.secret) {
				t.Errorf("嵌在文本里的凭据没被抹掉: %q\n记录: %s", tc.secret, blob)
			}
			if !strings.Contains(blob, credentialRedacted) {
				t.Errorf("应该留下抹除痕迹 %q，实际记录: %s", credentialRedacted, blob)
			}
		})
	}
}

// ⭐ **apiRoute must not become a smuggling channel for "the full path".**
//
// This field is collected at both levels, so it must meet the level-one boundary: **it must not
// point to one specific chat**. The frontend already strips ids and cuts the query, but this
// endpoint is public and the caller is not necessarily our frontend — the backend must strip it
// again itself (same rule as the existing route_pattern one).
//
// Missing it breaks two things at once, neither with an error:
//
//	One, level one leaks "which chat this person was just in", and level one has no user consent;
//	Two, aggregation breaks (each record has a different apiRoute, so "which endpoint fails the most" cannot be answered).
//
// Mutation check: replace stripRouteIDs(stripQuery(...)) in sanitizeAuto with a bare
// truncate(in.APIRoute, ...), and this test turns red.
func TestFeedbackAPIRouteIsSanitized(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})

	cases := []struct {
		name   string
		in     string
		want   string
		absent []string
	}{
		{
			name:   "真实 id 段要抹成 {id}",
			in:     "/api/workspaces/ws_growth/agents/agent_abc123/runs",
			want:   "/api/workspaces/{id}/agents/{id}/runs",
			absent: []string{"ws_growth", "agent_abc123"},
		},
		{
			// A real session id is sesn-<14-digit timestamp>-<random>, with no underscore at all.
			// This shape once slipped past route_pattern stripping (see the last section of README-feedback.md).
			name:   "连字符式会话 id 也要抹掉",
			in:     "/api/runs/sesn-20260101000003-ex003/events",
			want:   "/api/runs/{id}/events",
			absent: []string{"sesn-20260101000003-ex003", "20260101000003"},
		},
		{
			// Identity really did get into the query in the past (?actor=).
			name: "query 整段砍掉",
			in:   "/api/runs?actor=someone%40example.com&token=amak_live_key",
			want: "/api/runs",
			// ⚠️ We cannot assert that bare "actor" / "token" do not appear: **the record itself has
			// an actor column** (holding the reporter's email), so the serialized record always
			// contains that word. Asserting on such a generic word would make the test turn red
			// because of an unrelated field, and whoever fixes it would likely delete the assertion —
			// letting the real thing we want to block through as well. So we only assert the few
			// **specific values** in the query.
			absent: []string{"actor=", "someone%40", "amak_live_key", "token="},
		},
		{
			// Reverse check: route literals and workspace keys must not be stripped by mistake, or this field becomes useless.
			name:   "路由字面量不能被误杀",
			in:     "/api/skills/skill-hub",
			want:   "/api/skills/skill-hub",
			absent: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := svc.SubmitFeedback(asActor("u@example.com"), FeedbackInput{
				Level: "auto", RoutePattern: "/w/{id}/chat",
				ErrorCode: tc.name, APIRoute: tc.in, APIMethod: "GET",
			}); err != nil {
				t.Fatalf("上报失败: %v", err)
			}
			page, err := svc.ListFeedback(asActor("dev@example.com"),
				persistmodel.FeedbackQuery{ErrorCode: tc.name})
			if err != nil {
				t.Fatalf("读列表失败: %v", err)
			}
			if len(page.Reports) != 1 {
				t.Fatalf("要有 1 条，实际 %d", len(page.Reports))
			}
			got, _ := page.Reports[0].Payload["apiRoute"].(string)
			if got != tc.want {
				t.Errorf("apiRoute = %q，期望 %q", got, tc.want)
			}
			blob := mustMarshalForTest(t, page.Reports[0])
			for _, secret := range tc.absent {
				if strings.Contains(blob, secret) {
					t.Errorf("%q 不该出现在记录里: %s", secret, blob)
				}
			}
		})
	}
}

// ⭐ apiRoute does not go into dedup_key.
//
// Not because it is dangerous (low cardinality; it would not break rate limiting the way logid
// would), but because **there is no need**: route_pattern + error_code + http_status already frame
// "the same kind of problem", and one more dimension would only split rows that should be merged.
//
// Difference from TestAutoFeedbackIsRateLimited: that one uses identical inputs, while this one
// **deliberately makes apiRoute different each time** — if it were in the key, 20 records would
// become 20 rows.
func TestFeedbackAPIRouteIsNotInDedupKey(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})

	for i := 0; i < 20; i++ {
		if err := svc.SubmitFeedback(asActor("u@example.com"), FeedbackInput{
			Level: "auto", RoutePattern: "/w/{id}/chat",
			ErrorCode: "internal", HTTPStatus: 500,
			// Different every time: in real use the same page does call different endpoints.
			APIRoute: fmt.Sprintf("/api/endpoint-%d", i), APIMethod: "GET",
		}); err != nil {
			t.Fatalf("第 %d 条上报失败: %v", i, err)
		}
	}

	page, err := svc.ListFeedback(asActor("dev@example.com"), persistmodel.FeedbackQuery{})
	if err != nil {
		t.Fatalf("读列表失败: %v", err)
	}
	if len(page.Reports) != 1 {
		t.Fatalf("20 条同类错误该合成 1 行（apiRoute 不进去重键），实际 %d 行", len(page.Reports))
	}
	if got := page.Reports[0].Occurrences; got != 20 {
		t.Errorf("量级不能丢：occurrences 该是 20，实际 %d", got)
	}
}

// ---------------------------------------------------------------------------
// 3. Level-one rate limiting
// ---------------------------------------------------------------------------

// TestAutoFeedbackIsRateLimited pins "the same error N times does not create N rows".
//
// ⚠️ **This decides whether the feature DoSes itself.** One broken endpoint can make one user
// produce hundreds of errors in a minute (page remounts, SSE reconnects, polling retries), and
// production runs multiple instances with hundreds of users.
func TestAutoFeedbackIsRateLimited(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})
	ctx := asActor("someone@example.com")

	const times = 20
	for i := 0; i < times; i++ {
		if err := svc.SubmitFeedback(ctx, FeedbackInput{
			Level: "auto", RoutePattern: "/w/x/chat", ErrorCode: "internal", HTTPStatus: 500,
		}); err != nil {
			t.Fatalf("第 %d 次上报失败: %v", i, err)
		}
	}

	page, err := svc.ListFeedback(asActor("dev@example.com"), persistmodel.FeedbackQuery{})
	if err != nil {
		t.Fatalf("读列表失败: %v", err)
	}
	if len(page.Reports) != 1 {
		t.Fatalf("刷 %d 次只该留 1 条（限流），实际 %d 条", times, len(page.Reports))
	}
	// The magnitude must not be lost: merge into one row, but still be able to tell how many times it happened.
	// Without this field, "happened 20 times" and "happened" cannot be told apart.
	if got := page.Reports[0].Occurrences; got != times {
		t.Errorf("occurrences 要累加到 %d，实际 %d", times, got)
	}
}

// TestAutoFeedbackSeparatesDifferentErrors: different errors must **not** be merged.
//
// Paired with the previous one, it guards against "rate limiting too hard": an implementation
// that "keeps only one row for all auto records" would pass the previous test, but then a second
// real problem would never be seen.
func TestAutoFeedbackSeparatesDifferentErrors(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})

	// Five records that differ in only one dimension each: route / error code / status code / person.
	// Every dimension must be able to separate them — **the values must all differ from each other**,
	// or a wrong implementation that "looks at only one dimension" could also pass (see the lesson
	// of the mutation fixture in this repo).
	inputs := []struct {
		actor string
		in    FeedbackInput
	}{
		{"a@example.com", FeedbackInput{Level: "auto", RoutePattern: "/w/x/chat", ErrorCode: "internal", HTTPStatus: 500}},
		{"a@example.com", FeedbackInput{Level: "auto", RoutePattern: "/w/x/runs", ErrorCode: "internal", HTTPStatus: 500}},
		{"a@example.com", FeedbackInput{Level: "auto", RoutePattern: "/w/x/chat", ErrorCode: "not_found", HTTPStatus: 500}},
		{"a@example.com", FeedbackInput{Level: "auto", RoutePattern: "/w/x/chat", ErrorCode: "internal", HTTPStatus: 403}},
		{"b@example.com", FeedbackInput{Level: "auto", RoutePattern: "/w/x/chat", ErrorCode: "internal", HTTPStatus: 500}},
	}
	for i, tc := range inputs {
		if err := svc.SubmitFeedback(asActor(tc.actor), tc.in); err != nil {
			t.Fatalf("第 %d 条上报失败: %v", i, err)
		}
	}

	page, err := svc.ListFeedback(asActor("dev@example.com"), persistmodel.FeedbackQuery{})
	if err != nil {
		t.Fatalf("读列表失败: %v", err)
	}
	if len(page.Reports) != len(inputs) {
		t.Errorf("五条在不同维度上不同的记录都该独立存在，实际只有 %d 条", len(page.Reports))
	}
}

// TestUserFeedbackIsNotDeduped: level two is **deliberately not deduped**: two clicks by the user
// should make two rows.
//
// Losing something the user actively submitted is much worse than storing one extra row — they
// would see "received, thanks" while the database has nothing, and the whole value of this
// feature is that the feedback really arrives.
func TestUserFeedbackIsNotDeduped(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})
	ctx := asActor("someone@example.com")

	for i := 0; i < 3; i++ {
		if err := svc.SubmitFeedback(ctx, FeedbackInput{
			Level: "user", RoutePattern: "/w/x/chat", ErrorCode: "internal", HTTPStatus: 500,
		}); err != nil {
			t.Fatalf("第 %d 次上报失败: %v", i, err)
		}
	}
	page, err := svc.ListFeedback(asActor("dev@example.com"), persistmodel.FeedbackQuery{Level: persistmodel.FeedbackUser})
	if err != nil {
		t.Fatalf("读列表失败: %v", err)
	}
	if len(page.Reports) != 3 {
		t.Errorf("用户主动提交 3 次就该有 3 条（不去重），实际 %d 条", len(page.Reports))
	}
}

// ---------------------------------------------------------------------------
// 4. The two levels collect different field sets
// ---------------------------------------------------------------------------

// TestAutoAndUserCollectDifferentFields pins the collection boundary.
//
// Both levels send **exactly the same** input, only level differs — so any difference can only
// come from how sanitizeAuto / sanitizeUser split the work. This is the core invariant of the
// feature.
func TestAutoAndUserCollectDifferentFields(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})

	// An input that "gives everything". Level one drops part of it, level two keeps it.
	base := FeedbackInput{
		RoutePattern:  "/w/x/chat/{id}",
		ErrorCode:     "internal",
		HTTPStatus:    500,
		Message:       "加载失败，请稍后重试",
		Browser:       "Chrome 120",
		OS:            "macOS",
		AppVersion:    "abc1234",
		Path:          "/w/growth/chat/sesn_20260828_secret",
		AgentID:       "agent_secret_id",
		LogID:         "20260830120000-logid-secret",
		APIRoute:      "/api/workspaces/ws_secret/agents",
		APIMethod:     "POST",
		ConsoleErrors: []string{"TypeError: x is not a function"},
		Recent: []FeedbackCall{
			{Method: "GET", Path: "/api/runs/run_secret/events", Status: 500, Code: "internal"},
		},
	}

	autoIn := base
	autoIn.Level = "auto"
	if err := svc.SubmitFeedback(asActor("a@example.com"), autoIn); err != nil {
		t.Fatalf("一级上报失败: %v", err)
	}
	userIn := base
	userIn.Level = "user"
	if err := svc.SubmitFeedback(asActor("a@example.com"), userIn); err != nil {
		t.Fatalf("二级上报失败: %v", err)
	}

	page, err := svc.ListFeedback(asActor("dev@example.com"), persistmodel.FeedbackQuery{})
	if err != nil {
		t.Fatalf("读列表失败: %v", err)
	}
	var autoRec, userRec *FeedbackReport
	for i := range page.Reports {
		switch page.Reports[i].Level {
		case persistmodel.FeedbackAuto:
			autoRec = &page.Reports[i]
		case persistmodel.FeedbackUser:
			userRec = &page.Reports[i]
		}
	}
	if autoRec == nil || userRec == nil {
		t.Fatalf("两级各该有一条，实际拿到 %d 条", len(page.Reports))
	}

	// ---- Level one: those four keys **must not exist** ----
	//
	// Check "key does not exist" rather than "value is empty": payload is a map, and an
	// implementation that "sets an empty value" would pass the assertion below, but then the field
	// was still touched by the collection flow.
	for _, key := range []string{"path", "agentId", "recent", "consoleErrors"} {
		if _, ok := autoRec.Payload[key]; ok {
			t.Errorf("一级不该采 %q，但它在 payload 里: %v", key, autoRec.Payload[key])
		}
	}
	// Also confirm none of those specific sensitive values got into the level-one record.
	autoBlob := mustMarshalForTest(t, autoRec)
	for _, secret := range []string{"sesn_20260828_secret", "agent_secret_id", "run_secret"} {
		if strings.Contains(autoBlob, secret) {
			t.Errorf("一级记录里出现了本该只在二级出现的标识 %q: %s", secret, autoBlob)
		}
	}
	// ⚠️ **logid is an exception: both levels should collect it.**
	//
	// It needs no user consent — the string is generated by our own server, contains none of the
	// user's input, and is not a credential, while the actor column already stores "who reported
	// this". Level one needs it the most: those records are left without the user knowing, and
	// nobody is there to explain "what I was doing at the time".
	//
	// Putting it in this test (instead of a separate one) is deliberate: it sits right next to the
	// "level one does not collect path/agentId" lines, so a reader sees both sides of the boundary
	// at once — **what should be blocked and what should not**. Writing only the "not collected"
	// half would make the next person think "less is better for level one" and delete it in passing.
	if autoRec.LogID != "20260830120000-logid-secret" {
		t.Errorf("一级也要采 logid（它不需要用户同意），实际 %q", autoRec.LogID)
	}
	// ⚠️ **Same for apiRoute / apiMethod: both levels should collect them, same reason as logid.**
	//
	// They fill a real blind spot of level one: routePattern is the **page** route, and level one
	// does not collect recent, so before this, level-one records could not show "which endpoint
	// failed" at all — the first question to answer when debugging.
	//
	// Placed next to these lines on both sides of the boundary for exactly the same reason as the
	// logid block.
	if got, _ := autoRec.Payload["apiRoute"].(string); got != "/api/workspaces/{id}/agents" {
		t.Errorf("一级也要采 apiRoute（且 id 要被抹掉），实际 %q", got)
	}
	if got, _ := autoRec.Payload["apiMethod"].(string); got != "POST" {
		t.Errorf("一级也要采 apiMethod，实际 %q", got)
	}
	// ⚠️ **The backend must strip ids again**: this endpoint is public, and the caller is not
	// necessarily our frontend. A path with real ids breaks aggregation and also lets level one's
	// "do not collect the full path" boundary be bypassed.
	if strings.Contains(autoBlob, "ws_secret") {
		t.Errorf("apiRoute 里的真实 id 必须被抹成 {id}，实际记录：%s", autoBlob)
	}

	// ---- Level two: those keys must be present ----
	//
	// Verifying only "level one does not have them" is not enough: an implementation that
	// "collects them at neither level" would make everything above green, and then user feedback
	// would also lack the information needed to locate the problem — as if the feature was never
	// built.
	for _, key := range []string{"path", "agentId", "recent", "consoleErrors"} {
		if _, ok := userRec.Payload[key]; !ok {
			t.Errorf("二级应该采 %q，但 payload 里没有: %v", key, userRec.Payload)
		}
	}
	// Level two's path must be the full path (with ids) — that is exactly what it has beyond level one.
	if got, _ := userRec.Payload["path"].(string); !strings.Contains(got, "sesn_20260828_secret") {
		t.Errorf("二级的 path 要保留完整路径（含 id），实际是 %q", got)
	}
	// Level two must collect it too, and **keep it verbatim**.
	//
	// Paired with the level-one assertion above: checking only one side would leave an
	// implementation that "collects it at only one level" half green, while both levels need it.
	if userRec.LogID != "20260830120000-logid-secret" {
		t.Errorf("二级要采服务端 logid 且逐字保留，实际 %q", userRec.LogID)
	}

	// ---- The ones both levels share ----
	for _, rec := range []*FeedbackReport{autoRec, userRec} {
		if rec.Payload["message"] == nil {
			t.Errorf("%s 级要采 message（后端给的中文句子，不是用户输入）", rec.Level)
		}
		if rec.UserAgent != "Chrome 120 / macOS" {
			t.Errorf("%s 级的浏览器概要不对: %q", rec.Level, rec.UserAgent)
		}
	}
}

// TestNetworkErrorFeedbackHasNoLogID pins "do not make one up when there is no server logid".
//
// # What this guards against
//
// `network_error` is the only error code **with no response object** (status is always 0): fetch
// only rejects on network-layer failures (DNS failure, connection refused, TLS failure, CORS
// preflight failure), and then the request **never reached the server** — the HTTP middleware
// that generates the logid (httpd's withLogID) never ran, no id was generated, and the server logs
// have no line for this request at all.
//
// So the correct behavior is to **leave it empty**. This test guards against someone adding a
// placeholder string "so the field has a value" (`"-"` / `"unknown"` / one generated by the
// frontend). The comments in core/observe.go and shared/infra/sandbox/client.go both explicitly
// forbid that mistake: it is a valid string, it gets reported as a real logid, and while debugging
// you think you have a match but searching finds nothing.
//
// ⚠️ Run both levels: **both levels collect logid**, so both can hit this trap. Testing only one
// level would leave a placeholder implementation on the other level unwatched.
func TestNetworkErrorFeedbackHasNoLogID(t *testing.T) {
	for _, level := range []string{"auto", "user"} {
		t.Run(level, func(t *testing.T) {
			svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})

			// The shape the frontend sends on network_error: status 0, code network_error, logId
			// empty string. (See that catch branch in web/src/data/http.ts; there the res variable
			// has not even been assigned yet.)
			if err := svc.SubmitFeedback(asActor("u@example.com"), FeedbackInput{
				Level:        level,
				RoutePattern: "/w/ama/usage",
				HTTPStatus:   0,
				ErrorCode:    "network_error",
				ErrorKind:    "other",
				Message:      "连不上服务端，请确认后端已启动",
				Path:         "/w/ama/usage",
				LogID:        "",
			}); err != nil {
				t.Fatalf("上报失败: %v", err)
			}

			page, err := svc.ListFeedback(asActor("dev@example.com"), persistmodel.FeedbackQuery{})
			if err != nil {
				t.Fatalf("读列表失败: %v", err)
			}
			if len(page.Reports) != 1 {
				t.Fatalf("要有 1 条，实际 %d", len(page.Reports))
			}
			rec := page.Reports[0]

			if rec.LogID != "" {
				t.Errorf("network_error 下不该有 logid，实际 %q。\n"+
					"那类失败下请求从未到达服务端，withLogID 中间件没跑过，"+
					"服务端日志里不存在这次请求——编一个 id 会让排查的人白搜一遍。", rec.LogID)
			}
			// After serialization the key must **disappear entirely** (LogID has omitempty), not become `"logId":""`.
			// An empty string makes the reader think "there is a logid, it just was not fetched", while the truth is "there is none".
			if blob := mustMarshalForTest(t, rec); strings.Contains(blob, `"logId"`) {
				t.Errorf("没有 logid 时那个键该整个不出现（omitempty），实际: %s", blob)
			}
		})
	}
}

// TestFeedbackLogIDIsNotInDedupKey pins "logid must not go into the dedup key".
//
// ⚠️ **This guards against silently turning off rate limiting.** logid is different for every
// request; once it is in dedup_key, the same error N times produces N different keys and N rows —
// rate limiting fails completely, which is exactly how this feature would DoS itself (see that
// section of README-feedback.md).
//
// It raises no error and the UI looks completely normal (one row saves, a hundred rows save too);
// it only shows up during a real incident, when the table is flooded and the read endpoint fails
// too.
//
// ⚠️ **Once "level one also collects logid", this went from hypothetical to a real risk**: level
// one is the one that dedupes, and every level-one row now carries a different logid. So this
// **uses the real path** (SubmitFeedback N times in a row) instead of only unit-testing the pure
// function — the former proves no step in the whole chain mixes logid into the key, the latter
// only proves that one function does not.
func TestFeedbackLogIDIsNotInDedupKey(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})
	ctx := asActor("someone@example.com")

	// The same error 20 times, **each with a different logid** (that is what really happens:
	// a new logid per request). They must still merge into one row.
	const times = 20
	for i := 0; i < times; i++ {
		if err := svc.SubmitFeedback(ctx, FeedbackInput{
			Level: "auto", RoutePattern: "/w/x/chat", ErrorCode: "internal", HTTPStatus: 500,
			LogID: fmt.Sprintf("20260830120000-logid-%d", i),
		}); err != nil {
			t.Fatalf("第 %d 次上报失败: %v", i, err)
		}
	}

	page, err := svc.ListFeedback(asActor("dev@example.com"), persistmodel.FeedbackQuery{})
	if err != nil {
		t.Fatalf("读列表失败: %v", err)
	}
	if len(page.Reports) != 1 {
		t.Fatalf("20 条带不同 logid 的同类错误必须合成 1 行，实际 %d 行。\n"+
			"logid 进了 dedup_key —— 限流完全失效，一个坏接口就能把表刷爆。",
			len(page.Reports))
	}
	if got := page.Reports[0].Occurrences; got != times {
		t.Errorf("量级不能丢：occurrences 该是 %d，实际 %d", times, got)
	}
	// After merging, the **first** logid is kept (on conflict only touches occurrences and
	// last_seen_at, preserving the first occurrence). This also pins that behavior.
	if got := page.Reports[0].LogID; got != "20260830120000-logid-0" {
		t.Errorf("合并后该保留首次那个 logid（首次现场比第 20 次有价值），实际 %q", got)
	}

	// Ask the pure function directly too: the whole chain is green, but the key itself should also
	// not depend on logid. Both layers are checked because someone may change only one of them later.
	now := time.Now().UTC()
	base := persistmodel.FeedbackRecord{
		Actor:        "u@example.com",
		RoutePattern: "/w/x/chat",
		ErrorCode:    "internal",
		HTTPStatus:   500,
	}
	withLog := base
	withLog.LogID = "20260830120000-some-logid"
	other := base
	other.LogID = "20260830120001-a-different-one"

	k1 := feedbackDedupKey(base, now)
	k2 := feedbackDedupKey(withLog, now)
	k3 := feedbackDedupKey(other, now)

	if k1 != k2 || k2 != k3 {
		t.Errorf("logid 不能影响去重键，否则每条反馈都是新行、限流完全失效。\n"+
			"无 logid: %s\n有 logid: %s\n换一个:  %s", k1, k2, k3)
	}
}

// TestFeedbackTruncatesLogID: the length limit applies to logid too.
//
// Like other fields it comes from the frontend, i.e. input the caller controls — without a limit,
// one request could stuff hundreds of MB into that column. A real logid is about 50 characters;
// cutting at 200 is plenty.
func TestFeedbackTruncatesLogID(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})

	if err := svc.SubmitFeedback(asActor("u@example.com"), FeedbackInput{
		Level: "user", RoutePattern: "/w/x", ErrorCode: "internal",
		LogID: strings.Repeat("x", 5000),
	}); err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	page, err := svc.ListFeedback(asActor("dev@example.com"), persistmodel.FeedbackQuery{})
	if err != nil {
		t.Fatalf("读列表失败: %v", err)
	}
	if got := len([]rune(page.Reports[0].LogID)); got > maxFeedbackReasonLen {
		t.Errorf("logid 应被截到 %d 个字符以内，实际 %d 个", maxFeedbackReasonLen, got)
	}
}

// TestFeedbackRecentCallsCarryLogID pins that each of the "recent failed API calls" carries a
// logid.
//
// # Why this test exists on its own
//
// It is **the only way to get a logid in this feedback scenario (cannot reach the server)**: the
// current failure has no logid of its own (the request did not arrive), but if the user used the
// app normally for a while before the backend went down, the earlier failures that **did reach**
// the server each carry a real logid — following them locates the moment the backend went down.
//
// ⚠️ It also pins "the calls with no logid do not store an empty-string key": mixing a pile of
// empty strings in would hide "which calls really reached the server", which is the whole value of
// this trail in this scenario.
func TestFeedbackRecentCallsCarryLogID(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})

	if err := svc.SubmitFeedback(asActor("u@example.com"), FeedbackInput{
		Level: "user", RoutePattern: "/w/ama/usage", ErrorCode: "network_error", HTTPStatus: 0,
		Recent: []FeedbackCall{
			// The call before the backend went down: reached the server, has a real logid.
			{Method: "GET", Path: "/api/workspaces/ws_1/runs", Status: 500, Code: "internal",
				LogID: "20260830115900-before-the-crash"},
			// The call after it went down: the request never left, no logid.
			{Method: "GET", Path: "/api/workspaces/ws_1/usage", Status: 0, Code: "network_error"},
		},
	}); err != nil {
		t.Fatalf("上报失败: %v", err)
	}

	page, err := svc.ListFeedback(asActor("dev@example.com"), persistmodel.FeedbackQuery{})
	if err != nil {
		t.Fatalf("读列表失败: %v", err)
	}
	recent, ok := page.Reports[0].Payload["recent"].([]map[string]any)
	if !ok || len(recent) != 2 {
		t.Fatalf("recent 应有 2 条，实际 %#v", page.Reports[0].Payload["recent"])
	}

	// First: has a logid, kept verbatim. This is **the only traceable clue** in this scenario.
	if got := recent[0]["logId"]; got != "20260830115900-before-the-crash" {
		t.Errorf("到达过服务端的那次调用要带真 logid，实际 %#v", got)
	}
	// Second: **that key should not exist at all**, rather than being an empty string.
	if got, ok := recent[1]["logId"]; ok {
		t.Errorf("没到达服务端的那次不该有 logId 键，实际 %#v。\n"+
			"落一堆空串会让「哪几次真到过服务端」看不出来，"+
			"而那正是这份轨迹在 network_error 场景下的全部价值。", got)
	}
}

// TestFeedbackStripsRouteIDs: the backend must strip ids from the route again.
//
// The frontend already strips them, but this is a public HTTP endpoint and the caller is not
// necessarily our frontend; a route_pattern containing ids **silently breaks** two things: every
// dedup key is different (rate limiting fails), and group by becomes a table where every row has
// count 1 (aggregation is meaningless).
func TestFeedbackStripsRouteIDs(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})
	if err := svc.SubmitFeedback(asActor("a@example.com"), FeedbackInput{
		Level:        "auto",
		RoutePattern: "/w/growth/runs/run_abc123/events/42",
		ErrorCode:    "internal",
	}); err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	page, err := svc.ListFeedback(asActor("dev@example.com"), persistmodel.FeedbackQuery{})
	if err != nil {
		t.Fatalf("读列表失败: %v", err)
	}
	got := page.Reports[0].RoutePattern
	if strings.Contains(got, "run_abc123") {
		t.Errorf("路由里的资源 id 必须被抹掉，实际是 %q", got)
	}
	if got != "/w/growth/runs/{id}/events/{id}" {
		t.Errorf("抹完应该是 /w/growth/runs/{id}/events/{id}，实际是 %q", got)
	}
}

// TestFeedbackStripsHyphenatedSessionIDs: hyphenated session ids must be stripped too.
//
// # This is a hole found only end to end, not extra coverage
//
// A real session id is `sesn-20260101000003-ex003` — **no underscore at all**, not purely numeric,
// not a hex string, not a UUID, so none of the original four rules matched, and route_pattern with
// a real session id went straight into the database. Two harms, neither with an error:
//
//   - **Level one leaked "which chat this person was just in"**, and level one is exactly the
//     level with no user consent;
//   - every dedup key is different (rate limiting fails), and aggregation becomes a table where
//     every row has count 1.
//
// TestFeedbackStripsRouteIDs above cannot catch it, because it uses `run_abc123` (with an
// underscore). The mock seed data also uses `run_1`, so the whole e2e suite was green too —
// **the same field has two shapes in the test environment and in production**, which is the
// common cause of this kind of hole.
func TestFeedbackStripsHyphenatedSessionIDs(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})
	if err := svc.SubmitFeedback(asActor("a@example.com"), FeedbackInput{
		Level:        "auto",
		RoutePattern: "/w/ama/chat/sesn-20260101000003-ex003",
		ErrorCode:    "internal",
	}); err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	page, err := svc.ListFeedback(asActor("dev@example.com"), persistmodel.FeedbackQuery{})
	if err != nil {
		t.Fatalf("读列表失败: %v", err)
	}
	got := page.Reports[0].RoutePattern
	if strings.Contains(got, "sesn-20260101000003-ex003") {
		t.Errorf("会话 id 必须被抹掉，实际是 %q", got)
	}
	if got != "/w/ama/chat/{id}" {
		t.Errorf("抹完应该是 /w/ama/chat/{id}，实际是 %q", got)
	}
}

// TestFeedbackKeepsRouteLiterals: stripping ids must not also strip route literals and workspace
// keys.
//
// It is **paired** with the previous one: guarding only against misses would tempt someone to
// loosen the rule to "strip anything with a hyphen", and then segments like `ark-agent` (a real
// workspace key) and `skill-hub` would also become `{id}`, all routes would look the same, and the
// field would lose all value. Both directions must be pinned.
func TestFeedbackKeepsRouteLiterals(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})
	if err := svc.SubmitFeedback(asActor("a@example.com"), FeedbackInput{
		Level:        "auto",
		RoutePattern: "/w/ark-agent/skill-hub",
		ErrorCode:    "internal",
	}); err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	page, err := svc.ListFeedback(asActor("dev@example.com"), persistmodel.FeedbackQuery{})
	if err != nil {
		t.Fatalf("读列表失败: %v", err)
	}
	if got := page.Reports[0].RoutePattern; got != "/w/ark-agent/skill-hub" {
		t.Errorf("带连字符的路由字面量与空间 key 不该被抹，实际是 %q", got)
	}
}

// TestSubmitFeedbackRejectsUnknownLevel: an unrecognized level must be rejected; **do not pick a
// default**.
//
// Defaulting to auto would make user-submitted feedback take part in dedup and get merged into an
// existing record — the user sees "received" while their feedback does not exist on its own.
// Defaulting to user is worse: all level-one flooding would bypass rate limiting.
func TestSubmitFeedbackRejectsUnknownLevel(t *testing.T) {
	svc := newFeedbackService(t, nil)
	for _, level := range []string{"", "AUTO", "urgent", "1"} {
		err := svc.SubmitFeedback(asActor("a@example.com"), FeedbackInput{Level: level})
		if err == nil {
			t.Errorf("level=%q 应该被拒绝，实际接受了", level)
		}
	}
}

// TestFeedbackRequiresActor: without an identity it never reaches the database.
//
// Returns ErrInvalidArgument (400) rather than 401/403, consistent with onboardingActor: these two
// routes are already behind auth.Protect, so reaching here without an identity means the caller
// did not come through HTTP.
func TestFeedbackRequiresActor(t *testing.T) {
	svc := newFeedbackService(t, nil)
	if err := svc.SubmitFeedback(context.Background(), FeedbackInput{Level: "user"}); err == nil {
		t.Error("没有身份时应该拒绝上报")
	}
	if _, err := svc.ListFeedback(context.Background(), persistmodel.FeedbackQuery{}); err == nil {
		t.Error("没有身份时应该拒绝读取")
	}
}

// TestListFeedbackPagination: pagination parameters must be clamped, and the total is separate from the page.
func TestListFeedbackPagination(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})
	dev := asActor("dev@example.com")

	// Create 5 user-level rows (not deduped, so there really are 5).
	for i := 0; i < 5; i++ {
		if err := svc.SubmitFeedback(asActor("a@example.com"), FeedbackInput{
			Level: "user", RoutePattern: "/w/x", ErrorCode: "internal",
		}); err != nil {
			t.Fatalf("上报失败: %v", err)
		}
	}

	page, err := svc.ListFeedback(dev, persistmodel.FeedbackQuery{Limit: 2})
	if err != nil {
		t.Fatalf("读列表失败: %v", err)
	}
	if len(page.Reports) != 2 {
		t.Errorf("limit=2 该返回 2 条，实际 %d", len(page.Reports))
	}
	// ⚠️ total is **the total count matching the filter**, not the count of this page. Callers use
	// it to tell "is there a next page"; using "does this page have limit rows" is wrong when the
	// last page happens to be full.
	if page.Total != 5 {
		t.Errorf("total 该是 5（总数），实际 %d", page.Total)
	}

	// A limit over the cap is clamped to the cap, not an error — script authors should not have to read our constants before pulling data.
	page, err = svc.ListFeedback(dev, persistmodel.FeedbackQuery{Limit: 99999})
	if err != nil {
		t.Fatalf("超大 limit 应该被收敛而不是报错: %v", err)
	}
	if page.Limit != maxFeedbackPageSize {
		t.Errorf("limit 该被截到 %d，实际 %d", maxFeedbackPageSize, page.Limit)
	}

	// limit=0 uses the default.
	page, err = svc.ListFeedback(dev, persistmodel.FeedbackQuery{})
	if err != nil {
		t.Fatalf("读列表失败: %v", err)
	}
	if page.Limit != feedbackDefaultLimit {
		t.Errorf("limit=0 该用默认值 %d，实际 %d", feedbackDefaultLimit, page.Limit)
	}
}

// TestListFeedbackFilters filters by level and errorCode.
func TestListFeedbackFilters(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})
	dev := asActor("dev@example.com")

	for _, tc := range []FeedbackInput{
		{Level: "auto", RoutePattern: "/a", ErrorCode: "internal", HTTPStatus: 500},
		{Level: "auto", RoutePattern: "/b", ErrorCode: "access_denied", HTTPStatus: 403},
		{Level: "user", RoutePattern: "/c", ErrorCode: "internal", HTTPStatus: 500},
	} {
		if err := svc.SubmitFeedback(asActor("a@example.com"), tc); err != nil {
			t.Fatalf("上报失败: %v", err)
		}
	}

	page, err := svc.ListFeedback(dev, persistmodel.FeedbackQuery{Level: persistmodel.FeedbackUser})
	if err != nil {
		t.Fatalf("读列表失败: %v", err)
	}
	if len(page.Reports) != 1 || page.Reports[0].Level != persistmodel.FeedbackUser {
		t.Errorf("按 level=user 筛该得 1 条 user，实际 %d 条", len(page.Reports))
	}

	page, err = svc.ListFeedback(dev, persistmodel.FeedbackQuery{ErrorCode: "access_denied"})
	if err != nil {
		t.Fatalf("读列表失败: %v", err)
	}
	if len(page.Reports) != 1 || page.Reports[0].ErrorCode != "access_denied" {
		t.Errorf("按 errorCode 筛该得 1 条，实际 %d 条", len(page.Reports))
	}

	// An unrecognized level must be an error, not treated as "no filter" — silently not filtering
	// would make a script think "there are no user-level rows" while it actually got everything.
	if _, err := svc.ListFeedback(dev, persistmodel.FeedbackQuery{Level: "bogus"}); err == nil {
		t.Error("认不出的 level 应该报错")
	}
}

// TestFeedbackRejectsWorkspaceKeyAsID: a workspace key must not be stored as an id.
//
// The frontend often has the key (growth) at hand instead of the id (ws_xxx), and once a key is
// stored this column can no longer be joined with the workspaces table — it shows up as "the
// workspace in the feedback cannot be found".
func TestFeedbackRejectsWorkspaceKeyAsID(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})
	if err := svc.SubmitFeedback(asActor("a@example.com"), FeedbackInput{
		Level: "user", RoutePattern: "/w/x", WorkspaceID: "growth",
	}); err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	page, err := svc.ListFeedback(asActor("dev@example.com"), persistmodel.FeedbackQuery{})
	if err != nil {
		t.Fatalf("读列表失败: %v", err)
	}
	if got := page.Reports[0].WorkspaceID; got != "" {
		t.Errorf("不像 id 的空间标识该被丢掉，实际存了 %q", got)
	}
}

// TestFeedbackTruncatesLongText: overlong fields must be truncated instead of stored as is.
//
// All these fields come from the caller; without a limit, one request could stuff hundreds of MB
// into PG. Truncate by rune, not by byte: cutting by byte would split a Chinese character in half,
// and PG's text column reports 22021 invalid byte sequence **on write**, which is a 500 with an
// error pointing at encoding.
func TestFeedbackTruncatesLongText(t *testing.T) {
	svc := newFeedbackService(t, feedbackTestReaders{allow: []string{"dev@example.com"}})
	long := strings.Repeat("很长的中文错误信息", 500) // 4500 runes, far over the limit
	if err := svc.SubmitFeedback(asActor("a@example.com"), FeedbackInput{
		Level: "user", RoutePattern: "/w/x", Message: long,
	}); err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	page, err := svc.ListFeedback(asActor("dev@example.com"), persistmodel.FeedbackQuery{})
	if err != nil {
		t.Fatalf("读列表失败: %v", err)
	}
	msg, _ := page.Reports[0].Payload["message"].(string)
	if n := len([]rune(msg)); n > maxFeedbackTextLen {
		t.Errorf("message 该被截到 %d 个字符，实际 %d", maxFeedbackTextLen, n)
	}
	// After truncation it must still be valid UTF-8 (guaranteed by truncating by rune).
	if !utf8ValidForTest(msg) {
		t.Error("截断后的文本不是合法 UTF-8——按字节截会切坏汉字")
	}
}
