-- ┌── Table outline: auth_oidc_states ─────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ state          text                      not null
-- │ nonce          text                      not null
-- │ code_verifier  text                      not null
-- │ return_to      text                      not null default ''::text
-- │ used_at        timestamp with time zone
-- │ created_at     timestamp with time zone  not null
-- │ expires_at     timestamp with time zone  not null
-- │
-- │ primary key auth_oidc_states_pkey
-- │      (state)
-- │ index auth_oidc_states_expires_idx
-- │      (expires_at)
-- └────────────────────────────────────────────────────────────────────────────

-- -- OIDC logins in progress (Authorization Code + PKCE) ----------------------
--
-- The in-between state of one login, living between the two requests "start login" and "come back
-- from the IdP" (10 minutes).
--
-- **Why it must be stored in the database**: multi-instance deployment. The login starts on instance
-- A, and the request where the IdP sends the browser back **very likely lands on instance B**. If
-- state / nonce / PKCE verifier only lived in A's memory, B could not find them, and the symptom is
-- "I clicked consent at the IdP, but coming back it says the login expired", occasionally.
--
-- Besides state, the two other columns are required by the standard OIDC flow: nonce prevents ID
-- token replay, and code_verifier is PKCE.
create table if not exists auth_oidc_states (
    -- The OAuth state parameter, 32 random bytes (auth.newToken). **It is the CSRF protection**: if
    -- the state in the callback is not found in this table, that callback was not started by us.
    state text primary key,

    -- The OIDC nonce, generated at the start and written into the authorization request; the nonce
    -- claim in the ID token must equal it, otherwise a token stolen elsewhere is being replayed.
    nonce text not null,

    -- The PKCE code_verifier (RFC 7636). The authorization request only carries its S256 digest, and
    -- the token exchange carries the plain value: someone who intercepted the code cannot exchange it
    -- without it.
    --
    -- ⚠️ It is a **one-time secret**, so once state is consumed this row is useless, and expiry
    -- cleanup deletes it.
    code_verifier text not null,

    -- Where to send the user after a successful login. **Only in-site relative paths are stored**,
    -- already passed through sanitizeReturnTo before writing. Stored in the database instead of
    -- round-tripping through the browser: whatever can be read back was certainly written by us.
    return_to text not null default '',

    -- When it was consumed; NULL = not used yet. "One-time" relies on the single UPDATE in
    -- ConsumeOIDCState (the check and the mark in one statement), not on reading this field.
    used_at timestamptz,

    created_at timestamptz not null,
    expires_at timestamptz not null
);

-- For expiry cleanup (piggybacked on the write path; the write rate is the login rate, enough to
-- clean everything).
create index if not exists auth_oidc_states_expires_idx on auth_oidc_states (expires_at);
