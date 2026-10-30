-- ┌── Table outline: ark_bindings ─────────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ workspace_id        text    not null
-- │ agent_id            text    not null
-- │ ark_agent_id        text    not null
-- │ ark_environment_id  text    not null
-- │ ord                 bigint  not null (sequence)
-- │ doc                 jsonb   not null
-- │
-- │ primary key ark_bindings_pkey
-- │      (workspace_id, agent_id)
-- │ index ark_bindings_workspace_idx
-- │      (workspace_id, ord)
-- └────────────────────────────────────────────────────────────────────────────
-- ├── Keys of the doc column (jsonb) (= json.Marshal(ArkBinding))
-- │ ⚠️ This block is **generated from the json tags of the Go type**, not by sampling data
-- │    in the database. Keys with omitempty are absent when the value is zero, so sampling
-- │    always misses them, yet they are part of the contract. After changing the type, run:
-- │        cd server && go run ./cmd/gen-doc-outline -w
-- │    Source of truth: model/persistmodel/persistmodel.go, type ArkBinding
-- │
-- │ workspaceId        string
-- │ agentId            string
-- │ arkAgentId         string
-- │ arkEnvironmentId   string
-- │ createdAt          string
-- │ skillsFingerprint  string
-- │ arkSkillIds        []string   ← omitempty, the key may be missing entirely
-- │ specFingerprint    string   ← omitempty, the key may be missing entirely
-- │ keyFingerprint     string   ← omitempty, the key may be missing entirely
-- ┤──────────────────────────────────────────────────────────────────────────

-- The mapping "AMA agent → Ark Agent + Environment".
--
-- **This table exists because of a hard constraint of Ark**: an Environment's name is unique within a
-- project, and creating a duplicate returns 409 ResourceConflict. If the mapping were only in process
-- memory, after a restart the cache is empty, creating the same name again gives 409 right away, and
-- that workspace can never start a new conversation (and every restart would also create one more Ark
-- Agent, since Agents may share names). So it must survive restarts.
--
-- The primary key is (workspace_id, agent_id): one AMA agent maps to one Ark Agent in a workspace.
-- Environments are **reused per workspace**, so rows of the same workspace have the same
-- ark_environment_id.
create table if not exists ark_bindings (
    workspace_id text not null,
    agent_id     text not null,
    -- The two extracted columns serve queries and debugging; doc is still the authoritative data, as
    -- in the other tables.
    ark_agent_id       text not null,
    ark_environment_id text not null,
    ord  bigserial not null,
    doc  jsonb not null,
    primary key (workspace_id, agent_id)
);

-- ListArkBindings lists by workspace, to reuse environments. ord keeps the order stable (see the file
-- header).
create index if not exists ark_bindings_workspace_idx on ark_bindings (workspace_id, ord);
