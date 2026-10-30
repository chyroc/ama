-- ┌── Table outline: lark_user_app_prefs ──────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ actor        text                      not null
-- │ lark_app_id  text                      not null
-- │ updated_at   timestamp with time zone  not null
-- │
-- │ primary key lark_user_app_prefs_pkey
-- │      (actor)
-- └────────────────────────────────────────────────────────────────────────────

-- lark_user_app_prefs: "which Feishu app do I use by default", one row per person.
--
-- # Why it is needed: one person may have several Feishu apps
--
-- A user may have several apps in different workspaces (possibly all with the same name, e.g. "AMA
-- Assistant"). The card on the integration management page must make clear "which one is in use",
-- and before this table the answer was "go through the workspaces they are in and take the
-- **first**": the user had no way to change it, and the `cli_xxx` line in the UI looked hard-coded.
--
-- # ⚠️ Why account-level (primary key is only the actor), not (actor, workspace)
--
-- This is the easiest part of this change to get backwards. **Authorization is an account-level
-- action; choosing an app is a workspace-level constraint**. The two have different dimensions and
-- each belongs in its own place:
--
--   * Which app to use at injection → **does not look at this table**; it looks at which workspace
--     that run belongs to (feishu.ResolveApp takes LarkAppQuery.WorkspaceID, pinned with no fallback).
--     That is a correctness issue: tokens are bound to apps, and the wrong one gives 401 or cannot
--     read the group, with no error.
--   * This table only answers "which one to use when there is no workspace context": the card on
--     the integration management page, and the two actions start/remove authorization. They have no
--     workspace to speak of anyway.
--
-- With (actor, workspace), users would have to choose again, or even scan a code again, every time
-- they enter a new workspace, which is exactly what the "authorization is account-level" design
-- avoids.
--
-- # Store app_id, not "which number"
--
-- A number would **silently point to another app** when the user is removed from some workspace or
-- some app is disconnected, a mismatch with zero errors. With app_id, invalidation shows up as "this
-- app cannot be found", and the read path (feishu.ResolveApp) quietly falls back to the default
-- order; at most the user notices their choice was reset.
create table if not exists lark_user_app_prefs (
    -- Whose preference. **This is the Actor ID** (the user's email), same as user_preferences /
    -- user_onboarding.
    actor       text        not null,
    -- The Feishu app they chose (cli_xxx).
    --
    -- ⚠️ **No foreign key**: the Feishu app table (feishu_apps) belongs to another package, and this
    -- row dangling after the app is disconnected is **acceptable**: the read path falls back to the
    -- default order when it cannot find it (see appInWorkspace). A foreign key would instead make
    -- "disconnect app" fail because of someone else's preference.
    lark_app_id text        not null,

    updated_at  timestamptz not null,

    constraint lark_user_app_prefs_pkey primary key (actor)
);

comment on table lark_user_app_prefs is
    '"Which Feishu app I use by default", one row per person. Account-level: injection picks per the run''s workspace and does not read this table.';
comment on column lark_user_app_prefs.lark_app_id is
    'Stores app_id instead of an index: an index would silently point at another app after one is disconnected, a mismatch with no error. When dangling, reads fall back to the default order.';
