// PostgreSQL implementation of Store.
//
// # Relationship with MemStore
//
// MemStore is **the reference behavior**, and this implementation matches it method by method, including the parts that look odd:
// idempotent deletes, lists never returning nil, listing events of a missing run returning an empty slice instead of ErrNotFound,
// deleting a workspace **not** cleaning dangling references from agents in other workspaces. Every deviation from the reference has its reason written down.
//
// In other words, every assertion in store_test.go must pass on both implementations; those tests are already
// parameterized to run once on each (see runStoreSuite in this package's store_test.go).
//
// ⚠️ **MemStore is next door in repository/dao/coremem, not in this package.** Both implementations live under
// repository/dao but in separate packages; see the coremem file header for why: that package **deliberately does not
// import core**, otherwise core's own internal tests would create a cycle when constructing MemStore. This package has no such
// constraint (no package core test constructs PGStore), so it can import core.
//
// This package's suite gets MemStore through coremem's public API; crossing packages does not affect the contract discipline,
// and both implementations still each run once.
//
// # Why the whole entity is stored as JSONB
//
// ⚠️ **This is the historical reason for existing tables, not the rule for new tables.** In one sentence: the HTTP contract is byte-sensitive and sensitive to
// field presence, and round-tripping the Go struct through JSONB as is guarantees the shape stays the same, while a hand-written column mapping
// fails silently when a field is missed.
//
// ⚠️ **The second half of that argument has been overturned** (changing one json tag leaves every test green, yet existing rows read back
// empty strings with zero errors in production); the current rule is "fields become columns, one-to-many uses join tables". The original text, the
// two measured pieces of evidence that overturned it, and how existing tables converge are in the package comment of repository/schema/schema.go and
// repository/schema/README.md. The three workspaces columns in this package are step one of "splitting doc".
//
// # Deep copies are free here
//
// Every MemStore read method must clone explicitly, otherwise a caller changing a return value pollutes the stored state.
// This implementation reads by "fetch bytes from the database → json.Unmarshal into a new object", which is naturally an independent copy,
// with no shared underlying arrays. But **non-nil slices** are not free: `"tags": []` in seed.json
// decodes correctly into an empty slice, but once some slice field in doc is null or missing entirely,
// it decodes as nil, encodes back as null, and the frontend's .map() blows up. So the read path always goes through
// the same clone functions as MemStore (cloneAgent and so on), which guard the non-nil invariant.
//
// # Concurrency
//
// *sql.DB itself is a concurrency-safe connection pool, so this type has no lock. Atomicity across statements relies on transactions
// (client.WithTx), needed only for cascading deletes and Reset / Clear.

package coredao

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/repository/schema"
	"github.com/chyroc/ama/server/shared/infra/cryptobox"
	"github.com/chyroc/ama/server/shared/infra/pg"
)

// The DDL scripts themselves are under repository/schema/, **one table per file, the file name is the table name**,
// shipped with the binary (go:embed).
//
// **The embed statement must be in the same directory as the .sql files** (go:embed cannot cross parent directories; measured:
// `../repository/schema/x.sql` fails to compile), so this only calls it and does not embed.
//
// ⚠️ **The order is decided by schema.CorePG(), not here**. That list spells out the only real
// order dependency (workspaces must come before workspace_members). To add a table change it there, not here.
// Why each table is built the way it is is written at the top of its .sql; do not copy it here again, two copies of a comment drift apart sooner or later.

// pgTables is all the tables this module uses, listed in **a fixed order unrelated to foreign keys**.
//
// The TRUNCATE of Clear / Reset must list them all at once: split into several statements, any one failing in the middle would
// leave a half-deleted database. In one TRUNCATE, PostgreSQL guarantees atomicity.
var pgTables = []string{
	"workspaces",
	// Workspace members. **Must go with the TRUNCATE**: it is a child table of workspaces; without clearing it,
	// members survive clearing the database, so the next Reset loading the seed hits the (workspace_id, lower(email)) unique
	// index with 23505, showing as "loading the seed fails after clearing the database", with the error pointing at the unique constraint
	// and not at "the previous round's members were not cleared".
	"workspace_members",
	"agents", "skills", "skill_files", "ark_secrets", "ark_bindings",
	// Ark skill upload records. **Must go with the TRUNCATE**: after clearing, the bindings are gone too, and keeping the mappings would attach
	// a batch of skill_ids belonging to the old project to newly created Agents (clearing the database often comes with switching keys).
	// Clearing them costs only one re-upload, and those old skills on the Ark side cannot be deleted anyway.
	"ark_skill_uploads",
	"runs", "run_events",
	"schedules",
	// Agent Ark sync tasks. **Must go with the TRUNCATE**: without clearing, the partial unique index "an agent can have only
	// one unfinished task" would block queueing again after clearing, showing as "after clearing the database,
	// saving an agent never triggers sync again", with zero errors.
	"agent_sync_tasks",
	// Entity history versions. **Must go with the TRUNCATE**, and missing it has a more direct consequence than most tables:
	//
	// this table auto-increments revision_no per (entity_kind, entity_id), computed as "query the current max
	// + 1". If clearing removes agents but not this table, **the previous round's history rows are still there**, so
	// a newly created agent (its id reused as the same agent_1 because of restart identity) starts numbering its first change
	// from version 6, and it can even "roll back" to the previous round's agent content.
	//
	// Measured: before this line was added, these contract tests on PG reported "number should be 1, got 2",
	// then hit a primary key conflict right after. The in-memory implementation cannot show it (it creates an empty map each time).
	"entity_revisions",
	// Onboarding state. **Must also go with the TRUNCATE**, even though it is account level and belongs to no workspace.
	//
	// The reason is what Clear/Reset are for: they are the two _debug dev routes (not enabled in production),
	// used to restore the database to **a known initial state** for e2e. Missing this table, onboarding state survives "clearing the database",
	// so the "new user logs in for the first time" scenario **cannot be reproduced** after clearing,
	// which is exactly one of the main uses of clearing.
	//
	// The cost is only that the developer sees onboarding again, and production never reaches this code.
	"user_onboarding",
	// Issue feedback. **Must also go with the TRUNCATE**, for the same reason as user_onboarding:
	// Clear/Reset must give e2e a known initial state, and the automatic records piled up in the feedback table would make
	// "did this error come from this run" impossible to tell. More importantly, **rate limiting would carry across tests**
	// (the dedup key is a ten-minute bucket, so a record from the previous test eats the next test's report),
	// so the assertion "a level-one automatic record was stored" would be red on the second run.
	"feedback_reports",
	// User preferences. **Must also go with the TRUNCATE**, for the same reason as user_onboarding: Clear/Reset
	// must give e2e a known initial state. Keeping them, the assertion "new users follow system by default" would be
	// overridden by a "dark" left over from the last run, and that failure looks like a broken color scheme, not dirty data.
	"user_preferences",
	// Account-level Ark credentials. **Must also go with the TRUNCATE**: it is one of the things most worth clearing when clearing the database
	// (which often comes with switching keys, see the ark_skill_uploads section). Keeping them, the inherit rows on the workspace side
	// would keep falling back to a key from the previous round.
	"global_ark_secrets",
	// Issues and their comments and attachments. **All three must go with the TRUNCATE**, for the same reason as feedback_reports:
	// Clear/Reset gives e2e a known initial state, and keeping the previous round's issues would make
	// the assertion "is the newly created one in the list" read a pile of old data on the second run.
	//
	// The attachments table especially must not be missed: it holds bytes, and without clearing the database grows a chunk with every e2e run,
	// while no page ever shows these orphan attachments; only the disk knows.
	//
	// The order is attachments → comments → issues (children before parents). TRUNCATE in one
	// statement is atomic and does not really depend on order; it is written this way to save the next reader one round of reasoning.
	"issue_attachments", "issue_comments", "issues",
	// Product suggestions and their replies and attachments. **All three must go with the TRUNCATE**, for the same reason as the issue group above.
	//
	// ⚠️ They are **platform level** (that table has no workspace_id), so clearing a workspace does not take them along;
	// only here can they be cleared. Missing them is more hidden than the issue group: suggestions hang under no workspace,
	// so the "my suggestions" list in e2e keeps growing across tests, and every entry looks legitimate.
	//
	// Same order: children before parents.
	"suggestion_attachments", "suggestion_replies", "suggestions",
	// Session container mapping. **Must also go with the TRUNCATE**: after clearing, all runs are gone, and keeping these rows makes them
	// orphans pointing to Runs that do not exist; those containers still take resources on the Docker daemon,
	// while the ledger no longer matches any conversation. After clearing, the reclaim sweep still deletes them by container creation time.
	"run_sandbox_sessions",
	// Start of Ark credential failures. After clearing, environments and bindings are gone, so keeping these is orphan rows pointing to environments that do not exist.
	"ark_auth_failures",
	// Feishu user OAuth credentials. **Must go with the TRUNCATE**: it is account level (belongs to no workspace),
	// but clearing exists to give e2e a known initial state; keeping the previous round's tokens would make
	// the scenario "an unauthorized user sees the 'authorize' button" **impossible to reproduce** after clearing,
	// for the same reason as the user_onboarding section.
	//
	// The cost is only that the developer scans the code once more, and production never reaches this code.
	"lark_user_tokens",
	// Device-code intermediate state of Feishu authorization. Cleared along: keeping them would point to flows that no longer exist after clearing,
	// and they only live a few minutes anyway, so clearing costs nothing.
	"lark_user_auth_sessions",
	// In-flight messages of the general message queue (shared/infra/pgmq). **Must go with the TRUNCATE**: otherwise messages the previous round
	// did not consume would be delivered in the next round, showing as "an extra operation out of nowhere in e2e",
	// while that message came from a test that already ended; nothing would show the connection when debugging.
	"mq_messages",
	// The three host executor tables. hosts is the parent table and must come last: workspace_hosts has a foreign key to it.
	// Within one TRUNCATE the order does not really matter (PG handles everything in one statement); children first saves the reader
	// one round of reasoning, same as the issue group. host_dispatches has no foreign key, so its position does not matter.
	"host_dispatches", "workspace_hosts", "hosts",
	// Account-level personal files, no foreign key dependencies.
	"user_files",
}

// PGStore is the PostgreSQL implementation of Store.
//
// Safe to share across goroutines: the underlying *sql.DB is a concurrency-safe connection pool.
type PGStore struct {
	client *pg.Client
	// crypto is for "encrypt before storing, decrypt after reading", and only applies to the plaintext keys of Ark credentials.
	//
	// **The zero value works** (degrades to plaintext), so existing call sites and probes need no change. Why encryption lives in
	// the Store and not the Service: the secret structs in the Service flow around in memory, get built into views,
	// and get compared, and forgetting to decrypt anywhere would send ciphertext to Ark as the key (401 saying "invalid credential").
	// Keeping it in this layer makes "into and out of the database" the only conversion point, impossible to miss.
	crypto *cryptobox.Box
}

// PGOption is an optional wiring item for PGStore.
type PGOption func(*PGStore)

// WithCrypto gives the Store an encryptor: the plaintext keys of Ark credentials are encrypted before storing and decrypted after reading.
//
// Without it they are stored as plaintext (the behavior before this option was added); see the package comment of shared/infra/cryptobox.
func WithCrypto(box *cryptobox.Box) PGOption {
	return func(s *PGStore) { s.crypto = box }
}

// sealKey turns a plaintext key into the form to store. Returned as is when no encryptor is installed.
func (s *PGStore) sealKey(plain string) (string, error) {
	if s.crypto == nil {
		return plain, nil
	}
	return s.crypto.Seal(plain)
}

// openKey restores that column from the database to plaintext.
//
// **Even without an encryptor it goes through cryptobox's check**: the database may already hold ciphertext (another instance had the key configured),
// and then it must error instead of using ciphertext as the key. So this uses a zero-value Box instead of returning directly.
func (s *PGStore) openKey(stored string) (string, error) {
	if s.crypto == nil {
		var zero cryptobox.Box
		return zero.Open(stored)
	}
	return s.crypto.Open(stored)
}

// Compile-time check that it implements Store. Without this line, a wrong signature would only be found when cmd/amad wires it.
var _ core.Store = (*PGStore)(nil)

// NewPGStore builds a PG Store and creates the table structure (idempotent, can be run repeatedly).
//
// **It does not load the seed**: empty in, empty out. Use NewSeededPGStore for the seed.
func NewPGStore(ctx context.Context, client *pg.Client, opts ...PGOption) (*PGStore, error) {
	if client == nil {
		return nil, fmt.Errorf("%w: pg client 不能为 nil", persistmodel.ErrInvalidArgument)
	}
	s := &PGStore{client: client}
	for _, opt := range opts {
		opt(s)
	}
	if err := s.migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// NewSeededPGStore creates the tables and loads the seed **only when the database is empty**.
//
// "Only when empty" is the whole point of persistence: a process restart must not wipe user data.
// That is exactly the difference from NewSeededMemStore: the in-memory version starts with the seed every time, because it never had
// data from last time anyway.
//
// Emptiness is judged by the workspaces table: the other five tables all hang under workspaces, and without workspaces they mean nothing.
func NewSeededPGStore(ctx context.Context, client *pg.Client, opts ...PGOption) (*PGStore, error) {
	s, err := NewPGStore(ctx, client, opts...)
	if err != nil {
		return nil, err
	}

	var n int
	if err := s.client.QueryRow(ctx, `select count(*) from workspaces`).Scan(&n); err != nil {
		return nil, fmt.Errorf("core: 查空间数失败: %w", err)
	}
	if n > 0 {
		// There is already data; keep it as is.
		return s, nil
	}
	if err := s.Reset(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// migrate runs the embedded DDL scripts.
//
// **One script = one table** (schema.CorePG() gives them in order, 42 of them). Each gets its own Exec:
// with **no parameters** pgx uses the simple query protocol, which allows several semicolon-separated statements run in one implicit transaction;
// with parameters it uses the extended protocol, where multiple statements are not allowed. The scripts have no placeholders, so this path works.
//
// ⚠️ **So the boundary of the implicit transaction is "one table", not "one domain"**; this is a behavior change from the split,
// and it is for the better: core_pg.sql used to cover ten tables in one file, and one 42P07 midway rolled back the whole file,
// **skipping** the remaining nine tables together; now only the clashing table is skipped and the rest are created.
//
// Why each table is built the way it is, and why the order of added columns cannot change, is written at the top of its .sql.
func (s *PGStore) migrate(ctx context.Context) error {
	// DDL goes through a dedicated connection without statement_timeout: statements like building an index on a large table legitimately run
	// longer than the default 10s timeout for business queries. See the comment on pg.WithoutQueryTimeouts.
	return s.client.WithoutQueryTimeouts(ctx, func(ctx context.Context, exec pg.ExecFunc) error {
		for _, script := range schema.CorePG() {
			if _, err := exec(ctx, script.SQL); err != nil {
				if pg.IsPermissionDenied(err) {
					return fmt.Errorf("core: 建表失败（账号没有建表权限——要用有 CREATE 权限的账号，"+
						"通常是把它设成库的 Owner）[%s]: %w", script.Name, err)
				}
				// **These two SQLSTATEs must be swallowed when multiple instances start at once.**
				//
				// `create table if not exists` is not atomic: two processes both see "table does not exist",
				// then both try to create it, and the slower one hits 42P07 (table already exists) or 23505/42710 (pg_type unique key).
				// These two errors exactly mean the table is already there, which is the result we want.
				//
				// amad runs as multiple instances in production, and during rolling deploys old and new instances start together; without swallowing them it shows as
				// **some instance randomly failing to start**, with the error pointing at table creation, not at concurrency.
				// feishudao/store_pg.go handled this long ago; this core one did not before (it does not show with a single instance).
				if state := pg.SQLState(err); state == sqlStateDuplicateTable ||
					state == sqlStateDuplicateObject {
					continue
				}
				// Include the file name (= table name): it used to just say "core: table creation failed", and you had to search through ten tables.
				return fmt.Errorf("core: 建表失败（%s）: %w", script.Name, err)
			}
		}
		return nil
	})
}

// SQLSTATEs that can be ignored during table creation; see the notes in migrate.
const (
	sqlStateDuplicateTable  = "42P07"
	sqlStateDuplicateObject = "42710"
)

// ---------------------------------------------------------------------------
// Encoding and decoding
// ---------------------------------------------------------------------------

// marshalDoc encodes an entity into the bytes of the doc column.
//
// The standard library is enough: doc is **internal storage**, not an HTTP response, so encoding/json's HTML escaping
// does not matter here: it escapes < > & **inside** JSON strings, and the Go values decoded back are exactly the same.
// The only thing that cares about bytes is RunEvent.Raw, which never goes through here (see insertEvents).
func marshalDoc(v any) ([]byte, error) {
	blob, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("core: 编码失败: %w", err)
	}
	return blob, nil
}

// scanDoc decodes the bytes of the doc column back into an entity.
func scanDoc[T any](blob []byte, out *T) error {
	if err := json.Unmarshal(blob, out); err != nil {
		return fmt.Errorf("core: 解码失败: %w", err)
	}
	return nil
}

// queryDocs runs a query that "selects only the doc column", decodes every row and passes it through clone.
//
// clone is not redundant: slice fields in doc that are null or missing decode as nil, while the contract requires [].
// The returned slice is **guaranteed non-nil**; an empty result is a slice of length 0.
func queryDocs[T any](
	ctx context.Context,
	c *pg.Client,
	clone func(T) T,
	query string,
	args ...any,
) ([]T, error) {
	rows, err := c.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]T, 0)
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return nil, fmt.Errorf("core: 读取失败: %w", err)
		}
		var v T
		if err := scanDoc(blob, &v); err != nil {
			return nil, err
		}
		out = append(out, clone(v))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 遍历结果失败: %w", err)
	}
	return out, nil
}

// getDoc fetches and decodes the doc of one row; returns ErrNotFound if not found.
func getDoc[T any](
	ctx context.Context,
	c *pg.Client,
	clone func(T) T,
	query string,
	args ...any,
) (T, error) {
	var zero T
	var blob []byte
	if err := c.QueryRow(ctx, query, args...).Scan(&blob); err != nil {
		if pg.IsNoRows(err) {
			return zero, persistmodel.ErrNotFound
		}
		return zero, fmt.Errorf("core: 查询失败: %w", err)
	}
	var v T
	if err := scanDoc(blob, &v); err != nil {
		return zero, err
	}
	return clone(v), nil
}

// isoToTimeOr turns a fixed-length ISO string into time.Time, falling back to fallback on an empty string or parse failure.
//
// It is separate from isoToTime in store_pg_schedule.go because **the target columns differ in nullability**:
// that one serves next_run_at (nullable; falling back to NULL on parse failure happens to be the safe side, the worst being that this
// task does not run). Here the target columns are created_at / updated_at, **not null**, and falling back to NULL would be
// a constraint violation, with the error pointing at the database, not at "the timestamp passed in had the wrong shape".
//
// So this falls back to the fallback the caller gives (usually now()): a slightly off timestamp does not affect correctness,
// while failing to create a workspace is a failure.
func isoToTimeOr(iso string, fallback time.Time) time.Time {
	if t := isoToTime(iso); t.Valid {
		return t.Time
	}
	return fallback
}

// nullIfEmpty writes an empty string as SQL NULL, used for **nullable columns with omitempty**
// like created_by / updated_by.
//
// # Why not just write an empty string
//
// These two fields have omitempty in doc (see apimodel.Workspace): when empty **the whole key is absent**.
// The migration backfill used `doc ->> 'createdBy'`, which gives SQL NULL for "key absent" and an empty string for "key present but
// empty": two different facts ("never recorded who created it" vs "recorded, and it was empty").
//
// The write path must use the same encoding as the backfill, otherwise the same "unknown who" would have two shapes in the database, NULL and empty string:
// backfilled old rows are NULL, newly written rows are empty strings. Then looking for rows "missing a creator" with `is null`
// would miss half of them, with zero errors.
//
// ⚠️ name does not go through this function: it has no omitempty, and an empty name is a real (though invalid) value,
// not "missing".
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---------------------------------------------------------------------------
// Workspaces
// ---------------------------------------------------------------------------

// The source of truth for members is the workspace_members table, not the members array in doc.
//
// # Read path: after reading a workspace, fill the members back in
//
// The Store contract has not changed: the apimodel.Workspace returned by ListWorkspaces / GetWorkspace still carries
// the full Members slice, and neither the frontend nor authz knows the storage underneath changed. So every read method must
// call fillMembers once after reading the workspace rows.
//
// **The write path no longer touches doc** (step three of "splitting doc" already stopped writing it, see CreateWorkspace), so the old
// risk "also storing members in doc gives the same relationship two storage locations" is naturally gone.
// That inconsistency used to show as "the UI says admin, but it blocks as viewer", with no error; now there is no second place.
//
// # Why the order is (created_at, id)
//
// A join table has no inherent row order, while member order **has meaning**: firstOwnerEmail in core/ark.go
// takes "the first owner", and core/credentialinject.go uses it to decide whose credential to inject.
// Without explicit ordering, a workspace with several owners would randomly switch whose credential is injected, with zero errors.
// id is the tie-breaker because two members can be added within the same millisecond (see that section in schema_pg.sql).
//
// ⚠️ **Adding a member field requires changing four places at once**: this constant, the two scans below, and the two inserts.
// Missing any one does not fail compilation; that field is just silently lost. `source` was lost exactly this way once
// (those two columns are deleted now, see workspace_members.sql). The source of truth for members is
// the workspace_members table, not doc, so the jsonb protection of "a whole round trip never loses fields"
// **does not hold** here.
const memberSelectColumns = `id, member_email, role, name, avatar_url, created_at`

// fillMembers fills in members for a batch of workspaces.
//
// **One SQL fetches everything, not one query per workspace.** ListWorkspacesFor gets all workspaces and then filters,
// so querying one by one would be N+1; the number of workspaces grows with usage, and that path runs every time the sidebar opens.
func (s *PGStore) fillMembers(ctx context.Context, wss []apimodel.Workspace) error {
	if len(wss) == 0 {
		return nil
	}
	ids := make([]string, 0, len(wss))
	for _, ws := range wss {
		ids = append(ids, ws.ID)
	}
	rows, err := s.client.Query(ctx, `
        select workspace_id, `+memberSelectColumns+`
        from workspace_members
        where workspace_id = any($1)
        order by workspace_id, created_at, id`, ids)
	if err != nil {
		return fmt.Errorf("core: 列空间成员失败: %w", err)
	}
	defer rows.Close()

	byWS := make(map[string][]apimodel.WorkspaceMember, len(wss))
	for rows.Next() {
		var wsID string
		var m apimodel.WorkspaceMember
		var joined time.Time
		if err := rows.Scan(&wsID, &m.ID, &m.Email, &m.Role, &m.Name, &m.AvatarURL,
			&joined); err != nil {
			return fmt.Errorf("core: 读取空间成员失败: %w", err)
		}
		m.JoinedAt = apimodel.FormatISO(joined)
		byWS[wsID] = append(byWS[wsID], m)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("core: 列空间成员失败: %w", err)
	}
	for i := range wss {
		// Non-nil empty slice: the contract requires [], and nil encodes as null while SettingsPage reads .length directly.
		if ms, ok := byWS[wss[i].ID]; ok {
			wss[i].Members = ms
		} else {
			wss[i].Members = []apimodel.WorkspaceMember{}
		}
	}
	return nil
}

// fillTimes writes the two time columns into the ISO string fields of Workspace.
//
// # Why timestamps go through a separate function
//
// created_at / updated_at are `timestamptz` in the database (for database order by), while the contract wants
// ISO strings with exactly three decimal places (see "Timestamps: unit and format" in CLAUDE.md). So unlike name they cannot
// be scanned straight into the struct, and go through apimodel.FormatISO.
//
// **The columns are the only source of truth.** The read path no longer looks at doc (see workspaceSelectColumns), so on entry
// ws.CreatedAt / ws.UpdatedAt are always empty strings, and the two "fill if empty" checks below are really decided by the columns' Valid.
// The checks are kept anyway: they give this function well-defined behavior for "column is NULL" too, instead of writing a fake time like
// `0001-01-01T00:00:00.000Z`; tests can produce NULL.
func fillTimes(ws *apimodel.Workspace, created, updated sql.NullTime) {
	if ws.CreatedAt == "" && created.Valid {
		ws.CreatedAt = apimodel.FormatISO(created.Time)
	}
	if updated.Valid {
		ws.UpdatedAt = apimodel.FormatISO(updated.Time)
	} else if ws.UpdatedAt == "" {
		// The column is empty too (should not happen after the migration); fall back to createdAt = "never changed".
		ws.UpdatedAt = ws.CreatedAt
	}
}

// scanWorkspaceRow builds a Workspace from the **columns**; the doc column no longer takes part.
//
// The column order matches workspaceSelectColumns exactly; change one and you must change the other.
//
// ⚠️ created_by / updated_by / name are **nullable columns** (deliberately no default, see
// schema/workspaces.sql), so they are scanned into sql.NullString first: scanning straight into string on NULL
// reports `converting NULL to string is unsupported`. Both NULL and empty string become an empty string on the Go side.
func scanWorkspaceRow(scan func(dest ...any) error) (apimodel.Workspace, error) {
	var ws apimodel.Workspace
	var name, createdBy, updatedBy sql.NullString
	var created, updated sql.NullTime
	if err := scan(&ws.ID, &ws.Key, &name, &ws.Description,
		&createdBy, &updatedBy, &created, &updated); err != nil {
		return apimodel.Workspace{}, err
	}
	ws.Name = name.String
	ws.CreatedBy = createdBy.String
	ws.UpdatedBy = updatedBy.String
	// CloneWorkspace sets Members to a non-nil empty slice (the contract requires [], nil encodes as null).
	// The real members are then filled in by fillMembers.
	ws = persistmodel.CloneWorkspace(ws)
	fillTimes(&ws, created, updated)
	return ws, nil
}

// workspaceSelectColumns is the column list for reading workspaces. workspaces has no doc column; every field is a real column.
//
// ⚠️ The order matches the scan arguments of scanWorkspaceRow exactly.
const workspaceSelectColumns = `id, key, name, description, created_by, updated_by, created_at, updated_at`

func (s *PGStore) ListWorkspaces(ctx context.Context) ([]apimodel.Workspace, error) {
	// (created_at, id) replaces the old ord; see that migration comment in schema_pg.sql.
	rows, err := s.client.Query(ctx,
		`select `+workspaceSelectColumns+` from workspaces order by created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("core: 列空间失败: %w", err)
	}
	defer rows.Close()

	// Non-nil empty slice: the contract requires [], nil encodes as null.
	wss := make([]apimodel.Workspace, 0)
	for rows.Next() {
		ws, err := scanWorkspaceRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("core: 读取空间失败: %w", err)
		}
		wss = append(wss, ws)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 列空间失败: %w", err)
	}
	if err := s.fillMembers(ctx, wss); err != nil {
		return nil, err
	}
	return wss, nil
}

// getWorkspaceBy fetches a workspace by some column and fills in times and members.
func (s *PGStore) getWorkspaceBy(ctx context.Context, column, value string) (apimodel.Workspace, error) {
	ws, err := scanWorkspaceRow(s.client.QueryRow(ctx,
		`select `+workspaceSelectColumns+` from workspaces where `+column+` = $1`, value).Scan)
	if err != nil {
		if pg.IsNoRows(err) {
			return apimodel.Workspace{}, persistmodel.ErrNotFound
		}
		return apimodel.Workspace{}, fmt.Errorf("core: 查空间失败: %w", err)
	}
	one := []apimodel.Workspace{ws}
	if err := s.fillMembers(ctx, one); err != nil {
		return apimodel.Workspace{}, err
	}
	return one[0], nil
}

func (s *PGStore) GetWorkspace(ctx context.Context, id string) (apimodel.Workspace, error) {
	return s.getWorkspaceBy(ctx, "id", id)
}

func (s *PGStore) GetWorkspaceByKey(ctx context.Context, key string) (apimodel.Workspace, error) {
	return s.getWorkspaceBy(ctx, "key", key)
}

func (s *PGStore) KeyTaken(ctx context.Context, key, exceptID string) (bool, error) {
	// When exceptID is an empty string, `id <> ''` holds for every real ID, so there is no need for two SQLs.
	// This matches MemStore's keyTakenLocked: it also compares ws.ID != exceptID.
	var taken bool
	err := s.client.QueryRow(ctx,
		`select exists(select 1 from workspaces where key = $1 and id <> $2)`,
		key, exceptID).Scan(&taken)
	if err != nil {
		return false, fmt.Errorf("core: 查 key 占用失败: %w", err)
	}
	return taken, nil
}

func (s *PGStore) CreateWorkspace(ctx context.Context, ws apimodel.Workspace) (apimodel.Workspace, error) {
	// Checking once before inserting is for **the error text**: a unique index conflict can only tell us "it clashed",
	// while the contract requires the exact Chinese text (the frontend shows message directly to the user).
	// When the insert below fails it checks 23505 again: that is the fallback under concurrency: when two requests pass this
	// pre-check at the same time, the database still blocks the second.
	taken, err := s.KeyTaken(ctx, ws.Key, "")
	if err != nil {
		return apimodel.Workspace{}, err
	}
	if taken {
		return apimodel.Workspace{}, fmt.Errorf("%w: 空间 Key「%s」已被占用", persistmodel.ErrConflict, ws.Key)
	}

	created := isoToTimeOr(ws.CreatedAt, time.Now().UTC())

	// The workspace and its creator member **must be in one transaction**: as two statements, from the moment the workspace is created until the member is written,
	// the workspace has "no members at all", and authz gives 403 to every non-member,
	// so the creator could not open the workspace they just created. With multiple instances this window really gets hit.
	err = s.client.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`insert into workspaces
			     (id, key, name, description, created_by, updated_by, created_at, updated_at)
			 values ($1, $2, $3, $4, $5, $6, $7, $8)`,
			ws.ID, ws.Key, ws.Name, ws.Description,
			nullIfEmpty(ws.CreatedBy), nullIfEmpty(ws.UpdatedBy),
			created, created); err != nil {
			return err
		}
		return insertMembersTx(ctx, tx, ws.ID, ws.Members)
	})
	if err != nil {
		if pg.IsUniqueViolation(err) {
			return apimodel.Workspace{}, fmt.Errorf("%w: 空间 Key「%s」已被占用", persistmodel.ErrConflict, ws.Key)
		}
		return apimodel.Workspace{}, fmt.Errorf("core: 建空间失败: %w", err)
	}
	return persistmodel.CloneWorkspace(ws), nil
}

// ListAuthzRoles is the **only** query on the authorization path; the contract is on core.Store.
//
// It picks this actor's row directly through the unique index on (workspace_id, email_key).
//
// # ⚠️ Why `exists (select 1 from workspaces ...)` is required
//
// Authorization means "workspace gone = no access". Without this condition, **member rows left after deleting a workspace
// would still let authorization accept them**. DeleteWorkspace deletes both tables in one transaction, so normally nothing is left;
// but authorization is a security boundary, and it must not treat "I did not check whether the workspace still exists" as "the workspace still exists".
// This exists is in **the same SQL** as that select, so it costs no extra round trip.
func (s *PGStore) ListAuthzRoles(
	ctx context.Context, workspaceID, actor string,
) ([]persistmodel.AuthzRole, error) {
	// **Exactly the same function** as the normalization on the write side: the email_key column stores the lowercased, trimmed email,
	// and not passing through it here would miss the row with an uppercase email (showing as "I am clearly a member but it says I'm not").
	who := emailKeyOf(actor)

	rows, err := s.client.Query(ctx, `
        select role
          from workspace_members
         where workspace_id = $1
           and exists (select 1 from workspaces w where w.id = $1)
           and email_key = $2`,
		workspaceID, who)
	if err != nil {
		return nil, fmt.Errorf("core: 查判权角色失败: %w", err)
	}
	defer rows.Close()

	// Non-nil empty slice, same as the other list methods in this file.
	out := make([]persistmodel.AuthzRole, 0, 1)
	for rows.Next() {
		var r persistmodel.AuthzRole
		var role string
		if err := rows.Scan(&role); err != nil {
			return nil, fmt.Errorf("core: 读判权角色失败: %w", err)
		}
		r.Role = apimodel.WorkspaceRole(role)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 查判权角色失败: %w", err)
	}
	return out, nil
}

// emailKeyOf normalizes an email into the form the email_key column should store: **lowercased and trimmed**.
//
// The unique index is on the two bare columns (workspace_id, email_key), so the invariant "one row per person"
// rests on this function: writing an uppercase email without it would make that row **coexist** with the existing lowercase row
// (the index sees them as different), while memberOf with EqualFold hits only one of them, showing as "changing the role
// does not take effect", with zero errors. So every place that writes email_key must go through it, without exception.
func emailKeyOf(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// insertMembersTx writes a batch of members into the join table, used for creating workspaces and loading the seed.
//
// on conflict do nothing: a person has only one row per workspace (the unique index is
// (workspace_id, email_key)). The seed has no duplicates, but the seed-loading path
// has a window between "empty database check" and "actually inserting", and with several instances starting at once two processes may both judge it empty.
func insertMembersTx(
	ctx context.Context, tx *sql.Tx, workspaceID string, members []apimodel.WorkspaceMember,
) error {
	for _, m := range members {
		joined := isoToTimeOr(m.JoinedAt, time.Now().UTC())
		if _, err := tx.ExecContext(ctx, `
            insert into workspace_members
                (id, workspace_id, member_email, role, name, avatar_url,
                 email_key, created_at, updated_at)
            values ($1, $2, $3, $4, $5, $6, $7, $8, $8)
            on conflict do nothing`,
			m.ID, workspaceID, m.Email, string(m.Role), m.Name, m.AvatarURL,
			emailKeyOf(m.Email),
			joined); err != nil {
			return err
		}
	}
	return nil
}

func (s *PGStore) SaveWorkspace(ctx context.Context, ws apimodel.Workspace) (apimodel.Workspace, error) {
	// MemStore's behavior: find this row first, check uniqueness only if key changed (excluding itself), then overwrite.
	// "Do not check when key did not change" is not an optimization: without it, every settings save would clash with itself.
	cur, err := s.GetWorkspace(ctx, ws.ID)
	if err != nil {
		return apimodel.Workspace{}, err // missing means ErrNotFound
	}
	if ws.Key != cur.Key {
		taken, err := s.KeyTaken(ctx, ws.Key, ws.ID)
		if err != nil {
			return apimodel.Workspace{}, err
		}
		if taken {
			return apimodel.Workspace{}, fmt.Errorf("%w: 空间 Key「%s」已被占用", persistmodel.ErrConflict, ws.Key)
		}
	}

	updated := isoToTimeOr(ws.UpdatedAt, time.Now().UTC())

	// Members use a **diff sync**, not "delete all then insert all".
	//
	// Delete-then-insert looks simple, but then every save of workspace settings (say a rename) rewrites all member rows:
	// created_at gets refreshed, so the member list order changes with it, and firstOwnerEmail takes
	// the first owner, which decides whose credential is injected. A diff sync keeps "members not touched" as they were.
	err = s.client.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`update workspaces
			    set key = $2, name = $3, description = $4,
			        created_by = $5, updated_by = $6, updated_at = $7
			  where id = $1`,
			ws.ID, ws.Key, ws.Name, ws.Description,
			nullIfEmpty(ws.CreatedBy), nullIfEmpty(ws.UpdatedBy),
			updated)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// GetWorkspace above just found it; missing here means someone else deleted it in between.
			return persistmodel.ErrNotFound
		}
		return syncMembersTx(ctx, tx, ws.ID, ws.Members, updated)
	})
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			return apimodel.Workspace{}, persistmodel.ErrNotFound
		}
		if pg.IsUniqueViolation(err) {
			return apimodel.Workspace{}, fmt.Errorf("%w: 空间 Key「%s」已被占用", persistmodel.ErrConflict, ws.Key)
		}
		return apimodel.Workspace{}, fmt.Errorf("core: 存空间失败: %w", err)
	}
	return persistmodel.CloneWorkspace(ws), nil
}

// syncMembersTx aligns the join table to the given member list: delete extras, add missing ones, update changed roles.
//
// It compares by **lower(email)**, not member id: if the same person changes id once, comparing by id would become
// "delete one, add one", resetting created_at and changing the list order, while the UpdateMemberRole
// path passes exactly "the same person, a new role". Only comparing by email lines them up.
func syncMembersTx(
	ctx context.Context, tx *sql.Tx, workspaceID string,
	members []apimodel.WorkspaceMember, now time.Time,
) error {
	rows, err := tx.QueryContext(ctx,
		`select email_key, role, name, avatar_url
		   from workspace_members
		  where workspace_id = $1`,
		workspaceID)
	if err != nil {
		return err
	}
	// It stores "what this row looks like now", to decide whether an update is really needed;
	// comparing only role would treat a name or avatar change as "no change" and silently drop it.
	// ⚠️ **All three fields must be stored**: missing one means "changing it does not count as a change", so that change is silently
	// dropped (save succeeds, nothing changes in the UI).
	type memberRow struct{ role, name, avatar string }
	existing := make(map[string]memberRow)
	for rows.Next() {
		var email string
		var cur memberRow
		if err := rows.Scan(&email, &cur.role, &cur.name, &cur.avatar); err != nil {
			rows.Close()
			return err
		}
		existing[email] = cur
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	keep := make(map[string]bool, len(members))
	for _, m := range members {
		key := emailKeyOf(m.Email)
		keep[key] = true
		cur, ok := existing[key]
		switch {
		case !ok:
			joined := isoToTimeOr(m.JoinedAt, now)
			if _, err := tx.ExecContext(ctx, `
                insert into workspace_members
                    (id, workspace_id, member_email, role, name, avatar_url,
                     email_key, created_at, updated_at)
                values ($1, $2, $3, $4, $5, $6, $7, $8, $8)
                on conflict (workspace_id, email_key)
                do update set role = excluded.role, name = excluded.name,
                              avatar_url = excluded.avatar_url,
                              updated_at = excluded.updated_at`,
				m.ID, workspaceID, m.Email, string(m.Role), m.Name, m.AvatarURL,
				key,
				joined); err != nil {
				return err
			}
		case cur.role != string(m.Role) || cur.name != m.Name || cur.avatar != m.AvatarURL:
			// Only update when something really changed; otherwise every save of workspace settings would refresh updated_at,
			// making "when was this member last changed" meaningless.
			//
			// **All three fields must be compared**: comparing only role would treat a name or avatar change as "no change"
			// and silently drop it: save succeeds, yet nothing changes in the UI.
			if _, err := tx.ExecContext(ctx, `
                update workspace_members
                   set role = $3, name = $4, avatar_url = $5, updated_at = $6
                 where workspace_id = $1 and email_key = $2`,
				workspaceID, key, string(m.Role), m.Name, m.AvatarURL,
				now); err != nil {
				return err
			}
		}
	}
	for email := range existing {
		if !keep[email] {
			if _, err := tx.ExecContext(ctx,
				`delete from workspace_members
				  where workspace_id = $1 and email_key = $2`,
				workspaceID, email); err != nil {
				return err
			}
		}
	}
	return nil
}

// DeleteWorkspace deletes a workspace and cascades cleanup: agents, skills, runs, the events of those runs, Ark credentials.
//
// Five tables in one transaction, so a failure halfway does not leave half a workspace.
//
// It deliberately does **not** clean dangling references from agents in other workspaces, copying LocalApiClient's behavior
// (api.ts:270-278 does not clean them either), consistent with MemStore. No "improving" here.
//
// A missing ID is not an error (idempotent): all five DELETEs affect 0 rows and commit normally.
func (s *PGStore) DeleteWorkspace(ctx context.Context, id string) error {
	err := s.client.WithTx(ctx, func(tx *sql.Tx) error {
		// Delete events first: they only know run_id, and once runs are deleted there is no way to find which ones to delete.
		// A subquery instead of SELECT then delete: one less round trip and no intermediate state.
		if _, err := tx.ExecContext(ctx,
			`delete from run_events where run_id in (select id from runs where workspace_id = $1)`,
			id); err != nil {
			return err
		}
		for _, q := range []string{
			`delete from runs where workspace_id = $1`,
			// Members are deleted with the workspace. Otherwise a batch of member rows pointing to a workspace that does not exist stays in the database, and those rows
			// sit on the (workspace_id, email_key) unique index; once someone recreates a workspace with the same id
			// (the seed uses fixed ids), loading members hits 23505 right away.
			`delete from workspace_members where workspace_id = $1`,
			`delete from agents where workspace_id = $1`,
			// Attachments are deleted before skills: they could be fully deleted by workspace_id too, but writing them before skills
			// makes the intermediate state "skill gone, attachments still there" impossible (within one transaction it is not visible anyway;
			// this order saves the next reader one round of reasoning).
			`delete from skill_files where workspace_id = $1`,
			`delete from skills where workspace_id = $1`,
			// The workspace is gone, so its credentials go too: keeping them is dangling data, and a secret that should not be kept.
			`delete from ark_secrets where workspace_id = $1`,
			// Same for agent-level credentials, and **even more so**: agents are hard-deleted with no deleted_at,
			// so these rows get no other chance to be reclaimed. Missing them would leave a batch of
			// secrets pointing to agents that do not exist in the database forever, shown on no page.
			`delete from agent_ark_secrets where workspace_id = $1`,
			// Same for Ark resource bindings: with the workspace gone, those mappings mean nothing.
			// (The resources on the Ark side are not deleted: there is no key here, and deleting a workspace should not call external services.)
			`delete from ark_bindings where workspace_id = $1`,
			// Scheduled tasks are deleted along. Otherwise they are not just dangling data: the scheduler claims them every cycle,
			// really creates an Ark session each time and fails, burning tokens for nothing.
			`delete from schedules where workspace_id = $1`,
			// Ark sync tasks are deleted along, for word-for-word the same reason as scheduled tasks: keeping them, the sync executor would claim them
			// every cycle and then hit Ark again and again for an agent that no longer exists.
			`delete from agent_sync_tasks where workspace_id = $1`,
			// Issues, comments and attachments are deleted with the workspace. All three tables have a workspace_id column
			// (the one on comments and attachments is deliberately redundant), so there is no need to join back to issues.
			//
			// Attachments first: they hold bytes, so missing them costs far more than elsewhere, and it is completely invisible:
			// no page ever shows orphan attachments, the database just keeps growing.
			`delete from issue_attachments where workspace_id = $1`,
			`delete from issue_comments where workspace_id = $1`,
			`delete from issues where workspace_id = $1`,
			`delete from workspaces where id = $1`,
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("core: 删空间失败: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Agent
// ---------------------------------------------------------------------------

// agentSelectColumns is the column list for reading agents: **doc + the 11 columns pulled out so far**.
//
// # Why doc is still here, while workspaces dropped doc when switching reads
//
// That table pulled all 9 keys out into columns, leaving nothing else in doc, so it could stop reading it entirely.
// **agents only pulled out some scalars**: `system` / `model` / `skillIds` / `feishuAppIds` /
// `envs` / `envAliases` / `followUp` / `progressDetail` / `integrationIds`
// all still live only in doc.
// Not reading doc would lose them all, with zero errors (struct fields become zero values, showing on the page as
// "the config was suddenly emptied").
//
// So this round **adds on top** rather than replacing: doc deserializes the full Agent, then the values of these 11 columns
// overwrite it. Only after later rounds pull out the remaining keys can doc be dropped from here.
//
// ⚠️ The order matches the scan arguments of scanAgentRow exactly; change one and you must change the other.
const agentSelectColumns = `doc, name, description, status, version, ` +
	`created_at, updated_at, created_by, updated_by, executor_type, host_id, host_source`

// scanAgentRow first restores the whole Agent from doc, then overwrites the 11 pulled-out fields with the **columns**.
//
// # Why the overwrite direction is "columns over doc" and not the other way around
//
// The columns are the authoritative values after this round. Dual writes keep both sides consistent, so normally the overwrite is an identity;
// what really matters is **whom to trust when they disagree**, and the answer must be the columns, otherwise this step is for nothing
// (after the next round stops writing doc, doc starts going stale, and still reading doc then means reading old values).
//
// # ⚠️ All eleven columns are nullable, and NULL always becomes the Go zero value, consistent with doc deserialization
//
// Scanning straight into string on NULL reports `converting NULL to string is unsupported`,
// and such rows may exist (the column is NULL when doc has no createdBy key at all).
//
// Both NULL and empty string become an empty string on the Go side, word for word the same as "key absent in doc → zero value",
// so the HTTP contract does not change at all. version likewise: NULL becomes 0.
//
// ⚠️ **status is not normalized in any way.** An empty string must be passed down as is: `AgentStatus.IsDisabled`
// explicitly requires an empty string to mean "enabled", and filling in "active" here would merge "never stated" and
// "explicitly enabled" into one value, and differ from the behavior of the doc path.
func scanAgentRow(scan func(dest ...any) error) (apimodel.Agent, error) {
	var blob []byte
	var name, description, status sql.NullString
	var createdAt, updatedAt, createdBy, updatedBy, executorType, hostID, hostSource sql.NullString
	var version sql.NullInt64
	if err := scan(&blob, &name, &description, &status, &version,
		&createdAt, &updatedAt, &createdBy, &updatedBy, &executorType, &hostID, &hostSource); err != nil {
		return apimodel.Agent{}, err
	}

	var a apimodel.Agent
	if err := scanDoc(blob, &a); err != nil {
		return apimodel.Agent{}, err
	}

	a.Name = name.String
	a.Description = description.String
	a.Status = apimodel.AgentStatus(status.String)
	a.Version = int(version.Int64)
	a.CreatedAt = createdAt.String
	a.UpdatedAt = updatedAt.String
	a.CreatedBy = createdBy.String
	a.UpdatedBy = updatedBy.String
	a.ExecutorType = executorType.String
	a.HostID = hostID.String
	// ⚠️ **Deliberately no empty→workspace normalization here**: the PG scan and the mem store are two read entry points,
	// and normalizing in the storage layer would drift into two sets of rules. The meaning of an empty string is interpreted in one place, apimodel.EffectiveHostSource,
	// at the business decision point (existing NULL = assigned by the workspace).
	a.HostSource = hostSource.String

	// clone guarantees slice fields are non-nil (the contract requires [], nil encodes as null).
	return persistmodel.CloneAgent(a), nil
}

func (s *PGStore) ListAgents(ctx context.Context, workspaceID string) ([]apimodel.Agent, error) {
	rows, err := s.client.Query(ctx,
		`select `+agentSelectColumns+` from agents where workspace_id = $1 order by ord`,
		workspaceID)
	if err != nil {
		return nil, fmt.Errorf("core: 列 agent 失败: %w", err)
	}
	defer rows.Close()

	// Non-nil empty slice: the contract requires [], nil encodes as null.
	out := make([]apimodel.Agent, 0)
	for rows.Next() {
		a, err := scanAgentRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("core: 读取 agent 失败: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 列 agent 失败: %w", err)
	}
	return out, nil
}

func (s *PGStore) GetAgent(ctx context.Context, id string) (apimodel.Agent, error) {
	a, err := scanAgentRow(s.client.QueryRow(ctx,
		`select `+agentSelectColumns+` from agents where id = $1`, id).Scan)
	if err != nil {
		if pg.IsNoRows(err) {
			return apimodel.Agent{}, persistmodel.ErrNotFound
		}
		return apimodel.Agent{}, fmt.Errorf("core: 查 agent 失败: %w", err)
	}
	return a, nil
}

// marshalAgentDoc encodes Agent into doc, but **wipes the 8 fields already pulled out into columns**
// (step three of "splitting doc": stop writing).
//
// # Why "wipe 8 keys" instead of "stop writing the column entirely"
//
// The workspaces round stopped writing by setting doc to NULL directly, because all 9 of its keys were pulled out into columns.
// **agents only pulled out 8 scalars**: system / model / skillIds / feishuAppIds / envs /
// envAliases / followUp / progressDetail / integrationIds
// still only live in doc, and the read path still deserializes it (see agentSelectColumns).
// So in this round the doc column **must still be written**, just no longer duplicating those 8 fields that already have columns.
//
// So there is also **no need** for `alter column doc drop not null`: doc is still always non-empty JSON.
// That DDL was a precondition for stopping writes on workspaces; copying it here would loosen a constraint that should stay.
//
// # ⚠️ The benefit and cost of this step
//
// The benefit is removing "the same fact stored twice": during dual writes both the column and doc have name, and the risk of the two drifting
// is always there (say someone changes only one). After stopping writes the column is the **only** source of truth, and drift can no longer happen.
//
// The cost is that **the rollback window closes**: rows written after this version have no such 8 keys in doc,
// and an old binary (a version before switching reads) reading them gets empty names. So the precondition of this version is
// **that the read switch is fully rolled out**.
//
// # Why "set to zero value + omitempty" instead of manipulating a map
//
// Going through a map means Marshal, then Unmarshal, then delete, then Marshal: three rounds of encoding, with key names hard-coded as strings;
// changing a json tag would not error, it would just silently miss the delete (this repo has hit exactly the pitfall of tag changes with zero errors).
// On the zero-value path field names are checked by the compiler, and a rename fails compilation directly.
//
// ⚠️ But **only keys with omitempty really disappear**. On Agent, name / description /
// status / version / createdAt / updatedAt have **no** omitempty, so after zeroing doc still
// keeps empty values like `"name":""`. That is acceptable: they no longer carry information, and the read path does not look at
// them (the columns overwrite afterwards). What really matters is **no longer writing real values**, so the two copies cannot drift.
// createdBy / updatedBy have omitempty and disappear entirely.
func marshalAgentDoc(a apimodel.Agent) ([]byte, error) {
	a.Name = ""
	a.Description = ""
	a.Status = ""
	a.Version = 0
	a.CreatedAt = ""
	a.UpdatedAt = ""
	a.CreatedBy = ""
	a.UpdatedBy = ""
	a.ExecutorType = ""
	a.HostID = ""
	a.HostSource = ""
	return marshalDoc(a)
}

// agentColumnValues spreads an Agent into the 11 column values to be "dual written".
//
// ⚠️ **The order matches the column lists in the SQL of the three write paths exactly** (CreateAgent / SaveAgent /
// insertSeed all use `$4..$14`); changing this means changing those three too. A misaligned order does not fail compilation,
// it just writes the name into the description column, and since the read path still looks at doc in this round, tests cannot catch it.
//
// # These columns were added step by step while "splitting doc"
//
// doc is still the source of truth for complex fields like system/model/skillIds; the scalar columns already take part in reads (scanAgentRow
// columns over doc). The read switch-over is done one step at a time.
//
// # Why created_by / updated_by / the three executor columns go through nullIfEmpty
//
// The first two have omitempty in doc (see apimodel.Agent): when empty the whole key is absent, so the backfill's
// `doc ->> 'createdBy'` gives SQL NULL. The write path must use **the same encoding** as the backfill, otherwise the same
// "unknown who created it" would have two shapes in the database, NULL (backfilled old rows) and empty string (newly written rows),
// and then looking for rows "missing a creator" with `is null` would miss half of them, with zero errors.
//
// executor_type / host_id / host_source are later-added nullable columns (NULL in existing rows); on the doc side
// the two executor keys have omitempty and host_source never goes into doc at all (see marshalAgentDoc),
// so empty values are always written as NULL.
//
// The other 5 columns have no omitempty; the key is always present and an empty value is an empty string, so writing an empty string matches the backfill.
//
// ⚠️ **version is written as an integer directly, with no processing.** It is Ark's optimistic lock (incremented on every update),
// and this column only syncs the current value to the column; no rollback/migration logic may overwrite it.
func agentColumnValues(a apimodel.Agent) []any {
	return []any{
		a.Name, a.Description, string(a.Status), a.Version,
		a.CreatedAt, a.UpdatedAt,
		nullIfEmpty(a.CreatedBy), nullIfEmpty(a.UpdatedBy),
		// executor_type / host_id are nullable columns (NULL in existing rows), and these two doc keys have
		// omitempty: empty values must be written as NULL, the same encoding as existing rows, for the same reason as created_by above.
		nullIfEmpty(a.ExecutorType), nullIfEmpty(a.HostID),
		// host_source is nullable (NULL for existing/Docker container rows); doc never stores this key at all (see
		// marshalAgentDoc), so it has no "same encoding as the backfill" issue here, consistent with the two executor columns.
		nullIfEmpty(a.HostSource),
	}
}

// CreateAgent stores a new agent.
//
// Unlike CreateWorkspace, **there is no uniqueness check**: MemStore's CreateAgent is also a plain
// append, and agents have no unique constraint besides the primary key. A primary key clash is reported as is (not ErrConflict:
// that path does not exist in MemStore at all; the Service generates the ID, and a clash means the ID generator is broken).
func (s *PGStore) CreateAgent(ctx context.Context, a apimodel.Agent) (apimodel.Agent, error) {
	cloned := persistmodel.CloneAgent(a)
	blob, err := marshalAgentDoc(cloned)
	if err != nil {
		return apimodel.Agent{}, err
	}
	args := append([]any{a.ID, a.WorkspaceID, blob}, agentColumnValues(cloned)...)
	if _, err := s.client.Exec(ctx,
		`insert into agents (id, workspace_id, doc,
		                     name, description, status, version,
		                     created_at, updated_at, created_by, updated_by,
		                     executor_type, host_id, host_source)
		 values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		args...); err != nil {
		return apimodel.Agent{}, fmt.Errorf("core: 建 agent 失败: %w", err)
	}
	return cloned, nil
}

func (s *PGStore) SaveAgent(ctx context.Context, a apimodel.Agent) (apimodel.Agent, error) {
	cloned := persistmodel.CloneAgent(a)
	blob, err := marshalAgentDoc(cloned)
	if err != nil {
		return apimodel.Agent{}, err
	}
	// Same as SaveWorkspace: ord is not touched, replaced in place.
	args := append([]any{a.ID, a.WorkspaceID, blob}, agentColumnValues(cloned)...)
	res, err := s.client.Exec(ctx,
		`update agents
		    set workspace_id = $2, doc = $3,
		        name = $4, description = $5, status = $6, version = $7,
		        created_at = $8, updated_at = $9, created_by = $10, updated_by = $11,
		        executor_type = $12, host_id = $13, host_source = $14
		  where id = $1`,
		args...)
	if err != nil {
		return apimodel.Agent{}, fmt.Errorf("core: 存 agent 失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apimodel.Agent{}, persistmodel.ErrNotFound
	}
	return cloned, nil
}

func (s *PGStore) DeleteAgent(ctx context.Context, id string) error {
	if _, err := s.client.Exec(ctx, `delete from agents where id = $1`, id); err != nil {
		return fmt.Errorf("core: 删 agent 失败: %w", err)
	}
	// **Its own Ark key must be deleted too.**
	//
	// Agents are hard-deleted with no deleted_at, so this row gets no other chance to be reclaimed: keeping it leaves a
	// secret pointing to an agent that does not exist, lying in the database forever and shown on no page.
	//
	// More importantly, **crosstalk**: agent ids are generated by an in-process counter (nextID), so deleting and recreating may
	// hit the same id, and then the new agent would **silently inherit** the previous agent's credential:
	// it runs, but with someone else's key, billed to someone else, completely invisible in the UI.
	if err := s.DeleteAgentArkSecret(ctx, id); err != nil {
		return err
	}
	// **Its Ark sync tasks must be deleted too.** Otherwise the sync executor keeps claiming that task,
	// then hits Ark again and again for an agent that no longer exists (syncAgentToArk treats it as
	// "not a failure" and returns right away, so there is not even an error, just a wasted cycle).
	//
	// More importantly, still **crosstalk**: agent ids are generated by an in-process counter, so deleting and recreating may hit the same
	// id, and then the unfinished old task would block the new agent from queueing (partial unique index),
	// showing as "this newly created agent is saved but never synced". Same reason as the key above.
	if _, err := s.client.Exec(ctx,
		`delete from agent_sync_tasks where agent_id = $1`, id); err != nil {
		return fmt.Errorf("core: 删 agent 的方舟同步任务失败: %w", err)
	}
	// Deleting 0 rows is not an error (idempotent): deletes in the frontend are no-op semantics, and double-clicking delete must not pop an error.
	return nil
}

// AgentsBoundTo lists the agent IDs bound to a Feishu app, in **insertion order**.
//
// The order is not optional: TestAgentsBoundTo in store_test.go asserts agent_triage comes before
// agent_analyst. Without order by PostgreSQL may order arbitrarily.
//
// The check uses jsonb containment (@>) instead of decoding doc and looping in Go: it can use a GIN index,
// and avoids deserializing a whole agent to read one field. to_jsonb($1::text) wraps the argument as a JSON
// string, so `["fs_1"] @> "fs_1"` holds.
func (s *PGStore) AgentsBoundTo(ctx context.Context, feishuAppID string) ([]string, error) {
	rows, err := s.client.Query(ctx,
		`select id from agents where doc -> 'feishuAppIds' @> to_jsonb($1::text) order by ord`,
		feishuAppID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("core: 读取绑定关系失败: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 遍历绑定关系失败: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Skills
// ---------------------------------------------------------------------------

func (s *PGStore) ListSkills(ctx context.Context, workspaceID string) ([]apimodel.Skill, error) {
	return queryDocs(ctx, s.client, persistmodel.CloneSkill,
		`select doc from skills where workspace_id = $1 order by ord`, workspaceID)
}

func (s *PGStore) GetSkill(ctx context.Context, id string) (apimodel.Skill, error) {
	return getDoc(ctx, s.client, persistmodel.CloneSkill, `select doc from skills where id = $1`, id)
}

func (s *PGStore) SkillNameTaken(ctx context.Context, workspaceID, name string) (bool, error) {
	var taken bool
	err := s.client.QueryRow(ctx,
		`select exists(select 1 from skills where workspace_id = $1 and name = $2)`,
		workspaceID, name).Scan(&taken)
	if err != nil {
		return false, fmt.Errorf("core: 查技能名占用失败: %w", err)
	}
	return taken, nil
}

// CreateSkill stores a new skill.
//
// **Duplicate names are not checked here**: uniqueness is a Service rule (Service.CreateSkill asks
// SkillNameTaken first), and it only applies on the create side; Service.UpdateSkill deliberately allows renaming into a clash.
// See the comment on skills_name_idx in schema_pg.sql: that index is not UNIQUE, exactly for this reason.
func (s *PGStore) CreateSkill(ctx context.Context, sk apimodel.Skill) (apimodel.Skill, error) {
	cloned := persistmodel.CloneSkill(sk)
	blob, err := marshalDoc(cloned)
	if err != nil {
		return apimodel.Skill{}, err
	}
	if _, err := s.client.Exec(ctx,
		`insert into skills (id, workspace_id, name, doc) values ($1, $2, $3, $4)`,
		sk.ID, sk.WorkspaceID, sk.Name, blob); err != nil {
		return apimodel.Skill{}, fmt.Errorf("core: 建技能失败: %w", err)
	}
	return cloned, nil
}

func (s *PGStore) SaveSkill(ctx context.Context, sk apimodel.Skill) (apimodel.Skill, error) {
	cloned := persistmodel.CloneSkill(sk)
	blob, err := marshalDoc(cloned)
	if err != nil {
		return apimodel.Skill{}, err
	}
	res, err := s.client.Exec(ctx,
		`update skills set workspace_id = $2, name = $3, doc = $4 where id = $1`,
		sk.ID, sk.WorkspaceID, sk.Name, blob)
	if err != nil {
		return apimodel.Skill{}, fmt.Errorf("core: 存技能失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apimodel.Skill{}, persistmodel.ErrNotFound
	}
	return cloned, nil
}

// DeleteSkill deletes a skill and its attachments.
//
// Two DELETEs in one transaction: run separately, a failure in the middle would leave orphan attachments,
// and orphan attachments are never found by any read path (reads always start from the skill, then fetch attachments);
// they just keep taking up space until someone checks by hand.
func (s *PGStore) DeleteSkill(ctx context.Context, id string) error {
	err := s.client.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `delete from skill_files where skill_id = $1`, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `delete from skills where id = $1`, id)
		return err
	})
	if err != nil {
		return fmt.Errorf("core: 删技能失败: %w", err)
	}
	return nil
}

// PutSkillFiles fully replaces a skill's attachments.
//
// Delete then insert, in one transaction. **It cannot be replaced by upsert**: upsert only adds and overwrites,
// so files in the previous version but not in this one would stay in the database and keep being injected into containers: the agent sees a
// file not shown in the UI, without any error.
func (s *PGStore) PutSkillFiles(
	ctx context.Context, skillID, workspaceID string, files []apimodel.SkillFile,
) error {
	err := s.client.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`delete from skill_files where skill_id = $1`, skillID); err != nil {
			return err
		}
		for _, f := range files {
			// Insert one by one instead of building one multi-row VALUES: there are at most 64 attachments (see maxSkillFiles),
			// and the few saved round trips are not worth a piece of easy-to-get-wrong code for building placeholders.
			//
			// When file_id is an empty string write NULL, not an empty string: that column means "is there a matching
			// files row", and an empty string is a third state that is neither NULL nor pointing to any row.
			// The emptiness check matches the two attachment places (TrimSpace).
			var fileID any
			if strings.TrimSpace(f.FileID) != "" {
				fileID = strings.TrimSpace(f.FileID)
			}

			// ⚠️⚠️ **Not a single byte of content is written anymore**: this is the fourth step of the four (4a: Go stops reading and writing).
			//
			// # Why this version can stop writing unconditionally, while step three could not
			//
			// The invariant has not changed; it is still one sentence: **the bytes must be stored in exactly one place, never in neither.**
			// What changed is whether "the other place" still exists:
			//
			//	step three: content is the **fallback**. When file_id is empty (no object storage configured, write failed)
			//	            it is the only copy, so it must be written conditionally.
			//	now:        **file_id being empty is itself already an error**. putSkillFileBlobs now
			//	            returns an error (see core/blob.go), and uploads that cannot get a file_id fail outright,
			//	            never reaching here. So content no longer has any fallback duty.
			//
			// So this is **not** a casual step of "turning conditional into unconditional"; it holds only because
			// the write path side was changed to fail fast. Both must ship in the same version; changing only one side loses bytes:
			//
			//	only this changed (write path still tolerant) → files whose object storage write failed exist nowhere, lost silently
			//	only the write path changed (this still writes content) → nothing lost, but the precondition for 4b dropping the column does not hold
			//
			// # Why the column is not even mentioned anymore
			//
			// 4b does `drop column content`, and dropping a column **is not backward compatible**: if any instance is still
			// inserting it at that moment, you get a batch of 42703 (that incident in schema/README.md, where every
			// workspace-scoped endpoint returned 500 together). So 4a's job is to make "no instance touches this column"
			// a fact, and only then can 4b drop it safely. The column name disappearing from this SQL is **the whole content** of that.
			if _, err := tx.ExecContext(ctx,
				`insert into skill_files (skill_id, workspace_id, path, file_id)
				 values ($1, $2, $3, $4)`,
				skillID, workspaceID, f.Path, fileID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("core: 存技能附件失败: %w", err)
	}
	return nil
}

// SkillFiles gets all attachments of a skill, **without content bytes**, by path ascending.
//
// The order matches MemStore (which sorts at PutSkillFiles time): without order by,
// PostgreSQL may change the order after any page split, while injection logs and retries must be reproducible.
//
// # ⚠️ Content in the return value is always nil and Size always 0; this is not a bug
//
// After step four (4a) this **no longer selects content**, so this layer cannot get the bytes or compute the size.
// Filling in bytes and Size is the job of `core.Service.hydrateSkillFiles` (it holds both
// the files table metadata and the bytes in object storage, the only place that can fill them in).
//
// ⚠️ **So do not hand this method's return value directly to users or to container injection.** A call site that bypasses hydrate
// hands out a batch of zero-byte files, and the container injection path **degrades silently**: the agent reads a
// skill with a complete directory and all-empty content, with zero errors. The three call sites in `core/agent.go` are each followed by
// a hydrate; keep that shape when changing them.
func (s *PGStore) SkillFiles(ctx context.Context, skillID string) ([]apimodel.SkillFile, error) {
	// ⚠️ **content is no longer in this SQL** (4a stopped reading). This is the precondition for 4b's `drop column`:
	// dropping a column is not backward compatible, and if any instance still selects it at that moment you get a batch of 42703
	// (see that incident in schema/README.md). So "no instance touches this column"
	// must become a fact first, and this line is half of that (the other half is the insert in PutSkillFiles above).
	rows, err := s.client.Query(ctx,
		`select path, coalesce(file_id, '')
		   from skill_files where skill_id = $1 order by path`, skillID)
	if err != nil {
		return nil, fmt.Errorf("core: 查技能附件失败: %w", err)
	}
	defer rows.Close()

	out := make([]apimodel.SkillFile, 0)
	for rows.Next() {
		var f apimodel.SkillFile
		if err := rows.Scan(&f.Path, &f.FileID); err != nil {
			return nil, fmt.Errorf("core: 读取技能附件失败: %w", err)
		}
		// Both Content and Size stay zero values, filled in by hydrateSkillFiles from the files table and object storage.
		//
		// ⚠️ This used to say `f.Size = len(f.Content)`, reasoning that "Size is a function of the content length,
		// and storing it twice risks a mismatch". That reasoning was right, but **it assumed this layer could get the content**;
		// now it cannot, and computing it anyway only gives a fake value that is always 0, while Size flows to the UI
		// ("how many files and how big is this skill" in ListSkills). Better leave the zero value for hydrate to fill
		// than make up a 0 here that looks like a real value.
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 遍历技能附件失败: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Ark credentials
// ---------------------------------------------------------------------------

// GetArkSecret gets the credential with the plaintext key; returns ErrNotFound if not configured.
//
// Note that MemStore has **no** clone here: ArkSecret has no slice or pointer fields, so a value copy is enough.
// This implementation decodes a new object from JSON every time, which is also an independent copy.
func (s *PGStore) GetArkSecret(ctx context.Context, workspaceID string) (apimodel.ArkSecret, error) {
	sec, err := getDoc(ctx, s.client,
		func(v apimodel.ArkSecret) apimodel.ArkSecret { return v },
		`select doc from ark_secrets where workspace_id = $1`, workspaceID)
	if err != nil {
		return apimodel.ArkSecret{}, err
	}
	// The key in doc may be ciphertext (v1: prefix) or existing plaintext; openKey recognizes the prefix.
	// In inherit rows APIKey is an empty string, and Open returns it as is.
	plain, err := s.openKey(sec.APIKey)
	if err != nil {
		return apimodel.ArkSecret{}, fmt.Errorf("core: 解密方舟凭证失败: %w", err)
	}
	sec.APIKey = plain
	return sec, nil
}

// PutArkSecret overwrites a credential.
//
// MemStore assigns into a map: overwrite if present, add if not, and **there is no "does not exist" error**.
// In SQL that is an upsert.
//
// doc contains the plaintext API key. apimodel.ArkSecret embeds ArkCredential, and the APIKey field has no json
// tag, so it is stored under the key "APIKey"; this only affects the in-database representation, not any response
// (the external view of the credential is ArkCredential, assembled by the Service).
func (s *PGStore) PutArkSecret(ctx context.Context, sec apimodel.ArkSecret) error {
	// **Encrypt before marshal**: ciphertext goes into doc, rather than writing plaintext and then figuring something out.
	// Note this changes a copy of the argument (sec is passed by value), so the caller's plaintext copy is unaffected;
	// the Service uses it to call Ark, and would get 401 with ciphertext.
	sealed, err := s.sealKey(sec.APIKey)
	if err != nil {
		return fmt.Errorf("core: 加密方舟凭证失败: %w", err)
	}
	sec.APIKey = sealed
	blob, err := marshalDoc(sec)
	if err != nil {
		return err
	}
	if _, err := s.client.Exec(ctx,
		`insert into ark_secrets (workspace_id, doc) values ($1, $2)
		 on conflict (workspace_id) do update set doc = excluded.doc`,
		sec.WorkspaceID, blob); err != nil {
		return fmt.Errorf("core: 存方舟凭证失败: %w", err)
	}
	return nil
}

func (s *PGStore) DeleteArkSecret(ctx context.Context, workspaceID string) error {
	if _, err := s.client.Exec(ctx,
		`delete from ark_secrets where workspace_id = $1`, workspaceID); err != nil {
		return fmt.Errorf("core: 删方舟凭证失败: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Onboarding (account level)
// ---------------------------------------------------------------------------

// GetOnboarding gets a person's onboarding state; returns ErrNotFound if there is no record.
//
// Columns are scanned by hand instead of going through getDoc: this table has no doc jsonb, and all five scalars are real columns
// (see schema_onboarding_pg.sql for why).
func (s *PGStore) GetOnboarding(ctx context.Context, email string) (persistmodel.OnboardingRecord, error) {
	var completedAt, skippedAt, updatedAt sql.NullTime
	var version int
	err := s.client.QueryRow(ctx,
		`select completed_at, skipped_at, version, updated_at
		   from user_onboarding where email = $1`, email).
		Scan(&completedAt, &skippedAt, &version, &updatedAt)
	if err != nil {
		// Use pg.IsNoRows instead of errors.Is(err, sql.ErrNoRows): consistent with the other read methods in this file
		// (getDoc uses it too), and nothing here needs to change if the driver changes.
		if pg.IsNoRows(err) {
			return persistmodel.OnboardingRecord{}, persistmodel.ErrNotFound
		}
		return persistmodel.OnboardingRecord{}, fmt.Errorf("core: 查新手引导状态失败: %w", err)
	}
	return persistmodel.OnboardingRecord{
		Email:       email,
		CompletedAt: completedAt.Time,
		SkippedAt:   skippedAt.Time,
		Version:     version,
		UpdatedAt:   updatedAt.Time,
	}, nil
}

// PutOnboarding records onboarding state: merge if present, add if not.
//
// # Why upsert instead of "read then write"
//
// Production has multiple instances. The same person clicking twice quickly (or one click being retried) may send two requests to two processes;
// with "select whether it exists, insert if not" both would see "not there" and both insert,
// and the slower one hits the primary key unique constraint and returns 500, while the user did nothing wrong, and this failure **never reproduces with a single instance**.
// Letting the database decide leaves no gap: the unique constraint is global, whoever comes first inserts, and the later one goes to update.
//
// # Why the two timestamps use coalesce
//
// `coalesce(excluded.x, user_onboarding.x)` = "use the input if given; if the input is NULL,
// keep the old value". That way after "skipped first, then watched it later on their own" both times are in the database.
// A plain `set completed_at = excluded.completed_at` would wipe the other column to NULL,
// losing the fact "they once skipped", which is the whole reason skipped_at exists on its own.
//
// The in-memory implementation has an equivalent explicit zero-value check; the two must match exactly (see MemStore.PutOnboarding).
func (s *PGStore) PutOnboarding(ctx context.Context, rec persistmodel.OnboardingRecord) error {
	if _, err := s.client.Exec(ctx,
		`insert into user_onboarding (email, completed_at, skipped_at, version, updated_at)
		 values ($1, $2, $3, $4, $5)
		 on conflict (email) do update
		   set completed_at = coalesce(excluded.completed_at, user_onboarding.completed_at),
		       skipped_at   = coalesce(excluded.skipped_at,   user_onboarding.skipped_at),
		       version      = excluded.version,
		       updated_at   = excluded.updated_at`,
		rec.Email, nullTime(rec.CompletedAt), nullTime(rec.SkippedAt),
		rec.Version, rec.UpdatedAt); err != nil {
		return fmt.Errorf("core: 存新手引导状态失败: %w", err)
	}
	return nil
}

// -- Account-level Ark credentials -----------------------------------------------------------

// GetGlobalArkSecret gets a person's account-level credential; returns ErrNotFound if never configured.
//
// Columns are scanned by hand instead of going through getDoc: this table has no doc jsonb, and all fields are real columns
// (see schema_globalark_pg.sql for why).
func (s *PGStore) GetGlobalArkSecret(ctx context.Context, email string) (apimodel.GlobalArkSecret, error) {
	var sec apimodel.GlobalArkSecret
	var region, check string
	err := s.client.QueryRow(ctx,
		`select api_key, api_key_masked, region, check_status, checked_at, check_error, updated_at
		   from global_ark_secrets where email = $1`, email).
		Scan(&sec.APIKey, &sec.ApiKeyMasked, &region, &check,
			&sec.CheckedAt, &sec.CheckError, &sec.UpdatedAt)
	if err != nil {
		if pg.IsNoRows(err) {
			return apimodel.GlobalArkSecret{}, persistmodel.ErrNotFound
		}
		return apimodel.GlobalArkSecret{}, fmt.Errorf("core: 查账号级方舟凭证失败: %w", err)
	}
	sec.Email = email
	sec.Region = apimodel.ArkRegion(region)
	sec.Check = apimodel.ArkCredentialCheck(check)
	// The database may hold ciphertext (v1: prefix) or existing plaintext; openKey recognizes the prefix.
	plain, err := s.openKey(sec.APIKey)
	if err != nil {
		return apimodel.GlobalArkSecret{}, fmt.Errorf("core: 解密账号级方舟凭证失败: %w", err)
	}
	sec.APIKey = plain
	return sec, nil
}

// PutGlobalArkSecret stores/overwrites a person's account-level credential.
//
// One upsert, **not read-then-write**: across instances read-then-write is TOCTOU (see PutOnboarding).
// Overwrites the whole row, no coalesce: the caller (Service) always gives a complete copy.
func (s *PGStore) PutGlobalArkSecret(ctx context.Context, sec apimodel.GlobalArkSecret) error {
	sealed, err := s.sealKey(sec.APIKey)
	if err != nil {
		return fmt.Errorf("core: 加密账号级方舟凭证失败: %w", err)
	}
	if _, err := s.client.Exec(ctx,
		`insert into global_ark_secrets
		   (email, api_key, api_key_masked, region, check_status, checked_at, check_error, updated_at)
		 values ($1, $2, $3, $4, $5, $6, $7, $8)
		 on conflict (email) do update
		   set api_key        = excluded.api_key,
		       api_key_masked = excluded.api_key_masked,
		       region         = excluded.region,
		       check_status   = excluded.check_status,
		       checked_at     = excluded.checked_at,
		       check_error    = excluded.check_error,
		       updated_at     = excluded.updated_at`,
		sec.Email, sealed, sec.ApiKeyMasked, string(sec.Region),
		string(sec.Check), sec.CheckedAt, sec.CheckError, sec.UpdatedAt); err != nil {
		return fmt.Errorf("core: 存账号级方舟凭证失败: %w", err)
	}
	return nil
}

// DeleteGlobalArkSecret deletes the account-level credential; missing is not an error (idempotent).
func (s *PGStore) DeleteGlobalArkSecret(ctx context.Context, email string) error {
	if _, err := s.client.Exec(ctx,
		`delete from global_ark_secrets where email = $1`, email); err != nil {
		return fmt.Errorf("core: 删账号级方舟凭证失败: %w", err)
	}
	return nil
}

// -- Agent-level Ark credentials -------------------------------------------------------

func (s *PGStore) GetAgentArkSecret(
	ctx context.Context, agentID string,
) (apimodel.AgentArkSecret, error) {
	var sec apimodel.AgentArkSecret
	var region, check string
	err := s.client.QueryRow(ctx,
		`select workspace_id, api_key, api_key_masked, region, check_status,
		        checked_at, check_error, updated_at, updated_by
		   from agent_ark_secrets where agent_id = $1`, agentID).
		Scan(&sec.WorkspaceID, &sec.APIKey, &sec.ApiKeyMasked, &region, &check,
			&sec.CheckedAt, &sec.CheckError, &sec.UpdatedAt, &sec.UpdatedBy)
	if err != nil {
		if pg.IsNoRows(err) {
			return apimodel.AgentArkSecret{}, persistmodel.ErrNotFound
		}
		return apimodel.AgentArkSecret{}, fmt.Errorf("core: 查 agent 级方舟凭证失败: %w", err)
	}
	sec.AgentID = agentID
	sec.Region = apimodel.ArkRegion(region)
	sec.Check = apimodel.ArkCredentialCheck(check)
	// The database may hold ciphertext (v1: prefix) or existing plaintext; openKey recognizes the prefix.
	plain, err := s.openKey(sec.APIKey)
	if err != nil {
		return apimodel.AgentArkSecret{}, fmt.Errorf("core: 解密 agent 级方舟凭证失败: %w", err)
	}
	sec.APIKey = plain
	return sec, nil
}

// PutAgentArkSecret stores/overwrites an agent's credential.
//
// One upsert, **not read-then-write**: across instances read-then-write is TOCTOU (see PutOnboarding).
// Overwrites the whole row, no coalesce: the caller (Service) always gives a complete copy.
func (s *PGStore) PutAgentArkSecret(ctx context.Context, sec apimodel.AgentArkSecret) error {
	sealed, err := s.sealKey(sec.APIKey)
	if err != nil {
		return fmt.Errorf("core: 加密 agent 级方舟凭证失败: %w", err)
	}
	if _, err := s.client.Exec(ctx,
		`insert into agent_ark_secrets
		   (agent_id, workspace_id, api_key, api_key_masked, region,
		    check_status, checked_at, check_error, updated_at, updated_by)
		 values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 on conflict (agent_id) do update
		   set workspace_id   = excluded.workspace_id,
		       api_key        = excluded.api_key,
		       api_key_masked = excluded.api_key_masked,
		       region         = excluded.region,
		       check_status   = excluded.check_status,
		       checked_at     = excluded.checked_at,
		       check_error    = excluded.check_error,
		       updated_at     = excluded.updated_at,
		       updated_by     = excluded.updated_by`,
		sec.AgentID, sec.WorkspaceID, sealed, sec.ApiKeyMasked, string(sec.Region),
		string(sec.Check), sec.CheckedAt, sec.CheckError, sec.UpdatedAt, sec.UpdatedBy,
	); err != nil {
		return fmt.Errorf("core: 存 agent 级方舟凭证失败: %w", err)
	}
	return nil
}

// DeleteAgentArkSecret deletes the agent-level credential; missing is not an error (idempotent).
func (s *PGStore) DeleteAgentArkSecret(ctx context.Context, agentID string) error {
	if _, err := s.client.Exec(ctx,
		`delete from agent_ark_secrets where agent_id = $1`, agentID); err != nil {
		return fmt.Errorf("core: 删 agent 级方舟凭证失败: %w", err)
	}
	return nil
}

// -- User preferences ---------------------------------------------------------------

// GetUserPreferences gets a person's language and appearance; returns ErrNotFound if there is no record.
//
// Columns are scanned by hand instead of going through getDoc: this table has no doc jsonb, and all three scalars are real columns
// (see schema_prefs_pg.sql for why).
func (s *PGStore) GetUserPreferences(ctx context.Context, email string) (persistmodel.UserPreferencesRecord, error) {
	var language, appearance string
	var updatedAt time.Time
	err := s.client.QueryRow(ctx,
		`select language, appearance, updated_at
		   from user_preferences where email = $1`, email).
		Scan(&language, &appearance, &updatedAt)
	if err != nil {
		if pg.IsNoRows(err) {
			return persistmodel.UserPreferencesRecord{}, persistmodel.ErrNotFound
		}
		return persistmodel.UserPreferencesRecord{}, fmt.Errorf("core: 查用户偏好失败: %w", err)
	}
	return persistmodel.UserPreferencesRecord{
		Email:      email,
		Language:   language,
		Appearance: appearance,
		UpdatedAt:  updatedAt,
	}, nil
}

// PutUserPreferences stores a person's preferences: overwrite the whole row if present, add if not.
//
// # Why upsert instead of "read then write"
//
// Same as PutOnboarding: production has multiple instances, and the same person changing it once in each of two tabs may send two requests to
// two processes. With "select whether it exists, insert if not" both would see "not there", then both
// insert, and the slower one hits the primary key and returns 500, while the user did nothing wrong, and this failure **never
// reproduces with a single instance**. Letting the database decide leaves no gap.
//
// # Why no coalesce here (the opposite of PutOnboarding)
//
// There the two columns each record a separate event that happened (skipped / watched), so "keep the old value when the input is missing" is needed.
// Here both fields are always submitted together: the frontend takes one complete preferences object. Making it a merge would
// **lose "changing from dark back to follow system"**: system is the default value, and under the "no new value provided" rule
// it looks exactly like "unchanged", so the user's click does nothing, with no error.
//
// The in-memory implementation also overwrites the whole row; the two must match (see MemStore.PutUserPreferences).
func (s *PGStore) PutUserPreferences(ctx context.Context, rec persistmodel.UserPreferencesRecord) error {
	if _, err := s.client.Exec(ctx,
		`insert into user_preferences (email, language, appearance, updated_at)
		 values ($1, $2, $3, $4)
		 on conflict (email) do update
		   set language   = excluded.language,
		       appearance = excluded.appearance,
		       updated_at = excluded.updated_at`,
		rec.Email, rec.Language, rec.Appearance, rec.UpdatedAt); err != nil {
		return fmt.Errorf("core: 存用户偏好失败: %w", err)
	}
	return nil
}

// nullTime turns a zero time.Time into SQL NULL.
//
// **It must convert**: a zero time.Time is year 1 AD, and putting it straight into timestamptz stores a real instant,
// so coalesce thinks "this column has a value" and the merge behavior breaks entirely; and a non-zero CompletedAt
// makes the frontend think this person has finished onboarding, so it never shows even once.
func nullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}

// ---------------------------------------------------------------------------
// Issue feedback (across workspaces)
// ---------------------------------------------------------------------------

// PutFeedback stores one piece of feedback; level one is deduped by dedup_key.
//
// # Rate limiting lives in this SQL
//
// `on conflict (dedup_key) do update set occurrences = ... + 1` is the whole implementation of rate limiting,
// **decided by the database**. This is "unique constraint + ON CONFLICT" from that table in CLAUDE.md:
// under concurrency across instances, two processes compute the same dedup_key for the same error; the first inserts,
// the later one goes to update automatically. "Check whether it exists, insert if not" in Go is TOCTOU:
// both would see "not yet" and both insert, and the slower one hits the unique constraint and returns 500,
// **and that happens exactly during an incident, when concurrency is highest and this path most needs to be reliable.**
//
// # ⚠️ Only two columns change on conflict
//
// `occurrences` is incremented and `last_seen_at` advanced; **not a single other column changes**. Keeping the first one is deliberate:
// the scene of the first occurrence is more valuable than the 300th (it often still carries the context that triggered it).
// Updating payload along the way would lose "what the earliest one looked like" forever.
//
// # Why level two needs no branch
//
// A user-level DedupKey is an empty string → written here as SQL NULL → **NULLs are never equal to each other in a unique index**
// (PG semantics), so every one can be inserted, and `on conflict` never fires.
// No second SQL is needed for it, and no if.
//
// # Why log_id is not among the updated columns either
//
// Only level one can reach `on conflict` (level two's dedup_key is NULL and never conflicts),
// and level one **does collect logid**, so this column really has a value on auto rows, and the choice has real consequences:
// that row keeps the logid of the **first** occurrence, and the next 299 are dropped.
//
// That is right, the same direction as keeping the first payload / created_at: the scene of the first occurrence is more valuable than the 300th
// (it often still carries the context that triggered it), and occurrences already says "how many times it happened".
// The other way, "overwrite with the latest every time", would keep changing the one logid that can be searched,
// pointing at the least informative occurrence.
func (s *PGStore) PutFeedback(ctx context.Context, rec persistmodel.FeedbackRecord) error {
	blob, err := marshalDoc(rec.Payload)
	if err != nil {
		return err
	}
	if _, err := s.client.Exec(ctx,
		`insert into feedback_reports
		   (id, level, actor, workspace_id, route_pattern, http_status, error_code,
		    error_kind, payload, user_agent, created_at, last_seen_at, occurrences,
		    dedup_key, log_id)
		 values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		 on conflict (dedup_key) do update
		   set occurrences  = feedback_reports.occurrences + 1,
		       last_seen_at = excluded.last_seen_at`,
		rec.ID, string(rec.Level), rec.Actor, nullText(rec.WorkspaceID), rec.RoutePattern,
		rec.HTTPStatus, rec.ErrorCode, rec.ErrorKind, blob, rec.UserAgent,
		rec.CreatedAt, rec.LastSeenAt, rec.Occurrences, nullText(rec.DedupKey),
		// An empty string is written as NULL. **Both levels may be empty**, for only one reason: failures like network_error
		// never reached the server, so no logid was generated. It should be NULL, not an empty string.
		nullText(rec.LogID)); err != nil {
		return fmt.Errorf("core: 存问题反馈失败: %w", err)
	}
	return nil
}

// ListFeedback lists feedback by conditions, returning this page and the total matching the conditions.
//
// # The total and this page use two queries, not a window function
//
// `count(*) over ()` gets both at once, but it requires computing every row to fill that window column,
// so limit saves no work. Split in two, count uses the index and the page query uses limit,
// and each can benefit from indexes. This table grows with error volume, so the difference keeps getting more visible.
//
// # The time window compares fixed-length ISO strings, not converted to timestamptz
//
// ⚠️ This is the opposite of the runs table, **because the storage type differs**: runs.started_at is text,
// while created_at here is timestamptz. So the time window must parse the ISO string into time.Time first
// before passing it as a parameter; comparing a string directly with a timestamptz column makes PG do an implicit conversion,
// which both cannot use feedback_reports_created_idx and may give surprising results with time zones.
func (s *PGStore) ListFeedback(ctx context.Context, q persistmodel.FeedbackQuery) ([]persistmodel.FeedbackRecord, int, error) {
	// The where is built dynamically: all conditions are optional. Use $n placeholders instead of splicing values into the SQL:
	// those values (error_code) come from HTTP query parameters, and string splicing would be SQL injection.
	var conds []string
	var args []any
	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}
	if q.Level != "" {
		add("level = $%d", string(q.Level))
	}
	if q.ErrorCode != "" {
		add("error_code = $%d", q.ErrorCode)
	}
	// Half-open [Since, Until): a closed interval would put the boundary row into two adjacent pages,
	// so a script paging by time would process the same row twice.
	if q.Since != "" {
		t, err := apimodel.ParseISO(q.Since)
		if err != nil {
			return nil, 0, fmt.Errorf("%w: since 不是合法的时间戳", persistmodel.ErrInvalidArgument)
		}
		add("created_at >= $%d", t)
	}
	if q.Until != "" {
		t, err := apimodel.ParseISO(q.Until)
		if err != nil {
			return nil, 0, fmt.Errorf("%w: until 不是合法的时间戳", persistmodel.ErrInvalidArgument)
		}
		add("created_at < $%d", t)
	}
	where := ""
	if len(conds) > 0 {
		where = " where " + strings.Join(conds, " and ")
	}

	var total int
	if err := s.client.QueryRow(ctx, `select count(*) from feedback_reports`+where, args...).
		Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("core: 统计问题反馈失败: %w", err)
	}
	// When the total is 0 return right away, saving a query. **Still return a non-nil empty slice**:
	// list endpoints never return null (see the core/handler.go file header).
	if total == 0 {
		return []persistmodel.FeedbackRecord{}, 0, nil
	}

	// order by matches MemStore exactly: created_at desc, id desc.
	// **The id tie-breaker cannot be dropped**: when several records at the same instant have an unstable order, paging repeats or misses records,
	// and that error never shows with small data (it only appears once a full page accumulates).
	pageArgs := append(append([]any{}, args...), q.Limit, q.Offset)
	rows, err := s.client.Query(ctx,
		`select id, level, actor, workspace_id, route_pattern, http_status, error_code,
		        error_kind, payload, user_agent, created_at, last_seen_at, occurrences,
		        log_id
		   from feedback_reports`+where+
			fmt.Sprintf(` order by created_at desc, id desc limit $%d offset $%d`,
				len(args)+1, len(args)+2),
		pageArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("core: 查问题反馈失败: %w", err)
	}
	defer rows.Close()

	out := make([]persistmodel.FeedbackRecord, 0, q.Limit)
	for rows.Next() {
		var rec persistmodel.FeedbackRecord
		// log_id, like workspace_id, is a nullable column, so it is read with NullString.
		// **Scanning straight into string errors on NULL**, and NULL is the norm, not an exception:
		// both levels may be NULL: failed requests like network_error never reached the server.
		var level, wsID, logID sql.NullString
		var blob []byte
		if err := rows.Scan(&rec.ID, &level, &rec.Actor, &wsID, &rec.RoutePattern,
			&rec.HTTPStatus, &rec.ErrorCode, &rec.ErrorKind, &blob, &rec.UserAgent,
			&rec.CreatedAt, &rec.LastSeenAt, &rec.Occurrences, &logID); err != nil {
			return nil, 0, fmt.Errorf("core: 读问题反馈失败: %w", err)
		}
		rec.Level = persistmodel.FeedbackLevel(level.String)
		rec.WorkspaceID = wsID.String
		rec.LogID = logID.String
		// A payload that cannot be decoded **does not make this record broken**: the other fields (route, status code, error code)
		// are already enough to diagnose, and losing the whole record over one jsonb parse failure is too costly. Leave an empty map and go on.
		if len(blob) > 0 {
			payload := map[string]any{}
			if err := scanDoc(blob, &payload); err == nil {
				rec.Payload = payload
			}
		}
		if rec.Payload == nil {
			rec.Payload = map[string]any{}
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("core: 读问题反馈失败: %w", err)
	}
	return out, total, nil
}

// nullText turns an empty string into SQL NULL.
//
// Two places need it, for different reasons:
//
//   - workspace_id: an empty string would make "errors outside any workspace" look like a workspace whose id is an empty string in the database,
//     and `where workspace_id is null` would not find them.
//   - dedup_key: **this one is functional**. An empty string is a concrete value, and the unique index would consider "the second empty string
//     a duplicate of the first", so of all feedback users submit themselves only the first could be stored,
//     and all later ones would be eaten by on conflict and turned into +1 on the first. Only NULLs are never equal.
//     This is the easiest point in this implementation to get wrong, and **getting it wrong gives no error at all**.
func nullText(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// ---------------------------------------------------------------------------
// Ark resource bindings
// ---------------------------------------------------------------------------

// GetArkBinding gets the Ark counterpart of an AMA agent; returns ErrNotFound if none.
//
// See schema_pg.sql for why this table exists: Ark Environment names are globally unique,
// and the mapping must survive process restarts, otherwise creating again with the same name after a restart gives 409.
func (s *PGStore) GetArkBinding(ctx context.Context, workspaceID, agentID string) (persistmodel.ArkBinding, error) {
	return getDoc(ctx, s.client,
		func(v persistmodel.ArkBinding) persistmodel.ArkBinding { return v },
		`select doc from ark_bindings where workspace_id = $1 and agent_id = $2`,
		workspaceID, agentID)
}

// PutArkBinding overwrites.
//
// MemStore assigns into a map: overwrite if present, add if not, with no "does not exist" error.
// In SQL that is an upsert. ord is not updated: consistent with the other Save*, replaced in place without changing order.
func (s *PGStore) PutArkBinding(ctx context.Context, b persistmodel.ArkBinding) error {
	blob, err := marshalDoc(b)
	if err != nil {
		return err
	}
	if _, err := s.client.Exec(ctx,
		`insert into ark_bindings (workspace_id, agent_id, ark_agent_id, ark_environment_id, doc)
		 values ($1, $2, $3, $4, $5)
		 on conflict (workspace_id, agent_id) do update
		   set ark_agent_id = excluded.ark_agent_id,
		       ark_environment_id = excluded.ark_environment_id,
		       doc = excluded.doc`,
		b.WorkspaceID, b.AgentID, b.ArkAgentID, b.ArkEnvironmentID, blob); err != nil {
		return fmt.Errorf("core: 存方舟资源绑定失败: %w", err)
	}
	return nil
}

func (s *PGStore) ListArkBindings(ctx context.Context, workspaceID string) ([]persistmodel.ArkBinding, error) {
	return queryDocs(ctx, s.client,
		func(v persistmodel.ArkBinding) persistmodel.ArkBinding { return v },
		`select doc from ark_bindings where workspace_id = $1 order by ord`, workspaceID)
}

func (s *PGStore) DeleteArkBinding(ctx context.Context, workspaceID, agentID string) error {
	if _, err := s.client.Exec(ctx,
		`delete from ark_bindings where workspace_id = $1 and agent_id = $2`,
		workspaceID, agentID); err != nil {
		return fmt.Errorf("core: 删方舟资源绑定失败: %w", err)
	}
	// Deleting 0 rows is not an error (idempotent), consistent with the other Delete*.
	return nil
}

// RepointArkEnvironment repoints bindings in the same workspace still pointing at oldEnvID to newEnvID.
//
// The column and doc change together (doc is the authoritative data; ark_environment_id is just a column pulled out for querying,
// see the ark_bindings.sql file header), and jsonb_set touches only one key, **without reading and rewriting the whole row**:
// sibling agents catching up skills/prompts also write this table, and read-modify-write would overwrite the fingerprint they just wrote.
// ark_environment_id=$2 in the where is the CAS guard: rows already moved by someone else are left alone.
func (s *PGStore) RepointArkEnvironment(
	ctx context.Context, workspaceID, oldEnvID, newEnvID string,
) (int, error) {
	tag, err := s.client.Exec(ctx,
		`update ark_bindings
		    set ark_environment_id = $3,
		        doc = jsonb_set(doc, '{arkEnvironmentId}', to_jsonb($3::text))
		  where workspace_id = $1 and ark_environment_id = $2`,
		workspaceID, oldEnvID, newEnvID)
	if err != nil {
		return 0, fmt.Errorf("core: 批量改指方舟环境失败: %w", err)
	}
	n, err := tag.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("core: 取批量改指行数失败: %w", err)
	}
	return int(n), nil
}

// ---------------------------------------------------------------------------
// Ark skill upload records
// ---------------------------------------------------------------------------

func (s *PGStore) GetArkSkillUpload(
	ctx context.Context, workspaceID, skillID, contentHash, keyFingerprint string,
) (persistmodel.ArkSkillUpload, error) {
	u := persistmodel.ArkSkillUpload{
		WorkspaceID: workspaceID, SkillID: skillID,
		ContentHash: contentHash, KeyFingerprint: keyFingerprint,
	}
	err := s.client.QueryRow(ctx,
		`select ark_skill_id, ark_version, created_at from ark_skill_uploads
		 where workspace_id = $1 and skill_id = $2 and content_hash = $3
		   and key_fingerprint = $4`,
		workspaceID, skillID, contentHash, keyFingerprint).
		Scan(&u.ArkSkillID, &u.ArkVersion, &u.CreatedAt)
	if err != nil {
		// Use pg.IsNoRows instead of errors.Is(err, sql.ErrNoRows): consistent with the other read methods in this file
		// (getDoc uses it too), and nothing here needs to change if the driver changes.
		if pg.IsNoRows(err) {
			return persistmodel.ArkSkillUpload{}, persistmodel.ErrNotFound
		}
		return persistmodel.ArkSkillUpload{}, fmt.Errorf("core: 查方舟技能上传记录失败: %w", err)
	}
	return u, nil
}

// PutArkSkillUpload records an upload result and returns the one that finally takes effect.
//
// # Why on conflict do nothing + read back, not upsert
//
// Under concurrency two instances may both find "this version was never uploaded", so each uploads a copy to Ark
// and gets a different ark_skill_id. Then **first come, first served**: whoever is stored first counts,
// and the losing copy becomes an orphan on the Ark side (it cannot be cleaned; Ark has no delete endpoint, and this leak is accepted on purpose,
// see the comment on ensureArkSkills).
//
// Writing `do update set ark_skill_id = excluded.ark_skill_id` would be worse: two instances
// would keep changing the same row, the same skill would resolve to different Ark ids at different times, so what hangs on the Ark Agent
// keeps flapping, and every flap costs one more UpdateAgent call.
//
// Reading back is required: with `do nothing` RowsAffected is 0, and we cannot get the winner's id,
// while the caller needs it to build skills.
func (s *PGStore) PutArkSkillUpload(ctx context.Context, u persistmodel.ArkSkillUpload) (persistmodel.ArkSkillUpload, error) {
	if _, err := s.client.Exec(ctx,
		`insert into ark_skill_uploads
		   (workspace_id, skill_id, content_hash, key_fingerprint,
		    ark_skill_id, ark_version, created_at)
		 values ($1, $2, $3, $4, $5, $6, $7)
		 on conflict (workspace_id, skill_id, content_hash, key_fingerprint) do nothing`,
		u.WorkspaceID, u.SkillID, u.ContentHash, u.KeyFingerprint,
		u.ArkSkillID, u.ArkVersion, u.CreatedAt,
	); err != nil {
		return persistmodel.ArkSkillUpload{}, fmt.Errorf("core: 存方舟技能上传记录失败: %w", err)
	}
	// Read back unconditionally: on conflict the row in the database was written by someone else, and we must take it instead of our input.
	return s.GetArkSkillUpload(ctx, u.WorkspaceID, u.SkillID, u.ContentHash, u.KeyFingerprint)
}

// ---------------------------------------------------------------------------
// Run history
// ---------------------------------------------------------------------------

// ListRuns lists runs by workspace, supporting five filters agentId / status / search / scheduleId / trigger,
// plus limit/offset paging.
//
// The filter logic matches MemStore case by case:
//
//   - Status is an allowlist; empty means no filter;
//   - Search does a case-insensitive substring match on **the three fields title / agentName / run id**.
//     Matching only the title would be a silent behavior change (api.ts:594-598).
//   - Trigger is an allowlist; empty means no filter. **It can only query doc**; see the section below.
//
// The sort `started_at desc, ord` is equivalent to MemStore's sort.SliceStable by string descending:
// this database's Collate is C (byte comparison), matching Go's `>` exactly; ord keeps it stable when
// started_at ties. **This equivalence depends on Collate C**; see the note in schema_pg.sql.
//
// # Paging (Limit/Offset) and the problem it cures
//
// Before paging was added this query had no LIMIT, fetching the doc of **all** runs in a workspace at once
// and fully deserializing them in Go. Measured (local PG 17, each size with an equal number of noise rows in other workspaces):
//
//	100 rows     1.28 ms    0.34 MB /   2714 allocs
//	1000 rows   13.1  ms    3.58 MB /  26893 allocs
//	5000 rows   55.1  ms   19.2  MB / 134340 allocs      ← linear growth
//	5000 rows but only the first 20   6.6 ms    0.07 MB /    561 allocs
//
// ⚠️ **The cost is not in sorting.** The 55ms breaks down as: SQL sort+scan 6.0ms (11%), returning doc
// 30.5ms (57%), Go deserialization+Clone 16.9ms (32%). So "add an expression index" cannot cure
// this problem: that direction gains under 6%, and it would add write amplification on the frequently written runs table.
// The only effective thing is **returning fewer rows**, that is, LIMIT. The breakdown method is in TestListRunsCostBreakdown.
//
// **Limit == 0 means no limit**, and then the generated SQL is exactly the same as before paging was added (backward compatible;
// see persistmodel.RunQuery.Limit for why).
func (s *PGStore) ListRuns(ctx context.Context, workspaceID string, q persistmodel.RunQuery) ([]apimodel.Run, error) {
	var sb strings.Builder
	sb.WriteString(`select doc from runs where workspace_id = $1`)
	args := []any{workspaceID}

	if q.AgentID != "" {
		args = append(args, q.AgentID)
		fmt.Fprintf(&sb, ` and agent_id = $%d`, len(args))
	}
	// Filter by scheduled task. **It uses the column, not doc** (`doc->>'scheduleId'`): this column has the partial index
	// runs_schedule_idx, which jsonb field extraction cannot use.
	if q.ScheduleID != "" {
		args = append(args, q.ScheduleID)
		fmt.Fprintf(&sb, ` and schedule_id = $%d`, len(args))
	}
	if len(q.Status) > 0 {
		// RunStatus is a string alias type that the driver does not accept, so convert to []string first.
		statuses := make([]string, 0, len(q.Status))
		for _, st := range q.Status {
			statuses = append(statuses, string(st))
		}
		args = append(args, statuses)
		fmt.Fprintf(&sb, ` and status = any($%d)`, len(args))
	}
	if q.Search != "" {
		// Case-insensitive: lower() on both sides, matching MemStore's strings.ToLower.
		// No ILIKE: that would need escaping % and _, and search terms are typed casually by users, so something like `50%`
		// would become a wildcard. position() is a plain substring search with no metacharacters.
		args = append(args, strings.ToLower(q.Search))
		n := len(args)
		fmt.Fprintf(&sb, ` and (position($%d in lower(title)) > 0`+
			` or position($%d in lower(agent_name)) > 0`+
			` or position($%d in lower(id)) > 0)`, n, n, n)
	}
	// Filter by trigger type. **It can only query doc**: the runs table has no trigger scalar column (agent_id / status /
	// schedule_id all have one; only this one was not pulled out). Pulling out a column means migrating existing data, and this filter
	// always stacks after `workspace_id = $1`, acting on an already narrowed set, so it is not pulled out for now;
	// when it really becomes a bottleneck, add a column + alter following the schedule_id precedent.
	if len(q.Trigger) > 0 {
		triggers := make([]string, 0, len(q.Trigger))
		for _, tg := range q.Trigger {
			triggers = append(triggers, string(tg))
		}
		args = append(args, triggers)
		fmt.Fprintf(&sb, ` and doc->>'trigger' = any($%d)`, len(args))
	}
	// Most recently **active** first, not most recently created: Feishu reuses conversations by scope, and a new turn only appends events
	// without changing started_at; sorting by started_at would leave a conversation that just spoke at the bottom of the list (the user thinks the message was lost).
	//
	// lastActivityAt is only in doc, and old data has no such key, so coalesce falls back to the started_at column.
	// nullif is required: `doc->>'x'` gives SQL NULL for a missing key but an empty string for an empty string,
	// and an empty string sorts before every timestamp ('' < '2026-…'), which would push all old conversations to the top.
	//
	// ⚠️ This sort cannot use the **order** of runs_workspace_started_idx (the expression does not match the bare index columns,
	// so a Sort is added), but the filtering itself **does use** that index via a Bitmap Index Scan; this was corrected once by measurement,
	// so do not read this as "the whole index is unused" and go add an expression index; that direction gains under 6%.
	// ListRunsForUsage **still sorts by started_at**: it is a time-window aggregation with a different basis; do not change them together.
	sb.WriteString(` order by coalesce(nullif(doc->>'lastActivityAt', ''), started_at) desc, ord`)

	// Paging. **Limit == 0 means no limit**, and then the whole limit clause does not appear; callers from before paging
	// (`RunQuery{}`) get exactly the same SQL and results; see persistmodel.RunQuery.Limit.
	//
	// ⚠️ Offset and Limit are checked independently: `offset 5` with Limit==0 (skip the first 5, give all the rest)
	// is a valid combination, and putting Offset inside Limit's if would make it silently stop working.
	if q.Limit > 0 {
		args = append(args, q.Limit)
		fmt.Fprintf(&sb, ` limit $%d`, len(args))
	}
	if q.Offset > 0 {
		args = append(args, q.Offset)
		fmt.Fprintf(&sb, ` offset $%d`, len(args))
	}

	return queryDocs(ctx, s.client, persistmodel.CloneRun, sb.String(), args...)
}

// ListRunsForUsage fetches runs by workspace + time window, for usage aggregation.
//
// **Aggregation is not done in SQL**, even though sum()/count() look like work for the database: usage, trigger and
// durationMs are only in doc jsonb, with no scalar columns and no indexes; casting doc->>'…'
// could not use indexes, and would need aligning behavior one by one between "SQL NULL / sum() of an empty set returning NULL" and "Go zero values".
// Percentiles would even need percentile_cont, which is not the same definition as MemStore's nearest-rank.
// Fetching and computing once in Go makes both implementations agree naturally, with no need for two definitions.
//
// The time window only uses string comparison on started_at, so it goes through runs_workspace_started_idx;
// switching to started_at::timestamptz would make that index unusable (see the comment in schema_pg.sql).
func (s *PGStore) ListRunsForUsage(ctx context.Context, workspaceID string, q persistmodel.UsageQuery) ([]apimodel.Run, error) {
	var sb strings.Builder
	sb.WriteString(`select doc from runs where workspace_id = $1`)
	args := []any{workspaceID}

	if q.Since != "" {
		args = append(args, q.Since)
		fmt.Fprintf(&sb, ` and started_at >= $%d`, len(args))
	}
	if q.Until != "" {
		// Open on the right, matching MemStore's >= exactly.
		args = append(args, q.Until)
		fmt.Fprintf(&sb, ` and started_at < $%d`, len(args))
	}
	sb.WriteString(` order by started_at desc, ord`)

	return queryDocs(ctx, s.client, persistmodel.CloneRun, sb.String(), args...)
}

// ListActiveRuns lists the conversations still running in all workspaces. See the notes on the Store interface.
//
// It queries the status column instead of doc->>'status': that column was **pulled out for filtering** (see the runs table comment
// in core_pg.sql); the authoritative data is still in doc, and the read path only deserializes doc as usual.
//
// **The status allowlist is hard-coded in SQL instead of passed as a parameter**: this query has only one caller (the long-term subscription sweep),
// and "which statuses count as alive" is a cross-package convention (matching feishu's isTerminalRunStatus).
// Making it a parameter would scatter that convention across call sites, and the next person would easily miss requires_action.
func (s *PGStore) ListActiveRuns(ctx context.Context) ([]apimodel.Run, error) {
	return queryDocs(ctx, s.client, persistmodel.CloneRun,
		`select doc from runs
		  where status in ('running', 'rescheduling', 'requires_action')
		  order by started_at, id`)
}

// ClaimRunWatch atomically claims "this conversation is subscribed long-term by me". See the notes on the Store interface.
//
// # The shape is copied from ClaimDueSchedules; do not "simplify" it
//
// Both pitfalls from there apply here exactly; the postmortem is in the long comment in store_pg_schedule.go:
//
//   - **`where id in (subquery)` does not stop concurrent double claims.** Under READ COMMITTED, the EvalPlanQual
//     recheck only looks at the outer WHERE, and `id in (a precomputed set)` has nothing to do with the row's new content and is always true,
//     so the later instance updates successfully anyway. So the outer query must **repeat** the business conditions.
//   - **`for update skip locked`** makes two concurrent statements get disjoint sets already at row selection,
//     instead of waiting for the lock and then walking into that broken recheck. This is the standard way to build a task queue on PostgreSQL.
//
// The only difference from there: this claims just one row (by id), so no limit is needed, and there is no
// "limit punched through by Nested Loop" pitfall. The CTE is kept anyway: it makes row selection and update the same batch of rows.
//
// What each of the three conditions blocks:
//
//	id = $1                                this row
//	status in (the still-running ones)     final states are not subscribed (an idle conversation has no events to receive)
//	watch_until is null or < $3            do not take one someone else is subscribed to with a valid lease
func (s *PGStore) ClaimRunWatch(
	ctx context.Context, runID, owner string, now time.Time, leaseFor time.Duration,
) (bool, error) {
	res, err := s.client.Exec(ctx, `
		with target as (
		     select id from runs
		      where id = $1
		        and status in ('running', 'rescheduling', 'requires_action')
		        and (watch_until is null or watch_until < $4)
		      for update skip locked
		)
		update runs
		   set watch_by = $2, watch_until = $3
		  from target
		 where runs.id = target.id
		   -- Repeat the business conditions: the EPQ recheck looks here, see the comment above.
		   and runs.status in ('running', 'rescheduling', 'requires_action')
		   and (runs.watch_until is null or runs.watch_until < $4)`,
		runID, owner, now.Add(leaseFor), now)
	if err != nil {
		return false, fmt.Errorf("core: 认领会话订阅失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// When the driver does not support RowsAffected there is no way to tell whether it was claimed. **Better not subscribe than subscribe twice**:
		// missing one at most means this turn's events are filled in later by SyncRunEvents, while subscribing twice wastes a connection.
		// Same as feishu's ClaimInflightTurn.
		return false, nil
	}
	return n > 0, nil
}

// ReleaseRunWatch clears the claim lease of the long-term subscription. See the notes on the Store interface.
func (s *PGStore) ReleaseRunWatch(ctx context.Context, runID string) error {
	if _, err := s.client.Exec(ctx,
		`update runs set watch_by = null, watch_until = null where id = $1`,
		runID); err != nil {
		return fmt.Errorf("core: 释放会话订阅租约失败: %w", err)
	}
	return nil
}

// IncrRunRescueCount: see the notes on the core.Store interface.
//
// returning does three things at once, "increment + read the new value + check existence": 0 rows means the run is gone
// (ErrNotFound), with no need to select then update (that is TOCTOU; when several instances rescue at once
// two callers may read the same old value).
func (s *PGStore) IncrRunRescueCount(ctx context.Context, runID string) (int, error) {
	var n int
	err := s.client.QueryRow(ctx,
		`update runs set rescue_count = rescue_count + 1 where id = $1
		  returning rescue_count`, runID).Scan(&n)
	if err != nil {
		if pg.IsNoRows(err) {
			return 0, core.ErrNotFound
		}
		return 0, fmt.Errorf("core: 自增救援计数失败: %w", err)
	}
	return n, nil
}

// staleActiveRunPredicate is the condition for "a stuck active conversation", as a SQL fragment.
//
// **It is pulled out so listing and reclaiming use the same rule**: written twice, changing one and forgetting the other
// would make the sweep list a batch but change none of them; the logs show "found N" and then silence,
// exactly the hardest kind to track down, "zero errors but does nothing".
//
// ⚠️ **Make the placeholder number a parameter; do not hard-code $1.** The two callers have different parameter positions (first when listing,
// while reclaiming has id / status / doc in front), and hard-coding would make one of them always report
// `mismatched param and argument count`. This was hit in practice: the MemStore side was all green while
// PG failed outright, the typical shape of "two implementations drifting", which is why the contract tests on both sides must run.
//
// `coalesce(nullif(doc->>'lastActivityAt', empty string), started_at)`: old runs have no
// lastActivityAt key (omitempty), so `doc->>` returns NULL; and ones corrupted into an empty string midway must fall back too.
// It matches persistmodel.LastActivityOf exactly, so both stores pick the same batch.
//
// ⚠️ **Do not change it to `doc->'lastActivityAt'`** (single arrow): that returns jsonb, and comparing it with text
// either errors or compares in jsonb order, where jsonb strings carry quotes and compare differently from lexical order.
func staleActiveRunPredicate(staleParam int) string {
	return fmt.Sprintf(`
		status in ('running', 'rescheduling', 'requires_action')
		and coalesce(nullif(doc->>'lastActivityAt', ''), started_at) < $%d`, staleParam)
}

// ListStaleActiveRuns lists conversations with no activity for a long time but a status still active. See the notes on the Store interface.
//
// Ascending by activity time: **the oldest handled first**, so even when this round is cut off by the limit, the longest backlog is sure
// to be handled (same as "those that started first are subscribed first" in ListActiveRuns).
func (s *PGStore) ListStaleActiveRuns(ctx context.Context, staleBefore string) ([]apimodel.Run, error) {
	return queryDocs(ctx, s.client, persistmodel.CloneRun,
		`select doc from runs
		  where`+staleActiveRunPredicate(1)+`
		  order by coalesce(nullif(doc->>'lastActivityAt', ''), started_at), id`,
		staleBefore)
}

// RetireStaleRun pushes a stuck conversation to a final state. See the notes on the Store interface.
//
// The condition is **written again** in the WHERE (instead of read then write), so this is a CAS:
// if that late tool_result really arrived in between, the run's lastActivityAt was refreshed,
// and this UPDATE matches no rows and returns false: **it will not kill the just-revived conversation again**.
//
// The same mechanism also solves mutual exclusion across instances: whoever changes it first wins, and the later one matches no rows.
func (s *PGStore) RetireStaleRun(
	ctx context.Context, run apimodel.Run, staleBefore string,
) (bool, error) {
	cloned := persistmodel.CloneRun(run)
	blob, err := marshalDoc(cloned)
	if err != nil {
		return false, err
	}
	res, err := s.client.Exec(ctx,
		`update runs set status = $2, doc = $3
		  where id = $1 and`+staleActiveRunPredicate(4),
		run.ID, string(run.Status), blob, staleBefore)
	if err != nil {
		return false, fmt.Errorf("core: 回收卡死的会话失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// It cannot tell whether anything was changed. **Better report "not changed"**: the caller then logs nothing,
		// and tries again next round (the rule is idempotent). Falsely claiming a change would make this zombie disappear from the logs.
		return false, nil
	}
	return n > 0, nil
}

func (s *PGStore) GetRun(ctx context.Context, id string) (apimodel.Run, error) {
	return getDoc(ctx, s.client, persistmodel.CloneRun, `select doc from runs where id = $1`, id)
}

// ListRunEvents lists events, supporting a type allowlist and thread filtering, by sequence ascending.
//
// **When the run does not exist it returns an empty slice instead of ErrNotFound**: the return type of the frontend's listRunEvents is
// an array, not nullable, and a 404 would make the page throw. It comes naturally here: no rows found means an empty result.
//
// It reads the raw column instead of doc: the raw bytes of Raw must be written into the response byte for byte, and going through jsonb would
// reorder keys and squeeze whitespace. Index fields come from the typed columns, without decoding doc.
func (s *PGStore) ListRunEvents(ctx context.Context, runID string, q persistmodel.EventQuery) ([]apimodel.RunEvent, error) {
	var sb strings.Builder
	sb.WriteString(`select raw, type, sequence, thread_id, event_id from run_events where run_id = $1`)
	args := []any{runID}

	if len(q.Types) > 0 {
		args = append(args, q.Types)
		fmt.Fprintf(&sb, ` and type = any($%d)`, len(args))
	}
	if q.ThreadID != "" {
		// Rows with a NULL thread_id (session-level events) are naturally excluded by =, matching MemStore's
		// `e.ThreadID == nil || *e.ThreadID != q.ThreadID`.
		args = append(args, q.ThreadID)
		fmt.Fprintf(&sb, ` and thread_id = $%d`, len(args))
	}
	// Ark's event list defaults to asc (old to new); keep it the same.
	sb.WriteString(` order by sequence`)

	rows, err := s.client.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]apimodel.RunEvent, 0)
	for rows.Next() {
		var (
			raw      string
			typ      string
			sequence int64
			threadID sql.NullString
			eventID  sql.NullString
		)
		if err := rows.Scan(&raw, &typ, &sequence, &threadID, &eventID); err != nil {
			return nil, fmt.Errorf("core: 读取事件失败: %w", err)
		}
		e := apimodel.RunEvent{
			Raw:      json.RawMessage(raw),
			Type:     typ,
			Sequence: int(sequence),
			EventID:  eventID.String,
		}
		if threadID.Valid {
			v := threadID.String
			e.ThreadID = &v
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 遍历事件失败: %w", err)
	}
	return out, nil
}

// CreateRun stores a new run.
//
// Like CreateAgent, **there is no uniqueness check**: a run has no unique constraint besides the primary key, the ID is generated by the Service,
// and a clash means the ID generator is broken, so reporting the error as is is enough.
//
// The five scalar columns pulled out serve ListRuns filtering and sorting (see schema_pg.sql); doc is
// the authoritative data.
func (s *PGStore) CreateRun(ctx context.Context, r apimodel.Run) (apimodel.Run, error) {
	cloned := persistmodel.CloneRun(r)
	blob, err := marshalDoc(cloned)
	if err != nil {
		return apimodel.Run{}, err
	}
	if _, err := s.client.Exec(ctx,
		`insert into runs (id, workspace_id, agent_id, status, started_at, title, agent_name,
		                   schedule_id, doc, host_id, host_source)
		 values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		r.ID, r.WorkspaceID, r.AgentID, string(r.Status), r.StartedAt, r.Title, r.AgentName,
		nullableScheduleID(r), blob, nullIfEmpty(r.HostID), nullIfEmpty(r.HostSource)); err != nil {
		return apimodel.Run{}, fmt.Errorf("core: 建运行记录失败: %w", err)
	}
	return cloned, nil
}

// nullableScheduleID turns Run.ScheduleID into a value that can be written into a nullable column: an empty string gives NULL.
//
// **An empty string cannot be passed directly**: runs_schedule_idx is a partial index `where schedule_id is not null`,
// and an empty string is a real value that would squeeze every conversation/Feishu run into that index, while it exists precisely to "only index
// rows that really belong to some task". It also means something different: an empty string reads like "belongs to a task whose id is empty".
//
// Same as the handling of thread_id / event_id in AppendRunEvent in this file; those two comments explain the same thing.
func nullableScheduleID(r apimodel.Run) any {
	if r.ScheduleID == "" {
		return nil
	}
	return r.ScheduleID
}

// SaveRun overwrites.
//
// Same as SaveWorkspace / SaveAgent: **ord is not touched**, replaced in place. Changing ord would make every conversation whose status updates
// jump to the end of the list, and a conversation updates status / eventCount on every event.
func (s *PGStore) SaveRun(ctx context.Context, r apimodel.Run) (apimodel.Run, error) {
	cloned := persistmodel.CloneRun(r)
	blob, err := marshalDoc(cloned)
	if err != nil {
		return apimodel.Run{}, err
	}
	res, err := s.client.Exec(ctx,
		`update runs set workspace_id = $2, agent_id = $3, status = $4, started_at = $5,
		        title = $6, agent_name = $7, schedule_id = $8, doc = $9, host_id = $10,
		        host_source = $11
		 where id = $1`,
		r.ID, r.WorkspaceID, r.AgentID, string(r.Status), r.StartedAt, r.Title, r.AgentName,
		nullableScheduleID(r), blob, nullIfEmpty(r.HostID), nullIfEmpty(r.HostSource))
	if err != nil {
		return apimodel.Run{}, fmt.Errorf("core: 存运行记录失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apimodel.Run{}, persistmodel.ErrNotFound
	}
	return cloned, nil
}

// AppendRunEvent appends one event, with sequence number "current max +1", retrying on a clash.
//
// # When the same event is sent again, return the existing row instead of inserting another
//
// One Ark session can have several subscribers **at the same time** (Feishu inbound / Feishu resume /
// browser SSE / scheduled tasks), each doing "receive a frame → call this method once". The sequence number is assigned by this method itself,
// so two subscribers coming in with the same event would each get a new number and each write a row; the primary key
// (run_id, sequence) does not conflict at all, **so two rows are stored peacefully**, and the same line shows up several times in the UI.
//
// What blocks it is run_events_event_id_idx (a unique index; see that postmortem in schema_pg.sql).
// Hitting it means this event is already in the database, so read **the existing row** and return it,
// so the caller gets exactly the same result as the first write (sequence especially matters: the bytes forwarded to the browser
// carry it, and two subscribers must forward the same number).
//
// The two conflicts must be handled separately; **they cannot both be treated as a sequence clash to retry**:
//   - clash on (run_id, sequence) → someone took this number; back off, reread max and try again;
//   - clash on (run_id, event_id) → this event already exists; return that row directly, retrying makes no sense.
//
// # Why a retry loop instead of one INSERT ... SELECT max()+1
//
// One statement looks more elegant, but **it is not safe under READ COMMITTED**: the SELECTs of two concurrent transactions
// can both read the same max and compute the same number, and the second still hits the primary key. What really blocks it has always been
// the unique constraint, not that subquery.
//
// This used to be `on conflict (run_id, sequence) do nothing`, which looked "safe",
// but really **silently dropped events**: ConcurrentAppendRunEvent in store_test.go appends 16 concurrently,
// and only 1 survived. Ark events are pushed down concurrently, so this path really gets hit,
// and the dropped events show up in the UI as "parts of the reply are missing", with no way to find the cause from the symptoms.
//
// So just retry honestly: a clash means someone got there first; reread max and try again. Conflicts only happen on the same run,
// and the event rate of one run is the rate an agent talks, so the retry count stays low.
//
// Only after the number is settled does NewRunEventFromArk write it into the raw text, so every retry must regenerate
// the bytes, not reuse the previous round's.
//
// # ⚠️ "Two round trips, merging into one SQL saves half": this idea is wrong and was disproved by measurement
//
// This method makes two round trips per event (max first, then insert), which looks exactly like the "read-modify-write"
// anti-pattern named in CLAUDE.md. I measured a round following that intuition, and the conclusion is **do not change it**:
//
//   - **That max is not slow, and does not grow with data volume.** With 800 rows one call takes 0.22ms, and the plan is
//     an index scan (`actual time=0.072ms`): the primary key (run_id, sequence) covers it exactly.
//     Measured in segments at 0/100/200/300/400/500 rows, the per-event time is **flat**, with no degradation.
//   - **It looks slow because of contention.** With 190+ concurrent go tests hitting the same local PG,
//     the same max took 1.86ms while `select 1` took 1.09ms too: **the round-trip floor itself rose 10x**.
//     The "max is 23%" computed from that number is queuing time, not query cost.
//   - **The merged version is not safer under concurrency.** Measured with 16 concurrent: the current one retried 49 times,
//     `insert ... select max()+1` retried 44 times; both rely on the unique constraint as the backstop.
//   - ⚠️ **And the merged version is wrong**: `sequence` must be written into the bytes of `raw` by injectKeys and then
//     forwarded to the browser (see apimodel.NewRunEventFromArk); if the DB computed the number itself, the number in raw
//     would not match the column. This alone rules out that path.
//
// So the two round trips here are **intentional**; do not "optimize" it again. See the section at the top of
// TestListRunEventsQueryPlan in bench_runevents_test.go (that test turning red randomly came from the same contention).
//
// raw is stored as is in a text column, not through jsonb: those bytes must come back into the HTTP response byte for byte;
// see the "exception" in schema_pg.sql.
func (s *PGStore) AppendRunEvent(
	ctx context.Context, runID string, raw []byte,
) (apimodel.RunEvent, bool, error) {
	const maxAttempts = 50

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var seq int64
		if err := s.client.QueryRow(ctx,
			`select coalesce(max(sequence), 0) + 1 from run_events where run_id = $1`,
			runID).Scan(&seq); err != nil {
			return apimodel.RunEvent{}, false, fmt.Errorf("core: 取事件序号失败: %w", err)
		}

		ev := apimodel.NewRunEventFromArk(raw, int(seq))
		doc, err := marshalDoc(map[string]any{
			"type":              ev.Type,
			"sequence":          ev.Sequence,
			"session_thread_id": ev.ThreadID,
		})
		if err != nil {
			return apimodel.RunEvent{}, false, err
		}
		var threadID any
		if ev.ThreadID != nil {
			threadID = *ev.ThreadID
		}
		// An empty id is stored as NULL instead of an empty string: NULLs are never equal in a unique index, so undecodable frames
		// (no id) can have any number of rows without blocking each other. An empty string would be compared as a real value.
		var eventID any
		if ev.EventID != "" {
			eventID = ev.EventID
		}

		_, err = s.client.Exec(ctx,
			`insert into run_events (run_id, sequence, type, thread_id, doc, raw, event_id)
			 values ($1, $2, $3, $4, $5, $6, $7)`,
			runID, ev.Sequence, ev.Type, threadID, doc, string(ev.Raw), eventID)
		if err == nil {
			// Really inserted.
			return ev, true, nil
		}
		if !pg.IsUniqueViolation(err) {
			return apimodel.RunEvent{}, false, fmt.Errorf("core: 追加事件失败: %w", err)
		}
		// The clash is on the event id: this event was already stored; read that row back as is and return it.
		//
		// Tell them apart by constraint name instead of "query once to see if it exists": the latter is TOCTOU and costs one more round trip.
		if pg.ConstraintName(err) == runEventsEventIDIdx && ev.EventID != "" {
			existing, getErr := s.getRunEventByEventID(ctx, runID, ev.EventID)
			if getErr == nil {
				// Already there: created=false. The caller uses it to avoid triggering external side effects again
				// (observability reporting hangs on this path, and several instances each reporting once would inflate usage).
				return existing, false, nil
			}
			// Cannot read it back (a very narrow race: that row was just removed by a cascade from a run delete); treat it as a sequence clash and keep retrying.
			lastErr = err
			continue
		}
		// Sequence clash: someone got ahead; back off a little, reread max and try again.
		lastErr = err
		time.Sleep(time.Duration(attempt*2+1) * time.Millisecond)
	}
	return apimodel.RunEvent{}, false, fmt.Errorf("core: 追加事件失败（连续 %d 次撞上序号冲突）: %w",
		maxAttempts, lastErr)
}

// runEventsEventIDIdx is the name of the unique index on run_events that dedups by event id.
//
// It must match the create unique index in schema_pg.sql: AppendRunEvent relies on it to tell
// "this event already exists" apart from "the number was taken"; a mismatched name would make the former be mistaken for the latter,
// retrying 50 times for nothing and then reporting an unrelated error.
const runEventsEventIDIdx = "run_events_event_id_idx"

// getRunEventByEventID fetches the row by Ark event id, for AppendRunEvent to read back after hitting the unique index.
func (s *PGStore) getRunEventByEventID(
	ctx context.Context, runID, eventID string,
) (apimodel.RunEvent, error) {
	var typ, rawText string
	var seq int64
	var threadID *string
	if err := s.client.QueryRow(ctx,
		`select sequence, type, thread_id, raw from run_events
		  where run_id = $1 and event_id = $2`,
		runID, eventID).Scan(&seq, &typ, &threadID, &rawText); err != nil {
		return apimodel.RunEvent{}, err
	}
	return apimodel.RunEvent{
		Raw:      json.RawMessage(rawText),
		Type:     typ,
		Sequence: int(seq),
		ThreadID: threadID,
		EventID:  eventID,
	}, nil
}

// ---------------------------------------------------------------------------
// For e2e
// ---------------------------------------------------------------------------

// Reset clears and loads the seed back, in one transaction.
//
// A failure midway rolls everything back, never leaving a database that is "cleared but without the seed"; every e2e test relies on it
// to prepare data, and half a database is harder to track down than an error.
//
// The seed comes from loadSeed() (shared with MemStore), so both implementations load the same data.
func (s *PGStore) Reset(ctx context.Context) error {
	data, err := persistmodel.LoadSeed()
	if err != nil {
		return err
	}
	err = s.client.WithTx(ctx, func(tx *sql.Tx) error {
		if err := truncateAll(ctx, tx); err != nil {
			return err
		}
		return s.insertSeed(ctx, tx, data)
	})
	if err != nil {
		return fmt.Errorf("core: 重置失败: %w", err)
	}
	return nil
}

// Clear deletes all data.
func (s *PGStore) Clear(ctx context.Context) error {
	err := s.client.WithTx(ctx, func(tx *sql.Tx) error {
		return truncateAll(ctx, tx)
	})
	if err != nil {
		return fmt.Errorf("core: 清空失败: %w", err)
	}
	return nil
}

// truncateAll clears all six tables.
//
// The six tables are in one TRUNCATE, and PostgreSQL guarantees atomicity.
//
// RESTART IDENTITY resets the ord sequence to zero: otherwise the first agent created after Clear
// would continue the previous round's ord. The value itself does not matter (only used for relative ordering), but resetting gives
// reproducible numbers for "clear and insert again", one less variable when debugging.
//
// TRUNCATE has no WHERE, which is the only kind of delete the safeupdate extension does not police (it only blocks
// UPDATE / DELETE); that is fine, since the meaning of this method is exactly "clear everything".
func truncateAll(ctx context.Context, tx *sql.Tx) error {
	stmt := `truncate table ` + strings.Join(pgTables, ", ") + ` restart identity`
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("清空表失败: %w", err)
	}
	return nil
}

// insertSeed writes a memData into the database, **assuming the tables are already empty** (the caller just did TRUNCATE).
//
// Workspace order is decided by created_at (each workspace in the seed has a different createdAt), and members go into a separate
// join table. The other tables are still inserted in slice order, kept in order by ord.
func (s *PGStore) insertSeed(ctx context.Context, tx *sql.Tx, data persistmodel.Seed) error {
	for _, ws := range data.Workspaces {
		created := isoToTimeOr(ws.CreatedAt, time.Now().UTC())
		if _, err := tx.ExecContext(ctx,
			`insert into workspaces
			     (id, key, name, description, created_by, updated_by, created_at, updated_at)
			 values ($1, $2, $3, $4, $5, $6, $7, $8)`,
			ws.ID, ws.Key, ws.Name, ws.Description,
			nullIfEmpty(ws.CreatedBy), nullIfEmpty(ws.UpdatedBy),
			created, created); err != nil {
			return fmt.Errorf("装载空间 %s 失败: %w", ws.ID, err)
		}
		if err := insertMembersTx(ctx, tx, ws.ID, ws.Members); err != nil {
			return fmt.Errorf("装载空间 %s 的成员失败: %w", ws.ID, err)
		}
	}
	for _, a := range data.Agents {
		blob, err := marshalAgentDoc(a)
		if err != nil {
			return err
		}
		// ⚠️ Seed loading is also a write path, and the dual write here **must not miss a single spot**: missing it, the agents the seed creates
		// would have all-empty columns with a correct doc, and in the version that switches reads their names/status would all become empty,
		// while this round's tests stay all green (the read path still looks at doc).
		args := append([]any{a.ID, a.WorkspaceID, blob}, agentColumnValues(a)...)
		if _, err := tx.ExecContext(ctx,
			`insert into agents (id, workspace_id, doc,
			                     name, description, status, version,
			                     created_at, updated_at, created_by, updated_by,
			                     executor_type, host_id, host_source)
			 values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			args...); err != nil {
			return fmt.Errorf("装载 agent %s 失败: %w", a.ID, err)
		}
	}
	for _, sk := range data.Skills {
		blob, err := marshalDoc(sk)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`insert into skills (id, workspace_id, name, doc) values ($1, $2, $3, $4)`,
			sk.ID, sk.WorkspaceID, sk.Name, blob); err != nil {
			return fmt.Errorf("装载技能 %s 失败: %w", sk.ID, err)
		}
	}
	for _, r := range data.Runs {
		blob, err := marshalDoc(r)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`insert into runs (id, workspace_id, agent_id, status, started_at, title, agent_name,
			                   schedule_id, doc, host_id, host_source)
			 values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			r.ID, r.WorkspaceID, r.AgentID, string(r.Status), r.StartedAt, r.Title, r.AgentName,
			nullableScheduleID(r), blob, nullIfEmpty(r.HostID), nullIfEmpty(r.HostSource)); err != nil {
			return fmt.Errorf("装载 run %s 失败: %w", r.ID, err)
		}
	}
	for runID, events := range data.Events {
		if err := insertEvents(ctx, tx, runID, events); err != nil {
			return err
		}
	}
	// arkSecrets in the seed is always empty (seed.json has no such section), but write it anyway:
	// the return type of loadSeed allows it to be non-empty, and missing this step would silently lose data once seed credentials are added.
	//
	// This must also go through sealKey: otherwise if the seed ever has real credentials, they would lie in the database as plaintext,
	// and the read path, judging by prefix, would treat them as existing plaintext: usable, but "stored encrypted" would have a gap.
	for _, sec := range data.ArkSecrets {
		sealed, err := s.sealKey(sec.APIKey)
		if err != nil {
			return fmt.Errorf("加密种子方舟凭证 %s 失败: %w", sec.WorkspaceID, err)
		}
		sec.APIKey = sealed
		blob, err := marshalDoc(sec)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`insert into ark_secrets (workspace_id, doc) values ($1, $2)
			 on conflict (workspace_id) do update set doc = excluded.doc`,
			sec.WorkspaceID, blob); err != nil {
			return fmt.Errorf("装载方舟凭证 %s 失败: %w", sec.WorkspaceID, err)
		}
	}
	return nil
}

// insertEvents writes a batch of events.
//
// **raw is stored as is in a text column**: not through jsonb, not through marshalDoc. This is the only
// place in the whole module where the bytes must stay exact: those bytes are written straight into the HTTP response, to get around
// encoding/json's HTML escaping. Storing them in jsonb would reorder keys and squeeze whitespace, and the escaping differences would come back.
//
// doc stores a snapshot of the index fields, convenient for querying the database directly when debugging; the read path does not use it.
func insertEvents(ctx context.Context, tx *sql.Tx, runID string, events []apimodel.RunEvent) error {
	for _, e := range events {
		doc, err := marshalDoc(map[string]any{
			"type":              e.Type,
			"sequence":          e.Sequence,
			"session_thread_id": e.ThreadID,
		})
		if err != nil {
			return err
		}
		var threadID any
		if e.ThreadID != nil {
			threadID = *e.ThreadID
		}
		// Same as AppendRunEvent: an empty id is stored as NULL, so the unique index lets frames without an id through.
		// The ids of those 77 seed events were measured to be all distinct, so there is no clash here.
		var eventID any
		if e.EventID != "" {
			eventID = e.EventID
		}
		if _, err := tx.ExecContext(ctx,
			`insert into run_events (run_id, sequence, type, thread_id, doc, raw, event_id)
			 values ($1, $2, $3, $4, $5, $6, $7)`,
			runID, e.Sequence, e.Type, threadID, doc, string(e.Raw), eventID); err != nil {
			return fmt.Errorf("装载 run %s 的事件 #%d 失败: %w", runID, e.Sequence, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Feishu user OAuth credentials
// ---------------------------------------------------------------------------
//
// ⚠️⚠️ Both tokens are **encrypted columns**; going in and out of this layer they pass through sealKey / openKey,
// word for word the same as the Ark credential group. See the package comment of shared/infra/cryptobox and sealKey in this file.
//
// **Encrypting before storing and decrypting after reading both live here**, so the Service layer always gets plaintext:
// structures in that layer flow around in memory, get built into views and compared, and forgetting to decrypt anywhere would send ciphertext
// to Feishu as the token, getting back 99991668 "invalid token", while the real cause (missing key config) cannot be found.

// GetLarkUserToken gets a person's credentials for a Feishu app; returns ErrNotFound if never authorized.
//
// The two tokens returned are **plaintext**.
func (s *PGStore) GetLarkUserToken(
	ctx context.Context, actor, larkAppID string,
) (persistmodel.LarkUserToken, error) {
	row := s.client.QueryRow(ctx,
		`select actor, lark_app_id, access_token, refresh_token,
		        expires_at, refresh_expires_at, scope, open_id, user_name,
		        created_at, updated_at, last_refresh_error
		   from lark_user_tokens
		  where actor = $1 and lark_app_id = $2`, actor, larkAppID)

	var tok persistmodel.LarkUserToken
	if err := row.Scan(
		&tok.Actor, &tok.LarkAppID, &tok.AccessToken, &tok.RefreshToken,
		&tok.ExpiresAt, &tok.RefreshExpiresAt, &tok.Scope, &tok.OpenID, &tok.UserName,
		&tok.CreatedAt, &tok.UpdatedAt, &tok.LastRefreshError,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return persistmodel.LarkUserToken{}, persistmodel.ErrNotFound
		}
		return persistmodel.LarkUserToken{}, fmt.Errorf("core: 查飞书用户凭据失败: %w", err)
	}
	// ⚠️ **Both must be decrypted.** Decrypting only access would make renewal send ciphertext back to Feishu, which then reports
	// invalid_grant, looking like "the user revoked authorization", so we would delete a good credential as invalid.
	plainAccess, err := s.openKey(tok.AccessToken)
	if err != nil {
		return persistmodel.LarkUserToken{}, fmt.Errorf("core: 解密飞书 access_token 失败: %w", err)
	}
	plainRefresh, err := s.openKey(tok.RefreshToken)
	if err != nil {
		return persistmodel.LarkUserToken{}, fmt.Errorf("core: 解密飞书 refresh_token 失败: %w", err)
	}
	tok.AccessToken = plainAccess
	tok.RefreshToken = plainRefresh
	return tok, nil
}

// PutLarkUserToken overwrites credentials (upsert). The tokens in the input are plaintext.
//
// ⚠️ **It changes a copy of the input** (tok is passed by value), so it does not replace the caller's plaintext copy with ciphertext;
// the caller is about to use it to call Feishu, and would get 401 with ciphertext. Same pitfall as PutArkSecret.
func (s *PGStore) PutLarkUserToken(ctx context.Context, tok persistmodel.LarkUserToken) error {
	sealedAccess, err := s.sealKey(tok.AccessToken)
	if err != nil {
		return fmt.Errorf("core: 加密飞书 access_token 失败: %w", err)
	}
	sealedRefresh, err := s.sealKey(tok.RefreshToken)
	if err != nil {
		return fmt.Errorf("core: 加密飞书 refresh_token 失败: %w", err)
	}
	if _, err := s.client.Exec(ctx,
		`insert into lark_user_tokens
		     (actor, lark_app_id, access_token, refresh_token,
		      expires_at, refresh_expires_at, scope, open_id, user_name,
		      created_at, updated_at, last_refresh_error)
		 values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		 on conflict (actor, lark_app_id) do update set
		     access_token       = excluded.access_token,
		     refresh_token      = excluded.refresh_token,
		     expires_at         = excluded.expires_at,
		     refresh_expires_at = excluded.refresh_expires_at,
		     scope              = excluded.scope,
		     open_id            = excluded.open_id,
		     user_name          = excluded.user_name,
		     updated_at         = excluded.updated_at,
		     last_refresh_error = excluded.last_refresh_error`,
		tok.Actor, tok.LarkAppID, sealedAccess, sealedRefresh,
		tok.ExpiresAt, tok.RefreshExpiresAt, tok.Scope, tok.OpenID, tok.UserName,
		tok.CreatedAt, tok.UpdatedAt, tok.LastRefreshError,
	); err != nil {
		return fmt.Errorf("core: 存飞书用户凭据失败: %w", err)
	}
	return nil
}

// DeleteLarkUserToken deletes credentials; missing is not an error (idempotent). This implements "revoke authorization".
func (s *PGStore) DeleteLarkUserToken(ctx context.Context, actor, larkAppID string) error {
	if _, err := s.client.Exec(ctx,
		`delete from lark_user_tokens where actor = $1 and lark_app_id = $2`,
		actor, larkAppID); err != nil {
		return fmt.Errorf("core: 删飞书用户凭据失败: %w", err)
	}
	return nil
}

// ListLarkUserTokens lists all of a person's credentials.
//
// **order by lark_app_id cannot be dropped**: without it the order is decided by physical position in the heap; on a small table it happens to
// equal insertion order (so tests are all green), and it starts changing once there are more rows or after an update;
// see the lesson that small tables hide a missing order by. The MemStore side sorts by the same key too.
func (s *PGStore) ListLarkUserTokens(
	ctx context.Context, actor string,
) ([]persistmodel.LarkUserToken, error) {
	rows, err := s.client.Query(ctx,
		`select actor, lark_app_id, access_token, refresh_token,
		        expires_at, refresh_expires_at, scope, open_id, user_name,
		        created_at, updated_at, last_refresh_error
		   from lark_user_tokens
		  where actor = $1
		  order by lark_app_id`, actor)
	if err != nil {
		return nil, fmt.Errorf("core: 列飞书用户凭据失败: %w", err)
	}
	defer rows.Close()

	out := []persistmodel.LarkUserToken{}
	for rows.Next() {
		var tok persistmodel.LarkUserToken
		if err := rows.Scan(
			&tok.Actor, &tok.LarkAppID, &tok.AccessToken, &tok.RefreshToken,
			&tok.ExpiresAt, &tok.RefreshExpiresAt, &tok.Scope, &tok.OpenID, &tok.UserName,
			&tok.CreatedAt, &tok.UpdatedAt, &tok.LastRefreshError,
		); err != nil {
			return nil, fmt.Errorf("core: 读飞书用户凭据失败: %w", err)
		}
		plainAccess, err := s.openKey(tok.AccessToken)
		if err != nil {
			return nil, fmt.Errorf("core: 解密飞书 access_token 失败: %w", err)
		}
		plainRefresh, err := s.openKey(tok.RefreshToken)
		if err != nil {
			return nil, fmt.Errorf("core: 解密飞书 refresh_token 失败: %w", err)
		}
		tok.AccessToken = plainAccess
		tok.RefreshToken = plainRefresh
		out = append(out, tok)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: 读飞书用户凭据失败: %w", err)
	}
	return out, nil
}

// -- "Which Feishu app I use by default" -------------------------------------------------
//
// ⚠️ These two methods **do not touch encryption**, deliberately different from the token group above: app_id (cli_xxx) is not
// a secret (visible to everyone in Feishu groups), and encrypting it would only make the database contents unreadable when debugging.

// GetLarkUserAppPref gets the Feishu app id this person picked; returns ErrNotFound if never picked.
//
// **It returns ErrNotFound instead of an empty string**: the caller must tell "never picked" from "picked, but that app is gone",
// and an empty string would merge the two into one.
func (s *PGStore) GetLarkUserAppPref(ctx context.Context, actor string) (string, error) {
	var appID string
	if err := s.client.QueryRow(ctx,
		`select lark_app_id from lark_user_app_prefs where actor = $1`, actor,
	).Scan(&appID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", persistmodel.ErrNotFound
		}
		return "", fmt.Errorf("core: 查飞书应用偏好失败: %w", err)
	}
	if strings.TrimSpace(appID) == "" {
		return "", persistmodel.ErrNotFound
	}
	return appID, nil
}

// PutLarkUserAppPref records the Feishu app this person picked (upsert).
func (s *PGStore) PutLarkUserAppPref(ctx context.Context, actor, larkAppID string) error {
	if _, err := s.client.Exec(ctx,
		`insert into lark_user_app_prefs (actor, lark_app_id, updated_at)
		 values ($1, $2, $3)
		 on conflict (actor) do update set
		     lark_app_id = excluded.lark_app_id,
		     updated_at  = excluded.updated_at`,
		actor, larkAppID, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("core: 存飞书应用偏好失败: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Device-code intermediate state of Feishu user authorization
// ---------------------------------------------------------------------------

// PutLarkAuthSession records a device code pending authorization; for the same (actor, app) it overwrites the old one.
//
// Overwrite instead of coexisting: when the user clicks "start authorization" again, the previous one was most likely abandoned by closing the page,
// and two coexisting rows would leave polling not knowing which device_code to trust.
func (s *PGStore) PutLarkAuthSession(ctx context.Context, sess persistmodel.LarkAuthSession) error {
	if _, err := s.client.Exec(ctx,
		`insert into lark_user_auth_sessions
		     (actor, lark_app_id, device_code, poll_interval, created_at, expires_at)
		 values ($1, $2, $3, $4, $5, $6)
		 on conflict (actor, lark_app_id) do update set
		     device_code   = excluded.device_code,
		     poll_interval = excluded.poll_interval,
		     created_at    = excluded.created_at,
		     expires_at    = excluded.expires_at`,
		sess.Actor, sess.LarkAppID, sess.DeviceCode, sess.PollInterval,
		sess.CreatedAt, sess.ExpiresAt); err != nil {
		return fmt.Errorf("core: 存飞书授权会话失败: %w", err)
	}
	return nil
}

// GetLarkAuthSession gets the device code pending authorization; returns ErrNotFound if none **or expired**.
//
// ⚠️ **The expiry check is done in SQL** (`expires_at > now()`), not after reading:
// the database is the only clock, avoiding clock differences between instances causing "instance A says not expired,
// instance B says expired". The in-memory implementation filters with time.Now(), with the same meaning.
//
// Expired counts as missing, because polling with a device_code Feishu already invalidated only fails forever,
// and that failure does not point at "start over once".
func (s *PGStore) GetLarkAuthSession(
	ctx context.Context, actor, larkAppID string,
) (persistmodel.LarkAuthSession, error) {
	row := s.client.QueryRow(ctx,
		`select actor, lark_app_id, device_code, poll_interval, created_at, expires_at
		   from lark_user_auth_sessions
		  where actor = $1 and lark_app_id = $2 and expires_at > now()`,
		actor, larkAppID)

	var sess persistmodel.LarkAuthSession
	if err := row.Scan(&sess.Actor, &sess.LarkAppID, &sess.DeviceCode,
		&sess.PollInterval, &sess.CreatedAt, &sess.ExpiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return persistmodel.LarkAuthSession{}, persistmodel.ErrNotFound
		}
		return persistmodel.LarkAuthSession{}, fmt.Errorf("core: 查飞书授权会话失败: %w", err)
	}
	return sess, nil
}

// DeleteLarkAuthSession deletes the pending authorization record; missing is not an error (idempotent).
func (s *PGStore) DeleteLarkAuthSession(ctx context.Context, actor, larkAppID string) error {
	if _, err := s.client.Exec(ctx,
		`delete from lark_user_auth_sessions where actor = $1 and lark_app_id = $2`,
		actor, larkAppID); err != nil {
		return fmt.Errorf("core: 删飞书授权会话失败: %w", err)
	}
	return nil
}
