-- ┌── Table outline: entity_revisions ─────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id            text     not null
-- │ workspace_id  text     not null
-- │ entity_kind   text     not null
-- │ entity_id     text     not null
-- │ revision_no   integer  not null
-- │ snapshot      text     not null
-- │ summary       text     not null default ''::text
-- │ created_at    text     not null
-- │ created_by    text     not null default ''::text
-- │ reason        text     not null default 'edit'::text
-- │
-- │ primary key entity_revisions_pkey
-- │      (id)
-- │ index entity_revisions_entity_idx
-- │      (entity_kind, entity_id, revision_no DESC)
-- │ unique entity_revisions_no_key
-- │      (entity_kind, entity_id, revision_no)
-- └────────────────────────────────────────────────────────────────────────────

-- Entity revisions (snapshots before a change), supporting "I made a mistake, switch back to the
-- previous version".
--
-- # ⚠️ Naming: "version" already has two different meanings in this repository; this is a third thing
--
-- So it is called **revision**, not version. The difference between the three must be stated once,
-- because mixing them up gives no error:
--
--   | Name                      | Type   | What it is |
--   | ------------------------- | ------ | ---------- |
--   | `apimodel.Agent.Version`  | int    | a **monotonic counter** incremented on every agent change, shown in the UI as `v7` |
--   | `Skill.LatestVersion`     | string | also a counter, but **a string** (exactly the opposite of the above, easy to get backwards) |
--   | **this table's `revision_no`** | int | **the sequence number of history snapshots**, starting from 1 independently per entity |
--
-- ⚠️ **`revision_no` and `Agent.Version` are not the same thing, and are deliberately not equal.** The
-- former counts "how many snapshots were stored", the latter "how many times it was changed"; not every
-- change necessarily produces a snapshot (nothing is stored if the content did not change; see
-- core/revision.go), and old snapshots beyond the limit get cleaned up. Making them equal would
-- immediately create an implicit contract nobody could maintain.
--
-- # ⚠️ Why snapshot is text and not jsonb
--
-- The rule of this directory is "expand fields into columns, join tables for one-to-many, jsonb only
-- for things whose columns truly cannot be listed" (see "Default rules for table design" in the
-- README). **Here neither is chosen; the third option, `text`, is**, for the reason the README itself
-- states:
--
--   > ⚠️ **"Byte-exact fidelity" needs `text` / `bytea`, not jsonb.** jsonb reorders keys, drops
--   > duplicate keys, squeezes whitespace and normalizes numbers: **it is exactly not faithful**.
--
-- The **whole value of a snapshot is byte-exact fidelity**: three months later it must restore exactly
-- "what the user filled in at that moment". And the UI diffs two snapshots: jsonb's key reordering
-- would invent fake "changes", and such dirty diffs destroy exactly the reason this feature exists.
-- The precedent is `run_events.raw` in the same directory, whose comment says literally "text instead
-- of jsonb, for byte-exact fidelity".
--
-- **Expanding into columns does not work, and that is not laziness**: the field sets of the three
-- entities **all differ** (AgentDraft has 13 fields, SkillDraft 5, ScheduleDraft 7); expanding them
-- would mean either three tables with very different structures or one wide table with most columns
-- always NULL. Worse, **the shape of a snapshot must evolve with the entity**: adding a field to agent
-- would require adding a column to the snapshot table and backfilling it, and missing that column
-- would mean "after rollback that field is silently reset to its zero value", exactly the mistake this
-- feature can least afford.
--
-- And the README's second decision question is answered here with **"none at all"**: no query needs to
-- filter, sort, group or deduplicate by fields **inside** snapshot. The five things to query
-- (entity_kind / entity_id / revision_no / created_at / created_by) **are all real columns**, and the
-- unique constraint is on real columns too. That is exactly the legitimate shape the README
-- describes, "jsonb (here text) only holds the part nobody queries, and the other fields of the table
-- are still real columns", same as `feedback_reports.payload` and
-- `feishu_progress_deliveries.applied_snapshot`.
--
-- # What is stored is the Draft, not the whole entity: a structural protection for the optimistic lock
--
-- snapshot holds `apimodel.AgentDraft` / `SkillDraft` / `ScheduleDraft`, **not** `apimodel.Agent` /
-- `Skill` / `Schedule`. This is not about saving bytes; it is a safety design:
--
-- Those three Draft types **have no Version / LatestVersion field at all**, nor ID / WorkspaceID /
-- CreatedAt / CreatedBy. So the mistake "restore the counter too on rollback" **cannot even be written
-- at the type level**: there is no such field to read. See "Why rollback must go through UpdateX" in
-- core/revision.go.
--
-- For schedule it has a second effect: `NextRunAt` / `RunNowRequested` / `RunCount` /
-- `ConsecutiveFailures` are **run-time state of the scheduler**, and the first two are also columns of
-- the claim protocol (see schedules.sql). None of them is in ScheduleDraft, so they can never be
-- brought back by a snapshot: restoring an expired NextRunAt would make the task jump the gun and run
-- a round at once.
create table if not exists entity_revisions (
    -- The id of this snapshot (an opaque string starting with rev_).
    id text primary key,

    -- The owning workspace.
    --
    -- **Redundant but necessary**: the workspace could be found from entity_id alone, but that would
    -- mean querying three tables by kind. Cleanup needs to count per workspace and Guard needs to
    -- resolve authorization, and both need it directly available. It is copied by the writer from **the
    -- entity at that time**; entities never move between workspaces, so it never goes stale.
    workspace_id text not null,

    -- agent / skill / schedule.
    --
    -- **No check constraint**, the same judgment as issues.status: values are validated on the Go side
    -- (apimodel.ValidRevisionKind), and a check would only turn "support one more kind of entity later"
    -- into a DDL migration.
    entity_kind text not null,

    -- The id of the snapshotted entity.
    --
    -- **No foreign key**, for two reasons: first, this repository has zero foreign keys across tables
    -- (agents / skills are hard-deleted, with no deleted_at); second, once the entity is deleted these
    -- rows are reclaimed by cleanup, and cascading deletes through a foreign key would turn "delete the
    -- agent and the history goes too" into an implicit behavior, while it should be explicit.
    entity_id text not null,

    -- Which snapshot of this entity it is, **starting from 1 independently per entity**, monotonically
    -- increasing.
    --
    -- ⚠️ **It does not equal Agent.Version** (see the table in the file header). It is also **not
    -- guaranteed to be contiguous**: old snapshots beyond the limit get cleaned up, so what remains may
    -- be 8/9/10 while 1..7 are gone. Do not show it in the UI as "how many times it was changed".
    revision_no integer not null,

    -- The entity's content right before the change, as JSON text.
    --
    -- **text, not jsonb, for byte-exact fidelity**; see that whole passage in the file header.
    snapshot text not null,

    -- A one-sentence summary for people (the entity's name at the time), shown in the list.
    --
    -- **Denormalized, and deliberately accepting that it goes stale**: what it records is "what that
    -- version was called at the time", which should be frozen at the moment of writing. This is not the
    -- same as the README's warning about "denormalized fields frozen forever": that one is about copying
    -- someone else's **current** value (names, avatars) that goes stale, while here what is copied is a
    -- **historical value**, and being stale is its meaning.
    summary text not null default '',

    -- When the snapshot was produced.
    --
    -- **text rather than timestamptz**: it goes into the HTTP contract, and the frontend compares and
    -- sorts it as a string, so by the either-or rule in CLAUDE.md "Timestamps: unit and format" it is
    -- text (a fixed-length ISO string with three decimals).
    created_at text not null,

    -- Whose change produced this snapshot, the user's email.
    --
    -- **It may be an empty string**: system paths (a task scheduled by self-wakeup being changed, a
    -- marketplace skill auto-updating) have no human identity. An empty string means "not changed by a
    -- person"; the UI must say so truthfully, not guess someone to fill in.
    created_by text not null default '',

    -- Which action produced this snapshot: edit (a normal edit) or rollback (a rollback).
    --
    -- It exists because **a rollback itself also produces a snapshot** (see core/revision.go); without
    -- marking it, the history list would contain a record of unknown origin, and users could not tell
    -- "my change" from "caused by my rollback". default 'edit' is a fallback for when it is not passed.
    reason text not null default 'edit'
);

-- revision_no cannot repeat within the same entity.
--
-- ⚠️ **This is a correctness gate under concurrency, not a nicety.** revision_no is generated by "query
-- the current max + 1", a check-then-write: two instances saving the same agent at the same time
-- would compute the same number. Without this unique constraint the result would be **two snapshots
-- with the same revision_no**, and "roll back to version 5" would no longer have a single answer.
-- With it, the later writer hits the unique conflict and retries (see AppendEntityRevision in
-- coredao).
create unique index if not exists entity_revisions_no_key
    on entity_revisions (entity_kind, entity_id, revision_no);

-- List an entity's history, newest first.
--
-- **revision_no DESC is in the index**, exactly matching the order by of ListEntityRevisions:
-- otherwise PG would need an extra Sort, and this list page is a low-frequency path "only opened when
-- something goes wrong", which should not pay a sorting cost either. Cleanup also uses this index to
-- find "the ones after position N".
create index if not exists entity_revisions_entity_idx
    on entity_revisions (entity_kind, entity_id, revision_no desc);
