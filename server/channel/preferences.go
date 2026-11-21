package channel

import "github.com/chyroc/ama/server/model/apimodel"

// FollowUpQueues maps the stored preference to its default-safe channel behavior.
// Unknown/legacy empty preference means steer, not queue; a lookup failure must not silently
// reverse the agent's behavior.
func FollowUpQueues(mode apimodel.FollowUpMode) bool {
	return apimodel.NormalizeFollowUpMode(mode) == apimodel.FollowUpQueue
}

// ShowsProgress defaults to detailed progress for absent/legacy preferences. This default
// is deliberately opposite to FollowUpQueues: they have different product defaults.
func ShowsProgress(detail apimodel.ProgressDetail) bool {
	return apimodel.NormalizeProgressDetail(detail) != apimodel.ProgressDetailBrief
}
