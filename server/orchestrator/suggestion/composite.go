// Orchestration layer: how Service is satisfied by the domain packages of the service layer.
//
// # This is where the orchestration layer wires the service layer
//
// This package's Service list (service.go) says "what the handler can call"; this file says "who
// satisfies it": **it holds the domain package of service/<domain>, and at run time its
// implementation is what runs** (all methods are promoted by embedding; not a single hand-written
// forward).
//
// If **truly cross-domain** orchestration ever appears (one action that touches this domain and
// another at the same time), the explicit methods go on the composite here: that is where the
// orchestration layer should have code. Single-domain business rules go to service/<domain>, not
// this package.
//
// # Why embedding instead of hand-written forwards
//
// N hand-written `return c.svc.X(...)` will rot: changing a signature means changing two places,
// and missing one gives no error, the method just cannot be reached (the full argument is in "Why
// interfaces instead of empty forwarding shells" in the file header of orchestrator.go). Embedding
// does the same delegation at the type level with zero maintenance.
//
// # Why the wiring code is in this package and not in the top-level orchestrator
//
// composite is an unexported struct of this package and cannot be built from the top level;
// exporting it would spread pure wiring details outside the package. Same argument as the file
// headers of the three composite.go files of workspace / agent / skill.
package suggestion

import (
	suggestionsvc "github.com/chyroc/ama/server/service/suggestion"
)

// composite holds the product suggestion domain package of the service layer; all methods are
// promoted by embedding.
type composite struct {
	*suggestionsvc.Service
}

// New wraps the service layer's domain package as this domain's Service.
//
// ⚠️ **No nil check here**. The top-level orchestrator.New already checks (it is the only entry point
// of the wiring, and its error can say "which domain package was not passed").
func New(svc *suggestionsvc.Service) Service {
	return &composite{Service: svc}
}

// *composite must satisfy Service as a whole; this line is the gatekeeper of this file: when a
// method is added to the service.go list but service/<domain> does not have it yet, this turns red
// right away.
var _ Service = (*composite)(nil)
