package core

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/gopkg/arkframe"
)

// CloseRunAt settles a run at the caller-supplied moment. A reclaim path must pass
// its last activity rather than the later discovery time. Negative durations are zero.
func CloseRunAt(run *apimodel.Run, at time.Time) {
	run.EndedAt = apimodel.FormatISO(at)
	if started, err := time.Parse("2006-01-02T15:04:05.000Z", run.StartedAt); err == nil {
		ms := at.Sub(started).Milliseconds()
		if ms < 0 {
			ms = 0
		}
		run.DurationMs = &ms
	}
}

// TouchRunThread updates one thread's status without duplicating it. Agent names
// come from the event when present, otherwise the run's current name is retained.
func TouchRunThread(run *apimodel.Run, event apimodel.RunEvent, now time.Time) {
	if event.ThreadID == nil || *event.ThreadID == "" {
		return
	}
	status := "running"
	switch event.Type {
	case "session.thread_status_idle":
		status = "idle"
	case "session.thread_status_terminated":
		status = "terminated"
	}
	for i := range run.Threads {
		if run.Threads[i].ID == *event.ThreadID {
			run.Threads[i].Status = status
			if status != "running" && run.Threads[i].EndedAt == "" {
				run.Threads[i].EndedAt = apimodel.FormatISO(now)
			}
			return
		}
	}
	run.Threads = append(run.Threads, apimodel.RunThread{
		ID: *event.ThreadID, AgentName: agentNameOf(event.Raw, run.AgentName), Status: status,
		ParentThreadID: nil, StartedAt: apimodel.FormatISO(now),
	})
}

// IsInterruptOnly reports whether a batch contains only user interruption events.
func IsInterruptOnly(raw []json.RawMessage) bool {
	for _, item := range raw {
		if arkframe.Type(item) != "user.interrupt" {
			return false
		}
	}
	return len(raw) > 0
}

// TruncateTitle flattens whitespace and truncates to 30 runes, matching the UI.
func TruncateTitle(text string) string {
	oneLine := strings.Join(strings.Fields(text), " ")
	runes := []rune(oneLine)
	if len(runes) > 30 {
		return string(runes[:30]) + "…"
	}
	return oneLine
}
