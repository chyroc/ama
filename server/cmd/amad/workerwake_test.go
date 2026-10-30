// Tests for the worker wake-up shim.
//
// Only one test, but it guards against a mistake that **only happens in production and unit tests naturally cannot see**.
package main

import (
	"context"
	"testing"
	"time"

	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/shared/infra/pgmq"
)

// capturingQueue records the arguments of Publish.
type capturingQueue struct {
	topic   string
	payload []byte
	ttl     time.Duration
	called  bool
}

func (c *capturingQueue) Publish(_ context.Context, topic string, payload []byte, ttl time.Duration) error {
	c.called = true
	c.topic, c.payload, c.ttl = topic, payload, ttl
	return nil
}

func (c *capturingQueue) Consume(ctx context.Context, _ string, _ func(context.Context, pgmq.Message) error) error {
	<-ctx.Done()
	return nil
}

// TestWorkerWakePayloadIsNotNil: the wake-up payload **must not be nil**.
//
// # This is a real production bug found by testing; do not change it back to nil
//
// `mq_messages.payload` is `bytea **not null**` (see repository/schema/mq_messages.sql).
// The wake-up signal itself carries no information (the topic already has envID; receiving it means "time to wake up"), so
// the first version just passed `nil`. That looks perfectly reasonable in Go, but pgx treats a nil slice as SQL NULL,
// so every single wake-up fails with:
//
//	ERROR: null value in column "payload" of relation "mq_messages"
//	violates not-null constraint (SQLSTATE 23502)
//
// # Why unit tests miss this and production can hardly find it
//
// Missed: the six wake-up cases in core use an in-memory fake that never touches the database, **all green**.
// Hard to find: wake-up is degrade-open (a failed send only logs one Warn and never affects sending messages),
// so in production it shows not as an error but as "wake-up does not work at all, responses get slower",
// and after the polling rate is lowered it is slow anyway, so the two cannot be told apart.
//
// Use an empty slice `[]byte{}` instead of nil: it goes into the database as a **zero-length bytea**, which satisfies not null.
//
// ⚠️ This case does not need a real database (then CI would skip it, which is the same as not having it). It watches exactly
// "is the value passed to the queue nil", which is the layer where it went wrong.
func TestWorkerWakePayloadIsNotNil(t *testing.T) {
	q := &capturingQueue{}
	w := &workerWakeups{q: q}

	if err := w.Wake(context.Background(), "env_abc"); err != nil {
		t.Fatalf("Wake 失败: %v", err)
	}
	if !q.called {
		t.Fatal("Wake 没有真的调 Publish")
	}
	if q.payload == nil {
		t.Fatal("唤醒的 payload 是 nil——pgx 会把它当 SQL NULL，\n" +
			"而 mq_messages.payload 是 bytea **not null**，于是每次唤醒都报\n" +
			"`null value in column \"payload\" ... violates not-null constraint`。\n" +
			"唤醒是 degrade-open 的（失败只打 Warn），所以线上表现只是「响应变慢」，\n" +
			"不指向任何东西。用 []byte{} 而不是 nil。")
	}
}

// TestWorkerWakeTopicAndTTLComeFromCore: topic and ttl must come from the same source as core.
//
// Writing them separately on each side leads to "the wake-up was sent but nobody received it": the sender uses topic A,
// the subscriber listens on topic B, zero errors, only slower responses. So this asserts they **are**
// the two values exported by core, not copied literals.
func TestWorkerWakeTopicAndTTLComeFromCore(t *testing.T) {
	q := &capturingQueue{}
	w := &workerWakeups{q: q}
	if err := w.Wake(context.Background(), "env_abc"); err != nil {
		t.Fatalf("Wake 失败: %v", err)
	}
	if want := core.WorkerWakeTopic("env_abc"); q.topic != want {
		t.Errorf("topic = %q，要 %q——发送端与订阅端的 topic 不一致时，"+
			"唤醒发出去了但没人收到，且零报错", q.topic, want)
	}
	if q.ttl != core.WorkerWakeTTL {
		t.Errorf("ttl = %v，要 %v", q.ttl, core.WorkerWakeTTL)
	}
	if q.ttl <= 0 {
		t.Error("ttl 必须为正，pgmq.Publish 会直接拒绝非正的 ttl")
	}
}
