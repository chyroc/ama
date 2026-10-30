// Snapshots and "did this save actually change anything": the two checks before recording history.
//
// # These two functions belong to this domain, not to revision
//
// The file header of core/revision.go already says so: snapshotX / sameXDraft are "which fields of
// my entity
// should be recorded in history", knowledge owned by the domain that owns the entity; service/revision
// only takes
// an any as payload and knows nothing about apimodel.Agent. So they moved with this domain instead
// of staying there.
// service/schedule/snapshot.go is a precedent of the same shape.
//
// The skill ones are still in core/revision.go and core/revisiondiff.go, and will go when that
// package is split.
//
// # ⚠️ Must compare the shape **after normalization**, otherwise the check is always "changed"
//
// The draft is deserialized from the HTTP request body, where empty lists may be `nil` (old
// frontends do not send the key),
// while the database stores `[]`. `reflect.DeepEqual(nil, []string{})` is **false**:
// comparing directly would judge every save as "changed", so even "a save that changed nothing"
// stores
// an identical snapshot, filling the 20-entry cap for nothing and pushing out the version you
// really wanted.
// **This is not an optimization, it is correctness**: it would destroy exactly the reason this
// feature exists, with zero errors
// (history full of duplicate snapshots, which nobody would notice).
//
// So both functions below first pass their inputs through the same normalization functions as
// UpdateAgent, word for word,
// then compare. There is only one copy of the normalization functions (the ones UpdateAgent uses),
// so they cannot diverge.
package agent

import (
	"reflect"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Snapshot turns an agent's **current** state into a draft that can be rolled back to.
//
// ⚠️ This function is **the single source of truth** for "which fields get rolled back". If you add
// a field to Agent and want it to be
// rolled back, add a line here; missing it means "after rollback that field stays as it is",
// with no error, and only visible when someone really rolls back.
//
// It can be guarded by one test: TestSnapshotCoversEveryDraftField fills every field of the entity
// with
// non-zero values, then asserts the draft from the snapshot has no zero-value field; missing one
// turns it red.
//
// **It is exported because the rollback slot needs it**: the agent slot in core/revision.go and
// core/revision_test.go are both outside this package. Same as schedulesvc.Snapshot.
func Snapshot(a apimodel.Agent) apimodel.AgentDraft {
	return apimodel.AgentDraft{
		Name:           a.Name,
		Description:    a.Description,
		System:         a.System,
		Model:          a.Model,
		BuiltinTools:   a.BuiltinTools,
		SkillIDs:       a.SkillIDs,
		FeishuAppIDs:   a.FeishuAppIDs,
		Status:         a.Status,
		IntegrationIDs: a.IntegrationIDs,
		Envs:           a.Envs,
		EnvAliases:     a.EnvAliases,
		FollowUp:       a.FollowUp,
		ProgressDetail: a.ProgressDetail,
		// The executor choice goes with the snapshot: rolling back to a past version must restore
		// the executor too, otherwise you get
		// the mismatched combination "old prompt, new executor". HostSource (workspace assignment /
		// personal direct connect)
		// is part of this choice; missing it would make rollback quietly turn personal direct connect
		// back into workspace assignment.
		ExecutorType: a.ExecutorType,
		HostID:       a.HostID,
		HostSource:   a.HostSource,
	}
}

// sameAgentDraft reports whether this draft, once stored, would equal the current state.
//
// before is the result of Snapshot(current entity) (already the normalized stored shape),
// draft is the one from the request body.
func sameAgentDraft(before, draft apimodel.AgentDraft) bool {
	// Pass draft through the assignment logic of UpdateAgent to get "what it will look like once
	// stored".
	// ⚠️ Every line here must stay consistent with UpdateAgent; when adding a field change both
	// sides;
	// missing one field means "changed it but recorded no history", and that change can never be
	// rolled back.
	after := apimodel.AgentDraft{
		Name:           strings.TrimSpace(draft.Name),
		Description:    draft.Description,
		System:         draft.System,
		Model:          draft.Model,
		BuiltinTools:   nonNil(draft.BuiltinTools),
		SkillIDs:       nonNil(draft.SkillIDs),
		FeishuAppIDs:   nonNil(draft.FeishuAppIDs),
		Status:         before.Status,
		IntegrationIDs: normalizeIntegrationIDs(draft.IntegrationIDs),
		Envs:           normalizeAgentEnvs(draft.Envs),
		EnvAliases:     normalizeAgentEnvAliases(draft.EnvAliases),
		FollowUp:       apimodel.NormalizeFollowUpMode(draft.FollowUp),
		ProgressDetail: apimodel.NormalizeProgressDetail(draft.ProgressDetail),
		ExecutorType:   strings.TrimSpace(draft.ExecutorType),
		HostID:         strings.TrimSpace(draft.HostID),
		// An empty host_source means the same as workspace (old rows are NULL); it must be
		// normalized before comparing,
		// otherwise every save of an old agent would be judged "executor changed" and record a
		// useless history entry.
		HostSource: apimodel.EffectiveHostSource(
			strings.TrimSpace(draft.ExecutorType), strings.TrimSpace(draft.HostSource)),
	}
	// Status is only assigned when non-empty (that `if draft.Status != ""` in UpdateAgent),
	// so an empty string means "keep as is"; it is already preset to before.Status above, and here
	// only non-empty values override it.
	if draft.Status != "" {
		after.Status = draft.Status
	}
	// Pass before through the same normalization too.
	//
	// **Normalizing only the draft side is not enough.** The database copy may have been written
	// before a normalization rule changed
	// (e.g. sorting IntegrationIDs was added later); then the two shapes differ,
	// and the check would again always be "changed". Passing both through the same function compares
	// "content" rather than "how it is written".
	normalized := apimodel.AgentDraft{
		Name:           strings.TrimSpace(before.Name),
		Description:    before.Description,
		System:         before.System,
		Model:          before.Model,
		BuiltinTools:   nonNil(before.BuiltinTools),
		SkillIDs:       nonNil(before.SkillIDs),
		FeishuAppIDs:   nonNil(before.FeishuAppIDs),
		Status:         before.Status,
		IntegrationIDs: normalizeIntegrationIDs(before.IntegrationIDs),
		Envs:           normalizeAgentEnvs(before.Envs),
		EnvAliases:     normalizeAgentEnvAliases(before.EnvAliases),
		FollowUp:       apimodel.NormalizeFollowUpMode(before.FollowUp),
		ProgressDetail: apimodel.NormalizeProgressDetail(before.ProgressDetail),
		ExecutorType:   strings.TrimSpace(before.ExecutorType),
		HostID:         strings.TrimSpace(before.HostID),
		HostSource: apimodel.EffectiveHostSource(
			strings.TrimSpace(before.ExecutorType), strings.TrimSpace(before.HostSource)),
	}
	return reflect.DeepEqual(normalized, after)
}
