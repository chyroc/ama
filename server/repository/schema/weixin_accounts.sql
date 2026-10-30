-- ┌── Table outline: weixin_accounts ──────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id               text  not null
-- │ workspace_id     text  not null
-- │ bot_id           text  not null
-- │ bot_token        text  not null
-- │ base_url         text  not null
-- │ owner_user_id    text  not null default ''::text
-- │ agent_id         text  not null default ''::text
-- │ status           text  not null default 'active'::text
-- │ last_error       text  not null default ''::text
-- │ sync_cursor      text  not null default ''::text
-- │ created_by       text  not null default ''::text
-- │ created_at       text  not null
-- │ updated_at       text  not null
-- │ last_inbound_at  text  not null default ''::text
-- │
-- │ primary key weixin_accounts_pkey
-- │      (id)
-- │ unique weixin_accounts_bot_id_key
-- │      (bot_id)
-- │ index weixin_accounts_workspace_idx
-- │      (workspace_id, created_at)
-- └────────────────────────────────────────────────────────────────────────────

-- weixin_accounts: WeChat bots connected to a workspace (server/weixin).
--
-- A row is created when someone scans the login QR code in a workspace and confirms on the phone. The
-- iLink bot it gets is bound to the WeChat user who scanned (owner_user_id); messages that user sends to
-- the bot are answered by the bound agent.
--
-- # One bot, one workspace
--
-- bot_id is unique across the whole deployment: a bot has exactly one long-poll consumer (two pollers race
-- on the cursor and each sees part of the messages), so it cannot be connected to two workspaces at once.
-- Scanning again in the same workspace updates the token of the existing row (insert ... on conflict).
create table if not exists weixin_accounts (
    -- id is the AMA id (wxa_...), used in URLs.
    id              text        not null,
    workspace_id    text        not null,
    -- bot_id is ilink_bot_id from the login result.
    bot_id          text        not null,
    -- bot_token is the bot's credential, encrypted with shared/infra/cryptobox when a key is configured.
    -- It never leaves the server.
    bot_token       text        not null,
    -- base_url is the API base URL the server returned with the login (may differ from the login host).
    base_url        text        not null,
    -- owner_user_id is the WeChat user who scanned the code (ilink_user_id).
    owner_user_id   text        not null default '',
    -- agent_id is the bound agent; '' means none yet (the bot then answers with a hint).
    agent_id        text        not null default '',
    -- status is active / expired. expired means the platform rejected the token (ret -14); the poller
    -- stops and the user has to scan again.
    status          text        not null default 'active',
    -- last_error is the last problem worth showing on the page ('' when fine).
    last_error      text        not null default '',
    -- sync_cursor is get_updates_buf: the long-poll cursor, saved after every batch so the next poller
    -- (on any instance) continues where this one stopped.
    sync_cursor     text        not null default '',
    created_by      text        not null default '',
    -- created_at / updated_at / last_inbound_at go into the HTTP response: fixed-length ISO strings.
    created_at      text        not null,
    updated_at      text        not null,
    last_inbound_at text        not null default '',

    constraint weixin_accounts_pkey primary key (id),
    constraint weixin_accounts_bot_id_key unique (bot_id)
);

create index if not exists weixin_accounts_workspace_idx
    on weixin_accounts (workspace_id, created_at);
