package skill

import (
	"context"
	"fmt"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// ForkSkill copies a skill into a custom skill in this workspace.
//
// # Why it is needed
//
// The body of a built-in skill cannot be changed. Without fork the user is stuck: to change one
// sentence based on the official skill, they could only copy and paste the whole text by hand (and
// attachments would not come along).
//
// The copy is custom and can be edited freely.
// That is exactly what the user wants: once they change it, it is their own skill.
//
// # sourceID goes in the body, not the path, for a reason
//
// The route is `POST /api/workspaces/{workspaceId}/skills/fork`, not `POST /api/skills/{id}/fork`.
// With the latter, the authz Guard would resolve the owning workspace from the skill id, and
// core/resource.go returns ErrPlatformResource for the builtin prefix (translated to "write
// denied"); so **forking a builtin skill would always be 403**, and the error would not point at
// "the route shape was chosen wrong" at all. Under the workspace route, workspaceId is in the path,
// and Guard needs no resolution.
func (s *Service) ForkSkill(
	ctx context.Context, workspaceID, sourceID string,
) (apimodel.Skill, error) {
	if _, err := s.store.GetWorkspace(ctx, workspaceID); err != nil {
		return apimodel.Skill{}, fmt.Errorf("%w: 空间不存在", persistmodel.ErrNotFound)
	}

	// The source may be a built-in skill (not in the database; the source of truth is code constants).
	var src apimodel.Skill
	if IsBuiltinSkill(sourceID) {
		b, ok := BuiltinSkill(sourceID)
		if !ok {
			return apimodel.Skill{}, fmt.Errorf("%w: 技能不存在", persistmodel.ErrNotFound)
		}
		src = b
	} else {
		got, err := s.store.GetSkill(ctx, sourceID)
		if err != nil {
			return apimodel.Skill{}, fmt.Errorf("%w: 技能不存在", persistmodel.ErrNotFound)
		}
		// **No forking across workspaces.** Without this check, workspace B's skill body could be
		// copied into workspace A, and the caller would only need to know an id; that is a silent
		// unauthorized read. Built-in skills are exempt (WorkspaceID is an empty string); they are
		// visible in every workspace anyway.
		if got.WorkspaceID != workspaceID {
			return apimodel.Skill{}, fmt.Errorf("%w: 技能不属于这个空间", persistmodel.ErrNotFound)
		}
		src = got
	}

	// Attachments must come along **with their content bytes**. The copy in doc only has path+size;
	// building the copy from it would give a pile of 0-byte files, and that **gives no error**; the
	// agent just reads empty files.
	files, err := s.SkillFiles(ctx, sourceID)
	if err != nil {
		return apimodel.Skill{}, err
	}

	name, err := s.freeSkillName(ctx, workspaceID, src.Name)
	if err != nil {
		return apimodel.Skill{}, err
	}

	return s.CreateSkillFromPackage(ctx, workspaceID, apimodel.SkillPackage{
		Name:         name,
		DisplayTitle: src.DisplayTitle,
		Description:  src.Description,
		Instructions: src.Instructions,
		Files:        files,
	}, apimodel.SkillCreateOptions{})
}

// freeSkillName finds a skill name not taken in this workspace, adding suffixes starting from base.
//
// **Cannot rely on a database conflict**: the name index of the skills table is deliberately not
// UNIQUE (see that long comment in schema_pg.sql); uniqueness is only enforced on the Service's
// create side.
func (s *Service) freeSkillName(ctx context.Context, workspaceID, base string) (string, error) {
	// The suffix limit prevents an endless loop; it is not a business rule. Really hitting 20 copies
	// with the same name means something else is wrong, and an error is more useful then than
	// trying further.
	const maxTries = 20
	for i := 0; i < maxTries; i++ {
		cand := base
		switch i {
		case 0:
			cand = base + "-copy"
		default:
			cand = fmt.Sprintf("%s-copy-%d", base, i+1)
		}
		// Is there a limit on total name length? No hard limit, but name clashes with built-in skills must be avoided too.
		if BuiltinSkillNameTaken(cand) {
			continue
		}
		taken, err := s.store.SkillNameTaken(ctx, workspaceID, cand)
		if err != nil {
			return "", err
		}
		if !taken {
			return cand, nil
		}
	}
	return "", fmt.Errorf("%w: 副本名都被占用了，请先改名或删掉一些副本", persistmodel.ErrConflict)
}
