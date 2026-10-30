// Queued pending messages: enqueue, take, send.
//
// # Not the same thing as "auto-resend when hitting the tool execution window"
//
// That one is a **race** of tens of milliseconds (Ark rejects user.message between sending
// tool_use and the worker returning tool_result). It has nothing to do with user intent; it is
// retried in place by sendEventsThroughWindow in chat.go with a 1-second budget. The user never
// notices.
//
// This file handles **queuing done on purpose by the user**: they know the agent is still running
// and has to wait a whole turn (tens of seconds to minutes), they just do not want to hold the
// message back. The waits differ by three orders of magnitude, so the handling is completely
// different: that one can sleep inside one HTTP request, this one cannot.
//
// # Who sends: take one when the turn ends
//
// Taking is hooked on the **long-lived subscription** (core/runwatcher.go): the moment it
// recognizes turnEnded is the only reliable signal that "the previous turn really ended, the
// next message can go".
//
//	Ark pushes session.status_idle (stop_reason != requires_action)
//	  ↓ the watcher callback recognizes turnEnded
//	  ↓ DrainPendingMessage: PG atomically claims the head of the queue
//	  ↓ AppendRunEvents sends it → delete that record
//	  ↓ a new turn starts; the watcher subscribes to it again on its next tick
//
// ⚠️ **The check must be turnEnded, not "the run became idle"**: requires_action is also idle on
// the Ark side, but it means "stuck waiting for a tool result", and the agent will continue.
// Treating it as the end would push queued messages in while the agent is still running, which
// is exactly what this feature is meant to avoid.
//
// # ⚠️ Taking only happens "while the subscription is alive", and subscriptions are started by the patrol
//
// Taking is hooked on the subscription callback, so **turns that end while nothing is subscribed
// are missed**. The patrol only scans every watchScanInterval (30 seconds), so there is a real
// window:
//
//	user sends a message → this turn starts
//	  ↓ (subscription not started yet, up to 30 seconds)
//	this turn ends ← no subscriber sees turnEnded
//	  ↓ the patrol subscribes only on its next tick
//	the queued message is still in the queue, and only goes out when the **next** turn ends
//
// Measured twice on a real deployment: two turns both ended before the subscription started, and
// the queue sat still for 90 seconds (not a single "queued message sent" line in the logs); once
// the subscription was alive and another turn was triggered, it was taken within 5 seconds.
// **When the queue does not move, first check whether this session was subscribed at that time**,
// do not go check the claim SQL: that path is correct, just nobody is listening.
//
// This does not affect correctness (no message lost, no reordering, no resend), only latency. To
// really remove it, the session would have to be added to the subscription table in place right
// after AppendRunEvents succeeds, instead of waiting for the patrol to find it.
//
// # Take only one at a time
//
// Taking two would send both into Ark at once, and Ark rejects the second one while a turn is in
// progress (that window). So after sending one we stop, and take the next when its turn ends; the
// queue drains one by one naturally.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/gopkg/arkframe"
)

const (
	// pendingQueueLimit is the maximum number of queued messages per session.
	//
	// Same value as SEND_QUEUE_LIMIT on the frontend (web/src/lib/useSendQueue.ts): if the two
	// differ, it shows up as "the frontend says you can queue more, the backend rejects", and that
	// rejection makes no sense to the user.
	//
	// 20 is "enough and still readable": reaching it means the agent has been running a long time,
	// and piling on more just makes it harder to tell which message is which.
	pendingQueueLimit = 20

	// pendingClaimLease is the claim lease for taking.
	//
	// It only covers the short "claim → send" step (one Ark call, seconds). 2 minutes is a loose
	// value; if the holder is rebuilt, another instance takes over after at most this long.
	pendingClaimLease = 2 * time.Minute

	// pendingMaxAttempts is how many times a message is sent automatically at most.
	//
	// Once reached, it is marked failed and the original text is kept for the user. Without a
	// limit, a message that "can never be sent" (e.g. the linked agent was disabled) would be
	// retried at the end of every turn, flooding error logs forever.
	pendingMaxAttempts = 3
)

// EnqueuePendingMessage appends a message to the end of this session's queue.
//
// It returns all queued messages of the session after enqueuing, for the frontend to render
// directly (saves a round trip).
//
// **When full it returns ErrPrecondition instead of silently dropping**: silently dropping is the
// worst way this feature can fail. The user thinks it was sent, then waits for a reply that never
// comes.
func (s *Service) EnqueuePendingMessage(
	ctx context.Context, runID, text string,
) ([]apimodel.PendingMessage, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: 消息不能为空", ErrInvalidArgument)
	}
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("%w: 这次运行不存在", ErrNotFound)
	}

	// Frozen sessions cannot queue. **This gate must not be skipped**: without it, users could pile
	// messages into a queue that can never be sent, and each one would have to run all
	// pendingMaxAttempts before failing. The text matches the two in AppendRunEvents word for
	// word: to the user it is the same thing.
	if err := s.ensureRunWritable(ctx, run); err != nil {
		return nil, err
	}

	existing, err := s.store.ListPendingMessages(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("读排队消息失败: %w", err)
	}
	if waiting := countWaiting(existing); waiting >= pendingQueueLimit {
		return nil, fmt.Errorf(
			"%w: 已有 %d 条消息在排队，先取消几条再发", ErrPrecondition, waiting)
	}

	now := time.Now().UTC()
	m := apimodel.PendingMessage{
		ID:     s.nextID("pmsg"),
		RunID:  runID,
		Text:   text,
		Status: apimodel.PendingWaiting,
		Source: apimodel.PendingFromWeb,
		Actor:  s.ownerFrom(ctx).Email,
	}
	if err := s.store.EnqueuePendingMessage(ctx, m, now); err != nil {
		return nil, err
	}
	return s.store.ListPendingMessages(ctx, runID)
}

// ListPendingMessages lists messages on this session that have not been sent yet, in enqueue order.
func (s *Service) ListPendingMessages(
	ctx context.Context, runID string,
) ([]apimodel.PendingMessage, error) {
	if _, err := s.store.GetRun(ctx, runID); err != nil {
		return nil, fmt.Errorf("%w: 这次运行不存在", ErrNotFound)
	}
	return s.store.ListPendingMessages(ctx, runID)
}

// CancelPendingMessage cancels a queued message and drops it.
func (s *Service) CancelPendingMessage(ctx context.Context, runID, id string) error {
	msgs, err := s.store.ListPendingMessages(ctx, runID)
	if err != nil {
		return fmt.Errorf("读排队消息失败: %w", err)
	}
	for _, m := range msgs {
		if m.ID == id {
			return s.store.DeletePendingMessage(ctx, id)
		}
	}
	// Not found means it was already cancelled (idempotent). **Do not return 404**: users often
	// click cancel twice, and "does not exist" on the second click only makes them think something
	// went wrong.
	return nil
}

// DrainPendingMessage takes the head of this session's queue and sends it, one at a time.
//
// # When it is called: the moment turnEnded is recognized
//
// Called by the long-lived subscription (core/runwatcher.go) when it receives a terminal frame.
// **That is the only reliable signal**: an idle of the requires_action kind is not an end, and
// polling run.Status would push messages in while the agent is still running.
//
// It returns whether a message was really sent. If so, a new turn has started, the watcher
// subscribes to this session again on its next tick, and the next message is taken when that
// turn ends; that is how the queue drains one by one.
//
// **Best-effort overall, returns no error**: the worst result of a failed take is falling back to
// "this feature does not exist" (the message stays in the queue and is retried next turn); it
// should not mark the subscription path as failed.
func (s *Service) DrainPendingMessage(ctx context.Context, runID, owner string) bool {
	m, ok, err := s.store.ClaimNextPendingMessage(
		ctx, runID, owner, time.Now(), pendingClaimLease)
	if err != nil {
		s.log().Warn("认领排队消息失败", "run", runID, "error", err.Error())
		return false
	}
	if !ok {
		// Nothing pending. This is the normal result for most turns; do not log (it would drown out useful logs).
		return false
	}

	// Retry limit reached: mark failed, keep the original text for the user.
	//
	// Checked **after** claiming: claiming does attempts+1, so what we see here is "which attempt
	// this is, counting this one".
	if m.Attempts > pendingMaxAttempts {
		s.log().Warn("排队消息反复发不出去，放弃",
			"run", runID, "message", m.ID, "attempts", m.Attempts)
		s.failPending(ctx, m.ID, fmt.Sprintf(
			"自动发送 %d 次都没成功，这条消息没有发出。", pendingMaxAttempts))
		return false
	}

	event, err := json.Marshal(map[string]any{
		"type":    "user.message",
		"content": []map[string]any{{"type": "text", "text": m.Text}},
	})
	if err != nil {
		s.failPending(ctx, m.ID, "拼消息事件失败")
		return false
	}

	if _, err := s.AppendRunEvents(ctx, runID, []json.RawMessage{event}); err != nil {
		// **Tell "did not work this time" from "will never work".**
		//
		// A precondition failure (agent deleted/disabled, no Ark key configured) will not get better
		// however many times we retry, so mark it failed right away instead of running all three
		// attempts, each of which calls Ark. Everything else (hit the window, network blip, 5xx)
		// goes back to the queue and is retried next turn.
		if errors.Is(err, ErrPrecondition) || errors.Is(err, ErrInvalidArgument) {
			s.log().Warn("排队消息发不出去且重试无益，放弃",
				"run", runID, "message", m.ID, "error", err.Error())
			s.failPending(ctx, m.ID, userMessage(err))
			return false
		}
		// **No need to put it back explicitly**: once the lease expires it becomes claimable again
		// (that is what `claimed_until < now` in ClaimNextPendingMessage is for). The cost is
		// waiting a full lease (2 minutes) before retrying, which is acceptable: getting here means
		// the previous attempt really failed, and an immediate retry would most likely fail too.
		s.log().Warn("排队消息这次没发成，等租约过期后重试",
			"run", runID, "message", m.ID, "error", err.Error())
		return false
	}

	// Delete once sent. **No sent state is kept**: keeping it would only make the "how many are
	// still queued on this session" query slower over time, and "this message was sent" is
	// already recorded in run_events.
	if err := s.store.DeletePendingMessage(ctx, m.ID); err != nil {
		// Failing to delete is real trouble: this message would be sent again next turn. So log it as Warn instead of swallowing it.
		s.log().Warn("排队消息已发出但删除失败，下一轮可能重复发送",
			"run", runID, "message", m.ID, "error", err.Error())
	}
	s.log().Info("排队消息已发出", "run", runID, "message", m.ID)
	return true
}

// drainAllowed decides whether this frame's "turn ended" is the kind after which **the next
// message may go**.
//
// It is one step narrower than turnEnded, with a single difference: **an interrupt by the user
// (stop_reason is user_interrupt) does not count**.
//
// # Why interrupts must be excluded
//
// Redirect ("send now") is implemented as two steps, interrupt + message (RedirectRun in
// core/chat.go). The interrupt makes Ark push a session.status_idle, which satisfies turnEnded,
// so between those two steps the take would think "the previous turn ended" and send **the head
// of the queue**.
//
// The user clicked "send message 3 right now", and what goes out is message 3 **and** message 1,
// in no fixed order. This is the same shape of trap as "requires_action is not an end": an
// intermediate idle is mistaken for "this turn really ended".
//
// ⚠️ **Do not change turnEnded itself**: it is also used by the scheduled task path
// (scheduler.go), which wants exactly "however it ended, this turn is over". It is right for the
// two checks to differ.
//
// The queue does not get stuck forever after an interrupt: an interrupt means the user wants to
// cut in, that message itself starts a new turn, and when that turn ends normally the next one
// is taken as usual.
func drainAllowed(raw []byte) bool {
	return arkframe.StopReason(raw) != "user_interrupt"
}

// failPending marks a queued message as unsendable. A failure here is only logged; the next turn will try once more.
func (s *Service) failPending(ctx context.Context, id, reason string) {
	if err := s.store.FailPendingMessage(ctx, id, reason); err != nil {
		s.log().Warn("标记排队消息失败时出错", "message", id, "error", err.Error())
	}
}

// countWaiting counts messages still waiting (excluding failed ones, which are no longer sent automatically and do not use the quota).
func countWaiting(msgs []apimodel.PendingMessage) int {
	n := 0
	for _, m := range msgs {
		if apimodel.NormalizePendingStatus(m.Status) != apimodel.PendingFailed {
			n++
		}
	}
	return n
}

// ensureRunWritable decides whether this session can still accept new messages.
//
// Pulled out for queuing: **block at enqueue time**, instead of finding out at take time that it
// cannot be sent. The text matches the two in AppendRunEvents word for word: to the user it is
// the same thing.
func (s *Service) ensureRunWritable(ctx context.Context, run apimodel.Run) error {
	agent, err := s.store.GetAgent(ctx, run.AgentID)
	switch {
	case err == nil && agent.Status.IsDisabled():
		return fmt.Errorf("%w: 关联的 Agent 已停用，这个会话只能查看历史；要继续对话请先到 Agent 详情页启用它", ErrPrecondition)
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound):
		return fmt.Errorf("%w: 关联的 Agent 已被删除，这个会话只能查看历史，不能再发消息", ErrPrecondition)
	default:
		// fail-open, same reason as AppendRunEvents: a database blip should not look like "your agent was deleted".
		s.log().Warn("查 agent 失败，本次排队不做冻结判断",
			"run", run.ID, "error", err.Error())
		return nil
	}
}
