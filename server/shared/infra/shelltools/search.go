// glob / grep: search files and content in the executor (container / host).
//
// Output format matches the SDK's local implementation (arkruntime/toolset/search.go):
//   - glob: one path **relative to root** per line, newest mtime first (same mtime: by path ascending)
//   - grep: content mode gives `relative-path:line:content` per line; also files_with_matches and count
//
// Paths must be relative: that is what the SDK gives, and the agent feeds them straight into
// read/edit. Absolute paths would not cause an error, but the agent's next calls would carry a
// repeated prefix.
package shelltools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/volcengine/ark-runtime-go/arkruntime/toolset"
)

// Limits matching SDK DefaultLimits.
const (
	globMaxMatches   = 1000
	grepMaxMatches   = 200
	searchMaxOutputB = 100 << 10
)

// searchRoot decides the root directory of one search.
//
// Input path first, then workdir, then `/`. The return value serves both as "the directory to
// run in" and "the root used to trim the output prefix". They must be the same, otherwise the
// relative paths in the output have the wrong base.
func searchRoot(workdir, reqPath string) (string, error) {
	if strings.TrimSpace(reqPath) != "" {
		return resolvePath(workdir, reqPath)
	}
	if workdir != "" {
		return workdir, nil
	}
	return "/", nil
}

// ---------------------------------------------------------------------------
// glob
// ---------------------------------------------------------------------------

// GlobTool finds files by wildcard pattern in the container.
type GlobTool struct {
	sh      Execer
	workdir string
}

// NewGlobTool builds a glob tool inside the container.
func NewGlobTool(sh Execer, workdir string) *GlobTool {
	return &GlobTool{sh: sh, workdir: workdir}
}

// Name returns the tool name.
func (t *GlobTool) Name() string { return "glob" }

// Execute finds matching files, newest mtime first.
//
// # Why find -printf instead of the shell's globstar
//
// A pattern like `**/*.go` needs globstar turned on in the shell, and the behavior when nothing
// matches depends on the nullglob setting. The two options combine into four behaviors, and it
// is hard to be sure which one is the default in the container.
// find's -path matching is deterministic, and it gets the mtime for sorting in the same pass.
//
// Matching behaves like the SDK: **the pattern is tried against both the "relative path" and the
// "file name"** (the SDK line `!re.MatchString(rel) && !re.MatchString(name)`). So a pattern
// without a slash like `*.go` matches files at any depth, which is what the agent expects.
func (t *GlobTool) Execute(ctx context.Context, input json.RawMessage) toolset.Result {
	var req struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path,omitempty"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &req); err != nil {
			return errResult("解析 glob 入参失败: %v", err)
		}
	}
	if strings.TrimSpace(req.Pattern) == "" {
		return errResult("pattern must not be empty")
	}
	root, err := searchRoot(t.workdir, req.Path)
	if err != nil {
		return errResult("%v", err)
	}

	// -printf '%T@ %p\n' gives mtime and path; sorting is done locally.
	// Not `find -newer` and the like: that needs a reference file, which we would have to create first.
	//
	// The two -path branches match the SDK's "match the relative path or the file name":
	//   *pattern  → relative path (including directories)
	//   */pattern → file name only (also covers files directly under root)
	pattern := req.Pattern
	script := fmt.Sprintf(
		`cd %s 2>/dev/null || { echo "__NOROOT__" >&2; exit 2; }; `+
			`find . -type f \( -path %s -o -path %s -o -name %s \) -printf '%%T@ %%p\n' 2>/dev/null`,
		shQuote(root),
		shQuote("./"+strings.TrimPrefix(pattern, "./")),
		shQuote("*/"+pattern),
		shQuote(pattern))

	res, err := runShell(ctx, t.sh, "", script)
	if err != nil {
		return execFailure(err)
	}
	if exitCode(res) == 2 {
		return errResult("%s: no such directory", root)
	}

	type match struct {
		path string
		mod  float64
	}
	var matches []match
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		mod, _ := strconv.ParseFloat(line[:sp], 64)
		p := strings.TrimPrefix(line[sp+1:], "./")
		matches = append(matches, match{path: p, mod: mod})
	}

	// Newest mtime first, same mtime by path ascending: same as the SDK's sort.Slice.
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].mod == matches[j].mod {
			return matches[i].path < matches[j].path
		}
		return matches[i].mod > matches[j].mod
	})

	truncated := false
	if len(matches) > globMaxMatches {
		matches = matches[:globMaxMatches]
		truncated = true
	}

	var b strings.Builder
	for _, m := range matches {
		b.WriteString(m.path)
		b.WriteByte('\n')
	}
	if truncated {
		fmt.Fprintf(&b, "\n[truncated: first %d matches]\n", globMaxMatches)
	}
	return textResult(b.String())
}

// ---------------------------------------------------------------------------
// grep
// ---------------------------------------------------------------------------

// GrepTool searches content in the container.
type GrepTool struct {
	sh      Execer
	workdir string
}

// NewGrepTool builds a grep tool inside the container.
func NewGrepTool(sh Execer, workdir string) *GrepTool {
	return &GrepTool{sh: sh, workdir: workdir}
}

// Name returns the tool name.
func (t *GrepTool) Name() string { return "grep" }

// Execute searches content recursively.
//
// Three output_mode values, same as the SDK:
//   - content (default): `relative-path:line:content`
//   - files_with_matches: only file names, by path ascending
//   - count: total number of matches
//
// # Using grep -E instead of treating the pattern as a Go regex
//
// The SDK uses Go's regexp (RE2); the container has GNU grep. Their syntax is **not fully the
// same** (RE2 has no back-references, GNU ERE does; some character class forms differ too). We
// pick `grep -E` because the search must run in the container, where the files are. Most
// patterns behave the same on both sides; the rare ones that differ show up as "nothing found"
// rather than wrong results, and the agent can work around it by changing the pattern.
func (t *GrepTool) Execute(ctx context.Context, input json.RawMessage) toolset.Result {
	var req struct {
		Pattern         string `json:"pattern"`
		Path            string `json:"path,omitempty"`
		GlobFilter      string `json:"glob_filter,omitempty"`
		CaseInsensitive bool   `json:"case_insensitive,omitempty"`
		OutputMode      string `json:"output_mode,omitempty"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &req); err != nil {
			return errResult("解析 grep 入参失败: %v", err)
		}
	}
	if strings.TrimSpace(req.Pattern) == "" {
		return errResult("pattern must not be empty")
	}
	root, err := searchRoot(t.workdir, req.Path)
	if err != nil {
		return errResult("%v", err)
	}

	flags := "-rnE"
	if req.CaseInsensitive {
		flags += "i"
	}
	// --include is only added when glob_filter is given; -I skips binary files (the SDK does not
	// search binaries either).
	include := ""
	if f := strings.TrimSpace(req.GlobFilter); f != "" {
		include = " --include=" + shQuote(f)
	}

	script := fmt.Sprintf(
		`cd %s 2>/dev/null || { echo "__NOROOT__" >&2; exit 2; }; `+
			`grep %s -I%s -- %s . 2>/dev/null`,
		shQuote(root), flags, include, shQuote(req.Pattern))

	res, err := runShell(ctx, t.sh, "", script)
	if err != nil {
		return execFailure(err)
	}
	if exitCode(res) == 2 && strings.Contains(stderrOrErr(res), "__NOROOT__") {
		return errResult("%s: no such directory", root)
	}
	// grep exits with 1 when nothing matches. That is not an error: return an empty result.
	// Treating it as a failure would make the agent think the tool is broken and retry the same
	// search again and again.
	if exitCode(res) == 1 && strings.TrimSpace(res.Stdout) == "" {
		return emptyGrepResult(req.OutputMode)
	}

	return formatGrep(res.Stdout, req.OutputMode)
}

// emptyGrepResult is the output of each mode when nothing was found.
func emptyGrepResult(mode string) toolset.Result {
	if mode == "count" {
		return textResult("0\n")
	}
	return textResult("")
}

// formatGrep turns raw `./path:line:content` output into the shape each mode needs.
func formatGrep(stdout, mode string) toolset.Result {
	if mode == "" {
		mode = "content"
	}

	var b strings.Builder
	count := 0
	files := map[string]bool{}

	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// grep -rn output is `./a/b.go:12:content`. Only split on the first two colons:
		// the text may contain colons, and splitting more would cut the content.
		rest := strings.TrimPrefix(line, "./")
		i1 := strings.IndexByte(rest, ':')
		if i1 < 0 {
			continue
		}
		i2 := strings.IndexByte(rest[i1+1:], ':')
		if i2 < 0 {
			continue
		}
		file := rest[:i1]
		lineNo := rest[i1+1 : i1+1+i2]
		text := rest[i1+1+i2+1:]

		count++
		files[file] = true
		if mode == "content" && count <= grepMaxMatches && b.Len() < searchMaxOutputB {
			fmt.Fprintf(&b, "%s:%s:%s\n", file, lineNo, text)
		}
	}

	switch mode {
	case "files_with_matches":
		list := make([]string, 0, len(files))
		for f := range files {
			list = append(list, f)
		}
		sort.Strings(list)
		var fb strings.Builder
		for _, f := range list {
			fb.WriteString(f)
			fb.WriteByte('\n')
		}
		return textResult(fb.String())
	case "count":
		return textResult(fmt.Sprintf("%d\n", count))
	default:
		out := b.String()
		if count > grepMaxMatches {
			out += fmt.Sprintf("\n[truncated: first %d of %d matches]\n", grepMaxMatches, count)
		}
		return textResult(out)
	}
}
