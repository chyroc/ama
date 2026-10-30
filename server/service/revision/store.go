// Persistence contract of the entity revision (Revision) domain.
//
// # Declarations live with the domain, not in core/store.go
//
// The reason is exactly the same as for service/issue/store.go and service/suggestion/store.go:
// core/store.go is a shared file every resource domain goes through, and many parallel changes
// touch it. Piling one domain's method declarations there costs not "a big file" but **conflict
// surface**: two changes each move their own domain, yet their diffs collide in the same hunk.
//
// Now the declarations live here, and core/store.go only embeds one line `revisionsvc.Store`.
//
// # ⚠️ This set has only 5 methods, and **all are on entity_revisions itself**
//
// The biggest difference from the issue one: this domain has **no** GetAgent / GetSkill /
// GetSchedule.
//
// Before the move, the core side had them: `revisionEntityWorkspace` looked up the three entities
// by kind to confirm "does it still exist, which workspace does it belong to". Copying that here
// would make revision.Store declare three **tables owned by others**, and since this domain is
// downstream of those three entities, that would copy the read contract of agent/skill/schedule
// here; whenever any of those three domains is split into its own package, this file would have
// to change too.
//
// It was replaced by the Entity registry (see entity.go): whoever owns the table fills in "how to
// look it up". So this file goes back to one plain statement: **this domain only knows its own
// table**.
//
// ⚠️ This does not contradict the GetWorkspace in issue.Store; the rule is the same (written on
// that method): **reading someone else's table** may go into your own Store, **calling someone
// else's business logic** may not. issue only reads one row of workspaces; this domain, besides
// reading, must also **go through all the gates of UpdateX** on rollback (see the whole section
// in the file header of revision.go). That is business logic, not a single query.
//
// # ⚠️ Neither the PG nor the in-memory implementation needs a single line changed
//
// This relies on Go's implicit interfaces: both implementations (repository/dao/coredao,
// coremem) have the methods, so they satisfy this narrow interface automatically. **Do not add a
// `var _ revision.Store = ...` assertion to them**: that would make dao import service/revision,
// gluing back together the direction that was just split apart.
package revision

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Store is all the persistence methods this domain uses, **listing only the ones it uses**.
//
// The three entities (agent / skill / schedule) share this set of methods, told apart by kind.
// It is named revision rather than version; the reason is in the file header of
// domain/revision.go ("version" already has three different meanings in this repo).
type Store interface {
	// AppendEntityRevision appends a snapshot of the state before a change and returns the stored
	// row (including the assigned No).
	//
	// **No is assigned by the implementation; callers must not compute it**: that would be
	// "read current max + 1", a check-then-write. Multiple instances saving the same agent at
	// the same time would compute the same number, and "roll back to version 5" would no longer
	// have a single answer. The implementation must rely on the entity_revisions_no_key unique
	// constraint to catch this and retry; the rev.No passed in is ignored.
	//
	// ⚠️ **Callers must tolerate it failing**: failing to store a snapshot **must not make the
	// user's save fail** (see "recording failures always degrade" in Service.Record).
	AppendEntityRevision(ctx context.Context, rev apimodel.Revision) (apimodel.Revision, error)

	// ListEntityRevisions lists an entity's history, **newest first**, at most limit entries.
	//
	// ⚠️ **Revision.Snapshot is always empty in the result.** The list only renders names and
	// times, while one agent snapshot is a few KB and 20 of them are tens of KB; returning them
	// all would pay a constant cost for a rarely used page that is "only opened when something
	// went wrong". Use GetEntityRevision to fetch a single body.
	//
	// limit <= 0 means no limit. If the entity does not exist or never stored a snapshot, it
	// returns an empty slice, **not an error**.
	ListEntityRevisions(ctx context.Context, kind apimodel.RevisionKind, entityID string, limit int) ([]apimodel.Revision, error)

	// GetEntityRevision gets one snapshot by ID, **with the Snapshot body**. Returns ErrNotFound
	// if it does not exist.
	GetEntityRevision(ctx context.Context, id string) (apimodel.Revision, error)

	// DeleteEntityRevisions deletes all snapshots of an entity and returns how many were deleted.
	//
	// Used for "the entity was deleted": once the agent is gone, its history can never be rolled
	// back to anything. Not existing is not an error (idempotent).
	DeleteEntityRevisions(ctx context.Context, kind apimodel.RevisionKind, entityID string) (int64, error)

	// PurgeExcessRevisions is a global cleanup: keep only the latest keep entries per entity,
	// and return how many were deleted.
	//
	// **It keeps keep entries per entity**, not keep entries for the whole table: the latter
	// would let one frequently edited agent push out the history of every other entity, with no
	// error.
	//
	// ⚠️ **This is for the scheduled task; do not call it on the write path**: deleting
	// synchronously on the write path adds latency to every save, and "delete everything after
	// rank N" is itself a race under concurrent instances. See core/revisionpurge.go. keep <= 0
	// returns 0 directly (that would wipe all history, which is almost certainly a config error;
	// better to delete nothing than silently delete everything).
	PurgeExcessRevisions(ctx context.Context, keep int) (int64, error)
}
