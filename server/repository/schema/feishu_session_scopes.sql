-- ┌── Table outline: feishu_session_scopes ────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ lark_app_id   text                      not null
-- │ scope_key     text                      not null
-- │ agent_id      text                      not null
-- │ run_id        text                      not null
-- │ workspace_id  text                      not null
-- │ scope_type    text                      not null
-- │ chat_id       text                      not null
-- │ thread_id     text                      not null default ''::text
-- │ created_at    timestamp with time zone  not null
-- │ last_used_at  timestamp with time zone  not null
-- │ turn_count    integer                   not null default 1
-- │
-- │ primary key feishu_session_scopes_pkey
-- │      (lark_app_id, scope_key, agent_id)
-- │ index feishu_session_scopes_run_idx
-- │      (run_id)
-- └────────────────────────────────────────────────────────────────────────────

-- feishu_session_scopes: the mapping "session scope → Ark session", **so the bot has memory**.
--
-- # Why it is needed
--
-- Before this, inbound.go called CreateConversation for every message it received, so every sentence
-- was a brand-new Ark session, and the bot had no context at all: one sentence asks "what is the
-- weather in Hangzhou", the next asks "and tomorrow?", and it does not know what "and" refers to. It
-- gives no error, just puzzling answers, the kind of defect hardest to discover.
--
-- Mapping rules (the algorithm is in scope.go, pure functions):
--
--   messages in a thread (thread_id / root_id)    one session per thread
--   messages in a normal group (no thread anchor) one session per group
--   direct chat                                   **one session per message**
--
-- Direct chat is deliberately the other way round: it also used to reuse by chat_id, and later, per
-- product requirements, each direct message became its own session: the serial lock is on run_id,
-- and with reuse, one stuck round would make the user get "the previous one is still being processed"
-- for every following sentence, and direct chats are typed one after another, the hardest to bear.
-- The cost is that direct chats have no memory across messages; that is a tradeoff, not a defect; see
-- the file header of scope.go and this package's README.
--
-- # Why it must be stored in the database, not an in-process map
--
-- amad is deployed as multiple instances, and two adjacent messages usually do not land in the same
-- process (see CLAUDE.md in the repository). An in-process map would show up as "sometimes it
-- remembers, sometimes not", depending on the load balancer, and it is bound to degrade during
-- rolling releases. This is exactly the first kind of single-machine thinking that document names.
--
-- # Why a "deterministic ID" cannot replace this table
--
-- Because run_id **is the session id issued by Ark** (see CreateConversation in core/chat.go); what it
-- looks like is not up to us, and it cannot be computed by a rule. Computing one would require
-- inventing an AMA-side ID and storing the mapping, which is one more table. So what is stored here is
-- "scope → the ID Ark gave".
--
-- # Concurrency safety relies on a unique constraint, not query-then-create
--
-- When two instances receive two messages of the same group at the same time, both first query (both
-- find nothing), each creates an Ark session, and then comes to bind. **The primary key lets the
-- database decide**: insert ... on conflict do nothing; the one that loses reads back the winner's
-- run_id and reuses it (see the comment on Store.BindSessionScope). "Query, then create" here is
-- textbook TOCTOU: both instances would create, and the group would have two contexts from then on.
--
-- # Like feishu_chats, no doc jsonb
--
-- Not an external entity, no HTTP contract, all fields are scalars; real columns are plainer.
create table if not exists feishu_session_scopes (
    -- The three-part primary key. **agent_id is in the primary key on purpose**: if an app is
    -- rebound to another agent, that should be a new conversation; letting the new agent take over
    -- the old agent's sessions would mismatch the context.
    lark_app_id  text        not null,
    -- scope_key looks like "thread:omt_xxx" / "chat:oc_xxx" / "message:om_xxx", computed by scope.go
    -- and **deterministic**: the same message gives the same key on any instance, which is the
    -- precondition for instances to agree. Note "the same message" is the exact boundary: the keys of
    -- groups / threads are decided by chat_id / anchor, so many messages share one key; the key of a
    -- direct chat is decided by message_id, so each message has its own key.
    scope_key    text        not null,
    agent_id     text        not null,
    -- run_id is the bound Ark session ID (apimodel.Run.ID).
    run_id       text        not null,
    -- workspace_id is stored redundantly, for debugging (the scope itself is not queried by
    -- workspace).
    workspace_id text        not null,
    -- scope_type is thread / chat / message, purely for debugging (it can also be seen from the
    -- scope_key prefix). Deliberately no check constraint: when the rules evolve (like adding message
    -- this time) no migration is needed.
    scope_type   text        not null,
    -- chat_id / thread_id are purely for debugging: when something goes wrong you can look at the
    -- Feishu group and thread directly. thread_id is an empty string for non-thread scopes.
    chat_id      text        not null,
    thread_id    text        not null default '',
    created_at   timestamptz not null,
    -- last_used_at is the last time a message landed in this scope, for debugging "how long has this
    -- group been quiet", and it could later be used for expiry.
    last_used_at timestamptz not null,
    -- turn_count is how many rounds have run on this session. It restarts from 1 when rebinding to a
    -- new session.
    turn_count   int         not null default 1,

    constraint feishu_session_scopes_pkey primary key (lark_app_id, scope_key, agent_id)
);

-- Look up the scope by run_id: when recovering and debugging, often only the run_id is at hand.
create index if not exists feishu_session_scopes_run_idx
    on feishu_session_scopes (run_id);

comment on table feishu_session_scopes is
    'Session scope -> Ark session mapping. Topics reuse one session per topic, normal groups one per group; each 1:1 message gets a new session.';
comment on column feishu_session_scopes.scope_key is
    'Deterministic scope key (thread:xxx / chat:xxx / message:xxx), computed from the inbound message by scope.go.';
comment on column feishu_session_scopes.agent_id is
    'Part of the primary key: rebinding to another agent starts a new conversation and does not inherit the old agent''s session history.';
