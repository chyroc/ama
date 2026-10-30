// Orchestration layer: Agent CRUD, credential injection, Ark sync and console redirects.

package agent

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Service lists the methods that handlers actually use in this domain, **and only those**.
//
// This is a "narrow interface from the caller's point of view": new methods in the domain package
// are hidden from handlers by default. To expose one, you must add a line to this file on purpose.
// What it saves is all the empty forwarding methods: an interface only needs signatures, not
// function bodies.
//
// ⚠️ **The name is just Service; the package name carries the domain** (`agent.Service` /
// `issue.Service`). Before, these interfaces were packed into one package orchestrator and could
// only be told apart by prefix (AgentService / IssueService). After splitting into
// orchestrator/<domain>/, the prefix became redundant: the package name already says which domain
// it is.
//
// ⚠️ The package name guards **the boundary between domain interface declarations**: to use a
// sibling domain you must import it, and an import is visible in code review. **Blocking
// cross-domain access on the handler side does not rely on this**: that is enforced by "field types
// are narrow interfaces"; see the comments on the ten fields of Handler in apihandler/handler.go.
//
// ⚠️ **Signatures always use apimodel.X, never core.X.** This is not style, it is a hard rule: if
// this package imported core, core would show up in `go list -deps ./orchestrator/...`, the
// orchestration layer would depend again on the package it is meant to hide, and the layer would be
// pointless (full argument in the file header of orchestrator/routes.go).
//
// # These 9 methods used to live on workspace.Service
//
// agent used to have no handler file of its own: all 10 routes were in apihandler/handler.go, and
// the methods hung on workspace.Service, the only mixed domain among the nine domains at the time.
// This package is the tenth domain package. It pulls those 9 methods out: 6 are provided by
// service/agent, and 3 (Ark console redirect / querying and retrying sync tasks) still fall back to
// core. Wiring is in this package's composite.go.
type Service interface {
	CreateAgent(ctx context.Context, workspaceID string, draft apimodel.AgentDraft) (apimodel.Agent, error)
	DeleteAgent(ctx context.Context, id string) error
	GetAgent(ctx context.Context, id string) (apimodel.Agent, error)
	GetAgentArkConsole(ctx context.Context, agentID string) (apimodel.ArkConsoleRef, error)
	LatestAgentSyncTask(
		ctx context.Context, agentID string,
	) (apimodel.AgentSyncTask, error)
	ListAgents(ctx context.Context, workspaceID string) ([]apimodel.Agent, error)
	RetryAgentSync(ctx context.Context, agentID string) (apimodel.AgentSyncTask, error)
	SetAgentIntegrations(
		ctx context.Context, id string, ids []string,
	) (apimodel.Agent, error)
	UpdateAgent(ctx context.Context, id string, draft apimodel.AgentDraft) (apimodel.Agent, error)
}
