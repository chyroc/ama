// Performance baseline for ListRunEvents.
//
// # The key difference from the ListRuns one: a different growth axis
//
// Neither method has a LIMIT, but **the degradation conditions are completely different**, which decides whether and how to fix it:
//
//	ListRuns        driven by **the total number of runs** in the workspace → grows with usage, never goes back
//	ListRunEvents   driven by **the event count of a single session**     → has a natural upper bound (a conversation always ends)
//
// `where run_id = $1` is fully covered by indexes led by `(run_id, ...)` (both the primary key
// `(run_id, sequence)` and the unique index `(run_id, event_id)` work), so even with a hundred million rows in the table
// the query for one run is unaffected: the index goes straight to that range.
//
// ⚠️ **This must be confirmed by measurement, not by grep**: not finding
// `CREATE INDEX run_events...` in the schema file easily leads to "no index", while primary keys/unique constraints
// **come with their own indexes**. This repo redid work once over that misjudgment. The check is TestListRunEventsQueryPlan,
// which also records two measured corrections of "guessing wrong by reading the query plan".
//
// So the question for this bench is not "does it degrade with usage" but: **how expensive is one long session now**.
// One session was measured at 582 events, and raw is the complete Ark event kept byte for byte (a text column, not
// trimmed down), so the bytes each session detail page has to move are not small.
//
// # How to run
//
//	docker compose up -d
//	cd server && go test ./repository/dao/coredao/ -run '^$' \
//	    -bench 'BenchmarkListRunEvents' -benchtime 10x
//
// # Check
//
// At 582 events (the measured size) it **should not exceed 50ms**: this blocks the session detail page/chat page from
// opening. At 3000 events (a very long session) it should not exceed 250ms. Only past that does it need fixing, and the fix is **paging by
// sequence** (the frontend renders from start to end anyway), not adding an index: the index is already enough.

package coredao

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// benchEventRunID is the run id used for loading events.
const benchEventRunID = "run_bench_events"

// seedEventsForBench loads n events into one run.
//
// raw is built in **the shape and size of real Ark events**: in production raw is the full SSE frame (with content,
// usage, thread and so on); one message_delta measured around 300~800 bytes. An empty shell
// `{}` would make this bench report an optimistic number that does not exist in production, while what this bench must answer is exactly
// "how long does moving these bytes take".
func seedEventsForBench(tb testing.TB, s *PGStore, runID string, n int) {
	tb.Helper()
	ctx := context.Background()

	if err := s.Clear(ctx); err != nil {
		tb.Fatalf("清空失败：%v", err)
	}
	// Events hang on a run, so create that run first (there is no foreign key, but it must hold semantically,
	// and paths like GetRun use it).
	if _, err := s.CreateRun(ctx, apimodel.Run{
		ID:          runID,
		WorkspaceID: benchWS,
		AgentID:     "agent_0",
		AgentName:   "客服助手（压测）",
		Title:       "长会话压测",
		Status:      apimodel.RunTerminated,
		Trigger:     apimodel.TriggerManual,
		StartedAt:   benchISOAt(0),
		EventCount:  n,
	}); err != nil {
		tb.Fatalf("建 run 失败：%v", err)
	}

	for i := range n {
		// AppendRunEvent takes **raw bytes** and assigns sequence itself; use it instead of building
		// RunEvent by hand, so the data this bench loads comes from the same source as production.
		if _, _, err := s.AppendRunEvent(ctx, runID, []byte(buildBenchRaw(i))); err != nil {
			tb.Fatalf("灌事件失败：%v", err)
		}
	}
}

// buildBenchRaw builds the raw bytes of the i-th event.
//
// **It is a function so the two loading paths (seedEventsForBench one by one / seedEventsBulk in bulk)
// use the same content**: writing it twice would drift sooner or later, and after drifting the two would load different tables
// with nothing to report an error.
//
// ⚠️ raw **deliberately has no sequence key**. With one it would hit the primary key: AppendRunEvent computes
// `max(sequence)+1` itself, while NewRunEventFromArk prefers the value in raw when raw already has a non-zero sequence
// (see the injectKeys branch in domain/events.go). Once the two disagree you get 50 consecutive
// 23505 unique constraint violations, with an error saying "sequence clash", which looks like a concurrency problem
// but is really the fixture hard-coding the sequence.
//
// No sequence is also **the real shape**: some of Ark's delta frames do not carry this key,
// which is exactly why injectKeys exists.
func buildBenchRaw(i int) string {
	return fmt.Sprintf(`{"id":"sevt-%016d","type":"message_delta",`+
		`"session_thread_id":%q,`+
		`"content":[{"type":"text","text":%q}],`+
		`"usage":{"input_tokens":%d,"output_tokens":%d,`+
		`"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d},`+
		`"processed_at":"2026-01-15T08:%02d:%02d.123456789Z"}`,
		i, "thread_bench_0",
		"这是第 "+fmt.Sprint(i)+" 条增量，内容长度接近线上真实的一帧文本增量，"+
			"用来让 raw 列的字节数落在实际量级上而不是空壳。",
		12000+i, 3400+i, 88000+i, 1200+i,
		(i/60)%60, i%60)
}

// seedEventsBulk loads **the same data** as seedEventsForBench, but writes it with one multi-row INSERT
// instead of going through AppendRunEvent one by one.
//
// # Why this fast path exists
//
// AppendRunEvent makes **two round trips** per event (first `select max(sequence)+1`, then insert),
// which is how it should be (the sequence is written into the raw bytes and forwarded to the browser; letting the DB number it would not match,
// see that method's comment). But loading a fixture does not need that behavior: here we assign the sequence ourselves.
//
// The cost is real: 1200 events = 2400 round trips. And when tests run concurrently on this machine
// **load average is 217 / 64 cores** (3.4x oversubscribed), a single round trip goes from 0.10ms to 1.09ms,
// so the fixture alone takes over 10 seconds and pushes TestListRunEventsQueryPlan into a timeout.
// Even when that test PASSes it measured 10~11 seconds; red or not depends only on how busy the machine is.
//
// ⚠️ **Only for tests that "only care that these rows are in the table"** (query plans, query time).
// To check AppendRunEvent's own behavior (how sequences are assigned, how clashes are retried, what gets injected into raw),
// you must use seedEventsForBench, otherwise it is not that path being tested.
//
// ⚠️ **The event content is byte-for-byte the same as seedEventsForBench** (same buildBenchRaw + same
// apimodel.NewRunEventFromArk), so the tables the two load have no difference in data;
// the only difference is "how it was written". If they drift, this fast path starts testing different data.
func seedEventsBulk(tb testing.TB, s *PGStore, runID string, n int) {
	tb.Helper()
	ctx := context.Background()

	if err := s.Clear(ctx); err != nil {
		tb.Fatalf("清空失败：%v", err)
	}
	if _, err := s.CreateRun(ctx, apimodel.Run{
		ID: runID, WorkspaceID: benchWS, AgentID: "agent_0",
		AgentName: "客服助手（压测）", Title: "长会话压测",
		Status: apimodel.RunTerminated, Trigger: apimodel.TriggerManual,
		StartedAt: benchISOAt(0), EventCount: n,
	}); err != nil {
		tb.Fatalf("建 run 失败：%v", err)
	}

	// Build multi-row INSERTs in batches. Batch size 200: PostgreSQL's parameter limit is 65535,
	// 7 parameters per row → a theoretical cap of about 9300 rows; 200 leaves plenty of margin and keeps logs readable.
	const batch = 200
	for start := 0; start < n; start += batch {
		end := min(start+batch, n)

		var sb strings.Builder
		sb.WriteString(`insert into run_events ` +
			`(run_id, sequence, type, thread_id, doc, raw, event_id) values `)
		args := make([]any, 0, (end-start)*7)
		for i := start; i < end; i++ {
			// Uses the same constructor as seedEventsForBench, so the bytes are identical.
			ev := apimodel.NewRunEventFromArk([]byte(buildBenchRaw(i)), i+1)
			doc, err := marshalDoc(map[string]any{
				"type":              ev.Type,
				"sequence":          ev.Sequence,
				"session_thread_id": ev.ThreadID,
			})
			if err != nil {
				tb.Fatalf("编码 doc 失败：%v", err)
			}
			var threadID, eventID any
			if ev.ThreadID != nil {
				threadID = *ev.ThreadID
			}
			if ev.EventID != "" {
				eventID = ev.EventID
			}
			if i > start {
				sb.WriteString(", ")
			}
			b := (i - start) * 7
			fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d,$%d)",
				b+1, b+2, b+3, b+4, b+5, b+6, b+7)
			args = append(args, runID, ev.Sequence, ev.Type, threadID,
				doc, string(ev.Raw), eventID)
		}
		if _, err := s.client.Exec(ctx, sb.String(), args...); err != nil {
			tb.Fatalf("批量灌事件失败（%d~%d）：%v", start, end, err)
		}
	}
}

func runListRunEventsBench(b *testing.B, n int) {
	s := benchPGStore(b)
	seedEventsForBench(b, s, benchEventRunID, n)
	ctx := context.Background()

	got, err := s.ListRunEvents(ctx, benchEventRunID, persistmodel.EventQuery{})
	if err != nil {
		b.Fatalf("ListRunEvents 失败：%v", err)
	}
	if len(got) != n {
		b.Fatalf("规模不对：想要 %d 条，实际 %d 条", n, len(got))
	}
	var bytes int
	for _, e := range got {
		bytes += len(e.Raw)
	}
	b.Logf("%d 个事件，raw 合计 %.2f MB", n, float64(bytes)/(1<<20))

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		out, err := s.ListRunEvents(ctx, benchEventRunID, persistmodel.EventQuery{})
		if err != nil {
			b.Fatalf("ListRunEvents 失败：%v", err)
		}
		if len(out) != n {
			b.Fatalf("返回条数变了：%d", len(out))
		}
	}
}

// BenchmarkListRunEvents_582 is the **measured size**: one long session really can have 582 events.
// Check: no more than 50ms.
func BenchmarkListRunEvents_582(b *testing.B) { runListRunEventsBench(b, 582) }

// BenchmarkListRunEvents_3000 is a very long session (reusing the same run in a Feishu group for a long time).
// Check: no more than 250ms.
func BenchmarkListRunEvents_3000(b *testing.B) { runListRunEventsBench(b, 3000) }

// TestListRunEventsQueryPlan asserts that the run_events indexes **are enough**.
//
// This test exists **to prevent a misjudgment**, not to prevent a regression: in an earlier investigation, because
// `create index ... run_events` could not be found in the schema file by grep, the conclusion "missing index" was reached for a while,
// while the truth is that both the primary key `(run_id, sequence)` and the unique index `(run_id, event_id)` cover
// `where run_id = $1`.
//
// # Two measured corrections, written down so the next person does not guess again
//
// One: **the planner does not pick the primary key but `run_events_event_id_idx`**. Both indexes
// lead with run_id and are equivalent for this query; which one is picked is decided by cost estimation. So **do not assert
// which index is used**; that would make this test go red with the planner's mood. Only assert "not Seq Scan".
//
// Two: **`order by sequence` still does a Sort; the primary key's ordering is not used**.
// The reason is that the planner uses a **Bitmap** Index Scan, and a bitmap scan fetches rows in physical page order,
// naturally losing the index order, so a sort must be added. An earlier assumption asserted "no Sort", and measurement showed it was wrong.
//
// ⚠️ Neither of these **affects the performance conclusion**: 582 events measured 1.77ms, 3000 events 8.1ms,
// well within the checks. The cost of the Sort is negligible at this size. **This is exactly why you measure instead of
// drawing conclusions by reading the query plan**: having a Sort does not mean having a performance problem.
//
// So this test asserts only one thing: **it must not degrade into a Seq Scan**. That is the failure mode that grows with the whole table
// (say someone changed the primary key, or added a filter that makes the planner give up the index).
//
// # ⚠️ It used to go red randomly; the root cause was not a performance regression but a slow fixture
//
// It used to load 1200 events one by one with seedEventsForBench (600 for each of two runs), and
// AppendRunEvent makes **two round trips** per event → 2400 round trips. Under high machine load a single round trip can grow by an
// order of magnitude, so the fixture alone took over 10 seconds and pushed the whole test into a timeout (green on rerun).
//
// **It now loads in bulk with seedEventsBulk: from about 10 seconds to under 1 second**.
// The data content is byte-for-byte unchanged and the assertions were not touched. Mutation-verified: forcing the planner to give up the index,
// it still reports Seq Scan and goes red.
//
// ⚠️ **Do not cut the load count to "speed it up"**: 1200 events is exactly to simulate a real long session
// (a single long run can have over a thousand events); cutting it makes this test meaningless.
//
// ⚠️ **And do not use this to "optimize" AppendRunEvent's two round trips.** I once computed "8.3ms per event,
// 23% of it the max read" and concluded it was "a read-modify-write anti-pattern". **That conclusion was wrong**:
// the 8.3ms was almost all queuing caused by contention; when idle that max takes only 0.22ms (index scan,
// `actual time=0.072ms`, and it does not grow with the event count). Merging it into one SQL would also make the sequence embedded in raw
// disagree with the column. The full reasoning is in the AppendRunEvent comment in store_pg.go.
func TestListRunEventsQueryPlan(t *testing.T) {
	pgEnvOnce.Do(initPGEnv)
	if pgEnv.skip != "" {
		t.Skip(pgEnv.skip)
	}
	s, err := NewPGStore(context.Background(), pgEnv.client)
	if err != nil {
		t.Fatalf("建 PGStore 失败：%v", err)
	}
	ctx := context.Background()

	// Load events for two runs, so "filter by run_id" really has something to filter.
	//
	// ⚠️ Use seedEventsBulk instead of seedEventsForBench: this test only cares that **these rows are in the table**
	// (it asserts the query plan), not how they were written. AppendRunEvent one by one takes
	// 2400 round trips; on a busy machine the fixture alone takes over 10 seconds and pushes this test into a timeout,
	// which is exactly why it used to go red randomly (see the function header). The data content is byte-for-byte the same on both paths.
	seedEventsBulk(t, s, benchEventRunID, 600)
	// Add one more run, simulating other sessions in the same table.
	if _, err := s.CreateRun(ctx, apimodel.Run{
		ID: "run_bench_other", WorkspaceID: benchWS, AgentID: "agent_1",
		AgentName: "别的 agent", Title: "另一条会话", Status: apimodel.RunTerminated,
		Trigger: apimodel.TriggerManual, StartedAt: benchISOAt(1),
	}); err != nil {
		t.Fatalf("建对照 run 失败：%v", err)
	}
	// The control run is also loaded in bulk, for the same reason.
	{
		var sb strings.Builder
		sb.WriteString(`insert into run_events ` +
			`(run_id, sequence, type, thread_id, doc, raw, event_id) values `)
		args := make([]any, 0, 600*7)
		for i := range 600 {
			raw := fmt.Sprintf(`{"id":"sevt-o-%d","type":"message_delta",`+
				`"sequence":%d,"session_thread_id":"thread_other"}`, i, i+1)
			if i > 0 {
				sb.WriteString(", ")
			}
			b := i * 7
			fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d,$%d)",
				b+1, b+2, b+3, b+4, b+5, b+6, b+7)
			args = append(args, "run_bench_other", i+1, "message_delta",
				"thread_other", []byte(`{}`), raw, fmt.Sprintf("sevt-o-%d", i))
		}
		if _, err := s.client.Exec(ctx, sb.String(), args...); err != nil {
			t.Fatalf("灌对照事件失败：%v", err)
		}
	}

	const q = `explain (analyze, buffers, format json)
	           select raw, type, sequence, thread_id, event_id from run_events
	           where run_id = $1 order by sequence`

	var raw []byte
	if err := pgEnv.client.QueryRow(ctx, q, benchEventRunID).Scan(&raw); err != nil {
		t.Fatalf("取执行计划失败：%v", err)
	}
	var plan any
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatalf("解析执行计划失败：%v", err)
	}
	pretty, _ := json.MarshalIndent(plan, "", "  ")
	text := string(pretty)
	t.Logf("ListRunEvents 的执行计划（两条 run 各 600 事件）：\n%s", text)

	if strings.Contains(text, "Seq Scan") {
		t.Errorf("run_events 的主查询走了顺序扫，说明 (run_id, ...) 前导的索引"+
			"不再被用来满足 `where run_id = $1`——那会让 ListRunEvents 从"+
			"「与表大小无关」变成「随全表增长退化」。检查是不是改了主键，"+
			"或加了让 planner 放弃索引的过滤条件。\n计划：%s", text)
	}
	// ⚠️ **Deliberately no assertion on whether Sort exists or which index is used**; see the method comment:
	// measured, the planner uses a Bitmap Index Scan (losing order, so a Sort is always added), and the choice between the two
	// equivalent indexes is decided by cost estimation. Asserting those would make the test go red with the planner,
	// and none of them affect the performance conclusion.
}
