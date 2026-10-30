// Detects "the agent failed to fetch Feishu history" and logs one line.
//
// # Why it is needed: we just removed the only signal
//
// A history fetch failure (lark-cli in the container has no credentials, or the Feishu app lacks
// `im:message.group_msg`) used to **have** a signal: the agent would tell the user plainly, so
// someone in the chat saw it and reported it. That signal was bad (a regular user was told an
// internal CLI name, an error code, and a fix command he could not run), so permissionNotice in
// feishu/larkprompt.go was changed to degrade silently.
//
// ⚠️ **But "the user should not notice" does not mean "we do not need to know".** If only the
// prompt were changed, this failure would become a totally silent event: the session replies as
// usual, the run succeeds as usual, the log has not a single word, and the agent just lacks the
// earlier context and answers worse than it should. That is exactly the kind of failure this repo
// keeps getting hurt by: **zero errors, and symptoms that do not point to the real cause** (it
// shows up as "the bot answers without context", and nobody would think of a history fetch
// failure). So silent degrading must **come with a server-side trace**; only both together are
// complete.
//
// # Why it hangs on the tool decorator instead of being judged elsewhere
//
// Of three candidate places, only this one can see the truth:
//
//   - **feishu/inbound.go** (where the prompt is built): it only knows "I gave the agent the
//     command to fetch history", not whether the agent ran it or what the result was. That is
//     the starting point, not the result.
//   - **core/sessionenv.go** (where credentials are injected): it can find "no credentials
//     configured", but not "credentials exist, permission is missing" (230027). Also, **whether
//     this authorization is enabled and whether this round used history are two different
//     things**: even if it is not enabled, history may not be needed at all, and then an alert
//     is noise.
//   - **the tool decorator in shared/infra/shelltools** (this file): it sees the fact **it really
//     ran and it really failed**, catching both kinds of failure. The rule rests on the result,
//     not on configuration.
//
// # Only log, do not change behavior
//
// This **does not touch Result**: the agent still gets the original error it should see (it
// needs that information to decide "do not retry"). This layer is purely on the side; if it
// fails, nothing else is affected.
//
// ⚠️ The output has already been scrubbed by scrub.go (scrubbed in container.Exec, the only exit
// where credentials leave the process), so writing excerpts to the log here is safe. Even so,
// **only the error marker itself is written, not the whole output**: that may contain real chat
// content from the group, and putting it in the log would copy it into another system.
package shelltools

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/volcengine/ark-runtime-go/arkruntime/toolset"
)

// larkHistoryFailure describes the cause of one history fetch failure.
//
// reason is a short human-readable phrase used for log grouping ("how many sessions degraded
// because of missing credentials").
type larkHistoryFailure struct {
	reason string
	// fixHint is a fix suggestion **for ourselves**, not for the user.
	// The two cases are fixed in completely different ways (one is the platform configuring
	// credentials, the other is the app owner adding a permission). Merging them into one "history
	// fetch failed" log line means another round of digging to know whom to ask.
	fixHint string
}

// larkHistoryMarkers are the fingerprints of the two failures in the output.
//
// ⚠️ **Judge by output content, not by exit code.** A lark-cli failure is
// `{"ok":false,"error":{...}}` in JSON, and it is **not always** expressed with a non-zero exit
// code (this repo's rule "check ok == true for success, not code == 0" is about exactly this).
// Checking only IsError misses some, and the missed part is the most common form.
var larkHistoryMarkers = []struct {
	marker string
	fail   larkHistoryFailure
}{
	{
		marker: "not_configured",
		fail: larkHistoryFailure{
			reason:  "容器里的 lark-cli 没有飞书凭据",
			fixHint: "空间 owner 做一次「飞书用户授权」，或检查 LarkUsers 是否装配（见 core/worker.go 启动日志）",
		},
	},
	{
		// 230027 = missing im:message.group_msg. ⚠️ Not the same as 230001: that one means
		// "permission passed, only chat_id is wrong", not a permission problem (see core/README-integrations.md).
		marker: "230027",
		fail: larkHistoryFailure{
			reason:  "飞书应用缺 im:message.group_msg 权限",
			fixHint: "该飞书应用的所有者去开放平台补这个权限（按应用授权，每个应用各自申请）",
		},
	},
}

// detectLarkHistoryFailure decides whether one tool call is a "Feishu history fetch failure".
//
// Both conditions must hold; missing either gives false alarms:
//
//   - **the command really fetches history**: checking only `lark-cli` is not enough. The agent
//     may fail using lark-cli to send messages, read docs, or check the calendar, and those have
//     nothing to do with "missing context". Mixing them in would make this log line lose its
//     meaning of "how many sessions degraded";
//   - **the output has a failure fingerprint**: see the larkHistoryMarkers comment; judge by
//     content, not by exit code.
//
// A false second return value means "this call has nothing to do with this file", and the caller
// does nothing.
func detectLarkHistoryFailure(toolName, command, output string) (larkHistoryFailure, bool) {
	if toolName != "bash" || !strings.Contains(command, "lark-cli") {
		return larkHistoryFailure{}, false
	}
	// The two history commands, the same pair as given by historyGuide in feishu/larkprompt.go.
	//
	// ⚠️ **This is a cross-package coupling through string literals, and its direction cannot be
	// removed**: `infra/` may not import `feishu` (infra never imports business packages), so only a
	// string can be written here. If the command names change there, this **silently stops
	// matching**: no error, this log line just never prints again, and "no log line" looks exactly
	// like "everything is fine".
	//
	// Fix: pin it on the **producer** side. TestHistoryCommandsMatchShelltoolsDetector in
	// feishu/larkprompt_test.go asserts the commands historyGuide really produces contain these two
	// substrings, and points back to this file. If that test goes red, this place must change too.
	if !strings.Contains(command, "chat-messages-list") &&
		!strings.Contains(command, "threads-messages-list") {
		return larkHistoryFailure{}, false
	}
	for _, m := range larkHistoryMarkers {
		if strings.Contains(output, m.marker) {
			return m.fail, true
		}
	}
	return larkHistoryFailure{}, false
}

// bashCommandOf pulls the raw command out of the bash tool's input.
//
// The shape matches BashTool.Execute (`command` main field + `cmd` compatibility alias).
// On parse failure it returns an empty string: this layer is on the side and should not error
// just because the input is odd.
func bashCommandOf(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var req struct {
		Command string `json:"command"`
		Cmd     string `json:"cmd,omitempty"`
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return ""
	}
	if c := strings.TrimSpace(req.Command); c != "" {
		return c
	}
	return strings.TrimSpace(req.Cmd)
}

// larkResultText joins the text blocks in a tool result.
//
// The name has a lark prefix because this package already has two functions with close names,
// resultText (tools_test.go) and textResult (exec.go), each with its own shape; a name clash
// would not compile.
func larkResultText(res toolset.Result) string {
	var b strings.Builder
	for _, blk := range res.Content {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// logLarkHistoryFailure logs one Warn when a history fetch failure is detected.
//
// Warn instead of Info: this is a state that **needs a person to act** (either configure
// credentials or add a permission), and the user side now sees nothing at all, so the log is the
// only way in. It only prints on real failures and will not flood the log.
//
// ⚠️ The phrase "silently degraded on the user side" in the log is deliberate: the first question
// of the next person who sees this line will be "so what did the user see?". Writing the answer
// right in saves one trip through the code.
func logLarkHistoryFailure(log *slog.Logger, toolName string, input json.RawMessage, res toolset.Result) {
	if log == nil {
		return
	}
	fail, ok := detectLarkHistoryFailure(toolName, bashCommandOf(input), larkResultText(res))
	if !ok {
		return
	}
	log.Warn("agent 取飞书历史失败，本轮已降级作答（用户侧静默，不会看到这件事）",
		slog.String("原因", fail.reason),
		slog.String("处置", fail.fixHint),
		slog.String("影响", "这一轮缺群聊/话题上文，回答质量可能下降"))
}
