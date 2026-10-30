// "Which item is this on the Ark console" — one read-only endpoint each for agent / skill / session.
//
// # What is left in this file after arkconsole was split by domain
//
// The three endpoints now live in their own domain packages (service/agent/console.go,
// service/skill/console.go, service/run/console.go). "Which table this entity's id comes from
// and how the fingerprint is computed" is each domain's own knowledge. This file keeps only two
// things:
//
//  1. arkConsoleRegion: region lookup (the three-level fallback), shared by the three domains;
//  2. three adapters: they connect the Ark abilities on the core side to the narrow interfaces
//     each domain declares (agentsvc.Console / skillsvc.Ark / runsvc's ConsoleRegion).
//
// The adapters **hold *Service and ask each time**; they do not compute at wiring time. Same
// reason as the agentIntegrations / agentSyncs adapters in service.go.
//
// # Why a side-car endpoint instead of adding fields to Agent / Skill / Run
//
// The obvious move is to add an `arkAgentId` to `apimodel.Agent`. **Do not do that**, because of
// how it is stored: `SaveAgent` / `SaveSkill` / `SaveRun` all marshal the **whole struct** into
// the doc jsonb (`marshalDoc(cloned)` in store_pg.go). So that field would be saved as a snapshot,
// while the thing it points to **changes**:
//
//   - With a different Ark key, `ensureBinding` rebuilds the Ark Agent (the branch where
//     sameArkKey is false), and ArkAgentID becomes a new one;
//   - Change one character of a skill and `ArkSkillID` is a different one (Ark has no
//     update-skill API; CreateSkill makes a new id every time, see ark_skill_uploads.sql).
//
// The snapshot does not follow, so the link points to an Ark resource that **is no longer this
// agent's**. It raises no error, and clicking it still opens a page; the page just describes
// something else. That is exactly the "no error, and the error does not point to the real cause"
// kind of bug this repo keeps writing down. Same warning as the Run.AgentDeleted comment:
// **do not pass an enriched entity back to Save**. A side-car endpoint reads from the source
// tables live, so it does not have this problem.
//
// A separate reason: the source of truth for these three ids is in `persistmodel` (ArkBinding /
// ArkSkillUpload), and the rule for persistmodel is "does it go into an HTTP response". Sending
// persistmodel types out directly would break that rule; declaring a response struct that only
// carries ids (apimodel.ArkConsoleRef) keeps the rule intact.
//
// # Why not return a ready-made URL
//
// External observability platforms usually have **their own membership**, AMA users are not in
// them by default, so a deep link is a dead end for most people. The Ark project, on the other
// hand, is the project that owns the key the user configured, so anyone who can configure the key
// can already get in.
//
// So this returns the id and the region, and **the frontend builds the URL**
// (`lib/arkConsole.ts`). This way the backend does not need to know the console's path shape
// (that is the frontend's business, and the Ark docs write the same path two ways), and the
// frontend can simply not render the entry when it cannot build a link.
package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/chyroc/ama/server/model/apimodel"
	agentsvc "github.com/chyroc/ama/server/service/agent"
	skillsvc "github.com/chyroc/ama/server/service/skill"
)

// arkConsoleRegion returns the region of "the key this agent actually uses".
//
// It uses the same lookup as arkKey (resolveArkSecretForAgent, three-level fallback), and **this
// matters**: the settings page reads GetArkCredential and the chat reads arkKey, two separate
// paths. A wrong region builds a link to another region, and that link opens but cannot find the
// resource.
//
// No configured credential is not an error; it returns an empty string, and the caller then does
// not render the entry. This is also the real implementation behind runsvc's ConsoleRegion
// (service.New passes exactly this method value).
func (s *Service) arkConsoleRegion(ctx context.Context, workspaceID, agentID string) apimodel.ArkRegion {
	sec, err := s.resolveArkSecretForAgent(ctx, workspaceID, agentID)
	if err != nil {
		return ""
	}
	if sec.Region == "" {
		// Same fallback as arkKey, word for word (chat.go): older credential docs may lack this key.
		return apimodel.RegionCNBeijing
	}
	return sec.Region
}

// agentConsole connects the Ark lookup ability on the core side to service/agent (the console
// link).
//
// **It holds *Service instead of computing on the spot**: the s.arkCreds / s.store the methods read
// may be changed in tests after New, so computing them at wiring time would give wrong results.
// Same reason as agentIntegrations.
type agentConsole struct{ svc *Service }

func (c agentConsole) Region(ctx context.Context, workspaceID, agentID string) apimodel.ArkRegion {
	return c.svc.arkConsoleRegion(ctx, workspaceID, agentID)
}

func (c agentConsole) AgentBinding(
	ctx context.Context, workspaceID, agentID string,
) (agentsvc.ArkConsoleBinding, bool, error) {
	b, err := c.svc.store.GetArkBinding(ctx, workspaceID, agentID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// No binding = no chat has happened yet; this is a normal state.
			return agentsvc.ArkConsoleBinding{}, false, nil
		}
		return agentsvc.ArkConsoleBinding{}, false, fmt.Errorf("查方舟绑定失败: %w", err)
	}
	return agentsvc.ArkConsoleBinding{ArkAgentID: b.ArkAgentID}, true, nil
}

// skillArk connects the Ark lookup ability on the core side to service/skill (the console link).
//
// For all four methods, an error means "treat as not found": that flow is "find a usable entry",
// and if any step finds nothing it tries the next agent. This is exactly the same as the old core
// version before the split, which did a `continue` for each one.
type skillArk struct{ svc *Service }

func (a skillArk) Region(ctx context.Context, workspaceID, agentID string) apimodel.ArkRegion {
	return a.svc.arkConsoleRegion(ctx, workspaceID, agentID)
}

func (a skillArk) AgentBound(ctx context.Context, workspaceID, agentID string) bool {
	_, err := a.svc.store.GetArkBinding(ctx, workspaceID, agentID)
	return err == nil
}

func (a skillArk) KeyForAgent(ctx context.Context, workspaceID, agentID string) (skillsvc.ArkKey, bool) {
	key, err := a.svc.arkKey(ctx, workspaceID, agentID)
	if err != nil {
		return skillsvc.ArkKey{}, false
	}
	return skillsvc.ArkKey{APIKey: key.APIKey, Region: key.Region}, true
}

func (a skillArk) SkillUploadID(
	ctx context.Context, workspaceID, skillID, contentHash, apiKey string,
) (string, bool) {
	// The key fingerprint is computed here in the adapter (ArkKeyFingerprint lives in this
	// package): the domain package only passes "this key's plaintext + content fingerprint" and
	// does not need to know how the fingerprint is made.
	up, err := a.svc.store.GetArkSkillUpload(
		ctx, workspaceID, skillID, contentHash, ArkKeyFingerprint(apiKey))
	if err != nil {
		return "", false
	}
	return up.ArkSkillID, true
}
