-- ┌── Table outline: auth_login_failures ──────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ key           text                      not null
-- │ failures      integer                   not null
-- │ window_start  timestamp with time zone  not null
-- │ updated_at    timestamp with time zone  not null
-- │
-- │ primary key auth_login_failures_pkey
-- │      (key)
-- │ index auth_login_failures_updated_idx
-- │      (updated_at)
-- └────────────────────────────────────────────────────────────────────────────

-- -- Failed password login counter (anti brute force) --------------------------
--
-- Fixed-window counting: once a key fails N times within the window, further attempts are rejected;
-- the window resets by itself when it passes.
--
-- **Why it must be stored in the database**: in a multi-instance deployment, the load balancer spreads
-- one person's consecutive attempts over the instances. With an in-process map, N instances give N
-- times the attempt budget, and every restart resets it: as good as no rate limit, and tests never
-- show it.
--
-- The count is increased by **one upsert** (RecordLoginFailure in authdao), not by reading, adding
-- one and writing back: two concurrent failures doing read-modify-write would overwrite each other
-- and count one less, exactly when an attacker hits concurrently.
create table if not exists auth_login_failures (
    -- What is counted, like `email:alice@example.com`. The prefix is so that adding other dimensions
    -- later (e.g. by source IP) does not clash keys.
    key text primary key,

    -- Number of failures in the current window.
    failures integer not null,

    -- When the current window started. Earlier than "now - window length" means expired, and the
    -- next failure counts from 1 again.
    window_start timestamptz not null,

    updated_at timestamptz not null
);

-- For expiry cleanup (piggybacked on the write path, same as auth_sessions).
create index if not exists auth_login_failures_updated_idx on auth_login_failures (updated_at);
