package skill

import (
	"context"
	"errors"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/repository/dao/coremem"
)

type testBuiltins struct{ skill apimodel.Skill }

func (b testBuiltins) Skills() []apimodel.Skill { return []apimodel.Skill{b.skill} }
func (b testBuiltins) Skill(id string) (apimodel.Skill, bool) {
	if id == b.skill.ID {
		return b.skill, true
	}
	return apimodel.Skill{}, false
}
func (b testBuiltins) NameTaken(name string) bool { return name == b.skill.Name }
func (b testBuiltins) Files(string) []apimodel.SkillFile {
	return []apimodel.SkillFile{{Path: "references/example.md", Content: []byte("example"), Size: 7}}
}

type testSourcePolicy struct{}

func (testSourcePolicy) ReadOnlyReason(source apimodel.SkillSource) string {
	if source == "external-example" {
		return "来自外部示例源"
	}
	return ""
}

type testRevisions struct{}

func (testRevisions) Record(context.Context, apimodel.RevisionKind, string, string, string, any, apimodel.RevisionReason) {
}
func (testRevisions) Drop(context.Context, apimodel.RevisionKind, string) {}

func TestDownstreamBuiltinAndSourcePolicies(t *testing.T) {
	ctx := context.Background()
	store := coremem.NewMemStore()
	if _, err := store.CreateWorkspace(ctx, apimodel.Workspace{ID: "ws_test", Name: "Example", Key: "example"}); err != nil {
		t.Fatal(err)
	}
	builtin := apimodel.Skill{ID: "skill-builtin-example", Name: "example", Source: apimodel.SkillBuiltin}
	service, err := New(Config{
		Store: store, NextID: func(string) string { return "skill_test" }, Revisions: testRevisions{},
		Builtins: testBuiltins{skill: builtin}, Sources: testSourcePolicy{},
	})
	if err != nil {
		t.Fatal(err)
	}
	list, err := service.ListSkills(ctx, "ws_test")
	if err != nil || len(list) != 1 || list[0].ID != builtin.ID {
		t.Fatalf("downstream built-in catalog not used: %+v %v", list, err)
	}
	files, err := service.SkillFiles(ctx, builtin.ID)
	if err != nil || len(files) != 1 || string(files[0].Content) != "example" {
		t.Fatalf("downstream built-in files not used: %+v %v", files, err)
	}
	if _, err := store.CreateSkill(ctx, apimodel.Skill{
		ID: "skill_external", WorkspaceID: "ws_test", Name: "external", Source: "external-example", Tags: []string{},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateSkill(ctx, "skill_external", apimodel.SkillDraft{Name: "changed"}); !errors.Is(err, persistmodel.ErrPrecondition) {
		t.Fatalf("read-only downstream source must reject edits: %v", err)
	}
}
