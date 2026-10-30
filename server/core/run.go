// Run history: 3 forwarding methods. **The business logic has already moved to service/run.**
//
// # These empty shells are a transition state, not a design
//
// The logic lives in service/run; this file only passes calls through unchanged. The only
// reason to keep them is that **methods the handler cannot reach must still be covered by
// core**: the orchestrator layer's fallback interface (orchestrator.Fallback) requires all
// of these methods on *core.Service:
//
//	cmd/amad/orchestrator.go var _ orchestrator.Fallback = (*core.Service)(nil)
//
// Remove the methods from *Service and that assertion fails to compile.
//
// So this step deliberately moves only the logic and keeps the method set: **the value of
// this step is "run history business logic no longer lives in core"**, and the check is that
// `go list -deps ./service/run` does not contain core.
//
// ⚠️ **Do not add logic here.** Any check written here bypasses service/run: the two entry
// points give different answers for the same thing, and nothing reports an error. Add
// things to service/run.
//
// # ⚠️ This domain used to be spread over four files; it was not a single git mv
//
//	core/ark.go:583-882  ListRuns / GetRun / ListRunEvents / markAgentDeleted /
//	                     backfillFromArk / claimBackfill / markBackfilled /
//	                     Reset / Clear          ← main part
//	core/chat.go         AppendRunEvents / SyncRunEvents   ← **not moved**, see below
//	core/arkconsole.go   GetRunArkConsole                  ← **not moved**, see below
//
// Each of the two "not moved" items has a reason; neither was forgotten:
//
//   - **AppendRunEvents / SyncRunEvents are the write path and belong to the chat domain**
//     (they build an Ark client, store events one by one, and compute the status); the chat
//     domain is step 9b. This domain is downstream of them: they write events into the run,
//     and this domain only reads.
//   - **GetRunArkConsole (core/arkconsole.go) is mainly about credentials**: it reads the
//     run once only to get workspaceID / agentID to resolve the region. The other two
//     methods in the same file deal with the agent / skill domains, which were not split
//     yet, and the whole file was already judged "stays" in step 8.
package core

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// ListRuns lists the runs in a workspace, by startedAt descending. The real one is in service/run.
func (s *Service) ListRuns(
	ctx context.Context, workspaceID string, q persistmodel.RunQuery,
) ([]apimodel.Run, error) {
	return s.runs.ListRuns(ctx, workspaceID, q)
}

// GetRun gets one run, returning ErrNotFound if it does not exist. The real one is in service/run.
func (s *Service) GetRun(ctx context.Context, id string) (apimodel.Run, error) {
	return s.runs.GetRun(ctx, id)
}

// ListRunEvents lists the events of one run, by sequence ascending. The real one is in service/run.
func (s *Service) ListRunEvents(
	ctx context.Context, runID string, q persistmodel.EventQuery,
) ([]apimodel.RunEvent, error) {
	return s.runs.ListRunEvents(ctx, runID, q)
}
