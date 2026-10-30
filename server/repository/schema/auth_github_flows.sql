-- ┌── Table outline: auth_github_flows ────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ flow_id           text                      not null
-- │ binding_hash      text                      not null
-- │ device_code       text                      not null
-- │ user_code         text                      not null
-- │ verification_uri  text                      not null
-- │ interval_seconds  integer                   not null
-- │ next_poll_at      timestamp with time zone  not null
-- │ created_at        timestamp with time zone  not null
-- │ expires_at        timestamp with time zone  not null
-- │
-- │ primary key auth_github_flows_pkey
-- │      (flow_id)
-- │ index auth_github_flows_expires_idx
-- │      (expires_at)
-- └────────────────────────────────────────────────────────────────────────────

-- -- GitHub device flow logins in progress ------------------------------------
--
-- The in-between state of one login: living between "clicked log in with GitHub" and "the user
-- entered the code on github.com, and the browser's next poll got the token" (GitHub's expires_in,
-- usually 15 minutes).
--
-- **Why it must be stored in the database**: multi-instance deployment. The start lands on instance
-- A, and every later poll may land on any instance; if device_code, the current poll interval and the
-- next allowed poll time lived only in A's memory, B could not find them (showing up as an occasional
-- "login expired"), and N instances each keeping their own interval would multiply the poll rate to
-- GitHub by N; GitHub would answer slow_down, and the interval would keep growing.
--
-- Two kinds of secrets, handled differently:
--
--   device_code      stored as **plain text**: every poll must send it to GitHub as is, and a
--                    hash would be useless. It is only useful during the dozen or so minutes this
--                    row lives; once the token is obtained (or it expires/is denied) the row is
--                    deleted. **Never sent to the browser**: whoever has it can take the token after
--                    the user authorizes.
--   binding cookie   only the sha256 is stored (binding_hash). The browser polls with flow_id + the
--                    cookie, and only when both match is it the same person; see "The flow id cannot
--                    be claimed by someone else" in auth/github.go.
--
-- The GitHub access token **does not go into this table** (or any table); it is thrown away after use.
create table if not exists auth_github_flows (
    -- The opaque id given to the browser, 32 random bytes (auth.newToken). It alone is not enough to
    -- poll; the binding cookie is also needed.
    flow_id text primary key,

    -- sha256 (hex) of the plain binding cookie value. Polling compares it in the where condition; if
    -- it does not match, the row is treated as nonexistent.
    binding_hash text not null,

    -- The device_code from GitHub (a secret; see the file header).
    device_code text not null,

    -- The short code and entry URL for people. Stored only for logs and debugging; the browser
    -- already got them at the start.
    user_code text not null,
    verification_uri text not null,

    -- The current poll interval (seconds). When GitHub answers slow_down it is +5 and **persisted**:
    -- without storing it, the next poll landing on another instance would use the old interval and
    -- get another slow_down.
    interval_seconds integer not null,

    -- The next time we may ask GitHub. "Winning this poll" is decided by a single UPDATE checking
    -- `next_poll_at <= now` and pushing it back on the way, so among concurrent polls only one really
    -- asks GitHub.
    next_poll_at timestamptz not null,

    created_at timestamptz not null,
    expires_at timestamptz not null
);

-- For expiry cleanup (piggybacked on the write path that starts a login).
create index if not exists auth_github_flows_expires_idx on auth_github_flows (expires_at);
