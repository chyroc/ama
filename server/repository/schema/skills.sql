-- ┌── Table outline: skills ───────────────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id            text    not null
-- │ workspace_id  text    not null
-- │ name          text    not null
-- │ ord           bigint  not null (sequence)
-- │ doc           jsonb   not null
-- │
-- │ primary key skills_pkey
-- │      (id)
-- │ index skills_name_idx
-- │      (workspace_id, name)
-- │ index skills_workspace_idx
-- │      (workspace_id, ord)
-- └────────────────────────────────────────────────────────────────────────────
-- ├── Keys of the doc column (jsonb) (= json.Marshal(Skill))
-- │ ⚠️ This block is **generated from the json tags of the Go type**, not by sampling data
-- │    in the database. Keys with omitempty are absent when the value is zero, so sampling
-- │    always misses them, yet they are part of the contract. After changing the type, run:
-- │        cd server && go run ./cmd/gen-doc-outline -w
-- │    Source of truth: model/apimodel/apimodel.go, type Skill
-- │
-- │ id             string
-- │ workspaceId    string
-- │ name           string
-- │ displayTitle   string
-- │ description    string
-- │ instructions   string
-- │ source         SkillSource
-- │ latestVersion  string
-- │ tags           []string
-- │ files          []SkillFile   ← omitempty, the key may be missing entirely
-- │ createdAt      string
-- │ updatedAt      string
-- │ createdBy      string   ← omitempty, the key may be missing entirely
-- │ updatedBy      string   ← omitempty, the key may be missing entirely
-- ┤──────────────────────────────────────────────────────────────────────────

create table if not exists skills (
    id           text primary key,
    workspace_id text not null,
    name         text not null,
    ord          bigserial not null,
    doc          jsonb not null
);

create index if not exists skills_workspace_idx on skills (workspace_id, ord);

-- **This is deliberately not a UNIQUE index, although "skill names are unique within a workspace"
-- sounds obvious.**
--
-- Uniqueness is a Service-layer rule, and only on the **create** side: Service.CreateSkill first asks
-- SkillNameTaken and returns ErrConflict on a clash; but Service.UpdateSkill **deliberately does not
-- check**, copying the frontend LocalApiClient's updateSkill (api.ts:428-436, which does not check
-- either). TestSkillNameUniquePerWorkspaceOnly in core/service_test.go pins this asymmetry: "renaming
-- into a clash should be allowed".
--
-- With UNIQUE, that legitimate operation protected by the test would hit 23505 in the database and
-- show up as a 500: a case that passes on MemStore would turn red on PG. So only a normal index is
-- built here, to speed up the duplicate check in SkillNameTaken.
create index if not exists skills_name_idx on skills (workspace_id, name);
