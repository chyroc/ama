// Native implementation of background tasks (bash run_in_background).
//
// The file layout is **exactly the same** as the shell version in shelltools, and the task_id
// and tail/cat/kill hint text the agent gets are word for word the same too. The agent later
// checks the task with these bash commands:
//
//	/tmp/ama-bg/bg-<unixtime>-<pid>.log     output
//	/tmp/ama-bg/bg-<unixtime>-<pid>.pid     process id
//	/tmp/ama-bg/bg-<unixtime>-<pid>.status  exit code (appears only after the task ends)
//
// How it maps to the shell version: setsid → SysProcAttr{Setsid:true}; nohup → the new session
// is already detached from the controlling terminal (the daemon exiting does not send it
// SIGHUP) + stdio redirection + stdin /dev/null; the wrap-up shell
// `while kill -0; do sleep 1; done; echo $? > status` → one cmd.Wait() goroutine.
package hosttools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// BGLogDir is the directory where background task output is written, same value as
// shelltools.BGLogDir.
//
// It is in /tmp, not workdir: the agent may wipe workdir itself (rm -rf and start over is
// common), and background task output deleted along the way looks like "the task vanished".
const BGLogDir = "/tmp/ama-bg"

var bgMu sync.Mutex

// startBackground puts the command in the background and immediately returns a handle that
// can be queried.
//
// Why no extra tool is needed: the six tools share the same file system, so the agent can check
// with plain bash (tail/cat/kill); Ark only sends tool names it knows.
func startBackground(_ context.Context, workdir, command string, envs map[string]string) ToolResult {
	if err := os.MkdirAll(BGLogDir, 0o755); err != nil {
		return ErrorResult("后台启动失败: %v", err)
	}

	// The id has the same shape as the shell version: bg-<unixtime>-<pid>. The pid only exists
	// after Start, so start the process first and then set the file name. bgMu serializes the
	// two steps to avoid a clash of same second + same pid (pids do not repeat anyway; the lock
	// only makes the "create file → register reaper" window well defined).
	bgMu.Lock()
	defer bgMu.Unlock()

	logFile, err := os.CreateTemp(BGLogDir, ".bg-start-*.log")
	if err != nil {
		return ErrorResult("后台启动失败: %v", err)
	}
	tmpLog := logFile.Name()
	_ = logFile.Close()

	cmd := exec.Command(bashPath, "-lc", command)
	if workdir != "" {
		cmd.Dir = workdir
	}
	cmd.Env = mergeEnv(ExecEnv(), envs)
	// stdin is /dev/null (nil means /dev/null), stdout/stderr go to the log file.
	f, err := os.OpenFile(tmpLog, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return ErrorResult("后台启动失败: %v", err)
	}
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpLog)
		return ErrorResult("后台启动失败（%v）: %s", err, command)
	}
	// Close the fd on the daemon (parent) side: at Start the child already dup'ed its own
	// descriptor to keep writing the log, so the parent no longer needs this handle. Not
	// closing it leaves one open fd in the daemon per background task, and with many tasks
	// that hits the process fd limit.
	_ = f.Close()
	pid := cmd.Process.Pid
	id := "bg-" + strconv.FormatInt(time.Now().Unix(), 10) + "-" + strconv.Itoa(pid)
	logPath := filepath.Join(BGLogDir, id+".log")
	statusPath := filepath.Join(BGLogDir, id+".status")
	pidPath := filepath.Join(BGLogDir, id+".pid")

	if err := os.Rename(tmpLog, logPath); err != nil {
		// Do not kill the process when rename fails: the fd still holds the log, only the file
		// name is wrong. This almost never happens (rename within one directory); just say so.
		return ErrorResult("后台任务已启动但日志重命名失败: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return ErrorResult("后台任务已启动但 pid 文件写入失败: %v", err)
	}

	reapToFile(cmd, pid, statusPath)

	return TextResult(backgroundHint(id, strconv.Itoa(pid), logPath, statusPath))
}

// reapToFile waits in the background for the process to end and writes the exit code to the
// status file.
//
// Same behavior as the shell version's wrap-up loop, but without polling: cmd.Wait itself is
// "wait until reaped".
func reapToFile(cmd *exec.Cmd, pid int, statusPath string) {
	go func() {
		err := cmd.Wait()
		code := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			} else {
				code = 1
			}
		}
		_ = os.WriteFile(statusPath, []byte(strconv.Itoa(code)+"\n"), 0o644)
		_ = pid
	}()
}

// AdoptBackgroundJobs is called when the daemon starts: it scans leftover *.pid files and adds
// a reaper for each process still alive (after the daemon is killed or restarted for an
// upgrade, the exit codes of those orphan tasks must still be written to status, otherwise the
// agent's `cat status || echo RUNNING` shows RUNNING forever).
func AdoptBackgroundJobs() int {
	entries, err := os.ReadDir(BGLogDir)
	if err != nil {
		return 0
	}
	adopted := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".pid") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(BGLogDir, name))
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid <= 0 {
			continue
		}
		// Equivalent of kill -0: sending signal 0 only checks that the process exists.
		if err := syscall.Kill(pid, 0); err != nil {
			continue
		}
		id := strings.TrimSuffix(name, ".pid")
		statusPath := filepath.Join(BGLogDir, id+".status")
		if _, err := os.Stat(statusPath); err == nil {
			continue // already has an exit code, skip
		}
		// We are not its parent, so Wait cannot be used; poll kill -0 every 1s instead
		// (same pace as the shell version's wrap-up loop), so re-adopted tasks also get a status.
		go watchPid(pid, statusPath)
		adopted++
	}
	return adopted
}

// watchPid polls a process that is not our child and writes an exit code to status once it is
// gone.
//
// For a non-child the real exit code cannot be obtained (waitpid only works on children), so
// it writes 0, same as the shell version. The shell version's wrap-up loop is a **direct
// relative** of the background task (forked by the same daemon); after the daemon restarts
// nobody can wait on it, so this can only say "it ended".
func watchPid(pid int, statusPath string) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if err := syscall.Kill(pid, 0); err != nil {
			_ = os.WriteFile(statusPath, []byte("0\n"), 0o644)
			return
		}
	}
}

// backgroundHint matches the hint text of shelltools.BashTool.runBackground word for word.
func backgroundHint(id, pid, logPath, statusPath string) string {
	return fmt.Sprintf(`后台任务已启动（本次调用不等待它结束）。

task_id: %s
pid: %s
输出文件: %s
退出码文件: %s（任务结束后才出现）

查看进度：      bash 执行 `+"`tail -50 %s`"+`
判断是否结束：  bash 执行 `+"`cat %s 2>/dev/null || echo RUNNING`"+`
停止它：        bash 执行 `+"`kill %s`"+`

⚠️ 不要用 sleep 原地等它——先去做别的事，过一会儿再用上面两条命令看进度。
要是它比这一轮对话还长，就把 task_id 与查看方式一并告诉用户，由用户下次再问。`,
		id, pid, logPath, statusPath, logPath, statusPath, pid)
}
