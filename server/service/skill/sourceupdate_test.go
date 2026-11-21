package skill

import (
	"context"
	"errors"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
)

func TestUpdateSkillFromSourcePreservesNameAndWritesFilesFirst(t *testing.T) {
	skill := apimodel.Skill{
		ID: "skill_example", Name: "installed-name", DisplayTitle: "old title",
		Origin: &apimodel.SkillOrigin{Cksum: "old-checksum", Version: "v1"},
	}
	events := []string{}
	fresh, changed, err := UpdateSkillFromSource(context.Background(), skill, SkillUpdateHooks{
		Load: func(context.Context, apimodel.Skill) (SourcePackage, error) {
			return SourcePackage{Version: "v2", Cksum: "new-checksum", Package: apimodel.SkillPackage{
				Name: "upstream-renamed", Instructions: "new instructions",
				Files: []apimodel.SkillFile{{Path: "scripts/example.sh", Content: []byte("new")}},
			}}, nil
		},
		PutFiles: func(_ context.Context, got apimodel.Skill, files []apimodel.SkillFile) error {
			events = append(events, "files")
			if got.Name != "installed-name" || len(files) != 1 {
				t.Fatalf("wrong package for file write: skill=%#v files=%#v", got, files)
			}
			return nil
		},
		Save: func(_ context.Context, got apimodel.Skill) (apimodel.Skill, error) {
			events = append(events, "save")
			return got, nil
		},
		NowISO: func() string { return "2026-01-01T00:00:00.000Z" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !changed || fresh.Name != "installed-name" || fresh.Origin.Version != "v2" || len(events) != 2 || events[0] != "files" || events[1] != "save" {
		t.Fatalf("unexpected update: skill=%#v changed=%v events=%v", fresh, changed, events)
	}
}

func TestUpdateSkillFromSourceUnchangedSkipsFileWrite(t *testing.T) {
	skill := apimodel.Skill{Origin: &apimodel.SkillOrigin{Cksum: "same", Version: "v1"}}
	_, changed, err := UpdateSkillFromSource(context.Background(), skill, SkillUpdateHooks{
		Load: func(context.Context, apimodel.Skill) (SourcePackage, error) {
			return SourcePackage{Version: "v2", Cksum: "same"}, nil
		},
		Save: func(_ context.Context, skill apimodel.Skill) (apimodel.Skill, error) { return skill, nil },
		PutFiles: func(context.Context, apimodel.Skill, []apimodel.SkillFile) error {
			t.Fatal("unchanged content must not rewrite files")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("same checksum must not be reported as a content update")
	}
}

func TestUpdateSkillFromSourceFailureDoesNotSaveMetadata(t *testing.T) {
	want := errors.New("object storage unavailable")
	_, changed, err := UpdateSkillFromSource(context.Background(), apimodel.Skill{Origin: &apimodel.SkillOrigin{}}, SkillUpdateHooks{
		Load: func(context.Context, apimodel.Skill) (SourcePackage, error) {
			return SourcePackage{Cksum: "new", Package: apimodel.SkillPackage{Instructions: "new"}}, nil
		},
		PutFiles: func(context.Context, apimodel.Skill, []apimodel.SkillFile) error { return want },
		Save: func(context.Context, apimodel.Skill) (apimodel.Skill, error) {
			t.Fatal("failed file write must not save new metadata")
			return apimodel.Skill{}, nil
		},
	})
	if !errors.Is(err, want) || changed {
		t.Fatalf("changed=%v err=%v, want uncommitted file failure", changed, err)
	}
}

func TestRecordSkillSourceErrorKeepsContent(t *testing.T) {
	skill := apimodel.Skill{Instructions: "last usable", Origin: &apimodel.SkillOrigin{Cksum: "good"}}
	got := RecordSkillSourceError(skill, errors.New("upstream denied"), "2026-01-01T00:00:00.000Z")
	if got.Instructions != skill.Instructions || got.Origin.Cksum != "good" || got.Origin.LastError != "upstream denied" {
		t.Fatalf("failure changed last usable content: %#v", got)
	}
}
