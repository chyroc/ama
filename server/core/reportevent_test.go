package core

import (
	"context"
	"testing"
)

func TestReportPersistedEventRecoversAndKeepsRunContext(t *testing.T) {
	called := 0
	ReportPersistedEvent(context.Background(), "run_example", "event_example", "agent.message", 1,
		[]byte(`{"processed_at":"2026-01-01T00:00:00Z"}`), ReportEventHooks{
			LoadRun: func(context.Context, string) (RunTrace, bool, error) {
				return RunTrace{RunID: "run_example", AgentID: "agent_example"}, true, nil
			},
			AgentName: func(context.Context, string) string { return "Example Agent" },
			LogID:     func(context.Context) string { return "request_example" },
			Report: func(run RunTrace, event EventTrace) {
				called++
				if run.AgentName != "Example Agent" || run.LogID != "request_example" || event.ProcessedAt.IsZero() {
					t.Fatalf("incomplete trace context: run=%#v event=%#v", run, event)
				}
			},
		})
	if called != 1 {
		t.Fatalf("Report calls = %d, want 1", called)
	}
	ReportPersistedEvent(context.Background(), "run_example", "event_example", "agent.message", 1, nil,
		ReportEventHooks{
			LoadRun: func(context.Context, string) (RunTrace, bool, error) {
				return RunTrace{}, true, nil
			},
			Report: func(RunTrace, EventTrace) { panic("optional reporter failed") },
		})
}

func TestEventProcessedAtInvalidStaysZero(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{}`), []byte(`{"processed_at":null}`), []byte(`{"processed_at":"invalid"}`), []byte(`invalid`),
	} {
		if got := EventProcessedAt(raw); !got.IsZero() {
			t.Errorf("EventProcessedAt(%s) = %v, want zero", raw, got)
		}
	}
}
