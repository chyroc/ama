// Inbound message orchestration: one sentence in a Feishu chat → the agent runs one turn → three pieces of feedback go back to the user.
//
// # Where this file sits in the whole path
//
//	shared/infra/lark long connection  →  this file  →  core.Service (create session / send message / subscribe to events)
//	                        ↓
//	                   shared/infra/lark add reaction / send chain of thought / send answer
//
// This package and core are deliberately coupled only by an ID string (see the comment in core/agent.go), so we do not import
// core here. The three methods we need are pulled into the Conversations interface, and cmd/amad passes *core.Service in at wiring time.
//
// # The three things a turn gives the user
//
//	1. OnIt reaction    added on receipt, so the user knows right away the bot has started working (reply.go)
//	2. Chain of thought  thinking and tool calls pushed while running; closed when done, reaction removed (turnprogress.go)
//	3. Separate answer   after closing, the final answer is sent as a separate message (reply.go)
//
// **Only the third one must succeed.** The first two are enhancements: a reaction that fails or a chain of thought that cannot be pushed only
// degrades the experience, and must not stop the user from getting an answer. So their failures are only logged.
//
// # Dedup: Feishu delivers the same message more than once
//
// Network jitter, slow handling on our side, or an error returned by us: any of these makes Feishu deliver the same message again.
// Without dedup the same sentence runs the agent twice, and the user sees two replies.
//
// Dedup relies on the unique index (lark_message_id) of the messageBindings table. Three states, three ways to handle them:
//
//	pending    still running → ack directly, do not run again
//	processed  finished → ack directly
//	failed     last attempt died → let the retry through
//
// **Returning an error makes Feishu redeliver**, so "already handled" and "ignored on purpose" must both return nil.
// Getting this wrong results in an endless redelivery loop, not an error.
//
// # Order: subscribe first, then send the message
//
// core events **are only stored when someone is subscribed** (see the comment in core/chat.go and "what is still missing" in
// core/README.md). So StreamRunEvents must come before AppendRunEvents. The other way round,
// the part of the reply before the subscription is set up is neither seen nor stored.

package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chyroc/ama/server/feishu/feishumodel"

	"github.com/chyroc/ama/server/shared/gopkg/arkframe"
	"github.com/chyroc/ama/server/shared/infra/lark"
	"github.com/chyroc/ama/server/shared/infra/secret"
)

const (
	// runTimeout is the upper limit for how long the Feishu layer waits for one turn's reply.
	//
	// The agent may call tools and may think for a long time, but it must not hold on forever. After the timeout the chain of thought is closed,
	// whatever content has accumulated is still sent as the answer, instead of staying at "thinking" forever.
	// On expiry it only ends the event subscription; no interrupt is sent to Ark.
	runTimeout = 60 * time.Minute

	// replyFinishTimeout is the time budget for the finishing steps (close the chain of thought, remove the reaction, send the answer).
	//
	// **It gets its own budget and does not inherit runCtx**: by the time we reach finishing, runCtx has often already timed out
	// (long tasks are exactly the ones that most need finishing), and sending the answer with it would fail outright.
	// Then the user gets nothing, and the log only has a line saying context deadline exceeded.
	replyFinishTimeout = 30 * time.Second

	// redirectLockWait is the upper limit for waiting for the previous turn to hand back the serial lock during a redirect.
	//
	// By this point the previous turn **has already been interrupted**; leaving only takes a few local steps (receive the final frame, close the chain of thought,
	// delete the inflight record), measured at a few hundred milliseconds. 15 seconds leaves room for "that goroutine is stuck on
	// a remote call", not for waiting for a turn to really finish. If we really cannot get it, fall back to the queued notice,
	// which is better than leaving the user waiting.
	redirectLockWait = 15 * time.Second

	// redirectLockPoll is the polling interval while waiting for the lock.
	//
	// Kept small: the previous turn stepping aside is a local action, 100ms is enough when it is fast, and a larger interval wastes a whole window.
	redirectLockPoll = 100 * time.Millisecond
)

// The result of handling an inbound message, written to messageBinding.Status.
//
// **The real definitions are in feishumodel**: the PG implementation (repository/dao/feishudao) and MemStore must use
// the same literals. Writing them twice will drift sooner or later, and drift does not error; it just means "querying by the new value finds nothing".
const (
	inboundPending   = feishumodel.InboundPending
	inboundProcessed = feishumodel.InboundProcessed
	inboundFailed    = feishumodel.InboundFailed
)

// Claim state of an unfinished turn, written to InflightTurnRecord.Status.
//
// **It is separate from the conversation state**: how far the conversation has gone is driven by Ark events in the runs table; these two values only
// make the startup scans of multiple instances mutually exclusive. Every instance scans, and without CAS two instances would write to the same
// card at once, and the user would see the content jump back and forth.
//
// The real definitions are also in feishumodel, for the reason given in the group above.
const (
	inflightOpen     = feishumodel.InflightOpen
	inflightResuming = feishumodel.InflightResuming
)

// Conversations is what this package needs from core.
//
// It is an interface instead of a direct dependency on core: the two modules are coupled only by ID strings, and a reverse import would
// break that decoupling. The method signatures match the methods of the same name on *core.Service, which is passed in directly at wiring time.
type Conversations interface {
	// CreateConversation creates a new session; the returned run.ID is the Ark session id.
	CreateConversation(ctx context.Context, workspaceID, agentID string) (Run, error)
	// CreateFeishuConversation is the same, but marks the Run as trigger=feishu and carries a source description
	// (chat name / direct chat). **The Feishu path always uses it**, otherwise the run history cannot tell whether a session was
	// started from the web page or from a Feishu chat.
	CreateFeishuConversation(ctx context.Context, workspaceID, agentID, detail string) (Run, error)
	// AppendRunEvents sends events into the session (we only send user.message).
	AppendRunEvents(ctx context.Context, runID string, raw []json.RawMessage) ([]json.RawMessage, error)
	// StreamRunEvents subscribes to the session's event stream, blocking until the stream ends or ctx is cancelled.
	StreamRunEvents(ctx context.Context, runID string, onFrame func(raw []byte) error) error
	// AgentsBoundToFeishuApp lists the agent IDs bound to a Feishu app.
	AgentsBoundToFeishuApp(ctx context.Context, feishuAppID string) ([]string, error)

	// AgentDisabled answers "has this agent been disabled".
	//
	// **It must be asked before running**; we cannot rely on the error from AppendRunEvents. core does block
	// disabled agents, but that is an error, and when this package sees an error it only logs and returns,
	// so **people in the chat get nothing**, which looks like "the bot is ignoring me". Silent failures like this are the hardest to debug.
	// Asking first lets us reply with a human sentence; see how dispatch uses it.
	//
	// Not found / store query failed always returns false (fail-open): a store hiccup should not make every Feishu bot
	// go silent together, and "agent does not exist" is handled elsewhere (session creation reports it).
	AgentDisabled(ctx context.Context, agentID string) bool

	// AgentFollowUpQueues answers "should this agent queue the next message sent in a row".
	//
	// true = queue (send it after this turn finishes), false = redirect (interrupt the current turn and do the new one).
	//
	// # Why it returns bool instead of an enum
	//
	// Same as AgentDisabled and RunStatus: this package deliberately does not know core's domain model
	// (see the comment on Conversations). There are only two options here, so bool is enough. If a third option is ever needed,
	// switch to a string then; the change will be local.
	//
	// **Not found / store query failed always returns false (redirect)**, matching the default side of apimodel.FollowUpMode:
	// the empty string of that enum means redirect. Failing open to the same side avoids "the store hiccuped once
	// and this agent's behavior flipped".
	AgentFollowUpQueues(ctx context.Context, agentID string) bool

	// AgentShowsProgress answers "should this agent show detailed work in Feishu".
	//
	// true = create a chain-of-thought message and lay out thinking and tool calls while running;
	// false = this turn only has "reaction + final answer"; no chain-of-thought message appears in the chat at all.
	//
	// Same shape as AgentFollowUpQueues: this package deliberately does not know core's domain model,
	// so the apimodel.ProgressDetail enum is flattened into a bool at this boundary.
	//
	// **Not found / store query failed always returns true (show)**, matching the default side of apimodel.ProgressDetail:
	// the empty string of that enum means "show". Failing open to the same side means a store hiccup does not flip this
	// agent's behavior, and the flipped direction would be exactly "silently show nothing",
	// the kind of breakage users are least likely to notice themselves.
	AgentShowsProgress(ctx context.Context, agentID string) bool

	// InterruptRun interrupts this turn, **sending only the interrupt, not a new message**.
	//
	// ⚠️ **Do not switch back to the combined "interrupt + immediately send a new message" primitive** (RedirectRun in core).
	// That primitive is for the chat page, where the person sending the message and the person receiving the answer are the same (the browser keeps
	// the SSE open). The Feishu path is different: once the interrupt takes effect, the original subscriber's stream ends and it leaves, and
	// **nobody is waiting** for the answer to the new message. In practice the replies of two turns got mixed up; see the comment on interruptForRedirect.
	//
	// The new message of a redirect always goes through the normal AppendRunEvents path (after getting the serial lock);
	// that path subscribes, adds the context block, and anchors the answer back to the user's own message.
	InterruptRun(ctx context.Context, runID string) error

	// SyncRunEvents actively fetches the tail events of this session from Ark, and computes the final state along the way.
	//
	// Recovery after a service restart relies on it to fill the gap: our subscription broke, but **the turn keeps running on the Ark side**
	// (inference runs on the Ark server, and tools are claimed again after the worker restarts). So recovery does not resend the user message
	// (that would make the agent answer the same question twice); it pulls back what Ark produced during the gap.
	SyncRunEvents(ctx context.Context, runID string) error

	// RunStatus returns the session's current status string (running / idle / requires_action /
	// rescheduling / terminated).
	//
	// Recovery branches on it: if it is already final, finalize and finish; otherwise reattach the stream and keep listening.
	// It returns a bare string instead of a core type so this interface is self-contained; see the comment on this type.
	RunStatus(ctx context.Context, runID string) (string, error)
}

// Run is a minimal projection of apimodel.Run from core, with only the fields this package uses.
//
// We do not use apimodel.Run directly so that the Conversations interface is self-contained: this package does not need to know
// core's domain model, only a session ID.
type Run struct {
	ID string
}

// MessageSender is the ability to send cards, provided by shared/infra/lark.
type MessageSender interface {
	ReplyCard(ctx context.Context, token, messageID string, card map[string]any, opt lark.ReplyCardOption) (*lark.SentMessage, error)
	UpdateCard(ctx context.Context, token, messageID string, card map[string]any) error
}

// TokenSource exchanges a tenant_access_token. Sending messages needs it.
type TokenSource interface {
	TenantAccessToken(ctx context.Context, appID, appSecret string) (string, error)
}

// InboundConfig wires the inbound path.
type InboundConfig struct {
	// Store is used to look up apps, look up secrets, and record dedup.
	Store Store
	// Conversations is core's session ability.
	Conversations Conversations
	// Sender sends cards.
	Sender MessageSender
	// Tokens exchanges tokens.
	Tokens TokenSource

	// Reactor adds/removes reactions.
	//
	// May be nil: then OnIt is not added and everything else works as usual. A missing reaction only loses one "got it" hint,
	// and must not stop the whole turn from running.
	Reactor Reactor

	// Progress is the state machine store for chain-of-thought delivery.
	//
	// May be nil: then this turn has no chain of thought, only "reaction + final answer". **This is degradation, not failure**:
	// keeping "the user gets an answer" matters more than showing the thinking process.
	Progress ProgressStore

	// COT is Feishu's chain-of-thought message ability.
	//
	// May be nil: then progress uses the fallback card (see transport.go). Note it and Progress are separate things:
	// the former is a remote API, the latter a local state machine; missing either one only degrades, it does not error.
	COT COTMessenger

	// Fetcher prefetches message context with the bot's own credentials (card body, sub-messages of a merged forward, chat name).
	//
	// May be nil: then the `<harness:lark>` block falls back to the old form of "only id + command to fetch history", and
	// cards are still `[卡片]`. **This is degradation, not failure**: the whole value of prefetching is letting the agent save
	// a round trip and see more clearly; failing to fetch must not affect whether the user gets a reply. See larkcontext.go.
	Fetcher MessageFetcher

	// Logger defaults to slog.Default() when empty.
	Logger *slog.Logger
	// Now defaults to time.Now when empty; tests can inject it.
	Now func() time.Time
}

// Inbound handles inbound messages.
type Inbound struct {
	cfg InboundConfig
	log *slog.Logger
	now func() time.Time

	// localTurns records which sessions' turn locks this process currently holds.
	//
	// # It serves exactly one purpose: do not let reclaim take back a lock that is still alive
	//
	// reclaimFinishedTurn uses "is the run final" to decide whether a lock is left over, and **that test is not enough
	// under session reuse**: a turn lives longer than one inference (after answering it still closes the chain of thought, waits for the ordering gate,
	// sends the answer), and the run is already idle during that time. Looking only at idle would hand the lock to a second goroutine,
	// and two subscribers would consume the same event stream at once, each collecting the full text; the user gets duplicate, mixed-up replies.
	//
	// With this table, nothing is reclaimed while this process itself holds the lock; leftover locks across instances can still be reclaimed
	// (that is the real problem reclaim solves: another instance crashed and its record was not deleted).
	//
	// **Deliberately kept in process only**: it expresses "a goroutine in this process is running",
	// which is itself an in-process fact, and it should disappear when the process dies. Mutual exclusion across instances is still
	// covered by the inflight_turns table + staleAfter; see the distributed section of CLAUDE.md.
	localTurns sync.Map // runID → struct{}

	// drops writes rate-limited Info logs for messages that were "received but did not trigger". See dropped.go.
	drops *dropReporter
}

// holdsTurn answers "does this process hold this session's turn lock right now".
func (in *Inbound) holdsTurn(runID string) bool {
	_, ok := in.localTurns.Load(runID)
	return ok
}

// beginTurn grabs this session's turn lock, **first reserving a slot in process, then grabbing the one in the store**.
//
// # ⚠️⚠️ The order is "register first, then grab the lock"; do not reverse it
//
// Intuitively it should be "register after grabbing", but that leaves a **gap that cannot be closed**, and it is exactly
// the root cause of the flaky failure of `TestConcurrentMessagesDoNotDuplicateReplies`:
//
//	A: TryBeginTurn succeeds (the lock inside Store is already released here)
//	B: TryBeginTurn fails (A holds the record in the store)
//	B: holdsTurn(A's run) → false  ← ⚠️ A has not registered yet
//	B: reclaimFinishedTurn → run is idle → judged a "leftover lock", A's live lock is reclaimed
//	B: TryBeginTurn retry → succeeds  ← two goroutines hold the lock at once
//	A: localTurns.Store             ← too late
//
// Putting the registration right after `TryBeginTurn` (not a single statement in between) **only narrows the gap**;
// it cannot close it: when `Store.TryBeginTurn` returns, its internal mutex has already been released,
// so between "A returns" and "A registers" there is always a moment where the scheduler can preempt.
// That is how it failed in practice: after narrowing, 6 rounds × count=15 still hit it 13 times,
// and a probe (atomic counter, no timing change) captured that `reclaimed=1`.
//
// So it was changed to **reserve first**: once `LoadOrStore` succeeds, this session already has an owner in this process,
// whether or not the store lock is grabbed afterwards, `holdsTurn` can see it,
// so the reclaim gate can never misjudge our own live lock.
//
//	Reservation fails (someone in this process is already running) → treat it as "not grabbed" and let the caller redirect/queue
//	Reservation succeeds but the store lock is not grabbed → **the reservation must be removed**, or this session locks itself out
//
// ⚠️ The second item is the only risk of this approach: miss the removal once, and that session can never start a new turn
// in this process until it restarts, with **zero errors**; users just feel "the bot is ignoring me".
// The two `Delete` calls below are exactly that; do not remove them.
//
// ⚠️ It **cannot replace the store lock**: `localTurns` only works inside this process; mutual exclusion across instances still
// relies on the `inflight_turns` table (see the distributed section of CLAUDE.md). One is inside, one is outside.
//
// After grabbing, **someone must remove it**: the caller does `localTurns.Delete` in a defer,
// cleared together with the inflight record in the store (see the defer in runAndReply for the order).
func (in *Inbound) beginTurn(
	ctx context.Context, rec InflightTurnRecord, staleAfter time.Duration,
) (bool, error) {
	// 1. Reserve in process first. If someone already holds it, count it as "not grabbed": within this process one session
	// allows only one turn, which matches the meaning of the store lock, just decided earlier.
	if _, loaded := in.localTurns.LoadOrStore(rec.RunID, struct{}{}); loaded {
		return false, nil
	}

	// 2. Then grab the one in the store (the source of truth for cross-instance mutual exclusion).
	begun, err := in.cfg.Store.TryBeginTurn(ctx, rec, staleAfter)
	if err != nil {
		// ⚠️ Remove the reservation on error too, or this session is permanently locked in this process.
		in.localTurns.Delete(rec.RunID)
		return false, err
	}
	if !begun {
		// ⚠️ Same as above: if not grabbed, give the reservation back so the caller can take the redirect/queue path.
		in.localTurns.Delete(rec.RunID)
	}
	return begun, nil
}

// NewInbound creates an inbound handler.
func NewInbound(cfg InboundConfig) (*Inbound, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("%w: Store 不能为空", ErrInvalidArgument)
	}
	if cfg.Conversations == nil {
		return nil, fmt.Errorf("%w: Conversations 不能为空", ErrInvalidArgument)
	}
	if cfg.Sender == nil {
		return nil, fmt.Errorf("%w: Sender 不能为空", ErrInvalidArgument)
	}
	if cfg.Tokens == nil {
		return nil, fmt.Errorf("%w: Tokens 不能为空", ErrInvalidArgument)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Inbound{cfg: cfg, log: log, now: now, drops: newDropReporter(now)}, nil
}

// Handle handles one inbound message. The signature matches lark.MessageHandler.
//
// **The meaning of the return value matters**: returning an error makes Feishu redeliver this message. So only return an error for "this did not work,
// resending may work"; already handled, ignored on purpose, and the agent itself failing
// all return nil.
func (in *Inbound) Handle(ctx context.Context, msg *lark.InboundMessage) error {
	if msg == nil {
		return nil
	}
	log := in.log.With("larkApp", msg.AppID, "message", msg.MessageID, "chat", msg.ChatID)

	// What was received. **The body does not go into the log** (that is user data, kept long-term and searchable by the whole team);
	// only a fingerprint is logged: length + 4-byte hash, enough to answer "was it received / how long / is it the same one".
	log.Debug("收到入站消息",
		"chatType", msg.ChatType,
		"senderType", msg.SenderType,
		"mentioned", msg.Mentioned,
		"thread", msg.ThreadID,
		"text", secret.Fingerprint(msg.Text))

	// —— Trigger decision ——
	//
	// Two stages, **doing the half without I/O first**: was it @-mentioned, does it qualify for "no @ needed", does it have a body.
	// These three filter out most messages in a chat without a single remote call. Before this change it was a single line
	// `if !msg.Mentioned`; the only addition is the branch "not @-mentioned but may trigger without @".
	//
	// ⚠️ **Every "do not trigger" branch must state its reason**. When debugging "why does the topic group not reply" today,
	// these silent `return nil`s made the whole decision a black box: whether the message came in at all,
	// and at which step it was rejected, were all invisible. We could only guess by reading code, and each wrong guess cost half an hour.
	//
	// ⚠️⚠️ **A Debug log effectively does not exist in production**, so besides Debug each branch also goes through
	// dropReporter to write a **rate-limited Info**. The production level is Info (newLogger in cmd/amad),
	// so none of the three original "state the reason" logs ever reached the log platform. In production, "the message was rejected"
	// looked exactly like "the message never came in", and the two call for opposite actions. See dropped.go.
	if !msg.Mentioned && !mentionFreeEligible(msg) {
		log.Debug("不触发：没 @ 且不是话题的根",
			"chatType", msg.ChatType, "thread", msg.ThreadID, "rootID", msg.RootID)
		in.drops.report(log, msg, dropReasonForIneligible(msg))
		return nil
	}
	if strings.TrimSpace(msg.Text) == "" {
		log.Debug("不触发：正文为空（可能是纯图片/文件，或只 @ 了没说话）")
		in.drops.report(log, msg, dropReasonEmptyText)
		return nil
	}
	// The second stage reads the store once (the trigger mode is configured on the app + we need to know "who we are").
	// **Only messages in a topic without an @ get here**. In the default mode it returns false right after the read,
	// so ordinary chats, and any message with an @, cost exactly the same as before this change.
	//
	// There are two ways to reject here (mode not enabled / sent by one of our own), and **they call for completely different actions**:
	// the former means changing the config, the latter means self-trigger protection is working normally. So the reason is reported separately,
	// and mentionFreeAllowed passes back which one it is.
	if !msg.Mentioned {
		if allowed, reason := in.mentionFreeAllowed(ctx, msg, log); !allowed {
			log.Debug("不触发：免 @ 判定否决", "reason", reason)
			in.drops.report(log, msg, reason)
			return nil
		}
	}
	log.Info("触发 agent", "mentioned", msg.Mentioned, "chatType", msg.ChatType)

	// Dedup. Placed before the real work starts: on a duplicate delivery we should not even exchange a token.
	fresh, err := in.claim(ctx, msg)
	if err != nil {
		// If we cannot record the dedup, we do not dare go on: the agent might run twice. Letting Feishu redeliver once is safer.
		log.Warn("记录入站消息失败，让飞书重投", "error", err.Error())
		return err
	}
	if !fresh {
		log.Debug("重复投递，已忽略")
		return nil
	}

	// From here on, failures must be recorded as failed, or a redelivery would be blocked as pending.
	if err := in.dispatch(ctx, msg, log); err != nil {
		if markErr := in.mark(ctx, msg.MessageID, inboundFailed); markErr != nil {
			log.Warn("标记失败状态出错", "error", markErr.Error())
		}
		// **Do not propagate it upwards**: if the agent failed, a redelivery usually fails again and only gives the user a string of failure cards.
		// dispatch has already told the user about the error.
		log.Error("处理入站消息失败", "error", err.Error())
		return nil
	}
	return in.mark(ctx, msg.MessageID, inboundProcessed)
}

// mentionFreeEligible decides whether this message without an @ **could possibly** trigger without @.
//
// It only looks at the message itself (is it in a topic, is it the topic root), without reading the store. It exists to **keep the store read
// out**: most messages without an @ in a chat are rejected at this step, with the same behavior and cost as before the change.
//
// The test **must stay consistent** with the one in shouldTrigger: if this lets it through and that rejects it, the worst case is one extra
// store read; if this wrongly rejects it, being right over there does not help. So both places take their tests from trigger.go.
//
// ⚠️ **Replies inside a topic are rejected right here** (isTopicRootMessage), the same test as shouldTrigger.
// This is especially valuable for alert chats: replies far outnumber topic roots, and stopping them before the store read saves exactly that part.
//
// ⚠️ **We deliberately do not check "is it a bot" here.** That check needs to know "who we are"
// (the open_id set), which needs a store read, exactly what this stage avoids. Blocking our own is left to
// shouldTrigger in the second stage. **Do not add isBotMessage back here**: that would block third-party alert bots again,
// and no fix in the second stage would help (this stage returns first).
func mentionFreeEligible(msg *lark.InboundMessage) bool {
	if msg == nil {
		return false
	}
	return isTopicMessage(msg) && isTopicRootMessage(msg)
}

// dropReasonForIneligible states **exactly** which gate this message without an @ failed.
//
// The two ways to reject call for completely different actions, and merging them into one says nothing:
//
//	Not in a topic           → in a chat, someone @-mentions the bot to trigger it; this is expected
//	In a topic but not root  → this is a reply inside a topic and **by design needs an @** (a false trigger fixed on 2026-09-02)
//
// The latter especially needs its own report: when "why was this not answered" really happens in production, seeing "not in a topic" sends people
// to check why the thread_id was lost, while the truth is that it is in a topic, just not the message that opened it.
func dropReasonForIneligible(msg *lark.InboundMessage) string {
	if msg != nil && isTopicMessage(msg) {
		return dropReasonNotTopicRoot
	}
	return dropReasonIneligible
}

// mentionFreeAllowed reads the trigger mode on the app and decides whether this message without an @ should trigger.
//
// Returns (allowed, reason for not allowing). **The reason goes into the log**, so the two ways to reject are kept apart:
// "mode not enabled" means the user should change the config; "sent by one of our own" means self-trigger protection is working normally.
// Merging them would send whoever is debugging to change a config that is fine. When allowed, reason is the empty string.
//
// **When the app cannot be found, or the store read fails, do not allow.** This is a deliberate fail-closed:
// no-@ mode is an enhancement the user turned on explicitly, and falling back to the default mode when we cannot tell is the safe side. The opposite, "treat it as on
// when it cannot be read", would make an app we cannot find auto-reply to every message in a topic group, the worst outcome.
//
// Also, why this read is not cached: the trigger mode is configured on the app and users change it at any time, and **production runs multiple instances**.
// An in-process cache would mean "the mode was changed but some instances still follow the old one", showing up as "sometimes it replies, sometimes not",
// the kind of failure CLAUDE.md calls out as hardest to debug. This read is GetAppByAppID, which dispatch
// has to do anyway, through the same index.
func (in *Inbound) mentionFreeAllowed(
	ctx context.Context, msg *lark.InboundMessage, log *slog.Logger,
) (bool, string) {
	app, err := in.cfg.Store.GetAppByAppID(ctx, msg.AppID)
	if err != nil {
		// Do not propagate it upwards: returning an error makes Feishu redeliver this message (see the file header), and a message without an @
		// most likely should not trigger anyway; redelivery would only multiply the same failure into a string of them.
		log.Debug("取飞书应用失败，这条没 @ 的消息按不触发处理", "error", err.Error())
		return false, dropReasonAppLookupFailed
	}
	selves := in.selfBotOpenIDs(ctx, app, log)
	if isSelfMessage(msg, selves) {
		// Sent by one of our own (this app, or the bot of another AMA app). Blocking it is right,
		// but we must say so, otherwise "why does the bot ignore the message it sent itself" cannot be explained.
		return false, dropReasonSelfMessage
	}
	if !shouldTrigger(msg, app.TriggerMode, selves) {
		return false, dropReasonModeOff
	}
	return true, ""
}

// selfBotOpenIDs collects the bot open_ids of "our own": this app's, plus those of other
// AMA apps in this tenant. The no-@ decision uses it to block self-triggering; see the trigger.go file header.
//
// # Why "other AMA apps" are included
//
// Blocking only ourselves does not stop **two AMA bots in the same chat from triggering each other**: their open_ids differ,
// neither considers the other to be "itself", so A replies to B, B replies to A in an endless loop, and each round really burns model and container time.
// In production two apps have no-@ mode on and really share a chat, so this is not a hypothetical risk.
//
// # On store read failure return this app's ID, not an empty set
//
// In isSelfMessage an empty set means "do not know who we are → block all bots", which on a store hiccup would
// also block third-party alert bots as collateral (exactly what this change fixes). Keeping at least this app's own ID
// keeps the self-trigger defense in place; the cost is only that for that moment "another AMA bot" is not blocked,
// and that scenario needs two bots in the same chat anyway, which is far rarer than a store hiccup.
func (in *Inbound) selfBotOpenIDs(
	ctx context.Context, app *App, log *slog.Logger,
) map[string]bool {
	out := make(map[string]bool, 4)
	if id := strings.TrimSpace(app.BotOpenID); id != "" {
		out[id] = true
	}
	apps, err := in.cfg.Store.ListAllApps(ctx)
	if err != nil {
		// Log only: this app's own ID is already in the set, so self-triggering is still blocked.
		log.Debug("列全部飞书应用失败，本轮只挡本应用自己的 bot", "error", err.Error())
		return out
	}
	for _, other := range apps {
		if other == nil {
			continue
		}
		if id := strings.TrimSpace(other.BotOpenID); id != "" {
			out[id] = true
		}
	}
	return out
}

// dispatch is the part that does the real work: resolve ownership → create session → subscribe → send message → reply card.
func (in *Inbound) dispatch(ctx context.Context, msg *lark.InboundMessage, log *slog.Logger) error {
	app, err := in.cfg.Store.GetAppByAppID(ctx, msg.AppID)
	if err != nil {
		return fmt.Errorf("找不到这个飞书应用: %w", err)
	}

	// Record this chat along the way, so scheduled tasks can pick it as a delivery target.
	//
	// **Placed here instead of at the top of Handle**: we need GetAppByAppID to get the WorkspaceID before we know
	// which workspace this chat belongs to. It comes before the binding check because chats "with no agent bound" should also be remembered:
	// users will likely add the bot to the chat first and then go back to AMA to configure it.
	//
	// **Failure is only logged and does not interrupt this turn**: failing to record the chat only affects the dropdown when configuring scheduled tasks later,
	// while this message should be answered as usual. Returning an error would make Feishu redeliver the whole message (see the file header).
	in.rememberChat(ctx, app, msg, log)

	agentID, err := in.resolveBoundAgent(ctx, app, msg, log)
	if err != nil {
		return err
	}
	if agentID == "" {
		// No agent bound is not an error; it is just not configured yet. Tell the user to configure it instead of silently ignoring them:
		// if silent, the user would just think the bot is broken.
		return in.replyStatic(ctx, app, msg,
			"这个飞书应用还没有绑定 agent。到 AMA 的 agent 详情页把它绑上就能用了。")
	}

	// The bound agent has been disabled: likewise reply with a human sentence instead of staying silent.
	//
	// **Blocked here instead of relying only on core's 422**, for the same reason as above: core does block it,
	// but the error only bubbles up to Handle and becomes one log line; people in the chat get nothing,
	// which looks like "the bot is ignoring me", and ops has to dig through logs to learn someone disabled the agent.
	//
	// **Both the log and the reply are needed**: the reply is for people in the chat, the log for ops. Without the log, ops cannot
	// find the cause when "users report the bot does not reply"; without the reply, users have no idea what happened.
	if in.cfg.Conversations.AgentDisabled(ctx, agentID) {
		log.Info("绑定的 agent 已停用，不触发这一轮", "agent", agentID)
		return in.replyStatic(ctx, app, msg,
			"这个 agent 已停用，暂时不会回答。到 AMA 的 agent 详情页把状态改回「已启用」就能继续用了。")
	}

	secret, err := in.cfg.Store.AppSecret(ctx, app.AppID)
	if err != nil {
		return fmt.Errorf("取应用密钥失败: %w", err)
	}
	token, err := in.cfg.Tokens.TenantAccessToken(ctx, app.AppID, secret)
	if err != nil {
		return fmt.Errorf("换 token 失败（凭证可能已失效）: %w", err)
	}

	// Locate the session by scope: topics by topic, ordinary chats by chat, **direct chats by message** (one new session per direct message).
	// Rules are in scope.go.
	scope, runID, err := in.ensureScopedRun(ctx, app, msg, agentID, log)
	if err != nil {
		return err
	}
	log = log.With("run", runID, "agent", agentID, "scope", scope.Key)
	log.Info("飞书消息进来了，开跑")

	return in.runAndReply(ctx, token, msg, runID, agentID, log)
}

// resolveBoundAgent decides which agent should answer this message.
//
// An empty string means "this app has no agent bound yet". That is not an error; the caller replies with a hint.
//
// # An app can bind only one agent
//
// The constraint is guaranteed on the write side (core.Service.ensureFeishuAppsFree). But **we cannot assume it holds here**:
//
//   - the constraint was added later, and the store may have older 1:N data;
//   - with multiple instances the write-side check has a very narrow TOCTOU window, and PostgreSQL cannot express this
//     unique constraint (see the reason under agents_feishu_idx in repository/schema/core_pg.sql).
//
// So the strategy here is **tolerate and alert**, not assert: if multiple bindings really occur, still take the first one (by ord,
// same as the behavior before the change, so existing users do not suddenly stop getting replies), but log an ERROR naming
// the app and every candidate agent.
//
// We choose "answer as before + alert" over "refuse to answer" because the latter would turn a **config problem** into
// a **production outage**: users would only see the bot ignoring them, while this 1:N data may have been sitting there a long time.
// An alert is visible; silence is not.
func (in *Inbound) resolveBoundAgent(
	ctx context.Context, app *App, msg *lark.InboundMessage, log *slog.Logger,
) (string, error) {
	agentIDs, err := in.cfg.Conversations.AgentsBoundToFeishuApp(ctx, app.ID)
	if err != nil {
		return "", fmt.Errorf("查绑定的 agent 失败: %w", err)
	}
	if len(agentIDs) == 0 {
		return "", nil
	}
	if len(agentIDs) > 1 {
		// Name every candidate: if we only say "there is a conflict", the admin still has to dig up which ones.
		log.Error("这个飞书应用被多个 agent 绑定了，本轮按顺序取第一个回答；请去 AMA 解绑到只剩一个",
			"feishuApp", app.AppID,
			"candidates", strings.Join(agentIDs, ","),
			"chosen", agentIDs[0])
	}
	return agentIDs[0], nil
}

// feishuTriggerDetail is the text shown in the "source" column of the run history.
//
// Feishu inbound messages carry no chat name (the long connection only gives chat_id / chat_type), and looking up chat info costs an extra
// remote call that may fail, while this field is purely for humans to read. So we just give a stable label by session type.
func feishuTriggerDetail(msg *lark.InboundMessage) string {
	if msg.ChatType == ChatTypeP2P {
		return "飞书单聊"
	}
	return "飞书群聊"
}

// # Why it is not as simple as "create it if not found"
//
// Production has multiple instances; two messages from the same chat may land on two processes at once, both see "not bound yet",
// and both create an Ark session. So after creating, the **database** decides whose counts (the unique constraint of
// BindSessionScope); the loser drops the one it just created and uses the winner's instead. Otherwise one chat has two contexts,
// with no error.
//
// # Direct chats also take this path, even though every message needs a new session
//
// The scope key of a direct chat is message_id (see scope.go), so GetSessionScope almost always returns
// ErrScopeNotBound, and every direct message reaches the session creation step below. That is exactly the intended effect of "one
// separate session per message".
//
// **But that is no reason to bypass the scope and create the session directly.** Feishu delivers the same message more than once (see the file header),
// and on redelivery the message_id stays the same and so does the computed key. So this "look up first, then let the database decide" mechanism
// also stops duplicate deliveries before session creation: the losing attempt uses the winner's session, instead of creating a
// second Ark session for the same sentence and answering the same question twice. The inbound dedup table stops duplicates within the same
// process; this stops two instances receiving the same redelivered message at once.
//
// When no scope can be computed (chat_id, topic anchor, and message_id all empty), fall back to "a new session per message",
// i.e. the behavior before the change: better no context than mixing a pile of unrelated messages into one session.
func (in *Inbound) ensureScopedRun(
	ctx context.Context, app *App, msg *lark.InboundMessage, agentID string, log *slog.Logger,
) (SessionScope, string, error) {
	scope := ScopeForMessage(msg)
	if !scope.Valid() {
		log.Warn("这条消息算不出会话作用域，退回一次性会话（不会有上下文）",
			"chatType", msg.ChatType)
		run, err := in.cfg.Conversations.CreateFeishuConversation(
			ctx, app.WorkspaceID, agentID, feishuTriggerDetail(msg))
		if err != nil {
			return scope, "", fmt.Errorf("建会话失败: %w", err)
		}
		return scope, run.ID, nil
	}

	// Look up the bound one first. Most messages in chats / topics end here: one read, no Ark call.
	//
	// Direct chats almost always land in the ErrScopeNotBound branch (the key includes message_id); a hit only happens when
	// the same message is redelivered, and reusing it then is correct; see this function's comment.
	bound, err := in.cfg.Store.GetSessionScope(ctx, msg.AppID, scope.Key, agentID)
	switch {
	case err == nil:
		// Hit: reuse this session, so the agent can see the context of the earlier turns.
		if touchErr := in.cfg.Store.TouchSessionScope(
			ctx, msg.AppID, scope.Key, agentID, in.now(),
		); touchErr != nil {
			// Pure ops information; failure should not affect the conversation.
			log.Warn("更新作用域活跃时刻失败", "error", touchErr.Error())
		}
		log.Debug("复用已有会话", "run", bound.RunID, "turns", bound.TurnCount)
		return scope, bound.RunID, nil
	case errors.Is(err, ErrScopeNotBound):
		// Not bound yet: this topic / chat is speaking for the first time, or this is a new direct message; go on to create one.
	default:
		return scope, "", fmt.Errorf("查会话作用域失败: %w", err)
	}

	run, err := in.cfg.Conversations.CreateFeishuConversation(
		ctx, app.WorkspaceID, agentID, feishuTriggerDetail(msg))
	if err != nil {
		return scope, "", fmt.Errorf("建会话失败: %w", err)
	}

	now := in.now()
	winner, ok, err := in.cfg.Store.BindSessionScope(ctx, SessionScopeRecord{
		LarkAppID:   msg.AppID,
		ScopeKey:    scope.Key,
		AgentID:     agentID,
		RunID:       run.ID,
		WorkspaceID: app.WorkspaceID,
		ScopeType:   scope.Type,
		ChatID:      msg.ChatID,
		ThreadID:    scope.ThreadID,
		CreatedAt:   now,
		LastUsedAt:  now,
		TurnCount:   1,
	})
	if err != nil {
		// If binding fails, use the one just created to finish this turn: the user gets an answer, only the next message may get a new session.
		// Better than erroring out: with an error nobody would respond to this message at all.
		log.Warn("绑定会话作用域失败，这一轮没有上下文复用", "error", err.Error())
		return scope, run.ID, nil
	}
	if !ok {
		// Lost the race: another instance bound first. **We must switch to the winner's session**. In chats / topics, not doing so
		// would leave two contexts from then on; in direct chats the same message would get one session per instance,
		// and the same question would be answered twice.
		//
		// The session we just created is no longer used. We do not delete it: an empty session on the Ark side costs nothing,
		// and a session that never called a tool has no container at all (containers are created lazily, see shared/infra/dockertools/README.md);
		// deleting needs an extra remote call that may fail, and we would have to handle "deleted the one in use".
		log.Info("会话作用域已被别的实例绑定，改用它那条",
			"winner", winner.RunID, "discarded", run.ID)
		return scope, winner.RunID, nil
	}
	log.Info("新建会话", "run", run.ID, "scopeType", scope.Type)
	return scope, run.ID, nil
}

// runAndReply runs one turn and hands the result to the user in three steps: "reaction → chain of thought → separate answer".
//
// # The role of each step
//
//  1. OnIt reaction    put on the user's message right after receipt, so they know the bot has started working
//  2. Chain of thought  pushed while running: thinking text and tool calls; closed when done, then the reaction is removed
//  3. Separate answer   after closing, the final answer is sent as 1~N **separate messages**
//
// The three are independent: a chain of thought that cannot be pushed does not affect the answer, and a failed reaction affects nothing. **Only step three
// must succeed**: the user can live without the thinking process, but not without the answer.
//
// # Why the answer does not reuse the chain-of-thought message
//
// The chain of thought is a dedicated message type (message_cot); after closing it cannot be changed, and the client renders it as
// a collapsible "thinking process". Putting the answer in it would let the final conclusion be collapsed too: users scrolling the chat
// would only see a "done" and have to expand it to learn the answer. So the answer is always a separate message.
func (in *Inbound) runAndReply(
	ctx context.Context, token string, msg *lark.InboundMessage, runID, agentID string, log *slog.Logger,
) error {
	// This turn's ctx must not follow the inbound event: the stream must keep going after Handle returns.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runTimeout)
	defer cancel()

	inThread := msg.ThreadID == "" // if not in a topic, start a new topic

	// **Write the inflight record first, then subscribe, then send the message.** The order is deliberate: after this moment, however the process
	// dies (deploy, OOM, panic), the restart scan can find this turn and continue it.
	//
	// The opposite, "send the message first, then record", has a real window: the message is already in Ark, the agent has started
	// inferring, and we have left no trace yet. That turn is lost forever, and the user never gets a reply.
	rec := InflightTurnRecord{
		RunID:     runID,
		LarkAppID: msg.AppID,
		ChatID:    msg.ChatID,
		// AgentID is recorded for the **recovery path**: after a restart resumeLive only has this record,
		// and without it we cannot ask "should this agent show the chain of thought". See startTurnProgress.
		AgentID:       agentID,
		SrcMessageID:  msg.MessageID,
		ReplyInThread: inThread,
		Status:        inflightOpen,
		StartedAt:     in.now(),
	}
	// **Only one turn runs on a session at a time.** With session reuse this is no longer optional: a session's event stream is
	// shared, and with two concurrent turns both sides collect "what the agent said", while agentText cannot tell which sentence was meant
	// for whom, so both answers mix the content of both questions.
	//
	// It also protects this table itself: the table's primary key is run_id, so with two concurrent turns the later write overwrites the earlier one,
	// and DeleteInflightTurn from the turn that finishes first deletes the other turn's recovery record too.
	//
	// If we cannot grab it, tell the user clearly "the previous message is still being processed", instead of silently dropping their message.
	// ⚠️ Go through beginTurn instead of calling Store directly: localTurns must be registered **in the same step** as grabbing the lock,
	// or during the window in between another message would reclaim this live lock as a leftover. See beginTurn.
	begun, err := in.beginTurn(runCtx, rec, runTimeout)
	if err != nil {
		// If we cannot tell whether we may start, we do not start: better to let Feishu redeliver once than to have two concurrent turns garble the reply.
		return fmt.Errorf("开启轮次失败: %w", err)
	}
	if !begun {
		// The record is still there, but **we cannot trust this table alone**: it only says "someone started a turn and did not delete the record",
		// not that the turn is really still running. Leftover locks do happen: the "Ark does not close the stream" pitfall above kept
		// a record around for 10 extra minutes, during which every question from the user was blocked. So here we ask Ark once more:
		// if the linked run is already final, the previous turn has really ended; reclaim it directly and let this turn in.
		//
		// This extra remote call only happens on the cold path of **failing to grab the lock**; the normal path has none.
		//
		// ⚠️ **But never reclaim while this process itself holds this turn** (the localTurns gate).
		// The test "run is idle" is not enough under session reuse: idle only says "this session is not
		// inferring right now", while **a turn lives longer than one inference**. After the agent answers (idle),
		// the goroutine holding the lock still has to close the chain of thought, wait for the ordering gate, and send the answer; the run is
		// idle during that time. Letting a second goroutine in then means two subscribers consume the same event stream at once,
		// each collecting the **full** text, so the user gets several identical replies that glue the two turns' answers together.
		//
		// Measured (local sesn-example-07): sending 9 messages in a row produced only 3 interrupts;
		// the other 6 slipped in through this gap. Feishu showed 3 identical replies,
		// each containing the content of both turns, seq 40 + seq 46.
		if !in.holdsTurn(runID) && in.reclaimFinishedTurn(runCtx, runID, log) {
			begun, err = in.beginTurn(runCtx, rec, runTimeout)
			if err != nil {
				return fmt.Errorf("回收已结束轮次后重开失败: %w", err)
			}
		}
	}
	if !begun {
		// The previous turn is still running. Decide by the mode configured on this agent; **the default is redirect**.
		//
		// # Redirect = interrupt the previous turn + queue behind it, not "stuff the message into its turn"
		//
		// The old implementation was "call RedirectRun to stuff the new message into the running session, and let the running
		// turn finish it itself". That assumption **was wrong and caused a production bug**: the running goroutine
		// saw the final frame brought by the interrupt and dropped the stream (turnEnded judged it ended, and its
		// stop_reason was end_turn, identical to a normal end), so for the answer to the new message
		// **nobody was waiting**. It either landed in the answerBuffer of the message after that (two turns' replies mixed),
		// or was never sent at all. Measured on sesn-example-06: "2" and "3" went in through RedirectRun,
		// the answer to "3" was picked up by the turn for "4", and the user saw a reply mixing two turns.
		//
		// Now it is: **only send the interrupt, then wait for the lock, and after getting it take the normal path below**. So
		//
		//   - every message has its own goroutine waiting for its own answer, anchored to its own Feishu message;
		//   - answers do not mix, because only one turn holds the lock at any moment (the meaning of that lock is unchanged);
		//   - it also fixes a hidden defect: the user.message sent through RedirectRun **did not carry the
		//     <harness:lark> context block** (that is built by userMessageEvent), so for redirected
		//     messages the agent could not see chat_id / thread_id, and in practice it called tools at random to find context.
		if !in.cfg.Conversations.AgentFollowUpQueues(ctx, agentID) {
			// The interrupt was not sent (no body / Ark refused): we already fell back to the queued notice, so we are done.
			// **Do not go on waiting for the lock**: the previous turn was never interrupted, waiting for it would only waste a full window,
			// and the user has already been told "the previous message is still being processed".
			sent, err := in.interruptForRedirect(ctx, token, msg, runID, log)
			if err != nil {
				return err
			}
			if !sent {
				return nil
			}
			// Wait for the previous turn to leave and hand over the lock. If we cannot get it, fall back to the queued notice; do not silently drop this message.
			begun, err = in.awaitTurn(runCtx, rec, log)
			if err != nil {
				return err
			}
			if !begun {
				log.Warn("已发出中断但没等到上一轮交还锁，退回排队提示", "run", runID)
				return in.replyBusy(ctx, token, msg)
			}
		} else {
			log.Info("这条会话上已有一轮在跑，按配置排队（让用户稍后再问）", "agent", agentID)
			// The hourglass reaction + that notice together say "received, waiting for the previous turn". Without the reaction,
			// people in the chat cannot tell whether the bot is queueing or never saw the message.
			markQueued(ctx, in.cfg.Reactor, token, msg.MessageID, log)
			return in.replyBusy(ctx, token, msg)
		}
	}

	// Reaching here means we have the lock, **and registration was already done in the step that grabbed it** (see beginTurn).
	//
	// ⚠️⚠️ **Do not add a `localTurns.Store` here, and do not move registration back here.**
	// This used to be the registration point, and the comment said "must register here, before any return".
	// The reasoning was right (registering late leaves a window), but **the position itself was already late**: between TryBeginTurn succeeding and this
	// line lies the whole redirect, interrupt, awaitTurn section, during which this process holds the lock but is invisible to holdsTurn,
	// so a second message reclaimed this live lock as a leftover. That was exactly
	// the root cause of the flaky failure of TestConcurrentMessagesDoNotDuplicateReplies.
	//
	// Now "grab the lock" and "register" are the same step in beginTurn, so they can never come apart again.
	// The `localTurns.Delete` in the defer below is its matching other half.

	// **Cleanup has only this one exit.** Success, stream error, timeout, send failure: all end up here.
	// If cleanup only hung off the failure path, successful turns would leave their records in
	// the store, and the next restart would answer already-answered questions again (the user sees two identical replies).
	//
	// Use a ctx without cancellation: by now runCtx has often timed out or been cancelled,
	// and deleting with it would fail outright, leaving the record in the store, which is exactly the bug above.
	//
	// Deleting it also **releases the serial lock above**, so the next message in the same chat can start.
	defer func() {
		// Remove this process's lock-held marker first, then delete the record in the store.
		//
		// The order is deliberate: the other way round, the record is already deleted while the marker is still there, and for that moment
		// the goroutine waiting in awaitTurn can get the lock but is judged by holdsTurn as
		// "this process is holding it", so the reclaim check for its own message is needlessly skipped.
		in.localTurns.Delete(runID)

		delCtx, delCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer delCancel()
		if err := in.cfg.Store.DeleteInflightTurn(delCtx, runID); err != nil {
			log.Warn("清理未结轮次记录失败，重启后可能重复恢复这一轮", "error", err.Error())
		}
	}()

	// —— Step 1: add the OnIt reaction ——
	//
	// It comes first (right after grabbing the turn), because its only point is to be **fast**: the few seconds between a user @-mentioning the bot
	// and seeing the first feedback are when they are most likely to think "the bot is broken".
	// Creating the session and the chain of thought each need a remote round trip; adding the reaction after them would defeat its purpose.
	reactionID := markReceived(ctx, in.cfg.Reactor, token, msg.MessageID, log)

	// —— Step 2: start the chain of thought ——
	//
	// If we cannot get it, it is nil, all later calls are no-ops, and this turn degrades to "reaction + answer".
	turnKey := ProgressDeliveryKey(runID, msg.MessageID)
	progress := in.startTurnProgress(runCtx, token, rec, turnKey, agentID, log)

	// answer collects what the agent says, and is finally sent as separate messages.
	//
	// Collecting it separately from progress is deliberate: the chain of thought wants "thinking + tool calls", the answer wants
	// the text the agent finally says, and the two are different event types in the Ark event stream. Mixing them would let
	// the thinking process leak into the final answer.
	answer := &answerBuffer{
		// Each time new content arrives, write it back to the store, so the answer can continue after a service restart.
		//
		// **This step cannot be skipped.** The answer used to live only in this strings.Builder (process memory);
		// when the process died, the text already produced was gone. On recovery SyncRunEvents can fill in events from the Ark side,
		// but **frames already consumed by this process do not come again**, so the user got
		// an answer missing its beginning, with no sign that anything was missing.
		persist: in.inflightPersister(ctx, rec),
	}

	// **Ark does not close this stream by itself**; we must recognize the final frame and disconnect ourselves.
	//
	// Ark keeps an SSE long connection per session and keeps waiting for the next user message after end_turn, so
	// StreamRunEvents does not return. Without this step, wg.Wait() below would block until runCtx
	// times out (runTimeout, 60 minutes), and **the release of the serial lock above hangs off this function's defer**,
	// so the turn is clearly finished but the lock is held until the wait limit. When the user asks the next question in the chat, they get
	// "the previous message is still being processed", and the log looks completely normal. This bug really happened in production.
	//
	// The test is the same as turnEnded in core/scheduler.go and applyEventToRun in core/chat.go:
	// an idle with requires_action **does not count as ended** (the agent is waiting for a tool result, and continues after the worker
	// returns it); treating it as ended would cut the answer in half.
	//
	// Use a separate streamCtx instead of reusing cancel: cancellation here is "normal finishing",
	// and must not affect other timeout-based decisions on runCtx.
	streamCtx, stopStream := context.WithCancel(runCtx)
	defer stopStream()

	// Subscribe before sending: core events are only stored when someone is subscribed; the other way round loses the beginning.
	var streamErr error
	var interrupted atomic.Bool
	// Diagnostic counters for this turn: when we fall back to the default text, we use them to explain "why answer is empty".
	// Without them the fallback branch leaves no trace at all, and afterwards we can only guess (this investigation got stuck exactly here).
	var frames, messages atomic.Int64
	var failedUpstream atomic.Bool
	var lastStop atomic.Value
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		streamErr = in.cfg.Conversations.StreamRunEvents(streamCtx, runID, func(raw []byte) error {
			frames.Add(1)
			if text, ok := arkframe.AgentText(raw); ok && text != "" {
				messages.Add(1)
				answer.write(text)
			}
			// A tool call splits the text collected so far into segments: that segment is usually narration like "let me go check", not the reply.
			// See the type comment on answerBuffer: splitting **does not lose content**, and it falls back when the turn ends with tool_use.
			if arkframe.StartsToolCall(raw) {
				answer.reset()
			}
			// This turn was redirected by the user's next message. Record it, and **send no message when finishing because of it**:
			// being interrupted is not a failure, and having no answer is its normal result. See turnInterrupted.
			if arkframe.Interrupted(raw) {
				interrupted.Store(true)
			}
			// The Ark side gave up on this turn (rate-limit retries exhausted). It satisfies turnEnded but is not "finished answering";
			// when finishing we use it to switch to a message that can state the cause. See turnFailedUpstream.
			if arkframe.FailedUpstream(raw) {
				failedUpstream.Store(true)
			}
			if s := arkframe.StopReason(raw); s != "" {
				lastStop.Store(s)
			}
			// The chain of thought eats every frame: thinking, tool calls, and final state are all in there.
			progress.consume(streamCtx, raw)
			// The final frame must be fed to the chain of thought before dropping the stream, or the closing frame is lost,
			// and the chat is left with a thinking process that spins forever.
			if arkframe.TurnEnded(raw) {
				stopStream()
			}
			return nil
		})
		// Us dropping the stream is normal finishing, not a failure; do not let it mark this turn as error.
		if streamErr != nil && streamCtx.Err() != nil && runCtx.Err() == nil {
			streamErr = nil
		}
	}()

	// Give the subscription a moment to be set up. No complex handshake here: StreamRunEvents first fills in historical events internally,
	// so even if we send too early nothing is lost.
	time.Sleep(50 * time.Millisecond)

	// Prefetch context: card body / sub-messages of a merged forward / chat name.
	//
	// ⚠️ **It is placed here and not earlier** because it needs the token, and the token is only obtained at this step.
	// It never returns an error (see prefetchContext); failure only means a bit less context.
	prefetched := in.prefetchContext(runCtx, in.cfg.Fetcher, token, msg, log)
	// The prefetch also gets the chat name, which is recorded in the chat list: that is what is shown when picking a chat for scheduled tasks.
	// After a chat is renamed, the next time someone @-mentions the bot in the chat this gets updated. Failure only affects display.
	if name := strings.TrimSpace(prefetched.ChatName); name != "" {
		if err := in.cfg.Store.SetChatName(runCtx, msg.AppID, msg.ChatID, name); err != nil {
			log.Warn("更新群名失败，定时任务的群列表里可能仍显示旧名或 chat_id", "error", err.Error())
		}
	}

	event, err := arkframe.UserMessage(
		larkUserText(msg, prefetched, log),
		renderLarkPromptWithContext(msg, prefetched, log),
	)
	if err != nil {
		cancel()
		wg.Wait()
		in.abortTurn(ctx, progress, token, msg, reactionID, log)
		return err
	}
	if _, err := in.cfg.Conversations.AppendRunEvents(runCtx, runID, []json.RawMessage{event}); err != nil {
		cancel()
		wg.Wait()
		in.abortTurn(ctx, progress, token, msg, reactionID, log)
		return fmt.Errorf("发消息到会话失败: %w", err)
	}

	wg.Wait()
	if streamErr != nil {
		log.Warn("事件流异常结束", "error", streamErr.Error())
	}

	// Finish with a ctx without cancellation: runCtx may have timed out by now (long task), and closing and sending the answer
	// **must be completed**; otherwise the chain of thought spins forever and the user never gets the answer.
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), replyFinishTimeout)
	defer finishCancel()

	// Close the chain of thought. The final state depends on how the stream ended: a stream error counts as error; otherwise use the final state
	// computed from agent events (snapshotBuilder recognizes it from session.* events).
	outcome, errMessage := "done", ""
	if streamErr != nil {
		outcome, errMessage = "error", streamErr.Error()
	}
	progress.finish(finishCtx, outcome, errMessage)

	// —— This turn was redirected: close the chain of thought, but **send no message** ——
	//
	// The user sent the next message and interrupted this turn; the answer belongs to the new turn (it has its own goroutine running).
	// There is no answer to send here, and it **should not report an error**: being interrupted is the normal result of a redirect, not a failure.
	//
	// Without this branch we get that production bug: when sending "1, 2, 3" in a row, the first two turns each reached
	// sendFinalReply below with an empty answer, so each one got a pointless "this turn produced no reply,
	// something may have gone wrong during execution". See the notes in turnInterrupted and sendFinalReply.
	//
	// The reaction is not switched to DONE either: this turn did not finish answering, so DONE would be lying. The user's message already has
	// the "redirect" reaction from interruptForRedirect; removing OnIt is enough.
	if interrupted.Load() {
		log.Info("这一轮被下一条消息转向，不发回复（答案属于新的那一轮）", "run", runID)
		clearReceived(finishCtx, in.cfg.Reactor, token, msg.MessageID, reactionID, log)
		return nil
	}

	// —— Step 3: send the final answer as separate messages ——
	//
	// Waiting for the chain of thought to close keeps the order nice (thinking first, conclusion after), but **send even if it never closes**:
	// waitForProgressTerminal has a 10-second cap and lets it through after that. See the reply.go file header.
	if in.cfg.Progress != nil {
		waitForProgressTerminal(finishCtx, in.cfg.Progress, turnKey, in.now, log)
	}

	// Not a single character collected: **this path must leave a trace**.
	//
	// The fallback text used to log nothing (reply.go only assigned it), so "a user received 'something went wrong
	// during execution'" in production could not be found in the logs at all. Searching for that sentence gave 0 hits, and 0 could mean it never happened
	// or it was never logged; the two could not be told apart. This investigation got stuck exactly here, and in the end it was only
	// pinned down by digging through the raw event stream in PG.
	//
	// What is logged are **the quantities needed to find the cause**, not the body (see "log ids and enums,
	// not bodies" in backend.md): how many frames this turn received, how many of them were agent.message, the stop_reason of the final frame,
	// and whether the stream itself errored. Together the four separate several causes:
	//
	//	messages=0 frames>0 stop=end_turn          the agent really said nothing
	//	messages>0                                 it spoke but was lost by segment splitting (one of the bugs fixed here)
	//	stop=retries_exhausted                     killed by rate limiting, not our problem
	//	frames=0                                   no events received at all (subscription or Ark error)
	answerText := answer.text()
	if strings.TrimSpace(answerText) == "" {
		stop, _ := lastStop.Load().(string)
		log.Warn("这一轮没有产生回复，将发兜底提示",
			"run", runID,
			"frames", frames.Load(),
			"messages", messages.Load(),
			"stop_reason", stop,
			"failed_upstream", failedUpstream.Load(),
			"interrupted", interrupted.Load(),
			"stream_error", streamErr != nil)
	}

	// A turn killed by rate limiting gets a message that **states the cause and the next step**.
	//
	// The generic fallback "something may have gone wrong during execution" gives the user no next step: they do not know whether to wait, re-ask,
	// or find someone. In this case the cause is known (model-side retries exhausted), and the next step is known too
	// (wait a bit and resend the same message).
	if failedUpstream.Load() && strings.TrimSpace(answerText) == "" {
		answerText = replyRetriesExhausted
	}

	sendErr := sendFinalReply(
		finishCtx, in.cfg.Sender, token, msg.MessageID, turnKey, answerText, inThread, log)

	// Switch the reaction from "processing" to "done": remove OnIt first, then add DONE.
	//
	// **Placed after sending the answer**: the reaction indicates this turn's state, and it should only become "done"
	// after the answer has really been sent. If we switched the reaction first and then sent the answer, a send failure would leave a DONE on the message
	// while the user actually received nothing, which is more misleading than no reaction.
	if sendErr != nil {
		// The answer was not sent, so do not falsely report completion. Only remove OnIt.
		clearReceived(finishCtx, in.cfg.Reactor, token, msg.MessageID, reactionID, log)
		return sendErr
	}
	markDone(finishCtx, in.cfg.Reactor, token, msg.MessageID, reactionID, log)
	return nil
}

// abortTurn finishes up when "it failed before it started running": close the chain of thought, remove the reaction.
//
// No answer is sent; there is no answer to send, and dispatch's caller will tell the user about the error. But if the chain of thought
// has already been created it **must be closed**, or the chat is left with a thinking process that spins forever.
func (in *Inbound) abortTurn(
	ctx context.Context, progress *turnProgress, token string,
	msg *lark.InboundMessage, reactionID string, log *slog.Logger,
) {
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replyFinishTimeout)
	defer cancel()
	progress.finish(abortCtx, "error", "这一轮没能开始执行")
	clearReceived(abortCtx, in.cfg.Reactor, token, msg.MessageID, reactionID, log)
}

// rememberChat records the chat this message is in, so scheduled tasks can pick it as a delivery target.
//
// Only the ID is recorded, not the name: inbound messages **do not carry the chat name** (see lark.InboundMessage). The name is filled in two places:
// later in this turn, prefetchContext writes it back after getting the chat name (SetChatName), and when listing chats, chats without a name
// get it fetched on the spot (see chatnames.go). Failing to get the name **is not an error**.
//
// p2p (direct chats) are recorded too: the delivery target of a task like "remind me in 1 hour" is exactly a direct chat.
func (in *Inbound) rememberChat(
	ctx context.Context, app *App, msg *lark.InboundMessage, log *slog.Logger,
) {
	if err := in.cfg.Store.RememberChat(ctx, ChatRecord{
		LarkAppID:   msg.AppID,
		ChatID:      msg.ChatID,
		WorkspaceID: app.WorkspaceID,
		ChatType:    msg.ChatType,
		LastSeenAt:  in.now(),
	}); err != nil {
		log.Warn("记录会话失败，定时任务的群列表里可能看不到它", "error", err.Error())
	}
}

// replyStatic sends a fixed sentence, used for cases like "not configured yet" that do not need to run the agent.
func (in *Inbound) replyStatic(ctx context.Context, app *App, msg *lark.InboundMessage, text string) error {
	secret, err := in.cfg.Store.AppSecret(ctx, app.AppID)
	if err != nil {
		return err
	}
	token, err := in.cfg.Tokens.TenantAccessToken(ctx, app.AppID, secret)
	if err != nil {
		return err
	}
	_, err = in.cfg.Sender.ReplyCard(ctx, token, msg.MessageID, lark.TextCard(text), lark.ReplyCardOption{
		IdempotencyKey: "ama-notice:" + msg.MessageID,
	})
	return err
}

// larkUserText computes the body of this message that goes into the user message.
//
// # Why not use msg.Text directly
//
// For cards and merged forwards, msg.Text is a **placeholder** (`[卡片]` / `[合并转发]`, produced by
// decodeContent and resourcePlaceholder in shared/infra/lark); the real content needs one more API call
// (prefetchContext in larkcontext.go). The real content used to go only into the
// `<harness:lark>` block, so **what users saw in the run history was that placeholder**. That is exactly what this change
// fixes: the frontend folds the harness block into the "platform injected" section that is collapsed by default, so it is effectively invisible.
//
// # Shape: the placeholder as a prefix, the expanded body after it
//
//	[卡片]
//	Production alert: TLS error count over threshold
//
// Keeping the placeholder as a prefix has two reasons that cannot be dropped, **neither of them cosmetic**:
//
//   - **It keeps the fact that "this was originally a card".** With only the expanded body, neither the agent nor the run history
//     can see the message's original form, and that affects how it is handled: the body of a merged forward is what **someone else** said,
//     and a card body is system generated; neither equals "what this person is telling you now".
//   - **It marks in place that the body is untrusted.** See usableExpanded in larkprompt.go:
//     once the body is in the user message, "this part is external data" can only be expressed by labeling its source.
//
// When the expanded body is not available, **fall back to the placeholder** (not an empty string): the agent at least knows "there is something here
// that it can fetch itself"; see the line in ExpandMessage in shared/infra/lark, "never replace `[卡片]` with
// an empty string". An empty user message would make this turn look like the user said nothing.
func larkUserText(msg *lark.InboundMessage, prefetched larkContext, log *slog.Logger) string {
	if msg == nil {
		return ""
	}
	expanded := usableExpanded(prefetched)
	if expanded == "" {
		// All three cases come here: not a card/forward (most messages; Expanded is empty anyway), failure
		// to fetch content, and a body disguised as a harness block being blocked. The first two are normal and need no log
		// (prefetchContext already logged each of them); the third is a security event and must leave a trace.
		if log != nil && strings.TrimSpace(prefetched.Expanded) != "" {
			// ⚠️ **Do not log the content itself**: it has already been judged suspicious, and putting it into the log as-is means moving a possible
			// injection payload into another system (the same discipline as appendUntrusted).
			log.Warn("展开的正文以 harness 前缀开头，疑似伪装成平台注入块，退回占位符",
				slog.Int("expandedLen", len(prefetched.Expanded)))
		}
		return msg.Text
	}

	placeholder := strings.TrimSpace(msg.Text)
	if placeholder == "" {
		// In theory the placeholder is never empty (decodeContent always gives one for these two types), but if it really is empty
		// we still must not drop the body, or users would again not see the content.
		return expanded
	}
	// ⚠️ The expanded body of a merged forward **carries its own more detailed marker of the same kind** (`[合并转发，共 3 条]`,
	// put on the first line by expand.go), so prepending another `[合并转发]` would give
	//
	//	[合并转发]
	//	[合并转发，共 3 条]
	//
	// two lines saying the same thing, with the lower line more complete. The test is **comparing the prefix with the closing bracket removed**
	// (`[合并转发` vs `[合并转发，共 3 条]`), not exact equality: the two are never equal,
	// so testing for equality would test nothing.
	if marker := strings.TrimSuffix(placeholder, "]"); marker != "" &&
		strings.HasPrefix(expanded, marker) {
		return expanded
	}
	return placeholder + "\n" + expanded
}

// claim tries to claim a message. Returning true means it is a new message and should be handled.
//
// It also stores the four topic anchors along the way (chat_type / thread_id / root_id / parent_id).
// They are the **inputs** of scopeForMessage, kept purely for later diagnosis: when "continuing in a topic opened a new
// session", feishu_session_scopes only has the computed key, which cannot tell whether Feishu gave no anchor or
// we computed the key wrong, and the fixes for the two have nothing in common. See the comments on those four fields in store.go.
func (in *Inbound) claim(ctx context.Context, msg *lark.InboundMessage) (bool, error) {
	return in.cfg.Store.ClaimInboundMessage(ctx, InboundMessageRecord{
		LarkAppID: msg.AppID,
		MessageID: msg.MessageID,
		ChatID:    msg.ChatID,
		Status:    inboundPending,
		CreatedAt: in.now(),
		ChatType:  msg.ChatType,
		ThreadID:  msg.ThreadID,
		RootID:    msg.RootID,
		ParentID:  msg.ParentID,
	})
}

func (in *Inbound) mark(ctx context.Context, messageID, status string) error {
	return in.cfg.Store.MarkInboundMessage(ctx, messageID, status)
}

// ---------------------------------------------------------------------------
// Accumulating the final answer
// ---------------------------------------------------------------------------

// answerBuffer collects the text the agent finally says, and stores it along the way.
//
// # Why store it
//
// The answer used to live only in process memory. Once amad restarts (deploy, OOM, panic), the text already produced
// is gone; on recovery SyncRunEvents can only fill in events **still kept on the Ark side**, and frames consumed by the previous
// process do not come again. So the user got an answer missing its beginning, with no sign that anything was missing.
//
// What is stored is InflightTurnRecord.LastText, the same field the recovery path reads.
//
// # Keep only "the last segment", not all agent.message joined together
//
// Before calling a tool, the agent first says a transitional sentence for humans: "let me pull the chat history first",
// "let me pull the messages of this thread". Those are **process narration, not the reply**, and they are the same kind of event
// as the final reply in the event stream (agent.message); the type alone cannot tell them apart.
//
// The dividing line is the tool call: `agent.message → agent.tool_use` means the sentence just now was narration of "what I am about to do";
// after the tool result comes back the agent goes on talking, and the real reply is **the last segment**. So
// reset() clears the buffer at tool_use, and what remains is the segment before the final frame.
//
// Measured (sesn-example-06): one turn had 4 agent.message segments; the first 3 were transitions
// or leftovers from other turns, and only the 4th was the reply. Joining them all gave exactly the mess users saw in Feishu.
//
// # ⚠️ Splitting must not really drop text: a turn may end with tool_use
//
// reset() used to be "clear b". That implementation assumed that **after a tool call the agent always speaks again**,
// and that does not hold: after the tool result comes back the agent can end the turn directly (it already said everything
// before calling the tool, or the result left it nothing to say). Then there is no agent.message after the last tool_use,
// so text() returns an empty string, and sendFinalReply replaces a **real answer** with "this turn produced no reply,
// something may have gone wrong during execution". Not only did the user not get the reply already produced, they were told execution failed.
//
// Measured on the first turn of local sesn-example-01 (seq 3→13): seq 6 was a complete
// clarifying reply ("before starting the analysis, I need to confirm a few definitions with you: ..."), a tool_use at seq 7
// cleared it, the agent said nothing after the tool result, and the final state was end_turn. The user should have received seq 6;
// what they actually got was that "something went wrong during execution".
//
// So reset() was changed to **keep a backup** (prev): normally text() uses the new segment collected after clearing,
// which was the original intent; when the new segment is empty it falls back to prev. Better to send text that "may be narration"
// than to replace a reply already produced with "something went wrong during execution". The trade-off is clear:
// in the former the user reads one extra transitional sentence; in the latter the user loses the answer and is misled into chasing a failure that does not exist.
type answerBuffer struct {
	// persist writes the collected text back to the store; may be nil (tests and callers that do not need recovery).
	persist func(cardMsgID, text string)

	mu sync.Mutex
	b  strings.Builder
	// prev is the text dropped by the most recent reset(), used only when the new segment is empty. See the type comment.
	prev string
}

// write appends a piece of text and stores it.
//
// Storing every frame looks heavy, but the write here is "one UPDATE against local PG", and the agent's text frames themselves
// are sparse (delta frames do not come here; agentText only recognizes agent.message). The real high-frequency throttling is left to
// the chain-of-thought side (progressPushInterval), which is where a frame comes every few characters.
func (a *answerBuffer) write(text string) {
	a.mu.Lock()
	a.b.WriteString(text)
	current := a.b.String()
	a.mu.Unlock()

	if a.persist != nil {
		// cardMsgID is passed empty: the new path no longer has "the card that keeps getting PATCHed";
		// this field is kept only for compatibility with old records.
		a.persist("", current)
	}
}

// reset splits the text into segments: it clears the current segment, but **keeps it as a backup**.
//
// Used at tool calls: see the type comment. The segment just now is usually narration of "I am about to call a tool", not the reply.
//
// ⚠️ **It must not really be dropped**: a turn may end with tool_use (the agent ends right after the tool result comes back),
// and clearing then would replace a real reply with "something went wrong during execution". The backup lets text() fall back when the new segment is empty.
//
// **The stored value is cleared too** (what is stored is the state after clearing, not the backup): if the process dies at this moment, the recovery path reads
// LastText and would send narration that is already obsolete as the beginning of the answer (that is exactly the entry point of
// answer.write(rec.LastText) in resumeLive). The backup lives only in this process and is not stored; the recovery path has
// its own fallback (see finishAndClear in resume.go).
func (a *answerBuffer) reset() {
	a.mu.Lock()
	// Only update the backup when this segment is non-empty: when there is no text between two consecutive tool_use calls,
	// the previous backup with content must not be overwritten with an empty string.
	if cur := a.b.String(); strings.TrimSpace(cur) != "" {
		a.prev = cur
	}
	a.b.Reset()
	a.mu.Unlock()

	if a.persist != nil {
		a.persist("", "")
	}
}

// text returns the text collected so far; when the current segment is empty it falls back to the backup left by reset().
//
// The fallback is deliberate; see "a turn may end with tool_use" in the type comment.
func (a *answerBuffer) text() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cur := a.b.String(); strings.TrimSpace(cur) != "" {
		return cur
	}
	return a.prev
}

// inflightPersister builds a callback that "writes this turn's progress back to the store".
//
// It is called for every piece of text collected, so it is deliberately cheap and does not block the main flow: a failed write is logged once and never returns
// an error. Cutting off the reply the user is reading because one write to an ops table failed would cost far more.
//
// Use a ctx without cancellation: this turn's runCtx may have timed out (long task), but progress should still be recorded.
func (in *Inbound) inflightPersister(ctx context.Context, rec InflightTurnRecord) func(string, string) {
	var once sync.Once
	return func(cardMsgID, text string) {
		rec.CardMessageID = cardMsgID
		rec.LastText = text
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := in.cfg.Store.PutInflightTurn(writeCtx, rec); err != nil {
			// Shout only once: this is a high-frequency action, and logging every time would flood the log.
			once.Do(func() {
				in.log.Warn("更新未结轮次记录失败，重启后可能丢掉已产出的那段答案",
					"run", rec.RunID, "error", err.Error())
			})
		}
	}
}

// replyBusy tells the user "the previous message is still being processed".
//
// With session reuse, turns on the same session must be serial (see the comment on TryBeginTurn in runAndReply),
// and a blocked message **must not be silently dropped**: users would think the bot is broken. The idempotency key is per message,
// so even if Feishu redelivers the same message there is only one notice card.
// interruptForRedirect is the first step of a redirect: it only sends "stop" to Ark, **not a new message**.
//
// # Why not RedirectRun
//
// RedirectRun is the "interrupt + immediately send a new message" primitive, used by "send now" on the chat page. On that path
// the person sending the message and the person receiving the answer are the same (the browser keeps the SSE open). The Feishu path is different:
// this function sends the new message, while receiving the answer needs a subscriber, and **after the interrupt the original subscriber is gone**.
// Using it causes "the message got into Ark but nobody waits for its answer", which is exactly that mixed-up reply bug in production.
//
// So here we only send the interrupt, and leave the new message for the caller to send through the normal path **after getting the lock**. That path subscribes,
// builds the <harness:lark> context, and anchors the answer back to the user's own message.
//
// # Failures must speak, not stay silent
//
// When the interrupt fails, the previous turn is still running, and this message is neither queued nor sent. Return an error so the caller falls back to
// "the previous message is still being processed", which is the most accurate description at that moment, and also tells the user that resending once is enough.
// # Return value
//
// true means the interrupt has been sent; the caller goes on to wait for the lock and runs this message through the normal path.
// false means this redirect did not happen (no body, or Ark refused the interrupt), **the user has already received
// the "previous message is still being processed" notice**, and the caller is done. Do not wait for the lock: the previous turn was never interrupted,
// and waiting for it would only waste a full window.
func (in *Inbound) interruptForRedirect(
	ctx context.Context, token string, msg *lark.InboundMessage, runID string, log *slog.Logger,
) (bool, error) {
	if strings.TrimSpace(msg.Text) == "" {
		// With no body (pure image/sticker and the like) there is nothing to switch to: we should not interrupt a turn for nothing.
		log.Info("转向被跳过：这条消息没有正文", "run", runID)
		return false, in.replyBusy(ctx, token, msg)
	}
	if err := in.cfg.Conversations.InterruptRun(ctx, runID); err != nil {
		log.Warn("发中断失败，退回「上一句还在处理中」", "run", runID, "error", err.Error())
		return false, in.replyBusy(ctx, token, msg)
	}
	log.Info("已打断上一轮，接下来按正常路径跑这一条（转向）", "run", runID)
	// The reaction is the only immediate feedback of this action: after the previous turn is killed, the chat sees "the bot suddenly
	// stopped talking about the previous thing"; without it the interrupt looks like a failure.
	markRedirected(ctx, in.cfg.Reactor, token, msg.MessageID, log)
	return true, nil
}

// awaitTurn waits for the previous turn to leave and hand over the serial lock, returning true once it has it.
//
// # Why wait, instead of starting right after sending the interrupt
//
// The interrupt is asynchronous: InterruptRun returning only means Ark accepted it; the running goroutine still has to
// receive the final frame, close the chain of thought, and remove the reaction before it deletes the inflight record in its defer. That takes
// a few hundred milliseconds to a few seconds. Without waiting, TryBeginTurn fails and the user's message is treated as "blocked".
//
// The wait limit is deliberately short (redirectLockWait): by this point the previous turn **has already been interrupted**; leaving
// takes only a few local steps, and waiting long means something else is wrong (e.g. that goroutine is stuck sending a message).
// Falling back to the queued notice then is better than waiting forever; the user at least knows to resend.
func (in *Inbound) awaitTurn(
	ctx context.Context, rec InflightTurnRecord, log *slog.Logger,
) (bool, error) {
	deadline := in.now().Add(redirectLockWait)
	for {
		// ⚠️ Also go through beginTurn: a lock obtained by waiting is still a lock, and registering late leaves the same window.
		begun, err := in.beginTurn(ctx, rec, runTimeout)
		if err != nil {
			return false, fmt.Errorf("等上一轮让位时开启轮次失败: %w", err)
		}
		if begun {
			return true, nil
		}
		if in.now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			// If ctx is gone, stop waiting: the caller will fall back to the queued notice.
			log.Debug("等上一轮让位被取消", "run", rec.RunID)
			return false, nil
		case <-time.After(redirectLockPoll):
		}
	}
}

func (in *Inbound) replyBusy(ctx context.Context, token string, msg *lark.InboundMessage) error {
	_, err := in.cfg.Sender.ReplyCard(ctx, token, msg.MessageID,
		lark.TextCard("上一句还在处理中，等这一轮答完再问我下一个问题。"),
		lark.ReplyCardOption{IdempotencyKey: "ama-busy:" + msg.MessageID})
	return err
}

// reclaimFinishedTurn double-checks when grabbing the lock fails: has the previous turn actually already ended?
//
// # Why we cannot trust the inflight_turns table alone
//
// That table expresses "a turn is running" by "a record exists", but it can only prove that **someone started a turn and failed
// to delete the record**. There are several reasons the delete can fail, and each has really happened:
//
//   - Ark does not close the SSE itself and the subscriber did not recognize the final frame → the record stays until runTimeout (this production incident);
//   - the delete step itself failed (PG hiccup), with only one Warn line in the log;
//   - the process was killed with kill -9, and the defer never got a chance to run.
//
// In these cases the staleAfter fallback has to wait a full runTimeout (60 minutes), during which every question from the user
// gets "the previous message is still being processed", while the agent finished answering long ago. **A deadlock blocking users is the worst outcome**,
// far worse than occasionally running two turns concurrently, so here we actively check the truth.
//
// The test uses the run's real state: a final state (idle / terminated) means the turn on the Ark side really ended,
// so the record is a leftover; delete it and let the new turn in. requires_action **is not final** (the agent is waiting for a tool result,
// and continues after the worker returns it); in that case new messages really should be blocked.
//
// true means a leftover record was reclaimed, and the caller can retry TryBeginTurn.
// Any error returns false: when we cannot find out the truth, keep the original judgment "a turn is running";
// better to let the user resend once than to open a second turn under real concurrency and garble the reply.
func (in *Inbound) reclaimFinishedTurn(ctx context.Context, runID string, log *slog.Logger) bool {
	status, err := in.cfg.Conversations.RunStatus(ctx, runID)
	if err != nil {
		log.Warn("核对会话状态失败，按「上一轮仍在跑」处理", "error", err.Error())
		return false
	}
	if !isTerminalRunStatus(status) {
		return false
	}
	if err := in.cfg.Store.DeleteInflightTurn(ctx, runID); err != nil {
		log.Warn("回收已结束轮次的残留记录失败", "status", status, "error", err.Error())
		return false
	}
	log.Info("上一轮其实已经结束，回收残留的轮次记录后继续", "status", status)
	return true
}
