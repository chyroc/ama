package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
)

func TestScanRunWatchesClaimsBeforeSubscribing(t *testing.T) {
	claimed, watched := 0, 0
	runs := []apimodel.Run{{ID: "run_a", Status: apimodel.RunRunning}, {ID: "run_b", Status: apimodel.RunRunning}}
	ScanRunWatches(context.Background(), runs, "instance_example",
		slog.New(slog.NewTextHandler(io.Discard, nil)), RunWatchScanHooks{
			Claim: func(_ context.Context, runID string, _ string, _ time.Time, _ time.Duration) (bool, error) {
				claimed++
				return runID == "run_a", nil
			},
			Watch: func(context.Context, string) { watched++ },
		})
	if claimed != 2 || watched != 1 {
		t.Fatalf("only a successful persistent claim may subscribe: claims=%d watches=%d", claimed, watched)
	}
}

func TestScanRunWatchesRescuesActiveStreamWithoutClaim(t *testing.T) {
	claim, rescue := 0, 0
	old := apimodel.FormatISO(time.Now().UTC().Add(-2 * rescueAfter))
	ScanRunWatches(context.Background(), []apimodel.Run{{
		ID: "run_example", Status: apimodel.RunRequiresAction, LastActivityAt: old,
	}}, "instance_example", slog.New(slog.NewTextHandler(io.Discard, nil)), RunWatchScanHooks{
		HasActiveStream: func(string) bool { return true },
		Watch:           func(context.Context, string) {},
		Claim: func(context.Context, string, string, time.Time, time.Duration) (bool, error) {
			claim++
			return false, errors.New("must not claim a browser-owned local stream")
		},
		Rescue: func(context.Context, apimodel.Run, time.Duration) { rescue++ },
	})
	if claim != 0 || rescue != 1 {
		t.Fatalf("active stream must skip claiming but allow rescue: claims=%d rescues=%d age=%v threshold=%v status=%q want=%q", claim, rescue, ageSinceActivity(apimodel.Run{LastActivityAt: old}), rescueAfter, apimodel.RunRequiresAction, apimodel.RunRequiresAction)
	}
}
