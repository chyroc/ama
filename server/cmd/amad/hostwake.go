package main

import (
	"context"
	"log/slog"

	"github.com/chyroc/ama/server/app"
	"github.com/chyroc/ama/server/core"
	"github.com/jackc/pgx/v5/pgxpool"
)

const hostWakeChannel = "ama_host_wake"

func runHostWakeListener(ctx context.Context, pool *pgxpool.Pool, wakes core.HostWakes, log *slog.Logger) {
	_ = app.RunNotificationListener(ctx, app.NotificationListenerConfig{
		Channel: hostWakeChannel, Connect: func(ctx context.Context) (app.NotificationConnection, error) {
			return acquireHostWakeConnection(ctx, pool)
		}, Wake: wakes.Wake, Logger: log,
	})
}

// listenOnce keeps the existing call site. The common loop closes the connection on return.
func listenOnce(ctx context.Context, pool *pgxpool.Pool, wakes core.HostWakes, log *slog.Logger) error {
	return app.RunNotificationListener(ctx, app.NotificationListenerConfig{
		Channel: hostWakeChannel, Connect: func(ctx context.Context) (app.NotificationConnection, error) {
			return acquireHostWakeConnection(ctx, pool)
		}, Wake: wakes.Wake, Logger: log,
	})
}

func acquireHostWakeConnection(ctx context.Context, pool *pgxpool.Pool) (app.NotificationConnection, error) {
	acquired, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	// LISTEN is connection-local state. Hijack keeps it exclusive until the common
	// loop closes it; Release would let another query reuse the listening connection.
	return acquired.Hijack(), nil
}
