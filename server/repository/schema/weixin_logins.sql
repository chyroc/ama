-- ┌── Table outline: weixin_logins ────────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id                   text                      not null
-- │ workspace_id         text                      not null
-- │ created_by           text                      not null
-- │ qrcode               text                      not null
-- │ qrcode_content       text                      not null
-- │ poll_base_url        text                      not null
-- │ status               text                      not null
-- │ pending_verify_code  text                      not null default ''::text
-- │ refresh_count        integer                   not null default 1
-- │ account_id           text                      not null default ''::text
-- │ message              text                      not null default ''::text
-- │ created_at           timestamp with time zone  not null
-- │ expires_at           timestamp with time zone  not null
-- │
-- │ primary key weixin_logins_pkey
-- │      (id)
-- │ index weixin_logins_expires_idx
-- │      (expires_at)
-- └────────────────────────────────────────────────────────────────────────────

-- weixin_logins: QR-code logins in progress (server/weixin).
--
-- Starting a login and polling it are separate HTTP requests that usually land on different instances,
-- so the state cannot live in process memory. Rows expire after a few minutes; expired rows are deleted
-- when new logins start.
create table if not exists weixin_logins (
    id                  text        not null,
    workspace_id        text        not null,
    -- created_by is the person who started the login; only they can poll it (the QR code is a credential).
    created_by          text        not null,
    -- qrcode is the value polled on get_qrcode_status; qrcode_content is what is rendered as the QR image.
    qrcode              text        not null,
    qrcode_content      text        not null,
    -- poll_base_url changes when the server answers scaned_but_redirect.
    poll_base_url       text        not null,
    -- status is our own value (waiting / scanned / need_verify_code / confirmed / already_connected /
    -- expired / failed), not the server's spelling.
    status              text        not null,
    -- pending_verify_code is the number the user typed, sent with the next polls until it is accepted.
    pending_verify_code text        not null default '',
    -- refresh_count counts QR codes issued for this login (the first one included); capped at 3.
    refresh_count       int         not null default 1,
    -- account_id is the weixin_accounts row created when the login is confirmed.
    account_id          text        not null default '',
    message             text        not null default '',
    created_at          timestamptz not null,
    expires_at          timestamptz not null,

    constraint weixin_logins_pkey primary key (id)
);

create index if not exists weixin_logins_expires_idx
    on weixin_logins (expires_at);
