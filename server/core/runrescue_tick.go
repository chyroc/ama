package core

import (
	"context"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
)

const DefaultRescueCheckEvery = 30 * time.Second

// RescueRunTick periodically rereads a claimed run and invokes the stopped-work
// rescue only after the silence threshold. Fresh activity or a non-requires_action
// state suppresses it. The caller's claim, not this timer, prevents duplicate posts.
func RescueRunTick(
	ctx context.Context, runID string,
	getRun func(context.Context, string) (apimodel.Run, error),
	rescue func(context.Context, apimodel.Run, time.Duration),
) {
	if getRun == nil || rescue == nil {
		return
	}
	ticker := time.NewTicker(DefaultRescueCheckEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run, err := getRun(ctx, runID)
			if err != nil {
				return
			}
			if run.Status != apimodel.RunRequiresAction {
				continue
			}
			age := AgeSinceRunActivity(run, time.Now())
			if age < RescueAfter {
				continue
			}
			rescue(ctx, run, age)
		}
	}
}
