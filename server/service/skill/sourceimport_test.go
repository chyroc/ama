package skill

import (
	"context"
	"errors"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

func TestScanSourceDirectoriesKeepsFailuresAndMarksInstalled(t *testing.T) {
	items, err := ScanSourceDirectories(context.Background(), []string{"good", "bad", "installed"}, 0,
		func(dir string) string { return "source:" + dir },
		map[string]struct{}{"source:installed": {}},
		func(_ context.Context, dir string) (apimodel.SkillPackage, int, error) {
			if dir == "bad" {
				return apimodel.SkillPackage{}, 0, errors.New("invalid frontmatter")
			}
			return apimodel.SkillPackage{Name: dir, Files: []apimodel.SkillFile{{Path: "example"}}}, 42, nil
		}, func(err error) (apimodel.ScannedSkillStatus, string) {
			return apimodel.ScanInvalid, err.Error()
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[0].Status != apimodel.ScanOK || items[1].Status != apimodel.ScanInvalid || items[2].Status != apimodel.ScanInstalled {
		t.Fatalf("unexpected scan results: %#v", items)
	}
}

func TestImportSourceDirectoriesContinuesAndSkipsDuplicates(t *testing.T) {
	created, failed, err := ImportSourceDirectories(context.Background(), []string{"one", "bad", "one"},
		func(dir string) string { return dir }, map[string]struct{}{},
		func(_ context.Context, dir string) (apimodel.Skill, error) {
			if dir == "bad" {
				return apimodel.Skill{}, errors.New("broken package")
			}
			return apimodel.Skill{Name: dir}, nil
		}, func(err error) (apimodel.ScannedSkillStatus, string) {
			return apimodel.ScanInvalid, err.Error()
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 || len(failed) != 2 || failed[0].Status != apimodel.ScanInvalid || failed[1].Status != apimodel.ScanInstalled {
		t.Fatalf("created=%#v failed=%#v", created, failed)
	}
}
