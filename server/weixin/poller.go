package weixin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/chyroc/ama/server/channel"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/gopkg/arkframe"
	wx "github.com/chyroc/ama/server/shared/infra/weixin"
)

const (
	// turnTimeout caps how long one turn may run before the answer (whatever there is) is sent.
	turnTimeout = 60 * time.Minute
	// shutdownGrace is how long a stopping poller waits for turns in progress before cancelling them.
	shutdownGrace = 20 * time.Second
	// typingEvery is how often the typing indicator is renewed while a turn runs (the reference uses 5s).
	typingEvery = 5 * time.Second
	// Retry policy of the long poll, from the reference client: a short pause after a failure, a long one
	// after three in a row.
	retryDelay      = 2 * time.Second
	backoffDelay    = 30 * time.Second
	maxFailsInARow  = 3
	finishTimeout   = 30 * time.Second
	notifyTimeout   = 5 * time.Second
	expiredAccount  = "微信登录已失效（机器人被解绑或在别处登录），请重新扫码接入"
	pollLeasePrefix = "weixin:poll:"
)

// PollerConfig wires a Poller.
type PollerConfig struct {
	Store         Store
	Channel       channel.Store
	Conversations channel.Conversations
	Client        *wx.Client
	Leases        channel.LeaseStore
	Logger        *slog.Logger
	Now           func() time.Time
}

// Poller receives and answers messages for every connected WeChat bot. It is the channel.Source of the
// WeChat channel: Run keeps one long-poll loop per active account, and each loop runs on exactly one
// instance (channel.RunWithLease).
type Poller struct {
	cfg PollerConfig
	log *slog.Logger
	now func() time.Time
}

// NewPoller creates a Poller.
func NewPoller(cfg PollerConfig) (*Poller, error) {
	if cfg.Store == nil || cfg.Channel == nil || cfg.Conversations == nil || cfg.Client == nil || cfg.Leases == nil {
		return nil, fmt.Errorf("%w: Poller 的依赖不全", ErrInvalidArgument)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Poller{cfg: cfg, log: log.With("channel", string(channel.Weixin)), now: now}, nil
}

// Run blocks until ctx is done.
func (p *Poller) Run(ctx context.Context) error {
	return channel.NewSupervisor(p, channel.DefaultReconcileInterval, p.log).Run(ctx)
}

// Name implements channel.Source.
func (p *Poller) Name() string { return string(channel.Weixin) }

// ListAccounts implements channel.Source. The version changes with the token and base URL, so scanning
// again restarts the loop with the new credentials.
func (p *Poller) ListAccounts(ctx context.Context) ([]channel.Account, error) {
	recs, err := p.cfg.Store.ListActiveAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]channel.Account, 0, len(recs))
	for _, r := range recs {
		sum := sha256.Sum256([]byte(r.BotToken + "\x00" + r.BaseURL))
		out = append(out, channel.Account{Key: r.ID, Version: hex.EncodeToString(sum[:8]), Value: r})
	}
	return out, nil
}

// RunAccount implements channel.Source.
func (p *Poller) RunAccount(ctx context.Context, account channel.Account) error {
	log := p.log.With("account", account.Key)
	return channel.RunWithLease(ctx, p.cfg.Leases, pollLeasePrefix+account.Key, log, func(ctx context.Context) error {
		return p.poll(ctx, account.Key, log)
	})
}

// poll is one account's long-poll loop while this instance holds the account's lease.
func (p *Poller) poll(ctx context.Context, accountID string, log *slog.Logger) error {
	// Read the account again now: the cursor saved by the previous holder (maybe another instance) is
	// newer than the copy the supervisor listed.
	acc, err := p.cfg.Store.GetAccount(ctx, accountID)
	if err != nil {
		if errors.Is(err, ErrAccountNotFound) {
			return channel.Stop(err)
		}
		return err
	}
	if acc.Status == apimodel.WeixinAccountExpired {
		return channel.Stop(errors.New(expiredAccount))
	}
	log.Info("开始接收微信消息", "bot", acc.BotID, "workspace", acc.WorkspaceID)
	p.notify(ctx, acc, true, log)

	work := newDispatcher(ctx, func(ctx context.Context, msg wx.Message) { p.handle(ctx, accountID, msg) })
	defer func() {
		work.stop(shutdownGrace)
		p.notify(context.WithoutCancel(ctx), acc, false, log)
	}()

	cursor := acc.SyncCursor
	timeout := wx.DefaultLongPollTimeout
	fails := 0
	for ctx.Err() == nil {
		resp, err := p.cfg.Client.GetUpdates(ctx, acc.BaseURL, acc.BotToken, cursor, timeout)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			if wx.IsStaleToken(err) {
				log.Warn("微信 token 已失效，停止接收；需要重新扫码接入", "bot", acc.BotID)
				if serr := p.cfg.Store.SetAccountStatus(context.WithoutCancel(ctx), acc.ID,
					apimodel.WeixinAccountExpired, expiredAccount, apimodel.FormatISO(p.now())); serr != nil {
					log.Warn("标记微信账号失效失败", "error", serr.Error())
				}
				return channel.Stop(err)
			}
			fails++
			wait := retryDelay
			if fails >= maxFailsInARow {
				wait, fails = backoffDelay, 0
			}
			log.Warn("拉微信消息失败，稍后重试", "error", err.Error(), "wait", wait.String())
			if !channel.Sleep(ctx, wait) {
				return nil
			}
			continue
		}
		fails = 0
		if resp.LongPollingTimeoutMs > 0 {
			timeout = time.Duration(resp.LongPollingTimeoutMs) * time.Millisecond
		}
		// Save the cursor before handling the batch. A crash in between loses these messages instead of
		// answering them twice; dedup (channel.Store) catches a batch delivered again anyway.
		if resp.Cursor != "" && resp.Cursor != cursor {
			cursor = resp.Cursor
			if err := p.cfg.Store.SetAccountCursor(ctx, acc.ID, cursor); err != nil {
				log.Warn("保存微信同步游标失败，重启后可能重复收到这批消息（会被去重挡住）", "error", err.Error())
			}
		}
		for _, msg := range resp.Msgs {
			work.dispatch(msg)
		}
	}
	return nil
}

func (p *Poller) notify(ctx context.Context, acc AccountRecord, start bool, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	var err error
	if start {
		err = p.cfg.Client.NotifyStart(ctx, acc.BaseURL, acc.BotToken)
	} else {
		err = p.cfg.Client.NotifyStop(ctx, acc.BaseURL, acc.BotToken)
	}
	if err != nil {
		log.Debug("通知微信开始/停止接收失败（不影响收发）", "start", start, "error", err.Error())
	}
}

// handle answers one inbound message. It runs on the conversation's own worker (see dispatcher), so the
// messages of one user are answered in order and never run two turns on the same session at once.
func (p *Poller) handle(ctx context.Context, accountID string, msg wx.Message) {
	if msg.MessageType == wx.MessageTypeBot {
		return
	}
	log := p.log.With("account", accountID, "from", msg.FromUserID)
	if msg.GroupID != "" {
		log.Info("收到微信群消息，暂不支持群聊，忽略")
		return
	}
	id := messageID(msg)
	if id == "" || msg.FromUserID == "" {
		log.Warn("微信消息缺少 id 或发送人，忽略")
		return
	}
	log = log.With("message", id)

	fresh, err := p.cfg.Channel.ClaimInbound(ctx, channel.InboundMessage{
		Channel: channel.Weixin, AccountID: accountID, MessageID: id,
		ConversationID: msg.FromUserID, CreatedAt: p.now(),
	})
	if err != nil {
		log.Warn("记录入站消息失败，这条不处理", "error", err.Error())
		return
	}
	if !fresh {
		log.Debug("重复投递，已忽略")
		return
	}
	status := channel.InboundProcessed
	if err := p.answer(ctx, accountID, msg, log); err != nil {
		status = channel.InboundFailed
		log.Error("处理微信消息失败", "error", err.Error())
	}
	if err := p.cfg.Channel.MarkInbound(context.WithoutCancel(ctx), channel.Weixin, accountID, id, status); err != nil {
		log.Warn("更新入站消息状态失败", "error", err.Error())
	}
}

// answer runs the agent for one message and sends the reply. A returned error marks the message failed.
func (p *Poller) answer(ctx context.Context, accountID string, msg wx.Message, log *slog.Logger) error {
	// Read the account per message: the bound agent may have changed since the loop started.
	acc, err := p.cfg.Store.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if err := p.cfg.Store.TouchAccountInbound(ctx, acc.ID, apimodel.FormatISO(p.now())); err != nil {
		log.Debug("更新最近收到消息的时间失败", "error", err.Error())
	}
	reply := func(text string) error { return p.send(ctx, acc, msg, text, log) }

	text := messageText(msg)
	if text == "" {
		return reply(replyUnsupported)
	}
	switch command(text) {
	case cmdNew:
		if err := p.cfg.Channel.ResetScope(ctx, channel.Weixin, acc.ID, scopeKey(msg)); err != nil {
			return err
		}
		return reply(replyNewChat)
	case cmdHelp:
		return reply(replyHelp)
	}
	if acc.AgentID == "" {
		return reply(replyNotBound)
	}
	if p.cfg.Conversations.AgentDisabled(ctx, acc.AgentID) {
		log.Info("绑定的 agent 已停用，不触发这一轮", "agent", acc.AgentID)
		return reply(channel.ReplyAgentDisabled)
	}

	runID, err := channel.EnsureSession(ctx, p.cfg.Channel, p.cfg.Conversations, channel.SessionRequest{
		Scope: channel.ScopeKey{
			Channel: channel.Weixin, AccountID: acc.ID, Key: scopeKey(msg), AgentID: acc.AgentID,
		},
		WorkspaceID: acc.WorkspaceID,
		Detail:      "微信单聊",
		Now:         p.now(),
	}, log)
	if err != nil {
		_ = reply(replySendFailed)
		return err
	}
	log = log.With("run", runID, "agent", acc.AgentID)
	log.Info("微信消息进来了，开跑")

	stopTyping := p.startTyping(ctx, acc, msg, log)
	turnCtx, cancel := context.WithTimeout(ctx, turnTimeout)
	res, err := channel.RunTurn(turnCtx, channel.TurnConfig{
		Conversations: p.cfg.Conversations,
		RunID:         runID,
		BuildEvent: func(context.Context) (json.RawMessage, error) {
			return arkframe.UserMessage(text, harnessBlock(msg))
		},
	})
	cancel()
	stopTyping()
	if err != nil {
		_ = reply(replySendFailed)
		return err
	}
	if res.StreamErr != nil {
		log.Warn("事件流异常结束", "error", res.StreamErr.Error())
	}
	if ctx.Err() != nil {
		// The poller is stopping (deploy) and the turn was cut short.
		_ = p.send(context.WithoutCancel(ctx), acc, msg, replyInterruptRun, log)
		return ctx.Err()
	}
	if res.Interrupted {
		// Redirected by a newer message (for example from the web chat page on the same session).
		return nil
	}
	answer := cleanReply(res.Answer)
	if answer == "" {
		log.Warn("这一轮没有产生回复，将发兜底提示",
			"frames", res.Frames, "messages", res.Messages, "stop_reason", res.StopReason,
			"failed_upstream", res.FailedUpstream, "stream_error", res.StreamErr != nil)
		answer = channel.FallbackAnswer(res)
	}
	return reply(answer)
}

// send sends text to the message's sender, split into WeChat-sized pieces, echoing its context token.
func (p *Poller) send(ctx context.Context, acc AccountRecord, msg wx.Message, text string, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	for i, piece := range splitReply(text, maxReplyRunes) {
		if _, err := p.cfg.Client.SendText(ctx, acc.BaseURL, acc.BotToken, msg.FromUserID, msg.ContextToken, piece); err != nil {
			return fmt.Errorf("发微信消息失败（第 %d 段）: %w", i+1, err)
		}
	}
	log.Debug("已回复微信消息", "chars", len([]rune(text)))
	return nil
}

// startTyping shows "typing" while the turn runs and returns a function that clears it. The indicator is
// only a nicety: every failure here is ignored.
func (p *Poller) startTyping(ctx context.Context, acc AccountRecord, msg wx.Message, log *slog.Logger) func() {
	ticket, err := p.cfg.Client.GetTypingTicket(ctx, acc.BaseURL, acc.BotToken, msg.FromUserID, msg.ContextToken)
	if err != nil || ticket == "" {
		if err != nil {
			log.Debug("取输入状态票据失败，不显示「正在输入」", "error", err.Error())
		}
		return func() {}
	}
	typing := func(ctx context.Context, status int) {
		_ = p.cfg.Client.SendTyping(ctx, acc.BaseURL, acc.BotToken, msg.FromUserID, ticket, status)
	}
	typingCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(typingEvery)
		defer t.Stop()
		typing(typingCtx, wx.TypingStart)
		for {
			select {
			case <-typingCtx.Done():
				return
			case <-t.C:
				typing(typingCtx, wx.TypingStart)
			}
		}
	}()
	return func() {
		stop()
		wg.Wait()
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
		defer cancel()
		typing(cctx, wx.TypingCancel)
	}
}

// dispatcher runs messages on one worker per sender, so the poll loop never waits for a turn and one
// user's messages are answered in order.
//
// The queues are in-process on purpose: only the instance holding the account's lease polls it, so the
// work in progress is that instance's alone. What is lost if it dies is the turns in progress; their
// messages stay "pending" in the dedup table (see README "Known limits").
type dispatcher struct {
	ctx    context.Context
	cancel context.CancelFunc
	handle func(context.Context, wx.Message)

	mu     sync.Mutex
	queues map[string][]wx.Message
	wg     sync.WaitGroup
}

func newDispatcher(parent context.Context, handle func(context.Context, wx.Message)) *dispatcher {
	// Turns outlive the poll loop's ctx (a lost lease must not cut a running answer); stop() cancels them
	// after a grace period.
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	return &dispatcher{ctx: ctx, cancel: cancel, handle: handle, queues: map[string][]wx.Message{}}
}

func (d *dispatcher) dispatch(msg wx.Message) {
	key := strings.TrimSpace(msg.FromUserID)
	d.mu.Lock()
	defer d.mu.Unlock()
	if q, ok := d.queues[key]; ok {
		d.queues[key] = append(q, msg)
		return
	}
	d.queues[key] = []wx.Message{msg}
	d.wg.Add(1)
	go d.work(key)
}

func (d *dispatcher) work(key string) {
	defer d.wg.Done()
	for {
		d.mu.Lock()
		q := d.queues[key]
		if len(q) == 0 {
			delete(d.queues, key)
			d.mu.Unlock()
			return
		}
		msg := q[0]
		d.queues[key] = q[1:]
		d.mu.Unlock()
		d.handle(d.ctx, msg)
	}
}

// stop waits up to grace for the work in progress, then cancels what is left and waits for it to end.
func (d *dispatcher) stop(grace time.Duration) {
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
		d.cancel()
		<-done
	}
	d.cancel()
}
