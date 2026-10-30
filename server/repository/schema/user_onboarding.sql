-- ┌── Table outline: user_onboarding ──────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ email         text                      not null
-- │ completed_at  timestamp with time zone
-- │ skipped_at    timestamp with time zone
-- │ version       integer                   not null default 0
-- │ updated_at    timestamp with time zone  not null
-- │
-- │ primary key user_onboarding_pkey
-- │      (email)
-- └────────────────────────────────────────────────────────────────────────────

-- PostgreSQL table structure of onboarding completion state.
--
-- Embedded by store_pg.go with go:embed and run together with the other three when building a
-- PGStore. All statements are `if not exists`, **safe to run repeatedly**: the service runs them at
-- every startup.
--
-- # Why this table is required (neither existing storage works)
--
-- The requirement is "the same person is never onboarded twice", so "onboarded or not" must outlive a
-- session and also a browser. The database originally had **no account-level table at all**, only two
-- session-level ones:
--
--   * auth_sessions: the primary key is token_hash with expires_at, **gone at logout or expiry**.
--     Storing the state there is as good as not storing it: a user logging out and back in would be
--     onboarded again.
--
-- The frontend preferences (ama.prefs.v1) do not work either: that is localStorage, and switching
-- browsers / clearing the cache / switching devices would all repeat onboarding. And "being onboarded
-- again" is exactly the main failure mode of this feature.
--
-- # Why it is in core/ and not auth/
--
-- Because the two HTTP routes reading and writing it can only be mounted on core. auth.Handler.Register
-- registers on the **bare mux** (httpd/server.go), deliberately bypassing Protect and Guard: those are
-- the five "before login" endpoints. Onboarding endpoints require login and must go through the
-- protectedMux → Guard chain, and only core and feishu handlers are on that chain. The storage follows
-- the handler, so auth does not have to depend on core in reverse.
--
-- # The primary key is email
--
-- The user's email is the Actor ID (see ActorFrom in auth/context.go), globally unique. The value was
-- lower-cased by auth at the entry point (auth/normalizeEmail), so it is used directly as the primary
-- key here without normalizing again.
--
-- The primary key also serves as **the idempotency key**: two concurrent "mark complete" calls hit the
-- unique constraint and the database decides, instead of checking then writing in Go (that is TOCTOU,
-- and with multiple instances double writes would happen now and then).
--
-- # Why real columns instead of doc jsonb
--
-- The same rule as ark_skill_uploads: the fields are just a few scalars; real columns are plainer than
-- jsonb. There is one more hard reason here: **the upsert must merge by column**: completed_at and
-- skipped_at each get their own coalesce, which one stored doc cannot do.
--
-- # Times are timestamptz, not text
--
-- Consistent with the two auth tables. These timestamps **take no part in frontend sorting** (the
-- frontend only checks "is there a value"), so the "must be a fixed-length ISO string" constraint does
-- not apply; and for future operational queries by time (e.g. "how many newcomers finished onboarding
-- this week"), timestamptz can compare directly and use indexes.
--
-- Note "now" is passed in from the Go side, not SQL's now(), consistent with the auth / feishu
-- tables: time is a business rule and should not follow the database's time zone config, and tests
-- must be able to inject time.

create table if not exists user_onboarding (
    -- Who the onboarding state belongs to. **This is the Actor ID**, already lower-cased.
    email text primary key,

    -- When onboarding was finished. **Nullable**: NULL until it is finished.
    completed_at timestamptz,

    -- When onboarding was skipped. **Nullable**, same as above.
    --
    -- Two columns with completed_at instead of one status field: both give the same answer to "should
    -- we still onboard" ("no"), but the distinction **cannot be recovered afterwards**: if later we
    -- want a lighter hint for "people who skipped" and nothing for "people who finished", only separate
    -- records can support it. With one field, someone who skipped first and later finished on their
    -- own would only keep the last state.
    skipped_at timestamptz,

    -- The **onboarding content version** the user has seen.
    --
    -- This is the answer to "what about a later redesign": when the onboarding content changes, the
    -- frontend bumps ONBOARDING_VERSION by one, so people with a lower version (**including those who
    -- finished the previous version**) get onboarded once more, and those who saw the new version do
    -- not. Without this field there would only be two choices: everyone again, or nobody.
    --
    -- The point of default 0: people with no record read as zero, naturally lower than any version, so
    -- "newcomers" and "people who saw an old version" go through the same check, with no need to tell
    -- NULL from 0.
    --
    -- **The comparison is done in the frontend, not here**. The onboarding content is in the frontend,
    -- so the frontend should own that version number; the backend only stores the value it is told.
    version int not null default 0,

    -- When it last changed. For operational queries and debugging ("when was this record written").
    updated_at timestamptz not null
);
