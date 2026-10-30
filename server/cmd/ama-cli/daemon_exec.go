// The daemon runs process calls sent by amad (hostproto.ExecSpec) on the local machine.
//
// # Process groups and timeouts
//
// The six tools run long commands (bash timeout_ms up to 120s). A timeout must kill the whole
// process tree, otherwise child processes (python started by bash, etc.) become orphans and keep
// running. The approach is Setpgid, making the child the leader of a new process group; on timeout,
// SIGTERM the whole group first, give a 3-second grace, then SIGKILL the whole group. darwin and
// linux syscalls have the same semantics in these two places, so no per-platform files are needed.
//
// # Environment variables
//
// The daemon's own environment + the per-call env sent with the call (credentials / AMA_TOKEN /
// user-defined variables); the latter overrides keys with the same name. **Not written to the shell
// profile, not stored on disk**: these envs contain secrets and disappear when the process ends.
package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chyroc/ama/server/shared/hostproto"
)

// maxExecOutput is a protective hard cap against "a runaway command flooding output forever", so the
// daemon's memory does not blow up; the real 64KiB business truncation is done uniformly on the amad
// side, so this uses a loose 4MiB, keeping the beginning (error messages are usually at the front)
// and dropping the rest.
const maxExecOutput = 4 << 20

// killGrace is how long to wait after SIGTERM for the process to exit by itself.
const killGrace = 3 * time.Second

// maxDaemonExecTimeoutMS is the local hard cap on a single process timeout. It matches execWaitCap
// on the amad side (130s): the server waits at most that long, and the daemon should not let the
// process keep running after the server gave up.
const maxDaemonExecTimeoutMS int64 = 130_000

func execLocal(ctx context.Context, spec hostproto.ExecSpec) hostproto.ExecResult {
	cmd := exec.Command(spec.Path, spec.Args...)
	if spec.Cwd != "" {
		// The cwd sent by amad is a path relative to the daemon user's $HOME (.ama/runs/<sid>). In
		// service mode (launchd/systemd) the process's own cwd is `/` or a system directory, and
		// passing the relative path straight to cmd.Dir would resolve it relative to that directory,
		// or even fail outright (directory missing), so it is anchored to $HOME here; absolute paths
		// are kept as they are.
		cmd.Dir = resolveCwd(spec.Cwd)
	}
	cmd.Env = mergeEnviron(os.Environ(), spec.Envs)
	if spec.Stdin != "" {
		cmd.Stdin = bytes.NewBufferString(spec.Stdin)
	}
	var stdout, stderr limitedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Manage the process lifecycle ourselves (no exec.CommandContext): ctx only means amad cancelled,
	// while spec.TimeoutMS is the process timeout; both kill the whole process group.
	//
	// ⚠️ The timeout decision must use **a synchronization primitive independent of "the process
	// exited"**. An earlier version did "close(waitDone) after Wait returns, the kill goroutine races
	// waitDone in a select, and only writes the timeout into the channel if it wins"; processes like
	// sleep exit **immediately** on SIGTERM, so Wait almost always ran before the goroutine's SIGTERM
	// branch, close(waitDone) made the goroutine exit right away, and the timeout mark was lost
	// (TestExecLocalTimeout failed 5/5). Switched to context.AfterFunc: the fact that the timer fired
	// is recorded before the process exits, completely independent of who reaps first.
	timedOutCh := make(chan struct{}, 1)
	if err := cmd.Start(); err != nil {
		return hostproto.ExecResult{Err: err.Error(), Stderr: "启动进程失败: " + err.Error()}
	}
	var timer *time.Timer
	if spec.TimeoutMS > 0 {
		// When the timer fires, first put the timeout fact in the channel (buffered, never blocks),
		// then kill the process group. However fast the main goroutine's Wait returns, the fact is
		// already there.
		timer = time.NewTimer(time.Duration(spec.TimeoutMS) * time.Millisecond)
		go func() {
			select {
			case <-ctx.Done():
				// Cancelled from above (not this call's process timeout): kill too, but do not write timedOutCh.
			case <-timer.C:
				select {
				case timedOutCh <- struct{}{}:
				default:
				}
			}
			terminateProcessGroup(cmd)
		}()
		// Stop the timer after the process ends, so the normal path does not leak a goroutine.
		defer timer.Stop()
	}

	runErr := cmd.Wait()
	// Give the timer goroutine a definite end point: after Wait returns, if the timer fired,
	// timedOutCh already has a value; if not, defer timer.Stop() has stopped it. On the ctx-cancel
	// path the goroutine may still be in terminateProcessGroup, but the process no longer exists, so
	// ESRCH returns immediately.
	timedOut := false
	select {
	case <-timedOutCh:
		timedOut = true
	default:
	}
	res := hostproto.ExecResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		res.ExitCode = &code
		res.PID = cmd.ProcessState.Pid()
	}
	if runErr != nil {
		switch {
		case timedOut:
			res.Err = "timeout"
			res.Stderr = strings.TrimSpace(res.Stderr + "\n命令执行超时（" +
				time.Duration(spec.TimeoutMS).String() + "），进程组已被终止")
		case res.ExitCode == nil:
			// Err is only set when it did not start (executable missing, etc.); a non-zero exit is a normal process result.
			res.Err = runErr.Error()
		}
	}
	return res
}

// terminateProcessGroup kills the whole process group: SIGTERM first with killGrace to let it exit
// by itself, then SIGKILL as a fallback. The process having already exited (ESRCH) is normal and is
// swallowed.
func terminateProcessGroup(cmd *exec.Cmd) {
	if killProcessGroup(cmd, syscall.SIGTERM) {
		timer := time.NewTimer(killGrace)
		<-timer.C
		killProcessGroup(cmd, syscall.SIGKILL)
	}
}

// killProcessGroup sends a signal to the whole process group; returns false when the process group
// no longer exists (the child has exited), so the caller does not need to follow up with SIGKILL.
func killProcessGroup(cmd *exec.Cmd, sig syscall.Signal) bool {
	if cmd.Process == nil {
		return false
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		// If it cannot be killed, all we can do is wait for the next tick; no panic.
		return false
	}
	return true
}

type limitedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	remaining := maxExecOutput - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil // pretend to accept everything, so a broken pipe does not interrupt the child
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
		s += "\n[输出超过 " + humanBytes(maxExecOutput) + " 已截断，仅保留开头]"
	}
	return s
}

func humanBytes(n int) string {
	if n >= 1<<20 {
		return formatOneDecimal(float64(n)/(1<<20)) + " MiB"
	}
	return formatOneDecimal(float64(n)/(1<<10)) + " KiB"
}

func formatOneDecimal(v float64) string {
	// Simple inline code instead of pulling strconv into a bunch of call sites.
	whole := int(v)
	frac := int((v-float64(whole))*10 + 0.5)
	if frac == 0 {
		return itoa(whole)
	}
	return itoa(whole) + "." + itoa(frac)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// mergeEnviron merges environments: osEnv is the daemon's inherited environment, override is the
// per-call injection (credentials and so on), and the latter overrides keys with the same name. Not
// written to the shell profile, not stored on disk.
func mergeEnviron(osEnv []string, override map[string]string) []string {
	out := make([]string, 0, len(osEnv)+len(override))
	index := make(map[string]int, len(osEnv)+len(override))
	put := func(k, v string) {
		if i, ok := index[k]; ok {
			out[i] = k + "=" + v
			return
		}
		index[k] = len(out)
		out = append(out, k+"="+v)
	}
	for _, e := range osEnv {
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

// resolveCwd anchors a relative working directory to the daemon user's $HOME; absolute paths are
// returned as is. An empty string returns an empty string (letting exec use the default cwd).
func resolveCwd(cwd string) string {
	if cwd == "" || filepath.IsAbs(cwd) {
		return cwd
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return cwd // if HOME cannot be resolved, keep it as is and let exec report the error instead of guessing a path
	}
	return filepath.Join(home, cwd)
}
