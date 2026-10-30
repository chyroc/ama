// Native host implementations of read / write / edit.
//
// Output format and failure messages match the container version in shelltools word for word
// (which in turn matches the SDK word for word): agents depend on these strings (read's line
// numbers, edit's "not unique" error, wrote N bytes).
// The only difference from shelltools: file reads and writes happen on the daemon's own machine
// directly through os, without building shell commands.
package hosttools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// readInput is the input of the read tool.
type readInput struct {
	FilePath  string `json:"file_path"`
	ViewRange []int  `json:"view_range,omitempty"`
	Offset    int    `json:"offset,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

func runRead(_ context.Context, workdir string, raw json.RawMessage) ToolResult {
	var req readInput
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return ErrorResult("解析 read 入参失败: %v", err)
		}
	}
	abs, err := ResolvePath(workdir, req.FilePath)
	if err != nil {
		return ErrorResult("%v", err)
	}

	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrorResult("%s: no such file or directory", abs)
		}
		return ErrorResult("%s: %v", abs, err)
	}
	// Only regular files are accepted, same meaning as regular file/regular empty file from
	// `stat -c %F` in the container version.
	if !info.Mode().IsRegular() {
		return ErrorResult("path is not a regular file")
	}
	if info.Size() > MaxReadBytes {
		return ErrorResult("file too large: %d bytes", info.Size())
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return ErrorResult("%s: %v", abs, err)
	}
	if !utf8.Valid(data) {
		return ErrorResult("binary file cannot be read directly")
	}
	return formatRead(string(data), req.ViewRange, req.Offset, req.Limit)
}

// writeInput is the input of the write tool.
type writeInput struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

func runWrite(_ context.Context, workdir string, raw json.RawMessage) ToolResult {
	var req writeInput
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return ErrorResult("解析 write 入参失败: %v", err)
		}
	}
	abs, err := ResolvePath(workdir, req.FilePath)
	if err != nil {
		return ErrorResult("%v", err)
	}
	if len(req.Content) > MaxWriteBytes {
		return ErrorResult("content too large: %d bytes", len(req.Content))
	}
	// Create parent directories automatically, same as mkdir -p in the container version.
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return ErrorResult("写文件失败: %v", err)
	}
	// O_TRUNC overwrites, same as `>` in shell; new files get 0644 (subject to umask).
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return ErrorResult("写文件失败: %v", err)
	}
	if _, err := f.WriteString(req.Content); err != nil {
		_ = f.Close()
		return ErrorResult("写文件失败: %v", err)
	}
	if err := f.Close(); err != nil {
		return ErrorResult("写文件失败: %v", err)
	}
	// The output uses the path from the input (not the resolved absolute path), matching the SDK.
	return TextResult("wrote " + strconv.Itoa(len(req.Content)) + " bytes to " + req.FilePath)
}

// editInput is the input of the edit tool.
type editInput struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

func runEdit(_ context.Context, workdir string, raw json.RawMessage) ToolResult {
	var req editInput
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return ErrorResult("解析 edit 入参失败: %v", err)
		}
	}
	if req.OldString == "" {
		return ErrorResult("old_string must not be empty")
	}
	abs, err := ResolvePath(workdir, req.FilePath)
	if err != nil {
		return ErrorResult("%v", err)
	}

	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrorResult("%s: no such file or directory", abs)
		}
		return ErrorResult("%s: %v", abs, err)
	}
	if !info.Mode().IsRegular() {
		return ErrorResult("path is not a regular file")
	}
	if info.Size() > MaxReadBytes {
		return ErrorResult("file too large: %d bytes", info.Size())
	}
	rawBytes, err := os.ReadFile(abs)
	if err != nil {
		return ErrorResult("读文件失败: %v", err)
	}
	if !utf8.Valid(rawBytes) {
		return ErrorResult("binary file cannot be edited")
	}
	content := string(rawBytes)

	count := strings.Count(content, req.OldString)
	if count == 0 {
		return ErrorResult("old_string not found in %s", req.FilePath)
	}
	// Error when not unique, same as the container version / SDK: let the agent give more
	// context instead of guessing which place to change.
	if count > 1 && !req.ReplaceAll {
		return ErrorResult(
			"old_string appears %d times in %s; pass replace_all=true or give a longer unique string",
			count, req.FilePath)
	}

	var updated string
	if req.ReplaceAll {
		updated = strings.ReplaceAll(content, req.OldString, req.NewString)
	} else {
		updated = strings.Replace(content, req.OldString, req.NewString, 1)
	}
	if len(updated) > MaxWriteBytes {
		return ErrorResult("edited content too large: %d bytes", len(updated))
	}

	// Keep the original file permission bits.
	if err := os.WriteFile(abs, []byte(updated), info.Mode().Perm()); err != nil {
		return ErrorResult("写回文件失败: %v", err)
	}

	replaced := 1
	if req.ReplaceAll {
		replaced = count
	}
	return TextResult("edited " + req.FilePath + " (" + plural(replaced, "occurrence") + " replaced)")
}
