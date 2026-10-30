// Package pg is the access layer for PostgreSQL.
//
// The target is **standard PostgreSQL**: in production it can be self-hosted or a cloud
// provider's managed PostgreSQL; in local development it is postgres:17-alpine started by
// docker-compose. Both use host:port + user name and password, via pgx/v5/stdlib, on **the
// same code path**: what works locally is what runs in production.
//
// This file holds the shared Client. Connection config and DSN building are in standard.go,
// error checks are in errors.go, and queries and transactions are in query.go.
//
// # Platform-side things are not here
//
// This package does not create databases or roles. It does not need a superuser, but the
// account must be able to create tables (the easiest way is to make it the database Owner
// when creating the database). So the starting point of this package is "a database that
// already exists + an account that already has permissions".
//
// # Usage
//
//	client, err := pg.NewStandard(ctx, pg.StdConfig{
//	    Host:     "127.0.0.1",
//	    Port:     15432,
//	    Database: "ama",
//	    User:     "ama",
//	    Password: os.Getenv("AMA_PG_PASSWORD"),
//	})
//	if err != nil {
//	    return err
//	}
//	defer client.Close()
//
//	err = client.WithTx(ctx, func(tx *sql.Tx) error {
//	    _, err := tx.ExecContext(ctx, "insert into t(id) values($1)", 1)
//	    return err
//	})
//
// In practice the config is loaded from YAML (shared/infra/config + server/config.example.yaml),
// and connectivity shows up in the amad startup log.
package pg

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Client is a database connection pool.
//
// Safe for concurrent use: underneath it is a *sql.DB, which is itself a concurrency-safe
// connection pool, so one Client can be shared by many goroutines. Do not create a new Client
// per query: that would reconnect and redo the handshake every time.
type Client struct {
	db *sql.DB

	// database is the name of the database this pool connects to. Only used to label error
	// messages.
	database string

	// statementTimeoutMS / lockTimeoutMS are the default query timeouts (milliseconds) set when
	// the pool was built; 0 means off. WithoutQueryTimeouts must restore them before returning
	// its dedicated connection. A plain conn.Close() would hand the physical connection that
	// was SET to 0 back to the pool, and the next business query that borrows it would run with
	// no timeout at all.
	statementTimeoutMS int64
	lockTimeoutMS      int64
}

// NewWithDB creates a Client from a *sql.DB the caller built itself.
//
// Used for test injection, or when the caller wants full control over the DSN and pool
// settings. This package will not touch its pool config. The database name is empty, so
// db= in error messages will be empty. Use NewStandard if you want the label.
func NewWithDB(db *sql.DB) (*Client, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: db 不能为 nil", ErrInvalidArgument)
	}
	return &Client{db: db}, nil
}

// Raw returns the underlying *sql.DB, for features this package does not cover (Stats, Conn,
// Prepare, and so on).
func (c *Client) Raw() *sql.DB { return c.db }

// Database returns the name of the database this Client connects to.
func (c *Client) Database() string { return c.database }

// Ping checks that the connection still works.
//
// After a long idle period, a Ping makes problems easier to locate than letting a business
// query hit the wall. This matters most after a primary/standby failover, when the pool may
// still hold connections to the old node.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping 数据库失败（db=%s）: %w", c.database, err)
	}
	return nil
}

// Stats returns live connection pool stats, for tracking down connection leaks and a full pool.
func (c *Client) Stats() sql.DBStats { return c.db.Stats() }

// ExecFunc has the same shape as Client.Exec. It is used by the WithoutQueryTimeouts callback.
type ExecFunc func(ctx context.Context, query string, args ...any) (sql.Result, error)

// ddlBudget is the bounded total budget for **the whole callback** on the exempt connection
// of WithoutQueryTimeouts.
//
// With the statement timeout turned off, the client side must set an upper bound. Otherwise,
// if DDL waits on a lock, or the connection goes half-dead after the SET, and the caller passed
// a startup-time ctx with no deadline, the whole migration (and so the whole startup) would
// hang forever. "Exempt from 10s" does not mean "never returns". 5 minutes is enough for any
// table or index creation in this repo, and still keeps startup's worst case under control.
//
// If the caller's ctx has an earlier deadline, the caller still wins (the earlier one applies).
const ddlBudget = 5 * time.Minute

// WithoutQueryTimeouts turns off statement_timeout / lock_timeout on a **dedicated
// connection** and then runs fn. Every statement in fn uses this connection and is not
// limited by the default query timeouts.
//
// The only intended use is **table migrations at startup**: DDL like CREATE INDEX can
// legitimately run longer than 10s on a large table, and the default timeout for business
// queries (see the comment on StdConfig.StatementTimeout) must not be raised for it. The
// dedicated connection is returned right after use, so the business pool's default timeouts
// are not affected.
//
// Even with the statement timeout off, the whole callback is still limited by ddlBudget
// (5 minutes). The caller does not need to wrap its own timeout.
//
// ⚠️ **Do not use it for normal business queries.** Inside here there is no server-side
// per-statement timeout as a safety net, so the caller must make sure fn finishes (migration
// scripts are fixed DDL and run only once at startup).
func (c *Client) WithoutQueryTimeouts(
	ctx context.Context, fn func(ctx context.Context, exec ExecFunc) error,
) error {
	if fn == nil {
		return fmt.Errorf("%w: 回调不能为 nil", ErrInvalidArgument)
	}
	ctx, cancel := context.WithTimeout(ctx, ddlBudget)
	defer cancel()
	conn, err := c.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("取数据库连接失败（db=%s）: %w", c.database, err)
	}
	// ⚠️ **Before returning the connection, both GUCs must be restored to the pool defaults.**
	// This physical connection goes back to the pool. If not restored, the next business query
	// that borrows it has no timeout safety net, forever (verified by a real-database test:
	// pg_sleep(12) ran the full 12 seconds on a connection that had just been returned).
	restore := func() {
		// Use a separate short-timeout ctx: the incoming ctx may already be canceled, and if
		// the connection itself is broken we should not get stuck here either.
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ok := true
		if _, err := conn.ExecContext(rctx,
			"SET statement_timeout = "+strconv.FormatInt(c.statementTimeoutMS, 10)); err != nil {
			ok = false
		}
		if _, err := conn.ExecContext(rctx,
			"SET lock_timeout = "+strconv.FormatInt(c.lockTimeoutMS, 10)); err != nil {
			ok = false
		}
		if !ok {
			// If restoring fails, the connection that was SET to 0 must never go back to the
			// pool. Returning driver.ErrBadConn makes database/sql drop this physical
			// connection (instead of putting it back in the pool).
			_ = conn.Raw(func(_ any) error { return driverBadConn })
		}
	}
	defer func() {
		restore()
		conn.Close()
	}()
	// One SET can change only one parameter (putting them together is a syntax error), so send
	// two. They use the simple query protocol; once both succeed, the rest of this connection's
	// life is not limited by the default timeouts.
	if _, err := conn.ExecContext(ctx, "SET statement_timeout = 0"); err != nil {
		return fmt.Errorf("关闭 statement_timeout 失败（db=%s）: %w", c.database, err)
	}
	if _, err := conn.ExecContext(ctx, "SET lock_timeout = 0"); err != nil {
		return fmt.Errorf("关闭 lock_timeout 失败（db=%s）: %w", c.database, err)
	}
	exec := func(ctx context.Context, query string, args ...any) (sql.Result, error) {
		if strings.TrimSpace(query) == "" {
			return nil, fmt.Errorf("%w: SQL 不能为空", ErrInvalidArgument)
		}
		res, err := conn.ExecContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("执行失败（sql=%s）: %w", briefSQL(query), err)
		}
		return res, nil
	}
	return fn(ctx, exec)
}

// Close closes the connection pool.
func (c *Client) Close() error {
	if err := c.db.Close(); err != nil {
		return fmt.Errorf("关闭连接池失败（db=%s）: %w", c.database, err)
	}
	return nil
}
