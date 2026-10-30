-- ┌── Table outline: lark_user_auth_sessions ──────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ actor          text                      not null
-- │ lark_app_id    text                      not null
-- │ device_code    text                      not null
-- │ poll_interval  integer                   not null default 5
-- │ created_at     timestamp with time zone  not null
-- │ expires_at     timestamp with time zone  not null
-- │
-- │ primary key lark_user_auth_sessions_pkey
-- │      (actor, lark_app_id)
-- │ index lark_user_auth_sessions_expires_idx
-- │      (expires_at)
-- └────────────────────────────────────────────────────────────────────────────

-- lark_user_auth_sessions: the device code in-between state of user authorization, temporary rows
-- lasting a few minutes.
--
-- ⚠️⚠️ **This must be stored in the database, not in process memory.** amad is deployed as multiple
-- instances: the "start authorization" request lands on instance A, and after the user clicks
-- consent, the frontend's "is it done yet" request **very likely lands on instance B** (the load
-- balancer distributes at random). If device_code only lived in A's memory, B could not find it,
-- and the symptom is "I clicked authorize but the page keeps spinning", **occasionally** (fine when
-- it happens to land back on A), the hardest kind to track down. Rolling releases make it worse: A
-- can be rebuilt at any time, and memory is gone just like that.
--
-- The shape follows feishu_sessions (a precedent for the same thing): one row per pending
-- authorization session, with its own cleanup time.
create table if not exists lark_user_auth_sessions (
    -- The primary key is (authorizing person, Feishu app): one person should have only one pending
    -- authorization flow per app at a time. Clicking "authorize" again should **overwrite** the
    -- previous one (they most likely closed the page and gave up), not keep two side by side: then
    -- polling would not know which device_code to trust.
    actor       text        not null,
    lark_app_id text        not null,

    -- device_code is Feishu's polling credential. **As sensitive as a secret** (holding it lets you
    -- get the token), so the same rule as the feishu_sessions table: it goes into no HTTP response.
    --
    -- ⚠️ This column is **not encrypted**, deliberately unlike the two columns of the table above.
    -- The reason is that it only lives a few minutes and is deleted right after the token is
    -- obtained, while encryption would make operations like "clean up expired rows" decrypt first.
    device_code text        not null,
    -- interval is the poll interval in seconds suggested by Feishu, passed on to the frontend as is.
    poll_interval int       not null default 5,

    created_at  timestamptz not null,
    -- expires_at is **the cleanup time of this row** (derived from Feishu's expires_in).
    -- Past it, treat "this authorization flow as void" and let the user start again.
    expires_at  timestamptz not null,

    constraint lark_user_auth_sessions_pkey primary key (actor, lark_app_id)
);

-- Expiry cleanup scans this column.
create index if not exists lark_user_auth_sessions_expires_idx
    on lark_user_auth_sessions (expires_at);

comment on table lark_user_auth_sessions is
    'Intermediate state of the Feishu user-authorization device code. Must be in the database, not process memory: with several instances, start and poll often hit different processes.';
comment on column lark_user_auth_sessions.device_code is
    'Feishu poll credential, as sensitive as a secret, never sent in HTTP responses. Deliberately not encrypted: it only lives for a few minutes.';
