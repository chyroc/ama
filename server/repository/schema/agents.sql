-- ┌── Table outline: agents ───────────────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id             text     not null
-- │ workspace_id   text     not null
-- │ ord            bigint   not null (sequence)
-- │ doc            jsonb    not null
-- │ name           text
-- │ description    text
-- │ status         text
-- │ version        integer
-- │ created_at     text
-- │ updated_at     text
-- │ created_by     text
-- │ updated_by     text
-- │ executor_type  text
-- │ host_id        text
-- │ host_source    text
-- │
-- │ primary key agents_pkey
-- │      (id)
-- │ index agents_feishu_idx
-- │      (((doc -> 'feishuAppIds'::text)))
-- │ index agents_workspace_idx
-- │      (workspace_id, ord)
-- └────────────────────────────────────────────────────────────────────────────
-- ├── Keys of the doc column (jsonb) (= json.Marshal(Agent))
-- │ ⚠️ This block is **generated from the json tags of the Go type**, not by sampling data
-- │    in the database. Keys with omitempty are absent when the value is zero, so sampling
-- │    always misses them, yet they are part of the contract. After changing the type, run:
-- │        cd server && go run ./cmd/gen-doc-outline -w
-- │    Source of truth: model/apimodel/apimodel.go, type Agent
-- │
-- │ id              string
-- │ workspaceId     string
-- │ name            string
-- │ description     string
-- │ system          string
-- │ model           ModelConfig
-- │ version         int
-- │ builtin_tools   []string
-- │ skillIds        []string
-- │ feishuAppIds    []string
-- │ status          AgentStatus
-- │ integrationIds  []string
-- │ executorType    string   ← omitempty, the key may be missing entirely
-- │ hostId          string   ← omitempty, the key may be missing entirely
-- │ hostSource      string   ← omitempty, the key may be missing entirely
-- │ envs            []AgentEnv
-- │ envAliases      []AgentEnvAlias
-- │ followUp        FollowUpMode
-- │ progressDetail  ProgressDetail
-- │ createdAt       string
-- │ updatedAt       string
-- │ createdBy       string   ← omitempty, the key may be missing entirely
-- │ updatedBy       string   ← omitempty, the key may be missing entirely
-- ┤──────────────────────────────────────────────────────────────────────────

-- # Split between columns and doc
--
-- The eight scalars name / description / status / version / created_at / updated_at / created_by /
-- updated_by, plus the three executor columns, are real columns; doc only keeps structs and arrays
-- (system / model / skillIds / feishuAppIds / envs / envAliases / followUp / progressDetail /
-- integrationIds etc.).
--
-- # Why the two time columns are text while those of workspaces are timestamptz
--
-- **The rule is "who uses this column"** (see the "Timestamps" section of CLAUDE.md). workspaces'
-- created_at **replaces ord** as the sort key, and its consumer is the database; **agents keeps ord**,
-- so the only consumer of these two columns is the fixed three-decimal ISO string in the HTTP
-- contract.
--
-- # Why the scalar columns are nullable
--
-- created_by / updated_by must keep the difference between "never recorded" (NULL) and "present, just
-- empty" (''); for the other columns both NULL and empty string become a Go empty string, with no
-- change in behavior.
--
-- ⚠️ **status has no CHECK / no PG enum**, consistent across the repository. **An empty string must
-- mean "enabled"** (see the passage on AgentStatus.IsDisabled: writing `!= active` would make every
-- agent that never had a status stop replying).
--
-- ⚠️⚠️ **version is Ark's optimistic lock, not a revision number** (an update must send back the
-- current version, and every update adds one). No rollback logic may overwrite it, otherwise the next
-- sync to Ark hits 409.
create table if not exists agents (
    id           text primary key,
    workspace_id text not null,
    ord          bigserial not null,
    doc          jsonb not null,
    name         text,
    description  text,
    status       text,
    version      integer,
    created_at   text,
    updated_at   text,
    created_by   text,
    updated_by   text,
    -- Tool executor: empty = Docker container (default); 'host' = sent through amad long polling to
    -- the user machine host_id points to (see the three tables hosts / workspace_hosts /
    -- host_dispatches). host_id is a soft reference: after the machine is revoked, the read path
    -- reports it as invalid by "binding does not exist" and blocks it when saving the agent; no
    -- foreign key (same as runs.host_id).
    executor_type text,
    host_id       text,
    -- Host source, two levels of binding:
    --   workspace = assigned by the workspace: the machine is bound into the workspace in
    --               workspace_hosts, and an admin can assign it to any agent;
    --   personal  = direct personal link: the agent's creator attaches a machine they registered
    --               directly to the agent, without going through the workspace.
    -- The gate is service/agent.ensureExecutorAllowed. NULL is always interpreted as workspace by
    -- apimodel.EffectiveHostSource; no decision may bypass that function and check for empty itself.
    host_source   text
);

create index if not exists agents_workspace_idx on agents (workspace_id, ord);

-- AgentsBoundTo looks up "agents bound to some Feishu app", using jsonb array containment.
-- The GIN index lets doc->'feishuAppIds' @> to_jsonb($1) use the index instead of a full table scan.
create index if not exists agents_feishu_idx on agents using gin ((doc -> 'feishuAppIds'));

-- **There is no unique constraint "one Feishu app can only be bound to one agent" here; it is not an
-- oversight.**
--
-- That business rule is real (an inbound message can only have one responder; see
-- ensureFeishuAppsFree in core/agent.go), and by this repository's rules the database should decide
-- instead of checking existence in Go. But PostgreSQL cannot express it; both paths are blocked,
-- measured:
--
--   create unique index on agents ((jsonb_array_elements_text(doc->'feishuAppIds')));
--     ERROR:  set-returning functions are not allowed in index expressions
--
--   alter table agents add constraint ... exclude using gist ((<converted to text[]>) with &&);
--     ERROR:  data type text[] has no default operator class for access method "gist"
--     (btree_gist only adds opclasses for scalar types, not arrays)
--
-- For the database to decide, the binding would have to become a table with "one row per binding"
-- and feishu_app_id as the primary key. Then the same relation would be stored in two places (the doc
-- array here + that table), and SaveAgent / DeleteAgent / Reset / Clear would all have to keep them
-- in sync; missing one means the two disagree, and the symptom of that is exactly "the page shows it
-- is bound, but it does not reply". So it is not done for now; instead it is "validated by the
-- Service on write + tolerated with a warning on read", with the cost written plainly in
-- ensureFeishuAppsFree.
