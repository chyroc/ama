// This file holds convenience wrappers for queries and transactions.
//
// They are all thin forwards to the *sql.DB methods with the same names, and do only two
// things: catch errors like empty SQL locally, and add "which statement failed" context to
// the error. database/sql's raw errors do not include the SQL, and a pile of
// `ERROR: relation "x" does not exist` in the logs is impossible to tell apart.
//
// Transactions are exposed only in the WithTx form (callback style), with no bare Begin. See
// the WithTx comment for why.

package pg

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
)

// driverBadConn is database/sql/driver's "connection is broken" sentinel, used by IsRetryable.
// It gets its own alias so that errors.go does not need to import the driver package too.
var driverBadConn = driver.ErrBadConn

// briefSQL shortens SQL for error messages.
//
// Putting the full SQL into the error would flood the logs (think of a migration that is
// hundreds of lines), but leaving it out entirely makes it impossible to locate. So we take
// the first 120 characters and squash newlines.
func briefSQL(query string) string {
	s := strings.Join(strings.Fields(query), " ")
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

// Exec runs a statement that returns no rows (INSERT / UPDATE / DELETE / DDL).
//
// Note: **UPDATE and DELETE always have a WHERE.** Some managed PG services support the
// safeupdate extension; once it is loaded, UPDATE / DELETE without WHERE are rejected outright.
func (c *Client) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("%w: SQL 不能为空", ErrInvalidArgument)
	}
	res, err := c.db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("执行失败（sql=%s）: %w", briefSQL(query), err)
	}
	return res, nil
}

// Query runs a query that returns multiple rows.
//
// The returned *sql.Rows must be closed by the caller, otherwise the connection does not go
// back to the pool. The pool has 25 connections by default; leak enough and everything hangs.
// The usual pattern is `defer rows.Close()`.
func (c *Client) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("%w: SQL 不能为空", ErrInvalidArgument)
	}
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询失败（sql=%s）: %w", briefSQL(query), err)
	}
	return rows, nil
}

// QueryRow runs a query that fetches only one row.
//
// Like *sql.DB.QueryRowContext, the error is deferred until Scan, so the error is not wrapped
// here (there is nowhere to put it). When there is no row, Scan returns sql.ErrNoRows; check
// it with IsNoRows.
func (c *Client) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return c.db.QueryRowContext(ctx, query, args...)
}

// WithTx runs fn in a transaction. It commits if fn returns nil and rolls back if fn returns
// an error.
//
// It offers only this callback-style entry and no bare Begin, because hand-written
// transactions have two traps that people almost always fall into: forgetting to roll back on
// an error path (the connection stays held until the ctx times out), and a panic in fn after
// which the connection never goes back to the pool. The pool is limited (25 by default), and
// enough leaks turn into the whole service hanging.
//
// A panic is rolled back first and then re-raised as is, not swallowed: the caller's recover
// logic still works and the stack is not lost.
//
// Inside the transaction use tx, not the Client's own methods. Otherwise that statement uses
// another connection and is not in the transaction:
//
//	err := client.WithTx(ctx, func(tx *sql.Tx) error {
//	    if _, err := tx.ExecContext(ctx, "insert into a(id) values($1)", 1); err != nil {
//	        return err
//	    }
//	    _, err := tx.ExecContext(ctx, "update b set n=n+1 where id=$1", 1)
//	    return err
//	})
func (c *Client) WithTx(ctx context.Context, fn func(*sql.Tx) error) error {
	return c.WithTxOptions(ctx, nil, fn)
}

// WithTxOptions is like WithTx, but lets you set the isolation level or open a read-only
// transaction.
//
// With sql.LevelSerializable, be ready to handle 40001 (serialization failure). It is
// expected; detect it with IsRetryable / IsSerializationFailure and retry the whole
// transaction.
func (c *Client) WithTxOptions(
	ctx context.Context,
	opts *sql.TxOptions,
	fn func(*sql.Tx) error,
) error {
	if fn == nil {
		return fmt.Errorf("%w: 事务回调不能为 nil", ErrInvalidArgument)
	}

	tx, err := c.db.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("开启事务失败（db=%s）: %w", c.database, err)
	}

	// Roll back on panic too, otherwise this transaction keeps holding the connection.
	committed := false
	defer func() {
		if committed {
			return
		}
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p) // re-raise as is, do not swallow
		}
	}()

	if err := fn(tx); err != nil {
		// The rollback itself can also fail (connection broken). Then both errors must be
		// kept: reporting only the rollback failure would hide the real business error, and
		// what you see while debugging is useless info like "rollback failed".
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return fmt.Errorf("事务回滚失败（原始错误: %w）: %v", err, rbErr)
		}
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("事务提交失败（db=%s）: %w", c.database, err)
	}
	committed = true
	return nil
}
