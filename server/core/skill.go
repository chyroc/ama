// Skills: 10 forwarding methods. **The business logic has moved to service/skill** (step 11b).
//
// These empty shells are a transition state, not a design. The reason is word for word the same as
// in core/agent.go:
//
//  1. the orchestration layer's fallback interface requires these methods on *core.Service
//     (`var _ orchestrator.Fallback = (*core.Service)(nil)` in cmd/amad/orchestrator.go;
//     remove the methods and that assertion fails to compile);
//  2. core still has callers inside: skillark / skillinject / arkconsole use
//     GetSkill / SkillFiles, and the revision rollback uses updateSkill (the one with reason).
//
// ⚠️ **Do not add logic here.** Any check written here bypasses service/skill: the two entry points
// give different answers for the same thing, and nothing reports an error. Add things in
// service/skill.
//
// This file used to be the full skill domain implementation (split from the skill half of
// core/agent.go). It later moved to service/skill as a whole, and the comments moved with it.
package core

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
)

// ListSkills lists the skills in a workspace (built-in skills first). The real code is in
// service/skill.
func (s *Service) ListSkills(ctx context.Context, workspaceID string) ([]apimodel.Skill, error) {
	return s.skills.ListSkills(ctx, workspaceID)
}

// GetSkill gets one skill. The real code is in service/skill.
func (s *Service) GetSkill(ctx context.Context, id string) (apimodel.Skill, error) {
	return s.skills.GetSkill(ctx, id)
}

// CreateSkill creates a skill. The real code is in service/skill.
func (s *Service) CreateSkill(ctx context.Context, workspaceID string, draft apimodel.SkillDraft) (apimodel.Skill, error) {
	return s.skills.CreateSkill(ctx, workspaceID, draft)
}

// UpdateSkill updates a skill and bumps latestVersion. The real code is in service/skill.
func (s *Service) UpdateSkill(ctx context.Context, id string, draft apimodel.SkillDraft) (apimodel.Skill, error) {
	return s.skills.UpdateSkill(ctx, id, draft)
}

// updateSkill forwards UpdateSkill, with an extra reason used when recording history.
//
// It stays unexported: only the rollback in core/revision.go calls it. External callers all use
// UpdateSkill (that side decides the reason). The real code is skillsvc.UpdateSkillWithReason.
func (s *Service) updateSkill(
	ctx context.Context, id string, draft apimodel.SkillDraft, reason apimodel.RevisionReason,
) (apimodel.Skill, error) {
	return s.skills.UpdateSkillWithReason(ctx, id, draft, reason)
}

// CreateSkillFromPackage creates a skill from a parsed skill package. The real code is in
// service/skill.
func (s *Service) CreateSkillFromPackage(
	ctx context.Context, workspaceID string, pkg apimodel.SkillPackage, opts SkillCreateOptions,
) (apimodel.Skill, error) {
	return s.skills.CreateSkillFromPackage(ctx, workspaceID, pkg, opts)
}

// SkillFiles gets all attachments of a skill (with bytes), for the container injection path. The
// real code is in service/skill.
func (s *Service) SkillFiles(ctx context.Context, skillID string) ([]apimodel.SkillFile, error) {
	return s.skills.SkillFiles(ctx, skillID)
}

// SkillFileContent gets the content of one attachment by path. The real code is in service/skill.
func (s *Service) SkillFileContent(
	ctx context.Context, skillID, filePath string,
) (SkillFileContent, error) {
	return s.skills.SkillFileContent(ctx, skillID, filePath)
}

// UpdateSkillFile changes the content of an existing attachment. The real code is in service/skill.
func (s *Service) UpdateSkillFile(
	ctx context.Context, skillID, filePath string, content []byte,
) (apimodel.Skill, error) {
	return s.skills.UpdateSkillFile(ctx, skillID, filePath, content)
}

// DeleteSkill deletes a skill and unbinds it from all agents. The real code is in service/skill.
func (s *Service) DeleteSkill(ctx context.Context, id string) error {
	return s.skills.DeleteSkill(ctx, id)
}
