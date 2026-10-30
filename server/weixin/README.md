# weixin — the WeChat channel

Connect a WeChat bot to a workspace by scanning a QR code, bind it to an agent, and the agent answers
the messages the bot receives.

```
cmd/amad → httpd → weixin (Service + Handler)      the page: log in, list, bind, disconnect
cmd/amad → weixin.Poller → channel.Supervisor      one long-poll loop per connected bot
                 ↓                ↓
     shared/infra/weixin     server/channel        protocol / shared channel pieces
```

The protocol lives in [`shared/infra/weixin`](../shared/infra/weixin/) (a Go port of
[Tencent/openclaw-weixin](https://github.com/Tencent/openclaw-weixin), MIT). Everything that every chat
platform shares (dedup, "conversation → session", running a turn, one consumer per account) is in
[`server/channel`](../channel/). This package only holds what is specific to WeChat.

| File | What it does |
| --- | --- |
| `weixin.go` | Types, errors, the `Store` and `Agents` interfaces |
| `service.go` | QR-code login (start / poll / verify code) and connected accounts (list / bind / delete) |
| `handler.go` | HTTP routes (all under a workspace; declared in `authz/matrix.go`) |
| `poller.go` | The long-poll loop, per-user workers, answering one message, typing indicator |
| `message.go` | WeChat message → text for the agent, the `<harness:weixin>` block, `/new` and `/help` |
| `reply.go` | Cleaning and splitting the answer, fixed replies |
| `memstore.go` | In-memory `Store` (PG implementation: `repository/dao/channeldao/weixin_pg.go`) |

## HTTP

| Method | Path | Notes |
| --- | --- | --- |
| `GET` | `/api/workspaces/{workspaceId}/weixin/accounts` | Connected bots (no token, no cursor) |
| `PATCH` | `/api/workspaces/{workspaceId}/weixin/accounts/{accountId}` | `{"agentId": "..."}`, `""` unbinds |
| `DELETE` | `/api/workspaces/{workspaceId}/weixin/accounts/{accountId}` | Disconnect; the poller stops within a minute |
| `POST` | `/api/workspaces/{workspaceId}/weixin/logins` | Start a QR-code login |
| `GET` | `/api/workspaces/{workspaceId}/weixin/logins/{loginId}` | Advance it by one status check (waits up to 20s) |
| `POST` | `/api/workspaces/{workspaceId}/weixin/logins/{loginId}/verify-code` | `{"code": "..."}` when WeChat asks for the number shown on the phone |

Errors are `{"message": "...", "code": "..."}` like the rest of the API.

## How a message is answered

1. The poller of the account (only on the instance holding the Redis lease `weixin:poll:<accountId>`)
   long-polls `getupdates`, saves the cursor to `weixin_accounts.sync_cursor`, and hands each message to
   the worker of its sender. One worker per sender keeps one user's messages in order and never runs two
   turns on the same session.
2. The worker claims the message in `channel_inbound_messages` (a redelivered message stops here),
   reads the account again (the bound agent may have changed), and handles `/new` and `/help`.
3. With a bound, enabled agent, `channel.EnsureSession` finds or creates the session for scope
   `user:<sender>` (one continuous conversation per user), `channel.RunTurn` runs the turn, and the
   answer is sent back in pieces of at most 4000 characters, echoing the message's `context_token`.
   "Typing" is shown while the turn runs.

## Things that are easy to get wrong

- **A login belongs to the person who started it.** Polling checks workspace and starter; anything
  else is "not found". The QR code is a credential.
- **A bot can be connected to one workspace only** (`weixin_accounts.bot_id` is unique). Scanning again
  in the same workspace refreshes the token of the existing row and keeps its binding and cursor.
- **ret / errcode -14 means the token is dead.** The account is marked `expired`, its loop stops, and
  the page tells the user to scan again. Retrying would not help.
- **The cursor is saved before the batch is handled.** A crash in between loses that batch instead of
  answering it twice. Dedup covers the opposite case (the same batch delivered again).
- **Group messages are ignored** (the iLink bot is one-to-one today); they are logged.

## Known limits

- Media is not read: images / files / videos reach the agent as `[图片]` / `[文件]` / `[视频]`, and
  replies are text only. Porting the CDN download (AES-128-ECB) is the next step.
- Turns in progress when the polling instance stops get 20 seconds to finish; after that they are
  cancelled and the user is asked to send the message again. Unlike Feishu there is no recovery of
  unfinished turns after a restart, and messages queued behind a cancelled turn are dropped.
- Scheduled tasks cannot deliver to WeChat yet (that needs the last `context_token` per user stored).
