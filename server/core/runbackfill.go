// Limits and cooldown for backfilling Ark events on the "open run detail page" path.
//
// # Why this stays in core while the read path moved to service/run
//
// The test is "who does it call". The main caller here is `s.SyncRunEvents`, the **write path
// of the chat domain** (build the Ark client, persist events one by one, compute the status),
// and the chat domain has not been split out yet (step 9b). Moving this into service/run would
// make that read-only domain immediately need a narrow interface to reach the chat domain's
// write methods, and that would have to be rewritten again when 9b splits the package. Same
// rule as arkverify.go / arkconsole.go, which stayed behind in step 8.
//
// The other way around, all service/run needs is "someone backfills once when it should", so it
// declares a `Backfill func(ctx, runID) error`, which core/service.go fills with
// `s.backfillFromArk` at wiring time. It injects a function instead of declaring an interface;
// see the file header of service/run/service.go for why.
//
// ⚠️ **The limit, the cooldown and "failures do not enter cooldown" all live on this side.**
// service/run only decides "should we backfill", not "for how long". So change timeouts here,
// and change "which states trigger a backfill" over there.
package core

import (
	"context"
	"time"
)

// backfillTimeout is the time limit for backfilling Ark events inside **the request the user is
// waiting on**.
//
// # Why it must exist
//
// Backfilling calls Ark's HTTP API synchronously (step 4 of SyncRunEvents). Measured in
// production, this made `/api/runs/{runId}` the slowest endpoint: P50 320ms, **P95 1398ms**,
// and `/api/runs/{runId}/events` P50 412ms, **P95 1157ms** (44 deduplicated samples; static
// files in the same logs had P50 0~3ms, max 8ms, so this is real server-side work, not network
// or proxy).
//
// ⚠️ **"No explicit timeout" does not mean "no timeout", it means "10 minutes"**: the Ark SDK's
// NewClientConfig ships with defaultTimeout = 10 minutes (see the postmortem in arkrun.go
// instance(); every "chat stream interrupted" case seen in production stopped at exactly
// 10m0.00x s). The arkCallTimeout in LiveArkRunner.ListEvents is **30 seconds**. For "a user
// opens a page", waiting 30 seconds is no different from waiting 10 minutes: both mean "this
// page does not load". So this path needs its own, much shorter limit.
//
// # Where 3 seconds comes from
//
// Apart from the Ark step, everything in a backfill is local PG (measured: reading back one
// session with 582 events takes 1.77ms, 3000 events 8.1ms), so a healthy backfill takes tens of
// milliseconds, and 3 seconds is already dozens of times that. Normal requests never hit this
// limit; only a really unstable Ark does.
//
// We pick 3 seconds and not less because **a timeout means the latency of this backfill was
// paid for nothing** (events fetched halfway are still persisted, but the tail of that round is
// most likely incomplete). A 1-second limit would also cut off "Ark is a bit slow", so the
// detail page would often show "running" when the run has actually ended. That trades a
// performance problem for a correctness problem.
const backfillTimeout = 3 * time.Second

// backfillCooldown is the minimum gap between two backfills of the same run. It removes **the
// second Ark round trip within a single page open**.
//
// # What it blocks
//
// Opening a run detail page sends two requests in sequence: first `GET /api/runs/{id}`, then,
// after that returns, `GET /api/runs/{id}/events` (the fetch callback in
// web/src/pages/RunDetailPage.tsx). Both go through **the same** check (shouldBackfill in
// service/run), so each one calls Ark. **One page open pays for two cross-network round
// trips**, and the second one is almost always wasted: the first backfill already wrote the
// events to the database (persistArkEvent persists synchronously), so the second gets back the
// same batch and every event hits deduplication.
//
// So we record "this run was just backfilled", and a second call inside the window is skipped.
// **It still reads from the database and no data is missing**; it just does not call Ark one
// more time. This cuts the detail page's Ark round trips in half.
//
// # Where 1 second comes from, and why it must not be larger
//
// The window to cover is the single round trip "GetRun returns → frontend sends the events
// request", i.e. one local RTT (tens to hundreds of milliseconds). 1 second covers it, and
// **a real person refreshing by hand (> 1 second) is not blocked**. That matters: a user
// refreshes because they want the latest progress; blocking their backfill turns into
// "refreshing does nothing".
//
// ⚠️ **The time is recorded only after a successful backfill** (see backfillFromArk): if
// failures/timeouts were recorded too, one Ark hiccup would skip every backfill for the next
// full second, and this "optimization" would block the channel that is supposed to self-heal.
const backfillCooldown = time.Second

// backfillFromArk backfills Ark events once **inside the request the user is waiting on**, with
// a time limit and a cooldown.
//
// This is the only entry point the two read paths in service/run should use (core/service.go
// injects it as runsvc.Config.Backfill at wiring time). **Do not call SyncRunEvents directly on
// an HTTP path**: that method sets no limit of its own (Feishu's recovery path wants "fill in as
// much as possible", not "return as fast as possible"; see its comment), so calling it directly
// wires the 30-second Ark timeout into the user's page.
//
// # Why "short timeout as a safety net" and not "make it async in the background"
//
// Async (return database data right away, push the backfill to the background) looks cleaner,
// but on this frontend it is **a user-visible regression with no self-healing path**. The run
// detail page **has neither SSE nor polling** (RunDetailPage fetches once with useAsyncData and
// only re-runs when runId changes; the only run event stream on the whole site is in ChatPage).
// So with async, the first screen gets stale values that were not backfilled yet:
//
//   - the status chip shows "running" with a pulsing dot (RunStatusChip's LIVE set includes
//     running);
//   - end time shows "in progress", duration shows "—" (endedAt / durationMs missing);
//   - the last turn in the chat view stays at done=false, and the intermediate steps are fully
//     expanded instead of collapsed (timeline.ts).
//
// And these **cannot correct themselves**: no stream, no polling, no focus refetch; only a
// manual refresh by the user fixes them. That trades "the page is 1 second slower" for "the page
// shows the wrong state until the user clicks again". The latter is worse, and it is silent. To
// really go async, the detail page first needs a push or polling channel, which is a separate
// job.
//
// On timeout, **events already persisted are not rolled back**: persistArkEvent uses its own
// budget via context.WithoutCancel, so whatever was fetched is stored. Backfilling is
// idempotent (AppendRunEvent deduplicates through run_events_event_id_idx), so "fetched
// halfway" is a safe intermediate state, and the next call continues from there.
func (s *Service) backfillFromArk(ctx context.Context, runID string) error {
	if s.runner == nil {
		return nil
	}
	if !s.claimBackfill(runID) {
		// The second call within the same page open; skip. **Not an error**: the data in the database is complete.
		return nil
	}

	// ⚠️ The limit derived here is for the "call Ark" step, **not for persisting**.
	// Inside SyncRunEvents each event is persisted through persistArkEvent, which starts its own
	// budget with WithoutCancel, so this ctx expiring does not drop events already fetched along
	// with it (precedent: runwatcher.go retireMissingRun). This is important: cancelling the
	// database writes on timeout too would mean the latency was paid for nothing.
	callCtx, cancel := context.WithTimeout(ctx, backfillTimeout)
	defer cancel()

	if err := s.SyncRunEvents(callCtx, runID); err != nil {
		return err
	}
	s.markBackfilled(runID)
	return nil
}

// claimBackfill decides "should this run be backfilled now", and if so, claims this round.
//
// In-process state, **of the allowed kind**: under the "this is a distributed service" rule in
// CLAUDE.md, a process may only hold things where "losing them only makes things slower", and
// this table is exactly that. Losing it (switching instance, pod rebuilt) at worst costs one
// extra Ark call; functionality is not affected at all. With several pods in production, each
// keeps its own table, which only means each saves one fewer round trip; no backfill is missed
// and nothing is written twice (AppendRunEvent is idempotent).
func (s *Service) claimBackfill(runID string) bool {
	s.backfillMu.Lock()
	defer s.backfillMu.Unlock()
	if last, ok := s.lastBackfill[runID]; ok && time.Since(last) < backfillCooldown {
		return false
	}
	return true
}

// markBackfilled records "this run was just backfilled successfully".
//
// **Call it only after success**, see backfillCooldown for why: if failures were recorded too,
// one Ark hiccup would skip every backfill for a whole cooldown window, blocking self-healing.
func (s *Service) markBackfilled(runID string) {
	now := time.Now()

	s.backfillMu.Lock()
	defer s.backfillMu.Unlock()
	if s.lastBackfill == nil {
		s.lastBackfill = make(map[string]time.Time)
	}
	// Clean up expired entries while here; otherwise this table grows without bound with "the
	// total number of runs ever backfilled". That is a slow memory leak that gets worse the
	// longer the process lives, and it reports no error.
	// The cooldown window is only 1 second, so normally this table only holds the few runs
	// "someone is looking at right now".
	for id, t := range s.lastBackfill {
		if now.Sub(t) >= backfillCooldown {
			delete(s.lastBackfill, id)
		}
	}
	s.lastBackfill[runID] = now
}
