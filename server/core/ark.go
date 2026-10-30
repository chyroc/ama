// Ark credentials: 19 forwarding methods. **The business logic has moved to service/arkcred.**
//
// # These empty shells are a transition state, not a design
//
// The logic lives in service/arkcred; this file only forwards calls unchanged. The only reason to
// keep them is that **the methods handlers cannot reach must still be backed by core**: the
// orchestration layer's fallback interface (orchestrator.Fallback) requires all of these methods
// on *core.Service:
//
//	cmd/amad/orchestrator.go var _ orchestrator.Fallback = (*core.Service)(nil)
//
// Remove a method from *Service and that assertion fails to compile.
//
// So this step deliberately moves only the logic and leaves the method set alone: **the value of
// this step is "Ark credential business logic no longer lives in core"**; the check is that
// `go list -deps ./service/arkcred` does not include core.
//
// ⚠️ **Do not add logic here.** Any check written here bypasses service/arkcred: the two entry
// points would give different answers for the same thing, and nothing would raise an error. Add
// things in service/arkcred.
//
// # ⚠️ Where this file's boundary is drawn: it used to mix two domains
//
// Before the split, core/ark.go had 882 lines and **held two domains**:
//
//	:186-582  credentials (storage and resolution of the three credential levels)  ← moved to service/arkcred in step 8
//	:583-882  run history and maintenance (ListRuns / GetRun /                    ← moved to service/run in step 9a
//	          markAgentDeleted / backfillFromArk / Reset / Clear)
//
// Both halves have moved, so this file now **only has the credential-domain forwarders**. For
// where the run-history half went, see core/run.go (read-path forwarders), core/runbackfill.go
// (the backfill implementation; it calls the conversation domain's SyncRunEvents, so it stays in
// core), and core/store_e2e.go (Reset / Clear).
//
// # Three unexported resolve functions became exported, because core still uses them
//
// resolveArkSecret / resolveArkSecretForAgent became ResolveSecret / ResolveSecretForAgent on
// arkcredsvc: arkKey in core/chat.go and arkConsoleRegion in core/arkconsole.go both need the
// three-level resolution, and they **did not move along** (the former is on the conversation path,
// the latter depends on the agent / skill domains, which are not split yet).
// Forwarders for these two are kept further down in this file.

package core

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
)

// -- Three-level resolution (core still has two callers internally; see file header) ----------

// resolveArkSecret resolves the Ark credential actually in effect for a workspace (with
// plaintext). The real one is in service/arkcred.
func (s *Service) resolveArkSecret(ctx context.Context, workspaceID string) (apimodel.ArkSecret, error) {
	return s.arkCreds.ResolveSecret(ctx, workspaceID)
}

// resolveArkSecretForAgent resolves the Ark credential actually in effect for an agent (with
// plaintext). The real one is in service/arkcred. arkKey in core/chat.go and core/arkconsole.go
// go through it.
func (s *Service) resolveArkSecretForAgent(
	ctx context.Context, workspaceID, agentID string,
) (apimodel.ArkSecret, error) {
	return s.arkCreds.ResolveSecretForAgent(ctx, workspaceID, agentID)
}

// arkEffective works out "which key at which level this agent actually uses right now". The real
// one is in service/arkcred.
//
// This forwarder is kept because core's same-package test (agentark_test.go) asserts the result
// of the three-level decision directly, and those assertions are the only place that pins "the
// three-level resolution order". **Be more careful changing them than changing the implementation.**
func (s *Service) arkEffective(
	ctx context.Context, workspaceID, agentID string,
) apimodel.ArkEffective {
	return s.arkCreds.Effective(ctx, workspaceID, agentID)
}

// -- Workspace-level credentials (6) ------------------------------------------------------

func (s *Service) GetArkCredential(ctx context.Context, workspaceID string) (apimodel.ArkCredential, error) {
	return s.arkCreds.GetArkCredential(ctx, workspaceID)
}

func (s *Service) SaveArkCredential(
	ctx context.Context, workspaceID string, draft ArkCredentialDraft,
) (apimodel.ArkCredential, error) {
	return s.arkCreds.SaveArkCredential(ctx, workspaceID, draft)
}

func (s *Service) UpdateArkRegion(
	ctx context.Context, workspaceID string, region apimodel.ArkRegion,
) (apimodel.ArkCredential, error) {
	return s.arkCreds.UpdateArkRegion(ctx, workspaceID, region)
}

func (s *Service) VerifyArkCredential(ctx context.Context, workspaceID string) (apimodel.ArkCredential, error) {
	return s.arkCreds.VerifyArkCredential(ctx, workspaceID)
}

func (s *Service) UseGlobalArkCredential(ctx context.Context, workspaceID string) (apimodel.ArkCredential, error) {
	return s.arkCreds.UseGlobalArkCredential(ctx, workspaceID)
}

func (s *Service) DeleteArkCredential(ctx context.Context, workspaceID string) error {
	return s.arkCreds.DeleteArkCredential(ctx, workspaceID)
}

// -- Account-level credentials (4) --------------------------------------------------------

func (s *Service) GetGlobalArkCredential(ctx context.Context) (apimodel.GlobalArkCredential, error) {
	return s.arkCreds.GetGlobalArkCredential(ctx)
}

func (s *Service) SaveGlobalArkCredential(
	ctx context.Context, draft ArkCredentialDraft,
) (apimodel.GlobalArkCredential, error) {
	return s.arkCreds.SaveGlobalArkCredential(ctx, draft)
}

func (s *Service) DeleteGlobalArkCredential(ctx context.Context) error {
	return s.arkCreds.DeleteGlobalArkCredential(ctx)
}

func (s *Service) VerifyGlobalArkCredential(ctx context.Context) (apimodel.GlobalArkCredential, error) {
	return s.arkCreds.VerifyGlobalArkCredential(ctx)
}

// -- Agent-level credentials (5; the real ones are in service/arkcred/agent.go) -----------

func (s *Service) GetAgentArkCredential(ctx context.Context, agentID string) (apimodel.AgentArkCredential, error) {
	return s.arkCreds.GetAgentArkCredential(ctx, agentID)
}

func (s *Service) SaveAgentArkCredential(
	ctx context.Context, agentID string, draft ArkCredentialDraft,
) (apimodel.AgentArkCredential, error) {
	return s.arkCreds.SaveAgentArkCredential(ctx, agentID, draft)
}

func (s *Service) UpdateAgentArkRegion(
	ctx context.Context, agentID string, region apimodel.ArkRegion,
) (apimodel.AgentArkCredential, error) {
	return s.arkCreds.UpdateAgentArkRegion(ctx, agentID, region)
}

func (s *Service) VerifyAgentArkCredential(ctx context.Context, agentID string) (apimodel.AgentArkCredential, error) {
	return s.arkCreds.VerifyAgentArkCredential(ctx, agentID)
}

func (s *Service) DeleteAgentArkCredential(ctx context.Context, agentID string) error {
	return s.arkCreds.DeleteAgentArkCredential(ctx, agentID)
}

// -- Fetch keys automatically from the Volcengine account (3; the real ones are in service/arkcred/fetch.go) --

func (s *Service) GetArkFetchStatus(ctx context.Context) (ArkFetchStatus, error) {
	return s.arkCreds.GetArkFetchStatus(ctx)
}

func (s *Service) ListVolcAccounts(ctx context.Context) ([]apimodel.VolcAccount, error) {
	return s.arkCreds.ListVolcAccounts(ctx)
}

func (s *Service) ListVolcArkKeys(ctx context.Context, accountID string) ([]apimodel.ArkKeyOption, error) {
	return s.arkCreds.ListVolcArkKeys(ctx, accountID)
}
