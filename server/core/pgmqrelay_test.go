package core

import (
	"context"
	"testing"
	"time"

	"github.com/chyroc/ama/server/shared/infra/pgmq"
)

type pgmqRelayTestQueue struct {
	topic   string
	payload []byte
	ttl     time.Duration
}

func (q *pgmqRelayTestQueue) Publish(_ context.Context, topic string, payload []byte, ttl time.Duration) error {
	q.topic, q.payload, q.ttl = topic, payload, ttl
	return nil
}
func (q *pgmqRelayTestQueue) Consume(
	ctx context.Context, topic string, fn func(context.Context, pgmq.Message) error,
) error {
	return fn(ctx, pgmq.Message{Topic: topic, Payload: []byte("payload")})
}

func TestPGMQWorkerWakeupsUsesNonNilEmptyPayload(t *testing.T) {
	queue := &pgmqRelayTestQueue{}
	wakeups := PGMQWorkerWakeups{Queue: queue}
	if err := wakeups.Wake(context.Background(), "env_example"); err != nil {
		t.Fatal(err)
	}
	if queue.topic != WorkerWakeTopic("env_example") || queue.ttl != WorkerWakeTTL || queue.payload == nil || len(queue.payload) != 0 {
		t.Fatalf("wakeup = topic %q payload %#v ttl %s", queue.topic, queue.payload, queue.ttl)
	}
	called := false
	if err := wakeups.WatchWakeups(context.Background(), "env_example", func(context.Context) error {
		called = true
		return nil
	}); err != nil || !called {
		t.Fatalf("watch did not reach consumer: called=%v err=%v", called, err)
	}
}

func TestPGMQTerminalRelayTranslatesMessage(t *testing.T) {
	queue := &pgmqRelayTestQueue{}
	relay := PGMQTerminalRelay{Queue: queue}
	if err := relay.Consume(context.Background(), "terminal", func(_ context.Context, message RelayMessage) error {
		if string(message.Payload) != "payload" {
			t.Fatalf("payload = %q", message.Payload)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
