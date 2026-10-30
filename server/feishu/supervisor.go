// Long-connection supervisor: keeps one long connection for each connected Feishu app.
//
// The Receiver in shared/infra/lark manages **one** connection (grab the lock, connect, reconnect when dropped). The
// "N apps, and N changes" part (start on boot, add new apps, stop deleted ones, stop all on exit) is the generic
// channel.Supervisor; this file is the Feishu Source for it: which apps exist, and how to run one app's connection.
package feishu

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/chyroc/ama/server/channel"
	"github.com/chyroc/ama/server/shared/infra/lark"
)

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
	cfg   SupervisorConfig
	log   *slog.Logger
	inner *channel.Supervisor
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
	s := &Supervisor{cfg: cfg, log: log}
	s.inner = channel.NewSupervisor(s, channel.DefaultReconcileInterval, log)
	return s, nil
}

// Run blocks maintaining all connections until ctx is cancelled.
func (s *Supervisor) Run(ctx context.Context) error {
	// First take back the unfinished turns left by the previous process, then start receiving new messages. Recovering at this moment feels
	// closest to "the service never went down" for users.
	//
	// It runs in a goroutine because recovery may hold a stream for minutes (the agent is still running), and must not delay
	// setting up the long connections; that would make all new messages unreceivable for those minutes.
	//
	// Recovery and new messages competing for the same session are blocked by the inflight record itself: during recovery that
	// record stays (deleted only when finished), and a new message must TryBeginTurn before running, so it gets
	// "the previous message is still being processed". Cross-instance safety is covered by the CAS in the store
	// (see Store.ClaimInflightTurn / TryBeginTurn), not by timing here.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.cfg.Inbound.ResumeInflightTurns(ctx)
	}()
	// The inner supervisor waits for every connection to give its Redis lock back before returning; without waiting,
	// the next instance would have to wait a full TTL before taking over.
	err := s.inner.Run(ctx)
	wg.Wait()
	return err
}

// Name implements channel.Source.
func (s *Supervisor) Name() string { return string(channel.Feishu) }

// ListAccounts implements channel.Source: every connected app with an app id.
//
// Version stays empty: a running connection is not restarted when the app row changes (trigger mode and the like
// are read per message, not per connection).
func (s *Supervisor) ListAccounts(ctx context.Context) ([]channel.Account, error) {
	apps, err := s.cfg.Store.ListAllApps(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]channel.Account, 0, len(apps))
	for _, app := range apps {
		if id := strings.TrimSpace(app.AppID); id != "" {
			out = append(out, channel.Account{Key: id, Value: app})
		}
	}
	return out, nil
}

// RunAccount implements channel.Source: one app's long connection.
//
// Returning means "start it again at the next reconcile". Failures like invalid credentials keep failing, but
// retrying once a minute is an acceptable cost, and it recovers by itself after the user reconnects.
func (s *Supervisor) RunAccount(ctx context.Context, account channel.Account) error {
	app, ok := account.Value.(*App)
	if !ok || app == nil {
		return nil
	}
	secret, err := s.cfg.Store.AppSecret(ctx, app.AppID)
	if err != nil {
		// Without a secret it cannot connect. This is not an error: an app whose registration failed midway may have no secret.
		s.log.Warn("飞书应用没有密钥，跳过", "larkApp", app.AppID)
		return nil
	}

	// Reading the bot identity calls Feishu (two HTTP requests); it happens here in the account's own goroutine, never
	// under the supervisor's lock, so one app's Feishu API going down cannot block the other apps.
	receiver, err := lark.NewReceiver(lark.ReceiverConfig{
		AppID:     app.AppID,
		AppSecret: secret,
		Bot:       s.botIdentity(ctx, app, secret),
		Handler:   s.cfg.Inbound.Handle,
		Leases:    s.cfg.Leases,
		Logger:    s.log,
	})
	if err != nil {
		s.log.Error("建长连接失败", "larkApp", app.AppID, "error", err.Error())
		return nil
	}
	// State at once "what this connection can actually receive".
	//
	// ⚠️ An app "missing the include_bot permission and therefore receiving no bot messages at all" used to look
	// **word for word identical** in the logs to a perfectly normal app: "long connection started" + "ready", then nothing.
	// With these three values, "why does this chat not react" proves itself at the moment of connecting.
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
	return receiver.Run(ctx)
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
// It follows connecting, not every reconcile round: channel.Supervisor does not restart a live connection. Only the kind that "exits right after connecting" (invalid credentials and the like) restarts every minute,
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
