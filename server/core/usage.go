// Workspace usage aggregation.
//
// It answers "over a period of time, who is spending money, who is getting slower, who is
// failing", for the frontend /w/:key/usage page. The few numbers on the overview page are
// still reduced in the frontend itself (it only needs totals); this file expands them per agent
// and per day, which done in the frontend would mean pulling every run into the browser.
//
// # Three counting rules; read them before changing anything
//
// 1. **Latency percentiles only count finished runs**. DurationMs is *int64; while a run is not
//    finished it is **missing**, not 0 (see the comment on apimodel.Run). Putting nil into the
//    samples as 0 drags P50 down to the floor with a pile of zeros, and the page shows "the
//    agent suddenly got faster", a false statement nobody can act on. So samples only take
//    DurationMs != nil, and the sample size is reported to the frontend as is (CompletedRuns),
//    so it can label n.
//
// 2. **Percentiles use nearest-rank, no interpolation**. The sample size is often a single
//    digit (an agent running three times is normal), and a linearly interpolated "P90 = some
//    non-existent number between two real values" is noise at this scale.
//    nearest-rank guarantees every reported number is a duration that really happened once.
//
// 3. **The failure rule is ErrorSummary != "", not Status**. RunStatus has no "failed" value
//    (terminated covers both normal finish and errors), and the frontend OverviewPage judges it
//    the same way (runs.filter(r => r.errorSummary)). Switching to Status would change the
//    success rate out of nowhere.
//
// Numeric fields never use omitempty: 0 is a valid value, and a missing key makes the frontend
// compute NaN. Same for slices: nil encodes as null, but the contract requires [].

package core

import (
	"context"
	"sort"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// UsageQuery (the time window) is defined in store.go, next to RunQuery, because it is a Store input.

// WorkspaceUsage aggregates usage for one workspace.
//
// **A workspace that does not exist still returns 200 + an all-zero summary**, not ErrNotFound:
// the same rule as listRuns / listSkills (core/README.md). The frontend turns not_found into
// null, and the page would become a silent empty state.
//
// Read-only: never write aggregated results or derived fields back via SaveRun. That would
// freeze derived values into doc, with no symptom at all.
func (s *Service) WorkspaceUsage(ctx context.Context, workspaceID string, q persistmodel.UsageQuery) (UsageSummary, error) {
	runs, err := s.store.ListRunsForUsage(ctx, workspaceID, q)
	if err != nil {
		return UsageSummary{}, err
	}

	// Fetch the agents once to build a set, instead of calling GetAgent per row (N+1 on a
	// list path), same as ListRuns.
	alive := make(map[string]struct{})
	agents, err := s.store.ListAgents(ctx, workspaceID)
	if err != nil {
		// fail-open: a DB hiccup should not look like "all your agents were deleted". The only
		// cost is one missing marker.
		s.log().Warn("列 agent 失败，用量看板暂不标记已删除", "workspace", workspaceID, "error", err.Error())
		alive = nil
	} else {
		for _, a := range agents {
			alive[a.ID] = struct{}{}
		}
	}

	sum := UsageSummary{
		TotalRuns: len(runs),
		Agents:    make([]AgentUsage, 0),
		Daily:     make([]DailyUsage, 0),
	}

	// Aggregate one copy per agent and one per day. durations collects latency samples for the
	// whole workspace.
	perAgent := make(map[string]*AgentUsage)
	agentDurations := make(map[string][]int64)
	perDay := make(map[string]*DailyUsage)
	durations := make([]int64, 0, len(runs))

	for _, r := range runs {
		failed := r.ErrorSummary != ""
		if failed {
			sum.FailedRuns++
		}
		addUsageTotals(&sum.Usage, r.Usage)

		a := perAgent[r.AgentID]
		if a == nil {
			a = &AgentUsage{AgentID: r.AgentID, AgentName: r.AgentName}
			// alive being nil means listing agents failed; then mark none (fail-open).
			if alive != nil {
				if _, ok := alive[r.AgentID]; !ok {
					a.AgentDeleted = true
				}
			}
			perAgent[r.AgentID] = a
		}
		a.Runs++
		if failed {
			a.FailedRuns++
		}
		addUsageTotals(&a.Usage, r.Usage)

		// The date is just the first 10 characters of the ISO string. The format is fixed-length,
		// so slicing is less error-prone than time.Parse then Format, and it brings in no time
		// zone: StartedAt is UTC, so buckets should be UTC days.
		if len(r.StartedAt) >= 10 {
			day := r.StartedAt[:10]
			d := perDay[day]
			if d == nil {
				d = &DailyUsage{Date: day}
				perDay[day] = d
			}
			d.Runs++
			addUsageTotals(&d.Usage, r.Usage)
		}

		// Only finished runs go into latency samples; see item 1 in the file header.
		if r.DurationMs != nil {
			durations = append(durations, *r.DurationMs)
			agentDurations[r.AgentID] = append(agentDurations[r.AgentID], *r.DurationMs)
			sum.CompletedRuns++
			a.CompletedRuns++
		}
	}

	sum.P50Ms, sum.P90Ms, sum.AvgMs = latencyStats(durations)

	for id, a := range perAgent {
		a.P50Ms, a.P90Ms, a.AvgMs = latencyStats(agentDurations[id])
		sum.Agents = append(sum.Agents, *a)
	}
	// Most-called first; on equal counts sort by name ascending, so the order stays stable
	// despite map iteration (otherwise the same data gives a different order on every request,
	// and the frontend chart jumps).
	sort.Slice(sum.Agents, func(i, j int) bool {
		if sum.Agents[i].Runs != sum.Agents[j].Runs {
			return sum.Agents[i].Runs > sum.Agents[j].Runs
		}
		return sum.Agents[i].AgentName < sum.Agents[j].AgentName
	})

	for _, d := range perDay {
		sum.Daily = append(sum.Daily, *d)
	}
	// The trend chart goes left to right in time, so this is **ascending** (the opposite of the
	// descending run list).
	sort.Slice(sum.Daily, func(i, j int) bool {
		return sum.Daily[i].Date < sum.Daily[j].Date
	})

	return sum, nil
}

// addUsageTotals adds a run's four token counts into the summary.
//
// All four must be added: the frontend sums them and computes shares, so a missing one is NaN.
func addUsageTotals(sum *apimodel.TokenUsage, u apimodel.TokenUsage) {
	sum.InputTokens += u.InputTokens
	sum.OutputTokens += u.OutputTokens
	sum.CacheReadInputTokens += u.CacheReadInputTokens
	sum.CacheCreationInputTokens += u.CacheCreationInputTokens
}

// latencyStats computes P50 / P90 / average, in milliseconds.
//
// Percentiles use nearest-rank (no interpolation); see item 2 in the file header for why: the
// sample size is often a single digit, and interpolated numbers never happened in reality.
// **It sorts the passed-in slice in place**; callers always pass a temporary slice used only by
// this function.
//
// Empty samples return three zeros. The caller reports CompletedRuns separately, and the
// frontend uses it to tell "fast" from "no data".
func latencyStats(samples []int64) (p50, p90, avg int64) {
	n := len(samples)
	if n == 0 {
		return 0, 0, 0
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })

	var total int64
	for _, v := range samples {
		total += v
	}
	// Integer division just truncates: a millisecond average needs no fraction, and truncating
	// saves one float round trip compared with rounding.
	avg = total / int64(n)

	return samples[rankIndex(n, 50)], samples[rankIndex(n, 90)], avg
}

// rankIndex gives the index of the nearest-rank percentile in samples **already sorted
// ascending**.
//
// It computes ceil(pct/100 * n) - 1 with integers, so float error does not shift P50 by one
// slot at a boundary (0.5*6 in floating point may be 2.9999…, and ceil gives 3 rather than
// 3 and then minus one).
// For n>=1 the result always falls in [0, n-1].
func rankIndex(n, pct int) int {
	idx := (n*pct + 99) / 100 // ceil(n*pct/100)
	if idx < 1 {
		idx = 1
	}
	if idx > n {
		idx = n
	}
	return idx - 1
}
