// Package runtrace locates a run in an optional observability backend.
// It reads only the local run record and an injected locator; no backend query is proxied.
package runtrace

import (
	"context"
	"fmt"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// Ref identifies a trace in the observability backend.
// Enabled is false when no backend is configured; TraceID is then empty.
type Ref struct {
	TraceID     string `json:"traceID"`
	Enabled     bool   `json:"enabled"`
	WorkspaceID string `json:"workspaceId,omitempty"`
}

// Locator resolves a run to its observability reference.
type Locator interface {
	TraceRef(runID string) Ref
}

// Store checks that the run exists before exposing its trace location.
type Store interface {
	GetRun(ctx context.Context, id string) (apimodel.Run, error)
}

// Service provides read-only run trace locations.
type Service struct {
	store   Store
	locator Locator
}

// New builds a trace locator. A nil locator means the backend is not configured.
func New(store Store, locator Locator) *Service {
	return &Service{store: store, locator: locator}
}

// Get returns a run's observability reference, or Enabled=false when unconfigured.
func (s *Service) Get(ctx context.Context, runID string) (Ref, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return Ref{}, fmt.Errorf("%w: run id is required", persistmodel.ErrInvalidArgument)
	}
	if _, err := s.store.GetRun(ctx, runID); err != nil {
		return Ref{}, fmt.Errorf("%w: run not found", persistmodel.ErrNotFound)
	}
	if s.locator == nil {
		return Ref{Enabled: false}, nil
	}
	return s.locator.TraceRef(runID), nil
}
