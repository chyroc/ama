// Agent-level Ark credential: the most specific of the three config levels.
//
// # The three levels and their fallback order
//
//	agent level       agent_ark_secrets (this file)    ← highest priority
//	workspace level   ark_secrets
//	account level     global_ark_secrets               ← last fallback
//
// **More specific wins**; the only entry point for resolving is ResolveSecretForAgent in credential.go.
//
// # "No such row" means "follow the workspace", with nothing for the user to click
//
// This deliberately differs from the workspace level: a workspace referencing the account-level one requires **one explicit click**
// (UseGlobalArkCredential), because that is "switch to someone else's key" and should not be left to guessing.
// Agents following their workspace by default is how it has always worked; requiring a click to "follow the workspace" would make
// every existing agent suddenly have no credential after the upgrade.
//
// So the agent level **has no inherit storage state**: row = custom, no row = follow the workspace.
//
// # Lower levels keep no copy of upper levels
//
// When following, the agent side has no row at all and every read fetches fresh, so changing the workspace-level or account-level key
// means agents following it get the new value on their next resolve. ⚠️ Never change it to "copy into the agent row when following":
// then changing the upper level would not link through, and linking is the very reason three config levels exist. This is the same
// invariant as the one for SourceInherit, just with one more level.

package arkcred

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// GetAgentArkCredential gets an agent's credential view, **including "which level is actually in effect now"**.
//
// When not configured it returns an empty shell instead of 404, same as GetArkCredential: the frontend uses whether apiKeyMasked is empty to tell
// "does this agent have its own key", and whether the whole object is null to tell "still loading".
// Returning 404 would leave it stuck on the skeleton screen forever.
func (s *Service) GetAgentArkCredential(ctx context.Context, agentID string) (apimodel.AgentArkCredential, error) {
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			return apimodel.AgentArkCredential{}, fmt.Errorf("%w: agent 不存在", persistmodel.ErrNotFound)
		}
		return apimodel.AgentArkCredential{}, err
	}

	out := apimodel.AgentArkCredential{
		AgentID: agentID,
		Region:  apimodel.RegionCNBeijing,
		Check:   apimodel.CheckUnchecked,
		Custom:  false,
	}
	if sec, err := s.store.GetAgentArkSecret(ctx, agentID); err == nil {
		out = sec.Credential()
	} else if !errors.Is(err, persistmodel.ErrNotFound) {
		return apimodel.AgentArkCredential{}, err
	}

	// Whether or not it has its own key, compute "which level is in effect now".
	// Agents that "follow the workspace" need it even more (otherwise the UI could only say "following workspace settings",
	// while the user wants to know which key that actually is and whether it works).
	out.Effective = s.Effective(ctx, agent.WorkspaceID, agentID)
	return out, nil
}

// Effective computes "which key of which level this agent actually uses now", for display only.
//
// **It is exported because the core side still uses it**: core/ark.go keeps an unexported forwarder of the same name
// for core's same-package tests (agentark_test.go asserts the three-level decision directly).
//
// The most confusing part of three config levels is "I configured three places, which one is actually used". This function
// exists to state that answer directly, instead of making the user reason by jumping between three pages.
//
// **Any failure degrades to "none of the three levels configured"** (empty Level + empty mask), without returning an error: it adds
// information to the UI and should not turn the whole credential page into an error state over one database hiccup.
func (s *Service) Effective(ctx context.Context, workspaceID, agentID string) apimodel.ArkEffective {
	sec, err := s.ResolveSecretForAgent(ctx, workspaceID, agentID)
	if err != nil || strings.TrimSpace(sec.APIKey) == "" {
		return apimodel.ArkEffective{}
	}

	out := apimodel.ArkEffective{
		ApiKeyMasked: sec.ApiKeyMasked,
		Region:       sec.Region,
		Check:        sec.Check,
	}
	// Decide the level: ask from the most specific first, same as the resolve order.
	//
	// **Do not infer it from sec** (for example "Source is override, so it is the agent level"):
	// in the view built by ResolveSecretForAgent, Source means "relative to its own level",
	// and a workspace-level override looks the same as an agent-level override in that field.
	if agentSec, err := s.store.GetAgentArkSecret(ctx, agentID); err == nil &&
		strings.TrimSpace(agentSec.APIKey) != "" {
		out.Level = apimodel.LevelAgent
		return out
	}
	if row, err := s.store.GetArkSecret(ctx, workspaceID); err == nil {
		if apimodel.NormalizeArkSource(row.Source) == apimodel.SourceInherit {
			out.Level = apimodel.LevelGlobal
			// Say whose account-level credential it is: a workspace may have several admins, each with their own,
			// and without naming whose, nobody knows whose replacement would affect this.
			out.OwnerEmail = row.InheritFrom
			return out
		}
		out.Level = apimodel.LevelWorkspace
		return out
	}
	// Not even a workspace row, yet a key was resolved: should not happen (resolve reads from exactly these places).
	// Give an empty Level instead of guessing; the UI then shows only the mask, not the source.
	return out
}

// SaveAgentArkCredential attaches an agent's own key.
//
// After changing the key, the old check result is void at once (check reset to unchecked): the record is replaced as a whole,
// so checkedAt / checkError are gone naturally. Same as SaveArkCredential.
//
// The key has two sources (typed by hand / fetched from a Volcano Engine account), unified by resolveDraftKey, shared with the other two levels.
func (s *Service) SaveAgentArkCredential(
	ctx context.Context, agentID string, draft apimodel.ArkCredentialDraft,
) (apimodel.AgentArkCredential, error) {
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			return apimodel.AgentArkCredential{}, fmt.Errorf("%w: agent 不存在", persistmodel.ErrNotFound)
		}
		return apimodel.AgentArkCredential{}, err
	}

	// First turn the "fetch from Volcano Engine account" path into plaintext, then run exactly the same checks as the other two levels.
	plain, err := s.resolveDraftKey(ctx, draft)
	if err != nil {
		return apimodel.AgentArkCredential{}, err
	}
	draft.APIKey = plain
	if err := ValidateAPIKey(draft.APIKey); err != nil {
		return apimodel.AgentArkCredential{}, err
	}
	if !validRegions[draft.Region] {
		return apimodel.AgentArkCredential{}, fmt.Errorf("%w: 区域取值不合法", persistmodel.ErrInvalidArgument)
	}

	apiKey := strings.TrimSpace(draft.APIKey)

	// Same as the other two levels: the three-outcome online check before storing (see saveverify.go).
	reject, warning := s.verifyBeforeSave(ctx, apiKey, draft.Region, draft.ConfirmNotActivated)
	if reject != nil {
		return apimodel.AgentArkCredential{}, reject
	}

	sec := apimodel.AgentArkSecret{
		AgentID:      agentID,
		WorkspaceID:  agent.WorkspaceID,
		APIKey:       apiKey,
		ApiKeyMasked: maskSecret(apiKey),
		Region:       draft.Region,
		Check:        apimodel.CheckUnchecked,
		UpdatedAt:    apimodel.NowISO(),
		UpdatedBy:    s.actorEmail(ctx),
	}
	if err := s.store.PutAgentArkSecret(ctx, sec); err != nil {
		return apimodel.AgentArkCredential{}, err
	}

	out := sec.Credential()
	out.Effective = s.Effective(ctx, agent.WorkspaceID, agentID)
	out.Warning = warning
	return out, nil
}

// UpdateAgentArkRegion changes only the region of this agent's key, not the key.
//
// The same key is not necessarily activated in every region; changing region changes what was verified, so
// check is reset, and checkedAt and checkError are **removed** (set empty so omitempty applies).
// But **updatedBy is not touched**, same as UpdateArkRegion.
//
// ⚠️ An agent without its own key must be **rejected**: the region belongs to that key, and it uses the workspace/account-level one.
// Allowing it would write a row "with region but no key", and the next resolve would still fall back to the upper level:
// the user changed the region with no effect and no error. Same handling as UpdateArkRegion under inherit.
func (s *Service) UpdateAgentArkRegion(
	ctx context.Context, agentID string, region apimodel.ArkRegion,
) (apimodel.AgentArkCredential, error) {
	if !validRegions[region] {
		return apimodel.AgentArkCredential{}, fmt.Errorf("%w: 区域取值不合法", persistmodel.ErrInvalidArgument)
	}
	sec, err := s.store.GetAgentArkSecret(ctx, agentID)
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			return apimodel.AgentArkCredential{}, fmt.Errorf(
				"%w: 这个 Agent 跟随空间的方舟凭证，区域要到空间设置里改，"+
					"或先给它配一把自己的 key", persistmodel.ErrPrecondition)
		}
		return apimodel.AgentArkCredential{}, err
	}

	sec.Region = region
	sec.Check = apimodel.CheckUnchecked
	sec.CheckError = ""
	sec.CheckedAt = ""
	sec.UpdatedAt = apimodel.NowISO()
	// UpdatedBy deliberately left alone

	if err := s.store.PutAgentArkSecret(ctx, sec); err != nil {
		return apimodel.AgentArkCredential{}, err
	}
	out := sec.Credential()
	out.Effective = s.Effective(ctx, sec.WorkspaceID, agentID)
	return out, nil
}

// VerifyAgentArkCredential calls an Ark read-only endpoint for real with the key **actually in effect** for this agent.
//
// # Verify the key in effect, write back to where it belongs
//
// ⚠️ There is a pitfall here shaped exactly like the workspace level: an agent following the workspace holds a sec with **the upper level's plaintext**,
// and a casual PutAgentArkSecret would copy it into the agent row. Then the agent has its own copy from then on,
// **changing the upper level never links through again**, while the UI still shows "following workspace settings".
// This is the most hidden way to break the three-level mechanism: no error, looks right at the time, and days later you find the change had no effect.
//
// So write back to the agent row only when the agent really has its own key; otherwise **delegate to the upper level's check**.
//
// An invalid key is not an HTTP error (200 + check:"invalid"), same as VerifyArkCredential.
func (s *Service) VerifyAgentArkCredential(ctx context.Context, agentID string) (apimodel.AgentArkCredential, error) {
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			return apimodel.AgentArkCredential{}, fmt.Errorf("%w: agent 不存在", persistmodel.ErrNotFound)
		}
		return apimodel.AgentArkCredential{}, err
	}

	own, ownErr := s.store.GetAgentArkSecret(ctx, agentID)
	if ownErr != nil && !errors.Is(ownErr, persistmodel.ErrNotFound) {
		return apimodel.AgentArkCredential{}, ownErr
	}
	if errors.Is(ownErr, persistmodel.ErrNotFound) || strings.TrimSpace(own.APIKey) == "" {
		// Following the upper level: **write the check result back to the upper level's row**; store nothing here.
		if _, err := s.VerifyArkCredential(ctx, agent.WorkspaceID); err != nil {
			return apimodel.AgentArkCredential{}, err
		}
		return s.GetAgentArkCredential(ctx, agentID)
	}

	if s.verifier == nil {
		return apimodel.AgentArkCredential{}, fmt.Errorf("%w: 服务端未配置方舟校验器", persistmodel.ErrPrecondition)
	}
	verr := s.verifier.Verify(ctx, own.APIKey, own.Region)
	switch verr.Status {
	case VerifyStatusOK:
		own.Check = apimodel.CheckValid
		own.CheckError = ""
	case VerifyStatusInvalidKey:
		own.Check = apimodel.CheckInvalid
		own.CheckError = "方舟返回 401/403：API Key 无效或已被删除"
	case VerifyStatusNotActivated:
		own.Check = apimodel.CheckInvalid
		own.CheckError = "API Key 有效，但它所属的方舟账号还没开通 Managed Agents 服务，请到方舟控制台开通"
	default:
		// Not verified; do not change the conclusion. Pass the reason out and let the caller decide how to present it.
		if verr.Cause != nil {
			return apimodel.AgentArkCredential{}, fmt.Errorf("校验方舟凭证失败: %w", verr.Cause)
		}
		return apimodel.AgentArkCredential{}, fmt.Errorf("校验方舟凭证失败：方舟暂时不可用")
	}
	own.CheckedAt = apimodel.NowISO()

	if err := s.store.PutAgentArkSecret(ctx, own); err != nil {
		return apimodel.AgentArkCredential{}, err
	}
	out := own.Credential()
	out.Effective = s.Effective(ctx, agent.WorkspaceID, agentID)
	return out, nil
}

// DeleteAgentArkCredential switches this agent back to "follow workspace settings". Deleting something missing is not an error (idempotent).
//
// This is the "use workspace settings" action in the UI: delete the agent's row and resolve falls back naturally.
// **No "inherit marker" needs to be written**: having no row means exactly that; see the top of this file.
func (s *Service) DeleteAgentArkCredential(ctx context.Context, agentID string) error {
	return s.store.DeleteAgentArkSecret(ctx, agentID)
}
