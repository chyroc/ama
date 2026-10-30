// Tests in this package that need an HTTP handler build it here.
//
// # Why core's test files may import apihandler
//
// The handler moved out of core in step 15 of the package split (this file is the
// finishing piece of that move). Production code depends in one direction only:
//
//	apihandler → orchestrator → service/<domain>      (apihandler does not import core)
//
// Check: `go list -deps ./apihandler | grep -c 'ama/server/core$'` = 0.
//
// And **test dependencies do not enter the dependency graph of the package under test**,
// so core's _test.go importing apihandler creates no cycle and does not weaken that rule.
// The reverse would be wrong (apihandler production code importing core); that would undo
// this step.
//
// # Why these tests did not move together with the handler
//
// They need core's internal test fixtures: the fakeRunner in newChatService, the
// **unexported field** svc.windowRetry, and the provider stand-ins in integration_test.go.
// Moving them would mean moving the whole set of fixtures, and most tests those fixtures
// serve are business-layer tests that belong in core anyway.
//
// The rule is "what does this test assert": tests that assert **protocol conversion**
// (what a frame looks like, which status code) go with apihandler, and four have already
// moved (see apihandler/sseframe_test.go and failmapping_test.go); tests that assert
// **business behavior and only pass through HTTP on the way** stay here.

package core

import (
	"testing"

	"github.com/chyroc/ama/server/apihandler"
	"github.com/chyroc/ama/server/orchestrator"
	integrationsvc "github.com/chyroc/ama/server/service/integration"
)

// newTestHandler wires a *Service into a handler along the **real path**.
//
// ⚠️ It deliberately goes through orchestrator.New instead of passing svc directly as the
// interface: otherwise the tested path would not be the one used in production. In the
// orchestrator layer, the workspaceComposite struct picks implementations between "domain
// package" and "core fallback" by embedding depth, and has 16 explicit forwarders. That is
// exactly where mistakes happen, and skipping it would mean these tests never cover the
// path that actually runs.
//
// integrations may be nil (meaning no integration is connected), same meaning as in
// HandlerConfig.
func newTestHandler(
	t *testing.T, svc *Service, integrations *integrationsvc.IntegrationRegistry,
) *apihandler.Handler {
	t.Helper()
	orch, err := orchestrator.New(orchestrator.Config{
		Issue:      svc.Issues(),
		Suggestion: svc.Suggestions(),
		Revision:   svc.Revisions(),
		ArkCred:    svc.ArkCreds(),
		Workspace:  svc.Workspaces(),
		Agent:      svc.Agents(),
		Schedule:   svc.Schedules(),
		Run:        svc.Runs(),
		Skill:      svc.Skills(),
		Fallback:   svc,
	})
	if err != nil {
		t.Fatalf("装配编排层失败：%v", err)
	}
	h, err := apihandler.NewHandler(apihandler.HandlerConfig{
		Orchestrator: orch,
		Integrations: integrations,
		Logger:       discardLogger(),
	})
	if err != nil {
		t.Fatalf("装配 handler 失败：%v", err)
	}
	return h
}
