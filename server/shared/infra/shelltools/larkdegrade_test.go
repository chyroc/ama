// Test cases for larkdegrade.go.
//
// # This group guards something "nobody would notice is broken"
//
// A history fetch failure is now **fully silent** to the user (permissionNotice in
// feishu/larkprompt.go was changed to answer in degraded mode), so this log line is the only
// signal. And a log line that **does not print raises no error**: detection logic written wrong,
// command names no longer matching, the rule narrowed by mistake all show up as "the log is
// clean", which looks exactly like "everything is fine". So each one is pinned here.
package shelltools

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/volcengine/ark-runtime-go/arkruntime/toolset"
)

// bashInput builds an input for the bash tool.
func bashInput(command string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"command": command})
	return raw
}

// larkTextResult builds a tool result with text (this package already has textResult /
// resultText, hence the prefix).
func larkTextResult(text string, isErr bool) toolset.Result {
	return toolset.Result{
		Content: []toolset.ContentBlock{{Type: "text", Text: text}},
		IsError: isErr,
	}
}

// Output shapes of the two real failures, copied from real responses recorded in
// shared/infra/lark and in probes.
const (
	notConfiguredOut = `exit_code: 0
--- stdout ---
{"ok":false,"error":{"subtype":"not_configured","hint":"run lark-cli config init --new"}}`

	noPermissionOut = `exit_code: 0
--- stdout ---
{"ok":false,"error":{"code":230027,"msg":"no permission to get group message"}}`
)

// The two history commands, shape copied from historyGuide in feishu/larkprompt.go.
//
// ⚠️ **They can only be copied here, not referenced**: `infra/` may not import `feishu`. So
// "command names match on both sides" is pinned on the producer side
// (TestHistoryCommandsMatchShelltoolsDetector in feishu/larkprompt_test.go); this file only tests
// the detection logic itself.
const (
	chatHistoryCmd   = "lark-cli im +chat-messages-list --chat-id oc_x --order desc --page-size 50 --format json"
	threadHistoryCmd = "lark-cli im +threads-messages-list --thread omt_x --order asc --format json"
)

// TestDetectLarkHistoryFailure_Positives: both failures must be recognized, with **different causes**.
//
// The causes must be kept apart: one needs the platform to configure credentials, the other
// needs the app owner to add a permission, and different people are asked. Merging them into one
// "history fetch failed" means another round of digging after seeing the log.
func TestDetectLarkHistoryFailure_Positives(t *testing.T) {
	cases := []struct {
		name       string
		command    string
		output     string
		wantReason string
	}{
		{"没凭据/群历史", chatHistoryCmd, notConfiguredOut, "没有飞书凭据"},
		{"没凭据/话题历史", threadHistoryCmd, notConfiguredOut, "没有飞书凭据"},
		{"没权限/群历史", chatHistoryCmd, noPermissionOut, "im:message.group_msg"},
		{"没权限/话题历史", threadHistoryCmd, noPermissionOut, "im:message.group_msg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fail, ok := detectLarkHistoryFailure("bash", tc.command, tc.output)
			if !ok {
				t.Fatalf("应认出这是取历史失败：\n命令：%s\n输出：%s", tc.command, tc.output)
			}
			if !strings.Contains(fail.reason, tc.wantReason) {
				t.Errorf("归因应含 %q，实际 %q", tc.wantReason, fail.reason)
			}
			if strings.TrimSpace(fail.fixHint) == "" {
				t.Error("要给处置建议——不然看到日志的人还得再查一次该找谁")
			}
		})
	}

	// ⚠️ The **fixes of the two failures must differ**. If the values came from the same place, an
	// implementation where "both branches return the same struct" would also pass all the subtests
	// above (test fixtures must differ on every axis that a mutation could collapse).
	noCred, _ := detectLarkHistoryFailure("bash", chatHistoryCmd, notConfiguredOut)
	noPerm, _ := detectLarkHistoryFailure("bash", chatHistoryCmd, noPermissionOut)
	if noCred.reason == noPerm.reason || noCred.fixHint == noPerm.fixHint {
		t.Errorf("两种失败的归因与处置都必须不同，实际：\n没凭据 %+v\n没权限 %+v", noCred, noPerm)
	}
}

// TestDetectLarkHistoryFailure_Negatives is the **false-alarm guard** half.
//
// The meaning of this log line is "how many sessions degraded because of missing context";
// mixing in other failures breaks that meaning.
// ⚠️ With only positive cases, `return larkHistoryFailure{...}, true` (always matching) would
// also be all green.
func TestDetectLarkHistoryFailure_Negatives(t *testing.T) {
	cases := []struct {
		name    string
		tool    string
		command string
		output  string
	}{
		{"取历史成功了", "bash", chatHistoryCmd, `{"ok":true,"data":{"items":[]}}`},
		// Other uses of lark-cli failed: unrelated to "missing context".
		{"发消息失败", "bash", "lark-cli im +send --chat-id oc_x --text hi", noPermissionOut},
		{"读文档失败", "bash", "lark-cli doc +get --token abc", notConfiguredOut},
		// Other tools: read/grep and the like cannot be fetching history.
		{"不是 bash 工具", "read", chatHistoryCmd, notConfiguredOut},
		// Not lark-cli at all.
		{"别的命令撞词", "bash", "echo not_configured", notConfiguredOut},
		{"空命令", "bash", "", notConfiguredOut},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := detectLarkHistoryFailure(tc.tool, tc.command, tc.output); ok {
				t.Errorf("不该认成取历史失败：\ntool=%s\n命令：%s\n输出：%s",
					tc.tool, tc.command, tc.output)
			}
		})
	}
}

// TestTimedToolLogsLarkHistoryFailure checks **the decorator is really wired in**.
//
// The cases above that call detectLarkHistoryFailure directly cannot prove this: however correct
// the detection function is, if timedTool.Execute never calls it, it may as well not exist. That
// is a lesson this repo has learned before (a newly added module passes every check but never
// takes effect, with zero errors). So this test goes down the **real** path: wrap a fake tool,
// make it return failure output, then check the log.
func TestTimedToolLogsLarkHistoryFailure(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	tool := newTimedTool(&fakeTool{
		name: "bash",
		res:  larkTextResult(notConfiguredOut, false),
	}, log)

	tool.Execute(context.Background(), bashInput(chatHistoryCmd))

	out := buf.String()
	if !strings.Contains(out, "取飞书历史失败") {
		t.Errorf("装饰器应该记一条取历史失败的日志，实际日志：\n%s", out)
	}
	// The log must say "the user cannot see this": that is the first question of the next person
	// who sees it.
	if !strings.Contains(out, "静默") {
		t.Errorf("日志要点明用户侧已静默降级，实际：\n%s", out)
	}

	// Successful calls should not produce this log line (otherwise normal traffic floods it and it
	// loses its value as a signal).
	buf.Reset()
	okTool := newTimedTool(&fakeTool{
		name: "bash",
		res:  larkTextResult(`{"ok":true,"data":{"items":[]}}`, false),
	}, log)
	okTool.Execute(context.Background(), bashInput(chatHistoryCmd))
	if strings.Contains(buf.String(), "取飞书历史失败") {
		t.Errorf("成功的调用不该记这条日志：\n%s", buf.String())
	}
}

// TestBashCommandOf_AcceptsCmdAlias: both input field names must be accepted.
//
// BashTool.Execute takes the `command` main field + the `cmd` compatibility alias. If only one
// were accepted here, calls using the alias would **never be logged**, which is another silent failure.
func TestBashCommandOf_AcceptsCmdAlias(t *testing.T) {
	alias, _ := json.Marshal(map[string]string{"cmd": chatHistoryCmd})
	if got := bashCommandOf(alias); got != chatHistoryCmd {
		t.Errorf("cmd 别名也要认，实际取到 %q", got)
	}
	if got := bashCommandOf(bashInput(chatHistoryCmd)); got != chatHistoryCmd {
		t.Errorf("command 主字段取错了：%q", got)
	}
	// Odd input should not panic or error: this layer is on the side.
	for _, bad := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage("{")} {
		if got := bashCommandOf(bad); got != "" {
			t.Errorf("坏入参应返回空串，实际 %q", got)
		}
	}
}

// fakeTool is a tool that returns a fixed result, only for testing the decorator.
type fakeTool struct {
	name string
	res  toolset.Result
}

func (f *fakeTool) Name() string { return f.name }

func (f *fakeTool) Execute(context.Context, json.RawMessage) toolset.Result { return f.res }
