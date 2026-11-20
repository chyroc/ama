# Auth

Use the user's existing AgentPlan login. The CLI handles authentication; do not read credential files or ask for API keys in chat.

## Login

1. Run `auth status`. The CLI reuses a saved login or an eligible personal AgentPlan Max account from `arkcli` without changing the user's arkcli settings.
2. On `AUTH_REQUIRED`, follow a recoverable `arkcli_hint` when available. Otherwise ask the user to run the returned `setup_command` in their own terminal and enter the API key through its hidden prompt.
3. After the user completes login, run `auth status` again. Do not treat a failed login as permission to switch accounts.

## Switch accounts

When the user explicitly requests a different account or API key, ask them to run this in their own terminal:

```bash
python3 scripts/cua.py auth login --manual
```

The user enters the key through a hidden prompt, not in chat, command arguments or environment variables. This login remains active until `auth logout`. If it is rejected, repeat the returned manual login flow rather than silently using another account.

An agent-run login command returns instructions when it cannot prompt interactively; relay them to the user.

## Real-name verification

If `real_name_verification.status` is `unverified` or an operation returns `VOLCENGINE_REAL_NAME_REQUIRED`, provide `verification_url` and ask the user to complete verification on Volcengine before allocating a new desktop. Then rerun `auth status`. Existing allocated desktops remain usable; this is not an invalid API key.

## Auth Errors

| Error | Action |
| --- | --- |
| `AUTH_REQUIRED` | Follow the login steps above. When `manual_login_required=true`, use the returned manual `setup_command`. |
| `TOKEN_EXPIRED` / `REFRESH_FAILED` | Ask the user to run `setup_command` in their local terminal, then retry after login. |
| `FORBIDDEN` | Report missing permission; do not retry the same operation with the same key. |
| `VOLCENGINE_REAL_NAME_REQUIRED` | Follow the verification steps above without replacing a valid login. |
| `VOLCENGINE_REAL_NAME_CHECK_UNAVAILABLE` | Report the temporary failure; check status before retrying an allocation so a desktop or task is not created twice. |

## Logout

`auth logout` removes this Skill's saved login. It does not sign out of arkcli, whose account may be reused by later commands.
