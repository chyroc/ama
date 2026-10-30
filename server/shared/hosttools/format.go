package hosttools

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// formatRead formats file content in one of three modes, matching shelltools.formatRead and
// the SDK character for character:
//
//   - view_range=[start,end]: return those lines, without line numbers
//   - nothing given: return the whole file, without line numbers (the agent compares it
//     against old_string)
//   - offset/limit: return that range, with `%6d\t` line numbers, possibly followed by a
//     truncated notice
//
// "With or without line numbers" is not arbitrary: the agent relies on it. A whole-file read
// is for comparing, a paged read is for locating.
func formatRead(content string, viewRange []int, offset, limit int) ToolResult {
	if len(viewRange) > 0 {
		if len(viewRange) != 2 {
			return ErrorResult("view_range must be [start_line, end_line]")
		}
		lines := strings.Split(content, "\n")
		start := viewRange[0] - 1
		if start < 0 {
			start = 0
		}
		if start >= len(lines) {
			return TextResult("")
		}
		end := len(lines)
		if viewRange[1] > 0 && viewRange[1] < end {
			end = viewRange[1]
		}
		if end < start {
			return ErrorResult("view_range end line %d is before start line %d",
				viewRange[1], viewRange[0])
		}
		return TextResult(strings.Join(lines[start:end], "\n"))
	}

	// Nothing given: return the whole file, without line numbers.
	if offset == 0 && limit == 0 {
		return TextResult(content)
	}

	lines := strings.Split(content, "\n")
	// Drop the empty line left by a trailing newline, otherwise the line count is one too many.
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
		limit = ReadDefaultLines
	}
	end := offset + limit
	if end > len(lines) {
		end = len(lines)
	}

	var b strings.Builder
	for i := offset; i < end; i++ {
		line := lines[i]
		if utf8.RuneCountInString(line) > ReadMaxLineChars {
			line = string([]rune(line)[:ReadMaxLineChars])
		}
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, line)
	}
	if end < len(lines) {
		fmt.Fprintf(&b, "\n[truncated: showing lines %d-%d of %d]\n", offset+1, end, len(lines))
	}
	return TextResult(b.String())
}

// truncateBash truncates bash output by bytes and says plainly how much was cut.
//
// The wording is exactly the same as shelltools.truncate: without the notice, the model would
// take the cut point as the real end of the output and draw wrong conclusions from it.
func truncateBash(s string) string {
	if len(s) <= MaxBashOutputBytes {
		return s
	}
	return s[:MaxBashOutputBytes] +
		fmt.Sprintf("\n\n…（输出过长，已截断，共 %d 字节）", len(s))
}
