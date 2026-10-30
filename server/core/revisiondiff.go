// "Did this save actually change anything?": the check made before recording history.
//
// # Why it is needed
//
// The frontend save button sends back the **full draft** (see the comment on
// apimodel.AgentDraft), so clicking save twice in a row, or opening the detail page and
// clicking save without changing anything, both reach UpdateX.
// Without this check, each of those "nothing changed" saves would store an identical
// snapshot and fill up the 20-entry limit for nothing, pushing out the version you actually
// want to find. That defeats the whole reason this feature exists, so this check is not an
// optimization, it is correctness.
//
// # ⚠️ Compare the shape **after normalization**, or the check always says "changed"
//
// The draft is unmarshaled from the HTTP request body, where an empty list may be `nil`
// (old frontends do not send the key), while the database stores `[]`.
// `reflect.DeepEqual(nil, []string{})` is **false**: comparing directly would mark every
// save as "changed", so the problem above stays exactly as it was, with **zero errors**
// (the history is just full of duplicate snapshots, and nobody notices it is a bug).
//
// So each same*Draft here first runs its input through the exact same normalization
// functions used in UpdateX, then compares. There is only one copy of the normalization
// functions (the ones UpdateX uses), so they cannot diverge.
package core

// sameAgentDraft / sameSkillDraft **have moved with service/agent and service/skill**
// (same as snapshotAgent: "did this save change anything" is knowledge that belongs to the
// domain that owns the entity).

// sameScheduleDraft **has moved with service/schedule** (same as snapshotSchedule:
// "did this save change anything" is knowledge that belongs to the domain that owns the
// entity). Only the skill one is left in this file, and it will move with the skill domain.
