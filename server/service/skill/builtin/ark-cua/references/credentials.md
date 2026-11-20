# Login sync and credential resources

Use CookieCloud for browser login sync. Named environment variables, Secrets, Credential Sets and managed files use Credential Agent.

## Browser session sync

Reuse the current login and requested desktop. Browser login sync does not require `credentials setup`.

1. Identify the requested desktop and full website domains, such as `jd.com`. The user installs the official [CookieCloud Chrome extension](https://chromewebstore.google.com/detail/cookiecloud/ffjiejobkoibkjlhjnlgmcnnigeelbdl) on their own computer and signs in to those websites in the same Chrome.
2. Automatic configuration needs Node.js 22+ and the user's permission to connect to that Chrome. In Chrome 144+, the user enables `chrome://inspect/#remote-debugging` and approves the connection prompt; see [Chrome's guide](https://developer.chrome.com/blog/chrome-devtools-mcp-debug-your-browser-session). Use that browser's authorized local debugging address. Keep the existing browser and login state; do not restart Chrome or replace its profile to obtain access.
3. Run:

   ```bash
   python3 scripts/cua.py browser-sync --desktop-id <id> \
     --cdp-url <authorized-local-address> --domain <full-domain>
   ```

   Repeat `--domain` for each user-selected website; do not leave the list empty or add other websites.
4. If the helper returns `user_action_required`, explain the local and requested website lists. Use `--confirm-domains` only after the user agrees to replace the local list. An empty existing list means all websites; unrecognized keywords need the user's review in the extension. If the requested domains are outside the existing CUA connection, ask the user to adjust that selection in CUA rather than resetting the connection.
5. Report `configured` as configuration saved, and `uploaded` as a confirmed cloud upload. Supported desktops receive updates automatically, usually within a minute. Refresh the selected remote websites to apply page storage and confirm login. If automatic reception is unavailable or immediate delivery is needed, open **Connectors → Browser session sync（连接器 → 浏览器登录态）** in CUA and use **Import selected websites（导入所选网站）**.

The helper reuses the connection and preserves other extension settings. It closes its own connection afterward; the user can then turn off remote debugging if they enabled it for this operation. Keep configuration passwords, upload credentials and Cookie values out of model-visible tools and chat.

If automatic setup is unavailable, explain the limitation and offer manual configuration. Do not repeat a failed or unconfirmed upload automatically.

### Manual configuration

1. Open the requested desktop's CUA App with a fresh `desktop access [--desktop <id>]` URL.
2. Go to **Connectors → Browser session sync**. Reuse the existing connection, or create one for the user's selected domains if none exists.
3. In the user's local CookieCloud extension, choose upload mode and copy all five fields from this desktop's CUA page: server URL, UUID, encryption password, extra request headers and domain keywords. Use the supplied server URL rather than the extension's default public server. Copy sensitive fields directly between the page and extension, never through chat.
4. Enable LocalStorage if needed, refresh the selected local websites and upload. Confirm the upload status in CUA, then refresh the remote websites to confirm login. Use **Import selected websites** if automatic reception is unavailable.

Upload, browser delivery and website login are separate results. Automatic reception requires a desktop with the receiver extension; other desktops use **Import selected websites**. If the CUA sync page is also unavailable, report the limitation. Do not use a delegated cloud task to read local Cookies.

## Credential Agent resources

The local computer is the source and the requested cloud desktop is the target. The bundled CLI prepares Credential Agent as needed; these resources do not require the Chrome extension.

1. Run `credentials status [--desktop-id <id>]` to check readiness without changing devices. Agent readiness does not indicate browser login status. On `TARGET_CAPABILITY_UNAVAILABLE`, report that the requested resource sync is unavailable and stop setup.
2. Run `credentials setup --skip-browser [--desktop-id <id>]` when resource setup is requested or the target is not ready.
3. Run one exact resource command:

   ```bash
   python3 scripts/cua.py credentials sync env --desktop-id <id> <name>...
   python3 scripts/cua.py credentials sync secret --desktop-id <id> <name>...
   python3 scripts/cua.py credentials sync credential-set --desktop-id <id> --type <type> --name <name>
   python3 scripts/cua.py credentials sync file --desktop-id <id> --profile <profile>
   ```

4. Report the delivery result and any Job ID. A timeout or failure does not prove that nothing was delivered; do not submit the same delivery again blindly.

Resource arguments name what to deliver, not secret values:

| Resource | Source and use |
| --- | --- |
| `env <name>...` | Syncs only the named local environment variables; it does not set the global desktop environment. |
| `secret <name>...` | Names the Secrets to deliver. The Agent may request values through a hidden local terminal prompt. |
| `credential-set --type <type> --name <name>` | Selects a supported set type and name; the Agent handles any field prompts in the user's local terminal. |
| `file --profile <profile>` | Selects one supported built-in file profile, such as `codex-auth` or `aws-credentials`; the argument is not an arbitrary file path. |

If an Agent prompt cannot run interactively, ask the user to run the same command in their own terminal. Keep secret values and pairing codes inside the Agent flow, outside chat, model context and command arguments. Delivery is separate from using a resource; do not claim it changed the global environment or arbitrary files.

## Reset Credential Agent

```bash
python3 scripts/cua.py credentials reset --desktop-id <id> [--device-id <exact-id>]
```

Reset only the requested desktop's Agent. The command must confirm revocation of the exact target Device ID before resetting it; if revocation is unconfirmed, stop. Manage CookieCloud connections through the CUA page separately.
