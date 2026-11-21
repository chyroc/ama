package core

import (
	"context"
	"log/slog"
	"time"
)

// PollWaker coalesces duplicate wakeup hints for one poll loop. It is a local
// acceleration only; WatchWorkerWakeups supplies the cross-instance transport.
type PollWaker = pollWaker

// NewPollWaker returns a receiver that merges pending wakeups into one signal.
func NewPollWaker() *PollWaker { return newPollWaker() }

// Signal records a wakeup without blocking or accumulating duplicate polls.
func (w *PollWaker) Signal() { w.signal() }

// Wait returns after a wakeup or the timeout. The timeout remains the correctness
// fallback when the wakeup transport is unavailable or a hint is lost.
func (w *PollWaker) Wait(ctx context.Context, duration time.Duration) bool {
	return w.wait(ctx, duration)
}

// Drain removes a pending hint after a work item was already claimed.
func (w *PollWaker) Drain() { w.drain() }

// WatchWorkerWakeups subscribes to a downstream cross-instance wakeup transport.
// Subscription failure degrades to timer-only polling; it never stops the poll loop.
func WatchWorkerWakeups(
	ctx context.Context, wakeups WorkerWakeups, environmentID string, logger *slog.Logger,
) *PollWaker {
	if wakeups == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	waker := newPollWaker()
	go func() {
		err := wakeups.WatchWakeups(ctx, environmentID, func(context.Context) error {
			waker.Signal()
			return nil
		})
		if err != nil && ctx.Err() == nil {
			logger.Warn("订阅唤醒信号失败，本环境退化成只靠退避定时器（响应会变慢，但不影响正确性）",
				slog.Any("err", err))
		}
	}()
	return waker
}
