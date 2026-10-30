-- ┌── Table outline: suggestion_attachments ───────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id             text     not null
-- │ suggestion_id  text     not null
-- │ filename       text     not null
-- │ content_type   text     not null
-- │ size           integer  not null
-- │ author         text     not null
-- │ created_at     text     not null
-- │ ord            bigint   not null (sequence)
-- │ file_id        text     not null
-- │
-- │ primary key suggestion_attachments_pkey
-- │      (id)
-- │ index suggestion_attachments_sug_idx
-- │      (suggestion_id, ord)
-- └────────────────────────────────────────────────────────────────────────────

create table if not exists suggestion_attachments (
    -- Attachment id (starts with sat_). **This is what is in the URL**:
    -- /api/suggestion-attachments/{id}.
    id text primary key,

    suggestion_id text not null,

    -- ⚠️ **This column decides how the overreach check is done.**
    --
    -- On the issues side attachments have a redundant workspace_id column, so authz can decide by
    -- resolving one table. There is no workspace dimension here, so the rule is "is the author of the
    -- owning suggestion you", which needs a join back to suggestions. **Not copying author into this
    -- table** is deliberate: a redundant column must stay consistent with the main table, and the only
    -- gain here is saving one join, at the cost of one more place that can disagree (an attachment's
    -- author differing from the suggestion's author is perfectly legal: the platform team may have
    -- added the attachment).
    --
    -- So the permission check must go through the Service (SuggestionAttachmentContent in
    -- core/suggestion.go gets the suggestion first, then checks); it cannot be left to authz's Guard
    -- as for issues.

    -- The original file name at upload, only for display and Content-Disposition.
    -- **It takes part in no path building**; the URL only has the id.
    filename text not null,

    -- ⚠️ The type **sniffed by the server**, not the one the client declared in multipart.
    -- Reuses DetectAttachmentType from core/issueattach.go; that logic does not depend on the entity.
    content_type text not null,

    -- Byte count (computed at upload). The list only shows the size, without fetching bytes from
    -- object storage.
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

create index if not exists suggestion_attachments_sug_idx on suggestion_attachments (suggestion_id, ord);
