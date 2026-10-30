-- ┌── Table outline: suggestions ──────────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id          text    not null
-- │ title       text    not null
-- │ body        text    not null default ''::text
-- │ status      text    not null default 'open'::text
-- │ author      text    not null
-- │ created_at  text    not null
-- │ updated_at  text    not null
-- │ ord         bigint  not null (sequence)
-- │
-- │ primary key suggestions_pkey
-- │      (id)
-- │ index suggestions_author_idx
-- │      (author, ord DESC)
-- │ index suggestions_ord_idx
-- │      (ord DESC)
-- │ index suggestions_status_idx
-- │      (status, ord DESC)
-- └────────────────────────────────────────────────────────────────────────────

create table if not exists suggestions (
    -- The id of this suggestion (an opaque string starting with sug_).
    id text primary key,

    -- ⚠️ **This table has no workspace_id; see the file header.** It is not missing; this concept has
    -- no workspace.

    -- Title. **Guaranteed non-empty when stored**: the user may leave it empty, but the Service
    -- extracts one from the body (reusing DeriveIssueTitle from core/issuetitle.go), with a fallback
    -- if nothing can be extracted.
    --
    -- Why empty is not allowed: an empty title is an invisible row in the list; users would think the
    -- data is broken, or that it was not submitted and submit again. "Both title and body empty" is
    -- already rejected further upstream.
    title text not null,

    -- Markdown body; may be empty (only a title is valid).
    body text not null default '',

    -- open / resolved. **Two states**, not three like issues.
    --
    -- **No check constraint**: the value is validated by the Service (apimodel.ValidSuggestionStatus,
    -- anything unrecognized is rejected), and a check would only turn "add a level later" into a DDL
    -- migration. Same judgment as issues.status and feedback_reports.level.
    status text not null default 'open',

    -- The email of the person who submitted it (Actor). **Named, but not shown publicly**: only the
    -- platform team can see it.
    --
    -- Not nullable: all routes are behind auth.Protect, and nothing gets here without an identity.
    -- The value only comes from the context; **the request body has no such field**: accepting it
    -- would allow submitting suggestions under someone else's name.
    --
    -- ⚠️ This column is also **the basis of authorization**: people outside the platform team can only
    -- read rows where author = themselves. With no workspace_id to check, this column plays the role
    -- workspace_id plays in issues.
    author text not null,

    created_at text not null,
    -- Changing the title / body / status all refresh it; the Service sets it in one place.
    updated_at text not null,

    -- Insertion order. The list sorts by it descending (newest first), same as issues.
    --
    -- Not sorted by created_at: two rows created in the same millisecond would have an unstable
    -- order, showing up as two rows swapping places after a refresh, which looks like the data is
    -- jumping.
    ord bigserial not null
);

-- The main query for the platform team reading the whole list: by ord descending.
create index if not exists suggestions_ord_idx on suggestions (ord desc);

-- ⭐ Users reading "the ones I submitted" use this, and the platform team filtering by status uses
-- its prefix too.
--
-- author comes first because **it is the most selective column**: one person's suggestions are a
-- few rows, while suggestions in one status are half the table. With (status, author) instead, a
-- user looking up their few rows would first scan half the table.
create index if not exists suggestions_author_idx on suggestions (author, ord desc);

-- The platform team's "unresolved only", the most common filter on that page.
create index if not exists suggestions_status_idx on suggestions (status, ord desc);
