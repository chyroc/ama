// Isolation tests for pgToolResultStore (the PG ledger used in production).
//
// The core thing to pin down: **Recover returns only calls of the current session. It must
// never, like the SDK's local file ledger, flush leftover results from other sessions to the
// current session.** That was the root cause of a past production "poison pill cross-talk"
// incident (one fixed tool_use_id was sent to several sessions, causing a chain of
// 400 InvalidPayload). The in-memory ledger shares the same interface semantics as PG, so
// these tests are driven by the in-memory implementation.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"

	"github.com/chyroc/ama/server/repository/dao/coremem"
)

// TestPGToolResultStoreRecoverIsScopedToSession is the reason this whole table exists:
// leftover started/result entries of session A must not show up at all in Recover(B).
func TestPGToolResultStoreRecoverIsScopedToSession(t *testing.T) {
	ledger := newTestLedger(t)
	ctx := context.Background()

	sessionA := "sesn-A"
	sessionB := "sesn-B"
	callA1 := "call_A1"
	callA2 := "call_A2"
	callB1 := "call_B1"

	// A has one "finished but not delivered" (result) and one "killed halfway" (started).
	resA := selfhosted.NewUserToolResultEvent(callA1,
		[]selfhosted.ContentBlock{{Type: "text", Text: "A result"}}, false, "sthr-A")
	rawA, _ := json.Marshal(resA)
	if err := ledger.BeginToolResult(ctx, sessionA, callA1); err != nil {
		t.Fatal(err)
	}
	if err := ledger.MarkToolResultResult(ctx, sessionA, callA1, rawA); err != nil {
		t.Fatal(err)
	}
	if err := ledger.BeginToolResult(ctx, sessionA, callA2); err != nil {
		t.Fatal(err) // started state
	}
	// B has its own result.
	resB := selfhosted.NewUserToolResultEvent(callB1,
		[]selfhosted.ContentBlock{{Type: "text", Text: "B result"}}, false, "sthr-B")
	rawB, _ := json.Marshal(resB)
	if err := ledger.BeginToolResult(ctx, sessionB, callB1); err != nil {
		t.Fatal(err)
	}
	if err := ledger.MarkToolResultResult(ctx, sessionB, callB1, rawB); err != nil {
		t.Fatal(err)
	}

	// Recover B: only B's result is visible, none of A's calls (poison pill isolation).
	storeB := newPGToolResultStore(ledger, sessionB, ctx, nil)
	pendingB, answeredB, err := storeB.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if _, leak := pendingB[callA1]; leak {
		t.Fatalf("A 的 result 泄漏给了 B：%v（这就是毒丸串台）", pendingB)
	}
	if _, leak := answeredB[callA2]; leak {
		t.Fatalf("A 的 started 泄漏给了 B：%v", answeredB)
	}
	if got := pendingB[callB1]; got.ToolUseID != callB1 {
		t.Fatalf("B 应该恢复自己的 result，实际：%v", pendingB)
	}

	// Recover A: sees A's result (pending) and started (answered, triggers the unknown branch).
	storeA := newPGToolResultStore(ledger, sessionA, ctx, nil)
	pendingA, answeredA, err := storeA.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if pendingA[callA1].ToolUseID != callA1 {
		t.Fatalf("A 应恢复 callA1 的 result，实际：%v", pendingA)
	}
	if !answeredA[callA2] {
		t.Fatalf("A 的 started callA2 应进 answered（不重复执行），实际：%v", answeredA)
	}
}

// TestPGToolResultStoreBeginSkipsReExecution checks the state machine: already sent is not
// resent, an existing result is resent without rerunning, started becomes unknown.
func TestPGToolResultStoreBeginSkipsReExecution(t *testing.T) {
	ledger := newTestLedger(t)
	ctx := context.Background()
	sess, call := "sesn-X", "call_X"
	ev := selfhosted.Event{Type: selfhosted.EventTypeAgentToolUse, ID: call,
		SessionThreadID: "sthr-X"}

	store := newPGToolResultStore(ledger, sess, ctx, nil)

	// First time: never seen → empty decision (let the SDK execute it).
	d, err := store.Begin(call, ev)
	if err != nil {
		t.Fatal(err)
	}
	if d.Sent || d.Result.ToolUseID != "" {
		t.Fatalf("首次 Begin 应放行执行，实际：%+v", d)
	}

	// Mark as finished and store the result, then Begin → give the result directly, no rerun.
	out := selfhosted.NewUserToolResultEvent(call,
		[]selfhosted.ContentBlock{{Type: "text", Text: "done"}}, false, "sthr-X")
	if err := store.SaveResult(call, out); err != nil {
		t.Fatal(err)
	}
	d, _ = store.Begin(call, ev)
	if d.Result.ToolUseID != call {
		t.Fatalf("已 result 后 Begin 应重发结果不重跑，实际：%+v", d)
	}

	// After MarkSent, Begin → Sent=true, skipped.
	if err := store.MarkSent(call); err != nil {
		t.Fatal(err)
	}
	d, _ = store.Begin(call, ev)
	if !d.Sent {
		t.Fatalf("已 sent 后 Begin 应跳过，实际：%+v", d)
	}
}

// TestPGToolResultStoreStartedBecomesUnknown checks "the process died halfway through":
// Begin sees started, returns an unknown result, and never re-executes the tool.
func TestPGToolResultStoreStartedBecomesUnknown(t *testing.T) {
	ledger := newTestLedger(t)
	ctx := context.Background()
	sess, call := "sesn-Y", "call_Y"
	ev := selfhosted.Event{Type: selfhosted.EventTypeAgentToolUse, ID: call,
		SessionThreadID: "sthr-Y"}

	if err := ledger.BeginToolResult(ctx, sess, call); err != nil {
		t.Fatal(err)
	}
	store := newPGToolResultStore(ledger, sess, ctx, nil)
	d, err := store.Begin(call, ev)
	if err != nil {
		t.Fatal(err)
	}
	if d.Result.Type != selfhosted.EventTypeUserToolResult {
		t.Fatalf("started 态应回一条 user.tool_result（unknown），实际：%+v", d.Result)
	}
	if d.Result.IsError == nil || !*d.Result.IsError {
		t.Fatalf("unknown 结果必须 is_error=true（不为重复副作用隐瞒），实际：%+v", d.Result)
	}
}

// newTestLedger uses the in-memory Store as the ledger (satisfies ToolResultLedger).
func newTestLedger(t *testing.T) ToolResultLedger {
	t.Helper()
	return coremem.NewMemStore()
}

// failingLedger wraps the in-memory ledger and makes chosen methods return errors, to check
// that "a broken ledger must not block tool execution". This tradeoff was learned from a
// past production incident where all tool calls got stuck.
type failingLedger struct {
	ToolResultLedger
	failGet   bool
	failBegin bool
	failList  bool
}

func (f *failingLedger) GetToolResultState(ctx context.Context, sessionID, callID string,
) (string, []byte, bool, error) {
	if f.failGet {
		return "", nil, false, errLedgerDown
	}
	return f.ToolResultLedger.GetToolResultState(ctx, sessionID, callID)
}

func (f *failingLedger) BeginToolResult(ctx context.Context, sessionID, callID string) error {
	if f.failBegin {
		return errLedgerDown
	}
	return f.ToolResultLedger.BeginToolResult(ctx, sessionID, callID)
}

func (f *failingLedger) ListToolResultsForRecover(ctx context.Context, sessionID string,
) (map[string][]byte, map[string]bool, error) {
	if f.failList {
		return nil, nil, errLedgerDown
	}
	return f.ToolResultLedger.ListToolResultsForRecover(ctx, sessionID)
}

var errLedgerDown = errors.New("账本连接断了")

// TestPGToolResultStoreDegradesWhenLedgerFails: when any step of the ledger fails, neither
// Begin nor Recover may pass the error to the SDK (the SDK would then give up the whole
// work); they must fall back to "execute as usual".
func TestPGToolResultStoreDegradesWhenLedgerFails(t *testing.T) {
	ev := selfhosted.Event{Type: selfhosted.EventTypeAgentToolUse, ID: "call_x", Name: "bash"}

	t.Run("读状态失败仍执行", func(t *testing.T) {
		l := &failingLedger{ToolResultLedger: newTestLedger(t), failGet: true}
		s := newPGToolResultStore(l, "sesn-x", context.Background(), nil)
		d, err := s.Begin("call_x", ev)
		if err != nil {
			t.Fatalf("Begin 不该把账本错误抛给 SDK，实际 %v", err)
		}
		if d.Sent || d.Result.ID != "" {
			t.Errorf("降级应为空 decision（照常执行），实际 %+v", d)
		}
	})

	t.Run("写 started 失败仍执行", func(t *testing.T) {
		l := &failingLedger{ToolResultLedger: newTestLedger(t), failBegin: true}
		s := newPGToolResultStore(l, "sesn-x", context.Background(), nil)
		d, err := s.Begin("call_x", ev)
		if err != nil {
			t.Fatalf("Begin 不该把账本错误抛给 SDK，实际 %v", err)
		}
		if d.Sent || d.Result.ID != "" {
			t.Errorf("降级应为空 decision（照常执行），实际 %+v", d)
		}
	})

	t.Run("Recover 失败也不抛错", func(t *testing.T) {
		l := &failingLedger{ToolResultLedger: newTestLedger(t), failList: true}
		s := newPGToolResultStore(l, "sesn-x", context.Background(), nil)
		pending, answered, err := s.Recover()
		if err != nil {
			t.Fatalf("Recover 不该把账本错误抛给 SDK，实际 %v", err)
		}
		if len(pending) != 0 || len(answered) != 0 {
			t.Errorf("降级应为空集合，实际 pending=%d answered=%d", len(pending), len(answered))
		}
	})
}
