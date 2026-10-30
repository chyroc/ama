// The orchestration layer: the only entry for handlers.
//
// # What this step did
//
// Step 14 of the package split plan. **This package now directly holds the Services of nine domain packages**, instead of wrapping just one
// core.Service: the dependency direction was turned from "domain packages hang under core" into "domain packages hang under the orchestration layer":
//
//	before   httpd → orchestrator → core.Service (core → service/*, 12 domain packages)
//	after    httpd → orchestrator → service/<domain>; only what cannot be reached falls back to core
//
// The check is the machine-checkable one: `go list -deps ./orchestrator/... | grep -c service/`
// went from 0 to 9 (it was 8 at step 14; skill grew a Service in a later step).
//
// The value is not in what it does now, but in that **the boundary now has a compile-time hold**: what handlers can call
// is decided by the ten interface lists in orchestrator/<domain>/service.go, not by "whatever public methods core.Service has".
// core.Service has 254 methods (161 exported), and handlers really use 133 of them:
// the rest are invisible to handlers from now on.
//
// # ⚠️ Why "nine domains + one core fallback", not all twelve domains
//
// Of the twelve domain packages **only nine have a `*Service` that handlers call**: issue / suggestion /
// revision / schedule / workspace / arkcred / run / agent / skill (skill's
// Service grew in a later step; before that it only had pure functions for parsing and built-in skills). The remaining three
// moved out other things, and none of their methods is among those 133:
//
//	arkrun       Runner interface + LiveRunner (Ark calls; handlers do not touch them directly)
//	arkbackoff   Ladder / Gate (worker backoff and rate limiting)
//	integration  seven providers and the registry; the logic of the three that store data is still in core
//
// Of the 133 methods, 38 can still only be satisfied by core.Service (chat / worker / sandbox /
// preferences / feedback / usage); they **deliberately keep going through core**.
// The mixed state is the expected result of this step, not unfinished work.
//
// # How one interface mixes two sources: Go's selector depth, not hand-written forwarding
//
// The narrow interfaces are **the handlers' view**; their shape is right, and this step did not change a line. What changed is "who
// satisfies them". The hard part is the two that cross sources: of workspace.Service's 47 methods, 25 belong to
// three domain packages (workspace/schedule/run) and 22 are still in core; of agent.Service's 9,
// 7 come from service/agent and 2 are still in core. And an interface can only be satisfied as a whole or not at all.
//
// The solution is the two composite structs in orchestrator/{workspace,agent}:
// embed the domain packages and the core fallback together, and let Go's selector resolution rules pick the implementation, instead of hand-writing
// 24 forwarders.
//
// ⚠️ **A method name appearing twice at the same depth is ambiguous**, and Go reports
// "ambiguous selector" **at the call site**, not at the struct definition. So the fallback interfaces
// (workspace.Fallback / agent.Fallback) must **remove one by one** the 25 + 7 methods the domain packages
// already provide, leaving 22 and 2. Removing too few or too many is
// a compile error, caught on the spot by the static assertions over there
// (`var _ Service = (*composite)(nil)`), so it never silently goes to the wrong implementation.
//
// ⚠️ On the workspace side the three domain packages' types are **all named `Service`**, so only one can be embedded
// anonymously; the other two are named fields + 10 explicit forwarders, the only forwarding code of this composite;
// the reasons and trade-offs are in the file header of orchestrator/workspace/composite.go. The agent side
// has only two sources whose embedded names do not clash, so no forwarder is needed.
//
// # Why interfaces instead of empty forwarding shells
//
// Hand-writing 161 `func (o *Orchestrator) X(...) { return o.core.X(...) }` would surely rot
// (changing a signature means changing two places, and missing one reports no error; that method just cannot be called), and code generation does not
// fit this repo either (the main branch changes very often, and generated files attract conflicts).
//
// An interface needs only signatures, not function bodies, which saves exactly those empty shells. core.Service satisfies them implicitly,
// with zero changes.
//
// ⚠️ **Embedding *core.Service does not meet this constraint**: handlers would then get all 254 methods,
// and "what is public" would degrade into a convention nobody enforces.
//
// # One package per domain
//
// The conflict surface is cut by domain, copying the lesson at the top of core/store_issue.go: a domain's interface declarations follow the domain
// and do not stay in a shared file, otherwise it is "as good as not moved": adding a method to issue still means changing a
// shared file every domain passes through, and two sessions each changing their own domain still collide in the same diff hunk.
//
// ⚠️ **Step 17 turned these nine files into nine packages** (orchestrator/<domain>/). What it blocks is
// **between domain interface declarations**: the nine interfaces used to be in the same package, so ark.go could write IssueService directly,
// or even mix methods of two domains into one interface, with no error; now orchestrator/ark cannot see
// orchestrator/issue, and to reference it must import it, and code review sees that import.
//
// ⚠️ **It added no new wall on the handler side; do not describe it as if it did.** What stops handlers from calling across domains
// has always been "the field type is a narrow interface" (set up in step 16), unrelated to how packages are split: `h.issue` is reachable
// from any file in package apihandler, and import has nothing to do with field access. Splitting the packages only changed
// the type name in error messages (`orchestrator.IssueService` → `issue.Service`).
//
// The split was possible because the previous step removed the all-in-one Service. While that union of nine domains existed,
// splitting into subpackages would have let handlers reach the exported types of the nine subpackages indirectly through it, **making the exposure bigger, not smaller**;
// once the union was gone and handlers held nine separate fields per domain, giving each domain its own package was pure narrowing.
//
// # ⚠️ This package must not import core
//
// The HTTP handlers still live in package core (core/handler.go, a very large file).
// If this package imported core, core would show up again in `go list -deps ./orchestrator/...`:
// the orchestration layer would depend again on the package it is supposed to hide, and this layer would be pointless.
//
// ⚠️ **This step re-tested "is there a cycle", with the opposite conclusion from step 1: it really is a cycle now.**
// Step 1's comment said "tested, not true; both directions compile", which was right at the time,
// but core/handler.go later added the import `"github.com/chyroc/ama/server/orchestrator"`
// (`type Mux = orchestrator.Mux`), which closed the loop. Add a line
// `import ".../core"` to this package and build, and you get:
//
//	imports .../core from zz_probe.go
//	imports .../orchestrator from handler.go: import cycle not allowed
//
// This is exactly what that comment predicted: "the check quietly flips between true and false with changes on the core side", so
// **the main check is still the machine-checkable one**: whether core is in `go list -deps ./orchestrator/...`.
// The cycle is just one extra hard wall for now: it makes the "fallback" accepted only as an interface
// (see Fallback below), not as *core.Service directly.
//
// The opposite direction is allowed and in use: core imports this package (`type Mux = orchestrator.Mux`).
//
// So types in interface signatures are always written as apimodel.X. Step 1 already moved the
// 40 plain data types needed by handler signatures from core into model/apimodel,
// with type aliases left on the core side, so existing call sites did not change a line (see core/aliasmodel.go).
//
// ✅ **The handlers have already moved out of core** (step 15; now server/apihandler),
// so the "cycle" wall no longer exists: apihandler → orchestrator is one-way.
// But this package **still must not import core**; the check goes back to the machine-checkable one:
// `go list -deps ./orchestrator/... | grep -c 'ama/server/core$'` must be 0.
// Fallback still takes *core.Service as an interface; the reason changed from "the compiler does not allow it" to
// "with that edge, the orchestration layer would depend again on the package it is supposed to hide".

package orchestrator

import (
	"errors"

	"github.com/chyroc/ama/server/orchestrator/agent"
	"github.com/chyroc/ama/server/orchestrator/ark"
	"github.com/chyroc/ama/server/orchestrator/issue"
	"github.com/chyroc/ama/server/orchestrator/revision"
	"github.com/chyroc/ama/server/orchestrator/sandbox"
	"github.com/chyroc/ama/server/orchestrator/skill"
	"github.com/chyroc/ama/server/orchestrator/suggestion"
	"github.com/chyroc/ama/server/orchestrator/workspace"
	agentsvc "github.com/chyroc/ama/server/service/agent"
	arkcredsvc "github.com/chyroc/ama/server/service/arkcred"
	issuesvc "github.com/chyroc/ama/server/service/issue"
	revisionsvc "github.com/chyroc/ama/server/service/revision"
	runsvc "github.com/chyroc/ama/server/service/run"
	schedulesvc "github.com/chyroc/ama/server/service/schedule"
	skillsvc "github.com/chyroc/ama/server/service/skill"
	suggestionsvc "github.com/chyroc/ama/server/service/suggestion"
	workspacesvc "github.com/chyroc/ama/server/service/workspace"
)

// Orchestrator is the only entry for handlers.
//
// All ten fields are interfaces, so handlers get **only the methods in that domain's list**.
// Who satisfies them is decided by New: five domains are fully satisfied by domain packages (issue / suggestion /
// revision / ark / skill), workspace and agent are combinations of "domain packages + core fallback"
// (workspace built from three domain packages, agent from one), and the remaining
// sandbox logic is still in core.
type Orchestrator struct {
	workspace  workspace.Service
	agent      agent.Service
	skill      skill.Service
	ark        ark.Service
	issue      issue.Service
	suggestion suggestion.Service
	sandbox    sandbox.Service
	revision   revision.Service
}

// Config is what is needed to build an orchestration layer: nine domain packages, plus a fallback for "the ones that cannot be reached".
//
// ⚠️ **The field types are deliberately the concrete `*xxxsvc.Service`, not interfaces.** The job of the orchestration layer
// is to know what it orchestrates; taking interfaces would degrade "is this layer really connected to the domain packages" into something
// only known at runtime, which is exactly what this step pins at compile time.
//
// The only interface taken is Fallback: its real type is *core.Service, and this package cannot import core
// (cycle; see the file header).
type Config struct {
	// Nine domain packages. All required; New checks each one.
	Issue      *issuesvc.Service
	Suggestion *suggestionsvc.Service
	Revision   *revisionsvc.Service
	ArkCred    *arkcredsvc.Service
	Workspace  *workspacesvc.Service
	Agent      *agentsvc.Service
	Schedule   *schedulesvc.Service
	Run        *runsvc.Service

	// Skill is the skill domain's package; all 16 methods are satisfied directly by it (after the arkconsole domain split
	// even the console jump is in the domain package), used the same way as the four above.
	Skill *skillsvc.Service

	// Fallback covers the 38 of the 133 methods not yet moved out of core. Required.
	//
	// Its real type is *core.Service. **It is also the provider of the 22 "still through core" methods of workspace.Service
	// and the 2 of agent.Service** (yielding to the domain packages by nesting depth;
	// see the file header).
	Fallback Fallback
}

// Fallback is the source of "the methods not yet moved out of core".
//
// It is deliberately declared as an interface instead of taking *core.Service directly: that would make this package import core,
// a cycle (see the file header). *core.Service satisfies it implicitly, with zero changes.
//
// ⚠️ This interface **shrinks** as later steps move domains out of core, and eventually disappears entirely.
// Shrinking is the sign of progress; do not add methods to it just to "change one fewer place".
type Fallback interface {
	workspace.Fallback
	agent.Fallback
	sandbox.Service
}

// New builds an orchestration layer.
//
// ⚠️ None of Config's ten fields (nine domain packages + fallback) may be nil.
// A missing one shows as **a panic on the request path**
// (it blows up when a handler calls into that domain), not at startup, so each is checked here, and a wiring miss
// is reported at startup, naming which one.
func New(cfg Config) (*Orchestrator, error) {
	switch {
	case cfg.Issue == nil:
		return nil, errors.New("orchestrator: Issue 不能为 nil")
	case cfg.Suggestion == nil:
		return nil, errors.New("orchestrator: Suggestion 不能为 nil")
	case cfg.Revision == nil:
		return nil, errors.New("orchestrator: Revision 不能为 nil")
	case cfg.ArkCred == nil:
		return nil, errors.New("orchestrator: ArkCred 不能为 nil")
	case cfg.Workspace == nil:
		return nil, errors.New("orchestrator: Workspace 不能为 nil")
	case cfg.Agent == nil:
		return nil, errors.New("orchestrator: Agent 不能为 nil")
	case cfg.Schedule == nil:
		return nil, errors.New("orchestrator: Schedule 不能为 nil")
	case cfg.Run == nil:
		return nil, errors.New("orchestrator: Run 不能为 nil")
	case cfg.Skill == nil:
		return nil, errors.New("orchestrator: Skill 不能为 nil")
	case cfg.Fallback == nil:
		return nil, errors.New("orchestrator: Fallback 不能为 nil")
	}
	return &Orchestrator{
		// The five single-source domains: fully satisfied by the service layer's domain packages. **How they are held and satisfied**
		// is decided by each domain package's own composite.go (holding the service/<domain> package,
		// with methods promoted by embedding); the top level only hands the domain packages over, following the same rule as the two cross-source domains below,
		// "wiring lives in the domain package", so all ten domains have the same shape.
		issue:      issue.New(cfg.Issue),
		suggestion: suggestion.New(cfg.Suggestion),
		revision:   revision.New(cfg.Revision),
		ark:        ark.New(cfg.ArkCred),
		skill:      skill.New(cfg.Skill),

		// The two mixed domains. ⚠️ How they are put together is decided by each domain package (the composites are their unexported structs);
		// this package only hands over the domain packages and the fallback; see composite.go in each package.
		workspace: workspace.New(workspace.Config{
			Workspace: cfg.Workspace,
			Schedule:  cfg.Schedule,
			Run:       cfg.Run,
			Fallback:  cfg.Fallback,
		}),
		agent: agent.New(agent.Config{
			Agent:    cfg.Agent,
			Fallback: cfg.Fallback,
		}),

		// The logic of these three domains is still in core and has not been moved out.
		sandbox: cfg.Fallback,
	}, nil
}

// Accessors for the ten domains. Handlers get the methods their domain can call through them.
//
// They return interfaces instead of concrete types, so handlers get **only the methods in that domain's list**.
func (o *Orchestrator) Workspace() workspace.Service   { return o.workspace }
func (o *Orchestrator) Agent() agent.Service           { return o.agent }
func (o *Orchestrator) Skill() skill.Service           { return o.skill }
func (o *Orchestrator) Ark() ark.Service               { return o.ark }
func (o *Orchestrator) Issue() issue.Service           { return o.issue }
func (o *Orchestrator) Suggestion() suggestion.Service { return o.suggestion }
func (o *Orchestrator) Sandbox() sandbox.Service       { return o.sandbox }
func (o *Orchestrator) Revision() revision.Service     { return o.revision }

// The four domain packages must fully satisfy the narrow interface of their own domain.
//
// ⚠️ When these four lines fail to compile, look at the method named in the error: either the domain package changed the signature, or that method
// has not actually been moved out of core yet. **Do not loosen the interface just to make it pass**: the interface list is
// the only record of "what handlers can call".
var (
	_ issue.Service      = (*issuesvc.Service)(nil)
	_ suggestion.Service = (*suggestionsvc.Service)(nil)
	_ revision.Service   = (*revisionsvc.Service)(nil)
	_ ark.Service        = (*arkcredsvc.Service)(nil)
	_ skill.Service      = (*skillsvc.Service)(nil)
)

// All 16 of skill's methods come from the domain package (the arkconsole domain split added the last one, GetSkillArkConsole),
// so it is in the group of assertions above "fully satisfied by the domain package", and needs no composite.
