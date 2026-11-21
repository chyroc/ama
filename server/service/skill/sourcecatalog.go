package skill

import (
	"context"
	"fmt"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// SkillSourceCatalog discovers packages without exposing its provider protocol or credentials.
// Detail is used when the user pasted a specific link or identifier; Search handles other keywords.
type SkillSourceCatalog interface {
	// Reference recognizes a provider-specific pasted URL or ID without prescribing its shape.
	Reference(string) (string, bool)
	Detail(context.Context, string) (apimodel.MarketPackage, error)
	Search(context.Context, string, string, int, int) ([]apimodel.MarketPackage, int, error)
}

// MarkInstalled sets Installed by stable Origin.Identifier rather than by display name.
// A renamed upstream package is still the same installed reference.
func MarkInstalled(packages []apimodel.MarketPackage, installed map[string]struct{}) {
	for i := range packages {
		_, packages[i].Installed = installed[packages[i].Identifier]
	}
}

// InstalledSkillIdentifiers collects stable external references in one workspace query.
// Built-in and hand-written skills have no Origin and cannot collide with upstream identifiers.
func InstalledSkillIdentifiers(skills []apimodel.Skill) map[string]struct{} {
	installed := make(map[string]struct{}, len(skills))
	for _, skill := range skills {
		if skill.Origin != nil && skill.Origin.Identifier != "" {
			installed[skill.Origin.Identifier] = struct{}{}
		}
	}
	return installed
}

// SearchSkillSource marks installed references after each upstream search. A detail lookup
// returns exactly one item instead of incorrectly treating a pasted URL as a keyword search.
func SearchSkillSource(
	ctx context.Context, catalog SkillSourceCatalog, installed map[string]struct{},
	keyword, scope string, page, size int,
) ([]apimodel.MarketPackage, int, error) {
	if catalog == nil {
		return nil, 0, fmt.Errorf("%w: 技能来源目录未接入", persistmodel.ErrPrecondition)
	}
	if identifier, ok := catalog.Reference(keyword); ok {
		pkg, err := catalog.Detail(ctx, identifier)
		if err != nil {
			return nil, 0, err
		}
		MarkInstalled([]apimodel.MarketPackage{pkg}, installed)
		_, pkg.Installed = installed[pkg.Identifier]
		return []apimodel.MarketPackage{pkg}, 1, nil
	}
	items, total, err := catalog.Search(ctx, keyword, scope, page, size)
	if err != nil {
		return nil, 0, err
	}
	MarkInstalled(items, installed)
	return items, total, nil
}

// RefreshSkillSource performs one manual source refresh. A failed refresh records LastError
// through the injected callback without changing the last usable skill content.
func RefreshSkillSource(
	ctx context.Context, skill apimodel.Skill, hooks SkillUpdateHooks,
	recordError func(context.Context, apimodel.Skill, error),
) (apimodel.Skill, bool, error) {
	updated, changed, err := UpdateSkillFromSource(ctx, skill, hooks)
	if err != nil && recordError != nil {
		recordError(ctx, skill, err)
	}
	return updated, changed, err
}

// SetSkillAutoUpdate changes subscription preference only. It deliberately preserves content,
// UpdatedAt, LastCheckedAt and LastError: no upstream check happened during this write.
func SetSkillAutoUpdate(
	ctx context.Context, skill apimodel.Skill, enabled bool,
	save func(context.Context, apimodel.Skill) (apimodel.Skill, error),
) (apimodel.Skill, error) {
	if skill.Origin == nil {
		return apimodel.Skill{}, fmt.Errorf("%w: 这个技能不是从外部来源装来的，没有自动更新这回事", persistmodel.ErrPrecondition)
	}
	if skill.Origin.AutoUpdate == enabled {
		return skill, nil
	}
	origin := *skill.Origin
	origin.AutoUpdate = enabled
	skill.Origin = &origin
	if save == nil {
		return apimodel.Skill{}, fmt.Errorf("%w: 技能存储没有装配", persistmodel.ErrPrecondition)
	}
	return save(ctx, skill)
}
