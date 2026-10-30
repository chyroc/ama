// Snapshots and "did this save actually change anything": the two checks before recording history.
//
// # These two functions belong to this domain, not to revision
//
// The file header of core/revision.go already says it: snapshotX / sameXDraft are "which fields of my
// entity should go into history", knowledge that belongs to the domain owning the entity;
// service/revision only takes an any as payload and does not know apimodel.Schedule at all. So they
// move with this domain instead of staying there.
//
// The agent and skill versions are still in core/revision.go and core/revisiondiff.go and will move
// when those are split out.
//
// # ⚠️ Compare the shape **after normalization**, otherwise the result is always "changed"
//
// The draft is deserialized from the HTTP request body, while the database stores the shape after
// storing. Comparing without normalizing would judge every save as "changed", so even "saves that
// changed nothing" would each store an identical snapshot, filling the limit of 20 for nothing and
// pushing out the version you really want. **This is not an optimization, it is correctness**: it
// destroys exactly the reason this feature exists, with no error (the history is full of duplicate
// snapshots, and nobody notices).
package schedule

import (
	"reflect"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Snapshot turns the **current** state of a scheduled task into a draft that can be rolled back to.
//
// ⚠️ **Only the ScheduleDraft fields are taken; run-time state never goes into the snapshot**:
// NextRunAt / RunNowRequested are columns of the claim protocol (see schedules.sql), and RunCount /
// FailCount / ConsecutiveFailures / LastRunID are execution history. Restoring an expired NextRunAt
// would make the task jump the gun and run a round at once; restoring the counters would reset
// "failed 4 times in a row, about to auto-pause" to 0, quietly removing the cascade protection.
//
// **It is public because the rollback entry needs it** (the schedule entry in core/revision.go uses
// it to decide "is this rollback the same as the current state"). It is also the only source of
// truth for "which fields get rolled back": to make a field added to Schedule roll back too, add a
// line here; missing it shows up as "after rollback that field stays as it is", with no error and no
// crash. TestSnapshotCoversEveryDraftField in core/revision_test.go guards this with reflection.
func Snapshot(sch apimodel.Schedule) apimodel.ScheduleDraft {
	return apimodel.ScheduleDraft{
		AgentID:        sch.AgentID,
		Name:           sch.Name,
		Spec:           sch.Spec,
		Prompt:         sch.Prompt,
		ContextSummary: sch.ContextSummary,
		Delivery:       sch.Delivery,
		Status:         sch.Status,
	}
}

// sameScheduleDraft decides whether this draft, once stored, would equal the current state.
//
// before is the result of Snapshot(current entity) (already the normalized stored shape), draft is
// the one from the request body.
//
// ScheduleDraft is all scalars plus two embedded structs of pure scalars, no slices, so `==` would
// really be enough. It still uses DeepEqual to match the agent / skill versions: when a slice field
// is added to ScheduleDraft someday, `==` **simply stops compiling** (a good thing, it gets noticed),
// while someone changing it to a field-by-field comparison just to make it compile could easily miss
// one.
func sameScheduleDraft(before, draft apimodel.ScheduleDraft) bool {
	after := apimodel.ScheduleDraft{
		AgentID:        draft.AgentID,
		Name:           strings.TrimSpace(draft.Name),
		Spec:           draft.Spec,
		Prompt:         draft.Prompt,
		ContextSummary: draft.ContextSummary,
		Delivery:       draft.Delivery,
		Status:         before.Status,
	}
	// Same as UpdateScheduleWithReason: Status is only assigned when non-empty.
	if draft.Status != "" {
		after.Status = draft.Status
	}
	normalized := before
	normalized.Name = strings.TrimSpace(before.Name)
	return reflect.DeepEqual(normalized, after)
}
