// The persistence contract of the skill domain.
//
// The declaration follows the domain instead of staying in core/store.go, for exactly the same
// reason as in the issue.Store file header: core/store.go is the shared file every resource domain
// has to go through, a dozen concurrent sessions pass through it, and keeping declarations there
// means every domain adding a method collides on the same hunk.
//
// # ⚠️ Neither the PG nor the in-memory implementation needs a single line changed
//
// Thanks to Go's implicit interfaces: both implementations (repository/dao/coredao, coremem) have
// the methods and satisfy this narrow interface automatically. **Do not add
// `var _ skill.Store = ...` assertions to them**: that would make dao import service/skill in
// reverse, gluing back the direction just split apart. The assertion at the wiring point
// (core.New) is enough; that place already sees both sides.
//
// # Methods overlapping with other domains are not a mistake
//
// GetWorkspace / ListWorkspaces / ListAgents / SaveAgent overlap with the workspace / agent domain
// Stores. Go allows methods with the same name and signature to coexist across embedded interfaces
// (the signatures must match exactly; changing one is a compile error). The reason for each:
//
//	GetWorkspace     confirm the workspace exists when creating a skill (not turned into 404; see CreateSkill)
//	ListWorkspaces   deleting a skill must remove agent bindings **across all workspaces** (see DeleteSkill)
//	ListAgents/SaveAgent	same as the previous one: removing bindings reads, changes and writes agent rows
//
// All of these are "reading another domain's table", not calling another domain's Service, so they
// do not break "zero imports between domain packages".
package skill

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// Store is every persistence method this domain needs, **listing only the ones needed**.
type Store interface {
	// -- Tables owned by others (see the file header for why) -----------------------------------------

	ListWorkspaces(ctx context.Context) ([]apimodel.Workspace, error)
	GetWorkspace(ctx context.Context, id string) (apimodel.Workspace, error)
	ListAgents(ctx context.Context, workspaceID string) ([]apimodel.Agent, error)
	SaveAgent(ctx context.Context, a apimodel.Agent) (apimodel.Agent, error)

	// -- The skills table ------------------------------------------------------

	ListSkills(ctx context.Context, workspaceID string) ([]apimodel.Skill, error)
	GetSkill(ctx context.Context, id string) (apimodel.Skill, error)
	SkillNameTaken(ctx context.Context, workspaceID, name string) (bool, error)
	CreateSkill(ctx context.Context, s apimodel.Skill) (apimodel.Skill, error)
	SaveSkill(ctx context.Context, s apimodel.Skill) (apimodel.Skill, error)
	DeleteSkill(ctx context.Context, id string) error

	// -- skill_files and files (attachment bytes in object storage, metadata in the files table) --------

	PutSkillFiles(ctx context.Context, skillID, workspaceID string, files []apimodel.SkillFile) error
	SkillFiles(ctx context.Context, skillID string) ([]apimodel.SkillFile, error)
	CreateFile(ctx context.Context, rec persistmodel.FileRecord) error
	GetFiles(ctx context.Context, ids []string) (map[string]persistmodel.FileRecord, error)
}
