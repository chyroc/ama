-- ┌── Table outline: workspace_members ────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id            text                      not null
-- │ workspace_id  text                      not null
-- │ member_email  text                      not null
-- │ email_key     text                      not null
-- │ role          text                      not null
-- │ name          text                      not null default ''::text
-- │ avatar_url    text                      not null default ''::text
-- │ created_at    timestamp with time zone  not null
-- │ updated_at    timestamp with time zone  not null
-- │
-- │ primary key workspace_members_pkey
-- │      (id)
-- │ unique workspace_members_email_key_idx
-- │      (workspace_id, email_key)
-- │ index workspace_members_email_lookup_idx
-- │      (email_key)
-- │ index workspace_members_ws_order_idx
-- │      (workspace_id, created_at, id)
-- └────────────────────────────────────────────────────────────────────────────

-- Workspace member table: one row = one person who can enter this workspace.
--
-- # email_key is the column used for duplicate and permission checks
--
-- member_email stores what the admin typed, as is (may contain upper case, shown as is in the UI);
-- email_key stores the same email **lower-cased with whitespace removed**, and the unique key and
-- permission checks compare by it. Writes all go through coredao's emailKeyOf, so the invariant "the
-- same person has only one row in the same workspace" rests on the unique index
-- (workspace_id, email_key), decided by the database.
create table if not exists workspace_members (
    id           text primary key,
    workspace_id text not null,
    member_email text not null,
    email_key    text not null,
    role         text not null,
    name         text not null default '',
    avatar_url   text not null default '',
    created_at   timestamptz not null,
    updated_at   timestamptz not null
);

create unique index if not exists workspace_members_email_key_idx
    on workspace_members (workspace_id, email_key);

-- The member list is shown in join order (firstOwnerEmail also takes the first owner).
create index if not exists workspace_members_ws_order_idx
    on workspace_members (workspace_id, created_at, id);

-- Used for the reverse lookup "which workspaces is this person in".
create index if not exists workspace_members_email_lookup_idx
    on workspace_members (email_key);
