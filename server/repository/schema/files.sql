-- ┌── Table outline: files ────────────────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id              text     not null
-- │ bucket          text     not null
-- │ object_key      text     not null
-- │ filename        text     not null
-- │ content_type    text     not null
-- │ size            integer  not null
-- │ content_sha256  text     not null
-- │ author          text     not null
-- │ created_at      text     not null
-- │
-- │ primary key files_pkey
-- │      (id)
-- │ index files_object_key_idx
-- │      (object_key)
-- │ index files_sha256_idx
-- │      (content_sha256)
-- └────────────────────────────────────────────────────────────────────────────

-- The shared file table: metadata of all "files uploaded by users", plus where the bytes are in object
-- storage.
--
-- **The bytes are in S3-compatible object storage (shared/infra/s3), the metadata is here. PG stores no
-- file bytes**: attachment tables have no content column, and there is no table that stores bytes.
-- This table only covers "which files exist, and which key in which bucket the bytes are at".
--
-- ⚠️ **Bytes must never be stored on local disk.** In a multi-instance deployment each instance's disk
-- is its own and lost on rebuild, showing up as "images are sometimes broken", with no error. The
-- BlobStore must be shared storage outside the process.
--
-- # References: business tables hold file_id; files does not store an owner
--
-- Both would work; here **business tables hold file_id** (`issue_attachments.file_id`), because three
-- things are simpler in this direction:
--
--   * **Deletion comes for free.** Deleting attachments is
--     `delete from issue_attachments where issue_id = $1`, and once the attachment row is gone the
--     reference breaks naturally. The other way (files storing owner_type + owner_id) would need an
--     extra `delete from files where ...` at every delete point, and **missing one leaves orphan rows
--     forever**, with no error.
--   * **Overreach checks are unaffected.** `issue_attachments.workspace_id` is still in place, and
--     authz can decide by resolving one table from the attachment id.
--   * **The same content being referenced from several places is allowed**; see "dedup" below. The
--     owner direction cannot express one-to-many; storing the owner in files would need another join
--     table.
--
-- ⚠️ The cost must be stated: **files rows are never cleaned up automatically**. Deleting an attachment
-- only breaks the reference; the files row and the bytes are still there. This is deliberate; see the
-- next section.
--
-- # Consistency between bytes and rows: write bytes first, then the row; deleting only deletes the row
--
-- Failures in the two directions are not symmetric, so the side that "leaks" was chosen on purpose:
--
--   write:  write the bytes first, insert after success. Dying in between → **an extra copy of bytes
--           nobody references**, but the database is clean, the user sees "upload failed", and can
--           upload again.
--   delete: only delete the row in the database, **not the bytes**. → also a leak.
--
-- The other way round would give **attachments that can be referenced but not read**: the list has the
-- entry, opening it gives 404. That is user-visible damage, while a leak is only cost. **Between the
-- two evils, leak rather than damage.** Reconciliation cleanup (list keys → compare with the files
-- table → delete bytes with no row) is not done for now: it carries the risk of "delete the wrong one
-- and it is gone", and should not be written before there are measurements.
--
-- # Content hash: stored, but no dedup
--
-- `content_sha256` is computed and stored on every upload, for verification and debugging ("are these
-- two attachments the same file").
--
-- **But there is no unique index and no "reuse the same bytes for the same content"**. Dedup must
-- handle concurrent writes; the right way is a unique constraint + ON CONFLICT, which requires
-- "reference counting", otherwise A deleting an attachment would delete bytes B still uses. The gain
-- (a few MB saved) is close to zero at the current scale, so only the basis is kept, not the mechanism.
create table if not exists files (
    -- File id (starts with file_). Business tables reference this table by it.
    --
    -- ⚠️ **It does not appear in any URL.** The access path of attachments is still
    -- /api/issue-attachments/{attachment id}, where that id is the attachment row id starting with
    -- att_, not the file_id here. This is so that **links already handed out keep working**: attachment
    -- URLs end up in <img src>, in browser history, in other people's chat history.
    id text primary key,

    -- The bucket the object is in.
    --
    -- **It must be stored, not just the key**: after the bucket name changes (migration, scaling,
    -- region change), old records must still read from the original bucket. Storing only the key would
    -- make "where is this object" a constant in the code, and once the constant changes, all historical
    -- attachments break at once.
    bucket text not null,

    -- The full key of the object in the bucket.
    --
    -- The shape is `<purpose>/<yyyy>/<mm>/<file_id>`, e.g. issue-attachments/2026/09/file_x7k2. Each
    -- of the three parts has a reason:
    --   * the purpose prefix lets the storage side configure lifecycle rules by prefix and count usage
    --     separately;
    --   * year/month sharding avoids piling too many objects under one prefix (listing gets slow);
    --   * it ends with file_id instead of the file name: **the file name is user input**, and putting it
    --     in the key would mean dealing with path traversal, encoding and duplicates, while an id is
    --     naturally unique and unguessable.
    --
    -- ⚠️ **A key can never change once written**: it is the only clue to find those bytes.
    object_key text not null,

    -- The original file name, only for display and Content-Disposition.
    -- **It takes part in no path building** (see the comment on object_key).
    filename text not null,

    -- ⚠️ The type **sniffed by the server**, not the one the client declared in multipart.
    --
    -- This is a **security constraint, not metadata**, inherited as is from issue_attachments: a client
    -- claiming image/png with HTML content is the most classic attack. What is stored is always the
    -- result of http.DetectContentType, cross-checked with the extension (core/issueattach.go). It is
    -- returned as is in responses, and together with nosniff blocks "upload HTML and run it as a page".
    content_type text not null,

    -- Byte count. Stored redundantly so list queries do not need to read the bytes.
    size int not null,

    -- sha256 of the content, **lower-case hex, exactly 64 characters** (contentSHA256 in core/blob.go
    -- generates it with encoding/hex, so the shape is guaranteed by the code).
    --
    -- ⚠️ **Deliberately no check constraint**: the only consumer of this column is our own code, and a
    -- check would turn "switch to another digest algorithm later" into a DDL migration. The same
    -- tradeoff as the two enum columns (see the "no check" reason in issues.sql). For its use see the
    -- "Content hash" section of the file header.
    --
    -- **No unique index**: no dedup, which would need reference counting to be safe.
    content_sha256 text not null,

    -- The uploader's email. It may differ from the author in the business table (an attachment may be
    -- added by the platform team), so both are stored, without assuming "which is the source of truth".
    author text not null,

    -- Upload time. **An ISO text string** (apimodel.NowISO(), fixed three decimals), because it goes
    -- into the HTTP contract and the frontend sorts it by string comparison. See "Timestamps: unit and
    -- format" in CLAUDE.md.
    created_at text not null
);

-- Look up by key. **For reconciliation and debugging**: given a key, you must be able to ask "does the
-- database still reference it". Not unique: if the same bytes are ever deduplicated, keys would be
-- reused, and then this must allow several rows.
create index if not exists files_object_key_idx on files (object_key);

-- Look up by content hash. For debugging and future dedup.
create index if not exists files_sha256_idx on files (content_sha256);
