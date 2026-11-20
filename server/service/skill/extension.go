package skill

import "github.com/chyroc/ama/server/model/apimodel"

// BuiltinProvider supplies platform-provided skills to a downstream deployment.
// It contains no persistence: the implementation owns its immutable built-in content.
// Returning a new slice from Skills and Files lets callers safely join or modify results.
type BuiltinProvider interface {
	Skills() []apimodel.Skill
	Skill(id string) (apimodel.Skill, bool)
	NameTaken(name string) bool
	Files(id string) []apimodel.SkillFile
}

// SourcePolicy decides whether an externally sourced skill is read-only.
// An empty reason means the source is editable. This is deliberately a policy, not a
// list of company-specific source names: downstreams own those source identifiers.
type SourcePolicy interface {
	ReadOnlyReason(source apimodel.SkillSource) string
}

func (s *Service) builtinSkills() []apimodel.Skill {
	if s.builtins != nil {
		return s.builtins.Skills()
	}
	return BuiltinSkills()
}

func (s *Service) builtinSkill(id string) (apimodel.Skill, bool) {
	if s.builtins != nil {
		return s.builtins.Skill(id)
	}
	return BuiltinSkill(id)
}

func (s *Service) builtinNameTaken(name string) bool {
	if s.builtins != nil {
		return s.builtins.NameTaken(name)
	}
	return BuiltinSkillNameTaken(name)
}

func (s *Service) builtinFiles(id string) []apimodel.SkillFile {
	if s.builtins != nil {
		return s.builtins.Files(id)
	}
	return BuiltinSkillFiles(id)
}

func (s *Service) readOnlyReason(source apimodel.SkillSource) string {
	if s.sources != nil {
		return s.sources.ReadOnlyReason(source)
	}
	return ""
}
