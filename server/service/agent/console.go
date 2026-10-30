// Ark console jump for the Agent domain (a read-only side endpoint).
//
// This file was **moved** from GetAgentArkConsole in core/arkconsole.go (the step that split
// arkconsole
// by domain), with not a line of logic changed. The two arguments "why a side endpoint rather than a
// field on Agent" and "why
// only id and region are given, with the URL built by the frontend" are in the file header of
// core/arkconsole.go
// (that part is shared by three entities, so it stays there with the shared logic).

package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// Console is the Ark console locating capability the agent domain needs; **only the jump one is in
// use**.
//
// The real implementation is in core (region resolution carries the three-level fallback meaning,
// and the binding table belongs to the ark domain); at wiring time
// an adapter on the core side plugs it in. **May be nil**: then the region is an empty string and
// bindings are treated as "none",
// GetAgentArkConsole still works, just returning an all-empty ref; the frontend then does not render
// the link,
// the same behavior as "no Ark credentials configured".
type Console interface {
	// Region resolves the region of the key actually in effect for (workspace, agent); returns an
	// empty string when it cannot be resolved.
	Region(ctx context.Context, workspaceID, agentID string) apimodel.ArkRegion
	// AgentBinding looks up the agent's Ark binding; found false means it has not been created yet
	// (= no conversation yet, a normal state, not a fault).
	AgentBinding(ctx context.Context, workspaceID, agentID string) (binding ArkConsoleBinding, found bool, err error)
}

// ArkConsoleBinding is the only field in the binding lookup result this domain cares about.
type ArkConsoleBinding struct {
	// ArkAgentID is the Agent id on the Ark side.
	ArkAgentID string
}

// GetAgentArkConsole gives an agent's location in the Ark console.
//
// Returns ErrNotFound when the agent does not exist; **not yet created on the Ark side is not an
// error**, and an empty ref is returned.
func (s *Service) GetAgentArkConsole(ctx context.Context, agentID string) (apimodel.ArkConsoleRef, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return apimodel.ArkConsoleRef{}, fmt.Errorf("%w: Agent ID 不能为空", persistmodel.ErrInvalidArgument)
	}
	a, err := s.GetAgent(ctx, agentID)
	if err != nil {
		return apimodel.ArkConsoleRef{}, err
	}

	var region apimodel.ArkRegion
	if s.console != nil {
		region = s.console.Region(ctx, a.WorkspaceID, a.ID)
	}
	ref := apimodel.ArkConsoleRef{Region: region}

	if s.console == nil {
		return ref, nil
	}
	b, found, err := s.console.AgentBinding(ctx, a.WorkspaceID, a.ID)
	if err != nil {
		return apimodel.ArkConsoleRef{}, err
	}
	if !found {
		// No binding = no conversation yet. This is a **normal state**, not a fault.
		return ref, nil
	}
	ref.ArkAgentID = b.ArkAgentID
	return ref, nil
}
