package core

import "log/slog"

// SessionToolProvider builds the tool set for one Ark session.
type SessionToolProvider = sessionToolProvider

// SessionToolRouter chooses the container or host executor from the run's host_id
// snapshot. A host-selected session without a host provider is an explicit error,
// never a silent fallback to a different filesystem and credential environment.
type SessionToolRouter = toolRouter

// NewSessionToolRouter constructs an executor router with downstream error values.
func NewSessionToolRouter(
	store HostToolStore, container, host SessionToolProvider,
	containerErr, hostErr error, logger *slog.Logger,
) *SessionToolRouter {
	return newToolRouterWithErrors(store, container, host, containerErr, hostErr, logger)
}

func newToolRouterWithErrors(
	store HostToolStore, container, host sessionToolProvider,
	containerErr, hostErr error, logger *slog.Logger,
) *toolRouter {
	if logger == nil {
		logger = slog.Default()
	}
	return &toolRouter{store: store, container: container, host: host, log: logger,
		containerErr: containerErr, hostErr: hostErr}
}
