// Entity revision history: records what an entity looked like before a change, so it can be
// switched back after a mistake.
//
// # The scope of this feature: it is "undo", not "audit"
//
// The requirement as stated: this feature "is only for **recovering from mistakes**" and
// "should not steal the show". Those two points decide every tradeoff here:
//
//   - **Keep only the latest 20** (apimodel.MaxRevisionsPerEntity). Mistakes are noticed within
//     minutes to days, not months. Tracing who changed what a year ago is the job of an audit
//     log; a business table should not grow an endlessly long tail. The requirement also
//     explicitly asked to "avoid the database piling up junk data".
//   - **Recording failures always degrade**; they never block the user's save (see Record).
//   - **Only snapshot what users configure**, not runtime state.
//
// # ⚠️ Why rollback must go through UpdateX, not store.SaveX directly
//
// This is the easiest place in the whole feature to get wrong, and getting it wrong gives
// **zero errors**.
//
// Calling SaveX directly looks simpler (write back whatever is in the snapshot), but it skips a
// whole row of gates:
//
//	validateXDraft          validation (empty name, invalid cron expression...)
//	ensureIntegrationsAllowed / ensureEnvAliasesUnchanged
//	                        changes to injected authorizations must pass the gate. **Skipping it
//	                        is a silent privilege escalation hole**: a non-creator only needs to
//	                        roll back to a version "that had someone's personal authorization
//	                        checked" to get an injection they have no right to grant (same on
//	                        the service account side: one could roll back to a credential the
//	                        workspace never bound)
//	IsBuiltinSkill / readOnlySkillSource
//	                        built-in and marketplace skill bodies cannot be changed (the two
//	                        checks in core/agent.go)
//	ComputeNextRun          a changed spec must recompute the next fire time; without that the
//	                        task keeps running on the new expression while the UI shows the old
//	nonNil(...)             those slices must not be nil (null keeps the frontend save button lit
//	                        forever)
//	UpdatedBy = ActorFrom   audit trail
//
// Going through UpdateX **inherits all of these automatically**, none need to be copied. The
// cost of copying is not a few extra lines: the copies **will drift apart**. When someone adds a
// gate to UpdateAgent in the future, nobody will remember there is a second path here.
//
// ⚠️ **After the package split, this rule is carried by the Entity.Rollback slot**: this package
// no longer calls UpdateX directly (it cannot import service/agent), but calls the function the
// wiring side filled in. The rule is written as is on the Rollback field in entity.go, because
// **the person who can break it is now whoever fills in the slot, not this package**.
//
// # ⚠️ How optimistic lock / counter fields are protected: by types, not by discipline
//
// Snapshots store **AgentDraft / SkillDraft / ScheduleDraft**, not Agent / Skill / Schedule.
// Those three Draft types **have no Version / LatestVersion field at all**, so the bug "restore
// the counter too on rollback" **cannot be written at the type level**: there is no such field
// to read. That is far more reliable than "remember not to overwrite it".
//
// So when rolling back to version 3, Agent.Version goes from 7 to 8 (the unconditional
// `a.Version++` in UpdateAgent), and the content equals version 3. **Counters only move
// forward.**
//
// ⚠️ A common belief about Agent.Version needs correcting: it is **not** Ark's optimistic lock.
// That lock is **read fresh** from Ark on every update (updateAgent in core/arkrun.go:
// `upd := ma.AgentUpdate{Version: cur.Version}`, where cur comes from `ins.Agents().Get`), and
// the comment there literally says "we do not cache that version number". In the whole repo,
// apimodel.Agent.Version has only two writers (UpdateAgent and SetAgentIntegrations) and one
// reader (the v7 badge in the frontend).
//
// **But the reason to protect it does not go away, it just changes**: it is a monotonic counter
// and the UI shows it, so making it smaller would show users a version number going backwards.
// Skill.LatestVersion is stricter: that value **really is sent to Ark** (skill references carry a
// version number, see toMASkillRefs in arkrun.go), and restoring an old value would make Ark
// reference a skill version that is no longer the latest.
//
// # Rollback **also** creates a new snapshot
//
// Otherwise "undo this rollback" would be impossible, and that is exactly the second mistake in
// a mistake-recovery scenario (the user picked the wrong version). The state before rollback
// must be kept.
//
// That snapshot's Reason is rollback, which the UI uses to tell it apart from normal edits;
// without that, history would contain a record of unclear origin, and users could not tell
// "what I changed" from "what my rollback caused".
//
// ⚠️ **That snapshot is recorded inside the Entity.Rollback implementation; this package does not
// record it again.** Each of the three UpdateX has an internal version with a reason parameter
// (updateAgent / updateSkill / updateSchedule); rollback just passes rollback as the reason.
// Calling Record yourself before Rollback **records two** snapshots with identical content.
// This is not a guess: TestRollbackAlsoRecordsRevision caught it directly
// ("expected 2, got 3").
package revision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// ---------------------------------------------------------------------------
// Record: store a copy **before** the change
// ---------------------------------------------------------------------------

// Record stores a snapshot of the state before a change.
//
// # ⚠️ It never returns an error; failures are only logged
//
// This is on purpose, and it can only go this way: **failing to store history must not make the
// user's save fail**. The other way (fail-closed) means that as soon as the entity_revisions
// table has a problem, no agent/skill/schedule on the whole platform can be changed: a
// "nice to have" feature holding the core business hostage.
//
// The cost is "history may be missing an entry", which is visible (gaps in the sequence in the
// list), recoverable (the next change is recorded as usual), and affects no existing feature.
// Compared side by side, the direction is clear.
//
// ⚠️ But **failures must be logged**, otherwise "why are a few history entries missing" can never
// be tracked down.
//
// ⚠️ **There is no error in the signature; that is part of the interface, do not add one.** The
// callers (the three UpdateX) are all on write paths the user is waiting on. Give them an error
// and sooner or later someone writes `return err`, and then the degrade direction is reversed,
// while everything still compiles and tests stay green.
func (s *Service) Record(
	ctx context.Context,
	kind apimodel.RevisionKind,
	workspaceID, entityID, summary string,
	payload any,
	reason apimodel.RevisionReason,
) {
	blob, err := json.Marshal(payload)
	if err != nil {
		s.log().Warn("序列化历史版本快照失败，本次不记历史",
			slog.String("kind", string(kind)),
			slog.String("entity", entityID),
			slog.String("error", err.Error()))
		return
	}
	rev := apimodel.Revision{
		ID:          s.nextID("rev"),
		WorkspaceID: workspaceID,
		Kind:        kind,
		EntityID:    entityID,
		Snapshot:    blob,
		Summary:     summary,
		CreatedAt:   apimodel.NowISO(),
		// Whose change produced this snapshot. **An empty string is valid** (system paths have no
		// human identity); do not fill in a fake value here, that would describe "the system
		// changed a task the agent scheduled for itself" as something a person did.
		CreatedBy: auth.ActorFrom(ctx),
		Reason:    apimodel.NormalizeRevisionReason(reason),
	}
	if _, err := s.store.AppendEntityRevision(ctx, rev); err != nil {
		s.log().Warn("记录历史版本失败，本次改动照常生效",
			slog.String("kind", string(kind)),
			slog.String("entity", entityID),
			slog.String("error", err.Error()))
	}
}

// ---------------------------------------------------------------------------
// Read: list history / get one entry
// ---------------------------------------------------------------------------

// ListRevisions lists an entity's history, newest first, at most MaxRevisionsPerEntity entries.
//
// **The entity must exist**: if it does not, return ErrNotFound, not an empty list. An empty
// list would make "this agent was deleted" and "it has no history yet" look the same, and for
// the former the user should see 404.
//
// ⚠️ Returned Snapshot fields are always empty (the Store side deliberately does not fetch them);
// use GetRevision for the body.
func (s *Service) ListRevisions(
	ctx context.Context, kind apimodel.RevisionKind, entityID string,
) ([]apimodel.Revision, error) {
	if !apimodel.ValidRevisionKind(kind) {
		return nil, fmt.Errorf("%w: 不支持的实体类型 %q", persistmodel.ErrInvalidArgument, kind)
	}
	ent, err := s.entityOf(kind)
	if err != nil {
		return nil, err
	}
	// Confirm the entity still exists (this also covers "404 instead of an empty list"). The
	// returned workspace id is not needed here: authorization was already decided at the HTTP
	// boundary from the host entity, see the end of service.go.
	if _, err := ent.Locate(ctx, entityID); err != nil {
		return nil, err
	}
	return s.store.ListEntityRevisions(ctx, kind, entityID, apimodel.MaxRevisionsPerEntity)
}

// GetRevision gets one snapshot, **with its body**.
//
// ⚠️ **It must check that the snapshot really belongs to entityID**: without that, anyone holding
// a rev_ id could read the full system prompt of some agent in another workspace. Authorization
// is decided by resolving the workspace from entityID, and the snapshot id itself takes no part
// in that decision. This would be an unauthorized read with zero errors.
func (s *Service) GetRevision(
	ctx context.Context, kind apimodel.RevisionKind, entityID, revisionID string,
) (apimodel.Revision, error) {
	rev, err := s.store.GetEntityRevision(ctx, revisionID)
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			return apimodel.Revision{}, fmt.Errorf("%w: 历史版本不存在", persistmodel.ErrNotFound)
		}
		return apimodel.Revision{}, err
	}
	if rev.Kind != kind || rev.EntityID != entityID {
		// **Return 404, not 403**: this snapshot really does not belong to the entity the caller
		// is looking at, so for that entity it "does not exist". Returning 403 would instead
		// confirm "this thing exists but you cannot see it".
		return apimodel.Revision{}, fmt.Errorf("%w: 历史版本不存在", persistmodel.ErrNotFound)
	}
	return rev, nil
}

// ---------------------------------------------------------------------------
// Rollback
// ---------------------------------------------------------------------------

// RollbackRevision switches an entity back to a historical version.
//
// Three things happen in order; missing any of them has visible consequences:
//
//  1. **First store a snapshot of the current state** (Reason=rollback), otherwise this
//     rollback cannot be undone;
//  2. **Write back through UpdateX**, inheriting all validation and gates, see the whole section
//     in the file header;
//  3. Return the updated entity.
//
// ⚠️ Step 1 happens **before** step 2, and if step 2 fails that snapshot stays. This is on
// purpose: the other order (change first, record after) would lose the pre-rollback state when
// "the change succeeded but recording failed", while one extra snapshot equal to the current
// value is harmless (the user sees two identical entries, nothing is lost).
//
// ⚠️ **Step 1 is not in this function**; it is done inside the Entity.Rollback implementations
// (the three updateX). Adding it here again would record two identical snapshots, see the last
// section of the file header.
func (s *Service) RollbackRevision(
	ctx context.Context, kind apimodel.RevisionKind, entityID, revisionID string,
) (any, error) {
	rev, err := s.GetRevision(ctx, kind, entityID, revisionID)
	if err != nil {
		return nil, err
	}
	ent, err := s.entityOf(kind)
	if err != nil {
		return nil, err
	}
	return ent.Rollback(ctx, entityID, rev.Snapshot)
}

// ---------------------------------------------------------------------------
// Clear an entity's history when the entity is deleted
// ---------------------------------------------------------------------------

// Drop deletes all history of an entity.
//
// **Failures are only logged**, same direction as Record: deleting the agent already succeeded,
// and the whole delete must not be reported as failed because "history was not fully cleared".
// That would make the user retry, and the retry would fail again at the "agent is already gone"
// step. Leftover history rows are cleaned up by the scheduled cleanup (it groups by entity, and
// once the entity is gone nobody queries those rows).
//
// ⚠️ No error in the signature, for the same reason as Record; do not add one.
func (s *Service) Drop(ctx context.Context, kind apimodel.RevisionKind, entityID string) {
	n, err := s.store.DeleteEntityRevisions(ctx, kind, entityID)
	if err != nil {
		s.log().Warn("清理实体历史版本失败，残留由定时任务兜底",
			slog.String("kind", string(kind)),
			slog.String("entity", entityID),
			slog.String("error", err.Error()))
		return
	}
	if n > 0 {
		s.log().Info("实体已删除，其历史版本一并清掉",
			slog.String("kind", string(kind)),
			slog.String("entity", entityID),
			slog.Int64("条数", n))
	}
}
