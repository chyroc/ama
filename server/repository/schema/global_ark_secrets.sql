-- ┌── Table outline: global_ark_secrets ───────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ email           text  not null
-- │ api_key         text  not null
-- │ api_key_masked  text  not null default ''::text
-- │ region          text  not null default 'cn-beijing'::text
-- │ check_status    text  not null default 'unchecked'::text
-- │ checked_at      text  not null default ''::text
-- │ check_error     text  not null default ''::text
-- │ updated_at      text  not null default ''::text
-- │
-- │ primary key global_ark_secrets_pkey
-- │      (email)
-- └────────────────────────────────────────────────────────────────────────────

-- PostgreSQL table structure of account-level Ark credentials.
--
-- Embedded by store_pg.go with go:embed and run together with the others when building a PGStore.
-- All statements are `if not exists`, **safe to run repeatedly**: the service runs them at every
-- startup.
--
-- # Why this table exists: "configure once globally, reference from workspaces"
--
-- Before, an Ark key could only be configured on a workspace (ark_secrets, primary key
-- workspace_id). Someone with five workspaces had to paste the same key five times, and replace it
-- in each one when the key changed; missing one shows up as every conversation in that workspace
-- suddenly failing, while they think they already replaced it everywhere.
--
-- Now there is also an account-level one: configure it once, and a workspace just chooses to
-- "reference" it.
--
-- # ⚠️ The workspace side **keeps no copy**; that is the whole implementation of linking
--
-- ark_secrets.doc has an extra source field (inherit / override). With inherit, that row **has no
-- plain-text key**, and reading falls back to this table in the Service layer (resolveArkSecret in
-- core/ark.go).
--
-- So "change the global one, and the workspaces referencing it follow" needs no sync logic at all:
-- the workspace has no old value to sync. "Copy it into the workspace" would be the exact opposite:
-- changing the global one would not propagate, which is exactly the problem to solve.
--
-- # The primary key is email, not workspace_id
--
-- The user's email is the Actor ID (ActorFrom in auth/context.go), lower-cased at the entry point.
-- The same kind as user_onboarding / user_preferences.
--
-- # doc contains the plain-text API key
--
-- Same as ark_secrets. **Storing only a hash is impossible here**: that key must be sent to Ark as is
-- for inference, not used for verification; storing a hash would lose the usable key forever.
-- (Compare auth_api_keys: there others call us, and only comparison is needed, so only the sha256 is
-- stored. The direction is opposite, so is the way of storing.)
--
-- The plain text cannot get into any HTTP response, and that is guaranteed by **types**, not by
-- self-discipline: the external view apimodel.GlobalArkCredential has no plain-text field at all
-- (same as feishu.App not holding AppSecret).
--
-- # Why real columns instead of doc jsonb
--
-- The same rule as user_onboarding / ark_skill_uploads: the fields are just a few scalars, with no
-- nested structure and no "byte-sensitive HTTP contract" constraint (the external view is built by
-- the Service on the spot); real columns are plainer than jsonb, and changing one column does not
-- need deserializing the whole row.
create table if not exists global_ark_secrets (
    -- Who this credential belongs to. **This is the Actor ID**, already lower-cased.
    email text primary key,

    -- The plain-text API key. See "why storing only a hash is impossible" above.
    api_key text not null,

    -- The mask (like abcd****...****wxyz), which may go into responses.
    --
    -- Stored instead of computed every time: computing the mask needs the plain text, while read-only
    -- paths like list/details do not need to touch the plain text at all. One less place reading the
    -- plain text is one less leak surface.
    api_key_masked text not null default '',

    -- Region: cn-beijing / ap-southeast-1.
    --
    -- No CHECK constraint, for the same reason as user_preferences: with it, adding a region would
    -- mean changing the DDL, and DDL does not rerun on databases where the table already exists (this
    -- repository has no version table). Bad values are caught by the read path.
    region text not null default 'cn-beijing',

    -- Validation status: unchecked / valid / invalid.
    check_status text not null default 'unchecked',
    -- Time and failure reason of the last validation, both fixed-length ISO strings (may be empty).
    checked_at  text not null default '',
    check_error text not null default '',

    -- When it last changed, a fixed-length ISO string.
    --
    -- ⚠️ **Deliberately different** from user_preferences' timestamptz: this value goes straight into
    -- the HTTP response (the frontend's formatRelative shows "updated x minutes ago"), and this
    -- repository's external timestamps are always fixed-length ISO strings with three decimals (see
    -- the top of model/apimodel). Storing it as timestamptz and converting back would drop the .000,
    -- and the shape would no longer match the other endpoints.
    updated_at text not null default ''
);
