// Tests for "CoT delivery failures must leave a trace in the logs".
//
// # Why this group of assertions deserves its own file
//
// The full shape of that production failure: the user's message stayed at "thinking" forever, and **not a single log line
// could be found**: 3 log lines on the inbound side (inbound.go), 0 for success or failure on the delivery side.
//
// The cause was not "forgot to log"; the signature of recordFailure washed failures into successes: below the limit
// it does `return SaveProgressDelivery(...)`, which is nil when the store write succeeds, so push / finish
// return nil, and the caller's line
//
//	if err := p.driver.finish(...); err != nil { log.Warn("思维链封口失败…") }
//
// **is never entered**: it is dead code. So "the caller has a log" cannot count as evidence of coverage at all;
// it must be checked at recordFailure's own exits.
//
// ⚠️ Every test below asserts **the log text**, not the return value. That is deliberate: the return value is exactly what
// fooled everyone (it is nil, and "should" be nil, since the error really was recorded in the store).

package feishu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// captureLogs builds a logger that writes logs into a buffer.
func captureLogs() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// failingCOT is a COTMessenger that fails on every Append/Complete.
//
// When createErr is non-empty, even CreateCOT fails.
type failingCOT struct {
	createErr error
	appendErr error
	doneErr   error
	creates   int
}

func (f *failingCOT) CreateCOT(_ context.Context, _, _, _ string, _ lark.CreateCOTOption) (*lark.COTMessage, error) {
	f.creates++
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &lark.COTMessage{COTID: "cot_1", MessageID: "om_1"}, nil
}

func (f *failingCOT) AppendCOT(_ context.Context, _, _, _ string, _ []lark.COTEvent) error {
	return f.appendErr
}

func (f *failingCOT) CompleteCOT(_ context.Context, _, _, _ string, _ lark.COTCompleteReason) error {
	return f.doneErr
}

// failingDriverFixture is like driverFixture, but the logger and the failing messenger can be specified.
func failingDriverFixture(t *testing.T, cot COTMessenger, log *slog.Logger) (*progressDriver, ProgressStore) {
	t.Helper()
	store := NewMemProgressStore()
	ctx := context.Background()
	if _, err := store.EnsureProgressDelivery(ctx, &ProgressDelivery{
		RunID: "sesn-1", LarkAppID: "cli_1", ChatID: "oc_1",
		OriginMessageID: "om_src", Transport: TransportCOT,
	}); err != nil {
		t.Fatalf("建投递失败: %v", err)
	}
	token := "tok-a"
	if _, ok, err := store.ClaimProgressDelivery(ctx, "sesn-1", token, time.Hour); err != nil || !ok {
		t.Fatalf("认领失败: ok=%v err=%v", ok, err)
	}
	now := func() time.Time { return time.Now() }
	return &progressDriver{
		store:     store,
		transport: newCOTTransport(cot, "t", "sesn-1", now, log),
		token:     token,
		log:       log,
		now:       now,
	}, store
}

// ⭐ A single delivery failure must leave a Warn.
//
// This is the core assertion of this file. Before the logs were added, failures 1..4 **had zero logs at all**:
// the error was only written to the last_error column in the store, visible only if you thought of querying the store.
//
// Mutation check: delete p.log.Warn at the backoff exit of recordFailure, and this turns red.
func TestProgressDeliveryFailureIsLogged(t *testing.T) {
	log, buf := captureLogs()
	driver, _ := failingDriverFixture(t, &failingCOT{appendErr: errors.New("飞书返回 500")}, log)

	err := driver.push(context.Background(), "sesn-1", ProgressSnapshot{Thinking: "先看看"})

	// ⚠️ The return value is nil: this is exactly what fooled everyone; pin it first,
	// so that if someone later "fixes" it to return an error, this log does not lose its reason to exist without anyone noticing.
	if err != nil {
		t.Fatalf("recordFailure 记账成功时返回 nil，这是既有语义；实际 %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("投递失败必须留一条 Warn，否则用户卡在「思考中」而日志零线索。实际输出：%s", out)
	}
	for _, want := range []string{
		"sesn-1",     // which run
		"oc_1",       // which chat
		"failures=1", // which attempt
		"飞书返回 500",   // underlying error
		"backoff=",   // backoff duration: used to judge "will it heal by itself"
	} {
		if !strings.Contains(out, want) {
			t.Errorf("失败日志里缺 %q，排查时要多猜一步。实际输出：%s", want, out)
		}
	}
}

// ⭐ A closing failure must leave a trace.
//
// finish is called only once per turn, and ConsecutiveFailureCount is usually 1, so it **never reaches
// the "limit reached" branch that already had a log**. So before the logs were added this path was completely dark,
// and it matches exactly the symptom users report most: the chain of thought keeps spinning.
func TestProgressFinishFailureIsLogged(t *testing.T) {
	log, buf := captureLogs()
	driver, _ := failingDriverFixture(t, &failingCOT{doneErr: errors.New("封口被拒")}, log)
	ctx := context.Background()

	// First create it successfully and push once, so RemoteMessageID is stored (otherwise finish takes
	// the early return branch "not created remotely", and closing cannot be tested).
	if err := driver.push(ctx, "sesn-1", ProgressSnapshot{Thinking: "想"}); err != nil {
		t.Fatalf("首次推送应成功: %v", err)
	}
	buf.Reset()

	if err := driver.finish(ctx, "sesn-1", ProgressSnapshot{Terminal: true, Outcome: "done"}); err != nil {
		t.Fatalf("finish 的返回值语义同上，应为 nil；实际 %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("封口失败必须留 Warn——这正是「思维链一直在转」的成因。实际输出：%s", out)
	}
	if !strings.Contains(out, "封口被拒") {
		t.Errorf("要带上底层错误，实际输出：%s", out)
	}
}

// ⭐ "Not closing anymore" in an abnormal final state must also be said.
//
// completed is a normal finish (silent), while failed / create_uncertain means that chain of thought
// **will never be closed**. The two used to share the same `return nil`: a failure disguised as normal.
func TestFinishOnAbnormalTerminalStateWarns(t *testing.T) {
	log, buf := captureLogs()
	// The result of creating the CoT is uncertain → goes into the create_uncertain final state.
	driver, _ := failingDriverFixture(t, &failingCOT{
		createErr: fmt.Errorf("%w: 网关超时", lark.ErrCOTOutcomeUnknown),
	}, log)
	ctx := context.Background()

	if err := driver.push(ctx, "sesn-1", ProgressSnapshot{Thinking: "想"}); err != nil {
		t.Fatalf("push 应把失败记进库并返回 nil: %v", err)
	}
	buf.Reset()

	if err := driver.finish(ctx, "sesn-1", ProgressSnapshot{Terminal: true, Outcome: "done"}); err != nil {
		t.Fatalf("finish 撞终态应返回 nil: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("非正常终态下不封口必须说明，否则「思考中」转到天荒地老而日志无痕。实际输出：%s", out)
	}
	if !strings.Contains(out, "思考中") {
		t.Errorf("日志要说清用户侧看到什么，否则看到的人不会把它和用户报的现象联系起来。实际输出：%s", out)
	}
}

// ⭐ A normal finish does not log a Warn.
//
// This is the reverse control of the previous one. Without it, changing the Finished() branch to "always log a Warn"
// would also make the previous one green, and every normal turn would flood a Warn,
// and log flooding is itself a failure.
func TestFinishOnCompletedIsSilent(t *testing.T) {
	log, buf := captureLogs()
	driver, _ := failingDriverFixture(t, &failingCOT{}, log)
	ctx := context.Background()

	if err := driver.push(ctx, "sesn-1", ProgressSnapshot{Thinking: "想"}); err != nil {
		t.Fatalf("推送失败: %v", err)
	}
	if err := driver.finish(ctx, "sesn-1", ProgressSnapshot{Terminal: true, Outcome: "done"}); err != nil {
		t.Fatalf("封口失败: %v", err)
	}
	buf.Reset()

	// Close again: this time it hits completed.
	if err := driver.finish(ctx, "sesn-1", ProgressSnapshot{Terminal: true, Outcome: "done"}); err != nil {
		t.Fatalf("重复封口应返回 nil: %v", err)
	}

	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("正常收尾后重复封口不该打 Warn，否则正常对话刷屏。实际输出：%s", buf.String())
	}
}

// ⭐ A failure of the opening events must leave a trace.
//
// The return value of that branch is **exactly the same** as the success path (cotID, msgID, nil), so the caller cannot tell.
// Without a log, when an empty, forever spinning chain of thought appears in the chat, the logs have no trace.
//
// This log is also **the way to verify** the fallback "the next apply will start over with the full diff":
// by this repo's rule, a defense without a dedicated log cannot be confirmed to have ever taken effect.
func TestOpeningEventFailureIsLogged(t *testing.T) {
	log, buf := captureLogs()
	transport := newCOTTransport(&failingCOT{appendErr: errors.New("追加被拒")}, "t", "sesn-1", time.Now, log)

	cotID, msgID, err := transport.ensure(context.Background(), &ProgressDelivery{
		RunID: "sesn-1", ChatID: "oc_1", OriginMessageID: "om_src",
	})

	// The existing meaning stays: the IDs must be returned, otherwise this CoT is lost forever.
	if err != nil || cotID == "" || msgID == "" {
		t.Fatalf("既有语义是「返回 ID 让调用方落库」，实际 cot=%q msg=%q err=%v", cotID, msgID, err)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("开场事件失败必须留痕，否则空思维链零线索。实际输出：%s", out)
	}
	if !strings.Contains(out, "追加被拒") || !strings.Contains(out, "sesn-1") {
		t.Errorf("要带上底层错误与 run id，实际输出：%s", out)
	}
}

// ⭐ **The CoT body must not appear in the delivery logs.**
//
// This is a security assertion. Thinking text and tool output are user data (README-feedback.md lists user message bodies
// under "never collected at either level"), and diagnosis only needs "which step failed, which attempt".
//
// ⚠️ **The scenario must be "push successfully once, then fail", not "fail the first time".**
//
// When it fails the first time `d.AppliedSnapshot` is still nil, and user content **is not at hand
// in recordFailure at all**: any mutation that "logs the snapshot" would then only print an empty string,
// so this test would be always green and check nothing. This pitfall was found during mutation checks:
// the first version used "fail on the first push", and after adding `slog("snapshot", d.AppliedSnapshot.Thinking)`
// the test **stayed green**.
//
// Succeeding once first puts the snapshot into d.AppliedSnapshot, and only then is user content really within
// reach of this function, and only then does this assertion mean anything.
//
// Mutation check: add d.AppliedSnapshot.Thinking to the Warn in recordFailure, and this turns red.
func TestProgressFailureLogOmitsUserContent(t *testing.T) {
	log, buf := captureLogs()
	cot := &failingCOT{}
	driver, _ := failingDriverFixture(t, cot, log)
	ctx := context.Background()

	const secretThinking = "用户的私密业务内容不该进日志"
	const secretTool = "grep -r 内部项目代号 /repo"

	// 1. First push successfully once, storing a snapshot with user content into d.AppliedSnapshot.
	if err := driver.push(ctx, "sesn-1", ProgressSnapshot{
		Thinking: secretThinking,
		Tools:    []ProgressToolCall{{ID: "tc_1", Name: "bash", Args: secretTool}},
	}); err != nil {
		t.Fatalf("首推应成功: %v", err)
	}
	buf.Reset()

	// 2. Then make delivery start failing: at this point the user content is at hand in recordFailure.
	cot.appendErr = errors.New("飞书 500")
	if err := driver.push(ctx, "sesn-1", ProgressSnapshot{
		Thinking: secretThinking + "，又想了想",
		Tools:    []ProgressToolCall{{ID: "tc_1", Name: "bash", Args: secretTool, Done: true}},
	}); err != nil {
		t.Fatalf("失败时应记账并返回 nil: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("前提没满足：这一步应当已经失败并打了 Warn。实际输出：%s", out)
	}
	for _, leaked := range []string{secretThinking, secretTool, "内部项目代号", "又想了想"} {
		if strings.Contains(out, leaked) {
			t.Errorf("用户内容 %q 泄漏进日志了。实际输出：%s", leaked, out)
		}
	}
	// But the things needed for debugging must be there.
	if !strings.Contains(out, "sesn-1") {
		t.Errorf("run id 应保留，实际输出：%s", out)
	}
}
