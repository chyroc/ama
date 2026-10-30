# channel — what every chat platform shares

A **channel** connects a chat app to an AMA agent: a message comes in from the platform, the bound agent
runs one turn on an Ark session, and the answer goes back to the same conversation. Two channels exist:

| Channel | Package | Receives by | Protocol |
| --- | --- | --- | --- |
| Feishu / Lark | [`server/feishu`](../feishu/) | WebSocket long connection | [`shared/infra/lark`](../shared/infra/lark/) |
| WeChat | [`server/weixin`](../weixin/) | HTTP long poll (`getupdates`) | [`shared/infra/weixin`](../shared/infra/weixin/) |

This package holds everything that is the same for both, so a platform package only keeps what is
specific to it (how messages arrive, how they become text, how the answer is shown).

```
cmd/amad → server/<platform> → server/channel → shared/gopkg/arkframe
                 ↓
      shared/infra/<platform>
```

`channel` does not import `core`: like the platform packages, it is coupled to core only through the
`Conversations` interface, which `cmd/amad` fills in (`channelConversations`).

## Pieces

| Piece | File | What it does |
| --- | --- | --- |
| `Kind` | `channel.go` | Channel names. The value is also the run trigger (`apimodel.RunTrigger`), so runs show where they came from. |
| `Conversations` | `conversations.go` | The core abilities a channel needs: `CreateChannelConversation(kind)`, send events, subscribe, "is this agent disabled". |
| `RunTurn` | `turn.go` | Subscribe first, then send, then wait for the final frame; returns the answer and how the turn ended. |
| `AnswerBuffer` | `answer.go` | Which agent text is the answer: the last segment after a tool call, falling back to the one before it. |
| `Store`, `EnsureSession` | `store.go`, `session.go` | Inbound dedup and "conversation scope → Ark session", with the database deciding races. |
| `Supervisor`, `Source` | `supervisor.go` | One receive loop per connected account; adds, removes and restarts (on `Version` change) as accounts change. |
| `RunWithLease` | `lease.go` | Runs a loop on exactly one instance via a Redis lease; hands over when the holder dies. |
| Fixed replies | `replies.go` | "No answer", "rate limited", "agent disabled": the same sentence on every platform. |

Frame parsing itself (`agent.message`, tool calls, final frames, interrupts) is in
[`shared/gopkg/arkframe`](../shared/gopkg/arkframe/), shared with `core`.

## Storage

New channels share two tables (all real columns): `channel_inbound_messages` (dedup, key
`(channel, account_id, message_id)`) and `channel_session_scopes` (scope → session, key
`(channel, account_id, scope_key, agent_id)`). PG implementation: `repository/dao/channeldao`.

Feishu predates them and still uses its own `feishu_inbound_messages` / `feishu_session_scopes`, which
also keep Feishu-only diagnosis columns (thread anchors). Moving Feishu onto the shared tables is a
possible later step; it needs a data migration, so it was not done together with the refactor.

## Adding a channel

1. Protocol client in `shared/infra/<platform>` (no storage, no business rules), with a README that
   credits any code it was ported from.
2. A `server/<platform>` package with:
   - a `channel.Source` (list accounts, run one account's receive loop under `channel.RunWithLease`
     when the platform allows only one consumer);
   - per message: `Store.ClaimInbound` → resolve the bound agent → `Conversations.AgentDisabled` →
     `channel.EnsureSession` → `channel.RunTurn` → send `res.Answer` or `channel.FallbackAnswer(res)`;
   - its own tables in `repository/schema` (add them to `channelOrder`) and Store implementations.
3. Add the `Kind` here, the trigger to `apimodel.RunTrigger` (+ `IsChannel`), the frontend
   `RunTrigger` union, `CONVERSATION_TRIGGERS` in `ChatPage.tsx`, and `enum.trigger.*` messages.
4. Routes in `authz/matrix.go` and `authz/capability.go`, then regenerate OpenAPI and the role table.

## Things that are easy to get wrong

- **Subscribe before sending.** core stores a session's events only while someone is subscribed.
  `RunTurn` does it in the right order; do not hand-roll it.
- **Ark does not close the stream after a turn.** The final frame must be recognized
  (`arkframe.TurnEnded`), and `requires_action` is *not* final.
- **An interrupted turn is not a failed turn.** `TurnResult.Interrupted` means a newer message took
  over: send nothing.
- **The agent id is part of the scope key**, so rebinding an account to another agent starts new
  conversations instead of handing over the old agent's history.
