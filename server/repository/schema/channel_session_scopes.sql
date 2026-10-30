-- ┌── Table outline: channel_session_scopes ───────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ channel       text                      not null
-- │ account_id    text                      not null
-- │ scope_key     text                      not null
-- │ agent_id      text                      not null
-- │ run_id        text                      not null
-- │ workspace_id  text                      not null
-- │ created_at    timestamp with time zone  not null
-- │ last_used_at  timestamp with time zone  not null
-- │ turn_count    integer                   not null default 1
-- │
-- │ primary key channel_session_scopes_pkey
-- │      (channel, account_id, scope_key, agent_id)
-- │ index channel_session_scopes_run_idx
-- │      (run_id)
-- └────────────────────────────────────────────────────────────────────────────

-- channel_session_scopes: "conversation scope → Ark session", so the bot remembers earlier turns.
--
-- A channel computes a deterministic scope key from each message (for WeChat: one key per user), and
-- every message with the same key reuses the same Ark session. It must live in the database, not in a
-- process map: two adjacent messages usually land on different instances.
--
-- Concurrency relies on the primary key, not on query-then-create: two instances that both find no
-- binding both create a session, then insert ... on conflict do nothing decides the winner and the
-- loser reuses the winner's run_id (see channel.EnsureSession).
--
-- Feishu keeps its older feishu_session_scopes table; new channels share this one.
create table if not exists channel_session_scopes (
    channel      text        not null,
    account_id   text        not null,
    -- scope_key looks like "user:<id>", computed by the channel.
    scope_key    text        not null,
    -- agent_id is part of the key on purpose: rebinding the account to another agent starts new
    -- conversations instead of handing the old agent's history to the new one.
    agent_id     text        not null,
    -- run_id is the bound Ark session id (apimodel.Run.ID).
    run_id       text        not null,
    -- workspace_id is stored for diagnosis; nothing queries by it.
    workspace_id text        not null,
    created_at   timestamptz not null,
    last_used_at timestamptz not null,
    -- turn_count is how many turns ran on this session.
    turn_count   int         not null default 1,

    constraint channel_session_scopes_pkey primary key (channel, account_id, scope_key, agent_id)
);

-- When diagnosing, often only the run id is at hand.
create index if not exists channel_session_scopes_run_idx
    on channel_session_scopes (run_id);
