// toolRouter: this is where we choose whether an Ark session's tools run in a Docker container or on
// the host.
//
// # Why the split happens here
//
// Ark self_hosted environments are reused per workspace: work from container agents and host agents
// goes into **the same Ark queue of the same environment** and is picked up by the pollLoop of the
// same WorkerPool (see environment discovery in worker.go). poll/ack/heartbeat/reclaim/the tool
// result ledger are all independent of the executor; the only executor-specific part is "building
// the tool set for this session". handleWorkItem calls back ToolsFuncForSession before driving
// SessionToolRunner, so the split goes there.
//
// The check is the host_id snapshot on the run (copied from the agent when
// core.createConversation creates the run): non-empty -> host; empty -> Docker container (default).
// A failed run lookup also goes to the container, consistent with the fault-tolerance stance of
// skillinject.go: a problem in the routing component should not halt every conversation, and the
// container path is the default path.
package core

import (
	"context"
	"log/slog"

	"github.com/volcengine/ark-runtime-go/arkruntime/tools/agenttoolset"
	"github.com/volcengine/ark-runtime-go/arkruntime/toolset"
)

// sessionToolProvider is the minimal interface for "build a tool set per session".
// Both dockertools.Provider and HostToolProvider satisfy it.
type sessionToolProvider interface {
	ToolsFuncForSession(ctx context.Context, arkSessionID string,
	) func(*agenttoolset.AgentToolContext) (*toolset.Set, error)
}

// toolRouter implements sessionToolProvider and picks the executor by run.host_id internally.
type toolRouter struct {
	store     HostToolStore
	container sessionToolProvider
	// host takes an interface rather than *HostToolProvider: production passes the concrete type,
	// and tests can plug in a stub.
	host sessionToolProvider
	log  *slog.Logger
}

func newToolRouter(store HostToolStore, container sessionToolProvider, host sessionToolProvider, log *slog.Logger) *toolRouter {
	if log == nil {
		log = slog.Default()
	}
	return &toolRouter{store: store, container: container, host: host, log: log}
}

func (r *toolRouter) ToolsFuncForSession(
	ctx context.Context, arkSessionID string,
) func(*agenttoolset.AgentToolContext) (*toolset.Set, error) {
	useHost := false
	// Always look up the run's executor snapshot first. **Do not put GetRun inside the
	// `r.host != nil` condition**: if the lookup were skipped when no host executor is wired, sessions
	// with a non-empty host_id would silently go to the container (credentials/working directory all
	// wrong). When host_id is non-empty but no provider is wired, a clear error is returned below.
	if r.store != nil {
		run, err := r.store.GetRun(ctx, arkSessionID)
		if err != nil {
			// A transient storage failure still falls back to the container (it should not stop all
			// existing container conversations), but leave a trace.
			r.log.Warn("查 run 失败，工具集按 Docker 容器分流",
				slog.String("ark_session", arkSessionID), slog.Any("err", err))
		} else if run.HostID != "" {
			useHost = true
		}
	}
	chosen := r.container
	if useHost {
		chosen = r.host
	}
	if chosen == nil {
		return func(*agenttoolset.AgentToolContext) (*toolset.Set, error) {
			if useHost {
				return nil, errHostProviderNotConfigured
			}
			return nil, errContainerProviderNotConfigured
		}
	}
	return chosen.ToolsFuncForSession(ctx, arkSessionID)
}

var _ sessionToolProvider = (*toolRouter)(nil)
