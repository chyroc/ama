// Shared basics for running inside the execution backend (Docker container / host daemon):
// command execution, path checks, and result building.
//
// # Why all six tools must run on the same execution backend
//
// The SDK's built-in read/write/edit/glob/grep run **locally in the worker process**. If only
// bash is moved to the remote side, the two sides see different file systems: the agent
// writes a file with bash, then reads it with read, and cannot find it (or worse, reads a
// file with the same name on the worker machine). This really happened: the agent failed to
// create a directory with `mkdir`, switched to the write tool, and the file landed on the
// worker's local disk.
//
// That kind of mismatch raises no error; it only makes the agent draw wrong conclusions and
// keep retrying. So all six tools must share one file system, which means all of them run on
// the same execution backend.
package shelltools

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/toolset"
)

// Time and output limits for commands inside the execution backend.
const (
	// toolExecTimeout is the timeout for a single command. File tools are all short
	// operations, so this is shorter than bash's 120s: a read that takes two minutes means
	// something else is wrong.
	toolExecTimeout = 60 * time.Second

	// maxFileBytes is the per-file size limit that read/edit can handle.
	//
	// It matches the SDK default (toolset.DefaultLimits), and it **must exist**: file content
	// goes through command output as base64 once, and a file of hundreds of MB would blow up
	// the backend's response, showing up as a confusing network error.
	maxFileBytes = 2 << 20 // 2 MiB

	// maxWriteBytes is the limit for a single write.
	//
	// It is smaller than read's: the content is base64-encoded and put on the command line,
	// and a single command has a kernel limit. Going over it gives "argument list too long",
	// an error that does not point to "the file is too big" at all.
	//
	// ⚠️ **This value is too high; the real limit is about 96 KiB.** The constraint is
	// `MAX_ARG_STRLEN` (32 pages = 128 KiB, for a **single** argv element), **not `ARG_MAX`**
	// (2 MiB, which is the **total** of all arguments plus environment variables). Mixing up
	// the two has already caused a production outage on the skill injection path; see
	// maxInjectBatchBytes in skills.go.
	//
	// So content between 96 KiB and 512 KiB is **accepted and then fails**. A real fix needs
	// chunked writes for write too (first chunk `>`, later chunks `>>`, as injectSkills already
	// does). Only lowering this number would turn "large files cannot be written" from an
	// occasional failure into a certain rejection.
	maxWriteBytes = 512 << 10 // 512 KiB (⚠️ about 96 KiB actually usable, see above)
)

// runShell runs a shell command in the execution backend.
//
// Always use `bash -lc`: commands contain pipes, redirects and `&&`, which must be
// interpreted by a shell. Splitting into argv for exec would turn `a | b` into literal
// arguments of a.
func runShell(ctx context.Context, sh Execer, workdir, script string) (*Result, error) {
	return runShellTimeout(ctx, sh, workdir, script, toolExecTimeout)
}

// runShellTimeout is the same as runShell, but lets the caller set the timeout of this command.
//
// # Why this exists: ExecSpec.Timeout is the limit on the backend side; context cannot control it
//
// ⚠️ **Only widening the context deadline does not help.** We hit this (code review caught
// it): the skill package download had a 5-minute `context.WithTimeout`, while runShell
// hard-coded `ExecSpec.Timeout` to toolExecTimeout (60 seconds). So downloads longer than
// 60 seconds **were still killed by the backend**, and the "5 minutes" in the comment was
// false.
//
// The symptom was "large skills randomly fail to install on slow links", and the error came
// from the backend, not pointing at all to "we set the timeout to 60 seconds ourselves".
//
// Both timeouts must be given together: ExecSpec.Timeout controls the backend side, and the
// context controls our side's waiting.
func runShellTimeout(
	ctx context.Context, sh Execer, workdir, script string, timeout time.Duration,
) (*Result, error) {
	return sh.Exec(ctx, ExecSpec{
		Path:    "bash",
		Args:    []string{"-lc", script},
		Cwd:     workdir,
		Timeout: timeout,
	})
}

// shQuote wraps a string as a single-quoted shell literal.
//
// **Every string from the agent must go through this.** Paths, patterns and file content can
// all contain spaces, `$(...)`, backticks, `;`. Pasting them straight into a command leaves
// the door open to injection, and the resulting command also fails in many odd ways (for
// example a path with a space is treated as two arguments).
//
// Inside single quotes everything except the single quote itself is literal, so we only need
// to replace `'` with `'\”`.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// resolvePath normalizes a path from the agent into an absolute path inside the execution
// backend.
//
// Relative paths are resolved against workdir; when workdir is empty, against `/`. On the
// container path, workdir is `/workspace` (docker.workdir), so `./skills/x/SKILL.md` from the
// agent resolves to `/workspace/skills/x/SKILL.md`, which is where skills are injected.
//
// # Why we do **not** restrict absolute paths to workdir
//
// There used to be an "out of bounds check" here: when workdir was non-empty, any resolved
// path not under workdir got the error "path %q is outside working directory %q". That check never fired while
// workdir was an empty string for a long time. But once workdir was pinned to `/workspace`
// (so that `./skills/…` matches, see provider.go), it **immediately locked the five file
// tools into the home directory**: reading `/opt/skills`, `/etc`, or a repo the agent cloned
// somewhere else was all rejected. Observed for real:
// `read /mnt/skills/ark-tls/SKILL.md` returned "outside the working directory /workspace".
//
// And it does not stop someone who really wants to go outside: **the bash tool never goes
// through this function** (it hands the whole command to the backend's shell), so
// `cat /etc/passwd` always worked. So the net effect of this check was not "safer", but
// **made read/write/edit/glob/grep inconsistent with bash in what they can do**: the same file
// can be read by bash but not by read, and the agent just concludes the read tool is broken
// and works around it with bash.
//
// The real isolation boundary is the execution backend itself (session containers exist
// exactly to run untrusted code), not a string comparison here. So this function only decides
// "how relative paths are resolved", not "whether access is allowed".
//
// ⚠️ This does not mean giving up path checks: **the skill injection side still strictly
// blocks `..` and absolute paths** (safeSkillPath / safeSkillDir in skills.go). That side
// blocks **untrusted entry names** inside a zip, which is input an attacker can control. This
// side handles paths the agent sends itself, and the agent can do the same with bash anyway.
//
// No symlink resolution: where a symlink in the backend points is up to the backend, and we
// cannot judge it from outside.
func resolvePath(workdir, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("路径不能为空")
	}
	if !path.IsAbs(p) {
		base := workdir
		if base == "" {
			base = "/"
		}
		p = path.Join(base, p)
	}
	return path.Clean(p), nil
}

// textResult / errResult are equivalent to the SDK's TextResult / ErrorResult.
//
// We write our own instead of using the SDK ones: those two live in the toolset package, and
// our tools return toolset.Result, so using its constructors directly would create a
// back-and-forth in imports. The shape must be exactly the same, because this is the
// structure the agent receives.
func textResult(s string) toolset.Result {
	return toolset.Result{Content: []toolset.ContentBlock{{Type: "text", Text: s}}}
}

func errResult(format string, args ...any) toolset.Result {
	return toolset.Result{
		Content: []toolset.ContentBlock{{Type: "text", Text: fmt.Sprintf(format, args...)}},
		IsError: true,
	}
}

// execFailure separates "the command did not reach the execution backend" from "the command
// itself failed".
//
// The first is our problem (network, expired session, invalid auth); the second is a result
// the agent should see. Mixing them makes "the backend did not start" look the same as "the
// file does not exist".
func execFailure(err error) toolset.Result {
	return errResult("在容器里执行失败: %v", err)
}

// HTTPTimeout is the value of the **tightest HTTP client timeout layer** on the whole
// execution chain. It is only used in the timeout error text.
//
// There may be an HTTP client in front of the execution backend, and its timeout takes effect
// earlier than the command's own timeout. The timeout error must make clear that "the command
// is actually still running on the other side". The wiring side sets this value from the real
// config of its own client layer. Default 60s.
//
// ⚠️ **This is a variable, not a constant**: different backends can have a different tightest
// timeout layer. Callers of NewSet should set it during wiring from the timeout of their own
// HTTP client; if not set, the default 60s is used.
var HTTPTimeout = 60 * time.Second

// explainExecError turns an execution error into words the agent understands and that tell it
// what to do next.
//
// # Why it deserves its own layer
//
// The raw `Client.Timeout exceeded while awaiting headers` has three problems: it is in English,
// it talks about "timed out waiting for response headers" (an HTTP-layer implementation
// detail), and it **does not say whether the command ran at all**. In reality **the command is
// still running in the backend**. The agent sees "failed" and retries, so a release command
// runs twice.
//
// So this follows the rule in `ama-cli/README.md`: **classify errors by "should you retry"**.
// For timeouts the right guidance is not "retry" but "switch to background execution", since
// a retry would time out the same way.
func explainExecError(err error, command string) string {
	if err == nil {
		return ""
	}
	raw := err.Error()
	if !isTimeoutErr(err) {
		return fmt.Sprintf("在执行端里执行失败: %v", raw)
	}
	return fmt.Sprintf(`命令执行超时被中断（约 %s 后）——**但它很可能还在容器里继续跑**。

原因：同步执行的命令受 HTTP 客户端超时限制（%s），这是整条链上最紧的一层，
比 bash 工具自己的 %s 上限更早生效。命令没有被杀掉，只是我们这边不再等它。

⚠️ 不要原样重试：同一条命令还会在同一个地方超时，而且会与前一次并行跑（可能造成
重复发布、重复写入这类真实副作用）。

正确做法（二选一）：
1. **改用后台执行** —— 同一个 bash 工具，加上 %s，会立刻返回一个 task_id，
   之后用 `+"`tail` / `cat`"+` 查输出文件。长命令（构建、发布、跑测试）都该走这条。
2. 先确认上一次是否已经跑完/跑成功，再决定要不要重来。

原始错误：%v
命令：%s`,
		HTTPTimeout, HTTPTimeout, execTimeout,
		"`\"run_in_background\": true`", raw, truncate(command))
}

// isTimeoutErr reports whether this is a "waited too long" kind of error.
//
// We do not use errors.Is(err, context.DeadlineExceeded): an HTTP client timeout returns a
// `*url.Error` wrapping an internal timeout error, and the text `Client.Timeout exceeded` is
// its only stable feature (net/http's httpError is an unexported type). So we accept both:
// an interface assertion for the standard Timeout() method, and a string match for the rest.
func isTimeoutErr(err error) bool {
	var timeouter interface{ Timeout() bool }
	if errors.As(err, &timeouter) && timeouter.Timeout() {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return strings.Contains(err.Error(), "Client.Timeout exceeded")
}

// exitCode returns the exit code, or -1 if the process has not exited.
func exitCode(res *Result) int {
	if res == nil || res.ExitCode == nil {
		return -1
	}
	return *res.ExitCode
}

// stderrOrErr returns the failure reason: the backend's error first, then the process stderr.
func stderrOrErr(res *Result) string {
	if res == nil {
		return "(无输出)"
	}
	if res.Err != "" {
		return strings.TrimSpace(res.Err)
	}
	if s := strings.TrimSpace(res.Stderr); s != "" {
		return s
	}
	return "(无输出)"
}

// decodeB64 decodes base64 sent back by the execution backend.
//
// File content always travels between the backend and this process as base64, never as raw
// text: raw text gets rewritten by newline handling in the shell and HTTP layers (trailing
// newlines eaten, CRLF normalized), while content from read must match the file byte for
// byte. Otherwise an edit the agent makes based on it will fail to match.
func decodeB64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.TrimSpace(s))
}
