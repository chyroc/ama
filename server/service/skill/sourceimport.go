package skill

import (
	"context"
	"log/slog"

	"github.com/chyroc/ama/server/model/apimodel"
)

const DefaultSourceScanLimit = 200

// ScanSourceDirectories evaluates each directory independently and reports all results,
// including failures. The callback owns provider authentication and error classification;
// LimitDir supplies an existing persisted reference for duplicate detection.
func ScanSourceDirectories(
	ctx context.Context, dirs []string, limit int,
	identifier func(string) string, installed map[string]struct{},
	read func(context.Context, string) (apimodel.SkillPackage, int, error),
	classify func(error) (apimodel.ScannedSkillStatus, string),
	logger *slog.Logger,
) ([]apimodel.ScannedSkill, error) {
	if limit <= 0 {
		limit = DefaultSourceScanLimit
	}
	if len(dirs) > limit {
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("技能目录超过单次扫描上限，只扫前面一批",
			slog.Int("total", len(dirs)), slog.Int("扫描", limit))
		dirs = dirs[:limit]
	}
	items := make([]apimodel.ScannedSkill, 0, len(dirs))
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item := apimodel.ScannedSkill{Dir: dir}
		pkg, bytes, err := read(ctx, dir)
		if err != nil {
			if classify == nil {
				return nil, err
			}
			item.Status, item.Reason = classify(err)
			items = append(items, item)
			continue
		}
		item.Name = pkg.Name
		item.DisplayTitle = pkg.DisplayTitle
		item.Description = pkg.Description
		item.Files = len(pkg.Files)
		item.Bytes = bytes
		item.Status = apimodel.ScanOK
		if identifier != nil {
			if _, duplicate := installed[identifier(dir)]; duplicate {
				item.Status = apimodel.ScanInstalled
			}
		}
		items = append(items, item)
	}
	return items, nil
}

// ImportSourceDirectories imports each selected item independently. A duplicate reference
// is marked installed instead of triggering a second remote read or an opaque name collision.
// The implementation must let its persistent unique constraint decide concurrent new writes.
func ImportSourceDirectories(
	ctx context.Context, dirs []string,
	identifier func(string) string, installed map[string]struct{},
	importOne func(context.Context, string) (apimodel.Skill, error),
	classify func(error) (apimodel.ScannedSkillStatus, string),
) ([]apimodel.Skill, []apimodel.ScannedSkill, error) {
	created := make([]apimodel.Skill, 0, len(dirs))
	failed := make([]apimodel.ScannedSkill, 0)
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return created, failed, err
		}
		var reference string
		if identifier != nil {
			reference = identifier(dir)
			if _, duplicate := installed[reference]; duplicate {
				failed = append(failed, apimodel.ScannedSkill{Dir: dir, Status: apimodel.ScanInstalled, Reason: "这个技能已经导入过了"})
				continue
			}
		}
		skill, err := importOne(ctx, dir)
		if err != nil {
			if classify == nil {
				return created, failed, err
			}
			status, reason := classify(err)
			failed = append(failed, apimodel.ScannedSkill{Dir: dir, Status: status, Reason: reason})
			continue
		}
		created = append(created, skill)
		if reference != "" {
			installed[reference] = struct{}{}
		}
	}
	return created, failed, nil
}
