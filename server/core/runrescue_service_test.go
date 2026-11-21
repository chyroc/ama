package core

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

func TestRunStoppedWorkRescueChecksRemoteState(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		found       bool
		audit       int32
	}{
		{name: "stopped", state: ArkWorkStateStopped, found: true, audit: 1},
		{name: "active", state: ArkWorkStateActive, found: true},
		{name: "missing", found: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var audits atomic.Int32
			run := apimodel.Run{ID: "sesn-example", WorkspaceID: "ws_example", AgentID: "agent_example", Status: apimodel.RunRequiresAction}
			RunStoppedWorkRescue(context.Background(), run, RescueAfter,
				slog.New(slog.NewTextHandler(io.Discard, nil)), RunRescueHooks{
					GetBinding: func(context.Context, apimodel.Run) (persistmodel.ArkBinding, error) {
						return persistmodel.ArkBinding{ArkEnvironmentID: "env_example"}, nil
					},
					GetKey: func(context.Context, apimodel.Run) (ArkBindingKey, error) {
						return ArkBindingKey{APIKey: "test-key"}, nil
					},
					BaseURL: func(apimodel.ArkRegion) (string, error) { return "https://example.com", nil },
					FetchWorkState: func(context.Context, string, string, string, string) (string, bool, error) {
						return tc.state, tc.found, nil
					},
					Audit: func(context.Context, string, string, *slog.Logger) int {
						audits.Add(1)
						return 0
					},
				})
			if got := audits.Load(); got != tc.audit {
				t.Fatalf("audit calls = %d, want %d", got, tc.audit)
			}
		})
	}
}
