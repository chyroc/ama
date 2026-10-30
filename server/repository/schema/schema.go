// Package schema keeps all SQL/DDL in one place and go:embeds it into the binary.
//
// The point of keeping it in one place: to find a DDL script you look in one directory, instead of
// digging through the core / auth / feishu packages.
//
// # One table per file; the file name is the table name
//
// Each .sql file covers one table only, **file name = table name**. To find the definition of the
// runs table, open runs.sql. All statements of a table (create table, indexes, comments) are in its
// own file, so changing a table touches only one file, and the file name in the diff says directly
// which table changed.
//
// # The order is stated explicitly by the Order lists below, not by sorting file names
//
// The only cross-file dependencies are foreign keys (a referenced table must be created first); the
// apply order is stated explicitly in code, keeping "referenced tables first". Why not add number
// prefixes to file names (010_workspaces.sql):
//
//   - that conflicts with "file name = table name";
//   - number prefixes encode the order into file names, so inserting a table means either renaming
//     a run of prefixes or leaving gaps;
//   - the order of `ls` / `filepath.Glob` is **not guaranteed to be the same across environments**
//     (it depends on the file system and locale); sorting by it hands the apply order to the
//     deployment environment, and that kind of error can never be caught locally.
//
// # Why embed.FS instead of a pile of string variables
//
// After one `//go:embed all:*.sql`, **adding a table takes only two steps**: add a .sql file and add
// a line to the right Order list. There is no "added the file but forgot to embed it" gap that only
// shows up on a real database.
//
// ⚠️ **The `all:` prefix cannot be dropped.** Without it embed **silently skips** files starting with
// `_` or `.`. There are no such file names today, but that kind of gap gives no error (the script is
// quietly missing from the package, and then the table is quietly never created), while adding
// `all:` costs nothing.
//
// ⚠️ **go:embed cannot reach outside the package directory**; this is a language-level limit
// (`//go:embed ../schema/x.sql` fails to compile). So the embed statement must be in the same
// directory as the .sql files, and each business package calls this package's CorePG() / AuthPG() /
// FeishuPG().
//
// # The directory is called schema, not sql
//
// All three store_pg.go files import the standard library `database/sql`. If the directory were
// called sql, the package would either also be called sql and clash at every call site, or the
// directory name would differ from the package name; both would need an alias at the import.
//
// # These scripts run at every startup
//
// They are all create table / index if not exists, safe to re-run. The callers (the migrate of each
// store_pg.go) only swallow the two SQLSTATEs 42P07 / 42710, the normal result of multiple instances
// creating tables concurrently. Do not change this behavior.
//
// ⚠️ **This is a brand-new schema with no migration mechanism.** To change the structure of tables
// in a database that already has data later, `create table if not exists` will not add columns and
// `create index if not exists` will not change predicates; you must write
// `alter table ... add column if not exists` explicitly, and think through the window where old and
// new instances coexist during a rolling release (adding a column can ship with the code in the same
// release; dropping a column must span two releases). See README.md in this directory.
//
// # The dao/ next to it
//
// The implementations are in repository/dao/{coredao,authdao,feishudao,coremem}, **split into
// subpackages instead of flat**: each of them has names like PGStore / NewPGStore / pgFactory /
// testSchema, which would clash if flat; with subpackages each keeps its natural names, and each test
// binary holds its own sync.Once.
//
// The Store interfaces stay in core (the consumer declares the interface, the direction dependency
// inversion requires); only the implementations moved. The contract test suites live in dao with the
// PG implementations, to avoid a core ↔ dao test import cycle.
//
// ⚠️ coremem (the production fallback for AMA_STORE=mem) must not use a single core symbol: core's own
// internal tests construct MemStore, and importing core would create the test cycle
// core -> coremem -> core.
package schema

import (
	"embed"
	"fmt"
	"strings"
)

// files is all the DDL scripts in this directory.
//
// ⚠️ **The `all:` prefix cannot be dropped**; see the package comment: without it files starting with
// `_` / `.` are silently skipped.
//
//go:embed all:*.sql
var files embed.FS

// Script is one DDL script: the file name (= table name + .sql) plus the content.
//
// Name is included so that **errors point to the exact table**: "core: failed to create table
// (runs.sql)".
type Script struct {
	Name string
	SQL  string
}

// ---------------------------------------------------------------------------
// core: the six resource domains, plus the account-level / platform-level tables
// ---------------------------------------------------------------------------
//
// # The tables with a doc column (agents / skills / runs / schedules / ark_secrets / ark_bindings ...)
//
// ⚠️⚠️ **This is an existing shape, not the current rule; do not put doc into new tables following
// it.** The current rule is "expand fields into columns, use a join table for one-to-many; use jsonb
// only when the set of fields truly cannot be listed, or it is a whole raw payload nobody queries";
// the full version and the decision questions are in "Default rules for table design" in README.md
// in this directory.
//
// These tables are "one aggregate per table + the whole entity stored as jsonb": the extracted scalar
// columns only serve filtering / uniqueness / sorting, and doc is the authoritative data for all
// other fields. The original reason was "serializing the whole thing cannot lose fields"; **that
// reason does not hold**:
//
//   - **jsonb is not safer than columns; it just moves "a missing field" from compile time to run
//     time, where nobody notices.** Change a json tag and the Put-then-Get round-trip cases stay all
//     green (serializing and deserializing with the same tags is self-consistent but does not pin the
//     shape), while rows already in the database read back empty strings, with zero errors
//     throughout. **A missing column is a compile error or 42703; a missing doc key is data loss
//     found weeks later.**
//   - **PG cannot put a unique constraint on jsonb array elements**, so "one-to-many in a doc array"
//     cannot even deduplicate. That is why workspace_members and skill_files were built as join
//     tables.
//
// ⚠️ **"Goes into the HTTP contract" is not a reason to use doc**: the issues / suggestions domains
// are all real columns without a single doc, and they go into the contract just the same. **Being in
// the contract and using columns are compatible.** A new jsonb column is stopped by
// jsonbguard_test.go.
//
// # Exception: run_events.raw cannot use jsonb
//
// The **raw bytes** of apimodel.RunEvent.Raw are written straight into the HTTP response, to get
// around encoding/json's HTML escaping (< > & would be escaped, while JS's JSON.stringify does not
// escape them). jsonb reorders keys and squeezes whitespace, which would change those bytes. So raw
// is kept unchanged in a **text column**.
//
// # The ord column: a sortable anchor for "insertion order"
//
// MemStore's ListAgents / ListSkills / AgentsBoundTo return **insertion order** (slice order), while
// SQL rows have no inherent order: without ORDER BY, PostgreSQL may return them in another order
// after any VACUUM, page split or parallel scan. ListRuns needs it too: MemStore uses
// sort.SliceStable, keeping insertion order when startedAt is equal, so the matching SQL is
// `order by started_at desc, ord asc`.
//
// workspaces does not use ord; it uses (created_at, id); see workspaces.sql for why. **Do not see
// "workspaces does not use it" and delete them all on the way**: schedules.ord is part of the
// `for update skip locked` grab in ClaimDueSchedules, and store_pg_issue.go on the issues side says
// plainly it cannot be replaced by created_at.
var coreOrder = []string{
	"workspaces.sql",
	"workspace_members.sql",
	"agents.sql",
	"skills.sql",
	"skill_files.sql",
	"ark_secrets.sql",
	"ark_bindings.sql",
	"ark_skill_uploads.sql",
	"runs.sql",
	"run_events.sql",

	// Scheduled tasks.
	"schedules.sql",
	// Entity revisions (snapshots of agent / skill / schedule before a change), for rolling back
	// mistakes.
	//
	// It comes after the three snapshotted entities only for readability; **there is no real order
	// dependency**: it has no foreign keys (see "No foreign keys" in that file's header).
	"entity_revisions.sql",
	// Onboarding state; the primary key is the user's email, not organized by workspace.
	"user_onboarding.sql",
	// Problem feedback, cross-workspace, rate-limited by the dedup_key unique index.
	"feedback_reports.sql",
	// User preferences (language and appearance), account-level, primary key email.
	"user_preferences.sql",
	// Account-level Ark credentials.
	"global_ark_secrets.sql",
	// Agent-level Ark credentials (the most specific of the three config levels).
	"agent_ark_secrets.sql",
	// The task ledger of "sync to Ark right after saving an agent" (claim / retry / result polled by
	// the frontend).
	"agent_sync_tasks.sql",
	// One-time entry tickets for the terminal WebSocket.
	//
	// ⚠️ It must be stored in the database, not in the process: issuing and using the ticket are two
	// requests that almost certainly land on different instances. See that file's header.
	"terminal_tickets.sql",

	// The tool result ledger of the self_hosted worker (cross-instance, isolated per session).
	// It must be created before the worker starts; putting it at the end of the core group is enough,
	// as it has no foreign key relation with other tables.
	"selfhost_tool_results.sql",

	// ---- The three issue tables ----
	//
	// The passage below is about where this group differs from feedback_reports; it belongs to none
	// of the tables:
	//
	//	# This is not another form of feedback_reports; the two tables should not be merged
	//
	//	They look alike (both are "something is wrong"), but they are exact opposites on four axes:
	//
	//	  feedback_reports                                issues
	//	  ----------------------------------------------  ----------------------------------------
	//	  collected by machine (frontend catch stores it)  title and body written by a person
	//	  content **rebuilt** from an allow list           user input, stored as is
	//	  cross-workspace, workspace_id nullable           workspace-level, not null
	//	  one-off, deduplicated with occurrences+1         has status changes, comments, gets edited
	//	  only the dev team can read (email allow list)    workspace members read by role
	//
	//	**Merging would break feedback's core invariant**: the whole value of that table is "every
	//	field collected without the user knowing goes through an allow list" (see
	//	core/README-feedback.md). Adding a free-input body column would open a free-text hole in a
	//	table that promised "never to collect user input", and the feedback.neverCollected line in
	//	i18n is **a promise, not decoration**.
	//
	//	Putting feedback into issues the other way does not work either: it is rate-limited by the
	//	dedup_key unique index (one broken endpoint can produce hundreds of rows a minute), while
	//	issues **reported by people** must not be deduplicated: two people reporting the same thing
	//	should be two rows.
	//
	//	⚠️ "Must not be deduplicated" has one **explicitly bounded exception**: issues produced by a
	//	bot (scheduled inspection) are merged by origin + fingerprint. The reason and scope are written
	//	next to the partial unique index at the end of issues.sql; in one sentence: that invariant is
	//	about **people** (two independent testimonies should not be merged), while a bot rescans the
	//	same material N times, and not merging would only flood the list.
	//
	//	# Why attachment bytes cannot be stored on local disk
	//
	//	In a multi-instance deployment each instance's disk is its own and lost on rebuild. An upload
	//	lands on instance A while the read request is sent by the load balancer to instance B; the
	//	symptom is "the image sometimes shows and sometimes is broken", **with no error**, the hardest
	//	kind to track down. So bytes go into shared storage outside the process: S3-compatible object
	//	storage (shared/infra/s3), metadata in the files table, and attachment rows point to it by
	//	file_id. **PG stores no file bytes.** See the header of files.sql.
	//
	//	# Timestamps are text, not timestamptz
	//
	//	Fixed-length ISO strings with three decimals (apimodel.NowISO()), same as runs.started_at:
	//	lexical order equals time order, and the whole site sorts by string comparison. Converting to
	//	timestamptz and back would drop the .000, which changes the sort result directly. See the
	//	comment at the top of model/apimodel.
	//
	//	(feedback_reports.created_at is the only timestamptz, because only it needs a database-side
	//	time comparison with a query parameter. There is no such query here.)
	"issues.sql",
	"issue_comments.sql",
	"issue_attachments.sql",

	// ---- The three product suggestion tables ----
	//
	// ⚠️ **This is the third kind of "something is wrong" thing**, different from both
	// feedback_reports and issues; none of the three should be merged:
	//
	//	The repository already has feedback_reports and issues, and **a whole section of docs was
	//	already written just to tell them apart because their names look alike**
	//	(core/README-issue.md). This is the third, different from both on four axes:
	//
	//	              feedback_reports              issues                      suggestions
	//	  ----------  ----------------------------  --------------------------  ------------------------------
	//	  written by  machine (frontend catch)      a person                    **a person**
	//	  belongs to  cross-workspace, nullable     workspace-level, not null   **platform-level, no workspace
	//	              workspace_id                                              at all**
	//	  about       some request failed           a to-do in this workspace   **an opinion about the AMA
	//	                                                                        platform itself**
	//	  who reads   only the dev team (allow      workspace members by role   **the submitter sees their own +
	//	              list)                                                     the platform team sees all**
	//
	//	**The "belongs to" row is the key, and it is not another way of writing "workspace_id
	//	nullable".** The feedback table has a workspace_id column (the error **happened** to be in some
	//	workspace), while this table **has no such column at all**: the user is giving the platform an
	//	opinion, not giving some workspace one. Keeping an always-empty column would make the next
	//	person think it should be filtered by workspace, and write a filter that always returns empty.
	//
	//	The consequences of merging into either:
	//
	//	  * into feedback_reports: breaks its core invariant. The whole value of that table is "every
	//	    field collected without the user knowing goes through an allow list"; adding a free-input
	//	    body column opens a free-text hole in a table that promised "never to collect user
	//	    input". The feedback.neverCollected line in i18n **is a promise, not decoration**.
	//	  * into issues: issues.workspace_id is **not null**, and that column is **the basis of
	//	    authorization** (authz resolves roles from it). Putting a platform-level thing in would
	//	    require a fake workspace id, which makes Guard check roles against a workspace that does
	//	    not exist; the result is **nobody can read it, and the error says "workspace does not
	//	    exist"**, a failure that does not point to the real cause at all.
	//
	//	# Why this feature is not called feedback
	//
	//	That word is already taken in this repository by "automatic error collection": the
	//	feedback_reports table, the /api/feedback routes, the feedback.readers config, a whole range of
	//	feedback.* in i18n. Using it again costs not naming purity, but **the two things being confused
	//	for a long time in code, in conversations and in logs**, and this repository has already paid
	//	that cost once for "issue and feedback have similar names".
	//
	//	# Deliberately no deduplication; not a single dedup_key / fingerprint column
	//
	//	issues has one bounded exception (bot output merged by origin+fingerprint); **here there is not
	//	even that exception**, because its precondition does not hold: that partial unique index exists
	//	for "scheduled inspection rescanning the same material every X minutes", while suggestions are
	//	all typed by people.
	//
	//	Things submitted by people should not be deduplicated (two people submitting the same
	//	suggestion are two independent requests, and merging would lose "how many people want it",
	//	which is exactly the basis for priority); this matches the sentence in the issues file header
	//	word for word.
	//
	//	The worry about one person submitting ten in a row is real, but **deduplication is not its
	//	fix**: ten suggestions with different content should be ten. Real rate limiting would be by
	//	"count per unit of time", not by merging content. It is not done today; the rule and reason
	//	are in core/README-suggestion.md.
	//
	//	# Timestamps are text, not timestamptz
	//
	//	Fixed-length ISO strings with three decimals (apimodel.NowISO()), same as issues and
	//	runs.started_at: lexical order equals time order, and the whole site sorts by string
	//	comparison. Converting to timestamptz and back would drop the .000, which changes the sort
	//	result directly. The rule is the CLAUDE.md one "will the frontend compare this value as a
	//	string"; the list page does, so it is text.
	"suggestions.sql",
	"suggestion_replies.sql",
	"suggestion_attachments.sql",

	// The shared file table: metadata of attachments and skill files (bytes are in S3-compatible
	// object storage).
	"files.sql",

	// "Which container this Run uses" (container name + its Docker daemon + last activity time).
	"run_sandbox_sessions.sql",
	// When Ark credentials started failing in a row ("after 3 full days, drop to one try per day"
	// must remember it across releases).
	"ark_auth_failures.sql",
	// Messages the user queued to send.
	// ⚠️ Not the same thing as "automatic resend after hitting the tool execution window"; see the top
	// of that .sql file.
	"pending_messages.sql",
	// The generic message queue (shared/infra/pgmq). One table holds all topics.
	// The direct cause was cross-instance delivery for the webshell terminal; see the top of that
	// .sql file.
	"mq_messages.sql",

	// ---- The three tables for OAuth credentials from Feishu "user authorization" ----
	//
	// ⚠️ lark_user_tokens is the only credential table in this repository that **must be decryptable**
	// (the other two kinds are sha256 and "not stored"); the reason is in that .sql file's header:
	// renewal must send the refresh_token back to Feishu unchanged. Both columns go through
	// shared/infra/cryptobox, so coredao **must have the encryptor wired**, otherwise plain text is
	// stored.
	"lark_user_tokens.sql",
	"lark_user_auth_sessions.sql",
	"lark_user_app_prefs.sql",

	// ---- The three host executor tables (user machines brought online by the ama daemon) ----
	//
	// hosts is the account-level machine registry; workspace_hosts binds machines into workspaces
	// (FK cascade); host_dispatches is the downlink queue amad → daemon (exec calls and web terminal
	// frames). None of the three has foreign key dependencies on other business tables (host_id on
	// agent/run is deliberately a soft reference: when a machine is deleted the read path reports it
	// as invalid, instead of the reference locking the host's revocation), so putting them at the end
	// of the core group is enough.
	"hosts.sql",
	"workspace_hosts.sql",
	"host_dispatches.sql",

	// ---- Account-level personal files (/api/files, bytes in object storage) ----
	//
	// One table, no foreign key dependencies (owner_email is a soft reference to the users table, the
	// same as hosts.owner_email), so putting it at the end of the core group is enough.
	"user_files.sql",
}

// ---------------------------------------------------------------------------
// auth: the login session tables
// ---------------------------------------------------------------------------
//
// The passage below is about **why these tables differ from core's** (no jsonb, times as
// timestamptz); it belongs to none of the tables:
//
// ⚠️ **This group is now the model, not the exception.** The new rule (expand fields into columns)
// matches what it does; its sentence below "few fields, so hand-written mapping has no risk" can now
// be read more strongly: **build columns even with many fields**, because the defense against a
// missing field is the compiler and field-by-field round-trip cases, not the number of fields.
//
//	# Why a group of its own
//
//	The core group's notes explain the shared design of "the six resource domains" (one aggregate
//	per table + the whole entity stored as JSONB), while login sessions neither belong to those six
//	domains nor follow that design; see "Why no jsonb here" below.
//
//	# Why no jsonb here
//
//	Those six tables store jsonb because **their HTTP contract is byte-sensitive**: passing the
//	whole entity through JSON as is keeps the shape unchanged. The auth tables do not face that
//	contract: Session / Login are internal server structures, and the frontend sees a few fields
//	the handler builds on the spot. So here they are honestly split into columns, with the benefits:
//
//	  * every column has a clear type; expiry checks and deleting by email can use SQL and indexes
//	    directly;
//	  * few fields (8 / 10 columns), so hand-written mapping has no risk of "missing a field and
//	    going wrong silently".
//
//	# Times are timestamptz, not text
//
//	Same reason as schedules' next_run_at: expiry cleanup needs time comparison in the database
//	(`delete from ... where expires_at < $1`). Core's six tables store times as fixed-length ISO
//	strings because the frontend sorts them as strings; there is no such constraint here.
//
//	Note: **"now" is passed in from the Go side; SQL's now() is not used**. This matches feishu's
//	session table (see "Time comes from the Go side" in the header of feishudao/store_pg.go):
//	expiry is a business rule and should not follow the database's time zone config, and tests
//	must be able to inject time.
var authOrder = []string{
	"auth_sessions.sql",
	// In-progress state of OIDC login (state / nonce / PKCE verifier).
	"auth_oidc_states.sql",
	// In-progress state of the GitHub device flow (device_code / poll interval / hash of the binding
	// cookie).
	"auth_github_flows.sql",
	"auth_api_keys.sql",
	"users.sql",
	// Local accounts (email + password) and failure counts of password login. Both only reference
	// users' email softly, with no foreign key dependency; they come after users only for readability.
	"auth_local_accounts.sql",
	"auth_login_failures.sql",
}

// ---------------------------------------------------------------------------
// feishu: the seven Feishu integration tables
// ---------------------------------------------------------------------------
//
// The passage below is about the shared design of this group; it belongs to none of the tables:
//
// ⚠️⚠️ **Same as the core passage: this is an existing shape, not the current rule.** New tables are
// built with "expand fields into columns, join tables for one-to-many"; see the core group in this
// file and README.md for why.
// ⚠️ Note this group is **already split in two**: feishu_apps / feishu_sessions use doc (that is
// what the passage below is about), while the five added later (inbound_messages / inflight_turns /
// chats / session_scopes / progress_deliveries) **are all real columns**, and each of their .sql
// files says "the reason above for putting the whole struct into doc does not hold here". **The last
// five are the model now.**
//
//	# Why "one aggregate per table + the whole entity in doc"
//
//	The HTTP contract is sensitive to fields (the frontend's LocalApiClient is the source of truth
//	of the contract); with a hand-written set of column mappings, missing one field gives no
//	compile error and tests may not notice; only some corner of the frontend suddenly stops showing
//	it. Serializing the whole Go struct into jsonb naturally misses nothing: the struct decides
//	whether fields are added or removed.
//
//	Only the scalars "used for filtering or unique constraints" are pulled out as real columns:
//
//	  feishu_apps.workspace_id   ListApps filters by workspace
//	  feishu_apps.app_id         idempotent registration relies on hitting its unique key
//	  feishu_apps.created_at     the list is in ascending order of connection time
//	  feishu_sessions.expires_at expired sessions must be swept
//
//	jsonb reorders keys and drops duplicate keys, but that does not matter: the order of fields in
//	the response is decided by the Go struct at marshal time, not by the key order stored in the
//	database.
//
//	One known limit: jsonb **does not accept NUL bytes in strings**. An app name reported by Feishu
//	could in theory contain a NUL byte, and then the insert here fails. jsonb was chosen over json
//	for indexing and deduplication; that cost is acceptable.
var feishuOrder = []string{
	"feishu_apps.sql",
	"feishu_sessions.sql",
	"feishu_inbound_messages.sql",
	"feishu_inflight_turns.sql",
	"feishu_progress_deliveries.sql",
	"feishu_chats.sql",
	"feishu_session_scopes.sql",
}

// ---------------------------------------------------------------------------
// channel: the tables shared by chat channels, plus the WeChat channel's own tables
// ---------------------------------------------------------------------------
//
// All real columns, no doc. Feishu predates this group and keeps its own tables for the same jobs.
// No foreign keys, so the order is only for reading.
var channelOrder = []string{
	"channel_inbound_messages.sql",
	"channel_session_scopes.sql",
	// WeChat: connected bots and QR-code logins in progress.
	"weixin_accounts.sql",
	"weixin_logins.sql",
}

// CorePG returns the DDL scripts of the core tables **in apply order**.
//
// The caller (coredao's migrate) runs them one by one. **Why not join them into one big string**:
// applied separately, the 42P07 hit by "multiple instances creating tables concurrently" only skips
// **that one** script; joined into one, the whole thing runs in one implicit transaction, and one hit
// halfway would drop the dozens of tables after it too; see the part in migrate that swallows
// SQLSTATEs. Splitting them makes error handling more precise.
func CorePG() []Script { return load(coreOrder) }

// AuthPG returns the DDL scripts of the auth tables, in apply order.
func AuthPG() []Script { return load(authOrder) }

// FeishuPG returns the DDL scripts of the seven feishu tables, in apply order.
func FeishuPG() []Script { return load(feishuOrder) }

// ChannelPG returns the tables shared by chat channels (server/channel) and the WeChat channel.
func ChannelPG() []Script { return load(channelOrder) }

// load reads the scripts in the given order.
//
// ⚠️ **It panics if one cannot be read**, instead of returning an error or skipping. This is
// deliberate: the content of embed.FS is fixed at compile time, and failing to read at run time has
// only one cause: an Order list above names a file that does not exist (a typo, or a table added to
// the list without creating the file). That is an error that **must be found before deployment**,
// and a panic blows up at process startup; returning an error for the caller to decide would most
// likely be handled by logging a line and carrying on, and then that table is silently never
// created, exactly the kind of zero-error failure this repository keeps guarding against (a missing
// authz route declaration panics, for the same reason).
func load(names []string) []Script {
	out := make([]Script, 0, len(names))
	for _, name := range names {
		blob, err := files.ReadFile(name)
		if err != nil {
			panic(fmt.Sprintf("schema: 读不到建表脚本 %s（检查 repository/schema/ 下有没有这个文件、"+
				"以及 schema.go 的顺序列表里名字是否拼对）: %v", name, err))
		}
		if strings.TrimSpace(string(blob)) == "" {
			panic(fmt.Sprintf("schema: 建表脚本 %s 是空的", name))
		}
		out = append(out, Script{Name: name, SQL: string(blob)})
	}
	return out
}
