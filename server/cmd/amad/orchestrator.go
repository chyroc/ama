// Wiring and static assertions for the orchestration layer.
//
// # Why the wiring lives here
//
// It needs to see two sides at once: the nine domain packages (implementations) and orchestrator (the contract). The orchestration layer
// **cannot** import core, and core should not know who orchestrates it; only the wiring layer sees
// both sides. The same reason applies to authzMembership in this package, which adapts core.Service to authz.Membership
// (core deliberately does not import authz, see the top of core/resource.go).
//
// # ⚠️ "Can the orchestration layer import core" was re-tested: it really is a cycle now
//
// The two previous versions of this comment said "cycle" once and "tested, not true" once. **The current fact is that it is a cycle**:
// core/handler.go has the line `"github.com/chyroc/ama/server/orchestrator"`
// (`type Mux = orchestrator.Mux`), so if you add an import of core in orchestrator
// and build, you get `import cycle not allowed`.
//
// But **the main check is still the machine-checkable one**: `go list -deps ./orchestrator` must not contain
// core, because whether there is a cycle quietly flips between true and false with changes on the core side (it already flipped twice).
// The cycle is just one extra hard wall for now: it makes "the methods not yet moved out of core" fall back only
// through interfaces (orchestrator.Fallback); they cannot take *core.Service directly.
//
// # What this wiring guards against
//
// The ten interfaces of the orchestration layer are **hand-written lists**, and both the domain packages and core.Service satisfy them implicitly.
// If one day a domain changes a method signature (say, adds a parameter to ListIssues), the compile error
// appears at the orchestrator.New call site, and it names exactly which method does not match.
//
// ⚠️ How each of the four domain packages satisfies its interface is still asserted separately inside the orchestrator package (errors there are more precise);
// here we only cover "the fallback part" and "the composite can be put together".

package main

import (
	"github.com/chyroc/ama/server/apihandler"
	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/orchestrator"
)

// newOrchestrator assembles the nine domain packages and the core fallback into the orchestration layer.
//
// ⚠️ **The nine domain packages are taken from coreSvc**, not created here: half of their dependencies are still
// on the core side (issuing ids, getting the current operator, backfilling Ark events, scheduling syncs…), which the wiring cannot build.
// For those nine accessors and why they exist, see the file header of core/domainaccess.go.
//
// Each time a domain is moved out of core, its line here changes from `coreSvc.Xxx()` to a locally built instance,
// and the matching method in core/domainaccess.go is deleted.
func newOrchestrator(coreSvc *core.Service) (*orchestrator.Orchestrator, error) {
	return orchestrator.New(orchestrator.Config{
		// Four domains are fully satisfied by domain packages.
		Issue:      coreSvc.Issues(),
		Suggestion: coreSvc.Suggestions(),
		Revision:   coreSvc.Revisions(),
		ArkCred:    coreSvc.ArkCreds(),

		// The three domain packages Workspace/Schedule/Run make up the 24 methods of WorkspaceService,
		// and Agent alone makes up the 6 of AgentService; each domain's fallback (23 + 3) goes through Fallback.
		Workspace: coreSvc.Workspaces(),
		Agent:     coreSvc.Agents(),
		Schedule:  coreSvc.Schedules(),
		Run:       coreSvc.Runs(),

		// All 16 methods of the Skill domain come from service/skill (moved out of the main trunk in an earlier step;
		// the arkconsole domain split added the console jump).
		Skill: coreSvc.Skills(),

		// Fallback: the sandbox / devblob / admin domains are still entirely in core, plus
		// those 22 of WorkspaceService and 2 of AgentService.
		Fallback: coreSvc,
	})
}

// *core.Service must satisfy the orchestration layer's fallback interface.
//
// ⚠️ When this line fails to compile, look at the method named in the error: either core changed the signature, or
// that line in orchestrator/<domain>/service.go was copied wrong, or that method **has actually been moved into a domain package**
// (then the right fix is to delete it from orchestrator.Fallback, not to add
// a forwarding method in core to hook it back up).
//
// **Do not loosen the interface just to make it pass**: the interface list is the only record of "what handlers can call".
var _ orchestrator.Fallback = (*core.Service)(nil)

// *apihandler.Handler must satisfy the orchestration layer's route interface; httpd.Config.CoreHandler takes exactly that.
//
// The wiring point (httpd.New(...CoreHandler: coreHandler)) also checks the same thing,
// but the error there is buried in a struct literal with a dozen fields and does not point to the specific method that is wrong.
// This line pulls it out and reports it separately.
//
// ⚠️ If the error looks like "have Register(core.Mux) want Register(orchestrator.Mux)",
// check whether **the equals sign is still there** in the line `type Mux = orchestrator.Mux` in core/handler.go:
// without the equals sign it becomes a new type, and the method sets no longer match. The two type names print almost the same,
// so the error reads as if the compiler is talking nonsense.
var _ orchestrator.Routes = (*apihandler.Handler)(nil)
