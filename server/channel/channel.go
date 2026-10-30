// Package channel is what every chat-platform integration (a "channel") shares.
//
// A channel connects a chat app to an AMA agent: a message comes in from the platform, the bound agent
// runs one turn on an Ark session, and the answer goes back to the same conversation. Feishu and
// WeChat (Weixin) are the two channels today; each lives in its own package (server/feishu,
// server/weixin) and only keeps what is truly specific to its platform.
//
// # What lives here
//
//	Conversations   the few core abilities a channel needs (create a session, send, subscribe)
//	RunTurn         run one turn on a session and collect the answer
//	AnswerBuffer    which part of the agent's text is the answer (not the narration before a tool call)
//	Supervisor      keep one receive loop per connected account, add/remove as accounts change
//	RunWithLease    run a loop on only one instance at a time (Redis lease), hand over when lost
//	Store           inbound dedup + "conversation → session" mapping, shared by new channels
//
// # Layering
//
//	cmd/amad → server/<platform> → channel → shared/gopkg/arkframe
//	                ↓
//	        shared/infra/<platform>   (the platform's wire protocol)
//
// channel does not import core: like the Feishu package before it, a channel is coupled to core only by
// ID strings, through the Conversations interface that cmd/amad fills in.
package channel

// Kind names a channel. The value doubles as the run trigger stored on runs started from it
// (apimodel.RunTrigger), so it must match the RunTrigger union in web/src/types/domain.ts.
type Kind string

const (
	// Feishu is Feishu / Lark (server/feishu).
	Feishu Kind = "feishu"
	// Weixin is WeChat through the iLink bot API (server/weixin).
	Weixin Kind = "weixin"
)

// Valid reports whether k is a known channel.
func (k Kind) Valid() bool {
	switch k {
	case Feishu, Weixin:
		return true
	}
	return false
}
