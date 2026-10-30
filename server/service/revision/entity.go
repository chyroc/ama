// "Who owns this entity": the single seam between this domain and agent / skill / schedule.
//
// # Why it is needed: this domain is downstream of three entities, not a leaf
//
// The issue and suggestion domains are leaves; moving them out was all it took. Revision history
// is not: by nature it touches other domains' entities, and **in both directions**:
//
//	direction one  when agent/skill/schedule is saved or deleted, record a version / clear history
//	direction two  on rollback, this domain must write the snapshot back to those three entities
//
// Direction one is **others calling this domain**, which needs no mechanism: whoever wants to
// record a version takes this domain's Service and calls Record / Drop, and the interface is
// declared by the **consumer** in the shape it needs (that is how the core side uses it today,
// same as the ChatOrigins / DepartmentLookup precedents of "declare the interface locally, wire it
// in cmd/amad").
//
// The part that really needs design is direction two. **Zero imports between domain packages is
// a hard rule**, so this package cannot import service/agent; the write-back can only be a
// function supplied by the side that owns the entity, filled in at wiring time.
//
// # ⚠️ One table covers both switches, not two tables
//
// Before the move, the core side had **two** switches with exactly the same structure, each
// covering the same three kinds:
//
//	revisionEntityWorkspace   check whether the entity exists and which workspace it belongs to (read endpoints need it for 404)
//	RollbackRevision          write the snapshot back
//
// While they were separate there was a silent failure mode: **adding a fourth entity and updating
// only one switch**. Add read but not write, and history lists fine but clicking rollback says
// "unsupported entity type"; the other way round, the list is always empty but rollback works.
// Both compile, both with zero errors.
//
// Now one kind is one Entity slot, and **both functions must be given together**: missing one
// makes New return an error, so it fails at wiring time. A fourth entity is either filled in
// completely or not registered at all; the in-between state does not exist in the types.
//
// A side benefit is that this domain's Store therefore **does not need to declare GetAgent /
// GetSkill / GetSchedule** (those three are other domains' tables), and does not need
// IsBuiltinSkill injected either: the rule "built-in skills have no history" belongs to the skill
// domain and is written in the Locate of the skill slot. See the package comment in store.go.
//
// # ⚠️ No new registration mechanism was invented
//
// The shape is **copied from shared/gopkg/resource** (table filled at wiring time, read-only at
// runtime, no locks, a missing slot fails at wiring time), but it does not reuse that table, and
// no new shared package was created. Three reasons:
//
//   - resource.Registry answers "which workspace does this id belong to", with one function per
//     slot. Rollback is a second function; putting it there would hang something unrelated to
//     authorization onto the path every authorization check goes through.
//   - The keys are different things: resource.Kind is an int iota used by authz, while this
//     domain's key is apimodel.RevisionKind ("agent"/"skill"/"schedule", **which goes into HTTP
//     and the frontend union type**). Forcing them into one table needs an extra kind mapping
//     layer, and if that mapping is wrong, nothing reports it.
//   - This domain's key set is **closed** (apimodel.ValidRevisionKind has exactly three), so here
//     we can do a check resource cannot: **a missing slot is an error**. resource's kinds are
//     open, so it can only panic on duplicate registration.
//
// A small table that serves only this domain, with a closed key set, costs less than squeezing
// two jobs into one general registry.
package revision

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// Entity is the slot for "how revision history uses one kind of entity", filled in by the side
// that owns it.
//
// ⚠️ **Both functions are required**, for the reason in the file header: allowing them to be given
// separately allows a half-filled state of "can read, cannot roll back", with zero errors.
type Entity struct {
	// Locate confirms the entity still exists and returns the workspace it belongs to.
	//
	// Read endpoints rely on it to tell "this agent was deleted" apart from "it has no history
	// yet": the latter is an empty list, the former must be 404. **Do not make it always return
	// nil to save a query**: that would let a deleted entity still list its history.
	//
	// If the entity does not exist, return an error wrapping persistmodel.ErrNotFound.
	// If it "exists but by nature has no history" (like built-in skills), return
	// persistmodel.ErrPrecondition: it is visible in the list, so saying "does not exist" is wrong.
	Locate func(ctx context.Context, entityID string) (workspaceID string, err error)

	// Rollback writes a snapshot back to this entity and returns the entity after the write.
	//
	// ⚠️⚠️ **The implementation must go through the domain's UpdateX, not call store.SaveX
	// directly.** This is the easiest place in the whole feature to get wrong, and getting it
	// wrong gives **zero errors**: SaveX skips validation, the authorization gate, the NextRunAt
	// recomputation, and the "state before rollback" snapshot. The full list is in the file header
	// of revision.go; the item "changes to injected authorizations must pass the gate" is a silent
	// privilege escalation hole.
	//
	// snapshot is the raw bytes of Revision.Snapshot, **decoded by the implementation itself**:
	// only the owner knows whether it is an AgentDraft, a SkillDraft or a ScheduleDraft.
	// Decode with DecodeSnapshot; do not build your own error message.
	//
	// ⚠️ **The implementation must not call Record again itself.** The "state before rollback" is
	// recorded by the record call inside UpdateX (just pass reason as
	// apimodel.RevisionReasonRollback); recording it again yourself stores two snapshots with
	// identical content. This is not a guess: TestRollbackAlsoRecordsRevision caught it directly
	// ("expected 2, got 3").
	Rollback func(ctx context.Context, entityID string, snapshot []byte) (any, error)
}

// DecodeSnapshot decodes snapshot bytes into the owner's own draft type.
//
// It exists for one reason only: **so the message "this revision is corrupted" lives in one
// place**. The three entities each decode their own type, but users should see the same message,
// and it must be ErrPrecondition (the handler answers 412 based on it). Written three times, one
// copy will sooner or later become ErrInvalidArgument or forget to wrap the sentinel error, and
// then the frontend pops up "server error".
func DecodeSnapshot(snapshot []byte, dst any) error {
	if err := json.Unmarshal(snapshot, dst); err != nil {
		return fmt.Errorf("%w: 这份历史版本已损坏，无法回滚: %v", persistmodel.ErrPrecondition, err)
	}
	return nil
}

// entityOf returns the slot for a kind.
//
// ⚠️ **An unknown kind returns an error.** New already checks that all three slots are present,
// so getting here can only mean the kind itself is invalid (for example "workflow" passed in the
// URL), which is ErrInvalidArgument.
func (s *Service) entityOf(kind apimodel.RevisionKind) (Entity, error) {
	ent, ok := s.entities[kind]
	if !ok {
		return Entity{}, fmt.Errorf("%w: 不支持的实体类型 %q", persistmodel.ErrInvalidArgument, kind)
	}
	return ent, nil
}
