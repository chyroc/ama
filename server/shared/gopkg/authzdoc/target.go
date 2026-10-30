// Resolving the output path. It is its own file because both the generator and the consistency
// test need it, and they run from different working directories.

package authzdoc

import (
	"fmt"
	"os"
	"path/filepath"
)

// RelPath is the output path, relative to the **repo root** (not server/).
//
// Hard-coded rather than a parameter: this file's location is part of the contract (the frontend
// imports it); making it configurable would create two sources of truth.
const RelPath = "web/src/data/roleCapabilities.ts"

// ResolveTarget finds the absolute path of the output file.
//
// # Why we cannot copy what gen-openapi does
//
// gen-openapi walks up to `go.mod` and lands in server/, and its output is under server/.
// Here the output is **under web/ at the repo root**, one level above go.mod, so the check must
// become "the level that has both server/go.mod and web/package.json"; that is the repo root.
//
// ⚠️ **Do not look only for `.git`**: in a worktree `.git` is a file, not a directory, and this
// repo often has several worktrees in use at the same time. Checking for the two marker files is
// correct in both cases.
func ResolveTarget() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	start := dir
	for {
		_, serverErr := os.Stat(filepath.Join(dir, "server", "go.mod"))
		_, webErr := os.Stat(filepath.Join(dir, "web", "package.json"))
		if serverErr == nil && webErr == nil {
			return filepath.Join(dir, RelPath), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("从 %s 往上没找到同时含 server/go.mod 与 web/package.json 的仓库根", start)
		}
		dir = parent
	}
}
