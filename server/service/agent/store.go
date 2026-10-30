// Persistence contract of the Agent domain.
//
// # Declarations follow the domain, not left in core/store.go
//
// The reason is word for word the same as for the previous nine domains (issue / suggestion /
// revision / schedule / workspace /
// arkcred / run / arkrun / arkbackoff): core/store.go is the shared file every resource domain must
// pass through,
// and many concurrent changes go through it. Piling one domain's method declarations there costs
// not
// "a big file" but **conflict surface**: two changes each moving their own domain, yet the diffs
// collide on the same hunk.
//
// Now the declarations live here, and core/store.go only embeds one line, `agentsvc.Store`.
//
// # ⚠️ It contains two tables owned by others, each with its own reason
//
//	GetWorkspace    creating an agent must first confirm the workspace exists (returning 404 rather
//	                than creating an orphan)
//	AgentsBoundTo   reverse reference for Feishu apps. **The source of truth is on the agent side**
//	                (Agent.FeishuAppIDs),
//	                and the BoundAgentIDs on the Feishu app is a derived value computed live; so this
//	                method
//	                really still queries the agents table, just filtering by another field
//
// The rule is the same as in service/run/store.go: "which table this group describes", not "who
// calls it".
// AgentsBoundTo describes the agents table, so it belongs to this domain.
//
// # ⚠️ ListAgents / GetAgent overlap with declarations in two other domains, and that is not a mistake
//
// service/run/store.go and service/schedule/store.go each also declare
// ListAgents / GetAgent (the former computes derived fields for the list page, the latter looks up
// the agent linked to a task).
// Go allows an explicit method and an embedded interface's method with the same name and signature
// to coexist; **the two signatures must match exactly**,
// and changing one gives a compile error, which is exactly the gate we want.
package agent

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Store is all the persistence methods this domain needs, **listing only the ones used**.
type Store interface {
	// -- the agent's own table ------------------------------------------------

	// ListAgents lists the agents of a workspace. Returns a non-nil slice (possibly empty),
	// because the frontend calls .map() directly, and null would blow up.
	ListAgents(ctx context.Context, workspaceID string) ([]apimodel.Agent, error)

	// GetAgent fetches an agent by ID; returns ErrNotFound when it does not exist.
	GetAgent(ctx context.Context, id string) (apimodel.Agent, error)

	// CreateAgent creates an agent; returns ErrConflict on a unique key collision.
	CreateAgent(ctx context.Context, a apimodel.Agent) (apimodel.Agent, error)

	// SaveAgent overwrites the whole record.
	SaveAgent(ctx context.Context, a apimodel.Agent) (apimodel.Agent, error)

	// DeleteAgent deletes an agent; not existing is not an error (idempotent).
	DeleteAgent(ctx context.Context, id string) error

	// AgentsBoundTo lists the agent IDs bound to a Feishu app, in binding order.
	//
	// Used in two places: filling App.BoundAgentIDs on the Feishu side (AgentsBoundToFeishuApp),
	// and checking before save that these apps are not held by someone else (ensureFeishuAppsFree).
	//
	// **The order is the agents' storage order, not a set**: the frontend's isDirty is a bare
	// JSON.stringify
	// comparison, and order jitter would make the page show "unsaved changes".
	AgentsBoundTo(ctx context.Context, feishuAppID string) ([]string, error)

	// -- someone else's table: workspaces --------------------------------------

	// GetWorkspace fetches a workspace by ID; returns ErrNotFound when it does not exist.
	//
	// Used in only one place: CreateAgent confirming "this workspace really exists". Without the
	// check it would create an
	// agent under a nonexistent workspace, which appears in no list while the user gets a 200.
	GetWorkspace(ctx context.Context, id string) (apimodel.Workspace, error)
}
