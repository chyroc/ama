-- ┌── Table outline: schedules ────────────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id                 text                      not null
-- │ workspace_id       text                      not null
-- │ agent_id           text                      not null
-- │ status             text                      not null
-- │ next_run_at        timestamp with time zone
-- │ run_now_requested  boolean                   not null default false
-- │ claimed_by         text
-- │ claimed_until      timestamp with time zone
-- │ ord                bigint                    not null (sequence)
-- │ doc                jsonb                     not null
-- │
-- │ primary key schedules_pkey
-- │      (id)
-- │ index schedules_agent_idx
-- │      (agent_id, ord)
-- │ index schedules_due_idx
-- │      (next_run_at, ord) WHERE (status = 'active'::text)
-- │ index schedules_run_now_idx
-- │      (next_run_at, ord) WHERE run_now_requested
-- │ index schedules_workspace_idx
-- │      (workspace_id, ord)
-- └────────────────────────────────────────────────────────────────────────────
-- ├── Keys of the doc column (jsonb) (= json.Marshal(Schedule))
-- │ ⚠️ This block is **generated from the json tags of the Go type**, not by sampling data
-- │    in the database. Keys with omitempty are absent when the value is zero, so sampling
-- │    always misses them, yet they are part of the contract. After changing the type, run:
-- │        cd server && go run ./cmd/gen-doc-outline -w
-- │    Source of truth: model/apimodel/schedule.go, type Schedule
-- │
-- │ ⚠️ doc.nextRunAt / doc.runNowRequested are **stale copies**; the real columns with the
-- │    same names win. ClaimDueSchedules only updates the columns and does not rewrite doc:
-- │    a read-modify-write on the blob would lose updates under concurrency.
-- │
-- │ id                   string
-- │ workspaceId          string
-- │ agentId              string
-- │ agentName            string
-- │ name                 string
-- │ spec                 ScheduleSpec
-- │ prompt               string
-- │ contextSummary       string   ← omitempty, the key may be missing entirely
-- │ origin               ScheduleOrigin
-- │ status               ScheduleStatus
-- │ delivery             DeliveryTarget
-- │ nextRunAt            string   ← omitempty, the key may be missing entirely
-- │ runNowRequested      bool   ← omitempty, the key may be missing entirely
-- │ lastRunAt            string   ← omitempty, the key may be missing entirely
-- │ lastRunId            string   ← omitempty, the key may be missing entirely
-- │ lastError            string   ← omitempty, the key may be missing entirely
-- │ runCount             int
-- │ failCount            int
-- │ consecutiveFailures  int
-- │ createdAt            string
-- │ updatedAt            string
-- │ createdBy            string   ← omitempty, the key may be missing entirely
-- │ updatedBy            string   ← omitempty, the key may be missing entirely
-- │ agentDeleted         bool   ← omitempty, the key may be missing entirely
-- ┤──────────────────────────────────────────────────────────────────────────

-- Scheduled tasks.
--
-- # Scalar columns + doc
--
-- ⚠️ **This is an existing shape, not a model for new tables.** The current rule is "expand fields
-- into columns, join tables for one-to-many"; see "Default rules for table design" in README.md in
-- this directory. schedules followed those six tables back then; if this table were built today it
-- should be all real columns (like the issues / suggestions domains).
--
-- "Scalar columns + doc jsonb holding the whole entity": the extracted columns only serve filtering
-- and sorting, doc is the authoritative data, and the read path only deserializes doc. ord bigserial
-- gives "insertion order" a sortable anchor, because SQL rows have no inherent order while MemStore
-- returns insertion order.
--
-- # What differs from those six tables: next_run_at is timestamptz, not text
--
-- runs.started_at is an **opaque string** (fixed-length .000Z), because the frontend sorts it as a
-- string. Here it is the other way: a real timestamptz, because **claiming needs time comparison in
-- the database**:
--
--     where next_run_at <= now() and (claimed_until is null or claimed_until < now())
--
-- A string column could compare too (the lexical order of fixed-length ISO equals time order), but
-- then "now" would also have to be formatted into the same string and passed in, so "what is now"
-- would be decided by the application: with multiple instances, clocks on different machines drift,
-- and A would think it is due while B thinks it is not yet. timestamptz + now() makes **the database
-- the only clock**, the only choice that makes sense in a distributed setting.
--
-- The cost is converting at the boundary: apimodel.Schedule.NextRunAt is an ISO string (the shape the
-- frontend wants), converted to and from time.Time when storing and reading; see isoToTime / timeToISO
-- in store_pg_schedule.go. The nextRunAt in doc is still a string; SaveSchedule writes both together,
-- and **the column wins**.

create table if not exists schedules (
    id           text primary key,
    workspace_id text not null,
    agent_id     text not null,
    -- status is extracted because claiming only looks at active ones, so it goes into the index.
    status text not null,
    -- next_run_at may be NULL: a paused task has no next trigger time, and **it is also cleared at the
    -- moment of claiming** (see ClaimDueSchedules). NULL is naturally excluded by
    -- `next_run_at <= now()`, exactly the meaning we want, with no need for an extra is not null.
    next_run_at timestamptz,

    -- run_now_requested marks "this trigger is a test run clicked by a person"; it is cleared at the
    -- moment of claiming.
    --
    -- **Its only reason to exist is telling "a manual test run" apart from "the next_run_at left over
    -- from before the pause".** Claiming only accepts active (paused ones do not run), while
    -- "configure first, click once to see if it is right, then enable" requires paused tasks to be
    -- test-runnable once. Without this column both paths are broken:
    --
    --   * not loosening the status condition → run-now returns 200 for paused and changes next_run_at,
    --     while the task is never claimed, showing up as "I clicked test run and nothing happened",
    --     with zero errors;
    --   * loosening it unconditionally to "also claim paused when due" → every task runs right away
    --     **the moment it is paused**, because the next_run_at from before the pause is still in the
    --     past.
    --
    -- The claim condition is "(active or this column) and next_run_at is due", so a leftover true does
    -- not make a task run out of nowhere: pausing clears next_run_at, and without it nothing is
    -- claimed.
    run_now_requested boolean not null default false,

    -- -- Claim lease --------------------------------------------------------
    --
    -- These two columns are **the scheduler's run-time state and not in doc**: the frontend does not
    -- need to know which instance is running it, and they change far more often than the entity
    -- itself.
    --
    -- claimed_by is only for debugging (matching which instance claimed it in the logs); what really
    -- decides ownership is claimed_until: while the lease has not expired it belongs to someone else,
    -- once expired anyone can grab it. **It must have an expiry**: the instance may be rebuilt halfway
    -- (release, failed health check, scale-down), and then nobody releases the lock; only lease expiry
    -- makes the task runnable again.
    --
    -- ⚠️ **"Once expired anyone can grab it" was once only a wish in this comment; the SQL did not do
    -- it.** Claiming clears next_run_at, while the claim's where requires next_run_at to be non-null,
    -- so the interrupted record was filtered out by the first condition and the lease expiry check was
    -- never reached; the symptom was the task silently stopping, with all four status fields looking
    -- normal. Now ClaimDueSchedules covers this with the union of two branches, "normally due or zombie
    -- recovery", and **claimed_by therefore went from "only for debugging" to taking part in the
    -- decision** (non-empty means it was claimed before, used to tell zombies from "never scheduled").
    -- Read the long comment above that SQL before changing it.
    claimed_by    text,
    claimed_until timestamptz,

    ord bigserial not null,
    doc jsonb     not null
);

-- ListSchedules lists by workspace; ord keeps the order the same as MemStore.
create index if not exists schedules_workspace_idx on schedules (workspace_id, ord);

-- The main claim query: find active ones that are due.
--
-- Only active rows are indexed (a partial index): paused tasks never take part in claiming, so letting
-- them take index space makes no sense. next_run_at ascending puts "the ones most due" first, and with
-- limit it takes one batch at a time.
create index if not exists schedules_due_idx
    on schedules (next_run_at, ord)
    where status = 'active';

-- The manual test run branch: paused rows clicked for a test run, which the partial index above does
-- not cover.
--
-- **A new index was added on purpose, instead of changing the predicate of schedules_due_idx to
-- `status = 'active' or run_now_requested`.** The reason is that `create index if not exists` only
-- looks at the name and **does not change the predicate of an existing index**: written that way it
-- would be right in new databases, while in databases that already have it that index would still
-- only contain active rows: the DDL reports no error, and the change silently does not take effect.
-- Changing the predicate would need a drop and recreate, with a window without the index in between.
-- An index with a new name is naturally safe to re-run, and PG combines the two partial indexes with
-- BitmapOr.
create index if not exists schedules_run_now_idx
    on schedules (next_run_at, ord)
    where run_now_requested;

-- SchedulesForAgent looks up the tasks under an agent, for cascading cleanup when the agent is
-- deleted.
create index if not exists schedules_agent_idx on schedules (agent_id, ord);
