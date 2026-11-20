package core

import (
	"context"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

type usageOnlyStore struct {
	runs   []apimodel.Run
	agents []apimodel.Agent
}

func (s usageOnlyStore) ListRunsForUsage(context.Context, string, persistmodel.UsageQuery) ([]apimodel.Run, error) {
	return s.runs, nil
}
func (s usageOnlyStore) ListAgents(context.Context, string) ([]apimodel.Agent, error) {
	return s.agents, nil
}

func TestUsageServiceUsesOneAggregationDefinition(t *testing.T) {
	ms := func(v int64) *int64 { return &v }
	store := usageOnlyStore{runs: []apimodel.Run{
		{AgentID: "agent_a", AgentName: "A", DurationMs: ms(30), StartedAt: "2026-11-20T00:00:00.000Z"},
		{AgentID: "agent_a", AgentName: "A", DurationMs: ms(10), StartedAt: "2026-11-21T00:00:00.000Z"},
		{AgentID: "agent_deleted", AgentName: "Deleted", DurationMs: ms(20), StartedAt: "2026-11-21T01:00:00.000Z", ErrorSummary: "failed"},
	}}
	service := NewUsageService(store, nil)
	summary, err := service.WorkspaceUsage(context.Background(), "ws_test", persistmodel.UsageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.TotalRuns != 3 || summary.CompletedRuns != 3 || summary.FailedRuns != 1 {
		t.Fatalf("unexpected totals: %+v", summary)
	}
	if summary.P50Ms != 20 || summary.P90Ms != 30 || summary.AvgMs != 20 {
		t.Fatalf("unexpected nearest-rank stats: %+v", summary)
	}
	if len(summary.Daily) != 2 || summary.Daily[0].Date != "2026-11-20" {
		t.Fatalf("daily aggregation changed: %+v", summary.Daily)
	}
	var deleted *AgentUsage
	for i := range summary.Agents {
		if summary.Agents[i].AgentID == "agent_deleted" {
			deleted = &summary.Agents[i]
		}
	}
	if deleted == nil || !deleted.AgentDeleted {
		t.Fatalf("missing deleted agent marker: %+v", summary.Agents)
	}
}
