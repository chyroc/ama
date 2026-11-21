package skill

import (
	"context"
	"errors"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

func TestApplySkillSourceUpdateRejectsUnknownSource(t *testing.T) {
	_, _, err := ApplySkillSourceUpdate(context.Background(), apimodel.Skill{
		Origin: &apimodel.SkillOrigin{Kind: "unregistered"},
	}, SkillSourceUpdaters{})
	if !errors.Is(err, persistmodel.ErrPrecondition) {
		t.Fatalf("unknown source error = %v", err)
	}
}

func TestApplySkillSourceUpdateUsesRegisteredSource(t *testing.T) {
	want := apimodel.Skill{ID: "skill_example"}
	got, changed, err := ApplySkillSourceUpdate(context.Background(), apimodel.Skill{
		Origin: &apimodel.SkillOrigin{Kind: "example"},
	}, SkillSourceUpdaters{
		"example": func(context.Context, apimodel.Skill) (apimodel.Skill, bool, error) { return want, true, nil },
	})
	if err != nil || !changed || got.ID != want.ID {
		t.Fatalf("got=%#v changed=%v err=%v", got, changed, err)
	}
}
