// Aliases left on the core side after the authorization management framework (registry +
// four-state model) and the "not stored in the database" authorization sources moved into
// service/integration.
//
// # Why keep aliases instead of changing callers to integrationsvc.
//
// Exactly the same rule as for the previous eleven domains (precedents: ParseSkillPackage in
// core/skillpkgalias.go, ComputeNextRun in core/schedule.go): **packages outside core should not
// change because of a pure move**. This domain has especially many and scattered external
// references:
//
//	cmd/amad/main.go        builds all providers and registers them
//	authz/matrix.go         comments point at IntegrationRegistry and LarkUserProvider
//	openapi/                IntegrationState is part of the API contract
//	cmd/probe-lark-user     probe
//
// Changing all of these at once would put more than moved lines into the diff, and the whole
// value of a move MR is that "the diff contains only moved lines".
//
// # ⚠️ These are aliases (`=`), not new types and not new variables
//
// Types use **true aliases** `type X = Y`, so:
//
//   - compile-time assertions like `var _ core.IntegrationProvider = (*LarkUserProvider)(nil)`
//     still hold;
//   - the provider that stays in core (lark-user)
//     returns `core.IntegrationState`, which **is the same type** as the
//     `integrationsvc.IntegrationState` the registry expects, so it still satisfies
//     IntegrationProvider without changing a single character;
//   - the registry detects optional capabilities by **type assertion** (PollAuthorization /
//     ListApps / …), and the types in those assertions must also be exactly the same.
//
// With a copied definition instead of an alias, all three of the above would **silently** break
// at once: not a compile error, but at runtime "this source does not support polling / does not
// support multiple credentials", which shows on the page as authorization spinning forever.
//
// Same for sentinels: `var ErrX = integrationsvc.ErrX` is **the same variable**, so the
// `errors.Is` checks in the handler (→ 400 / 404 / 422) do not need to change. A copy would make
// the sentinel two different variables, and errors.Is would silently return false.
//
// # Sources still left in core
//
// lark-user **holds credentials** (refresh_token), and its Store method declarations are still in
// core/store.go; they have to move together, which is the next step.
package core

import (
	"github.com/chyroc/ama/server/model/apimodel"
	integrationsvc "github.com/chyroc/ama/server/service/integration"
)

// -- Four-state model (integration.go) ----------------------------------------

// IntegrationStatus is the current status of an authorization source, one of four.
type IntegrationStatus = integrationsvc.IntegrationStatus

// The four status values. **Each is handled differently**, which is exactly why we do not use
// two states: missing needs an admin to configure it; the user clicking authorize does nothing.
const (
	IntegrationOK      = integrationsvc.IntegrationOK
	IntegrationWarning = integrationsvc.IntegrationWarning
	IntegrationError   = integrationsvc.IntegrationError
	IntegrationMissing = integrationsvc.IntegrationMissing
)

// IntegrationDetail is one entry in the grey name/value list on the card.
//
// ⚠️ Value is shown to the user as is; **never put plaintext credentials in it**.
type IntegrationDetail = integrationsvc.IntegrationDetail

// IntegrationState is everything an authorization source shows.
//
// ⚠️ **It goes into the HTTP response as is** (the `openapi` package references it), so no token
// / secret may appear in any of its fields, including the two free-text fields Summary and
// FixHint. `TestIntegrationResponseNeverLeaksCredentials` searches the whole response body byte by
// byte to lock this in.
type IntegrationState = integrationsvc.IntegrationState

// IntegrationProvider is what an authorization source implements.
//
// ⚠️ The three providers left in core keep satisfying it through this alias; **it must be a true
// alias**, see the file header.
type IntegrationProvider = integrationsvc.IntegrationProvider

// AuthorizationStatus is the result of one "is authorization done yet" check (used for device
// code polling).
//
// ⚠️ Defined in integrationsvc; core/integration_larkuser.go also has the same alias line (that
// file is the only implementer, so a copy close by is easier to read). Both point at the same type.
type AuthorizationStatus = integrationsvc.AuthorizationStatus

// -- Registry (integration.go) ------------------------------------------------

// IntegrationRegistry is the provider registry. Safe for concurrent use; returns in registration
// order.
//
// ⚠️ **It must be the same instance as Handler.integrations** (cmd/amad passes the same pointer),
// not two copies. Two copies show up as "the authorization page has this source, the workspace
// binding page says it does not exist".
type IntegrationRegistry = integrationsvc.IntegrationRegistry

// NewIntegrationRegistry creates an empty registry.
var NewIntegrationRegistry = integrationsvc.NewIntegrationRegistry

// -- Framework sentinel errors ------------------------------------------------
//
// ⚠️ All are aliases (the same variable), so the errors.Is checks in the handler do not change.
// One sentinel per meaning is the convention in this domain; do not merge them. They map to
// different HTTP status codes and different UI behavior.

var (
	// ErrAuthorizeUnsupported means this source is not connected via "click to authorize".
	ErrAuthorizeUnsupported = integrationsvc.ErrAuthorizeUnsupported

	// ErrRevokeUnsupported means **the platform really has no** API to revoke authorization,
	// not "not implemented yet".
	ErrRevokeUnsupported = integrationsvc.ErrRevokeUnsupported

	// ErrAppsUnsupported means this source has no concept of "selectable apps".
	ErrAppsUnsupported = integrationsvc.ErrAppsUnsupported

	// ErrWorkspaceScopeUnsupported means the caller named a workspace, but this source does not
	// separate apps by workspace. **It must be an error, it must not fall back**.
	ErrWorkspaceScopeUnsupported = integrationsvc.ErrWorkspaceScopeUnsupported

	// ErrPollUnsupported means this source does not use the "poll for authorization result" flow.
	ErrPollUnsupported = integrationsvc.ErrPollUnsupported
)

// -- The four authorization sources that are not stored in the database -------

// AMAIntegrationID is the stable identifier of the "AMA platform" source.
const AMAIntegrationID = integrationsvc.AMAIntegrationID

// ManagedAPIKeys is the capability the "AMA platform" source needs. Implemented in the auth
// package, assembled by cmd/amad.
//
// ⚠️ auth/managedkey.go **deliberately does not import this interface type**; the two connect
// through Go's implicit interface satisfaction. A comment in `feishu/apps.go` also points at it.
type ManagedAPIKeys = integrationsvc.ManagedAPIKeys

// AMAProvider is the "AMA platform itself" authorization source.
type AMAProvider = integrationsvc.AMAProvider

// NewAMAProvider creates an AMA platform provider. keys may be nil (meaning not wired).
var NewAMAProvider = integrationsvc.NewAMAProvider

// -- Unexported aliases: keep the providers left in core unchanged ------------
//
// The four below are **exported** on the integrationsvc side; here each gets its original
// lowercase name back. **This is not overkill**: core/integration_larkuser.go references them,
// and with the aliases that file needs no change at all. When it moves over itself, delete these
// lines along with it.
//
// ⚠️ **They are exported on the integrationsvc side because this move forced it, not as a
// "while we are here, widen visibility"**: unexported symbols cannot be reached across packages.
// The test is "does anyone on the core side still use it"; once nobody does, change it back to
// lowercase on that side.

// providerTimeout is the time limit for a single provider status check, **and must be set per provider**.
const providerTimeout = integrationsvc.ProviderTimeout

var (
	// expiryDetail formats "valid until" as one detail; returns false for zero/absurd values.
	expiryDetail = integrationsvc.ExpiryDetail

	// plausibleExpiry decides whether an expiry time "looks real", catching the class of unit
	// mix-ups (milliseconds parsed as seconds give year 58624, and then that token "never expires").
	plausibleExpiry = integrationsvc.PlausibleExpiry

	// fmtPastTime renders a **past** time as "local time (how long ago)".
	//
	// ⚠️ Do not mix it up with expiryDetail, which renders future times: rendering a past time
	// with the future renderer would say "expired 3 days ago" for something that never expires
	// (service account AK/SK).
	fmtPastTime = integrationsvc.FmtPastTime
)

// SetIntegrationRegistry wires the authorization source registry, so error messages can look up
// display names.
//
// A setter instead of a Config field: the Feishu user provider in the registry depends on
// coreSvc, the construction order of the two is fixed, and the cycle can only be broken with a
// setter.
func (s *Service) SetIntegrationRegistry(reg *IntegrationRegistry) {
	s.integrations = reg
}

// integrationDisplayName looks up an authorization source's display name, falling back to the id.
func (s *Service) integrationDisplayName(providerID string) string {
	if s.integrations == nil {
		return providerID
	}
	p, err := s.integrations.Get(providerID)
	if err != nil {
		return providerID
	}
	return p.Name()
}

// agentAllowsIntegration reports whether this agent has a given authorization source enabled.
func agentAllowsIntegration(agent apimodel.Agent, id string) bool {
	for _, got := range agent.IntegrationIDs {
		if got == id {
			return true
		}
	}
	return false
}
