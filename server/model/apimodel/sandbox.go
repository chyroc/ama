// Public shape of the session container: state, file list, file content, shell exec result.
//
// Moved from core so the orchestration layer's signatures do not need to import core (that would be a cycle: core holds the
// HTTP handlers, which import the orchestration layer in turn). **Only the location changed**; fields and json tags
// are unchanged.

package apimodel

// SandboxState is the result of one state query, put directly into the HTTP response.
type SandboxState struct {
	// Status is one of four states; see the constants above.
	Status string `json:"status"`
	// SessionID / SandboxID have values in the three states other than none, for debugging.
	//
	// **Has omitempty**: they do not exist for none anyway; sending an empty string would only make the frontend write
	// one more "empty string also means absent" check.
	SessionID string `json:"sessionId,omitempty"`
	SandboxID string `json:"sandboxId,omitempty"`
	// CreatedAt is when the container was created, an ISO string (the NowISO shape).
	CreatedAt string `json:"createdAt,omitempty"`
	// ExpiresAt is when the container expires. The Docker executor manages by "reclaim after idle for how long", with no fixed
	// expiry time, so it is always empty now; the field is kept so the frontend types do not break.
	ExpiresAt string `json:"expiresAt,omitempty"`
	// PausedAt only has a value when paused.
	PausedAt string `json:"pausedAt,omitempty"`
	// CanResume says whether this container can be resumed.
	//
	// A separate field instead of letting the frontend infer from `status == "paused"`: behind paused there may be pause, stop,
	// or "the container was deleted but the workspace is still there"; whether it can resume is up to the executor.
	CanResume bool `json:"canResume"`
}

// SandboxListing is the result of one directory listing.
type SandboxListing struct {
	// Status is the container's four-state status. **When it is not running, Files is always empty**:
	// the frontend must check it before rendering the list, otherwise "container paused" shows as "directory is empty".
	Status string `json:"status"`
	// Path is the directory actually listed (after normalization); it may differ from the requested one.
	Path string `json:"path"`
	// Files is never nil. **An empty directory must return [], not null**: the frontend calls .map() directly,
	// and null would throw (the list endpoint convention in core/README.md).
	Files []SandboxFile `json:"files"`
	// Truncated being true means entries were cut off; see sandboxListMaxEntries.
	Truncated bool `json:"truncated"`
}

// SandboxFile is one entry returned by a directory listing.
type SandboxFile struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Type is "file" or "directory", taken directly from the platform.
	//
	// ⚠️ **Do not compute the type from the Mode bits yourself.** The Unix file type is an **enum** (4 bits),
	// not flag bits: `mode & S_IFLNK` is true for every regular file, so computing it that way would mark the whole directory
	// as symlinks. The platform already gives Type; use it.
	Type string `json:"type"`
	Size int64  `json:"size"`
	// Permissions is the human-readable form like "rwxr-xr-x", given directly by the platform.
	Permissions string `json:"permissions,omitempty"`
	ModTime     string `json:"modTime,omitempty"`
	// SymlinkTarget only has a value when this entry is a symlink.
	SymlinkTarget string `json:"symlinkTarget,omitempty"`
}

// SandboxFileContent is the content of one file, put directly into the HTTP response.
type SandboxFileContent struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	// Content is the text content. Binary files get no content; see Binary.
	Content string `json:"content"`
	// Truncated being true means only the first sandboxFileMaxBytes bytes were read.
	Truncated bool `json:"truncated"`
	// Binary being true means this is not a text file, and Content is empty.
	//
	// **A separate field instead of putting "this is binary" into Content**: otherwise the frontend cannot tell
	// "the file content happens to be that sentence" from "we decided it is binary".
	Binary bool `json:"binary"`
}

// SandboxShellResult is the result of one webshell command, put directly into the HTTP response.
type SandboxShellResult struct {
	// Status is the container's four-state status. **When it is not running, Output is always empty**;
	// the frontend must check it first (the container may be paused or reclaimed between two commands).
	Status string `json:"status"`
	// Output is the command output (stdout + stderr merged; a PTY cannot separate them anyway).
	Output string `json:"output"`
	// ExitCode is the exit code; nil while the command is still running.
	//
	// ⚠️ **Has omitempty; it is an existing exception to hard rule three** (listed in
	// allowedOmitEmpty in jsontag_test.go). The frontend reads `exitCode?: number`: while the command has not finished
	// this key **does not exist**, which is different from "finished with exit code 0". Without omitempty,
	// nil would serialize to `"exitCode": null`, and `null == 0` is false under JS loose comparison,
	// while `?? 0` turns it into 0: the two styles give different answers. Not emitting the key at all is the hardest to misuse.
	//
	// This tag is **an existing production contract**; this change only moved the type from core to domain,
	// without changing a single byte.
	ExitCode *int `json:"exitCode,omitempty"`
	// ShellStatus is the execution status from the platform (completed / hard_timeout and the like), for debugging.
	ShellStatus string `json:"shellStatus,omitempty"`
}
