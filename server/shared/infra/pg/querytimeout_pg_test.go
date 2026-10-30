// Real-database checks for query timeouts (statement_timeout / lock_timeout).
//
// These complement the fake-driver tests in pg_test.go: a correct DSN only proves the
// parameters were sent; this file proves the server **really enforces these values**, and
// that the dedicated connection of WithoutQueryTimeouts really is exempt.
//
// If the local PG is unreachable, the whole file is skipped (same rule as repository/schema:
// offline go test must stay green).
package pg

import (
	"context"
	"testing"
	"time"
)

// openRealTestDB connects to the local docker-compose PG and skips if it cannot connect.
func openRealTestDB(t *testing.T, cfg StdConfig) *Client {
	t.Helper()
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.Port == 0 {
		cfg.Port = 15432
	}
	if cfg.Database == "" {
		cfg.Database = "ama"
	}
	if cfg.User == "" {
		cfg.User = "ama"
	}
	if cfg.Password == "" {
		cfg.Password = "ama"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := NewStandard(ctx, cfg)
	if err != nil {
		t.Skipf("连不上本地 PG（%s），跳过真库用例：%v", cfg.Redacted(), err)
	}
	return c
}

func TestRealDBStatementTimeoutApplied(t *testing.T) {
	c := openRealTestDB(t, StdConfig{})
	defer c.Close()

	var got string
	// Get a fresh connection and read the current value; DSN startup parameters apply to
	// every pooled connection.
	row := c.QueryRow(context.Background(), "show statement_timeout")
	if err := row.Scan(&got); err != nil {
		t.Fatalf("show statement_timeout: %v", err)
	}
	if got != "10s" {
		t.Errorf("默认 statement_timeout 应为 10s，服务端实际返回 %q", got)
	}
	row = c.QueryRow(context.Background(), "show lock_timeout")
	if err := row.Scan(&got); err != nil {
		t.Fatalf("show lock_timeout: %v", err)
	}
	if got != "2s" {
		t.Errorf("默认 lock_timeout 应为 2s，服务端实际返回 %q", got)
	}
}

func TestRealDBStatementTimeoutAbortsLongQuery(t *testing.T) {
	c := openRealTestDB(t, StdConfig{})
	defer c.Close()

	// A query that surely runs past 10s must be cut off by the server (SQLSTATE 57014
	// query_canceled), instead of hanging until the caller's ctx ends. This is the core check
	// of this hardening.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := c.Exec(ctx, "select pg_sleep(12)")
	if err == nil {
		t.Fatal("pg_sleep(12) 应被 statement_timeout 掐断，实际成功")
	}
	if state := SQLState(err); state != "57014" {
		t.Fatalf("应是 57014（query_canceled），实际 state=%q err=%v", state, err)
	}
	if d := time.Since(start); d > 11*time.Second {
		t.Errorf("应在 ~10s 被掐断，实际等了 %v", d)
	}
}

func TestRealDBWithoutQueryTimeoutsRunsLongStatement(t *testing.T) {
	c := openRealTestDB(t, StdConfig{})
	defer c.Close()

	// On the exempt connection, pg_sleep should run past the default 10s without being cut
	// off. Migration DDL takes exactly this path.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := c.WithoutQueryTimeouts(ctx, func(ctx context.Context, exec ExecFunc) error {
		_, e := exec(ctx, "select pg_sleep(11)")
		return e
	})
	if err != nil {
		t.Fatalf("豁免连接上 pg_sleep(11) 不该被掐断：%v", err)
	}

	// The exemption only applies to the dedicated connection: once it is back in the pool,
	// normal queries are still protected by the default timeout.
	_, err = c.Exec(context.Background(), "select pg_sleep(12)")
	if state := SQLState(err); state != "57014" {
		t.Fatalf("业务连接仍应受 statement_timeout 保护（57014），实际 state=%q", state)
	}
}

func TestRealDBWithoutQueryTimeoutsHasBoundedContext(t *testing.T) {
	c := openRealTestDB(t, StdConfig{})
	defer c.Close()

	// When the caller gives no deadline, the ctx the callback gets must still have an upper
	// bound (ddlBudget). Otherwise "exempt from the statement timeout" turns into "never
	// returns", and startup migrations would hang forever.
	var seenDeadline bool
	err := c.WithoutQueryTimeouts(context.Background(), func(ctx context.Context, _ ExecFunc) error {
		_, seenDeadline = ctx.Deadline()
		return nil
	})
	if err != nil {
		t.Fatalf("回调返回错误：%v", err)
	}
	if !seenDeadline {
		t.Fatal("调用方无 deadline 时，豁免回调的 ctx 也必须带上 ddlBudget 上界")
	}
}
