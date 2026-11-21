package channel

import (
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

func TestDeliveryTargetOfKeepsThreadMarker(t *testing.T) {
	target := DeliveryTargetOf(DeliveryOrigin{
		FeishuAppID: "app_example", ChatID: "chat_example", AnchorMessageID: "message_example", InThread: true,
	})
	if target.Kind != apimodel.DeliveryFeishu || target.ThreadID == "" || target.AnchorMessageID != "message_example" {
		t.Fatalf("thread delivery was lost: %#v", target)
	}
	if got := DeliveryTargetOf(DeliveryOrigin{ChatID: "chat_example"}); got.ThreadID != "" {
		t.Fatalf("non-thread origin got thread marker: %#v", got)
	}
}
