package core

import (
	"context"
	"time"

	"github.com/chyroc/ama/server/shared/infra/pgmq"
)

// PGMQTerminalRelay adapts the persistent queue to TerminalRelay. The queue handles
// cross-instance delivery and redelivery; this adapter only translates message shape.
type PGMQTerminalRelay struct{ Queue pgmq.Queue }

func (r PGMQTerminalRelay) Publish(ctx context.Context, topic string, payload []byte, ttl time.Duration) error {
	return r.Queue.Publish(ctx, topic, payload, ttl)
}
func (r PGMQTerminalRelay) Consume(
	ctx context.Context, topic string, fn func(context.Context, RelayMessage) error,
) error {
	return r.Queue.Consume(ctx, topic, func(ctx context.Context, message pgmq.Message) error {
		return fn(ctx, RelayMessage{Payload: message.Payload})
	})
}

var _ TerminalRelay = PGMQTerminalRelay{}

// PGMQWorkerWakeups shares the terminal relay's queue for worker wake signals. A wakeup
// payload is []byte{} rather than nil because the database payload column is NOT NULL.
type PGMQWorkerWakeups struct{ Queue pgmq.Queue }

func (w PGMQWorkerWakeups) Wake(ctx context.Context, envID string) error {
	return w.Queue.Publish(ctx, WorkerWakeTopic(envID), []byte{}, WorkerWakeTTL)
}
func (w PGMQWorkerWakeups) WatchWakeups(
	ctx context.Context, envID string, fn func(context.Context) error,
) error {
	return w.Queue.Consume(ctx, WorkerWakeTopic(envID), func(ctx context.Context, _ pgmq.Message) error {
		return fn(ctx)
	})
}

var _ WorkerWakeups = PGMQWorkerWakeups{}
