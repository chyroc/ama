// This file does only one thing: make it easy for callers to classify errors returned by the
// database.
//
// Errors come in two layers, and they are checked in completely different ways:
//
//  1. **PostgreSQL business errors**: the connection worked and the SQL was sent, but the
//     server refused to run it. These are *pgconn.PgError with a SQLSTATE code (unique key
//     conflict, deadlock, table does not exist, ...).
//     Check them by SQLState, do not match the error text: the text changes with the
//     PostgreSQL version and locale, while SQLSTATE is fixed by the standard.
//
//  2. **Connection-layer errors**: the connection never worked, or it broke halfway.
//     Primary/standby failover, instance restarts and network blips all end up here, and some
//     of them recover on retry. See IsRetryable.
//
// The driver goes through database/sql, so "no rows" is sql.ErrNoRows, not pgx.ErrNoRows.
// Use IsNoRows; do not write errors.Is(err, pgx.ErrNoRows) yourself, because that is never true.

package pg

import (
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// Errors produced by this package itself (failures before any request is sent).
var (
	// ErrInvalidConfig means the config is invalid, for example Host / Database / User is
	// empty, or a DSN key name is misspelled.
	ErrInvalidConfig = errors.New("pg: 配置不合法")
	// ErrInvalidArgument means a call argument is invalid, for example empty SQL or a nil
	// callback.
	ErrInvalidArgument = errors.New("pg: 参数不合法")
)

// PgError is the structured error returned by PostgreSQL, the same as pgx's *pgconn.PgError.
// It is kept as an alias instead of a new type so that errors.As also works with code that
// uses pgx directly.
type PgError = pgconn.PgError

// SQLSTATE codes. Taken from Appendix A of the official PostgreSQL docs; only the ones this
// package checks are listed.
const (
	// SQLStateNotNullViolation is a not-null constraint violation.
	SQLStateNotNullViolation = "23502"
	// SQLStateForeignKeyViolation is a foreign key constraint violation.
	SQLStateForeignKeyViolation = "23503"
	// SQLStateUniqueViolation is a unique constraint violation. The most used one: use it to
	// tell "this row already exists".
	SQLStateUniqueViolation = "23505"
	// SQLStateCheckViolation is a CHECK constraint violation.
	SQLStateCheckViolation = "23514"
	// SQLStateSerializationFailure is a serialization failure. Just retry.
	SQLStateSerializationFailure = "40001"
	// SQLStateDeadlockDetected means a deadlock was detected. Just retry.
	SQLStateDeadlockDetected = "40P01"
	// SQLStateUndefinedTable means the table does not exist. Usually the table creation
	// script did not run.
	SQLStateUndefinedTable = "42P01"
	// SQLStateInsufficientPrivilege means insufficient privilege.
	SQLStateInsufficientPrivilege = "42501"
	// SQLStateAdminShutdown means the server closed the connection itself (admin shutdown).
	// Seen during instance restarts and primary/standby failover.
	SQLStateAdminShutdown = "57P01"
	// SQLStateCannotConnectNow means the server is still starting and does not accept
	// connections yet. Seen during cold start.
	SQLStateCannotConnectNow = "57P03"
	// SQLStateTooManyConnections means the connection limit was exceeded.
	SQLStateTooManyConnections = "53300"
)

// AsPgError tries to turn err back into a structured PostgreSQL error.
//
//	if pgErr, ok := pg.AsPgError(err); ok {
//	    log.Printf("PG error code=%s constraint=%s", pgErr.Code, pgErr.ConstraintName)
//	}
func AsPgError(err error) (*PgError, bool) {
	var pgErr *PgError
	if errors.As(err, &pgErr) {
		return pgErr, true
	}
	return nil, false
}

// SQLState returns the error's SQLSTATE code, or an empty string if it is not a structured
// PostgreSQL error.
func SQLState(err error) string {
	if pgErr, ok := AsPgError(err); ok {
		return pgErr.Code
	}
	return ""
}

// ConstraintName returns the name of the violated constraint. Use it with IsUniqueViolation
// to tell which unique key was hit.
func ConstraintName(err error) string {
	if pgErr, ok := AsPgError(err); ok {
		return pgErr.ConstraintName
	}
	return ""
}

// IsNoRows reports whether the error means "no rows".
//
// Note that it is sql.ErrNoRows: this package goes through database/sql, not bare pgx, so
// pgx.ErrNoRows would not match.
func IsNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}

// IsUniqueViolation reports whether the error is a unique constraint violation (23505).
//
// The typical use is "do not query before inserting; just insert, and treat a conflict as
// already exists". It saves one round trip compared to SELECT then INSERT, and has no race.
func IsUniqueViolation(err error) bool {
	return SQLState(err) == SQLStateUniqueViolation
}

// IsForeignKeyViolation reports whether the error is a foreign key constraint violation (23503).
func IsForeignKeyViolation(err error) bool {
	return SQLState(err) == SQLStateForeignKeyViolation
}

// IsNotNullViolation reports whether the error is a not-null constraint violation (23502).
func IsNotNullViolation(err error) bool {
	return SQLState(err) == SQLStateNotNullViolation
}

// IsCheckViolation reports whether the error is a CHECK constraint violation (23514).
func IsCheckViolation(err error) bool {
	return SQLState(err) == SQLStateCheckViolation
}

// IsDeadlock reports whether the error is a deadlock (40P01).
func IsDeadlock(err error) bool {
	return SQLState(err) == SQLStateDeadlockDetected
}

// IsSerializationFailure reports whether the error is a serialization failure (40001).
func IsSerializationFailure(err error) bool {
	return SQLState(err) == SQLStateSerializationFailure
}

// IsUndefinedTable reports whether the error means the table does not exist (42P01).
//
// When you see this, almost always the table creation script did not run, or you connected
// to the wrong database.
func IsUndefinedTable(err error) bool {
	return SQLState(err) == SQLStateUndefinedTable
}

// IsPermissionDenied reports whether the error is insufficient privilege (42501).
//
// Managed PG usually does not give you a superuser, and account permissions must be set up
// separately. So this error is often not a SQL mistake: the account was never granted access
// to the target database/table. Make the account the database Owner when creating it.
func IsPermissionDenied(err error) bool {
	return SQLState(err) == SQLStateInsufficientPrivilege
}

// IsTooManyConnections reports whether the connection limit was exceeded (53300).
//
// When you hit it, lower StdConfig.MaxOpenConns. The instance limit is max_connections
// (default 100; managed instances set it by instance size). Several service instances sharing
// one database can easily fill it up together.
func IsTooManyConnections(err error) bool {
	return SQLState(err) == SQLStateTooManyConnections
}

// IsRetryable reports whether "retrying once makes sense" for this error.
//
// It covers three kinds:
//
//   - Connection layer: cannot connect, connection broke halfway, timeout. Primary/standby
//     failover and instance restarts end up here.
//   - The server clearly says "not now": 57P03 (still starting), 57P01 (admin shutdown).
//   - Concurrency conflicts: 40001 / 40P01. Retrying is the standard way to handle these two.
//
// **Note that two "retrying will not help" cases look the same as retryable ones**: when the
// allowlist does not let you in, every TCP connection times out (not refused), and when the
// instance is stopped you also cannot connect. Retrying until dawn will not help in either
// case; go check the console.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	switch SQLState(err) {
	case SQLStateSerializationFailure, SQLStateDeadlockDetected,
		SQLStateCannotConnectNow, SQLStateAdminShutdown:
		return true
	}
	// pgx marks errors where "the request was surely not executed". Retrying those is safe.
	if pgconn.SafeToRetry(err) || pgconn.Timeout(err) {
		return true
	}
	// Cannot connect (Consul resolution failure, TCP refused and TLS handshake failure all
	// land here).
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return true
	}
	// The connection is no longer usable. This is what you get when the pool lends out a dead
	// connection.
	return errors.Is(err, sql.ErrConnDone) || errors.Is(err, driverBadConn)
}
