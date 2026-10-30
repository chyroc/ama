-- ┌── Table outline: ark_auth_failures ────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ environment_id   text                      not null
-- │ workspace_id     text                      not null
-- │ key_fingerprint  text                      not null
-- │ first_failed_at  timestamp with time zone  not null
-- │ last_failed_at   timestamp with time zone  not null
-- │ failures         integer                   not null default 0
-- │
-- │ primary key ark_auth_failures_pkey
-- │      (environment_id)
-- └────────────────────────────────────────────────────────────────────────────

-- Where consecutive Ark credential failures (401/403) started: one row per environment.
--
-- # The problem this table solves
--
-- When a workspace has a broken Ark API Key configured, that environment's worker gets 401/403 on
-- every poll. The backoff in core/workerauthbackoff.go pushes retries down to at most once every 5
-- minutes, but its count is **only in the process**: once the instance is rebuilt it starts again
-- from 30 seconds. If nobody replaces the key, it keeps spinning like this and keeps flooding
-- warning logs.
--
-- To do "after 3 full days of consecutive failures, drop to one try per day", we must know **when
-- the consecutive failures started**, and that moment must survive releases and instance rebuilds:
-- with frequent releases, an in-process count never reaches 3 days. So the start is stored in the
-- database, while the count and backoff ladder stay in the process (losing that part only means one
-- more try).
--
-- # Times are always computed by the database
--
-- first_failed_at / last_failed_at are both written with now() in SQL, and "how long has it been
-- failing" is also computed in SQL (now() - first_failed_at). The instance writing and the instance
-- reading are usually not the same; each using its own wall clock would put the clock difference
-- straight into "have 3 days passed"; same as run_sandbox_sessions.terminal_active_until.
--
-- Both columns are timestamptz: they are not in the HTTP contract, only for database comparisons (the
-- "timestamps" table in CLAUDE.md).

create table if not exists ark_auth_failures (
    -- The Ark environment id. Primary key: an environment uses only one key at a time (resolved from
    -- the workspace credentials), so the failure state is recorded per environment, the same key as
    -- the in-process gate (authBackoff.m).
    environment_id  text        primary key,
    -- The owning workspace, only for debugging ("which workspace's key is broken" is the first
    -- question to ask).
    workspace_id    text        not null,
    -- Fingerprint of the key used when failing (ArkKeyFingerprint: len + first 8 of the sha,
    -- irreversible). **A new key restarts the clock**: a new key should not inherit the old key's
    -- failure duration.
    key_fingerprint text        not null,
    -- When this run of consecutive failures started.
    first_failed_at timestamptz not null,
    -- The most recent failure. Only for debugging: together with first_failed_at it shows "how long
    -- this run of failures lasted, and whether it is still happening".
    last_failed_at  timestamptz not null,
    -- How many times it failed in this run. Only for debugging; the decision does not use it (the
    -- decision is the duration; see core.authPauseAfter).
    failures        integer     not null default 0
);
