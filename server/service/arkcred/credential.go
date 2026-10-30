// Business logic for workspace-level and account-level Ark credentials.
//
// The core constraint of credentials is **the plaintext key stays on the server**: the frontend only ever gets a mask
// (apimodel.ArkCredential has no plaintext field at all; the plaintext is in apimodel.ArkSecret).
//
// # How "configure once globally, reference from workspaces" works
//
// Two stores:
//
//	global_ark_secrets  account level, primary key email     ← the global one
//	ark_secrets         workspace level, primary key workspace_id, with an extra source in doc
//
// source has only two values:
//
//	override  this workspace stores its own key (doc has the plaintext)
//	inherit   this workspace stores **nothing**, and reads fall back to the account-level one
//
// **Linking comes naturally, because with inherit the workspace side has no copy at all.** Changing the global one = changing
// that row in global_ark_secrets, and every inherit workspace gets the new value on its next resolve,
// with no sync logic needed and no chance of "forgot to sync".
//
// ⚠️ **Never change it to "copy into the workspace when referencing"**: then changing the global one would not link through,
// and linking is the very reason this feature exists. A change like "also store the key in the workspace, faster to read"
// must be rejected outright: it gives zero errors, the feature just quietly disappears.
//
// The only entry point for resolving is ResolveSecret; arkKey in core/chat.go goes through it.

package arkcred

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// ResolveSecret resolves the Ark credential **actually in effect** for a workspace (with plaintext).
//
// # This is the only entry point of the whole reference mechanism
//
//	source = override (default) → use the workspace's own
//	source = inherit            → fall back to the account-level credential of the person in inheritFrom
//
// The fallback **fetches fresh on every read**, so changing the global one takes effect at once. Do not add a cache here: that would turn
// "how long until a global change takes effect" into an unclear question, and production has multiple instances each with its own cache.
//
// Both kinds of "missing" return ErrNotFound, and the caller decides how to present it:
//   - the workspace was never configured (no such row in ark_secrets)
//   - the workspace is inherit, but the referenced global credential does not exist (never configured, or deleted)
//
// The second is deliberately not told apart: for the user the result is the same (this workspace cannot use Ark now),
// and the guidance text is given by the upper layer based on source (see arkKey in core/chat.go).
//
// ⚠️ **It is exported because the core side still uses it** (arkKey in chat.go and
// arkConsoleRegion in arkconsole.go both go through three-level resolve). It used to be the unexported resolveArkSecret.
func (s *Service) ResolveSecret(ctx context.Context, workspaceID string) (apimodel.ArkSecret, error) {
	row, err := s.store.GetArkSecret(ctx, workspaceID)
	if err != nil {
		return apimodel.ArkSecret{}, err
	}
	if apimodel.NormalizeArkSource(row.Source) != apimodel.SourceInherit {
		return row, nil
	}

	// inherit: fall back to the account-level one.
	global, err := s.store.GetGlobalArkSecret(ctx, strings.ToLower(row.InheritFrom))
	if err != nil {
		return apimodel.ArkSecret{}, err
	}
	// Build a view that "looks like a workspace credential" for the caller, **but with all content from the global one**.
	// WorkspaceID and Source keep the workspace side's values; everything else (including plaintext and check status) comes from the global one.
	return apimodel.ArkSecret{
		ArkCredential: apimodel.ArkCredential{
			WorkspaceID:  workspaceID,
			ApiKeyMasked: global.ApiKeyMasked,
			Region:       global.Region,
			Check:        global.Check,
			CheckedAt:    global.CheckedAt,
			CheckError:   global.CheckError,
			UpdatedAt:    global.UpdatedAt,
			// UpdatedBy uses the referenced person's email: the UI must make clear "who configured this key".
			UpdatedBy: global.Email,
			Source:    apimodel.SourceInherit,
			// InheritFrom is passed through as is; the frontend shows "from xxx's global settings".
			InheritFrom: row.InheritFrom,
		},
		APIKey: global.APIKey,
	}, nil
}

// ResolveSecretForAgent resolves the Ark credential **actually in effect for an agent** (with plaintext).
//
// # Three-level fallback, more specific wins
//
//	agent level (agent_ark_secrets has this row)  → use it
//	otherwise                                     → delegate to ResolveSecret (workspace level → account level)
//
// # Why it is a separate function from ResolveSecret, instead of an optional agentID
//
// The two paths **really mean different things**, and putting the difference in the function name lets call sites state their intent:
//
//	the settings page shows "what is this workspace's default"   → ResolveSecret(ws)
//	conversations and the worker use "which key this agent uses"  → ResolveSecretForAgent(ws, agentID)
//
// With one function where an empty agentID means "ignore the agent level", callers could not tell which level's value
// they got, and when an agent-level credential exists the two are different keys. Using the wrong one shows as
// "the settings page says everything is fine, but some agent's conversation reports a credential problem".
//
// # Invariant: a lower level stores no copy of an upper level
//
// The agent level only has a row when the user **explicitly attaches a key to this agent**. With no row it falls back,
// and the fallback **fetches fresh on every read**, so changing the workspace-level or account-level key means agents following it
// get the new value on their next resolve. ⚠️ Never change it to "copy into the agent row when following": then changing the upper level would not link through,
// and linking is the very reason three config levels exist (same as the SourceInherit case, for word-for-word the same reason).
func (s *Service) ResolveSecretForAgent(
	ctx context.Context, workspaceID, agentID string,
) (apimodel.ArkSecret, error) {
	if strings.TrimSpace(agentID) != "" {
		sec, err := s.store.GetAgentArkSecret(ctx, agentID)
		if err == nil && strings.TrimSpace(sec.APIKey) != "" {
			// Build a view that "looks like a workspace credential" for the caller, with all content from the agent row.
			// WorkspaceID uses the one passed in (the one in the agent row may be a historical value, for example if the agent was moved between workspaces).
			return apimodel.ArkSecret{
				ArkCredential: apimodel.ArkCredential{
					WorkspaceID:  workspaceID,
					ApiKeyMasked: sec.ApiKeyMasked,
					Region:       sec.Region,
					Check:        sec.Check,
					CheckedAt:    sec.CheckedAt,
					CheckError:   sec.CheckError,
					UpdatedAt:    sec.UpdatedAt,
					UpdatedBy:    sec.UpdatedBy,
					// The agent attached its own key, so for itself it is override.
					Source: apimodel.SourceOverride,
				},
				APIKey: sec.APIKey,
			}, nil
		}
		if err != nil && !errors.Is(err, persistmodel.ErrNotFound) {
			// If the database cannot be read, return the error; **do not silently fall back to the workspace level**: that would make "the agent has its own key"
			// quietly stop working during a database hiccup, and the user's conversation would run on another key (billed to the wrong person).
			return apimodel.ArkSecret{}, err
		}
	}
	// No agent level → follow the workspace (the workspace may itself be inherit, falling back further to the account level).
	return s.ResolveSecret(ctx, workspaceID)
}

// GetArkCredential gets the external view of the credential.
//
// **When not configured it returns an empty shell instead of 404**: the frontend SettingsPage uses whether apiKeyMasked is empty
// to tell "not configured", and whether the whole object is null to tell "still loading". Returning 404 would leave it stuck
// on the skeleton screen forever. The empty shell's region must be cn-beijing (not an empty string), otherwise the region
// dropdown's selectedKey breaks.
//
// ⚠️ **With inherit this returns the mask of the global key, not an empty string**. With an empty string the frontend would decide
// "not configured" and show the empty "enter API Key" state, while this workspace actually works,
// and the user would think the reference did not take effect and paste the key again.
func (s *Service) GetArkCredential(ctx context.Context, workspaceID string) (apimodel.ArkCredential, error) {
	sec, err := s.ResolveSecret(ctx, workspaceID)
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			// Both cases land here: never configured, or inherit but the global one does not exist.
			// The latter must pass source through truthfully; otherwise the UI would show "custom for this workspace, not configured",
			// while the truth is "references the global one, but the global one is gone", and the next steps for the two are different.
			shell := apimodel.ArkCredential{
				WorkspaceID:  workspaceID,
				ApiKeyMasked: "",
				Region:       apimodel.RegionCNBeijing,
				Check:        apimodel.CheckUnchecked,
				Source:       apimodel.SourceOverride,
			}
			if row, rerr := s.store.GetArkSecret(ctx, workspaceID); rerr == nil {
				shell.Source = apimodel.NormalizeArkSource(row.Source)
				shell.InheritFrom = row.InheritFrom
			}
			return shell, nil
		}
		return apimodel.ArkCredential{}, err
	}
	out := sec.ArkCredential
	// Read-path normalization: existing rows have no source key, so it deserializes as an empty string.
	out.Source = apimodel.NormalizeArkSource(out.Source)
	return out, nil
}

// SaveArkCredential saves a new key.
//
// After changing the key, the old check result is void at once (check reset to unchecked). The record is replaced as a whole here,
// so checkedAt / checkError are gone naturally.
//
// updatedBy records the email of the **first owner** in the workspace (through the injected OwnerEmailOf,
// whose real implementation is workspacesvc.FirstOwnerEmail). Credential config must leave a trail, and this rule must have the same source as
// WorkspaceOwnerOf; see Config.OwnerEmailOf.
func (s *Service) SaveArkCredential(
	ctx context.Context, workspaceID string, draft apimodel.ArkCredentialDraft,
) (apimodel.ArkCredential, error) {
	// First turn the "fetch from Volcano Engine account" path into plaintext; after that both sources go through the same validation and storage.
	// It comes first so errors like "could not get the key" take priority over format validation; the latter would say
	// "API Key too short" when the user never typed a key at all.
	plain, err := s.resolveDraftKey(ctx, draft)
	if err != nil {
		return apimodel.ArkCredential{}, err
	}
	draft.APIKey = plain

	// Validation comes first, in the same order as LocalApiClient (it validates the key before looking up the workspace)
	if err := ValidateAPIKey(draft.APIKey); err != nil {
		return apimodel.ArkCredential{}, err
	}
	if !validRegions[draft.Region] {
		return apimodel.ArkCredential{}, fmt.Errorf("%w: 区域取值不合法", persistmodel.ErrInvalidArgument)
	}

	ws, err := s.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return apimodel.ArkCredential{}, fmt.Errorf("%w: 空间不存在", persistmodel.ErrNotFound)
	}

	apiKey := strings.TrimSpace(draft.APIKey)

	// Before storing, do one read-only online check (bad key blocked, not activated allowed after confirmation, dependency failure allowed + warning).
	// The three outcomes are in saveverify.go. **It only runs on the save-new-credential path; existing credentials are not batch-checked at startup.**
	reject, warning := s.verifyBeforeSave(ctx, apiKey, draft.Region, draft.ConfirmNotActivated)
	if reject != nil {
		return apimodel.ArkCredential{}, reject
	}

	sec := apimodel.ArkSecret{
		ArkCredential: apimodel.ArkCredential{
			WorkspaceID: workspaceID,
			// The mask is computed from the trimmed key (same as the frontend)
			ApiKeyMasked: maskSecret(apiKey),
			Region:       draft.Region,
			Check:        apimodel.CheckUnchecked,
			UpdatedAt:    apimodel.NowISO(),
			UpdatedBy:    s.ownerEmailOf(ws.Members),
			// **Attaching your own key means override**, even if this workspace was inherit before.
			// Without this line the zero-value empty string would be normalized to override, which happens to be right too,
			// but that is by coincidence, and it reads like an omission. Write it out explicitly.
			Source: apimodel.SourceOverride,
		},
		APIKey: apiKey,
	}
	if err := s.store.PutArkSecret(ctx, sec); err != nil {
		return apimodel.ArkCredential{}, err
	}
	out := sec.ArkCredential
	// When "allowed without a completed check", attach the transient warning to the response (not stored; GET does not include it).
	out.Warning = warning
	return out, nil
}

// UpdateArkRegion changes only the region, not the key.
//
// The same key is not necessarily activated in every region; changing region changes what was verified, so
// check is reset. Note checkedAt and checkError must be **removed** (set empty so omitempty applies and
// the keys vanish from JSON), but **updatedBy is not touched**, copied from api.ts:539-542.
func (s *Service) UpdateArkRegion(
	ctx context.Context, workspaceID string, region apimodel.ArkRegion,
) (apimodel.ArkCredential, error) {
	if !validRegions[region] {
		return apimodel.ArkCredential{}, fmt.Errorf("%w: 区域取值不合法", persistmodel.ErrInvalidArgument)
	}
	sec, err := s.store.GetArkSecret(ctx, workspaceID)
	if err != nil {
		return apimodel.ArkCredential{}, fmt.Errorf("%w: 请先填写 API Key", persistmodel.ErrPrecondition)
	}
	// **An inherit workspace cannot change the region here**: the region is a property of that key, which belongs to the global one.
	// Allowing it would write a row "with region but no key" for this workspace, and the next resolve would still fall back
	// to the global one: the user changed the region with no effect and no error. To change it, go to the global settings.
	if apimodel.NormalizeArkSource(sec.Source) == apimodel.SourceInherit {
		return apimodel.ArkCredential{}, fmt.Errorf(
			"%w: 本空间引用的是全局方舟凭证，请到全局设置里改区域，"+
				"或先改用本空间自定义的 key", persistmodel.ErrPrecondition)
	}

	sec.Region = region
	sec.Check = apimodel.CheckUnchecked
	sec.CheckError = ""
	sec.CheckedAt = ""
	sec.UpdatedAt = apimodel.NowISO()
	// UpdatedBy deliberately left alone

	if err := s.store.PutArkSecret(ctx, sec); err != nil {
		return apimodel.ArkCredential{}, err
	}
	return sec.ArkCredential, nil
}

// VerifyArkCredential calls an Ark read-only endpoint for real with the current key, and writes back the check result.
//
// # An invalid key is not an HTTP error
//
// Ark has no dedicated "test key" endpoint; the implementation lists Agents once with the key and treats 401/403 as invalid.
// The key point: **"the key does not work" is a business conclusion, returned as 200 + check:"invalid"**,
// not 401. The frontend SettingsPage calls this method without try/catch, so throwing would become an unhandled
// rejection: nothing shows on the page, the spinner finishes and nothing happens.
//
// Only two cases count as request failures: the credential is not configured at all (precondition not met), and network/service failure
// (then the check conclusion should stay unchanged instead of being written as invalid; not checked is not the same as failed).
func (s *Service) VerifyArkCredential(ctx context.Context, workspaceID string) (apimodel.ArkCredential, error) {
	// Use ResolveSecret to get the plaintext: with inherit, the key to verify is the global one.
	sec, err := s.ResolveSecret(ctx, workspaceID)
	if err != nil {
		return apimodel.ArkCredential{}, fmt.Errorf("%w: 请先填写 API Key", persistmodel.ErrPrecondition)
	}
	if s.verifier == nil {
		return apimodel.ArkCredential{}, fmt.Errorf("%w: 服务端未配置方舟校验器", persistmodel.ErrPrecondition)
	}

	verr := s.verifier.Verify(ctx, sec.APIKey, sec.Region)
	switch verr.Status {
	case VerifyStatusOK:
		sec.Check = apimodel.CheckValid
		sec.CheckError = ""
	case VerifyStatusInvalidKey:
		sec.Check = apimodel.CheckInvalid
		// The text must be readable Chinese: it is shown directly to the user.
		sec.CheckError = "方舟返回 401/403：API Key 无效或已被删除"
	case VerifyStatusNotActivated:
		sec.Check = apimodel.CheckInvalid
		sec.CheckError = "API Key 有效，但它所属的方舟账号还没开通 Managed Agents 服务，请到方舟控制台开通"
	default: // VerifyStatusSkipped
		// Not verified; do not change the conclusion. Pass the reason out and let the caller decide how to present it.
		if verr.Cause != nil {
			return apimodel.ArkCredential{}, fmt.Errorf("校验方舟凭证失败: %w", verr.Cause)
		}
		return apimodel.ArkCredential{}, fmt.Errorf("校验方舟凭证失败：方舟暂时不可用")
	}
	sec.CheckedAt = apimodel.NowISO()

	// ⚠️ **The check result must be written back to where it really belongs**.
	//
	// With inherit, sec is the view built by ResolveSecret, carrying **the global one's plaintext**.
	// Calling PutArkSecret directly would copy that key into this workspace's row, and this workspace would from then on have
	// its own copy, **changing the global one would never link through again**, while the UI still shows "from global settings".
	// This is the most hidden way to break the whole reference mechanism: no error, looks right at the time, and days later you find
	// "why didn't changing the global one take effect".
	if apimodel.NormalizeArkSource(sec.Source) == apimodel.SourceInherit {
		global, gerr := s.store.GetGlobalArkSecret(ctx, strings.ToLower(sec.InheritFrom))
		if gerr != nil {
			return apimodel.ArkCredential{}, gerr
		}
		global.Check = sec.Check
		global.CheckError = sec.CheckError
		global.CheckedAt = sec.CheckedAt
		if err := s.store.PutGlobalArkSecret(ctx, global); err != nil {
			return apimodel.ArkCredential{}, err
		}
		return sec.ArkCredential, nil
	}

	if err := s.store.PutArkSecret(ctx, sec); err != nil {
		return apimodel.ArkCredential{}, err
	}
	return sec.ArkCredential, nil
}

// UseGlobalArkCredential makes a workspace **reference** the current logged-in user's account-level Ark credential.
//
// # This is that "one explicit configuration step"
//
// A workspace never inherits anything silently: a new workspace is still "not configured", and someone must explicitly click
// "use global config" once to get here. Otherwise the user would not know which key they are using,
// and credential config is the last thing people should have to guess.
//
// **The row written has no plaintext** (APIKey is an empty string): that is exactly how linking works:
// the workspace has no copy, and every read falls back to the global one.
func (s *Service) UseGlobalArkCredential(ctx context.Context, workspaceID string) (apimodel.ArkCredential, error) {
	actor, err := actorFrom(ctx)
	if err != nil {
		return apimodel.ArkCredential{}, err
	}
	if _, err := s.store.GetWorkspace(ctx, workspaceID); err != nil {
		return apimodel.ArkCredential{}, fmt.Errorf("%w: 空间不存在", persistmodel.ErrNotFound)
	}

	// **First confirm the global one exists**. Without that, the user clicks "use global", the UI shows
	// the reference succeeded, then opening a conversation reports "credential does not exist"; by then they neither know where to configure it,
	// and will also suspect the reference did not take effect.
	if _, err := s.store.GetGlobalArkSecret(ctx, actor); err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			return apimodel.ArkCredential{}, fmt.Errorf(
				"%w: 你还没有配置全局方舟 API Key，请先到全局设置里填写", persistmodel.ErrPrecondition)
		}
		return apimodel.ArkCredential{}, err
	}

	row := apimodel.ArkSecret{
		ArkCredential: apimodel.ArkCredential{
			WorkspaceID: workspaceID,
			Source:      apimodel.SourceInherit,
			InheritFrom: actor,
			UpdatedAt:   apimodel.NowISO(),
			// UpdatedBy records **the person who clicked reference**, not the referenced person: this row's change was made by them.
			UpdatedBy: actor,
			// Region / Check / mask are all left empty: they belong to the global one and are fetched fresh on read.
			// Filling a copy here would go stale at once (changing the global one does not change this), and a stale mask
			// is more misleading than no mask: the user would think they are looking at the key currently in effect.
			Region: apimodel.RegionCNBeijing,
			Check:  apimodel.CheckUnchecked,
		},
		// **Plaintext left empty**; see the method comment.
		APIKey: "",
	}
	if err := s.store.PutArkSecret(ctx, row); err != nil {
		return apimodel.ArkCredential{}, err
	}
	// Read back once, so the response carries the mask and check status resolved from the global one.
	return s.GetArkCredential(ctx, workspaceID)
}

// GetGlobalArkCredential gets the current logged-in user's account-level credential (mask view).
//
// **When never configured it returns an empty shell instead of 404**, same as GetArkCredential: the frontend uses an empty mask for "not configured",
// and null for "loading".
func (s *Service) GetGlobalArkCredential(ctx context.Context) (apimodel.GlobalArkCredential, error) {
	actor, err := actorFrom(ctx)
	if err != nil {
		return apimodel.GlobalArkCredential{}, err
	}
	sec, err := s.store.GetGlobalArkSecret(ctx, actor)
	if err != nil {
		if errors.Is(err, persistmodel.ErrNotFound) {
			return apimodel.GlobalArkCredential{
				ApiKeyMasked: "",
				Region:       apimodel.RegionCNBeijing,
				Check:        apimodel.CheckUnchecked,
			}, nil
		}
		return apimodel.GlobalArkCredential{}, err
	}
	return sec.Credential(), nil
}

// SaveGlobalArkCredential saves the current logged-in user's account-level credential.
//
// ⚠️ **After saving, every workspace referencing it uses the new key at once**: that is the point of this feature,
// but it also means this operation has a wider impact than a workspace-level credential. The UI must make that clear.
func (s *Service) SaveGlobalArkCredential(
	ctx context.Context, draft apimodel.ArkCredentialDraft,
) (apimodel.GlobalArkCredential, error) {
	actor, err := actorFrom(ctx)
	if err != nil {
		return apimodel.GlobalArkCredential{}, err
	}
	// Same as SaveArkCredential: first turn "fetch from Volcano Engine account" into plaintext, then go through the same validation.
	plain, err := s.resolveDraftKey(ctx, draft)
	if err != nil {
		return apimodel.GlobalArkCredential{}, err
	}
	draft.APIKey = plain
	// Validation order is the same as SaveArkCredential: key first, then region.
	if err := ValidateAPIKey(draft.APIKey); err != nil {
		return apimodel.GlobalArkCredential{}, err
	}
	if !validRegions[draft.Region] {
		return apimodel.GlobalArkCredential{}, fmt.Errorf("%w: 区域取值不合法", persistmodel.ErrInvalidArgument)
	}

	apiKey := strings.TrimSpace(draft.APIKey)

	// Same as SaveArkCredential: the three-outcome online check before storing (see saveverify.go).
	reject, warning := s.verifyBeforeSave(ctx, apiKey, draft.Region, draft.ConfirmNotActivated)
	if reject != nil {
		return apimodel.GlobalArkCredential{}, reject
	}

	sec := apimodel.GlobalArkSecret{
		Email:        actor,
		APIKey:       apiKey,
		ApiKeyMasked: maskSecret(apiKey),
		Region:       draft.Region,
		// After changing the key, the old check result is void at once.
		Check:     apimodel.CheckUnchecked,
		UpdatedAt: apimodel.NowISO(),
	}
	if err := s.store.PutGlobalArkSecret(ctx, sec); err != nil {
		return apimodel.GlobalArkCredential{}, err
	}
	out := sec.Credential()
	out.Warning = warning
	return out, nil
}

// DeleteGlobalArkCredential deletes the current logged-in user's account-level credential. Deleting something missing is not an error (idempotent).
//
// # ⚠️ It does not change the workspaces that reference it
//
// Those workspaces end up "referencing a credential that does not exist", reading as not configured, and opening a conversation shows a
// clear guidance message (see arkKey in core/chat.go). **Cascading is deliberately not done**, for two reasons:
//
//  1. Cascading would scan all workspaces and rewrite them one by one, a batch write across aggregates; with multiple instances there is no transaction boundary,
//     and failing halfway would leave a batch of workspaces in an inconsistent state.
//  2. More importantly, **the meaning**: changing them to override means "quietly leaving these workspaces an empty config",
//     while what the user most likely wants is "switch to a new global key". Once they configure the new one, those workspaces
//     just work again, because they reference the position "my global credential", not one specific key.
func (s *Service) DeleteGlobalArkCredential(ctx context.Context) error {
	actor, err := actorFrom(ctx)
	if err != nil {
		return err
	}
	return s.store.DeleteGlobalArkSecret(ctx, actor)
}

// VerifyGlobalArkCredential calls an Ark read-only endpoint for real with the account-level key, and writes back the check result.
//
// The rules are word for word the same as VerifyArkCredential: an invalid key is a **business conclusion** (200 + invalid),
// and only "not configured" and "network/service failure" count as request failures.
func (s *Service) VerifyGlobalArkCredential(ctx context.Context) (apimodel.GlobalArkCredential, error) {
	actor, err := actorFrom(ctx)
	if err != nil {
		return apimodel.GlobalArkCredential{}, err
	}
	sec, err := s.store.GetGlobalArkSecret(ctx, actor)
	if err != nil {
		return apimodel.GlobalArkCredential{}, fmt.Errorf("%w: 请先填写 API Key", persistmodel.ErrPrecondition)
	}
	if s.verifier == nil {
		return apimodel.GlobalArkCredential{}, fmt.Errorf("%w: 服务端未配置方舟校验器", persistmodel.ErrPrecondition)
	}

	verr := s.verifier.Verify(ctx, sec.APIKey, sec.Region)
	switch verr.Status {
	case VerifyStatusOK:
		sec.Check = apimodel.CheckValid
		sec.CheckError = ""
	case VerifyStatusInvalidKey:
		sec.Check = apimodel.CheckInvalid
		sec.CheckError = "方舟返回 401/403：API Key 无效或已被删除"
	case VerifyStatusNotActivated:
		sec.Check = apimodel.CheckInvalid
		sec.CheckError = "API Key 有效，但它所属的方舟账号还没开通 Managed Agents 服务，请到方舟控制台开通"
	default:
		if verr.Cause != nil {
			return apimodel.GlobalArkCredential{}, fmt.Errorf("校验方舟凭证失败: %w", verr.Cause)
		}
		return apimodel.GlobalArkCredential{}, fmt.Errorf("校验方舟凭证失败：方舟暂时不可用")
	}
	sec.CheckedAt = apimodel.NowISO()

	if err := s.store.PutGlobalArkSecret(ctx, sec); err != nil {
		return apimodel.GlobalArkCredential{}, err
	}
	return sec.Credential(), nil
}

// DeleteArkCredential deletes the credential. Deleting something missing is not an error (idempotent).
func (s *Service) DeleteArkCredential(ctx context.Context, workspaceID string) error {
	return s.store.DeleteArkSecret(ctx, workspaceID)
}
