// Performance baseline for ListRuns.
//
// # Why this bench exists
//
// `ListRuns` fetches the doc (jsonb) of **all** runs in a workspace at once and fully
// deserializes them in Go, with no LIMIT / OFFSET; and the sort is
// `order by coalesce(nullif(doc->>'lastActivityAt',''), started_at) desc, ord`
// The expression does not match runs_workspace_started_idx, so that index cannot be used, and it is always
// a sequential scan + external sort.
//
// These two costs **do not show on small tables** (an existing lesson in this repo: small tables do not reveal a missing order by).
// In the seed database a workspace has only 5 runs, and everything runs in a fraction of a millisecond, so the premise "the conversation list is a small result set,
// acceptable" seems to hold forever. This bench pushes the scale to the size production is approaching,
// so the degradation becomes **a measurable number** rather than a judgment call.
//
// # How to run
//
//	docker compose up -d                      # repo root, PG 17 on 15432
//	cd server && go test ./repository/dao/coredao/ -run '^$' \
//	    -bench 'BenchmarkListRuns' -benchtime 10x
//
// When the database is unreachable the whole group Skips (same convention as store_pg_test.go), so running `go test ./...` offline
// stays green. **Note -run '^$'**: without it, all the integration tests in this package run too.
//
// To see the real query plan (to judge whether it is a sequential scan + external sort), use:
//
//	go test ./repository/dao/coredao/ -run TestListRunsQueryPlan -v
//
// # Check (a falsifiable hard threshold, not "noticeably high")
//
// With 5000 runs in one workspace, the P50 of `ListRuns` **should not exceed 150ms**. Exceeding it means
// "fetch everything + deserialize" has entered the range users notice. The conversation list is the first-screen dependency of the chat page's left column,
// and every extra 100ms there is a blank screen. Measured numbers are recorded in the ListRuns comment in store_pg.go.
//
// ⚠️ This bench **deliberately uses no tricks beyond b.ResetTimer to push the number down**: what it measures is exactly the full cost of one
// real HTTP request hitting the Store (query + scan + deserialize + Clone).

package coredao

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// benchWS is the workspace id used only by the bench.
//
// seedWS is not reused: integration tests Reset and reseed that workspace again and again, while the bench needs to control
// the row count itself. A separate id also makes "the same table holds other workspaces' data" true, which is how
// production looks, and it also checks that `where workspace_id = $1` really filters out other rows.
const benchWS = "ws_bench"

// benchNoiseWS is the id of "another workspace", used to put noise rows in the same table.
//
// Without it, `where workspace_id = $1` would match the whole table and the measured number would be **too optimistic**:
// the rows scanned sequentially would equal exactly the rows returned, while a production runs table holds dozens of workspaces.
const benchNoiseWS = "ws_bench_noise"

// seedRunsForBench loads n runs belonging to benchWS into the runs table, plus the same number of noise rows.
//
// Each run's doc shape is **built like real data**, not an empty shell:
//
//   - at least one thread (more with multi-agent collaboration);
//   - all four usage fields have values;
//   - lastActivityAt exists: that is exactly the key the sort expression reads. **It must exist**,
//     otherwise coalesce always takes the started_at branch and the measured sort cost is too low;
//   - title / agentName are Chinese strings of real length, so the actual jsonb byte size is close to production.
//
// ⚠️ **Some rows deliberately have no lastActivityAt** (one in every 7), because old production data lacks this key,
// and only a mix of both coalesce branches is the real sort input. All with or all without would make
// this bench measure a uniform case that does not exist in production.
func seedRunsForBench(tb testing.TB, s *PGStore, n int) {
	tb.Helper()
	ctx := context.Background()

	// Clear first so each size starts from a known state; otherwise when -bench runs several sizes the row counts add up,
	// and a later size measures "the sum of all previous sizes", a number that just looks "slower than expected".
	if err := s.Clear(ctx); err != nil {
		tb.Fatalf("清空失败：%v", err)
	}

	insert := func(ws string, i int) {
		threadID := fmt.Sprintf("thread_%s_%d", ws, i)
		run := apimodel.Run{
			ID:          fmt.Sprintf("run_%s_%06d", ws, i),
			WorkspaceID: ws,
			AgentID:     fmt.Sprintf("agent_%d", i%12),
			AgentName:   "客服助手（压测）",
			Title:       fmt.Sprintf("第 %d 轮会话：帮我查一下订单状态并给出处理建议", i),
			Status:      apimodel.RunTerminated,
			Trigger:     apimodel.TriggerManual,
			StartedAt:   benchISOAt(i),
			Usage: apimodel.TokenUsage{
				InputTokens:              12000 + i,
				OutputTokens:             3400 + i,
				CacheReadInputTokens:     88000 + i,
				CacheCreationInputTokens: 1200 + i,
			},
			Threads: []apimodel.RunThread{{
				ID:             threadID,
				AgentName:      "客服助手（压测）",
				Status:         "completed",
				ParentThreadID: nil,
				StartedAt:      benchISOAt(i),
			}},
			EventCount: 582, // measured event count of one conversation
		}
		// Leave one in every 7 without lastActivityAt, simulating old existing data.
		if i%7 != 0 {
			run.LastActivityAt = benchISOAt(i + 1)
		}
		if _, err := s.CreateRun(ctx, run); err != nil {
			tb.Fatalf("灌 run 失败：%v", err)
		}
	}

	for i := range n {
		insert(benchWS, i)
		insert(benchNoiseWS, i)
	}
}

// benchISOAt builds a valid ISO 8601 timestamp with exactly three decimal places, UTC.
//
// **It must have exactly three decimal places** (see the timestamp convention in CLAUDE.md): sorting is a string comparison,
// and mixing `.0` with `.000` changes the sort result directly, so the measured sort cost would not be production's.
func benchISOAt(i int) string {
	// From 2026-01-01, one minute apart, a span wide enough for 5000 to be distinct.
	min := i % 60
	hour := (i / 60) % 24
	day := (i/(60*24))%28 + 1
	return fmt.Sprintf("2026-01-%02dT%02d:%02d:00.000Z", day, hour, min)
}

// runListRunsBench is the skeleton shared by all sizes.
func runListRunsBench(b *testing.B, n int) {
	s := benchPGStore(b)
	seedRunsForBench(b, s, n)
	ctx := context.Background()

	// Run once first to confirm the row count matches. If loading went wrong, what is measured below is the wrong size, and that only shows as
	// "the number is smaller than expected", not revealing a fixture problem.
	got, err := s.ListRuns(ctx, benchWS, persistmodel.RunQuery{})
	if err != nil {
		b.Fatalf("ListRuns 失败：%v", err)
	}
	if len(got) != n {
		b.Fatalf("规模不对：想要 %d 条，实际 %d 条", n, len(got))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		out, err := s.ListRuns(ctx, benchWS, persistmodel.RunQuery{})
		if err != nil {
			b.Fatalf("ListRuns 失败：%v", err)
		}
		// Use the result, so the compiler does not optimize the whole call away.
		if len(out) != n {
			b.Fatalf("返回条数变了：%d", len(out))
		}
	}
}

// BenchmarkListRuns_100 is the "small table" control group.
//
// It exists not to find a problem but to **prove the problem depends on scale**: without this size,
// the 5000 number is just an isolated value that cannot say "how many times it grew".
func BenchmarkListRuns_100(b *testing.B) { runListRunsBench(b, 100) }

// BenchmarkListRuns_1000 is around the current production size.
func BenchmarkListRuns_1000(b *testing.B) { runListRunsBench(b, 1000) }

// BenchmarkListRuns_5000 is the check size: a P50 above 150ms counts as degradation.
func BenchmarkListRuns_5000(b *testing.B) { runListRunsBench(b, 5000) }

// BenchmarkListRunsFirst20 measures how fast "only the first 20" could be in theory.
//
// It is **the lower bound of the optimization target**, not the current state: ListRuns has no LIMIT now, so this bench directly
// runs an equivalent SQL with limit, to answer "how much would paging save".
// The difference between the two numbers is the upper bound of this optimization's gain.
func BenchmarkListRunsFirst20(b *testing.B) {
	s := benchPGStore(b)
	seedRunsForBench(b, s, 5000)
	ctx := context.Background()

	const q = `select doc from runs where workspace_id = $1
	           order by coalesce(nullif(doc->>'lastActivityAt', ''), started_at) desc, ord
	           limit 20`

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		out, err := queryDocs(ctx, s.client, persistmodel.CloneRun, q, benchWS)
		if err != nil {
			b.Fatalf("查询失败：%v", err)
		}
		if len(out) != 20 {
			b.Fatalf("想要 20 条，实际 %d 条", len(out))
		}
	}
}

// BenchmarkListRunsPaged20 measures the paging path **through the real Store API**.
//
// The difference from BenchmarkListRunsFirst20 matters: that one is the "theoretical lower bound" with hand-written SQL,
// this one goes through `ListRuns` + `RunQuery{Limit: 20}`, measuring **the path that really ships**.
// Only if the two are close is paging really hooked up. If this one is clearly slower than First20, something on the Go side
// is still doing work over everything (for example truncating only after fetching).
//
// Measured (5000 runs + 5000 noise rows, local PG 17):
//
//	no paging (RunQuery{})       62.5 ms   19.2 MB   134339 allocs
//	Limit=20 (this bench)         5.6 ms   0.07 MB      561 allocs
//	hand-written limit 20 bound   5.6 ms   0.07 MB      561 allocs
//
// **About 11x, with allocations down to 1/270.** And after paging the time is **decoupled from the total**; that is what this optimization
// really wanted (the unpaged one is linear, and 5000 is only the current size, not the end).
func BenchmarkListRunsPaged20(b *testing.B) {
	s := benchPGStore(b)
	seedRunsForBench(b, s, 5000)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		out, err := s.ListRuns(ctx, benchWS, persistmodel.RunQuery{Limit: 20})
		if err != nil {
			b.Fatalf("ListRuns 失败：%v", err)
		}
		if len(out) != 20 {
			b.Fatalf("想要 20 条，实际 %d 条", len(out))
		}
	}
}

// BenchmarkListRunsSearch measures the path with search.
//
// `position($1 in lower(title)) > 0` does one lower() + substring search on each of three columns,
// a full table scan that cannot use any index. This size answers "how much more search costs than no search".
func BenchmarkListRunsSearch(b *testing.B) {
	s := benchPGStore(b)
	seedRunsForBench(b, s, 5000)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := s.ListRuns(ctx, benchWS, persistmodel.RunQuery{Search: "订单状态"}); err != nil {
			b.Fatalf("ListRuns 失败：%v", err)
		}
	}
}

// benchPGStore gets a PGStore connected to the test schema, and Skips if unreachable.
//
// The difference from pgFactory: that one takes *testing.T and reseeds on every Reset, while the bench needs testing.B
// and controls its own data. They share pgEnvOnce, so the whole package creates the connection and schema only once.
func benchPGStore(b *testing.B) *PGStore {
	b.Helper()
	pgEnvOnce.Do(initPGEnv)
	if pgEnv.skip != "" {
		b.Skip(pgEnv.skip)
	}
	s, err := NewPGStore(context.Background(), pgEnv.client)
	if err != nil {
		b.Fatalf("建 PGStore 失败：%v", err)
	}
	return s
}

// TestListRunsQueryPlan prints the real query plan of ListRuns and asserts its structure.
//
// This is not a performance test (it does not measure time, so it is stable in CI) but a **structural assertion**: currently it must do
// a Sort, because the sort expression
// `coalesce(nullif(doc->>'lastActivityAt',”), started_at)` does not match
// the bare columns of runs_workspace_started_idx, and the index cannot give ordered output.
//
// ⚠️ **There is a counterintuitive fact here, corrected once by measurement**: `runs_workspace_started_idx`
// is not completely unused: the planner uses it for a **Bitmap Index Scan** to satisfy
// `where workspace_id = $1`, it just **cannot sort**. So the sentence in store_pg.go,
// "this sort cannot use runs_workspace_started_idx", should be read as "cannot use its **order**",
// not "the whole index is unused".
//
// This difference decides the optimization direction; it is not about wording:
//
//   - if it were "the index is not used at all" → add an expression index;
//   - it actually is "filtering uses the index, only ordering needs a Sort" → an expression index **gains very little**,
//     because the Sort itself is cheap (2000 rows measured 3.2ms). The real cost is **moving all the docs
//     back into Go and deserializing them**, which only LIMIT can fix.
//
// Why it is worth an assertion rather than just a comment: if someone adds an expression index / changes the sort key later,
// this test goes **red**, which is exactly the reminder to update that comment in store_pg.go; otherwise the comment quietly
// becomes a lie, and the next person makes decisions based on it.
//
// ⚠️ Use explain (analyze, buffers) instead of bare explain: bare explain only gives estimates,
// and on small tables estimates give plans different from the real execution (this repo's lesson: small tables do not reveal problems).
func TestListRunsQueryPlan(t *testing.T) {
	pgEnvOnce.Do(initPGEnv)
	if pgEnv.skip != "" {
		t.Skip(pgEnv.skip)
	}
	s, err := NewPGStore(context.Background(), pgEnv.client)
	if err != nil {
		t.Fatalf("建 PGStore 失败：%v", err)
	}
	ctx := context.Background()

	// 2000 rows are enough to make the planner give up the "the table is tiny anyway" choice, without making this test slow.
	seedRunsForBench(t, s, 2000)

	// analyze really runs it once, so what comes back is actual row counts and actual times.
	const q = `explain (analyze, buffers, format json)
	           select doc from runs where workspace_id = $1
	           order by coalesce(nullif(doc->>'lastActivityAt', ''), started_at) desc, ord`

	var raw []byte
	if err := pgEnv.client.QueryRow(ctx, q, benchWS).Scan(&raw); err != nil {
		t.Fatalf("取执行计划失败：%v", err)
	}
	var plan any
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatalf("解析执行计划失败：%v", err)
	}
	pretty, _ := json.MarshalIndent(plan, "", "  ")
	text := string(pretty)
	t.Logf("ListRuns 的执行计划（2000 条 run + 2000 条干扰行）：\n%s", text)

	// Assert there must be a Sort: the sort expression cannot give index order. If this goes red, ordering can now use an index;
	// go update the comment in store_pg.go.
	if !strings.Contains(text, `"Sort`) {
		t.Errorf("预期现状要做一次外部排序（排序表达式与索引裸列不匹配），"+
			"但计划里没有 Sort 节点。若已改成走索引的有序输出，"+
			"请同步更新 store_pg.go 里 ListRuns 那段注释。\n计划：%s", text)
	}
	// Assert there is **no** Limit: that is exactly the root cause of this performance problem; all rows have to be moved back into Go.
	// After paging is added this will go red; then flip the assertion and update the ListRuns comment.
	if strings.Contains(text, `"Limit`) {
		t.Errorf("计划里出现了 Limit，说明 ListRuns 已经支持分页了。"+
			"请把本断言反过来（改成要求有 Limit），并更新 store_pg.go 的注释。\n计划：%s", text)
	}
}

// TestListRunsCostBreakdown splits ListRuns' 55ms into three parts, answering "where does the cost go".
//
// Without this breakdown it is easy to blame the sort and go add an expression index, and that is **the wrong direction**,
// gaining under 6%. How each of the three parts is measured:
//
//	SQL all rows (with Sort, doc stays in the database)   count(*) wrapped around a subquery, doc not returned
//	SQL all rows (doc returned)                           exactly ListRuns' SQL, but not deserialized
//	Go deserialization                                    total ListRuns time minus the one above
//
// This test **sets no threshold** (numbers differ by machine); it only prints the three numbers, for use when making decisions.
// Run it: go test ./repository/dao/coredao/ -run TestListRunsCostBreakdown -v
func TestListRunsCostBreakdown(t *testing.T) {
	pgEnvOnce.Do(initPGEnv)
	if pgEnv.skip != "" {
		t.Skip(pgEnv.skip)
	}
	s, err := NewPGStore(context.Background(), pgEnv.client)
	if err != nil {
		t.Fatalf("建 PGStore 失败：%v", err)
	}
	ctx := context.Background()
	const n = 5000
	seedRunsForBench(t, s, n)

	// Timing takes the fastest of three runs: the fastest value is least affected by noise (the same approach as Go bench).
	fastest := func(label string, fn func()) time.Duration {
		best := time.Hour
		for range 3 {
			start := time.Now()
			fn()
			if d := time.Since(start); d < best {
				best = d
			}
		}
		t.Logf("%-46s %v", label, best)
		return best
	}

	// One: sort without returning doc, measuring the cost of sorting + scanning alone.
	sortOnly := fastest("① SQL 排序+扫描（doc 不回传）", func() {
		var cnt int
		q := `select count(*) from (
		        select id from runs where workspace_id = $1
		        order by coalesce(nullif(doc->>'lastActivityAt', ''), started_at) desc, ord
		      ) t`
		if err := pgEnv.client.QueryRow(ctx, q, benchWS).Scan(&cnt); err != nil {
			t.Fatalf("查询失败：%v", err)
		}
		if cnt != n {
			t.Fatalf("行数不对：%d", cnt)
		}
	})

	// Two: return doc but do not deserialize, adding the cost of "moving 19MB of jsonb over the network".
	withTransfer := fastest("② SQL 排序+扫描+回传 doc（不反序列化）", func() {
		rows, err := pgEnv.client.Query(ctx,
			`select doc from runs where workspace_id = $1
			 order by coalesce(nullif(doc->>'lastActivityAt', ''), started_at) desc, ord`, benchWS)
		if err != nil {
			t.Fatalf("查询失败：%v", err)
		}
		defer rows.Close()
		var total int
		for rows.Next() {
			var blob []byte
			if err := rows.Scan(&blob); err != nil {
				t.Fatalf("扫描失败：%v", err)
			}
			total += len(blob)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("遍历失败：%v", err)
		}
		t.Logf("   （doc 总字节数：%.2f MB）", float64(total)/(1<<20))
	})

	// Three: the full ListRuns, adding json.Unmarshal + Clone.
	full := fastest("③ ListRuns 完整（含反序列化+Clone）", func() {
		out, err := s.ListRuns(ctx, benchWS, persistmodel.RunQuery{})
		if err != nil {
			t.Fatalf("ListRuns 失败：%v", err)
		}
		if len(out) != n {
			t.Fatalf("条数不对：%d", len(out))
		}
	})

	t.Logf("──── 拆解（%d 条 run）────", n)
	t.Logf("排序+扫描        ：%v（占 %.0f%%）", sortOnly, 100*float64(sortOnly)/float64(full))
	t.Logf("doc 回传         ：%v（占 %.0f%%）", withTransfer-sortOnly,
		100*float64(withTransfer-sortOnly)/float64(full))
	t.Logf("Go 反序列化+Clone：%v（占 %.0f%%）", full-withTransfer,
		100*float64(full-withTransfer)/float64(full))
	t.Logf("结论：排序只占一小部分，加表达式索引治不了这个问题；" +
		"要治必须让**回传的行数**变少（LIMIT/分页）。")
}
