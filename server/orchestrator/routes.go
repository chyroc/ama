// Wiring seam: the "set of routes that can be mounted" that httpd gets.
//
// # What this file solves
//
// httpd/server.go used to say `CoreHandler *core.Handler`, so **the wiring layer was pinned directly to
// core**: httpd could see every method of Handler, and was recompiled with every signature change in core.
// The orchestration layer sits between handler and Service, but a piece was missing between handler and httpd;
// this file adds that piece.
//
// After that, httpd only knows two methods (Register / RegisterTerminalWS),
// and everything else on the handler is invisible to it.
//
// # ⚠️ The cycle has not come back, but only one direction is allowed
//
// The previous step concluded "this package must not import core, or there is a cycle". That needs to be said more precisely now,
// because **when tested, there was no cycle at that time**: core still does not import this package (the handler uses
// the local interface copy in core/handlerservice.go), so
//
//	orchestrator → core     compiles, no cycle reported
//
// But it is **still wrong**, and the reason is not the compiler but why this layer exists: add that edge, and
// `go list -deps ./orchestrator` contains core again, the orchestration layer depends again on the package it is supposed to hide,
// and "what handlers can call is decided by the interface list" stops holding on the spot. So that edge must not be added,
// and the check changes from "is there a cycle" to "is core in deps": the latter is machine-checkable,
// the former quietly flips between true and false with changes on the core side.
//
// The opposite direction is allowed, and tested to work:
//
//	core → orchestrator     compiles, no cycle (this package only depends on domain / storemodel)
//
// The rule that this package must not import core **still holds**; only the reason changed to a firmer one.
//
// # ⚠️ Why core.Mux must be an alias and not a separate copy
//
// Go method sets **match exactly**: `func (h *Handler) Register(mux core.Mux)`
// does not satisfy `interface{ Register(mux orchestrator.Mux) }`, even if the two Mux look
// exactly the same (both have only one HandleFunc): the same structure does not mean the same type.
//
// So the core side writes `type Mux = orchestrator.Mux` (an **alias**, with `=`),
// so both names point to the same type. If written as `type Mux orchestrator.Mux` (no `=`,
// a **new type**), the Routes assertion fails to compile on the spot, with the error
// "have Register(core.Mux) want Register(orchestrator.Mux)":
// the two type names look the same, so it reads as if the compiler is talking nonsense, while the only difference is that equals sign.
package orchestrator

import "net/http"

// Mux is the ability needed to register routes, with only one method, HandleFunc.
//
// **It is not hard-coded to *http.ServeMux because of auth**: what httpd passes in is a decorator wrapped with authentication and authorization
// (protectedMux in httpd + authz.Guard.Wrap); it wraps a Protect layer before handing the handler to the real
// mux. This way none of the dozens of route registration lines in business modules has to change,
// and every route is protected.
//
// ⚠️ Auth **must be wrapped per route like this, not around the whole mux**: if wrapped outside, requests with the wrong method
// (say, GET on an endpoint that only accepts POST) would first be stopped with 401 instead of returning 405;
// httpd's TestMethodRouting fails on the spot, and the error does not point to the real cause at all.
// For the tested comparison see auth.TestProtectAfterRoutingPreserves405.
//
// This interface used to live in core/handler.go; it moved here so httpd can say "I want a set of routes"
// without importing core. core.Mux is now an alias pointing to it, so **both sides are the same
// type**, and the registration code in core and feishu did not change by a single line.
type Mux interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// Routes is a set of HTTP routes that can be mounted on a mux.
//
// The only implementation is *core.Handler; the assertion is in cmd/amad/orchestrator.go (which sees
// both packages; this package cannot see core).
//
// The two methods are **deliberately separate**, because they are mounted on different muxes, and that distinction is a security boundary:
//
//   - RegisterTerminalWS is mounted on the **bare mux** (no authentication or authorization). A WebSocket handshake cannot carry
//     anything beyond the Cookie, so auth is done by redeeming a one-time ticket instead; see core/terminalticket.go.
//   - Register is mounted on the **mux wrapped with authentication + authorization**; all other business endpoints are here.
//
// Merging them into one method would make the caller pass two muxes, and passing them in the wrong order gives **zero errors**:
// business endpoints would be mounted on the bare mux, and site-wide auth silently disappears. With two methods, passing the wrong one is a compile error.
type Routes interface {
	// Register mounts the business routes on mux. mux must be the one **already wrapped with authentication and authorization**.
	Register(mux Mux)

	// RegisterTerminalWS mounts the container terminal's WebSocket route on mux.
	//
	// ⚠️ It takes the **bare mux**; see the comment on Routes above and core/terminalticket.go for why.
	RegisterTerminalWS(mux Mux)

	// RegisterHostDaemon mounts the ama daemon's heartbeat/result routes on the bare mux
	// (host_token authenticates itself, without Protect/Guard). Same reason as RegisterTerminalWS.
	RegisterHostDaemon(mux Mux)
}
