// Unit tests for `run_in_background` and the timeout layers.
//
// The two are together because they are two halves of the same failure: a long command hits the
// 60-second HTTP timeout (layers), while the agent thought it had used background execution
// (silently ignored).
package shelltools

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/toolset"
)

// timeoutErr builds an error with **the same shape** as an http.Client timeout.
//
// It must be a `*url.Error` wrapping an error whose Timeout() is true: using
// `errors.New("Client.Timeout exceeded")` directly only tests the string fallback branch,
// while real errors go through the interface assertion branch. Both must be recognized.
func timeoutErr() error {
	return &url.Error{
		Op:  "Post",
		URL: "https://executor.example.com/api/process/start",
		Err: &net.OpError{
			Op:  "dial",
			Err: timeoutOp{},
		},
	}
}

type timeoutOp struct{}

func (timeoutOp) Error() string {
	return "i/o timeout (Client.Timeout exceeded while awaiting headers)"
}
func (timeoutOp) Timeout() bool   { return true }
func (timeoutOp) Temporary() bool { return true }

func TestBashRunInBackgroundReturnsHandleWithoutWaiting(t *testing.T) {
	fe := &fakeExec{reply: []*Result{ok("AMA_BG_ID=bg-1700000000-42\nAMA_BG_PID=4242\n")}}
	tool := NewBashTool(fe, "/work")

	res := tool.Execute(context.Background(), raw(map[string]any{
		"command":           "sleep 900",
		"run_in_background": true,
	}))

	if res.IsError {
		t.Fatalf("后台启动不该报错: %s", bgText(res))
	}
	out := bgText(res)
	// The three parts of the handle: id, output file, how to check. Missing any one, the agent
	// cannot get the result back.
	for _, want := range []string{"bg-1700000000-42", "4242", BGLogDir, "task_id"} {
		if !strings.Contains(out, want) {
			t.Errorf("返回里缺 %q：\n%s", want, out)
		}
	}

	// Key point: the script really sent must put the command in the background (setsid/nohup + &),
	// not run it synchronously as-is. Otherwise it still hits the 60 seconds.
	script := fe.scripts[len(fe.scripts)-1]
	for _, want := range []string{"setsid", "nohup", "&"} {
		if !strings.Contains(script, want) {
			t.Errorf("脚本里缺 %q，说明没真的挂后台：\n%s", want, script)
		}
	}

	// The timeout of the start step must be bgStartTimeout (seconds), **not** execTimeout.
	// Using execTimeout would give the millisecond action "start a background task" a 120-second
	// budget too, which would drag the whole conversation round to death when the executor hangs.
	if got := lastSpecTimeout(fe); got != bgStartTimeout {
		t.Errorf("后台启动超时 = %v，想要 %v", got, bgStartTimeout)
	}
}

func TestBashRunInBackgroundQuotesCommand(t *testing.T) {
	// Single quotes/semicolons in the command must not break the outer script: an injection would
	// make the background task run unplanned things, and the error would not point to quotes at all.
	fe := &fakeExec{reply: []*Result{ok("AMA_BG_ID=bg-1\nAMA_BG_PID=2\n")}}
	tool := NewBashTool(fe, "")

	evil := `echo 'a'; rm -rf /tmp/should-not-happen`
	res := tool.Execute(context.Background(), raw(map[string]any{
		"command":           evil,
		"run_in_background": true,
	}))
	if res.IsError {
		t.Fatalf("不该报错: %s", bgText(res))
	}

	script := fe.scripts[len(fe.scripts)-1]
	// The whole command must appear as **one** quoted literal, not loose in the script.
	if strings.Contains(script, "\nrm -rf /tmp/should-not-happen") {
		t.Errorf("命令没被引用，注入生效了：\n%s", script)
	}
	if !strings.Contains(script, shQuote(evil)) {
		t.Errorf("命令没有以引用形式出现：\n%s", script)
	}
}

func TestBashRunInBackgroundReportsMissingID(t *testing.T) {
	// The script ran but printed no id: no id means no result, so it must be an error.
	// Returning "success" would make the agent think the task is running and check again and again
	// with an empty handle.
	fe := &fakeExec{reply: []*Result{ok("something unexpected\n")}}
	tool := NewBashTool(fe, "")

	res := tool.Execute(context.Background(), raw(map[string]any{
		"command":           "make build",
		"run_in_background": true,
	}))
	if !res.IsError {
		t.Fatalf("没取到 task_id 时必须报错，实际返回：%s", bgText(res))
	}
}

func TestBashForegroundStillSynchronous(t *testing.T) {
	// Without run_in_background the behavior does not change: run synchronously, return the result
	// as-is. This guards against regression: the background path should not change the default
	// behavior along the way.
	fe := &fakeExec{reply: []*Result{ok("hi\n")}}
	tool := NewBashTool(fe, "")

	res := tool.Execute(context.Background(), raw(map[string]any{"command": "echo hi"}))
	if res.IsError {
		t.Fatalf("不该报错: %s", bgText(res))
	}
	if !strings.Contains(bgText(res), "hi") {
		t.Errorf("同步结果丢了：%s", bgText(res))
	}
	script := fe.scripts[0]
	if strings.Contains(script, "setsid") || strings.Contains(script, BGLogDir) {
		t.Errorf("前台命令不该走后台脚本：%s", script)
	}
	if got := lastSpecTimeout(fe); got != execTimeout {
		t.Errorf("前台超时 = %v，想要 %v", got, execTimeout)
	}
}

func TestExplainExecErrorOnTimeoutIsActionable(t *testing.T) {
	msg := explainExecError(timeoutErr(), "aml-cli release publish")

	// Four things that must be said clearly. Missing one, this error goes back to "not pointing to
	// the real cause".
	checks := map[string]string{
		"进程还在跑":      "还在容器里继续跑",
		"是哪一层切的":     HTTPTimeout.String(),
		"给出后台执行这条出路": "run_in_background",
		"明说不要原样重试":   "不要原样重试",
	}
	for what, want := range checks {
		if !strings.Contains(msg, want) {
			t.Errorf("超时报错没说清「%s」（缺 %q）：\n%s", what, want, msg)
		}
	}
	// The original error must be kept: when debugging, that English sentence is the only anchor to
	// match against platform logs.
	if !strings.Contains(msg, "Client.Timeout exceeded") {
		t.Errorf("丢了原始错误：\n%s", msg)
	}
}

func TestExplainExecErrorOnNonTimeoutStaysPlain(t *testing.T) {
	// Non-timeout errors should not get the "use background execution instead" guidance: that would
	// mislead problems like "Session closed" into "the command is too long".
	msg := explainExecError(errors.New("session not found"), "ls")
	if strings.Contains(msg, "run_in_background") {
		t.Errorf("非超时错误不该建议后台执行：\n%s", msg)
	}
	if !strings.Contains(msg, "session not found") {
		t.Errorf("原始错误丢了：\n%s", msg)
	}
}

func TestIsTimeoutErrRecognizesBothShapes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// Real shape: url.Error wrapping an error that implements Timeout().
		{"http client 超时", timeoutErr(), true},
		// context timeout: the ExecSpec ctx expiring is also a timeout.
		{"context 超时", context.DeadlineExceeded, true},
		// String fallback: still recognized when the interface is not available.
		{"仅字符串", errors.New("net/http: request canceled (Client.Timeout exceeded)"), true},
		{"普通错误", errors.New("permission denied"), false},
		{"nil 之外的空错误", errors.New(""), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTimeoutErr(tc.err); got != tc.want {
				t.Errorf("isTimeoutErr(%v) = %v，想要 %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestTimeoutLayersOrdering pins down the conclusion "who cuts first" itself.
//
// This is not a tautology: four timeout layers are spread over three packages, and any "just
// raise it a bit" may make the HTTP layer exceed the bash limit, so long commands quietly fail in
// a different way. This test forces such a change to edit the test explicitly instead of taking
// effect silently.
func TestTimeoutLayersOrdering(t *testing.T) {
	// Layer 1 (HTTP client) must be strictly less than layer 2 (the bash limit sent to the executor),
	// otherwise the sentence "takes effect earlier than the bash limit" in explainExecError is wrong.
	if HTTPTimeout >= execTimeout {
		t.Errorf("HTTP 超时(%v) 不再比 bash 上限(%v) 紧——报错文案里的因果关系已经不成立",
			HTTPTimeout, execTimeout)
	}
	// Layer 3 is the SDK's 120s ceiling, not configurable. Layer 2 reaching it would be pointless.
	const sdkToolTimeoutCeiling = 120 * time.Second
	if execTimeout > sdkToolTimeoutCeiling {
		t.Errorf("bash 上限(%v) 超过了 SDK 那个配不了的天花板(%v)，超出的部分永远用不上",
			execTimeout, sdkToolTimeoutCeiling)
	}
	// The background start itself must be far below the sync limit: it is only a fork and should
	// not carry a long command's budget.
	if bgStartTimeout >= execTimeout {
		t.Errorf("后台启动超时(%v) 不该接近同步上限(%v)", bgStartTimeout, execTimeout)
	}
}

// TestRunBackgroundMarksTheExecer guards the step "after starting a background task, tell the
// executor".
//
// It uses the production shape: BashTool gets an Execer decorated by NewEnrichingExecer, and the
// decorator must pass MarkBackgroundStarted through to the base. If it is missed, the background
// task is silently frozen after a conversation round ends (pausing the container freezes
// background processes, and resuming does not unfreeze them), with zero errors, and the symptom
// (logs stop halfway) does not point to pausing at all. Nothing else on this chain would warn you.
func TestRunBackgroundMarksTheExecer(t *testing.T) {
	base := &markingExec{fakeExec: fakeExec{reply: []*Result{ok("AMA_BG_ID=bg-9\nAMA_BG_PID=99\n")}}}
	ex := NewEnrichingExecer(base, "sesn-x", nil, quietLog())
	tool := NewBashTool(ex, "")

	res := tool.Execute(context.Background(), raw(map[string]any{
		"command":           "make release",
		"run_in_background": true,
	}))
	if res.IsError {
		t.Fatalf("不该报错: %s", bgText(res))
	}
	if !base.marked {
		t.Error("起了后台任务却没通知执行端 —— 它会在空闲时被暂停冻住")
	}
}

func TestRunForegroundDoesNotMarkTheExecer(t *testing.T) {
	// A foreground command should not set this flag: setting it would make every later idle round
	// check the executor for nothing.
	base := &markingExec{fakeExec: fakeExec{reply: []*Result{ok("hi\n")}}}
	ex := NewEnrichingExecer(base, "sesn-x", nil, quietLog())
	tool := NewBashTool(ex, "")

	tool.Execute(context.Background(), raw(map[string]any{"command": "echo hi"}))
	if base.marked {
		t.Error("前台命令不该标记「起过后台任务」")
	}
}

// markingExec is a fakeExec with MarkBackgroundStarted, used to check that the optional
// interface is really called. In production code, this method is implemented by the session
// object on the executor container side.
type markingExec struct {
	fakeExec
	marked bool
}

func (m *markingExec) MarkBackgroundStarted() { m.marked = true }

// lastSpecTimeout gets the Timeout received by the last Exec.
func lastSpecTimeout(fe *fakeExec) time.Duration { return fe.lastTimeout }

// bgText joins the text blocks in the result.
func bgText(r toolset.Result) string {
	var b strings.Builder
	for _, c := range r.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}
