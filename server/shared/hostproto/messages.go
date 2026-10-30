// Package hostproto defines the HTTP messages between amad and the ama daemon on users' host
// machines.
//
// # Why its own package
//
// These structs are referenced by both sides:
//
//	amad (cmd/amad)        server: registration, heartbeat long polling, result collection
//	ama daemon (cmd/ama-cli) client: a resident process on the local machine
//
// They are in the same Go module, but the daemon must not depend on amad's business packages
// (service/host and core would drag in the whole Ark/PG stack, defeating the point of a CLI
// binary). So the wire structs live here and depend only on the standard library. Execution
// parameters are also defined here (ExecSpec/ExecResult) instead of reusing shared/infra/shelltools:
// that package depends on the Ark SDK's toolset, and the daemon only needs the JSON shape of "run a
// process".
//
// # Communication model
//
// The daemon only makes outbound connections (users' machines are usually behind NAT and should not
// open inbound ports). There are two parallel paths, both supported by the server forever:
//
//	WebSocket (preferred by new daemons; falls back to HTTP below when the handshake/keepalive fails):
//	  GET  /api/host-daemon/ws        host_token authenticated via the Authorization handshake header;
//	                                 after upgrade, hello/dispatches/results frames go both ways;
//	                                 frame shapes are in ws.go
//	HTTP (old daemons and the WS fallback period use this path):
//	  POST /api/host-daemon/register   user identity (AMA_TOKEN) auth; idempotent registration; returns host_token
//	  POST /api/host-daemon/heartbeat  host_token auth; long polling; the response body carries calls to run
//	  POST /api/host-daemon/results    host_token auth; batches exec results and terminal upstream frames back
//
// Both paths share the same queue semantics, and the source of truth for correctness is the PG
// host_dispatches table ((host_id, seq) monotonic): WS frames and heartbeat responses only "move
// rows from the database to the daemon". See repository/schema/host_dispatches.sql for protocol
// details.
package hostproto

import (
	"encoding/json"

	"github.com/chyroc/ama/server/shared/hosttools"
)

// The maximum hold time of heartbeat long polling. The server returns immediately when there is a
// new dispatch, otherwise holds until this limit and returns empty. 20s: clearly shorter than the
// common 60s gateway idle disconnect, and the daemon does not wait long to reconnect.
const HeartbeatLongPoll = 20

// RegisterRequest is the message for the daemon's first registration (or re-registration after the
// local credentials file changed).
//
// It uses **user identity** (Authorization: Bearer AMA_TOKEN and the other two usual auth
// channels), not host_token: host_token is exactly what this call obtains. The owner is always the
// email the server resolves from the auth context; a self-claimed owner in the body is not
// accepted.
type RegisterRequest struct {
	// HostUUID is a random uuid generated on the machine on the daemon's first start (persisted in
	// ~/.ama/host.json). Later processes on the same machine carry the same uuid, and the server uses
	// it to idempotently hit the same row.
	HostUUID string `json:"hostUuid"`
	// Name is the host name, only shown to users.
	Name string `json:"name"`
	// OS / Arch are runtime.GOOS / runtime.GOARCH (darwin/linux, amd64/arm64).
	OS   string `json:"os"`
	Arch string `json:"arch"`
	// DaemonVersion is the ama CLI version (the version injected at build time).
	DaemonVersion string `json:"daemonVersion"`
	// GNUToolsOK / ToolCheck are probe results from old daemons (the GNU-dialect script protocol).
	// The new protocol (KindTool) no longer needs brew GNU tools; these two fields are only for
	// registration/heartbeats of old daemons, and the new code's gate only looks at NativeTools.
	GNUToolsOK bool   `json:"gnuToolsOk,omitempty"`
	ToolCheck  string `json:"toolcheck,omitempty"`
	// NativeTools means the daemon supports the structured native tool protocol (KindTool). Running
	// tools on old daemons without it is rejected by the server outright with "please upgrade ama
	// daemon".
	NativeTools bool `json:"nativeTools"`
}

// RegisterResponse returns the host id and (only the first time) the plaintext host_token on success.
type RegisterResponse struct {
	HostID string `json:"hostId"`
	// Token is only non-empty when first issued. The daemon must immediately save it into
	// ~/.ama/host.json with chmod 600; the server only stores the sha256 and can never get the
	// plaintext back. When an existing host registers again (restart, upgrade) it is empty, and the
	// daemon keeps using its local copy.
	Token string `json:"token,omitempty"`
	// AlreadyRegistered=true means an existing host row was hit, and no new token was issued this time.
	AlreadyRegistered bool `json:"alreadyRegistered"`
}

// HeartbeatRequest is sent about every 20s and does three jobs: renew the online state, advance the
// fetch cursor, and refresh the host snapshot (when the daemon runs as a system service it only has
// host_token and no user AMA_TOKEN, and after an upgrade it no longer goes through register, so
// changes in version/GNU probing are carried by heartbeats; empty fields mean "same as last time",
// and the server does not overwrite them).
type HeartbeatRequest struct {
	HostID string `json:"hostId,omitempty"`
	// LastSeq is the delivery cursor the daemon has persisted: the server only returns queued rows
	// with seq greater than it, plus delivered rows whose claimed_until has expired (redelivery after
	// a crash/lost result, independent of seq).
	LastSeq int64 `json:"lastSeq"`

	// The five below are optional snapshot refreshes (only needed when the current value differs from registration).
	Name          string `json:"name,omitempty"`
	OS            string `json:"os,omitempty"`
	Arch          string `json:"arch,omitempty"`
	DaemonVersion string `json:"daemonVersion,omitempty"`
	GNUToolsOK    *bool  `json:"gnuToolsOk,omitempty"`
	Toolcheck     string `json:"toolcheck,omitempty"`
	// When NativeTools is non-nil, it refreshes the host's native protocol capability bit.
	NativeTools *bool `json:"nativeTools,omitempty"`
}

// Dispatch kind values.
const (
	// KindExec is one old-style shell process call; the payload is ExecSpec, and results go through
	// ExecResults. New daemons still accept it (compatibility with old servers), but new amad code only
	// sends KindTool.
	KindExec = "exec"
	// KindTool is one structured native tool call (the six tools + internal skill-injection
	// operations); the payload is hosttools.ToolCall, the daemon runs it locally with a native Go
	// implementation, and results go through ToolResults.
	KindTool = "tool"
	// The four kinds of web terminal frames; the payload is the matching frame JSON; no execution result, the daemon just acks.
	KindTermOpen   = "term_open"
	KindTermInput  = "term_input"
	KindTermResize = "term_resize"
	KindTermClose  = "term_close"
)

// Dispatch is one action sent to the daemon (the wire form of one host_dispatches row).
type Dispatch struct {
	ID  string `json:"id"`
	Seq int64  `json:"seq"`
	// Kind: see the Kind* constants.
	Kind string `json:"kind"`
	// SessionID is the Ark session id (== run id); for exec, the working directory and injection are isolated by it.
	SessionID string `json:"sessionId,omitempty"`
	// TerminalID is the browser terminal id a terminal frame belongs to.
	TerminalID string `json:"terminalId,omitempty"`
	// Payload: for KindExec it is ExecSpec JSON; for KindTool it is hosttools.ToolCall JSON; for
	// term_* it is the frame JSON. The daemon parses it by Kind.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// HeartbeatResponse is the long polling response. Empty Calls means "no new actions during this time".
type HeartbeatResponse struct {
	Calls []Dispatch `json:"calls"`
	// With Offline=true the daemon should stop running new tasks (the host has been
	// revoked/disabled). Currently only used to give a gentler exit signal than a bare 401; a deleted
	// host goes down the 401 path.
	Revoked bool `json:"revoked,omitempty"`
}

// ExecSpec is the parameters of one tool process call (same shape as shelltools.ExecSpec, defined
// separately here to cut the daemon's dependency on the Ark SDK).
type ExecSpec struct {
	// Path/Args are the process to run; on the six-tool path it is always bash -lc <script>.
	Path string   `json:"path"`
	Args []string `json:"args,omitempty"`
	// Cwd is the working directory, usually ~/.ama/runs/<sessionID>; the daemon makes sure it exists.
	Cwd string `json:"cwd,omitempty"`
	// TimeoutMS is the process timeout; when it hits, the daemon kills the whole process group. 0 means the daemon default.
	TimeoutMS int64 `json:"timeoutMs,omitempty"`
	// Envs are only for this one child process; not written to the shell profile, not stored on disk (contains credentials; do not log them).
	Envs map[string]string `json:"envs,omitempty"`
	// Stdin is written to the process's standard input.
	Stdin string `json:"stdin,omitempty"`
}

// ExecResult is the result after the process ends (same shape as shelltools.Result).
type ExecResult struct {
	PID int `json:"pid,omitempty"`
	// ExitCode is the process exit code; may be nil when killed by a signal / failed to start.
	ExitCode *int   `json:"exitCode,omitempty"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	// Err is a "the process did not run properly" error (on the daemon side), not the process's stderr.
	Err string `json:"err,omitempty"`
}

// ExecResultItem returns the result of one exec call. CallID is Dispatch.ID.
type ExecResultItem struct {
	CallID string     `json:"callId"`
	Result ExecResult `json:"result"`
}

// ToolResultItem returns the result of one KindTool call, with the same CAS semantics as ExecResultItem.
type ToolResultItem struct {
	CallID string               `json:"callId"`
	Result hosttools.ToolResult `json:"result"`
}

// Terminal upstream frame types (daemon → amad).
const (
	TermFrameOutput = "output"
	TermFrameError  = "error"
	TermFrameClosed = "closed"
)

// TermFrame is one frame of terminal output reported by the daemon.
type TermFrame struct {
	TerminalID string `json:"terminalId"`
	// Type takes a TermFrame* constant.
	Type string `json:"type"`
	// Data is the close reason (UTF-8 text) for closed/error; for output frames **the new protocol no
	// longer uses it** to carry bytes: PTY output is arbitrary binary (possibly invalid UTF-8), and as
	// a JSON string it would be rewritten by Go's string encoding / proxies in between. Output bytes
	// go in DataB64.
	Data string `json:"data,omitempty"`
	// DataB64 is the standard base64 (StdEncoding, with padding) of the raw PTY bytes of an output
	// frame. amad decodes it and writes the raw bytes to the browser WebSocket (the browser's xterm
	// handles the encoding itself). Only used by output frames; when empty, amad falls back to
	// reading Data (compatibility with old daemons).
	DataB64 string `json:"dataB64,omitempty"`
}

// ResultsRequest is a batch upload. The three kinds of content are unrelated; one POST may carry only some of them.
type ResultsRequest struct {
	HostID string `json:"hostId"`
	// ExecResults is idempotent: the server does a CAS by callID, and repeated uploads do not overwrite the first result.
	ExecResults []ExecResultItem `json:"execResults,omitempty"`
	// ToolResults are the results of KindTool, with the same idempotency rule as ExecResults.
	ToolResults []ToolResultItem `json:"toolResults,omitempty"`
	// TermFrames are not stored; they are relayed directly to the instance holding the browser's SSE/WS connection.
	TermFrames []TermFrame `json:"termFrames,omitempty"`
	// AckSeqs confirm term_* frames have been consumed (the server may mark them done for purge to
	// clean up); completion of exec/tool is decided by their own results, not here.
	AckSeqs []int64 `json:"ackSeqs,omitempty"`
}

// Err codes used when an exec/tool call fails early on the amad side (the daemon never ran it).
const (
	// ExecErrHostOffline: the host heartbeat timed out; the call was not sent or the result is unknown.
	ExecErrHostOffline = "host_offline"
	// ExecErrNoGNUTools: macOS missing GNU tools under the old protocol. The new protocol no longer
	// produces this code; it is kept for recognizing results from old daemons.
	ExecErrNoGNUTools = "no_gnu_tools"
	// ExecErrDaemonUpgrade: the daemon on the host is too old and does not support the native tool protocol.
	ExecErrDaemonUpgrade = "daemon_upgrade_required"
	// ExecErrEnqueue: an internal error in the enqueue/wait stage.
	ExecErrEnqueue = "enqueue_failed"
)

// FailedExecResult builds the result JSON for "the tool did not run on the host machine".
//
// It goes through the same decode path as an ExecResult returned normally by the daemon
// (host_dispatches.result). ExitCode is left nil (the process never started; making up an exit
// code would be a lie), Err holds a machine-checkable code, and stderr holds a Chinese explanation
// for the agent/user. The six tools turn Err+stderr into an IsError result.
func FailedExecResult(code, message string) []byte {
	b, _ := json.Marshal(ExecResult{Err: code, Stderr: message})
	return b
}
