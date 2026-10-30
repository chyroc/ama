// The **native** six-tool set for the host executor: no shell scripts are built. Ark's tool_use
// input is wrapped as-is into a hosttools.ToolCall and sent down, and the daemon runs it on the
// local machine with a native Go implementation (see the shared/hosttools package comment). So
// macOS no longer needs brew GNU tools.
//
// Credential/env injection and output scrubbing still go through SessionInjector (the same piece
// as the Docker executor): before each tool call, Prepare produces this session's env and the
// scrub function. env is only attached to bash calls (only bash starts child processes), and
// scrubbing covers the result text of all six tools.
package shelltools

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/volcengine/ark-runtime-go/arkruntime/toolset"

	"github.com/chyroc/ama/server/shared/hosttools"
)

// NativeDoer sends one structured tool call to the host daemon and gets the result back.
// hostNativeDoer in core/hosttool.go satisfies it (enqueues into host_dispatches, waits for the result).
type NativeDoer interface {
	DoNative(ctx context.Context, call hosttools.ToolCall) (hosttools.ToolResult, error)
}

// NewNativeSet builds a set of native six tools that run on the host daemon.
//
//   - workdir is the session working directory inside the executor (the host path is
//     .ama/runs/<sid>, which the daemon anchors at $HOME); relative paths in tool input are
//     based on it;
//   - localWorkdir is only used to satisfy SDK NewDefault (closed right after building); it must
//     be a path on the worker's own machine;
//   - injector provides credentials/env and scrubbing; nil means no injection;
//   - the host has no "idle pause", so no onIdle is attached.
func NewNativeSet(
	do NativeDoer, injector *SessionInjector, workdir, localWorkdir string, log *slog.Logger,
) (*toolset.Set, error) {
	mk := func(name string) toolset.Tool {
		return &nativeTool{do: do, inj: injector, name: name}
	}
	tools := []toolset.Tool{
		mk(hosttools.NameBash),
		mk(hosttools.NameRead),
		mk(hosttools.NameWrite),
		mk(hosttools.NameEdit),
		mk(hosttools.NameGlob),
		mk(hosttools.NameGrep),
	}
	return newSetFromTools(localWorkdir, tools, nil, log)
}

// nativeTool is a thin toolset.Tool: input is sent down as-is, and the reply becomes a toolset.Result directly.
type nativeTool struct {
	do   NativeDoer
	inj  *SessionInjector
	name string
}

func (t *nativeTool) Name() string { return t.name }

func (t *nativeTool) Execute(ctx context.Context, input json.RawMessage) toolset.Result {
	call := hosttools.ToolCall{Name: t.name, Input: input}

	// Fetch injection fresh on every call (the platform JWT expires; the Docker executor also
	// fetches once per Exec).
	var scrub func(string) string
	if t.inj != nil {
		envs, sc := t.inj.Prepare(ctx)
		if t.name == hosttools.NameBash {
			// Only bash starts child processes, so env goes only to it; the other tools run
			// directly in Go inside the daemon, and env would have no use there.
			call.Envs = envs
		}
		scrub = sc
	}

	res, err := t.do.DoNative(ctx, call)
	if err != nil {
		// This is "could not deliver the call to the host" (offline, enqueue failed, wait timed
		// out), not a failure of the tool itself. Express it with IsError so the agent sees the
		// reason and decides for itself.
		return toolset.Result{
			Content: []toolset.ContentBlock{{
				Type: "text", Text: "在宿主机执行失败: " + err.Error(),
			}},
			IsError: true,
		}
	}
	text := res.Text
	if scrub != nil {
		// ⚠️ The output of all six tools must be scrubbed: read/grep may read content with
		// credentials, and glob file names could in theory contain a secret string too. The
		// single-exit rule is the same as the Docker executor's (see scrub.go).
		text = scrub(text)
	}
	return toolset.Result{
		Content: []toolset.ContentBlock{{Type: "text", Text: text}},
		IsError: res.IsError,
	}
}
