package core

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// RunTrace is the minimum run context needed by an optional event reporter.
// Provider-specific trace IDs and clients stay outside this package.
type RunTrace struct {
	RunID       string
	WorkspaceID string
	AgentID     string
	AgentName   string
	Trigger     string
	Title       string
	LogID       string
}

// EventTrace is the minimum event data needed to build a provider-specific span.
type EventTrace struct {
	ID          string
	Type        string
	Sequence    int
	ProcessedAt time.Time
	Raw         []byte
}

// ReportEventHooks loads context only after the caller has established that this event
// was newly persisted. Duplicate deliveries must not call Report.
type ReportEventHooks struct {
	LoadRun   func(context.Context, string) (RunTrace, bool, error)
	AgentName func(context.Context, string) string
	LogID     func(context.Context) string
	Report    func(RunTrace, EventTrace)
	Logger    *slog.Logger
}

// ReportPersistedEvent keeps optional observation failures out of the conversation path.
// A missing run is not reported with fabricated dimensions. A reporter panic is logged
// and ignored; it must never interrupt a user-visible response stream.
func ReportPersistedEvent(
	ctx context.Context, runID, eventID, eventType string, sequence int, raw []byte,
	hooks ReportEventHooks,
) {
	if hooks.Report == nil {
		return
	}
	logger := hooks.Logger
	if logger == nil {
		logger = slog.Default()
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.Error("观测上报 panic（已忽略，对话不受影响）", "run", runID, "panic", recovered)
		}
	}()
	if hooks.LoadRun == nil {
		return
	}
	run, ok, err := hooks.LoadRun(ctx, runID)
	if err != nil || !ok {
		return
	}
	if hooks.AgentName != nil {
		run.AgentName = hooks.AgentName(ctx, run.AgentID)
	}
	if hooks.LogID != nil {
		run.LogID = hooks.LogID(ctx)
	}
	hooks.Report(run, EventTrace{
		ID: eventID, Type: eventType, Sequence: sequence,
		ProcessedAt: EventProcessedAt(raw), Raw: raw,
	})
}

// EventProcessedAt returns zero for absent, null or invalid processed_at. An unprocessed
// event must not be assigned time.Now(), which would silently falsify span order and duration.
func EventProcessedAt(raw []byte) time.Time {
	var probe struct {
		ProcessedAt *string `json:"processed_at"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.ProcessedAt == nil {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, *probe.ProcessedAt)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
