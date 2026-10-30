// Orchestration layer: how Service is satisfied by the domain package in the service layer.
//
// # This is where the orchestration layer wires up the service layer
//
// This package's Service list (service.go) says "what handlers can call"; this file says "who
// satisfies it": **it holds the service/<domain> package, and at runtime that package's
// implementation is what runs** (every method is promoted by embedding; not a single hand-written
// forwarder).
//
// If **truly cross-domain** orchestration ever appears (one action touching this domain and
// another at the same time), the explicit method goes on the composite here; that is where the
// orchestration layer should write code. Single-domain business rules go in service/<domain>, not
// in this package.
//
// # Why embedding instead of hand-written forwarding
//
// N hand-written `return c.svc.X(...)` will always rot: changing a signature means changing two
// places, and missing one gives no error, just a method that cannot be reached (full argument in
// "why an interface and not an empty forwarding shell" in the orchestrator.go header). Embedding
// does the same delegation at the type level, with zero maintenance.
//
// # Why the wiring code lives in this package and not in the top-level orchestrator
//
// composite is an unexported struct of this package, which the top level cannot build; exporting
// it would spread pure wiring details outside the package. Same argument as the headers of the
// workspace / agent / skill composite.go files.
package revision

import (
	revisionsvc "github.com/chyroc/ama/server/service/revision"
)

// composite holds the service layer's revision history domain package; every method is promoted by embedding.
type composite struct {
	*revisionsvc.Service
}

// New wraps the service layer's domain package as this domain's Service.
//
// ⚠️ **No nil check here.** The top-level orchestrator.New already checks (that is the single
// entry point for wiring, and its error can say "which domain package was not passed").
func New(svc *revisionsvc.Service) Service {
	return &composite{Service: svc}
}

// *composite must fully satisfy Service; this line is this file's gatekeeper: when a method is
// added to the service.go list but service/<domain> does not have it yet, this fails right away.
var _ Service = (*composite)(nil)
