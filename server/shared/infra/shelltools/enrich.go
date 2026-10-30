// Credential / session env injection decorator: wraps any Execer, merges env vars into each
// execution, and on the way out scrubs the secret values injected this time from the result.
//
// This layer does not care about the executor: the container path and the host-native path
// share the same SessionInjector (see injector.go). This file only keeps the thin Execer
// decorator shell for the container path.
package shelltools

import (
	"context"
	"log/slog"
)

// NewEnrichingExecer wraps an Execer that "fetches credentials fresh on each execution, merges
// env, and scrubs secrets on the way out".
//
//   - sessionID is the session id used to look up session env (the Ark session id);
//   - envs supplies session-level env (AMA CLI credentials + user-defined env + alias
//     declarations), may be nil;
//   - when log is nil, slog.Default() is used.
//
// If any source fails, it degrades to "do not inject that kind"; it never fails the whole command.
func NewEnrichingExecer(
	base Execer, sessionID string, envs SessionEnvSource, log *slog.Logger,
) Execer {
	return &enrichingExecer{
		base: base,
		inj:  NewSessionInjector(sessionID, envs, log),
	}
}

// enrichingExecer: see NewEnrichingExecer.
type enrichingExecer struct {
	base Execer
	inj  *SessionInjector
}

// MarkBackgroundStarted is passed through to the wrapped Execer (if it implements BackgroundMarker).
//
// It cannot be dropped: BashTool only talks to the Execer it holds. With a decorator in between,
// if we do not forward it, the "a background task was started" notice stops here, and idle
// pause will silently freeze the background process.
func (e *enrichingExecer) MarkBackgroundStarted() {
	if m, ok := e.base.(BackgroundMarker); ok {
		m.MarkBackgroundStarted()
	}
}

// Exec merges env, hands off to the wrapped Execer, and on the way out scrubs the secret values
// injected this time.
//
// ⚠️ **This is the only exit where credentials leave our process. Scrubbing here is enough,
// and it must be done here.** err does not need scrubbing: it is an error we create ourselves
// (failed to build the executor, timeout) and holds no executor output.
func (e *enrichingExecer) Exec(ctx context.Context, spec ExecSpec) (*Result, error) {
	merged, scrub := e.inj.prepareWithBase(ctx, spec.Envs)
	spec.Envs = merged
	res, err := e.base.Exec(ctx, spec)
	return scrub.result(res), err
}
