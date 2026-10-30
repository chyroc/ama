-- ┌── Table outline: user_files ───────────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id            text    not null
-- │ owner_email   text    not null
-- │ bucket        text    not null
-- │ object_key    text    not null
-- │ filename      text    not null
-- │ content_type  text    not null default ''::text
-- │ size          bigint  not null default 0
-- │ status        text    not null
-- │ upload_mode   text    not null
-- │ upload_id     text    not null default ''::text
-- │ created_at    text    not null
-- │
-- │ primary key user_files_pkey
-- │      (id)
-- │ index user_files_owner_created_idx
-- │      (owner_email, created_at)
-- │ unique user_files_owner_object_idx
-- │      (owner_email, object_key)
-- └────────────────────────────────────────────────────────────────────────────

-- The file table "for people to store files": each logged-in user puts their own files into object
-- storage, with object keys like `user-files/<lower-cased email>/<ufid>`, isolated per user.
--
-- # How it relates to the files table
--
-- `files` is the **attachment** table: things attached to business entities like issues /
-- suggestions / skills; business tables reference it by file_id, and it has no notion of owner (see
-- the "References" section of files.sql). This table is **account-level personal files**: not under
-- any workspace/business entity, owned by the uploader themselves, accessed through account-level
-- APIs (/api/files, ActionNone + owner_email in the SQL). The prefixes are also separate (user-files/
-- on the object storage side, disjoint from issue-attachments/), so lifecycle rules and usage stats
-- can be computed separately later. **Do not merge the two**: one is "an entity's attachments", the
-- other is "a person's files"; the owner models differ, and merging would have to be undone sooner or
-- later.
--
-- # Isolation model: owner_email is in every WHERE, never in any place the client controls
--
-- These routes have no workspace, so Guard cannot check workspace roles; therefore **every SQL
-- statement includes owner_email = the current login email** (the service layer takes it from
-- auth.ActorFrom and never accepts an email the request body claims). Looking up someone else's id
-- gives sql.ErrNoRows → 404, not 403: it does not reveal "this id exists but is not yours".
--
-- # State machine: uploading → completed
--
-- Large files use object storage multipart upload (init → parts → complete), a process that may span
-- several HTTP requests and minutes. The state is stored in the database instead of the process: with
-- multiple instances the next part request may land in another process, and an in-process map would
-- certainly lose it (CLAUDE.md "This is a distributed service"):
--
--   uploading  the row exists, the object is not fully assembled yet; upload_id stores the multipart
--              upload credential issued by object storage. simple mode (≤8MiB, one PUT) also creates
--              an uploading row first and sets completed once written.
--   completed  the object is in place and can be listed / downloaded / deleted.
--
-- The list only returns completed. uploading rows cannot be downloaded, and cannot go through DELETE
-- either (it returns 409 suggesting abort).
--
-- # How leftovers are collected (no automatic cleanup on purpose)
--
--   * Parts uploaded to object storage but never completed: configure the bucket lifecycle rule
--     AbortIncompleteMultipartUpload (abort after some days); this is a native object storage
--     capability, and the code takes no part.
--   * uploading rows in the database: no inspection task. The amount depends on the interruption
--     rate; observe first; later a scheduled task "delete uploading rows older than N days + abort
--     their uploadId" can be added.
--
-- # Delete semantics different from the files table
--
-- The files table "only deletes the row, not the object" because an object may be referenced in
-- several places (without reference counting it is better to leak). In this table an object **belongs
-- only to this one person's one row**, with no second referrer, so deletion is "delete the object
-- first (safe to repeat), then the row": if deleting the object fails the whole request fails and the
-- row stays for retry, leaving no long-term garbage of "gone from the database but the bytes are
-- still billed" (short-term interrupted parts are collected by the lifecycle above).
--
-- # Why the timestamp is text
--
-- created_at goes into the HTTP contract and the list sorts by it descending (the frontend compares
-- strings), so it is a fixed-length ISO string with three millisecond digits (apimodel.NowISO()), not
-- timestamptz. See "Timestamps: unit and format" in CLAUDE.md.
create table if not exists user_files (
    -- File id (uf_ prefix + random hex). **It is part of the URL** (/api/files/{id}), but takes no part
    -- in building the object key; the key is stored separately in object_key.
    id text primary key,

    -- Owner email, **lower-cased** (the service layer does ToLower + TrimSpace before writing,
    -- consistent with the normalization in auth/store.go). Isolation relies entirely on this column;
    -- see "Isolation model" in the file header.
    owner_email text not null,

    -- The bucket the object is in. **It must be stored**: after a bucket rename, old records must still
    -- read from the original bucket (exactly the reason for files.bucket).
    bucket text not null,

    -- The full key of the object in the bucket: `user-files/<owner_email>/<id>`.
    --
    -- ⚠️ **The file name does not go into the key**: user-entered file names may contain /, .., emoji,
    -- duplicates, and putting them in the key would mean dealing with path traversal and overwrites;
    -- ending the key with a random id makes it naturally unique and unguessable, and the original name
    -- is only stored in filename for display. Exactly the design of files.object_key.
    object_key text not null,

    -- The original file name, only for display and Content-Disposition on download; takes no part in
    -- path building.
    filename text not null,

    -- The content type declared by the client. **Different from content_type in the files table**:
    -- that column is the server's sniffing result (attachments are rendered as images by the browser,
    -- so HTML disguises must be blocked); here downloads go through time-limited signed links of object
    -- storage, and download responses carry Content-Disposition: attachment, with no inline rendering,
    -- so the client-declared type is only echoed back for display. May be an empty string.
    content_type text not null default '',

    -- Byte count after completion. 0 while uploading; on complete the value read back with HeadObject
    -- is stored, as "the truth the server saw", without trusting what the client reports.
    size bigint not null default 0,

    -- uploading / completed. No CHECK (consistent across the repository: enums are code literals, no
    -- database constraints).
    status text not null,

    -- simple / multipart: which path this upload takes. It decides which endpoints are valid (simple
    -- only allows PUT content, multipart only allows parts/complete/abort).
    upload_mode text not null,

    -- The object storage multipart uploadId; '' in simple mode. It is the only credential for
    -- continuing parts, and what must be given to object storage on abort.
    upload_id text not null default '',

    -- When the upload started, a fixed-length ISO string with three millisecond digits (see the
    -- timestamps section of the file header).
    created_at text not null
);

-- object_key is unique within one owner. Normally object_key contains a random id and never clashes;
-- this constraint is a safety net against "two places in the code using the same key and overwriting
-- each other", letting the database decide instead of query-then-create.
create unique index if not exists user_files_owner_object_idx
    on user_files (owner_email, object_key);

-- The main "list my files" query: where owner_email=$1 and status='completed'
-- order by created_at desc.
create index if not exists user_files_owner_created_idx
    on user_files (owner_email, created_at);
