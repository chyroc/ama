-- ┌── Table outline: ark_skill_uploads ────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ workspace_id     text  not null
-- │ skill_id         text  not null
-- │ content_hash     text  not null
-- │ key_fingerprint  text  not null default ''::text
-- │ ark_skill_id     text  not null
-- │ ark_version      text  not null
-- │ created_at       text  not null
-- │
-- │ primary key ark_skill_uploads_pkey
-- │      (workspace_id, skill_id, content_hash, key_fingerprint)
-- └────────────────────────────────────────────────────────────────────────────

-- The mapping "some content version of an AMA skill → the skill_id on the Ark side".
--
-- # This table exists because Ark has no "update skill" endpoint
--
-- Ark's Skills API only has create and get details (docs/ark-managed-agents/api/06-Skills/);
-- `CreateSkill` **produces a new skill_id on every call**, with no upsert, no version change, and no
-- delete either. So "has this skill been uploaded" can only be recorded by us: without it every new
-- conversation would upload it again, and every copy stays on the Ark side forever.
--
-- # Why content_hash is in the primary key
--
-- When a skill's content changes it must be uploaded again (a new skill_id), so the key must include
-- "which version". The rule is the content fingerprint computed by core.SkillFingerprint, **not**
-- updated_at or latest_version: those two change even when only tags change, and using them as the
-- key would upload a copy for nothing on every unrelated edit.
--
-- Rows of old versions are kept: historical sessions' Ark Agents may still point to that skill_id.
-- A row is only a few dozen bytes anyway, and that skill on the Ark side cannot be deleted either.
--
-- # Why it is split by workspace
--
-- Ark API Keys are **workspace-level** (ark_secrets has one row per workspace), and keys of different
-- workspaces point to different projects. The same AMA skill in two workspaces must be two different
-- Ark skills; not splitting by workspace would give workspace B a skill_id that does not exist in its
-- own project.
--
-- # No doc jsonb
--
-- It is not an external entity, has no HTTP contract, and the fields are just a few scalars; real
-- columns are plainer than jsonb. The same judgment as the internal Feishu tables.
create table if not exists ark_skill_uploads (
    workspace_id text not null,
    skill_id     text not null,
    content_hash text not null,
    -- key_fingerprint marks "which key this skill was uploaded with". The argument for splitting by
    -- workspace (different keys point to different projects) must be applied again now that
    -- agent-level credentials exist: the same workspace may have two keys, so the dimension narrows
    -- to "workspace + that key". For the meaning of an empty string see the comment on
    -- ArkSkillUpload.
    key_fingerprint text not null default '',
    ark_skill_id text not null,
    ark_version  text not null,
    created_at   text not null,
    primary key (workspace_id, skill_id, content_hash, key_fingerprint)
);
