// Tests for onboarding.
//
// # This feature has only one invariant that really matters
//
// "The same person is never onboarded twice". It sounds simple, but there are four independent
// ways it can fail, so it is pinned in four groups below:
//
//	a. after marking done, reading again must **not** say onboard → testOnboardingPersistsCompletion
//	b. A cannot read or change B's state                          → TestOnboardingIsScopedToActor
//	c. not logged in does not trigger it                          → TestOnboardingRequiresLogin
//	d. skipping is persisted too                                  → testOnboardingPersistsSkip
//
// a and d live in the **shared store suite** (runStoreSuite in store_test.go), so they run once
// against MemStore and once against the real PGStore. That is on purpose: in production the
// invariant is ultimately guaranteed by PG's primary key unique constraint, while the in-memory
// map assignment is naturally idempotent. Testing only the in-memory one would keep everything
// green even if "the PG side forgot the on conflict".

package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/auth"
)

// Two different people, used to check that accounts do not affect each other.
const (
	onboardingAlice = "alice@example.com"
	onboardingBob   = "bob@example.com"
)

// ---------------------------------------------------------------------------
// Service layer: b / c
// ---------------------------------------------------------------------------

// TestOnboardingIsScopedToActor is (b): A cannot read or write B's onboarding state.
//
// # What exactly this checks
//
// "Overreach" on these two endpoints **should not be expressible at all**: the actor only comes
// from auth.ActorFrom(ctx), and the parameters have no email. So this test does not check
// "overreach was blocked", but something stronger: **identity is decided entirely by the
// context**. The same call, with only the person in the context changed, must return the other
// person's state.
//
// The real mistake it catches is the "read email from the request body" kind of change: once the
// handler accepts an email in the body, A can turn off B's onboarding (or find out whether B has
// seen it), and that overreach reports no error at all.
func TestOnboardingIsScopedToActor(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	aliceCtx := auth.WithIdentity(ctx, auth.Identity{Email: onboardingAlice, Name: "Alice"})
	bobCtx := auth.WithIdentity(ctx, auth.Identity{Email: onboardingBob, Name: "Bob"})

	// alice finished onboarding.
	if _, err := svc.MarkOnboarding(aliceCtx, OnboardingComplete, 1); err != nil {
		t.Fatalf("alice 标记完成失败：%v", err)
	}

	// alice herself reads "done".
	aliceState, err := svc.GetOnboarding(aliceCtx)
	if err != nil {
		t.Fatalf("alice 读状态失败：%v", err)
	}
	if !aliceState.Done() {
		t.Fatal("alice 标记完成后自己读却说还要引导")
	}

	// **bob must read "not onboarded yet"**: he should not see alice's state.
	bobState, err := svc.GetOnboarding(bobCtx)
	if err != nil {
		t.Fatalf("bob 读状态失败：%v", err)
	}
	if bobState.Done() {
		t.Error("bob 读到了 alice 的引导状态——他会被跳过引导，而他从没看过")
	}
	if bobState.Version != 0 {
		t.Errorf("bob 没有记录时版本号应为 0，实际 %d", bobState.Version)
	}

	// The other way around: bob skips onboarding, and it **must not** touch alice's record.
	if _, err := svc.MarkOnboarding(bobCtx, OnboardingSkip, 1); err != nil {
		t.Fatalf("bob 标记跳过失败：%v", err)
	}
	aliceState, err = svc.GetOnboarding(aliceCtx)
	if err != nil {
		t.Fatalf("alice 再读失败：%v", err)
	}
	if aliceState.SkippedAt != "" {
		t.Error("bob 的跳过写到了 alice 的记录上")
	}
	if aliceState.CompletedAt == "" {
		t.Error("bob 的操作把 alice 的完成状态弄丢了")
	}
}

// TestOnboardingRequiresLogin is the Go-side half of (c): no identity means rejection.
//
// **Return ErrInvalidArgument (400) instead of making up an empty actor to query the database**,
// consistent with the four authorization management endpoints: querying with an empty string as
// the key shows up as "not found → treated as a brand new user → onboard", so every call without
// an identity would see onboarding, and all such calls would share the same empty-string record.
// The first person to mark it done would turn it off for everyone.
func TestOnboardingRequiresLogin(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	if _, err := svc.GetOnboarding(ctx); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("未登录读状态应回 ErrInvalidArgument，实际 %v", err)
	}
	if _, err := svc.MarkOnboarding(ctx, OnboardingComplete, 1); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("未登录标记应回 ErrInvalidArgument，实际 %v", err)
	}

	// An identity of only whitespace also counts as not logged in (the TrimSpace step).
	blankCtx := auth.WithIdentity(ctx, auth.Identity{Email: "   "})
	if _, err := svc.GetOnboarding(blankCtx); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("空白身份应回 ErrInvalidArgument，实际 %v", err)
	}
}

// TestOnboardingEmailIsCaseInsensitive: the same person with different letter case has only one
// record.
//
// auth already lowercases the email at the entry, so the normal path never produces two
// spellings. This is **defense in depth**: if some path bypasses normalization, the same person
// gets two records, which shows up as "onboarding occasionally appears again". Such intermittent
// behavior is very hard to trace back to "primary key case mismatch".
func TestOnboardingEmailIsCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)

	upper := strings.ToUpper(onboardingAlice)
	if _, err := svc.MarkOnboarding(
		auth.WithIdentity(ctx, auth.Identity{Email: upper}), OnboardingComplete, 1); err != nil {
		t.Fatalf("标记完成失败：%v", err)
	}

	// Reading in lowercase should get the same record.
	state, err := svc.GetOnboarding(auth.WithIdentity(ctx, auth.Identity{Email: onboardingAlice}))
	if err != nil {
		t.Fatalf("读状态失败：%v", err)
	}
	if !state.Done() {
		t.Error("大写邮箱标记完成后，小写读不到——同一个人有了两条记录")
	}
}

// TestMarkOnboardingRejectsUnknownAction: an unrecognized action must be rejected, not given a
// default.
//
// Defaulting to complete would let a misspelled request turn off the user's onboarding for good,
// while they saw nothing.
func TestMarkOnboardingRejectsUnknownAction(t *testing.T) {
	ctx := auth.WithIdentity(context.Background(), auth.Identity{Email: onboardingAlice})
	svc, _ := newService(t)

	if _, err := svc.MarkOnboarding(ctx, OnboardingAction("finish"), 1); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("未知动作应回 ErrInvalidArgument，实际 %v", err)
	}
	// After rejecting, no record may be left; otherwise "reject" just means no success was returned, and the side effect still happened.
	state, err := svc.GetOnboarding(ctx)
	if err != nil {
		t.Fatalf("读状态失败：%v", err)
	}
	if state.Done() {
		t.Error("未知动作被拒绝了，却把引导标记成完成了")
	}

	// Same for a negative version: if stored, it is less than any real version, and this person is onboarded on every login.
	if _, err := svc.MarkOnboarding(ctx, OnboardingComplete, -1); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("负版本号应回 ErrInvalidArgument，实际 %v", err)
	}
}

// ---------------------------------------------------------------------------
// HTTP layer: route shape and where identity comes from
// ---------------------------------------------------------------------------

// newOnboardingMux builds a mux with the routes installed.
//
// Mounted on a **bare mux** (not through auth.Protect), so the identity injected by
// auth.WithIdentity takes effect. In the real chain the identity is written by Protect; injecting
// it directly here tests the seam between handler and Service on its own.
func newOnboardingMux(t *testing.T) *http.ServeMux {
	t.Helper()
	svc, _ := newService(t)
	h := newTestHandler(t, svc, nil)
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}

// doOnboarding sends a request; an empty actor means no identity.
func doOnboarding(mux *http.ServeMux, method, path, body, actor string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if actor != "" {
		req = req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{Email: actor}))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestOnboardingHTTPRequiresLogin is the HTTP-layer half of (c): requests without identity must be rejected.
func TestOnboardingHTTPRequiresLogin(t *testing.T) {
	mux := newOnboardingMux(t)

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/user-onboarding", ""},
		{http.MethodPost, "/api/user-onboarding", `{"action":"complete","version":1}`},
	} {
		rec := doOnboarding(mux, tc.method, tc.path, tc.body, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s 未登录时应回 400，实际 %d", tc.method, tc.path, rec.Code)
		}
	}
}

// TestOnboardingHTTPIgnoresBodyEmail is the most important HTTP-layer case of (b):
// **the email in the request body must have no effect.**
//
// # Why test this separately
//
// The authz level of these two routes is ActionNone (logged in is enough), and the **only**
// premise for that level is "the caller comes from the context, and the parameters cannot name
// someone else". If someday someone lets the handler accept the email in the body "while at it",
// overreach goes from "not expressible" to "wide open", and ActionNone would still be green with
// no error at all. So this invariant must be pinned explicitly.
//
// Shape of the test: bob sends a request **claiming to be alice**, then we check whose record was
// changed.
func TestOnboardingHTTPIgnoresBodyEmail(t *testing.T) {
	mux := newOnboardingMux(t)

	// bob submits, but puts alice's email in the request body (with several common field spellings).
	body := `{"action":"complete","version":1,` +
		`"email":"` + onboardingAlice + `","actor":"` + onboardingAlice + `"}`
	if rec := doOnboarding(mux, http.MethodPost, "/api/user-onboarding", body, onboardingBob); rec.Code != http.StatusOK {
		t.Fatalf("bob 标记完成应成功，实际 %d：%s", rec.Code, rec.Body.String())
	}

	// alice reads her own: **it must still be "never onboarded"**.
	rec := doOnboarding(mux, http.MethodGet, "/api/user-onboarding", "", onboardingAlice)
	if rec.Code != http.StatusOK {
		t.Fatalf("alice 读状态应成功，实际 %d", rec.Code)
	}
	var alice OnboardingState
	if err := json.Unmarshal(rec.Body.Bytes(), &alice); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if alice.Done() {
		t.Error("bob 通过请求体里的 email 把 alice 的引导关掉了——身份没有只从 context 取")
	}

	// And bob's own was indeed recorded (showing the request itself succeeded, not that the whole endpoint did nothing).
	rec = doOnboarding(mux, http.MethodGet, "/api/user-onboarding", "", onboardingBob)
	var bob OnboardingState
	if err := json.Unmarshal(rec.Body.Bytes(), &bob); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if !bob.Done() {
		t.Error("bob 自己的引导状态没记上")
	}
}

// TestGetOnboardingHTTPNewUserReturns200: a brand new user reading the state must get 200 +
// version:0, **not 404**.
//
// 404 would be turned into null by the frontend's nullable getter, and null means "still
// loading" to the caller, so new users would be stuck loading forever and onboarding would never
// appear. This guards that shape.
func TestGetOnboardingHTTPNewUserReturns200(t *testing.T) {
	mux := newOnboardingMux(t)

	rec := doOnboarding(mux, http.MethodGet, "/api/user-onboarding", "", "newcomer@example.com")
	if rec.Code != http.StatusOK {
		t.Fatalf("全新用户读状态应回 200，实际 %d", rec.Code)
	}

	// **The version key must really be in the JSON**, even when the value is 0.
	// With omitempty it would disappear, and the frontend's `undefined < 1` is always false, so
	// new users are not onboarded: the one thing this feature has to get right silently breaks.
	// So we check the raw bytes here.
	if !strings.Contains(rec.Body.String(), `"version"`) {
		t.Errorf("响应里必须有 version 键（0 也要给），实际：%s", rec.Body.String())
	}
	var state OnboardingState
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if state.Version != 0 || state.Done() {
		t.Errorf("全新用户状态不对：%+v", state)
	}
}
