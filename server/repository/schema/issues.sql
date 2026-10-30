-- ┌── Table outline: issues ───────────────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id            text    not null
-- │ workspace_id  text    not null
-- │ title         text    not null
-- │ body          text    not null default ''::text
-- │ status        text    not null default 'open'::text
-- │ priority      text    not null default 'p2'::text
-- │ author        text    not null
-- │ created_at    text    not null
-- │ updated_at    text    not null
-- │ ord           bigint  not null (sequence)
-- │ origin        text    not null default ''::text
-- │ fingerprint   text    not null default ''::text
-- │
-- │ primary key issues_pkey
-- │      (id)
-- │ unique issues_bot_fingerprint_key
-- │      (workspace_id, origin, fingerprint) WHERE ((origin <> ''::text) AND (fingerprint <> ''::text))
-- │ index issues_workspace_idx
-- │      (workspace_id, ord DESC)
-- │ index issues_ws_status_idx
-- │      (workspace_id, status, ord DESC)
-- └────────────────────────────────────────────────────────────────────────────

create table if not exists issues (
    -- The id of this issue (an opaque string starting with iss_).
    id text primary key,

    -- The owning workspace. **Not null**: an issue is a workspace-level resource.
    --
    -- This column is not only data ownership but also **the basis of authorization**: routes are under
    -- /api/workspaces/{workspaceId}/issues, and flat paths (/api/issues/{id}) are resolved to this
    -- column by id through WorkspaceOf in core/resource.go, after which Guard checks the role.
    workspace_id text not null,

    -- Title. **Guaranteed non-empty when stored**: the user may leave it empty, but the Service
    -- extracts one from the body (core/issuetitle.go), falling back to "Untitled issue" if nothing can
    -- be extracted.
    --
    -- Why empty is not allowed: an empty title is an invisible row in the list, and users would think
    -- the data is broken. "Both title and body empty" is already rejected further upstream (that is an
    -- empty record, not a title problem).
    title text not null,

    -- Markdown body; may be empty (only a title is valid).
    body text not null default '',

    -- open / in_progress / closed.
    --
    -- **No check constraint**: the value is validated by the Service (apimodel.ValidIssueStatus,
    -- anything unrecognized is rejected), and a check constraint here would only turn "add a level
    -- later" into a DDL migration. Same judgment as feedback_reports.level.
    status text not null default 'open',

    -- p0 / p1 / p2, **default p2**.
    --
    -- Not default p0: defaulting to the highest level means every new issue shouts "most urgent", so
    -- p0 immediately loses its meaning: the real p0 drowns in a sea of p0, which is exactly the problem
    -- priority is meant to solve.
    priority text not null default 'p2',

    -- The creator's email (Actor).
    --
    -- Not nullable: all routes are behind auth.Protect, and nothing gets here without an identity.
    -- The value only comes from the context; **the request body has no such field**: accepting it
    -- would allow filing issues under someone else's name.
    author text not null,

    -- A fixed-length ISO string; see the file header.
    created_at text not null,
    -- Changing the title / body / status / priority all refresh it; the Service sets it in one place.
    updated_at text not null,

    -- Insertion order. The list sorts by it descending (newest first), the same approach as ord in
    -- skills/agents.
    --
    -- Not sorted by created_at: two rows created in the same millisecond would have an unstable order,
    -- showing up as two rows swapping places after a refresh, which looks like the data is jumping.
    ord bigserial not null,

    -- Who produced it, and the dedup fingerprint used by bots; see the "Issues produced by bots"
    -- section below.
    origin      text not null default '',
    fingerprint text not null default ''
);

-- The main list query: all issues of a workspace, by ord descending.
create index if not exists issues_workspace_idx on issues (workspace_id, ord desc);

-- After filtering by status it still needs descending order, so a composite index instead of a single
-- column. "Only the ones not closed yet" is the most common filter on this page.
create index if not exists issues_ws_status_idx on issues (workspace_id, status, ord desc);

-- ---------------------------------------------------------------------------
-- Issues produced by bots: origin + fingerprint
-- ---------------------------------------------------------------------------
--
-- ⚠️ **These two columns do not overturn the file header's "issues must not be deduplicated".** That
-- sentence is about **people**: two people reporting the same thing should be two rows, because they
-- are two independent testimonies, and merging would lose "how many people are affected", which is
-- exactly the basis for priority.
--
-- Bot inspection is a different matter. A scheduled task rescans **the same material** (the feedback
-- table + service logs) every X minutes, and as long as the problem is not fixed, it "discovers" the
-- same thing every round. Without dedup the result is not "a few more testimonies" but hundreds of
-- identical records flooding the list, and then the real problems people reported cannot be found.
--
-- So the rule is **who produced it**, not "does the content look alike", and the scope is explicit:
--
--   origin = ''          written by a person → the unique index does not cover it at all; never deduplicated
--   origin = 'schedule'  produced by scheduled inspection → same origin + same fingerprint merge into one
--
-- **The default is an empty string**: the Service treats an empty string as "written by a person".
-- Both columns are defined in the create table above.

-- ⭐ **Partial unique index: this line is the whole implementation of "dedup only for bots".**
--
-- `where origin <> '' and fingerprint <> ''` is the key: rows written by people (both columns empty)
-- are not in the index at all, so they are **never** judged duplicates of each other, nor of bot
-- rows. Removing this where would put the whole table into the index, which would really violate the
-- design decision in the file header.
--
-- A unique constraint instead of "query, then create in Go": with multiple instances two processes may
-- find the same problem at the same time, and query-then-create is TOCTOU (same as feedback's
-- dedup_key; see the argument in README-feedback.md). With it, under concurrency the slower one hits
-- the constraint, and the code falls back to "append a comment".
--
-- workspace_id is in the key: two workspaces each finding the same phenomenon in their own inspection
-- are two different things, and merging would mix across workspaces.
create unique index if not exists issues_bot_fingerprint_key
    on issues (workspace_id, origin, fingerprint)
    where origin <> '' and fingerprint <> '';
