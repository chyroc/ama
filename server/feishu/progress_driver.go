// The driver of progress delivery: pushes a ProgressDelivery from queued to completed.
//
// The meaning of the state machine fields is in progress.go, the two delivery paths in transport.go; this file only orchestrates:
//
//	grab the lease → make sure it is created remotely → push snapshots while running → close with the final state → release the lease
//
// # Three places that "look redundant but cannot be dropped"
//
// **1. Compare snapshots before every push.** Many frames in the agent's event stream do not affect display; without comparing, every frame
// becomes a remote call, and Feishu rate limits. Comparing is cheap; rate limiting is expensive.
//
// **2. Store right after creating remotely.** Even if the opening events were not sent, store cotID / messageID first.
// Without storing, if the process crashes at that moment the CoT is **lost forever**: it shows "thinking" in the chat,
// and nobody knows its ID, so it can neither be appended to nor closed.
//
// **3. Stop immediately when lease renewal fails.** It means this instance was stuck too long and the lease was taken over. Pushing on would have
// two instances pouring events into the same CoT, and the user would see the content jump back and forth.

package feishu

import (
	"context"
	"log/slog"
	"time"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// progressRetryBase is the backoff base after failure, growing exponentially with the consecutive failure count.
const progressRetryBase = 2 * time.Second

// progressDriver drives one delivery.
type progressDriver struct {
	store     ProgressStore
	transport progressTransport
	// fallback is the path taken when CoT is definitely rejected; may be nil (it is already the card path).
	fallback progressTransport
	token    string
	log      *slog.Logger
	now      func() time.Time
}

// push pushes the latest snapshot. If nothing changed it does nothing.
//
// Every call renews the lease: pushing events is itself proof of "I am still alive", and a separate renewal goroutine
// would add one more lifetime to manage.
func (p *progressDriver) push(ctx context.Context, runID string, snapshot ProgressSnapshot) error {
	d, err := p.store.GetProgressDelivery(ctx, runID)
	if err != nil {
		return err
	}
	if d.Finished() {
		// Already finished (maybe pushed to the end by another instance); do not touch it again.
		return nil
	}

	// 1. If nothing changed, do not push. See the file header.
	if d.AppliedSnapshot != nil && d.AppliedSnapshot.Equal(snapshot) {
		return nil
	}

	// 3. Stop immediately when renewal fails.
	if ok, err := p.store.RenewProgressLease(ctx, runID, p.token, ProgressLeaseTTL); err != nil {
		return err
	} else if !ok {
		return ErrProgressLeaseLost
	}

	transport := p.transport
	cotID, msgID, err := transport.ensure(ctx, d)
	if err != nil {
		// CoT definitely rejected → switch to the card. Result uncertain → go into create_uncertain and wait out the window.
		if p.fallback != nil && shouldFallbackToCard(err) {
			p.log.Warn("CoT 被拒，降级成普通卡片", "run", runID, "error", err.Error())
			transport = p.fallback
			p.transport, p.fallback = p.fallback, nil
			d.Transport = TransportCard
			cotID, msgID, err = transport.ensure(ctx, d)
		}
		if err != nil {
			return p.recordFailure(ctx, d, err)
		}
	}

	// 2. Once created, store it right away, even before the later pushes. See the file header.
	if d.RemoteMessageID != msgID || d.RemoteCOTID != cotID {
		d.RemoteCOTID, d.RemoteMessageID = cotID, msgID
		d.Status = ProgressActive
		now := p.now()
		d.CreateAttemptedAt = &now
		if err := p.store.SaveProgressDelivery(ctx, d, p.token); err != nil {
			return err
		}
	}

	prev := d.AppliedSnapshot
	if err := transport.apply(ctx, d, prev, snapshot); err != nil {
		return p.recordFailure(ctx, d, err)
	}

	d.AppliedSnapshot = &snapshot
	d.AppliedRevision++
	d.DesiredRevision = d.AppliedRevision
	d.ConsecutiveFailureCount = 0
	d.LastError = ""
	d.NextRetryAt = nil
	return p.store.SaveProgressDelivery(ctx, d, p.token)
}

// finish pushes the final state and closes.
func (p *progressDriver) finish(ctx context.Context, runID string, snapshot ProgressSnapshot) error {
	d, err := p.store.GetProgressDelivery(ctx, runID)
	if err != nil {
		return err
	}
	if d.Finished() {
		// Already final; nothing to close.
		//
		// ⚠️ **But there are two cases here, and they must be stated separately**: completed is a normal finish (silent),
		// while failed / create_uncertain means **that chain of thought will stay at "thinking" forever**:
		// nobody will close it anymore. The latter is a failure users really report, yet it shares the same `return nil`
		// with a normal finish (a failure disguised as normal, the shape this repo keeps hitting).
		if d.Status != ProgressCompleted {
			p.log.Warn("思维链已是非正常终态，本轮不再封口（用户侧会看到它一直停在「思考中」）",
				"run", runID, "chat", d.ChatID,
				"status", string(d.Status), "reason", d.StatusReason,
				"failures", d.ConsecutiveFailureCount)
		}
		return nil
	}
	// Not created remotely yet (e.g. it failed right at the start): nothing to close, just finish.
	// Forcing one to be created just to close it immediately would only leave an empty chain of thought in the chat.
	if d.RemoteMessageID == "" {
		d.Status = ProgressFailed
		d.StatusReason = "远端进度消息未建立"
		d.TerminalRequested, d.TerminalApplied = true, false
		return p.store.SaveProgressDelivery(ctx, d, p.token)
	}

	d.TerminalRequested = true
	if err := p.transport.finish(ctx, d, snapshot); err != nil {
		return p.recordFailure(ctx, d, err)
	}

	d.AppliedSnapshot = &snapshot
	d.AppliedRevision++
	d.DesiredRevision = d.AppliedRevision
	d.TerminalApplied = true
	d.Status = ProgressCompleted
	d.ConsecutiveFailureCount = 0
	d.LastError = ""
	return p.store.SaveProgressDelivery(ctx, d, p.token)
}

// recordFailure records a failure and schedules a retry; after reaching the consecutive failure limit it gives up.
//
// ⚠️ **Its return value is "did the bookkeeping succeed", not "did the delivery succeed".**
//
// This signature is easy to misread, and misreading it makes the whole delivery path vanish from the logs: below the limit
// it does `return SaveProgressDelivery(...)`, which is nil when the store write succeeds, so push / finish
// return a real delivery failure to the caller as success, and the caller's line
// `if err != nil { log.Warn("思维链封口失败…") }` **is never entered**:
// that Warn is dead code. In production the user's message stays at "thinking" forever,
// and not a single log line can be found (3 log lines on the inbound side, 0 for success or failure on the delivery side).
//
// Closing is especially fatal: `finish` is called only once per turn, and `ConsecutiveFailureCount` is usually still 1,
// so it never reaches the "limit reached" branch below, and does not even get to that Warn.
//
// The control flow is not changed (the error really is recorded in the store, and the retry really should be scheduled by backoff); instead **every exit
// logs by itself**: this is the only place that has "which delivery / which attempt / the underlying error" all at hand.
//
// What is logged are ids and counts: run id, chat id, delivery state, consecutive failure count, backoff duration,
// and the underlying error string. **The CoT body is not logged**: thinking text and tool output are user data,
// and diagnosis only needs "which step failed, how many times".
func (p *progressDriver) recordFailure(ctx context.Context, d *ProgressDelivery, cause error) error {
	d.ConsecutiveFailureCount++
	d.LastError = cause.Error()

	// The result of creating remotely is uncertain: it is its own final state, **with no retry**.
	// Retrying may leave a second chain of thought in the chat; see CreateUncertainWindow in progress.go.
	if d.RemoteMessageID == "" && lark.IsCOTOutcomeUnknown(cause) {
		d.Status = ProgressCreateUncertain
		d.StatusReason = "建进度消息时结果不确定，不重建以免出现两条"
		now := p.now()
		d.CreateAttemptedAt = &now
		// ⚠️ This Warn says "give up showing progress for this turn", and **must also say what the user sees**:
		// after entering this final state, later finish calls hit Finished() and return nil directly,
		// so that CoT stays at "thinking" forever. Without saying so, whoever sees this Warn
		// would not connect it with "the one the user reported as spinning forever".
		p.log.Warn("建进度消息结果不确定，放弃这一轮的进度展示（这条思维链会一直停在「思考中」，不会再被封口）",
			"run", d.RunID, "chat", d.ChatID, "error", cause.Error())
		return p.store.SaveProgressDelivery(ctx, d, p.token)
	}

	if d.ConsecutiveFailureCount >= ProgressMaxFailures {
		d.Status = ProgressFailed
		d.StatusReason = "连续失败次数达上限"
		p.log.Warn("进度投递连续失败，放弃",
			"run", d.RunID, "chat", d.ChatID, "failures", d.ConsecutiveFailureCount,
			"error", cause.Error())
		return p.store.SaveProgressDelivery(ctx, d, p.token)
	}

	// Exponential backoff.
	backoff := progressRetryBase * time.Duration(1<<(d.ConsecutiveFailureCount-1))
	next := p.now().Add(backoff)
	d.NextRetryAt = &next
	// ⚠️ **This is the most important line added in this change**: before it, failures 1..4 had zero logs
	// (the error was only written to the last_error column in the store, visible only if you thought of querying the store).
	//
	// Warn rather than Error: a single failure is recoverable, and the next throttle window starts over with the full diff.
	// When it really gives up at the limit, the line above already says so.
	//
	// The frequency is safe: the delivery side is already throttled (progressPushInterval),
	// and this only prints on failure, zero under normal traffic.
	p.log.Warn("思维链进度投递失败，已安排退避重试",
		"run", d.RunID, "chat", d.ChatID,
		"status", string(d.Status),
		"failures", d.ConsecutiveFailureCount,
		"backoff", backoff.String(),
		"error", cause.Error())
	return p.store.SaveProgressDelivery(ctx, d, p.token)
}
