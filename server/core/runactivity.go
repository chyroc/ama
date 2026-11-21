package core

import (
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// AgeSinceRunActivity returns how long ago the run last made real progress.
// Unparseable legacy timestamps return zero so a rescue does not fire on weak evidence.
func AgeSinceRunActivity(run apimodel.Run, now time.Time) time.Duration {
	at, err := time.Parse("2006-01-02T15:04:05.000Z", persistmodel.LastActivityOf(run))
	if err != nil {
		return 0
	}
	return now.Sub(at)
}

// RetireRunEndedAt uses the run's last real activity as its end time. If a legacy
// timestamp cannot be parsed, it falls back to the supplied current time so endedAt
// is not lost and the frontend does not show a terminated run as unfinished.
func RetireRunEndedAt(run apimodel.Run, now time.Time) time.Time {
	if at, err := time.Parse("2006-01-02T15:04:05.000Z", persistmodel.LastActivityOf(run)); err == nil {
		return at
	}
	return now.UTC()
}
