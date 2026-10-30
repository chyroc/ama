-- ┌── Table outline: skill_files ──────────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ skill_id      text  not null
-- │ workspace_id  text  not null
-- │ path          text  not null
-- │ file_id       text
-- │
-- │ primary key skill_files_pkey
-- │      (skill_id, path)
-- │ index skill_files_workspace_idx
-- │      (workspace_id)
-- └────────────────────────────────────────────────────────────────────────────

-- Files that come with a skill package (only for zip uploads; hand-written skills have none).
--
-- # Why a separate table instead of putting it into skills.doc
--
-- Two reasons, each enough on its own:
--
-- 1. **Paths must be unique within one skill, and jsonb array elements cannot have a unique
-- constraint.** Duplicate paths would make the later write overwrite the earlier one when injecting
-- into the container, silently losing files. In doc the only option would be "read it out and check
-- in Go", which is check-then-write and is sure to miss under multi-instance concurrency (see that
-- table in CLAUDE.md). As a real table, `primary key (skill_id, path)` lets the database decide.
--
-- 2. **File bytes cannot go into skills.doc.** doc is the return value of ListSkills, which outputs
-- all skills of a workspace at once; putting attachment bytes there would grow the list response
-- linearly with the attachments, while the list page only needs file names. Files in doc only have
-- path + size (Content has omitempty; see apimodel.SkillFile).
--
-- # The bytes are not in this table
--
-- The bytes are in S3-compatible object storage (shared/infra/s3), pointed to through the files table
-- by `file_id`. **The bytes must never be missing everywhere**: if writing the bytes fails, the whole
-- upload fails (see core.putSkillFileBlobs), leaving no half-finished result.
--
-- No ord column: sorting by path when reading is enough; attachments have no "insertion order"
-- meaning (unlike the skills list, whose order users can see).
create table if not exists skill_files (
    skill_id     text not null,
    workspace_id text not null,
    path         text not null,
    -- Points to the files table. The bytes are in the BlobStore; see core/blob.go.
    file_id      text,
    primary key (skill_id, path)
);

-- Getting all attachments of a skill (when injecting into the container), and the cascading delete
-- when a workspace is deleted, both rely on this.
create index if not exists skill_files_workspace_idx on skill_files (workspace_id);
