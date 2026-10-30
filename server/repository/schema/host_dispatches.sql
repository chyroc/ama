-- ┌── Table outline: host_dispatches ──────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id             text                      not null
-- │ host_id        text                      not null
-- │ seq            bigint                    not null
-- │ kind           text                      not null default ''::text
-- │ state          text                      not null default ''::text
-- │ session_id     text                      not null default ''::text
-- │ terminal_id    text                      not null default ''::text
-- │ payload        text                      not null default ''::text
-- │ result         text                      not null default ''::text
-- │ created_at     timestamp with time zone  not null default now()
-- │ delivered_at   timestamp with time zone
-- │ claimed_until  timestamp with time zone
-- │ updated_at     timestamp with time zone  not null default now()
-- │
-- │ primary key host_dispatches_pkey
-- │      (id)
-- │ index host_dispatches_claim_idx
-- │      (host_id, state, claimed_until, seq)
-- │ unique host_dispatches_host_seq_idx
-- │      (host_id, seq)
-- │ index host_dispatches_session_idx
-- │      (session_id)
-- └────────────────────────────────────────────────────────────────────────────

-- host_dispatches: the downlink queue amad → host daemon, which also holds execution results.
--
-- The daemon is on the user's machine with outbound-only networking (HTTP long polling), so amad
-- cannot connect to it directly. So everything "the daemon should do" goes into this table, and the
-- daemon pulls it with the cursor carried by its heartbeat:
--
--   kind = exec        one call of the six tools (bash/read/write/...). Must be strongly reliable:
--                      tools may have side effects, and losing a result would lead to duplicate
--                      execution after Ark reclaims (the same worry as selfhost_tool_results; that
--                      ledger covers it at the outer layer).
--                      State machine queued → delivered → done; result holds the execution result.
--   kind = term_open / term_input / term_resize / term_close
--                      frames of the browser web terminal. Weakly reliable: late keystrokes are
--                      useless; even if redelivered after claimed_until expires (30s), the daemon
--                      drops old frames; no execution result is stored, ack means done, and a
--                      cleanup task deletes them periodically.
--
-- # Correctness relies only on this table; NOTIFY only makes it "a bit faster"
--
-- pg_notify('ama_host_wake', host_id) in the same transaction as inserting the row/writing the result
-- gives a sub-millisecond wake-up; a lost notification is not fatal: the daemon heartbeat itself is a
-- long poll (20s hold + a 5s database scan as fallback), and the exec waiter also has a 1s ticker. It
-- depends on no in-process state across instances.
--
-- # (host_id, seq) and redelivery
--
-- seq increases monotonically within a host; the counter is hosts.last_dispatch_seq (the enqueue
-- transaction holds a per-host advisory lock, takes greatest(current max(seq), counter)+1 and writes
-- it back; see coredao.EnqueueDispatch). **Do not take numbers by max(seq) in this table**: done rows
-- get purged entirely, and numbering would wrap back. The daemon persists a last_seq cursor and only
-- pulls rows with seq > last_seq. Rows that are delivered but whose claimed_until has expired (daemon
-- crashed / result POST lost) can be pulled again; the daemon deduplicates by call id, so redelivery
-- does not mean re-execution.
--
-- # Why payload / result are text, not jsonb
--
-- They are whole payloads stored and fetched as is (ExecSpec JSON / execution result JSON / terminal
-- frame JSON), and no query filters or sorts by internal fields; and result must be returned to the
-- upper layer byte for byte, while jsonb normalizes whitespace (see "byte-exact fidelity needs text"
-- in repository/schema/README.md; run_events.raw is the same precedent).
create table if not exists host_dispatches (
    id          text        not null,
    host_id     text        not null,
    seq         bigint      not null,

    -- exec / term_open / term_input / term_resize / term_close, no CHECK (consistent across the
    -- repository).
    kind        text        not null default '',
    -- queued / delivered / done.
    state       text        not null default '',

    -- exec: the Ark session id (== run id); the tool set routes and injects by it.
    session_id  text        not null default '',
    -- term_*: the browser terminal id (the in-process termID carried back in uplink frames, for
    -- cross-instance relay).
    terminal_id text        not null default '',

    -- The JSON payload sent to the daemon.
    payload     text        not null default '',
    -- The result JSON of exec; always empty for terminal frames.
    result      text        not null default '',

    created_at   timestamptz not null default now(),
    delivered_at timestamptz,
    -- Claim lease: once delivered, if not done past it, it can be redelivered (around 120s for exec,
    -- 30s for term).
    claimed_until timestamptz,
    updated_at  timestamptz not null default now(),

    constraint host_dispatches_pkey primary key (id)
);

-- The authoritative order for cursor pulling and dedup.
create unique index if not exists host_dispatches_host_seq_idx
    on host_dispatches (host_id, seq);

-- Daemon pulling: a host's "queued with seq > cursor" + "delivered with an expired lease".
-- seq comes last so results are returned in order within the same host.
create index if not exists host_dispatches_claim_idx
    on host_dispatches (host_id, state, claimed_until, seq);

-- hostwatcher uses it to look things up / correlate logs by session when handling a host that went
-- offline; closing a terminal also scopes by it.
create index if not exists host_dispatches_session_idx
    on host_dispatches (session_id);
