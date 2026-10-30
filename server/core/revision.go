// Entity revision history (Revision): forwarding methods + the per-entity slot for each of the
// three entities.
//
// # The business logic has already moved to service/revision
//
// This file has two things left, of completely different nature; do not mix them up:
//
//  1. 5 forwarding methods (ListRevisions / GetRevision / RollbackRevision /
//     recordRevision / dropRevisions): **transitional**, see below.
//  2. The three slots in registerRevisionEntities, plus the three snapshotX functions:
//     **not transitional**. They belong to the agent / skill / schedule domains and
//     will move out together with each of those domains when it is split into its own package.
//
// ## 1. The 5 forwarding methods are a transitional state, not a design
//
// The logic lives in service/revision; this file only forwards calls as is. The only reason to
// keep them is that **methods the handler cannot reach still need core as a fallback**: the
// orchestration layer's fallback interface (orchestrator.Fallback) requires those three public
// methods on *core.Service:
//
//	cmd/amad/orchestrator.go var _ orchestrator.Fallback = (*core.Service)(nil)
//
// Remove the methods from *Service and that assertion fails to compile.
//
// So this step deliberately moves only the logic and leaves the method set alone: **the value of
// this step is "revision business logic no longer lives in core"**, checked by `go list -deps
// ./service/revision` not containing core.
// Those 3 public empty shells will be deleted in the separate MR that "moves handlers out of core".
//
// ⚠️ **Do not add logic here.** It looks like a normal revision.go, but any decision written
// here bypasses service/revision. Two entry points would give different answers for the same
// thing, and nothing would report an error.
//
// ## ⚠️ 2. recordRevision / dropRevisions will not be deleted with them
//
// They are **for agent.go / schedule.go in this package** (record a version before save, clear
// history after delete), not for handlers. So the "move handlers out of core" step cannot remove
// them; they wait until agent / skill / schedule are each split out, and move with their callers.
//
// Their future shape is already decided: **the consumer declares the narrow interface it needs**
// (only two methods, Record / Drop), and the wiring layer passes in *revisionsvc.Service. This
// follows the same precedent as ChatOrigins / DepartmentLookup, "declare the interface locally,
// wire it in cmd/amad" (see core/service.go).
//
// ⚠️ **Do not declare that interface ahead of time now.** An interface should be declared by
// **its user**, and right now the user is still in core and holds the concrete type
// s.revisions. Declaring an interface now that only we implement and only we use is just an
// extra layer of indirection, and it would be thrown away and rewritten when agent actually
// moves. That is normal Go practice ("accept interfaces" is about **parameters**, not fields).
package core

import (
	"context"
	"fmt"

	"github.com/chyroc/ama/server/model/apimodel"
	revisionsvc "github.com/chyroc/ama/server/service/revision"
	schedulesvc "github.com/chyroc/ama/server/service/schedule"
)

// ---------------------------------------------------------------------------
// Forwarding: the three used by handlers (transitional)
// ---------------------------------------------------------------------------

// ListRevisions lists an entity's history. The real implementation is in service/revision.
func (s *Service) ListRevisions(
	ctx context.Context, kind apimodel.RevisionKind, entityID string,
) ([]apimodel.Revision, error) {
	return s.revisions.ListRevisions(ctx, kind, entityID)
}

// GetRevision fetches a single snapshot, with its body. The real implementation is in service/revision.
func (s *Service) GetRevision(
	ctx context.Context, kind apimodel.RevisionKind, entityID, revisionID string,
) (apimodel.Revision, error) {
	return s.revisions.GetRevision(ctx, kind, entityID, revisionID)
}

// RollbackRevision switches an entity back to a historical version. The real implementation is in service/revision.
func (s *Service) RollbackRevision(
	ctx context.Context, kind apimodel.RevisionKind, entityID, revisionID string,
) (any, error) {
	return s.revisions.RollbackRevision(ctx, kind, entityID, revisionID)
}

// ---------------------------------------------------------------------------
// Forwarding: the two used by agent.go / schedule.go in this package (they move with their
// callers, not deleted with the handlers)
// ---------------------------------------------------------------------------

// recordRevision stores a snapshot of the state before the change. The real implementation is
// in service/revision.
//
// ⚠️ **No error return, on purpose**: failing to store history should not fail the user's save.
// The full argument is on revisionsvc.Service.Record. Getting the direction wrong reports no
// error, and tests stay green.
func (s *Service) recordRevision(
	ctx context.Context,
	kind apimodel.RevisionKind,
	workspaceID, entityID, summary string,
	payload any,
	reason apimodel.RevisionReason,
) {
	s.revisions.Record(ctx, kind, workspaceID, entityID, summary, payload, reason)
}

// dropRevisions deletes all history of an entity. The real implementation is in service/revision.
//
// ⚠️ Same as above, no error on purpose: the agent delete already succeeded, and it should not
// be reported as failed because "history was not fully cleared".
func (s *Service) dropRevisions(ctx context.Context, kind apimodel.RevisionKind, entityID string) {
	s.revisions.Drop(ctx, kind, entityID)
}

// defaultRevisionKeep is how many history entries each entity keeps by default.
//
// It is only an accessor for apimodel.MaxRevisionsPerEntity. It exists so revisionpurge.go does
// not have to reference the domain constant directly: **the limit has a single source of truth**,
// and the cleanup job and the read API must use the same value. Writing a literal in each place
// is an easy slip, and after that an inconsistency like "the UI shows 20, cleanup keeps 50"
// reports no error.
func defaultRevisionKeep() int { return apimodel.MaxRevisionsPerEntity }

// ---------------------------------------------------------------------------
// The slot for each of the three entities: how to look it up, how to write a snapshot back
// ---------------------------------------------------------------------------

// registerRevisionEntities hands over three slots for revisionsvc.New to wire up.
//
// **This is not transitional**: this domain is downstream of agent / skill / schedule, and domain
// packages do not import each other, so "how to look up, how to write back" can only be provided
// by the side that owns the entity. Full argument in the file header of
// service/revision/entity.go. These three slots will move out with each of those domains when it
// is split into its own package.
//
// ⚠️ **All three slots are required**; with one missing, revisionsvc.New returns an error and the
// service does not start. That is on purpose: the result of a missing slot is not a crash but
// **that entity type silently loses its history feature**. The list is always empty, rollback
// says "unsupported entity type", while this side keeps recording snapshots, so data piles up in
// the database and none of it shows in the UI.
func (s *Service) registerRevisionEntities() map[apimodel.RevisionKind]revisionsvc.Entity {
	return map[apimodel.RevisionKind]revisionsvc.Entity{
		apimodel.RevisionKindAgent: {
			Locate: func(ctx context.Context, id string) (string, error) {
				a, err := s.store.GetAgent(ctx, id)
				if err != nil {
					return "", fmt.Errorf("%w: Agent 不存在", ErrNotFound)
				}
				return a.WorkspaceID, nil
			},
			Rollback: func(ctx context.Context, id string, snapshot []byte) (any, error) {
				var draft apimodel.AgentDraft
				if err := revisionsvc.DecodeSnapshot(snapshot, &draft); err != nil {
					return nil, err
				}
				// ⚠️ **Go through updateAgent, not store.SaveAgent**: that path bypasses the
				// admin gate and validation, and leaves Version unchanged. See the file header of
				// service/revision/revision.go for why.
				//
				// This is also where "Version is never rolled back" lands: the draft has no such
				// field at all, and updateAgent does `a.Version++` as usual when it applies it.
				//
				// ⚠️ **"The state before rollback" is recorded by the recordRevision call inside
				// updateAgent**; do not record it again here. That would record two snapshots with
				// the same content (measured: TestRollbackAlsoRecordsRevision then reported
				// "expected 2, got 3"). reason is passed as rollback, so that snapshot is marked
				// rollback.
				return s.updateAgent(ctx, id, draft, apimodel.RevisionReasonRollback)
			},
		},

		apimodel.RevisionKindSkill: {
			Locate: func(ctx context.Context, id string) (string, error) {
				// Built-in skills are not stored and have no history (the body is a code constant).
				// Return ErrPrecondition, not 404: it is visible in the list, so "does not exist"
				// would be wrong. Same as UpdateSkill.
				if IsBuiltinSkill(id) {
					return "", fmt.Errorf("%w: 平台内置技能没有历史版本", ErrPrecondition)
				}
				sk, err := s.store.GetSkill(ctx, id)
				if err != nil {
					return "", fmt.Errorf("%w: 技能不存在", ErrNotFound)
				}
				return sk.WorkspaceID, nil
			},
			Rollback: func(ctx context.Context, id string, snapshot []byte) (any, error) {
				var draft apimodel.SkillDraft
				if err := revisionsvc.DecodeSnapshot(snapshot, &draft); err != nil {
					return nil, err
				}
				// updateSkill itself blocks built-in and read-only sources (bodies installed from
				// the market / a code repository cannot be edited), so this path **does not need
				// and should not** copy those two gates again; copying them would make them drift.
				// "The state before rollback" is also recorded inside it, see the agent slot above.
				return s.updateSkill(ctx, id, draft, apimodel.RevisionReasonRollback)
			},
		},

		apimodel.RevisionKindSchedule: {
			Locate: func(ctx context.Context, id string) (string, error) {
				sch, err := s.store.GetSchedule(ctx, id)
				if err != nil {
					return "", fmt.Errorf("%w: 定时任务不存在", ErrNotFound)
				}
				return sch.WorkspaceID, nil
			},
			Rollback: func(ctx context.Context, id string, snapshot []byte) (any, error) {
				var draft apimodel.ScheduleDraft
				if err := revisionsvc.DecodeSnapshot(snapshot, &draft); err != nil {
					return nil, err
				}
				// updateSchedule recomputes NextRunAt when spec changes. **Rolling back to an old
				// spec is exactly a spec change**; without recomputing, the task would keep running
				// on the current expression while the UI shows the old one. "The state before
				// rollback" is also recorded inside it.
				return s.updateSchedule(ctx, id, draft, apimodel.RevisionReasonRollback)
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Snapshots: turn the **current** state of an entity into a draft that can be rolled back to
// ---------------------------------------------------------------------------
//
// ⚠️ These three functions **belong in core; this is not an unfinished move**: they encode
// "which fields of my entity go into history", which is knowledge owned by the agent / skill /
// schedule domains. service/revision only takes an any as the payload and does not know these
// types. They will move with their own domains.

// snapshotAgent **has already moved with service/agent** (exported as agentsvc.Snapshot):
// "which fields of my entity go into history" is knowledge owned by the domain that owns the
// entity; service/revision only takes an any as the payload and does not know apimodel.Agent at
// all. Same for snapshotSchedule. Only the skill one is left below; it will move with the skill
// domain.

// snapshotSchedule is the same, for scheduled tasks. **The real implementation is in
// service/schedule** (that domain has already moved out; "which fields go into history" is its
// own knowledge).
//
// This alias is kept because two places inside core still use this name: the rollback check in
// the slot above in this file, and TestSnapshotCoversEveryDraftField in core/revision_test.go.
// It will go away together with the agent / skill ones.
var snapshotSchedule = schedulesvc.Snapshot
