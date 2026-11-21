package skill

import (
	"slices"
	"strings"

	"github.com/chyroc/ama/server/model/apimodel"
)

// BuiltinSkillProvider exposes the platform's read-only built-ins for downstream distributions.
// A downstream provider may omit entries that its own executor or platform cannot run.
type BuiltinSkillProvider struct {
	ExtraIDs []string
}

func (p BuiltinSkillProvider) Skills() []apimodel.Skill {
	allowed := make(map[string]bool, len(p.ExtraIDs))
	for _, id := range p.ExtraIDs {
		allowed[strings.TrimSpace(id)] = true
	}
	all := BuiltinSkills()
	out := make([]apimodel.Skill, 0, len(all))
	for _, sk := range all {
		if allowed[sk.ID] {
			out = append(out, sk)
		}
	}
	return out
}

func (p BuiltinSkillProvider) Skill(id string) (apimodel.Skill, bool) {
	if !slices.Contains(p.ExtraIDs, strings.TrimSpace(id)) {
		return apimodel.Skill{}, false
	}
	return BuiltinSkill(id)
}

func (p BuiltinSkillProvider) NameTaken(name string) bool {
	for _, sk := range p.Skills() {
		if sk.Name == strings.TrimSpace(name) {
			return true
		}
	}
	return false
}

func (p BuiltinSkillProvider) Files(id string) []apimodel.SkillFile {
	if _, ok := p.Skill(id); !ok {
		return nil
	}
	return BuiltinSkillFiles(id)
}
