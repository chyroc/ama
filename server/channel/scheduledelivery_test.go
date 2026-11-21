package channel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

func TestDeliverScheduledFallsBackToDirectChat(t *testing.T) {
	sent := false
	err := DeliverScheduled(context.Background(), apimodel.DeliveryTarget{
		Kind: apimodel.DeliveryFeishu, ChatID: "chat_example", AnchorMessageID: "deleted_anchor",
	}, "Daily", "body", "run_example", func(text string) map[string]any { return map[string]any{"text": text} }, ScheduleDeliveryHooks{
		ResolveToken: func(context.Context, apimodel.DeliveryTarget) (string, error) { return "token", nil },
		Reply: func(context.Context, string, string, map[string]any, bool, string) error {
			return errors.New("anchor deleted")
		},
		Send: func(_ context.Context, _, chatID string, body map[string]any, key string) error {
			sent = true
			if chatID != "chat_example" || key != "ama-schedule:run_example" || body["text"] != ScheduleCardText("Daily", "body") {
				t.Fatalf("wrong direct delivery: chat=%q body=%v key=%q", chatID, body, key)
			}
			return nil
		},
	})
	if err != nil || !sent {
		t.Fatalf("direct fallback failed: sent=%v err=%v", sent, err)
	}
}

func TestScheduleCardTextKeepsUntitledBody(t *testing.T) {
	if got := ScheduleCardText("", "body"); got != "body" {
		t.Fatalf("untitled body = %q", got)
	}
	if got := ScheduleCardText("Daily", "body"); !strings.Contains(got, "Daily") || !strings.Contains(got, "body") {
		t.Fatalf("titled body = %q", got)
	}
}
