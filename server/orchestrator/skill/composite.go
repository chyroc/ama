// Orchestrator layer: how Service is satisfied by the service-layer domain package.
//
// # This is where the orchestrator layer wires up the service layer
//
// This package's Service list (service.go) says "what the handler can call"; this file says
// "who satisfies it": **it holds the domain package of service/skill, and at run time its
// implementation is what runs** (all methods are promoted by embedding, with not a single
// hand-written forwarder).
//
// If **truly cross-domain** orchestration ever appears (one action must change both skill
// and another domain), write explicit methods on the composite here: that is where the
// orchestrator layer should have code. Single-domain business rules go to service/skill,
// not to this package.
//
// # Why embedding instead of hand-written forwarding
//
// Writing 16 `return c.svc.X(...)` by hand always rots: changing one signature
// means changing two places, and missing one gives no error, the method just cannot be
// reached (full reasoning in the file header of orchestrator.go, "why interfaces instead of
// empty forwarders"). Embedding does the same delegation at the type level, with no upkeep.
//
// # Why the wiring code is in this package and not in the top-level orchestrator
//
// composite is an unexported struct of this package, so the top level cannot build it;
// exporting it would spread pure wiring details outside the package. Same reasoning as the
// file headers of the two composite.go files of workspace / agent.
package skill

import (
	skillsvc "github.com/chyroc/ama/server/service/skill"
)

// composite holds the service-layer skill domain package; all 16 methods are promoted by embedding.
type composite struct {
	*skillsvc.Service
}

// New wraps the service-layer domain package as this domain's Service.
//
// ⚠️ **No nil check here.** The top-level orchestrator.New already checks (it is the only
// wiring entry point, and its error can point at "which domain package was not passed").
func New(svc *skillsvc.Service) Service {
	return &composite{Service: svc}
}

// *composite must satisfy all of Service. This line is the guard of this file:
// when a method is added to the service.go list but service/skill does not have it
// yet, this fails right away.
var _ Service = (*composite)(nil)
