# shared/infra/weixin — the WeChat iLink bot protocol

The wire protocol of WeChat's iLink bot API: QR-code login, long-polling for messages, sending text, and
the typing indicator. No storage and no business rules; the channel itself is `server/weixin`.

## Where it comes from

This is a Go port of the client side of [Tencent/openclaw-weixin](https://github.com/Tencent/openclaw-weixin)
(MIT License, Copyright Tencent), the official WeChat channel plugin for OpenClaw. Its
`docs/protocol.md` describes the protocol; the files we followed are `src/api/api.ts`,
`src/api/types.ts`, `src/auth/login-qr.ts`, `src/monitor/monitor.ts` and `src/messaging/send.ts`.
A read-only clone can live in `repos/openclaw-weixin` (git-ignored) for reference.

The protocol version we implement is `DefaultChannelVersion` (the openclaw-weixin release we ported). It
is sent as `base_info.channel_version` and encoded as `iLink-App-ClientVersion` (`0x00MMNNPP`).

## What is ported

| Call | Path | Notes |
| --- | --- | --- |
| `GetBotQRCode` | `POST /ilink/bot/get_bot_qrcode?bot_type=3` | Always on the login base URL; no `Authorization`. |
| `GetQRCodeStatus` | `GET /ilink/bot/get_qrcode_status` | Long poll. Our own deadline reads as `wait`. Only the common app headers. |
| `GetUpdates` | `POST /ilink/bot/getupdates` | Long poll with the `get_updates_buf` cursor. Our own deadline is an empty response that keeps the cursor. |
| `SendText` | `POST /ilink/bot/sendmessage` | One finished text item; echoes the conversation's `context_token`. |
| `GetTypingTicket` / `SendTyping` | `getconfig` / `sendtyping` | Typing indicator, status 1 = typing, 2 = cancel. |
| `NotifyStart` / `NotifyStop` | `msg/notifystart` / `msg/notifystop` | Best effort. |

Not ported yet: CDN media upload/download (AES-128-ECB), so inbound images / files / videos are only
named, not read, and replies are text only.

## Things that are easy to get wrong

- **`message_id` / `msg_id` / `svr_id` are uint64 numbers on the wire.** Decoding them into `float64`
  (or JavaScript numbers) silently loses precision above 2^53. They are read into `ID` (decimal text).
- **`context_token` must be echoed in every reply.** It is issued per inbound message. The reference
  client still sends without it, with a warning; whether the server accepts that is not documented.
- **`ret` or `errcode` equal to `-14` means the bot token is dead** (`IsStaleToken`): the user unbound
  the bot or logged in elsewhere. Retrying does not help; the user must scan again. The reference client
  pauses the account for an hour.
- **The login status `scaned` is spelled that way by the server.** `scaned_but_redirect` means: keep
  polling on `https://<redirect_host>`. `binded_redirect` means the bot is already connected and no new
  token is issued.
- **The API base URL after login can differ from the login base URL.** Use the `baseurl` returned with
  `confirmed` for every bot call.
- **Only one consumer per bot.** Two long pollers on the same bot race on the cursor and each sees part
  of the messages. `server/weixin` runs the poller under a Redis lease (`channel.RunWithLease`).
