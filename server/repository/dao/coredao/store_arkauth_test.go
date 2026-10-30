package coredao

import (
	"context"
	"testing"
	"time"
)

// testArkAuthFailureSince pins three rules of ark_auth_failures (both implementations run it):
//
//  1. Consecutive failures with the same key keep the start fixed; "how long so far" only grows;
//  2. A changed key (fingerprint changed) starts the clock over: a new key must not inherit the old key's failure duration,
//     otherwise the first failure after the user switches keys (say the new key is also wrong) pauses for a whole day right away;
//  3. After Clear, a new failure starts the clock over.
func testArkAuthFailureSince(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)
	const env, ws = "env_auth_test", "ws_demo"

	first, err := s.RecordArkAuthFailure(ctx, env, ws, "len=46 sha=aaaa1111")
	if err != nil {
		t.Fatalf("记第一次失败：%v", err)
	}
	if first < 0 || first > time.Minute {
		t.Fatalf("第一次失败的持续时长应约等于 0，实际 %v", first)
	}

	time.Sleep(50 * time.Millisecond)
	second, err := s.RecordArkAuthFailure(ctx, env, ws, "len=46 sha=aaaa1111")
	if err != nil {
		t.Fatalf("记第二次失败：%v", err)
	}
	if second < 40*time.Millisecond {
		t.Errorf("同一把 key 再失败，起点不该动：持续时长应 ≥ 两次间隔，实际 %v", second)
	}

	changed, err := s.RecordArkAuthFailure(ctx, env, ws, "len=46 sha=bbbb2222")
	if err != nil {
		t.Fatalf("换 key 后记失败：%v", err)
	}
	if changed >= second {
		t.Errorf("换了 key 应从头计时，实际持续 %v（换之前是 %v）", changed, second)
	}

	time.Sleep(50 * time.Millisecond)
	if err := s.ClearArkAuthFailure(ctx, env); err != nil {
		t.Fatalf("清记录：%v", err)
	}
	if err := s.ClearArkAuthFailure(ctx, env); err != nil {
		t.Fatalf("清不存在的记录不算错：%v", err)
	}
	after, err := s.RecordArkAuthFailure(ctx, env, ws, "len=46 sha=bbbb2222")
	if err != nil {
		t.Fatalf("清后记失败：%v", err)
	}
	if after >= 40*time.Millisecond {
		t.Errorf("清掉之后再失败应从头计时，实际 %v", after)
	}
}
