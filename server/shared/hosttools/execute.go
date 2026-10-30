// ExecuteTool is the only entry point for running one structured tool call on the daemon side.
//
// workdir is the session working directory (an absolute path the daemon has anchored under $HOME);
// bash and file operations are relative to it. An unknown tool name returns an IsError result (no
// panic), so both the agent and the server can see "this version does not know it".
package hosttools

import (
	"context"
	"encoding/json"
)

// ExecuteTool runs one tool call on this machine.
func ExecuteTool(ctx context.Context, workdir string, call ToolCall) ToolResult {
	switch call.Name {
	case NameBash:
		return runBash(ctx, workdir, call)
	case NameRead:
		return runRead(ctx, workdir, call.Input)
	case NameWrite:
		return runWrite(ctx, workdir, call.Input)
	case NameEdit:
		return runEdit(ctx, workdir, call.Input)
	case NameGlob:
		return runGlob(ctx, workdir, call.Input)
	case NameGrep:
		return runGrep(ctx, workdir, call.Input)
	case OpWriteFiles:
		return runWriteFiles(ctx, workdir, call.Input)
	case OpFetchArchive:
		return runFetchArchive(ctx, workdir, call.Input)
	default:
		return ErrorResult("宿主机执行端不认识的工具: %q（请升级 ama daemon）", call.Name)
	}
}

// MarshalResult / UnmarshalResult do JSON encoding/decoding for the queue layer
// (host_dispatches.result).
func MarshalResult(res ToolResult) ([]byte, error) { return json.Marshal(res) }
func UnmarshalResult(b []byte) (ToolResult, error) {
	var r ToolResult
	err := json.Unmarshal(b, &r)
	return r, err
}
