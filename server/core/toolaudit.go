package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

const (
	// DefaultToolAuditListLimit is the event page size for reconciliation.
	DefaultToolAuditListLimit = 1000
	// DefaultToolAuditMaxPages prevents a stuck server cursor from holding a worker slot forever.
	DefaultToolAuditMaxPages = 1000
	// DefaultToolAuditTimeout bounds listing and posting during process shutdown.
	DefaultToolAuditTimeout = 30 * time.Second
)

// AuditToolCalls posts an explicit not-executed result for each tool call that has no
// result. It returns the number successfully posted. A failed read or send never
// masquerades as complete reconciliation, and one failed call does not block the others.
func AuditToolCalls(ctx context.Context, api selfhosted.API, sessionID string, logger *slog.Logger) int {
	return auditToolCalls(ctx, api, sessionID, logger)
}

// ListOrphanToolCalls returns unanswered tool calls in event order, plus started and
// answered counts for diagnostics. Duplicate tool calls count once; result ids do not
// fall back to their unrelated event ids. Ark server-side tool results count as answered.
func ListOrphanToolCalls(
	ctx context.Context, api selfhosted.API, sessionID string,
) (orphans []selfhosted.Event, used, answered int, err error) {
	return listOrphanToolCalls(ctx, api, sessionID)
}

// OrphanToolResult creates the explicit error result for an unanswered tool call.
// It preserves the normal/custom result type and the same call-id fallback as the audit.
func OrphanToolResult(event selfhosted.Event) selfhosted.Event { return orphanToolResult(event) }

// ToolCallID returns the identifier used to pair a tool call with its result.
func ToolCallID(event selfhosted.Event) string { return toolCallID(event) }

// ToolResultCallID returns the call identifier carried by a result, without using
// the result's unrelated event id as a fallback.
func ToolResultCallID(event selfhosted.Event) string { return toolResultCallID(event) }
