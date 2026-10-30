// Native host implementations of glob / grep.
//
// The container version uses `find -printf` and GNU grep; the host version uses filepath.WalkDir +
// Go regexp (RE2). The output shape (relative paths, mtime descending, three grep modes, truncation
// notice) matches shelltools.
//
// It also removes a known difference in the container version: GNU grep's ERE and Go RE2 syntax are
// not fully equivalent (backreferences etc.). The native version uses RE2 like the SDK, so an invalid
// pattern reports a clear error instead of silently finding nothing.
package hosttools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// searchRoot decides the root directory of a search, with the same meaning as shelltools.searchRoot:
// the path argument first, then workdir, finally "/".
func searchRoot(workdir, reqPath string) (string, error) {
	if strings.TrimSpace(reqPath) != "" {
		return ResolvePath(workdir, reqPath)
	}
	if workdir != "" {
		return workdir, nil
	}
	return "/", nil
}

// globInput is the input of the glob tool.
type globInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
}

func runGlob(ctx context.Context, workdir string, raw json.RawMessage) ToolResult {
	var req globInput
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return ErrorResult("解析 glob 入参失败: %v", err)
		}
		if strings.TrimSpace(req.Pattern) == "" {
			return ErrorResult("pattern must not be empty")
		}
	}
	root, err := searchRoot(workdir, req.Path)
	if err != nil {
		return ErrorResult("%v", err)
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return ErrorResult("%s: no such directory", root)
	}
	re, err := globRegexp(req.Pattern)
	if err != nil {
		return ErrorResult("%v", err)
	}

	type match struct {
		path string
		mod  int64
	}
	var matches []match
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A subdirectory cannot be read (permissions etc.): skip it, same as the container
			// version's `2>/dev/null`.
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		name := filepath.ToSlash(d.Name())
		// Match both the "relative path" and the "file name": that way a pattern without a slash like
		// `*.go` matches at any depth, consistent with the SDK/container version.
		if !re.MatchString(rel) && !re.MatchString(name) {
			return nil
		}
		mod := int64(0)
		if info, ierr := d.Info(); ierr == nil {
			mod = info.ModTime().UnixNano()
		}
		matches = append(matches, match{path: rel, mod: mod})
		if len(matches) >= GlobMaxMatches {
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil && walkErr != fs.SkipAll && ctx.Err() == nil {
		return ErrorResult("glob 失败: %v", walkErr)
	}

	// mtime descending, ties by path ascending, consistent with the container version/SDK.
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].mod == matches[j].mod {
			return matches[i].path < matches[j].path
		}
		return matches[i].mod > matches[j].mod
	})

	var b strings.Builder
	for _, m := range matches {
		b.WriteString(m.path)
		b.WriteByte('\n')
	}
	if len(matches) >= GlobMaxMatches {
		b.WriteString("\n[truncated: first ")
		b.WriteString(strconv.Itoa(GlobMaxMatches))
		b.WriteString(" matches]\n")
	}
	return TextResult(b.String())
}

// grepInput is the input of the grep tool.
type grepInput struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path,omitempty"`
	GlobFilter      string `json:"glob_filter,omitempty"`
	CaseInsensitive bool   `json:"case_insensitive,omitempty"`
	OutputMode      string `json:"output_mode,omitempty"`
}

func runGrep(ctx context.Context, workdir string, raw json.RawMessage) ToolResult {
	var req grepInput
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return ErrorResult("解析 grep 入参失败: %v", err)
		}
	}
	if strings.TrimSpace(req.Pattern) == "" {
		return ErrorResult("pattern must not be empty")
	}
	root, err := searchRoot(workdir, req.Path)
	if err != nil {
		return ErrorResult("%v", err)
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return ErrorResult("%s: no such directory", root)
	}

	pattern := req.Pattern
	if req.CaseInsensitive {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return ErrorResult("%v", err)
	}
	var filter *regexp.Regexp
	if f := strings.TrimSpace(req.GlobFilter); f != "" {
		filter, err = globRegexp(f)
		if err != nil {
			return ErrorResult("%v", err)
		}
	}

	mode := req.OutputMode
	if mode == "" {
		mode = "content"
	}

	var content strings.Builder
	count := 0
	files := map[string]bool{}
	byteLimited := false
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if filter != nil && !filter.MatchString(rel) &&
			!filter.MatchString(filepath.ToSlash(d.Name())) {
			return nil
		}
		fileCount := scanFile(ctx, p, rel, re, mode, &content, &byteLimited)
		if fileCount > 0 {
			files[rel] = true
			count += fileCount
		}
		if byteLimited || count >= GrepMaxMatches {
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil && walkErr != fs.SkipAll && ctx.Err() == nil {
		return ErrorResult("grep 失败: %v", walkErr)
	}

	switch mode {
	case "files_with_matches":
		list := make([]string, 0, len(files))
		for f := range files {
			list = append(list, f)
		}
		sort.Strings(list)
		var b strings.Builder
		for _, f := range list {
			b.WriteString(f)
			b.WriteByte('\n')
		}
		return TextResult(b.String())
	case "count":
		return TextResult(strconv.Itoa(count) + "\n")
	default:
		out := content.String()
		if count > GrepMaxMatches || byteLimited {
			out += "\n[truncated: first " + strconv.Itoa(GrepMaxMatches) +
				" of " + strconv.Itoa(count) + " matches]\n"
		}
		return TextResult(out)
	}
}

// scanFile scans a file line by line and returns the number of matching lines; content mode writes
// lines into b (limited by the match count and the 100KiB output cap). Binary files are skipped, the
// same as GNU grep's -I.
func scanFile(ctx context.Context, path, rel string, re *regexp.Regexp, mode string,
	b *strings.Builder, byteLimited *bool) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()

	// Sniff the first 8KiB to detect binary: if it contains a NUL byte, do not search (grep -I's
	// heuristic). After sniffing, put the prefix back in front of the stream for the scanner with
	// MultiReader.
	head := make([]byte, 8192)
	n, _ := io.ReadFull(f, head)
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return 0
	}

	scanner := bufio.NewScanner(io.MultiReader(bytes.NewReader(head[:n]), f))
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	lineNo := 0
	fileCount := 0
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return fileCount
		default:
		}
		lineNo++
		line := scanner.Text()
		if !re.MatchString(line) {
			continue
		}
		fileCount++
		if mode == "content" && fileCount <= GrepMaxMatches {
			if b.Len() >= GrepMaxOutputB {
				*byteLimited = true
				continue
			}
			b.WriteString(rel)
			b.WriteByte(':')
			b.WriteString(strconv.Itoa(lineNo))
			b.WriteByte(':')
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return fileCount
}

// globRegexp translates a shell wildcard pattern into a regex, exactly matching the SDK's
// implementation: `**` crosses directories, `*` does not cross `/`, `?` is one character, and regex
// metacharacters are escaped.
func globRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		ch := pattern[i]
		switch ch {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(ch)
		default:
			b.WriteByte(ch)
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
