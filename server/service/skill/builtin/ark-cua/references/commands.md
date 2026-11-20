# AgentPlan CUA Skill commands

All commands: `python3 <skill_dir>/scripts/cua.py <command> [options]`.
The CLI uses its bundled service address and the current login.

## Authentication and health

```bash
python3 scripts/cua.py auth status
python3 scripts/cua.py auth login [--manual]
python3 scripts/cua.py auth logout
python3 scripts/cua.py ping
python3 scripts/cua.py diagnose
```

Login reuses an existing eligible AgentPlan account. If login is required, the user enters their key in their own terminal. Use `--manual` only for an explicitly requested account change; see [auth.md](auth.md). `ping` and `diagnose` inspect availability without creating a task.

## Core delegation

```bash
python3 scripts/cua.py delegate --objective "<request>" [--wait-ms 0]
python3 scripts/cua.py watch (--invocation-id <id> | --last) [--wait-ms 20000]
python3 scripts/cua.py answer (--invocation-id <id> | --last) --answer "<reply>"
python3 scripts/cua.py result (--invocation-id <id> | --last) [--timeout 600]
python3 scripts/cua.py cancel (--invocation-id <id> | --last)
```

Call `delegate` once and follow `next.command` until a terminal outcome. Use `cancel` only on explicit user request.

## Desktop inspection, start/recovery, shutdown, and CUA App access

```bash
python3 scripts/cua.py desktop list
python3 scripts/cua.py desktop access [--desktop <id>]
python3 scripts/cua.py desktop revoke-access (--ticket <ticket> | --access-url <url>)
python3 scripts/cua.py desktop start --idempotency-key <stable-unique-key> \
  [--desktop <id>]
python3 scripts/cua.py desktop shutdown --confirm --idempotency-key <stable-unique-key> \
  [--desktop <id>]
python3 scripts/cua.py desktop operation --operation-id <id>
python3 scripts/cua.py model get
```

`desktop shutdown` requires an explicit request to shut down, release or stop billing. It ends billing, revokes access and interrupts active tasks; the desktop is then stopped or deleted according to retention policy. If the user wants current work to finish first, wait for that task's terminal outcome. Include `--confirm`, reuse one stable idempotency key for retries, and follow `next.command` until terminal. When `data.operation.recoverable` is true, retain `data.desktop.desktop_id` and the `data.operation.purge_after` recovery deadline.

`desktop start` starts or recovers a desktop and reactivates billing, so it also requires an explicit request. Pass the exact `--desktop` id to recover a particular desktop; omit it only when the user accepts service selection or a new desktop. If that exact desktop is expired or unavailable, ask before creating a replacement. Reuse the same idempotency key for retries and follow `next.command` until the operation succeeds. Use `data.action`, `data.restoring` and `data.newly_allocated` to explain whether the desktop was reused, recovered or newly created.

`desktop access` returns a fresh temporary URL. Pass `--desktop <id>` for an exact owned desktop. Return `full_interface_url`, falling back to `access_url`, without changing the URL. Revoke a URL that may have leaked or is no longer needed.

If opening the URL fails, follow [troubleshooting.md](troubleshooting.md).

The Skill intentionally excludes reboot/reset and persistent model modification. Shutdown and unified start/recovery are its supported mutating desktop lifecycle actions.

## Tasks and reusable contexts

```bash
python3 scripts/cua.py task run --objective "<request>" \
  [--desktop <id-or-name>] [--title "<title>"] [--wait-ms 0]
python3 scripts/cua.py task continue (--context-id <id> | --last-context) \
  --objective "<next step>" [--wait-ms 0]
python3 scripts/cua.py task status (--task-id <id> | --last)
python3 scripts/cua.py task result (--task-id <id> | --last) [--timeout 600]
python3 scripts/cua.py task answer (--task-id <id> | --last) --answer "<reply>"
python3 scripts/cua.py task cancel (--task-id <id> | --last)

python3 scripts/cua.py context list
python3 scripts/cua.py context create [--title "<title>"] [--desktop <id-or-name>]
python3 scripts/cua.py context add-note (--context-id <id> | --last-context) --text "<background>"
python3 scripts/cua.py context show (--context-id <id> | --last-context)
python3 scripts/cua.py timeline show (--context-id <id> | --last-context)
```

Use the identifiers returned by each command. Relay CUA's questions and submit the user's replies; do not suppress or answer them on the user's behalf.

## Artifacts

```bash
python3 scripts/cua.py artifact list (--task-id <id> | --last)
python3 scripts/cua.py artifact save (--artifact-id <id> | --last) \
  [--task-id <id>] [--output <new-path>]
```

`artifact save` never overwrites an existing path and does not create a missing parent directory. Omit `--output` to use a secure temporary file. Downloads are limited to 256 MiB; HTML/interstitial content and missing artifacts are not written.

## Browser login sync

```bash
python3 scripts/cua.py browser-sync --desktop-id <id> --cdp-url <local-cdp-url> \
  --domain <full-domain> [--domain <another-domain>] [--confirm-domains]
```

Automatic setup needs Node.js 22+, the official CookieCloud extension and the user's Chrome connection permission. Use `--confirm-domains` only after the user approves the reported website-list change. Upload and website login are separate results; follow [credentials.md](credentials.md).

If the user needs the CUA App link, run `desktop access [--desktop <id>]`.

## Credential Agent resources

```bash
python3 scripts/cua.py credentials status [--desktop-id <id>]
python3 scripts/cua.py credentials setup --skip-browser [--desktop-id <id>]
python3 scripts/cua.py credentials sync env --desktop-id <id> <name>...
python3 scripts/cua.py credentials sync secret --desktop-id <id> <name>...
python3 scripts/cua.py credentials sync credential-set --desktop-id <id> --type <type> --name <name>
python3 scripts/cua.py credentials sync file --desktop-id <id> --profile <profile>
python3 scripts/cua.py credentials reset --desktop-id <id> [--device-id <exact-id>]
```

These commands use Credential Agent for named resources; follow [credentials.md](credentials.md). `setup --skip-browser` prepares devices without copying resource values. Resource arguments name what to sync, never the secret values. Agent readiness does not indicate browser login status.
