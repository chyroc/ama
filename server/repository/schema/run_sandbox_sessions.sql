-- ┌── Table outline: run_sandbox_sessions ─────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ run_id                 text                      not null
-- │ session_id             text                      not null
-- │ sandbox_id             text                      not null default ''::text
-- │ created_at             timestamp with time zone  not null
-- │ terminal_active_until  timestamp with time zone
-- │ last_active_at         timestamp with time zone
-- │
-- │ primary key run_sandbox_sessions_pkey
-- │      (run_id)
-- │ index run_sandbox_sessions_session_idx
-- │      (session_id)
-- └────────────────────────────────────────────────────────────────────────────

-- Session containers (one Docker container per Ark session).
--
-- # What this table stores
--
-- The first time a conversation (Run) really calls a tool, a Docker container is created to run it
-- (see the lazy creation in shared/infra/dockertools/session.go). This table records "which container
-- this Run uses, which Docker daemon it was created on, and when its last activity was".
--
-- # Why it must be stored in the database
--
-- The container name is computed from the session id, so the worker itself needs no mapping table.
-- But when **the frontend wants to look at the container**, the request lands on any instance through
-- the load balancer, and that instance must know two things: did this Run ever have a container (none
-- and gone of the four states are told apart by it), and which daemon the container is on (instances
-- may connect to different daemons). And **reclaim inspection** must know how long the container has
-- been idle: container labels cannot be changed after creation, and what one process remembers other
-- instances cannot see.
--
-- # Only store what does not change (plus two "times we write ourselves"); look up everything else live
--
-- ⚠️ **There is no status in this table, on purpose.** The authoritative state of the container is on
-- the Docker daemon, and it **changes by itself** (paused after idling, deleted by reclaim
-- inspection, removed by hand with docker rm). Storing a status snapshot would lead to "the page says
-- running, but it is long gone". So the status is always looked up with inspect.
--
-- The two columns `*_at` / `*_until` do not break this rule; the test is "who is the authority": only
-- amad can write them, so we are the only source of truth (see each one's comment below).
--
-- # Meaning of the columns (the names were left by an earlier executor; the meanings changed)
--
--   session_id  container name (`<prefix>-sess-<session id>`)
--   sandbox_id  id of the Docker daemon the container is on (the ID from `docker info`)
--
-- # Why created_at is timestamptz and not ISO text
--
-- By the rule in the table of CLAUDE.md "Timestamps: unit and format": the frontend does not compare
-- it as a string; it is only shown and used by the database for sorting, so it is timestamptz, letting
-- the database be the only clock.

create table if not exists run_sandbox_sessions (
    -- run_id is the Ark session id (chat.go uses the id returned by CreateSession directly as Run.ID),
    -- and it is the primary key: a Run has only one container at a time.
    --
    -- The primary key also serves as the idempotency key: after a container is deleted it is rebuilt
    -- (possibly on another daemon), and the rebuild overwrites with on conflict do update instead of
    -- piling up two rows in the table.
    run_id text primary key,

    -- session_id is the container name (the column name is historical).
    session_id text not null,

    -- sandbox_id is the id of the Docker daemon the container is on. An instance connected to another
    -- daemon uses it to know "this container is not here" instead of wrongly reporting "reclaimed".
    sandbox_id text not null default '',

    -- created_at is when this row was written, i.e. when the container was created.
    --
    -- When reclaim inspection judges "how long it has been idle", rows with no recorded activity fall
    -- back to this time.
    created_at timestamptz not null,

    -- terminal_active_until: "do not pause this container before this time".
    --
    -- # What it solves
    --
    -- With a webshell open, the container would still get paused: pausing happens after **a
    -- conversation round has been idle for 60 seconds** (idle in shared/infra/dockertools/session.go),
    -- and that check has no idea someone is typing commands in the terminal. So while the user is
    -- halfway through editing in `vim`, or watching logs with `tail -f`, the container freezes, and the
    -- screen says nothing; there is just no more output.
    --
    -- With this column, pausing first asks "is the terminal still in use", and skips this round if it
    -- is, the same approach as the neighbor "skip pausing while a background job is running"
    -- (dockertools' hasRunningBackgroundJob).
    --
    -- # ⚠️ Why it must be stored in the database, not an in-process map
    --
    -- **Pausing and the terminal are almost never in the same process.**
    --
    --   - Pausing happens on the instance that "grabbed this Ark work" (pollLoop in core/worker.go);
    --   - the terminal's SSE and each keystroke POST are sent to random instances by the load balancer.
    --
    -- So the fact "is the terminal in use" is invisible within one process; written into a map, this
    -- feature would most likely not work with multiple instances, and not working looks exactly like
    -- not doing it: the container still pauses, with no error.
    --
    -- # Why "an expiry time" instead of "a last activity time"
    --
    -- Storing the last activity time would make the reader know itself "how long still counts as in
    -- use", and the reader is in shared/infra/dockertools, a package that should not know such business
    -- thresholds (it does not even import core). Storing `*_until` reduces the check to one
    -- `now() < terminal_active_until` comparison, **with the database as the clock** (each instance's
    -- wall time drifts). runs.watch_until has the same shape.
    --
    -- Nullable, no default: NULL = nobody ever opened a terminal, which for the check is equivalent to
    -- "opened but long expired".
    terminal_active_until timestamptz,

    -- last_active_at: the last time the container ran a tool command (written by the database's now(),
    -- throttled to once every 30 seconds).
    --
    -- Reclaim inspection (dockertools.RunReaper) uses it to judge how long the container has been
    -- idle: past docker.containerTTL the container is deleted (the workspace volume is kept), past
    -- docker.workspaceTTL the volume is deleted too.
    --
    -- The opposite tradeoff from terminal_active_until above: here what is stored is "the last activity
    -- time": the threshold is a config item (dockertools' own business), not a business rule, and the
    -- reader compares the time itself.
    --
    -- Nullable, no default: NULL = no activity ever recorded; reads coalesce it to created_at.
    last_active_at timestamptz
);

-- Look up which Run uses a container by its name (for debugging: you see a container in docker ps and
-- want to know which conversation it belongs to). Not a unique index: during a rebuild the old record
-- may briefly stay elsewhere, and that should not block writing here.
create index if not exists run_sandbox_sessions_session_idx
    on run_sandbox_sessions (session_id);
