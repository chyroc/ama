package skill

import (
	"context"
	"fmt"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// SkillSourceUpdaters dispatches by stored Origin.Kind. Register only the sources that are
// actually wired; an absent or unknown source must not be reported as a successful check.
type SkillSourceUpdaters map[string]func(context.Context, apimodel.Skill) (apimodel.Skill, bool, error)

// ApplySkillSourceUpdate rejects nil Origin and unregistered source kinds explicitly. The
// updater owns provider reads; this dispatcher never invents a fallback for a different source.
func ApplySkillSourceUpdate(
	ctx context.Context, skill apimodel.Skill, updaters SkillSourceUpdaters,
) (apimodel.Skill, bool, error) {
	if skill.Origin == nil {
		return apimodel.Skill{}, false, fmt.Errorf("%w: 这个技能不是从外部来源装来的，没有可更新的来源", persistmodel.ErrPrecondition)
	}
	update := updaters[skill.Origin.Kind]
	if update == nil {
		return apimodel.Skill{}, false, fmt.Errorf("%w: 不认识的技能来源 %q", persistmodel.ErrPrecondition, skill.Origin.Kind)
	}
	return update(ctx, skill)
}
