package channel

import "github.com/chyroc/ama/server/model/apimodel"

// DeliveryOrigin is the channel-neutral location of a conversation. Credentials and app
// secrets are resolved later by the delivery implementation, not copied into this value.
type DeliveryOrigin struct {
	FeishuAppID     string
	ChatID          string
	AnchorMessageID string
	InThread        bool
}

// DeliveryTargetOf preserves the original anchor and marks a thread with a non-empty ThreadID.
// The actual thread identifier may be unavailable in the stored origin; the delivery side only
// needs the boolean reply_in_thread decision, so ChatID is the marker in that case.
func DeliveryTargetOf(origin DeliveryOrigin) apimodel.DeliveryTarget {
	target := apimodel.DeliveryTarget{
		Kind: apimodel.DeliveryFeishu, FeishuAppID: origin.FeishuAppID,
		ChatID: origin.ChatID, AnchorMessageID: origin.AnchorMessageID,
	}
	if origin.InThread {
		target.ThreadID = origin.ChatID
	}
	return target
}
