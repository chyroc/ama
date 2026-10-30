// Type aliases for the Ark-call domain: defined in service/arkrun; this is only a same-named entry.
//
// # Why aliases and not "another set of types in core"
//
// The real BindingKey / AgentSpec / CreatedAgent / Runner live in service/arkrun.
// On the core side, 10 files use them (chat / worker / arkconsole / skillark / agentsync
// and their tests), all keeping old names like `core.ArkBindingKey`, **because they are the
// same type**, not "two types that look the same".
//
// This is exactly the same approach as the sentinel errors (ErrInvalidArgument = storemodel's
// in core/core.go): aliases make it **impossible for the two sides to drift apart**. If this
// re-declared the structs instead, forgetting one side when adding a field would raise no error;
// values would just be silently truncated when passed across packages. That is exactly the shape
// recorded at the top of arkrun_test.go: "the ma.AgentSpec literal missed Skills".
//
// # ⚠️ The direction is one-way
//
// **core may import service/arkrun, not the other way round.** Check:
//
//	go list -deps ./service/arkrun | grep -c 'ama/server/core$'   # must be 0
//
// So do not add logic to this file: only aliases go here. To change behavior, go to service/arkrun.
//
// # ⚠️ These names are not part of the HTTP contract
//
// They are an internal translation layer between core and Ark, without a single json tag (the 8
// tags of ArkBinding are in model/persistmodel; they are the on-disk format of ark_bindings.doc
// and unrelated to this). So this rename does not affect any API; openapi.json has zero diff.
package core

import "github.com/chyroc/ama/server/service/arkrun"

// ArkRunner is all the external capability needed to "run one conversation on Ark".
//
// It is an interface so that Service tests need no network, for the same reason as ArkVerifier.
// The full method docs (each one carrying the failure it fixed) are on Runner in service/arkrun.
type ArkRunner = arkrun.Runner

// ArkBindingKey is the credentials and coordinates needed to access Ark.
type ArkBindingKey = arkrun.BindingKey

// ArkAgentSpec is the config used to create an Ark Agent, translated from apimodel.Agent.
type ArkAgentSpec = arkrun.AgentSpec

// ArkCreatedAgent is the result of creating an Ark Agent.
//
// Its SkillCount is "how many skills Ark confirmed as attached". Callers use it to decide whether
// they may write back the skill fingerprint. That distinction fixed a real failure where "the
// binding recorded false data and blocked its own repair path"; the reasoning is on CreatedAgent
// in service/arkrun.
type ArkCreatedAgent = arkrun.CreatedAgent

// LiveArkRunner is the ArkRunner that calls the real Ark.
type LiveArkRunner = arkrun.LiveRunner

// NewLiveArkRunner creates a runner that calls the real Ark.
//
// cmd/amad calls it during wiring (the ArkRunner field in main.go).
func NewLiveArkRunner() *LiveArkRunner { return arkrun.NewLive() }
