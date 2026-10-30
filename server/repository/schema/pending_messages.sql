-- ┌── Table outline: pending_messages ─────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id             text                      not null
-- │ run_id         text                      not null
-- │ workspace_id   text                      not null
-- │ body           text                      not null
-- │ actor          text                      not null default ''::text
-- │ source         text                      not null default 'web'::text
-- │ status         text                      not null default 'pending'::text
-- │ attempts       integer                   not null default 0
-- │ last_error     text                      not null default ''::text
-- │ created_at     timestamp with time zone  not null
-- │ claimed_by     text
-- │ claimed_until  timestamp with time zone
-- │ ord            bigint                    not null (sequence)
-- │
-- │ primary key pending_messages_pkey
-- │      (id)
-- │ index pending_messages_due_idx
-- │      (run_id, created_at, ord) WHERE (status = 'pending'::text)
-- │ index pending_messages_workspace_idx
-- │      (workspace_id, created_at, ord)
-- └────────────────────────────────────────────────────────────────────────────

-- pending_messages: messages the user queued while the agent is still running, **existing to survive
-- closing the page and restarts**.
--
-- # Why it must be stored in the database
--
-- Queuing used to live only in the browser's React state (web/src/lib/useSendQueue.ts). So:
--
--   1. **Closing / refreshing the page loses the queued messages**, while the user thinks they will
--      be sent;
--   2. Only the web page could queue. On the Feishu path "the previous one is still running" could
--      only answer "ask again later", and the user had to remember to resend;
--   3. Opening another tab gives two queues that do not know about each other; each queues its own,
--      and the final order is a mess.
--
-- Storing it in the database solves all three: whoever queued it, it is in the same table, and the
-- server sends it when the round ends.
--
-- ⚠️ **This is not the same thing as "automatic resend after hitting the tool execution window"**; do
-- not merge them. That one is a race of tens of milliseconds (Ark rejects user.message between sending
-- tool_use and the worker returning tool_result), unrelated to user intent, retried on the spot by the
-- server (sendEventsThroughWindow in core/chat.go, 1-second budget). This table is about **the user
-- queuing on purpose**: they know they have to wait a whole round, they just do not want to hold it
-- in. The waits differ by three orders of magnitude, and the handling is completely different.
create table if not exists pending_messages (
    -- id is this queued message's own identifier; cancel / send now locate it by it.
    id text primary key,

    -- run_id is which conversation it goes to. **It is Ark's session id** (see core/chat.go).
    --
    -- No foreign key, like other tables in this repository: cascading cleanup when a run is deleted is
    -- written out explicitly in DeleteRun, so what gets deleted can be seen in one place.
    run_id text not null,

    -- workspace_id is used for queries by workspace and cascading cleanup.
    workspace_id text not null,

    -- text is the sentence the user queued.
    body text not null,

    -- actor is the person who queued this message (email).
    --
    -- Stored for **permissions**: cancel / send now may only be done by that person or a workspace
    -- admin. On the Feishu path it is the email resolved from the Feishu user's open_id; if it cannot
    -- be resolved it is an empty string (then only admins can touch it).
    actor text not null default '',

    -- source records where it was queued from: web / feishu.
    --
    -- Only for debugging and display (one queued from Feishu must be able to reply "queued" in the
    -- group). No CHECK, like other tables in this repository: the value set is guaranteed by Go-side
    -- constants.
    source text not null default 'web',

    -- status is the state of this queued message: pending / sending / failed.
    --
    -- ⚠️ **It is not the "conversation state"**: that is in the runs table, driven by Ark events. This
    -- column only serves **mutual exclusion between instances**: every instance does the pick-up, and
    -- claims with a CAS on it.
    --
    -- There is no sent value: it is deleted once sent (see the notes in core/pendingmessage.go); keeping
    -- sent records would only make the query "how many are still queued on this conversation" slower and
    -- slower.
    status text not null default 'pending',

    -- attempts is how many times sending was tried. When the limit is reached it is marked failed and
    -- the original text is kept for the user.
    --
    -- Without it, a message that "can never be sent" (e.g. the agent was disabled) would be retried at
    -- the end of every round, flooding error logs forever.
    attempts int not null default 0,

    -- last_error is the reason of the last failure, shown to the user as "why it was not sent".
    last_error text not null default '',

    -- created_at is when it was queued, **and also the basis of the sending order**.
    --
    -- timestamptz instead of text: it is only used by the database for sorting and not in the HTTP
    -- contract (queuedAt on the contract side is the ISO string in doc, stored by the same write). See
    -- "Timestamps: unit and format" in CLAUDE.md.
    created_at timestamptz not null,

    -- claimed_by / claimed_until are the claim lease of the pick-up, with exactly the same semantics as
    -- those two columns of schedules.
    --
    -- claimed_by is only for debugging; what really decides ownership is claimed_until, which **must
    -- have an expiry**: the instance may be rebuilt while sending, and then nobody releases it; only
    -- lease expiry lets someone else take over.
    claimed_by    text,
    claimed_until timestamptz,

    -- ord orders messages queued at the same moment (a fallback when created_at's precision is not
    -- enough).
    ord bigserial not null
);

-- The main pick-up query: find the first message waiting to be sent on a conversation.
--
-- Only pending rows are indexed (a partial index): sending ones are being sent and failed ones are no
-- longer sent automatically, so letting them take index space makes no sense (same tradeoff as
-- schedules_due_idx).
--
-- ⚠️ **Do not change this index's predicate**: `create index if not exists` only looks at the name and
-- **does not change the predicate of an existing index**; written that way it would be right in new
-- databases while databases that already have it keep the old predicate: the DDL reports no error, and
-- the change silently does not take effect. To change the predicate use a new name (see the
-- post-mortem on schedules_run_now_idx).
create index if not exists pending_messages_due_idx
    on pending_messages (run_id, created_at, ord)
    where status = 'pending';

-- List by workspace (UI display + cascading cleanup when a workspace is deleted).
create index if not exists pending_messages_workspace_idx
    on pending_messages (workspace_id, created_at, ord);

comment on column pending_messages.status is
    'Claim status pending / sending / failed, only used for mutual exclusion across instances; the conversation state lives in the runs table.';
comment on column pending_messages.created_at is
    'Time it was queued, which is also the send order. timestamptz because it is only used for sorting in the database.';
