// All unit tests in this package are offline: no network, no database, no credentials.
//
// This file covers the parts unrelated to "how to connect": error classification, query
// wrappers, and transaction control flow. The three transaction paths (commit / rollback /
// panic) run on a minimal fake driver, with no third-party mock dependency. Tests for
// connection config and DSN building are in standard_test.go.
//
// For real connectivity, see "Verify connectivity" in the README.

package pg

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// ---------------------------------------------------------------------------
// DSN helpers
// ---------------------------------------------------------------------------

func dsnField(t *testing.T, dsn, key string) (string, bool) {
	t.Helper()
	for _, field := range strings.Fields(dsn) {
		idx := strings.Index(field, "=")
		if idx > 0 && field[:idx] == key {
			return strings.Trim(field[idx+1:], "'"), true
		}
	}
	return "", false
}

func TestQuoteDSNValue(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ama", "ama"},
		{"", "''"},
		{"my app", "'my app'"},
		{"it's", `'it\'s'`},
		{`back\slash`, `'back\\slash'`},
	}
	for _, tc := range cases {
		if got := quoteDSNValue(tc.in); got != tc.want {
			t.Errorf("quoteDSNValue(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}

	// A value with spaces must also come back correctly after the full DSN build
	cfg := baseStdConfig()
	cfg.ExtraParams = map[string]string{"application_name": "ama server"}
	dsn, err := cfg.toStdDSN()
	if err != nil {
		t.Fatalf("toStdDSN 失败: %v", err)
	}
	if !strings.Contains(dsn, "application_name='ama server'") {
		t.Errorf("含空格的值该被引起来，实际 DSN 为 %q", dsn)
	}

	// A value without spaces should not be quoted, and reading it back by key should give the
	// original value.
	// (A value with spaces cannot be read back: dsnField splits fields on whitespace, which is
	// its built-in limit.)
	cfg.ExtraParams = map[string]string{"application_name": "ama"}
	dsn, err = cfg.toStdDSN()
	if err != nil {
		t.Fatalf("toStdDSN 失败: %v", err)
	}
	if got, ok := dsnField(t, dsn, "application_name"); !ok || got != "ama" {
		t.Errorf("按键取回 application_name 得到 %q（ok=%v），期望 %q", got, ok, "ama")
	}
}

// ---------------------------------------------------------------------------
// Pool settings
// ---------------------------------------------------------------------------

// idleAfterUse borrows n connections, returns them all, and returns how many idle
// connections stay in the pool.
//
// The idle count only means something after "really using connections": a freshly opened
// pool always has Idle 0, so asserting it equals 0 is an always-true test that cannot tell
// whether applyStdPool took effect.
func idleAfterUse(t *testing.T, db *sql.DB, n int) int {
	t.Helper()
	ctx := context.Background()
	conns := make([]*sql.Conn, 0, n)
	for i := 0; i < n; i++ {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("取第 %d 条连接失败: %v", i+1, err)
		}
		conns = append(conns, conn)
	}
	// Return them all; only then does the idle pool keep connections per MaxIdleConns
	for _, conn := range conns {
		if err := conn.Close(); err != nil {
			t.Fatalf("归还连接失败: %v", err)
		}
	}
	return db.Stats().Idle
}

func TestErrorClassifiers(t *testing.T) {
	cases := []struct {
		name  string
		code  string
		check func(error) bool
	}{
		{"唯一键冲突", SQLStateUniqueViolation, IsUniqueViolation},
		{"外键冲突", SQLStateForeignKeyViolation, IsForeignKeyViolation},
		{"非空冲突", SQLStateNotNullViolation, IsNotNullViolation},
		{"CHECK 冲突", SQLStateCheckViolation, IsCheckViolation},
		{"死锁", SQLStateDeadlockDetected, IsDeadlock},
		{"串行化失败", SQLStateSerializationFailure, IsSerializationFailure},
		{"表不存在", SQLStateUndefinedTable, IsUndefinedTable},
		{"权限不足", SQLStateInsufficientPrivilege, IsPermissionDenied},
		{"连接数超限", SQLStateTooManyConnections, IsTooManyConnections},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := &pgconn.PgError{Code: tc.code, Message: "boom"}
			// Wrap twice, to mimic layered wrapping like Exec → WithTx
			wrapped := fmt.Errorf("外层: %w", fmt.Errorf("执行失败（sql=x）: %w", raw))

			if !tc.check(wrapped) {
				t.Errorf("包装后应仍能判出 %s", tc.code)
			}
			if got := SQLState(wrapped); got != tc.code {
				t.Errorf("SQLState = %q，期望 %q", got, tc.code)
			}
			if _, ok := AsPgError(wrapped); !ok {
				t.Error("AsPgError 应能从包装里挖回 PgError")
			}
			// Other classifiers must not report a false match
			for _, other := range cases {
				if other.code != tc.code && other.check(wrapped) {
					t.Errorf("%s 被误判成 %s", tc.code, other.code)
				}
			}
		})
	}
}

func TestConstraintName(t *testing.T) {
	raw := &pgconn.PgError{Code: SQLStateUniqueViolation, ConstraintName: "workspaces_key_idx"}
	wrapped := fmt.Errorf("执行失败: %w", raw)
	if got := ConstraintName(wrapped); got != "workspaces_key_idx" {
		t.Errorf("ConstraintName = %q，期望 workspaces_key_idx", got)
	}
	if got := ConstraintName(errors.New("普通错误")); got != "" {
		t.Errorf("非 PgError 应返回空串，实际 %q", got)
	}
}

// Errors that are not from PostgreSQL must not be classified as any kind of business error.
func TestClassifiersIgnoreOtherErrors(t *testing.T) {
	for _, err := range []error{nil, errors.New("随便"), io.EOF, ErrInvalidConfig} {
		if SQLState(err) != "" {
			t.Errorf("%v 的 SQLState 应为空", err)
		}
		if IsUniqueViolation(err) || IsDeadlock(err) || IsUndefinedTable(err) {
			t.Errorf("%v 不该被判成业务错误", err)
		}
	}
}

func TestIsNoRows(t *testing.T) {
	if !IsNoRows(sql.ErrNoRows) {
		t.Error("sql.ErrNoRows 应被判为 NoRows")
	}
	if !IsNoRows(fmt.Errorf("查询失败: %w", sql.ErrNoRows)) {
		t.Error("包装后的 sql.ErrNoRows 应仍被判出")
	}
	if IsNoRows(errors.New("别的错误")) {
		t.Error("普通错误不该被判为 NoRows")
	}
}

// connectErrorForTest produces a real *pgconn.ConnectError.
//
// The type has unexported fields and cannot be built with a literal, so let pgconn produce
// one by connecting to an address that surely fails. This tests the error type pgx really
// returns, which is more trustworthy than a hand-made one.
func connectErrorForTest(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Nobody listens on 127.0.0.1:1, and it is a local address, so no real network request
	// goes out
	_, err := pgconn.Connect(ctx, "host=127.0.0.1 port=1 user=x dbname=x sslmode=disable")
	if err == nil {
		t.Fatal("期望连接失败")
	}
	var connErr *pgconn.ConnectError
	if !errors.As(err, &connErr) {
		t.Fatalf("期望 *pgconn.ConnectError，实际 %T: %v", err, err)
	}
	return err
}

// IsRetryable is the most important check in this package for serverless setups: compute is
// suspended when idle and the workspace goes to sleep, so a failed first connection is
// expected and must be detected so it can be retried.
func TestIsRetryable(t *testing.T) {
	retryable := []struct {
		name string
		err  error
	}{
		{"服务端还在启动 57P03", &pgconn.PgError{Code: SQLStateCannotConnectNow}},
		{"管理员关闭 57P01", &pgconn.PgError{Code: SQLStateAdminShutdown}},
		{"串行化失败 40001", &pgconn.PgError{Code: SQLStateSerializationFailure}},
		{"死锁 40P01", &pgconn.PgError{Code: SQLStateDeadlockDetected}},
		{"连接已坏", driver.ErrBadConn},
		{"连接已关闭", sql.ErrConnDone},
		{"连不上", connectErrorForTest(t)},
	}
	for _, tc := range retryable {
		t.Run(tc.name, func(t *testing.T) {
			if !IsRetryable(fmt.Errorf("包一层: %w", tc.err)) {
				t.Errorf("%s 应当可重试", tc.name)
			}
		})
	}

	notRetryable := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"唯一键冲突", &pgconn.PgError{Code: SQLStateUniqueViolation}},
		{"表不存在", &pgconn.PgError{Code: SQLStateUndefinedTable}},
		{"权限不足", &pgconn.PgError{Code: SQLStateInsufficientPrivilege}},
		{"配置错误", ErrInvalidConfig},
		{"查不到行", sql.ErrNoRows},
	}
	for _, tc := range notRetryable {
		t.Run(tc.name, func(t *testing.T) {
			if IsRetryable(tc.err) {
				t.Errorf("%s 不该被判为可重试", tc.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Argument checks: empty SQL must be rejected locally, without wasting a round trip
// ---------------------------------------------------------------------------

func TestEmptyQueryRejectedLocally(t *testing.T) {
	// db is a fake driver, but these calls never even reach the driver layer
	c := &Client{db: openFakeDB(t), database: "ama"}
	ctx := context.Background()

	if _, err := c.Exec(ctx, "   "); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("空 SQL 的 Exec 应报 ErrInvalidArgument，实际 %v", err)
	}
	if _, err := c.Query(ctx, ""); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("空 SQL 的 Query 应报 ErrInvalidArgument，实际 %v", err)
	}
	if err := c.WithTx(ctx, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("nil 回调应报 ErrInvalidArgument，实际 %v", err)
	}
}

func TestNewWithDBRejectsNil(t *testing.T) {
	if _, err := NewWithDB(nil); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("nil db 应被拒，实际 %v", err)
	}
	if _, err := NewWithDB(openFakeDB(t)); err != nil {
		t.Errorf("合法 db 不该报错，实际 %v", err)
	}
}

// New must block config errors before connecting; otherwise one name resolution and one
// credential fetch are wasted.
func TestBriefSQL(t *testing.T) {
	if got := briefSQL("select\n  1,\n  2"); got != "select 1, 2" {
		t.Errorf("换行应被压掉，实际 %q", got)
	}
	long := strings.Repeat("x", 200)
	got := briefSQL(long)
	if len(got) > 130 {
		t.Errorf("长 SQL 应被截断，实际长度 %d", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("截断后应带省略号，实际 %q", got)
	}
}

// ---------------------------------------------------------------------------
// Transaction control flow
//
// A minimal fake driver that implements only Begin / Commit / Rollback / Exec, used to check
// the three paths of WithTx. No third-party dependency like sqlmock: this package only needs
// this much.
// ---------------------------------------------------------------------------

type fakeDriver struct{}

type fakeConn struct {
	// Records the transaction actions on this connection, like ["begin", "rollback"]
	actions *[]string
	failOn  string // makes one action fail, to test the error paths
}

var (
	fakeActions []string
	fakeFailOn  string
)

func (fakeDriver) Open(string) (driver.Conn, error) {
	return &fakeConn{actions: &fakeActions, failOn: fakeFailOn}, nil
}

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return fakeStmt{}, nil }
func (c *fakeConn) Close() error                        { return nil }

func (c *fakeConn) Begin() (driver.Tx, error) {
	if c.failOn == "begin" {
		return nil, errors.New("begin 失败")
	}
	*c.actions = append(*c.actions, "begin")
	return c, nil
}

func (c *fakeConn) Commit() error {
	if c.failOn == "commit" {
		*c.actions = append(*c.actions, "commit-failed")
		return errors.New("commit 失败")
	}
	*c.actions = append(*c.actions, "commit")
	return nil
}

func (c *fakeConn) Rollback() error {
	*c.actions = append(*c.actions, "rollback")
	if c.failOn == "rollback" {
		return errors.New("rollback 失败")
	}
	return nil
}

type fakeStmt struct{}

func (fakeStmt) Close() error  { return nil }
func (fakeStmt) NumInput() int { return -1 }
func (fakeStmt) Exec([]driver.Value) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}
func (fakeStmt) Query([]driver.Value) (driver.Rows, error) { return nil, io.EOF }

func init() {
	sql.Register("pgfake", fakeDriver{})
}

// openFakeDB opens a *sql.DB on the fake driver and closes it when the test ends.
func openFakeDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgfake", "")
	if err != nil {
		t.Fatalf("打开假驱动失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newTxClient prepares a Client on the fake driver and resets the action record.
func newTxClient(t *testing.T, failOn string) (*Client, *[]string) {
	t.Helper()
	fakeActions = nil
	fakeFailOn = failOn
	t.Cleanup(func() { fakeFailOn = "" })

	db, err := sql.Open("pgfake", "")
	if err != nil {
		t.Fatalf("打开假驱动失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// Keep only one connection so the action sequence is deterministic
	db.SetMaxOpenConns(1)
	return &Client{db: db, database: "ama"}, &fakeActions
}

func TestWithTxCommits(t *testing.T) {
	c, actions := newTxClient(t, "")
	err := c.WithTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("insert into t values(1)")
		return err
	})
	if err != nil {
		t.Fatalf("事务应当成功，实际 %v", err)
	}
	if got := strings.Join(*actions, ","); got != "begin,commit" {
		t.Errorf("动作序列 = %q，期望 begin,commit", got)
	}
}

// When the callback returns an error it must roll back, and **return the original business
// error**: wrapping it into something else would stop callers from detecting
// IsUniqueViolation and the like.
func TestWithTxRollsBackAndKeepsOriginalError(t *testing.T) {
	c, actions := newTxClient(t, "")
	sentinel := &pgconn.PgError{Code: SQLStateUniqueViolation, Message: "dup"}

	err := c.WithTx(context.Background(), func(*sql.Tx) error { return sentinel })
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if !IsUniqueViolation(err) {
		t.Errorf("原始的 PgError 应当被透传，实际 %v", err)
	}
	if got := strings.Join(*actions, ","); got != "begin,rollback" {
		t.Errorf("动作序列 = %q，期望 begin,rollback", got)
	}
}

// A panic must also roll back (otherwise this transaction keeps holding the connection), and
// be re-raised as is, not swallowed.
func TestWithTxRollsBackOnPanic(t *testing.T) {
	c, actions := newTxClient(t, "")

	func() {
		defer func() {
			p := recover()
			if p == nil {
				t.Error("panic 应当被原样抛出")
			}
			if s, ok := p.(string); !ok || s != "炸了" {
				t.Errorf("panic 值应当保持不变，实际 %v", p)
			}
		}()
		_ = c.WithTx(context.Background(), func(*sql.Tx) error { panic("炸了") })
	}()

	if got := strings.Join(*actions, ","); got != "begin,rollback" {
		t.Errorf("动作序列 = %q，期望 begin,rollback", got)
	}
}

// When the rollback also fails, both errors must be kept: reporting only the rollback failure
// would hide the real business error.
func TestWithTxRollbackFailureKeepsBothErrors(t *testing.T) {
	c, _ := newTxClient(t, "rollback")
	sentinel := &pgconn.PgError{Code: SQLStateUniqueViolation, Message: "dup"}

	err := c.WithTx(context.Background(), func(*sql.Tx) error { return sentinel })
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if !IsUniqueViolation(err) {
		t.Errorf("原始业务错误应当仍能判出，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "回滚失败") {
		t.Errorf("报错该提到回滚失败，实际 %v", err)
	}
}

func TestWithTxCommitFailure(t *testing.T) {
	c, _ := newTxClient(t, "commit")
	err := c.WithTx(context.Background(), func(*sql.Tx) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "提交失败") {
		t.Errorf("期望提交失败的错误，实际 %v", err)
	}
}

func TestWithTxOptionsReadOnly(t *testing.T) {
	c, actions := newTxClient(t, "")
	// The fake driver does not implement driver.ConnBeginTx, so database/sql rejects
	// non-default isolation levels/read-only. This only checks that the option is passed down
	// (the error comes from the driver, not this package).
	err := c.WithTxOptions(context.Background(), &sql.TxOptions{ReadOnly: true},
		func(*sql.Tx) error { return nil })
	if err == nil {
		t.Skip("驱动接受了 ReadOnly，跳过")
	}
	if len(*actions) != 0 {
		t.Errorf("开启事务失败时不该有动作记录，实际 %v", *actions)
	}
}

// The default idle timeout must be shorter than the platform's compute suspend threshold
// (300 seconds), otherwise the pool would hold a batch of dead connections pointing at
// compute that has been reclaimed. This is a hard rule; changing the default gets caught here.
