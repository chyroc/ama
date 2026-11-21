package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestCleanupOldToolResultsUsesRetention(t *testing.T) {
	now := time.Unix(1700000000, 0)
	var cutoff time.Time
	calls := 0
	CleanupOldToolResults(context.Background(), func(_ context.Context, at time.Time) (int64, error) {
		calls++
		cutoff = at
		return 1, nil
	}, time.Hour, now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if calls != 1 || !cutoff.Equal(now.Add(-time.Hour)) {
		t.Fatalf("unexpected cleanup: calls=%d cutoff=%v", calls, cutoff)
	}
}

func TestCleanupOldToolResultsDoesNotBlockOnFailure(t *testing.T) {
	CleanupOldToolResults(context.Background(), func(context.Context, time.Time) (int64, error) {
		return 0, errors.New("database unavailable")
	}, time.Hour, time.Now(), slog.New(slog.NewTextHandler(io.Discard, nil)))
}
