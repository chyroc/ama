// Cross-instance relay for terminal keystrokes: connects shared/infra/pgmq as core.TerminalRelay.
//
// # Why this shim is needed
//
// `core` declares what it needs (TerminalRelay), `shared/infra/pgmq` provides a generic queue,
// and the two do not know each other. **The direction is reversed**: the business package declares the interface, and the wiring layer plugs in the implementation.
// Exactly the same technique as larkLeases / schedulerLeases; see those two for the reasons.
//
// One more thing that must be done at this layer: `shared/infra/pgmq` deliberately does not import `shared/infra/pg`
// (nothing under infra/ in the repo imports it; that is a strictly kept invariant), so the adapter "turn *pg.Client
// into pgmq.DB" can only live here.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"

	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/shared/infra/pg"
	"github.com/chyroc/ama/server/shared/infra/pgmq"
	"github.com/jackc/pgx/v5"
)

// newTerminalRelay builds the terminal relay, **and also returns the worker wake-up** (both share the same queue).
//
// ⚠️ **It returns the concrete type, not core.TerminalRelay**: if it returned the interface type,
// "returning a nil *terminalRelay" would become a **non-nil interface value**,
// and `if s.termRelay != nil` on the core side would always be true, then crash with a nil pointer on the first keystroke.
// This trap was called out at schedulerLeases; we do not repeat it here.
//
// Build both shims at once instead of letting the caller build twice: pgmq.Queue itself is a stateless thin layer
// (the dedicated LISTEN connections are dialed by Consume per topic), so one shared instance is enough;
// building two only adds one more chance of config drift.
func newTerminalRelay(
	client *pg.Client, stdCfg pg.StdConfig, log *slog.Logger,
) (*terminalRelay, *workerWakeups, error) {
	dsn, err := stdCfg.DSN()
	if err != nil {
		return nil, nil, fmt.Errorf("拼监听连接的 DSN 失败: %w", err)
	}

	q, err := pgmq.New(pgmq.Config{
		DB: pgmqDB{client},
		// LISTEN needs a **dedicated** connection: database/sql is a pool, and after the pool swaps out the connection, notifications
		// are never received again, with no error (it shows as "messages wait for the next poll").
		// So here we give a dialer, and pgmq manages reconnects itself.
		Dial: func(ctx context.Context) (pgmq.Listener, error) {
			conn, cerr := pgx.Connect(ctx, dsn)
			if cerr != nil {
				return nil, cerr
			}
			return pgxListener{conn}, nil
		},
		Owner: hostnameOr("unknown"),
		Log:   log,
	})
	if err != nil {
		return nil, nil, err
	}
	return &terminalRelay{Queue: q}, &workerWakeups{q: q}, nil
}

// Terminal and worker wake adapters are the public core PGMQ implementations.
// They share one persistent queue; wake payloads stay non-nil empty bytes.

type terminalRelay = core.PGMQTerminalRelay
type workerWakeups struct{ q pgmq.Queue }

func (w *workerWakeups) Wake(ctx context.Context, envID string) error {
	return core.PGMQWorkerWakeups{Queue: w.q}.Wake(ctx, envID)
}
func (w *workerWakeups) WatchWakeups(
	ctx context.Context, envID string, fn func(context.Context) error,
) error {
	return core.PGMQWorkerWakeups{Queue: w.q}.WatchWakeups(ctx, envID, fn)
}

var _ core.WorkerWakeups = (*workerWakeups)(nil)

// pgmqDB adapts *pg.Client to pgmq.DB.
type pgmqDB struct{ c *pg.Client }

func (d pgmqDB) Exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return d.c.Exec(ctx, q, args...)
}
func (d pgmqDB) Query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return d.c.Query(ctx, q, args...)
}
func (d pgmqDB) QueryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return d.c.QueryRow(ctx, q, args...)
}

// pgxListener adapts a raw pgx connection to pgmq.Listener.
type pgxListener struct{ conn *pgx.Conn }

// Listen starts listening.
//
// ⚠️ The channel name **can only be spliced into the SQL, not passed as a parameter** (`listen $1` is invalid). This looks like an injection hole,
// but it is not: pgmq guarantees the channel name is `ama_mq_` + hex, with characters only from [0-9a-f_].
// Remember this dependency when changing channelFor over there.
func (l pgxListener) Listen(ctx context.Context, channel string) error {
	_, err := l.conn.Exec(ctx, "listen "+channel)
	return err
}

func (l pgxListener) WaitForNotification(ctx context.Context) error {
	_, err := l.conn.WaitForNotification(ctx)
	return err
}

func (l pgxListener) Close(ctx context.Context) error { return l.conn.Close(ctx) }

func hostnameOr(def string) string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return def
}
