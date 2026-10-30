// HTTP handlers of the two self-describing endpoints.
//
//	GET /api/openapi.json   this OpenAPI document
//	GET /llms.txt           the project description for agents to read
//
// # These two need no auth; where is the security boundary
//
// **They must need no auth**: an agent has to understand the platform before it can authenticate, and requiring "authenticate before you can learn
// how to authenticate" is a deadlock (same reason as the five /api/auth/ ones).
//
// So what is opened up? **Only structure, no content at all**:
//
//	spec      field names, types, paths, permission levels —— all from authz.Routes() and hand-written schemas
//	llms.txt  a piece of prose + one single variable: the origin the caller used in its own request
//
// ⚠️ That variable was added on 2026-09-01; before that this said "no template, no parameters".
// It **is not a leak**: the origin comes from the request's Host header, the address the caller itself just used,
// and returning it as-is tells it nothing new. **But this boundary only holds for "what the caller itself provides"**:
// putting in any value from Store or config must go through the test of this section again.
//
// Neither **touches Store, knows any workspace, or reads any credential**. This is pinned by
// TestPublicRoutesExposeNoTenantData: it starts the service with real seed data,
// then asserts these two responses contain no workspace name / agent name / email / key.
//
// ⚠️ If in the future you want to put dynamic info like "which providers this deployment actually enabled" into the spec,
// **that is the moment to re-evaluate this boundary**: such info is instance data, and would turn no-auth into an information leak.
//
// # The spec is a build-time product, not generated per request
//
// The handler serves the bytes embedded via go:embed directly, without running Generate() per request. Two reasons:
//
//  1. Generating per request means assembling 150 KB of JSON on every request, which is unnecessary.
//  2. **More importantly**: the embedded file is in git, so it can be diffed and code reviewed.
//     With per-request generation, "the spec changed" would be invisible in the commit history.
//
// The cost is needing a machine check to keep the file up to date; see TestSpecFileIsUpToDate in openapi_test.go.

package openapi

import (
	_ "embed"
	"net/http"

	"github.com/chyroc/ama/server/shared/gopkg/selfurl"
)

// specJSON is the generated spec, shipped with the binary.
//
// **It is a build product but committed** (unlike httpd/static): only committed can it be diffed,
// and make "changed routes without regenerating" visible in code review.
// Regenerate: `go run ./cmd/gen-openapi` (or `go generate ./openapi`).
//
//go:generate go run ../cmd/gen-openapi
//go:embed openapi.json
var specJSON []byte

// Spec returns the generated spec bytes. Used by tests and reconciliation.
func Spec() []byte { return specJSON }

// Handler serves those two self-describing endpoints.
//
// It has no dependencies (no Service, no Store, no logger): **this is exactly why it can need no auth**:
// it has nothing to leak. Before adding a Store dependency to it, reread the security boundary at the top of this file.
type Handler struct{}

// NewHandler creates a Handler.
func NewHandler() *Handler { return &Handler{} }

// Mux is the route registration ability Register needs, the same shape as core.Mux / auth.Mux.
type Mux interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// Register mounts the two routes.
//
// ⚠️ **The caller must register them on the bare mux, not through Protect/Guard**:
// that would require "log in before reading the docs", while agents read the docs first to learn how to log in.
// See the wiring in httpd/server.go.
func (h *Handler) Register(mux Mux) {
	mux.HandleFunc("GET "+PathSpec, h.serveSpec)
	mux.HandleFunc("GET "+PathLLMsTxt, h.serveLLMsTxt)
}

// serveSpec serves the OpenAPI document.
func (h *Handler) serveSpec(w http.ResponseWriter, _ *http.Request) {
	// application/json instead of application/vnd.oai.openapi+json: the latter is more precise,
	// but many HTTP clients and agent toolchains only know the former, and would download it as binary instead of parsing it.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// The spec ships with the binary, and the product of one version always serves the same bytes, so it can be cached.
	// But **not cached for long**: during rolling deploys new and old instances coexist, and a long client cache would get the old structure.
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(specJSON)
}

// serveLLMsTxt serves the description for agents to read.
func (h *Handler) serveLLMsTxt(w http.ResponseWriter, r *http.Request) {
	// text/markdown instead of text/plain: the content is Markdown, and declaring it lets renderers recognize it.
	// With charset: it is full of Chinese, and without it some clients decode it as latin-1 into garbage.
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	// The body changes with origin, so caching must be **bucketed by Host**, otherwise the same intermediate cache could send
	// the local copy (pointing to localhost) to production callers, or the other way round.
	// The other way round is what really hurts: the docs the agent reads point to production, and following them it touches production data.
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("Vary", "Host")
	_, _ = w.Write([]byte(LLMsTxt(selfurl.FromRequest(r))))
}
