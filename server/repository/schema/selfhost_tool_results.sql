-- ┌── Table outline: selfhost_tool_results ────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ session_id    text                      not null
-- │ call_id       text                      not null
-- │ state         text                      not null
-- │ result_event  jsonb                     not null default '{}'::jsonb
-- │ created_at    timestamp with time zone  not null default now()
-- │ updated_at    timestamp with time zone  not null default now()
-- │
-- │ primary key selfhost_tool_results_pkey
-- │      (session_id, call_id)
-- │ index selfhost_tool_results_updated_idx
-- │      (updated_at)
-- └────────────────────────────────────────────────────────────────────────────

-- selfhost_tool_results: the "result ledger" of tools executed by the self_hosted worker.
--
-- # Why this table is needed
--
-- When the worker runs a tool **with side effects** like bash in the execution container, the tool
-- may have finished while its result has not been sent back to Ark yet, and at that moment the
-- process is killed by a release/scale-down/health check. If a new instance reclaims the same work
-- and blindly reruns it, the tool **runs again** (charging money again, sending a message again). So
-- the execution state of each tool_use must be persisted:
--
--   started → execution started (killed at this step, unknown whether it finished; do not dare rerun)
--   result  → the tool finished and its result is stored (the taker resends the result, no rerun)
--   sent    → the result was sent back to Ark successfully (skip)
--
-- # Why it must be in PG, not the SDK's default local file ledger
--
-- The SDK (arkruntime's FileToolResultStore) writes the ledger to the worker instance's local disk by
-- default, `<workdir>/.ma_self_host_worker/tool_ledger/`. That holds under the single-machine
-- assumption "the worker is tied to a local disk", and is wrong at two levels in a multi-instance
-- deployment:
--
--   1. **Local disk is throwaway and not shared across instances.** When the work is reclaimed by
--      another instance, it cannot read the previous instance's local disk, and the ledger does not
--      exist for it: the "duplicate execution across restarts" it should prevent is not prevented at
--      all across instances; and the ledger is gone once the instance is rebuilt.
--   2. **With the path squashed into one global share, it "poisons across sessions".** Calling the
--      SDK's HandleItem with useWorkdirAsSession=true makes all concurrent work share one ledger. If
--      one work is killed before sending back and leaves a state=result leftover, then **every time
--      that instance picks up work of another session**, the runner's startup Recover() POSTs that
--      foreign result to the new session first; the new session has no such call, Ark returns 400
--      InvalidPayload, the send gate immediately declares the whole work dead, the leftover can never
--      be cleared, and all later sessions get poisoned one after another.
--
-- In PG: every instance can read it (really preventing duplicate execution); isolated by
-- (session_id, call_id), so Recover only takes rows of the current session, and foreign results
-- physically cannot cross over; nothing is lost on instance rebuild.
--
-- # Fields
--
-- There are no separate business columns to query (this is the worker's internal state, not in the
-- HTTP contract); reads are all point lookups by the (session_id, call_id) primary key, and
-- result_event is a whole event payload stored and fetched as is, stored as one jsonb block, which
-- fits the rule in repository/schema/README.md (no filter/sort needs, payload shape decided by the
-- SDK).
create table if not exists selfhost_tool_results (
    session_id text not null,
    call_id    text not null,

    -- started / result / sent, values matching the three states of the SDK's FileToolResultStore.
    state text not null,

    -- result_event is the user.tool_result event to resend to Ark (has a value in result/sent
    -- states). The started state stores a '{}' placeholder, keeping not null simple.
    result_event jsonb not null default '{}'::jsonb,

    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now(),

    primary key (session_id, call_id)
);

-- Cleaning up old ledger entries: rows in sent state are useless once sent (they will never need to be
-- resent by Recover); rows in started/result state that have not moved for a long time mean the work
-- was reclaimed on the Ark side, and keeping them only takes space. The worker pool periodically cleans
-- rows past the retention period by updated_at.
create index if not exists selfhost_tool_results_updated_idx
    on selfhost_tool_results (updated_at);
