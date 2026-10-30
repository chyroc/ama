// Package shelltools runs the bash tool of the SDK self-hosted worker inside the execution
// backend (a Docker container or the host machine).
//
// # Why this layer exists
//
// The SDK mainline (feat(selfHosted): add self-hosted worker runtime) ships six tools:
// bash/read/write/edit/glob/grep. But they **run locally in the worker process**
// (Workdir / BashPath in `toolset.Options` are local paths). Using them as-is means the
// agent runs arbitrary shell inside our service process. That is not a sandbox.
//
// Luckily the SDK tool contract is tiny:
//
//	type Tool interface {
//	    Name() string
//	    Execute(ctx, input json.RawMessage) Result
//	}
//
// So we only need to implement this interface and forward commands to the execution
// backend. We keep the SDK poll/ack/heartbeat/tool_result protocol and still run the work
// inside a container.
//
// # How results get back to Ark
//
// None of the 8 self-hosted worker endpoints has a way to report results, and agent-side
// events are rejected. The SDK sends results back with the `user.tool_result` event, which
// is one of the 7 user-side events Ark has **always accepted**, so this file is built on
// top of the SDK.
package shelltools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/toolset"
)

// execTimeout is the time limit for a single command inside the container.
//
// This value is passed down to the execution backend, which uses it to kill the process
// (the Docker backend wraps the command as `timeout -k 5 <seconds>`). It is not the same as
// the ctx timeout: setting only the ctx timeout just makes us stop waiting, while the process
// in the container keeps running (see "Command timeout" in shared/infra/dockertools/README.md).
//
// ⚠️ **This is not the tightest layer.** There are four timeout layers on the whole chain.
// The HTTP client layer (60s) is tighter than this one, so a command that runs for 90
// seconds gets a network error, not this timeout. The full list of the four layers and
// "which one cuts first" is in "Four timeout layers" in the README.
const execTimeout = 120 * time.Second

// BGLogDir is the directory where background task output is written.
//
// It lives in /tmp, not the workdir: the agent may wipe the workdir itself (`rm -rf` and
// recreating the directory is a common move), and if the background task output gets
// deleted along with it, the task looks like it "vanished".
const BGLogDir = "/tmp/ama-bg"

// bgStartTimeout is the timeout for the act of "putting a task in the background" itself.
//
// It only covers fork + writing the pid file, which normally takes milliseconds. It has
// nothing to do with how long the started command can run. 20 seconds tolerates jitter
// right after the container resumes.
const bgStartTimeout = 20 * time.Second

// BGProbeTimeout is the timeout for "check whether any background task is running".
//
// This step runs right before pausing, so it must be short: if we cannot get an answer,
// treat it as "none" and pause as usual. One stall must not hold up the end of a whole turn.
const BGProbeTimeout = 10 * time.Second

// maxOutputBytes is the size limit for a single result sent back to Ark.
//
// We truncate instead of sending as-is: tool output can be tens of MB of logs. Putting all of
// it into user.tool_result would blow up a request and pollute the model context. The cut is
// at the **end** so the head is kept, because the key information in command output (errors,
// usage) is usually at the front.
const maxOutputBytes = 64 * 1024

// BackgroundMarker is an optional interface an Execer can implement. After a background task
// is started successfully, BashTool uses it to tell the execution backend "this session has
// started a background task". The backend then checks whether any task is still running in
// the container before the idle wrap-up (pausing freezes background processes, and resuming
// does not unfreeze them).
//
// An Execer that does not implement this interface works as usual; it just never gets this
// notice.
type BackgroundMarker interface {
	MarkBackgroundStarted()
}

// BashTool runs shell commands in the execution backend.
//
// The name must be "bash": Ark's agent_toolset_20260401 sends tool_use under this exact name.
// Renaming it makes the SDK fail to find the tool in the Set and return an error result
// "unknown tool: bash". That is treated as a tool execution failure, not a config problem,
// so it is hard to track down.
type BashTool struct {
	session Execer
	workdir string
}

// NewBashTool creates a bash tool that runs on the given execution backend session.
//
// An empty workdir means the container's default directory.
func NewBashTool(session Execer, workdir string) *BashTool {
	return &BashTool{session: session, workdir: workdir}
}

// Name returns the tool name.
func (t *BashTool) Name() string { return "bash" }

// Execute hands the command to the container.
//
// The input shape matches the SDK BashTool: `command` is the main field, `cmd` is a
// compatible alias. If both are empty, return an error result right away. **Do not return a
// Go error**: the SDK contract is "tool failures are also expressed as Result{IsError:true}".
// Returning an error fails the whole work item, while the agent should see "this call failed"
// and decide by itself whether to retry.
func (t *BashTool) Execute(ctx context.Context, input json.RawMessage) toolset.Result {
	var req struct {
		Command   string `json:"command"`
		Cmd       string `json:"cmd,omitempty"`
		TimeoutMS int    `json:"timeout_ms,omitempty"`
		// RunInBackground keeps long commands off the synchronous channel.
		//
		// **Neither Ark nor the SDK declares this field.** The model guessed it on its own (it
		// has seen this parameter on Claude Code's bash tool elsewhere). But the guess makes
		// sense: long commands (builds, releases, test runs) hitting the 60-second HTTP timeout
		// is a real pain point. Before this, the field was **silently ignored**: the model
		// thought it had started a background task and could return at once, but in fact it
		// waited synchronously and got cut off, and the failure did not point to the cause at
		// all. So we really implement it here; see runBackground.
		RunInBackground bool `json:"run_in_background,omitempty"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &req); err != nil {
			return errResult("解析 bash 入参失败: %v", err)
		}
	}
	command := strings.TrimSpace(req.Command)
	if command == "" {
		command = strings.TrimSpace(req.Cmd)
	}
	if command == "" {
		return errResult("bash 入参缺少 command")
	}

	if req.RunInBackground {
		return t.runBackground(ctx, command)
	}

	timeout := execTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}

	// Use `bash -lc`: the agent sends a whole shell command (with pipes, redirects, &&), which
	// must be interpreted by a shell. Splitting it into argv for exec would turn `a | b` into
	// literal arguments of a.
	res, err := t.session.Exec(ctx, ExecSpec{
		Path:    "bash",
		Args:    []string{"-lc", command},
		Cwd:     t.workdir,
		Timeout: timeout,
	})
	if err != nil {
		// This means "could not get the command into the container" (network, Session
		// closed), not that the command itself failed. We still express it with IsError so
		// the agent can see the reason and decide on its own.
		return errResult("%s", explainExecError(err, command))
	}

	return toolset.Result{
		Content: []toolset.ContentBlock{{Type: "text", Text: formatResult(res)}},
		IsError: !res.OK(),
	}
}

// runBackground puts the command in the background and returns a handle that can be queried
// right away.
//
// # Why it exists
//
// The tightest layer on the synchronous channel is the HTTP client timeout (see "Four timeout
// layers" in the README). Past it we get `Client.Timeout exceeded while awaiting headers`,
// while **the process in the container is still running**. The result: "the command is
// actually running, but the agent thinks it failed", so it retries, and the same release
// command runs twice. Background execution moves such commands off the synchronous channel
// and avoids the four timeout layers at the root.
//
// # Why no new companion tool is needed to query it
//
// All six tools share one file system (that is exactly why all of them run in the
// container). So once the output is in a file, the agent can check it with the existing
// `read` / `bash cat`. **A new tool would be worse**: Ark's `agent_toolset_20260401` only
// sends the names it knows. The model would not even know a tool we added exists, and we
// could only teach it through prompts, which is another chain that drifts.
func (t *BashTool) runBackground(ctx context.Context, command string) toolset.Result {
	// The task id is made from a timestamp + random number inside the container, not in Go:
	// this command can be started by any instance, and the id must match the file names in
	// the container, so letting the container produce it is simplest.
	//
	// setsid detaches the process from the current session, nohup blocks SIGHUP. Both are
	// needed: the execution backend may clean up the process group when an exec ends, and
	// nohup alone cannot stop that.
	script := fmt.Sprintf(`
set -u
mkdir -p %[1]s
id="bg-$(date +%%s)-$$"
log="%[1]s/$id.log"
status="%[1]s/$id.status"
: > "$log"
setsid nohup bash -lc %[2]s >> "$log" 2>&1 < /dev/null &
pid=$!
echo "$pid" > "%[1]s/$id.pid"
# Reaper process: after it exits, write the exit code to disk so readers can tell "still running" from "finished".
setsid nohup bash -c "while kill -0 $pid 2>/dev/null; do sleep 1; done; wait $pid 2>/dev/null; echo \$? > '$status'" >/dev/null 2>&1 < /dev/null &
echo "AMA_BG_ID=$id"
echo "AMA_BG_PID=$pid"
`, BGLogDir, shQuote(command))

	res, err := t.session.Exec(ctx, ExecSpec{
		Path:    "bash",
		Args:    []string{"-lc", script},
		Cwd:     t.workdir,
		Timeout: bgStartTimeout,
	})
	if err != nil {
		return errResult("%s", explainExecError(err, command))
	}
	if !res.OK() {
		return errResult("后台启动失败（exit_code=%d）: %s", exitCode(res), stderrOrErr(res))
	}

	id := parseKV(res.Stdout, "AMA_BG_ID")
	pid := parseKV(res.Stdout, "AMA_BG_PID")
	if id == "" {
		// No id means no way to get the result, so do not report "success". That would make
		// the agent think the task is running and then query an empty handle over and over.
		return errResult("后台任务已启动但没能取到任务 id，输出：%s", truncate(res.Stdout))
	}

	logPath := BGLogDir + "/" + id + ".log"
	statusPath := BGLogDir + "/" + id + ".status"

	// Tell the execution backend "this session has started a background task", so at idle
	// wrap-up it checks first before deciding whether to pause.
	//
	// ⚠️ **Do not skip this step**: the idle pause freezes background processes (see
	// hasRunningBackgroundJob in dockertools). If skipped, the background task silently
	// freezes after a turn ends, and when the agent comes back it only sees the log stopped
	// halfway. Zero errors, and nothing points to the pause.
	//
	// We use an optional interface instead of swapping Execer for a bigger one: BashTool takes
	// a fake Execer in unit tests, and changing the interface would force every test to
	// implement a method unrelated to bash. **The interface method must be exported**: the
	// backend that really implements this marker is in another package, and an unexported
	// method cannot satisfy an interface across packages.
	if m, ok := t.session.(BackgroundMarker); ok {
		m.MarkBackgroundStarted()
	}

	return textResult(fmt.Sprintf(`后台任务已启动（本次调用不等待它结束）。

task_id: %s
pid: %s
输出文件: %s
退出码文件: %s（任务结束后才出现）

查看进度：      bash 执行 `+"`tail -50 %s`"+`
判断是否结束：  bash 执行 `+"`cat %s 2>/dev/null || echo RUNNING`"+`
停止它：        bash 执行 `+"`kill %s`"+`

⚠️ 不要用 sleep 原地等它——先去做别的事，过一会儿再用上面两条命令看进度。
要是它比这一轮对话还长，就把 task_id 与查看方式一并告诉用户，由用户下次再问。`,
		id, pid, logPath, statusPath, logPath, statusPath, pid))
}

// parseKV takes the value from the `KEY=value` line in the output.
func parseKV(out, key string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, key+"="); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// formatResult turns the container's execution result into text for the model.
//
// All three parts are needed: the exit code decides how the model judges success, stderr is
// often the only failure reason, and the backend's own error (Result.Err) is not the same as
// the process stderr. Mixing them would make "the container did not start" and "the command
// failed" look the same.
func formatResult(res *Result) string {
	var b strings.Builder

	code := "(未退出)"
	if res.ExitCode != nil {
		code = fmt.Sprintf("%d", *res.ExitCode)
	}
	fmt.Fprintf(&b, "exit_code: %s\n", code)

	if res.Err != "" {
		fmt.Fprintf(&b, "--- sandbox error ---\n%s\n", res.Err)
	}
	if out := strings.TrimRight(res.Stdout, "\n"); out != "" {
		fmt.Fprintf(&b, "--- stdout ---\n%s\n", out)
	}
	if errOut := strings.TrimRight(res.Stderr, "\n"); errOut != "" {
		fmt.Fprintf(&b, "--- stderr ---\n%s\n", errOut)
	}

	return truncate(b.String())
}

// truncate cuts by bytes and says clearly how much was cut.
//
// Without saying so, the model would treat the cut point as the real end of the output and
// draw wrong conclusions from it.
func truncate(s string) string {
	if len(s) <= maxOutputBytes {
		return s
	}
	return s[:maxOutputBytes] + fmt.Sprintf("\n\n…（输出过长，已截断，共 %d 字节）", len(s))
}
