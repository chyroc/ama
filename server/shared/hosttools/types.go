// Package hosttools is the **native implementation** and wire shapes of the six tools on the host
// executor.
//
// # Why this package exists
//
// On the Docker container path, the six tools bash/read/write/edit/glob/grep work as "amad builds a
// GNU-dialect shell script → bash runs it in the container" (shared/infra/shelltools). A host
// (especially macOS) ships BSD versions of the commands and has no setsid, so running those scripts
// would first need brew install coreutils/findutils/util-linux.
//
// So the host path uses another protocol: amad sends **structured tool calls** (ToolCall), and the
// daemon uses this package's native Go implementation on the local machine to read and write files,
// walk directories, and fork the system's own /bin/bash. Zero third-party dependencies (standard
// library only), so it works on a Mac out of the box. The Docker container path still uses
// shelltools; the two do not affect each other.
//
// # Boundaries
//
// This package depends only on the standard library and sits at the same level as
// shared/hostproto: both the daemon (cmd/ama-cli) and amad can import it. The amad side only uses
// the shapes and flow logic ToolCall/ToolResult/SkillBundle/InjectSkills; the daemon side calls
// ExecuteTool to actually run things.
package hosttools

import (
	"context"
	"encoding/json"
	"fmt"
)

// Tool names. They must match the names sent by Ark's agent_toolset exactly.
const (
	NameBash  = "bash"
	NameRead  = "read"
	NameWrite = "write"
	NameEdit  = "edit"
	NameGlob  = "glob"
	NameGrep  = "grep"
)

// Internal operation names: only started by amad's skill-injection logic; Ark never sends these names.
const (
	// OpWriteFiles writes several files at once (a skill's extra files).
	OpWriteFiles = "__ama_write_files"
	// OpFetchArchive downloads a presigned zip, verifies it, and unpacks it (skills that come as an archive).
	OpFetchArchive = "__ama_fetch_archive"
)

// Size limits matching shelltools: the limits the two executors give the agent must have the same shape.
const (
	// MaxReadBytes is the per-file limit read/edit can handle (matches shelltools.maxFileBytes).
	MaxReadBytes = 2 << 20 // 2 MiB
	// MaxWriteBytes is the limit for one write (matches shelltools.maxWriteBytes).
	// On the native path the content goes through JSON, not argv, so there is no real
	// MAX_ARG_STRLEN 128KiB limit; 512 KiB is kept here only to keep the tool behavior the agent
	// already knows.
	MaxWriteBytes = 512 << 10 // 512 KiB

	// ReadDefaultLines / ReadMaxLineChars match the SDK and shelltools.
	ReadDefaultLines = 2000
	ReadMaxLineChars = 2000

	GlobMaxMatches = 1000
	GrepMaxMatches = 200
	// GrepMaxOutputB is the hard cap on the output body in content mode.
	GrepMaxOutputB = 100 << 10

	// MaxBashOutputBytes is the limit on one bash result sent back (shelltools.maxOutputBytes),
	// keeping the head; the actual process output also has a wider 4MiB capture cap on the daemon side.
	MaxBashOutputBytes = 64 * 1024

	// WriteFilesBatchBytes is the total limit on all file contents in one OpWriteFiles call.
	// Content goes through JSON (not shell argv), so the budget only needs to leave room within the
	// 8MiB whole-frame limit. 768 KiB is chosen: one frame including the JSON wrapping stays far below
	// 8MiB, yet it can fit a typical whole skill package in one go.
	WriteFilesBatchBytes = 768 << 10
)

// ToolCall is one tool call that amad sends to the daemon.
//
// Input is the raw tool_use input sent by Ark (the JSON for read/write/...), which the daemon parses
// by Name itself; for internal operations (Op*) Input is an input shape defined by this package
// (see inject.go).
type ToolCall struct {
	// Name takes a Name* / Op* constant.
	Name string `json:"name"`
	// Input is the tool's raw input.
	Input json.RawMessage `json:"input,omitempty"`
	// Envs is only for bash: the credential/session env merged by amad, overriding the environment
	// the daemon inherits. Other tools don't carry it.
	Envs map[string]string `json:"envs,omitempty"`
}

// ToolResult is the result of one tool call.
//
// Unlike the shell path, Text here is already the **final text shown to the agent** on the daemon
// side (the three-part exit_code, line numbers, and truncated notes are all put together), so amad
// only runs one credential-redaction pass and no longer interprets the exit code and stdout.
type ToolResult struct {
	Text    string `json:"text,omitempty"`
	IsError bool   `json:"isError,omitempty"`
}

// Doer is the ability to "send one tool call to the executor and get the result back", used by the
// amad-side flow logic. Local execution in the daemon uses ExecuteTool and does not go through this
// interface.
type Doer interface {
	Do(ctx context.Context, call ToolCall) (ToolResult, error)
}

// TextResult / ErrorResult have the same shape as the shelltools/SDK constructors.
func TextResult(text string) ToolResult {
	return ToolResult{Text: text}
}

func ErrorResult(format string, args ...any) ToolResult {
	if len(args) > 0 {
		return ToolResult{IsError: true, Text: fmt.Sprintf(format, args...)}
	}
	return ToolResult{IsError: true, Text: format}
}
