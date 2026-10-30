-- ┌── Table outline: workspace_hosts ──────────────────────────────────────
-- │ ⚠️ This block is **generated from the catalog of a real database**, not written by
-- │    hand. The shape the DDL below actually creates is what counts. After changing
-- │    the DDL, run:
-- │        python3 scripts/gen-schema-outline.py
-- │
-- │ id            text                      not null
-- │ workspace_id  text                      not null
-- │ host_id       text                      not null
-- │ added_by      text                      not null default ''::text
-- │ created_at    timestamp with time zone  not null
-- │
-- │ primary key workspace_hosts_pkey
-- │      (id)
-- │ index workspace_hosts_host_idx
-- │      (host_id)
-- │ unique workspace_hosts_unique_idx
-- │      (workspace_id, host_id)
-- └────────────────────────────────────────────────────────────────────────────

-- workspace_hosts: bindings of "which hosts this workspace may use", one row each.
--
-- hosts is an account-level resource (machines registered by their owner); to choose one as the
-- executor for an agent in some workspace, the host owner must first bind it into that workspace. The
-- shape copies workspace_integrations (account-level resource → workspace binding).
--
-- # v1 only allows the host owner to bind
--
-- The Go write path (service/host) checks host.owner_email == the current actor; the storage layer
-- does not check, the same split as workspace_integrations. Open it up later if "workspace members
-- sharing someone else's host" is wanted.
--
-- # The foreign key with on delete cascade is deliberate (the opposite of workspace_integrations)
--
-- workspace_integrations deliberately has no foreign key, because a deleted credential is a legal
-- "dangling reference" state, and unbinding should not delete the owner's credential. Here it is
-- different: after a host row is revoked by its owner (DELETE /api/hosts/{id}), workspace bindings
-- pointing to it have no value of their own, and keeping them would only make a machine that no
-- longer exists show up in the agent form; cascading delete is exactly the wanted meaning. The
-- host_id on an agent is a doc/column snapshot, and the read path reports it as invalid by "binding
-- does not exist" (same as when a cloud container binding is deleted).
create table if not exists workspace_hosts (
    id           text        not null,
    workspace_id text        not null,
    host_id      text        not null,

    -- Who bound it, for auditing.
    added_by     text        not null default '',

    created_at   timestamptz not null,

    constraint workspace_hosts_pkey primary key (id),
    -- When the host is gone the binding goes too; see the file header.
    constraint workspace_hosts_host_fk
        foreign key (host_id) references hosts (id) on delete cascade
);

-- The same workspace does not bind the same machine twice; let the database decide (TOCTOU, same
-- reason as workspace_integrations).
create unique index if not exists workspace_hosts_unique_idx
    on workspace_hosts (workspace_id, host_id);

-- Used when deleting a host / listing "which workspaces this machine is bound to" (the cascading delete
-- itself uses the foreign key, not this, but the read path and debugging look up by host_id).
create index if not exists workspace_hosts_host_idx
    on workspace_hosts (host_id);
