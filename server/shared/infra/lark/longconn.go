// Feishu event long connection (websocket).
//
// # Why a long connection instead of a callback URL
//
// The usual way is to give Feishu a public URL and let it POST events to it. That path has three
// problems for us: it needs a public entry point, it needs signature checks and decryption, and
// users have to tick event subscriptions one by one on the open platform.
//
// A long connection removes all three: the SDK dials a websocket to Feishu and events are pushed
// down this connection. **No callback URL, no verification token, no encrypt key**: only
// AppID + AppSecret. The SDK channel itself guarantees the source, so the dispatcher passes
// empty signature parameters.
//
// # Only one long connection per app
//
// This is a platform constraint, and the most important thing in this file. When several amad
// instances each connect to the same app on their own, Feishu kicks out the earlier one, and
// the instances keep evicting each other, **with no error**. It shows up as inbound messages
// randomly getting lost (this one goes to A, the next to B, and nobody gets the ones sent while
// a connection was being kicked).
//
// So the Runner must grab a Redis lock before connecting, and **never connects if it cannot get
// the lock** (fail-closed). Better that this instance receives no messages than two instances
// fighting. See the package comment of shared/infra/redis.
//
// # Why the SDK's auto-reconnect is turned off
//
// WithAutoReconnect(false) is on purpose. SDK reconnects would bypass the lock above: the lease
// has expired and another instance has taken the lock, but the SDK keeps quietly reconnecting,
// and again two instances are connected at once.
// Reconnect, backoff and readiness checks are all handled by the Runner, with the same lifetime
// as the lease.
package lark

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// Error codes the SDK returns for bad credentials. **Reconnecting ten thousand times will not
// help with these**: bad credentials stay bad, and we can only wait for the user to reconnect
// the app. Without telling them apart, an app with invalid credentials would back off and
// reconnect forever, flooding the log with no progress.
//
// Both codes must be recognized. Only 1000040345 is not enough: **in testing, calling real Feishu
// with an app_id that does not exist returns 1000040346** (found by running
// cmd/probe-feishu-longconn). Recognizing only the former would send the most common mistake,
// "wrong app_id", into endless reconnects. Unit tests cannot find this difference: we write the
// codes the fake Feishu returns ourselves.
const (
	// codeInvalidCredential means app_id or app_secret is wrong.
	codeInvalidCredential = 1000040345
	// codeInvalidAppID means the app_id itself does not exist.
	codeInvalidAppID = 1000040346
)

// Reconnect backoff. Most cases recover after a short blip, so start at 1 second; when the
// connection cannot be made it usually takes a long time, so cap at 30 seconds to avoid
// wasting connections.
const (
	backoffMin = 1 * time.Second
	backoffMax = 30 * time.Second
)

// Leader lease. The TTL must be much larger than the renewal interval: an occasionally slow
// renewal (GC, network blips) should not lose the lock.
// 4x margin: the lock is only lost after three renewals in a row fail.
const (
	leaseTTL      = 20 * time.Second
	leaseRenew    = 5 * time.Second
	leaseRetryGap = 5 * time.Second
)

// LeaseStore is what leader election needs; it is provided by shared/infra/redis.
//
// It is an interface instead of a direct dependency on shared/infra/redis: this package is the
// Feishu access layer and should not depend on another infrastructure package.
// Tests can also plug in a fake implementation without starting Redis.
type LeaseStore interface {
	Acquire(ctx context.Context, key string, ttl time.Duration) (Lease, bool, error)
}

// Lease is a lock currently held.
type Lease interface {
	Renew(ctx context.Context, ttl time.Duration) (bool, error)
	Release(ctx context.Context) error
}

// InboundMessage is a received Feishu message, with the SDK's shape reduced to the fields we use.
//
// This conversion layer exists so the feishu business layer does not know about larkim: changing
// SDK versions or adding a callback mode later does not touch the business layer.
type InboundMessage struct {
	// AppID is the Feishu app that received this message, used to look up the AMA workspace.
	AppID string
	// MessageID looks like om_xxx; **deduplication relies on it** (Feishu delivers the same message more than once).
	MessageID string
	// ChatID is the chat the message is in.
	ChatID string
	// ChatType is p2p / group / topic_group.
	ChatType string
	// ThreadID is the topic ID; empty when not in a topic.
	ThreadID string
	// RootID / ParentID are the context messages in a reply.
	RootID   string
	ParentID string
	// SenderOpenID is the person who sent the message.
	SenderOpenID string
	// SenderType is the sender type: a real person is user, a bot is app **or** bot.
	//
	// ⚠️ **Which value a bot gets cannot be confirmed from the event side**: the SDK's EventSender
	// comment says "currently only user is supported", the event docs define the field as
	// `"user" or "bot"`, and the history message API gives `user/app/anonymous`. The three
	// contradict each other, and the docs have no sample payload of a bot sending a message. So
	// consumers must **accept both**; see isBotMessage in feishu/trigger.go.
	//
	// **It is no longer the main check against self-triggering** (that is now an exact open_id
	// match); it is only a fallback when "we do not know who we are". See the file header of
	// feishu/trigger.go for why.
	SenderType string
	// Text is the plain text **with @mentions removed**, ready to give to the agent.
	Text string
	// MessageType is Feishu's msg_type: text / post / interactive / merge_forward / ...
	//
	// It is carried because **two types cannot be identified or understood from Text alone**: the
	// pushed content of cards and merged forwards is incomplete (see the file header of expand.go),
	// and the business layer uses this to decide "whether to call the API once more to fetch the
	// real content". Do not detect the type by "does Text look like a placeholder": that is string
	// matching, and a user typing `[卡片]` themselves would be misjudged.
	MessageType string
	// Mentioned means this message @-mentions the bot. Group messages without an @ usually should not trigger the agent.
	Mentioned bool
	// CreatedAtMS is the time the message was sent (milliseconds).
	CreatedAtMS string
}

// MessageHandler handles one inbound message.
//
// **Returning an error makes Feishu retry**, so only return an error for "it failed this time,
// a resend might work". Already handled, or deliberately ignored (bot not @-mentioned), must
// both return nil, or Feishu keeps redelivering.
type MessageHandler func(ctx context.Context, msg *InboundMessage) error

// BotIdentity is the bot's own identity, used to check whether a message @-mentions it and to filter out its own messages.
type BotIdentity struct {
	OpenID string
	AppID  string
}

// ReceiverConfig is the config of one long connection.
type ReceiverConfig struct {
	// AppID / AppSecret are the Feishu app credentials; the long connection only needs these two.
	AppID     string
	AppSecret string

	// Bot is the bot identity, used for the @ check.
	Bot BotIdentity

	// Handler handles received messages.
	Handler MessageHandler

	// Leases is the lock store for leader election. **Required**: without it we cannot guarantee only one connection per app.
	Leases LeaseStore

	// Logger defaults to slog.Default() when empty.
	Logger *slog.Logger
}

func (c ReceiverConfig) validate() error {
	if strings.TrimSpace(c.AppID) == "" {
		return fmt.Errorf("%w: appID 不能为空", ErrInvalidArgument)
	}
	if strings.TrimSpace(c.AppSecret) == "" {
		return fmt.Errorf("%w: appSecret 不能为空", ErrInvalidArgument)
	}
	if c.Handler == nil {
		return fmt.Errorf("%w: Handler 不能为空", ErrInvalidArgument)
	}
	if c.Leases == nil {
		// No default: without leader election two instances might connect at once, which silently
		// loses messages and is harder to debug than failing to start. Better to fail here.
		return fmt.Errorf("%w: Leases 不能为空（长连接必须选主，见包注释）", ErrInvalidArgument)
	}
	return nil
}

// LeaseKey is the leader lock key for a Feishu app.
func LeaseKey(appID string) string {
	return "lark:longconn:" + strings.TrimSpace(appID)
}

// Receiver maintains the long connection of one Feishu app: grab lock → connect → back off and reconnect when dead → release lock on shutdown.
type Receiver struct {
	cfg ReceiverConfig
	log *slog.Logger

	// dial creates a long connection. When nil, defaultDial (the real SDK client) is used.
	// Tests inject it to plug in a fake connection and avoid dialing the real Feishu websocket.
	dial func() wsConn
}

// NewReceiver creates a receiver. **It does not connect**; call Run.
func NewReceiver(cfg ReceiverConfig) (*Receiver, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Receiver{cfg: cfg, log: log.With("appId", cfg.AppID)}, nil
}

// Run maintains the long connection, blocking until ctx is canceled.
//
// Full lifecycle: grab the lock (wait and retry if not available) → connect → back off and
// reconnect when the connection drops → disconnect at once and go back to grabbing the lock if
// the lease is lost. When ctx is canceled it releases the lock and returns nil.
func (r *Receiver) Run(ctx context.Context) error {
	backoff := backoffMin
	for {
		if ctx.Err() != nil {
			return nil
		}

		lease, ok, err := r.cfg.Leases.Acquire(ctx, LeaseKey(r.cfg.AppID), leaseTTL)
		if err != nil {
			r.log.Warn("抢长连接的锁失败，退避后重试", "error", err.Error(), "backoff", backoff.String())
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			backoff = nextBackoff(backoff)
			continue
		}
		if !ok {
			// The lock is held by another instance: this is **normal**, not an error. That instance is
			// receiving messages, and we just wait for it to exit. No doubling backoff here; poll at a
			// fixed interval.
			if !sleepCtx(ctx, leaseRetryGap) {
				return nil
			}
			continue
		}

		err = r.serveWithLease(ctx, lease)
		// On the shutdown path the original ctx is often already canceled; releasing with it would fail and we could only wait for the TTL to expire.
		if relErr := lease.Release(context.WithoutCancel(ctx)); relErr != nil {
			r.log.Warn("释放长连接的锁失败（会自己过期）", "error", relErr.Error())
		}
		if ctx.Err() != nil {
			return nil
		}

		if isInvalidCredential(err) {
			// Credentials are bad; reconnecting ten thousand times will not help. Exit and let the upper layer decide (usually waiting for the user to reconnect the app).
			r.log.Error("飞书凭证已失效，停止长连接；需要重新接入这个应用", "error", safeErr(err))
			return err
		}
		if err == nil {
			// The connection was canceled by the watchdog: another instance took over the lease (not a
			// disconnect). No doubling backoff; go straight back to the lock loop. If the lock is not
			// available, Acquire waits by leaseRetryGap on its own.
			continue
		}
		r.log.Warn("长连接失活，退避后重连", "error", safeErr(err), "backoff", backoff.String())
		if !sleepCtx(ctx, backoff) {
			return nil
		}
		backoff = nextBackoff(backoff)
	}
}

// serveWithLease connects and watches the connection while holding the lease; when it returns,
// the connection is always closed.
//
// nil means "ctx was canceled": either the process is shutting down, or the watchdog found the
// lease taken over by another instance and cut the connection (see keepLease). errConnDead means
// the connection is dead (half-open / kicked / error), and the caller should back off and
// reconnect. Invalid credentials return the SDK's fatal error.
func (r *Receiver) serveWithLease(ctx context.Context, lease Lease) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Renewal loop: once the lease is lost (e.g. this machine stalled for a long time and another
	// instance took the lock), we must **cut the connection at once**. Otherwise we stay connected
	// together with the new holder, which is exactly what we are guarding against.
	var renewWG sync.WaitGroup
	renewWG.Add(1)
	go func() {
		defer renewWG.Done()
		r.keepLease(runCtx, lease, cancel)
	}()

	dial := r.dial
	if dial == nil {
		dial = r.defaultDial
	}
	conn := dial()
	// Close once more as a fallback: conn.Run already closed it on dead / cancel; this is idempotent.
	defer func() {
		cancel()
		renewWG.Wait()
		conn.Close()
	}()

	r.log.Info("飞书长连接建立中")
	// Run blocks until: ctx is canceled (returns nil) or the connection dies (returns errConnDead).
	// Unlike the old implementation, it does not call the SDK's Start directly: Start blocks
	// forever once connected and does not return even when the connection dies. See the comments
	// on wsConn and sdkConn.
	return conn.Run(runCtx)
}

// defaultDial creates a connection wrapping the real Feishu SDK client.
func (r *Receiver) defaultDial() wsConn {
	return newSDKConn(r.cfg.AppID, r.cfg.AppSecret, r.log, r.dispatcher())
}

// keepLease renews periodically; if renewal fails it calls cancel to cut this round's connection.
func (r *Receiver) keepLease(ctx context.Context, lease Lease, cancel context.CancelFunc) {
	ticker := time.NewTicker(leaseRenew)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok, err := lease.Renew(ctx, leaseTTL)
			if err != nil {
				// One failed renewal is not fatal: the TTL is 4x the renewal interval, so there are two more chances.
				r.log.Warn("续约失败，还会再试", "error", err.Error())
				continue
			}
			if !ok {
				r.log.Warn("长连接的锁已被别的实例接管，主动断开")
				cancel()
				return
			}
		}
	}
}

// wsConn is the lifecycle handle of one Feishu long connection.
//
// Run blocks until:
//   - ctx is canceled (returns nil): the process is shutting down, or the watchdog found the
//     lease taken over by another instance;
//   - the connection dies (returns errConnDead): the peer disconnected, we were kicked, a read
//     or write failed, or the heartbeat failed.
//
// Close is idempotent and is used for fallback cleanup.
//
// The only reason this abstraction exists: once the Feishu SDK's `larkws.Client.Start()` is
// connected, it runs `select {}`, **blocking forever and not returning even when the connection
// dies** (see sdkConn.Run), so it cannot be used as a lifecycle signal directly. sdkConn uses
// "a dead-signal chan + running Start in a goroutine and waiting" to turn "dead" into an error
// that returns, and tests inject a fake connection.
type wsConn interface {
	Run(ctx context.Context) error
	Close()
}

// errConnDead means this long connection is dead; the caller should dial a new one (while holding the lease).
var errConnDead = errors.New("长连接失活，需要重连")

// sdkConn wraps a real `*larkws.Client`.
type sdkConn struct {
	client *larkws.Client
	log    *slog.Logger

	deadOnce   sync.Once
	dead       chan struct{}
	deadReason string
}

// newSDKConn creates the SDK websocket client and wires out its dead signal.
//
// The callbacks split the work like this:
//   - OnReady: connected; log the line "which events are subscribed";
//   - OnDisconnected: peer disconnected → markDead;
//   - OnError: connect/connection problems the SDK treats as errors → markDead;
//   - WithLogger: SDK internal logs go here. **Only this can catch half-open connections**: in
//     that case read blocks forever and OnDisconnected does not fire; only a failed heartbeat
//     ping write shows up, as a `ping failed` Warn (see sdkLogger).
func newSDKConn(appID, appSecret string, log *slog.Logger, events *dispatcher.EventDispatcher) *sdkConn {
	c := &sdkConn{
		log:  log,
		dead: make(chan struct{}),
	}
	c.client = larkws.NewClient(
		appID,
		appSecret,
		larkws.WithEventHandler(events),
		// Turn off SDK auto-reconnect: reconnecting must share the lease's lifetime (one connection
		// per app), see the package comment and the lock loop in Run.
		larkws.WithAutoReconnect(false),
		larkws.WithLogger(&sdkLogger{log: log, markDead: c.markDead}),
		larkws.WithOnReady(func() {
			// Include "which events are subscribed". **"Connected" and "what will be received" are two
			// different things**, and this log used to say only the former: an app whose connection is
			// fine but which receives no bot messages because of missing permissions had logs that looked
			// exactly like a healthy app (we hit this once).
			//
			// Writing out the event names has another use: the dispatcher only registers these events (the
			// unregistered ones are listed at the end of dispatcher()), so searching for this line answers
			// "why does recall do nothing" without reading code.
			// ⚠️ But **not registered ≠ quietly ignored**: on a long connection an unregistered event makes
			// the SDK reply 500 and Feishu drops the connection (see dispatcher()), so whatever is missing
			// from this line will break the connection.
			log.Info("飞书长连接已就绪，开始收消息", "events", subscribedEvents)
		}),
		larkws.WithOnDisconnected(func() {
			log.Warn("飞书长连接已断开")
			c.markDead("对端断开")
		}),
		larkws.WithOnError(func(err error) {
			log.Error("飞书长连接出错", "error", safeErr(err))
			c.markDead("连接出错: " + safeErr(err))
		}),
	)
	return c
}

// markDead marks the connection as dead. Safe to call many times (sync.Once); only the first reason is kept.
func (c *sdkConn) markDead(reason string) {
	c.deadOnce.Do(func() {
		c.deadReason = reason
		close(c.dead)
	})
}

// Run watches one connection until ctx is canceled or the connection dies.
//
// ⚠️ Do not just `return c.client.Start(ctx)`: after connecting and starting the heartbeat, Start
// runs `select {}` (zero cases) and **never returns**. When half-open or kicked, it hangs there
// forever, our reconnect loop never sees it exit, and a dead connection can only be recovered by
// restarting the process. So Start runs in a goroutine here, and we wait on ctx cancel / the dead
// signal / a fatal connect error ourselves.
//
// ⚠️ Close really closes the underlying websocket (the SDK's disconnect closes the connection),
// but **cannot bring back the Start goroutine**: `select {}` has no case that can be released. So
// we deliberately do not wait for Start to return: waiting would block forever. The cost is that
// each death/shutdown leaks two tiny blocked goroutines, Start and pingLoop (deaths are rare,
// about once every few days, so this is acceptable, and the SDK's shape gives no way to clean
// them up from outside).
func (c *sdkConn) Run(ctx context.Context) error {
	startErr := make(chan error, 1)
	go func() {
		startErr <- c.client.Start(ctx)
	}()
	select {
	case <-ctx.Done():
		// Shutdown or lease taken over: really disconnect the websocket. The old implementation could
		// not do this: the SDK does not watch ctx, and Close came after the never-returning Start, so
		// the watchdog's cancel did nothing, leaving a window where "two instances are connected at once".
		c.client.Close()
		return nil
	case <-c.dead:
		c.log.Warn("长连接失活信号", "reason", c.deadReason)
		c.client.Close()
		return errConnDead
	case err := <-startErr:
		// Only fatal errors during connect (invalid credentials etc.) make Start return before select{}.
		// err==nil should not happen in theory (it never returns once connected); treat it as dead to be safe.
		if err != nil {
			return err
		}
		return errConnDead
	}
}

func (c *sdkConn) Close() {
	if c != nil && c.client != nil {
		c.client.Close()
	}
}

// sdkLogger bridges the SDK's internal logs to slog, and picks out the "connection is dead"
// signal from them.
//
// A failed SDK heartbeat logs a Warn (`ping failed, err: ...`); read failures / closed
// connections log an Error. With a half-open connection (the peer quietly dropped it and we do
// not know), read blocks forever and the **only** signal is a failed heartbeat ping write, so
// among Warns only this one marks the connection dead. Other Warns (such as
// `receive unknown message`) are harmless; marking dead on them would cause needless reconnects.
type sdkLogger struct {
	log      *slog.Logger
	markDead func(reason string)
}

// Compile-time check that sdkLogger implements the SDK's logger interface (a wrong signature fails at compile time).
var _ larkcore.Logger = (*sdkLogger)(nil)

func (l *sdkLogger) Debug(_ context.Context, _ ...interface{}) {
	// Things like ping success are too noisy; drop them.
}

func (l *sdkLogger) Info(_ context.Context, args ...interface{}) {
	l.log.Debug(sdkMsg(args))
}

func (l *sdkLogger) Warn(_ context.Context, args ...interface{}) {
	msg := sdkMsg(args)
	l.log.Warn("飞书长连接 SDK 告警", "detail", msg)
	if strings.Contains(msg, "ping failed") {
		l.markDead("心跳失败（半开连接）")
	}
}

func (l *sdkLogger) Error(_ context.Context, args ...interface{}) {
	msg := sdkMsg(args)
	l.log.Error("飞书长连接 SDK 错误", "detail", msg)
	l.markDead("SDK 错误: " + msg)
}

// sdkMsg flattens the SDK's fmt.Sprintf-style arguments into one line.
func sdkMsg(args []interface{}) string {
	if len(args) == 0 {
		return ""
	}
	if format, ok := args[0].(string); ok {
		return strings.TrimSpace(fmt.Sprintf(format, args[1:]...))
	}
	return fmt.Sprint(args...)
}

// subscribedEvents are the events this long connection actually registered handlers for,
// **used only for logging**.
//
// ⚠️ It must match what dispatcher() registers. It is a constant because the SDK's
// EventDispatcher does not expose "which are registered"; we cannot ask, so we must declare it
// ourselves. When adding an event, change both places, or the log will lie (and "the log lies"
// is worse than "one log line missing", see docs/guides/backend.md).
//
// One direction of consistency is checked by machine: TestSubscribedEventsAllHaveHandlers runs
// dispatcher.Do for each event in this list; if the constant lists one the dispatcher did not
// register, it fails (that is exactly the shape that breaks the connection). The other direction
// (registered but missing from the constant) is not caught; this comment is what guards it.
const subscribedEvents = "im.message.receive_v1, im.message.reaction.created_v1, im.message.reaction.deleted_v1, im.chat.member.bot.added_v1, im.chat.member.bot.deleted_v1"

// dispatcher turns SDK events into our InboundMessage and passes them to Handler.
//
// The two empty strings in NewEventDispatcher("", "") are verificationToken and encryptKey:
// **with a long connection the SDK channel guarantees the source, so no signature check or
// decryption is done**, which is exactly why a long connection does not need these two credentials.
//
// ⚠️ **Every event subscribed in the console must have a handler registered here, even if the
// business does not care about it.**
// On a long connection "not registered" does not mean "ignored", it means **disconnected**: when
// the SDK's dispatcher.Do finds no handler it returns NotFoundEventHandlerErr
// (dispatcher.go:274); ws/client.go:638 gets the err and replies **HTTP 500** to Feishu, Feishu
// then closes this connection, and on our side we log
// "receive message failed: use of closed network connection" and reconnect.
// So **every time a user adds an emoji reaction or adds the bot to a group, the bot goes offline
// once**, and all messages during the reconnect window are lost (reaction: iss_example2, joining a
// group: iss_example3).
// ⚠️ This path differs from webhook: webhook replies 200 to unregistered events and nothing
// happens, so "it was fine in the webhook days" does not imply "it is fine with a long connection".
func (r *Receiver) dispatcher() *dispatcher.EventDispatcher {
	return dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
			msg := r.toInbound(event)
			if msg == nil {
				return nil
			}
			return r.cfg.Handler(ctx, msg)
		}).
		// Emoji reactions have nothing to do with AMA's business (they do not trigger a run or enter a
		// conversation), but **must be registered**, for the reason above: not registering breaks the
		// connection. An empty handler is what the SDK itself does
		// (ws/channel.go:542 also registers a do-nothing handler for messages it does not care about).
		OnP2MessageReactionCreatedV1(func(ctx context.Context, event *larkim.P2MessageReactionCreatedV1) error {
			r.log.Debug("忽略表情回应事件", "event", "im.message.reaction.created_v1")
			return nil
		}).
		OnP2MessageReactionDeletedV1(func(ctx context.Context, event *larkim.P2MessageReactionDeletedV1) error {
			r.log.Debug("忽略表情回应事件", "event", "im.message.reaction.deleted_v1")
			return nil
		}).
		// Bot added to / removed from a group (im.chat.member.bot.added_v1 / bot.deleted_v1).
		// **Must be registered** (not registering breaks the connection, see this function's doc
		// comment; in production, bot.added broke the connection every time the bot was added to a
		// group, iss_example3), and we checked that **under the current design there is no business
		// action to take**. This is not a temporary patch; it is the final shape under this design:
		//
		//   - Message routing goes msg.AppID → GetAppByAppID → WorkspaceID
		//     (dispatch in feishu/inbound.go). **The group itself is not bound to a workspace**, so
		//     joining a group has no binding to create (this corrects the guess in an old TODO, which
		//     said "joining a group should create a binding");
		//   - The memory of "which groups the bot is in" (rememberChat) is **message driven**: after
		//     joining, the first message in the group fills it in, so doing it early in the join event
		//     makes no sense;
		//   - Being removed from a group has no state to clear either: keeping the scope binding
		//     (SessionScope) means "if added back to the group, the earlier conversation can continue",
		//     which is the right meaning; the only cost of keeping the group memory is one stale item in
		//     the scheduled task dropdown (delivery will fail with an error that can be looked up). To
		//     really clear it we would need a delete method on Store; that is a feature, to be done when
		//     needed.
		OnP2ChatMemberBotAddedV1(func(ctx context.Context, event *larkim.P2ChatMemberBotAddedV1) error {
			r.log.Info("机器人被拉进群，当前无需处理（消息路由靠 AppID，不靠群），仅消化事件防止断连",
				"event", "im.chat.member.bot.added_v1")
			return nil
		}).
		OnP2ChatMemberBotDeletedV1(func(ctx context.Context, event *larkim.P2ChatMemberBotDeletedV1) error {
			r.log.Info("机器人被移出群，当前无需清理（群级没有绑定状态），仅消化事件防止断连",
				"event", "im.chat.member.bot.deleted_v1")
			return nil
		})
	// Events known to be unregistered and **not received currently** (not subscribed in the Feishu
	// console; a check of 72h of production logs found zero hits). They are listed so nothing is
	// silent: **once the console subscribes to any of them, it will show up as
	// "not found handler → 500 → disconnect"** (symptoms in this function's doc comment), and then
	// it must be registered here:
	//
	//   - im.message.recalled_v1 (recall): handling it for real would mean canceling the running run,
	//     which is a business decision; not done without a need. ⚠️ Also do not paper over it with an
	//     empty swallowing handler first: that would hide "the user recalled, the bot is still
	//     running" from the logs;
	//   - im.chat.member.user.added_v1 / user.deleted_v1 / user.withdrawn_v1
	//     (group member changes), im.chat.disbanded_v1 (disbanded), im.chat.updated_v1 (group
	//     info updated), im.message.read_v1 (read receipts),
	//     im.chat.access_event.bot.p2p_chat_entered_v1 (user enters the bot's direct chat):
	//     unrelated to AMA's business; deal with them once the console subscribes to them.
	//
	// card.action.trigger (card buttons) is not in this list and **does not cause disconnects**: on a
	// long connection card frames are dropped directly by the SDK's ws layer (in v3.10.0
	// ws/client.go returns for MessageTypeCard without looking up a handler or replying 500).
	// Handling card callbacks needs the SDK's card handler hook (in v3.10.0 WithCardHandler is still
	// commented out), which is separate from the dispatcher.
}

// toInbound reduces an SDK event to an InboundMessage, and also checks @ and filters the bot's
// own messages. nil means this message needs no handling.
func (r *Receiver) toInbound(event *larkim.P2MessageReceiveV1) *InboundMessage {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return nil
	}
	m := event.Event.Message
	senderOpenID := ""
	senderType := ""
	if event.Event.Sender != nil {
		if event.Event.Sender.SenderId != nil {
			senderOpenID = str(event.Event.Sender.SenderId.OpenId)
		}
		senderType = str(event.Event.Sender.SenderType)
	}
	// Filter out messages the bot sent itself. Otherwise the bot's reply would be treated as a new
	// message and trigger another round, and it would chat with itself forever.
	//
	// ⚠️ **This defense depends on Bot.OpenID having a value, and it may be empty** (see botIdentity
	// in feishu/supervisor.go: left empty when it cannot be read, and then this if never matches).
	// In @-trigger mode mentionsBot still backs it up (the bot's replies usually do not @ itself),
	// but **auto-trigger without @ removes that backup too**. So SenderType is passed along as well,
	// and the business layer checks again on the no-@ path; see feishu/trigger.go.
	if senderOpenID != "" && senderOpenID == strings.TrimSpace(r.cfg.Bot.OpenID) {
		return nil
	}

	text, mentioned := parseMessageText(str(m.Content), str(m.MessageType), m.Mentions, r.cfg.Bot)
	// A direct chat counts as @-mentioning the bot: the user came to talk to it specifically, so no @ should be required.
	if strings.TrimSpace(str(m.ChatType)) == "p2p" {
		mentioned = true
	}

	return &InboundMessage{
		AppID:        r.cfg.AppID,
		MessageID:    str(m.MessageId),
		ChatID:       str(m.ChatId),
		ChatType:     str(m.ChatType),
		ThreadID:     str(m.ThreadId),
		RootID:       str(m.RootId),
		ParentID:     str(m.ParentId),
		SenderOpenID: senderOpenID,
		SenderType:   senderType,
		Text:         text,
		MessageType:  strings.TrimSpace(str(m.MessageType)),
		Mentioned:    mentioned,
		CreatedAtMS:  str(m.CreateTime),
	}
}

// isInvalidCredential reports whether the error is "bad credentials"; reconnecting makes no sense for those.
func isInvalidCredential(err error) bool {
	var clientErr *larkws.ClientError
	if errors.As(err, &clientErr) && clientErr != nil {
		return clientErr.Code == codeInvalidCredential || clientErr.Code == codeInvalidAppID
	}
	return false
}

// nextBackoff doubles the backoff, capped at backoffMax.
func nextBackoff(d time.Duration) time.Duration {
	next := d * 2
	if next > backoffMax {
		return backoffMax
	}
	return next
}

// sleepCtx sleeps for a while; returns false early if ctx is canceled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// safeErr turns an error into a loggable string; also safe with nil.
func safeErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// str dereferences the SDK's *string fields; nil becomes an empty string.
func str(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}
