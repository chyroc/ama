// shelltools is a layer that does not depend on the concrete executor (Docker container, host
// daemon, ...): the six tools bash/read/write/edit/glob/grep are all implemented as "generate a
// shell script → hand it to an Execer → format the result". They only know the neutral contract
// in this package and depend on no platform access package.
//
// dockertools connects Execer to docker exec; the host daemon path goes through NewNativeSet.
package shelltools

import (
	"context"
	"time"
)

// Execer is the command execution ability the six tools need.
//
// It is narrowed to an interface instead of taking a concrete implementation: tests can run
// without a real container / real daemon, and the same tools can sit on different executors.
// The dockertools session handle implements it directly.
type Execer interface {
	Exec(ctx context.Context, spec ExecSpec) (*Result, error)
}

// ExecSpec is the convenience input for running a command.
//
// It has no platform-specific fields; each executor maps it in its own way (Cmd/Env/WorkingDir/User
// for docker exec).
type ExecSpec struct {
	// Path is the executable path or command name. Required.
	Path string

	// Args is the argument list. Note it does not include Path itself (not the argv[0] style).
	Args []string

	// Cwd is the working directory. Empty means the executor's default directory.
	Cwd string

	// Timeout is the execution timeout of the process **on the executor side** (millisecond precision).
	//
	// It is not the same as the ctx timeout: this value is passed down to the executor to kill the
	// process, while ctx governs our side's HTTP request itself. To get "the command runs at most
	// 30 seconds", set this. Setting only ctx just makes the request time out and return, while
	// the process on the executor keeps running.
	Timeout time.Duration

	// Envs are extra environment variables.
	Envs map[string]string

	// Stdin is the standard input to write to the process.
	Stdin string

	// User sets the run-as identity. Empty means the executor's default user.
	User string
}

// Result is the result of running a command.
type Result struct {
	// PID is the process id.
	PID int
	// ExitCode is the exit code; nil while the process has not exited.
	ExitCode *int
	// Stdout / Stderr are the process output.
	Stdout string
	Stderr string
	// Err is an execution error reported by the executor daemon (not the process's stderr).
	Err string
}

// OK reports whether the command finished successfully (exit code 0 and no executor error).
func (r *Result) OK() bool {
	return r != nil && r.Err == "" && r.ExitCode != nil && *r.ExitCode == 0
}
