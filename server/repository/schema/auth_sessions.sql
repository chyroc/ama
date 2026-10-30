-- ┌── Table outline: auth_sessions ────────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ token_hash   text                      not null
-- │ email        text                      not null
-- │ name         text                      not null default ''::text
-- │ avatar_url   text                      not null default ''::text
-- │ employee_id  text                      not null default ''::text
-- │ created_at   timestamp with time zone  not null
-- │ expires_at   timestamp with time zone  not null
-- │
-- │ primary key auth_sessions_pkey
-- │      (token_hash)
-- │ index auth_sessions_email_idx
-- │      (email)
-- │ index auth_sessions_expires_idx
-- │      (expires_at)
-- └────────────────────────────────────────────────────────────────────────────

-- -- Logged-in sessions -------------------------------------------------------
--
-- **It stores the sha256 of the token, not the token itself.** So even if this table is read, no
-- usable token can be recovered. To check, the incoming token is hashed again and looked up by
-- primary key.
create table if not exists auth_sessions (
    -- sha256 of the token (hex, 64 characters). Primary key: every request looks it up, and a
    -- primary key hit is the fastest.
    token_hash text primary key,

    -- Who the session belongs to. **This is the Actor ID**, already lower-cased (see
    -- auth.normalizeEmail).
    email text not null,

    -- Display details from the identity source at login (OIDC IdP / local account), stored so we do
    -- not ask on every request. May be empty: identity sources do not guarantee an avatar or
    -- employee number.
    name        text not null default '',
    avatar_url  text not null default '',
    employee_id text not null default '',

    created_at timestamptz not null,
    expires_at timestamptz not null
);

-- Used to delete all sessions by email (kick a person out).
--
-- It is also the query path for "how many active sessions does a person have": the same person
-- logged in on several devices has several rows, on purpose: logging out on the laptop should not
-- kick out the phone.
create index if not exists auth_sessions_email_idx on auth_sessions (email);

-- Used by expiry cleanup (`delete from auth_sessions where expires_at < $1`).
--
-- Cleanup is done on the write path instead of a separate scheduled task: the write rate of the
-- session table is the login rate, enough to clear expired rows, and an extra scheduled task that
-- has to grab a lock is not worth it.
create index if not exists auth_sessions_expires_idx on auth_sessions (expires_at);
