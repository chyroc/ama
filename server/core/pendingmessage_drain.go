package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
)

const (
	// DefaultPendingClaimLease covers claim and one send attempt.
	DefaultPendingClaimLease = 2 * time.Minute
	// DefaultPendingMaxAttempts bounds automatic retries of a queued message.
	DefaultPendingMaxAttempts = 3
)

// PendingMessageStore provides atomic claiming and final cleanup for queued messages.
// Claim must increment Attempts and use persistent storage so two instances cannot
// send the same message. A false found result is an empty queue, not an error.
type PendingMessageStore interface {
	ClaimNextPendingMessage(context.Context, string, string, time.Time, time.Duration) (apimodel.PendingMessage, bool, error)
	DeletePendingMessage(context.Context, string) error
	FailPendingMessage(context.Context, string, string) error
}

// PendingMessageDrainHooks connects queue delivery to the downstream chat service
// and its error classification. Permanent errors use the existing precondition and
// invalid-argument sentinels; transient failures wait for the claim lease to expire.
type PendingMessageDrainHooks struct {
	Store       PendingMessageStore
	Append      func(context.Context, string, []json.RawMessage) error
	FailReason  func(error) string
	IsPermanent func(error) bool
	Logger      *slog.Logger
	Lease       time.Duration
	MaxAttempts int
	Now         func() time.Time
}

// DrainPendingMessage sends at most one queue head. It is called only after a real
// turn end, never for a user interrupt. Failed sends remain owned by the persistent
// claim and are retried after its lease expires; deletion failures are logged because
// the next turn may otherwise deliver a duplicate.
func DrainPendingMessage(ctx context.Context, runID, owner string, hooks PendingMessageDrainHooks) bool {
	if hooks.Store == nil || hooks.Append == nil {
		return false
	}
	logger := hooks.Logger
	if logger == nil {
		logger = slog.Default()
	}
	lease := hooks.Lease
	if lease <= 0 {
		lease = DefaultPendingClaimLease
	}
	maxAttempts := hooks.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultPendingMaxAttempts
	}
	now := hooks.Now
	if now == nil {
		now = time.Now
	}
	message, found, err := hooks.Store.ClaimNextPendingMessage(ctx, runID, owner, now(), lease)
	if err != nil {
		logger.Warn("认领排队消息失败", "run", runID, "error", err.Error())
		return false
	}
	if !found {
		return false
	}
	fail := func(reason string) {
		if err := hooks.Store.FailPendingMessage(ctx, message.ID, reason); err != nil {
			logger.Warn("标记排队消息失败时出错", "message", message.ID, "error", err.Error())
		}
	}
	if message.Attempts > maxAttempts {
		logger.Warn("排队消息反复发不出去，放弃", "run", runID, "message", message.ID, "attempts", message.Attempts)
		fail(fmt.Sprintf("自动发送 %d 次都没成功，这条消息没有发出。", maxAttempts))
		return false
	}
	event, err := json.Marshal(map[string]any{
		"type": "user.message", "content": []map[string]any{{"type": "text", "text": message.Text}},
	})
	if err != nil {
		fail("拼消息事件失败")
		return false
	}
	if err := hooks.Append(ctx, runID, []json.RawMessage{event}); err != nil {
		if hooks.IsPermanent != nil && hooks.IsPermanent(err) {
			logger.Warn("排队消息发不出去且重试无益，放弃", "run", runID, "message", message.ID, "error", err.Error())
			if hooks.FailReason != nil {
				fail(hooks.FailReason(err))
			} else {
				fail(err.Error())
			}
			return false
		}
		logger.Warn("排队消息这次没发成，等租约过期后重试", "run", runID, "message", message.ID, "error", err.Error())
		return false
	}
	if err := hooks.Store.DeletePendingMessage(ctx, message.ID); err != nil {
		logger.Warn("排队消息已发出但删除失败，下一轮可能重复发送", "run", runID, "message", message.ID, "error", err.Error())
	}
	logger.Info("排队消息已发出", "run", runID, "message", message.ID)
	return true
}

// DrainAllowedAfterTurn excludes user interrupts from automatic queue delivery.
func DrainAllowedAfterTurn(raw []byte) bool { return drainAllowed(raw) }
