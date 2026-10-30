// Ark console jump for the skill domain (one read-only side API).
//
// This file **was moved from** GetSkillArkConsole in core/arkconsole.go (in the step that split
// arkconsole into domains), with not a line of logic changed. For the two arguments "why a side
// API instead of adding fields to Skill" and "why only id and region are given and the frontend
// builds the URL", see the header of core/arkconsole.go (that part is shared by three entities and
// stays there with the shared logic).

package skill

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// Ark is the Ark-side locating capability the skill domain needs; **only the console jump uses
// it**.
//
// The real thing is in core (resolving region and key carries the three-level fallback semantics,
// and the binding and upload tables belong to the ark domain); an adapter on the core side plugs it
// in at wiring time. **May be nil**: then GetSkillArkConsole always returns an all-empty ref, the
// frontend does not render the entry, the same as "no Ark credential configured".
type Ark interface {
	// Region resolves the region of the key actually in effect for (workspace, agent); an empty
	// agentID uses the workspace level. Returns an empty string if it cannot be resolved.
	Region(ctx context.Context, workspaceID, agentID string) apimodel.ArkRegion
	// AgentBound tells whether an agent has created an Ark binding (≈ has ever opened a
	// conversation). A query error counts as "never created"; this path means "find a usable
	// entry", so tolerance comes first.
	AgentBound(ctx context.Context, workspaceID, agentID string) bool
	// KeyForAgent resolves the key the agent actually uses; the second return value is false if it cannot be resolved.
	KeyForAgent(ctx context.Context, workspaceID, agentID string) (key ArkKey, ok bool)
	// SkillUploadID looks up the Ark skill id in the upload records by (skill content fingerprint +
	// key fingerprint); the second return value false means never uploaded.
	SkillUploadID(ctx context.Context, workspaceID, skillID, contentHash, apiKey string) (id string, ok bool)
}

// ArkKey is the two properties of the key the skill domain needs.
type ArkKey struct {
	// APIKey is the plaintext key, used to compute the key fingerprint for SkillUploadID; never logged.
	APIKey string
	// Region is this key's region.
	Region apimodel.ArkRegion
}

// GetSkillArkConsole gives a skill's location in the Ark console.
//
// # Why pick "some agent that binds it"
//
// The source of truth for Ark skill ids is ark_skill_uploads, whose primary key is the four-tuple
// `(workspace, skill, content fingerprint, key fingerprint)`; that is, the question "which id is
// this skill on the Ark side" **itself depends on which key asks**: the same skill uploaded with
// two keys is two Ark skills, belonging to two projects.
//
// So a key must be located first. The approach is to find an agent that binds this skill and has
// already created an Ark binding, and use its key. If none is found, return an empty ref (no agent
// has used the skill yet, so it really is not on the Ark side).
//
// ⚠️ **Do not change this to "take any upload record"** (for example the latest by skill id): that
// record may have been uploaded with another key, while the current user can access the project of
// their own key; the link would point at a resource they cannot see, showing as "no permission or
// does not exist" when opened. Better to offer no entry.
func (s *Service) GetSkillArkConsole(ctx context.Context, skillID string) (apimodel.ArkConsoleRef, error) {
	skillID = strings.TrimSpace(skillID)
	if skillID == "" {
		return apimodel.ArkConsoleRef{}, fmt.Errorf("%w: 技能 ID 不能为空", persistmodel.ErrInvalidArgument)
	}
	sk, err := s.GetSkill(ctx, skillID)
	if err != nil {
		return apimodel.ArkConsoleRef{}, err
	}
	if s.ark == nil {
		// No ark locating capability wired (missing wiring or test environment): return an all-empty ref, no entry.
		return apimodel.ArkConsoleRef{}, nil
	}
	// Built-in skills belong to no workspace (WorkspaceID is an empty string), so neither
	// workspace nor key can be located. They really are uploaded on the Ark side (one copy per
	// workspace using them), but "which one is yours" depends on which workspace you are looking from,
	// and the skill detail page has no such context. No entry.
	//
	// This one cannot even give a region (region is also resolved per workspace), so it is a truly
	// empty ref.
	if sk.WorkspaceID == "" {
		return apimodel.ArkConsoleRef{}, nil
	}

	// region is resolved by **workspace** (agentID empty → workspace level), independent of whether
	// an upload record is found below.
	//
	// ⚠️ **Include the region even when not found**; do not return an all-empty ref: the frontend
	// uses "has region but no id" to judge "you are using Ark, this skill just has not been uploaded
	// yet", and all-empty would read as "Ark was never configured", which is something else (same
	// handling as the agent one).
	ref := apimodel.ArkConsoleRef{Region: s.ark.Region(ctx, sk.WorkspaceID, "")}

	agents, err := s.store.ListAgents(ctx, sk.WorkspaceID)
	if err != nil {
		return apimodel.ArkConsoleRef{}, fmt.Errorf("列 agent 失败: %w", err)
	}

	for _, a := range agents {
		if !slices.Contains(a.SkillIDs, skillID) {
			continue
		}
		// Has this agent created an Ark binding? If not, it has never opened a conversation, so the
		// skills it binds have not been uploaded either.
		if !s.ark.AgentBound(ctx, a.WorkspaceID, a.ID) {
			continue
		}
		key, ok := s.ark.KeyForAgent(ctx, a.WorkspaceID, a.ID)
		if !ok {
			continue
		}
		// The fingerprint must be computed from the **current** content: after the skill changes it
		// is a new id on the Ark side, and pointing at the old one takes the user to outdated
		// content.
		files, err := s.SkillFiles(ctx, sk.ID)
		if err != nil {
			continue
		}
		id, ok := s.ark.SkillUploadID(ctx, sk.WorkspaceID, sk.ID, SkillFingerprint(sk, files), key.APIKey)
		if !ok {
			continue
		}
		// ⚠️ The region is **this key's**, not the workspace one computed above: the skill was
		// uploaded with this key, and an agent-level credential may point to another region. Using the
		// wrong one would point the link to a region that does not have this skill.
		return apimodel.ArkConsoleRef{ArkSkillID: id, Region: key.Region}, nil
	}
	return ref, nil
}
