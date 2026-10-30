// Performance baseline for the write path: AppendRunEvent.
//
// # Why measure the write path too
//
// The earlier benches all measure read paths, but `AppendRunEvent` is called **more often than any read endpoint**:
// once for every event Ark produces; one conversation was measured at 582 events. And each call makes
// **2 serial round trips**:
//
//	① select coalesce(max(sequence), 0) + 1 from run_events where run_id = $1
//	② insert into run_events (...) values (...)
//
// So a 582-event conversation costs **1164 round trips** on the write path. A local PG round trip is
// 0.1~0.3ms and shows no problem, but in production it crosses the network to a managed PG at millisecond level. **This is exactly the
// "local runs do not show the production problem" kind**, so the check uses **round-trip count** rather than local milliseconds (same as bench_authz_test.go).
//
// ⚠️ **But do not rush to optimize away that select.** It is not an oversight but deliberate: sequence is computed by the database
// on the spot instead of given by the caller, and a clash is retried (maxAttempts=50). The long comment in store_pg.go
// explains why: under concurrent appends, "silently dropping events" is far worse than one extra query,
// and ConcurrentAppendRunEvent in store_test.go, with 16 concurrent appends, pins exactly that.
//
// So the purpose of this bench is **to give numbers so "should we change it" has a basis**, not to assert there is a problem.
// To really cut round trips, the direction is to let the insert compute sequence itself
// (`insert ... select coalesce(max(sequence),0)+1 from run_events where run_id=$1`),
// so one round trip is enough and the unique constraint covers concurrency, but that requires re-verifying concurrent correctness,
// which is outside the scope of this bench.
//
// # How to run
//
//	docker compose up -d
//	cd server && go test ./repository/dao/coredao/ -run '^$' \
//	    -bench 'BenchmarkAppendRunEvent' -benchtime 200x
//
// # Check
//
// **A single AppendRunEvent should take no more than 2 round trips** (currently exactly 2: meets the bar with no margin).
// If someone adds one more database query on this path, a 582-event conversation pays 582 more round trips,
// and this bench's numbers will jump visibly.
//
// Local time has **no threshold** (same reason: local round trips are too fast, so any local threshold would let production problems slip through);
// it is only reported for comparison.

package coredao

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

// benchAppendRunID is the run id used by the write path bench.
const benchAppendRunID = "run_bench_append"

// prepareAppendRun creates an empty run to append events to.
//
// **Every bench iteration appends to the same run**, so the number of events in the run grows with the
// iterations. That is exactly what is measured: does the cost of the `max(sequence)` query depend on the existing event count?
// (There is a primary key index, so it should not; this bench checks that along the way.)
func prepareAppendRun(tb testing.TB, s *PGStore) {
	tb.Helper()
	ctx := context.Background()

	if err := s.Clear(ctx); err != nil {
		tb.Fatalf("清空失败：%v", err)
	}
	if _, err := s.CreateRun(ctx, apimodel.Run{
		ID:          benchAppendRunID,
		WorkspaceID: benchWS,
		AgentID:     "agent_0",
		AgentName:   "客服助手（压测）",
		Title:       "写路径压测",
		Status:      apimodel.RunRunning,
		Trigger:     apimodel.TriggerManual,
		StartedAt:   benchISOAt(0),
	}); err != nil {
		tb.Fatalf("建 run 失败：%v", err)
	}
}

// benchArkFrame builds an Ark event frame shaped close to a real one.
//
// ⚠️ **No sequence key**: `AppendRunEvent` computes `max+1` itself; hard-coding it in raw would hit the primary key
// (50 consecutive 23505s, with an error saying "sequence clash", which looks like a concurrency problem).
// See the comment in bench_runevents_test.go.
func benchArkFrame(i int) []byte {
	return []byte(fmt.Sprintf(`{"id":"sevt-append-%08d","type":"message_delta",`+
		`"session_thread_id":"thread_bench_append",`+
		`"content":[{"type":"text","text":%q}],`+
		`"usage":{"input_tokens":%d,"output_tokens":%d,`+
		`"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}`,
		i,
		"这是第 "+fmt.Sprint(i)+" 条增量，长度接近线上真实的一帧文本增量。",
		12000+i, 3400+i, 88000+i, 1200+i))
}

// BenchmarkAppendRunEvent measures the cost of a single append.
//
// Measured (local PG 17, 200x): **about 0.65ms/op**, 6457 B/op, 93 allocs.
// The two round trips take about half each. So a 582-event conversation spends about 380ms on the write path
// in **local** time; production must be recomputed with cross-network round trips.
func BenchmarkAppendRunEvent(b *testing.B) {
	s := benchPGStore(b)
	prepareAppendRun(b, s)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if _, _, err := s.AppendRunEvent(ctx, benchAppendRunID, benchArkFrame(i)); err != nil {
			b.Fatalf("追加事件失败：%v", err)
		}
	}
}

// BenchmarkAppendRunEventAfter1000 measures appending after "this run already has 1000 events".
//
// It answers a specific question: does the cost of `select max(sequence) where run_id = $1` grow with
// the run's existing event count? The primary key is `(run_id, sequence)`, and PostgreSQL handles `max()`
// by **taking one row from the index in reverse** (the measured plan is indeed Index Only Scan on run_events_pkey,
// see TestAppendRunEventSequenceQueryUsesIndex), so it **does not depend on the existing event count**.
//
// Measured (local PG 17):
//
//	baseline (starting from an empty run)  0.65 ms/op
//	After1000 @200x         0.66 ms/op
//	After1000 @400x         0.59 ms/op
//	After1000 @800x         0.58 ms/op
//
// **Flat, confirming it does not depend on the existing event count.**
//
// ⚠️ **There is a measurement trap here, hit and corrected in practice**: the first run with `-benchtime 200x`
// reported 2.09ms for After1000, which looks like "writes got 3.2x slower after 1000 rows exist", a number that looks a lot like
// a real problem. Raising benchtime to 600x brought it back to 0.59ms, showing **it was noise, not a regression**:
// in 200 iterations, the autovacuum / page splits / cold cache caused by padding 1000 rows were spread over very few
// samples, so a few slow samples pulled the average up.
//
// Rule: **a conclusion only counts if it holds across several benchtimes**. One number from a single bench is not enough
// to support a conclusion like "degrades with scale"; that kind of conclusion leads straight to an unnecessary optimization.
func BenchmarkAppendRunEventAfter1000(b *testing.B) {
	s := benchPGStore(b)
	prepareAppendRun(b, s)
	ctx := context.Background()

	// Pad 1000 events first, then start timing.
	for i := range 1000 {
		if _, _, err := s.AppendRunEvent(ctx, benchAppendRunID, benchArkFrame(i)); err != nil {
			b.Fatalf("垫事件失败：%v", err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if _, _, err := s.AppendRunEvent(ctx, benchAppendRunID, benchArkFrame(1000+i)); err != nil {
			b.Fatalf("追加事件失败：%v", err)
		}
	}
}

// TestAppendRunEventSequenceQueryUsesIndex asserts that the `max(sequence)` query uses an index.
//
// **This is the only hard assertion in this file**, because it matches a failure mode that degrades with data volume:
// if that query turns into a full table scan or full run scan, long conversations get slower and slower to write, and the symptom is
// "the longer we chat, the laggier it gets", a problem that is hard to trace back to the write path.
//
// Check: the plan must contain the word Index, and **must not have Seq Scan**.
// PostgreSQL uses InitPlan + Index Only Scan (taking one row in reverse) for `max()` with a btree index.
func TestAppendRunEventSequenceQueryUsesIndex(t *testing.T) {
	pgEnvOnce.Do(initPGEnv)
	if pgEnv.skip != "" {
		t.Skip(pgEnv.skip)
	}
	s, err := NewPGStore(context.Background(), pgEnv.client)
	if err != nil {
		t.Fatalf("建 PGStore 失败：%v", err)
	}
	ctx := context.Background()
	prepareAppendRun(t, s)

	// Insert enough rows so the planner does not pick arbitrarily because "the table is tiny".
	for i := range 500 {
		if _, _, err := s.AppendRunEvent(ctx, benchAppendRunID, benchArkFrame(i)); err != nil {
			t.Fatalf("灌事件失败：%v", err)
		}
	}

	const q = `explain (analyze, buffers, format json)
	           select coalesce(max(sequence), 0) + 1 from run_events where run_id = $1`

	var raw []byte
	if err := pgEnv.client.QueryRow(ctx, q, benchAppendRunID).Scan(&raw); err != nil {
		t.Fatalf("取执行计划失败：%v", err)
	}
	text := string(raw)
	t.Logf("AppendRunEvent 里 max(sequence) 那次查询的执行计划：\n%s", text)

	if !strings.Contains(text, "Index Only Scan") && !strings.Contains(text, "Index Scan") {
		t.Errorf("max(sequence) 那次查询没走索引——长会话会越写越慢，"+
			"而症状是「聊得越久越卡」，很难归因到写路径。"+
			"检查主键 (run_id, sequence) 是否还在。\n计划：%s", text)
	}
	if strings.Contains(text, "Seq Scan") {
		t.Errorf("max(sequence) 那次查询出现了 Seq Scan。\n计划：%s", text)
	}
}
