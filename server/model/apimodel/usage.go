// Public shape of usage statistics.

package apimodel

// UsageSummary is the usage summary of one workspace.
type UsageSummary struct {
	TotalRuns int `json:"totalRuns"`
	// FailedRuns is the number of runs with a non-empty ErrorSummary.
	FailedRuns int `json:"failedRuns"`
	// CompletedRuns is the number of finished runs (DurationMs non-nil), i.e. the sample size for duration stats.
	CompletedRuns int `json:"completedRuns"`
	// Usage is the four token counts summed over the whole workspace.
	Usage TokenUsage `json:"usage"`
	// P50Ms / P90Ms / AvgMs are workspace-wide duration percentiles, over finished runs only; 0 when there are no samples.
	P50Ms int64 `json:"p50Ms"`
	P90Ms int64 `json:"p90Ms"`
	AvgMs int64 `json:"avgMs"`
	// Agents is sorted by call count descending; ties are sorted by agent name ascending (keeps the order stable).
	Agents []AgentUsage `json:"agents"`
	// Daily is sorted by date ascending, so the frontend can draw the trend directly.
	Daily []DailyUsage `json:"daily"`
}

// AgentUsage is the usage of one agent.
//
// AgentName comes from the snapshot on the run, not a fresh lookup in the agent table: agents are hard-deleted,
// and after deletion these past runs must still be counted under "it", otherwise the usage table gets an extra row with no name.
type AgentUsage struct {
	AgentID   string `json:"agentId"`
	AgentName string `json:"agentName"`
	// AgentDeleted means this agent no longer exists; the frontend uses it to show a "deleted" mark.
	AgentDeleted  bool       `json:"agentDeleted"`
	Runs          int        `json:"runs"`
	FailedRuns    int        `json:"failedRuns"`
	CompletedRuns int        `json:"completedRuns"`
	Usage         TokenUsage `json:"usage"`
	P50Ms         int64      `json:"p50Ms"`
	P90Ms         int64      `json:"p90Ms"`
	AvgMs         int64      `json:"avgMs"`
}

// DailyUsage is the usage of one day; Date looks like 2026-08-25.
type DailyUsage struct {
	Date  string     `json:"date"`
	Runs  int        `json:"runs"`
	Usage TokenUsage `json:"usage"`
}
