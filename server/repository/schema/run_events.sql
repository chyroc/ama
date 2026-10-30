-- ┌── Table outline: run_events ───────────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ run_id     text    not null
-- │ sequence   bigint  not null
-- │ type       text    not null
-- │ thread_id  text
-- │ doc        jsonb   not null
-- │ raw        text    not null
-- │ event_id   text
-- │
-- │ primary key run_events_pkey
-- │      (run_id, sequence)
-- │ unique run_events_event_id_idx
-- │      (run_id, event_id)
-- └────────────────────────────────────────────────────────────────────────────

create table if not exists run_events (
    run_id text not null,
    -- sequence is a monotonically increasing number within a session. It is part of the primary
    -- key, so a run has no duplicate sequence.
    sequence bigint not null,
    type     text not null,
    -- thread_id may be NULL: the session_thread_id of session-level events (session.status_*) is
    -- null. When filtering by thread, NULL is naturally excluded by `thread_id = $1`, matching
    -- MemStore.
    thread_id text,
    -- doc is a snapshot of the event's index fields, for checking the database directly when
    -- debugging; **the read path does not deserialize it**, and index fields always come from the
    -- three typed columns above.
    doc jsonb not null,
    -- raw is the authoritative data; see the "exception" in the file header. text instead of jsonb,
    -- for byte-exact fidelity.
    raw text not null,
    -- event_id is the event id issued by Ark (sevt-... / call_... / evt-...), extracted from raw.
    --
    -- **Its only purpose is that unique index**; see run_events_event_id_idx below.
    -- It may be NULL: frames that cannot be decoded (the fallback branch of NewRunEventFromArk) have
    -- no id; such frames are still stored as is and take no part in deduplication: NULLs are never
    -- equal to each other in a PostgreSQL unique index.
    event_id text,
    primary key (run_id, sequence)
);

-- One event can only have one row within a run.
--
-- # This index stops a real failure, not a theoretical nicety
--
-- One Ark session can have several subscribers **at the same time**: Feishu inbound
-- (feishu/inbound.go), Feishu resume (feishu/resume.go), the browser's SSE (core/handler.go
-- streamRunEvents), scheduled tasks (core/scheduler.go). Each subscriber does "receive a frame →
-- store a row", and sequence is handed out by the Store itself (current max +1), so two subscribers
-- receiving **the same** event each get a new sequence number and each write a row: the primary key
-- (run_id, sequence) does not conflict at all, and both rows go in. The same reply then shows several
-- times in the UI, while single-instance and unit tests are all green; only real use triggers it.
--
-- So let the database decide (CLAUDE.md: "creation with remote side effects → unique constraint +
-- ON CONFLICT"), instead of checking and then writing in Go: check-then-write is TOCTOU, and both
-- subscribers' SELECTs would say "not there yet".
create unique index if not exists run_events_event_id_idx on run_events (run_id, event_id);
