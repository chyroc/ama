-- ┌── Table outline: issue_attachments ────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id            text     not null
-- │ issue_id      text     not null
-- │ workspace_id  text     not null
-- │ filename      text     not null
-- │ content_type  text     not null
-- │ size          integer  not null
-- │ author        text     not null
-- │ created_at    text     not null
-- │ ord           bigint   not null (sequence)
-- │ file_id       text     not null
-- │
-- │ primary key issue_attachments_pkey
-- │      (id)
-- │ index issue_attachments_issue_idx
-- │      (issue_id, ord)
-- │ index issue_attachments_ws_idx
-- │      (workspace_id)
-- └────────────────────────────────────────────────────────────────────────────

create table if not exists issue_attachments (
    -- Attachment id (starts with att_). **This is what is in the URL**: /api/issue-attachments/{id}.
    id text primary key,

    issue_id text not null,

    -- ⚠️ **This column is the basis of the overreach check.**
    --
    -- Attachment URLs are put into <img src>, and anyone with the URL will request it. authz resolves
    -- this column from the attachment id, then checks "is this person a member of this workspace":
    -- someone in workspace A holding an attachment URL of workspace B must get 403. Same as
    -- issue_comments, the redundancy here lets that lookup query only one table.
    workspace_id text not null,

    -- The original file name at upload, only for display and Content-Disposition.
    -- **It takes part in no path building**; the URL only has the id.
    filename text not null,

    -- ⚠️ The type **sniffed by the server**, not the one the client declared in multipart.
    --
    -- A client claiming image/png with HTML content is the most classic attack. What is stored here
    -- is always the result of http.DetectContentType, cross-checked with the extension
    -- (core/issueattach.go). It is returned as is in responses, and together with nosniff blocks
    -- "upload HTML and run it as a page".
    content_type text not null,

    -- Byte count (computed at upload). The list only wants to show "1.2 MB", without fetching bytes
    -- from object storage.
    size int not null,

    author text not null,
    -- Attachments cannot be edited (to replace one, delete and upload again), so there is no
    -- updated_at.
    created_at text not null,

    ord bigserial not null,

    -- Points to the files table. **Bytes are only in object storage** (files.bucket +
    -- files.object_key); neither this table nor PG as a whole stores attachment bytes.
    file_id text not null
);

create index if not exists issue_attachments_issue_idx on issue_attachments (issue_id, ord);
-- Bulk delete by this column when a workspace is deleted.
create index if not exists issue_attachments_ws_idx on issue_attachments (workspace_id);
