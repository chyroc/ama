-- ┌── Table outline: feishu_inbound_messages ──────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ message_id   text                      not null
-- │ lark_app_id  text                      not null
-- │ chat_id      text                      not null
-- │ status       text                      not null
-- │ created_at   timestamp with time zone  not null
-- │ chat_type    text
-- │ thread_id    text
-- │ root_id      text
-- │ parent_id    text
-- │
-- │ primary key feishu_inbound_messages_pkey
-- │      (message_id)
-- │ index feishu_inbound_messages_created_idx
-- │      (created_at)
-- └────────────────────────────────────────────────────────────────────────────

-- feishu_inbound_messages: dedup records of inbound messages.
--
-- Feishu **delivers the same message more than once**: a network hiccup, our slow processing, or an
-- error we returned all make it deliver again. Without dedup the same sentence would run the agent
-- twice, and the user would see two cards.
--
-- This table deliberately has no doc: it is not an external entity, has no HTTP contract, and the
-- fields are just a few scalars; real columns are plainer than jsonb.
create table if not exists feishu_inbound_messages (
    -- message_id is the Feishu message ID (om_xxx), **the dedup key**, so it is the primary key.
    message_id  text        not null,
    -- lark_app_id is the Feishu app that received this message (cli_xxx).
    lark_app_id text        not null,
    -- chat_id is the conversation the message is in.
    chat_id     text        not null,
    -- status is the processing state: pending / processed / failed.
    --
    -- Three states, three treatments (see Store.ClaimInboundMessage): pending and processed both count
    -- as duplicate delivery and are acked right away; **failed is let through for retry**: the one that
    -- failed last time should be tried again when Feishu redelivers it.
    --
    -- No CHECK: the value set is guaranteed by constants on the Go side, like other tables in the
    -- repository.
    status      text        not null,
    -- created_at is when it was claimed, used to clean up old records.
    created_at  timestamptz not null,

    -- Four thread anchors, **existing purely for diagnosing afterwards**; they take part in neither
    -- dedup nor any business decision.
    --
    -- Problems like "continued in a thread, but a new session was started" have two completely
    -- different root causes:
    --
    --   Feishu gave no anchor at all (root_id / parent_id both empty)  → a platform or SDK parsing problem
    --   an anchor was given but we computed the wrong key              → a scopeForMessage problem
    --
    -- **The fixes have nothing in common, and afterwards the database alone cannot tell them apart**:
    -- feishu_session_scopes only keeps the computed scope_key, none of the inputs before computing.
    -- So the inputs of scopeForMessage (root_id / parent_id read by directThreadAnchor, plus thread_id
    -- read by threadAnchor, plus chat_type, which decides the branch) are stored together, and
    -- diagnosis no longer depends on reproducing it.
    --
    -- All four columns are nullable with no default: an empty value itself means something, and is
    -- exactly the most important signal: **"Feishu did not give this anchor"**. A default '' would
    -- merge "not given" with "given as an empty string".
    chat_type   text,
    thread_id   text,
    root_id     text,
    parent_id   text,

    constraint feishu_inbound_messages_pkey primary key (message_id)
);

-- Scanned by time when cleaning up old records.
create index if not exists feishu_inbound_messages_created_idx
    on feishu_inbound_messages (created_at);

comment on column feishu_inbound_messages.root_id is
    'root_id reported by Feishu (om_xxx), the anchor for reusing a session inside a 1:1 chat thread. NULL = Feishu did not send it, a key signal when diagnosing.';
