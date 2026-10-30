// Local execution entry point for KindTool, plus path helpers.
package main

import (
	"context"
	"os"
	"path/filepath"

	"github.com/chyroc/ama/server/shared/hosttools"
)

// hostWorkdirPrefix has the same value as core.hostWorkdir on the amad side: the prefix of the
// session directory relative to the daemon user's $HOME; cwd ends up as $HOME/.ama/runs/<sessionID>.
const hostWorkdirPrefix = ".ama/runs"

func hostWorkdirRel(sessionID string) string {
	return filepath.Join(hostWorkdirPrefix, sessionID)
}

// validSessionID blocks path traversal in sessionID: it is joined directly into the working
// directory relative to $HOME, and if it contained `../` or a path separator, cwd would escape
// ~/.ama/runs (for example landing in ~/.ssh), letting remotely sent calls read and write in an
// unexpected directory. Ark session ids only contain letters, digits, _ and -, so this is a
// whitelist.
func validSessionID(sessionID string) bool {
	if sessionID == "" || len(sessionID) > 128 {
		return false
	}
	for _, r := range sessionID {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// executeToolLocal runs one native tool call on the local machine.
//
// Before any tool runs, the session directory is ensured to exist: read/glob on a missing cwd would
// report "no such file/directory" directly, and the very first turn may well be a tool call (skill
// injection only happens when skills are bound), so the agent cannot be expected to mkdir itself.
func executeToolLocal(ctx context.Context, cwd string, call hosttools.ToolCall) hosttools.ToolResult {
	ensureWorkdir(cwd)
	return hosttools.ExecuteTool(ctx, cwd, call)
}

func ensureWorkdir(cwd string) {
	if cwd == "" {
		return
	}
	// Do not panic on failure: hosttools gives an actionable error itself (directory missing and so on).
	_ = os.MkdirAll(cwd, 0o755)
}

func boolPtr(b bool) *bool { return &b }
