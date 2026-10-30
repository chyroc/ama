// self_hosted worker: lives in the amad process and sends tool execution to Docker containers (or
// to the user's own host).
//
// # Why it exists
//
// Conversations run in a self_hosted environment (see EnsureEnvironment in arkrun.go). In this kind
// of environment Ark **only does model inference**: it decides which tool to call, then puts the
// session into the work queue and waits for our worker to take it, run it, and hand the result back
// with a `user.tool_result` event.
//
//	Ark /work/poll → ack → heartbeat                       (done by SDK)
//	         ↓
//	  subscribe to session stream, wait for agent.tool_use   (done by SDK)
//	         ↓
//	  run that command in the session's Docker container     ← shared/infra/dockertools
//	         ↓
//	  POST user.tool_result back to Ark                     (done by SDK)
//	         ↓
//	  agent gets the result and keeps reasoning, until end_turn
//
// **With no worker running, the conversation hangs**: the agent says it will run a command, then
// the Session stays at `requires_action` and never moves. The user sees "the reply stopped
// halfway", with no error at all. So it is part of the chat feature, not an optional ops component.
// That is also why it lives in amad and not in a separate process (a separate process becomes a
// hidden dependency where "forgetting to start it breaks the whole feature").
//
// # One worker per environment
//
// The work queue is split by environment, and environments are created per workspace (agents in the
// same workspace share one). So we start one worker goroutine per "workspace + environment":
// workspace A's key can only poll workspace A's environment. Poll targets are found from the
// binding table; see refresh.
package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chyroc/ama/server/shared/gopkg/xlog"
	"github.com/chyroc/ama/server/shared/infra/docker"
	"github.com/chyroc/ama/server/shared/infra/dockertools"
	"github.com/volcengine/ark-runtime-go/arkruntime"
	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

// refreshInterval is the interval for "rescan which environments to poll".
//
// After a new workspace configures credentials, or the first conversation creates an environment,
// it takes this long before the worker starts polling it. 30 seconds is a compromise: shorter turns
// store queries into pointless polling load; longer makes the window of "first conversation hangs
// after configuring credentials" too visible.
const refreshInterval = 30 * time.Second

// workerRootDir is the root where the SDK puts local session working directories.
//
// Note it is not the same as the working directory **inside the container**: this one only
// satisfies the SDK's argument check when building the tool set; the cwd of bash in the container
// is decided by dockertools (docker.workdir, default /workspace).
const workerRootDir = "/tmp/ama-worker-sessions"

// WorkerConfig is the configuration of the self_hosted worker.
type WorkerConfig struct {
	// Docker is the configuration of the Docker executor. When Enabled is false it is not wired up
	// (only the host executor remains).
	Docker DockerExecConfig

	// Concurrency is the number of work items handled at once **per environment**; <=0 means 1
	// (serial).
	//
	// Passed in by cmd/amad from config.Sandbox.WorkerConcurrencyOrDefault(). For why it must be
	// greater than 1, and why concurrency cannot come from the SDK's Run, see the comment on
	// runOne.
	//
	// With leader election (only one pod polls an env), this value is in effect a "global per env"
	// limit. Each running work item may hold its own container, so it also indirectly limits the
	// number of containers running at once: set it lower on small machines.
	Concurrency int

	// ManagedKeys is the source of "AMA managed API keys per employee", used to inject AMA_TOKEN
	// into the container (so the `ama` command preinstalled in the container works). It can be nil:
	// then those three variables are not injected, and an agent with the built-in ama skill enabled
	// will fail when it runs `ama` in the container.
	//
	// It is implemented by auth.Service (using the deterministic derivation in auth/managedkey.go).
	ManagedKeys ManagedKeyIssuer

	// LarkUsers is the Feishu user authorization provider, used to inject the two variables that
	// lark-cli reads into the container (so the agent can read group chat and thread history as the
	// workspace owner's Feishu identity). It can be nil: then an agent with "Feishu user
	// authorization" enabled gets a clear error (instead of silently not injecting).
	//
	// ⚠️ It passes the provider itself, not a narrow interface: **fetching credentials and checking
	// card status must use the same object**, otherwise the row written back after renewal and the
	// row the card reads would take two different paths, giving "the card says authorized,
	// injection says not".
	LarkUsers *LarkUserProvider

	// PublicBaseURL is AMA's public address, injected as AMA_BASE_URL for the CLI in the container.
	//
	// ⚠️ **It must be set for local development.** When empty, the CLI falls back to its built-in
	// production address, so "it works fine locally" while it is changing **production** data.
	PublicBaseURL string

	// Leases make sure "only one pod polls an environment at a time", saving N-1 copies of idle
	// RPM.
	//
	// **It can be empty, and empty is safe**: then every pod polls, which is the behavior before
	// this version. It is fully correct (two pods taking the same work is settled by the AckWork
	// race); it just costs quota. So this layer is a pure degrade-open optimization; do not change
	// it to "do not start without it". The full argument and the hard rule "TTL must be < reclaim
	// threshold < Ark's 90-second stop line" are in the file header of core/workerlease.go.
	Leases Leases

	// ToolResults is the tool result ledger. It records how far each tool_use got (started/result/
	// sent), so that after a worker restart/reclaim, tools with side effects (bash etc.) are not
	// run again.
	//
	// **Strongly recommended (use the PG implementation in production), may be empty**: when empty,
	// the SDK holds no ledger, and every taker reruns unconfirmed tools. It still works, but
	// commands may run twice in the window where the process is killed.
	//
	// ⚠️ It must be an implementation that is **cross-instance and isolated per session** (PG), not
	// the worker's local disk: the SDK's default FileToolResultStore writes to a shared local
	// directory, which another pod cannot read after reclaim, and it flushes leftover results from
	// other sessions into the current session (in production this once caused a poison-pill mix-up
	// across sessions, with 400 InvalidPayload hanging things in a chain). See core/pgtoolresult.go
	// and repository/schema/selfhost_tool_results.sql.
	ToolResults ToolResultLedger

	// Wakeups "wake that env's poll loop at once when a new message arrives", to keep response
	// speed after polling has been slowed down.
	//
	// **It can be empty**: then only the step-backoff timer is used, and in the worst case it takes
	// one step interval (up to 60 seconds) to find new work. Correct, just slow, so this is also
	// degrade-open.
	//
	// ⚠️ **It must work across pods** (the implementation uses PG LISTEN/NOTIFY in
	// shared/infra/pgmq): with leader election there is only one poll loop per env globally, while
	// the request that sends a message lands randomly via the load balancer, **usually not on the
	// pod holding the lease**. An in-process signal does not work here; see core/workerwake.go.
	Wakeups WorkerWakeups

	// HostExec is the exec entry of the host executor (host.Service.Exec).
	//
	// **It can be empty**: when the host domain is not configured (as in tests), the host tool
	// Provider is not built. If an agent then picks the host executor, the tool set returns a clear
	// "host executor not configured" error, and does not silently run on the amad machine itself.
	//
	// Polling Ark does not depend on Docker config: work for host agents is also in Ark's
	// self_hosted queue, so as long as either Docker or HostExec is available, the worker pool
	// should start (see enabled).
	HostExec HostExecutor

	// ReaperLeases is the leader-election lock (Redis lease) for the Docker reaper sweep. It may be
	// empty: then every instance sweeps (deletion is idempotent, so the result is correct, just
	// repeated). See dockertools.RunReaper.
	ReaperLeases Leases
}

// DockerExecConfig holds the wiring parameters of the Docker executor.
type DockerExecConfig struct {
	// When Enabled is false the Docker executor is not wired up.
	Enabled bool
	// Host is the daemon address (unix:// / tcp://); when empty, DOCKER_HOST or the default socket
	// is used.
	Host string
	// Tools is the container spec and lifecycle configuration.
	Tools dockertools.Config
}

// enabled reports whether the configuration is enough to start the worker.
//
// Either of the two executors is enough:
//   - Docker: one container per session (default);
//   - HostExec: the host path (tools are sent to the ama daemon via long poll).
//
// With neither, the worker does not start: an agent would hang forever at requires_action as soon
// as it calls a tool, and starting a poller that can never do any work would only flood the logs.
func (c WorkerConfig) enabled() bool {
	return c.Docker.Enabled || c.HostExec != nil
}

// WorkerPool keeps a set of long-running workers per "workspace + environment".
type WorkerPool struct {
	svc *Service
	cfg WorkerConfig
	log *slog.Logger
	// tools is the Docker executor; **nil when it is not enabled or cannot reach the daemon**
	// (host-only deployments). Tool sets always go through toolsRoute, which picks between it and
	// hostTools by run.host_id.
	tools     *dockertools.Provider
	hostTools *HostToolProvider
	// toolsRoute is the tool factory actually used by processWork, wired up in Run.
	toolsRoute sessionToolProvider
	cancel     context.CancelFunc

	// processWork actually runs one claimed work item (GetSession → tools → return results). In
	// production, runOne sets it to our own handleWorkItem (the tool result ledger uses PG).
	// pollLoop tests replace it with a fake (it only needs to drive the fake API), so they can keep
	// testing only poll/ack/concurrency/gates, without depending on the real tool set and envinit.
	// When nil, pollLoop safely returns an error right away.
	processWork workProcessor

	// The three leader-election durations exist **only so tests can inject them**; production
	// always uses the three constants above.
	//
	// ⚠️ Do not promote them to WorkerConfig fields exposed to the config center: the chain of
	// inequalities between TTL, the reclaim threshold (45 seconds) and Ark's server-side stop line
	// (90 seconds) is a correctness dependency. A remotely tunable knob will sooner or later be
	// pushed out of range, and that is a hang window with zero errors. See the file header of
	// core/workerlease.go.
	leaseTTL    time.Duration
	leaseRenew  time.Duration
	leaseRetry  time.Duration
	leaseSpread time.Duration

	// auth is the per-environment long-backoff gate for "Ark credentials invalid (401/403)".
	//
	// ⚠️ It is read and written at the same time by the refresh/ensureWorker goroutine and by each
	// environment's worker goroutine, so it has its own lock (unlike arkbackoff.Ladder, which is
	// owned by a single pollLoop). For why the gate is in ensureWorker and not in the reconnect
	// loop, see the file header of workerauthbackoff.go. In the wrong place, "switching the key
	// does not fix it either", with zero errors.
	auth *authBackoff

	mu      sync.Mutex
	running map[string]context.CancelFunc // "workspaceID\x00environmentID" → stop function
	wg      sync.WaitGroup
}

// NewWorkerPool creates a worker pool.
//
// Returns nil when the configuration is incomplete; the caller uses that to decide whether to
// start, and logs the consequence.
func NewWorkerPool(svc *Service, cfg WorkerConfig, logger *slog.Logger) *WorkerPool {
	if svc == nil || !cfg.enabled() {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &WorkerPool{
		svc:         svc,
		cfg:         cfg,
		log:         logger,
		running:     map[string]context.CancelFunc{},
		auth:        newAuthBackoff(),
		leaseTTL:    workerLeaseTTL,
		leaseRenew:  workerLeaseRenew,
		leaseRetry:  workerLeaseRetryGap,
		leaseSpread: workerLeaseSpread,
	}
}

// Run starts and blocks until ctx is cancelled.
func (p *WorkerPool) Run(ctx context.Context) error {
	ctx, p.cancel = context.WithCancel(ctx)
	defer p.cancel()

	// The env source is used by both executors (the host also needs AMA_TOKEN and user-defined env
	// injected), so it is built outside the Docker block and shared. Output secret masking for
	// webshell and the host terminal is also set up here unconditionally (they do not go through
	// the Exec output of the six tools).
	envSrc := NewSessionEnvSource(
		p.svc, p.cfg.ManagedKeys, p.cfg.LarkUsers, p.cfg.PublicBaseURL, p.log,
	)
	if envSrc != nil {
		p.svc.SetShellEnvScrubber(envSrc)
	}

	// Docker executor: one container per Ark session. If the daemon cannot be reached, **do not
	// stop the whole worker from starting** (the host executor may still work), but state the
	// consequence loudly.
	if p.cfg.Docker.Enabled {
		if tools, err := p.setupDocker(ctx, envSrc); err != nil {
			p.log.Error("Docker 执行端装配失败：选了容器执行的 agent 一调工具就会报错",
				slog.String("err", err.Error()),
				slog.String("排查", "确认 amad 能访问 Docker daemon（/var/run/docker.sock 或 DOCKER_HOST）"))
		} else {
			p.tools = tools
		}
	} else {
		p.log.Info("未启用 Docker 执行端（docker.enabled=false）")
	}

	// Host executor: wired up only if HostExec is set. Skills/env share the same sources as the
	// container path, and the injection logic is in shelltools, so behavior is the same. There is
	// no container lifecycle, so onIdle is a no-op.
	var hostTools *HostToolProvider
	if p.cfg.HostExec != nil {
		hostTools = NewHostToolProvider(
			p.cfg.HostExec, p.svc, NewSkillFileLoader(p.svc, p.log),
			envSrc, p.log,
		)
		p.log.Info("已装配宿主机执行端：选了宿主机的 agent 将在用户机器上执行工具")
	} else {
		p.log.Info("未装配宿主机执行端（没有 host executor）")
	}
	p.hostTools = hostTools
	// The tool factory actually used by processWork: picks container or host by run.host_id.
	//
	// ⚠️ When p.tools is nil, pass a **nil interface value**, not an interface wrapping a nil
	// pointer, otherwise toolRouter cannot tell it is "not wired up" and calls methods on a nil
	// pointer.
	var containerTools sessionToolProvider
	if p.tools != nil {
		containerTools = p.tools
	}
	p.toolsRoute = newToolRouter(p.svc, containerTools, hostTools, p.log)

	// Wire up the closure that "handles one work item": binds the per-session tool factory and the
	// PG ledger. pollLoop calls it through p.processWork. The tool set factory is built per session
	// (the container handle is looked up by a name computed from the session id); the ledger is PG
	// (cross-instance, isolated per session, no local files).
	//
	// ⚠️ **Wire it up only once, here; pollLoop passes the logger in per item.** It used to be set
	// in runOne, but runOne is **one goroutine per environment**, and p is shared by the whole
	// pool: an environment started later would overwrite the earlier closure, so work from every
	// environment used the logger of "the last environment that started". In production,
	// handleWorkItem and SDK logs were tagged with the workspace/environment of **another
	// workspace** (observed: for ws_a's work, the SDK line `list events failed` was tagged ws_b).
	// Searching logs by workspace while debugging would miss your own lines and mix in others, with
	// zero errors. It was also a concurrent write with no lock (several runOne assigning at once
	// while pollLoop reads).
	p.processWork = func(workCtx context.Context, item selfhosted.WorkItem,
		workAPI selfhosted.API, arkSessionID string, logger *slog.Logger,
	) error {
		// ⚠️ The tool set factory must use this work item's workCtx, not the outer environment
		// worker's ctx: when the factory is called, toolRouter synchronously looks up the run once
		// (GetRun), and the host path also does preparation that may block, such as skill
		// injection. When the lease is lost/reassigned, workCtx ends at once; with the outer ctx
		// that preparation cannot be stopped, and the goroutine keeps holding a slot. Building a
		// new factory on every call costs next to nothing (it only assembles fields and queries the
		// database once; see the comment on ToolsFuncForSession above).
		return p.handleWorkItem(workCtx, item, workAPI, arkSessionID, workerRootDir,
			p.toolsRoute.ToolsFuncForSession(workCtx, arkSessionID),
			p.cfg.ToolResults, logger, log.New(sdkLogWriter{logger}, "", 0))
	}

	p.log.Info("self_hosted worker 池启动",
		slog.Bool("docker", p.tools != nil),
		slog.Bool("host", hostTools != nil))

	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()

	// Cleanup of old rows in the tool result ledger (sent rows are useless; started/result rows are
	// zombie leftovers). DELETE is idempotent and instances running it at once do not conflict, so
	// no leader election is needed.
	toolCleanup := time.NewTicker(toolResultCleanupInterval)
	defer toolCleanup.Stop()

	p.refresh(ctx)
	for {
		select {
		case <-ctx.Done():
			p.shutdown()
			return nil
		case <-ticker.C:
			p.refresh(ctx)
		case <-toolCleanup.C:
			p.cleanupToolResults(ctx)
		}
	}
}

// toolResultCleanupInterval is how often the tool result ledger is cleaned; toolResultRetention is
// how long rows are kept.
//
// Once a sent row has been returned successfully it is never Recovered again, so it is just filler.
// If started/result rows have not had updated_at change for a long time, the matching work has been
// reclaimed on the Ark side; keeping them only takes up rows and slows down future Recover. Keep
// for 1 hour (far longer than the life of a work item plus reclaim wait), and clear rows old
// enough.
const (
	toolResultCleanupInterval = 10 * time.Minute
	toolResultRetention       = time.Hour
)

func (p *WorkerPool) cleanupToolResults(ctx context.Context) {
	if p.cfg.ToolResults == nil {
		return
	}
	n, err := p.cfg.ToolResults.DeleteOldToolResults(ctx, time.Now().Add(-toolResultRetention))
	if err != nil {
		p.log.Warn("清理工具结果账本失败（不影响处理，下轮再试）", slog.Any("err", err))
		return
	}
	if n > 0 {
		p.log.Info("清理陈年工具结果账本", slog.Int64("删除行数", n))
	}
}

// setupDocker connects to the Docker daemon, builds a dockertools.Provider, and installs all the
// dependencies it needs.
//
// **None** of the Set* calls may be skipped. Every symptom of skipping one comes with zero errors:
//   - SetSkillLoader skipped: skill attachments never reach the container, and the agent reading
//     a script referenced in SKILL.md says the file does not exist;
//   - SetSessionLedger skipped: containers are not visible on the page ("sometimes visible" with
//     multiple instances), and the reaper sweep
//     does not know how long a container has been idle (it can only judge by creation time, so
//     long sessions get wrongly deleted);
//   - SetTerminalActivity skipped: a user halfway through vim in the web terminal has the
//     container frozen by idle pause;
//   - SetSessionEnvSource skipped: ama in the container says there are no credentials, and the
//     env the user filled in has no effect.
func (p *WorkerPool) setupDocker(ctx context.Context, envSrc *SessionEnvSource) (*dockertools.Provider, error) {
	client, err := docker.New(docker.Config{Host: p.cfg.Docker.Host})
	if err != nil {
		return nil, err
	}
	tools := dockertools.New(client, p.cfg.Docker.Tools, p.log)
	initCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := tools.Init(initCtx); err != nil {
		return nil, err
	}

	tools.SetSkillLoader(NewSkillFileLoader(p.svc, p.log))
	tools.SetSessionLedger(p.svc, p.svc, p.svc)
	tools.SetTerminalActivity(p.svc)
	if envSrc != nil {
		tools.SetSessionEnvSource(envSrc)
		// ⚠️ The check must be "is the secret configured", not just ManagedKeys != nil: that object
		// is never nil, so with only the nil check this log always says "wired up", including when
		// ama in the container is sure to fail.
		if enabled, ok := p.cfg.ManagedKeys.(interface{ ManagedKeyEnabled() bool }); ok && !enabled.ManagedKeyEnabled() {
			p.log.Info("未配 auth.managedKeySecret，容器内 ama 命令不可用（用户自定义 env 仍然生效）")
		} else {
			p.log.Info("已装配 AMA 凭据注入：勾了「AMA」授权的 agent 可用 ama 命令",
				slog.String("说明", "凭据归属空间 owner，agent 能以它操作 AMA 平台数据"))
		}
		if p.cfg.LarkUsers == nil || !p.cfg.LarkUsers.configured() {
			p.log.Info("未装配飞书用户凭据注入：勾了「飞书用户授权」的 agent " +
				"用 lark-cli 读群聊/话题历史会失败（要先在空间里接入飞书应用）")
		} else {
			p.log.Info("已装配飞书用户凭据注入：勾了「飞书用户授权」的 agent 可用 lark-cli 读群聊与话题历史")
		}
	}
	p.svc.SetSandboxAttacher(DockerSandboxAttacher{P: tools})

	// Image pre-pull and the reaper sweep both run in the background: the first saves the first
	// session from waiting for an image pull, the second clears idle containers.
	go func() {
		pctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		if err := tools.PrepareImage(pctx); err != nil {
			p.log.Warn("预拉会话容器镜像失败（建容器时会再试）", slog.String("err", err.Error()))
		}
	}()
	go tools.RunReaper(ctx, reaperLeases(p.cfg.ReaperLeases))
	return tools, nil
}

// SetReaperLeases installs the leader-election lock for the Docker reaper sweep. **It must be
// called before Run** (the sweep starts in Run). Passing nil means no lock: every instance sweeps;
// deletion is idempotent, so the result is correct.
func (p *WorkerPool) SetReaperLeases(leases Leases) {
	if p == nil {
		return
	}
	p.cfg.ReaperLeases = leases
}

// reaperLeases adapts core.Leases to dockertools.Leases (the two sides have different Lease types).
// When given nil it returns a **nil interface value**, so the reaper sweep knows "there is no
// lock".
func reaperLeases(l Leases) dockertools.Leases {
	if l == nil {
		return nil
	}
	return reaperLeaseAdapter{l}
}

type reaperLeaseAdapter struct{ l Leases }

func (a reaperLeaseAdapter) Acquire(ctx context.Context, key string, ttl time.Duration) (dockertools.Lease, bool, error) {
	lease, ok, err := a.l.Acquire(ctx, key, ttl)
	if err != nil || !ok || lease == nil {
		return nil, ok, err
	}
	return lease, true, nil
}

// refresh scans the bindings of all workspaces and starts a worker for each environment that does
// not have one yet.
//
// It only adds, never removes: once an environment has had a conversation, work may keep coming,
// and stopping its worker would hang those sessions. On process exit, shutdown collects them all.
func (p *WorkerPool) refresh(ctx context.Context) {
	// ⚠️ **Call the domain package directly, not the forwarding methods on core.** Step 14 deleted
	// that batch of `func (s *Service) X() { return s.workspaces.X() }`. The reason they existed
	// was "the orchestration layer is not connected to the domain packages yet", and the
	// orchestration layer now holds the domain packages directly (see the file header of
	// orchestrator/orchestrator.go). This is an **in-process** caller, which can already see
	// s.workspaces; an extra forwarding layer only means "who really provides this method" takes
	// one more hop to find.
	spaces, err := p.svc.workspaces.ListWorkspaces(ctx)
	if err != nil {
		p.log.Warn("worker 池扫描空间失败，下个周期重试", slog.Any("err", err))
		return
	}

	for _, ws := range spaces {
		bindings, err := p.svc.store.ListArkBindings(ctx, ws.ID)
		if err != nil {
			// ⚠️ **A query error and "this workspace has no bindings" must be kept apart.**
			//
			// These two used to be written together as `if err != nil || len(bindings) == 0 {
			// continue }`, so a database blip **silently skipped** this workspace: the worker would
			// not start, and in that workspace every tool call hung at requires_action, with **zero
			// logs the whole time**. Monitoring saw "the agent is not working", which does not
			// point to the database at all.
			//
			// Warn rather than Error: the next cycle retries, and a single failure is not a fault.
			// But it must be logged, otherwise this path leaves no trace when things go wrong. When
			// that incident was debugged, the Warn from ListWorkspaces above was the only clue
			// left.
			p.log.Warn("列空间的方舟绑定失败，跳过该空间，下个周期重试",
				slog.String("workspace", ws.ID), slog.Any("err", err))
			continue
		}
		if len(bindings) == 0 {
			continue
		}
		// One worker per environment: dedupe, then start them one by one.
		//
		// **Deduping by environment is enough; no need to also dedupe by key**: an Environment
		// belongs to the project of exactly one key (ensureBinding guarantees this; see
		// ArkBinding.KeyFingerprint), so "the same envID" always means "the same key".
		//
		// But agentID must come from **this binding itself**: with three-level credentials,
		// different agents may use different keys, and ensureWorker below uses agentID to resolve
		// credentials. Picking an arbitrary agent would resolve to another key, and that key is
		// invalid for this environment.
		seen := map[string]bool{}
		for _, b := range bindings {
			if b.ArkEnvironmentID == "" || seen[b.ArkEnvironmentID] {
				continue
			}
			seen[b.ArkEnvironmentID] = true
			p.ensureWorker(ctx, ws.ID, b.AgentID, b.ArkEnvironmentID)
		}
	}
}

// ensureWorker starts a worker for one "workspace + environment" (does nothing if it is already
// running).
func (p *WorkerPool) ensureWorker(ctx context.Context, workspaceID, agentID, envID string) {
	id := workspaceID + "\x00" + envID

	p.mu.Lock()
	if _, ok := p.running[id]; ok {
		p.mu.Unlock()
		return
	}
	workerCtx, cancel := context.WithCancel(ctx)
	p.running[id] = cancel
	p.mu.Unlock()

	// Credentials are resolved by **this agent**, not by workspace.
	//
	// ⚠️ This used to say "agentID is only here to reuse arkKey's signature; the worker itself has
	// nothing to do with any specific agent". **Since three-level credentials, that sentence is
	// false**: agent-level keys let different agents in the same workspace resolve to different
	// credentials, and this worker needs exactly the key that created the envID environment.
	// Passing the wrong agent gets another key, which is invalid for this environment (the
	// environment belongs to someone else's project). The symptom is that the worker starts, but
	// poll keeps failing and all tool calls hang at requires_action.
	key, err := p.svc.arkKey(ctx, workspaceID, agentID)
	if err != nil {
		p.log.Warn("空间没有可用的方舟凭证，worker 不启动（该空间的工具调用会卡住）",
			slog.String("workspace", workspaceID), slog.Any("err", err))
		p.mu.Lock()
		delete(p.running, id)
		p.mu.Unlock()
		cancel()
		return
	}

	// Do not start a worker again for an environment with invalid credentials (401/403) during the
	// backoff period.
	//
	// ⚠️ **The check must be here, not in the reconnect loop below**: the Ark client is built once
	// by runOne with key.APIKey (outside the loop), so retries inside the loop always use the same
	// invalid key. Only by coming back here and going through arkKey above again can the new key
	// the user switched to get in. The full argument is in the file header of workerauthbackoff.go.
	//
	// The check includes the fingerprint: once the key changes it is let through at once (wait at
	// most one refresh cycle, ≤30 seconds), without waiting for the backoff to expire.
	fingerprint := ArkKeyFingerprint(key.APIKey)
	if blocked, left := p.auth.blocked(envID, fingerprint, time.Now()); blocked {
		// ⚠️ This **must only be Debug**. refresh runs every 30 seconds; with Warn, one environment
		// with a misconfigured key floods 120 lines an hour, exactly the kind of noise this change
		// set out to remove. "Why this environment is not working" has already been said by
		// logAuthBackoff at failure time, with all four fields.
		p.log.Debug("方舟凭据仍在认证退避期内，本轮跳过这个环境",
			slog.String("workspace", workspaceID),
			slog.String("environment", envID),
			slog.Duration("剩余", left.Round(time.Second)))
		p.mu.Lock()
		delete(p.running, id)
		p.mu.Unlock()
		cancel()
		return
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() {
			p.mu.Lock()
			delete(p.running, id)
			p.mu.Unlock()
		}()
		p.runOne(workerCtx, key, envID)
	}()
}

// runOne runs the worker for one environment, and reconnects when it breaks.
func (p *WorkerPool) runOne(ctx context.Context, key ArkBindingKey, envID string) {
	logger := p.log.With(
		slog.String("workspace", key.WorkspaceID),
		slog.String("environment", envID))

	// Put the workspace/environment entity ids into ctx: all downstream xlog logs of this worker
	// carry them automatically, so searching ma_environment_id / ama_workspace_id on the log
	// platform pulls up all logs of this environment.
	ctx = xlog.With(ctx,
		xlog.KWorkspaceID, key.WorkspaceID,
		xlog.KMEnvironmentID, envID)

	baseURL, err := arkBaseURL(key.Region)
	if err != nil {
		logger.Error("解析方舟地址失败，worker 退出", slog.Any("err", err))
		return
	}
	client := arkruntime.NewClientWithApiKey(key.APIKey, arkruntime.WithBaseUrl(baseURL))

	// Build a worker per session.
	//
	// ⚠️ **Do not build a shared one**: ToolsFunc needs to know "which Ark session this work item
	// is", but the SDK does not pass the session id into ToolsFunc (AgentToolContext only has
	// Workdir). An environment-level worker is shared by concurrent work items, so recording "who
	// is current" on it is bound to mix them up.
	//
	// We used to recover the id from the last segment of workdir; **that clue is gone**: after
	// switching to HandleItem, the SDK hardcodes workdir to the root directory (`handleItem(ctx,
	// item, true)`, the opposite of Run's false), so the last segment became `ama-worker-sessions`.
	// The symptom was every tool call erroring, no container ever being created, and conversations
	// hanging at requires_action. See the comment on dockertools.Provider.ToolsFuncForSession for
	// details.
	//
	// Building a worker only assembles a few fields (no network), so one per work item costs next
	// to nothing.
	//
	// api is passed in by pollLoop instead of built here: that layer wraps it with a guard that
	// "does not send StopWork on exit" (see shutdownSafeAPI). Building our own would bypass the
	// guard, and the symptom is "one restart, and the ongoing conversation hangs forever", with
	// zero errors.
	//
	// ⚠️ **If you want to tune EventPollInterval here: this road is blocked, do not check it
	// again.**
	//
	// The list polling tick is the SDK default of 500ms (session_tool_runner.go:433-435), and
	// because our wrapper does not satisfy EventStreamer we are **permanently downgraded to list
	// polling** (see the opening part of workersendguard.go), so this default is always in effect,
	// and denser than needed.
	//
	// But `EventPollInterval` only exists on `selfhosted.SessionToolRunnerOptions`;
	// **`EnvironmentWorkerOptions` has no such field** (checked on v0.4.1-0.20260826085225; all its
	// fields are the ten above), and the SDK in lib/environments/worker.go:214-221 forwards only
	// six to the runner: WorkID/Tools/CustomTools/ResultStore/MaxIdle/Logger. That middle hop
	// hardcodes it, so it cannot be passed in from our side.
	//
	// To really lower this frequency there are only three ways: upgrade the SDK (wait for it to
	// expose the field), bypass EnvironmentWorker and use SessionToolRunner directly (we would have
	// to take over the work lifecycle and heartbeat, which HandleItem does for us now), or get
	// upstream to add it. None of these is a quick change, so this time only the rate-limit backoff
	// is fixed (see arkbackoff.Gate.Observe).
	//
	// The closure that "handles one work item" (p.processWork) is wired up once in Run, **not
	// here**: see that comment in Run.

	// **Do not use worker.Run**; we poll ourselves, see the comment on pollLoop.
	api := selfhosted.NewClientAPI(client)
	workerID := arkWorkerID()
	concurrency := p.cfg.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	logger = logger.With(slog.String("worker_id", workerID))
	logger.Info("worker 开始 poll", slog.Int("并发", concurrency))

	// envCancel lets the environment health watchdog "restart this environment worker on this pod"
	// from inside (when the lock-holding pollLoop hangs and concurrency slots are full; see
	// workerpollhealth.go). After it is called, runLeasedPoll/runOne exit, the running entry
	// registered by ensureWorker is deleted with them, and refresh rebuilds a clean pollLoop in the
	// next cycle (≤30s).
	envCtx, envCancel := context.WithCancel(ctx)
	defer envCancel()

	// pollLoop normally only returns when ctx is cancelled; in other cases (network down, poll
	// error) we must reconnect, otherwise this environment never has a worker again, and
	// conversations silently hang.
	//
	// ⚠️ **There is also a leader-election layer around it** (runLeasedPoll): every round must
	// first win this environment's poll right before polling. For why that layer is a pure
	// optimization, and the hard rule "TTL must be < arkReclaimOlderThan", see the file header of
	// core/workerlease.go.
	//
	// It returns non-nil in only one case: invalid Ark credentials (401/403). Then an
	// environment-level backoff is recorded, and the next refresh uses it to decide whether to
	// start this environment again. The fingerprint is irreversible, so it is safe to log.
	//
	// The log is written here, and only once per backoff period: the worker exits right after, and
	// can only be started again after the backoff expires. The old shape was one line every 5
	// seconds × (number of environments); in production that flooded thousands of "worker exited
	// abnormally" lines in under half an hour.
	if err := p.runLeasedPoll(envCtx, api, envID, workerID, concurrency, logger, envCancel); err != nil {
		p.onAuthFailure(ctx, logger, envID, key.WorkspaceID, ArkKeyFingerprint(key.APIKey), err)
	}
}

// runLeasedPoll is the loop that "polls only after winning the poll right", and is the main body of
// runOne.
//
// **It is a separate method rather than inlined in runOne**: the first half of runOne builds the
// Ark client, resolves the address, and assembles the worker factory, all of which need real
// credentials; while the logic of this layer (leader election, renewal, stopping on lock loss,
// reconnect) can all be driven with a fake API + fake lock. Only after splitting can
// workerlease_test.go test it. Without the split, those six cases could only test a loop "inlined
// after network setup", which means it could not be tested.
//
// It normally only returns when ctx is cancelled, and then returns nil.
//
// **The return value is non-nil in only one case**: invalid Ark credentials (401/403). The caller
// runOne uses it to record an environment-level backoff. The check is returned from here instead of
// being recorded in this layer because the key fingerprint lives in runOne (this layer deliberately
// does not touch real credentials, so workerlease_test.go can drive it with a fake API).
//
// ⚠️ It returns an error instead of taking a keyFingerprint parameter so that the five calls in
// workerlease_test.go do not change: they use this call as a statement, so adding a return value
// does not affect them, while adding a parameter would mean changing all of them.
func (p *WorkerPool) runLeasedPoll(
	ctx context.Context,
	api selfhosted.API,
	envID, workerID string,
	concurrency int,
	logger *slog.Logger,
	envCancelOpt ...context.CancelFunc,
) error {
	var envCancel context.CancelFunc
	if len(envCancelOpt) > 0 {
		envCancel = envCancelOpt[0]
	}
	// Spread out a little before the first lock attempt, so the first pod to start does not grab
	// the leases of every env at once (see workerLeaseSpread).
	if d := p.leaseSpreadDelay(); d > 0 && !arkSleep(ctx, d) {
		return nil
	}
	for {
		lease, ok, err := p.acquirePollLease(ctx, envID)
		if err != nil {
			// Redis error. **This must not stop polling**: this layer is an optimization, and if it
			// is down we fall back to "every pod polls", not "nobody polls". But leave a trace,
			// otherwise "why did RPM go up again" cannot be traced to its source.
			logger.Warn("抢 poll 权失败，本轮照常 poll（退化成每个实例都 poll）",
				slog.Any("err", err))
		} else if !ok {
			// The lock is held by another pod. This is **normal**: it is polling this environment.
			// Retry at a fixed interval, no backoff (reason in workerLeaseRetryGap). Leave a Debug
			// trace: this path used to be fully silent, so when an environment stalled there was no
			// way to find out even "who is still waiting for the lock". Debug is turned on when
			// needed and does not flood logs normally. A real "lock holder hung" is detected by the
			// environment health watchdog on the holder side, which releases the lock
			// (workerpollhealth.go); it does not rely on this.
			logger.Debug("poll 权在别的实例手上，本实例待命重试", slog.String("environment", envID))
			if !arkSleep(ctx, p.leaseRetry) {
				logger.Info("worker 停止")
				return nil
			}
			continue
		}

		// Poll only while holding the lease; when the lease is lost, stop this round at once and
		// let the new holder have it alone.
		leaseCtx, cancelLease := context.WithCancel(ctx)
		var leaseWG sync.WaitGroup
		if lease != nil {
			logger.Info("拿到 poll 权，开始 poll 这个环境")
			leaseWG.Add(1)
			go func() {
				defer leaseWG.Done()
				p.keepPollLease(leaseCtx, lease, cancelLease, logger)
			}()
		}

		// Environment-level liveness heartbeat + watchdog. It must be independent of pollLoop: when
		// pollLoop hangs (concurrency slots are full of work that does not respond to cancel, so it
		// never reaches PollWork), this goroutine must still detect "holding the lock but not
		// getting work", release the lock so another pod can take over, and restart this
		// environment's worker. See workerpollhealth.go.
		health := newPollHealth(time.Now())
		var healthWG sync.WaitGroup
		healthWG.Add(1)
		go func() {
			defer healthWG.Done()
			p.runPollHealthWatchdog(leaseCtx, health, envID, workerID, lease, envCancel, logger)
		}()

		err = p.pollLoop(leaseCtx, api, envID, workerID, concurrency, logger, health)

		// The watchdog is collected together with this round of polling.
		cancelLease()
		healthWG.Wait()

		// Cleanup order: first stop the renewal loop, then release the lock.
		//
		// ⚠️ Release with a **ctx that cannot be cancelled**: on the shutdown path the original ctx
		// is often already cancelled, and releasing with it fails; then we can only wait for the
		// TTL to expire (up to 20 seconds with nobody polling this environment). Same as in
		// lark.Receiver.Run.
		cancelLease()
		leaseWG.Wait()
		if lease != nil {
			if relErr := lease.Release(context.WithoutCancel(ctx)); relErr != nil {
				logger.Warn("释放 poll 权失败（会自己过期）", slog.Any("err", relErr))
			}
		}

		if err != nil && ctx.Err() == nil {
			// Invalid credentials (401/403): **exit this worker, do not reconnect here**.
			//
			// ⚠️ Returning here does not mean "give up on this environment". The error goes back to
			// runOne, which records a backoff (only that layer has the key fingerprint); then this
			// worker exits and its p.running entry is deleted. The next refresh (≤30 seconds)
			// resolves credentials again and decides whether to start it. **Only that path can get
			// the new key the user switched to**, while the client in this loop was built with the
			// old key, so ten thousand reconnects all give the same 401. The full argument is in
			// the file header of workerauthbackoff.go.
			if arkAuthStatus(err) {
				return err
			}
			logger.Warn("worker 异常退出，5 秒后重连", slog.Any("err", err))
		}
		select {
		case <-ctx.Done():
			logger.Info("worker 停止")
			return nil
		case <-time.After(p.reconnectGap()):
		}
	}
}

// reconnectGap is how long to wait before retrying after pollLoop exits abnormally.
//
// It is fixed at 5 seconds in production. **Tests must be able to shorten it**: after losing the
// lock this method sleeps before going back to grab the lock, and 5 seconds would make the
// "takeover" case wait more than 5 seconds, so slow it could only be skipped. It is derived from
// leaseRetry instead of adding a new field: both are "how long before trying again", of the same
// order of magnitude.
func (p *WorkerPool) reconnectGap() time.Duration {
	if p.leaseRetry > 0 && p.leaseRetry < 5*time.Second {
		return p.leaseRetry
	}
	return 5 * time.Second
}

// pollLoop takes work and handles it **concurrently**, replacing the SDK's own
// environments.EnvironmentWorker.Run.
//
// # Why not use the SDK's Run
//
// One work item is one turn of conversation. `Run` is serial (`for poller.Next() {
// w.runClaimedWork(...) }` in `lib/environments/worker.go`, with no go), and a work item only exits
// after the session has been idle for `MaxIdle` (default 60 seconds): `SessionToolRunner.Next()`
// blocks reading a channel, and the channel is closed by the idle timer. So under the same
// environment **the second person sending a message has to wait for the first person to go idle**,
// and environments are per workspace (agents in the same workspace share one), so this is "a
// colleague is chatting, and my message gets no response". Zero errors, and one person testing
// locally never hits it.
//
// # Why not just wrap Run in a goroutine
//
// Then N pollers would each poll the same queue, and acks would race (`AckWork` is a racing
// queued→starting operation): the winner is random, and the losers make a wasted trip. Also, each
// poller inside is still serial, so concurrency cannot be controlled.
//
// # Why not use the SDK's WorkPoller
//
// ⚠️ **The first line of `WorkPoller.Next()` is `p.runPendingStop()`, which sends `StopWork` for
// the previous work item** (`lib/environments/poller.go:81,177-186`). In the serial model that is
// right (the previous one has finished), but with concurrency the second `Next()` marks the first
// work item, **still running**, as ended:
//
//	for poller.Next() {          // ← this sends StopWork for the previous one
//	    go worker.HandleItem(...) // ← while the previous one is still running in this goroutine
//	}
//
// That work item becomes final on the Ark side, and when we finish and `postResult`, we return
// results to a work item that has ended. The agent hangs at `requires_action`, while `StopWork` and
// `postResult` **both return success**. This pendingStop has no switch to turn it off, so the only
// way is to not use WorkPoller and call PollWork/AckWork ourselves (both are on the public
// `selfhosted.API`).
//
// The job of `StopWork` goes to the defer inside `HandleItem` (`w.stopItem` in `worker.go`), which
// settles each work item on its own, so it fits concurrency naturally.
//
// # The concurrency limit is decided by container count, not amad's CPU
//
// Each running work item may hold its own container, all packed on the same Docker daemon, so the
// limit is about whether the host can take it. Failing to create a container does not hang
// silently: dockertools reports the error to the agent; see that package's README.
func (p *WorkerPool) pollLoop(
	ctx context.Context,
	api selfhosted.API,
	envID, workerID string,
	concurrency int,
	logger *slog.Logger,
	healthOpt ...*pollHealth,
) error {
	// Always passed on the production path; nil when old unit tests do not pass it. All heartbeat
	// marks are nil-safe (see the pollHealth methods).
	var health *pollHealth
	if len(healthOpt) > 0 {
		health = healthOpt[0]
	}
	// sem limits concurrency; wg makes sure all running work has finished before returning
	// (otherwise reconnects would stack up two batches of goroutines).
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	defer wg.Wait()

	// workerCtx is **the lifetime of this worker**, used only to judge "is the process exiting".
	//
	// ⚠️ **Do not change the places that use it below back to a bare `ctx`.** It deliberately has a
	// new name because each work item's goroutine is likely to have a per-work ctx (for example one
	// derived just for HandleItem so its own retries are cut off after the work finishes). That
	// kind of ctx **is also cancelled when it ends normally**, so:
	//
	//	if ctx.Err() == nil { resend orphan tool results }   ← always false once shadowed by a per-work ctx
	//
	// The fallback silently becomes dead code, **with zero errors, and tests may still be green**
	// (if a case goes down the "process exit" branch, what it expects is exactly no resend).
	// Pinning this check with a name that cannot be shadowed is more reliable than relying on "the
	// next person remembers not to shadow ctx".
	workerCtx := ctx

	// The API given to each work item is wrapped with a guard: on process exit, StopWork is **not
	// allowed**. For why, see shutdownSafeAPI.
	//
	// Add one more layer of rate-limit backoff: when we hit Ark's account RPM, **all concurrent
	// work in this environment** backs off together. The gate is built here (one per pollLoop)
	// rather than one per work item, because rate limits are per account: if each runner backs off
	// on its own, N staggered retries still add up to one steady high-frequency request stream. See
	// the file header of workerratelimit.go.
	//
	// ⚠️ **The gate must also cover the PollWork below.** It used to wrap only
	// ListEvents/SendEvent, on the grounds that "PollWork has arkBackoff, and stacking another
	// layer would conflict". That reason **only holds in a single process**: in production there
	// are (environments × instances) poll loops, each backing off on its own and adding up to one
	// steady high-frequency stream, exactly the shape that same file header points out. See the
	// comment on pollWork below. Add a layer of redacted logging to all Ark calls of this pollLoop
	// (take work/ack/create session/download skills/pull events/ return results/heartbeat). Method
	// name, duration and failure status code all go to the log platform. After a release, the SDK's
	// own logs for those steps are not collected, so "which step the tool is stuck on" would become
	// a black box. It wraps the outermost layer, so PollWork/AckWork and every work item's calls go
	// through it. See workerapilog.go.
	api = newInstrumentedWorkerAPI(api, logger)

	gate := newArkRateLimitGate(logger)
	workAPI := newRateLimitedAPI(newShutdownSafeAPI(ctx, api, logger), gate)

	// Idle backoff steps + a cross-pod wake receiver; the two go together: the steps bring idle RPM
	// down to single digits, and the wakeup makes sure "when there is work, we do not wait a full
	// step interval". See workerbackoff.go / workerwake.go.
	//
	// When waker is nil (Wakeups not configured), only the steps are used: correct, just waits up
	// to one step interval in the worst case.
	var ladder backoffLadder
	waker := p.watchWakeups(ctx, envID, logger)

	failures := 0
	// authRecordCleared: whether this pollLoop has cleared the invalid-credential record in the
	// database. Successful polls happen every second or so, so deleting a row every time is
	// wasteful; and after a pod is rebuilt the in-process gate is empty and cannot tell "is there
	// an old record in the database", so each pollLoop deletes it once on its first success.
	authRecordCleared := false
	for {
		if ctx.Err() != nil {
			return nil
		}

		// Take a slot first, then poll: when concurrency is full, do not take new work.
		//
		// **The order must not be reversed.** If we polled first and then took a slot, the work we
		// got would wait for a slot, while Ark has already handed it to us: holding the lease
		// without working, until it times out and another worker grabs it (`ReclaimOlderThanMS`).
		// That is a wasted retry, and the logs do not show why.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return nil
		}

		// PollWork must also go through the rate-limit gate: on a 429, **all** calls in this
		// environment wait together, not just this one poll loop backing off. See
		// pollWorkThroughGate.
		item, err := pollWorkThroughGate(ctx, api, gate, selfhosted.PollWorkRequest{
			EnvironmentID: envID,
			WorkerID:      workerID,
			// Long poll: when there is no work, the server holds this request. The value matches
			// the SDK's defaultPollBlockMS, **and is also the limit given in Ark's docs** (block_ms
			// ranges 1~999ms), so there is no room to "raise it a bit" here.
			BlockMS: arkPollBlockMS,
			// Take back work that "the previous process left unfinished". The threshold is 45s: it
			// must be later than the old worker's 30s heartbeat self-kill line (to avoid running
			// twice), and earlier than Ark's 90s server-side stop line (to avoid hanging forever).
			// See the full argument at arkReclaimOlderThan.
			ReclaimOlderThanMS: int(arkReclaimOlderThan / time.Millisecond),
		})
		if err != nil {
			<-sem
			if ctx.Err() != nil {
				return nil
			}
			// 4xx (except timeout/conflict/rate limit) is a config problem, and retrying does not
			// help: the environment does not exist, or the key has no permission. Let it bubble up
			// to runOne to reconnect; that layer logs it and retries after 5 seconds, so a wrong
			// envID does not turn into an endless loop hammering the API.
			if arkFatalStatus(err) {
				return fmt.Errorf("poll work: %w", err)
			}
			failures++
			wait := arkBackoff(failures)
			logger.Warn("poll work 失败，退避后重试",
				slog.Any("err", err), slog.Duration("等待", wait))
			if !arkSleep(ctx, wait) {
				return nil
			}
			continue
		}
		// PollWork round trip succeeded (whether or not it got work): record a liveness heartbeat.
		// The environment health watchdog uses it to judge "has the lock holder hung and stopped
		// taking work" (see workerpollhealth.go). Note we only beat when err==nil: network faults
		// during backoff retries must not count as "healthy".
		health.beat(time.Now())
		// One successful PollWork proves the key is good: clear the auth failure count.
		//
		// ⚠️ **This line must not be dropped.** Dropping it looks exactly like the
		// arkbackoff.Ladder.Reset case: an environment that occasionally blips a 401 would pile the
		// count all the way up to the 5-minute step and never come back, so the next failure after
		// every recovery starts from the top step. Zero errors.
		// TestPollLoopClearsAuthBackoffOnSuccess locks it in. **That one is a wiring test**:
		// TestAuthBackoffResetsAfterSuccess, which only tests the data structure, cannot stop this
		// line from being deleted (checked: with only the latter, deleting this line keeps all
		// tests green).
		p.auth.clear(envID)
		if !authRecordCleared {
			// The start time in the database must be cleared too: otherwise, if this key works for
			// a while and then breaks again, "how long it has been invalid in a row" would count
			// from long ago, and a single failure would pause it for a whole day.
			p.clearAuthFailureRecord(ctx, envID)
			authRecordCleared = true
		}
		failures = 0

		// No work (the long poll timed out); rest a bit, then go again.
		//
		// ⚠️ **This sleep must not be skipped.** Skipping it was the direct source of thousands of
		// 429s in production.
		//
		// Ark's block_ms limit is 999 (the docs say "ranges 1~999 milliseconds"), so the "long
		// poll" holds for at most 1 second. When idle, this for loop is a timer **firing once a
		// second**, not "hang as long as there is no work". One loop is ~60 req/min; in production,
		// with N environments × M pods of loops, it adds up to ~1200 RPM all on the same Ark
		// account, **all idle spinning**: in the same window, "took work" 0 times, tool execution 0
		// times, not a single piece of work done.
		//
		// The SDK itself has this sleep (`sleep(p.ctx, jitter(time.Second, 3*time.Second))` in
		// `lib/environments/poller.go`). When we copied its poll loop we **left this line out**.
		// That was the root cause at the time, not a lack of quota.
		//
		// **It is now a step backoff** (1s → 10s → 20s → 30s → 60s, see workerbackoff.go): a fixed
		// [1s,3s) only fixes "once a second", not the "number of environments" dimension, and that
		// dimension only grows with the business. The idler it is, the harder it slows down; long
		// unused environments sink to once every 60 seconds.
		//
		// **Every step still has jitter** (done in the ladder, see jitterAround): loops moving in
		// lockstep stack requests into spikes and still hit rate limits even if the average is
		// fine. This does not go away with steps.
		//
		// The wait **can be interrupted by a wakeup** (someone sent a message), so slowing down
		// does not cost response speed. Both are required; see the file header of workerwake.go.
		if item == nil || item.ID == "" {
			<-sem
			if !waker.wait(ctx, ladder.Next()) {
				return nil
			}
			continue
		}
		if item.EnvironmentID == "" {
			item.EnvironmentID = envID
		}
		// A work item without a session id cannot be handled; ack it so we do not keep getting the
		// same one.
		if item.SessionIDValue() == "" {
			logger.Warn("work 没有 session id，丢弃", slog.String("work_id", item.ID))
			p.discardWork(ctx, api, *item, workerID, logger)
			<-sem
			continue
		}

		// ack moves work from queued to starting. **If it fails, we must not handle it**: ack is a
		// racing operation, and failure means we cannot prove this work is ours (another instance
		// may have grabbed it). Running it anyway would mean two instances handling the same turn:
		// two containers, two tool_results.
		if err := api.AckWork(ctx, selfhosted.AckWorkRequest{
			EnvironmentID: item.EnvironmentID,
			WorkID:        item.ID,
			WorkerID:      workerID,
		}); err != nil {
			<-sem
			if ctx.Err() != nil {
				return nil
			}
			if arkFatalStatus(err) {
				return fmt.Errorf("ack work: %w", err)
			}
			// 404/409/412 = someone else is already handling it. This is a normal race, not an
			// error.
			if !arkResolvedStatus(err) {
				logger.Warn("ack work 失败，跳过这条",
					slog.String("work_id", item.ID), slog.Any("err", err))
			}
			continue
		}

		// **Copy the value instead of holding a pointer**: the next poll writes item, so a closure
		// capturing the pointer would read someone else's work.
		it := *item

		// ⚠️⚠️ **Go back to the fastest step as soon as we get work. This line must not be
		// deleted.**
		//
		// The symptom of missing it is "active environments get slower the more they are used",
		// **with zero errors**: a heavily used environment would climb to the 60-second step from a
		// few empty polls between conversation turns, and then **never come back**. The next
		// message would take a minute to be noticed, while logs, monitoring and tests are all
		// green. This is the line in the whole change that is easiest to miss and hardest to debug.
		// `TestWorkerBackoffResetsAfterWork` locks it in (remove this line and that case turns
		// red).
		ladder.Reset()
		// Also clear any pending wake signal: this round goes straight back to poll anyway, and
		// keeping it would only make the next wait return at once and waste a request.
		waker.drain()

		logger.Info("领到 work",
			slog.String("work_id", it.ID), slog.String("ark_session", it.SessionIDValue()))

		wg.Add(1)
		go func() {
			defer wg.Done()

			// One ctx log context per work item, carrying the run/Ark session/work id. That way all
			// logs in this round for "take work → ack → tool execution → return result → failover"
			// can be tied together by one ma_session_id. This is the most important search key when
			// debugging "the tool should have run but did not".
			//
			// ⚠️ **It must be a new goroutine-private variable wctx, not `ctx = xlog.With(ctx)`
			// overwriting the shared outer ctx**: pollLoop starts several work goroutines
			// concurrently, and each writing the same ctx variable is a data race (seen with
			// -race). The new variable only adds values and passes through the same cancel signal;
			// downstream workCtx and all logs reuse it, which satisfies "change ctx first so child
			// functions can reuse it".
			wctx := xlog.With(ctx,
				xlog.KRunID, it.SessionIDValue(),
				xlog.KMSessionID, it.SessionIDValue(),
				xlog.KWorkID, it.ID,
				xlog.KWorkerID, workerID)
			activeNow := 0
			if health != nil {
				activeNow = int(health.activeWork.Load()) + 1
			}
			xlog.Info(wctx, "开始处理一条 work（环境内并发槽占用 %d/%d）", activeNow, cap(sem))
			defer func() { <-sem }()
			// Track the number of in-flight work items (= slots in use). Printed when the
			// environment health watchdog sees a stall, to tell apart "slots are full of hung work"
			// from "polling network is stuck" (see workerpollhealth.go).
			health.workStarted()
			defer health.workDone()
			// panic fallback: one crashing work item must not take down the whole worker (that
			// would stall all later conversations in this environment, and outside the HTTP layer
			// nobody catches the panic stack).
			defer func() {
				if r := recover(); r != nil {
					logger.Error("处理 work panic",
						slog.String("work_id", it.ID), slog.Any("panic", r))
				}
			}()
			// ⚠️ **Build a new worker for every work item; do not reuse the outer one.**
			//
			// Because ToolsFunc needs to know "which Ark session this is", and the SDK does not
			// pass the session id into ToolsFunc (AgentToolContext only has Workdir). The outer
			// worker is environment-level and shared by concurrent work items, so there is no way
			// to record "who is current" on it; recording it would mix them up.
			//
			// We used to recover the id from the last segment of workdir, but **HandleItem
			// hardcodes workdir to the root directory** (`handleItem(ctx, item, true)`, the
			// opposite of Run's false), so the last segment became `ama-worker-sessions` and
			// recovering failed outright → every tool call errored, and no container could ever be
			// created. See the comment on dockertools.Provider.ToolsFuncForSession for details.
			//
			// Building a worker only assembles a few fields (no network, see
			// NewEnvironmentWorkerForClient), so one per work item costs next to nothing.
			//
			// ⚠️ **Each work item needs its own ctx; do not use the outer one directly.**
			//
			// This is the seam for the "zombie work" fix: when results can never be delivered, or
			// this session's run has already reached a final state, we must be able to stop **only
			// this one**. With the outer ctx it would become "stop the whole environment's worker",
			// interrupting other healthy conversations in the same environment.
			//
			// Derived from workerCtx rather than Background: process exit must still propagate
			// down, otherwise this work would not go away on shutdown.
			//
			// ⚠️ **Deliberately not named `ctx`**: that would shadow the outer one and turn the "is
			// the process exiting" checks below into always false. See the comment on workerCtx at
			// the top of pollLoop (that fallback would silently become dead code, and tests might
			// still be green).
			workCtx, cancelWork := context.WithCancel(wctx)
			defer cancelWork()

			// Two gates, both required. For why, and why one is not enough, see the file header of
			// core/workersendguard.go.
			//
			// One, delivery gate: after a permanent 4xx (400 payload invalid / 404 session
			// expired), stop sending SendEvent for this session and stop this work. **429 is not
			// permanent.**
			//
			// ⚠️ The order is "gate on the outside": workAPI already has rate-limit backoff and the
			// exit guard, and only wrapping outside can short-circuit **before hitting the
			// network**. Wrapped inside, every call would first wait on the rate-limit gate, which
			// saves nothing.
			sendGuard := newWorkSendGuard(workAPI, it.SessionIDValue(), cancelWork, logger)
			// failover wraps outside sendGuard: when the watchdog decides "tool results have not
			// come back for a long time = the executor is hung", it stops heartbeats and blocks
			// StopWork, handing the work back to Ark to be reassigned after 45s to a healthy worker
			// that really runs it (if not taken within 90s the server marks it stopped, see the
			// arkReclaimOlderThan comment), instead of ending or erroring. The SDK's heartbeats and
			// final StopWork both go through it. See workerfailover.go.
			failover := newWorkFailover(sendGuard, it.ID, logger)

			// Two, final-state gate: when that run in the database reaches terminated, stop this
			// work. **It must query the database**: the instance that finishes that run is usually
			// not the one running this work.
			go p.watchRunTerminated(workCtx, it.SessionIDValue(), cancelWork, logger)

			// Three, progress watchdog: once a tool call is out and has no result after
			// workToolProgressBound, it fails over. Events are read with pollLoop's api (not slowed
			// down by per-work gates), so the supervisor can still read events when "the executor
			// is hung".
			go p.watchWorkToolProgress(workCtx, failover, api, it, cancelWork, logger)

			// Run one work item (production = our own handleWorkItem: the tool result ledger uses
			// PG, isolated per session; heartbeats and the final StopWork are inside too, through
			// the failover wrapper). See core/workerhandle.go and pgtoolresult.go.
			//
			// The check uses `workerCtx` (the worker's lifetime); **do not change it to workCtx**.
			// That is the rule written in the workerCtx comment at the top of pollLoop.
			if p.processWork == nil {
				logger.Error("worker 未装配 processWork，跳过这条 work",
					slog.String("work_id", it.ID))
				return
			}
			if err := p.processWork(workCtx, it, failover, it.SessionIDValue(), logger); err != nil &&
				workerCtx.Err() == nil {
				logger.Warn("处理 work 失败",
					slog.String("work_id", it.ID), slog.Any("err", err))
				xlog.Warn(wctx, "处理 work 失败（将进入收尾对账）：%v", err)
			} else if failover.isAbandoned() {
				// failover let go: this work has been handed back to Ark for reassignment, and this
				// executor no longer reports results. Log a separate line so it is not mixed up
				// with "finished normally" and the failover is visible.
				xlog.Warn(wctx, "这条 work 已 failover 交回方舟重派（执行者判定卡死），本实例不报结果")
			} else {
				xlog.Info(wctx, "一条 work 处理结束（正常）")
			}

			// Final reconciliation: every tool_use Ark started must have a result returned; for
			// missing ones, send an error result.
			//
			// ⚠️ **This step must come after HandleItem, and run whether it succeeded or failed.**
			// None of the ways a runner exits (idle timeout, lease lost, event stream breaking the
			// moment a tool_use was sent, pod replaced) sends the missing tool_result, and missing
			// one means the whole turn hangs forever at requires_action. See the file header of
			// workertoolaudit.go for details.
			//
			// # Why use a ctx without cancellation
			//
			// ⚠️ It is **not** because "HandleItem cancels our ctx": it cancels the workCtx it
			// derives itself (`workCtx, cancel := context.WithCancel(ctx)`); this outer ctx is not
			// affected. A self-review once got this wrong, so it is written down to avoid changing
			// it again for that wrong reason.
			//
			// The real reason: this step must be able to finish even while **the process is
			// shutting down**. Once the worker's ctx is cancelled, the `ctx.Err() == nil` below
			// skips the resend (on purpose, see below), but when `refresh` replaces a worker, or an
			// upper layer cancels for another reason, reconciliation should still finish instead of
			// being cut off halfway. It only needs a bounded timeout of its own, not to be
			// cancelled together with upstream.
			//
			// # But no resend on process exit
			//
			// The check is **workerCtx** (this worker's lifetime), not the ctx here. On process
			// exit that work is **left on purpose for a new instance to reclaim** (see
			// shutdownSafeAPI). Resending results would push the turn forward before the reclaim:
			// the tool did not actually run, yet the agent gets "could not run". That turns a
			// restart that could heal itself into a real failed turn.
			//
			// ⚠️ **Using workerCtx instead of ctx is deliberate**; do not change it back: if a
			// per-work ctx shows up in this goroutine (it is also cancelled on normal finish), a
			// bare `ctx` would make this check always false, and the fallback would silently become
			// dead code. See the workerCtx comment at the top of pollLoop.
			//
			// ⚠️ **It must be passed sendGuard, not workAPI.** This was the direct cause of a
			// production incident: the gate is built **outside** workAPI, so passing workAPI means
			// sneaking under the gate. In the same work item the gate applied to the runner but not
			// to the fallback.
			//
			// In production (session sesn-20260101000010-ex010) the shape was: the runner hit a 400
			// and set the gate, the work was stopped, and then the fallback used workAPI to send
			// both results again as is, each getting a 500. Ark redelivered this work every 2.2
			// seconds, so in 372 seconds it produced **210 resends that were bound to fail** + 15
			// 429s (those 15 came from the fallback's ListEvents, which is why the gate also wraps
			// reads; see workSendGuard.ListEvents). The evidence is hard: in that window, "skip
			// this delivery" in `workersendguard.go` appeared **0 times**, while "resending tool
			// result failed" appeared 210 times: not one fallback request went through the gate.
			//
			// With the gate passed, the fallback logs "resend failed" (the gate returns the error
			// it remembered). **That is right**: it honestly reports "this result was not
			// delivered". Turning it into a fake success would leave a fake "resent" line in the
			// logs, and that is exactly the only useful information when debugging a
			// requires_action hang. See the SendEvent comment in workersendguard.go. Work that
			// failover let go **gets no error results**: those tool_use calls are left for the
			// healthy worker Ark reassigns after 45s to really run (that is the whole point of
			// failover). Sending error results would turn "waiting for reassignment" into "got a
			// fake result". The reassigned worker does this reconciliation itself when it finishes,
			// so the fallback is still there.
			if workerCtx.Err() == nil && !failover.isAbandoned() {
				auditCtx, cancelAudit := context.WithTimeout(
					context.WithoutCancel(workerCtx), toolAuditTimeout)
				auditToolCalls(auditCtx, sendGuard, it.SessionIDValue(),
					logger.With(slog.String("work_id", it.ID)))
				cancelAudit()
			}
		}()
	}
}

// shutdownSafeAPI wraps selfhosted.API and **blocks StopWork while the process is exiting**.
//
// # This is the fix for a real incident
//
// Session sesn-20260101000006-ex006 hung forever at its 47th tool call. Timeline:
//
//	13:07:45.783  session idle, container paused    ← normal pause after the previous end_turn
//	13:07:45.799  worker stopped                    ← ./dev.sh restart, ctx cancelled
//	13:07:48.929  agent.tool_use                    ← the model wants a tool again, but nobody is listening
//	13:07:50.970  worker pool started               ← the new process is up, but can never take this one back
//
// The database had 47 agent.tool_use but only 46 user.tool_result, and the last event was
// `session.status_idle{stop_reason: requires_action}`. On the page it is "stopped halfway".
//
// # Why the StopWork sent on exit is fatal
//
// The SDK's handleItem has a defer: once ctx is cancelled, heartbeatLoop returns directly (that
// path **does not call markStopped**), so cause is an empty string, and `shouldStopItem("")` falls
// into the default branch and returns true: **a graceful exit is treated as "this work is done"**,
// and `StopWork{Force: true}` is sent.
//
// The branches with a non-empty cause (lease_lost / heartbeat_lost / ...) do **not** send it: the
// SDK's intent is "do not touch it when ownership is unclear". Only the path "I am exiting myself",
// which should keep the work most of all, got filed under "normal finish".
//
// The result is that the work on the Ark side reaches a final state, and `sequence` stays where it
// is. When the new process comes up, it is no longer in the work queue. **This is not slow, it is
// permanent.**
//
// # Why reclaim cannot cover it
//
// reclaim grabs work that is "claimed but with an expired heartbeat". Work settled by StopWork is
// final, and not grabbable. So that parameter (arkReclaimOlderThan) solves the other half of the
// problem: the case where **the process is killed with kill -9 and the defer does not run**. Both
// are needed:
//
//	graceful exit (SIGTERM, runs defer) → this guard does not send StopWork → new process reclaims it
//	hard kill (kill -9, no defer)       → StopWork was never sent         → new process reclaims it
//
// Without the guard, the first case (also the most common: rolling releases, ./dev.sh) can never be
// saved.
//
// # Why wrap at the API layer and not elsewhere
//
// That defer is inside the SDK, and `EnvironmentWorkerOptions` has no switch to turn it off
// (confirmed: none of the 11 fields of the whole struct is about stop). The **only** seam we can
// hook into is the interface taken by `NewEnvironmentWorker(api, ...)`: StopWork eventually goes
// through it.
type shutdownSafeAPI struct {
	selfhosted.API
	// ctx is the worker's lifetime. Once it is cancelled, it means "we are exiting", not "this work
	// finished normally".
	ctx context.Context
	log *slog.Logger
}

// newShutdownSafeAPI adds the guard. If api is nil it returns it unchanged for the caller to
// handle.
func newShutdownSafeAPI(ctx context.Context, api selfhosted.API, logger *slog.Logger) selfhosted.API {
	if api == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &shutdownSafeAPI{API: api, ctx: ctx, log: logger}
}

// StopWork gives up the call outright while the process is exiting.
//
// **The check is the worker's ctx, not the stopCtx passed in.** The SDK side uses
// `context.Background()` (made on the spot in stopItem), so it would still be sent on exit. That is
// exactly what we need to block.
func (a *shutdownSafeAPI) StopWork(ctx context.Context, req selfhosted.StopWorkRequest) error {
	if a.ctx != nil && a.ctx.Err() != nil {
		// Leave a log line: this work is kept **on purpose**, waiting for the new process to
		// reclaim it. Without it, the next person would think "that conversation vanished on
		// restart" for some other reason.
		a.log.Info("进程退出中，保留在途 work 不结算（等新实例 reclaim 接手）",
			slog.String("work_id", req.WorkID))
		return nil
	}
	return a.API.StopWork(ctx, req)
}

// discardWork settles a work item we cannot handle, so we do not keep getting it.
//
// ack then stop: with only stop and no ack it is still queued, and the next round takes it again.
//
// ⚠️ **Both steps can fail, and failure shows up as "the upstream Warn floods the logs".** Before
// discarding, the caller logs a "work has no session id, discarding" line, and that sentence claims
// "it has been discarded". When ack fails it was not really discarded, the next round gets the same
// one again, and the same Warn shows up every round. It looks like "Ark keeps sending the same bad
// work", while the real cause is **we did not settle it**. The direction is exactly reversed; this
// is the most time-wasting kind of misdirection.
//
// logger is passed in by the caller instead of using p.log: the caller's logger already carries the
// environment / worker fields; the pool-level one would lose "which worker of which environment".
func (p *WorkerPool) discardWork(
	ctx context.Context, api selfhosted.API, item selfhosted.WorkItem, workerID string, logger *slog.Logger,
) {
	if item.ID == "" {
		return
	}
	if err := api.AckWork(ctx, selfhosted.AckWorkRequest{
		EnvironmentID: item.EnvironmentID,
		WorkID:        item.ID,
		WorkerID:      workerID,
	}); err != nil {
		// Not acked = this work is still queued = the next round will get it again. Without logging
		// this, the upstream "discard" would become a recurring claim that is not true.
		logger.Warn("丢弃 work 时 ack 失败，它仍在队列里、下一轮会被再次领到",
			slog.String("work_id", item.ID),
			slog.String("environment_id", item.EnvironmentID),
			slog.Any("err", err))
		return
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), arkStopTimeout)
	defer cancel()
	if err := api.StopWork(stopCtx, selfhosted.StopWorkRequest{
		EnvironmentID: item.EnvironmentID,
		WorkID:        item.ID,
	}); err != nil {
		// ack succeeded but stop did not: this work is stuck in starting until the reclaim window
		// (45 seconds) expires and it is taken back. Milder than the one above, but still worth a
		// trace, otherwise "why did this work come back after 45 seconds" cannot be traced to its
		// source.
		logger.Warn("丢弃 work 时 stop 失败，它会卡到 reclaim 窗口过期后重来",
			slog.String("work_id", item.ID),
			slog.String("environment_id", item.EnvironmentID),
			slog.Any("err", err))
	}
}

// A few poll constants and error classes, matching the SDK's lib/environments/poller.go.
//
// Those are package-private (isFatal4xx / backoff / jitter / sleep), so we cannot reach them and
// have to copy them. **The values must stay the same as the SDK's**: they map to real incidents,
// and casually changing them would step into those pits again.
const (
	// arkPollBlockMS is the number of milliseconds the long poll holds, matching the SDK's
	// defaultPollBlockMS.
	//
	// ⚠️ **999 is Ark's hard limit**, not a value we picked: the doc "Long-poll pulling work" says
	// "block_ms ranges 1-999 milliseconds; if omitted, it returns immediately without blocking". So
	// hoping to "make the long poll a bit longer to lower frequency" is a dead end: slowing down
	// when idle can only come from the idle interval.
	//
	// **That idle interval is now a step backoff** (1s → 10s → 20s → 30s → 60s), see
	// core/workerbackoff.go; the matching event wakeup is in core/workerwake.go. The old
	// arkPollIdleMin/Max here (fixed [1s,3s)) has been replaced by it.
	arkPollBlockMS = 999
	// arkBackoffCap is the cap of the backoff on poll failure.
	arkBackoffCap = 60 * time.Second
	// arkStopTimeout is the timeout for settling one work item.
	arkStopTimeout = 10 * time.Second
	// arkReclaimOlderThan is "how long without a heartbeat before another worker may grab it".
	//
	// **Without this parameter a conversation hangs forever**, not just slower: on process restart
	// (local ./dev.sh, rolling release in production) nobody takes over the in-flight work; the new
	// worker only takes new queued work, and that session stays at requires_action forever. See
	// "why reclaim is required" in pollLoop.
	//
	// ⚠️⚠️ **The value has two hard bounds, upper and lower; 45 seconds sits between them, it was
	// not picked at random.**
	//
	// Upper bound: Ark's server-side lease stop line arkWorkServerStopAfter = 90 seconds. 90
	// seconds after a work item's last heartbeat, **the server marks it stopped by itself**,
	// regardless of whether we send StopWork or whether poll carries reclaim (observed: four work
	// items all became stopped at last heartbeat +90.00x seconds; our own handler does not send
	// StopWork at all on normal finish, see the defer in handleWorkItem). stopped is final, and
	// PollWork **will never return it again**. So the reclaim threshold must leave a window of
	// "threshold passed, server not stopped yet" for a poll to hit:
	//
	//	window = 90s - 45s = 45s
	//
	// ⚠️ **The threshold used to be 90 seconds, a zero window; that incident is why this value was
	// chosen.** A rolling release killed an old pod when its work had been taken only 5 seconds
	// before. The new pod polled every second with reclaim=90s, but poll checks the timeout only
	// once, when the request starts: the request at
	// 89.9 seconds judged "not quite yet"
	// and returned empty, and by the next one the server had stopped it at 90.0 seconds, so it
	// could never be taken back. Production session sesn-20260101000012-ex012 hung forever at
	// requires_action because of this.
	//
	// Lower bound: the old worker's heartbeat self-kill line workerHeartbeatTTL = 30 seconds. If
	// heartbeats keep failing for over 30 seconds, the old worker stops by itself (markCause +
	// cancel in workerHeartbeat). The reclaim threshold must be later than that, otherwise when the
	// network just blips and the old worker is still alive and about to recover, the new worker has
	// already grabbed the work: that is running twice, two containers, two tool_results, which is
	// worse than hanging. 45s leaves 15s of margin over the self-kill line (about one heartbeat
	// interval).
	//
	// The poll frequency inside the window was checked too: in an environment with in-flight work,
	// the ladder resets to the first step (1s) every time it takes work, and the new pod that takes
	// over after the old one dies climbs again from the first step, polling several times within
	// the 45s window. Only environments with no work at all for over two minutes reach the 30s/60s
	// steps, and those have no in-flight work, so there is no need for reclaim anyway.
	arkReclaimOlderThan = 45 * time.Second
	// arkWorkServerStopAfter is the time after which Ark's server "marks active work stopped when
	// there is no heartbeat". **There is no official doc for it; the value comes from production
	// measurements**: all four samples became stopped exactly
	// 90.00x seconds after the last heartbeat (env-20260101000001-ex001).
	// We request desired_ttl=30s for heartbeats, but the server clearly clamps the lease to this
	// lower bound. It is the basis of arkReclaimOlderThan's upper bound; re-measure this value
	// before changing the threshold.
	arkWorkServerStopAfter = 90 * time.Second
)

// arkWorkerID generates the worker id reported to Ark.
//
// It includes the host name so the Ark side can tell which instance took the work; the random
// suffix makes sure several processes on the same machine (or a new process after restart) do not
// collide. A collision would make two workers reclaim each other's work.
//
// ⚠️ This uses **crypto/rand**; do not switch to math/rand to save an import: for backoff jitter,
// where "roughly random is fine", the two are the same, but a worker id collision means reclaiming
// each other, and pseudo-random **easily collides on seeds** when several processes start at once
// (rolling release). The math/rand used for jitter is in workerbackoff.go / workerlease.go.
func arkWorkerID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ama-worker-%d", time.Now().UnixNano())
	}
	suffix := hex.EncodeToString(b[:])
	if host, err := os.Hostname(); err == nil && host != "" {
		return "ama-worker-" + host + "-" + suffix
	}
	return "ama-worker-" + suffix
}

// arkFatalStatus decides "retrying is useless": the 4xx codes other than timeout, conflict and rate
// limit.
//
// The rule is copied from the SDK's isFatal4xx. 408/409/429 are excluded: they are transient and
// succeed after backoff.
func arkFatalStatus(err error) bool {
	var apiErr *selfhosted.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 &&
		apiErr.StatusCode != http.StatusRequestTimeout &&
		apiErr.StatusCode != http.StatusConflict &&
		apiErr.StatusCode != http.StatusTooManyRequests
}

// arkResolvedStatus decides "this work already has an owner", copied from the SDK's
// isResolvedStatus.
//
// These three status codes are not errors: losing the race in concurrent claiming looks like this,
// and logging them as warn would flood the logs.
func arkResolvedStatus(err error) bool {
	return selfhosted.IsStatus(err, http.StatusNotFound) ||
		selfhosted.IsStatus(err, http.StatusConflict) ||
		selfhosted.IsStatus(err, http.StatusPreconditionFailed)
}

// arkBackoff is the exponential backoff on poll failure, copied from the SDK's backoff.
func arkBackoff(n int) time.Duration {
	if n <= 0 {
		return time.Second
	}
	if n > 6 {
		return arkBackoffCap
	}
	if d := time.Duration(1<<n) * time.Second; d <= arkBackoffCap {
		return d
	}
	return arkBackoffCap
}

// arkPollIdleJitter has been replaced by the step backoff in core/workerbackoff.go.
//
// It used a fixed range of [1s,3s); now every step has its own ±20% jitter (jitterAround). The
// reason "randomizing is not a nice-to-have" carries over unchanged: N loops with a fixed interval
// would always stack requests on the same moments, and even with a fine average RPM they still
// periodically hit rate limits.

// pollWorkThroughGate passes one PollWork through the rate-limit gate.
//
// # Why PollWork also goes through the gate
//
// rateLimitedAPI used to skip PollWork on purpose, on the grounds that "it has arkBackoff, and
// stacking another layer would conflict". **That reason only holds in a single process**:
// `failures` is a local variable, one per pollLoop, while production has "number of environments ×
// number of pods" loops (often dozens), each backing off on its own and still adding up to one
// steady high-frequency stream. That is exactly the shape written in the file header of
// workerratelimit.go; it just did not count PollWork at the time.
//
// # Why this does not conflict with arkBackoff
//
// The two handle different things, and they are in series, not competing:
//   - the gate handles the **account-level** "the whole environment should not send requests now":
//     the loop that hits 429 sets the window,
//     and other calls in the same environment (including ListEvents/SendEvent) wait together;
//   - arkBackoff handles whether **this loop itself** should back off longer and longer on repeated
//     failures.
//
// The order is "wait on the gate first → send the request → feed the result back to the gate"; the
// waits of the two layers add up and do not cancel out. The real problem was never backoff being
// too short, but **polling at full speed even with no work**, which is handled by the idle interval
// above; this only makes retries after a rate limit really slow down.
func pollWorkThroughGate(
	ctx context.Context,
	api selfhosted.API,
	gate *arkRateLimitGate,
	req selfhosted.PollWorkRequest,
) (*selfhosted.WorkItem, error) {
	if gate != nil {
		if !gate.Wait(ctx) {
			return nil, ctx.Err()
		}
	}
	item, err := api.PollWork(ctx, req)
	if gate != nil {
		gate.Observe(err)
	}
	return item, err
}

// shutdown stops all workers.
//
// **It does not delete containers**: containers belong to sessions, not to this process. During a
// rolling release other instances are still using them, and the next instance to come up will keep
// using them. Idle containers are cleared by the reaper sweep based on activity time.
func (p *WorkerPool) shutdown() {
	p.mu.Lock()
	for _, cancel := range p.running {
		cancel()
	}
	p.mu.Unlock()
	p.wg.Wait()
}

// sdkLogWriter forwards the standard library log output to slog.
type sdkLogWriter struct{ log *slog.Logger }

func (w sdkLogWriter) Write(b []byte) (int, error) {
	w.log.Info(strings.TrimRight(string(b), "\n"), slog.String("from", "ark-sdk"))
	return len(b), nil
}
