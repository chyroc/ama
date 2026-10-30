-- ┌── Table outline: agent_sync_tasks ─────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id                text                      not null
-- │ workspace_id      text                      not null
-- │ agent_id          text                      not null
-- │ status            text                      not null
-- │ failure_reason    text                      not null default ''::text
-- │ failure_stage     text                      not null default ''::text
-- │ attempt_count     integer                   not null default 0
-- │ resync_requested  boolean                   not null default false
-- │ claimed_by        text
-- │ claimed_until     timestamp with time zone
-- │ created_at        timestamp with time zone  not null
-- │ updated_at        timestamp with time zone  not null
-- │ finished_at       timestamp with time zone
-- │ ord               bigint                    not null (sequence)
-- │
-- │ primary key agent_sync_tasks_pkey
-- │      (id)
-- │ unique agent_sync_tasks_active_idx
-- │      (workspace_id, agent_id) WHERE (finished_at IS NULL)
-- │ index agent_sync_tasks_agent_idx
-- │      (agent_id, created_at DESC, ord DESC)
-- │ index agent_sync_tasks_due_idx
-- │      (created_at, ord) WHERE (finished_at IS NULL)
-- │ index agent_sync_tasks_finished_idx
-- │      (finished_at) WHERE (finished_at IS NOT NULL)
-- └────────────────────────────────────────────────────────────────────────────

-- agent_sync_tasks: the task ledger for "sync the agent to Ark right after saving it".
--
-- # Why it is needed: there used to be a zero-error gap between saving and taking effect
--
-- An Ark Agent is created once and cached forever; `ensureBinding` reuses an existing binding as soon
-- as it finds one, and **catching up only happens the moment a new conversation starts**
-- (syncBindingSkills / syncBindingSpec in core/chat.go). `Service.UpdateAgent` only writes the
-- database and does `Version++` from start to finish, never touching the runner.
--
-- So the user edits the prompt and clicks save, PATCH returns 200, the database really changed, **and
-- nothing happens on the Ark side** until the next time someone starts a new conversation. It shows up
-- as "I changed it but it acts as if I did not", with zero errors.
--
-- This table makes it **happen on save**: enqueue a task, the background claims and runs it, and the
-- result comes back here for the frontend to query.
--
-- # Why a new table instead of putting a few keys into agents.doc
--
-- Three reasons; the second is the hard one:
--
--   1. `agents` is a table with doc, and adding one more key to doc is exactly the default path the
--      README names, "zero DDL, zero errors, mergeable the same day"; this is where the rule is most
--      easily broken.
--   2. **Tasks must be claimed concurrently**: `for update skip locked` must select rows by status /
--      claimed_until, which must be real columns that can go into an index. Inside jsonb the claim
--      statement could not even be written.
--   3. Different lifetimes. An agent is a long-lived entity, a task is a short-lived record that must
--      be cleaned up by finished_at after it runs.
--
-- All fields can be listed (the state machine's fields are our own), so **there is not a single
-- jsonb**, following the default rule of repository/schema/README.md.
--
-- # The same claim semantics as schedules / pending_messages
--
-- The two columns claimed_by / claimed_until mean exactly the same thing: the former is only for
-- debugging, and **what really decides ownership is claimed_until**. The instance may be rebuilt in the
-- middle of a sync (release / scale-down / failed health check), and then nobody releases it; only
-- lease expiry lets another instance take over. That is the only way "a background goroutine started
-- on some instance" can heal itself with multiple instances.
create table if not exists agent_sync_tasks (
    -- id is the task's own identifier (astask_xxx); the frontend polls with it.
    id text primary key,

    -- workspace_id / agent_id are the sync target.
    --
    -- No foreign key, like other tables in this repository: cascading cleanup when an agent is
    -- deleted is written out explicitly in DeleteAgent, so what gets deleted can be seen in one place.
    workspace_id text not null,
    agent_id     text not null,

    -- status: pending / running / succeeded / failed.
    --
    -- ⚠️ **succeeded rows must be kept; they cannot be "deleted on success" like pending_messages.** The
    -- consumer of that table is the server itself (once sent nobody asks again), while the consumer of
    -- this table is **frontend polling**: if deleted, the frontend's next tick finds no row at all, and
    -- has to guess between "the sync succeeded" and "the task is not stored yet". And the wrong guess is
    -- especially bad: showing success as "still syncing", spinning until timeout. Keeping it leaves only
    -- one check: read that row's status. Cleanup is left to the finished_at index.
    status text not null,

    -- failure_reason is the failure reason, **a sentence (in Chinese) shown directly to the user**
    -- (same as core's error message convention). Empty string on success.
    failure_reason text not null default '',

    -- failure_stage is which step failed: credential / environment / agent / skills / spec.
    --
    -- A separate column instead of folding it into failure_reason: the frontend gives different
    -- guidance by it (credential problems send the user to workspace settings, Ark service failures
    -- ask them to retry), and working out "which step" from a Chinese sentence could only be done by
    -- string matching, which is bound to drift.
    failure_stage text not null default '',

    -- attempt_count is how many times it has run. At the limit (a core-side constant) it lands in
    -- failed, with no endless retries.
    --
    -- Without it, an agent that can never sync (e.g. its key was revoked) would be reclaimed by the
    -- lease again and again and rerun every period, flooding both Ark and the logs.
    attempt_count int not null default 0,

    -- resync_requested means "while this round was running, the user saved once more".
    --
    -- ⚠️ **This column cannot be dropped, otherwise the change of that save is silently lost.** If an
    -- enqueue finds a running task, it cannot change that task's state (that would steal someone
    -- else's lease and make two instances sync at once), nor simply open a new one (the partial unique
    -- index blocks it). So it sets this flag, and after that round finishes another round runs. Without
    -- it the symptom is: a change the user saved during a sync only takes effect at the next new
    -- conversation, which is exactly the symptom this whole table is meant to eliminate.
    resync_requested boolean not null default false,

    -- claimed_by / claimed_until are the claim lease, same semantics as schedules / pending_messages.
    claimed_by    text,
    claimed_until timestamptz,

    -- created_at / updated_at use timestamptz: they are only used by the database for sorting and
    -- timeout checks, not in the HTTP contract (times on the contract side are computed by core into
    -- ISO strings). See "Timestamps: unit and format" in CLAUDE.md.
    created_at timestamptz not null,
    updated_at timestamptz not null,

    -- finished_at is when it entered a final state, only for cleanup. NULL when not final.
    finished_at timestamptz,

    -- ord orders tasks enqueued at the same moment (a fallback when created_at's precision is not
    -- enough).
    ord bigserial not null
);

-- ⚠️⚠️ This partial unique index is the **only** judge of "two clicks in a row queue only one task".
--
-- Enqueuing uses `on conflict do nothing`, letting the database decide "does this agent already have
-- an unfinished task". **Do not fall back to "query, then create"**: that is TOCTOU; two instances (or
-- the same user clicking twice) would each find "none" and each create one, so the same agent gets
-- synced by two instances at once.
--
-- The predicate is `finished_at is null` (not `status in (...)`): the final state and finished_at are
-- written in the same write, so using it as the predicate only checks one column, and it exactly
-- complements the predicate of the cleanup index below.
create unique index if not exists agent_sync_tasks_active_idx
    on agent_sync_tasks (workspace_id, agent_id)
    where finished_at is null;

-- The main query for claiming pending tasks: unfinished ones in enqueue order.
--
-- ⚠️ **Do not change the predicates of these two indexes**: `create index if not exists` only looks at
-- the name and **does not change the predicate of an existing index**; it would be right in new
-- databases while databases that already have it keep the old predicate: the DDL reports no error,
-- and the change silently does not take effect. To change the predicate use a new name (see the
-- post-mortems in pending_messages.sql and on schedules_run_now_idx).
create index if not exists agent_sync_tasks_due_idx
    on agent_sync_tasks (created_at, ord)
    where finished_at is null;

-- Frontend polling looks up the latest one per agent (including finished ones), and cascading cleanup
-- when a workspace/agent is deleted.
create index if not exists agent_sync_tasks_agent_idx
    on agent_sync_tasks (agent_id, created_at desc, ord desc);

-- Clean up old records.
create index if not exists agent_sync_tasks_finished_idx
    on agent_sync_tasks (finished_at)
    where finished_at is not null;

comment on column agent_sync_tasks.status is
    'Task status pending / running / succeeded / failed; succeeded rows are kept so the frontend can poll them.';
comment on column agent_sync_tasks.resync_requested is
    'The user saved again while this round was running, so run one more round afterwards; without this flag that change would be lost silently.';
