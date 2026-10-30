package channel

import (
	"context"
	"errors"
	"time"
)

// Errors returned by Store implementations.
var (
	// ErrScopeNotBound means no session is bound to the scope yet.
	ErrScopeNotBound = errors.New("channel: 会话作用域还没有绑定会话")
	// ErrInvalidArgument means a required field is empty.
	ErrInvalidArgument = errors.New("channel: 参数不合法")
)

// InboundStatus is the processing state of an inbound message.
type InboundStatus string

const (
	// InboundPending is claimed and being handled. A redelivery is ignored.
	InboundPending InboundStatus = "pending"
	// InboundProcessed is done. A redelivery is ignored.
	InboundProcessed InboundStatus = "processed"
	// InboundFailed failed last time. A redelivery is let through to try again.
	InboundFailed InboundStatus = "failed"
)

// InboundMessage is one dedup record. (Channel, AccountID, MessageID) is the key.
type InboundMessage struct {
	Channel   Kind
	AccountID string
	MessageID string
	// ConversationID is the platform conversation the message came from, kept for diagnosis only.
	ConversationID string
	Status         InboundStatus
	CreatedAt      time.Time
}

// ScopeKey identifies one conversation scope of one account, for one agent.
//
// AgentID is part of the key on purpose: rebinding an account to another agent starts new conversations
// instead of handing the old agent's session history to the new one.
type ScopeKey struct {
	Channel   Kind
	AccountID string
	// Key is computed by the channel from the message ("user:<id>", "group:<id>" ...) and must be
	// deterministic, so every instance computes the same key for the same conversation.
	Key     string
	AgentID string
}

// Scope maps a conversation scope to the Ark session it reuses, so the agent remembers earlier turns.
type Scope struct {
	ScopeKey
	RunID       string
	WorkspaceID string
	CreatedAt   time.Time
	LastUsedAt  time.Time
	TurnCount   int
}

// Store is the storage a channel's inbound path needs. It must be shared by all instances (PostgreSQL in
// production): two instances can receive the same redelivered message, or two messages of the same
// conversation, at the same moment.
//
// Feishu still uses its own older tables for the same two jobs (feishu_inbound_messages and
// feishu_session_scopes); new channels use this one.
type Store interface {
	// ClaimInbound records a message as pending. true means it is new (or failed last time) and should be
	// handled; false means another delivery already has it.
	ClaimInbound(ctx context.Context, msg InboundMessage) (bool, error)
	// MarkInbound updates the status of a claimed message.
	MarkInbound(ctx context.Context, channel Kind, accountID, messageID string, status InboundStatus) error

	// GetScope returns the session bound to the scope, or ErrScopeNotBound.
	GetScope(ctx context.Context, key ScopeKey) (Scope, error)
	// BindScope binds a scope to a session unless one is already bound. ok=false means another instance
	// won the race; winner is the scope that is bound now, and the caller must use winner.RunID.
	BindScope(ctx context.Context, scope Scope) (winner Scope, ok bool, err error)
	// TouchScope records one more turn on the scope.
	TouchScope(ctx context.Context, key ScopeKey, now time.Time) error
	// ResetScope forgets the session of a scope for every agent, so the next message starts a new one.
	ResetScope(ctx context.Context, channel Kind, accountID, key string) error
}
