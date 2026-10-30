// Store contract for onboarding: the shared suite in store_test.go runs it **once each** against MemStore and a real PGStore.
//
// These two (no more onboarding after completion / skip is persisted too) are in the shared suite instead of memory-only tests
// for a concrete reason: in production the invariant is finally guaranteed by PG's primary key unique constraint, while the in-memory map
// assignment is naturally idempotent. Testing only memory, an error like "PG forgot the on conflict" would still be all green.
//
// The Service and HTTP layer tests are still in core/onboarding_test.go.

package coredao

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// Two different people, to check accounts do not affect each other.
const (
	onboardingAlice = "alice@example.com"
	onboardingBob   = "bob@example.com"
)

// ---------------------------------------------------------------------------
// Store layer: cases a / d, run once each against the two implementations
// ---------------------------------------------------------------------------

// testOnboardingPersistsCompletion is (a): after marking complete, a read must not still say onboarding is needed.
//
// **This is the core of the whole feature.** When it breaks, the user is onboarded on every login,
// and each time it "looks normal": no error, no unusual log, just very annoying.
func testOnboardingPersistsCompletion(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)

	// No record at first: the Store layer returns ErrNotFound, which the Service translates into "brand-new user".
	if _, err := s.GetOnboarding(ctx, onboardingAlice); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("没有记录时应回 ErrNotFound，实际 %v", err)
	}

	done := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	if err := s.PutOnboarding(ctx, persistmodel.OnboardingRecord{
		Email: onboardingAlice, CompletedAt: done, Version: 1, UpdatedAt: done,
	}); err != nil {
		t.Fatalf("标记完成失败：%v", err)
	}

	got, err := s.GetOnboarding(ctx, onboardingAlice)
	if err != nil {
		t.Fatalf("读引导状态失败：%v", err)
	}
	if got.CompletedAt.IsZero() {
		t.Error("标记完成后 completedAt 还是零值——这个人会被再引导一遍")
	}
	if !got.CompletedAt.Equal(done) {
		t.Errorf("完成时刻不对：期望 %v 实际 %v", done, got.CompletedAt)
	}
	if got.Version != 1 {
		t.Errorf("版本号没存住：期望 1 实际 %d", got.Version)
	}

	// **Marking again must not error** (the user refreshed and clicked complete again, or the request was retried).
	// This is exactly the on conflict half: without it the second time hits the primary key unique constraint,
	// and the user sees a 500 though they did nothing wrong.
	later := done.Add(time.Hour)
	if err := s.PutOnboarding(ctx, persistmodel.OnboardingRecord{
		Email: onboardingAlice, CompletedAt: later, Version: 1, UpdatedAt: later,
	}); err != nil {
		t.Fatalf("重复标记完成应当幂等，实际报错：%v", err)
	}
	got, err = s.GetOnboarding(ctx, onboardingAlice)
	if err != nil {
		t.Fatalf("重复标记后读失败：%v", err)
	}
	if !got.CompletedAt.Equal(later) {
		t.Errorf("重复标记应覆盖成新时刻：期望 %v 实际 %v", later, got.CompletedAt)
	}
}

// testOnboardingPersistsSkip is (d): a skip must be stored too, as durably as finishing.
//
// Otherwise the user is bothered on every login after clearly saying they do not want to see it,
// which is worse than "no onboarding", because it throws away an explicit choice by the user.
func testOnboardingPersistsSkip(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)

	at := time.Date(2026, 8, 28, 11, 0, 0, 0, time.UTC)
	if err := s.PutOnboarding(ctx, persistmodel.OnboardingRecord{
		Email: onboardingAlice, SkippedAt: at, Version: 1, UpdatedAt: at,
	}); err != nil {
		t.Fatalf("标记跳过失败：%v", err)
	}

	got, err := s.GetOnboarding(ctx, onboardingAlice)
	if err != nil {
		t.Fatalf("读引导状态失败：%v", err)
	}
	if got.SkippedAt.IsZero() {
		t.Error("跳过后 skippedAt 还是零值——用户下次登录还会被引导")
	}
	// A skip **must not** also fill completedAt: the two mean different things, and once mixed you can no longer tell
	// "they finished it" from "they did not want to see it".
	if !got.CompletedAt.IsZero() {
		t.Errorf("只跳过时 completedAt 应为零值，实际 %v", got.CompletedAt)
	}
}

// testOnboardingIsPerAccount is the Store-layer half of (b): two people's records do not interfere.
//
// The Service layer has a stronger one (TestOnboardingIsScopedToActor), but pin it at this layer first:
// a wrong primary key (say everyone writes into the same key) would let the first person who finishes onboarding turn it off for the whole company,
// and that is invisible in a single-person test.
func testOnboardingIsPerAccount(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)

	at := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	if err := s.PutOnboarding(ctx, persistmodel.OnboardingRecord{
		Email: onboardingAlice, CompletedAt: at, Version: 1, UpdatedAt: at,
	}); err != nil {
		t.Fatalf("给 alice 标记失败：%v", err)
	}

	// bob should have no record at all.
	if _, err := s.GetOnboarding(ctx, onboardingBob); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("alice 标记完成后 bob 竟然也有记录了（err=%v）——引导状态串号了", err)
	}

	// Check the other direction too: after bob marks his own, alice's record is unaffected.
	bobAt := at.Add(time.Hour)
	if err := s.PutOnboarding(ctx, persistmodel.OnboardingRecord{
		Email: onboardingBob, SkippedAt: bobAt, Version: 2, UpdatedAt: bobAt,
	}); err != nil {
		t.Fatalf("给 bob 标记失败：%v", err)
	}
	alice, err := s.GetOnboarding(ctx, onboardingAlice)
	if err != nil {
		t.Fatalf("读 alice 失败：%v", err)
	}
	if !alice.CompletedAt.Equal(at) || alice.Version != 1 || !alice.SkippedAt.IsZero() {
		t.Errorf("bob 的标记改到了 alice 的记录：%+v", alice)
	}
}

// testOnboardingMergesBothTimestamps pins the merge behavior of upsert.
//
// Scenario: the user first clicked skip, then later replayed it from preferences and finished it. Both times should be in the database.
//
// **This is also a check that "the two implementations behave the same".** PG uses coalesce, memory checks zero values explicitly;
// if they differ, this test is half green, half red, and the first reaction would be "the PG environment is broken",
// when the real cause is that the two implementations understand the same action differently.
func testOnboardingMergesBothTimestamps(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)

	skipped := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	if err := s.PutOnboarding(ctx, persistmodel.OnboardingRecord{
		Email: onboardingAlice, SkippedAt: skipped, Version: 1, UpdatedAt: skipped,
	}); err != nil {
		t.Fatalf("标记跳过失败：%v", err)
	}

	// Later they finished it. This time only CompletedAt is set; SkippedAt is the zero value.
	completed := skipped.Add(24 * time.Hour)
	if err := s.PutOnboarding(ctx, persistmodel.OnboardingRecord{
		Email: onboardingAlice, CompletedAt: completed, Version: 1, UpdatedAt: completed,
	}); err != nil {
		t.Fatalf("标记完成失败：%v", err)
	}

	got, err := s.GetOnboarding(ctx, onboardingAlice)
	if err != nil {
		t.Fatalf("读引导状态失败：%v", err)
	}
	if !got.CompletedAt.Equal(completed) {
		t.Errorf("完成时刻不对：期望 %v 实际 %v", completed, got.CompletedAt)
	}
	// Key assertion: **the skip time must not be wiped**. Wiping it loses the fact "they once skipped",
	// and that is the whole reason skipped_at exists on its own (later, people who skipped get a different prompt).
	if got.SkippedAt.IsZero() {
		t.Error("标记完成把 skippedAt 抹掉了——upsert 少了 coalesce")
	}
	if !got.SkippedAt.Equal(skipped) {
		t.Errorf("跳过时刻被改了：期望 %v 实际 %v", skipped, got.SkippedAt)
	}
}
