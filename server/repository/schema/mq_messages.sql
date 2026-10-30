-- ┌── Table outline: mq_messages ──────────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id             bigint                    not null (sequence)
-- │ topic          text                      not null
-- │ payload        bytea                     not null
-- │ created_at     timestamp with time zone  not null
-- │ expires_at     timestamp with time zone  not null
-- │ claimed_by     text
-- │ claimed_until  timestamp with time zone
-- │
-- │ primary key mq_messages_pkey
-- │      (id)
-- │ index mq_messages_due_idx
-- │      (topic, id)
-- └────────────────────────────────────────────────────────────────────────────

-- The table of the generic message queue (shared/infra/pgmq). **One table holds all topics**, not one
-- table per topic.
--
-- # Why this table exists
--
-- The direct cause is the webshell terminal: the terminal's downlink SSE and the uplink "every
-- keystroke" are two independent HTTP requests, and the load balancer sends them **independently** to
-- random instances, while the PTY connection is only held by the instance that opened the SSE. Landing
-- on another instance gives 404, with a hit rate of about 1/number of instances.
--
-- A PTY can only have one holder, so it cannot be fixed by "retry a few times until hitting the right
-- instance". Only one way is left: **keystrokes land somewhere all instances can see, and the instance
-- holding the connection takes them**. This table is that place.
--
-- # Why not Redis pub/sub, the pgmq extension, or an external message queue
--
--   - **Redis pub/sub does not guarantee delivery**: in proxy / cluster deployments subscriptions are
--     not necessarily routed across nodes, while the client still works; precisely because it works
--     it is more dangerous: it drops messages occasionally.
--   - **The pgmq extension** needs superuser to install, local docker PG does not have it by default,
--     and requiring it would break local development and offline tests entirely; and it only has a
--     pull model (polling), two orders of magnitude slower than push.
--   - **Generic message queue semantics do not fit**: you cannot deliver to a specific instance (the
--     queue→consumer mapping is decided by rebalance and drifts, and delivering to the wrong instance
--     is exactly the shape of the 404 above); at-least-once delivers duplicates (a duplicate
--     keystroke = a visible extra character for the user); topics are usually resources that must be
--     requested in advance, while topics here are `term:<terminalID>`, one per terminal, created
--     dynamically.
--
-- # Latency: why we dare put individual keystrokes into the database
--
-- Measured locally (60 runs per setting):
--
--	same instance directly (no database)  p50 0.00ms   ← fast path
--	polling @20ms                         p50 10.9ms   p95 20.7ms
--	polling @200ms                        p50 113ms    p95 200ms    ← already feels broken
--	LISTEN/NOTIFY (push)                  p50 0.08ms   p95 0.18ms   ← two orders of magnitude faster
--
-- So the implementation is **push first, polling as a fallback**: steady-state latency is decided by
-- NOTIFY, and polling only covers "the connection just reconnected and the notifications during that
-- gap are gone".
--
-- # ⚠️ This table stores messages **in flight**, not history
--
-- A message is deleted once consumed successfully; there is no `sent` state and no archive table. The
-- reason is the same as pending_messages: keeping them would only make the most common condition,
-- "waiting to be consumed", slower and slower, and there is no audit need here. If archiving is
-- needed, create another table; do not add a status column to this one.
--
-- # Who deletes: delete on consume + hourly safety cleanup, **both are needed**
--
-- The vast majority of rows are reclaimed by "delete once consumed", living only a few milliseconds.
-- But one kind cannot be reclaimed that way: **messages sent that nobody comes to consume**: the user
-- typed a key, and the instance holding that PTY did not come to take it within the TTL (30 seconds)
-- (the instance happened to be rebuilt, or the user closed the terminal and left).
--
-- So `shared/infra/pgmq/purge.go` runs an **hourly** safety cleanup (leader elected with a Redis
-- lease; if it cannot get it, it does not run; without Redis every instance runs it, which is safe
-- because delete is safe to repeat).
--
-- Missing it would not blow up the table (payloads are only a few dozen bytes), but **the index would
-- only grow**: `term:<terminalID>` is one topic per terminal, rows of dead topics would hold entries
-- of mq_messages_due_idx forever, plus the dead tuples from deletes, and the autovacuum load would
-- keep rising. **This kind of degradation is the hardest to notice**, because no single day is
-- "obviously worse".

create table if not exists mq_messages (
    -- id is both the primary key and **the delivery order**. bigserial instead of a timestamp: several
    -- messages queued in the same millisecond must be ordered, and wall clocks drift across instances.
    id bigserial primary key,

    -- topic is the queue name, defined by the caller (webshell uses "term:<terminalID>").
    --
    -- ⚠️ **Do not use topic as the NOTIFY channel name**: PG channel names are limited to 63
    -- characters (measured: going over gives `channel name too long`), while topics usually carry
    -- business ids of uncontrolled length. In shared/infra/pgmq the channel name is always `ama_mq_` +
    -- a short hash of the topic; see that package's comment.
    topic text not null,

    -- payload is the message body, **stored and fetched as is**; this layer does not interpret it.
    --
    -- bytea instead of jsonb: what is forwarded here are terminal PTY frames, containing control
    -- characters and non-UTF-8 byte sequences, which jsonb cannot store and would rewrite.
    --
    -- ⚠️ The payload goes through the table, **not through NOTIFY**: NOTIFY payloads are limited to under
    -- 8000 bytes (measured: going over gives `payload string too long`). "Small messages via
    -- notification, large ones via the table" would split into two delivery paths, one of which would
    -- always lack tests; so notifications are only ever a "wake up" signal, and data is always in this
    -- table.
    payload bytea not null,

    created_at timestamptz not null,

    -- After expires_at this message is no longer delivered, and cleanup deletes it along the way.
    --
    -- **Strongly time-sensitive messages must be able to expire**: a backlog of keystrokes flooding
    -- into the terminal later is harmful: after reconnecting the user would see a string of characters
    -- typed minutes ago suddenly run. The TTL is decided by the publisher per topic.
    expires_at timestamptz not null,

    -- claimed_by / claimed_until are the claim lease, with exactly the same semantics as those two
    -- columns of pending_messages.
    --
    -- claimed_by is only for debugging; what really decides ownership is claimed_until, which **must
    -- have an expiry**: the instance may be rebuilt while processing, and then nobody releases it;
    -- only lease expiry lets someone else take over.
    claimed_by    text,
    claimed_until timestamptz
);

-- The main claim query: the oldest unclaimed message on a topic.
--
-- ⚠️ **Deliberately no partial index predicate like `where claimed_until is null` here**: the claim
-- condition contains `claimed_until < now()`, and now() is not an immutable function, so it cannot go
-- into an index predicate; a partial index would not work for "rows whose lease just expired".
--
-- ⚠️ Do not change this index's predicate later either: `create index if not exists` **only looks at
-- the name and does not change the predicate of an existing index**; it would be right in new
-- databases while databases that already have it keep the old one, and the DDL reports no error
-- (pending_messages.sql records the same thing). To change it, use a new name.
create index if not exists mq_messages_due_idx
    on mq_messages (topic, id);

comment on column mq_messages.payload is
    'Message body, stored and read as is. bytea because it carries control characters and non-UTF-8 bytes from PTY frames.';
comment on column mq_messages.expires_at is
    'Not delivered after this time. Replaying a backlog of time-sensitive messages (such as terminal keystrokes) is harmful.';
comment on column mq_messages.claimed_until is
    'When the claim lease expires. This decides ownership, not claimed_by - that one is only for debugging.';
