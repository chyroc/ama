// Persistence contract of the scheduled task (Schedule) domain.
//
// # The declarations follow the domain instead of staying in core/store.go
//
// Exactly the same reason as service/issue/store.go, service/suggestion/store.go and
// service/revision/store.go: core/store.go is a file every resource domain has to go through, and
// many parallel changes pass through it. Piling one domain's method declarations there costs not
// "a big file" but **conflict surface**: two changes each moving their own domain end up with diffs
// colliding in the same hunk.
//
// Now the declarations live here, and core/store.go only embeds one line `schedulesvc.Store`.
//
// # ⚠️ This group includes the methods the scheduler needs, although the scheduler itself stays in core
//
// ClaimDueSchedules / ReleaseScheduleClaim / SchedulesForAgent are **never called by this domain's
// Service**: they are used by core/scheduler.go (claiming and releasing leases) and core/agent.go
// (cascading cleanup when an agent is deleted). By the "list only what is needed" rule they should
// have stayed in core/store.go.
//
// They were moved anyway, because the rule for this group is not "who calls them" but **"which
// table they describe"**: all three read and write schedules, and the claim semantics (see the
// passage on ClaimDueSchedules) are **two sides of the same invariant** as CreateSchedule /
// SaveSchedule: clearing next_run_at, consuming RunNowRequested, how leases expire; these must be
// read together with the CRUD methods. Splitting them into two files would only make the next person
// changing the claim SQL read half of it.
//
// Once run/chat are split out and scheduler.go moves into this package, the callers of these three
// methods come back to this package, and this passage can be deleted. **Until then they are part of
// this package's public surface**, and the core side keeps using them through embedding.
//
// # ⚠️ Neither the PG nor the memory implementation needs a single line changed
//
// This relies on Go's implicit interfaces: both implementations (repository/dao/coredao, coremem)
// have the methods and satisfy this narrow interface automatically. **Do not add a
// `var _ schedule.Store = ...` assertion to them**: that would make dao import service/schedule in
// reverse, gluing back the direction that was just split apart.
package schedule

import (
	"context"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Store is all the persistence methods this domain needs, **listing only the ones needed** (plus
// the scheduler's three; see the file header for why).
type Store interface {
	// GetWorkspace checks whether a workspace exists.
	//
	// ⚠️ **This is one of this domain's two "tables of others"** (the other is the agent pair below);
	// it is here on purpose, not out of laziness. CreateSchedule must reject missing workspaces.
	//
	// The rule is exactly the same as for GetWorkspace in issue.Store: **reading someone else's
	// table** may go into your own Store; **calling someone else's Service** (which has business
	// rules and side effects) must go up through the orchestrator. This is just an existence query.
	GetWorkspace(ctx context.Context, id string) (apimodel.Workspace, error)

	// GetAgent gets the related agent; returns ErrNotFound if it does not exist.
	//
	// Used in three places: checking the agent exists and is in the same workspace when creating /
	// changing a task (**a cross-workspace reference is overreach**: it would make a task run with
	// another workspace's credentials), computing "agent deleted" when reading a single item, and the
	// precondition check before a test run. Same as above, just reading a table.
	GetAgent(ctx context.Context, id string) (apimodel.Agent, error)

	// ListAgents lists all agents of a workspace.
	//
	// Only used by ListSchedules to mark "the related agent was deleted": load everything at once
	// into a set, **not one query each**, which would be N+1; same approach as ListRuns.
	ListAgents(ctx context.Context, workspaceID string) ([]apimodel.Agent, error)

	// -- The schedules table itself --------------------------------------------

	// ListSchedules lists all scheduled tasks of a workspace, in creation order.
	ListSchedules(ctx context.Context, workspaceID string) ([]apimodel.Schedule, error)
	// GetSchedule gets one by ID; returns ErrNotFound if it does not exist.
	GetSchedule(ctx context.Context, id string) (apimodel.Schedule, error)
	CreateSchedule(ctx context.Context, sch apimodel.Schedule) (apimodel.Schedule, error)
	// SaveSchedule overwrites; returns ErrNotFound if it does not exist.
	SaveSchedule(ctx context.Context, sch apimodel.Schedule) (apimodel.Schedule, error)
	// DeleteSchedule deletes a task; a missing one is not an error (safe to repeat).
	DeleteSchedule(ctx context.Context, id string) error

	// ClaimDueSchedules claims tasks that are due and returns the ones this call got.
	//
	// **This is the pivot of correctness for the whole scheduling path, not a query method.** amad
	// runs as multiple instances (see CLAUDE.md at the repository root); N processes find the same task
	// due in the same second. Claiming must let **exactly one** win, otherwise the user gets N
	// duplicate reports, and single-instance tests are always green.
	//
	// Semantics: tasks with status=active, next_run_at <= now and no valid lease are atomically marked
	// "owned by owner, lease until now+leaseFor", and **next_run_at is cleared** at the same time.
	//
	// **Tasks with RunNowRequested true are claimed even when paused**; that is the "manual test run"
	// branch (see apimodel.Schedule.RunNowRequested). Claiming consumes the flag too, so one click
	// gives exactly one round; after the run, finish sees status is still paused and does not
	// schedule a next one. **Do not change it to look only at next_run_at**: the next_run_at left over
	// from before the pause is still in the past, and every task would run at once the moment it is
	// paused.
	//
	// Clearing next_run_at is required: one fire may run for several minutes, and keeping it would
	// make every scan period see "it is due" again. An in-process map cannot stop this with multiple
	// instances. The lease covers "claimed but the process died": after it expires someone else can
	// claim it again.
	//
	// limit <= 0 means no limit. owner is the caller's instance id, only for debugging.
	ClaimDueSchedules(ctx context.Context, owner string, now time.Time, leaseFor time.Duration, limit int) ([]apimodel.Schedule, error)

	// ReleaseScheduleClaim clears a task's claim lease.
	//
	// **It must be called after each round**, otherwise the lease only expires after 15 minutes, and
	// any task with an interval shorter than the lease (e.g. "every 5 minutes") cannot be claimed
	// before then; the symptom is "the interval is quietly stretched to 15 minutes", with nothing in
	// the logs. This trap was caught by TestSchedulerAutoPausesAfterFailures: pushing five ticks in a
	// row, only the first one really ran.
	//
	// **Call SaveSchedule first, then Release**: the other way round, for a moment next_run_at is
	// still the old (due) one while the lease is gone, and another instance would claim it right away
	// and run it again.
	//
	// A missing one is not an error (safe to repeat).
	ReleaseScheduleClaim(ctx context.Context, id string) error

	// SchedulesForAgent lists the task IDs under an agent, in creation order.
	//
	// For the cascading cleanup when an agent is deleted: without the agent, its scheduled tasks can
	// never produce results again.
	SchedulesForAgent(ctx context.Context, agentID string) ([]string, error)
}
