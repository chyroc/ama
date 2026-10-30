// LISTEN adapter for hostWake: one dedicated PG connection subscribes to the fixed channel ama_host_wake;
// the notification payload is host_id, and on receipt it fans out to this instance's heartbeat waiters and exec waiters.
//
// Why we use pgx LISTEN ourselves instead of pgmq: pgmq's Queue is a "message row + empty NOTIFY signal"
// model (messages go into mq_messages and are consumed competitively by topic). Host wake-up does not need to store messages
// or compete: every amad instance must be woken (heartbeat/exec waits happen on any instance),
// so it is a broadcast, not a queue. PG's native NOTIFY delivers one copy to each LISTEN connection, which fits.
//
// In common with pgmq: both need a **dedicated connection** (the database/sql pool would swap out the connection that is LISTENing),
// reconnect automatically on disconnect, and do one catch-up sweep right after reconnecting (NOTIFY is not persisted; notifications during
// the disconnect window are lost; host.Service also has a 1s ticker and long polling as fallback, see the hostwake.go file header).
package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"log/slog"

	"github.com/chyroc/ama/server/core"
)

// hostWakeChannel must match the channel name used by pg_notify in the storage layer exactly
// (repository/dao/coredao/store_pg_host.go).
const hostWakeChannel = "ama_host_wake"

// runHostWakeListener runs the LISTEN loop, blocking until ctx ends.
//
// pool is used to take a dedicated connection (not returned to the pool). Each connection loop: LISTEN → WaitForNotification
// → Wake. After the connection drops / ctx is cancelled it reconnects with a simple backoff; pgx's WaitForNotification
// returns an error by itself when the connection is dead.
func runHostWakeListener(ctx context.Context, pool *pgxpool.Pool, wakes core.HostWakes, log *slog.Logger) {
	for ctx.Err() == nil {
		if err := listenOnce(ctx, pool, wakes, log); err != nil && ctx.Err() == nil {
			log.Warn("宿主机唤醒 LISTEN 连接断开，2 秒后重连", slog.Any("err", err))
			t := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}
}

func listenOnce(ctx context.Context, pool *pgxpool.Pool, wakes core.HostWakes, log *slog.Logger) error {
	// Acquire a connection and do not release it: LISTEN is connection-level session state and must be exclusive.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "listen "+hostWakeChannel); err != nil {
		return err
	}
	log.Info("已订阅宿主机唤醒通道", slog.String("channel", hostWakeChannel))

	for ctx.Err() == nil {
		notification, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if notification == nil || notification.Channel != hostWakeChannel {
			continue
		}
		// The payload is host_id. Broadcast to all goroutines on this instance waiting for this machine.
		wakes.Wake(notification.Payload)
	}
	return ctx.Err()
}
