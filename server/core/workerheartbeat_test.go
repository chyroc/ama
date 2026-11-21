package core

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

type heartbeatProbeAPI struct {
	selfhosted.API
	err error
}

func (a heartbeatProbeAPI) HeartbeatWork(context.Context, selfhosted.HeartbeatWorkRequest) (*selfhosted.HeartbeatResponse, error) {
	return nil, a.err
}

func TestRunWorkerHeartbeatStopsAfterPermanentFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var cause atomic.Value
	RunWorkerHeartbeat(ctx, selfhosted.WorkItem{ID: "work_example"},
		heartbeatProbeAPI{err: &selfhosted.APIError{StatusCode: 400}}, cancel,
		func(value string) { cause.Store(value) })
	if got := cause.Load(); got != HeartbeatCausePermanentFailure {
		t.Fatalf("unexpected stop cause: %v", got)
	}
	if ctx.Err() == nil {
		t.Fatal("permanent heartbeat failure must cancel the work")
	}
}

func TestIsFatalWorker4xxLeavesRetriesAlone(t *testing.T) {
	for _, code := range []int{408, 409, 429, 412, 500} {
		if IsFatalWorker4xx(&selfhosted.APIError{StatusCode: code}) {
			t.Errorf("status %d must not permanently stop a work item", code)
		}
	}
	if !IsFatalWorker4xx(&selfhosted.APIError{StatusCode: 400}) {
		t.Fatal("a malformed work request is permanent")
	}
}
