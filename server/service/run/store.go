// Persistence contract of the run history (Run) domain.
//
// # The declarations follow the domain instead of staying in core/store.go
//
// Exactly the same reason as service/issue/store.go and service/revision/store.go: core/store.go is
// a file every resource domain has to go through, and many parallel changes pass through it.
// Piling one domain's method declarations there costs not "a big file" but **conflict surface**:
// two changes each moving their own domain end up with diffs colliding in the same hunk.
//
// Now the declarations live here, and core/store.go only embeds one line `runsvc.Store`.
//
// # ⚠️ This domain's Store has a table belonging to someone else: ListAgents / GetAgent
//
// The rule is the one written on GetWorkspace in service/issue/store.go: **reading someone else's
// table** may go into your own Store; **calling someone else's business logic** may not.
//
// This domain reads agents only to compute two derived fields (Run.AgentDeleted / AgentDisabled;
// see markAgentDeleted in service.go); those are two plain queries that go through none of the
// agent domain's gates. Switching to an Entity registry (the revision approach) would be
// over-engineering here: revision needs it because rollback must **go through all of UpdateX's
// gates**, while this domain only reads.
//
// ⚠️ Conversely, when the agent domain is split out later, these two methods **need no change in
// this file**: they are read contracts, and what the agent domain moves is business logic.
//
// # ⚠️ This group deliberately does not include CreateRun / SaveRun / AppendRunEvent
//
// Those three are **write paths**, called from chat.go / worker.go / scheduler.go / runwatcher.go,
// which all still live in core; that is for a later step. This step only moves the **read-only**
// "run history" domain, so it only lists the four methods it really needs. Listing more would grow
// a batch of declarations with no callers in this file before that later step, which is exactly
// the kind of zero-error "written but never called" thing.
//
// # ⚠️ Neither the PG nor the memory implementation needs a single line changed
//
// This relies on Go's implicit interfaces: both implementations (repository/dao/coredao, coremem)
// have the methods and satisfy this narrow interface automatically. **Do not add a
// `var _ run.Store = ...` assertion to them**: that would make dao import service/run in reverse,
// gluing back the direction that was just split apart.
package run

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// Store is all the persistence methods this domain needs, **listing only the ones needed**.
type Store interface {
	// ListRuns lists the runs of a workspace, newest startedAt first.
	//
	// ⚠️ **Descending order is a string comparison on startedAt**, so that value must be a
	// fixed-length ISO string with three decimals (the shape apimodel.NowISO / apimodel.FormatISO
	// produce). Mixing in time.RFC3339, which drops `.000`, **changes the sort result directly**:
	// `.`(0x2E) < `Z`(0x5A), so with both shapes mixed, the order is no longer decided by time.
	// See "Timestamps: unit and format" in CLAUDE.md.
	ListRuns(ctx context.Context, workspaceID string, q persistmodel.RunQuery) ([]apimodel.Run, error)

	// GetRun gets a run by ID; returns ErrNotFound if it does not exist.
	GetRun(ctx context.Context, id string) (apimodel.Run, error)

	// ListRunEvents lists the events of a run, by sequence ascending.
	//
	// **When the run does not exist it returns an empty slice, not ErrNotFound**: the frontend's
	// listRunEvents returns an array, not a nullable, and a 404 would make the page throw.
	//
	// ⚠️ **RunEvent.Raw is byte-exact**: that column in the database is text/bytea, not jsonb,
	// because jsonb reorders keys and squeezes whitespace, which is exactly not exact. The contract
	// case EventRawIsByteExact guards this. This domain passes it out unchanged; **do not do any
	// json deserialize-and-serialize on this path**. See the notes in model/persistmodel.
	ListRunEvents(ctx context.Context, runID string, q persistmodel.EventQuery) ([]apimodel.RunEvent, error)

	// ListAgents lists the agents of a workspace, so the list page can build a map once to mark
	// "deleted / disabled".
	//
	// **Do not change it to GetAgent one by one**: that is N+1 on a list page. See "This domain's
	// Store has a table belonging to someone else" in the file header.
	ListAgents(ctx context.Context, workspaceID string) ([]apimodel.Agent, error)

	// GetAgent gets an agent by ID; returns ErrNotFound if it does not exist.
	// The single-item path (GetRun) uses it to compute the same two derived fields.
	GetAgent(ctx context.Context, id string) (apimodel.Agent, error)
}
