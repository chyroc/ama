package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/gopkg/arkframe"
)

// RunScheduleOnceHooks supplies conversation creation, optional trigger metadata,
// event sending, and the event stream for one scheduled round.
type RunScheduleOnceHooks struct {
	Create func(context.Context, apimodel.Schedule) (apimodel.Run, error)
	Tag    func(context.Context, apimodel.Schedule, apimodel.Run) error
	Stream func(context.Context, string, func([]byte) error) error
	Append func(context.Context, string, []json.RawMessage) error
	Logger *slog.Logger
	Settle time.Duration
}

// RunScheduleOnce creates a session, subscribes before sending the prompt, and
// returns the final agent text. The stream is cancelled on a real turn end; an
// empty answer is a failed round. The run ID is returned even on failure for LastRunID.
func RunScheduleOnce(ctx context.Context, sch apimodel.Schedule, hooks RunScheduleOnceHooks) (string, string, error) {
	logger := hooks.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if hooks.Create == nil || hooks.Stream == nil || hooks.Append == nil {
		return "", "", fmt.Errorf("调度执行装配不完整")
	}
	run, err := hooks.Create(ctx, sch)
	if err != nil {
		return "", "", fmt.Errorf("建会话失败: %w", err)
	}
	if hooks.Tag != nil {
		if err := hooks.Tag(ctx, sch, run); err != nil {
			logger.Warn("回写触发来源失败", slog.Any("err", err))
		}
	}
	var mu sync.Mutex
	var builder strings.Builder
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	streamDone := make(chan struct{})
	var streamErr error
	go func() {
		defer close(streamDone)
		streamErr = hooks.Stream(streamCtx, run.ID, func(raw []byte) error {
			if text, ok := arkframe.AgentText(raw); ok && text != "" {
				mu.Lock()
				builder.WriteString(text)
				mu.Unlock()
			}
			if arkframe.TurnEnded(raw) {
				stopStream()
			}
			return nil
		})
	}()
	settle := hooks.Settle
	if settle <= 0 {
		settle = 50 * time.Millisecond
	}
	timer := time.NewTimer(settle)
	<-timer.C
	timer.Stop()
	event, err := ScheduleUserMessage(sch, sch.Prompt)
	if err != nil {
		return run.ID, "", err
	}
	if err := hooks.Append(ctx, run.ID, []json.RawMessage{event}); err != nil {
		return run.ID, "", fmt.Errorf("发送指令失败: %w", err)
	}
	<-streamDone
	if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
		logger.Warn("事件流异常结束", slog.Any("err", streamErr))
	}
	mu.Lock()
	text := strings.TrimSpace(builder.String())
	mu.Unlock()
	if text == "" {
		return run.ID, "", fmt.Errorf("这一轮没有产生任何回复（可能超时或执行环境有问题）")
	}
	return run.ID, text, nil
}
