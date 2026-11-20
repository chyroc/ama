# Troubleshooting

Branch on `error.code`; do not parse message text when a stable code exists.

| Code | Action |
| --- | --- |
| `AUTH_REQUIRED` / `TOKEN_EXPIRED` / `REFRESH_FAILED` | Follow [auth.md](auth.md); when needed, the user runs `setup_command` in their own terminal. |
| `FORBIDDEN` | Report missing permission. |
| `VOLCENGINE_REAL_NAME_REQUIRED` | Do not treat this as an expired API key. Show `verification_url`, ask the user to complete real-name verification on Volcengine, then run `auth status` and retry the allocation. Existing allocated CUA invocations remain usable. |
| `VOLCENGINE_REAL_NAME_CHECK_UNAVAILABLE` | Retry the same read/status operation later. If the failed operation was about to allocate a new CUA, do not submit a duplicate request while status is unknown. |
| `DESKTOP_NOT_BOUND` | Report that no CUA desktop is provisioned. |
| `CONFIRMATION_REQUIRED` / `ConfirmationRequired` | Do not bypass confirmation. Run shutdown only after the user explicitly requests it, then include `--confirm`. |
| `IDEMPOTENCY_KEY_REQUIRED` / `IdempotencyKeyRequired` | Supply one stable unique key for the user-requested lifecycle action and reuse it for retries of that same action. |
| `DesktopRetentionExpired` / `DesktopNotRecoverable` | The exact retained desktop cannot be recovered. Do not silently omit its id and allocate a replacement; explain the state and ask whether the user wants a new desktop. |
| `DesktopRetentionCleanupPending` | Cleanup of an expired primary desktop is still in progress. Report it and retry start later only if the user still wants it. |
| `BillingAccessSuspended` / `AgentPlanSubscriptionInactive` | Starting would violate current billing or subscription eligibility. Report the condition; do not bypass it with a new allocation. |
| `IdempotencyConflict` | The key is already bound to a different start or shutdown request. Do not change the key to force the action; reconcile the original request first. |
| `ACTIVE_RUN_CONFLICT` | Stop; the new task did not start. Tell the user the desktop is busy. |
| `REVISION_CONFLICT` / `RevisionConflict` | The desktop changed before shutdown was accepted. Inspect current desktop state; do not submit a new shutdown without renewed user intent. |
| `runtime_capability_required` | Revoke the failed desktop ticket and run `desktop access` once for a new URL. Do not rewrite it. If the new URL also fails, report that desktop access is unavailable. |
| `INVOCATION_NOT_FOUND` | Recheck the ID or use `--last`; never guess. |
| `INVOCATION_NOT_WAITING_INPUT` | Check task status before answering. |
| `CONTEXT_NOT_FOUND` | Use `context list` or the exact context ID. |
| `ARTIFACT_NOT_FOUND` | Recheck `artifact list`; a placeholder may have no bytes. |
| `PAYLOAD_TOO_LARGE` | Shorten task input or use an artifact smaller than 256 MiB. |
| `MODEL_TIMEOUT` | Report the safe reason and request ID; retry only when requested. |
| `DESKTOP_UNHEALTHY` / `SESSION_CLEANUP` / `UPSTREAM_FAILURE` | Report safe diagnostic fields; do not retry blindly. |
| `GATEWAY_TIMEOUT` / `CUA_BACKEND_UNAVAILABLE` / `RATE_LIMITED` / `NETWORK` | Retry the same watch/status/result operation; do not create a duplicate task. |
| `VALIDATION_ERROR` | Correct the indicated argument or choose a new output path. |
| `COOKIECLOUD_NODE_REQUIRED` | Automatic configuration needs Node.js 22+. Use the manual CookieCloud page flow when it is unavailable. |
| `COOKIECLOUD_CHROME_UNAVAILABLE` | Check that CookieCloud is installed and the user allowed the Chrome connection. Preserve the browser and login state; do not claim an unconfirmed upload succeeded. |
| `COOKIECLOUD_API_UNAVAILABLE` | Report that automatic setup is unavailable and offer the manual CUA page flow. Keep the current login and requested desktop. |
| `COOKIECLOUD_CONFIG_INVALID` | Report that setup could not be completed. Chrome settings were not written; do not expose the raw configuration. |
| `COOKIECLOUD_DOMAIN_SCOPE` | Explain which requested websites are missing from the CUA selection and ask the user to adjust it there. Preserve the existing connection. |
| `TARGET_CAPABILITY_UNAVAILABLE` | Report that the requested Credential Agent resource sync is unavailable on this desktop and stop setup. |
| `AGENT_INSTALL_FAILED` / `TARGET_AGENT_UNAVAILABLE` | Report that Credential Agent setup failed and preserve the existing installation. |
| `SOURCE_SETUP_FAILED` / `SOURCE_BOOTSTRAP_REQUIRED` / `SOURCE_ENROLLMENT_REQUIRED` | Report that Credential Agent setup is incomplete; follow any returned local setup instructions before resource delivery. |
| `SOURCE_BROWSER_USER_ACTION_REQUIRED` / `BROWSER_SETUP_REQUIRED` | Follow the CookieCloud flow in [credentials.md](credentials.md) for browser login sync. Use `credentials setup --skip-browser` for other resources. |
| `INTERNAL` | Retry a read-only request once. For an unconfirmed mutation or resource delivery, report the failure rather than submitting it again. |

## Common situations

- Login is required: show `setup_command` and ask the user to run it in their local terminal. Never ask the user to paste a key into chat.
- A task appears stuck: continue watching while the outcome is `in_progress`.
- The desktop is busy: do not retry after `ACTIVE_RUN_CONFLICT` unless the user asks later.
- An artifact is missing or HTML: ask CUA to export a registered downloadable artifact.
- An output path already exists: select a new path; overwriting is intentionally unsupported.
- CookieCloud upload fails: verify that all five configuration fields came from this desktop's CUA page, including its server URL and extra headers; keep passwords and upload credentials outside chat.
