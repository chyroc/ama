-- ┌── Table outline: issue_comments ───────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id            text    not null
-- │ issue_id      text    not null
-- │ workspace_id  text    not null
-- │ body          text    not null
-- │ author        text    not null
-- │ created_at    text    not null
-- │ updated_at    text    not null
-- │ ord           bigint  not null (sequence)
-- │
-- │ primary key issue_comments_pkey
-- │      (id)
-- │ index issue_comments_issue_idx
-- │      (issue_id, ord)
-- │ index issue_comments_ws_idx
-- │      (workspace_id)
-- └────────────────────────────────────────────────────────────────────────────

create table if not exists issue_comments (
    id text primary key,

    issue_id text not null,

    -- A redundant column, **on purpose**: deleting a workspace cascades by it directly, without
    -- joining back to issues first. That join would be pure extra cost inside the cascading delete
    -- transaction, and this column never changes (comments never move to another workspace), so
    -- there is no consistency risk.
    workspace_id text not null,

    body text not null,
    author text not null,
    created_at text not null,
    -- **Comments can be edited**, so it also has updated_at. The frontend uses it to show "edited".
    updated_at text not null,

    -- Comments are listed in insertion order, **oldest first** (a conversation is read top down),
    -- the opposite of issues' newest-first order.
    ord bigserial not null
);

create index if not exists issue_comments_issue_idx on issue_comments (issue_id, ord);
-- Bulk delete by this column when a workspace is deleted.
create index if not exists issue_comments_ws_idx on issue_comments (workspace_id);
