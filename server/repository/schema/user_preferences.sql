-- ┌── Table outline: user_preferences ─────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ email       text                      not null
-- │ language    text                      not null default 'zh-CN'::text
-- │ appearance  text                      not null default 'system'::text
-- │ updated_at  timestamp with time zone  not null
-- │
-- │ primary key user_preferences_pkey
-- │      (email)
-- └────────────────────────────────────────────────────────────────────────────

-- PostgreSQL table structure of user preferences (language and appearance).
--
-- Embedded by store_pg.go with go:embed and run together with the others when building a PGStore.
-- All statements are `if not exists`, **safe to run repeatedly**: the service runs them at every
-- startup.
--
-- # Why it is stored in the database (localStorage is not enough)
--
-- These two preferences used to be only in localStorage (`ama.prefs.v1`). Once the requirement became
-- "configure once, globally", that place no longer worked: switch computers, switch browsers, clear
-- the cache once, and the setting is gone. Users think "I set dark mode on this platform", not "I set
-- it in this browser on this machine".
--
-- It is the same kind of account-level data as user_onboarding, and copies its shape (primary key
-- email, real columns instead of jsonb, times as timestamptz).
--
-- # ⚠️ localStorage is not removed; it is demoted to a cache
--
-- **This is not a "delete it after migrating" change.** The frontend applies the appearance on
-- <html> before mounting (before the first frame in main.tsx), when there is no login yet, let alone
-- a network round trip. Relying only on the database, every page open would flash light first and
-- then switch to dark, which is exactly why localStorage existed in the first place.
--
-- So both are written: the database is authoritative (across devices), localStorage is a local cache
-- (to avoid the first-frame flash). Reading renders with the cache first, then corrects with the
-- database copy after login.
--
-- # The primary key is email, and there is no difference between "not set" and "set to the default"
--
-- The user's email is the Actor ID (ActorFrom in auth/context.go), lower-cased at the entry point.
--
-- Both columns are **not null + default**, not nullable: this differs from user_onboarding's
-- completed_at/skipped_at, which must tell "never happened" from "happened", while here "never set a
-- language" and "set it to Chinese" are **exactly the same** result for the UI. Creating that
-- distinction would only add a state nobody consumes.
--
-- A user who never saved preferences reads as ErrNotFound; the Service returns zero values → the
-- frontend gets the defaults (zh-CN + system).
create table if not exists user_preferences (
    -- Who the preferences belong to. **This is the Actor ID**, already lower-cased.
    email text primary key,

    -- UI language: zh-CN / en-US.
    --
    -- **No CHECK constraint**, consistent with the other enum columns in this repository
    -- (feishu_apps.doc's triggerMode has none either). Reason: with it, adding a language would mean
    -- changing the DDL, and DDL does not rerun on databases where the table already exists (this
    -- repository has no version table; everything is create ... if not exists). So a new language
    -- could be stored in new databases and hit the constraint in databases where the table already
    -- exists: two environments behaving differently, much worse than storing a bad value.
    --
    -- Bad values are caught by the **read path**: the Service's normalize falls back to the default
    -- when it does not recognize one, the same approach as the field-by-field checks in the
    -- frontend's loadPrefs().
    language text not null default 'zh-CN',

    -- Appearance: light / dark / system. Default system, matching the frontend's DEFAULT_PREFS.
    --
    -- ⚠️ This default **must be system**. Changing it to light would force every user who never saved
    -- preferences from "follow the system" to light the next time they open it; they never touched any
    -- switch, and dark mode users would feel someone changed their settings.
    appearance text not null default 'system',

    -- When it last changed. For debugging ("when was this written").
    --
    -- The time is passed in from the Go side, not SQL's now(), consistent with the auth / feishu /
    -- onboarding tables: time is a business rule and should not follow the database's time zone
    -- config, and tests must be able to inject it.
    updated_at timestamptz not null
);
