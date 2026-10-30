-- ┌── Table outline: lark_user_tokens ─────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ actor               text                      not null
-- │ lark_app_id         text                      not null
-- │ access_token        text                      not null
-- │ refresh_token       text                      not null
-- │ expires_at          timestamp with time zone  not null
-- │ refresh_expires_at  timestamp with time zone  not null
-- │ scope               text                      not null default ''::text
-- │ open_id             text                      not null default ''::text
-- │ user_name           text                      not null default ''::text
-- │ created_at          timestamp with time zone  not null
-- │ updated_at          timestamp with time zone  not null
-- │ last_refresh_error  text                      not null default ''::text
-- │
-- │ primary key lark_user_tokens_pkey
-- │      (actor, lark_app_id)
-- └────────────────────────────────────────────────────────────────────────────

-- OAuth credentials obtained from Feishu "user authorization".
--
-- ⚠️⚠️ This is **the first credential in this repository that must be stored decryptably**, unlike the
-- other two ways of storing:
--
--     auth_api_keys.key_hash     sha256: only needs to verify "is what you gave right", never recovered
--     ark_secrets.api_key        cryptobox encryption: the plain text is needed to call Ark
--     lark_user_tokens.*_token   cryptobox encryption: the plain text is needed to refresh
--
-- Storing a hash is **impossible** here: renewal must send the refresh_token to Feishu as is. So both
-- columns go through shared/infra/cryptobox (AES-256-GCM, column shape `v1:<base64>`).
--
-- # Why it must be stored
--
-- Feishu user OAuth access_tokens expire in about 2 hours, and **the only way to renew is the
-- refresh_token we store ourselves**. Without storing it there is no way to renew; the user would
-- have to scan a code again every two hours.
--
-- The rule is "**who holds the authority over this thing**": the refresh_token is something Feishu
-- gave us and only we have, so the authority is in our database. ⚠️ **Do not take this table as
-- permission to "cache other platforms' authorization status"**: other platforms' status follows the
-- platform, and storing a copy would disagree with it.
create table if not exists lark_user_tokens (
    -- The email of the authorizing person (Actor). One row per person: whichever workspace they use it
    -- in, it is the same user identity.
    --
    -- The reason not to organize by (workspace, person) is that **Feishu itself is not organized by
    -- workspace**: authorization is a one-off action by this person for some Feishu app, unrelated to
    -- AMA's workspaces. Storing per workspace would make the same person scan N codes.
    actor            text        not null,
    -- Feishu app id (cli_xxx). In the primary key: each user registers their own app, and the same
    -- person under different apps has **two unrelated tokens** (scope and tenant may both differ).
    lark_app_id      text        not null,

    -- ⚠️ Both columns are cryptobox ciphertext (`v1:` prefix), not plain text.
    access_token     text        not null,
    refresh_token    text        not null,

    -- Two expiry times. **timestamptz rather than text**: only for database comparison and sorting,
    -- not in the HTTP contract (the contract carries secret-free display values like "valid until").
    -- See "Pick the column type by purpose" in CLAUDE.md.
    expires_at       timestamptz not null,
    refresh_expires_at timestamptz not null,

    -- The raw scope text obtained at authorization (space-separated). **Not a secret**; debugging
    -- needs to see "which permissions did they really grant": without
    -- im:message.group_msg:get_as_user, reading group history is denied.
    scope            text        not null default '',
    -- The user id on the Feishu side, only for debugging (it matches sender_open_id in inbound messages).
    open_id          text        not null default '',
    -- Display name, so the "integration management" card can say whose token this is. Not a secret.
    user_name        text        not null default '',

    created_at       timestamptz not null,
    updated_at       timestamptz not null,
    -- last_refresh_error is the reason of the last failed renewal (cleared on success).
    --
    -- ⚠️ **It lets the card tell warning from error**: one failure from a network hiccup is a warning
    -- (the refresh_token has not expired, it will recover next time), while invalid_grant is an error
    -- (the user revoked authorization on the Feishu side and must scan again). Two states would call
    -- the former "invalid" and make the user re-authorize again and again. ⚠️ **Never write a token
    -- into this column**: it goes into the HTTP response.
    last_refresh_error text      not null default '',

    constraint lark_user_tokens_pkey primary key (actor, lark_app_id)
);

comment on table lark_user_tokens is
    'Feishu user OAuth credentials. Both access/refresh columns are cryptobox ciphertext; stored rather than fetched live because refreshing needs our own saved refresh_token.';
comment on column lark_user_tokens.refresh_token is
    'cryptobox ciphertext. This is the only reason the table must be decryptable (instead of storing sha256): refreshing sends the original value back to Feishu.';
comment on column lark_user_tokens.last_refresh_error is
    'Reason for the latest refresh failure, used to tell "network blip" (warning) from "authorization revoked" (error). Contains no credentials.';
