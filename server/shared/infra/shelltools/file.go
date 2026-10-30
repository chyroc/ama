// read / write / edit: read and write files in the executor (container / host).
//
// The input and output formats of the three tools **match the SDK's local implementation word
// for word** (arkruntime/toolset/file.go): Ark sends tool_use by name, and the agent reads results
// in the SDK's output format. If the shape differs, the agent misjudges. For example, if read has
// no line numbers, the agent cannot use edit to point at an exact spot.
package shelltools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/volcengine/ark-runtime-go/arkruntime/toolset"
)

// readDefaultLines is the default line count when read gets no limit, same as the SDK.
const readDefaultLines = 2000

// readMaxLineChars is the max characters per line; longer lines are cut, same as the SDK.
const readMaxLineChars = 2000

// ---------------------------------------------------------------------------
// read
// ---------------------------------------------------------------------------

// ReadTool reads files in the container.
type ReadTool struct {
	sh      Execer
	workdir string
}

// NewReadTool builds a read tool inside the container.
func NewReadTool(sh Execer, workdir string) *ReadTool {
	return &ReadTool{sh: sh, workdir: workdir}
}

// Name returns the tool name. It must be "read": Ark sends calls by this name.
func (t *ReadTool) Name() string { return "read" }

// Execute reads a file.
//
// The input has three modes, same as the SDK:
//   - nothing given: return the whole file (no line numbers)
//   - view_range=[start,end]: return those lines (no line numbers)
//   - offset/limit: return that range, **with `%6d\t` line numbers**, possibly with a truncated
//     note at the end
//
// Whether line numbers are added is not arbitrary: the SDK does it this way and the agent depends
// on it. A whole-file read must be usable for direct comparison; a paged read needs line numbers
// to locate things.
func (t *ReadTool) Execute(ctx context.Context, input json.RawMessage) toolset.Result {
	var req struct {
		FilePath  string `json:"file_path"`
		ViewRange []int  `json:"view_range,omitempty"`
		Offset    int    `json:"offset,omitempty"`
		Limit     int    `json:"limit,omitempty"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &req); err != nil {
			return errResult("解析 read 入参失败: %v", err)
		}
	}
	abs, err := resolvePath(t.workdir, req.FilePath)
	if err != nil {
		return errResult("%v", err)
	}

	// Get "type + size + content" in one command, saving two round trips.
	// stat runs first: if it is not a regular file or is too big, the content should not be read.
	script := fmt.Sprintf(
		`p=%s; if [ ! -e "$p" ]; then echo "__NOENT__" >&2; exit 2; fi; `+
			`t=$(stat -c '%%F' "$p"); s=$(stat -c '%%s' "$p"); `+
			`if [ "$t" != "regular file" ] && [ "$t" != "regular empty file" ]; then echo "__NOTFILE__:$t" >&2; exit 3; fi; `+
			`if [ "$s" -gt %d ]; then echo "__TOOBIG__:$s" >&2; exit 4; fi; `+
			`base64 -w0 "$p"`,
		shQuote(abs), maxFileBytes)

	res, err := runShell(ctx, t.sh, t.workdir, script)
	if err != nil {
		return execFailure(err)
	}
	if exitCode(res) != 0 {
		return readFailure(res, abs)
	}

	data, err := decodeB64(res.Stdout)
	if err != nil {
		return errResult("解码文件内容失败: %v", err)
	}
	if !utf8.Valid(data) {
		return errResult("binary file cannot be read directly")
	}

	return formatRead(string(data), req.ViewRange, req.Offset, req.Limit)
}

// readFailure turns the failure markers from the container into errors close to the SDK's.
func readFailure(res *Result, abs string) toolset.Result {
	msg := stderrOrErr(res)
	switch {
	case strings.Contains(msg, "__NOENT__"):
		return errResult("%s: no such file or directory", abs)
	case strings.Contains(msg, "__NOTFILE__"):
		return errResult("path is not a regular file")
	case strings.Contains(msg, "__TOOBIG__"):
		size := strings.TrimPrefix(strings.Fields(msg)[0], "__TOOBIG__:")
		return errResult("file too large: %s bytes", size)
	}
	return errResult("读文件失败（exit=%d）: %s", exitCode(res), msg)
}

// formatRead formats content in the three modes, matching the SDK's ReadTool word for word.
func formatRead(content string, viewRange []int, offset, limit int) toolset.Result {
	if len(viewRange) > 0 {
		if len(viewRange) != 2 {
			return errResult("view_range must be [start_line, end_line]")
		}
		lines := strings.Split(content, "\n")
		start := viewRange[0] - 1
		if start < 0 {
			start = 0
		}
		if start >= len(lines) {
			return textResult("")
		}
		end := len(lines)
		if viewRange[1] > 0 && viewRange[1] < end {
			end = viewRange[1]
		}
		if end < start {
			return errResult("view_range end line %d is before start line %d", viewRange[1], viewRange[0])
		}
		return textResult(strings.Join(lines[start:end], "\n"))
	}

	// Nothing given: return the whole file without line numbers.
	if offset == 0 && limit == 0 {
		return textResult(content)
	}

	lines := strings.Split(content, "\n")
	// Drop the empty line caused by the trailing newline, otherwise the line count is one too many.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if offset < 0 {
		offset = 0
	}
	if offset > len(lines) {
		offset = len(lines)
	}
	if limit <= 0 {
		limit = readDefaultLines
	}
	end := offset + limit
	if end > len(lines) {
		end = len(lines)
	}

	var b strings.Builder
	for i := offset; i < end; i++ {
		line := lines[i]
		if utf8.RuneCountInString(line) > readMaxLineChars {
			line = string([]rune(line)[:readMaxLineChars])
		}
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, line)
	}
	if end < len(lines) {
		fmt.Fprintf(&b, "\n[truncated: showing lines %d-%d of %d]\n", offset+1, end, len(lines))
	}
	return textResult(b.String())
}

// ---------------------------------------------------------------------------
// write
// ---------------------------------------------------------------------------

// WriteTool writes files into the container.
type WriteTool struct {
	sh      Execer
	workdir string
}

// NewWriteTool builds a write tool inside the container.
func NewWriteTool(sh Execer, workdir string) *WriteTool {
	return &WriteTool{sh: sh, workdir: workdir}
}

// Name returns the tool name.
func (t *WriteTool) Name() string { return "write" }

// Execute writes a file (overwrite) and creates parent directories automatically.
//
// Content goes through base64 instead of plain `echo` or a heredoc: what the agent writes may
// contain quotes, backticks, `$(...)`, CRLF, and a tail that does not end with a newline. The
// shell would rewrite all of these. base64 is the only way to guarantee "bytes written == bytes
// received".
func (t *WriteTool) Execute(ctx context.Context, input json.RawMessage) toolset.Result {
	var req struct {
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &req); err != nil {
			return errResult("解析 write 入参失败: %v", err)
		}
	}
	abs, err := resolvePath(t.workdir, req.FilePath)
	if err != nil {
		return errResult("%v", err)
	}
	if len(req.Content) > maxWriteBytes {
		return errResult("content too large: %d bytes", len(req.Content))
	}

	encoded := base64.StdEncoding.EncodeToString([]byte(req.Content))
	// mkdir -p the parent directory: the SDK's local implementation also creates it (os.MkdirAll).
	// Otherwise the agent would have to call bash once first, one more round trip.
	script := fmt.Sprintf(
		`p=%s; mkdir -p "$(dirname "$p")" && printf '%%s' %s | base64 -d > "$p"`,
		shQuote(abs), shQuote(encoded))

	res, err := runShell(ctx, t.sh, t.workdir, script)
	if err != nil {
		return execFailure(err)
	}
	if exitCode(res) != 0 {
		return errResult("写文件失败（exit=%d）: %s", exitCode(res), stderrOrErr(res))
	}
	// Output format matches the SDK: wrote N bytes to <the path from the input> (not the resolved
	// absolute path).
	return textResult(fmt.Sprintf("wrote %d bytes to %s", len(req.Content), req.FilePath))
}

// ---------------------------------------------------------------------------
// edit
// ---------------------------------------------------------------------------

// EditTool does string replacement in the container.
type EditTool struct {
	sh      Execer
	workdir string
	read    *ReadTool
	write   *WriteTool
}

// NewEditTool builds an edit tool inside the container.
func NewEditTool(sh Execer, workdir string) *EditTool {
	return &EditTool{
		sh:      sh,
		workdir: workdir,
		read:    NewReadTool(sh, workdir),
		write:   NewWriteTool(sh, workdir),
	}
}

// Name returns the tool name.
func (t *EditTool) Name() string { return "edit" }

// Execute replaces old_string with new_string.
//
// # Why "read it back, replace locally, write it back" instead of sed
//
// sed needs regex metacharacters and the delimiter escaped, while old_string is **arbitrary
// text** (multi-line, contains `/`, `&`, backslashes). Turning it safely into a sed script needs
// escaping rules far more complex than doing the string replacement in Go, and a mistake silently
// writes wrong file content.
//
// The cost is one more round trip, and file size is limited by maxFileBytes / maxWriteBytes.
// In return, replacement behaves exactly like the SDK (including "error if not unique").
func (t *EditTool) Execute(ctx context.Context, input json.RawMessage) toolset.Result {
	var req struct {
		FilePath   string `json:"file_path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all,omitempty"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &req); err != nil {
			return errResult("解析 edit 入参失败: %v", err)
		}
	}
	if req.OldString == "" {
		return errResult("old_string must not be empty")
	}
	abs, err := resolvePath(t.workdir, req.FilePath)
	if err != nil {
		return errResult("%v", err)
	}

	// Read the original. Reuse read's command so the "size/type/encoding" checks are the same.
	script := fmt.Sprintf(
		`p=%s; if [ ! -f "$p" ]; then echo "__NOENT__" >&2; exit 2; fi; `+
			`s=$(stat -c '%%s' "$p"); if [ "$s" -gt %d ]; then echo "__TOOBIG__:$s" >&2; exit 4; fi; `+
			`base64 -w0 "$p"`,
		shQuote(abs), maxFileBytes)
	res, err := runShell(ctx, t.sh, t.workdir, script)
	if err != nil {
		return execFailure(err)
	}
	if exitCode(res) != 0 {
		return readFailure(res, abs)
	}
	raw, err := decodeB64(res.Stdout)
	if err != nil {
		return errResult("解码文件内容失败: %v", err)
	}
	if !utf8.Valid(raw) {
		return errResult("binary file cannot be edited")
	}
	content := string(raw)

	count := strings.Count(content, req.OldString)
	if count == 0 {
		return errResult("old_string not found in %s", req.FilePath)
	}
	// Error if not unique, same as the SDK: let the agent give a longer context itself,
	// instead of us guessing which spot to change.
	if count > 1 && !req.ReplaceAll {
		return errResult(
			"old_string appears %d times in %s; pass replace_all=true or give a longer unique string",
			count, req.FilePath)
	}

	var updated string
	if req.ReplaceAll {
		updated = strings.ReplaceAll(content, req.OldString, req.NewString)
	} else {
		updated = strings.Replace(content, req.OldString, req.NewString, 1)
	}

	if len(updated) > maxWriteBytes {
		return errResult("edited content too large: %d bytes", len(updated))
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(updated))
	writeScript := fmt.Sprintf(`printf '%%s' %s | base64 -d > %s`, shQuote(encoded), shQuote(abs))
	wres, err := runShell(ctx, t.sh, t.workdir, writeScript)
	if err != nil {
		return execFailure(err)
	}
	if exitCode(wres) != 0 {
		return errResult("写回文件失败（exit=%d）: %s", exitCode(wres), stderrOrErr(wres))
	}

	replaced := 1
	if req.ReplaceAll {
		replaced = count
	}
	return textResult(fmt.Sprintf("edited %s (%s replaced)", req.FilePath, plural(replaced, "occurrence")))
}

// plural builds "N xxx", adding s when N>1.
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
