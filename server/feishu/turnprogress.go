// Chain-of-thought delivery for one conversation turn: wires progressDriver into the inbound path.
//
// progress.go defines the state machine, transport.go the two delivery paths, progress_driver.go does the pushing,
// and this file is the layer that **wires them into a real conversation turn**: open a delivery, feed it while running, close with the final state.
//
// Before this file existed, none of those three files had any production caller: they compiled and their unit tests were all green,
// but the chat only ever had one card. See "why the chain of thought did not take effect" in the README.
//
// # The delivery key is the "turn", not the "session"
//
// This is the most important thing in this file. The primary key column of feishu_progress_deliveries is called run_id,
// but **what we store in it is the turn key** (runID + source message ID), because with sessions reused by scope
// one runID runs many turns:
//
//	first turn finishes → that delivery goes into completed (final)
//	second turn starts  → EnsureProgressDelivery hits the primary key and reads back the first turn's **already final** record
//	                    → the driver sees Finished() and returns directly, pushing nothing
//
// So from the second turn on there is no chain of thought, with zero errors throughout: clean logs, every return value nil.
// This is the same kind of pitfall turnIdempotencyKey guards against (see that comment in inbound.go);
// the only difference is that one breaks the card, this one breaks the chain of thought.
//
// **Why the DDL is not changed**: this repo's schema is "go:embed + run once on every startup", all
// create ... if not exists, with no alter table and no version table (see the notes in schema_pg.sql).
// Changing the primary key would not take effect on databases where the table already exists, so new local databases would have a composite key while old production ones keep a single column;
// that split is far more dangerous than changing the key value here. The column type is text, and what goes in is up to us.
package feishu

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// progressPushInterval is the minimum interval for pushing progress to the chain of thought.
//
// Same reason as cardUpdateInterval: pushing every time the agent emits a few characters gets rate limited by Feishu, and the client itself
// has rendering latency, so users would not see a difference however fast. **The final state is not bound by it**; it is pushed unconditionally.
const progressPushInterval = 1 * time.Second

// turnProgress is the chain-of-thought delivery of one conversation turn.
//
// It can be nil: no ProgressStore / CoT ability configured, or the lease is held by another instance. Then every method
// is a no-op, and this turn only has "reaction + final answer", with no chain of thought. **This is degradation, not failure**:
// keeping "the user gets an answer" matters more than showing the thinking process.
type turnProgress struct {
	driver  *progressDriver
	key     string
	builder *snapshotBuilder
	log     *slog.Logger
	now     func() time.Time

	mu       sync.Mutex
	lastPush time.Time
	// stopped records whether pushing has been given up (the lease was lost, or it was closed).
	stopped bool
}

// startTurnProgress opens a chain-of-thought delivery for this turn.
//
// If it cannot (not configured, lease not grabbed, record not created) it always returns nil, not an error: the chain of thought is an **enhancement**,
// and it failing to run should not fail this conversation turn. When it returns nil the caller goes on with the reaction and final answer.
//
// # An empty agentID is treated as "show"
//
// The recovery path (resume.go) uses the inflight record in the store, and agent_id is a column added later,
// so **existing rows read back an empty string**. An empty string cannot be looked up to an agent, and AgentShowsProgress fails open
// to true, the same as the behavior before this switch was added, so existing recovered turns do not suddenly lose their chain of thought.
func (in *Inbound) startTurnProgress(
	ctx context.Context, token string, rec InflightTurnRecord, key, agentID string, log *slog.Logger,
) *turnProgress {
	if in.cfg.Progress == nil {
		// No state machine wired: this happens locally with AMA_STORE=mem and no in-memory implementation plugged in.
		log.Debug("未配置 ProgressStore，这一轮没有思维链")
		return nil
	}

	// This agent is configured to "not show detailed work": do not create it at all.
	//
	// **Not created and then hidden**: Feishu's cot_hidden parameter was measured to make the message stay at "thinking" forever;
	// see shared/infra/lark/cot.go. Not creating it at all is the real "do not show", and it takes the existing degradation path
	// at the start of this file (turnProgress is nil, and everything after is a no-op).
	if !in.cfg.Conversations.AgentShowsProgress(ctx, agentID) {
		log.Debug("该 agent 配置为不展示详细工作内容，这一轮不建思维链", "agent", agentID)
		return nil
	}

	d := &ProgressDelivery{
		// The turn key, not runID. See the file header for why: using runID would silently drop the chain of thought from the second turn on.
		RunID:           key,
		LarkAppID:       rec.LarkAppID,
		ChatID:          rec.ChatID,
		OriginMessageID: rec.SrcMessageID,
		ReplyInThread:   rec.ReplyInThread,
		Transport:       TransportCOT,
	}
	if _, err := in.cfg.Progress.EnsureProgressDelivery(ctx, d); err != nil {
		log.Warn("建思维链投递记录失败，这一轮只发最终答案", "error", err.Error())
		return nil
	}

	lease, err := newLeaseToken()
	if err != nil {
		log.Warn("生成租约令牌失败，这一轮只发最终答案", "error", err.Error())
		return nil
	}
	if _, ok, err := in.cfg.Progress.ClaimProgressDelivery(ctx, key, lease, ProgressLeaseTTL); err != nil {
		log.Warn("认领思维链投递失败，这一轮只发最终答案", "error", err.Error())
		return nil
	} else if !ok {
		// Another instance holds the lease of this delivery. **Not an error**: a normal result under concurrency;
		// that instance will finish pushing the chain of thought, and we only care about the answer.
		log.Info("思维链投递已被别的实例认领，本轮不推进度")
		return nil
	}

	// CoT is the main path, the card is the fallback path. Degrading only happens on "definite rejection" (the tenant has not enabled the CoT
	// preview, invalid parameters); with an uncertain result it does not degrade, since it may already have been created; see transport.go.
	var cot progressTransport
	if in.cfg.COT != nil {
		cot = newCOTTransport(in.cfg.COT, token, key, in.now, log)
	}
	card := newCardTransport(in.cfg.Sender, token, key)
	primary, fallback := cot, progressTransport(card)
	if primary == nil {
		// No CoT ability wired: go straight to the card, with no second fallback.
		log.Debug("未配置 CoT 能力，进度走降级卡片")
		primary, fallback = card, nil
	}

	return &turnProgress{
		driver: &progressDriver{
			store:     in.cfg.Progress,
			transport: primary,
			fallback:  fallback,
			token:     lease,
			log:       log,
			now:       in.now,
		},
		key:     key,
		builder: newSnapshotBuilder(),
		log:     log,
		now:     in.now,
	}
}

// consume eats one Ark event frame, and pushes the latest snapshot when needed.
//
// Throttling is controlled by progressPushInterval, and **the timer only counts when the snapshot really changed**: unchanged frames should not even
// take up time, or a string of irrelevant frames would waste the quota on "nothing changed".
func (p *turnProgress) consume(ctx context.Context, raw []byte) {
	if p == nil {
		return
	}
	if !p.builder.Consume(raw) {
		return
	}

	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	if p.now().Sub(p.lastPush) < progressPushInterval {
		p.mu.Unlock()
		return
	}
	p.lastPush = p.now()
	p.mu.Unlock()

	p.push(ctx)
}

// push pushes the current snapshot once. Failure is only logged: progress failing to push should not affect this turn's answer.
func (p *turnProgress) push(ctx context.Context) {
	if err := p.driver.push(ctx, p.key, p.builder.Snapshot()); err != nil {
		// When the lease is lost it must **stop immediately**, or two instances would pour events into the same chain of thought,
		// and the user would see the content jump back and forth (see the third item in the progress_driver.go file header).
		if errors.Is(err, ErrProgressLeaseLost) {
			p.mu.Lock()
			p.stopped = true
			p.mu.Unlock()
			p.log.Warn("思维链租约已易主，本轮停止推进度", "key", p.key)
			return
		}
		p.log.Warn("推思维链进度失败", "key", p.key, "error", err.Error())
	}
}

// finish marks the final state and closes.
//
// outcome is done / error, and message only matters for error. After closing, this chain of thought accepts no more
// appends, so **unclosed tools must be closed here**; that is done by cotEventBuilder.Terminal
// in the transport layer (see "why closing at the end cannot be skipped" in cotevents.go).
//
// # One more push is required before closing
//
// Throttling (progressPushInterval) means **progress in the last window has not been pushed yet**. And the Terminal
// batch only "finishes up": it adds END + RESULT for unclosed tools, but does not add START;
// that is Diff's job. So for a tool call that only appeared in the last second, the client only receives its end
// and never its start; by AG-UI semantics that event has nowhere to attach, and the whole tool disappears.
//
// Likewise the last piece of thinking text would be lost. Neither gives an error: closing succeeds, the logs are clean, only part of the content is missing.
//
// So first push the full diff unconditionally (push compares snapshots internally, and makes no remote call when nothing really changed),
// then close.
func (p *turnProgress) finish(ctx context.Context, outcome, message string) {
	if p == nil {
		return
	}

	// First push what accumulated in the last throttle window, then mark the final state.
	//
	// The order cannot be reversed: marking final first and then pushing would give Diff a snapshot with the Terminal mark,
	// while the real closing events are done by driver.finish below, so they would be duplicated.
	p.mu.Lock()
	stopped := p.stopped
	p.mu.Unlock()
	if !stopped {
		p.push(ctx)
	}

	p.builder.MarkTerminal(outcome, message)

	p.mu.Lock()
	stopped = p.stopped
	p.stopped = true
	p.mu.Unlock()
	if stopped {
		// The lease no longer belongs to us; leave closing to whoever took over, since forcing a push here would only fight.
		return
	}

	snapshot := p.builder.Snapshot()
	if err := p.driver.finish(ctx, p.key, snapshot); err != nil {
		p.log.Warn("思维链封口失败，用户可能看到一条一直在转的思维链",
			"key", p.key, "error", err.Error())
	}
}

// release gives up the lease so another instance can take over right away.
//
// It only matters when **closing failed**: after a successful close SaveProgressDelivery has already cleared the lease.
// Failure is not logged: the lease expires anyway, and failing to release it at most delays takeover by 30 seconds.
func (p *turnProgress) release(ctx context.Context, store ProgressStore) {
	if p == nil || store == nil {
		return
	}
	_ = store.ReleaseProgressLease(ctx, p.key, p.driver.token)
}

// progressDeliveryKey builds the chain-of-thought delivery key of one conversation turn.
//
// It must include the source message ID; see the file header for why: with session reuse one runID runs many turns, and using only runID
// would silently drop the chain of thought from the second turn on. The value rule is the same as turnIdempotencyKey (one user message
// is one turn, and it does not change on retry); the two are just used in different places.
func ProgressDeliveryKey(runID, srcMessageID string) string {
	if strings.TrimSpace(srcMessageID) == "" {
		// Should not happen in theory (inbound messages always carry a message ID). Falling back to runID at least keeps retries of the same turn idempotent.
		return runID
	}
	return runID + ":" + srcMessageID
}

// newLeaseToken generates a lease token.
//
// It must be different every time: when two instances get the same token, RenewProgressLease would take the other's lease
// as its own, so "who holds it" loses meaning, and both push into the same chain of thought.
func newLeaseToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成租约令牌失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
