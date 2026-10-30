-- ┌── Table outline: users ────────────────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ email       text                      not null
-- │ name        text                      not null default ''::text
-- │ avatar_url  text                      not null default ''::text
-- │ created_at  timestamp with time zone  not null
-- │ updated_at  timestamp with time zone  not null
-- │
-- │ primary key users_pkey
-- │      (email)
-- └────────────────────────────────────────────────────────────────────────────

-- -- User profiles --------------------------------------------------------------
--
-- The **cross-session source of truth** for "what is the name of the person with this email, and
-- which avatar".
--
-- # Relation to name/avatar_url in auth_sessions
--
-- auth_sessions also has these two fields, but its **primary key is token_hash with expires_at, and
-- it disappears at logout or expiry**, with one row per device. It stores "a snapshot at the moment of
-- this login", not this person's profile. So:
--
--   auth_sessions  one row per login, disappears   ← for authentication, not touched this time
--   users          one row per person, long-lived  ← for profiles, this one
--
-- Keeping both is not redundancy: the session one must follow "which identity this login used"
-- (changing it would affect the authentication path), the users one must follow "what this person is
-- called now". At login an upsert goes into users, and each holds on its own.
--
-- # Why the primary key is email instead of a separate id
--
-- **The Actor is the user's email**, globally unique; authz permission checks, workspace_members
-- links and auth_api_keys ownership all use it. Four account-level tables in the database already
-- have this shape (user_onboarding / user_preferences / global_ark_secrets all have email as the
-- primary key).
--
-- With a separate id, **no table would reference it**: workspace_members stores member_email. That
-- would be one more key nobody uses but that must be kept unique; and this repository's id generator
-- (core's nextID) is an in-process counter, not guaranteed unique across instances; agents has
-- already hit the trap "delete and recreate, collide on the same id" because of it.
--
-- # Rows with "only email, the rest to be filled in" are allowed
--
-- **Do not design it as "a row only exists after login"**: inviting someone who has never logged in
-- into a workspace is a normal operation, and then only the email is known. So name / avatar_url
-- default to empty strings, and empty shell rows are a valid state; they are filled in by an upsert
-- at the first login. The frontend already copes with empty avatars (Radix's Avatar.Fallback shows
-- the initial; see the comment on apimodel.WorkspaceMember.AvatarURL).
--
-- ⚠️ **workspace_members.member_email deliberately has no foreign key to here**: the identity of the
-- fixed API Key (AMA_API_KEY_EMAIL) is a pure config string that never goes through login, so it
-- never has a users row. With a foreign key, e2e / CI / probes would all fail, and the error would
-- point to the foreign key, not to "this identity never logged in".
create table if not exists users (
    -- The user's email, **which is the Actor ID**, already lower-cased (see auth.normalizeEmail).
    email text primary key,

    -- Display name and avatar. **May be empty**: that is the state of someone invited who has not
    -- logged in yet.
    name       text not null default '',
    avatar_url text not null default '',

    created_at timestamptz not null,
    updated_at timestamptz not null
);
