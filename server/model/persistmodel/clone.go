// Deep-copy helpers: every Store read path needs them.
//
// # Why here and not in core
//
// They only depend on model/apimodel, and **both Store implementations need them**: MemStore uses
// them so callers changing return values cannot pollute the stored state, and the PG
// implementation uses them to uphold the invariant "slices are non-nil" (a nil slice decoded from
// jsonb encodes back as null, and the frontend's .map() blows up).
//
// They used to be declared in core/store.go, so moving the PG implementation out of core would
// have been blocked by them. With them here, any package can implement Store without importing
// core.
//
// # The shared invariant: slices are copied and stay non-nil
//
// nil encodes as null, and the frontend contract requires [] (it calls .map() / .length
// directly). **But there are two deliberate exceptions** (Skill.Files and SkillFile.Content); see
// their comments: those two must stay nil when nil, or TestSeedRoundTrip's byte-by-byte
// comparison fails.

package persistmodel

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/chyroc/ama/server/model/apimodel"
)

// CloneStrings copies a string slice and guarantees it is non-nil.
func CloneStrings(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// CloneWorkspace copies a workspace (including the members slice).
func CloneWorkspace(ws apimodel.Workspace) apimodel.Workspace {
	out := ws
	out.Members = make([]apimodel.WorkspaceMember, len(ws.Members))
	copy(out.Members, ws.Members)
	return out
}

// CloneAgent copies an agent.
func CloneAgent(a apimodel.Agent) apimodel.Agent {
	out := a
	out.BuiltinTools = CloneStrings(a.BuiltinTools)
	out.SkillIDs = CloneStrings(a.SkillIDs)
	out.FeishuAppIDs = CloneStrings(a.FeishuAppIDs)
	// ⚠️ These two must also end up non-nil, for exactly the same reason as the three above
	// (nil → null → the frontend is "dirty" forever).
	//
	// **The seed data does not have these two keys**, so without handling them here
	// `TestSeedRoundTrip`'s byte-by-byte comparison fails at once; that test is exactly the guard
	// for this invariant.
	out.IntegrationIDs = CloneStrings(a.IntegrationIDs)
	out.Envs = CloneAgentEnvs(a.Envs)
	out.EnvAliases = CloneAgentEnvAliases(a.EnvAliases)
	return out
}

// CloneAgentEnvAliases deep-copies the credential alias list and guarantees it is non-nil.
//
// AgentEnvAlias holds only strings, so assigning element by element is a deep copy. The reason for
// non-nil is exactly the same as CloneAgentEnvs (nil → null → the frontend's isDirty is "dirty"
// forever).
func CloneAgentEnvAliases(in []apimodel.AgentEnvAlias) []apimodel.AgentEnvAlias {
	out := make([]apimodel.AgentEnvAlias, len(in))
	copy(out, in)
	return out
}

// CloneAgentEnvs deep-copies the env list and guarantees it is non-nil.
//
// AgentEnv holds only value types (two strings + one bool), so assigning element by element is a
// deep copy; no need to recurse into it like slice fields.
func CloneAgentEnvs(in []apimodel.AgentEnv) []apimodel.AgentEnv {
	out := make([]apimodel.AgentEnv, len(in))
	copy(out, in)
	return out
}

// CloneSkill copies a skill.
func CloneSkill(sk apimodel.Skill) apimodel.Skill {
	out := sk
	out.Tags = CloneStrings(sk.Tags)
	// Files must be deep-copied too: it is a slice, and its elements hold a []byte. Copying only
	// the outer slice would make two callers share the same content bytes; that is a data race,
	// not a logic error, and the symptom is occasional torn reads.
	//
	// **nil must stay nil**; do not fill in an empty slice in passing: Skill.Files has omitempty,
	// an empty slice would serialize as "files":[], the seed has no such key, and
	// TestSeedRoundTrip would fail.
	if sk.Files != nil {
		files := make([]apimodel.SkillFile, 0, len(sk.Files))
		for _, f := range sk.Files {
			files = append(files, CloneSkillFile(f))
		}
		out.Files = files
	}
	return out
}

// CloneSkillFile copies an attachment, including its content bytes.
func CloneSkillFile(f apimodel.SkillFile) apimodel.SkillFile {
	out := f
	if f.Content != nil {
		out.Content = append([]byte(nil), f.Content...)
	}
	return out
}

// SortSkillFiles sorts by path ascending.
//
// Sorting is not just for looks: writing in a fixed order when injecting into the container makes
// logs and failure retries reproducible; and PG uses `order by path`, so if the two differed, unit
// tests (which run the in-memory version) would not see ordering problems.
func SortSkillFiles(files []apimodel.SkillFile) {
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
}

// CloneSchedule copies a schedule.
//
// Schedule is all scalars plus two embedded structs of pure scalars (ScheduleSpec /
// DeliveryTarget), with no slices or pointers, so a value copy is a deep copy. **When adding
// fields, come back and check this still holds**: if a slice field is ever added (for example
// several delivery targets), it must be copied here, or two callers would share the same
// underlying array; that is a data race, not a logic error, and the symptom is occasional torn
// reads.
func CloneSchedule(sch apimodel.Schedule) apimodel.Schedule {
	return sch
}

// CloneRevision copies a revision snapshot.
//
// ⚠️ **Snapshot is a []byte and must really be copied**: a value copy only copies the slice
// header, and two callers would share the same underlying array. That is exactly the difference
// from CloneSchedule's "a value copy is a deep copy": that type is all scalars, this one is not.
// Sharing an underlying array is a data race, not a logic error; the symptom is occasional torn
// reads, and single-instance tests cannot catch it.
//
// nil stays nil (do not copy it into an empty slice): list queries deliberately leave out
// snapshot, and `[]byte{}` would make json output `"snapshot":""`, which is not a valid JSON
// value, and the frontend's parse would blow up.
func CloneRevision(rev apimodel.Revision) apimodel.Revision {
	out := rev
	if rev.Snapshot != nil {
		out.Snapshot = make([]byte, len(rev.Snapshot))
		copy(out.Snapshot, rev.Snapshot)
	}
	return out
}

// CloneRun copies a run record.
func CloneRun(r apimodel.Run) apimodel.Run {
	out := r
	out.Threads = make([]apimodel.RunThread, len(r.Threads))
	copy(out.Threads, r.Threads)
	// DurationMs is a pointer; copy it into a new one so the caller does not get an address it can change.
	if r.DurationMs != nil {
		v := *r.DurationMs
		out.DurationMs = &v
	}
	// Same for ParentThreadID in threads.
	for i, th := range out.Threads {
		if th.ParentThreadID != nil {
			v := *th.ParentThreadID
			out.Threads[i].ParentThreadID = &v
		}
	}
	return out
}

// CloneEvent copies an event, including its raw bytes.
func CloneEvent(e apimodel.RunEvent) apimodel.RunEvent {
	out := e
	out.Raw = append(json.RawMessage(nil), e.Raw...)
	if e.ThreadID != nil {
		v := *e.ThreadID
		out.ThreadID = &v
	}
	return out
}

// CloneFeedback deep-copies one feedback record.
//
// **The Payload map must be copied**: the other fields are scalars, but a map is a reference type;
// without copying, the caller gets the stored one, and changing it in place changes the store
// (and without holding the lock). That is exactly why "read methods always return deep copies".
func CloneFeedback(rec FeedbackRecord) FeedbackRecord {
	if rec.Payload == nil {
		return rec
	}
	payload := make(map[string]any, len(rec.Payload))
	for k, v := range rec.Payload {
		payload[k] = v
	}
	rec.Payload = payload
	return rec
}

// MaskSecret masks a secret, behaving **byte for byte the same** as maskSecret in the frontend's
// api.ts.
//
//	""        → ""
//	len ≤ 8   → the same number of asterisks (so an 8-character secret leaks its length; this is existing frontend behavior)
//	len ≥ 9   → first 4 + min(16, len-8) asterisks + last 4; total length saturates at 24
//
// Length counts **UTF-16 code units**, not bytes and not runes: JS String.length has UTF-16
// semantics. Secrets are all ASCII in practice, so the three agree; but implementing it in UTF-16
// guarantees that if Chinese or emoji is ever pasted in, the result matches the frontend instead
// of each side computing its own.
//
// Here and not in core: both core and repository/dao need it to build/assert masks, and it is a
// pure string function with zero dependencies.
func MaskSecret(secret string) string {
	if secret == "" {
		return ""
	}
	units := utf16.Encode([]rune(secret))
	n := len(units)
	if n <= 8 {
		return strings.Repeat("*", n)
	}
	stars := n - 8
	if stars > 16 {
		stars = 16
	}
	head := string(utf16.Decode(units[:4]))
	tail := string(utf16.Decode(units[n-4:]))
	return head + strings.Repeat("*", stars) + tail
}

// LastActivityOf gets the activity time used for sorting; old data without LastActivityAt falls
// back to StartedAt. Same rule as coalesce(nullif(...)) on the PG side, so both stores produce the
// same order.
//
// Here and not in core: MemStore's ListRuns and the repository/dao contract tests both need it;
// the latter use it as the sort key to verify order, instead of reimplementing it (two
// implementations would drift).
func LastActivityOf(r apimodel.Run) string {
	if r.LastActivityAt != "" {
		return r.LastActivityAt
	}
	return r.StartedAt
}

// NormalizeEmail normalizes the case and whitespace of an email.
//
// **Every entry point must go through it**: the case returned by SSO is not guaranteed to be
// stable, and without normalizing at the entry, the same person becomes two Actors downstream.
// This kind of bug only shows up for a certain person on a certain login, and is extremely hard
// to track down.
//
// Here and not in auth: it is an **invariant shared** by MemStore and the PG implementation (both
// use the normalized email as the primary key), and the PG implementation is in
// repository/dao/authdao. Each implementation normalizing on its own is correct, but the algorithm
// must be the same one; writing it twice will diverge sooner or later, and the divergence shows up
// as "the same person has two records", with no error.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
