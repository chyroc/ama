package core

import (
	"context"
	"log/slog"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// UsageStore supplies the runs and current agents needed for read-only aggregation.
// It deliberately has no save method: derived usage must never be persisted back to runs.
type UsageStore interface {
	ListRunsForUsage(ctx context.Context, workspaceID string, q persistmodel.UsageQuery) ([]apimodel.Run, error)
	ListAgents(ctx context.Context, workspaceID string) ([]apimodel.Agent, error)
}

// UsageService computes usage summaries with one definition for nearest-rank percentiles.
type UsageService struct {
	store UsageStore
	log   *slog.Logger
}

// NewUsageService builds the read-only usage aggregator.
func NewUsageService(store UsageStore, logger *slog.Logger) *UsageService {
	if logger == nil {
		logger = slog.Default()
	}
	return &UsageService{store: store, log: logger}
}
