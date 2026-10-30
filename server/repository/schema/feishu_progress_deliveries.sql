-- ┌── Table outline: feishu_progress_deliveries ───────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ run_id                     text                      not null
-- │ lark_app_id                text                      not null
-- │ chat_id                    text                      not null
-- │ origin_message_id          text                      not null
-- │ reply_in_thread            boolean                   not null default false
-- │ transport                  text                      not null
-- │ status                     text                      not null
-- │ status_reason              text
-- │ remote_cot_id              text
-- │ remote_message_id          text
-- │ desired_revision           bigint                    not null default 1
-- │ applied_revision           bigint                    not null default 0
-- │ applied_snapshot           jsonb
-- │ terminal_requested         boolean                   not null default false
-- │ terminal_applied           boolean                   not null default false
-- │ lease_token                text
-- │ lease_until                timestamp with time zone
-- │ create_attempted_at        timestamp with time zone
-- │ attempt_count              integer                   not null default 0
-- │ consecutive_failure_count  integer                   not null default 0
-- │ next_retry_at              timestamp with time zone
-- │ last_error                 text
-- │ created_at                 timestamp with time zone  not null
-- │ updated_at                 timestamp with time zone  not null
-- │ finished_at                timestamp with time zone
-- │
-- │ primary key feishu_progress_deliveries_pkey
-- │      (run_id)
-- │ index feishu_progress_deliveries_claim_idx
-- │      (terminal_requested DESC, next_retry_at, created_at) WHERE (status = ANY (ARRAY['queued'::text, 'processing'::text, 'active'::text]))
-- │ index feishu_progress_deliveries_finished_idx
-- │      (finished_at) WHERE (finished_at IS NOT NULL)
-- └────────────────────────────────────────────────────────────────────────────

-- feishu_progress_deliveries: the delivery state machine of agent progress (CoT / card summary).
--
-- One conversation produces one record; it is responsible for delivering the agent's thinking process
-- to Feishu live, and guarantees: no duplicate creation, no lost final state, no fights between
-- instances, and it can continue after a crash.
--
-- # Why a table instead of running in memory
--
-- Progress delivery is an operation **with remote side effects that may last several minutes**. In
-- memory, a process restart would leave the CoT message already created stuck at "thinking" forever:
-- nobody knows its cot_id, so it can neither be appended to nor closed, and the user sees a message
-- spinning forever.
--
-- # Three groups of fields for three problems
--
-- **1. Snapshot replay (applied / desired / staged).** A PUT every few characters the agent produces
-- would overwhelm Feishu. So progress is first collected into "snapshots", and only pushed when the
-- content really changed. The three version numbers satisfy `applied <= staged <= desired`: desired is
-- the latest wanted, staged is being pushed, applied is confirmed pushed. After a crash it continues
-- from applied, without pushing again what was already pushed.
--
-- **2. Lease (lease_token / lease_until).** Several instances handling the same delivery at once would
-- reorder events and even create CoT twice. Only the lease holder may act, and the lease expires, so
-- others can take over if the holder crashes. Split with that Redis lock for the long connection: that
-- one decides "who receives messages", this one decides "who pushes this progress".
--
-- **3. The creation-uncertain window (create_attempted_at + status=create_uncertain).** When creating
-- the CoT times out or gets 5xx, we **do not know whether the other side created it**. Retrying right
-- away might put two chains of thought in the group, giving up right away might leave one spinning
-- forever. So the attempt time is recorded, it enters the create_uncertain state, and the decision
-- comes after the window; see ErrCOTOutcomeUnknown in shared/infra/lark/cot.go.
--
-- No CHECK: the value set is guaranteed by Go-side constants, consistent with the other tables in this
-- file (another project wrote twenty-odd CHECKs; our DDL convention does not).
create table if not exists feishu_progress_deliveries (
    -- run_id is the Ark session ID; one record per conversation, so it is the primary key.
    run_id            text        not null,
    -- lark_app_id / chat_id are the delivery target.
    lark_app_id       text        not null,
    chat_id           text        not null,
    -- origin_message_id is the user message that triggered this round; the CoT hangs under it.
    origin_message_id text        not null,
    -- reply_in_thread: whether to open a thread.
    reply_in_thread   boolean     not null default false,

    -- transport is the delivery method: message_cot (main path) or card_summary (fallback).
    transport         text        not null,
    -- status: queued / processing / active / completed / failed / create_uncertain.
    status            text        not null,
    -- status_reason is the reason for entering the final state, for debugging.
    status_reason     text,

    -- remote_cot_id / remote_message_id are the identifiers on the Feishu side.
    -- message_cot has both; card_summary only has the latter.
    remote_cot_id     text,
    remote_message_id text,

    -- The three snapshot fields; see "1" above.
    desired_revision  bigint      not null default 1,
    applied_revision  bigint      not null default 0,
    applied_snapshot  jsonb,
    -- terminal_requested / terminal_applied: whether the final state was requested / pushed.
    -- The ordering gate of reply delivery looks at terminal_applied (see inbound.go).
    terminal_requested boolean    not null default false,
    terminal_applied   boolean    not null default false,

    -- Lease; see "2" above.
    lease_token       text,
    lease_until       timestamptz,

    -- Creation-uncertain window; see "3" above.
    create_attempted_at timestamptz,

    -- Retry count and backoff.
    attempt_count             int  not null default 0,
    consecutive_failure_count int  not null default 0,
    next_retry_at     timestamptz,
    last_error        text,

    created_at        timestamptz not null,
    updated_at        timestamptz not null,
    finished_at       timestamptz,

    constraint feishu_progress_deliveries_pkey primary key (run_id)
);

-- Claim pending deliveries: final states first (the user is waiting for the last bit), then by retry
-- time.
create index if not exists feishu_progress_deliveries_claim_idx
    on feishu_progress_deliveries (terminal_requested desc, next_retry_at, created_at)
    where status in ('queued', 'processing', 'active');

-- Clean up old records.
create index if not exists feishu_progress_deliveries_finished_idx
    on feishu_progress_deliveries (finished_at)
    where finished_at is not null;
