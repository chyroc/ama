// Persistence contract of the Ark credential domain: one table per credential level, plus two tables read from other domains.
//
// # The declaration goes with the domain, not in core/store.go
//
// The reason is word for word the same as for the previous five domains (service/issue, suggestion, revision, schedule, workspace):
// core/store.go is the common file every resource domain must pass through, and a dozen concurrent sessions touch
// it. Piling one domain's method declarations there costs not "a big file" but **conflict surface**: two sessions each move
// their own domain, yet their diffs clash in the same hunk.
//
// Now the declaration lives here, and core/store.go just embeds one line `arkcredsvc.Store`.
//
// # ⚠️ Why the three levels are in the same interface
//
// Because **resolving a credential once reads the three tables in order** (ResolveSecretForAgent: agent → workspace →
// account). That order is the core invariant of this domain. Splitting into three packages would force one to import another
// (zero imports between domain packages is a hard rule), or push the fallback order up to the callers, and **each level a caller
// misses is one "agent uses the wrong key", with zero errors**. The test is the same as for the schedule claim methods:
// "which table this group describes" and "who must be together for this rule to hold".
//
// # ⚠️ Plaintext keys are stored encrypted, but that is not in this interface
//
// The apimodel.ArkSecret / GlobalArkSecret / AgentArkSecret these methods take and return **carry plaintext
// fields**; encryption and decryption are done transparently on the repository/dao side (shared/infra/cryptobox). This domain always
// sees plaintext. **Do not add your own encryption in the business layer**: two layers of encryption would make existing rows unreadable.
package arkcred

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Store is all the persistence methods this domain uses, **only the ones it uses**.
type Store interface {
	// -- Workspace-level Ark credentials (ark_secrets) --------------------------------------

	// GetArkSecret gets the credential with the plaintext key; returns ErrNotFound if not configured.
	//
	// ⚠️ Rows with Source inherit **have no plaintext** (that is exactly how linking works),
	// so callers must not use its APIKey directly; go through ResolveSecret.
	GetArkSecret(ctx context.Context, workspaceID string) (apimodel.ArkSecret, error)
	PutArkSecret(ctx context.Context, sec apimodel.ArkSecret) error
	// DeleteArkSecret deletes the credential; missing is not an error (idempotent).
	DeleteArkSecret(ctx context.Context, workspaceID string) error

	// -- Account-level Ark credential (the one "configured once globally, referenced by workspaces") ----------------

	// GetGlobalArkSecret gets a person's account-level credential; returns ErrNotFound if never configured.
	//
	// **The key is the employee email**. When the workspace side has Source inherit, reads fall back here
	// (ResolveSecret). Workspaces keep no copy, so changing this one takes effect at once for every workspace that references it.
	GetGlobalArkSecret(ctx context.Context, email string) (apimodel.GlobalArkSecret, error)

	// PutGlobalArkSecret stores/overwrites a person's account-level credential.
	//
	// **One upsert, not read-then-write**: across instances read-then-write is TOCTOU (see PutOnboarding).
	PutGlobalArkSecret(ctx context.Context, sec apimodel.GlobalArkSecret) error

	// DeleteGlobalArkSecret deletes the account-level credential; missing is not an error (idempotent).
	//
	// ⚠️ **It does not change any workspace's source**. Workspaces referencing it end up "referencing a credential that does not
	// exist", which reads as not configured. This is deliberate; see DeleteGlobalArkCredential.
	DeleteGlobalArkSecret(ctx context.Context, email string) error

	// -- Agent-level Ark credential (the most specific of the three levels) ----------------------------

	// GetAgentArkSecret gets an agent's own key; returns ErrNotFound if never configured.
	//
	// **"No such row" means "follow the workspace setting"**, not "not configured". The agent level has no inherit
	// storage state (row = custom, no row = follow); see apimodel.AgentArkCredential for why.
	//
	// The fallback chain is agent → workspace → account level, with ResolveSecretForAgent as the only entry point.
	// **Lower levels keep no copy of upper levels**, so changing an upper level links through at once.
	GetAgentArkSecret(ctx context.Context, agentID string) (apimodel.AgentArkSecret, error)

	// PutAgentArkSecret stores/overwrites an agent's credential.
	//
	// **One upsert, not read-then-write**: across instances read-then-write is TOCTOU (see PutOnboarding).
	PutAgentArkSecret(ctx context.Context, sec apimodel.AgentArkSecret) error

	// DeleteAgentArkSecret deletes the agent-level credential; missing is not an error (idempotent).
	//
	// After deletion the agent falls back to the workspace level: that is the "use workspace setting" action in the UI.
	DeleteAgentArkSecret(ctx context.Context, agentID string) error

	// -- Two tables read from other domains -------------------------------------------------
	//
	// The test is word for word the same as for GetWorkspace in issue.Store: **reading another domain's table** may go into your own
	// Store; **going through another domain's UpdateX gate** may not. The latter is business logic, not a single query.

	// GetWorkspace checks whether the workspace exists (precondition before storing a credential); returns ErrNotFound if not.
	GetWorkspace(ctx context.Context, id string) (apimodel.Workspace, error)

	// GetAgent checks whether the agent exists and which workspace it belongs to; returns ErrNotFound if not.
	//
	// **WorkspaceID is load-bearing**: the agent-level credential falls back to "the workspace it is in",
	// and the workspaceID in the agent row is the source of truth (the agent may have been moved between workspaces).
	GetAgent(ctx context.Context, id string) (apimodel.Agent, error)
}
