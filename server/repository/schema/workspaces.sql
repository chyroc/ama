-- ┌── Table outline: workspaces ───────────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id           text                      not null
-- │ key          text                      not null
-- │ description  text                      not null default ''::text
-- │ created_at   timestamp with time zone
-- │ updated_at   timestamp with time zone
-- │ name         text
-- │ created_by   text
-- │ updated_by   text
-- │
-- │ primary key workspaces_pkey
-- │      (id)
-- │ index workspaces_created_idx
-- │      (created_at, id)
-- │ unique workspaces_key_unique
-- │      (key)
-- └────────────────────────────────────────────────────────────────────────────

-- Workspaces.
--
-- # Why sort by (created_at, id) instead of ord like the other tables
--
-- SQL rows have no inherent order, and the list needs a **total-order** sort key (see the ord passage
-- in schema.go). created_at only has millisecond precision (the application gives NowISO(), exactly
-- three decimals), so two rows created in the same millisecond are bound to tie; the primary key id
-- breaks the tie: (created_at, id) is a total order with no ties.
--
-- ⚠️ **The role of id here is "determinism", not "time order".** nextID interleaves across instances
-- and goes back after a restart, so the relative order of two rows in the same millisecond may not be
-- the real insertion order, but it **is the same for every query**, which is exactly what ord is meant
-- to guarantee. This tradeoff holds for workspaces (one person creating two workspaces in the same
-- millisecond is not a real scenario). **Do not apply this reasoning to schedules / issues**: the
-- former's ord is in ClaimDueSchedules' `order by next_run_at, ord ... for update skip locked`, the
-- pivot of correctness for multi-instance grabbing; for the latter, store_pg_issue.go says plainly
-- "cannot be replaced by created_at".
--
-- # Why name / created_by / updated_by are nullable
--
-- NULL means "this value was never recorded", an empty string means "there is a value, it is just
-- empty"; the two must be kept apart. The Go side reads NULL as an empty string (scanWorkspaceRow
-- goes through sql.NullString).
--
-- description is `not null default ''`: the write path writes ws.Description directly, with no
-- NULL/empty string distinction.
create table if not exists workspaces (
    id text primary key,
    -- key is globally unique. CreateWorkspace must return ErrConflict when it clashes.
    key text not null constraint workspaces_key_unique unique,
    description text not null default ''::text,
    created_at timestamp with time zone,
    -- The initial value equals created_at: "never changed".
    updated_at timestamp with time zone,
    name text,
    created_by text,
    updated_by text
);

-- Sort index, matching ListWorkspaces' `order by created_at, id`.
create index if not exists workspaces_created_idx on workspaces (created_at, id);
