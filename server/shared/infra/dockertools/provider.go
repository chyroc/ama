// Package dockertools runs the six tools for each Ark session inside a Docker container.
//
// The SDK's EnvironmentWorker asks for a tool set for each work item through ToolsFunc, and this
// package implements that callback: find (or create) the container for the Ark session → build the
// six tools with shelltools, all going through `docker exec` into the same container → pause or
// stop the container once a turn goes idle.
//
// # One container per Ark session; the workspace is a named volume
//
//	container  <prefix>-sess-<sessionID>   lazy create; pause/stop when idle; rebuilt if removed
//	workspace  <prefix>-ws-<sessionID>     mounted on the container workdir; survives rebuilds
//
// The workspace lives in its own volume because the container is something that "can be thrown away
// at any time" (daemon restart, removed by the reaper, someone runs docker rm by hand), while the
// repos the agent cloned and the dependencies it installed should not be lost with it. Losing the
// container only means the next tool call spends a second or two rebuilding, and the files are all
// still there; only when the volume is gone too is it "workspace reset". The logs report the two
// cases separately.
//
// # Multiple instances
//
// Several amad instances may connect to the same Docker daemon, or each to its own. This package
// **does not keep "which session uses which container" in process memory**: the name is derived
// from the session id, so any instance can find it by name directly.
//
//   - Containers are created with a deterministic name, and the daemon guarantees only one with a
//     given name can be created: the instance that loses the race gets 409 and switches to inspect
//     to reuse the one the other instance built. No check-then-create (TOCTOU) double creation;
//   - State is always looked up fresh (inspect), never from an in-process "I remember it was
//     paused": pause and resume may happen on different instances;
//   - "When was this session last active" and "is a terminal open" are stored in PG (through
//     SessionLedger / TerminalActivity, implemented by core), and the reaper decides based on them;
//   - The reaper first takes a Redis lease (per daemon id), and does not run if it cannot get one.
//
// ⚠️ **Instances on different daemons do not share containers.** When a session's work lands on an
// instance connected to another daemon, that instance creates a new container with a volume on its
// own daemon, so the workspace looks reset. For the workspace to work across instances, all
// instances must connect to the same daemon. See README.
package dockertools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/tools/agenttoolset"
	"github.com/volcengine/ark-runtime-go/arkruntime/toolset"

	"github.com/chyroc/ama/server/shared/infra/docker"
	"github.com/chyroc/ama/server/shared/infra/shelltools"
)

// Engine is the Docker Engine API this package uses; *docker.Client satisfies it. It is narrowed to
// an interface so it can be tested offline.
type Engine interface {
	Ping(ctx context.Context) error
	Info(ctx context.Context) (*docker.SystemInfo, error)
	ImageInspect(ctx context.Context, ref string) (*docker.ImageInfo, error)
	ImagePull(ctx context.Context, ref string) error
	ContainerCreate(ctx context.Context, name string, cfg docker.ContainerConfig) (string, error)
	ContainerStart(ctx context.Context, id string) error
	ContainerStop(ctx context.Context, id string, timeout time.Duration) error
	ContainerPause(ctx context.Context, id string) error
	ContainerUnpause(ctx context.Context, id string) error
	ContainerInspect(ctx context.Context, id string) (*docker.ContainerInfo, error)
	ContainerRemove(ctx context.Context, id string, force bool) error
	ContainerList(ctx context.Context, labels []string) ([]docker.ContainerSummary, error)
	VolumeCreate(ctx context.Context, name string, labels map[string]string) (*docker.VolumeInfo, error)
	VolumeInspect(ctx context.Context, name string) (*docker.VolumeInfo, error)
	VolumeRemove(ctx context.Context, name string) error
	VolumeList(ctx context.Context, labels []string) ([]docker.VolumeInfo, error)
	Run(ctx context.Context, container string, opt docker.RunOptions) (*docker.RunResult, error)
	ExecCreate(ctx context.Context, container string, cfg docker.ExecConfig) (string, error)
	ExecStartAttached(ctx context.Context, id string, tty bool) (*docker.HijackedConn, error)
	ExecResize(ctx context.Context, id string, rows, cols int) error
}

var _ Engine = (*docker.Client)(nil)

// What to do with the container when idle.
const (
	// IdlePause freezes the container (cgroup freezer): processes and memory are kept as is, resume
	// takes milliseconds. The cost is that the memory stays in use. This is the default.
	IdlePause = "pause"
	// IdleStop stops the container: memory is freed, all processes end, the workspace (volume) is
	// kept. Resuming has to start
	// the container again (a second or two), and background processes that were running do not come
	// back.
	IdleStop = "stop"
	// IdleNone does nothing; the container keeps running until the reaper removes it.
	IdleNone = "none"
)

// Config is the Docker executor configuration. Zero-value fields take defaults (see withDefaults).
type Config struct {
	// Image is the session container image. It needs bash, coreutils (including timeout),
	// findutils, grep.
	Image string
	// Workdir is the working directory in the container; the workspace volume is mounted here, and
	// skills are installed under its skills/.
	Workdir string
	// Prefix is the prefix for container names, volume names and labels, used to keep what this
	// service creates apart from others'.
	// **When two AMA deployments run on one daemon, each must set its own**, otherwise the reaper
	// will remove the other one's containers.
	Prefix string
	// CPUs is the CPU quota per container (cores, may be fractional); <=0 means no limit.
	CPUs float64
	// MemoryBytes is the memory limit per container; <=0 means no limit.
	MemoryBytes int64
	// PidsLimit is the process count limit per container; <=0 means no limit.
	PidsLimit int64
	// Network is the container network mode (bridge / none / custom network name); empty uses the
	// daemon default.
	Network string
	// IdleAction is what to do with the container when a turn goes idle: pause / stop / none.
	IdleAction string
	// ContainerTTL is how long without activity before the reaper removes the container (the volume
	// stays); <=0 never removes.
	ContainerTTL time.Duration
	// WorkspaceTTL is how long without activity before the workspace volume is removed (the real
	// "workspace reset"); <=0 never removes.
	WorkspaceTTL time.Duration
	// ReapInterval is the reaper interval.
	ReapInterval time.Duration
}

// Defaults.
const (
	DefaultImage        = "ama-sandbox:latest"
	DefaultWorkdir      = "/workspace"
	DefaultPrefix       = "ama"
	DefaultContainerTTL = 24 * time.Hour
	DefaultWorkspaceTTL = 7 * 24 * time.Hour
	DefaultReapInterval = 10 * time.Minute
)

// WithDefaults returns the config with defaults filled in.
func (c Config) WithDefaults() Config {
	if strings.TrimSpace(c.Image) == "" {
		c.Image = DefaultImage
	}
	if strings.TrimSpace(c.Workdir) == "" {
		c.Workdir = DefaultWorkdir
	}
	c.Workdir = filepath.Clean(c.Workdir)
	if strings.TrimSpace(c.Prefix) == "" {
		c.Prefix = DefaultPrefix
	}
	switch strings.TrimSpace(c.IdleAction) {
	case IdlePause, IdleStop, IdleNone:
	default:
		c.IdleAction = IdlePause
	}
	if c.ReapInterval <= 0 {
		c.ReapInterval = DefaultReapInterval
	}
	return c
}

// ---------------------------------------------------------------------------
// Dependencies (implemented by core, wired in cmd/amad)
// ---------------------------------------------------------------------------

// SessionRecorder records "which daemon this Ark session's container was created on, and its name".
//
// Requests from the frontend to view the container land on any instance, and that instance needs
// this record to know "this session had a container" (it separates none from gone among the four
// states). Failures do not block tool calls; the implementation logs them itself.
type SessionRecorder interface {
	RecordSession(ctx context.Context, arkSessionID, containerName, daemonID string)
}

// SessionLookup looks up "did this Ark session have a container before". It is only used for log
// wording: when neither the container nor the volume exists, it tells "first creation" apart from
// "workspace was reset". May be nil.
type SessionLookup interface {
	LastSession(ctx context.Context, arkSessionID string) (string, error)
}

// ActivityLedger records and queries "when the session was last active". It is stored in PG and
// visible across instances.
//
// The reaper relies on it to judge how long a container has been idle. **Container labels cannot be
// changed**, and what is kept in process memory is only visible to this instance, so it has to go
// into the database.
type ActivityLedger interface {
	// TouchSession sets this session's last activity time to now. Failures are only logged.
	TouchSession(ctx context.Context, arkSessionID string)
	// SessionActivity fetches last activity times in bulk; sessions not in the database are absent
	// from the result.
	SessionActivity(ctx context.Context, arkSessionIDs []string) (map[string]time.Time, error)
}

// TerminalActivity answers "which containers still have a web terminal open" (returns container
// names).
//
// Pausing and the terminal are almost never in the same process (terminal requests land on any
// instance through the load balancer), so this must be a cross-instance query (PG), not an
// in-process flag. On error it is treated as "not in use".
type TerminalActivity interface {
	ActiveTerminalSessions(ctx context.Context) ([]string, error)
}

// Leases is the leader-election lock for the reaper (Redis lease).
type Leases interface {
	Acquire(ctx context.Context, key string, ttl time.Duration) (Lease, bool, error)
}

// Lease is a lock currently held.
type Lease interface {
	Release(ctx context.Context) error
}

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

// Provider manages Docker containers per Ark session. It is safe for concurrent use.
type Provider struct {
	engine Engine
	cfg    Config
	log    *slog.Logger

	mu       sync.Mutex
	daemonID string
	skills   shelltools.SkillLoader
	envs     shelltools.SessionEnvSource
	recorder SessionRecorder
	lookup   SessionLookup
	activity ActivityLedger
	terms    TerminalActivity

	// pullMu makes "image missing, pull it now" happen only once per process; others wait for it.
	pullMu sync.Mutex

	// now is the clock, injected by tests.
	now func() time.Time
}

// New creates a Provider. It does not touch the network; call Init to connect to the daemon.
func New(engine Engine, cfg Config, logger *slog.Logger) *Provider {
	if logger == nil {
		logger = slog.Default()
	}
	return &Provider{engine: engine, cfg: cfg.WithDefaults(), log: logger, now: time.Now}
}

// Init connects to the daemon and records the daemon id. It returns an error if it cannot connect;
// the caller uses that to decide whether to start the worker.
func (p *Provider) Init(ctx context.Context) error {
	if err := p.engine.Ping(ctx); err != nil {
		return fmt.Errorf("连不上 Docker daemon: %w", err)
	}
	info, err := p.engine.Info(ctx)
	if err != nil {
		return fmt.Errorf("读 Docker daemon 信息失败: %w", err)
	}
	p.mu.Lock()
	p.daemonID = info.ID
	p.mu.Unlock()
	p.log.Info("Docker 执行端已连上 daemon",
		slog.String("daemon_id", info.ID),
		slog.String("daemon_name", info.Name),
		slog.String("server_version", info.ServerVersion),
		slog.String("image", p.cfg.Image),
		slog.String("workdir", p.cfg.Workdir),
		slog.String("idle_action", p.cfg.IdleAction))
	return nil
}

// PrepareImage makes sure the image is local, pulling it if not. It blocks until the pull finishes
// or fails.
//
// It is called once in the background at startup so the first session does not have to wait for a
// pull. Failure is not fatal: creating the container tries again, and that error includes a hint on
// "how to build this image".
func (p *Provider) PrepareImage(ctx context.Context) error {
	return p.ensureImage(ctx)
}

func (p *Provider) ensureImage(ctx context.Context) error {
	p.pullMu.Lock()
	defer p.pullMu.Unlock()
	if _, err := p.engine.ImageInspect(ctx, p.cfg.Image); err == nil {
		return nil
	} else if !docker.IsNotFound(err) {
		return err
	}
	start := p.now()
	p.log.Info("会话容器镜像不在本地，开始拉取", slog.String("image", p.cfg.Image))
	if err := p.engine.ImagePull(ctx, p.cfg.Image); err != nil {
		return fmt.Errorf("镜像 %s 不在本地且拉取失败（自建镜像请先 `docker build -t %s sandbox-image/`，"+
			"或把 docker.image 配成一个能拉到的镜像）: %w", p.cfg.Image, p.cfg.Image, err)
	}
	p.log.Info("会话容器镜像已拉取", slog.String("image", p.cfg.Image),
		slog.Duration("took", p.now().Sub(start).Round(time.Millisecond)))
	return nil
}

// DaemonID returns the id of the connected daemon (empty string before Init).
func (p *Provider) DaemonID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.daemonID
}

// Workdir returns the working directory in the container.
func (p *Provider) Workdir() string { return p.cfg.Workdir }

// SkillsRootDir returns the root directory of skill files in the container (core writes it into the
// prompt).
func (p *Provider) SkillsRootDir() string { return shelltools.SkillsRootFor(p.cfg.Workdir) }

// SetSkillLoader wires up "which skills this session should install". When nil, no skill files are
// injected.
func (p *Provider) SetSkillLoader(l shelltools.SkillLoader) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.skills = l
}

// SetSessionEnvSource wires up session-level env (AMA CLI credentials + user-defined env).
//
// ⚠️ Forgetting to wire it fails with zero errors: the container is created as usual, but ama
// inside the container says it has no credentials and the user's env does not take effect.
func (p *Provider) SetSessionEnvSource(src shelltools.SessionEnvSource) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.envs = src
}

// SetSessionLedger wires up the session ledger (record containers, look up history, record
// activity). All three may be nil.
func (p *Provider) SetSessionLedger(r SessionRecorder, l SessionLookup, a ActivityLedger) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recorder, p.lookup, p.activity = r, l, a
}

// SetTerminalActivity wires up the "do not pause while a web terminal is open" check. When nil, it
// is not asked.
func (p *Provider) SetTerminalActivity(t TerminalActivity) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.terms = t
}

// ---------------------------------------------------------------------------
// Naming
// ---------------------------------------------------------------------------

// docker names only allow [a-zA-Z0-9][a-zA-Z0-9_.-]*.
var safeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,100}$`)

// nameKey turns a session id into a piece usable in a name. Ark ids (sesn-xxx) are usable as is;
// with other characters or when too long it falls back to a sha256 prefix, so it stays
// deterministic.
func nameKey(arkSessionID string) string {
	if safeName.MatchString(arkSessionID) {
		return arkSessionID
	}
	sum := sha256.Sum256([]byte(arkSessionID))
	return "h" + hex.EncodeToString(sum[:])[:32]
}

// ContainerName returns the name of the session container.
func (p *Provider) ContainerName(arkSessionID string) string {
	return p.cfg.Prefix + "-sess-" + nameKey(arkSessionID)
}

// VolumeName returns the name of the session workspace volume.
func (p *Provider) VolumeName(arkSessionID string) string {
	return p.cfg.Prefix + "-ws-" + nameKey(arkSessionID)
}

// Label keys.
func (p *Provider) labelManaged() string { return p.cfg.Prefix + ".managed" }
func (p *Provider) labelSession() string { return p.cfg.Prefix + ".ark-session" }
func (p *Provider) labelKind() string    { return p.cfg.Prefix + ".kind" }

func (p *Provider) labels(arkSessionID, kind string) map[string]string {
	return map[string]string{
		p.labelManaged(): "true",
		p.labelSession(): arkSessionID,
		p.labelKind():    kind,
	}
}

// ---------------------------------------------------------------------------
// Tool set
// ---------------------------------------------------------------------------

// ToolsFuncForSession returns a tool set factory that can be passed to the SDK, with the session id
// given explicitly by the caller (the worker polls by itself and the WorkItem has the id, so there
// is no need to derive it from workdir).
func (p *Provider) ToolsFuncForSession(
	ctx context.Context, arkSessionID string,
) func(*agenttoolset.AgentToolContext) (*toolset.Set, error) {
	return func(env *agenttoolset.AgentToolContext) (*toolset.Set, error) {
		id := strings.TrimSpace(arkSessionID)
		if id == "" {
			return nil, errors.New("dockertools: 没有方舟 session id，无法确定用哪个容器")
		}
		local := ""
		if env != nil {
			local = env.Workdir
		}
		return p.ToolsFor(ctx, id, local)
	}
}

// ToolsFor prepares a tool set for an Ark session. **It does not create the container**: the
// container is created/resumed on the first real tool call (turns with only a text reply never
// touch the container).
//
// localWorkdir is only used to pass the SDK's parameter check when building the Set; it does not
// affect any tool's behavior.
func (p *Provider) ToolsFor(_ context.Context, arkSessionID, localWorkdir string) (*toolset.Set, error) {
	s := p.session(arkSessionID)
	log := p.log.With(slog.String("ark_session", arkSessionID))
	p.mu.Lock()
	envs := p.envs
	p.mu.Unlock()
	ex := shelltools.NewEnrichingExecer(s, arkSessionID, envs, log)
	onIdle := func() error {
		// The work's ctx is often already canceled at this point (Set.Close is a defer), so
		// start a new one.
		ctx, cancel := context.WithTimeout(context.Background(), idleTimeout)
		defer cancel()
		s.idle(ctx)
		return nil
	}
	return shelltools.NewSet(ex, p.cfg.Workdir, localWorkdir, onIdle, log)
}

// idleTimeout is the total timeout for idle cleanup (probe background jobs + pause/stop). stop has
// to give processes 10 seconds to exit cleanly.
const idleTimeout = 30 * time.Second

// session builds a session handle. Handles are light: one per work item, and they are not put in
// any in-process table.
func (p *Provider) session(arkSessionID string) *session {
	return &session{
		p:            p,
		arkSessionID: arkSessionID,
		name:         p.ContainerName(arkSessionID),
		volume:       p.VolumeName(arkSessionID),
		log:          p.log.With(slog.String("ark_session", arkSessionID)),
	}
}
