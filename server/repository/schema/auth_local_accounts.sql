-- ┌── Table outline: auth_local_accounts ──────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ email          text                      not null
-- │ password_hash  text                      not null
-- │ is_admin       boolean                   not null default false
-- │ bootstrap      boolean                   not null default false
-- │ created_by     text                      not null default ''::text
-- │ created_at     timestamp with time zone  not null
-- │ updated_at     timestamp with time zone  not null
-- │
-- │ primary key auth_local_accounts_pkey
-- │      (email)
-- │ unique auth_local_accounts_bootstrap_uniq
-- │      (bootstrap) WHERE bootstrap
-- └────────────────────────────────────────────────────────────────────────────

-- -- Local accounts (email + password) -----------------------------------------
--
-- Only stores what "can log in with a password" needs. Display name and avatar are still in users
-- (the profile shared by all login methods) and are not repeated here.
--
-- # Why not add password_hash to users
--
-- users is "who this person is"; people logging in with OIDC also have a row, and they have no
-- password. Putting the password column there would give most rows a credential column that is
-- always empty, and "can this row log in with a password" would be answered by an emptiness check:
-- miss it in one place and "an empty password can log in". With a separate table, "does this row
-- exist" is the answer.
create table if not exists auth_local_accounts (
    -- Lower-cased email, which is the Actor ID (see persistmodel.NormalizeEmail).
    email text primary key,

    -- bcrypt hash (with cost and salt, like $2a$12$...). **Neither plain text nor a reversible
    -- ciphertext is stored**.
    password_hash text not null,

    -- Platform admin: can create other local accounts when self sign-up is closed.
    -- The first registered account is an admin automatically (see bootstrap).
    is_admin boolean not null default false,

    -- "This is the first account registered on a fresh deployment". **A unique index ensures at most
    -- one row is true**; this is the gate where the database decides when "two people race to
    -- register the first account": see CreateFirstLocalAccount in authdao, an
    -- insert ... on conflict do nothing with a not exists. not exists alone cannot stop concurrency
    -- (both sides read "nobody"); only the unique index can.
    bootstrap boolean not null default false,

    -- Who created this account: empty for self sign-up, the admin's email when created by an admin.
    -- For debugging.
    created_by text not null default '',

    created_at timestamptz not null,
    updated_at timestamptz not null
);

create unique index if not exists auth_local_accounts_bootstrap_uniq
    on auth_local_accounts (bootstrap) where bootstrap;
