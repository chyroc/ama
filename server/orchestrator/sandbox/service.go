// Orchestration layer: session containers (Docker): state, files, shell, terminal.

package sandbox

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
	"net/http"
)

// Service is the set of methods the handler really uses in this domain, **listing only the ones used**.
//
// This is a "narrow interface from the consumer's point of view": new methods on the domain package
// (still core.Service for now) are by default
// invisible to the handler; to expose one you must add a line explicitly in this file. What it saves
// is exactly those 161
// empty forwarding methods: an interface only needs signatures, not function bodies.
//
// ⚠️ **The name is just Service; the domain is carried by the package name** (`issue.Service` /
// `ark.Service`).
// Before, nine interfaces were crammed into one package orchestrator and could only be told apart by
// prefix
// (IssueService / ArkService); after splitting into orchestrator/<domain>/ that prefix became
// redundant: the package name already says the domain.
//
// ⚠️ What the package name controls is **between domain interface declarations**: before, with nine
// interfaces in the same package, a file of one domain could directly
// reference another domain's interface name with no error; now referencing it needs importing the
// sibling package, which is visible.
// **Cross-domain blocking on the handler side does not rely on it**: that is handled by "field types
// are narrow interfaces"; see
// the comments on the ten fields of Handler in apihandler/handler.go.
//
// ⚠️ **Signatures always use apimodel.X, never core.X**. This is not style, it is a hard rule:
// if this package imported core, core would appear in `go list -deps ./orchestrator/...`,
// and the orchestration layer would depend again on the package it is meant to shield, making this
// layer pointless (full reasoning in
// the file header of orchestrator/routes.go). Step 1 moved these types into domain
// precisely so these signatures could be written.
type Service interface {
	IssueTerminalTicket(ctx context.Context, runID string) (string, error)
	ListRunSandboxFiles(ctx context.Context, runID, path string) (apimodel.SandboxListing, error)
	ReadRunSandboxFile(ctx context.Context, runID, path string) (apimodel.SandboxFileContent, error)
	ResumeRunSandbox(ctx context.Context, runID string) (apimodel.SandboxState, error)
	RunSandboxShell(ctx context.Context, runID, command string) (apimodel.SandboxShellResult, error)
	RunSandboxState(ctx context.Context, runID string) (apimodel.SandboxState, error)
	StreamRunSandboxTerminal(w http.ResponseWriter, r *http.Request, runID string) error
	StreamRunSandboxTerminalWS(
		w http.ResponseWriter, r *http.Request, runID string,
	)
	WriteRunSandboxTerminal(
		ctx context.Context, runID, terminalID string, payload []byte,
	) error
}
