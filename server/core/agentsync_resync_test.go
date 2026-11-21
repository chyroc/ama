package core

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/persistmodel"
)

type resyncStoreProbe struct {
	current persistmodel.AgentSyncTaskRow
	err     error
	requeue int
}

func (s *resyncStoreProbe) LatestAgentSyncTask(context.Context, string) (persistmodel.AgentSyncTaskRow, error) {
	return s.current, s.err
}
func (s *resyncStoreProbe) RequeueAgentSyncTask(context.Context, string, time.Time) error {
	s.requeue++
	return s.err
}

func TestRequeueAgentSyncIfRequestedRereadsLatestTask(t *testing.T) {
	task := persistmodel.AgentSyncTaskRow{ID: "task_old", AgentID: "agent_example"}
	store := &resyncStoreProbe{current: persistmodel.AgentSyncTaskRow{
		ID: "task_old", AgentID: "agent_example", ResyncRequested: true,
	}}
	if !RequeueAgentSyncIfRequested(context.Background(), store, task, time.Now(), slog.Default()) {
		t.Fatal("a newer save must be queued even when the claimed task looks successful")
	}
	if store.requeue != 1 {
		t.Fatalf("requeue calls = %d, want 1", store.requeue)
	}
}

func TestRequeueAgentSyncIfRequestedLeavesFailureToLease(t *testing.T) {
	task := persistmodel.AgentSyncTaskRow{ID: "task_old", AgentID: "agent_example"}
	store := &resyncStoreProbe{err: errors.New("store unavailable"), current: task}
	if RequeueAgentSyncIfRequested(context.Background(), store, task, time.Now(), slog.Default()) {
		t.Fatal("a failed read must not manufacture a successful requeue")
	}
}
