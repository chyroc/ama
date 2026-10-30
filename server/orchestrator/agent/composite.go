// Composition layer: how Service is satisfied by "service/agent + core fallback" together.
//
// The shape is copied from orchestrator/workspace/composite.go (the original home of mixed
// domains), but one step simpler: there are only two sources, and **both can be embedded
// anonymously**. The implicit field name of *agentsvc.Service is Service, and Fallback's is
// Fallback, so they do not clash. That is why this file **has not a single line of forwarding
// code**:
//
//	*agentsvc.Service   embedded anonymously, contributes 6 methods (CRUD / credential injection)
//	Fallback            embedded anonymously, contributes the 3 still in core (console link / sync task)
//
// 6 + 3 = 9, which is exactly the number of methods on Service.
//
// # ⚠️ Those 3 lines were not copied by hand, and do not copy them by hand
//
// The signatures in Fallback are **exactly the same, character for character** as the
// matching 2 in service.go. They were filtered mechanically from the latter (removing the 7
// already provided by service/agent). The problem with copying by hand is not the effort; it
// is that a copy mistake does not always report an error at the right line. See the same
// point in the file header of workspace/composite.go for the full argument.
//
// # ⚠️ The composite picks implementations by embedding depth, not by forwarding
//
// Same rule as in workspace: **the same name at the same depth is ambiguous**, and Go reports
// "ambiguous selector" at the **call site**, not at the struct definition. So Fallback must
// remove every one of the 6 methods the domain package already provides. Miss one: that
// method appears twice at depth 1 → the composite does not satisfy Service → the static
// assertion at the bottom fails. Remove one too many: nobody provides it → it fails the same
// way. **Both mistakes are compile errors**; it never silently picks the wrong
// implementation.
//
// # ⚠️ Why the wiring code lives in this package and not in the top-level orchestrator
//
// composite is an unexported struct of this package, so the top-level orchestrator cannot
// build it. Exporting it would spread a pure wiring detail like "who embeds whom" outside the
// package. Outside code only needs to say "this domain package + this fallback". Same
// argument as in the file header of workspace/composite.go.

package agent

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
	agentsvc "github.com/chyroc/ama/server/service/agent"
)

// Fallback is the 2 methods of Service that **still only core.Service can satisfy**
// (the step that split out the arkconsole domain moved GetAgentArkConsole into the domain
// package, so 3 became 2). The signatures match orchestrator/agent/service.go exactly; see
// the file header for why.
//
// ⚠️ **Exporting it is required, not incidental**: the top-level orchestrator.Fallback
// embeds it (the real value behind that interface is *core.Service), and you cannot embed an
// unexported name across packages.
//
// It shrinks as later steps move domains out of core, and will eventually disappear.
type Fallback interface {
	LatestAgentSyncTask(
		ctx context.Context, agentID string,
	) (apimodel.AgentSyncTask, error)
	RetryAgentSync(ctx context.Context, agentID string) (apimodel.AgentSyncTask, error)
}

// Config is what is needed to build this domain's Service: the domain package + a fallback
// for "the ones it cannot reach".
//
// ⚠️ The field is deliberately the concrete `*agentsvc.Service` and not an interface, for the
// same reason as the top-level orchestrator.Config: the job of this layer is to know what it
// is composing.
type Config struct {
	Agent    *agentsvc.Service
	Fallback Fallback
}

// New combines the domain package and the core fallback into this domain's Service.
//
// ⚠️ **No nil checks here**. The top-level orchestrator.New already checks each one (it is the
// only wiring entry point, so its error can point at "which domain package was not passed").
// Checking again here would only give the same error two reports with different wording.
func New(cfg Config) Service {
	return &composite{
		Service:  cfg.Agent,
		Fallback: cfg.Fallback,
	}
}

// composite makes Service satisfied by "service/agent + core fallback" together.
//
// Both members are embedded anonymously and their names do not clash, so not a single
// forwarding method is needed. That is the whole difference from the workspace composite
// (where all three domain packages name their type Service, so only one can be embedded).
type composite struct {
	*agentsvc.Service
	Fallback
}

// *composite must fully satisfy Service.
//
// ⚠️ This line is the gatekeeper of this file. If it fails there are only two causes, both in
// Fallback: a method the domain package already provides was not removed (same name at the
// same depth → ambiguous), or one too many was removed (nobody provides it). The error names
// the method.
var _ Service = (*composite)(nil)
