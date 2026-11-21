# Extending AMA from a downstream Go module

This example is the sanitized counterpart of a private distribution that imports AMA. It shows
where company-specific code stops and public code starts. It does **not** contain a private service
name, endpoint, credential, SDK, or configuration protocol.

The example is a separate Go module so its dependencies and local development `replace` directive
do not change AMA's own `server/go.mod`.

## Boundaries

```text
downstream configuration service
  → implements DocumentStore (downstream)
  → configsource.Provider (public, standard library only)
  → downstream Config and validation (downstream)

downstream database pool factory
  → pg.Connector (public)
  → pg.NewWithDB (public)
  → public transactions and SQLSTATE error handling

downstream binding store and credential directory
  → WorkspaceIntegrationStore / WorkspaceIntegrationCredentialLookup (public)
  → WorkspaceIntegrationService (public)
  → application credential references, never personal credential sharing
```

`examples/company/extension.go` contains the configuration and database adapters.
`examples/company/credentials.go` shows how to keep a private row type and credential protocol
outside AMA while reusing workspace binding rules. A real adapter must also connect
`WorkspaceExists` to its own workspace store; the placeholder in this example is not an
authorization check. `extension_test.go` and `credentials_test.go`
run without a network or real credentials.

## Why configuration loading has two parts

A public `Provider` only retrieves YAML layers and a log-safe source name. It does not require
every downstream to adopt AMA's full `Config` struct. A private distribution normally has extra
settings; putting them into the public schema would leak internal product details and make every
customization an upstream change.

The example decodes those layers into its own `Config`. Public settings come before secrets;
`yaml.Unmarshal` into the same struct preserves fields that the second document does not mention.
A real deployment should also define environment-variable precedence, unknown-field reporting, and
validation, and never log secret values.

If the downstream uses AMA's full configuration schema unchanged, `config.LoadFromProvider` is the
shorter route: it reuses the public parser and environment overrides as well.

## Database ownership

The downstream pool factory owns driver selection, authentication, and pool settings. It returns a
`*sql.DB`; the public `pg.Client` supplies the rest. A failed `Ping` closes the newly created pool,
so a failed startup does not leave a half-open client.

`go.mod` uses `replace ../../server` only to test the example against the checkout. A real
downstream uses a released AMA version or an immutable commit and does not commit a path on a
developer's machine.

## Verify

From the repository root:

```bash
go -C examples/company test ./...
go -C examples/company vet ./...
```

## What remains private

- service names, namespaces, endpoints, account identifiers, and credentials;
- the SDK and protocol used to fetch configuration;
- driver-specific pool setup and deployment environment detection;
- private configuration fields and validation rules.

The public repository should receive only reusable interfaces, generic implementations, tests, and
sanitized examples such as this one.
