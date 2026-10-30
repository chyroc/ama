// Tests for usage aggregation: one invariant per test.
//
// Every test here is the kind where "a naive implementation gets it wrong, and when it's wrong the
// page shows no error, the numbers just quietly change". Especially the one about counting unfinished
// runs as 0 ms in the percentile samples: it drags P50 all the way down to 0, which looks like "the
// agent got faster" rather than "the data was computed wrong".

package core

import (
	"context"
	"testing"

	"github.com/chyroc/ama/server/repository/dao/coremem"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// usageRun builds a run for aggregation. A negative durationMs means "unfinished" (DurationMs is nil).
func usageRun(id, agentID, agentName, startedAt string, durationMs int64, errSummary string, in, out int) apimodel.Run {
	r := apimodel.Run{
		ID:          id,
		WorkspaceID: "ws_usage",
		AgentID:     agentID,
		AgentName:   agentName,
		Status:      apimodel.RunIdle,
		Trigger:     triggerManual,
		StartedAt:   startedAt,
		Usage: apimodel.TokenUsage{
			InputTokens:              in,
			OutputTokens:             out,
			CacheReadInputTokens:     in / 2,
			CacheCreationInputTokens: in / 4,
		},
		Threads:      []apimodel.RunThread{},
		ErrorSummary: errSummary,
	}
	if durationMs >= 0 {
		d := durationMs
		r.DurationMs = &d
	}
	return r
}

// newUsageService starts a clean Service with only the given runs (no seed, so seed data doesn't skew the numbers).
func newUsageService(t *testing.T, runs ...apimodel.Run) *Service {
	t.Helper()
	store := coremem.NewMemStore()
	svc, err := New(Config{Store: store})
	if err != nil {
		t.Fatalf("装配 Service 失败：%v", err)
	}
	ctx := context.Background()
	for _, r := range runs {
		if _, err := store.CreateRun(ctx, r); err != nil {
			t.Fatalf("造 run 失败：%v", err)
		}
	}
	return svc
}

// TestUsageExcludesUnfinishedRunsFromLatency is the most important test in this file.
//
// On an unfinished run DurationMs is **missing**, not 0. If nil is counted as 0 in the samples, P50
// gets dragged to the bottom by a pile of zeros: with three 1000ms runs plus two running ones, P50
// goes from 1000 to 0, and the page shows "this agent is unbelievably fast", a false statement the
// user can't act on.
func TestUsageExcludesUnfinishedRunsFromLatency(t *testing.T) {
	ctx := context.Background()
	svc := newUsageService(t,
		usageRun("r1", "a1", "甲", "2026-08-25T10:00:00.000Z", 1000, "", 100, 10),
		usageRun("r2", "a1", "甲", "2026-08-25T11:00:00.000Z", 1000, "", 100, 10),
		usageRun("r3", "a1", "甲", "2026-08-25T12:00:00.000Z", 1000, "", 100, 10),
		// Two running ones: they count toward TotalRuns and the token totals, but not the duration samples.
		usageRun("r4", "a1", "甲", "2026-08-25T13:00:00.000Z", -1, "", 100, 10),
		usageRun("r5", "a1", "甲", "2026-08-25T14:00:00.000Z", -1, "", 100, 10),
	)

	sum, err := svc.WorkspaceUsage(ctx, "ws_usage", persistmodel.UsageQuery{})
	if err != nil {
		t.Fatalf("聚合失败：%v", err)
	}

	if sum.TotalRuns != 5 {
		t.Errorf("TotalRuns 应含未结束的，期望 5，实际 %d", sum.TotalRuns)
	}
	if sum.CompletedRuns != 3 {
		t.Errorf("CompletedRuns 只数已结束的，期望 3，实际 %d", sum.CompletedRuns)
	}
	// All three samples are 1000, so all three statistics must be 1000. If any of them is 0, it means
	// nil was counted as 0.
	if sum.P50Ms != 1000 || sum.P90Ms != 1000 || sum.AvgMs != 1000 {
		t.Errorf("未结束的 run 不该进耗时样本，期望 1000/1000/1000，实际 %d/%d/%d",
			sum.P50Ms, sum.P90Ms, sum.AvgMs)
	}
	// The token totals must include the two unfinished runs: they have already used tokens.
	if sum.Usage.InputTokens != 500 {
		t.Errorf("token 汇总应含未结束的 run，期望 500，实际 %d", sum.Usage.InputTokens)
	}
	if len(sum.Agents) != 1 || sum.Agents[0].CompletedRuns != 3 || sum.Agents[0].Runs != 5 {
		t.Errorf("按 agent 的两个计数也要分开，实际 %#v", sum.Agents)
	}
}

// TestUsageLatencyNearestRank pins that percentiles use nearest-rank, not interpolation.
//
// Sample sizes are often single digits, and an interpolated "P90" is a number between two real
// durations that never happened.
func TestUsageLatencyNearestRank(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name                      string
		durations                 []int64
		wantP50, wantP90, wantAvg int64
	}{
		// One sample: all three statistics are that sample itself.
		{"n=1", []int64{500}, 500, 500, 500},
		// Two samples: ceil(2*0.5)=1 → index 0; ceil(2*0.9)=2 → index 1.
		// An interpolating implementation would give P90=1900, a number not in the samples.
		{"n=2", []int64{1000, 2000}, 1000, 2000, 1500},
		// Even sample count (4): ceil(4*0.5)=2 → index 1 → 200.
		// "Average the middle two" would give 250, also not in the samples.
		{"n=4", []int64{100, 200, 300, 400}, 200, 400, 250},
		// Ten samples; P90 lands exactly on the 9th (index 8).
		{"n=10", []int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}, 50, 90, 55},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runs := make([]apimodel.Run, 0, len(tc.durations))
			for i, d := range tc.durations {
				// startedAt is deliberately in the reverse order of duration, to make sure the
				// implementation sorts before taking percentiles rather than getting it right by insertion order.
				started := "2026-08-25T1" + string(rune('0'+i%10)) + ":00:00.000Z"
				runs = append(runs, usageRun("r"+string(rune('a'+i)), "a1", "甲", started, d, "", 10, 1))
			}
			svc := newUsageService(t, runs...)
			sum, err := svc.WorkspaceUsage(ctx, "ws_usage", persistmodel.UsageQuery{})
			if err != nil {
				t.Fatalf("聚合失败：%v", err)
			}
			if sum.P50Ms != tc.wantP50 {
				t.Errorf("P50 期望 %d，实际 %d", tc.wantP50, sum.P50Ms)
			}
			if sum.P90Ms != tc.wantP90 {
				t.Errorf("P90 期望 %d，实际 %d", tc.wantP90, sum.P90Ms)
			}
			if sum.AvgMs != tc.wantAvg {
				t.Errorf("平均 期望 %d，实际 %d", tc.wantAvg, sum.AvgMs)
			}
		})
	}
}

// TestUsageEmptyWorkspaceIsAllZero: a workspace that doesn't exist must still return 200 + all zeros,
// and slices must be [] not null.
//
// Returning ErrNotFound would be turned into null by the frontend, and the page becomes a silent
// empty state; a nil slice encodes as null, and the frontend's .map() blows up.
func TestUsageEmptyWorkspaceIsAllZero(t *testing.T) {
	ctx := context.Background()
	svc := newUsageService(t)

	sum, err := svc.WorkspaceUsage(ctx, "ws_does_not_exist", persistmodel.UsageQuery{})
	if err != nil {
		t.Fatalf("不存在的空间不该报错，实际 %v", err)
	}
	if sum.TotalRuns != 0 || sum.FailedRuns != 0 || sum.CompletedRuns != 0 {
		t.Errorf("空空间的计数都该是 0，实际 %#v", sum)
	}
	if sum.P50Ms != 0 || sum.P90Ms != 0 || sum.AvgMs != 0 {
		t.Errorf("无样本时分位都该是 0，实际 %d/%d/%d", sum.P50Ms, sum.P90Ms, sum.AvgMs)
	}
	if sum.Agents == nil {
		t.Error("Agents 必须是空切片而不是 nil（nil 编码成 null，前端 .map() 会炸）")
	}
	if sum.Daily == nil {
		t.Error("Daily 必须是空切片而不是 nil")
	}
}

// TestUsageFailureIsErrorSummaryNotStatus: failure is judged by ErrorSummary, not Status.
//
// RunStatus has no "failed" value: terminated covers both normal endings and errors. The frontend
// OverviewPage checks errorSummary; both sides must count the same way, or the success rate changes
// out of nowhere.
func TestUsageFailureIsErrorSummaryNotStatus(t *testing.T) {
	ctx := context.Background()
	terminatedOK := usageRun("r1", "a1", "甲", "2026-08-25T10:00:00.000Z", 100, "", 10, 1)
	// terminated with no error: a normal ending, not a failure.
	terminatedOK.Status = apimodel.RunTerminated
	withErr := usageRun("r2", "a1", "甲", "2026-08-25T11:00:00.000Z", 100, "工具执行超时", 10, 1)
	// An error occurred but the status is still idle: still counts as a failure.
	withErr.Status = apimodel.RunIdle

	svc := newUsageService(t, terminatedOK, withErr)
	sum, err := svc.WorkspaceUsage(ctx, "ws_usage", persistmodel.UsageQuery{})
	if err != nil {
		t.Fatalf("聚合失败：%v", err)
	}
	if sum.FailedRuns != 1 {
		t.Errorf("只有带 errorSummary 的算失败，期望 1，实际 %d", sum.FailedRuns)
	}
	if len(sum.Agents) != 1 || sum.Agents[0].FailedRuns != 1 {
		t.Errorf("按 agent 的失败数也要同口径，实际 %#v", sum.Agents)
	}
}

// TestUsageGroupsDeletedAgentByNameSnapshot: after an agent is deleted, its past runs still group
// under its name.
//
// Agents are hard-deleted. If names were filled in by looking up the agent table at query time,
// these runs would group under a row with no name; the usage table would get an extra row with an
// empty name "", and you couldn't tell whose it is.
func TestUsageGroupsDeletedAgentByNameSnapshot(t *testing.T) {
	ctx := context.Background()
	store := coremem.NewMemStore()
	svc, err := New(Config{Store: store})
	if err != nil {
		t.Fatalf("装配 Service 失败：%v", err)
	}

	// Only create the agent a_alive; a_gone never existed (same as having been hard-deleted).
	if _, err := store.CreateAgent(ctx, apimodel.Agent{
		ID: "a_alive", WorkspaceID: "ws_usage", Name: "活着的",
		SkillIDs: []string{}, FeishuAppIDs: []string{}, BuiltinTools: []string{},
	}); err != nil {
		t.Fatalf("造 agent 失败：%v", err)
	}
	for _, r := range []apimodel.Run{
		usageRun("r1", "a_alive", "活着的", "2026-08-25T10:00:00.000Z", 100, "", 10, 1),
		usageRun("r2", "a_gone", "已删掉的", "2026-08-25T11:00:00.000Z", 200, "", 20, 2),
	} {
		if _, err := store.CreateRun(ctx, r); err != nil {
			t.Fatalf("造 run 失败：%v", err)
		}
	}

	sum, err := svc.WorkspaceUsage(ctx, "ws_usage", persistmodel.UsageQuery{})
	if err != nil {
		t.Fatalf("聚合失败：%v", err)
	}
	if len(sum.Agents) != 2 {
		t.Fatalf("两个 agent 各一行，实际 %d 行：%#v", len(sum.Agents), sum.Agents)
	}
	byID := map[string]AgentUsage{}
	for _, a := range sum.Agents {
		byID[a.AgentID] = a
	}
	gone, ok := byID["a_gone"]
	if !ok {
		t.Fatal("已删除的 agent 也要有一行")
	}
	if gone.AgentName != "已删掉的" {
		t.Errorf("名字要用 run 上的快照，期望「已删掉的」，实际「%s」", gone.AgentName)
	}
	if !gone.AgentDeleted {
		t.Error("已删除的 agent 要标 AgentDeleted")
	}
	if byID["a_alive"].AgentDeleted {
		t.Error("还活着的 agent 不该被标成已删除")
	}
}

// TestUsageDailyBucketsAreHalfOpen: daily buckets have no overlap and no gaps.
//
// The per-day run counts must add up to the total. Closed intervals would put a run on the boundary
// into two adjacent buckets; both numbers look reasonable, so it can't be spotted from the symptoms.
func TestUsageDailyBucketsAreHalfOpen(t *testing.T) {
	ctx := context.Background()
	svc := newUsageService(t,
		usageRun("r1", "a1", "甲", "2026-08-25T00:00:00.000Z", 100, "", 10, 1),
		usageRun("r2", "a1", "甲", "2026-08-25T23:59:59.999Z", 100, "", 10, 1),
		usageRun("r3", "a1", "甲", "2026-08-26T00:00:00.000Z", 100, "", 10, 1),
		usageRun("r4", "a1", "甲", "2026-08-27T12:00:00.000Z", 100, "", 10, 1),
	)

	sum, err := svc.WorkspaceUsage(ctx, "ws_usage", persistmodel.UsageQuery{})
	if err != nil {
		t.Fatalf("聚合失败：%v", err)
	}
	if len(sum.Daily) != 3 {
		t.Fatalf("三天各一个桶，实际 %d：%#v", len(sum.Daily), sum.Daily)
	}
	// The trend chart goes left to right, so it is ascending.
	for i := 1; i < len(sum.Daily); i++ {
		if sum.Daily[i-1].Date >= sum.Daily[i].Date {
			t.Fatalf("Daily 必须按日期升序，实际 %#v", sum.Daily)
		}
	}
	total := 0
	for _, d := range sum.Daily {
		total += d.Runs
	}
	if total != sum.TotalRuns {
		t.Errorf("每天的 run 数加起来要等于总数（不重不漏），%d vs %d", total, sum.TotalRuns)
	}
	if sum.Daily[0].Date != "2026-08-25" || sum.Daily[0].Runs != 2 {
		t.Errorf("08-25 那天应有 2 条（含 23:59:59.999），实际 %#v", sum.Daily[0])
	}
}

// TestUsageAgentsSortedStably: the order of the per-agent rows must be stable.
//
// Map iteration order is random; without explicit sorting the same data would come back in a
// different order on each request, and the frontend chart would jump on every refresh.
func TestUsageAgentsSortedStably(t *testing.T) {
	ctx := context.Background()
	svc := newUsageService(t,
		usageRun("r1", "a_few", "少的", "2026-08-25T10:00:00.000Z", 100, "", 10, 1),
		usageRun("r2", "a_many", "多的", "2026-08-25T11:00:00.000Z", 100, "", 10, 1),
		usageRun("r3", "a_many", "多的", "2026-08-25T12:00:00.000Z", 100, "", 10, 1),
		// Same count (1) as a_few. Sorting is by **code point** ascending, not by pinyin:
		// the first char of a_tie's name is U+540C < a_few's U+5C11, so a_tie comes before a_few.
		// This test deliberately uses Chinese names to pin this down; an assertion written from
		// pinyin intuition would fail here.
		usageRun("r4", "a_tie", "同的", "2026-08-25T13:00:00.000Z", 100, "", 10, 1),
	)

	// Run it several times so the random map iteration order has a chance to show up.
	var first []string
	for i := 0; i < 5; i++ {
		sum, err := svc.WorkspaceUsage(ctx, "ws_usage", persistmodel.UsageQuery{})
		if err != nil {
			t.Fatalf("聚合失败：%v", err)
		}
		got := make([]string, 0, len(sum.Agents))
		for _, a := range sum.Agents {
			got = append(got, a.AgentID)
		}
		if first == nil {
			first = got
			continue
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("同样的数据两次排出不同顺序：%v vs %v", first, got)
			}
		}
	}
	// The most-called one comes first, the rest by name ascending
	if first[0] != "a_many" {
		t.Errorf("调用最多的应排最前，实际 %v", first)
	}
	if first[1] != "a_tie" || first[2] != "a_few" {
		t.Errorf("次数相同应按名字码点升序（同 U+540C < 少 U+5C11），实际 %v", first)
	}
}

// TestUsageRespectsTimeWindow: the time window must really exclude runs outside it, and their tokens must not be counted either.
func TestUsageRespectsTimeWindow(t *testing.T) {
	ctx := context.Background()
	svc := newUsageService(t,
		usageRun("r1", "a1", "甲", "2026-08-24T10:00:00.000Z", 100, "", 1000, 100),
		usageRun("r2", "a1", "甲", "2026-08-25T10:00:00.000Z", 200, "", 2000, 200),
		usageRun("r3", "a1", "甲", "2026-08-26T10:00:00.000Z", 300, "", 4000, 400),
	)

	sum, err := svc.WorkspaceUsage(ctx, "ws_usage", persistmodel.UsageQuery{
		Since: "2026-08-25T00:00:00.000Z",
		Until: "2026-08-26T00:00:00.000Z",
	})
	if err != nil {
		t.Fatalf("聚合失败：%v", err)
	}
	if sum.TotalRuns != 1 {
		t.Errorf("窗口内只有 1 条，实际 %d", sum.TotalRuns)
	}
	if sum.Usage.InputTokens != 2000 {
		t.Errorf("窗外的 token 不能算进来，期望 2000，实际 %d", sum.Usage.InputTokens)
	}
	if len(sum.Daily) != 1 || sum.Daily[0].Date != "2026-08-25" {
		t.Errorf("只该有 08-25 一个桶，实际 %#v", sum.Daily)
	}
}
