package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// NotificationConnection is the narrow pgx behavior needed for a LISTEN loop. The
// downstream owns its DSN, authentication and pool; this package owns reconnect and shutdown.
type NotificationConnection interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	WaitForNotification(context.Context) (*pgconn.Notification, error)
	Close(context.Context) error
}

// NotificationListenerConfig wires one fixed PG NOTIFY channel. Connect must return a
// dedicated connection for each attempt; a pooled LISTEN connection must not change identity
// between waits. Wake is process-local acceleration only, never the correctness mechanism.
type NotificationListenerConfig struct {
	Channel string
	Connect func(context.Context) (NotificationConnection, error)
	Wake    func(string)
	Logger  *slog.Logger
	// RetryDelay defaults to two seconds. Tests may use a shorter value.
	RetryDelay time.Duration
}

// RunNotificationListener subscribes, filters the fixed channel and reconnects after a
// failure. Notifications can be lost during reconnects, so callers must retain a timer or
// long-poll fallback; this loop only reduces latency and is safe to cancel at shutdown.
func RunNotificationListener(ctx context.Context, cfg NotificationListenerConfig) error {
	if cfg.Channel == "" || cfg.Connect == nil {
		return fmt.Errorf("app: notification listener requires a channel and connection factory")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	delay := cfg.RetryDelay
	if delay <= 0 {
		delay = 2 * time.Second
	}
	for ctx.Err() == nil {
		err := listenNotifications(ctx, cfg, logger)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			logger.Warn("唤醒 LISTEN 连接断开，稍后重连", slog.Any("err", err), slog.Duration("retry_delay", delay))
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}

func listenNotifications(ctx context.Context, cfg NotificationListenerConfig, logger *slog.Logger) error {
	conn, err := cfg.Connect(ctx)
	if err != nil {
		return err
	}
	if conn == nil {
		return fmt.Errorf("app: notification connection factory returned nil")
	}
	defer conn.Close(context.Background()) //nolint:errcheck // the listen error is more actionable
	if _, err := conn.Exec(ctx, "listen "+cfg.Channel); err != nil {
		return err
	}
	logger.Info("已订阅唤醒通道", slog.String("channel", cfg.Channel))
	for ctx.Err() == nil {
		notification, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if notification == nil || notification.Channel != cfg.Channel || cfg.Wake == nil {
			continue
		}
		cfg.Wake(notification.Payload)
	}
	return ctx.Err()
}
