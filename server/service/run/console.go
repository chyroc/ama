// Ark console links for the run history domain (one read-only sidecar endpoint).
//
// This file was **moved from** GetRunArkConsole in core/arkconsole.go (when arkconsole was split by
// domain), with not a line of logic changed. The two arguments "why a sidecar endpoint instead of a
// field on Run" and "why only id and region are given and the frontend builds the URL" are in the
// file header of core/arkconsole.go (that passage is shared by three entities and stays there with
// the shared logic).

package run

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// GetRunArkConsole gives the location of a run (session) in the Ark console.
//
// This one is the simplest: **run.ID is the Ark session id**, no table lookup needed. The run is
// still read once, because workspaceID / agentID are needed to resolve the region, and a missing run
// should return 404 instead of a fake ref that can still build a URL.
func (s *Service) GetRunArkConsole(ctx context.Context, runID string) (apimodel.ArkConsoleRef, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return apimodel.ArkConsoleRef{}, fmt.Errorf("%w: 运行 ID 不能为空", persistmodel.ErrInvalidArgument)
	}
	// Use store instead of Service.GetRun: the latter also fetches missing events from Ark
	// (backfill), and this endpoint only asks "where is it in the console"; it should not add a
	// network round trip.
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			return apimodel.ArkConsoleRef{}, fmt.Errorf("%w: 运行记录不存在", persistmodel.ErrNotFound)
		}
		return apimodel.ArkConsoleRef{}, err
	}
	return apimodel.ArkConsoleRef{
		ArkSessionID: run.ID,
		Region:       s.consoleRegion(ctx, run.WorkspaceID, run.AgentID),
	}, nil
}

// consoleRegion resolves the region of the key actually in effect for (workspace, agent); returns an
// empty string when none is injected or it cannot be resolved, and the frontend then does not render
// the entry.
func (s *Service) consoleRegion(ctx context.Context, workspaceID, agentID string) apimodel.ArkRegion {
	if s.consoleRegionFn == nil {
		return ""
	}
	return s.consoleRegionFn(ctx, workspaceID, agentID)
}
