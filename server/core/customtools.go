// Worker-side custom tools: the provider interface, plus the two nil-safe helpers every call site
// uses.
//
// # Declaring and executing are two separate things; this file connects them
//
// Ark has to know the tool exists before the model will call it, and the worker side has to
// register an implementation under the same name before a call has anywhere to land. Missing
// either half fails in a completely different way:
//
//   - declared but not implemented: Ark pushes `agent.custom_tool_use` down, the runner does not
//     find the name in CustomTools, treats it as "not my tool" and skips it, and the work hangs on
//     requires_action;
//   - implemented but not declared: the model never learns the tool exists, **never calls it, and
//     nothing reports an error**.
//
// So there is exactly one wiring path: core.Config.CustomTools (the consumer-side interface) →
// Service field →
//   - declarations: arkSpecFor(a, s.customToolDecls()), shared by all three paths (create Agent /
//     catch up / verify catch-up);
//   - execution: p.svc.customToolsForWork(sessionID, workID, logger), passed to
//     selfhosted.SessionToolRunnerOptions.CustomTools.
//
// # Why the interface lives in core instead of importing datasources
//
// Same approach as ArkVerifier / BlobStore: the consumer declares what it needs and cmd/amad plugs
// the concrete implementation in. core does not need to know whether the datasource is MySQL or
// TOS, or how its credentials are configured.
package core

import (
	"log/slog"

	"github.com/volcengine/ark-runtime-go/arkruntime/toolset"

	"github.com/chyroc/ama/server/shared/infra/ma"
)

// CustomToolProvider declares custom tools to Ark and hands out their worker-side implementations.
//
// The two methods must expose the same set of names (`Declarations()` Name == `ToolsFor()` map
// key), otherwise one half silently has no counterpart. The concrete implementation
// (shared/infra/datasources.Registry) guarantees this by using one config and one set of constants,
// and unit tests pin it down.
type CustomToolProvider interface {
	// Declarations returns the Ark-side tool declarations; nil means "no custom tool".
	//
	// They must be **stable**: ArkSpecFingerprint feeds them into the Agent fingerprint, so a
	// different result on every call means one extra UpdateAgent per conversation. That is why the
	// datasource inventory (table names / object keys) is probed once at startup.
	Declarations() []ma.CustomToolSpec

	// ToolsFor returns the tool implementations for one work item.
	//
	// A new map per work item (not shared process-wide): the tool closures carry sessionID / workID
	// so audit lines can name the session they belong to. The SDK's CustomTools is a process-level
	// map; the caller works around that, see customToolsForWork.
	ToolsFor(sessionID, workID string, logger *slog.Logger) map[string]toolset.Tool
}

// customToolDecls returns the declarations to put into an Ark Agent spec.
//
// Nil-safe: with no datasource configured it returns nil, arkSpecFor leaves spec.CustomTools unset,
// and the Agent sent to Ark is byte-for-byte what it was before this feature (no empty array that
// would clear existing tools).
func (s *Service) customToolDecls() []ma.CustomToolSpec {
	if s.customTools == nil {
		return nil
	}
	return s.customTools.Declarations()
}

// customToolsForWork returns the tool implementations bound to this work item.
//
// A fresh map per work item; see CustomToolProvider.ToolsFor. With no datasource configured it
// returns nil, so the runner's CustomTools is nil and behavior is exactly the same as before.
func (s *Service) customToolsForWork(sessionID, workID string, logger *slog.Logger) map[string]toolset.Tool {
	if s == nil || s.customTools == nil {
		return nil
	}
	if logger == nil {
		logger = s.log()
	}
	return s.customTools.ToolsFor(sessionID, workID, logger)
}
