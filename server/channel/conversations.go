package channel

import (
	"context"
	"encoding/json"
)

// Conversations is what a channel needs from core.
//
// It is an interface so channels do not import core (they are coupled only by ID strings);
// cmd/amad adapts *core.Service to it. Channel packages extend it with their own extra needs.
type Conversations interface {
	// CreateChannelConversation creates a new Ark session for an agent, marked as started from the given
	// channel (the run's trigger) with a human-readable source description for the run history.
	CreateChannelConversation(ctx context.Context, workspaceID, agentID string, kind Kind, detail string) (Run, error)
	// AppendRunEvents sends events into the session (channels only send user.message).
	AppendRunEvents(ctx context.Context, runID string, raw []json.RawMessage) ([]json.RawMessage, error)
	// StreamRunEvents subscribes to the session's events and blocks until the stream ends or ctx is done.
	StreamRunEvents(ctx context.Context, runID string, onFrame func(raw []byte) error) error
	// AgentDisabled reports whether the agent was disabled. It must be asked before running so the
	// channel can answer with a sentence instead of going silent. Lookup failures return false.
	AgentDisabled(ctx context.Context, agentID string) bool
}

// Run is the part of apimodel.Run a channel uses: the Ark session id.
type Run struct {
	ID string
}
