-- ┌── Table outline: feishu_inflight_turns ────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ run_id           text                      not null
-- │ lark_app_id      text                      not null
-- │ chat_id          text                      not null
-- │ src_message_id   text                      not null
-- │ card_message_id  text
-- │ reply_in_thread  boolean                   not null
-- │ last_text        text                      not null
-- │ status           text                      not null
-- │ retry_count      integer                   not null
-- │ started_at       timestamp with time zone  not null
-- │ updated_at       timestamp with time zone  not null
-- │ agent_id         text
-- │
-- │ primary key feishu_inflight_turns_pkey
-- │      (run_id)
-- │ index feishu_inflight_turns_status_started_idx
-- │      (status, started_at)
-- └────────────────────────────────────────────────────────────────────────────

-- feishu_inflight_turns: conversation rounds in progress, **existing to survive process restarts**.
--
-- # Why it must be stored in the database
--
-- All the state of a round used to live only in the cardUpdater of inbound.go (process memory). Once
-- amad restarts (release, scaling, crash), that round is lost completely, and three things happen at
-- once:
--
--   1. The card stays at "thinking" forever. PATCHing a card already sent needs its message_id, and
--      that ID was only in memory; after a restart there is no anchor to change it again.
--      runAndReply's finishing logic (updater.finish) also disappears with the process, and the
--      fallback "this round produced no reply" never runs.
--   2. **That inbound message is poisoned forever.** Its row in feishu_inbound_messages stays
--      pending, and ClaimInboundMessage treats pending as duplicate delivery and acks it right away,
--      so the user resending and Feishu redelivering are all blocked, and this sentence can never be
--      asked again. So recovery **must** change that row along the way; see resume.go.
--   3. The run history keeps a zombie record that is "in progress" forever.
--
-- # Why a new table instead of adding columns to feishu_inbound_messages
--
-- Because this is **another entity**: a conversation round, with a lifetime different from that
-- inbound message (one run_id runs many rounds), and the fields do not overlap at all.
--
-- # Why no doc jsonb
--
-- Following feishu_inbound_messages: it is not an external entity, has no HTTP contract, and the
-- fields are just a few scalars; real columns are plainer than jsonb. The "whole struct into doc"
-- approach above was to keep the response shape, and there is no response here.
create table if not exists feishu_inflight_turns (
    -- run_id is the primary key, and **it is Ark's session id** (see CreateConversation in
    -- core/chat.go: apimodel.Run.ID is taken straight from the Ark session id, with no second
    -- identifier). So with it you can both look up the local run record and start a compensating
    -- fetch from Ark directly.
    run_id           text        not null,
    -- lark_app_id is the Feishu app this round belongs to. Recovery uses it to get the AppSecret and
    -- exchange a tenant_access_token: the token expires in two hours and must never be stored and
    -- reused; only what is needed to rebuild it is stored.
    lark_app_id      text        not null,
    -- chat_id is the conversation the message is in, for locating logs and, in the future, "sending
    -- plain text when even a card cannot be sent".
    chat_id          text        not null,
    -- src_message_id is the user's message (om_xxx). Two uses: replying to create the card when none
    -- exists yet, and **undoing the poisoning in item 2 above**: it is exactly the primary key of
    -- feishu_inbound_messages.
    src_message_id   text        not null,
    -- card_message_id is the message ID of the bot's reply card, i.e. the PATCH target.
    --
    -- **It may be NULL, and NULL has its own meaning**: nothing has been produced yet and the card was
    -- never created. Recovery branches on it: with a value, PATCH that zombie card directly (the user
    -- sees the same card come back to life); without one, reply again to create the card, with the
    -- idempotency key "ama-run:"+run_id letting Feishu deduplicate, avoiding a second card from the
    -- window "the card was created but we crashed before writing this column back".
    card_message_id  text,
    -- reply_in_thread records whether the card was created as a thread reply. A rebuilt card must
    -- match, otherwise the two cards of the same round would be one in the thread and one in the group.
    reply_in_thread  boolean     not null,
    -- last_text is the content already shown on the card. Recovery uses it as the starting point and
    -- continues appending, so the half the user already read is not cleared and redone.
    last_text        text        not null,
    -- status is the claim state: inflight (unfinished, can be claimed) / resuming (claimed by some
    -- instance).
    --
    -- It is not the "conversation state": that is in the runs table, driven by Ark events. This column
    -- only serves **mutual exclusion between instances**: every instance scans, and claims with a CAS
    -- on it; see ClaimInflightTurn.
    --
    -- No CHECK: the value set is guaranteed by Go-side constants, consistent with other tables in this
    -- repository.
    status           text        not null,
    -- retry_count is how many times it was resumed automatically. Each claim adds 1; when the limit is
    -- reached it gives up and tells the user to resend, so a round that "never finishes" does not run
    -- for nothing on every restart.
    retry_count      int         not null,
    -- started_at is the start of this round, used to judge "stuck too long".
    --
    -- The limit is 1 hour: past that, the execution container has very likely been deleted by the
    -- reclaim inspection and the workspace reset; that round really cannot continue, and resuming would
    -- only give an agent confused in an empty directory.
    started_at       timestamptz not null,
    -- updated_at is the last write time, purely for debugging (one look tells which step it is stuck
    -- at).
    updated_at       timestamptz not null,
    -- agent_id: which agent ran this round, only for the recovery path. After a restart resumeLive only
    -- has this record, and it needs to ask "is this agent configured to 'not show detailed work'"
    -- (apimodel.ProgressDetail); without this column it could not ask. When the agent cannot be found,
    -- the upper layer fails open to "show".
    agent_id         text,

    constraint feishu_inflight_turns_pkey primary key (run_id)
);

-- The startup scan uses this composite index: first filter unfinished ones by status, then by
-- started_at ascending (the ones interrupted first are recovered first, keeping the causal
-- intuition).
create index if not exists feishu_inflight_turns_status_started_idx
    on feishu_inflight_turns (status, started_at);

comment on column feishu_inflight_turns.card_message_id is
    'Message ID of the reply card (the PATCH target). NULL means the card has not been created yet; not the same as an empty string.';
comment on column feishu_inflight_turns.status is
    'Claim status inflight / resuming, only used for mutual exclusion across instances; the conversation state lives in the runs table.';
comment on column feishu_inflight_turns.started_at is
    'Start of the turn. After more than 1 hour (the order of the execution container idle-reclaim limit) it is not resumed, because the workspace may already be gone.';
comment on column feishu_inflight_turns.agent_id is
    'The agent running this turn. When NULL/empty the resume path falls back to "show the chain of thought".';
