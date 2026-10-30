// hosttool: the tool set Provider for the host executor. Its shape mirrors
// dockertools.Provider, but the executor is not a Docker container; it is the ama daemon on
// the user's machine, which picks up KindTool tasks through host.Service.
//
// Differences from dockertools:
//
//   - No container lifecycle (no create/pause/resume): the host is always on, and the
//     workspace is a persistent directory on the machine, ~/.ama/runs/<sessionID>;
//   - Instead of building shell scripts, Ark's tool_use input is packed into a
//     hosttools.ToolCall, queued into host_dispatches through host.Service.ToolCall, and
//     the call blocks until the daemon executes it natively;
//   - env injection and secret masking on output still go through
//     shelltools.SessionInjector (the same piece as the container path);
//   - Skill injection is driven by hosttools.InjectSkills (two native internal operations,
//     write_files/fetch_archive) instead of sending base64 shell scripts, so a Mac does
//     not need GNU tools from brew.
package core

import (
	"context"
	"log/slog"
	"sync"

	"github.com/volcengine/ark-runtime-go/arkruntime/tools/agenttoolset"
	"github.com/volcengine/ark-runtime-go/arkruntime/toolset"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/hosttools"
	"github.com/chyroc/ama/server/shared/infra/shelltools"
)

// HostExecutor is everything tool execution needs from the host domain (host.Service
// satisfies it). It is declared as a narrow interface in core so core does not depend back
// on the service/host package.
type HostExecutor interface {
	ToolCall(ctx context.Context, hostID, sessionID string, call hosttools.ToolCall) (hosttools.ToolResult, error)
}

// HostToolStore is the read-only access needed to look up the run (to get the host_id snapshot) before routing.
type HostToolStore interface {
	GetRun(ctx context.Context, id string) (apimodel.Run, error)
}

// hostWorkdir is the working directory of each Ark session on the host (relative to the
// daemon user's $HOME). It matches the container's /workspace: skills go under its skills/,
// and the agent's relative paths are based on it.
const hostWorkdir = ".ama/runs"

// HostSkillBundle is an alias of hosttools.SkillBundle: the wiring side uses the core name
// when assembling skills, while the actual shape is defined by hosttools.
type HostSkillBundle = hosttools.SkillBundle

// HostSkillLoader gets which skills an Ark session should install (for native injection on the host).
type HostSkillLoader interface {
	HostSkillsFor(ctx context.Context, arkSessionID string) ([]HostSkillBundle, error)
}

// HostToolProvider produces, per session, the six-tool set that runs on the host.
type HostToolProvider struct {
	exec   HostExecutor
	store  HostToolStore
	skills HostSkillLoader
	envs   shelltools.SessionEnvSource
	log    *slog.Logger

	mu       sync.Mutex
	injected map[string]bool // arkSessionID → skills injected (in-memory flag; if lost, replay is idempotent)
}

// maxInjectedSessions caps the in-memory "skills injected" flags so they do not grow without
// bound when the amad process runs for a long time without restart. Past the cap the whole
// map is cleared (see the comment at the injection site).
const maxInjectedSessions = 4096

// NewHostToolProvider wires it up. envs may be nil (no session env injected).
func NewHostToolProvider(
	exec HostExecutor, store HostToolStore, skills HostSkillLoader,
	envs shelltools.SessionEnvSource, log *slog.Logger,
) *HostToolProvider {
	if log == nil {
		log = slog.Default()
	}
	return &HostToolProvider{
		exec:     exec,
		store:    store,
		skills:   skills,
		envs:     envs,
		log:      log,
		injected: make(map[string]bool),
	}
}

// ToolsFuncForSession has the same shape as dockertools.Provider, for toolRouter to call.
func (p *HostToolProvider) ToolsFuncForSession(
	ctx context.Context, arkSessionID string,
) func(*agenttoolset.AgentToolContext) (*toolset.Set, error) {
	return func(local *agenttoolset.AgentToolContext) (*toolset.Set, error) {
		return p.toolsFor(ctx, arkSessionID, local)
	}
}

func (p *HostToolProvider) toolsFor(
	ctx context.Context, arkSessionID string, local *agenttoolset.AgentToolContext,
) (*toolset.Set, error) {
	// Which machine this session uses: the run snapshots host_id from the agent when it is created (chat.go).
	run, err := p.store.GetRun(ctx, arkSessionID)
	if err != nil || run.HostID == "" {
		// Getting here means toolRouter routed wrongly (the agent is host but the run has no
		// snapshot). Do not silently execute on the amad machine; fail clearly so the symptom
		// points at the snapshot path.
		return nil, &hostRouteError{sessionID: arkSessionID, cause: err}
	}

	// The working directory on the host is isolated per session: ~/.ama/runs/<sessionID>.
	// The daemon resolves ToolCall's relative paths against $HOME and runs mkdir -p.
	sessionWorkdir := hostWorkdir + "/" + arkSessionID

	doer := &hostNativeDoer{
		exec:      p.exec,
		hostID:    run.HostID,
		sessionID: arkSessionID,
	}
	injector := shelltools.NewSessionInjector(arkSessionID, p.envs,
		p.log.With(slog.String("ark_session", arkSessionID), slog.String("host", run.HostID)))

	// First-time skill injection. The in-memory flag only saves repeated writes within this
	// instance and this round; replaying after failover/reclaim to another instance is safe
	// (it overwrites files of the same name, and cache hits skip re-downloading).
	if p.skills != nil {
		p.mu.Lock()
		done := p.injected[arkSessionID]
		p.mu.Unlock()
		if !done {
			if bundles, err := p.skills.HostSkillsFor(ctx, arkSessionID); err != nil {
				// Same policy as the container path: skill failures are warn-only and do not block the whole work.
				p.log.Warn("宿主机执行端取技能失败，本轮不注入技能",
					slog.String("ark_session", arkSessionID), slog.Any("err", err))
			} else if err := hosttools.InjectSkills(ctx, doer, sessionWorkdir, "",
				bundles, p.log.With(slog.String("ark_session", arkSessionID))); err != nil {
				p.log.Warn("宿主机执行端注入技能失败",
					slog.String("ark_session", arkSessionID), slog.Any("err", err))
			} else {
				p.mu.Lock()
				p.injected[arkSessionID] = true
				// Bounded: when the amod process runs long without restart, one bool per past
				// session would slowly grow. Past the cap the whole map is cleared. Skill
				// injection is idempotent (overwrites files of the same name); the worst case is
				// that a still-running session injects once more on its next tool call, which is
				// acceptable.
				if len(p.injected) > maxInjectedSessions {
					p.injected = make(map[string]bool)
					p.injected[arkSessionID] = true
				}
				p.mu.Unlock()
			}
		}
	}

	// local.Workdir only satisfies the parameter of the SDK's NewDefault (it is closed right
	// after creation); the real path base is sessionWorkdir. The host has no idle pause, so onIdle=nil.
	localWorkdir := ""
	if local != nil {
		localWorkdir = local.Workdir
	}
	return shelltools.NewNativeSet(doer, injector, sessionWorkdir, localWorkdir,
		p.log.With(slog.String("ark_session", arkSessionID), slog.String("host", run.HostID)))
}

// hostNativeDoer queues a hosttools.ToolCall and waits for the result synchronously. It
// satisfies both shelltools.NativeDoer (six tools) and hosttools.Doer (skill injection).
type hostNativeDoer struct {
	exec      HostExecutor
	hostID    string
	sessionID string
}

func (d *hostNativeDoer) DoNative(
	ctx context.Context, call hosttools.ToolCall,
) (hosttools.ToolResult, error) {
	return d.exec.ToolCall(ctx, d.hostID, d.sessionID, call)
}

// Do is hosttools.Doer, the same thing as DoNative (skill injection internal operations use the same queue).
func (d *hostNativeDoer) Do(
	ctx context.Context, call hosttools.ToolCall,
) (hosttools.ToolResult, error) {
	return d.DoNative(ctx, call)
}

var (
	_ hosttools.Doer        = (*hostNativeDoer)(nil)
	_ shelltools.NativeDoer = (*hostNativeDoer)(nil)
)

// hostRouteError gives a diagnosable error when "the agent chose the host but the run has no host_id snapshot".
type hostRouteError struct {
	sessionID string
	cause     error
}

func (e *hostRouteError) Error() string {
	if e.cause != nil {
		return "宿主机执行端找不到 run " + e.sessionID + ": " + e.cause.Error()
	}
	return "run " + e.sessionID + " 没有宿主机快照（host_id 为空），无法在宿主机上执行"
}
