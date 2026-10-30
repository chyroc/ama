// Entity history versions (revisions): record what things looked like before a change, so they can be switched back after a mistake.
//
// # ⚠️ Why it is called revision and not version
//
// In this repo "version" already has two different meanings, and both are in this package. Making a third thing
// called Version would turn "which version is this version" into a question to re-decide every time you read the code,
// and getting it wrong gives **zero errors**: the types are all int/string, and a wrong assignment still compiles.
//
//	| Name                   | Type   | What it is                                                   |
//	| ---------------------- | ------ | ------------------------------------------------------------ |
//	| Agent.Version          | int    | monotonic counter, +1 on every agent change; UI shows v7     |
//	| Skill.LatestVersion    | string | also a counter, but **a string** (the opposite)              |
//	| **Revision.No** (here) | int    | **sequence number of history snapshots**, from 1 per entity  |
//
// ⚠️ **Revision.No and Agent.Version are deliberately not equal**: the former counts "how many snapshots were saved", the latter counts
// "how many changes were made". Not every change creates a snapshot (nothing saved when content did not change), and old snapshots over the limit are deleted,
// so No is both smaller than Version and not guaranteed to be continuous. Do not show it in the UI as "number of changes".
//
// # Snapshots store the Draft, not the whole entity: this is a structural safety design
//
// Snapshot holds AgentDraft / SkillDraft / ScheduleDraft, **not** Agent /
// Skill / Schedule. Those three Draft types have no Version / LatestVersion field at all,
// so the mistake "restore the counter too when rolling back" **cannot even be written at the type level**.
// For the full reasoning see "Why rollback must go through UpdateX" in server/core/revision.go.
package apimodel

import "encoding/json"

// RevisionKind is the type of entity being snapshotted.
//
// The values must match the RevisionKind union in the frontend web/src/types/domain.ts exactly:
// that is a discriminated union; one extra value and TypeScript errors, one missing and the label cannot be found and the UI shows blank.
type RevisionKind string

const (
	// RevisionKindAgent is the history of an agent.
	RevisionKindAgent RevisionKind = "agent"
	// RevisionKindSkill is the history of a skill.
	RevisionKindSkill RevisionKind = "skill"
	// RevisionKindSchedule is the history of a scheduled task.
	RevisionKindSchedule RevisionKind = "schedule"
)

// AllRevisionKinds returns all entity types; **this is the only source of truth for that list**.
//
// The wiring layer uses it to check "are all three entities registered with how to look up and how to roll back"
// (service/revision.New): one missing does not crash; that kind of entity silently loses its history feature.
//
// ⚠️ **When adding a fourth entity, changing this is enough**: ValidRevisionKind reads the same list,
// so the two cannot split. A split would mean "validation passes but nobody registered it" or the reverse, both with zero errors.
//
// Each call returns a new slice, so callers cannot append into someone else's data (same as BuiltinSkills).
func AllRevisionKinds() []RevisionKind {
	return []RevisionKind{RevisionKindAgent, RevisionKindSkill, RevisionKindSchedule}
}

// ValidRevisionKind reports whether this is a known entity type.
//
// **Unknown is always rejected**; do not fall back to some kind: a fallback would let a misspelled kind silently land in
// another entity's history, and on rollback that data would write A's content into B.
func ValidRevisionKind(k RevisionKind) bool {
	for _, known := range AllRevisionKinds() {
		if k == known {
			return true
		}
	}
	return false
}

// RevisionReason is "which action produced this snapshot".
type RevisionReason string

const (
	// RevisionReasonEdit is a snapshot produced by a normal edit. **The zero value falls back to it**.
	RevisionReasonEdit RevisionReason = "edit"
	// RevisionReasonRollback is a snapshot produced by a rollback.
	//
	// A rollback **also saves a snapshot of the state before it** (see core/revision.go); without marking it,
	// the history list would get an entry of unclear origin, and users could not tell "what I changed" from "what my rollback caused".
	RevisionReasonRollback RevisionReason = "rollback"
)

// NormalizeRevisionReason normalizes an empty string to edit.
//
// Existing rows and missing values are both empty strings, and an empty string has no label in the UI and shows blank.
func NormalizeRevisionReason(r RevisionReason) RevisionReason {
	if r == RevisionReasonRollback {
		return RevisionReasonRollback
	}
	return RevisionReasonEdit
}

// Revision is one history snapshot.
//
// Timestamps are fixed-length ISO strings (the NowISO shape), like other contract time fields in this package.
type Revision struct {
	// ID is the id of this snapshot (starts with rev_).
	ID string `json:"id"`
	// WorkspaceID is the owning workspace. Authorization resolution needs it.
	WorkspaceID string `json:"workspaceId"`
	// Kind is the type of entity being snapshotted.
	Kind RevisionKind `json:"kind"`
	// EntityID is the id of the entity being snapshotted.
	EntityID string `json:"entityId"`
	// No is which snapshot of this entity this is, starting at 1.
	//
	// ⚠️ **It is not Agent.Version, and is not guaranteed to be continuous** (old snapshots over the limit are deleted);
	// see the table at the top of this file.
	No int `json:"no"`
	// Snapshot is the entity content just before the change, **carried out as raw JSON** (not a string).
	//
	// # ⚠️ It must be json.RawMessage; writing []byte silently turns it into base64
	//
	// Both types are `[]byte` underneath, assignable to each other, and **the compiler cannot tell them apart**, so it is easy to write
	// the latter. But `encoding/json` treats them in completely opposite ways:
	//
	//	json.RawMessage → embedded as is     "snapshot":{"name":"…"}
	//	[]byte          → **base64 encoded**  "snapshot":"eyJuYW1lIjoi…"
	//
	// With the latter, the frontend gets a base64 string, and `typeof` is string instead of object,
	// so "expand to see this version" renders a pile of garbage. **This is not a guess**: we hit it when testing against the real service,
	// while all Go unit tests and e2e were green (the tests compared `string(rev.Snapshot)`,
	// which turns both types back into the same bytes and happens to hide the difference).
	//
	// string does not work either: the whole JSON would be treated as a string and escaped once more
	// (`"{\"name\":…}"`), and every call site in the frontend would need one more parse.
	//
	// ⚠️ **It is empty in the list endpoint** (see Store.ListEntityRevisions): one agent snapshot is
	// a few KB, 20 of them are tens of KB, and the list only renders names and times. Fetch one at a time when the body is needed.
	Snapshot json.RawMessage `json:"snapshot,omitempty"`
	// Summary is one sentence for humans (the name that version had at the time).
	//
	// **Deliberately frozen at write time**: it records "what that version was called"; being stale is exactly its meaning.
	Summary string `json:"summary"`
	// CreatedAt is when the snapshot was produced.
	CreatedAt string `json:"createdAt"`
	// CreatedBy is whose change produced this snapshot, an employee email.
	//
	// **May be an empty string**: system paths (a task scheduled by the agent itself being changed, a marketplace skill auto-updating) have no
	// human identity. An empty string means "not changed by a person"; say so honestly in the UI, do not guess a person to fill in.
	// Has omitempty, same as Agent.CreatedBy: a missing key and an empty string are the same thing to the frontend,
	// and one fewer shape is one fewer branch to check.
	CreatedBy string `json:"createdBy,omitempty"`
	// Reason is edit or rollback; see RevisionReason.
	Reason RevisionReason `json:"reason"`
}

// MaxRevisionsPerEntity is the maximum number of history snapshots kept per entity.
//
// # Why 20
//
// This feature is for **switching back after a mistake** (the user's own words), not audit trails. Mistakes are discovered
// within minutes to days, not months: really tracing who changed what a year ago is a different need,
// and it belongs to audit logs, not to giving every business table an endlessly growing tail.
//
// 20 means "anything within the last 20 changes can be recovered". A larger value would not make the feature easier to use
// (the list needs paging, picking a diff is harder); it would only add more garbage to the database, and the user explicitly raised this.
//
// ⚠️ **Cleanup of the excess is not done on the write path** (that would add latency to every save, and with multiple instances running concurrently
// "delete everything after the 21st" is itself a race); a scheduled task reclaims them in one place, see core/revisionpurge.go.
// So the database **may briefly exceed this number**, and the UI only takes the first N: the rule is "it settles eventually",
// not "never exceeds at any moment".
const MaxRevisionsPerEntity = 20
