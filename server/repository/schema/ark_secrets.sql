-- ┌── Table outline: ark_secrets ──────────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ workspace_id  text   not null
-- │ doc           jsonb  not null
-- │
-- │ primary key ark_secrets_pkey
-- │      (workspace_id)
-- └────────────────────────────────────────────────────────────────────────────
-- ├── Keys of the doc column (jsonb) (= json.Marshal(ArkSecret))
-- │ ⚠️ This block is **generated from the json tags of the Go type**, not by sampling data
-- │    in the database. Keys with omitempty are absent when the value is zero, so sampling
-- │    always misses them, yet they are part of the contract. After changing the type, run:
-- │        cd server && go run ./cmd/gen-doc-outline -w
-- │    Source of truth: model/apimodel/apimodel.go, type ArkSecret
-- │
-- │ ⚠️ APIKey has **no json tag**, so the key name is `APIKey` (upper case), not `apiKey`;
-- │ ⚠️ it is encrypted with cryptobox before it is stored (`v1:` prefix); the value in the
-- │    database is ciphertext, not the key.
-- │
-- │ APIKey  string
-- ┤──────────────────────────────────────────────────────────────────────────

-- One row per workspace, so workspace_id is the primary key directly.
-- doc contains the **plain-text API key** (apimodel.ArkSecret.APIKey) and never goes into any HTTP response.
create table if not exists ark_secrets (
    workspace_id text primary key,
    doc          jsonb not null
);
