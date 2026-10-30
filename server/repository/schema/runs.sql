-- ┌── Table outline: runs ─────────────────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id            text                      not null
-- │ workspace_id  text                      not null
-- │ agent_id      text                      not null
-- │ status        text                      not null
-- │ started_at    text                      not null
-- │ title         text                      not null
-- │ agent_name    text                      not null
-- │ ord           bigint                    not null (sequence)
-- │ doc           jsonb                     not null
-- │ schedule_id   text
-- │ watch_by      text
-- │ watch_until   timestamp with time zone
-- │ host_id       text
-- │ host_source   text
-- │ rescue_count  integer                   not null default 0
-- │
-- │ primary key runs_pkey
-- │      (id)
-- │ index runs_schedule_idx
-- │      (schedule_id, started_at DESC, ord) WHERE (schedule_id IS NOT NULL)
-- │ index runs_watchable_idx
-- │      (watch_until, started_at) WHERE (status = ANY (ARRAY['running'::text, 'rescheduling'::text, 'requires_action'::text]))
-- │ index runs_workspace_started_idx
-- │      (workspace_id, started_at DESC, ord)
-- └────────────────────────────────────────────────────────────────────────────
-- ├── Keys of the doc column (jsonb) (= json.Marshal(Run))
-- │ ⚠️ This block is **generated from the json tags of the Go type**, not by sampling data
-- │    in the database. Keys with omitempty are absent when the value is zero, so sampling
-- │    always misses them, yet they are part of the contract. After changing the type, run:
-- │        cd server && go run ./cmd/gen-doc-outline -w
-- │    Source of truth: model/apimodel/apimodel.go, type Run
-- │
-- │ id              string
-- │ workspaceId     string
-- │ agentId         string
-- │ agentName       string
-- │ title           string
-- │ status          RunStatus
-- │ trigger         RunTrigger
-- │ triggerDetail   string   ← omitempty, the key may be missing entirely
-- │ scheduleId      string   ← omitempty, the key may be missing entirely
-- │ hostId          string   ← omitempty, the key may be missing entirely
-- │ hostSource      string   ← omitempty, the key may be missing entirely
-- │ startedAt       string
-- │ lastActivityAt  string   ← omitempty, the key may be missing entirely
-- │ endedAt         string   ← omitempty, the key may be missing entirely
-- │ durationMs      *int64   ← omitempty, the key may be missing entirely
-- │ usage           TokenUsage
-- │ threads         []RunThread
-- │ eventCount      int
-- │ errorSummary    string   ← omitempty, the key may be missing entirely
-- │ agentDeleted    bool   ← omitempty, the key may be missing entirely
-- │ agentDisabled   bool   ← omitempty, the key may be missing entirely
-- ┤──────────────────────────────────────────────────────────────────────────

create table if not exists runs (
    id           text primary key,
    workspace_id text not null,
    agent_id     text not null,
    status       text not null,
    -- started_at is an **opaque string** (like 2026-08-25T10:00:00.000Z), not timestamptz.
    -- A fixed-length ISO string with three decimals: lexical order equals time order; converting to
    -- timestamptz and back would drop the .000, which changes the sort result directly. See the
    -- comment at the top of model/apimodel.
    started_at text not null,
    title      text not null,
    agent_name text not null,
    ord        bigserial not null,
    doc        jsonb not null,

    -- Points back to the scheduled task that triggered this round. **Only for filtering**; the
    -- authoritative data is still in doc, like agent_id / status. Schedule only stores LastRunID
    -- (overwritten on every fire); without this column, the runs of several tasks in a workspace would
    -- be mixed together with no way to tell them apart. NULL for runs not triggered by a scheduled
    -- task.
    schedule_id text,

    -- The claim lease of the long-running subscription (see core/runwatcher.go): the server must
    -- attach a long-running subscription to sessions "still running but watched by nobody" to store
    -- events into history; with multiple instances "who subscribes" must have an answer, otherwise
    -- every instance attaches one and wastes Ark long connections. Same semantics as schedules'
    -- claimed_by / claimed_until: watch_by is only for debugging, what really decides ownership is
    -- watch_until, which **must have an expiry**: the instance may be rebuilt during the subscription,
    -- and then nobody releases it; only lease expiry lets another instance take over.
    --
    -- It is on the runs table instead of a separate table: it is run-time state of the run, with the
    -- same lifetime as the run, and not in the HTTP contract.
    watch_by    text,
    watch_until timestamptz,

    -- The executor snapshotted from the agent when the conversation was created
    -- (core.createConversation): host id and source (workspace / personal). Later executor changes on
    -- the agent do not rewrite past runs. Empty = Docker container. Web terminal tickets are tiered by
    -- host_source (container/workspace machine = admin, personal machine = owner only), and the read
    -- path interprets it with apimodel.EffectiveHostSource. Deliberately a soft reference, no foreign
    -- key.
    host_id     text,
    host_source text,

    -- Rescue count: how many times the failover→reclaim self-healing chain failed on this run.
    -- Each time inspection resends an orphaned tool result it is +1 (runrescue.go). It is a
    -- **distributed counter**: failover's watchdog is on the executing instance, and the final rescue
    -- may be on another instance, so an in-process count would not match up.
    -- The first rescue is a normal safety net (Warn); a second consecutive rescue of the same run
    -- means the self-healing chain "hand back to Ark for redispatch → a healthy worker really runs it"
    -- has broken, and it must escalate to Error for a person to step in, instead of silently resending
    -- while the model fails round after round. Not in doc: it is run-time state, and not in the HTTP
    -- contract.
    rescue_count integer not null default 0
);

-- The main query of ListRuns: filter by workspace + started_at descending.
--
-- **The order of this index is equivalent to MemStore's descending string sort, provided this
-- database's Collate is C.** C compares bytes, matching Go's `a > b` (also by bytes) position by
-- position. With a locale collation like zh_CN.utf8 the comparison results may differ; the ama
-- database is created with C (see POSTGRES_INITDB_ARGS in docker-compose.yml and
-- shared/infra/pg/README.md); it cannot be changed, and do not change it.
create index if not exists runs_workspace_started_idx on runs (workspace_id, started_at desc, ord);

-- Look up the run history of a schedule.
--
-- A partial index (`where schedule_id is not null`): the vast majority of runs come from chat/Feishu,
-- and for them this column is always NULL; letting those rows take index space makes no sense.
--
-- The sort key **deliberately differs** from ListRuns' order by (that one is
-- `coalesce(nullif(doc->>'lastActivityAt',''), started_at) desc`, which an expression index would not
-- match). This only aims to fetch the set "runs of one task" quickly and leaves sorting to the result
-- set: the history of a single scheduled task is a small result set, the same tradeoff as in
-- ListRuns.
create index if not exists runs_schedule_idx
    on runs (schedule_id, started_at desc, ord)
    where schedule_id is not null;

-- The main claim query: find runs "still running and either unclaimed or with an expired lease".
--
-- The partial index only holds runs still running: idle / terminated never take part in claiming,
-- and letting them take index space makes no sense (same tradeoff as schedules_due_idx).
--
-- ⚠️ `create index if not exists` only looks at the name and **does not change the predicate of an
-- existing index**. To change the predicate, use a new name.
create index if not exists runs_watchable_idx
    on runs (watch_until, started_at)
    where status in ('running', 'rescheduling', 'requires_action');
