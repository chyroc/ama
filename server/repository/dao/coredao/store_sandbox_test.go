package coredao

import (
	"context"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/persistmodel"
)

// testRunSandboxActivity pins three rules for container activity time (both implementations run it):
//
//  1. When no activity was ever recorded, fall back to created_at; the reclaim sweep uses it to judge "idle for how long" beyond orphans;
//  2. After Touch, the newer time is returned;
//  3. A Run with no record does not appear in the result, and Touching it is not an error (plain-text chats have no container).
func testRunSandboxActivity(t *testing.T, newStore storeFactory) {
	ctx := context.Background()
	s := newStore(t, false)
	created := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Millisecond)
	if err := s.PutRunSandbox(ctx, persistmodel.RunSandbox{
		RunID: "sesn-act", SessionID: "ama-sess-sesn-act", SandboxID: "daemon-A", CreatedAt: created,
	}); err != nil {
		t.Fatalf("写容器记录: %v", err)
	}

	got, err := s.RunSandboxActivity(ctx, []string{"sesn-act", "sesn-missing"})
	if err != nil {
		t.Fatalf("查活动时刻: %v", err)
	}
	if _, ok := got["sesn-missing"]; ok {
		t.Error("没有记录的 Run 不该出现在结果里")
	}
	if at := got["sesn-act"]; !at.Equal(created) {
		t.Errorf("没记过活动时应退到 created_at=%v，实际 %v", created, at)
	}

	if err := s.TouchRunSandboxActivity(ctx, "sesn-act"); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if err := s.TouchRunSandboxActivity(ctx, "sesn-missing"); err != nil {
		t.Fatalf("Touch 不存在的记录不算错: %v", err)
	}
	got, err = s.RunSandboxActivity(ctx, []string{"sesn-act"})
	if err != nil {
		t.Fatalf("查活动时刻: %v", err)
	}
	if at := got["sesn-act"]; time.Since(at) > time.Minute {
		t.Errorf("Touch 之后应是刚才的时刻，实际 %v", at)
	}

	empty, err := s.RunSandboxActivity(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("空入参应回空结果，实际 %v %v", empty, err)
	}
}
