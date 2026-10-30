// Read paths of run history: list the runs of a workspace, get one, list its events.
//
// # This part used to live in the lower half of core/ark.go
//
// That file used to mix two domains (credentials + run history). When an earlier refactoring step
// moved credentials to service/arkcred, **this half was deliberately left behind**, because it
// depended on chat.go's SyncRunEvents and fetching missing events from Ark. Now that edge is cut by
// Config.Backfill (see the file header of service.go), so this whole part could be moved here.
//
// ⚠️ **The backfill implementation did not move with it** (core/runbackfill.go: 3-second limit +
// 1-second cooldown + "failures do not enter the cooldown"). The rule is the same as for what
// arkverify.go keeps in core: it calls SyncRunEvents, which belongs to the chat domain, and the chat
// domain has not been split out yet.
package run

import (
	"context"
	"errors"
	"fmt"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// maxRunPageSize is the most ListRuns returns per page; anything above is cut to it instead of
// being an error (same approach as maxSuggestionPageSize: people paging through should not have to
// read our constants first).
//
// ⚠️ **There is no matching "default" constant, unlike feedback / suggestion**: for those two
// `Limit <= 0` means the default 50, while here 0 means no limit. See listRuns in the handler for
// why: truncating by default would silently break the numbers of 6 existing callers that expect
// everything.
//
// Where 200 comes from: 20 items on the first screen of the session list are enough, and 200 leaves
// ten times the room for "load more"; and the pathological case of 5000 items (55ms / 19MB) can
// only get 1/25 of it under this limit.
const maxRunPageSize = 200

// ListRuns lists the runs of a workspace, newest startedAt first.
//
// It also marks "the related agent was deleted / disabled" (Run.AgentDeleted / AgentDisabled); see
// markAgentDeleted.
//
// ⚠️ **This is the second enrich path alongside GetRun.** Both must mark it; changing only one shows
// up as "the list does not show it is frozen, only opening it does", and the session sidebar of the
// frontend ChatPage reads exactly the list.
//
// # Paging parameters are settled here
//
// The same place as ListFeedback / ListSuggestions (business rules should not be scattered in the
// handler), but **with the opposite default**: those two give 50 for `Limit <= 0`, this one gives
// "no limit". See listRuns in the handler for why: this route already had 6 callers expecting
// everything, and truncating by default would silently break their numbers.
//
// There must still be a limit: limit comes straight from the URL, and without a cap
// `?limit=99999999` would make one request load the whole workspace into memory, which is exactly
// what this paging is meant to fix.
//
// ⚠️ **Sorting happens in the Store, as a string comparison on startedAt**; that value must be a
// fixed-length ISO string with three decimals; see the comment on Store.ListRuns for why. This
// method does not reorder.
func (s *Service) ListRuns(
	ctx context.Context, workspaceID string, q persistmodel.RunQuery,
) ([]apimodel.Run, error) {
	// Negative means not given: `?limit=-1` is a typo, and a negative value in SQL's `limit` fails
	// right away (PG: LIMIT must not be negative), an error that does not point to "your parameter
	// is wrong".
	if q.Limit < 0 {
		q.Limit = 0
	}
	if q.Limit > maxRunPageSize {
		q.Limit = maxRunPageSize
	}
	if q.Offset < 0 {
		q.Offset = 0
	}

	runs, err := s.store.ListRuns(ctx, workspaceID, q)
	if err != nil {
		return nil, err
	}

	// Load all agents of this workspace at once into a map instead of GetAgent one by one; the
	// latter is N+1 on a list page. Runs are already filtered by workspaceID, and runs of one
	// workspace only point to agents of the same workspace, so one query is enough.
	agents, err := s.store.ListAgents(ctx, workspaceID)
	if err != nil {
		// fail-open, for the same reason as markAgentDeleted.
		s.log().Warn("列 agent 失败，运行历史暂不标记已删除", "workspace", workspaceID, "error", err.Error())
		return runs, nil
	}
	// Store the status, not just existence: disabled must also be marked in the list, and looking
	// it up again would be N+1 again.
	status := make(map[string]apimodel.AgentStatus, len(agents))
	for _, a := range agents {
		status[a.ID] = a.Status
	}
	for i := range runs {
		st, ok := status[runs[i].AgentID]
		if !ok {
			runs[i].AgentDeleted = true
			continue
		}
		runs[i].AgentDisabled = st.IsDisabled()
	}
	return runs, nil
}

// GetRun gets one run; returns ErrNotFound if it does not exist.
//
// The status allow list for backfill is in isSyncableRunStatus, **the same rule as
// ListRunEvents**; both must change together: changing only one shows up as "after refreshing the
// detail page, sometimes everything is filled in, sometimes not", depending on which endpoint is
// called first.
//
// Backfill goes through s.backfill (**the one with a short timeout given by the wiring code**); do
// not call the chat domain's SyncRunEvents directly here; see core/runbackfill.go for why.
func (s *Service) GetRun(ctx context.Context, id string) (apimodel.Run, error) {
	run, err := s.store.GetRun(ctx, id)
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			return apimodel.Run{}, fmt.Errorf("%w: 运行记录不存在", persistmodel.ErrNotFound)
		}
		return apimodel.Run{}, err
	}
	if s.shouldBackfill(run.Status, id) {
		// ⚠️ **Re-read unconditionally, not only when backfill succeeds.**
		//
		// A backfill timeout means "halfway through": events already fetched are stored as usual
		// (persistArkEvent uses a WithoutCancel ctx), and the run's status has already been changed
		// by applyEventToRun. Re-reading only when err == nil would hide those **already stored**
		// updates from this response; they show up after one refresh, so it looks like "the status
		// is wrong the first time it opens, right after a refresh".
		if err := s.backfill(ctx, id); err != nil {
			s.log().Warn("同步方舟运行事件失败", "run", id, "error", err.Error())
		}
		if updated, err := s.store.GetRun(ctx, id); err == nil {
			run = updated
		}
	}
	// **It must be here, after the re-read above**: `updated` is the raw run straight from the
	// Store, where AgentDeleted is always false; marking it earlier would be silently overwritten.
	s.markAgentDeleted(ctx, &run)
	return run, nil
}

// markAgentDeleted writes "the related agent was deleted / disabled" into derived fields.
//
// Agents are hard-deleted with no deleted_at, so this can only be computed when reading the Run,
// the same way as feishu.App.BoundAgentIDs. This field **must never be stored**: callers get a copy
// cloned by the Store (cloneRun), so changing it does not leak into runs.doc; but the other way
// round, **do not hand an enriched Run back to SaveRun**, or the derived value would be stored as a
// stale snapshot.
//
// **Only ErrNotFound counts; any other error keeps false (fail-open).** Calling a database hiccup
// "your agent was deleted" shows the user a false statement they cannot act on, and would also
// freeze chats across the whole workspace; that is much worse than missing a mark once.
//
// # Disabled is also computed here, for exactly the same reason as "deleted"
//
// The frontend ChatPage's frozen deliberately only uses the derived fields computed by the server,
// not the local agents list (see the comment there: a single source also avoids first-frame issues
// like "a flash while agents are still loading"). Disabled must reuse the same channel; otherwise
// the frontend would have to find the agent in the list and check status itself, which is exactly
// what that comment avoids, and it would create a second source of truth next to the gate in the
// backend's AppendRunEvents.
//
// It is still called markAgentDeleted: it is "fold the agent side's availability into the run";
// both derived fields come from the same source and the same query, and splitting them into two
// methods would add a query.
func (s *Service) markAgentDeleted(ctx context.Context, run *apimodel.Run) {
	agent, err := s.store.GetAgent(ctx, run.AgentID)
	if err == nil {
		run.AgentDisabled = agent.Status.IsDisabled()
		return
	}
	if errors.Is(err, persistmodel.ErrNotFound) {
		run.AgentDeleted = true
		return
	}
	s.log().Warn("查 agent 失败，这次运行暂不标记已删除", "run", run.ID, "error", err.Error())
}

// ListRunEvents lists the events of a run, by sequence ascending.
//
// When the run does not exist it returns an empty slice, not ErrNotFound; see the comment on the
// Store interface.
//
// ⚠️ **An event's Raw is passed out unchanged**: that column is byte-exact (text/bytea, not jsonb),
// and this method does no deserialize-and-serialize. See the comment on Store.ListRunEvents.
//
// # The status allow list for backfill must match ListActiveRuns
//
// ⚠️ **This once only accepted `running`, a hole with no error.** Under self_hosted, Ark switches to
// `idle + requires_action` on every tool call, and `applyEventToRun` sets the run to
// `requires_action` accordingly (see chat.go). So "SSE broke during tool execution", the most
// common place for the stream to break, fell exactly on the excluded status, and the tail events
// (including the idle with `end_turn`) **could never be filled in**.
//
// On the frontend: `groupTurns()` never gets the closing event, the last turn's `done` stays false
// forever, "after a turn only input and output remain" stops working, and the intermediate steps
// stay expanded. The endpoint returns 200 and the logs show nothing unusual; part of the data is
// just missing.
//
// The rule follows the three-state allow list of `ListActiveRuns` (running / rescheduling /
// requires_action); that convention literally says "the next person will easily miss
// requires_action", and that is exactly what was missed here.
//
// # Backfill goes through s.backfill, the same entry as GetRun
//
// **This is the second request in the same page load as GetRun** (the detail page first calls
// getRun, then listRunEvents), so each would hit Ark once: one page load pays two cross-network
// round trips, and the second is almost always wasted. The backfill cooldown exists for exactly
// this; see core/runbackfill.go.
func (s *Service) ListRunEvents(
	ctx context.Context, runID string, q persistmodel.EventQuery,
) ([]apimodel.RunEvent, error) {
	run, err := s.store.GetRun(ctx, runID)
	if err == nil && s.shouldBackfill(run.Status, runID) {
		if err := s.backfill(ctx, runID); err != nil {
			s.log().Warn("同步方舟运行事件失败", "run", runID, "error", err.Error())
		}
	}
	return s.store.ListRunEvents(ctx, runID, q)
}

// shouldBackfill decides "should we go to Ark now to fill in the tail events".
//
// All three conditions are required, **exactly equivalent to that if in core/ark.go before the
// move**:
//
//	s.backfill != nil            did the wiring connect Ark (originally written as s.runner != nil)
//	isSyncableRunStatus(status)  might this run still have a tail not filled in
//	!hasActiveStream(runID)      is someone subscribed to it over SSE (if so it would be wasted)
//
// It is one method instead of that if written in two places: GetRun and ListRunEvents are **two
// entry points of the same rule**; written separately they will drift apart someday, and the
// drift shows up as "sometimes everything is filled in, sometimes not", with no error, depending on
// which endpoint is called first.
func (s *Service) shouldBackfill(status apimodel.RunStatus, runID string) bool {
	if s.backfill == nil {
		return false
	}
	if !isSyncableRunStatus(status) {
		return false
	}
	return s.hasStream == nil || !s.hasStream(runID)
}

// isSyncableRunStatus decides whether this run might still have tail events not filled in.
//
// **Exactly the same allow list as Store.ListActiveRuns** (running / rescheduling /
// requires_action). If the two drift apart, "some sessions' tail events can never be filled in",
// with no error, so it is better hard-coded in one place than checked separately at each call site.
func isSyncableRunStatus(status apimodel.RunStatus) bool {
	switch status {
	case apimodel.RunRunning, apimodel.RunRescheduling, apimodel.RunRequiresAction:
		return true
	}
	return false
}
