package core

import (
	"context"
	"log/slog"
	"time"
)

const (
	// DefaultWindowRetryBudget covers the short tool-result rejection window.
	DefaultWindowRetryBudget = time.Second
	// DefaultWindowRetryStep matches the short rejection window without long request waits.
	DefaultWindowRetryStep = 40 * time.Millisecond
)

// SendEventsThroughWindow retries only transient tool-window rejections within a
// short budget. Long agent turns belong to the persistent message queue, not a
// synchronous HTTP wait. The last error is returned unchanged on timeout or cancellation.
func SendEventsThroughWindow[T any](
	ctx context.Context, send func() (T, error), isTransient func(error) bool,
	budget, step time.Duration, sessionID string, logger *slog.Logger,
) (T, error) {
	if budget <= 0 {
		budget = DefaultWindowRetryBudget
	}
	if step <= 0 {
		step = DefaultWindowRetryStep
	}
	if logger == nil {
		logger = slog.Default()
	}
	deadline := time.Now().Add(budget)
	for attempt := 0; ; attempt++ {
		accepted, err := send()
		if err == nil {
			if attempt > 0 {
				logger.Info("撞上工具执行窗口，重试后发送成功", "session", sessionID, "attempts", attempt+1)
			}
			return accepted, nil
		}
		if isTransient == nil || !isTransient(err) || ctx.Err() != nil {
			var zero T
			return zero, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			logger.Info("工具执行窗口在重试预算内没有关闭，交给调用方排队", "session", sessionID, "attempts", attempt+1)
			var zero T
			return zero, err
		}
		wait := step
		if remaining < wait {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			var zero T
			return zero, err
		case <-time.After(wait):
		}
	}
}
