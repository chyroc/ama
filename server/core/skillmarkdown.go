package core

import (
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
)

// RenderSkillMarkdown returns a skill's SKILL.md body. Non-empty instructions
// are returned unchanged so user frontmatter remains intact. An empty body gets
// a minimal title/description document so a bound skill still has a visible directory.
func RenderSkillMarkdown(skill apimodel.Skill) string {
	if strings.TrimSpace(skill.Instructions) != "" {
		return skill.Instructions
	}
	title := skill.DisplayTitle
	if strings.TrimSpace(title) == "" {
		title = skill.Name
	}
	markdown := "# " + title + "\n"
	if strings.TrimSpace(skill.Description) != "" {
		markdown += "\n" + skill.Description + "\n"
	}
	return markdown
}
