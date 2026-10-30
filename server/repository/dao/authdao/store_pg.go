// PostgreSQL implementation of Store.
//
// The in-memory implementation is lost on process restart, and with multiple instances each instance knows only its own sessions. Production must use
// this one; see "sessions must live outside the process" in the auth package comment.
//
// ⚠️ **MemStore is in `auth/store.go`, not in this package, and not in some authmem either.**
// Unlike core (two implementations `coredao` + `coremem` side by side under repository/dao),
// only the PG half of auth was moved here. That was a decision made after measuring, not an omission; the reason is in
// the "why it stays in auth" section of auth/store.go: two whitebox tests in auth reach directly into
// MemStore's private fields (store.mu / store.apiKeys / store.sessions / store.now)
// to assert "plaintext is never stored", and moving the package would mean exporting those fields.
//
// This package can import auth because **no test in package auth constructs a PGStore**;
// MemStore, on the other hand, is constructed by auth's internal tests, so moving it out would create a cycle
// (`import cycle not allowed in test`, while go build still passes).
// The test for this asymmetry is "whose internal tests construct it", the same rule as for coredao/coremem.
//
// The contract suite (store_test.go) still runs once each on mem and pg; it gets the in-memory implementation through the public
// `auth.NewMemStore()`, so crossing packages does not affect the contract discipline.
//
// # The same pattern as the PG Stores of core / feishu
//
// The DDL script is go:embed-ed into the binary, run once at construction, all if not exists, and the two SQLSTATEs of concurrent table creation
// are swallowed. The reasons are exactly the same as in feishudao/store_pg.go, which has detailed comments.
//
// # One difference from them: no jsonb, plain columns
//
// Those six tables store jsonb because the HTTP contract is byte-sensitive. The auth tables do not face that contract: Session /
// Login are server-internal structures, and the frontend sees a few fields the handler builds on the spot. So here they are split into columns,
// in exchange for "expiry checks and delete-by-email can go straight through SQL and indexes". See the header of schema_pg.sql.
//
// # Expired rows are cleaned up on the write path along the way
//
// Same as feishudao/store_pg.go: the read path only checks "has this row expired", and the real deletion hangs on the write path.
// The reason is that the read path must be fast (every request goes through it), while the write frequency (= login frequency) is already enough to clean
// expired rows. A cleanup failure should not fail this write: leftover expired rows only take space and do not affect correctness.

package authdao

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chyroc/ama/server/auth"

	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/repository/schema"
	"github.com/chyroc/ama/server/shared/infra/pg"
)

// The DDL scripts themselves are under repository/schema/, **one table per file, the file name is the table name**
// (auth_sessions.sql / auth_oidc_states.sql / auth_local_accounts.sql and so on, see schema.AuthPG()),
// shipped with the binary, not depending on placing .sql files correctly at deploy time.
//
// **The embed statement must be in the same directory as the .sql files** (go:embed cannot cross parent directories), so this only calls it
// and does not embed. The order is decided by schema.AuthPG(); to add a table, change it there.

// SQLSTATEs that can be ignored during table creation.
//
// `create table if not exists` is not atomic: when two instances start at once, both see "table does not exist",
// then both try to create it, and the slower one hits 42P07 (table already exists) or 42710 (object already exists). These two errors exactly
// mean the table is already there, which is the result we want, so they are swallowed. **This always happens during rolling deploys**;
// without swallowing, the new instance would not start, and the error does not point at "another instance is creating the table at the same time" at all.
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

var _ auth.Store = (*PGStore)(nil)

// NewPGStore builds a PG Store and runs the DDL script once.
func NewPGStore(ctx context.Context, client *pg.Client) (*PGStore, error) {
	if client == nil {
		return nil, fmt.Errorf("%w: pg.Client 不能为 nil", auth.ErrInvalidArgument)
	}
	// Applied file by file (one file = one table), in the order given by schema.AuthPG().
	// No file has placeholder parameters, so pgx uses the simple query protocol, where multiple statements are allowed.
	//
	// ⚠️ After splitting into one file per table, the granularity of "swallow 42P07" also got finer: the whole auth_pg.sql used to
	// skip all four tables on one clash, now only the clashing table is skipped. This is better; do not change it back.
	// DDL goes through a dedicated connection without statement_timeout (building an index may exceed the default 10s for business queries).
	// See the comment on pg.WithoutQueryTimeouts.
	if err := client.WithoutQueryTimeouts(ctx, func(ctx context.Context, exec pg.ExecFunc) error {
		for _, script := range schema.AuthPG() {
			if _, err := exec(ctx, script.SQL); err != nil {
				if state := pg.SQLState(err); state != sqlStateDuplicateTable && state != sqlStateDuplicateObject {
					if pg.IsPermissionDenied(err) {
						return fmt.Errorf("建 auth 的表失败（账号没有建表权限——要用库的 Owner 或有 CREATE 权限的账号）[%s]: %w", script.Name, err)
					}
					// Include the file name (= table name), otherwise "failed to create auth tables" means searching through four tables yourself.
					return fmt.Errorf("建 auth 的表失败（%s）: %w", script.Name, err)
				}
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return &PGStore{client: client, now: time.Now}, nil
}

// ---- Sessions ----

func (s *PGStore) PutSession(ctx context.Context, session auth.Session) error {
	if strings.TrimSpace(session.TokenHash) == "" {
		return fmt.Errorf("%w: TokenHash 不能为空", auth.ErrInvalidArgument)
	}

	// Clean expired rows along the way. On the write path rather than the read path; see the file header for why.
	_, _ = s.client.Exec(ctx, `delete from auth_sessions where expires_at < $1`, s.now())

	const q = `
insert into auth_sessions (token_hash, email, name, avatar_url, employee_id, created_at, expires_at)
values ($1, $2, $3, $4, $5, $6, $7)
on conflict (token_hash) do update set
    email = excluded.email,
    name = excluded.name,
    avatar_url = excluded.avatar_url,
    employee_id = excluded.employee_id,
    created_at = excluded.created_at,
    expires_at = excluded.expires_at`
	if _, err := s.client.Exec(ctx, q,
		session.TokenHash, session.Email, session.Name,
		session.AvatarURL, session.EmployeeID,
		session.CreatedAt, session.ExpiresAt,
	); err != nil {
		return fmt.Errorf("写入会话失败: %w", err)
	}
	return nil
}

func (s *PGStore) GetSession(ctx context.Context, tokenHash string) (auth.Session, error) {
	if strings.TrimSpace(tokenHash) == "" {
		return auth.Session{}, auth.ErrNotFound
	}
	const q = `
select token_hash, email, name, avatar_url, employee_id, created_at, expires_at
from auth_sessions where token_hash = $1`

	var out auth.Session
	err := s.client.QueryRow(ctx, q, tokenHash).Scan(
		&out.TokenHash, &out.Email, &out.Name,
		&out.AvatarURL, &out.EmployeeID,
		&out.CreatedAt, &out.ExpiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.Session{}, auth.ErrNotFound
	}
	if err != nil {
		return auth.Session{}, fmt.Errorf("读取会话失败: %w", err)
	}
	// Expired means missing, same as the in-memory implementation (see the comment on Store.GetSession).
	if out.Expired(s.now()) {
		return auth.Session{}, auth.ErrNotFound
	}
	return out, nil
}

func (s *PGStore) DeleteSession(ctx context.Context, tokenHash string) error {
	if strings.TrimSpace(tokenHash) == "" {
		return nil
	}
	if _, err := s.client.Exec(ctx, `delete from auth_sessions where token_hash = $1`, tokenHash); err != nil {
		return fmt.Errorf("删除会话失败: %w", err)
	}
	return nil
}

func (s *PGStore) DeleteSessionsByEmail(ctx context.Context, email string) (int, error) {
	target := persistmodel.NormalizeEmail(email)
	if target == "" {
		return 0, fmt.Errorf("%w: email 不能为空", auth.ErrInvalidArgument)
	}
	res, err := s.client.Exec(ctx, `delete from auth_sessions where email = $1`, target)
	if err != nil {
		return 0, fmt.Errorf("删除会话失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// A driver that does not support counts is not an error: the delete itself already succeeded.
		return 0, nil
	}
	return int(n), nil
}

// ---- OIDC login flow ----

// PutOIDCState writes one in-progress OIDC login.
//
// ⚠️ **On conflict used_at must be reset to null.** state is 32 random bytes and never really
// repeats, so this branch is never reached normally; but missing it plants a dead state where "a consumed state can never come back"
// (the in-memory implementation overwrites the whole row and naturally has no such problem; exactly the kind of "two implementations drift apart").
func (s *PGStore) PutOIDCState(ctx context.Context, st auth.OIDCState) error {
	if strings.TrimSpace(st.State) == "" {
		return fmt.Errorf("%w: state 不能为空", auth.ErrInvalidArgument)
	}

	// Clean expired rows along the way: the write frequency is the login frequency, enough to keep it clean. Failure does not affect this write.
	_, _ = s.client.Exec(ctx, `delete from auth_oidc_states where expires_at < $1`, s.now())

	const q = `
insert into auth_oidc_states (state, nonce, code_verifier, return_to, used_at, created_at, expires_at)
values ($1, $2, $3, $4, null, $5, $6)
on conflict (state) do update set
    nonce = excluded.nonce,
    code_verifier = excluded.code_verifier,
    return_to = excluded.return_to,
    used_at = null,
    created_at = excluded.created_at,
    expires_at = excluded.expires_at`
	if _, err := s.client.Exec(ctx, q, st.State, st.Nonce, st.CodeVerifier, st.ReturnTo, st.CreatedAt, st.ExpiresAt); err != nil {
		return fmt.Errorf("写入 OIDC 登录流程失败: %w", err)
	}
	return nil
}

// ConsumeOIDCState consumes one OIDC login; one-time use is guaranteed by this SQL.
//
// `where state = $1 and used_at is null and expires_at > $2` and `set used_at = $2`
// are in **the same UPDATE**, so there is no window between "check" and "mark as used": PG serializes two concurrent
// UPDATEs on the same row, and the later one re-evaluates where and matches zero rows.
// ⚠️ Writing it as "select to check, then update" breaks it: two concurrent callbacks can both pass the check.
func (s *PGStore) ConsumeOIDCState(ctx context.Context, state string) (auth.OIDCState, error) {
	if strings.TrimSpace(state) == "" {
		return auth.OIDCState{}, auth.ErrNotFound
	}
	const q = `
update auth_oidc_states set used_at = $2
 where state = $1 and used_at is null and expires_at > $2
returning state, nonce, code_verifier, return_to, used_at, created_at, expires_at`

	var out auth.OIDCState
	var usedAt sql.NullTime
	err := s.client.QueryRow(ctx, q, state, s.now()).Scan(
		&out.State, &out.Nonce, &out.CodeVerifier, &out.ReturnTo, &usedAt, &out.CreatedAt, &out.ExpiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.OIDCState{}, auth.ErrNotFound
	}
	if err != nil {
		return auth.OIDCState{}, fmt.Errorf("消费 OIDC 登录流程失败: %w", err)
	}
	if usedAt.Valid {
		out.UsedAt = usedAt.Time
	}
	return out, nil
}

// ---- GitHub device flow ----

// PutGitHubFlow writes one GitHub login flow and cleans expired rows along the way (same as PutOIDCState).
func (s *PGStore) PutGitHubFlow(ctx context.Context, f auth.GitHubFlow) error {
	if strings.TrimSpace(f.FlowID) == "" || strings.TrimSpace(f.DeviceCode) == "" {
		return fmt.Errorf("%w: flow id 与 device code 都不能为空", auth.ErrInvalidArgument)
	}
	_, _ = s.client.Exec(ctx, `delete from auth_github_flows where expires_at < $1`, s.now())

	const q = `
insert into auth_github_flows (flow_id, binding_hash, device_code, user_code, verification_uri,
                               interval_seconds, next_poll_at, created_at, expires_at)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	if _, err := s.client.Exec(ctx, q, f.FlowID, f.BindingHash, f.DeviceCode, f.UserCode,
		f.VerificationURI, f.IntervalSeconds, f.NextPollAt, f.CreatedAt, f.ExpiresAt); err != nil {
		return fmt.Errorf("写入 GitHub 登录流程失败: %w", err)
	}
	return nil
}

// ClaimGitHubPoll claims one polling chance. The check (binding right, not expired, time reached) and pushing back
// next_poll_at are in **the same UPDATE**: PG serializes concurrent UPDATEs on the same row, and the later one
// re-evaluates where, sees the already pushed-back next_poll_at, and matches zero rows.
//
// When not claimed, it reads once more only to **tell the reason apart** ("not time yet" returns pending, "missing" returns invalid);
// that read takes no part in any write decision, so it is not a read-modify-write.
func (s *PGStore) ClaimGitHubPoll(ctx context.Context, flowID, bindingHash string, now time.Time) (auth.GitHubFlow, error) {
	if strings.TrimSpace(flowID) == "" || strings.TrimSpace(bindingHash) == "" {
		return auth.GitHubFlow{}, auth.ErrNotFound
	}
	const q = `
update auth_github_flows
   set next_poll_at = $3::timestamptz + interval_seconds * interval '1 second'
 where flow_id = $1 and binding_hash = $2 and expires_at > $3 and next_poll_at <= $3
returning flow_id, binding_hash, device_code, user_code, verification_uri,
          interval_seconds, next_poll_at, created_at, expires_at`
	var f auth.GitHubFlow
	err := s.client.QueryRow(ctx, q, flowID, bindingHash, now).Scan(
		&f.FlowID, &f.BindingHash, &f.DeviceCode, &f.UserCode, &f.VerificationURI,
		&f.IntervalSeconds, &f.NextPollAt, &f.CreatedAt, &f.ExpiresAt,
	)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return auth.GitHubFlow{}, fmt.Errorf("抢 GitHub 轮询失败: %w", err)
	}
	var interval int
	err = s.client.QueryRow(ctx,
		`select interval_seconds from auth_github_flows where flow_id = $1 and binding_hash = $2 and expires_at > $3`,
		flowID, bindingHash, now).Scan(&interval)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.GitHubFlow{}, auth.ErrNotFound
	}
	if err != nil {
		return auth.GitHubFlow{}, fmt.Errorf("查 GitHub 登录流程失败: %w", err)
	}
	return auth.GitHubFlow{IntervalSeconds: interval}, auth.ErrPollTooSoon
}

// SlowDownGitHubFlow adds 5 seconds to the interval (or uses the larger value GitHub gave) and persists it.
//
// Both greatest(...) in SET read the interval_seconds from **before the update** (PG's UPDATE semantics),
// so next_poll_at uses the new interval and the two columns agree.
func (s *PGStore) SlowDownGitHubFlow(ctx context.Context, flowID string, minInterval int, now time.Time) (int, error) {
	const q = `
update auth_github_flows
   set interval_seconds = greatest(interval_seconds + 5, $2::int),
       next_poll_at = $3::timestamptz + greatest(interval_seconds + 5, $2::int) * interval '1 second'
 where flow_id = $1
returning interval_seconds`
	var n int
	err := s.client.QueryRow(ctx, q, flowID, minInterval, now).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, auth.ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("更新 GitHub 轮询间隔失败: %w", err)
	}
	return n, nil
}

// ConsumeGitHubFlow deletes the row and reports whether this call was the one that deleted it. `delete ... returning` is one statement,
// so of two concurrent calls only one gets the row.
func (s *PGStore) ConsumeGitHubFlow(ctx context.Context, flowID string, now time.Time) (bool, error) {
	var expiresAt time.Time
	err := s.client.QueryRow(ctx,
		`delete from auth_github_flows where flow_id = $1 returning expires_at`, flowID).Scan(&expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("消费 GitHub 登录流程失败: %w", err)
	}
	return now.Before(expiresAt), nil
}

// DeleteGitHubFlow deletes one row. Missing is not an error.
func (s *PGStore) DeleteGitHubFlow(ctx context.Context, flowID string) error {
	if _, err := s.client.Exec(ctx, `delete from auth_github_flows where flow_id = $1`, flowID); err != nil {
		return fmt.Errorf("删除 GitHub 登录流程失败: %w", err)
	}
	return nil
}

// ---- Local accounts ----

// CreateLocalAccount creates a normal local account. An existing email is decided by the primary key (on conflict do nothing),
// not check-then-create: with two concurrent sign-ups for the same email, check-then-create would let the later write overwrite the earlier password.
func (s *PGStore) CreateLocalAccount(ctx context.Context, acct auth.LocalAccount) (bool, error) {
	email := persistmodel.NormalizeEmail(acct.Email)
	if email == "" || acct.PasswordHash == "" {
		return false, fmt.Errorf("%w: 邮箱与密码哈希都不能为空", auth.ErrInvalidArgument)
	}
	res, err := s.client.Exec(ctx, `
insert into auth_local_accounts (email, password_hash, is_admin, bootstrap, created_by, created_at, updated_at)
values ($1, $2, $3, false, $4, $5, $6)
on conflict (email) do nothing`,
		email, acct.PasswordHash, acct.Admin, acct.CreatedBy, acct.CreatedAt, acct.UpdatedAt)
	if err != nil {
		return false, fmt.Errorf("创建本地账号失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("创建本地账号失败（读不到影响行数）: %w", err)
	}
	return n > 0, nil
}

// CreateFirstLocalAccount creates an account only when "there are no users at all".
//
// # Why this one statement
//
// `where not exists (users) and not exists (accounts)` alone does not stop concurrency: two requests can both
// read "nobody" and both insert. So there is also **a unique index on the bootstrap column** (at most one row in the whole database
// with bootstrap=true): the later one hits the unique constraint, `on conflict do nothing` turns it into "0 rows affected",
// and the caller knows from that "you are not the first". No transaction, no table lock, no advisory lock needed.
//
// on conflict has no conflict target: both conflicts, primary key (same email) and the bootstrap unique index, must be swallowed.
func (s *PGStore) CreateFirstLocalAccount(ctx context.Context, acct auth.LocalAccount) (bool, error) {
	email := persistmodel.NormalizeEmail(acct.Email)
	if email == "" || acct.PasswordHash == "" {
		return false, fmt.Errorf("%w: 邮箱与密码哈希都不能为空", auth.ErrInvalidArgument)
	}
	res, err := s.client.Exec(ctx, `
insert into auth_local_accounts (email, password_hash, is_admin, bootstrap, created_by, created_at, updated_at)
select $1, $2, $3, true, $4, $5, $6
 where not exists (select 1 from users)
   and not exists (select 1 from auth_local_accounts)
on conflict do nothing`,
		email, acct.PasswordHash, acct.Admin, acct.CreatedBy, acct.CreatedAt, acct.UpdatedAt)
	if err != nil {
		return false, fmt.Errorf("创建第一个本地账号失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("创建第一个本地账号失败（读不到影响行数）: %w", err)
	}
	return n > 0, nil
}

func (s *PGStore) GetLocalAccount(ctx context.Context, email string) (auth.LocalAccount, error) {
	key := persistmodel.NormalizeEmail(email)
	if key == "" {
		return auth.LocalAccount{}, auth.ErrNotFound
	}
	var a auth.LocalAccount
	err := s.client.QueryRow(ctx, `
select email, password_hash, is_admin, bootstrap, created_by, created_at, updated_at
  from auth_local_accounts where email = $1`, key).Scan(
		&a.Email, &a.PasswordHash, &a.Admin, &a.Bootstrap, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.LocalAccount{}, auth.ErrNotFound
	}
	if err != nil {
		return auth.LocalAccount{}, fmt.Errorf("读取本地账号失败: %w", err)
	}
	return a, nil
}

func (s *PGStore) HasAnyUser(ctx context.Context) (bool, error) {
	var any bool
	if err := s.client.QueryRow(ctx,
		`select exists (select 1 from users) or exists (select 1 from auth_local_accounts)`).Scan(&any); err != nil {
		return false, fmt.Errorf("查询是否已有用户失败: %w", err)
	}
	return any, nil
}

// ---- Password login failure counts ----

// RecordLoginFailure records one attempt and returns the total within the window. **One upsert**, no read-modify-write.
//
// When the window start is earlier than now-window it counts as a new window: the count resets to 1 and the start becomes now. Both case
// expressions read the row from **before the update** (column references other than excluded are the old values), so they see
// the same condition.
func (s *PGStore) RecordLoginFailure(ctx context.Context, key string, now time.Time, window time.Duration) (int, error) {
	if strings.TrimSpace(key) == "" {
		return 0, fmt.Errorf("%w: key 不能为空", auth.ErrInvalidArgument)
	}
	cutoff := now.Add(-window)
	// Clean counts that expired long ago (older than two windows) along the way; failure does not affect this count.
	_, _ = s.client.Exec(ctx, `delete from auth_login_failures where updated_at < $1`, now.Add(-2*window))

	var n int
	err := s.client.QueryRow(ctx, `
insert into auth_login_failures (key, failures, window_start, updated_at)
values ($1, 1, $2, $2)
on conflict (key) do update set
    failures = case when auth_login_failures.window_start < $3 then 1
                    else auth_login_failures.failures + 1 end,
    window_start = case when auth_login_failures.window_start < $3 then $2
                        else auth_login_failures.window_start end,
    updated_at = $2
returning failures`, key, now, cutoff).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("记录登录失败次数失败: %w", err)
	}
	return n, nil
}

func (s *PGStore) ClearLoginFailures(ctx context.Context, key string) error {
	if _, err := s.client.Exec(ctx, `delete from auth_login_failures where key = $1`, key); err != nil {
		return fmt.Errorf("清除登录失败次数失败: %w", err)
	}
	return nil
}

// ---- API Key ----

func (s *PGStore) PutAPIKey(ctx context.Context, key auth.APIKeyRecord) error {
	if strings.TrimSpace(key.KeyHash) == "" {
		return fmt.Errorf("%w: KeyHash 不能为空", auth.ErrInvalidArgument)
	}
	if strings.TrimSpace(key.ID) == "" {
		return fmt.Errorf("%w: ID 不能为空", auth.ErrInvalidArgument)
	}

	// **No expiry cleanup here**, unlike the two tables above: an API Key is a long-lived credential with no concept of expiry.
	// There is only one way to make it go away: the user explicitly revokes it.
	const q = `
insert into auth_api_keys (id, key_hash, email, name, prefix, created_at, last_used_at)
values ($1, $2, $3, $4, $5, $6, $7)
on conflict (id) do update set
    key_hash = excluded.key_hash,
    email = excluded.email,
    name = excluded.name,
    prefix = excluded.prefix,
    created_at = excluded.created_at,
    last_used_at = excluded.last_used_at`
	if _, err := s.client.Exec(ctx, q,
		key.ID, key.KeyHash, key.Email, key.Name, key.Prefix,
		key.CreatedAt, nullableTime(key.LastUsedAt),
	); err != nil {
		return fmt.Errorf("写入 API Key 失败: %w", err)
	}
	return nil
}

func (s *PGStore) GetAPIKeyByHash(ctx context.Context, keyHash string) (auth.APIKeyRecord, error) {
	if strings.TrimSpace(keyHash) == "" {
		return auth.APIKeyRecord{}, auth.ErrNotFound
	}
	const q = `
select id, key_hash, email, name, prefix, created_at, last_used_at
from auth_api_keys where key_hash = $1`

	out, err := scanAPIKey(s.client.QueryRow(ctx, q, keyHash))
	if errors.Is(err, sql.ErrNoRows) {
		return auth.APIKeyRecord{}, auth.ErrNotFound
	}
	if err != nil {
		return auth.APIKeyRecord{}, fmt.Errorf("读取 API Key 失败: %w", err)
	}
	return out, nil
}

func (s *PGStore) ListAPIKeysByEmail(ctx context.Context, email string) ([]auth.APIKeyRecord, error) {
	target := persistmodel.NormalizeEmail(email)
	if target == "" {
		return nil, fmt.Errorf("%w: email 不能为空", auth.ErrInvalidArgument)
	}
	// Ordering matches the in-memory implementation exactly (see the comment on MemStore.ListAPIKeysByEmail):
	// with the same created_at fall back to id descending, otherwise the order of two keys created at the same instant is random.
	const q = `
select id, key_hash, email, name, prefix, created_at, last_used_at
from auth_api_keys where email = $1
order by created_at desc, id desc`

	rows, err := s.client.Query(ctx, q, target)
	if err != nil {
		return nil, fmt.Errorf("读取 API Key 列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []auth.APIKeyRecord
	for rows.Next() {
		rec, err := scanAPIKey(rows)
		if err != nil {
			return nil, fmt.Errorf("读取 API Key 列表失败: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取 API Key 列表失败: %w", err)
	}
	return out, nil
}

func (s *PGStore) DeleteAPIKey(ctx context.Context, email, id string) (bool, error) {
	target := persistmodel.NormalizeEmail(email)
	if target == "" || strings.TrimSpace(id) == "" {
		return false, nil
	}
	// **email goes into the where condition, instead of checking ownership first and then deleting**: that would have a TOCTOU window,
	// and one more round trip. Let the database decide "if it is yours, delete it" in one go.
	res, err := s.client.Exec(ctx,
		`delete from auth_api_keys where id = $1 and email = $2`, id, target)
	if err != nil {
		return false, fmt.Errorf("吊销 API Key 失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// The driver does not support counts: the delete itself succeeded, but **this must not return true**:
		// the upper layer uses this boolean to tell "deleted" from "not yours", and guessing wrong would turn
		// "this key does not exist" into "revoked". Return false so it goes 404;
		// the user refreshes and sees the real state.
		return false, nil
	}
	return n > 0, nil
}

func (s *PGStore) TouchAPIKey(ctx context.Context, keyHash string, at time.Time) error {
	if strings.TrimSpace(keyHash) == "" {
		return nil
	}
	// One UPDATE, no read-modify-write: this runs once on every request with a key,
	// and reading back then writing would let two concurrent requests overwrite each other (with both results "looking plausible").
	if _, err := s.client.Exec(ctx,
		`update auth_api_keys set last_used_at = $2 where key_hash = $1`, keyHash, at); err != nil {
		return fmt.Errorf("记录 API Key 使用时间失败: %w", err)
	}
	return nil
}

// PutUser writes or updates a person's profile.
//
// # Empty values do not overwrite, expressed in SQL instead of read-then-write
//
// `coalesce(nullif(excluded.name, ”), users.name)` means "use the new value if non-empty,
// otherwise keep the old value". Writing "GetUser first to see if there is a name, then decide what to write" also works,
// but that is read-modify-write: two devices logging in at once would overwrite each other, with both results "looking plausible".
//
// Why it must not overwrite: SSO **does not guarantee** name and picture every time. If one login misses them and
// writes empty strings back, that person suddenly shows only an email in the member list, with no error at all.
//
// created_at uses `users.created_at` (the old value) instead of excluded: that is the moment "this person was first seen",
// and refreshing it on every later login would make the field meaningless.
func (s *PGStore) PutUser(ctx context.Context, user auth.User) error {
	email := strings.ToLower(strings.TrimSpace(user.Email))
	if email == "" {
		return fmt.Errorf("auth: 用户邮箱不能为空")
	}
	if _, err := s.client.Exec(ctx, `
        insert into users (email, name, avatar_url, created_at, updated_at)
        values ($1, $2, $3, $4, $5)
        on conflict (email) do update set
            name       = coalesce(nullif(excluded.name, ''), users.name),
            avatar_url = coalesce(nullif(excluded.avatar_url, ''), users.avatar_url),
            updated_at = excluded.updated_at`,
		email, user.Name, user.AvatarURL, user.CreatedAt, user.UpdatedAt); err != nil {
		return fmt.Errorf("保存用户档案失败: %w", err)
	}
	return nil
}

// GetUser gets a profile by email.
func (s *PGStore) GetUser(ctx context.Context, email string) (auth.User, error) {
	key := strings.ToLower(strings.TrimSpace(email))
	var u auth.User
	err := s.client.QueryRow(ctx,
		`select email, name, avatar_url, created_at, updated_at from users where email = $1`,
		key).Scan(&u.Email, &u.Name, &u.AvatarURL, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if pg.IsNoRows(err) {
			return auth.User{}, auth.ErrNotFound
		}
		return auth.User{}, fmt.Errorf("查用户档案失败: %w", err)
	}
	return u, nil
}

// ListUsers gets profiles for a batch of emails with **one SQL**. The convention is on the auth.Store interface.
//
// `where email = any($1)` instead of looping GetUser: the latter is N+1, and the caller is exactly
// "render avatars for a list of people"; a page of 20 agents would be 20 round trips. Same precedent as coredao's
// fillMembers (it also queries the members of all workspaces with one SQL).
//
// Emails not found **are not put in the map and are not an error**: "this person never logged in" is normal (invited but never logged in,
// the machine identity of a fixed API Key, colleagues who left in existing data), and treating it as a failure would make the whole list page
// fail to open because of one unknown email.
func (s *PGStore) ListUsers(ctx context.Context, emails []string) (map[string]auth.User, error) {
	out := make(map[string]auth.User, len(emails))
	// Dedupe before querying: one person creating several agents is normal, and repeated emails make any($1) scan for nothing.
	// Empty input returns right away without hitting the database: `any('{}')` is valid, but it is a pointless round trip.
	keys := make([]string, 0, len(emails))
	seen := make(map[string]struct{}, len(emails))
	for _, e := range emails {
		key := strings.ToLower(strings.TrimSpace(e))
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return out, nil
	}

	rows, err := s.client.Query(ctx,
		`select email, name, avatar_url, created_at, updated_at
		   from users
		  where email = any($1)`, keys)
	if err != nil {
		return nil, fmt.Errorf("批量查用户档案失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var u auth.User
		if err := rows.Scan(&u.Email, &u.Name, &u.AvatarURL, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("批量查用户档案失败: %w", err)
		}
		out[u.Email] = u
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("批量查用户档案失败: %w", err)
	}
	return out, nil
}

// rowScanner extracts what QueryRow and Rows have in common, so the scan logic is written once.
//
// Writing it twice, "added a column but missed one place" would not fail compilation; that column would just always be zero on one of the paths.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanAPIKey scans one row into APIKeyRecord.
func scanAPIKey(row rowScanner) (auth.APIKeyRecord, error) {
	var out auth.APIKeyRecord
	// last_used_at is nullable (null means never used), so it goes through sql.NullTime.
	// Scanning directly into time.Time errors on null.
	var lastUsed sql.NullTime
	if err := row.Scan(&out.ID, &out.KeyHash, &out.Email, &out.Name,
		&out.Prefix, &out.CreatedAt, &lastUsed); err != nil {
		return auth.APIKeyRecord{}, err
	}
	if lastUsed.Valid {
		out.LastUsedAt = lastUsed.Time
	}
	return out, nil
}

// nullableTime turns a zero time into SQL null.
//
// **The zero value must be stored as null, not 0001-01-01**: that date is fake data that passes every check,
// so "never used" would show as "last used: year 1 AD", without any error.
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
