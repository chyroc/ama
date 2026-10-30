// idleHook turns the SDK's "tool set closed" into a "this session round is idle" callback.
//
// # Where this signal comes from
//
// The SDK's EnvironmentWorker finishes one work item like this (see the SDK's worker.go):
//
//	tools, owned, err := w.toolsFor(toolEnv)
//	if owned {
//	    defer func() { _ = tools.Close() }()   // ← the signal we want
//	}
//	...
//	for runner.Next() { ... }                  // exits only after end_turn plus MaxIdle of idle time
//
// So `Set.Close()` happens right after "this conversation round has fully stopped", and before
// closing the SDK calls `drainInFlight()` to wait for all running tool calls to finish. So the
// callback **can never fire in the middle of a command**.
// With ToolsFunc, owned is always true, so this defer always runs.
//
// # Why it is a "fake tool"
//
// `Set.Close()` only acts on members that implement `ClosableTool` (Tool + Close), so to get this
// signal we must put a member into the Set. This member does no work: Name is a name Ark will
// never send, so Execute is never called.
//
// Why not implement Close on BashTool: the six tools share one executor, and any of them may be
// the last. Hanging it on one of them means betting on map iteration order (Set.Close walks all
// members; several members each pausing once is idempotent but logs several lines). A separate
// sentinel does not depend on adding or removing tools, and fires only once.
package shelltools

import (
	"context"
	"encoding/json"

	"github.com/volcengine/ark-runtime-go/arkruntime/toolset"
)

// idleHookName is the name the sentinel takes in the Set.
//
// The `__ama` prefix is so that it **never collides with a tool name sent by Ark**: if it did,
// when Ark calls that tool it would get an empty result, and the agent would only see "the tool
// did not respond", with no way to trace it back here.
// Ark tool names are plain lowercase words like bash/read/write, never shaped like this.
const idleHookName = "__ama_idle_hook"

// idleHook is the sentinel member in the Set, there only to get the idle signal through Close.
//
// When onIdle is nil, Close just returns nil: not every executor needs to do something when idle
// (a host daemon may have no notion of "pause").
type idleHook struct {
	onIdle func() error
}

// newIdleHook builds an idle sentinel.
func newIdleHook(onIdle func() error) *idleHook {
	return &idleHook{onIdle: onIdle}
}

// Name returns a name Ark will never send; see idleHookName.
func (h *idleHook) Name() string { return idleHookName }

// Execute should never be called. If it really is called, the name collided; say so clearly
// instead of silently returning empty.
func (h *idleHook) Execute(_ context.Context, _ json.RawMessage) toolset.Result {
	return errResult("%s 是内部占位工具，不该被调用（名字撞了？见 idle.go）", idleHookName)
}

// Close is the real entry point: the SDK calls it when a conversation round ends idle.
//
// Whatever the callback returns is returned upward. The Docker executor's callback chooses to
// always return nil (a failed pause only means no resources were saved; returning an error would
// only make the SDK log a noisy line). That is the executor's own trade-off.
func (h *idleHook) Close() error {
	if h.onIdle == nil {
		return nil
	}
	return h.onIdle()
}
