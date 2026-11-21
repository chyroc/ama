package skill

import (
	"context"
	"errors"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

func TestInstalledSkillIdentifiersAndMarkInstalled(t *testing.T) {
	installed := InstalledSkillIdentifiers([]apimodel.Skill{
		{Origin: &apimodel.SkillOrigin{Identifier: "same-source"}},
		{Name: "hand-written"},
		{Origin: &apimodel.SkillOrigin{}},
	})
	packages := []apimodel.MarketPackage{{Identifier: "same-source"}, {Identifier: "other"}}
	MarkInstalled(packages, installed)
	if !packages[0].Installed || packages[1].Installed {
		t.Fatalf("installed markers = %#v", packages)
	}
}

func TestSetSkillAutoUpdateChangesOnlyPreference(t *testing.T) {
	skill := apimodel.Skill{
		Name: "installed", LatestVersion: "v1", UpdatedAt: "unchanged",
		Origin: &apimodel.SkillOrigin{AutoUpdate: false, LastCheckedAt: "last check", LastError: "last error"},
	}
	updated, err := SetSkillAutoUpdate(context.Background(), skill, true,
		func(_ context.Context, got apimodel.Skill) (apimodel.Skill, error) { return got, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Origin.AutoUpdate || updated.UpdatedAt != skill.UpdatedAt ||
		updated.Origin.LastCheckedAt != "last check" || updated.Origin.LastError != "last error" {
		t.Fatalf("preference write changed content or check history: %#v", updated)
	}
	called := false
	if _, err := SetSkillAutoUpdate(context.Background(), skill, false,
		func(context.Context, apimodel.Skill) (apimodel.Skill, error) {
			called = true
			return skill, nil
		}); err != nil || called {
		t.Fatalf("unchanged preference must not write: called=%v err=%v", called, err)
	}
	if _, err := SetSkillAutoUpdate(context.Background(), apimodel.Skill{}, true, nil); !errors.Is(err, persistmodel.ErrPrecondition) {
		t.Fatalf("custom skill auto-update error = %v", err)
	}
}

func TestRefreshSkillSourceRecordsErrorWithoutChangingContent(t *testing.T) {
	skill := apimodel.Skill{Instructions: "usable", Origin: &apimodel.SkillOrigin{}}
	want := errors.New("upstream failed")
	recorded := false
	_, _, err := RefreshSkillSource(context.Background(), skill, SkillUpdateHooks{
		Load:     func(context.Context, apimodel.Skill) (SourcePackage, error) { return SourcePackage{}, want },
		Save:     func(context.Context, apimodel.Skill) (apimodel.Skill, error) { return skill, nil },
		PutFiles: func(context.Context, apimodel.Skill, []apimodel.SkillFile) error { return nil },
	}, func(_ context.Context, got apimodel.Skill, gotErr error) {
		recorded = true
		if got.Instructions != "usable" || !errors.Is(gotErr, want) {
			t.Fatalf("recorded wrong failure: skill=%#v err=%v", got, gotErr)
		}
	})
	if !errors.Is(err, want) || !recorded {
		t.Fatalf("refresh error = %v recorded=%v", err, recorded)
	}
}
