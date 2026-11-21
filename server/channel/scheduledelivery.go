package channel

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
)

// ScheduleDeliveryHooks resolves a delivery target's current credentials and sends a card.
// No secret or token is persisted on the schedule; each delivery resolves it afresh.
type ScheduleDeliveryHooks struct {
	ResolveToken func(context.Context, apimodel.DeliveryTarget) (string, error)
	Reply        func(context.Context, string, string, map[string]any, bool, string) error
	Send         func(context.Context, string, string, map[string]any, string) error
	Logger       *slog.Logger
}

// ScheduleCardText always includes the task title so a report is distinguishable from a reply.
func ScheduleCardText(title, text string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return text
	}
	return "**⏰ " + title + "**\n\n" + text
}

// DeliverScheduled sends one run's result with a stable idempotency key. An anchor reply
// falling through to direct chat delivery is deliberate: a deleted anchor should not hide the report.
func DeliverScheduled(
	ctx context.Context, target apimodel.DeliveryTarget, title, text, runID string,
	card func(string) map[string]any, hooks ScheduleDeliveryHooks,
) error {
	if target.Kind != apimodel.DeliveryFeishu {
		return fmt.Errorf("暂不支持的投递方式: %s", target.Kind)
	}
	if hooks.ResolveToken == nil || hooks.Send == nil || card == nil {
		return fmt.Errorf("schedule delivery is not fully wired")
	}
	token, err := hooks.ResolveToken(ctx, target)
	if err != nil {
		return err
	}
	idempotencyKey := "ama-schedule:" + runID
	body := card(ScheduleCardText(title, text))
	if anchor := strings.TrimSpace(target.AnchorMessageID); anchor != "" && hooks.Reply != nil {
		if err := hooks.Reply(ctx, token, anchor, body, target.ThreadID != "", idempotencyKey); err == nil {
			return nil
		} else {
			logger := hooks.Logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Warn("回复锚点消息失败，退回直发到会话", slog.String("chat", target.ChatID), slog.Any("err", err))
		}
	}
	return hooks.Send(ctx, token, target.ChatID, body, idempotencyKey)
}
