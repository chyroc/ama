-- ┌── Table outline: auth_api_keys ────────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id            text                      not null
-- │ key_hash      text                      not null
-- │ email         text                      not null
-- │ name          text                      not null default ''::text
-- │ prefix        text                      not null default ''::text
-- │ created_at    timestamp with time zone  not null
-- │ last_used_at  timestamp with time zone
-- │
-- │ primary key auth_api_keys_pkey
-- │      (id)
-- │ index auth_api_keys_email_idx
-- │      (email, created_at DESC)
-- │ unique auth_api_keys_hash_unique
-- │      (key_hash)
-- └────────────────────────────────────────────────────────────────────────────

-- -- API Keys created by users -------------------------------------------------
--
-- **The same security model** as the session table above: it stores the sha256 of the plain text,
-- not the plain text itself. So even if this table is read, no usable key can be recovered. To check,
-- the incoming key is hashed again and looked up in the unique index.
--
-- # Not the same thing as AMA_API_KEY in config
--
--   AMA_API_KEY   a process-level **singleton**, one key set by an environment variable, for e2e /
--                 CI / probes. Not stored.
--   this table    created by users themselves, **many of them**, revocable, with last-used time.
--
-- Having both channels is deliberate; see the file header of auth/apikey.go.
--
-- # Keys are bound to a person, not a workspace, so there is no workspace_id here
--
-- **This is not a missing column.** Permissions are already decided by authz from "this person's role
-- in this workspace"; a key is just another way to prove "who I am". Making it workspace-level would
-- create two parallel permission models (key scope vs member role) that will sooner or later
-- disagree, and the disagreement shows up as "an endpoint is sometimes 403, sometimes 200", which
-- does not point to the real cause. See auth/apikey.go.
--
-- The UI lives in workspace settings (that is where users manage things), but the data model is
-- account-level.
create table if not exists auth_api_keys (
    -- The id of this record (an opaque string starting with ak_). Revoking uses it; **it is not the
    -- key itself**.
    id text primary key,

    -- sha256 of the plain text (hex, 64 characters).
    --
    -- **The unique constraint is not optional**: it guarantees the same key is never stored twice,
    -- and it is the index of the check path: every request with an API Key looks it up once, and
    -- without an index that is a full table scan.
    key_hash text not null constraint auth_api_keys_hash_unique unique,

    -- Who this key belongs to, **which is also its identity**. Already lower-cased (see
    -- auth.normalizeEmail).
    email text not null,

    -- Display name, to help users tell keys apart (e.g. ci-deploy).
    name text not null default '',

    -- The first few characters of the plain text (amak_xxxxxx), **only to recognize which key it is in
    -- the list**. It carries only 6 characters of information, leaving more than 250 bits of entropy;
    -- the full key cannot be recovered.
    prefix text not null default '',

    created_at timestamptz not null,

    -- The last time it was used to call an endpoint.
    --
    -- **Nullable, and null means "never used"**: users find keys they can revoke by it. A default
    -- time would merge "never used" with "used", and after that they could never be told apart.
    last_used_at timestamptz
);

-- Used to list all keys of a person (newest first by creation time).
--
-- The descending order is in the index instead of only (email): listing is the only read path of
-- this table besides checking, and it can follow the index order directly without an extra sort.
create index if not exists auth_api_keys_email_idx on auth_api_keys (email, created_at desc);
