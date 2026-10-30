package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
)

// TestShouldRescue pins the three joint conditions for a rescue. The most dangerous regression is
// "resend just because it went silent": that collides with a long tool still running (up to
// 120s), producing two results for the same call, which is worse than being stuck.
func TestShouldRescue(t *testing.T) {
	old := rescueAfter
	cases := []struct {
		name      string
		status    apimodel.RunStatus
		age       time.Duration
		workState string
		found     bool
		want      bool
	}{
		{"三条件全满足", apimodel.RunRequiresAction, old + time.Second, arkWorkStateStopped, true, true},
		{"age 恰好等于阈值也算（>=）", apimodel.RunRequiresAction, old, arkWorkStateStopped, true, true},
		{"静默不够久：reclaim 还有机会，不插手", apimodel.RunRequiresAction, old - time.Second, arkWorkStateStopped, true, false},
		{"work 还 active：有 worker 在跑，绝不补发", apimodel.RunRequiresAction, old + time.Second, arkWorkStateActive, true, false},
		{"work 还 queued：即将被领，不补发", apimodel.RunRequiresAction, old + time.Second, arkWorkStateQueued, true, false},
		{"work stopping：正在收尾，不补发", apimodel.RunRequiresAction, old + time.Second, arkWorkStateStopping, true, false},
		{"work 查无此物（404）：不补发", apimodel.RunRequiresAction, old + time.Second, "", false, false},
		{"run 还在 running：还没轮到工具", apimodel.RunRunning, old + time.Second, arkWorkStateStopped, true, false},
		{"run 已 idle：这一轮结束了", apimodel.RunIdle, old + time.Second, arkWorkStateStopped, true, false},
		{"run 已 terminated：终态", apimodel.RunTerminated, old + time.Second, arkWorkStateStopped, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRescue(tc.status, tc.age, tc.workState, tc.found); got != tc.want {
				t.Errorf("shouldRescue(status=%s age=%v work=%q found=%v) = %v，要 %v",
					tc.status, tc.age, tc.workState, tc.found, got, tc.want)
			}
		})
	}
}

// TestRescueAfterExceedsServerStopLine ensures the rescue threshold is always later than Ark's
// 90-second stop line. Equal or earlier would resend a "not executed" result first while the work
// may still really run via reclaim.
func TestRescueAfterExceedsServerStopLine(t *testing.T) {
	if rescueAfter <= arkWorkServerStopAfter {
		t.Fatalf("rescueAfter %v 必须晚于 arkWorkServerStopAfter %v，"+
			"否则会在 reclaim 还能真跑的时候抢先补发错误结果", rescueAfter, arkWorkServerStopAfter)
	}
}

// TestRescueNeedsHuman pins the alert levels: the first rescue is a normal fallback; only from the
// second one on is it "the self-heal chain is broken, a human must step in". When the counter store
// is unavailable, never escalate (avoids false incidents).
func TestRescueNeedsHuman(t *testing.T) {
	cases := []struct {
		name        string
		n           int
		countFailed bool
		want        bool
	}{
		{"第 1 次：正常兜底", 1, false, false},
		{"第 2 次：升级要人", 2, false, true},
		{"第 7 次：持续要人", 7, false, true},
		{"计数失败时第 2 次也不升级", 2, true, false},
		{"计数失败时第 100 次也不升级", 100, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rescueNeedsHuman(tc.n, tc.countFailed); got != tc.want {
				t.Errorf("rescueNeedsHuman(%d, %v) = %v，要 %v",
					tc.n, tc.countFailed, got, tc.want)
			}
		})
	}
}

// TestIncrRunRescueCount: the counter must increment and read the new value, accumulate across
// calls, and return ErrNotFound when the run does not exist. It is the distributed basis for the
// "self-heal chain broken" escalation alert; if it is wrong we either miss alerts or raise false ones.
func TestIncrRunRescueCount(t *testing.T) {
	svc, _, _ := newChatService(t)
	ctx := context.Background()
	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}
	for i, want := range []int{1, 2, 3} {
		var got int
		got, err = svc.store.IncrRunRescueCount(ctx, run.ID)
		if err != nil {
			t.Fatalf("第 %d 次自增失败：%v", i+1, err)
		}
		if got != want {
			t.Errorf("第 %d 次自增应得 %d，实际 %d", i+1, want, got)
		}
	}
	if _, err := svc.store.IncrRunRescueCount(ctx, "sesn-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Errorf("run 不存在应报 ErrNotFound，实际 %v", err)
	}
}
