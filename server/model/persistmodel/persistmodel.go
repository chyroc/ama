// Package persistmodel holds types **used only by the storage layer**: Store interface inputs, and
// row structs that never go into HTTP responses.
//
// # How it divides work with model/apimodel
//
//	model/apimodel      entities in the HTTP contract. Handlers writeJSON them out directly,
//	                     so they are byte-sensitive and sensitive to field presence.
//	model/persistmodel  only flow across the "Service ↔ Store" seam, never into responses.
//
// Two packages rather than one file in domain: a package boundary turns "in the contract" vs
// "not in the contract" **into something both the compiler and code review can see**. Mixed into
// domain, they could only be told apart by file names and comments, and if the next person added a
// json tag to OnboardingRecord, or casually writeJSON'd it out, nothing would stop it; once such a
// change ships it becomes a de facto contract, and taking it back means touching the frontend.
//
// # ⚠️ The rule is "will it go into an HTTP response", not "does it have json tags"
//
// The two are **not equivalent**; do not use the latter as the filter:
//
//   - ArkBinding has 8 json tags, but they are for the **jsonb doc column**
//     (marshalDoc in core/store_pg.go); it never reaches a handler.
//   - The four *Query types appear in core/handler.go, but all are **constructed as inputs**
//     (`RunQuery{...}`); no writeJSON outputs them.
//
// So **do not change a single character** of these json tags: ArkBinding's decide the shape of the
// data already in ark_bindings.doc, and renaming one key is a silent data incompatibility (old rows
// read back with all fields empty, and no error).
//
// # Why this package must be a leaf
//
// Its whole point is to make "Store interface signatures only depend on leaf packages" hold: these
// types used to be declared in core, so any package wanting to implement core.Store was forced to
// import core, not for the interface, but for a few row structs. So this package **may only import
// the standard library and model/apimodel**, and especially not core or infra.
package persistmodel

import (
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
)

// RunQuery is the filter for run history, matching the frontend's RunQuery.
type RunQuery struct {
	AgentID string
	// Status is the status whitelist; empty means no filter.
	Status []apimodel.RunStatus
	// Search does a case-insensitive substring match on three fields: title / agent name / run id.
	Search string
	// ScheduleID only shows runs triggered by one schedule; empty means no filter.
	//
	// **Do not use Trigger instead**: `trigger = schedule` only gives "runs of all schedules", and
	// when a workspace has several schedules they are mixed together; exactly what needs telling
	// apart is not told apart.
	//
	// ⚠️ This filter **always misses historical runs from before this field existed** (no link was
	// recorded then); callers must say there is a gap instead of treating it as "never ran"; see
	// apimodel.Run.ScheduleID.
	ScheduleID string

	// Trigger is the trigger whitelist; empty means no filter.
	//
	// It serves the chat page's left column, "only sessions where a person talks to an agent"
	// (manual + feishu), which used to be done on the client by the frontend's `runs.filter(...)`.
	// **Once paging came in, that approach was wrong**: the client can only filter the page it has,
	// so the left column misses sessions, while the page renders normally with no error.
	//
	// ⚠️ **Does not overlap with ScheduleID; do not use it instead**: `Trigger=[schedule]` gives
	// "runs of all schedules", mixed together when a workspace has several schedules; that is exactly
	// what ScheduleID tells apart (see above). The two fields can be combined.
	//
	// ⚠️ On the PG side it **can only query doc** (`doc->>'trigger'`): the runs table has no trigger
	// scalar column, and extracting one means migrating existing data. The session list is already
	// narrowed by workspace_id first, so this step filters that small set, an acceptable cost; extract
	// a column if it ever becomes a bottleneck (following the schedule_id precedent).
	Trigger []apimodel.RunTrigger

	// Limit is the most rows to fetch on this page; **0 means no limit**.
	//
	// # Why 0 means "no limit" and not "none at all"
	//
	// This zero-value meaning is deliberate: it makes `RunQuery{}` behave **exactly the same** as
	// before paging was added. ListRuns has 6 call sites (5 in the frontend + the CLI), and the
	// full-data ones (usage stats, schedule history) keep working as before without passing Limit.
	// Interpreting 0 as "zero rows" would silently give every call site that has not caught up an
	// empty list, and an empty list in the UI looks just like "no runs yet".
	//
	// The cost is that "really want 0 rows" cannot be expressed, but no caller needs that.
	Limit int
	// Offset is how many rows to skip, for paging.
	//
	// ⚠️ **Offset > 0 with Limit == 0 is a meaningful combination** (skip the first N, return the
	// rest); both implementations must support it; do not ignore Offset when Limit == 0.
	//
	// ⚠️ Deep paging on PG is O(offset): `offset 10000` still scans the first 10000 rows and throws
	// them away. The only current use is shallow paging like "first screen + load more", which is
	// fine; real deep paging would need keyset paging (a cursor on (lastActivityAt, ord)), which
	// means changing the sort key too.
	Offset int
}

// UsageQuery is the time window for usage aggregates, used by ListRunsForUsage.
//
// Here and not in usage.go: it is a **Store input**, the same kind as RunQuery / EventQuery.
//
// Both ends are **fixed-length ISO strings** (like 2026-08-25T10:00:00.000Z), not time.Time:
// runs.started_at is stored in exactly this shape, and fixed three decimals make lexical order equal
// time order, so both Store implementations can compare strings directly (MemStore with `<` / `>=`,
// PG with `started_at >= $n`). Switching to time.Time would mean parsing on both sides, inviting
// time zone and precision differences. See the timestamp comment at the top of model/apimodel.
//
// The window is **half-open** [Since, Until): when bucketing by day, an open right end keeps the
// last millisecond of a day from falling into two buckets. An empty string means no limit on that
// side.
type UsageQuery struct {
	Since string
	Until string
}

// EventQuery is the filter for events, matching the frontend's EventQuery.
//
// ⚠️ **Same name as, but unrelated to,** EventQuery in shared/infra/ma: that one is the input of
// Ark's ListEvents (shared/infra/ma/session_events.go); this one is a core.Store input. infra does
// not import core, and the two never appear in the same expression, so the shared name is not a
// problem; but do not mix them up when changing things.
type EventQuery struct {
	// Types is the event type whitelist, matching Ark's types parameter; empty means no filter.
	Types []string
	// ThreadID only shows one thread; empty means no filter.
	ThreadID string
}

// OnboardingRecord is the stored form.
//
// Separate from OnboardingState because what is stored has one more field than what is returned,
// UpdatedAt (for debugging and operations; the frontend does not need it), and time at this layer
// is time.Time: Store writes it into timestamptz columns.
type OnboardingRecord struct {
	Email       string
	CompletedAt time.Time
	SkippedAt   time.Time
	Version     int
	UpdatedAt   time.Time
}

// UserPreferencesRecord is the stored form.
//
// Separate from UserPreferences for the same reason as onboarding: what is stored has one more
// field, UpdatedAt (for debugging; the frontend does not need it), and at this layer it is
// time.Time: Store writes it into timestamptz.
type UserPreferencesRecord struct {
	Email      string
	Language   string
	Appearance string
	UpdatedAt  time.Time
}

// RunSandbox is the stored record of "which container this Run uses".
//
// **Only invariants.** Anything that changes (running / paused / stopped) is always queried live;
// see the header of core/sandbox.go.
type RunSandbox struct {
	// RunID is the Ark session id (chat.go uses the id returned by CreateSession directly as Run.ID).
	RunID string
	// SessionID is the container name (`<prefix>-sess-<session id>`). The column name is historical.
	SessionID string
	// SandboxID is the id of the Docker daemon the container is on. An instance connected to
	// another daemon uses it to know "the container is not on my side", instead of misreporting it
	// as "reclaimed".
	SandboxID string
	// CreatedAt is when the container was created.
	CreatedAt time.Time
}

// FeedbackLevel has moved to domain (FeedbackReport.Level needs it, and that type goes into HTTP
// responses; keeping it here would make domain import storemodel in reverse, while storemodel
// already imports domain, a cycle). The alias here keeps existing call sites unchanged.
type FeedbackLevel = apimodel.FeedbackLevel

// The two levels of FeedbackLevel.
const (
	FeedbackAuto = apimodel.FeedbackAuto
	FeedbackUser = apimodel.FeedbackUser
)

// FeedbackRecord is the stored form.
//
// Separate from FeedbackReport for the same reason as OnboardingRecord: time at this layer is
// time.Time (written into timestamptz columns and compared in the database), and there is one more
// field, DedupKey; that is an implementation detail and **never appears in any response** (it is
// predictable, and exposing it would tell people how to forge deduplication).
type FeedbackRecord struct {
	ID           string
	Level        FeedbackLevel
	Actor        string
	WorkspaceID  string
	RoutePattern string
	HTTPStatus   int
	ErrorCode    string
	ErrorKind    string
	UserAgent    string
	// LogID is the logid of that server request, **collected at both levels** (see
	// core.FeedbackInput.LogID). Empty when unavailable; Store writes it as SQL NULL.
	LogID       string
	Payload     map[string]any
	CreatedAt   time.Time
	LastSeenAt  time.Time
	Occurrences int
	// DedupKey is the rate-limit key, **non-empty for auto level, empty for user level** (Store
	// writes empty strings as NULL, and NULLs are never equal in a unique index, so user level is
	// naturally not deduplicated).
	DedupKey string
}

// FeedbackQuery is the filter and paging condition for the read API.
//
// ⚠️ **Paging is not optional**: without it one query pulls the whole table, and this table grows
// with the number of errors. That API is for "automated improvement", that is, it is called by
// scripts repeatedly, so it must be able to page reliably.
type FeedbackQuery struct {
	// An empty Level means both levels.
	Level FeedbackLevel
	// An empty ErrorCode means no filter.
	ErrorCode string
	// Since / Until are a time window of **fixed-length ISO strings**, half-open [Since, Until).
	//
	// The reason for half-open is exactly the same as for usage aggregates (see UsageQuery): a
	// closed interval would put the boundary row on two adjacent pages, so a script paging by time
	// would process the same row twice.
	Since string
	Until string
	// Limit is rows per page; 0 uses the default 50, capped at 200.
	Limit int
	// Offset is how many rows to skip.
	Offset int
}

// SuggestionQuery is the filter and paging condition for the product suggestion list.
//
// ⚠️ **Author is also the authorization implementation**, not an ordinary filter: when non-staff
// call the list, Service forces it to the caller themselves (ListSuggestions in
// core/suggestion.go). When adding new filters, do not treat it as "a user-selectable filter": the
// author parameter in the request is **never accepted**; accepting it would let anyone read
// anyone's suggestions.
//
// Paging follows the same direction as FeedbackQuery: one use of this API is for agents to pull
// data for analysis, that is, it is called by scripts repeatedly, so it must be able to page
// reliably.
type SuggestionQuery struct {
	// An empty Author means no filter by author (**only staff can reach this branch**).
	Author string
	// An empty Status means both states.
	Status apimodel.SuggestionStatus
	// Limit is rows per page; 0 uses the default 50, capped at 200.
	Limit int
	// Offset is how many rows to skip.
	Offset int
}

// ArkBinding is the Ark-side counterpart of an AMA agent.
//
// **It must be stored**, not only kept in memory: Ark Environment names are **unique** within a
// project (measured: creating an environment with a duplicate name returns 409 ResourceConflict).
// Kept only in memory, the cache is empty after a process restart, so the same name is created
// again: straight 409, and that workspace can never start a new conversation again. On top of
// that, every restart would create one more Ark Agent (Agents allow duplicate names), piling up.
//
// This trap only showed up after restarting the service: the first run worked fine, and after a
// restart it failed 100% of the time.
type ArkBinding struct {
	WorkspaceID      string `json:"workspaceId"`
	AgentID          string `json:"agentId"`
	ArkAgentID       string `json:"arkAgentId"`
	ArkEnvironmentID string `json:"arkEnvironmentId"`
	CreatedAt        string `json:"createdAt"`

	// SkillsFingerprint is the fingerprint of "which set of skills is currently attached to this
	// Ark Agent".
	//
	// # The only reason it exists is that an Ark Agent is created once and cached forever
	//
	// ensureBinding returns right away when it hits an existing binding, and CreateAgent is never
	// called again (see the comment there). So if the user binds a skill after the first
	// conversation, the Ark side never knows; it looks like "I bound a skill, but the agent acts as
	// if nothing happened", with no error.
	//
	// With this fingerprint, every new conversation can compare: if equal, do nothing (zero extra
	// round trips); only if different, call Ark's UpdateAgent once. Computed by SkillsFingerprint;
	// an empty set is an empty string.
	//
	// **No omitempty**: an empty string is a meaningful value ("this agent has no skills"), and a
	// missing key and an empty string should be the same thing here; it does not go into HTTP
	// responses, only into doc, so both decode correctly to an empty string. It is left without
	// omitempty so the field is visible in doc.
	SkillsFingerprint string `json:"skillsFingerprint"`

	// ArkSkillIDs are the ids of that set of skills on the Ark side, only for debugging (they can
	// be matched up in the console).
	//
	// **It takes part in no decision**: decisions always use the fingerprint. It is kept because
	// when something goes wrong, "which skills are actually attached to the Ark Agent" can only be
	// seen here, and Ark has no list API; it cannot be checked outside the console.
	ArkSkillIDs []string `json:"arkSkillIds,omitempty"`

	// SpecFingerprint is the fingerprint of "which system / model / tools this Ark Agent currently
	// has".
	//
	// # It and SkillsFingerprint are two organs with the same disease
	//
	// SkillsFingerprint cures "an Ark Agent is created once and cached forever, and later skill
	// changes go unnoticed". **The same sentence holds word for word for system / model /
	// description / tools**, but back then only skills were brought up to date: the user changes the
	// system prompt in the UI from A to B, `ensureBinding` hits the existing binding and returns, and
	// the Ark side stays A forever; it looks like "I changed the prompt, but the agent still talks the
	// old way", **with no error**, and **forever** (unlike skills, where any add or remove changes the
	// fingerprint and heals it).
	//
	// Confirmed by measurement: create the agent with system="V1" → open a conversation → change to
	// "V2" → open another conversation; Get from the Ark side still returns "V1", and the Agent
	// version stays at 1.
	//
	// # ⚠️ It must be computed from the **full** system produced by arkSpecFor, not agent.System
	//
	// The system sent to Ark is assembled: the user's text + the self-wakeup notes + the
	// `<harness:env>` block (see core/arkSpecFor). Computing the fingerprint from the user's text
	// alone means "changed one env variable" would not trigger an update, and that block is exactly
	// the only source the model has for runtime facts.
	//
	// **Has omitempty**: existing bindings have no such key and decode to an empty string, and an
	// empty string deliberately means "unknown which one is on the Ark side" → not equal to any real
	// fingerprint → brought up to date once on the next conversation. See core.ArkSpecFingerprint
	// (it always returns a non-empty string, precisely for this).
	SpecFingerprint string `json:"specFingerprint,omitempty"`

	// KeyFingerprint marks "which key this set of Ark resources was created with".
	//
	// # Why it must exist
	//
	// Ark Environments and Agents **belong to the project of the key used to create them**. And
	// ensureBinding "reuses the environment of any binding in the same workspace"; back when keys
	// were only workspace-level this was always right (the same workspace always had the same
	// key), but once agent-level credentials came in it no longer holds:
	//
	//	agent A created env-1 with key1
	//	agent B uses key2 and reused env-1        ← using key2 to access key1's project
	//
	// On the Ark side this shows up as "environment does not exist" or an auth failure, and the
	// error **does not point at the real cause at all** (the user sees "credential invalid", while
	// both keys are fine on their own). The ark_skill_uploads comment described the same kind of
	// problem long ago: "not splitting by workspace would give workspace B a skill_id that does not
	// exist in its own project"; back then splitting by workspace was enough.
	//
	// So the reuse boundary narrows from "same workspace" to "same workspace + same key".
	//
	// **Store a fingerprint, not the plaintext and not the mask**: the plaintext must never go into
	// doc (which is stored), and a mask has only 8 meaningful characters and collides for a key
	// with the same first and last four. The fingerprint uses secret.Fingerprint (first 4 bytes of
	// sha256 + length); the collision probability is negligible.
	//
	// **Has omitempty**: existing bindings have no such key and decode to an empty string. An empty
	// string means "unknown which key created it", and sameArkKey decides how to handle it (see
	// the comment there).
	KeyFingerprint string `json:"keyFingerprint,omitempty"`
}

// ArkSkillRef is one skill reference attached to an Ark Agent.
type ArkSkillRef struct {
	// ArkSkillID is the Ark-side skill id (the one CreateSkill returns).
	ArkSkillID string
	// Version is the Ark-side version number; empty means latest.
	Version string
}

// ArkSkillUpload is the mapping record "one version of an AMA skill's content → an Ark skill".
//
// See the ark_skill_uploads section in schema_pg.sql: Ark has no API to update a skill, and
// CreateSkill produces a new id every time, so this mapping has to be recorded ourselves.
type ArkSkillUpload struct {
	WorkspaceID string
	// SkillID is the AMA-side skill id.
	SkillID string
	// ContentHash is the content fingerprint, computed by SkillFingerprint.
	ContentHash string
	// KeyFingerprint marks "which key this skill was uploaded to Ark with".
	//
	// Same reason as ArkBinding.KeyFingerprint: Ark skills belong to the project of the key used to
	// upload them. `ark_skill_uploads` was originally split by workspace (the table comment says
	// "not splitting by workspace would give workspace B a skill_id that does not exist in its own
	// project"); that argument has to be made again once agent-level credentials exist: **one
	// workspace may also have two keys**.
	//
	// An empty string is an existing row (uploaded before the upgrade); those were always uploaded
	// with the workspace key of the time.
	KeyFingerprint string
	ArkSkillID     string
	ArkVersion     string
	CreatedAt      string
}

// Ref turns the upload record into a reference for attaching to an Agent.
func (u ArkSkillUpload) Ref() ArkSkillRef {
	return ArkSkillRef{ArkSkillID: u.ArkSkillID, Version: u.ArkVersion}
}

// ISOLayout is this project's timestamp layout: fixed three decimal digits, ending in a literal Z.
//
// It matches the same-shaped isoLayout in model/apimodel, but that one is private, while both core
// and repository/dao need it to parse the same strings, so a public copy lives here.
//
// ⚠️ **Do not replace it with time.RFC3339**: when parsing it accepts both .000 and no fraction,
// but when formatting it drops .000, so the same instant appears as two different strings in the
// database; and the frontend sorts by **string**, so mixing the two shapes directly changes the
// sort result (`.`(0x2E) < `Z`(0x5A)). See "Timestamps: units and format" in CLAUDE.md.
const ISOLayout = "2006-01-02T15:04:05.000Z"

// AuthzRole is "one matching member row" on the permission-check path, carrying only the fields
// the check really needs.
//
// # Why the permission check needs its own narrow type
//
// The permission check asks only one thing: "what role does this actor have in this workspace".
// Using apimodel.Workspace (the whole aggregate, with **all members**) would fetch 300 rows for
// nothing on every check in a 300-member workspace, while the check only cares about **one** row.
type AuthzRole struct {
	// Role is the role this row grants.
	Role apimodel.WorkspaceRole
}

// LarkUserToken is the OAuth credential from Feishu "user authorization", in stored form.
//
// # ⚠️ It lives in storemodel and not domain, because it **must never go into an HTTP response**
//
// This package README's rule is "will it go into an HTTP response", and this type carries two
// plaintext tokens; once it appears in any response body, the credentials leak for good (into the
// browser, logs, maybe model context). Putting it in domain would make "just put it into some DTO"
// far too easy, and that leak gives no error. What the frontend needs goes through
// core.IntegrationState (which only has redacted values and metadata like "valid until").
//
// # The two tokens are **plaintext** at this layer
//
// Encryption and decryption are kept inside the Store implementation (sealKey/openKey in coredao),
// so this struct is plaintext going in and out of Store; exactly the same handling as ArkSecret.
// See coredao/store_pg.go for why: the Service layer's structs move around in memory, and
// forgetting to decrypt anywhere would send ciphertext out as a token.
type LarkUserToken struct {
	// Actor is the email of the person who authorized.
	Actor string
	// LarkAppID is the Feishu app id (cli_xxx). Together with Actor it forms the primary key.
	LarkAppID string

	// AccessToken is the one used to call Feishu APIs; expires in about 2 hours. **Plaintext**.
	AccessToken string
	// RefreshToken is the one used to renew. **Plaintext**.
	//
	// ⚠️ This is the only credential in this repo that "must be stored decryptable": renewal must
	// send it back to Feishu as is, so storing sha256 is not possible. See the header of
	// schema/core_larkuser_pg.sql.
	RefreshToken string

	// ExpiresAt / RefreshExpiresAt are the expiry times of the two tokens.
	//
	// ⚠️ **Both must be stored**: access expiring only means one renewal (invisible to the user),
	// while refresh expiring means **the user must scan again**. Mixing them up would leave the card
	// unable to say whether "you need to do something".
	ExpiresAt        time.Time
	RefreshExpiresAt time.Time

	// Scope is the permissions actually granted at authorization (space separated). Not secret; for
	// debugging.
	//
	// Reading group history needs im:message.group_msg:get_as_user, and without it Feishu rejects
	// the call; then this column tells you it was "not granted at authorization", not a code
	// problem.
	Scope string
	// OpenID / UserName are the Feishu-side identity, purely for display and debugging. Not secret.
	OpenID   string
	UserName string

	CreatedAt time.Time
	UpdatedAt time.Time
	// LastRefreshError is the reason for the most recent renewal failure; cleared on success.
	//
	// ⚠️ **Must never contain any token**: it goes into HTTP responses via IntegrationState. Its
	// use is to separate "network blip" (warning, fixes itself next time) from "authorization
	// revoked" (error, must re-authorize); see the lesson that credential validity needs an
	// "unknown" state: with only two states, a blip is treated as invalid, and users re-authorize
	// again and again.
	LastRefreshError string
}

// LarkAuthSession is the device-code intermediate state of Feishu user authorization, in stored
// form.
//
// ⚠️ **It must be stored, not kept in process memory**: production amad runs multiple instances,
// and the "start authorization" request and the "is it done yet" polling request often land in
// different processes. In memory, the symptom is "clicked authorize, the page spins forever", and
// it is **intermittent** (it works if luck lands it on the same instance), the hardest kind to
// debug.
//
// ⚠️ DeviceCode is as sensitive as a secret (it can be exchanged for a token); **it never goes
// into any HTTP response**.
type LarkAuthSession struct {
	// Actor is the person who started authorization, LarkAppID the Feishu app to authorize. The two
	// form the primary key: one person should have only one pending flow per app at a time, and
	// starting again overwrites it.
	Actor     string
	LarkAppID string
	// DeviceCode is Feishu's polling handle. **Not logged, not in responses.**
	DeviceCode string
	// PollInterval is the poll interval in seconds suggested by Feishu, passed to the frontend as is.
	PollInterval int
	CreatedAt    time.Time
	// ExpiresAt is when this authorization flow becomes void (derived from Feishu's expires_in).
	//
	// ⚠️ On read, **expired means treat as missing**, otherwise the user would keep polling with a
	// device_code Feishu voided long ago, and the error would not point at "start again".
	ExpiresAt time.Time
}

// FileRecord is one row of the `files` table: metadata of one uploaded file, and where it is in
// object storage.
//
// **Not in HTTP responses** (hence here and not in model/apimodel): the attachment response shapes
// are still apimodel.IssueAttachment / SuggestionAttachment, unchanged by a single field. This
// record is internal to the storage layer; the frontend does not need to know which key in which
// bucket holds the bytes.
//
// The full design rationale (why this table exists, why business tables hold file_id references,
// how consistency is traded off) is in the header of repository/schema/files.sql.
type FileRecord struct {
	ID string
	// Bucket and ObjectKey together locate the bytes.
	//
	// ⚠️ **Both must be stored**: after a bucket name change, old records must still read from the
	// original bucket. Storing only the key turns "where the object is" into a constant in code, and
	// changing that constant breaks every historical attachment.
	Bucket    string
	ObjectKey string

	Filename string
	// ContentType is the type **sniffed by the server**, not the one the client declared.
	// This is a security constraint, not metadata; see the comment on that column in files.sql.
	ContentType   string
	Size          int
	ContentSHA256 string
	Author        string
	// CreatedAt is an ISO string (apimodel.NowISO(), fixed three decimal digits).
	CreatedAt string
}

// AgentSyncTaskRow is one row of the agent_sync_tasks table.
//
// # Why not use apimodel.AgentSyncTask directly
//
// The rule is the one in this directory's README: "will it go into an HTTP response". The three
// lease fields (ClaimedBy / ClaimedUntil / ResyncRequested) **only serve mutual exclusion across
// instances**; the frontend should not see them, and no frontend behavior should depend on them.
// Put them in the contract type, and sooner or later someone uses claimedUntil in the frontend to
// compute "how much longer", when that value means "when the lease expires", not "when it
// finishes".
//
// Times are time.Time rather than ISO strings: at this layer they take part in database comparisons
// (the claim condition is `claimed_until < now`), while the domain side has fixed-length ISO strings
// for display. Both are written together in one write; see "Timestamps: units and format" in
// CLAUDE.md for the rule.
type AgentSyncTaskRow struct {
	ID          string
	WorkspaceID string
	AgentID     string
	// Status is one of the four apimodel.AgentSyncStatus values; it is a string here so storemodel
	// does not depend on domain's enum (this package is a leaf package; see the README).
	Status          string
	FailureReason   string
	FailureStage    string
	AttemptCount    int
	ResyncRequested bool
	ClaimedBy       string
	ClaimedUntil    time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	// A non-zero FinishedAt means a final state was reached. Zero means not finished yet; **this
	// is also the predicate of that partial unique index** (`where finished_at is null`), so it also
	// decides "can this agent queue another entry right now".
	FinishedAt time.Time
	// Ord orders tasks queued at the same instant; maps to the PG bigserial column.
	//
	// **Getting "the latest one" cannot compare CreatedAt alone**: two entries queued in the same
	// millisecond (delete and recreate, or the user clicking twice) cannot be ordered by time alone,
	// and PG may swap ties freely; that would make the frontend occasionally read the previous,
	// finished task and show "syncing" as "done".
	Ord int64
}

// TerminalTicketRow is a one-time entry ticket for the terminal WebSocket.
//
// Maps to the terminal_tickets table; the DDL and the full argument for "why it must be stored, why
// redemption must be atomic" are in the header of repository/schema/terminal_tickets.sql.
//
// Only appears in the storage layer: **the ticket value never goes anywhere beyond the HTTP
// response**; the issuing API returns a one-time string to the frontend, after which it only
// appears as a query parameter on that one WS handshake.
type TerminalTicketRow struct {
	// Ticket is the ticket value (32 bytes from crypto/rand, hex).
	Ticket string
	// RunID restricts this ticket to opening the terminal of this one run.
	//
	// ⚠️ The WS side must check it. Without that, any valid ticket could open the terminal of **any**
	// run: "one-time" done right but the authorization scope lost, and the latter is the real
	// security boundary.
	RunID string
	// Actor is the identity at issue time (lowercase email).
	//
	// The WS connection has no cookie, so **this is its only source of identity**; every later
	// action uses it, and no identity claimed by the client is accepted.
	Actor string
	// ExpiresAt is the expiry time. Not in the HTTP contract, only for database comparison, so it is
	// time.Time rather than an ISO string (see "pick a column type by purpose" in CLAUDE.md).
	ExpiresAt time.Time
	// A zero UsedAt means never redeemed. A time rather than a bool: when something goes wrong you
	// need to answer "when was this ticket used".
	UsedAt time.Time
	// CreatedAt is the issue time; with UsedAt it shows how long the ticket was held.
	CreatedAt time.Time
}

// Host is a host machine registered by a user (`ama daemon` running permanently on the user's own
// Mac/Linux), in stored form. **An account-level resource**: the owner is the person who registered
// it; the binding between machine and workspace is another table, WorkspaceHost. See the header of
// repository/schema/hosts.sql for the table structure and background.
//
// ⚠️ This type does not go into HTTP responses: it carries TokenHash (the sha256 of the token) and
// must never leave the server. The host view for the frontend is built separately in service/host.
type Host struct {
	// ID is the opaque primary key (`hst_` prefix), used inside and outside to build URLs; HostUUID is not used for that.
	ID string
	// OwnerEmail is the lowercase email of the registrant (owner).
	OwnerEmail string
	// Name is the host name shown to users; may be renamed repeatedly and duplicated; not used in routing.
	Name string
	// OS / Arch are runtime.GOOS / GOARCH: darwin/linux, amd64/arm64.
	OS   string
	Arch string
	// HostUUID is a random id generated on the machine on the daemon's first start, bound to the
	// local credentials file. Registering the same machine again idempotently hits the same row; the
	// unique key is (OwnerEmail, HostUUID): different people registering the same shared machine are
	// different rows.
	HostUUID string
	// TokenHash is the sha256 of host_token; empty means not issued yet. The plaintext token is
	// returned to the daemon only once at registration/reissue; the database only ever has the hash.
	TokenHash string
	// Status is online / offline. An empty string reads as offline.
	Status string
	// DaemonVersion is the daemon version reported in heartbeats, purely for diagnostics.
	DaemonVersion string
	// GNUToolsOK / Toolcheck are probe results from old daemons (the GNU-dialect shell script
	// protocol); daemons on the new protocol no longer report them, and the execution gate ignores
	// them; the columns and fields are kept for compatibility with old data.
	GNUToolsOK bool
	Toolcheck  string
	// NativeTools is the capability bit for the daemon supporting the structured native tool
	// protocol (KindTool). Running tools on old daemons without it is rejected outright with
	// "please upgrade ama daemon".
	NativeTools bool
	// CreatedAt is the first registration time.
	CreatedAt time.Time
	// LastSeenAt is the most recent heartbeat time. **A nullable column: the zero time.Time maps to
	// SQL NULL**, meaning no heartbeat was ever received after registration. The PG layer scans it
	// with sql.NullTime, the same convention as the other nullable timestamptz columns in this file
	// (FeedbackRecord.LastSeenAt and so on), without exposing sql.NullTime in the row type; callers
	// always treat the zero value as "never online".
	LastSeenAt time.Time
	// LastDispatchSeq is the highest downstream command sequence number assigned to this host. The
	// counter is deliberately not kept in the host_dispatches table: done rows there are purged
	// periodically, and taking the next number from max(seq) would wrap back to 1, so the last_seq
	// cursor persisted by the daemon could never fetch new commands again (commands silently lost).
	// Enqueueing uses greatest(existing max(seq), this column)+1 under the same per-host advisory
	// lock, so even with old and new code running side by side during a rolling deploy numbers never
	// collide. See host_dispatches.sql.
	LastDispatchSeq int64
}

// WorkspaceHost is the binding "this workspace may use this host", one per row, in stored form.
// Its shape copies WorkspaceIntegration (account-level resource → workspace binding); see the table
// comment in repository/schema/workspace_hosts.sql.
type WorkspaceHost struct {
	// ID is the binding primary key (`wsh_` prefix).
	ID string
	// WorkspaceID is which workspace.
	WorkspaceID string
	// HostID references hosts.id. When the host is deleted this row is removed by foreign key cascade.
	HostID string
	// AddedBy is who bound it, for auditing; it does not mean "whose identity executes".
	AddedBy string
	// CreatedAt is the binding time; lists sort by (CreatedAt, ID) ascending.
	CreatedAt time.Time
}

// BoundHost is a combined row "host row + its binding info in a workspace";
// Store.ListWorkspaceHosts uses it to bring the two workspace_hosts columns along: who registered
// the machine is Host.OwnerEmail, who bound it into the workspace is AddedBy (if the binding is
// removed and someone else binds it again, AddedBy changes and OwnerEmail does not). Only for the
// read path, never stored.
type BoundHost struct {
	Host
	// BindingID is the binding row id (wsh_ prefix), for unbinding/debugging.
	BindingID string
	// AddedBy is who bound this machine into the workspace (lowercase email). Old data may be an empty string.
	AddedBy string
	// BoundAt is the binding time.
	BoundAt time.Time
}

// HostDispatch is one downstream command from amad → the host daemon (an exec call or a web
// terminal frame), in stored form. State machine queued → delivered → done; see the header of
// repository/schema/host_dispatches.sql for the table structure and redelivery semantics.
type HostDispatch struct {
	// ID is this command's id (`htc_` prefix); the daemon includes it when reporting the result.
	ID string
	// HostID is which machine it is sent to, and also the NOTIFY channel payload.
	HostID string
	// Seq is a sequence number increasing monotonically per host, assigned serially in the enqueue
	// transaction; the daemon's persisted cursor only fetches rows with a larger seq.
	Seq int64
	// Kind is exec / term_open / term_input / term_resize / term_close.
	Kind string
	// State is queued / delivered / done.
	State string
	// SessionID is the Ark session id for exec (== run id).
	SessionID string
	// TerminalID is the browser terminal id that term_* frames belong to.
	TerminalID string
	// Payload is the raw downstream JSON (ExecSpec / terminal frame), stored and fetched as a whole, never queried by inner fields.
	Payload string
	// Result is the raw exec result JSON; always empty for terminal frames. It must be returned to
	// the upper layer byte for byte, so it is text, not jsonb (jsonb normalizes whitespace).
	Result string
	// CreatedAt is the enqueue time.
	CreatedAt time.Time
	// DeliveredAt is the most recent delivery time (including redelivery after expiry). **A nullable
	// column: zero = NULL**; queued rows never claimed are zero.
	DeliveredAt time.Time
	// ClaimedUntil is when the claim lease expires; after that, delivered rows can be fetched again.
	// **A nullable column: zero = NULL**; queued / done rows have no lease.
	ClaimedUntil time.Time
	// UpdatedAt is the most recent state change time.
	UpdatedAt time.Time
}
