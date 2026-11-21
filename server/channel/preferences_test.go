package channel

import (
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

func TestChannelPreferencesKeepOppositeDefaults(t *testing.T) {
	if FollowUpQueues("") {
		t.Fatal("legacy empty follow-up must steer, not queue")
	}
	if !FollowUpQueues(apimodel.FollowUpQueue) {
		t.Fatal("explicit queue preference was lost")
	}
	if !ShowsProgress("") {
		t.Fatal("legacy empty progress preference must show details")
	}
	if ShowsProgress(apimodel.ProgressDetailBrief) {
		t.Fatal("explicit brief preference was lost")
	}
}
