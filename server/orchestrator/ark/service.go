// Orchestration layer: Ark credentials (workspace level / agent level / global) and key fetching.

package ark

import (
	"context"

	"github.com/chyroc/ama/server/model/apimodel"
)

// Service is the methods handlers really use in this domain, **listing only the ones used**.
//
// This is "a narrow interface from the consumer's view": new methods on the domain package (still core.Service for now) are by default
// invisible to handlers, and opening one up means adding a line explicitly in this file. What it saves is exactly those 161
// empty forwarding methods: an interface only needs signatures, not function bodies.
//
// ⚠️ **The name is just Service; the domain is carried by the package name** (`issue.Service` / `ark.Service`).
// Previously nine interfaces were crowded into the same package orchestrator, distinguishable only by prefix
// (IssueService / ArkService); after splitting into orchestrator/<domain>/ that prefix became
// redundant: the package name already says the domain.
//
// ⚠️ What the package name guards is **between domain interface declarations**: previously, with nine interfaces in one package, one domain's file could directly
// reference another domain's interface name without error; now referencing it requires importing the sibling package, which is visible.
// **Blocking cross-domain access on the handler side does not rely on it**: that is guarded by "field types are narrow interfaces"; see
// the comments on the ten fields of Handler in apihandler/handler.go.
//
// ⚠️ **Signatures always use apimodel.X, never core.X**. This is not style but a hard constraint:
// if this package imported core, core would appear in `go list -deps ./orchestrator/...`,
// and the orchestration layer would depend again on the package it should shield, making this layer pointless (the full argument is in
// the file header of orchestrator/routes.go). Step 1 moved this batch of types into domain
// precisely so these signatures could be written.
type Service interface {
	DeleteAgentArkCredential(ctx context.Context, agentID string) error
	DeleteArkCredential(ctx context.Context, workspaceID string) error
	DeleteGlobalArkCredential(ctx context.Context) error
	GetAgentArkCredential(ctx context.Context, agentID string) (apimodel.AgentArkCredential, error)
	GetArkCredential(ctx context.Context, workspaceID string) (apimodel.ArkCredential, error)
	GetArkFetchStatus(ctx context.Context) (apimodel.ArkFetchStatus, error)
	GetGlobalArkCredential(ctx context.Context) (apimodel.GlobalArkCredential, error)
	ListVolcAccounts(ctx context.Context) ([]apimodel.VolcAccount, error)
	ListVolcArkKeys(ctx context.Context, accountID string) ([]apimodel.ArkKeyOption, error)
	SaveAgentArkCredential(
		ctx context.Context, agentID string, draft apimodel.ArkCredentialDraft,
	) (apimodel.AgentArkCredential, error)
	SaveArkCredential(
		ctx context.Context, workspaceID string, draft apimodel.ArkCredentialDraft,
	) (apimodel.ArkCredential, error)
	SaveGlobalArkCredential(
		ctx context.Context, draft apimodel.ArkCredentialDraft,
	) (apimodel.GlobalArkCredential, error)
	UpdateAgentArkRegion(
		ctx context.Context, agentID string, region apimodel.ArkRegion,
	) (apimodel.AgentArkCredential, error)
	UpdateArkRegion(
		ctx context.Context, workspaceID string, region apimodel.ArkRegion,
	) (apimodel.ArkCredential, error)
	UseGlobalArkCredential(ctx context.Context, workspaceID string) (apimodel.ArkCredential, error)
	VerifyAgentArkCredential(ctx context.Context, agentID string) (apimodel.AgentArkCredential, error)
	VerifyArkCredential(ctx context.Context, workspaceID string) (apimodel.ArkCredential, error)
	VerifyGlobalArkCredential(ctx context.Context) (apimodel.GlobalArkCredential, error)
}
