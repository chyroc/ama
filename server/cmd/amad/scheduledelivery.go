package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/chyroc/ama/server/channel"
	"github.com/chyroc/ama/server/feishu"
	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/shared/infra/lark"
)

type scheduleDeliverer struct {
	store  feishu.Store
	sender larkSender
	tokens larkTokens
}

func (d scheduleDeliverer) Deliver(
	ctx context.Context, target apimodel.DeliveryTarget, title, text, runID string,
) error {
	return channel.DeliverScheduled(ctx, target, title, text, runID, lark.TextCard, channel.ScheduleDeliveryHooks{
		ResolveToken: func(ctx context.Context, target apimodel.DeliveryTarget) (string, error) {
			app, err := d.store.GetApp(ctx, target.FeishuAppID)
			if err != nil {
				return "", fmt.Errorf("找不到投递用的飞书应用（可能已被断开）: %w", err)
			}
			secret, err := d.store.AppSecret(ctx, app.AppID)
			if err != nil {
				return "", fmt.Errorf("取应用密钥失败: %w", err)
			}
			token, err := d.tokens.TenantAccessToken(ctx, app.AppID, secret)
			if err != nil {
				return "", fmt.Errorf("换 token 失败（凭证可能已失效）: %w", err)
			}
			return token, nil
		},
		Reply: func(ctx context.Context, token, anchor string, card map[string]any, inThread bool, idem string) error {
			_, err := d.sender.ReplyCard(ctx, token, anchor, card, lark.ReplyCardOption{ReplyInThread: inThread, IdempotencyKey: idem})
			return err
		},
		Send: func(ctx context.Context, token, chatID string, card map[string]any, idem string) error {
			client, err := lark.New(lark.Config{})
			if err != nil {
				return err
			}
			_, err = client.SendCard(ctx, token, chatID, card, idem)
			return err
		},
		Logger: slog.Default(),
	})
}

func scheduleCardText(title, text string) string { return channel.ScheduleCardText(title, text) }
