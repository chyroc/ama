package core

import (
	"log/slog"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

// WorkSendGuard is the selfhosted API wrapper that stops a work item after its
// session permanently rejects events. It also suppresses subsequent reads, which
// would otherwise keep spending the account's request quota while the work exits.
type WorkSendGuard = workSendGuard

// NewWorkSendGuard wraps api with the permanent-send-failure gate.
// A nil api is returned unchanged. A nil cancel suppresses events without stopping a work item.
func NewWorkSendGuard(
	api selfhosted.API, sessionID string, cancel func(), logger *slog.Logger,
) selfhosted.API {
	return newWorkSendGuard(api, sessionID, cancel, logger)
}
