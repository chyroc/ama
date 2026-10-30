-- ┌── Table outline: feishu_sessions ──────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id          text                      not null
-- │ expires_at  timestamp with time zone  not null
-- │ doc         jsonb                     not null
-- │
-- │ primary key feishu_sessions_pkey
-- │      (id)
-- │ index feishu_sessions_expires_idx
-- │      (expires_at)
-- └────────────────────────────────────────────────────────────────────────────
-- ├── Keys of the doc column (jsonb) (= json.Marshal(Session))
-- │ ⚠️ This block is **generated from the json tags of the Go type**, not by sampling data
-- │    in the database. Keys with omitempty are absent when the value is zero, so sampling
-- │    always misses them, yet they are part of the contract. After changing the type, run:
-- │        cd server && go run ./cmd/gen-doc-outline -w
-- │    Source of truth: feishu/app.go, type Session
-- │
-- │ WorkspaceID          string
-- │ ActorID              string
-- │ DeviceCode           string
-- │ Region               string
-- │ PollIntervalSeconds  int
-- │ ExpiresAt            time.Time
-- ┤──────────────────────────────────────────────────────────────────────────

-- feishu_sessions: device code registration sessions, a ten-minute in-between state.
create table if not exists feishu_sessions (
    -- id is a high-entropy session identifier (see session.go); the frontend polls with it.
    id         text        not null,
    -- expires_at is **the cleanup time of this row**, computed from PutSession's ttl.
    --
    -- ⚠️ It is not ExpiresAt in doc. doc.ExpiresAt is the expiry of **the Feishu device code**,
    -- derived by the business layer from Feishu's expires_in; this column is **how long this platform
    -- is willing to keep the row** (sessionTTL, deliberately shorter; see feishu.go). The two are often
    -- close but not the same thing; do not merge them.
    expires_at timestamptz not null,
    -- doc is the JSON of the whole Session struct.
    --
    -- It contains DeviceCode (Feishu's polling credential), so this table is as sensitive as secrets.
    -- The Session struct appears in no API response; the handler only derives status and the poll
    -- interval from it.
    doc        jsonb       not null,

    constraint feishu_sessions_pkey primary key (id)
);

-- Expiry cleanup scans this column.
create index if not exists feishu_sessions_expires_idx
    on feishu_sessions (expires_at);

comment on column feishu_sessions.expires_at is
    'When this row is cleaned up (computed from the PutSession ttl), not doc.ExpiresAt (when the Feishu device code expires).';
