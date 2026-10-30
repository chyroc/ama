// Native host implementation of bash: Go forks the system's own /bin/bash -lc directly.
//
// Why this is not an "external dependency": macOS and every Linux distribution ship with bash.
// We just no longer require GNU coreutils/findutils/util-linux from brew, and no longer insert
// gnubin into PATH. The agent sends a whole command with pipes / redirects / &&, which must be
// interpreted by a shell, so we still use `bash -lc` instead of splitting argv for exec.
//
// The process group and timeout handling is moved from cmd/ama-cli/daemon_exec.go: Setpgid makes
// the child the group leader; on timeout we SIGTERM the whole group, wait 3 seconds, then SIGKILL
// the whole group, so child processes started by bash are not left behind as orphans.
package hosttools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// bashPath is the shell on the host. macOS ships /bin/bash 3.2, and Linux has it too.
	bashPath = "/bin/bash"
	// BashTimeout is the default limit for one command, same as shelltools.execTimeout.
	BashTimeout = 120 * time.Second
	// killGrace is how long to wait after SIGTERM for the process to exit on its own.
	killGrace = 3 * time.Second
	// MaxProcOutput is the hard cap on output the daemon collects from one command, so a runaway
	// command cannot exhaust memory; the 64KiB truncation for the agent is done in formatBashResult.
	MaxProcOutput = 4 << 20
)

type bashInput struct {
	Command         string `json:"command"`
	Cmd             string `json:"cmd,omitempty"`
	TimeoutMS       int    `json:"timeout_ms,omitempty"`
	RunInBackground bool   `json:"run_in_background,omitempty"`
}

// ExecEnv is the injection point for the daemon process environment: os.Environ() by default,
// tests can replace it.
var ExecEnv = os.Environ

func runBash(ctx context.Context, workdir string, call ToolCall) ToolResult {
	var req bashInput
	if len(call.Input) > 0 {
		if err := json.Unmarshal(call.Input, &req); err != nil {
			return ErrorResult("解析 bash 入参失败: %v", err)
		}
	}
	command := strings.TrimSpace(req.Command)
	if command == "" {
		command = strings.TrimSpace(req.Cmd)
	}
	if command == "" {
		return ErrorResult("bash 入参缺少 command")
	}

	if req.RunInBackground {
		return startBackground(ctx, workdir, command, call.Envs)
	}

	timeout := BashTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	res := runProcess(ctx, ProcessSpec{
		Cwd:     workdir,
		Command: command,
		Envs:    call.Envs,
		Timeout: timeout,
	})
	if res.StartErr != "" {
		return ErrorResult("在宿主机执行失败: %s", res.StartErr)
	}
	return TextResult(formatBashResult(res))
}

// ProcessSpec is the input of one synchronous process run (also used internally for background tasks).
type ProcessSpec struct {
	Cwd     string
	Command string
	Envs    map[string]string
	Timeout time.Duration
	// When Background=true, Setsid detaches from the session and stdio is redirected to the given
	// file; output is not collected.
	Background bool
	StdoutFile string
	StderrFile string
}

// ProcessResult is the result of a process run.
type ProcessResult struct {
	PID      int
	ExitCode *int
	Stdout   string
	Stderr   string
	// TimedOut means the process was killed by our timeout.
	TimedOut bool
	// StartErr means the process did not start (bash missing, etc.); a non-zero exit is not reported here.
	StartErr string
}

// runProcess runs one bash -lc, with Setpgid and killing the whole group on timeout. When workdir
// is a relative path, the caller (daemon) anchors it to $HOME first; absolute paths are allowed here.
func runProcess(ctx context.Context, spec ProcessSpec) ProcessResult {
	cmd := exec.Command(bashPath, "-lc", spec.Command)
	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}
	cmd.Env = mergeEnv(ExecEnv(), spec.Envs)

	var stdout, stderr limitedBuffer
	var logFile *os.File
	if spec.Background {
		f, err := os.OpenFile(spec.StdoutFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return ProcessResult{StartErr: "打开后台任务日志失败: " + err.Error()}
		}
		logFile = f
		cmd.Stdout = f
		cmd.Stderr = f
		cmd.Stdin = nil // for /dev/null meaning see the Setpgid/Setsid part
	} else {
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		cmd.Stdin = bytes.NewReader(nil)
	}
	attr := &syscall.SysProcAttr{Setpgid: true}
	if spec.Background {
		// New session: the process detaches from the current controlling terminal, so the daemon
		// exiting will not send it SIGHUP.
		attr.Setsid = true
	}
	cmd.SysProcAttr = attr

	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return ProcessResult{StartErr: err.Error()}
	}
	pid := cmd.Process.Pid

	timedOutCh := make(chan struct{}, 1)
	// watchDone is closed after cmd.Wait() returns, so the timeout watcher exits right away.
	//
	// ⚠️ timer.Stop alone is not enough: Stop does not wake a goroutine that is in select on
	// timer.C. With only Stop, the watcher hangs until the outer ctx ends (one goroutine leaked
	// per run). Worse, once it wakes up on a later unrelated ctx.Done(), it calls killpg on this
	// pid that **ended long ago**. The PID may have been reused by the system for another process
	// group, which kills the wrong processes. watchDone makes the watcher exit for sure on the
	// normal end path; only a real timeout / cancel runs killpg.
	watchDone := make(chan struct{})
	var timer *time.Timer
	if spec.Timeout > 0 {
		timer = time.NewTimer(spec.Timeout)
		go func() {
			select {
			case <-watchDone:
				return
			case <-ctx.Done():
			case <-timer.C:
			}
			select {
			case timedOutCh <- struct{}{}:
			default:
			}
			terminateProcessGroup(pid)
		}()
	}

	runErr := cmd.Wait()
	// Close watchDone before stopping the timer: makes sure the watcher does not trigger killpg
	// after the command has ended.
	close(watchDone)
	if timer != nil {
		timer.Stop()
	}
	if logFile != nil {
		_ = logFile.Close()
	}
	timedOut := false
	select {
	case <-timedOutCh:
		timedOut = true
	default:
	}

	res := ProcessResult{
		PID:      pid,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		TimedOut: timedOut,
	}
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		res.ExitCode = &code
	}
	if runErr != nil && res.ExitCode == nil && !timedOut {
		res.StartErr = runErr.Error()
	}
	if timedOut {
		res.Stderr = strings.TrimSpace(res.Stderr + "\n命令执行超时（" +
			spec.Timeout.String() + "），进程组已被终止")
	}
	return res
}

// terminateProcessGroup sends SIGTERM, waits a grace period, then SIGKILLs the whole process group.
func terminateProcessGroup(pid int) {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		return // ESRCH: already exited, the normal case.
	}
	timer := time.NewTimer(killGrace)
	<-timer.C
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// mergeEnv overlays the per-call env onto the environment the daemon inherited.
func mergeEnv(base []string, override map[string]string) []string {
	if len(override) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(override))
	index := make(map[string]int, len(base)+len(override))
	put := func(k, v string) {
		if i, ok := index[k]; ok {
			out[i] = k + "=" + v
			return
		}
		index[k] = len(out)
		out = append(out, k+"="+v)
	}
	for _, e := range base {
		k, v, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		put(k, v)
	}
	for k, v := range override {
		put(k, v)
	}
	return out
}

// limitedBuffer has the same shape as the type of the same name in daemon_exec.go: past
// MaxProcOutput only the beginning is kept.
type limitedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	remaining := MaxProcOutput - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) String() string {
	s := b.buf.String()
	if b.truncated {
		s += "\n[输出超过 4 MiB 已截断，仅保留开头]"
	}
	return s
}

// formatBashResult builds the three-part text shown to the model, matching shelltools.formatResult
// word for word: exit_code, --- stdout ---, --- stderr ---, then truncated at 64KiB.
func formatBashResult(res ProcessResult) string {
	var b strings.Builder
	code := "(未退出)"
	if res.ExitCode != nil {
		code = strconv.Itoa(*res.ExitCode)
	}
	b.WriteString("exit_code: ")
	b.WriteString(code)
	b.WriteByte('\n')
	if out := strings.TrimRight(res.Stdout, "\n"); out != "" {
		b.WriteString("--- stdout ---\n")
		b.WriteString(out)
		b.WriteByte('\n')
	}
	if errOut := strings.TrimRight(res.Stderr, "\n"); errOut != "" {
		b.WriteString("--- stderr ---\n")
		b.WriteString(errOut)
		b.WriteByte('\n')
	}
	return truncateBash(b.String())
}
