// Orchestration layer: how Service is satisfied by the domain package of the service layer.
//
// # This is where the orchestration layer orchestrates the service layer
//
// This package's Service list (service.go) says "what handlers can call"; this file says
// "who satisfies it": **it holds the domain package service/<domain>, and what runs at runtime is that package's implementation**
// (all methods are promoted by embedding; there is not a single hand-written forwarding line).
//
// If **truly cross-domain** orchestration appears later (one action touching both this domain and another), explicit methods
// go on the composite here: that is where the orchestration layer should write code; single-domain business rules go into
// service/<domain>, not into this package.
//
// # Why embedding instead of hand-written forwarding
//
// Hand-writing N `return c.svc.X(...)` is bound to rot: changing a signature means changing two places, and missing one
// gives no error, that method just cannot be called (the full argument is in "why an interface and not empty forwarding" in the
// orchestrator.go file header). Embedding promotion does the same delegation at the type level, with zero maintenance.
//
// # Why the wiring code is in this package, not in the top-level orchestrator
//
// composite is an unexported struct of this package, which the top level cannot build; exporting it would spread pure wiring details
// outside the package. Same argument as the file headers of the three composite.go files of workspace / agent / skill.
package ark

import (
	arkcredsvc "github.com/chyroc/ama/server/service/arkcred"
)

// composite holds the Ark credential domain package of the service layer; all methods are promoted by embedding.
type composite struct {
	*arkcredsvc.Service
}

// New wraps the domain package of the service layer into this domain's Service.
//
// ⚠️ **No nil check here**. The top-level orchestrator.New already checked (it is the single
// entry point of wiring, and its error can point to "which domain package was not passed").
func New(svc *arkcredsvc.Service) Service {
	return &composite{Service: svc}
}

// *composite must satisfy Service as a whole; this line is the gatekeeper of this file:
// when the service.go list gains a method that service/<domain> does not have yet, this turns red on the spot.
var _ Service = (*composite)(nil)
