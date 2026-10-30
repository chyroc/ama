-- ┌── Table outline: agent_ark_secrets ────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ agent_id        text  not null
-- │ workspace_id    text  not null
-- │ api_key         text  not null
-- │ api_key_masked  text  not null default ''::text
-- │ region          text  not null default 'cn-beijing'::text
-- │ check_status    text  not null default 'unchecked'::text
-- │ checked_at      text  not null default ''::text
-- │ check_error     text  not null default ''::text
-- │ updated_at      text  not null default ''::text
-- │ updated_by      text  not null default ''::text
-- │
-- │ primary key agent_ark_secrets_pkey
-- │      (agent_id)
-- │ index agent_ark_secrets_ws_idx
-- │      (workspace_id)
-- └────────────────────────────────────────────────────────────────────────────

-- PostgreSQL table structure of agent-level Ark credentials.
--
-- Embedded by store_pg.go with go:embed and run together with the others when building a PGStore.
-- All statements are `if not exists`, **safe to run repeatedly**: the service runs them at every
-- startup.
--
-- # Why this table exists: different agents in one workspace may need different Ark accounts
--
-- Before, a key could at most be configured on a workspace. But agents in the same workspace often
-- belong to different business lines, and an Ark key is **the boundary of billing and quota**: two
-- business lines sharing one key cannot split their bills.
--
-- Hence a third level. The fallback order of the three levels is:
--
--	agent level  →  workspace level  →  account level (global)
--
-- The more specific wins. The only entry point for resolution is resolveArkSecretForAgent in
-- core/ark.go.
--
-- # Why a separate table instead of putting it into agents.doc
--
-- ⚠️ **`apimodel.Agent` is both the stored form and the HTTP response form** (`GET /api/agents/{id}`
-- writeJSONs it directly). A plain-text key in doc would leak to the frontend, and that leak gives no
-- error; only the network panel shows it.
--
-- Ark credentials have always relied on **types** to keep the plain text inside the server
-- (`ArkSecret` has the plain text / `ArkCredential` only has the mask, two types); putting the plain
-- text into a struct that "is serialized out anyway" would tear down that guarantee. So the plain
-- text gets its own table, and `agents.doc` only holds a publishable enum.
--
-- # The primary key is agent_id
--
-- One agent has at most one key of its own. workspace_id is included so deleting a workspace can
-- cascade the cleanup (agents are hard-deleted with no deleted_at, so nobody would collect orphan
-- rows).
--
-- # doc contains the plain-text API key
--
-- Same as ark_secrets / global_ark_secrets: that key must be **sent to Ark as is** for inference, not
-- used for verification; storing a hash would lose the usable key forever. (Compare auth_api_keys:
-- there others call us, and only comparison is needed, so only the sha256 is stored. The direction is
-- opposite, so is the way of storing.)
--
-- Before storing it is encrypted with shared/infra/cryptobox (`v1:` prefix); see sealKey in
-- store_pg.go.
--
-- # Why real columns instead of doc jsonb
--
-- The same rule as global_ark_secrets: the fields are just a few scalars, with no nested structure and
-- no "byte-sensitive HTTP contract" constraint (the external view is built by the Service on the
-- spot).

create table if not exists agent_ark_secrets (
    -- Which agent this credential belongs to. An agent has at most one.
    agent_id text primary key,

    -- The owning workspace; **deleting a workspace cascades the cleanup by it**.
    --
    -- No foreign key: other tables in this repository have none either (agents itself has none); the
    -- cascading delete is written out explicitly in DeleteWorkspace, so what gets deleted can be seen
    -- in one place.
    workspace_id text not null,

    -- The plain-text API key (ciphertext when stored; see "doc contains the plain-text" above).
    api_key text not null,

    -- The mask (like abcd****...****wxyz), which may go into responses.
    --
    -- Stored instead of computed every time: computing the mask needs the plain text, while read-only
    -- paths like list/details do not need to touch the plain text at all. One less place reading the
    -- plain text is one less leak surface.
    api_key_masked text not null default '',

    -- Region: cn-beijing / ap-southeast-1.
    --
    -- No CHECK constraint, for the same reason as global_ark_secrets: with it, adding a region would
    -- mean changing the DDL, and DDL does not rerun on databases where the table already exists (this
    -- repository has no version table). Bad values are caught by the read path.
    region text not null default 'cn-beijing',

    -- Validation status: unchecked / valid / invalid.
    check_status text not null default 'unchecked',
    -- Time and failure reason of the last validation, both fixed-length ISO strings (may be empty).
    checked_at  text not null default '',
    check_error text not null default '',

    -- When it last changed and who did it; credential config must leave a trace.
    --
    -- ⚠️ updated_at uses **fixed-length ISO text**, not timestamptz: this value goes straight into the
    -- HTTP response (the frontend's formatRelative shows "updated x minutes ago"), and this
    -- repository's external timestamps are always fixed-length ISO strings with three decimals (see
    -- the top of model/apimodel). Storing it as timestamptz and converting back would drop the .000,
    -- and the shape would no longer match the other endpoints.
    updated_at text not null default '',
    updated_by text not null default ''
);

-- Look up by workspace: deleting a workspace must also clear the credentials of all its agents.
create index if not exists agent_ark_secrets_ws_idx on agent_ark_secrets (workspace_id);
