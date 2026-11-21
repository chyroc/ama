package skill

import (
	"context"
	"fmt"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// SourcePackage is an upstream skill version and its already validated content.
// Parse/checksum decisions made by the provider remain outside this update algorithm.
type SourcePackage struct {
	Version string
	// DisplayVersion may be a short commit or another presentation form. Empty uses Version.
	DisplayVersion string
	Cksum          string
	Package        apimodel.SkillPackage
}

// SkillUpdateHooks supplies the private upstream and persistent side effects. PutFiles
// receives every changed package, including zero files, so upstream-deleted files are removed.
type SkillUpdateHooks struct {
	Load     func(context.Context, apimodel.Skill) (SourcePackage, error)
	Save     func(context.Context, apimodel.Skill) (apimodel.Skill, error)
	PutFiles func(context.Context, apimodel.Skill, []apimodel.SkillFile) error
	NowISO   func() string
}

// UpdateSkillFromSource preserves the installed skill name and all unrelated fields.
// It compares content checksums, not version strings. A changed package writes files before
// metadata, so readers never see a new file list pointing at bytes that do not yet exist.
// If PutFiles or Save fails, the returned skill is empty and changed is false; persistent
// implementations must leave the previous metadata intact and support retrying the update.
func UpdateSkillFromSource(
	ctx context.Context, skill apimodel.Skill, hooks SkillUpdateHooks,
) (apimodel.Skill, bool, error) {
	var zero apimodel.Skill
	if skill.Origin == nil {
		return zero, false, fmt.Errorf("%w: 这个技能不是从外部来源装来的，没有可更新的来源", persistmodel.ErrPrecondition)
	}
	if hooks.Load == nil || hooks.Save == nil || hooks.PutFiles == nil {
		return zero, false, fmt.Errorf("%w: 技能更新存储没有完整装配", persistmodel.ErrPrecondition)
	}
	if hooks.NowISO == nil {
		hooks.NowISO = apimodel.NowISO
	}
	fresh, err := hooks.Load(ctx, skill)
	if err != nil {
		return zero, false, err
	}
	origin := *skill.Origin
	origin.LastCheckedAt = hooks.NowISO()
	origin.LastError = ""
	if fresh.Cksum != "" && fresh.Cksum == origin.Cksum {
		if fresh.Version != "" {
			origin.Version = fresh.Version
		}
		skill.Origin = &origin
		saved, err := hooks.Save(ctx, skill)
		return saved, false, err
	}

	skill.DisplayTitle = firstNonEmpty(fresh.Package.DisplayTitle, skill.DisplayTitle)
	skill.Description = firstNonEmpty(fresh.Package.Description, skill.Description)
	skill.Instructions = fresh.Package.Instructions
	skill.LatestVersion = fresh.DisplayVersion
	if skill.LatestVersion == "" {
		skill.LatestVersion = fresh.Version
	}
	skill.UpdatedAt = hooks.NowISO()
	files := make([]apimodel.SkillFile, 0, len(fresh.Package.Files))
	for _, file := range fresh.Package.Files {
		files = append(files, apimodel.SkillFile{Path: file.Path, Size: len(file.Content)})
	}
	persistmodel.SortSkillFiles(files)
	skill.Files = nil
	if len(files) > 0 {
		skill.Files = files
	}
	origin.Cksum = fresh.Cksum
	if fresh.Version != "" {
		origin.Version = fresh.Version
	}
	skill.Origin = &origin
	if err := hooks.PutFiles(ctx, skill, fresh.Package.Files); err != nil {
		return zero, false, err
	}
	saved, err := hooks.Save(ctx, skill)
	if err != nil {
		return zero, false, err
	}
	return saved, true, nil
}

// RecordSkillSourceError changes only the origin's check timestamp and diagnostic.
// It never changes the last usable skill content.
func RecordSkillSourceError(skill apimodel.Skill, cause error, now string) apimodel.Skill {
	if skill.Origin == nil || cause == nil {
		return skill
	}
	origin := *skill.Origin
	origin.LastCheckedAt = now
	origin.LastError = cause.Error()
	skill.Origin = &origin
	return skill
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
