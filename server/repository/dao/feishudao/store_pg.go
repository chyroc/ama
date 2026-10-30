// PostgreSQL implementation of Store.
//
// The in-memory implementation (feishu/store.go) is lost on process restart, and connected apps need to really be stored. This file is that step:
// the same Store interface with a different backend, and not a line changes in the business layer.
//
// ⚠️ **memStore is not in this package; it stays in feishu** (the "in-memory implementation" section of feishu/store.go).
// Two implementations of the same interface living in two places is a choice made after weighing the cost, not a half-finished move. The full numbers and
// criteria are in that section; do not repeat them here (two copies of a comment drift apart sooner or later). The short version: memStore's
// signatures carry feishu types like App / Session, so it can only import feishu, while 5
// `package feishu` tests construct it directly → a cycle. The way out would be migrating all 12 test files to
// feishu_test and exporting 32 internal identifiers, not worth it.
//
// **This package has no such constraint**, so it can import feishu: no `package feishu` test
// constructs PGStore. The test is "whose internal tests construct it", not "which layer it is in"; the pair next door in core
// (coredao can, coremem cannot) is another example of the same rule.
//
// Note this does not affect the contract discipline: this package's eachStore gets the in-memory implementation through the **public API**
// (feishu.NewMemStore); crossing packages is no problem, and both implementations still each run once.
//
// # Why "the whole entity in one jsonb column" instead of one column per field
//
// ⚠️ **This is the historical reason for the two existing tables apps / sessions, not the rule for new tables.** The five this package added later
// (inbound_messages / inflight_turns / chats / session_scopes /
// progress_deliveries) are all real columns, and **those five are the model now**. The current rule and the two measured pieces of evidence
// that overturned this argument are in repository/schema/README.md.
//
// This repo's HTTP contract is very sensitive to fields (the source of truth is the frontend's LocalApiClient), and App has 17 fields,
// with a 15-field Readiness nested inside. If a hand-written column mapping misses one, it does not fail compilation,
// and tests may not catch it either; you only find out when some corner of the frontend suddenly stops showing it. Passing the whole struct through
// encoding/json never misses one: the struct decides which fields exist, and the serialization path follows automatically.
//
// ⚠️ **Under the new rule "many fields" is not a reason to use doc** (17 fields means 17 columns), because the defense against missing fields
// is the compiler and field-by-field round-trip tests, not the field count, and on the doc side **neither defense exists**.
//
// The cost is that "querying by some field in doc" needs a jsonb expression. As soon as such a query really appears, pull that field
// out into a real column; the four pulled out now (workspace_id / app_id / created_at / expires_at)
// were picked by exactly this standard: **someone filters, sorts, or puts a unique constraint on it**.
//
// jsonb reorders keys, but the field order in the response is decided by the Go struct at marshal time, unrelated to the key order stored in the database.
//
// # Three known differences from the in-memory implementation
//
// One: **the three unexported fields of Readiness (inspectedName / inspectedAvatar /
// inspectedBotOpenID) do not go through JSON and become zero after storing.** The in-memory implementation's Clone assigns the whole struct
// and carries them along. No impact for now: they are consumed in refreshReadiness right after inspect,
// and no code reads them again "after reading back from the Store". If that ever happens, Readiness needs a
// MarshalJSON, or they should become exported fields.
//
// Two: **SaveApp also updates the app_id column.** The in-memory SaveApp only overwrites apps[id] and does not touch
// the byAppID reverse index; if AppID really changed, the in-memory GetAppByAppID would point to a stale value.
// That behavior is not copied here: the pulled-out columns must agree with doc, otherwise the two would silently drift apart.
// No current caller changes AppID (completeRegistration reads back the same one), so behavior is unaffected.
//
// Three: **expired sessions are cleaned on the write path, not the read path.** See the comment on GetSession.
//
// # Time comes from the Go side, not the database
//
// Like the in-memory implementation it uses the now field, so tests can inject it. Using SQL's now() would make "expired" depend on the server clock,
// while checks like sessionTTL are business rules and should not follow the database's time zone settings.

package feishudao

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/chyroc/ama/server/feishu"
	"github.com/chyroc/ama/server/feishu/feishumodel"
	"strings"
	"time"

	"github.com/chyroc/ama/server/repository/schema"
	"github.com/chyroc/ama/server/shared/infra/pg"
)

// The DDL scripts themselves are under repository/schema/, **one table per file, the file name is the table name**
// (feishu_apps.sql / feishu_sessions.sql / …, seven in total), shipped with the binary,
// not depending on placing .sql files correctly at deploy time.
//
// **The embed statement must be in the same directory as the .sql files** (go:embed cannot cross parent directories), so this only calls it
// and does not embed. The order is decided by schema.FeishuPG(); to add a table, change it there.

// appIDConstraint is the name of the unique constraint on the Feishu appID; it must match the one in schema_pg.sql.
//
// It tells "appID clash" apart from "primary key clash": both are 23505, but only the former should be translated into
// ErrAppExists. A primary key clash means newAppID() generated a duplicate, which is a bug and must not be swallowed by the business layer as
// "this app is already connected".
const appIDConstraint = "feishu_apps_app_id_key"

// SQLSTATEs that can be ignored during table creation.
//
// `create table if not exists` is not atomic: when two processes start at once, both see "table does not exist",
// then both try to create it, and the slower one hits 42P07 (table already exists) or 23505 (pg_type's unique key).
// These two errors exactly mean the table is already there, which is the result we want, so they are swallowed. Other errors are reported as usual.
const (
	sqlStateDuplicateTable  = "42P07"
	sqlStateDuplicateObject = "42710"
)

// PGStore is the PostgreSQL implementation of Store. Safe for concurrent use (backed by the *sql.DB connection pool).
type PGStore struct {
	client *pg.Client

	// now is how the current time is obtained, matching the in-memory implementation; only tests override it.
	now func() time.Time
}

// Compile-time check that it really implements Store.
var _ feishu.Store = (*PGStore)(nil)

// NewPGStore builds a PG Store and runs the DDL script once.
//
// Table creation is in the constructor rather than a separate migrate command: this module has only two tables and the scripts are all if not exists,
// so running them on every start costs nothing, in exchange for "never forgetting to run migrations at deploy". When there are more tables
// and destructive changes later, split out a separate migration tool.
func NewPGStore(ctx context.Context, client *pg.Client) (*PGStore, error) {
	if client == nil {
		return nil, fmt.Errorf("%w: pg.Client 不能为 nil", feishu.ErrInvalidArgument)
	}
	// Applied file by file (one file = one table), in the order given by schema.FeishuPG().
	// No file has placeholder parameters, so pgx uses the simple query protocol, where multiple statements are allowed.
	//
	// ⚠️ After splitting into one file per table, the granularity of "swallow 42P07" also got finer: the whole feishu_pg.sql used to
	// skip all seven tables on one clash, now only the clashing table is skipped. This is better; do not change it back.
	// DDL goes through a dedicated connection without statement_timeout (building an index may exceed the default 10s for business queries).
	// See the comment on pg.WithoutQueryTimeouts.
	if err := client.WithoutQueryTimeouts(ctx, func(ctx context.Context, exec pg.ExecFunc) error {
		for _, script := range schema.FeishuPG() {
			if _, err := exec(ctx, script.SQL); err != nil {
				if state := pg.SQLState(err); state != sqlStateDuplicateTable && state != sqlStateDuplicateObject {
					// Include the file name (= table name), otherwise "failed to create feishu tables" means searching through seven tables yourself.
					return fmt.Errorf("建 feishu 的表失败（%s）: %w", script.Name, err)
				}
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return &PGStore{client: client, now: time.Now}, nil
}

// ---- Registration sessions ----

func (s *PGStore) PutSession(ctx context.Context, id string, session feishu.Session, ttl time.Duration) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return feishu.ErrInvalidArgument
	}
	if ttl <= 0 {
		ttl = feishumodel.SessionTTL
	}
	now := s.now()

	// Clean expired rows along the way. On the write path rather than the read path; see GetSession for why.
	// A cleanup failure should not fail this write: leftover expired rows only take space and do not affect correctness.
	_, _ = s.client.Exec(ctx, `delete from feishu_sessions where expires_at < $1`, now)

	raw, err := json.Marshal(session)
	if err != nil {
		return fmt.Errorf("序列化注册会话失败: %w", err)
	}
	// Overwrite: Poll writes again with the same id when it hits slow_down or corrects the account domain.
	_, err = s.client.Exec(ctx, `
		insert into feishu_sessions (id, expires_at, doc)
		values ($1, $2, $3)
		on conflict (id) do update
		set expires_at = excluded.expires_at, doc = excluded.doc`,
		id, now.Add(ttl), raw,
	)
	if err != nil {
		return fmt.Errorf("写注册会话失败: %w", err)
	}
	return nil
}

// GetSession reads a registration session; returns ErrSessionLost if missing or expired.
//
// # Why expired rows are only filtered here, not deleted
//
// The in-memory implementation sweeps on every read and write (there cleaning is just deleting a map key, at no cost).
// With SQL, a DELETE on the read path means a write on every poll, and during registration the frontend polls
// every 5 seconds, a disproportionate cost.
//
// So this filters with `expires_at >= now`: **the externally visible behavior is exactly the same** (expired sessions cannot be read,
// returning ErrSessionLost), and the physical cleanup moves to PutSession. As long as someone starts a registration, sessions keep
// being written, so they never pile up uncleaned.
//
// The boundary matches the in-memory implementation: sweepLocked checks now.After(expiresAt), so at exactly the expiry instant
// it is still valid, which is why this is >= and not >.
func (s *PGStore) GetSession(ctx context.Context, id string) (feishu.Session, error) {
	var raw []byte
	err := s.client.QueryRow(ctx,
		`select doc from feishu_sessions where id = $1 and expires_at >= $2`,
		strings.TrimSpace(id), s.now(),
	).Scan(&raw)
	if pg.IsNoRows(err) {
		return feishu.Session{}, feishu.ErrSessionLost
	}
	if err != nil {
		return feishu.Session{}, fmt.Errorf("读注册会话失败: %w", err)
	}
	var session feishu.Session
	if err := json.Unmarshal(raw, &session); err != nil {
		return feishu.Session{}, fmt.Errorf("解析注册会话失败: %w", err)
	}
	return session, nil
}

// DeleteSession deletes a registration session; missing is not an error, same as the in-memory delete(map).
func (s *PGStore) DeleteSession(ctx context.Context, id string) error {
	if _, err := s.client.Exec(ctx,
		`delete from feishu_sessions where id = $1`, strings.TrimSpace(id),
	); err != nil {
		return fmt.Errorf("删注册会话失败: %w", err)
	}
	return nil
}

// ---- Apps ----

func (s *PGStore) CreateApp(ctx context.Context, app *feishu.App, secret string) (*feishu.App, error) {
	if app == nil || strings.TrimSpace(app.AppID) == "" {
		return nil, feishu.ErrInvalidArgument
	}

	// Check once before inserting. **The real defense is the unique constraint** (the IsUniqueViolation branch below);
	// this step only copies the in-memory check-then-write, so the normal path outside concurrency gets one less PG error,
	// and both implementations take the same branch for the same input. Both paths return ErrAppExists,
	// so the business layer sees exactly the same error.
	var exists bool
	if err := s.client.QueryRow(ctx,
		`select true from feishu_apps where app_id = $1`, app.AppID,
	).Scan(&exists); err == nil {
		return nil, feishu.ErrAppExists
	} else if !pg.IsNoRows(err) {
		return nil, fmt.Errorf("查飞书应用是否已接入失败: %w", err)
	}

	stored := app.Clone()
	raw, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("序列化飞书应用失败: %w", err)
	}
	// Write NULL instead of an empty string when the secret is empty: the in-memory implementation does not put anything into secrets when secret == "",
	// so AppSecret() returns ErrAppNotFound. Storing an empty string would make this return ("", nil),
	// and the two implementations would drift apart.
	var secretArg any
	if secret != "" {
		secretArg = secret
	}
	_, err = s.client.Exec(ctx, `
		insert into feishu_apps (id, workspace_id, app_id, app_secret, created_at, doc)
		values ($1, $2, $3, $4, $5, $6)`,
		stored.ID, stored.WorkspaceID, stored.AppID, secretArg, stored.CreatedAt, raw,
	)
	if err != nil {
		if pg.IsUniqueViolation(err) && pg.ConstraintName(err) == appIDConstraint {
			return nil, feishu.ErrAppExists
		}
		return nil, fmt.Errorf("落飞书应用失败: %w", err)
	}
	return stored.Clone(), nil
}

func (s *PGStore) GetApp(ctx context.Context, id string) (*feishu.App, error) {
	return s.scanApp(s.client.QueryRow(ctx,
		`select doc from feishu_apps where id = $1`, strings.TrimSpace(id),
	))
}

func (s *PGStore) GetAppByAppID(ctx context.Context, appID string) (*feishu.App, error) {
	return s.scanApp(s.client.QueryRow(ctx,
		`select doc from feishu_apps where app_id = $1`, strings.TrimSpace(appID),
	))
}

// scanApp decodes the doc of one row into App, and translates no rows into ErrAppNotFound.
//
// It must go through Clone after decoding: slice fields in doc that are null or missing decode as nil,
// while the contract requires []. Old rows really carry null (the old Clone had this bug), and this is where it is caught.
func (s *PGStore) scanApp(row *sql.Row) (*feishu.App, error) {
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		if pg.IsNoRows(err) {
			return nil, feishu.ErrAppNotFound
		}
		return nil, fmt.Errorf("读飞书应用失败: %w", err)
	}
	var app feishu.App
	if err := json.Unmarshal(raw, &app); err != nil {
		return nil, fmt.Errorf("解析飞书应用失败: %w", err)
	}
	return app.Clone(), nil
}

// ListApps lists the apps under a workspace, ordered by connection time ascending.
//
// The return value is always non-nil (even with none), matching the in-memory make([]*App, 0, n):
// the handler puts it into {"apps": ...}, and nil would serialize as null instead of [], forcing the frontend to add one more check.
// ListAllApps lists the apps of all workspaces. See the notes on the Store interface.
func (s *PGStore) ListAllApps(ctx context.Context) ([]*feishu.App, error) {
	rows, err := s.client.Query(ctx, `
		select doc from feishu_apps
		order by created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("列全部飞书应用失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]*feishu.App, 0, 8)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("读飞书应用失败: %w", err)
		}
		var app feishu.App
		if err := json.Unmarshal(raw, &app); err != nil {
			return nil, fmt.Errorf("解析飞书应用失败: %w", err)
		}
		out = append(out, app.Clone())
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列全部飞书应用失败: %w", err)
	}
	// Same as ListApps: the SQL pass is only accurate to microseconds, so the Go side sorts again by the nanoseconds in doc.
	feishu.SortApps(out)
	return out, nil
}

func (s *PGStore) ListApps(ctx context.Context, workspaceID string) ([]*feishu.App, error) {
	rows, err := s.client.Query(ctx, `
		select doc from feishu_apps
		where workspace_id = $1
		order by created_at, id`,
		strings.TrimSpace(workspaceID),
	)
	if err != nil {
		return nil, fmt.Errorf("列飞书应用失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]*feishu.App, 0, 8)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("读飞书应用失败: %w", err)
		}
		var app feishu.App
		if err := json.Unmarshal(raw, &app); err != nil {
			return nil, fmt.Errorf("解析飞书应用失败: %w", err)
		}
		out = append(out, app.Clone())
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列飞书应用失败: %w", err)
	}

	// SQL already sorted once, but sort again here: the created_at column is timestamptz with only **microsecond**
	// precision, while createdAt in doc is nanoseconds. Two apps connected within the same millisecond may tie on the SQL side;
	// ordering them on the Go side with full precision makes the order exactly the same as the in-memory implementation.
	feishu.SortApps(out)
	return out, nil
}

// SaveApp overwrites an existing app; returns ErrAppNotFound if missing.
//
// It deliberately does not touch the app_secret column: in the in-memory implementation the secret is in another map that SaveApp cannot reach at all.
// If this wrote it casually (say an upsert overwriting the whole row), an ordinary readiness refresh would wipe the secret.
func (s *PGStore) SaveApp(ctx context.Context, app *feishu.App) (*feishu.App, error) {
	if app == nil {
		return nil, feishu.ErrInvalidArgument
	}
	stored := app.Clone()
	raw, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("序列化飞书应用失败: %w", err)
	}
	res, err := s.client.Exec(ctx, `
		update feishu_apps
		set workspace_id = $2, app_id = $3, created_at = $4, doc = $5
		where id = $1`,
		stored.ID, stored.WorkspaceID, stored.AppID, stored.CreatedAt, raw,
	)
	if err != nil {
		if pg.IsUniqueViolation(err) && pg.ConstraintName(err) == appIDConstraint {
			return nil, feishu.ErrAppExists
		}
		return nil, fmt.Errorf("保存飞书应用失败: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return nil, feishu.ErrAppNotFound
	}
	return stored.Clone(), nil
}

// AppSecret gets the app secret.
//
// Both a missing app and an app that exists but never stored a secret (app_secret is NULL) return ErrAppNotFound;
// the latter matches the in-memory implementation: secrets is a separate map, and CreateApp does not write into it when given an empty secret,
// so that lookup misses too.
func (s *PGStore) AppSecret(ctx context.Context, appID string) (string, error) {
	var secret sql.NullString
	err := s.client.QueryRow(ctx,
		`select app_secret from feishu_apps where app_id = $1`, strings.TrimSpace(appID),
	).Scan(&secret)
	if pg.IsNoRows(err) {
		return "", feishu.ErrAppNotFound
	}
	if err != nil {
		return "", fmt.Errorf("读飞书应用密钥失败: %w", err)
	}
	if !secret.Valid {
		return "", feishu.ErrAppNotFound
	}
	return secret.String, nil
}

func (s *PGStore) SetAppSecret(ctx context.Context, appID, secret string) error {
	appID = strings.TrimSpace(appID)
	if appID == "" || secret == "" {
		return feishu.ErrInvalidArgument
	}
	res, err := s.client.Exec(ctx,
		`update feishu_apps set app_secret = $2 where app_id = $1`, appID, secret,
	)
	if err != nil {
		return fmt.Errorf("刷新飞书应用密钥失败: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return feishu.ErrAppNotFound
	}
	return nil
}

// DeleteApp disconnects an app and clears its secret too.
//
// The secret is a column on the same row, so deleting the row deletes the secret along with it; there is never a state of "app gone, secret still there",
// which the in-memory implementation needs two deletes to avoid.
func (s *PGStore) DeleteApp(ctx context.Context, id string) error {
	res, err := s.client.Exec(ctx,
		`delete from feishu_apps where id = $1`, strings.TrimSpace(id),
	)
	if err != nil {
		return fmt.Errorf("断开飞书应用失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// When the driver does not support RowsAffected there is no way to tell whether anything was deleted; better let it pass than wrongly report missing.
		return nil
	}
	if n == 0 {
		return feishu.ErrAppNotFound
	}
	return nil
}

// ClaimInboundMessage claims an inbound message, for dedup. See the notes on the Store interface.
//
// # Why insert ... on conflict instead of "check then insert"
//
// Check-then-insert races: when Feishu redelivers the same message **concurrently**, two goroutines may both see "missing"
// and both go on, so the same message still runs twice; dedup would be pointless. Here the unique key decides:
// only the one whose insert succeeds gets to process it.
//
// The on conflict branch has `where status = failed`, so:
//
//	no such record    insert succeeds, RowsAffected=1   → claimed, process
//	pending exists    conflict, no update, RowsAffected=0 → duplicate delivery, ack
//	processed exists  same as above                      → duplicate delivery, ack
//	failed exists     conflict but updated to pending, =1 → let the retry through
//
// # The four anchor columns use coalesce(existing, new), not a direct overwrite
//
// Only the failed branch reaches do update, and **the anchors from the first delivery are the input the key was computed from**,
// and the only thing that counts as evidence when diagnosing. Redelivered frames are not guaranteed to carry the same fields (Feishu redeliveries may be incomplete);
// overwriting directly would swap the evidence for something of unknown origin. coalesce keeps existing non-NULL values,
// while also handling "these columns did not exist at first delivery (rows written by old code)": those are NULL and get filled by the new values.
func (s *PGStore) ClaimInboundMessage(ctx context.Context, rec feishu.InboundMessageRecord) (bool, error) {
	id := strings.TrimSpace(rec.MessageID)
	if id == "" {
		return false, feishu.ErrInvalidArgument
	}
	res, err := s.client.Exec(ctx, `
		insert into feishu_inbound_messages (
			message_id, lark_app_id, chat_id, status, created_at,
			chat_type, thread_id, root_id, parent_id
		)
		values ($1, $2, $3, $4, $5, $7, $8, $9, $10)
		on conflict (message_id) do update
			set status = excluded.status, created_at = excluded.created_at,
				chat_type = coalesce(feishu_inbound_messages.chat_type, excluded.chat_type),
				thread_id = coalesce(feishu_inbound_messages.thread_id, excluded.thread_id),
				root_id   = coalesce(feishu_inbound_messages.root_id,   excluded.root_id),
				parent_id = coalesce(feishu_inbound_messages.parent_id, excluded.parent_id)
			where feishu_inbound_messages.status = $6
	`, id, rec.LarkAppID, rec.ChatID, rec.Status, rec.CreatedAt, feishumodel.InboundFailed,
		nullIfEmpty(rec.ChatType), nullIfEmpty(rec.ThreadID),
		nullIfEmpty(rec.RootID), nullIfEmpty(rec.ParentID))
	if err != nil {
		return false, fmt.Errorf("认领入站消息失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// When the driver does not support RowsAffected there is no way to tell whether it was claimed. **Better not process than process twice**:
		// missing one message means the user asks again, while running the agent twice means two cards and double the cost.
		return false, nil
	}
	return n > 0, nil
}

// nullIfEmpty turns an empty string into NULL, so "Feishu did not give this field" is NULL in the database and not an empty string.
//
// That difference is the only useful information when diagnosing: NULL clearly means the platform gave no anchor, while an empty string is ambiguous.
// See the comments on those four columns in schema_pg.sql.
func nullIfEmpty(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

// GetInboundMessage gets a dedup record. See the notes on the Store interface.
//
// The four anchor columns are nullable (rows written by old code, or Feishu did not give them); read with sql.NullString, NULL becomes an empty string,
// since on the Go side "not given" is an empty string, matching the in-memory implementation.
func (s *PGStore) GetInboundMessage(
	ctx context.Context, messageID string,
) (feishu.InboundMessageRecord, error) {
	id := strings.TrimSpace(messageID)
	if id == "" {
		return feishu.InboundMessageRecord{}, feishu.ErrInvalidArgument
	}
	var (
		rec                                  feishu.InboundMessageRecord
		chatType, threadID, rootID, parentID sql.NullString
	)
	err := s.client.QueryRow(ctx, `
		select message_id, lark_app_id, chat_id, status, created_at,
		       chat_type, thread_id, root_id, parent_id
		  from feishu_inbound_messages
		 where message_id = $1
	`, id).Scan(&rec.MessageID, &rec.LarkAppID, &rec.ChatID, &rec.Status, &rec.CreatedAt,
		&chatType, &threadID, &rootID, &parentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return feishu.InboundMessageRecord{}, feishu.ErrInboundMessageNotFound
		}
		return feishu.InboundMessageRecord{}, fmt.Errorf("查入站消息记录失败: %w", err)
	}
	rec.ChatType = chatType.String
	rec.ThreadID = threadID.String
	rec.RootID = rootID.String
	rec.ParentID = parentID.String
	return rec, nil
}

// MarkInboundMessage updates the processing status; missing is not an error.
func (s *PGStore) MarkInboundMessage(ctx context.Context, messageID, status string) error {
	id := strings.TrimSpace(messageID)
	if id == "" {
		return feishu.ErrInvalidArgument
	}
	if _, err := s.client.Exec(ctx,
		`update feishu_inbound_messages set status = $2 where message_id = $1`, id, status,
	); err != nil {
		return fmt.Errorf("更新入站消息状态失败: %w", err)
	}
	return nil
}

// ---- Unfinished turns ----

// PutInflightTurn writes or overwrites an unfinished turn. See the notes on the Store interface.
//
// The write path calls it again and again (writing back card_message_id after creating the card, updating last_text on every card refresh), so it is an
// upsert rather than an insert. **It deliberately does not overwrite status and retry_count**: those two columns are managed by
// ClaimInflightTurn, and a routine update from the write path must not push "claimed" back to "unclaimed";
// otherwise one card refresh would let another instance claim it again, giving the same card two writers.
func (s *PGStore) PutInflightTurn(ctx context.Context, rec feishu.InflightTurnRecord) error {
	id := strings.TrimSpace(rec.RunID)
	if id == "" {
		return feishu.ErrInvalidArgument
	}
	status := rec.Status
	if status == "" {
		status = feishumodel.InflightOpen
	}
	// Store an empty string as NULL: NULL means "card not created yet", which is not the same as an empty string (see the column comment in the schema).
	var card any
	if c := strings.TrimSpace(rec.CardMessageID); c != "" {
		card = c
	}
	now := s.now()
	if _, err := s.client.Exec(ctx, `
		insert into feishu_inflight_turns (
			run_id, lark_app_id, chat_id, src_message_id, card_message_id,
			reply_in_thread, last_text, status, retry_count, started_at, updated_at,
			agent_id
		)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		on conflict (run_id) do update set
			card_message_id = excluded.card_message_id,
			last_text       = excluded.last_text,
			updated_at      = excluded.updated_at
	`, id, rec.LarkAppID, rec.ChatID, rec.SrcMessageID, card,
		rec.ReplyInThread, rec.LastText, status, rec.RetryCount, rec.StartedAt, now,
		rec.AgentID,
	); err != nil {
		return fmt.Errorf("记录未结轮次失败: %w", err)
	}
	return nil
}

// TryBeginTurn starts a conversation turn; only one turn at a time per conversation. See the notes on the Store interface.
//
// # One SQL does "take over + check staleness"
//
// Uses `on conflict do update ... where` instead of "check then insert": the latter is TOCTOU;
// two instances would both see "nobody is running" and both insert, and the serialization would be for nothing.
//
// The condition in the where clause is the real mutual exclusion check: on conflict the existing row is updated only if it is stale enough,
// otherwise RowsAffected=0, and the caller learns "a turn is already running on this conversation".
//
// Liveness uses greatest(started_at, updated_at): a long task's started_at can be very old,
// but as long as it keeps refreshing the card (updated_at moving) it is not dead and must not be taken over.
func (s *PGStore) TryBeginTurn(
	ctx context.Context, rec feishu.InflightTurnRecord, staleAfter time.Duration,
) (bool, error) {
	id := strings.TrimSpace(rec.RunID)
	if id == "" {
		return false, feishu.ErrInvalidArgument
	}
	status := rec.Status
	if status == "" {
		status = feishumodel.InflightOpen
	}
	var card any
	if c := strings.TrimSpace(rec.CardMessageID); c != "" {
		card = c
	}
	now := s.now()
	started := rec.StartedAt
	if started.IsZero() {
		started = now
	}
	// staleAfter <= 0 means "never take over", which a zero-length window cannot express, so give a
	// cutoff that can never be reached: no existing record is older than it.
	staleBefore := now.Add(-staleAfter)
	if staleAfter <= 0 {
		staleBefore = time.Unix(0, 0)
	}

	res, err := s.client.Exec(ctx, `
		insert into feishu_inflight_turns (
			run_id, lark_app_id, chat_id, src_message_id, card_message_id,
			reply_in_thread, last_text, status, retry_count, started_at, updated_at,
			agent_id
		)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $13)
		on conflict (run_id) do update set
			lark_app_id     = excluded.lark_app_id,
			chat_id         = excluded.chat_id,
			src_message_id  = excluded.src_message_id,
			card_message_id = excluded.card_message_id,
			reply_in_thread = excluded.reply_in_thread,
			last_text       = excluded.last_text,
			status          = excluded.status,
			retry_count     = 0,
			started_at      = excluded.started_at,
			updated_at      = excluded.updated_at,
			agent_id        = excluded.agent_id
		where greatest(
			feishu_inflight_turns.started_at, feishu_inflight_turns.updated_at
		) < $12
	`, id, rec.LarkAppID, rec.ChatID, rec.SrcMessageID, card,
		rec.ReplyInThread, rec.LastText, status, 0, started, now, staleBefore,
		rec.AgentID,
	)
	if err != nil {
		return false, fmt.Errorf("开启轮次失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读开启轮次结果失败: %w", err)
	}
	return affected > 0, nil
}

// ClaimInflightTurn claims an unfinished turn, for mutual exclusion across instances. See the notes on the Store interface.
//
// Same style as ClaimInboundMessage: let the database decide who is the only one taking over.
//
//	status = inflight   updated to resuming, RowsAffected=1 → claimed, this instance takes over
//	status = resuming   where does not match, RowsAffected=0 → another instance took over, skip
//	record missing      same as above                       → already finished and deleted, skip
//
// retry_count +1 along the way: claiming counts as "resumed one more time". It is in the same UPDATE instead of
// two steps, because a crash between two steps would keep the count from ever rising, and that "never finishing" turn
// would run for nothing on every restart, never reaching the limit.
func (s *PGStore) ClaimInflightTurn(ctx context.Context, runID string) (bool, error) {
	id := strings.TrimSpace(runID)
	if id == "" {
		return false, feishu.ErrInvalidArgument
	}
	res, err := s.client.Exec(ctx, `
		update feishu_inflight_turns
		set status = $2, retry_count = retry_count + 1, updated_at = $3
		where run_id = $1 and status = $4
	`, id, feishumodel.InflightResuming, s.now(), feishumodel.InflightOpen)
	if err != nil {
		return false, fmt.Errorf("认领未结轮次失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// When the driver does not support RowsAffected there is no way to tell whether it was claimed. **Better not take over than write twice**:
		// missing one means the user resends, while two instances writing the same card makes the content jump back and forth, worse than not recovering.
		return false, nil
	}
	return n > 0, nil
}

// ListInflightTurns lists all unfinished turns, by started_at ascending. See the notes on the Store interface.
//
// **resuming is listed too**, not just inflight: claiming is the very next step, and
// ClaimInflightTurn decides who can take over. This just hands out all candidates, so the "who is responsible" decision
// exists in only one place.
func (s *PGStore) ListInflightTurns(ctx context.Context) ([]feishu.InflightTurnRecord, error) {
	rows, err := s.client.Query(ctx, `
		select run_id, lark_app_id, chat_id, src_message_id, card_message_id,
		       reply_in_thread, last_text, status, retry_count, started_at, updated_at,
		       agent_id
		from feishu_inflight_turns
		order by started_at, run_id
	`)
	if err != nil {
		return nil, fmt.Errorf("列未结轮次失败: %w", err)
	}
	defer rows.Close()

	out := make([]feishu.InflightTurnRecord, 0)
	for rows.Next() {
		var rec feishu.InflightTurnRecord
		var card sql.NullString
		// agent_id is a later-added column, and **existing rows are NULL**, so it must be read with NullString.
		// Scanning directly into string would error on old data, and then the whole recovery flow could not run.
		var agent sql.NullString
		if err := rows.Scan(
			&rec.RunID, &rec.LarkAppID, &rec.ChatID, &rec.SrcMessageID, &card,
			&rec.ReplyInThread, &rec.LastText, &rec.Status, &rec.RetryCount,
			&rec.StartedAt, &rec.UpdatedAt, &agent,
		); err != nil {
			return nil, fmt.Errorf("读未结轮次失败: %w", err)
		}
		rec.CardMessageID = card.String
		rec.AgentID = agent.String
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读未结轮次失败: %w", err)
	}
	return out, nil
}

// DeleteInflightTurn deletes a turn record; missing is not an error. See the notes on the Store interface.
// RememberChat records a group chat with an idempotent upsert.
//
// **When `excluded.name` is empty the old name is kept**: inbound messages usually do not carry the group name; if one does
// and every later one without it wiped it, the symptom would be "the group name comes and goes".
// nullif + coalesce express this in one SQL, without reading first to decide.
func (s *PGStore) RememberChat(ctx context.Context, rec feishu.ChatRecord) error {
	app := strings.TrimSpace(rec.LarkAppID)
	chat := strings.TrimSpace(rec.ChatID)
	if app == "" || chat == "" {
		return feishu.ErrInvalidArgument
	}
	seen := rec.LastSeenAt
	if seen.IsZero() {
		seen = s.now()
	}
	if _, err := s.client.Exec(ctx,
		`insert into feishu_chats
		     (lark_app_id, chat_id, workspace_id, chat_type, name, last_seen_at)
		 values ($1, $2, $3, $4, $5, $6)
		 on conflict (lark_app_id, chat_id) do update
		    set workspace_id = excluded.workspace_id,
		        chat_type    = excluded.chat_type,
		        name         = coalesce(nullif(excluded.name, ''), feishu_chats.name),
		        last_seen_at = excluded.last_seen_at`,
		app, chat, rec.WorkspaceID, rec.ChatType, rec.Name, seen,
	); err != nil {
		return fmt.Errorf("记录会话失败: %w", err)
	}
	return nil
}

// SetChatName only changes a group's name; see the notes on feishu.Store.
//
// `name <> $3` avoids a pointless row write when the name has not changed (listing groups fills in names for unnamed groups repeatedly,
// and each inbound turn refreshes it once too).
func (s *PGStore) SetChatName(ctx context.Context, larkAppID, chatID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if _, err := s.client.Exec(ctx,
		`update feishu_chats set name = $3
		  where lark_app_id = $1 and chat_id = $2 and name <> $3`,
		strings.TrimSpace(larkAppID), strings.TrimSpace(chatID), name,
	); err != nil {
		return fmt.Errorf("更新群名失败: %w", err)
	}
	return nil
}

// ListChats lists the group chats recorded in a workspace, most recently seen first.
func (s *PGStore) ListChats(ctx context.Context, workspaceID string) ([]feishu.ChatRecord, error) {
	rows, err := s.client.Query(ctx,
		`select lark_app_id, chat_id, workspace_id, chat_type, name, last_seen_at
		   from feishu_chats
		  where workspace_id = $1
		  order by last_seen_at desc, chat_id`,
		strings.TrimSpace(workspaceID))
	if err != nil {
		return nil, fmt.Errorf("查会话列表失败: %w", err)
	}
	defer rows.Close()

	out := make([]feishu.ChatRecord, 0)
	for rows.Next() {
		var rec feishu.ChatRecord
		if err := rows.Scan(&rec.LarkAppID, &rec.ChatID, &rec.WorkspaceID,
			&rec.ChatType, &rec.Name, &rec.LastSeenAt); err != nil {
			return nil, fmt.Errorf("读取会话失败: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历会话失败: %w", err)
	}
	return out, nil
}

func (s *PGStore) DeleteInflightTurn(ctx context.Context, runID string) error {
	id := strings.TrimSpace(runID)
	if id == "" {
		return feishu.ErrInvalidArgument
	}
	if _, err := s.client.Exec(ctx,
		`delete from feishu_inflight_turns where run_id = $1`, id,
	); err != nil {
		return fmt.Errorf("删除未结轮次失败: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Session scope → Ark session
// ---------------------------------------------------------------------------

// sessionScopeColumns is the columns read when looking up scope bindings, shared by two queries so the order cannot drift.
const sessionScopeColumns = `lark_app_id, scope_key, agent_id, run_id, workspace_id,
	scope_type, chat_id, thread_id, created_at, last_used_at, turn_count`

// GetSessionScope looks up the binding of a scope. See the notes on the Store interface.
func (s *PGStore) GetSessionScope(
	ctx context.Context, larkAppID, scopeKey, agentID string,
) (feishu.SessionScopeRecord, error) {
	app, scope, agent, ok := feishu.TrimScopeKeys(larkAppID, scopeKey, agentID)
	if !ok {
		return feishu.SessionScopeRecord{}, feishu.ErrInvalidArgument
	}
	row := s.client.QueryRow(ctx,
		`select `+sessionScopeColumns+`
		   from feishu_session_scopes
		  where lark_app_id = $1 and scope_key = $2 and agent_id = $3`,
		app, scope, agent)
	rec, err := scanSessionScope(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Never bound is a normal state (the bot is @-ed in the group for the first time), not a failure.
			return feishu.SessionScopeRecord{}, feishu.ErrScopeNotBound
		}
		return feishu.SessionScopeRecord{}, fmt.Errorf("查会话作用域失败: %w", err)
	}
	return rec, nil
}

// BindSessionScope binds a scope. See the notes on the Store interface.
//
// # One SQL does "take over + read back"
//
// The combination of `on conflict do nothing` and `returning` has a trap: **on conflict returning returns
// no rows** (that row was not written by this statement). So when it loses, query once more to read back the winner's row for
// the caller, which must continue with the winner's run_id; otherwise two messages each hang on their own session and the context still breaks.
//
// Not `on conflict do update set run_id = excluded.run_id`: that would let a later message **replace** the
// already bound session, resetting the context once for every concurrent message, exactly the problem being fixed.
func (s *PGStore) BindSessionScope(
	ctx context.Context, rec feishu.SessionScopeRecord,
) (feishu.SessionScopeRecord, bool, error) {
	app, scope, agent, ok := feishu.TrimScopeKeys(rec.LarkAppID, rec.ScopeKey, rec.AgentID)
	if !ok {
		return feishu.SessionScopeRecord{}, false, feishu.ErrInvalidArgument
	}
	runID := strings.TrimSpace(rec.RunID)
	if runID == "" {
		return feishu.SessionScopeRecord{}, false, feishu.ErrInvalidArgument
	}
	now := s.now()
	created, lastUsed := rec.CreatedAt, rec.LastUsedAt
	if created.IsZero() {
		created = now
	}
	if lastUsed.IsZero() {
		lastUsed = now
	}
	turns := rec.TurnCount
	if turns == 0 {
		turns = 1
	}

	row := s.client.QueryRow(ctx, `
		insert into feishu_session_scopes (
			lark_app_id, scope_key, agent_id, run_id, workspace_id,
			scope_type, chat_id, thread_id, created_at, last_used_at, turn_count
		)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		on conflict (lark_app_id, scope_key, agent_id) do nothing
		returning `+sessionScopeColumns,
		app, scope, agent, runID, rec.WorkspaceID,
		rec.ScopeType, rec.ChatID, rec.ThreadID, created, lastUsed, turns)

	out, err := scanSessionScope(row)
	if err == nil {
		return out, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return feishu.SessionScopeRecord{}, false, fmt.Errorf("绑定会话作用域失败: %w", err)
	}

	// Conflict: another instance bound it first. Read its row back so the caller reuses the same session.
	existing, getErr := s.GetSessionScope(ctx, app, scope, agent)
	if getErr != nil {
		return feishu.SessionScopeRecord{}, false, fmt.Errorf("回读会话作用域失败: %w", getErr)
	}
	return existing, false, nil
}

// RebindSessionScope rebinds with CAS, for when the old session has become invalid. See the notes on the Store interface.
//
// `where run_id = $expect` is that CAS: when two instances find the same session invalid at once,
// only one can change it, the other gets RowsAffected=0 and rereads the latest one based on that.
func (s *PGStore) RebindSessionScope(
	ctx context.Context, rec feishu.SessionScopeRecord, expectRunID string,
) (bool, error) {
	app, scope, agent, ok := feishu.TrimScopeKeys(rec.LarkAppID, rec.ScopeKey, rec.AgentID)
	if !ok {
		return false, feishu.ErrInvalidArgument
	}
	runID := strings.TrimSpace(rec.RunID)
	expect := strings.TrimSpace(expectRunID)
	if runID == "" || expect == "" {
		return false, feishu.ErrInvalidArgument
	}
	res, err := s.client.Exec(ctx, `
		update feishu_session_scopes
		   set run_id = $4, last_used_at = $5, turn_count = 1
		 where lark_app_id = $1 and scope_key = $2 and agent_id = $3
		   and run_id = $6`,
		app, scope, agent, runID, s.now(), expect)
	if err != nil {
		return false, fmt.Errorf("改绑会话作用域失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读改绑结果失败: %w", err)
	}
	return affected > 0, nil
}

// TouchSessionScope updates the last active time and the turn count; missing is not an error.
func (s *PGStore) TouchSessionScope(
	ctx context.Context, larkAppID, scopeKey, agentID string, at time.Time,
) error {
	app, scope, agent, ok := feishu.TrimScopeKeys(larkAppID, scopeKey, agentID)
	if !ok {
		return feishu.ErrInvalidArgument
	}
	if at.IsZero() {
		at = s.now()
	}
	// The count is incremented in SQL, not read then written: the latter loses updates with multiple instances.
	if _, err := s.client.Exec(ctx, `
		update feishu_session_scopes
		   set last_used_at = $4, turn_count = turn_count + 1
		 where lark_app_id = $1 and scope_key = $2 and agent_id = $3`,
		app, scope, agent, at,
	); err != nil {
		return fmt.Errorf("更新会话作用域活跃时刻失败: %w", err)
	}
	return nil
}

// scanSessionScope reads one row into SessionScopeRecord.
//
// It takes rowScanner (defined in progress_pg.go) instead of a concrete type, so the QueryRow result and
// insert ... returning share one column order. Writing it twice would sooner or later shift by one column, and after shifting the types
// are often still compatible (all text), so neither compilation nor tests report anything; the data just lands in the wrong place.
func scanSessionScope(row rowScanner) (feishu.SessionScopeRecord, error) {
	var rec feishu.SessionScopeRecord
	if err := row.Scan(
		&rec.LarkAppID, &rec.ScopeKey, &rec.AgentID, &rec.RunID, &rec.WorkspaceID,
		&rec.ScopeType, &rec.ChatID, &rec.ThreadID,
		&rec.CreatedAt, &rec.LastUsedAt, &rec.TurnCount,
	); err != nil {
		return feishu.SessionScopeRecord{}, err
	}
	return rec, nil
}
