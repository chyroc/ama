// Agent: 10 forwarding methods. **The business logic has moved to service/agent.**
//
// # These empty shells are a transition state, not a design
//
// The logic lives in service/agent; this file only forwards calls unchanged. The only reason to
// keep them is that **the methods handlers cannot reach must still be backed by core**: the
// orchestration layer's fallback interface (orchestrator.Fallback) requires all of these methods
// on *core.Service:
//
//	cmd/amad/orchestrator.go var _ orchestrator.Fallback = (*core.Service)(nil)
//
// Remove a method from *Service and that assertion fails to compile.
//
// So this step deliberately moves only the logic and leaves the method set alone: **the value of
// this step is "agent business logic no longer lives in core"**; the check is that
// `go list -deps ./service/agent` does not include core.
//
// ⚠️ **Do not add logic here.** Any check written here bypasses service/agent: the two entry
// points would give different answers for the same thing, and nothing would raise an error. Add
// things in service/agent.
//
// # ⚠️ This file used to be two domains glued together; the skill half has moved to core/skill.go
//
// Before the package split, core/agent.go had 1722 lines, but **only the first 1092 were the
// agent domain**: from line 1094 on (after a "skills" separator comment) it was all
// the skill domain, including ListSkills / CreateSkill / SkillFiles and so on. That half now lives
// in **core/skill.go** (a pure git mv, not one character changed), waiting for its own split.
//
// This is written here because it tripped someone up: in an earlier round someone estimated the
// scope by "how many lines agent.go has" and counted those six hundred skill lines as the agent
// domain. **Domain ≠ file**; now each domain has its own file name to say where it lives.
//
// # ⚠️ The enqueue side of agentsync has **not** moved; it is still in core/agentsync.go
//
// The `EnqueueAgentSync` family shares a pair of in-process state fields with the resident
// executor `AgentSyncer`, and the executor depends on the ark / chat domains, which are not split
// yet, so it cannot move. service/agent calls back into it through the single-method interface
// `agentsvc.Syncs` (wiring in core/service.go). The full reasoning is in the file header of
// service/agent/service.go.
package core

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
)

// ListAgents lists the agents in a workspace. The real one is in service/agent.
func (s *Service) ListAgents(ctx context.Context, workspaceID string) ([]apimodel.Agent, error) {
	return s.agents.ListAgents(ctx, workspaceID)
}

// GetAgent gets an agent, returning ErrNotFound if it does not exist. The real one is in service/agent.
func (s *Service) GetAgent(ctx context.Context, id string) (apimodel.Agent, error) {
	return s.agents.GetAgent(ctx, id)
}

// CreateAgent creates an agent. The real one is in service/agent.
func (s *Service) CreateAgent(
	ctx context.Context, workspaceID string, draft apimodel.AgentDraft,
) (apimodel.Agent, error) {
	return s.agents.CreateAgent(ctx, workspaceID, draft)
}

// UpdateAgent updates an agent. The real one is in service/agent.
func (s *Service) UpdateAgent(
	ctx context.Context, id string, draft apimodel.AgentDraft,
) (apimodel.Agent, error) {
	return s.agents.UpdateAgent(ctx, id, draft)
}

// updateAgent is the implementation of UpdateAgent, with one extra argument: "what reason to
// record for this change in the history".
//
// ⚠️ **This unexported forwarder must not be deleted**: the agent entry's Rollback in
// core/revision.go calls it (a rollback must record the reason as rollback, not edit).
func (s *Service) updateAgent(
	ctx context.Context, id string, draft apimodel.AgentDraft, reason apimodel.RevisionReason,
) (apimodel.Agent, error) {
	return s.agents.UpdateAgentWithReason(ctx, id, draft, reason)
}

// DeleteAgent deletes an agent and clears all of its history versions. The real one is in service/agent.
func (s *Service) DeleteAgent(ctx context.Context, id string) error {
	return s.agents.DeleteAgent(ctx, id)
}

// SetAgentIntegrations changes "which authorizations this agent may inject". The real one is in
// service/agent.
func (s *Service) SetAgentIntegrations(
	ctx context.Context, id string, ids []string,
) (apimodel.Agent, error) {
	return s.agents.SetAgentIntegrations(ctx, id, ids)
}
