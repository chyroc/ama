-- ┌── Table outline: feishu_chats ─────────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ lark_app_id   text                      not null
-- │ chat_id       text                      not null
-- │ workspace_id  text                      not null
-- │ chat_type     text                      not null
-- │ name          text                      not null default ''::text
-- │ last_seen_at  timestamp with time zone  not null
-- │
-- │ primary key feishu_chats_pkey
-- │      (lark_app_id, chat_id)
-- │ index feishu_chats_workspace_idx
-- │      (workspace_id, last_seen_at DESC, chat_id)
-- └────────────────────────────────────────────────────────────────────────────

-- feishu_chats: the registry of conversations (groups) the bot has seen.
--
-- # Why it is needed
--
-- Scheduled tasks must be able to choose "which group to send the report to", and before this the
-- system had **no** "workspace → conversation" mapping at all: feishu_apps has no chat field, and the
-- chat_id in feishu_inbound_messages is a dedup record keyed by message_id (and gets cleaned up), not
-- a permanent registry.
--
-- The alternative was letting users type oc_xxx by hand in the UI. Rejected because: they would have
-- to dig the ID out of Feishu themselves, and a wrong one **gives no error**: the report just never
-- gets sent. So instead it records an entry as inbound messages pass through (see rememberChat in
-- inbound.go): zero input from the user, and naturally only groups the bot is really in are listed.
--
-- # Like feishu_inbound_messages, no doc
--
-- It is not an external entity and has no HTTP contract; the fields are just a few scalars, and real
-- columns are plainer than jsonb.
create table if not exists feishu_chats (
    -- (lark_app_id, chat_id) is the primary key: the same group may be seen by several apps, one row
    -- each.
    lark_app_id text not null,
    chat_id     text not null,
    -- workspace_id is stored redundantly, so ListChats does one index scan instead of joining
    -- feishu_apps.
    workspace_id text not null,
    -- chat_type is p2p / group / topic_group.
    chat_type text not null,
    -- name is the group name, **empty when first recorded**: inbound messages do not carry it; it
    -- needs a separate call to im/v1/chats. Filled in by SetChatName: written once when the inbound
    -- prefetch gets the group name, and fetched on the spot for groups with empty names when listing
    -- groups (feishu/chatnames.go). When neither works the UI falls back to showing chat_id.
    name text not null default '',
    -- last_seen_at is the last time a message was received in this group; ListChats sorts by it
    -- descending.
    last_seen_at timestamptz not null,

    constraint feishu_chats_pkey primary key (lark_app_id, chat_id)
);

-- ListChats lists by workspace + by most recently seen, descending.
create index if not exists feishu_chats_workspace_idx
    on feishu_chats (workspace_id, last_seen_at desc, chat_id);
