// Long-connection supervisor: keeps one long connection for each connected Feishu app.
//
// # Why this layer exists
//
// The Receiver in shared/infra/lark only manages **one** connection (grab the lock, connect, reconnect when dropped). But we have N
// connected apps, and N changes: users may connect new apps or disconnect old ones at any time. This layer is responsible for:
//
//	on process start   open a connection for every existing app
//	periodic reconcile add connections for new apps, stop connections for deleted apps
//	on process exit    stop them all and give the Redis locks back
//
// # Why polling reconcile instead of event-driven
//
// Connecting/disconnecting is a **very low frequency** operation (a workspace connects only a few apps in its whole life), while event-driven would either add
// a new message path or make feishu.Service depend back on this type. Polling once a minute costs
// "a newly connected app may wait up to a minute before receiving messages"; the registration flow itself needs the user to click agree on the authorization page,
// so this delay is buried in it and goes unnoticed.
package feishu

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// reconcileInterval is the reconcile interval. See the file header for why.
const reconcileInterval = time.Minute

// SupervisorConfig wires the supervisor.
type SupervisorConfig struct {
	// Store is used to enumerate connected apps and get secrets.
	Store Store
	// Inbound handles received messages.
	Inbound *Inbound
	// Leases is the lock store for leader election, passed straight through to each connection.
	Leases lark.LeaseStore
	// Clients builds Feishu clients by region, used to read the bot identity.
	Clients func(region lark.Region) (*lark.Client, error)
	// Logger defaults to slog.Default() when empty.
	Logger *slog.Logger
}

// Supervisor keeps the long connections of all apps.
type Supervisor struct {
	cfg SupervisorConfig
	log *slog.Logger

	mu      sync.Mutex
	running map[string]context.CancelFunc // Feishu appID → stops its connection
	wg      sync.WaitGroup
}

// NewSupervisor creates a supervisor. **It does not connect**; call Run.
func NewSupervisor(cfg SupervisorConfig) (*Supervisor, error) {
	if cfg.Store == nil {
		return nil, ErrInvalidArgument
	}
	if cfg.Inbound == nil {
		return nil, ErrInvalidArgument
	}
	if cfg.Leases == nil {
		return nil, ErrInvalidArgument
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Supervisor{
		cfg:     cfg,
		log:     log,
		running: make(map[string]context.CancelFunc),
	}, nil
}

// Run blocks maintaining all connections until ctx is cancelled.
func (s *Supervisor) Run(ctx context.Context) error {
	// First take back the unfinished turns left by the previous process, then start receiving new messages. Recovering at this moment feels
	// closest to "the service never went down" for users.
	//
	// It runs in a goroutine because recovery may hold a stream for minutes (the agent is still running), and must not delay
	// setting up the long connections; that would make all new messages unreceivable for those minutes.
	//
	// # Recovery and new messages competing for the same session are blocked by the inflight record itself
	//
	// Sessions are now reused by scope (see scope.go), so a session being recovered **will** receive new messages from that chat;
	// this differs from the "one turn, one session" assumption before the change. No extra lock is needed: during recovery that
	// inflight record stays (deleted only when finished), and a new message must TryBeginTurn before running,
	// so it is naturally blocked and gets "the previous message is still being processed". Cross-instance safety is likewise covered by the CAS in the store
	// (see Store.ClaimInflightTurn / TryBeginTurn), not by timing here.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.cfg.Inbound.ResumeInflightTurns(ctx)
	}()

	s.reconcile(ctx)

	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Each connection's ctx is derived from this ctx, so they receive the cancellation signal by themselves.
			// Here we only wait for them to give the locks back; without waiting, the locks would remain in Redis when the process exits,
			// and the next instance would have to wait a full TTL before taking over.
			s.wg.Wait()
			return nil
		case <-ticker.C:
			s.reconcile(ctx)
		}
	}
}

// reconcile reconciles once: start what should start, stop what should stop.
func (s *Supervisor) reconcile(ctx context.Context) {
	apps, err := s.cfg.Store.ListAllApps(ctx)
	if err != nil {
		s.log.Warn("列飞书应用失败，这轮对账跳过", "error", err.Error())
		return
	}

	want := make(map[string]*App, len(apps))
	for _, app := range apps {
		if id := strings.TrimSpace(app.AppID); id != "" {
			want[id] = app
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Stop connections of apps that were deleted.
	for appID, cancel := range s.running {
		if _, ok := want[appID]; !ok {
			s.log.Info("飞书应用已断开，停掉它的长连接", "larkApp", appID)
			cancel()
			delete(s.running, appID)
		}
	}

	// Start connections for new apps.
	for appID, app := range want {
		if _, ok := s.running[appID]; ok {
			continue
		}
		s.startLocked(ctx, app)
	}
}

// startLocked starts a long connection for one app. The caller must hold the lock.
//
// ⚠️ **No remote calls inside the lock.** botIdentity now asks Feishu every time (two HTTP requests,
// each with a 30-second timeout); here, one app's Feishu API going down could hold s.mu for a minute,
// also blocking other apps from connecting and the "stop connections of deleted apps" half. So it moved into
// the goroutine below, and inside the lock we only "register + start the goroutine".
func (s *Supervisor) startLocked(ctx context.Context, app *App) {
	secret, err := s.cfg.Store.AppSecret(ctx, app.AppID)
	if err != nil {
		// Without a secret it cannot connect. This is not an error: an app whose registration failed midway may have no secret.
		s.log.Warn("飞书应用没有密钥，跳过", "larkApp", app.AppID)
		return
	}

	connCtx, cancel := context.WithCancel(ctx)
	s.running[app.AppID] = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		defer func() {
			// Remove it from the table; the next reconcile retries (failures like invalid credentials keep failing,
			// but retrying once a minute is an acceptable cost, and it recovers automatically after the user reconnects).
			s.mu.Lock()
			if s.running[app.AppID] != nil {
				delete(s.running, app.AppID)
			}
			s.mu.Unlock()
		}()

		// Reading the bot identity calls Feishu, so it is outside the lock, before building the Receiver.
		receiver, err := lark.NewReceiver(lark.ReceiverConfig{
			AppID:     app.AppID,
			AppSecret: secret,
			Bot:       s.botIdentity(connCtx, app, secret),
			Handler:   s.cfg.Inbound.Handle,
			Leases:    s.cfg.Leases,
			Logger:    s.log,
		})
		if err != nil {
			s.log.Error("建长连接失败", "larkApp", app.AppID, "error", err.Error())
			return
		}
		// State at once "what this connection can actually receive".
		//
		// ⚠️ **This log line is a direct product of the 2026-09-02 investigation.** Before it, an
		// app "missing the include_bot permission and therefore receiving no bot messages at all" looked **word for word identical** in the logs
		// to a perfectly normal app: both had only the two lines "long connection started" + "ready",
		// then nothing. And the readiness API did not help either: runtimeConnected is only a REST liveness probe,
		// and all green still receives no messages (see the comment on that field in app.go).
		//
		// So debugging could only rely on "send a message in the chat and see if it reacts", and the alert chat had no real people,
		// so even that could not be done. With these three values, "why does this chat not react" proves itself **at the moment of connecting**,
		// without waiting for the next alert.
		//
		// What is logged are enums and booleans, with no user data at all.
		s.log.Info("已为飞书应用拉起长连接",
			"larkApp", app.AppID,
			"workspace", app.WorkspaceID,
			"triggerMode", string(NormalizeTriggerMode(app.TriggerMode)),
			// receive chat messages without @ (only those sent by users)
			"groupMsgScope", groupMessageGranted(app),
			// receive chat messages without @ (**including those sent by other bots**): chats like alert chats where "only bots talk"
			// depend on it entirely, and when it is false such chats get not a single event.
			"botMsgScope", botMessageGranted(app))
		s.warnIfBotMessagesUnreachable(app)
		if err := receiver.Run(connCtx); err != nil {
			s.log.Error("长连接退出", "larkApp", app.AppID, "error", err.Error())
		}
	}()
}

// groupMessageGranted / botMessageGranted fold the grant state of the two levels of chat message permissions into a
// three-valued string that can be logged.
//
// ⚠️ **"Never checked" must be kept apart from "not granted".** Neither is true, but the handling is completely different:
// the former is "I do not know yet" (do not draw conclusions from it), the latter is "really missing, go apply on the open platform".
// Folding into a boolean would call the former false, so the log would be **making groundless accusations**, and whoever reads the log would
// go apply for a permission that may have been there all along. Same as the readiness rule "if it cannot be read, do not report it as missing".
func groupMessageGranted(app *App) string {
	if app == nil || app.Readiness == nil {
		return "unknown"
	}
	return strconv.FormatBool(app.Readiness.GroupMessageScopeGranted)
}

func botMessageGranted(app *App) string {
	if app == nil || app.Readiness == nil {
		return "unknown"
	}
	return strconv.FormatBool(app.Readiness.BotMessageScopeGranted)
}

// warnIfBotMessagesUnreachable warns explicitly when "no-@ is on, but the permission to receive bot messages is missing".
//
// # Why this defense can only live at connect time, not in dropReporter
//
// Intuitively it belongs with the inbound "received but did not trigger" drop logs, but **there it would never fire**:
// without the permission Feishu **does not push events at all**, and Handle is never called. A log that can never be printed
// is a Schrödinger defense, worse than none, because it makes people think someone is taking care of it
// (docs/guides/backend.md: "search for its dedicated log; 0 hits means it never took effect").
//
// Connecting is the only moment that "does not depend on messages arriving", so the check is placed here.
//
// # Why Warn instead of Info
//
// This is a state where **the feature cannot work under the current config**, not a normal state: the user explicitly chose no-@ for topic groups,
// and the most common speakers in such chats are bots (measured in an alert chat: 43 of 44 were bots).
// For normal apps this never prints, so it does not pollute "search for Warn".
//
// # Frequency: at worst once a minute, and only for apps that are really misconfigured
//
// It follows connecting, not every reconcile round: `reconcile` is guarded by `s.running`, and a live connection is not
// restarted. Only the kind that "exits right after connecting" (invalid credentials and the like) restarts every minute,
// which is the same rhythm as the adjacent "started a long connection for the Feishu app", adding no new flooding.
//
// ⚠️ Only report when **we know for sure it is not granted**. When readiness is nil (never checked), stay quiet:
// that is "I do not know yet", and warning on it would be a groundless accusation, same as the readiness rule "if it cannot be read, do not report it as missing".
func (s *Supervisor) warnIfBotMessagesUnreachable(app *App) {
	if app == nil || app.Readiness == nil {
		return
	}
	if NormalizeTriggerMode(app.TriggerMode) != TriggerTopicAndMention {
		return
	}
	if app.Readiness.BotMessageScopeGranted {
		return
	}
	// State the **consequence**, not just "permission missing", following the rule at messages.ts:660.
	// This sentence must let whoever reads the log act directly, without digging through docs.
	s.log.Warn("这个飞书应用开了话题群免 @，但没有「收其他机器人消息」的权限，"+
		"机器人发的消息（含各类告警）飞书一条都不会推下来；"+
		"去开放平台申请该权限、经管理员审批并发布新版本后才会生效",
		"larkApp", app.AppID,
		"workspace", app.WorkspaceID,
		"scope", lark.ScopeGroupMessageIncludeBot)
}

// botIdentity reads the bot's own identity, used to judge "was I @-mentioned" and to filter out its own messages.
//
// # ⚠️ Feishu wins; the store is only a cache: changed on 2026-09-01
//
// This used to be "use `app.BotOpenID` directly if non-empty", so **once stored wrong it never healed**.
// Hit in practice: app `cli_example_main` had `ou_aaaa…` in the store, while its bot on
// the Feishu side was actually `ou_bbbb…`; the two did not match.
//
// That wrong value is not harmless: the self filter in `shared/infra/lark` (`SenderOpenID == Bot.OpenID`)
// and `isSelfMessage` in `trigger.go` **both use it as the test**, and comparing against a wrong open_id
// never matches, so **the whole self-trigger defense fails**. It did not blow up before because the no-@ path still had
// "block all bots" as a safety net; after that gate was removed on 2026-09-01, this is the only defense.
//
// So now Feishu is asked on every connect, and when a mismatch is found we **write back to the store + log a Warn**:
// without writing back, the next process would have to look it up again, and the wrong value would stay shown in the UI.
//
// When it cannot be fetched, **fall back to the store value** (may be empty): failing to read the identity should not stop the long connection from starting,
// and the no-@ path falls back to "block all bots" when the open_id set is empty, still the safe side.
func (s *Supervisor) botIdentity(ctx context.Context, app *App, secret string) lark.BotIdentity {
	bot := lark.BotIdentity{OpenID: strings.TrimSpace(app.BotOpenID), AppID: app.AppID}
	if s.cfg.Clients == nil {
		return bot
	}
	client, err := s.cfg.Clients(lark.NormalizeRegion(app.Region))
	if err != nil {
		return bot
	}
	token, err := client.TenantAccessToken(ctx, app.AppID, secret)
	if err != nil {
		return bot
	}
	info, err := client.BotInfo(ctx, token)
	if err != nil || info == nil || strings.TrimSpace(info.OpenID) == "" {
		return bot
	}
	actual := strings.TrimSpace(info.OpenID)
	if actual == bot.OpenID {
		return bot
	}
	// Mismatch: Feishu wins, and say so. Existing wrong values are discovered through this log.
	s.log.Warn("飞书应用的 bot open_id 与库里存的不一致，以飞书为准并回写",
		"larkApp", app.AppID, "stored", bot.OpenID, "actual", actual)
	bot.OpenID = actual
	s.persistBotOpenID(ctx, app, actual)
	return bot
}

// persistBotOpenID writes the corrected open_id back to the store.
//
// **Failures are only logged**: this round already connected with the correct value; a failed write back at most means looking it up again on next start,
// and should not stop the long connection from starting.
func (s *Supervisor) persistBotOpenID(ctx context.Context, app *App, openID string) {
	fresh, err := s.cfg.Store.GetAppByAppID(ctx, app.AppID)
	if err != nil {
		s.log.Warn("回写 bot open_id 前重读应用失败", "larkApp", app.AppID, "error", err.Error())
		return
	}
	// Re-read before changing: the user may have just changed the trigger mode or similar in between, and writing back the old object in hand would overwrite it.
	fresh.BotOpenID = openID
	if _, err := s.cfg.Store.SaveApp(ctx, fresh); err != nil {
		s.log.Warn("回写 bot open_id 失败", "larkApp", app.AppID, "error", err.Error())
	}
}

// LarkClient exposes Service's ability to build clients, for cmd/amad to wire up the Supervisor.
//
// Service.larkClient is unexported (used inside this package), but the supervisor is wired at the process level and needs the same
// address config (tests override AccountsURL / OpenURL with httptest).
func (s *Service) LarkClient(region lark.Region) (*lark.Client, error) {
	return s.larkClient(region)
}
