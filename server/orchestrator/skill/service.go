// Orchestrator layer: skill create/read/update/delete, upload parsing and storing, file
// read/write, console links, and fork.

package skill

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Service is the set of methods the handler actually uses in this domain. **Only the ones
// it uses are listed.**
//
// This is a "narrow interface from the consumer's side": new methods on the domain package
// are invisible to the handler by default; to expose one, you must add a line to this file
// explicitly. What this saves is the empty forwarding methods: an interface only needs
// signatures, not function bodies.
//
// ⚠️ **The name is just Service; the domain is carried by the package name**
// (`skill.Service` / `issue.Service`). Before, these interfaces were packed into the same
// package orchestrator and could only be told apart by prefix (SkillService /
// IssueService); after splitting into orchestrator/<domain>/ that prefix became
// repetition, since the package name already says which domain it is.
//
// ⚠️ The package name only guards **between domain interface declarations**: to use a
// sibling domain you must import it yourself, and the new import is visible in code review.
// **Blocking cross-domain access on the handler side does not rely on it**: that is done by
// "the field type is a narrow interface"; see the comments on the ten Handler fields in
// apihandler/handler.go.
//
// ⚠️ **Signatures always use apimodel.X, never core.X.** This is not style, it is a hard
// rule: if this package imports core, core shows up in `go list -deps ./orchestrator/...`,
// the orchestrator layer depends again on the package it is supposed to hide, and the layer
// is pointless (full reasoning in the file header of orchestrator/routes.go).
//
// # Where these 16 methods came from
//
// The first 9 (core skill features) used to sit on the mixed-domain workspace.Service and
// were provided by core through workspace.Fallback; the last 7 (market / import) used to be
// a separate market domain package. The two were merged into one skill domain package
// (splitting them only cut "the skill itself" apart from "where the skill comes from").
//
// **All 16 are satisfied directly by *skillsvc.Service** (the step that split out
// arkconsole moved the last one, GetSkillArkConsole, into the domain package too; its
// dependency is injected as a narrow Ark interface, and the real one is in the adapter in
// core/arkconsole.go). One source, no Fallback; for how the domain package is held, see
// composite.go in this package.
type Service interface {
	CreateSkill(ctx context.Context, workspaceID string, draft apimodel.SkillDraft) (apimodel.Skill, error)
	CreateSkillFromPackage(
		ctx context.Context, workspaceID string, pkg apimodel.SkillPackage, opts apimodel.SkillCreateOptions,
	) (apimodel.Skill, error)
	DeleteSkill(ctx context.Context, id string) error
	ForkSkill(
		ctx context.Context, workspaceID, sourceID string,
	) (apimodel.Skill, error)
	GetSkill(ctx context.Context, id string) (apimodel.Skill, error)
	GetSkillArkConsole(ctx context.Context, skillID string) (apimodel.ArkConsoleRef, error)
	ListSkills(ctx context.Context, workspaceID string) ([]apimodel.Skill, error)
	SkillFileContent(
		ctx context.Context, skillID, filePath string,
	) (apimodel.SkillFileContent, error)
	UpdateSkill(ctx context.Context, id string, draft apimodel.SkillDraft) (apimodel.Skill, error)
	UpdateSkillFile(
		ctx context.Context, skillID, filePath string, content []byte,
	) (apimodel.Skill, error)
}
