// The two paths for delivering progress: the CoT message (main) and the card summary (fallback).
//
// # Why two
//
// CoT is a **preview feature** of Feishu, not enabled for every tenant and every chat. When it cannot be created, we must not just show
// no progress at all: users would wait minutes after @-mentioning the bot in the chat with no feedback. So there is a
// fallback: an ordinary interactive card PATCHed in place. It works worse (no folding, long thinking takes up the screen),
// but "there is feedback" is preserved.
//
// # When to degrade and when not to
//
// **Degrade only when "definitely rejected", not when "the result is uncertain".** This distinction is the whole reason
// the error classification in shared/infra/lark/cot.go exists:
//
//	definitely rejected (4xx)  certainly not created remotely  → safe, can switch to the card
//	result uncertain (5xx)     may already have been created   → creating another card means two progress messages in the chat
//
// The latter goes into the create_uncertain state and waits out a window; see progress.go.
package feishu

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// COTMessenger is the three CoT APIs, provided by shared/infra/lark.
//
// It is an interface instead of using *lark.Client directly: tests need to plug in a fake, and the fallback logic must be able to simulate
// the two failures "definitely rejected" and "result uncertain".
type COTMessenger interface {
	CreateCOT(ctx context.Context, token, receiveIDType, receiveID string, opt lark.CreateCOTOption) (*lark.COTMessage, error)
	AppendCOT(ctx context.Context, token, cotID, messageID string, events []lark.COTEvent) error
	CompleteCOT(ctx context.Context, token, cotID, messageID string, reason lark.COTCompleteReason) error
}

// progressTransport is the ability to "deliver the snapshot", implemented once for each path.
type progressTransport interface {
	// ensure makes sure it has been created remotely, returning (cotID, messageID). If already created, it returns directly.
	ensure(ctx context.Context, d *ProgressDelivery) (string, string, error)
	// apply pushes the changes from prev → next.
	apply(ctx context.Context, d *ProgressDelivery, prev *ProgressSnapshot, next ProgressSnapshot) error
	// finish closes it.
	finish(ctx context.Context, d *ProgressDelivery, snapshot ProgressSnapshot) error
}

// ---------------------------------------------------------------------------
// CoT main path
// ---------------------------------------------------------------------------

type cotTransport struct {
	messenger COTMessenger
	token     string
	events    *cotEventBuilder
	now       func() time.Time
	// log exists for only one place: the silent branch where the opening events fail (see ensure).
	// It is defaulted to slog.Default() at construction (the same way as inbound.go / supervisor.go in this package),
	// so it is never nil here.
	log *slog.Logger
}

func newCOTTransport(m COTMessenger, token, runID string, now func() time.Time, log *slog.Logger) *cotTransport {
	if log == nil {
		log = slog.Default()
	}
	return &cotTransport{messenger: m, token: token, events: newCOTEventBuilder(runID), now: now, log: log}
}

func (t *cotTransport) ensure(ctx context.Context, d *ProgressDelivery) (string, string, error) {
	if d.RemoteCOTID != "" && d.RemoteMessageID != "" {
		return d.RemoteCOTID, d.RemoteMessageID, nil
	}
	msg, err := t.messenger.CreateCOT(ctx, t.token, "chat_id", d.ChatID, lark.CreateCOTOption{
		OriginMessageID: d.OriginMessageID,
		ReplyInThread:   d.ReplyInThread,
		// When the chat does not support topic replies, drop that parameter and retry automatically; that is a definite rejection with no side effect.
		AllowReplyInThreadFallback: true,
	})
	if err != nil {
		return "", "", err
	}
	// Right after creating it, send the opening events so the client knows this is a running chain of thought.
	opening := t.events.Start(t.now())
	if err := t.messenger.AppendCOT(ctx, t.token, msg.COTID, msg.MessageID, opening); err != nil {
		// Failing to send the opening does not affect what follows: the next apply will start over with the full diff.
		// Here we return the IDs so the caller stores them first; **otherwise this CoT is lost**.
		//
		// ⚠️ **But this return looks exactly like the success one** (both cotID, msgID, nil),
		// and the caller cannot tell "the opening was sent" from "the opening failed". Without a log,
		// when an **empty, forever spinning** chain of thought appears in the chat, there is no trace in the logs.
		//
		// This log is also the **way to verify** the claim "the next apply will start over with the full diff":
		// only if it appeared and no later delivery failed do we know that fallback really caught it.
		t.log.Warn("思维链开场事件发送失败，等下一次 apply 带完整差异重来",
			"run", d.RunID, "chat", d.ChatID, "cot", msg.COTID, "error", err.Error())
		return msg.COTID, msg.MessageID, nil
	}
	return msg.COTID, msg.MessageID, nil
}

func (t *cotTransport) apply(ctx context.Context, d *ProgressDelivery, prev *ProgressSnapshot, next ProgressSnapshot) error {
	events := t.events.Diff(prev, next, t.now())
	return t.append(ctx, d, events)
}

func (t *cotTransport) finish(ctx context.Context, d *ProgressDelivery, snapshot ProgressSnapshot) error {
	// Close the unclosed ones first, then close the CoT. In the reverse order the added events would be rejected (no appends after closing),
	// and those tools would show as spinning forever.
	if err := t.append(ctx, d, t.events.Terminal(snapshot, t.now())); err != nil {
		return err
	}
	reason := lark.COTCompleteDone
	if snapshot.Outcome == "error" {
		reason = lark.COTCompleteError
	}
	// **Do not use timeout**: the client only renders the inner RUN_FINISHED when the outer reason is done,
	// and timeout would show a cancelled run as timed out. See the comment on lark.COTCompleteTimeout.
	return t.messenger.CompleteCOT(ctx, t.token, d.RemoteCOTID, d.RemoteMessageID, reason)
}

// append sends in batches according to the platform limit.
func (t *cotTransport) append(ctx context.Context, d *ProgressDelivery, events []lark.COTEvent) error {
	for _, batch := range batchCOTEvents(events) {
		if err := t.messenger.AppendCOT(ctx, t.token, d.RemoteCOTID, d.RemoteMessageID, batch); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Card fallback path
// ---------------------------------------------------------------------------

type cardTransport struct {
	sender MessageSender
	token  string
	runID  string
}

func newCardTransport(sender MessageSender, token, runID string) *cardTransport {
	return &cardTransport{sender: sender, token: token, runID: runID}
}

func (t *cardTransport) ensure(ctx context.Context, d *ProgressDelivery) (string, string, error) {
	if d.RemoteMessageID != "" {
		return "", d.RemoteMessageID, nil
	}
	sent, err := t.sender.ReplyCard(ctx, t.token, d.OriginMessageID,
		lark.TextCard(renderProgressCard(ProgressSnapshot{})), lark.ReplyCardOption{
			ReplyInThread: d.ReplyInThread,
			// Stable idempotency key: if creating the card succeeds but we crash before writing the ID back, Feishu dedupes on retry by itself,
			// and it does not become two cards.
			IdempotencyKey: "ama-progress:" + t.runID,
		})
	if err != nil {
		return "", "", err
	}
	return "", sent.MessageID, nil
}

func (t *cardTransport) apply(ctx context.Context, d *ProgressDelivery, _ *ProgressSnapshot, next ProgressSnapshot) error {
	return t.sender.UpdateCard(ctx, t.token, d.RemoteMessageID, lark.TextCard(renderProgressCard(next)))
}

func (t *cardTransport) finish(ctx context.Context, d *ProgressDelivery, snapshot ProgressSnapshot) error {
	return t.sender.UpdateCard(ctx, t.token, d.RemoteMessageID, lark.TextCard(renderProgressCard(snapshot)))
}

// renderProgressCard renders the snapshot into the body of the fallback card.
//
// Cards cannot fold, so here **only a summary is given**: the thinking text is not laid out in full (long thinking would fill the screen),
// and tool calls only list name and status. The real details are left to the CoT path.
func renderProgressCard(s ProgressSnapshot) string {
	var b []string
	if s.Terminal {
		if s.Outcome == "error" {
			b = append(b, "**执行失败**")
			if s.ErrorMessage != "" {
				b = append(b, s.ErrorMessage)
			}
		} else {
			b = append(b, "**已完成**")
		}
	} else {
		b = append(b, "**思考中…**")
	}
	for _, tool := range s.Tools {
		status := "执行中"
		if tool.Done {
			status = "完成"
		}
		b = append(b, fmt.Sprintf("- `%s` %s", nonEmpty(tool.Name, "tool"), status))
	}
	out := ""
	for i, line := range b {
		if i > 0 {
			out += "\n"
		}
		out += line
	}
	return out
}

// ---------------------------------------------------------------------------
// Choosing the path
// ---------------------------------------------------------------------------

// shouldFallbackToCard decides whether a CoT failure should degrade to the card.
//
// **Degrade only when definitely rejected.** An uncertain result means the CoT may already have been created,
// and creating another card would leave two progress messages running in the chat. See the file header.
func shouldFallbackToCard(err error) bool {
	if err == nil {
		return false
	}
	if lark.IsCOTOutcomeUnknown(err) {
		return false
	}
	// Bad parameters, no permission, the tenant has not enabled this feature: all definite, so degrading is safe.
	var apiErr *lark.APIError
	return errors.As(err, &apiErr)
}
