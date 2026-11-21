package pgmq

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// DialPGX opens one dedicated LISTEN connection per call. The DSN and authentication are
// supplied by the process; this package never reuses a pooled connection for notifications.
func DialPGX(dsn string) Dialer {
	return func(ctx context.Context) (Listener, error) {
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return nil, err
		}
		return pgxListener{conn: conn}, nil
	}
}

type pgxListener struct{ conn *pgx.Conn }

func (l pgxListener) Listen(ctx context.Context, channel string) error {
	// LISTEN cannot bind its channel as a query parameter. channelFor restricts names to
	// ama_mq_ plus hex/underscore, so no caller-provided SQL fragment reaches this statement.
	_, err := l.conn.Exec(ctx, "listen "+channel)
	return err
}
func (l pgxListener) WaitForNotification(ctx context.Context) error {
	_, err := l.conn.WaitForNotification(ctx)
	return err
}
func (l pgxListener) Close(ctx context.Context) error { return l.conn.Close(ctx) }
