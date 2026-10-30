// Type aliases: these types have moved to model/apimodel. One line each is kept here so
// existing call sites do not need to change.
//
// # Why they moved
//
// The orchestration layer (server/orchestrator) declares narrow interfaces from the
// "consumer's point of view", and those signatures carry these types. They used to live in
// core, so the orchestration layer had to import core. **That edge is exactly what this
// layer is meant to remove**: once the orchestration layer depends on core, core shows up
// in `go list -deps ./orchestrator`, and "what a handler can call is decided by the
// interface list" stops being true right away.
//
// ⚠️ This comment used to say "the handler also imports the orchestration layer, which makes
// a cycle, and Go refuses to compile it". **Testing showed that is false**: core used its
// own local copy of the interfaces in handlerservice.go and never imported the orchestration
// layer, so both directions compiled back then. Later core did import the orchestration
// layer (`type Mux = orchestrator.Mux` in handler.go), and there was still no cycle.
// The real source of the constraint is the design, not the compiler. See the file header
// of orchestrator/routes.go for the rule.
//
// ✅ **The handler has already moved out of core** (it is now server/apihandler).
// This package no longer has any HTTP code, and the `type Mux = orchestrator.Mux` import
// went with it. **This package does not import the orchestration layer at all now**; the
// direction is one-way: apihandler → orchestrator.
//
// That move turned out smaller than estimated here: the handler code only needed eight
// alias symbols from core (all in this file and in integrationalias.go / skillpkgalias.go),
// and their real definitions are all on the domain / storemodel / service side.
// Pointing at the real definitions was enough; no new alias had to be made.
//
// # Why keep aliases instead of changing the call sites
//
// These types have 300+ references, most of them inside the core package. Aliases leave
// all those references untouched, so this change is a **pure move**: the diff only shows
// "a type definition moved to another file" and "a batch of aliases was added", and the
// review does not need to check "is this one changed correctly?" at every site.
//
// The repo already has a precedent: core.ErrNotFound = persistmodel.ErrNotFound (core/core.go).
//
// ⚠️ **An alias is the same type, not a new type**: `core.SandboxState` and
// `apimodel.SandboxState` are fully identical in the type system; you can assign one to the
// other and pass one where the other is expected. So aliases cause no runtime difference
// and do not affect JSON serialization.
//
// New code should write apimodel.X directly; these aliases exist only for existing call sites.
package core

import "github.com/chyroc/ama/server/model/apimodel"

// The external shape of a session container.
type (
	SandboxState       = apimodel.SandboxState
	SandboxListing     = apimodel.SandboxListing
	SandboxFile        = apimodel.SandboxFile
	SandboxFileContent = apimodel.SandboxFileContent
	SandboxShellResult = apimodel.SandboxShellResult
)

// Feedback. ⚠️ Not the same thing as Issue / Suggestion; see README-feedback.md.
type (
	FeedbackInput  = apimodel.FeedbackInput
	FeedbackCall   = apimodel.FeedbackCall
	FeedbackReport = apimodel.FeedbackReport
	FeedbackPage   = apimodel.FeedbackPage
)

// Input shapes for workspaces and members.
type (
	WorkspaceInput = apimodel.WorkspaceInput
	WorkspacePatch = apimodel.WorkspacePatch
	MemberInput    = apimodel.MemberInput
)

// Usage statistics.
type (
	UsageSummary = apimodel.UsageSummary
	AgentUsage   = apimodel.AgentUsage
	DailyUsage   = apimodel.DailyUsage
)

// User preferences.
type UserPreferences = apimodel.UserPreferences

// Onboarding.
type (
	OnboardingAction = apimodel.OnboardingAction
	OnboardingState  = apimodel.OnboardingState
)

// The two values of OnboardingAction.
//
// ⚠️ The constants must be kept too: an alias only bridges the type name; a value
// reference like `core.OnboardingComplete` does not follow it automatically.
const (
	OnboardingComplete = apimodel.OnboardingComplete
	OnboardingSkip     = apimodel.OnboardingSkip
)

// Ark console deep links and the authorization state for "get a key".
type (
	ArkConsoleRef  = apimodel.ArkConsoleRef
	ArkFetchStatus = apimodel.ArkFetchStatus
)

// Second batch: also plain data types, moved for exactly the same reasons as above.
type (
	ArkCredentialDraft = apimodel.ArkCredentialDraft
	ArkKeyRef          = apimodel.ArkKeyRef
	SkillCreateOptions = apimodel.SkillCreateOptions
	SkillFileContent   = apimodel.SkillFileContent
	SuggestionPage     = apimodel.SuggestionPage
)
