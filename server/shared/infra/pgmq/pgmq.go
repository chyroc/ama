// Package pgmq is a message queue built on PostgreSQL: **low latency, and nothing is lost**.
//
// It has only two actions, and callers do not need to know LISTEN/NOTIFY exists:
//
//	q.Publish(ctx, topic, payload)        // success once it is stored
//	q.Consume(ctx, topic, fn)             // consumes until ctx ends
//
// # Why this package exists
//
// The direct cause was a class of webshell terminal failures: the terminal's downstream SSE
// and the upstream "every keystroke" are two separate HTTP requests, and the load balancer
// sends each of them **independently** to a random pod, while the PTY's WebSocket is held
// only by the pod that opened the SSE. Landing on any other pod returns 404. In production,
// 14 of 24 requests in one session got 404.
//
// The general form of that problem is "**what instance A receives must be handled by
// instance B**", and it comes up again and again in this repo (the "long connections / SSE
// subscribers" row in the table at the top of CLAUDE.md). So this is built as a general
// queue, instead of adding a terminal-only table to core.
//
// # Why PG, and not Redis, not the pgmq extension, and not RMQ
//
// ⚠️ All three were checked; do not go through it again (details in the file header of
// repository/schema/mq_messages.sql):
//
//   - **Managed Redis cannot always relay pub/sub**: with a proxy-style cluster, pub/sub often
//     does not cross replicas, and when instances span several availability zones, messages
//     are sometimes lost. The client library **does** have Subscribe and **does work**, which
//     makes it more dangerous: it loses messages only sometimes, and only across replicas.
//   - **The pgmq extension cannot and should not be installed**: managed PG usually does not
//     have it and the database user is not a superuser; the local docker PG does not have
//     the extension at all, so using it would break local development and offline tests.
//   - **General message queues (like RocketMQ) do not match the semantics**. Three hard
//     reasons:
//     ① **They cannot deliver to a chosen instance**: the queue→consumer-instance mapping is
//     decided by rebalance and drifts. PartitionKey only guarantees "same key goes to the
//     same queue", not "goes to the same process". Delivering to the wrong instance is
//     exactly the shape of the terminal keystroke 404.
//     ② **at-least-once**, and a duplicated keystroke = the user visibly gets an extra
//     character; broadcast mode is usually worse, with only best-effort push.
//     ③ **A topic is usually a resource you must request in advance**, while terminal topics
//     are `term:<id>`, one per terminal, created on the fly.
//     These three are semantic problems: even with zero latency it still could not be used.
//
// # Latency: push first, polling as a fallback
//
// Measured locally (cmd/probe-term-relay, 60 runs per setting):
//
//	polling @20ms          p50 10.9ms   p95 20.7ms
//	polling @200ms         p50 113ms    p95 200ms
//	LISTEN/NOTIFY (push)   p50 0.08ms   p95 0.18ms   ← two orders of magnitude faster
//
// So in steady state NOTIFY does the waking, and polling is only a fallback. **Both are
// needed**; see Consume for why.
//
// # Three measured platform facts (they shape the design here)
//
//  1. **NOTIFY is transactional**: after `begin; pg_notify(...); rollback;` the subscriber
//     gets nothing. So Publish puts the insert and the notify in one statement, and **the
//     classic race "the notification arrives before the message is stored" cannot happen**.
//  2. **No lost wakeups**: LISTEN is a **per-connection registration**. Once LISTEN has run,
//     notifications that arrive later are queued on that connection. A test that sent the
//     notification 30ms before WaitForNotification got it immediately 30/30 times. So the
//     "check the table first → then wait for a notification" loop in Consume is safe.
//  3. **Two hard limits**: NOTIFY payload **< 8000 bytes**, channel name **≤ 63 characters**
//     (both measured; going over gives `payload string too long` / `channel name too long`).
//     So here the channel name is `ama_mq_` + a short hash of the topic (topics usually carry
//     business ids, so their length is not under control), and **payloads always go through
//     the table; NOTIFY is only a "wake up" signal**. Doing "small messages by notification,
//     large messages by table" would split delivery into two paths, one of which would never
//     have tests.
//
// # ⚠️ LISTEN must own a connection exclusively
//
// `database/sql` is a **connection pool**, while LISTEN is bound to one specific connection.
// After the pool swaps that connection out, notifications never arrive again, **with no error
// at all**. The symptom is "messages wait until the next poll to be handled", a performance
// problem that only shows up under load. So this package needs two things: a pooled DB (to
// read and write the table) and a Dialer that can open an **exclusive connection** (only for
// LISTEN).
//
// # Layering: this package does not import shared/infra/pg
//
// **No package** under `infra/` in the whole repo imports `core` or `shared/infra/pg`; this
// rule is strictly kept. So this package only **declares locally** the few actions it needs
// (DB / Listener / Dialer), and `cmd/amad` writes a shim to plug `*pg.Client` in. It is
// exactly the same technique as `LeaseStore` in `shared/infra/lark`, with the dependency
// pointing the other way: infra declares, the wiring layer injects.
package pgmq

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// DB is the read/write capability this package needs, injected by the caller (usually
// *pg.Client).
//
// **Only these three are declared on purpose**: this package needs no transactions, and
// should not get the power to close the connection pool.
type DB interface {
	Exec(ctx context.Context, query string, args ...any) (sql.Result, error)
	Query(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRow(ctx context.Context, query string, args ...any) *sql.Row
}

// Listener is an **exclusive** connection, used only for LISTEN and waiting for notifications.
//
// Why not reuse DB: see "LISTEN must own a connection exclusively" in the package comment.
type Listener interface {
	// Listen starts listening on a channel.
	Listen(ctx context.Context, channel string) error
	// WaitForNotification blocks until a notification arrives, or ctx ends.
	WaitForNotification(ctx context.Context) error
	// Close closes this connection.
	Close(ctx context.Context) error
}

// Dialer opens a new exclusive connection. It is called again when reconnecting.
type Dialer func(ctx context.Context) (Listener, error)

// Message is one message delivered to a consumer.
type Message struct {
	// ID is the auto-increment sequence number in the queue, which is also the delivery order.
	// Only used for debugging and dedup.
	ID int64
	// Topic is the name of the queue it belongs to.
	Topic string
	// Payload is the bytes stored as is at publish time. This layer does not interpret them.
	Payload []byte
	// CreatedAt is the enqueue time (database clock, not any instance's wall clock).
	CreatedAt time.Time
}

// Queue is everything this package offers.
//
// It is an interface instead of an exposed struct: callers (like core) declare their
// dependency on it, and tests can plug in a fake without starting a database to run unit
// tests.
type Queue interface {
	// Publish puts a message into topic. A nil return means it is **already stored** and will
	// not be lost.
	//
	// ttl is how long this message is valid; after it expires it is no longer delivered.
	// **Time-sensitive messages must use a short ttl**: terminal keystrokes that pile up and
	// then flood into the container are harmful (the user sees characters typed minutes ago
	// suddenly get executed).
	Publish(ctx context.Context, topic string, payload []byte, ttl time.Duration) error

	// Consume keeps consuming topic until ctx ends; a normal end returns nil.
	//
	// Consuming only counts as successful when fn returns nil (the message is deleted). If it
	// returns an error, the message **stays in the queue** and is redelivered after the lease
	// expires, so fn must tolerate duplicate delivery.
	Consume(ctx context.Context, topic string, fn func(context.Context, Message) error) error
}

// Config holds the parameters for New.
type Config struct {
	// DB reads and writes the message table. Required.
	DB DB
	// Dial opens the exclusive connection used for LISTEN.
	//
	// **It may be nil**: then it falls back to pure polling (latency equals PollEvery), and
	// features are not affected. This fallback is on purpose: local/test setups should not
	// have to prepare two kinds of connections just to work.
	Dial Dialer
	// PollEvery is the fallback polling interval. Defaults to defaultPollEvery when empty.
	PollEvery time.Duration
	// ClaimFor is the claim lease duration. Defaults to defaultClaimFor when empty.
	ClaimFor time.Duration
	// Owner identifies this instance, only for debugging (stored in claimed_by). Defaults to
	// "unknown" when empty.
	Owner string
	// Log: no logging when empty.
	Log *slog.Logger
}

const (
	// defaultPollEvery is the fallback polling interval.
	//
	// It is 1 second rather than shorter: steady-state latency is set by NOTIFY (measured p50
	// 0.08ms), and polling only covers "the connection just reconnected and the notifications
	// from that gap are gone". A shorter interval only means useless database queries: empty
	// queries from N instances × M topics would hit the database all the time.
	defaultPollEvery = 1 * time.Second

	// defaultClaimFor is the claim lease duration.
	//
	// Between claiming and finishing, the instance may be rebuilt (deploy, scale-down, failed
	// health check); then nobody releases the lock, and **only lease expiry lets someone else
	// take over**. 30 seconds: far longer than normal processing time (milliseconds), but not
	// so long that one instance rebuild leaves messages stuck for too long.
	defaultClaimFor = 30 * time.Second
)

// queue is the implementation of Queue.
type queue struct {
	db        DB
	dial      Dialer
	pollEvery time.Duration
	claimFor  time.Duration
	owner     string
	log       *slog.Logger
}

// New builds a queue. It returns an error when DB is nil: that is a wiring error and should
// not silently fall back.
func New(cfg Config) (Queue, error) {
	if cfg.DB == nil {
		return nil, errors.New("pgmq: 必须提供 DB")
	}
	q := &queue{
		db:        cfg.DB,
		dial:      cfg.Dial,
		pollEvery: cfg.PollEvery,
		claimFor:  cfg.ClaimFor,
		owner:     cfg.Owner,
		log:       cfg.Log,
	}
	if q.pollEvery <= 0 {
		q.pollEvery = defaultPollEvery
	}
	if q.claimFor <= 0 {
		q.claimFor = defaultClaimFor
	}
	if q.owner == "" {
		q.owner = "unknown"
	}
	return q, nil
}

// Publish: see the Queue interface.
//
// insert and notify are in **one statement**, so either both succeed or neither happens.
// The race "the notification went out but the message is not stored yet" cannot happen
// (NOTIFY is transactional; measured).
func (q *queue) Publish(ctx context.Context, topic string, payload []byte, ttl time.Duration) error {
	if topic == "" {
		return errors.New("pgmq: topic 不能为空")
	}
	if ttl <= 0 {
		return errors.New("pgmq: ttl 必须为正（强时效消息积压后重放是有害的）")
	}
	// ⚠️ now() and expires_at are both computed by the **database**, not with Go time: wall
	// clocks differ between instances, and local time would make "expired" come out
	// differently on different instances.
	_, err := q.db.Exec(ctx, `
		with inserted as (
		    insert into mq_messages (topic, payload, created_at, expires_at)
		    values ($1, $2, now(), now() + make_interval(secs => $3))
		    returning topic
		)
		select pg_notify($4, '') from inserted`,
		topic, payload, ttl.Seconds(), channelFor(topic))
	if err != nil {
		return fmt.Errorf("pgmq: 发布失败: %w", err)
	}
	return nil
}

// Consume: see the Queue interface.
//
// The loop shape is "**drain the table first → then wait for a notification**", and the order
// cannot be reversed: if it waited first, messages already in the table before this run
// started would only be found when the next new message arrives.
//
// The waiting side has three wake-up sources; when any one arrives it goes back to draining:
//
//	NOTIFY        main source in steady state, measured p50 0.08ms
//	poll ticker   fallback, covers "notifications sent during a reconnect gap"
//	ctx ends      exit
func (q *queue) Consume(ctx context.Context, topic string, fn func(context.Context, Message) error) error {
	if topic == "" {
		return errors.New("pgmq: topic 不能为空")
	}
	if fn == nil {
		return errors.New("pgmq: 消费函数不能为空")
	}

	// When a notification comes, push into this. Capacity 1 with non-blocking writes, which
	// **merges** bursts of notifications: typing ten characters does not need ten wakeups,
	// since waking up drains the table anyway.
	woke := make(chan struct{}, 1)
	if q.dial != nil {
		go q.listenLoop(ctx, topic, woke)
	}

	ticker := time.NewTicker(q.pollEvery)
	defer ticker.Stop()

	for {
		// Drain first. One wakeup may cover several messages, so keep taking until empty.
		for {
			msg, ok, err := q.claim(ctx, topic)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				// A failed query is not a reason to stop (it may be a one-off blip); wait for
				// the next round.
				q.logWarn("pgmq: 取消息失败", "topic", topic, "err", err)
				break
			}
			if !ok {
				break
			}
			if err := fn(ctx, msg); err != nil {
				// Processing failed: **do not delete**; let it be redelivered after the lease
				// expires.
				q.logWarn("pgmq: 处理消息失败，将重投", "topic", topic, "id", msg.ID, "err", err)
				continue
			}
			if err := q.delete(context.WithoutCancel(ctx), msg.ID); err != nil {
				// Failing to delete only causes one redelivery, which is better than losing a
				// message. fn must tolerate duplicates anyway.
				q.logWarn("pgmq: 删消息失败，可能重投", "topic", topic, "id", msg.ID, "err", err)
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-woke:
		case <-ticker.C:
		}
	}
}

// listenLoop maintains the exclusive connection: reconnect when it drops, and nudge a drain
// right after reconnecting.
//
// ⚠️ **It must nudge once after reconnecting**: notifications sent while disconnected **are
// gone** (NOTIFY is not persisted and not resent). Without the nudge those messages would
// wait for the next poll to be found. That is exactly why the polling fallback exists; an
// active nudge here shrinks that window to the minimum.
func (q *queue) listenLoop(ctx context.Context, topic string, woke chan<- struct{}) {
	channel := channelFor(topic)
	for ctx.Err() == nil {
		conn, err := q.dial(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			q.logWarn("pgmq: 拨监听连接失败，稍后重试", "topic", topic, "err", err)
			sleep(ctx, q.pollEvery)
			continue
		}
		if err := conn.Listen(ctx, channel); err != nil {
			_ = conn.Close(context.WithoutCancel(ctx))
			if ctx.Err() != nil {
				return
			}
			q.logWarn("pgmq: LISTEN 失败，稍后重试", "topic", topic, "err", err)
			sleep(ctx, q.pollEvery)
			continue
		}

		// Just connected: nudge once to cover the notifications lost while disconnected.
		poke(woke)

		for ctx.Err() == nil {
			if err := conn.WaitForNotification(ctx); err != nil {
				break // connection broke or ctx ended; the outer loop reconnects
			}
			poke(woke)
		}
		_ = conn.Close(context.WithoutCancel(ctx))
	}
}

// claim atomically claims the earliest deliverable message on topic.
//
// ⚠️ The SQL shape is copied from pending_messages (coredao/store_pending.go): the CTE picks
// the row with `for update skip locked`, and **the business conditions are written again in
// the outer UPDATE**. Both reasons are fully reviewed in coredao/store_pg_schedule.go:188. In
// short:
//
//   - Under READ COMMITTED, the EvalPlanQual recheck **only looks at the outer WHERE**, and
//     the id set in `where id in (subquery)` is computed up front and always true, so two
//     instances would claim the same row at the same time;
//   - If the conditions are moved out, limit gets beaten by a Nested Loop Semi Join, so it must
//     stay in the CTE (CTEs in data-modifying statements are materialized).
func (q *queue) claim(ctx context.Context, topic string) (Message, bool, error) {
	var m Message
	err := q.db.QueryRow(ctx, `
		with due as (
		     select id from mq_messages
		      where topic = $1
		        and expires_at > now()
		        and (claimed_until is null or claimed_until < now())
		      order by id
		      limit 1
		      for update skip locked
		)
		update mq_messages
		   set claimed_by = $2, claimed_until = now() + make_interval(secs => $3)
		  from due
		 where mq_messages.id = due.id
		   -- Repeat the business conditions: the EPQ recheck looks here. Without it two instances claim at once.
		   and mq_messages.expires_at > now()
		   and (mq_messages.claimed_until is null or mq_messages.claimed_until < now())
		returning mq_messages.id, mq_messages.topic, mq_messages.payload, mq_messages.created_at`,
		topic, q.owner, q.claimFor.Seconds()).
		Scan(&m.ID, &m.Topic, &m.Payload, &m.CreatedAt)
	if err != nil {
		// ⚠️ Check sql.ErrNoRows, not pgx.ErrNoRows: the injected DB is wrapped on
		// database/sql (shared/infra/pg is), so the pgx one **never matches**. And "queue is
		// empty" is the most common path; checking the wrong one would report every round as
		// a query failure.
		if errors.Is(err, sql.ErrNoRows) {
			return Message{}, false, nil
		}
		return Message{}, false, fmt.Errorf("pgmq: 认领失败: %w", err)
	}
	return m, true, nil
}

// delete removes the message after it was consumed successfully.
func (q *queue) delete(ctx context.Context, id int64) error {
	if _, err := q.db.Exec(ctx, `delete from mq_messages where id = $1`, id); err != nil {
		return fmt.Errorf("pgmq: 删消息失败: %w", err)
	}
	return nil
}

// PurgeExpired deletes all expired messages and returns how many were deleted.
//
// It is exposed separately instead of being put into Consume: expired messages may belong to
// topics with **no consumer at all** (for example, the terminal connection dropped long ago),
// and nobody would ever come to clean those up. The caller hooks it into its own scheduled
// check, and the instance that wins the lock runs it once.
func PurgeExpired(ctx context.Context, db DB) (int64, error) {
	res, err := db.Exec(ctx, `delete from mq_messages where expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("pgmq: 清过期消息失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil // if the driver does not support it, fine; the delete itself succeeded
	}
	return n, nil
}

// channelFor maps a topic to a NOTIFY channel name.
//
// ⚠️ **The topic cannot be used as the channel name directly**: PG channel names are limited
// to 63 characters (going over gives `channel name too long`, measured), and topics usually
// carry business ids, so their length is not under control. We take the hex of the first 16
// bytes of sha256 (32 characters) + a fixed prefix, 39 in total, safely within the limit.
//
// Hashing also solves escaping: the channel name appears in places like `listen <name>` that
// cannot be parameterized, and the hash only uses [0-9a-f], so injection is impossible.
func channelFor(topic string) string {
	sum := sha256.Sum256([]byte(topic))
	return "ama_mq_" + hex.EncodeToString(sum[:16])
}

// poke nudges the wake-up channel without blocking. If it is full, a wakeup is already
// pending and there is no need to queue another.
func poke(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// sleep is a time.Sleep that ctx can interrupt.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (q *queue) logWarn(msg string, args ...any) {
	if q.log != nil {
		q.log.Warn(msg, args...)
	}
}
