-- ┌── Table outline: channel_inbound_messages ─────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ channel          text                      not null
-- │ account_id       text                      not null
-- │ message_id       text                      not null
-- │ conversation_id  text                      not null default ''::text
-- │ status           text                      not null
-- │ created_at       timestamp with time zone  not null
-- │
-- │ primary key channel_inbound_messages_pkey
-- │      (channel, account_id, message_id)
-- │ index channel_inbound_messages_created_idx
-- │      (created_at)
-- └────────────────────────────────────────────────────────────────────────────

-- channel_inbound_messages: dedup records of inbound messages, for every channel except Feishu.
--
-- Chat platforms deliver the same message more than once (retries, a cursor handed over between two
-- instances during a deploy). Without dedup the same sentence runs the agent twice and the user gets
-- two answers. The primary key lets the database decide who handles a message: insert ... on conflict.
--
-- Feishu keeps its older feishu_inbound_messages table (it stores extra thread anchors for diagnosis);
-- new channels share this one, told apart by the channel column.
create table if not exists channel_inbound_messages (
    -- channel is the channel kind (weixin ...), the same value as runs.trigger for runs it starts.
    channel         text        not null,
    -- account_id is the AMA id of the connected account that received the message.
    account_id      text        not null,
    -- message_id is the platform's message id, unique within one account.
    message_id      text        not null,
    -- conversation_id is the platform conversation (user / group) the message came from, for diagnosis.
    conversation_id text        not null default '',
    -- status is pending / processed / failed. failed is let through when the message is delivered
    -- again; the other two count as duplicates. No CHECK: the values are Go constants.
    status          text        not null,
    -- created_at is when the message was claimed, used to clean up old rows.
    created_at      timestamptz not null,

    constraint channel_inbound_messages_pkey primary key (channel, account_id, message_id)
);

create index if not exists channel_inbound_messages_created_idx
    on channel_inbound_messages (created_at);
