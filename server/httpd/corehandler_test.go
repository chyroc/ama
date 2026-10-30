// End-to-end tests in httpd build the core routes through here.
//
// # Why this file exists
//
// During the package split the handler moved out of core (it is now server/apihandler), and it
// **no longer takes a *core.Service**: it takes the orchestration layer itself and gets the narrow
// interfaces of the ten domains from it. So the line
// `core.NewHandler(core.HandlerConfig{Service: coreSvc})` in four test files of this package all had
// to change. All four were identical, so they were pulled into one function, to avoid changing four
// places and missing one the next time the wiring changes.
//
// # ⚠️ Wiring goes through the real orchestrator.New, no shortcuts
//
// The tests in this package are **end to end** (real mux + real Guard + real Protect + real seed);
// their value is that they run the production path. If the wiring took a shortcut here (e.g. using
// coreSvc directly as the interface), the workspaceComposite combination in the orchestration layer
// (which picks an implementation between the domain packages and the core fallback by nesting
// depth, plus 16 explicit forwards) would not be tested at all, and that is currently the part most
// likely to go wrong.
//
// The shape matches the real wiring in cmd/amad/orchestrator.go; when that changes this must
// follow, and the compiler points it out right away (missing any of the eight domain packages is a
// compile error).

package httpd

import (
	"testing"

	"github.com/chyroc/ama/server/apihandler"
	"github.com/chyroc/ama/server/core"
	"github.com/chyroc/ama/server/orchestrator"
	integrationsvc "github.com/chyroc/ama/server/service/integration"
)

// newCoreRoutes turns a core.Service into the set of routes httpd.Config.CoreHandler takes.
//
// integrations may be nil (no integration source wired), the same meaning as in HandlerConfig.
func newCoreRoutes(
	t *testing.T, coreSvc *core.Service, integrations *integrationsvc.IntegrationRegistry,
) orchestrator.Routes {
	t.Helper()
	orch, err := orchestrator.New(orchestrator.Config{
		Issue:      coreSvc.Issues(),
		Suggestion: coreSvc.Suggestions(),
		Revision:   coreSvc.Revisions(),
		ArkCred:    coreSvc.ArkCreds(),
		Workspace:  coreSvc.Workspaces(),
		Agent:      coreSvc.Agents(),
		Schedule:   coreSvc.Schedules(),
		Run:        coreSvc.Runs(),
		Skill:      coreSvc.Skills(),
		Fallback:   coreSvc,
	})
	if err != nil {
		t.Fatalf("建 orchestrator 失败: %v", err)
	}
	h, err := apihandler.NewHandler(apihandler.HandlerConfig{
		Orchestrator: orch,
		Integrations: integrations,
	})
	if err != nil {
		t.Fatalf("建 apihandler.Handler 失败: %v", err)
	}
	return h
}
