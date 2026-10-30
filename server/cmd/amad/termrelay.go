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
	"time"

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
	return &terminalRelay{q: q}, &workerWakeups{q: q}, nil
}

// terminalRelay adapts pgmq.Queue to core.TerminalRelay.
//
// It does only one real thing: translate pgmq.Message into core.RelayMessage. This translation looks redundant,
// but it is exactly the cost of "core does not depend on a specific queue": when switching to another implementation, this is what changes.
type terminalRelay struct{ q pgmq.Queue }

func (r *terminalRelay) Publish(ctx context.Context, topic string, payload []byte, ttl time.Duration) error {
	return r.q.Publish(ctx, topic, payload, ttl)
}

func (r *terminalRelay) Consume(
	ctx context.Context, topic string, fn func(context.Context, core.RelayMessage) error,
) error {
	return r.q.Consume(ctx, topic, func(c context.Context, m pgmq.Message) error {
		return fn(c, core.RelayMessage{Payload: m.Payload})
	})
}

// Compile-time check that this shim really satisfies the interface on the core side.
// Without it the error would appear at the wiring site, pointing to "type mismatch" instead of here.
var _ core.TerminalRelay = (*terminalRelay)(nil)

// workerWakeups adapts the same pgmq.Queue to core.WorkerWakeups.
//
// # Why reuse the terminal relay's queue
//
// Both need the same thing: **deliver a signal to "another pod"**. The terminal relay delivers "to the pod holding the PTY",
// and worker wake-up delivers "to the pod that holds the poll right for this env".
// Underneath both are PG LISTEN/NOTIFY (measured p50 0.08ms); only topic and payload differ.
//
// Reuse instead of starting a new one: pgmq's Consume is isolated by topic, so the two never clash;
// and each extra queue adds one more set of "connect, reconnect, clean up" to maintain, with exactly the same failure modes.
//
// ⚠️ **Redis pub/sub cannot replace it**: on a proxy-based Redis cluster, pub/sub does not cross replicas,
// the proxy layer does not route subscriptions, and pods are spread across several data centers. It **does work**, and exactly because it works
// it is dangerous: it loses messages occasionally, and only across data centers. For the full argument see repository/schema/mq_messages.sql.
type workerWakeups struct{ q pgmq.Queue }

// Wake: see core.WorkerWakeups.
//
// ⚠️ The payload is an **empty slice `[]byte{}`, not nil**. The signal itself carries no information
// (the topic already has envID; receiving it means "this environment should wake up"), but
// `mq_messages.payload` is `bytea **not null**`: passing nil is treated by pgx as SQL NULL,
// and the whole insert fails with `null value in column "payload" violates not-null constraint`
// (SQLSTATE 23502).
//
// **Unit tests cannot find this trap**: the fake on the core side never touches the database,
// and all six wake-up cases are green; in production every message send fails once and wake-up **does not work at all**,
// and since it is degrade-open (a failure only logs one Warn), the only symptom is "responses got slower",
// pointing to nothing. It was only found by testing for real; see bytea not null on the shared/infra/pgmq side.
//
// The short ttl is deliberate, same as the terminal keystroke one: a backlog of old wake-ups flooding in after a pod rebuild only triggers a string of
// useless polls, which is exactly what this change is meant to save.
//
// ⚠️ topic and ttl are both taken from core, **not written again here**: if the two sides differ, the result is
// "the wake-up was sent but nobody received it": zero errors, only slower responses. That kind of drift is a trap this repo
// has recorded again and again, so we make it impossible at compile time.
func (w *workerWakeups) Wake(ctx context.Context, envID string) error {
	return w.q.Publish(ctx, core.WorkerWakeTopic(envID), []byte{}, core.WorkerWakeTTL)
}

// WatchWakeups: see core.WorkerWakeups.
func (w *workerWakeups) WatchWakeups(
	ctx context.Context, envID string, fn func(context.Context) error,
) error {
	return w.q.Consume(ctx, core.WorkerWakeTopic(envID), func(c context.Context, _ pgmq.Message) error {
		return fn(c)
	})
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
