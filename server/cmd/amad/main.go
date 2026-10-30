// amad is AMA's backend service process.
//
// This is the first main package in this repo. It currently wires up two business modules: "connect Feishu apps" (feishu) and
// the core business (core: workspaces / members / Agents / skills / Ark credentials / run history).
// Later modules just add a wiring section in run the same way.
//
// # Where config comes from
//
// One YAML file + env var overrides. The decisions and priority are all in shared/infra/config; this file is only responsible for
// "load once, pass down".
//
//	AMA_CONFIG=config.yaml   config file path (default config.yaml)
//	go run ./cmd/amad
//
// Running locally usually needs no env vars at all: `cp config.example.yaml config.yaml` is enough.
//
// ⚠️ **Env vars have the highest priority** and override fields of the same name in the file. This is deliberate (when production breaks you can
// change one env var and restart to recover), but the cost is "changed the file and it did not take effect". So the startup log lists, one by one,
// the field names overridden by env vars.
//
// # Identities are real
//
// Login is through local accounts or OIDC, and **the email is the user's unique ID**. All endpoints except the five /api/auth/ ones and /healthz
// require login. The credential is a random token in an HttpOnly Cookie (the database stores only its sha256).
// e2e / CI / scripts use a fixed API Key (the apiKey section; enabled only when both items are set; not set in production by default).
//
// # Data goes to PostgreSQL by default
//
// Both modules use the PG Store; connection parameters are in the pg section of the config, locally matching the database started by
// `docker compose up -d`. When core's database is empty it installs the seed data exported from the frontend's seed.ts; **when non-empty it does not overwrite**,
// so restarting does not wipe what you just created.
//
// server.store=mem falls back to the in-memory Store (gone on process restart). This opening is kept because it depends on no
// external service: when the database is not up yet, or you just want a quick look at the frontend, it is easier than changing config.
// ⚠️ It only suits a single local process: with multiple instances each instance knows only its own sessions, and logins drop randomly.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/chyroc/ama/server/apihandler"
	"github.com/chyroc/ama/server/auth"
	"github.com/chyroc/ama/server/authz"
	"github.com/chyroc/ama/server/channel"
	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/feishu"
	"github.com/chyroc/ama/server/httpd"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/repository/dao/authdao"
	"github.com/chyroc/ama/server/repository/dao/coredao"
	"github.com/chyroc/ama/server/repository/dao/coremem"
	"github.com/chyroc/ama/server/repository/dao/feishudao"
	hostsvc "github.com/chyroc/ama/server/service/host"
	userfilesvc "github.com/chyroc/ama/server/service/userfile"
	"github.com/chyroc/ama/server/shared/gopkg/xlog"
	"github.com/chyroc/ama/server/shared/infra/config"
	"github.com/chyroc/ama/server/shared/infra/cryptobox"
	"github.com/chyroc/ama/server/shared/infra/dockertools"
	"github.com/chyroc/ama/server/shared/infra/github"
	"github.com/chyroc/ama/server/shared/infra/lark"
	"github.com/chyroc/ama/server/shared/infra/oidc"
	"github.com/chyroc/ama/server/shared/infra/pg"
	"github.com/chyroc/ama/server/shared/infra/pgmq"
	"github.com/chyroc/ama/server/shared/infra/redis"
	"github.com/chyroc/ama/server/shared/infra/s3"
	"github.com/chyroc/ama/server/shared/infra/secret"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"
)

func main() {
	log := newLogger()
	slog.SetDefault(log)

	// Receive SIGINT / SIGTERM and hand them to Server.Run for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, log); err != nil {
		log.Error("服务退出", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

// newLogger builds the global logger: writes to stderr, wrapped in xlog.NewHandler,
// so every *Context log line automatically carries the request id and the fields put into ctx by xlog.With.
func newLogger() *slog.Logger {
	// xlog's unified redaction hook: every xlog message and string KV value goes through secret.Redact before being emitted.
	xlog.SetRedactor(secret.Redact)

	stderr := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(xlog.NewHandler(stderr))
}

// envOr reads an env var, giving a placeholder when empty.
//
// Only used for the few **non-sensitive** identifiers in the startup log (host name). Empty values are printed as "-" instead of omitted,
// because "this item is empty" is itself a clue.
func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// startupPhase logs one line of "which step of startup we are at".
//
// # Why every step needs a line
//
// In one past crash loop, two pods stopped at "waiting for the service to start", and **there was no way to tell where they were stuck**:
// the last log line was the init log of some SDK, followed by 226 seconds of silence,
// and then they were SIGTERM'd by the platform. What it was doing in those 226 seconds left no clue; we could only guess by reading code.
//
// With per-phase markers, **the last line tells you where it is stuck**: if the next phase was not printed, it is stuck inside that step.
// This is the only low-cost way to locate faults like "the process is alive but not ready", because it needs no reproduction.
//
// It is deliberately logged **before each step starts**, not after it finishes: the stuck step never logs "finished";
// only "started" can point to it.
func startupPhase(log *slog.Logger, n int, what string) {
	log.Info("启动阶段", slog.Int("step", n), slog.String("phase", what))
}

func run(ctx context.Context, log *slog.Logger) error {
	// First print "who I am and which port I listen on".
	//
	// A wrong port shows as "the process is fine, the health check always fails", with errors not pointing to the port at all,
	// so it must be in our own log.
	host, _ := os.Hostname()
	log.Info("amad 启动", slog.String("host", envOr("HOSTNAME", host)))

	// Config is loaded only once, then passed down.
	//
	// **This used to read the disk twice** (openStores and newFeishuSupervisor each did config.Load once),
	// and the file could change in between, giving two different versions of the config. So it is loaded in one place here.
	startupPhase(log, 1, "加载配置")
	cfg, err := loadConfig(log)
	if err != nil {
		return err
	}

	startupPhase(log, 2, "连接数据库并装配 Store")
	coreStore, feishuStore, authStore, termRelay, workerWakes, mqDB, pgDSN, closeStores, err := openStores(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer closeStores()

	// File bytes (attachments, skill files, skill packages, personal files) are all in S3-compatible object storage; PG only stores metadata.
	s3Client, blobs, err := openBlobStore(ctx, cfg, log)
	if err != nil {
		return err
	}

	// Login methods: local accounts (email + password) and generic OIDC, each turned on or off by auth.local / auth.oidc.
	//
	// ⚠️ **When neither is on, people cannot log in** (the fixed API Key and user API Keys still work). The service starts as usual:
	// refusing to start would also block deployments that "only want to run CI with an API Key", but it must be said loudly in the startup log.
	startupPhase(log, 3, "装配登录方式（本地账号 / OIDC / GitHub）")
	oidcClient := newOIDCClient(cfg, log)
	githubClient := newGitHubClient(cfg, log)

	// Core business: workspaces / members / Agents / skills / Ark credentials / run history / chats.
	//
	// **Build core before feishu**: feishu.DeleteApp relies on core to clean up dangling bindings on the agent side
	// (Config.Unbinder), so the order cannot be reversed. core does not need feishu, so there is no cycle.
	startupPhase(log, 4, "装配核心业务（core）")
	// Login and auth. **All endpoints except those five under /api/auth/ require login.**
	//
	// ⚠️ **It must be built before newIntegrationRegistry**: the AMA authorization provider uses it
	// to check the status of managed API Keys (authSvc.ManagedKeysFor(), **read-only**; that source does not support
	// revoking authorization). It only depends on authStore and oidcClient, both of which are ready at this point.
	authSvc, err := auth.New(auth.Config{
		Store: authStore,
		Local: auth.LocalConfig{
			Enabled:       cfg.Auth.Local.Enabled,
			AllowSignup:   cfg.Auth.Local.AllowSignup,
			MaxFailures:   cfg.Auth.Local.MaxFailures,
			FailureWindow: time.Duration(cfg.Auth.Local.FailureWindowSeconds) * time.Second,
		},
		OIDC:                     oidcClient,
		OIDCDisplayName:          cfg.Auth.OIDC.DisplayName,
		OIDCAllowUnverifiedEmail: cfg.Auth.OIDC.AllowUnverifiedEmail,
		GitHub: auth.GitHubConfig{
			Client:              githubClient,
			DisplayName:         cfg.Auth.GitHub.DisplayName,
			AllowedUsers:        cfg.Auth.GitHub.AllowedUsers,
			AllowedEmailDomains: cfg.Auth.GitHub.AllowedEmailDomains,
			AllowedOrgs:         cfg.Auth.GitHub.AllowedOrgs,
		},
		Admins: cfg.Auth.Admins,
		// Fixed API Key channel: for e2e / CI / scripts / probes (they have no browser).
		// **Enabled only when both key and email are set**; not set in production by default, so it does not exist there.
		APIKey: auth.APIKey{
			Key:   cfg.APIKey.Key,
			Email: cfg.APIKey.Email,
			Name:  cfg.APIKey.Name,
		},
		// The derivation secret of platform-managed API Keys, for the `ama` command inside containers.
		// Empty means the feature is unavailable (the service starts as usual); see auth/managedkey.go.
		ManagedKeySecret: cfg.Auth.ManagedKeySecret,
		Logger:           log,
	})
	if err != nil {
		return err
	}
	logLoginMethods(cfg, authSvc, log)
	if authSvc.APIKeyEnabled() {
		// Say it out loud: it is a permanent key that bypasses login; configured in the wrong place it is a backdoor.
		log.Warn("已启用固定 API Key 通道，请确认这是预期的（线上应只给 CI/脚本用）",
			slog.String("email", cfg.APIKey.Email))
	}
	// This line is the **only** way for ops to know "can the ama command in containers work":
	// the two modes are exactly the same in service features; the only difference is whether that variable is injected into the container.
	if !authSvc.ManagedKeyEnabled() {
		log.Warn("未配置 auth.managedKeySecret：勾了「ama」内置技能的 agent " +
			"在容器里跑 ama 会失败（报「没有凭据」）。生成一把：openssl rand -base64 32")
	}

	// Origin allowlist for the terminal WebSocket.
	//
	// ⚠️ **This line is the only startup-side clue for ops to know "does the terminal use WS or the fallback".**
	// Missing this item gives zero errors: the service starts as usual, the terminal still works, only the browser cannot connect over WS
	// and silently falls back to SSE+POST, where echo goes out of order when typing fast, one RTT per character,
	// i.e. exactly the two bugs reported back then come back unchanged.
	//
	// The check is written as "is it configured" rather than "does the object exist": the latter is always true (it is a slice field),
	// and would make the log say "wired up" while the feature is actually unusable (the repo's rule "do not trust logs that only check
	// the object is non-nil" records this; it was learned the hard way).
	if len(cfg.Server.TerminalWSOrigins) == 0 {
		log.Warn("未配置 server.terminalWSOrigins：终端的 WebSocket 通道对浏览器不可用" +
			"（CheckOrigin 会拒绝一切带 Origin 的连接），前端将静默回退到 SSE+POST——" +
			"那条路连打时回显可能乱序、每字符一个往返。" +
			"线上填 https://ama.example.com；本地填 http://127.0.0.1:<dev.sh 的 PORT>")
	} else {
		log.Info("终端 WebSocket 已启用",
			slog.Any("allowed_origins", cfg.Server.TerminalWSOrigins))
	}

	// Host executor domain service: must be built before core.New, since agent domain save validation gets it through
	// core.Config.HostBindings. hostWakes is this process's NOTIFY fan-out registry.
	hostWakes := core.NewHostWakes()
	hostSvc, err := hostsvc.New(hostsvc.Config{
		Store: coreStore,
		Wait:  hostWakes,
		// The Sink (terminal upstream relay) is set with SetTermSink after coreSvc is built.
		Log: log,
	})
	if err != nil {
		return err
	}

	// Authorization management: one provider registry. Sources with missing config are still **registered**, only showing status
	// missing: the page must be able to show "not integrated" with configuration guidance, instead of making that card disappear entirely.
	//
	// This registry does not depend on core, so building it early has no side effects.
	integrations := newIntegrationRegistry(authSvc, log)

	coreSvc, err := core.New(core.Config{
		Store: coreStore,
		// When saving an agent with the "host executor", validate host binding (host domain service).
		HostBindings: hostSvc,
		// Really calls Ark: validating a credential lists Agents once with the key, and judges invalid by 401/403.
		ArkVerifier: core.NewLiveArkVerifier(),
		// The chat path also calls real Ark: creates the self_hosted environment + Agent + Session, and relays the event stream.
		// It uses **each workspace's own configured key**, not a process-level env var.
		ArkRunner: core.NewLiveArkRunner(),
		// Who counts as "the dev team", for permission checks of the feedback read endpoint.
		//
		// ⚠️ **Not passing it means nobody can read** (core.ListFeedback rejects everything for nil).
		// What is passed here is the value cfg.Feedback itself: it has a CanRead method, which exactly satisfies
		// the core.FeedbackReaders interface, so no extra adapter is needed.
		FeedbackReaders: cfg.Feedback,
		// "Auto-fetch Ark API Keys from the Volcengine account": not wired in the open-source version (it relies on a personal account authorization
		// that was removed, to get a person token); when nil users paste the key by hand; see the top of core/arkfetch.go.
		ArkKeyFetcher: nil,
		// Where the bytes of attachments and skill files go: S3-compatible object storage (in the mem tier without config, in process memory).
		// See openBlobStore and shared/infra/s3.
		BlobStore: blobs,
		// Cross-instance relay of terminal keystrokes. **May be nil** (with the in-memory Store); then cross-instance keystrokes
		// return 404, i.e. the behavior before the fix; see the file header of core/sandboxterm.go.
		TerminalRelay: termRelay,
		// Wake up the worker poll loop of that environment when a message is sent. **It must cross pods**: after leader election an
		// environment has only one poll loop globally, while this HTTP request lands randomly through the load balancer,
		// usually not on that pod. When nil (in-memory Store) it degrades to relying only on the backoff timer.
		WorkerWakeups: workerWakes,
		// Allowlist of page origins allowed to open the terminal WebSocket.
		//
		// ⚠️ **When empty the terminal WS rejects every connection with an Origin** (fail-closed),
		// so the browser silently falls back to SSE+POST: that path works, but echo goes out of order when typing fast,
		// one RTT per character. In other words, missing this item **reports no error; those two bugs just come back unchanged**.
		// The check is whether the terminal status bar has a "compatibility mode" badge. See config.Server.TerminalWSOrigins.
		TerminalWSOrigins: cfg.Server.TerminalWSOrigins,
		Logger:            log,
	})
	if err != nil {
		return err
	}

	// "Who created this agent" needs an avatar and name, and the profiles are in auth's users table.
	//
	// A setter instead of a Config field: core does not import authdao (that would tie the six resource domains to the
	// auth storage), so it goes through the UserProfileLookup interface declared on the core side + one adapter layer here.
	// Same precedent as DepartmentLookup: "declare the interface on the consumer side".
	//
	// **With the in-memory store authStore is also non-nil** (MemStore implements the same interfaces),
	// so this is wired unconditionally; the ones who really "have no profile" are people who never logged in,
	// and then ListUsers does not return them, and the frontend falls back to initials.
	coreSvc.SetUserProfileLookup(userProfileLookup{store: authStore})
	// Error messages need the display names of authorization sources; the same registry instance is passed (the Handler uses it too).
	coreSvc.SetIntegrationRegistry(integrations)

	// The authorization layer: judges "can this person do this" by workspace member role.
	//
	// **It feeds two places at once**: httpd's Guard (blocking all HTTP business routes), and feishu's
	// Authorizer (the few requireManage calls inside feishu.Service, guaranteeing the order "authorize first, then look up the session").
	// Both share the same role checks, otherwise "one layer allows, the other rejects" would happen,
	// and the error could not point to the real cause.
	membership := authzMembership{core: coreSvc, feishu: feishuStore}
	startupPhase(log, 5, "装配授权矩阵（authz）")
	guard, err := authz.New(authz.Config{Membership: membership, Logger: log})
	if err != nil {
		return err
	}

	startupPhase(log, 6, "装配飞书模块")
	svc, err := feishu.New(feishu.Config{
		Store: feishuStore,
		// The real authorizer. **Leaving it empty rejects everything** (denyAllAuthorizer), so it must be passed here;
		// the old AllowAllAuthorizer has been removed.
		Authorizer: authz.NewFeishuAuthorizer(membership),
		//
		// Unbinder makes "disconnect app" also clear feishuAppIds on agents. Without it, dangling
		// bindings would remain: the app is gone, but the agent detail page still shows it bound to an ID that cannot be found.
		// ⚠️ **Connected directly to the agent domain package, not through the forwarders on core.** Those forwarding methods were a
		// stopgap from when "the orchestration layer was not yet connected to the domain packages"; step 14 already connected it. The real implementations of these two methods
		// (UnbindFeishuApp / AgentsBoundToFeishuApp) were always in service/agent.
		Unbinder: coreSvc.Agents(),
	})
	if err != nil {
		return err
	}

	// Feishu user authorization: registered as the last card.
	//
	// ⚠️ **It can only be registered here, not moved into newIntegrationRegistry**: it depends on coreSvc
	// (to look up "which workspaces this person is in") and feishu's svc (to look up that workspace's app and secret),
	// and both are built after the registry (the registry must be built before core.New and passed in).
	// Forcing it earlier would need a lazy resolution layer,
	// which would turn "is it installed" into something only known at runtime.
	//
	// The registration order is the card order, so it is the last card: it is also the only source that needs the user to **go to Feishu themselves
	// and click agree once**, so putting it last does not interrupt the earlier "configure once and done" cards.
	larkUserProvider := core.NewLarkUserProvider(
		coreStore, feishu.NewUserAppResolver(svc, coreSvc.Workspaces()), newLarkOAuthClient(cfg, log),
		// owners is only used to say clearly on the card "whose authorization this workspace's agents actually use".
		// **It must come from the same source as the injection path** (the same firstOwnerEmail), otherwise the UI says A is in effect
		// while runtime uses B, with zero errors.
		//
		// ⚠️ Connecting directly to the workspace domain package is exactly the shortest path to "same source": the
		// firstOwnerEmail in the injection path is workspacesvc.FirstOwnerEmail (see core/workspace.go),
		// and WorkspaceOwnerOf on core was only a forwarder to it. Step 14 removed that forwarding layer.
		coreSvc.Workspaces(), log)
	integrations.Register(larkUserProvider)

	// Build the orchestration layer. **It is now really on the request path** (step 15 of the package split): apihandler below
	// takes narrow interfaces of ten domains from it, 133 methods in total, instead of the 254 of coreSvc.
	//
	// ⚠️ orchestrator.New checks one by one that the eight domain packages are non-nil, and builds the workspace composite
	// on the spot. A missing one shows as "a panic only when a handler calls into that domain", a fault on the request
	// path, whose error does not point to the wiring. Built here and reported here, the error message names the domain directly.
	orch, err := newOrchestrator(coreSvc)
	if err != nil {
		return err
	}

	// Upstream terminal frames of hosts must be delivered to the instance holding the browser terminal connection; core.Service owns that
	// terminalHub + pgmq relay, so it is adapted into the sink the host domain needs and injected afterwards
	// (hostSvc must be built before core.New, so the sink goes through a setter).
	hostSvc.SetTermSink(hostTermSink{svc: coreSvc})
	// Scrub secrets before storing exec results (the same credential list as webshell).
	hostSvc.SetResultScrubber(newHostResultScrubber(coreSvc))
	hostSvc.SetToolResultScrubber(newHostToolResultScrubber(coreSvc))
	// Sending frames of the host web terminal (host.Service → host_dispatches), and also routing
	// terminal WS with a non-empty run.host_id through the host path.
	coreSvc.SetHostTerminalDispatcher(hostSvc)

	// Account-level personal files domain: metadata in coreStore (PG/mem), bytes in object storage (multipart upload +
	// time-limited download links). s3Client is nil only in the mem tier without object storage configured; then userFileSvc
	// stays nil, and that group of routes is still registered, returning 503 when called (userFilesOr503):
	// "no object storage connected" is a deployment state and should not show up as the endpoint not existing (404).
	var userFileSvc *userfilesvc.Service
	if s3Client != nil {
		userFileSvc, err = userfilesvc.New(userfilesvc.Config{
			Store:   coreStore,
			Objects: &userFileObjects{client: s3Client},
			Log:     log,
		})
		if err != nil {
			return err
		}
	} else {
		log.Warn("没配对象存储（s3.bucket），个人文件接口 /api/files 回 503")
	}

	coreHandler, err := apihandler.NewHandler(apihandler.HandlerConfig{
		// ⚠️ **What is passed is orch, not coreSvc.** This line is where the rule "handlers only know
		// the orchestration layer" lands: to let handlers see one more method, adding it in core
		// is not enough; a line must be added explicitly in orchestrator/<domain>.go.
		//
		// It takes the whole orchestration layer instead of ten domain fields, so the wiring point cannot get it wrong:
		// mixing up two of ten lines of `Issue: orch.Issue()` gives zero errors; see HandlerConfig.
		Orchestrator: orch,
		Logger:       log,
		Integrations: integrations,
		Hosts:        hostSvc,
		HostDaemon:   hostSvc,
		// When no object storage is configured (only possible in the mem tier) it is nil, and file endpoints return 503.
		UserFiles: userFileSvc,
		DevRoutes: cfg.Server.DevRoutes,
	})
	if err != nil {
		return err
	}

	// The feedback read allowlist must also be stated, in both directions:
	//
	//   * When the list is empty nobody can read GET /api/feedback (fail-closed). Without saying so, it shows as
	//     "even the dev team gets 403", while the error only says "only the dev team can view": it looks like
	//     a code bug, while actually the list is not configured.
	//   * When configured, print the count and the list. This is not a credential (it is a list of emails), so it can go into the log,
	//     and "who can read error reports from users across the whole site" deserves a trace in the startup log.
	if len(cfg.Feedback.Readers) == 0 {
		log.Warn("未配置问题反馈读取白名单（feedback.readers）—— " +
			"上报照常工作，但 GET /api/feedback 谁都读不到（含你自己）。" +
			"要读就把邮箱加进配置，或设 AMA_FEEDBACK_READERS=a@example.com,b@example.com")
	} else {
		log.Info("问题反馈读取白名单已配置",
			slog.Int("count", len(cfg.Feedback.Readers)),
			slog.Any("readers", cfg.Feedback.Readers))
	}
	// The Cookie's Secure follows the deployment form: locally it is http://, and with Secure the browser would not store it at all,
	// showing as "login succeeds but a refresh asks to log in again", with no error.
	authHandler, err := auth.NewHandler(auth.HandlerConfig{
		Service:      authSvc,
		SecureCookie: cfg.Server.SecureCookie(),
		// OIDC's redirect_uri is built from it. **When empty, only requests from this machine can use OIDC login**
		// (selfurl.Callback only falls back for loopback addresses), so missing it in production means nobody can log in:
		// the Warn below is the only clue for ops to know this in advance.
		PublicURL: cfg.Server.PublicURL,
		Logger:    log,
	})
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.Server.PublicURL) == "" {
		// ⚠️ The check is "is it configured", not "does the object exist": wiring always succeeds, and missing config only blows up when a user
		// really clicks login, and then the error is in the browser, not in the startup log.
		log.Warn("未配置 server.publicURL：OIDC 回调地址只能按请求推导，" +
			"而那**只对本机请求生效**——线上开了 OIDC 就必须配，否则点 SSO 登录必失败。" +
			"顺带它也决定 Cookie 要不要带 Secure。")
	}

	startupPhase(log, 7, "装配 HTTP 路由")
	srv, err := httpd.New(httpd.Config{
		Addr:          cfg.Server.AddrOrDefault(),
		FeishuService: svc,
		CoreHandler:   coreHandler,
		AuthHandler:   authHandler,
		Authz:         guard,
		AllowedOrigin: cfg.Server.AllowedOrigin,
		Logger:        log,
	})
	if err != nil {
		return err
	}

	// self_hosted worker: permanently polls Ark's work queue and sends tool execution to Docker containers (or hosts).
	//
	// **It is not an optional ops component; it is part of the chat feature.** Chats run in the self_hosted environment,
	// where Ark only does model inference; tool calls rely on this worker to pick them up, run them, and return results. Without it,
	// a session stops at requires_action as soon as the agent calls a tool and never moves again: users see "the reply stopped halfway"
	// with no error at all. So when the config is incomplete, this must **say the consequence loudly**, instead of skipping silently.
	startupPhase(log, 8, "装配 self_hosted worker 池")
	// A wrong docker.memory **fails startup** instead of silently not limiting memory: it is a resource limit,
	// and "thought it was limited but it was not" is harder to notice than failing to start.
	dockerMemory, err := cfg.Docker.MemoryBytes()
	if err != nil {
		return err
	}
	workerPool := core.NewWorkerPool(coreSvc, core.WorkerConfig{
		// Docker executor: one container per Ark session (enabled by default). See shared/infra/dockertools.
		Docker: dockerExecConfig(cfg, dockerMemory),
		// How many works each environment handles concurrently. **It must be passed**: the zero value of core.WorkerConfig is 0,
		// which is treated as 1 (serial); then a second person sending a message in the same workspace must wait until the first one is idle for MaxIdle
		// (60 seconds), with zero errors. See pollLoop in core/worker.go.
		Concurrency: cfg.Worker.ConcurrencyOrDefault(),
		// AMA's own credential source: makes the `ama` command preinstalled in containers usable.
		//
		// It uses auth.Service's deterministic derivation (auth/managedkey.go): the plaintext is not stored,
		// it is computed fresh each time, so it can be called repeatedly on this hot path. When nil those three variables are not injected.
		ManagedKeys: authSvc,
		// The Feishu user authorization provider, used to inject lark-cli's two variables.
		//
		// ⚠️ **This line must not be missed**: without it, the whole addLarkUserEnvs part in core/sessionenv.go
		// is dead code: agents with "Feishu user authorization" checked still get no credentials,
		// and lark-cli reports `not_configured`, a message pointing to the CLI's own config,
		// not to "the server did not connect the provider to the worker". Zero errors, zero tests turning red.
		// Same as the dead-code-passes-all-checks lesson.
		//
		// It can be assigned directly here: its static type is the concrete pointer *core.LarkUserProvider,
		// and a nil pointer compares equal to nil, so there is no "nil interface value is not nil" trap.
		LarkUsers: larkUserProvider,
		// The public address, injected as AMA_BASE_URL. ⚠️ Locally server.publicURL must be configured,
		// otherwise the ama in containers would operate on **production** data (the CLI's default is the production address).
		PublicBaseURL: cfg.Server.PublicURL,
		// Cross-pod wake-up signal: the pod that sends a message wakes up the pod holding the poll right.
		//
		// **Both sides must be installed**: this is the **subscriber side** (the worker receives signals); the sender side is in
		// core.Config.WorkerWakeups (the HTTP side sends signals). With only one side installed,
		// the whole wake-up path is dead, and the only symptom is "slow responses", with zero errors.
		// When nil (in-memory Store) it degrades to relying only on the stepped backoff timer.
		Wakeups: workerWakes,
		// The tool result ledger uses PG (cross-instance, isolated per session, not lost on pod rebuild). **Do not use the SDK's
		// default worker-local file ledger**: multiple works sharing a local directory would let one tool result that failed to return
		// pollute all other sessions after it (400 InvalidPayload crossing sessions); see
		// the core/selfhosted ledger and repository/schema/selfhost_tool_results.sql.
		ToolResults: coreStore,
		// Host executor: tool calls go through hostSvc into the host_dispatches queue, picked up by the ama daemon on the user's machine
		// with long polling. With it configured, the worker starts even without Docker configured.
		HostExec: hostSvc,
	}, log)

	// Feishu long connections: open one websocket for each connected app; @-mentioning the bot in a group drives the agent.
	//
	// **No callback address needed**: a long connection only needs AppID + AppSecret, and events are pushed down that connection.
	// But it needs Redis: a Feishu app allows only one long connection, and multiple instances connecting directly would evict each other **without errors**,
	// showing as inbound messages randomly lost, so before connecting we must grab a lock, and not connect if we cannot.
	// Without Redis configured the service starts as usual, it just receives no Feishu messages: say so clearly here, so nobody spends ages
	// guessing at "the bot ignores me".
	startupPhase(log, 9, "连接 Redis 并装配飞书长连接")
	feishuSupervisor, rdb, closeRedis, err := newFeishuSupervisor(ctx, cfg, svc, feishuStore, coreSvc, log)
	if err != nil {
		return err
	}
	defer closeRedis()

	// HTTP, worker and Feishu long connections run in parallel; if any one exits, all are wound down.
	// errgroup cancels ctx at the first non-nil error, so the others exit gracefully along with it.
	//
	// **This line marks "all wiring is done"**: after it there is no more blocking initialization,
	// so seeing it means the stuck point is not in startup (go check the health check/port); not seeing it means
	// it is stuck in some startupPhase above, and the last phase is the stuck point.
	startupPhase(log, 10, "启动各常驻 goroutine（HTTP / worker / 飞书长连接）")
	g, runCtx := errgroup.WithContext(ctx)
	g.Go(func() error { return srv.Run(runCtx) })
	if workerPool != nil {
		// Install poll leader election: **only one pod polls an environment at a time**,
		// saving N-1 copies of idle RPM (measured in production: 5 pods × 7 environments = 35 loops, ~1050 RPM all idle).
		//
		// ⚠️ **It must be before Run**; see the comment on SetPollLeases (installing after Run would leave
		// the workers already started using the old config, with zero errors).
		//
		// When rdb is nil (no Redis configured), pass nil: then it falls back to "every pod polls",
		// fully correct, just wasting quota: this layer is a degrade-open optimization, not correctness.
		workerPool.SetPollLeases(schedulerLeases(rdb))
		if rdb == nil {
			log.Warn("未配置 Redis，worker poll 不做选主 —— " +
				"每个实例都会 poll 同一个环境，方舟账号 RPM 是单实例的 N 倍" +
				"（功能正常，重复领取由方舟的 AckWork 竞争裁决）")
		}
		// Leader lock for the Docker reclaim sweep: only one instance scans the same daemon. When rdb is nil it falls back to
		// "every instance scans" (deletion is idempotent, the result is correct). **It also must be installed before Run.**
		workerPool.SetReaperLeases(schedulerLeases(rdb))
		g.Go(func() error { return workerPool.Run(runCtx) })
	} else {
		log.Warn("Docker 执行端与宿主机执行端都没启用，self_hosted worker 不启动 —— " +
			"对话可以发消息，但 agent 一调工具就会卡在 requires_action。" +
			"检查配置里的 docker.enabled")
	}
	// Scheduled task scheduler: permanently scans for tasks that are due, drives the agent to run a turn, and delivers the result back to where it came from.
	//
	// **It and the worker are two different things**: the worker makes "the agent can call tools", the scheduler makes "it works even when nobody asks".
	// Without it, not a single scheduled task configured in the UI fires, **and no error is reported**, so the warning below
	// must state the consequence (same precedent as the workerPool part).
	scheduler := core.NewScheduler(core.SchedulerConfig{
		Service: coreSvc,
		// Delivery capability. Without it, tasks configured to "send to a Feishu group" only land in run history (the scheduler warns).
		Deliverer: scheduleDeliverer{store: feishuStore},
		// Leader election is **an optimization, not correctness**: scan only after grabbing the lock, saving useless scans on N-1 instances.
		// Without Redis configured pass nil; then every instance scans, and PG's atomic claiming guarantees no duplicate firing.
		Leases: schedulerLeases(rdb),
		Logger: log,
	})
	if scheduler != nil {
		g.Go(func() error { return scheduler.Run(runCtx) })
		if rdb == nil {
			log.Warn("未配置 Redis，定时任务调度器不做选主 —— " +
				"每个实例都会扫一遍，重复触发由 PostgreSQL 的原子认领兜住" +
				"（功能正常，只是多几次无用查询）")
		}
	}

	// Agent Ark sync executor: brings the agent up to date on the Ark side right after it is saved.
	//
	// **Without it, clicking save after changing the prompt only writes the database and never touches Ark**; users would wait until the next time someone opens a new chat
	// for it to take effect, and that reports no error, showing as "I changed it but it acts as if I did not". So the warning below
	// must also state the consequence (same precedent as the scheduler / workerPool parts).
	//
	// Leader election is **an optimization, not correctness**: correctness rests on PG's atomic claiming (ClaimDueAgentSyncTasks);
	// without Redis configured every instance scans, and claiming guarantees exactly one wins.
	agentSyncer := core.NewAgentSyncer(core.AgentSyncerConfig{
		Service: coreSvc,
		Leases:  schedulerLeases(rdb),
		Logger:  log,
	})
	if agentSyncer != nil {
		g.Go(func() error { return agentSyncer.Run(runCtx) })
		if rdb == nil {
			log.Warn("未配置 Redis，agent 方舟同步执行器不做选主 —— " +
				"每个实例都会扫一遍，重复同步由 PostgreSQL 的原子认领兜住" +
				"（功能正常，只是多几次无用查询）")
		}
	}

	// Permanent session subscriptions: attach a subscription to sessions "still running but watched by nobody", storing events into history.
	//
	// **Without it, the reply of a turn where the user "sent a message and closed the page" never goes into history**: after refreshing they only see
	// their own message, while the agent did answer. See the file header of core/runwatcher.go.
	//
	// It shares the same leader election adapter as scheduler (a different key); without Redis configured pass nil:
	// then every instance scans, and duplicates are covered by PG claiming.
	// Host offline sweep: machines without a heartbeat for 90s are set offline, their in-flight exec calls are failed, and waiters are woken.
	hostWatcher := core.NewHostWatcher(hostSvc, hostWakes, schedulerLeases(rdb), log)
	g.Go(func() error { return hostWatcher.Run(runCtx) })

	// PG LISTEN wake-up loop: fans out ama_host_wake notifications to this instance's heartbeat/exec waiters.
	// Only started in the PG tier (the mem tier has no DSN; then plain tickers are the fallback, and a single process needs no cross-instance wake-up).
	if pgDSN != "" {
		listenPool, lerr := pgxpool.New(runCtx, pgDSN)
		if lerr != nil {
			return lerr
		}
		g.Go(func() error {
			runHostWakeListener(runCtx, listenPool, hostWakes, log)
			listenPool.Close()
			return nil
		})
	}

	runWatcher := core.NewRunWatcher(core.RunWatcherConfig{
		Service: coreSvc,
		Leases:  schedulerLeases(rdb),
		Logger:  log,
	})
	if runWatcher != nil {
		g.Go(func() error { return runWatcher.Run(runCtx) })
	} else {
		// The same convention as the workerPool / scheduler parts: **if it does not start, state the consequence**,
		// otherwise the gap "sessions nobody watches do not store events" comes back with nothing in the log.
		log.Warn("未接入方舟，会话常驻订阅不启动 —— " +
			"没人打开对话页时，agent 的回复不会进运行历史")
	}

	// Expiry cleanup of mq_messages.
	//
	// **It cleans the "sent but consumed by nobody" kind**: deleting on successful consumption already reclaims the vast majority,
	// and what is left are ones the pod holding the PTY did not pick up within the TTL (the pod was rebuilt, or the user closed the terminal).
	// Without cleanup they stay in the table forever, making mq_messages_due_idx only grow.
	//
	// ⚠️ In the mem tier mqDB is nil (no such table), NewPurger returns nil, and it does not start here.
	if purger := pgmq.NewPurger(pgmq.PurgerConfig{
		DB: mqDB,
		// Leader election is **an optimization, not correctness**: delete is idempotent, and several instances cleaning at once only waste work.
		// Without Redis configured pass nil; then every instance cleans.
		Leases: purgeLeases(rdb),
		Log:    log,
	}); purger != nil {
		g.Go(func() error { return purger.Run(runCtx) })
	}

	// Cleanup of excess history versions.
	//
	// Each entity keeps only the latest apimodel.MaxRevisionsPerEntity entries, scanned once an hour.
	// **Not deleted synchronously on the write path**: that would add latency to every save, and "delete everything after the 21st"
	// is itself a race with multiple instances. For the full reasoning see the file header of core/revisionpurge.go.
	//
	// ⚠️ What is passed here is the schedulerLeases adapter (core.Leases), not purgeLeases
	// (pgmq.Leases): the two have the same shape but different types; see the comment on purgeLeases.
	// The lock key is separate on the core side, so it does not starve the scheduler or vice versa.
	if rp := core.NewRevisionPurger(core.RevisionPurgerConfig{
		Service: coreSvc,
		Leases:  schedulerLeases(rdb),
		Logger:  log,
	}); rp != nil {
		g.Go(func() error { return rp.Run(runCtx) })
	}

	if feishuSupervisor != nil {
		g.Go(func() error { return feishuSupervisor.Run(runCtx) })
	}
	return g.Wait()
}

// feishuConversations adapts *core.Service to feishu.Conversations.
//
// The two modules are deliberately coupled only by an ID string (see the comment in core/agent.go), so feishu does not import
// core, and only declares the few methods it needs. The only mismatch is the return type of CreateChannelConversation
// (core gives apimodel.Run, a channel only wants a session ID), converted here.
type feishuConversations struct{ svc *core.Service }

// CreateChannelConversation marks the run with the channel as its trigger; channel.Kind values equal
// the channel members of apimodel.RunTrigger (see apimodel.RunTrigger.IsChannel).
func (c feishuConversations) CreateChannelConversation(
	ctx context.Context, workspaceID, agentID string, kind channel.Kind, detail string,
) (feishu.Run, error) {
	run, err := c.svc.CreateChannelConversation(ctx, workspaceID, agentID, apimodel.RunTrigger(kind), detail)
	if err != nil {
		return feishu.Run{}, err
	}
	return feishu.Run{ID: run.ID}, nil
}

func (c feishuConversations) AppendRunEvents(
	ctx context.Context, runID string, raw []json.RawMessage,
) ([]json.RawMessage, error) {
	return c.svc.AppendRunEvents(ctx, runID, raw)
}

func (c feishuConversations) StreamRunEvents(
	ctx context.Context, runID string, onFrame func(raw []byte) error,
) error {
	return c.svc.StreamRunEvents(ctx, runID, onFrame)
}

func (c feishuConversations) AgentsBoundToFeishuApp(
	ctx context.Context, feishuAppID string,
) ([]string, error) {
	return c.svc.Agents().AgentsBoundToFeishuApp(ctx, feishuAppID)
}

func (c feishuConversations) AgentDisabled(ctx context.Context, agentID string) bool {
	return c.svc.Agents().AgentDisabled(ctx, agentID)
}

// AgentFollowUpQueues flattens apimodel.FollowUpMode into a bool.
//
// The feishu side deliberately does not know core's domain models (see the comment on feishu.Conversations),
// so the enum degrades here into "queue or not".
//
// **Not found returns false (steer)**, matching the default side of apimodel.FollowUpMode: that enum's
// empty string means steer. Failing open to the same side means a database hiccup does not flip this agent's behavior.
func (c feishuConversations) AgentFollowUpQueues(ctx context.Context, agentID string) bool {
	agent, err := c.svc.GetAgent(ctx, agentID)
	if err != nil {
		return false
	}
	return apimodel.NormalizeFollowUpMode(agent.FollowUp) == apimodel.FollowUpQueue
}

// AgentShowsProgress flattens apimodel.ProgressDetail into a bool.
//
// Same as AgentFollowUpQueues: the feishu side deliberately does not know core's domain models, so the enum degrades here into
// "show the chain of thought or not".
//
// **Not found returns true (show)**, matching the default side of apimodel.ProgressDetail: that enum's
// empty string means "show". Note the fail-open direction here is the opposite of AgentFollowUpQueues
// (which returns false), because the two have their "default level" on different sides; the rule is **fall back to each one's own
// default**, not "both return false". Getting the direction wrong would make a database hiccup wipe this agent's
// chain of thought for a whole turn, with zero errors on the user side.
func (c feishuConversations) AgentShowsProgress(ctx context.Context, agentID string) bool {
	agent, err := c.svc.GetAgent(ctx, agentID)
	if err != nil {
		return true
	}
	return apimodel.NormalizeProgressDetail(agent.ProgressDetail) != apimodel.ProgressDetailBrief
}

// InterruptRun only sends the interrupt, no new message. The new message of steering is sent by the feishu side through the normal path
// after it gets the lock; for the reason see the comment on core.Service.InterruptRun.
func (c feishuConversations) InterruptRun(ctx context.Context, runID string) error {
	return c.svc.InterruptRun(ctx, runID)
}

func (c feishuConversations) SyncRunEvents(ctx context.Context, runID string) error {
	return c.svc.SyncRunEvents(ctx, runID)
}

// RunStatus flattens apimodel.RunStatus into a bare string.
//
// The feishu side deliberately does not know core's domain models (see the comment on feishu.Conversations),
// so the type degrades here into string, and the value range is pinned by comments on both sides.
func (c feishuConversations) RunStatus(ctx context.Context, runID string) (string, error) {
	run, err := c.svc.GetRun(ctx, runID)
	if err != nil {
		return "", err
	}
	return string(run.Status), nil
}

// authzMembership adapts core.Service and feishu.Store together into authz.Membership.
//
// authz needs two things: looking up someone's role in some workspace, and resolving a resource id to its owning workspace.
// The former and the resolution of agent/skill/run/schedule are in core, and Feishu app resolution is in feishu:
// one interface spans two modules, so the adapter lives in the process layer (the same reason as feishuConversations:
// the two business modules are deliberately coupled only by ID strings, and neither imports the other).
type authzMembership struct {
	core   *core.Service
	feishu feishu.Store
}

func (m authzMembership) RoleOf(
	ctx context.Context, workspaceID, actor string,
) (apimodel.WorkspaceRole, bool, error) {
	return m.core.RoleOf(ctx, workspaceID, actor)
}

// WorkspaceOf maps authz's resource kinds to the queries of the two modules.
//
// The two packages each define their own ResourceKind enum (core.KindAgent / authz.KindAgent),
// deliberately not shared: core cannot import authz (authz needs core.Service as its Membership implementation,
// which would be a cycle). The enums are small, and duplicating one is cheaper than introducing a cycle. A wrong mapping would apply authorization to the wrong resource,
// so each one is written out explicitly here, without int conversion.
func (m authzMembership) WorkspaceOf(
	ctx context.Context, kind authz.ResourceKind, id string,
) (string, error) {
	var coreKind core.ResourceKind
	switch kind {
	case authz.KindAgent:
		coreKind = core.KindAgent
	case authz.KindSkill:
		coreKind = core.KindSkill
	case authz.KindRun:
		coreKind = core.KindRun
	case authz.KindSchedule:
		coreKind = core.KindSchedule
	case authz.KindIssue:
		coreKind = core.KindIssue
	case authz.KindIssueComment:
		coreKind = core.KindIssueComment
	case authz.KindIssueAttachment:
		// ⚠️ This branch governs "who can see images in someone else's workspace". A wrong mapping (e.g. written as KindIssue)
		// would look up the issues table with an attachment id, resolution always fails → every image 404s.
		// That symptom looks like "the image feature is broken", not like a wrong mapping.
		coreKind = core.KindIssueAttachment
	case authz.KindFeishuApp:
		// Ownership of Feishu apps is on the feishu side.
		app, err := m.feishu.GetApp(ctx, id)
		if err != nil {
			if errors.Is(err, feishu.ErrAppNotFound) {
				return "", fmt.Errorf("%w: 飞书应用不存在", authz.ErrResourceNotFound)
			}
			return "", err
		}
		return app.WorkspaceID, nil
	default:
		return "", fmt.Errorf("%w: 未知的资源类型", authz.ErrResourceNotFound)
	}

	ws, err := m.core.WorkspaceOf(ctx, coreKind, id)
	if err != nil {
		// core uses its own ErrNotFound to express "resource does not exist", while authz recognizes
		// ErrResourceNotFound. If the translation were lost, "resource deleted" would be treated as a database failure and return 500,
		// while the frontend's six nullable getters expect 404 → null.
		if errors.Is(err, core.ErrNotFound) {
			return "", fmt.Errorf("%w: %s", authz.ErrResourceNotFound, err.Error())
		}
		// Platform-level resources (built-in skills) must be translated likewise, otherwise they would be treated as a database failure and return 500:
		// then no workspace could open built-in skills, and the error would point to "internal server error".
		if errors.Is(err, core.ErrPlatformResource) {
			return "", fmt.Errorf("%w: %s", authz.ErrPlatformResource, err.Error())
		}
		return "", err
	}
	return ws, nil
}

// larkLeases adapts shared/infra/redis to lark.LeaseStore.//
// shared/infra/lark should not depend back on shared/infra/redis (it is the Feishu integration layer, not the infrastructure assembly layer),
// so it only defines the interface, and the process layer connects it.
type larkLeases struct{ client *redis.Client }

func (l larkLeases) Acquire(
	ctx context.Context, key string, ttl time.Duration,
) (lark.Lease, bool, error) {
	lease, ok, err := l.client.Acquire(ctx, key, ttl)
	if err != nil || !ok {
		return nil, ok, err
	}
	return lease, true, nil
}

// newFeishuSupervisor wires up the Feishu long connections, and hands out the Redis client too.
//
// **rdb is returned so the scheduler can reuse the same connection pool**: both need leader election (the long connection grabs "one connection
// per app", the scheduler grabs "only one instance scans per tick"); building one client each would double the number of connections and the config
// sources. Without Redis configured it returns nil, and each side decides how to degrade: the long connection is fail-closed
// (does not start), the scheduler is fail-open (scans as usual, duplicates covered by PG's atomic claiming).
func newFeishuSupervisor(
	ctx context.Context, cfg *config.Config, svc *feishu.Service, store feishu.Store,
	coreSvc *core.Service, log *slog.Logger,
) (*feishu.Supervisor, *redis.Client, func(), error) {
	noop := func() {}

	// Config is passed in by the caller. **This used to do its own config.Load**; besides reading the disk twice, there was
	// a more hidden problem: back then load failure and "Redis not configured" were mixed in the same if, so a broken config file
	// was reported as "Redis not configured", a warning that did not point to the real cause at all. Now config is loaded
	// in one place in run, a load failure exits early, and here we only need to check whether Redis is configured.
	if !cfg.Redis.Enabled() {
		log.Warn("未配置 Redis，飞书长连接不启动 —— " +
			"应用能接入、能检测，但群里 @ 机器人不会有任何反应。" +
			"配置见 server/config.example.yaml 的 redis 段（本地 docker compose up -d）")
		return nil, nil, noop, nil
	}

	rdb, err := redis.New(ctx, cfg.Redis.ToRedisConfig())
	if err != nil {
		return nil, nil, noop, fmt.Errorf("连 Redis 失败（飞书长连接要它选主）: %w", err)
	}

	inbound, err := feishu.NewInbound(feishu.InboundConfig{
		Store:         store,
		Conversations: feishuConversations{svc: coreSvc},
		Sender:        larkSender{},
		Tokens:        larkTokens{},
		// Prefetch message context: card bodies, sub-messages of merged forwards, group names. Failures only degrade;
		// see feishu/larkcontext.go.
		Fetcher: larkFetcher{},
		// Add/remove the OnIt reaction. It is the first feedback after a user @-mentions the bot.
		Reactor: larkReactor{},
		// Chain of thought: the state machine is stored in PG, and the remote side uses Feishu's message_cot API.
		//
		// **Both may be nil** (AMA_STORE=mem has no PG implementation); then this turn degrades to
		// "reaction + final answer", without the thinking process. See the comments on those two fields of InboundConfig.
		Progress: progressStoreOf(store, log),
		COT:      larkCOT{},
		Logger:   log,
	})
	if err != nil {
		_ = rdb.Close()
		return nil, nil, noop, err
	}

	sup, err := feishu.NewSupervisor(feishu.SupervisorConfig{
		Store:   store,
		Inbound: inbound,
		Leases:  larkLeases{client: rdb},
		Clients: svc.LarkClient,
		Logger:  log,
	})
	if err != nil {
		_ = rdb.Close()
		return nil, nil, noop, err
	}
	return sup, rdb, func() { _ = rdb.Close() }, nil
}

// schedulerLeases adapts shared/infra/redis to core.Leases.
//
// **When rdb is nil it must return a nil interface value**, not a non-nil interface wrapping a nil pointer:
// then the scheduler would think "leader election is available", and every Acquire would hit a nil client.
// This is the classic Go interface trap: in `var l core.Leases = schedulerLeaseStore{nil}`, l != nil.
func schedulerLeases(rdb *redis.Client) core.Leases {
	if rdb == nil {
		return nil
	}
	return schedulerLeaseStore{client: rdb}
}

// purgeLeases adapts shared/infra/redis to pgmq.Leases.
//
// The same shape as schedulerLeases but **a different type**: pgmq declares its own Leases/Lease locally
// (infra/ does not import core; see the layering in that package's comment), so that adapter cannot be reused directly.
//
// ⚠️ The same trap: when rdb is nil it must return **the literal nil**, not a non-nil interface wrapping nil:
// that would make `p.leases != nil` in Purger always true, and then on the first lock grab it would
// crash with a nil pointer.
func purgeLeases(rdb *redis.Client) pgmq.Leases {
	if rdb == nil {
		return nil
	}
	return purgeLeaseStore{client: rdb}
}

type purgeLeaseStore struct{ client *redis.Client }

func (s purgeLeaseStore) Acquire(
	ctx context.Context, key string, ttl time.Duration,
) (pgmq.Lease, bool, error) {
	lease, ok, err := s.client.Acquire(ctx, key, ttl)
	if err != nil || !ok {
		return nil, ok, err
	}
	return lease, true, nil
}

type schedulerLeaseStore struct{ client *redis.Client }

func (s schedulerLeaseStore) Acquire(
	ctx context.Context, key string, ttl time.Duration,
) (core.Lease, bool, error) {
	lease, ok, err := s.client.Acquire(ctx, key, ttl)
	if err != nil || !ok {
		return nil, ok, err
	}
	return lease, true, nil
}

// dockerExecConfig translates the docker section of the config file into core.DockerExecConfig.
func dockerExecConfig(cfg *config.Config, memory int64) core.DockerExecConfig {
	d := cfg.Docker
	return core.DockerExecConfig{
		Enabled: d.DockerEnabled(),
		Host:    d.Host,
		Tools: dockertools.Config{
			Image:        d.Image,
			Workdir:      d.Workdir,
			Prefix:       d.Prefix,
			CPUs:         d.CPUs,
			MemoryBytes:  memory,
			PidsLimit:    d.PidsLimit,
			Network:      d.Network,
			IdleAction:   d.IdleAction,
			ContainerTTL: d.ContainerTTLOrDefault(),
			WorkspaceTTL: d.WorkspaceTTLOrDefault(),
		},
	}
}

// larkSender / larkTokens are the default implementations of sending messages and exchanging tokens, calling real Feishu directly.
//
// A new Client is built each time instead of sharing one: Client itself is stateless, and building it only fills in a few fields,
// while sharing one would first require deciding which region to use, which is a property of each app.
type larkSender struct{}

func (larkSender) ReplyCard(
	ctx context.Context, token, messageID string, card map[string]any, opt lark.ReplyCardOption,
) (*lark.SentMessage, error) {
	client, err := lark.New(lark.Config{})
	if err != nil {
		return nil, err
	}
	return client.ReplyCard(ctx, token, messageID, card, opt)
}

func (larkSender) UpdateCard(ctx context.Context, token, messageID string, card map[string]any) error {
	client, err := lark.New(lark.Config{})
	if err != nil {
		return err
	}
	return client.UpdateCard(ctx, token, messageID, card)
}

type larkTokens struct{}

func (larkTokens) TenantAccessToken(ctx context.Context, appID, secret string) (string, error) {
	client, err := lark.New(lark.Config{})
	if err != nil {
		return "", err
	}
	return client.TenantAccessToken(ctx, appID, secret)
}

// larkFetcher prefetches message context, implementing feishu.MessageFetcher.
//
// Like larkSender it builds a new Client each time (stateless; see the reason there). Failures are always degraded by the caller;
// see prefetchContext in feishu/larkcontext.go: it never returns an error upstream.
type larkFetcher struct{}

func (larkFetcher) GetMessage(
	ctx context.Context, token, messageID string, withUserCard bool,
) ([]lark.FetchedMessage, error) {
	client, err := lark.New(lark.Config{})
	if err != nil {
		return nil, err
	}
	return client.GetMessage(ctx, token, messageID, withUserCard)
}

func (larkFetcher) GetChat(ctx context.Context, token, chatID string) (*lark.ChatInfo, error) {
	client, err := lark.New(lark.Config{})
	if err != nil {
		return nil, err
	}
	return client.GetChat(ctx, token, chatID)
}

func (larkFetcher) ListMessages(
	ctx context.Context, token string, container lark.HistoryContainer,
	id string, pageSize, maxPages int, newestFirst bool,
) ([]lark.FetchedMessage, error) {
	client, err := lark.New(lark.Config{})
	if err != nil {
		return nil, err
	}
	return client.ListMessages(ctx, token, container, id, pageSize, maxPages, newestFirst)
}

// larkReactor adds/removes reactions, implementing feishu.Reactor.
//
// Failures are always swallowed by the caller (see markReceived in feishu/reply.go): one missing "received"
// hint should not stop a whole chat turn from running.
type larkReactor struct{}

func (larkReactor) AddReaction(ctx context.Context, token, messageID, emojiType string) (string, error) {
	client, err := lark.New(lark.Config{})
	if err != nil {
		return "", err
	}
	return client.AddReaction(ctx, token, messageID, emojiType)
}

func (larkReactor) RemoveReaction(ctx context.Context, token, messageID, reactionID string) error {
	client, err := lark.New(lark.Config{})
	if err != nil {
		return err
	}
	return client.RemoveReaction(ctx, token, messageID, reactionID)
}

// larkCOT is the three APIs of Feishu chain-of-thought messages, implementing feishu.COTMessenger.
type larkCOT struct{}

func (larkCOT) CreateCOT(
	ctx context.Context, token, receiveIDType, receiveID string, opt lark.CreateCOTOption,
) (*lark.COTMessage, error) {
	client, err := lark.New(lark.Config{})
	if err != nil {
		return nil, err
	}
	return client.CreateCOT(ctx, token, receiveIDType, receiveID, opt)
}

func (larkCOT) AppendCOT(ctx context.Context, token, cotID, messageID string, events []lark.COTEvent) error {
	client, err := lark.New(lark.Config{})
	if err != nil {
		return err
	}
	return client.AppendCOT(ctx, token, cotID, messageID, events)
}

func (larkCOT) CompleteCOT(
	ctx context.Context, token, cotID, messageID string, reason lark.COTCompleteReason,
) error {
	client, err := lark.New(lark.Config{})
	if err != nil {
		return err
	}
	return client.CompleteCOT(ctx, token, cotID, messageID, reason)
}

// progressStoreOf takes the state machine store for chain-of-thought delivery out of the Feishu Store.
//
// Of the two implementations only the PG one has a ProgressStore (the state machine's correctness relies entirely on SQL atomicity; the lease comparison
// is written in the where of UPDATE, which the in-memory version cannot emulate for multi-instance mutual exclusion). So with AMA_STORE=mem
// this returns nil, and that turn has no chain of thought: **this is a degradation, not a fault**; the answer is sent as usual.
//
// Say so when returning nil, otherwise "there is no thinking process in the group" would leave no clue when debugging:
// every code path ran through; only that optional dependency was empty.
func progressStoreOf(store feishu.Store, log *slog.Logger) feishu.ProgressStore {
	if ps, ok := store.(feishu.ProgressStore); ok {
		return ps
	}
	log.Warn("当前 Store 不支持思维链投递（AMA_STORE=mem），" +
		"飞书群里只会有 OnIt 表情与最终答案，没有实时思考过程")
	return nil
}

// chatOrigins adapts *feishu.Service to core.ChatOrigins.
//
// One more example of "core only declares what it needs, the process layer connects it" (same as feishuConversations /
// larkLeases). The only translation needed here is turning feishu.ChatOrigin into apimodel.DeliveryTarget:
// the fields on both sides map one to one, they just belong to different packages.
type chatOrigins struct{ svc *feishu.Service }

func (c chatOrigins) OriginOf(
	ctx context.Context, runID string,
) (apimodel.DeliveryTarget, bool) {
	origin, ok := c.svc.ChatOriginOf(ctx, runID)
	if !ok {
		return apimodel.DeliveryTarget{}, false
	}
	target := apimodel.DeliveryTarget{
		Kind:            apimodel.DeliveryFeishu,
		FeishuAppID:     origin.FeishuAppID,
		ChatID:          origin.ChatID,
		AnchorMessageID: origin.AnchorMessageID,
	}
	// ThreadID cannot be given a real value here (the inflight record only has the boolean "was the reply in a thread back then"),
	// but the delivery side only uses it to decide "reply_in_thread or not", so filling in a non-empty marker is enough.
	//
	// **Without it, self-wakeup results from a thread would be sent to the group** (instead of the original thread),
	// which reports no error; it is just in the wrong place.
	if origin.InThread {
		target.ThreadID = origin.ChatID
	}
	return target, true
}

// scheduleDeliverer sends scheduled task results back to Feishu, implementing core.Deliverer.
//
// # Why this adapter is in cmd/amad and not in core or feishu
//
// It needs three things at once: core's DeliveryTarget (the shape of the delivery address), feishu's Store
// (looking up apps and secrets), and shared/infra/lark's message sending. core and feishu are deliberately coupled only by ID strings
// (see the comment in core/agent.go), and putting it on either side would break that decoupling, so it lives in the process wiring layer,
// the same technique as feishuConversations / larkLeases.
type scheduleDeliverer struct {
	store  feishu.Store
	sender larkSender
	tokens larkTokens
}

// Deliver delivers a piece of text to Feishu.
//
// **No credentials are stored on the Schedule** (see the comment on apimodel.DeliveryTarget), so every delivery must
// look up the secret and exchange a token fresh: tenant_access_token expires in two hours, and storing it would only make it
// silently fail two hours later.
func (d scheduleDeliverer) Deliver(
	ctx context.Context, target apimodel.DeliveryTarget, title, text, runID string,
) error {
	if target.Kind != apimodel.DeliveryFeishu {
		// Only the Feishu kind is handled. run_only already returns on the core side and never gets here;
		// when webhooks are added in the future, branch here.
		return fmt.Errorf("暂不支持的投递方式: %s", target.Kind)
	}
	app, err := d.store.GetApp(ctx, target.FeishuAppID)
	if err != nil {
		return fmt.Errorf("找不到投递用的飞书应用（可能已被断开）: %w", err)
	}
	secret, err := d.store.AppSecret(ctx, app.AppID)
	if err != nil {
		return fmt.Errorf("取应用密钥失败: %w", err)
	}
	token, err := d.tokens.TenantAccessToken(ctx, app.AppID, secret)
	if err != nil {
		return fmt.Errorf("换 token 失败（凭证可能已失效）: %w", err)
	}

	card := lark.TextCard(scheduleCardText(title, text))
	// The idempotency key uses the run id: one run maps to exactly one delivery, and the value does not change on retry,
	// so Feishu can recognize it as the same message rather than a new one (same as the inbound side).
	idem := "ama-schedule:" + runID

	// **With an anchor message, use a reply**: this is the only workable implementation of "send back to the thread if it came from a thread":
	// Feishu's send message API only takes chat_id, and landing in a thread requires reply + reply_in_thread.
	// Without an anchor, post straight to the chat: the content is still in the group, just not in the thread, which is an acceptable degradation.
	if anchor := strings.TrimSpace(target.AnchorMessageID); anchor != "" {
		_, err := d.sender.ReplyCard(ctx, token, anchor, card, lark.ReplyCardOption{
			ReplyInThread:  target.ThreadID != "",
			IdempotencyKey: idem,
		})
		if err == nil {
			return nil
		}
		// The anchor message may have been recalled/deleted, and then the reply fails. **Fall back to posting directly instead of erroring**:
		// what users want is "see the report in the group"; not landing in the original thread is secondary.
		slog.Warn("回复锚点消息失败，退回直发到会话",
			slog.String("chat", target.ChatID), slog.Any("err", err))
	}

	client, err := lark.New(lark.Config{})
	if err != nil {
		return err
	}
	_, err = client.SendCard(ctx, token, target.ChatID, card, idem)
	return err
}

// scheduleCardText builds the card body: a bold title line, followed by the body.
//
// Scheduled task cards **must carry their own title**: it is not a reply to some message, and people in the group seeing a block of text
// would not know what sent it or why now. Including the task name gives the context.
func scheduleCardText(title, text string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return text
	}
	return "**⏰ " + title + "**\n\n" + text
}

// openStores decides the data layer of the three modules by server.store, and returns a cleanup function.
//
// Default pg: connection parameters are in the pg section of the config. **If it cannot connect it errors and exits, without silently falling back to memory**:
// that would look fine until, after a restart, you find all the data gone.
// To run without a database, explicitly set server.store=mem (or AMA_STORE=mem).
//
// ⚠️ When auth uses the in-memory Store, **each instance knows only its own sessions**: logging in lands on A, the next request
// lands on B, and the login drops. So mem only suits a single local process; production must use pg.
// ⚠️ It also returns a pgmq.DB: for "expired message cleanup" (see shared/infra/pgmq/purge.go).
// In the mem tier it is nil: then there is no mq_messages table, so nothing to clean.
func openStores(
	ctx context.Context, cfg *config.Config, log *slog.Logger,
) (core.Store, feishu.Store, auth.Store, core.TerminalRelay, core.WorkerWakeups, pgmq.DB, string, func(), error) {
	if cfg.Server.UseMemStore() {
		log.Warn("用内存 Store，进程重启数据就没了（server.store=mem）；多实例下还会随机掉登录")
		coreStore, err := coremem.NewSeededMemStore()
		if err != nil {
			return nil, nil, nil, nil, nil, nil, "", nil, err
		}
		// With the in-memory Store there is no terminal relay: that path needs a table visible to all instances.
		// **A single process does not need it** (every POST is sure to land in the same process), so nil is right.
		return coreStore, feishu.NewMemStore(), auth.NewMemStore(), nil, nil, nil, "", func() {}, nil
	}

	stdCfg := cfg.PG.ToStdConfig()
	client, err := pg.NewStandard(ctx, stdCfg)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, "", nil, fmt.Errorf("连 PostgreSQL 失败（%s）: %w", stdCfg.Redacted(), err)
	}

	// Encryptor for Ark credentials. **A wrong key must exit right here**, not wait until the first read/write:
	// the error then would be "cannot decrypt credential", pointing to the data instead of the config.
	crypto, err := cryptobox.New(cfg.Ark.SecretKey)
	if err != nil {
		client.Close()
		return nil, nil, nil, nil, nil, nil, "", nil, fmt.Errorf("方舟凭证加密密钥不可用（ark.secretKey）: %w", err)
	}
	// This line is the **only** way for ops to know "is encryption really in effect": the two modes are exactly the same
	// in function (keys work normally either way); the only difference is the shape in the database. So both modes must be stated, with the consequence.
	if crypto.Enabled() {
		log.Info("方舟凭证加密落库已启用（AES-256-GCM）")
	} else {
		log.Warn("未配置 ark.secretKey，方舟凭证以**明文**落库；" +
			"库被读到即等于全部用户的方舟 key 泄露。生成一把：openssl rand -base64 32")
	}

	// Create tables + install seeds only into an empty database: when non-empty existing data is kept, so a restart does not wipe what users created.
	coreStore, err := coredao.NewSeededPGStore(ctx, client, coredao.WithCrypto(crypto))
	if err != nil {
		client.Close()
		return nil, nil, nil, nil, nil, nil, "", nil, fmt.Errorf("初始化 core 的 PG Store 失败: %w", err)
	}
	feishuStore, err := feishudao.NewPGStore(ctx, client)
	if err != nil {
		client.Close()
		return nil, nil, nil, nil, nil, nil, "", nil, fmt.Errorf("初始化 feishu 的 PG Store 失败: %w", err)
	}
	authStore, err := authdao.NewPGStore(ctx, client)
	if err != nil {
		client.Close()
		return nil, nil, nil, nil, nil, nil, "", nil, fmt.Errorf("初始化 auth 的 PG Store 失败: %w", err)
	}

	relay, wakeups, err := newTerminalRelay(client, stdCfg, log)
	if err != nil {
		client.Close()
		return nil, nil, nil, nil, nil, nil, "", nil, fmt.Errorf("初始化终端中转失败: %w", err)
	}

	dsn, err := stdCfg.DSN()
	if err != nil {
		client.Close()
		return nil, nil, nil, nil, nil, nil, "", nil, fmt.Errorf("拼宿主机唤醒监听 DSN 失败: %w", err)
	}
	log.Info("已连上 PostgreSQL", slog.String("dsn", stdCfg.Redacted()))
	return coreStore, feishuStore, authStore, relay, wakeups, pgmqDB{client}, dsn, func() { client.Close() }, nil
}

// openBlobStore decides where file bytes go, returning the S3 client (the personal files domain needs it too) and the BlobStore for core.
//
//   - s3 configured → use it, the same for both tiers. Unreachable / bucket unusable errors and exits directly.
//   - s3 not configured, PG tier → **refuse to start**. The PG tier is a multi-instance setup, and bytes must go into shared object storage;
//     it will **not** fall back to writing bytes into PG either (that table has been removed).
//   - s3 not configured, mem tier → bytes go into process memory (the same "gone on restart" tier as mem metadata),
//     and the personal files endpoints return 503 (they need presigned links and multipart upload, which the in-memory version does not do).
func openBlobStore(ctx context.Context, cfg *config.Config, log *slog.Logger) (*s3.Client, core.BlobStore, error) {
	client, err := newS3Client(ctx, cfg, log)
	if err != nil {
		return nil, nil, err
	}
	if client != nil {
		return client, &blobStore{client: client}, nil
	}
	if !cfg.Server.UseMemStore() {
		return nil, nil, errors.New("没配对象存储（s3.bucket / AMA_S3_BUCKET 为空）。" +
			"附件、技能文件、个人文件的字节只存对象存储，不存 PostgreSQL——" +
			"本地 `docker compose up -d s3` 起一个 RustFS，并按 server/config.example.yaml 的 s3 段配置")
	}
	log.Warn("没配对象存储，文件字节放进程内存（server.store=mem），重启即丢")
	return nil, coremem.NewMemBlobStore(), nil
}

// userProfileLookup adapts auth.Store to core.UserProfileLookup.
//
// There is only one reason it exists: **type conversion**. The auth side returns auth.User (the storage shape with CreatedAt /
// UpdatedAt), while the core side wants apimodel.UserProfile (the HTTP contract shape with only three
// fields). Letting core take auth.User directly would muddle the rule "does it go into the response":
// exactly why the model/persistmodel package exists.
//
// ⚠️ No `if nil` wrapper like departmentLookupOf: authStore here is **always non-nil**
// (if openStores fails it returns an error directly), so there is no "not connected" state.
type userProfileLookup struct {
	store auth.Store
}

func (l userProfileLookup) ListUsers(
	ctx context.Context, emails []string,
) (map[string]apimodel.UserProfile, error) {
	users, err := l.store.ListUsers(ctx, emails)
	if err != nil {
		return nil, err
	}
	out := make(map[string]apimodel.UserProfile, len(users))
	for email, u := range users {
		out[email] = apimodel.UserProfile{
			Email:     u.Email,
			Name:      u.Name,
			AvatarURL: u.AvatarURL,
		}
	}
	return out, nil
}

// newIntegrationRegistry builds the provider registry of "authorization management" from config.
//
// # Providers with missing config must be registered too
//
// When some source is not fully configured, **it is still registered** (internal client nil, status showing missing),
// instead of skipped. Skipped, that card would disappear from the page entirely, users would not know it exists, and so would never
// configure it: "the feature does not exist" and "the feature exists but is not configured" must be two different things in the UI.
//
// The registration order is the card order. The Feishu user authorization card depends on coreSvc and is registered last in run().
func newIntegrationRegistry(authSvc *auth.Service, log *slog.Logger) *core.IntegrationRegistry {
	reg := core.NewIntegrationRegistry(log)

	// The AMA platform itself: the credential switch for the `ama` command inside containers.
	//
	// ⚠️ It has a different shape from other providers: **it queries our own database** (auth_api_keys),
	// while the others ask external platforms in real time. This does not break the "state is not stored" discipline of integration.go:
	// that one targets external platform state, while this provider's authority **is in our database**.
	reg.Register(core.NewAMAProvider(authSvc.ManagedKeysFor(), log))
	return reg
}

// newOIDCClient builds the OIDC client from auth.oidc, returning **a literal nil interface** when not configured or misconfigured.
//
// ⚠️ The return type is the interface auth.OIDCClient, so it **must not return a nil *oidc.Client**:
// that would give "a non-nil interface value", auth would think OIDC is enabled, and the first click on the button would panic.
//
// Construction does no network requests (discovery is lazy; see the shared/infra/oidc package comment), so an IdP
// that is temporarily unreachable does not stop the service from starting.
func newOIDCClient(cfg *config.Config, log *slog.Logger) auth.OIDCClient {
	o := cfg.Auth.OIDC
	if !o.Enabled() {
		return nil
	}
	client, err := oidc.New(oidc.Config{
		Issuer:       o.Issuer,
		ClientID:     o.ClientID,
		ClientSecret: o.ClientSecret,
		Scopes:       o.Scopes,
	})
	if err != nil {
		// Configured but invalid: say it loudly, but do not stop the service from starting (local accounts and API Keys still work).
		log.Error("OIDC 配置不合法，OIDC 登录不可用", slog.Any("err", err))
		return nil
	}
	return client
}

// newGitHubClient builds the GitHub device flow client from auth.github, returning **a literal nil interface** when not enabled or misconfigured
// (same reason as newOIDCClient). When allowedOrgs is configured it adds the read:org scope automatically: without it organization
// membership cannot be looked up, showing as "everyone in the organization is rejected", with an error not pointing to the scope.
func newGitHubClient(cfg *config.Config, log *slog.Logger) auth.GitHubClient {
	g := cfg.Auth.GitHub
	if !g.Active() {
		return nil
	}
	scopes := g.Scopes
	if len(scopes) == 0 {
		scopes = github.DefaultScopes
	}
	if len(g.AllowedOrgs) > 0 && !slices.Contains(scopes, "read:org") {
		scopes = append(slices.Clone(scopes), "read:org")
	}
	client, err := github.New(github.Config{ClientID: g.ClientID, Scopes: scopes, Proxy: g.Proxy})
	if err != nil {
		log.Error("GitHub 登录配置不合法，GitHub 登录不可用", slog.Any("err", err))
		return nil
	}
	return client
}

// logLoginMethods writes "how people can log in" into the startup log.
//
// The check asks the Service itself (LocalEnabled / OIDCEnabled), not "does the config object exist":
// the latter is always true, and would make the log say "enabled" while the feature is actually unusable.
func logLoginMethods(cfg *config.Config, authSvc *auth.Service, log *slog.Logger) {
	if !authSvc.LocalEnabled() && !authSvc.OIDCEnabled() && !authSvc.GitHubEnabled() {
		log.Error("没有启用任何登录方式：auth.local.enabled 为 false、auth.oidc 与 auth.github 都未配置——" +
			"**谁都登不进网页**（只有 API Key 能调接口）。至少配一种：" +
			"auth.local.enabled: true（邮箱密码），auth.oidc.issuer + auth.oidc.clientId（SSO），" +
			"或 auth.github.enabled + auth.github.clientId")
		return
	}
	if authSvc.GitHubEnabled() {
		g := cfg.Auth.GitHub
		log.Info("GitHub 登录（device flow）已启用",
			slog.String("clientId", authSvc.GitHubClientID()),
			slog.Bool("走专用代理", strings.TrimSpace(g.Proxy) != ""),
			slog.Int("准入用户名", len(g.AllowedUsers)),
			slog.Int("准入邮箱域名", len(g.AllowedEmailDomains)),
			slog.Int("准入组织", len(g.AllowedOrgs)))
		if strings.TrimSpace(g.ClientID) == "" {
			log.Warn("auth.github.clientId 未配置，正在用 GitHub CLI 的公开 client id：用户在 GitHub 授权页看到的是" +
				"「GitHub CLI」，且对方随时可能吊销它。正式部署请建一个自己的 OAuth App（勾选 Enable Device Flow）填进来")
		}
		if !authSvc.GitHubHasAllowlist() {
			log.Error("auth.github 已启用但没配任何准入白名单（allowedUsers / allowedEmailDomains / allowedOrgs）：" +
				"**任何 GitHub 账号都会被拒绝登录**。至少配一项，如 AMA_AUTH_GITHUB_ALLOWED_USERS=<你的 GitHub 用户名>")
		}
	}
	if authSvc.LocalEnabled() {
		log.Info("邮箱密码登录已启用",
			slog.Bool("开放自助注册", cfg.Auth.Local.AllowSignup),
			slog.Int("平台管理员名单", len(cfg.Auth.Admins)))
		if !cfg.Auth.Local.AllowSignup {
			log.Info("自助注册已关闭：库里还没有任何用户时允许注册第一个账号（它成为平台管理员），" +
				"之后由平台管理员调 POST /api/admin/users 建账号")
		}
	}
	if authSvc.OIDCEnabled() {
		redirect := strings.TrimRight(strings.TrimSpace(cfg.Server.PublicURL), "/") + auth.PathOIDCCallback
		log.Info("OIDC 登录已启用（IdP 侧要登记下面这个 redirect_uri）",
			slog.String("issuer", cfg.Auth.OIDC.Issuer),
			slog.String("redirect_uri", redirect),
			slog.Bool("允许未验证邮箱", cfg.Auth.OIDC.AllowUnverifiedEmail))
		if cfg.Auth.OIDC.AllowUnverifiedEmail {
			log.Warn("auth.oidc.allowUnverifiedEmail 已打开：IdP 不验证邮箱时，任何人都能冒充任何邮箱登录。" +
				"只在确认 IdP 的邮箱字段由管理员维护时才这么配")
		}
	}
}

// loadConfig loads the config and writes "where the values came from" into the startup log.
//
// # Those two log lines are not just nice to have
//
// Env vars have the highest priority (see the shared/infra/config package comment); the cost is that **leftover env vars of the same name
// keep winning**, so "changed the file and it did not take effect" becomes a trap with no error. The second log line lists the overridden
// field names one by one, making this visible at a glance at startup. Nothing beyond field names is printed: secrets must not go into logs.
func loadConfig(log *slog.Logger) (*config.Config, error) {
	sources := config.SourcesFromEnv()
	res, err := config.LoadFrom(sources)
	if err != nil {
		return nil, fmt.Errorf("加载配置失败（%s）: %w", sources.Path, err)
	}

	log.Info("配置已加载",
		slog.String("source", res.Source),
		slog.String("store", res.Config.Server.StoreOrDefault()))
	if len(res.EnvOverridden) > 0 {
		log.Warn("以下字段被环境变量覆盖了 —— 改配置文件对它们无效，要改环境变量",
			slog.Any("fields", res.EnvOverridden))
	}
	// Unknown fields do not block startup (config going live before code is a normal order), but they must be visible:
	// a misspelled key looks exactly like "this version does not know it yet", and only this log line can tell them apart.
	if len(res.UnknownFields) > 0 {
		log.Warn("配置里有本版本不认识的字段，已忽略 —— 若不是新配置项那就是键名拼错了",
			slog.Any("fields", res.UnknownFields))
	}
	return res.Config, nil
}

// newLarkOAuthClient builds the client for Feishu user OAuth.
//
// The default (China) domain is enough: the zero value of `lark.Config{}` is accounts.feishu.cn + open.feishu.cn.
//
// ⚠️ **Here it deliberately does not choose the domain by the app's region**, unlike feishu.Service.larkClient. The reason is
// that every operation there holds a specific app (knowing whether it is the feishu or lark domain), while this provider
// is **account-level**, built once and used for a long time, and when it is built we do not yet know which app will be used.
//
// The cost: **user authorization for apps on the international domain (Lark) fails**: accounts.feishu.cn cannot find
// a client_id that only exists on larksuite, and reports 20048 "app does not exist". All current users are on
// the China domain, so it stays this way for now; to really support the international domain, the right fix is to make this client "built per region
// on the fly" (like larkClient), not to change the default here: changing the default would only flip the direction of failure.
func newLarkOAuthClient(cfg *config.Config, log *slog.Logger) *lark.Client {
	client, err := lark.New(lark.Config{})
	if err != nil {
		// A zero-value config should in theory never fail. If it really fails, say so and degrade to "not integrated":
		// then the card shows missing, instead of stopping the whole service from starting.
		log.Warn("建飞书 OAuth 客户端失败，飞书用户授权将显示未接入", slog.Any("err", err))
		return nil
	}
	return client
}
