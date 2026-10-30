// Tests for the turn serial lock: **after the previous turn ends, the next message must be able to go through**.
//
// This suite guards against a real production incident: the user asked "do you have lark-cli", the agent answered completely,
// the user went on to ask "tell me a story", and the bot replied "the previous message is still being processed", blocking for 10 minutes.
//
// # Why the old tests were all green and did not catch it
//
// The old fakeConversations.StreamRunEvents returned after sending its frames, so in unit tests
// "the stream ends by itself" was naturally true. **Real Ark is not like that**: it keeps an SSE long connection per session,
// keeps waiting for the next user message after end_turn, and never closes the stream. So a subscriber that does not recognize the final frame itself
// blocks until runTimeout, and the defer releasing the serial lock comes after it.
//
// So this file uses stickyConversations: **the stream does not end by itself**, and only returns when ctx is cancelled.
// That is Ark's real behavior. Run with it, the code before the change hangs on wg.Wait() until timeout.
package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/channel"
	"github.com/chyroc/ama/server/shared/gopkg/arkframe"
	"github.com/chyroc/ama/server/shared/infra/lark"
)

// stickyConversations is a fake of "Ark does not close the stream by itself".
//
// It differs from fakeConversations in only this one thing, but that is the whole point of this group:
// after sending its frames it **blocks** until the caller cancels ctx. A caller that cannot recognize the final frame hangs until timeout.
type stickyConversations struct {
	mu sync.Mutex

	frames [][]byte
	// status is the return value of RunStatus; an empty string is treated as running.
	status    string
	statusErr error

	// streamCalls records how many times it was subscribed, to assert the second turn really started.
	streamCalls int
	// appended is the batches of events sent into the session.
	appended [][]json.RawMessage

	appendErr error
	// createErr makes session creation fail.
	createErr error
	created   int
}

func (f *stickyConversations) CreateConversation(_ context.Context, _, _ string) (Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return Run{}, f.createErr
	}
	f.created++
	// Always returns the same session: this group wants exactly "two messages in a row on the same session".
	return Run{ID: "sesn-sticky"}, nil
}

func (f *stickyConversations) CreateChannelConversation(
	ctx context.Context, workspaceID, agentID string, _ channel.Kind, _ string,
) (Run, error) {
	return f.CreateConversation(ctx, workspaceID, agentID)
}

func (f *stickyConversations) AppendRunEvents(
	_ context.Context, _ string, raw []json.RawMessage,
) ([]json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appendErr != nil {
		return nil, f.appendErr
	}
	f.appended = append(f.appended, raw)
	return raw, nil
}

// StreamRunEvents **does not return** after sending the frames, simulating Ark's long connection.
//
// It only ends when ctx is cancelled, returning ctx.Err(); the SDK has the same shape in the real implementation
// (see the handling of dec.Close in core/arkrun.go).
func (f *stickyConversations) StreamRunEvents(
	ctx context.Context, _ string, onFrame func(raw []byte) error,
) error {
	f.mu.Lock()
	f.streamCalls++
	frames := f.frames
	f.mu.Unlock()

	for _, frame := range frames {
		if err := onFrame(frame); err != nil {
			return err
		}
	}
	// Ark keeps waiting here for the next user message, without closing the stream.
	<-ctx.Done()
	return ctx.Err()
}

func (f *stickyConversations) AgentsBoundToFeishuApp(context.Context, string) ([]string, error) {
	return []string{"agent_1"}, nil
}

func (f *stickyConversations) AgentDisabled(context.Context, string) bool { return false }

// The tests in this file check the **serial lock** itself (blocking new messages when the lock cannot be grabbed), so they always take "queue":
// the redirect path does not go through replyBusy, which would change what they test entirely.
// The redirect branch is covered separately by inbound_followup_test.go.
func (f *stickyConversations) AgentFollowUpQueues(context.Context, string) bool { return true }

// Always true = show the chain of thought, matching the default mode; these tests do not care about this dimension.
func (f *stickyConversations) AgentShowsProgress(context.Context, string) bool { return true }

func (f *stickyConversations) InterruptRun(context.Context, string) error {
	return nil
}

func (f *stickyConversations) SyncRunEvents(context.Context, string) error { return nil }

func (f *stickyConversations) RunStatus(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return "", f.statusErr
	}
	if f.status == "" {
		return "running", nil
	}
	return f.status, nil
}

func (f *stickyConversations) streamCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streamCalls
}

// stickyFixture wires up an inbound handler using stickyConversations.
func stickyFixture(t *testing.T, conv *stickyConversations, sender *fakeSender) (*Inbound, Store) {
	t.Helper()
	store := NewMemStore()
	if _, err := store.CreateApp(context.Background(),
		&App{ID: "fsa_1", WorkspaceID: "ws_1", AppID: "cli_1", Name: "测试应用"},
		"secret"); err != nil {
		t.Fatalf("预置应用失败: %v", err)
	}
	in, err := NewInbound(InboundConfig{
		Store:         store,
		Conversations: conv,
		Sender:        sender,
		Tokens:        fakeTokens{},
	})
	if err != nil {
		t.Fatalf("建入站处理器失败: %v", err)
	}
	return in, store
}

// endTurnFrame builds a frame of "this turn really finished answering".
//
// stop_reason=end_turn is Ark's normal finish (the measured shape; see that
// sesn-...-kvz6t session in the run_events table).
func endTurnFrame() []byte {
	return mustJSON(map[string]any{
		"type":        "session.status_idle",
		"stop_reason": map[string]any{"type": "end_turn"},
	})
}

// requiresActionFrame builds a frame of "the agent is stuck waiting for a tool result".
//
// **This is also session.status_idle**; the only difference is stop_reason. In a real turn it appears in the middle:
// after the agent starts a tool call Ark goes idle, and continues once the worker returns the result.
func requiresActionFrame(callID string) []byte {
	return mustJSON(map[string]any{
		"type": "session.status_idle",
		"stop_reason": map[string]any{
			"type": "requires_action", "event_ids": []string{callID},
		},
	})
}

// terminatedFrame builds a frame of "the session was terminated".
func terminatedFrame() []byte {
	return mustJSON(map[string]any{"type": "session.status_terminated"})
}

// handleWithin runs Handle once, failing if it takes longer than limit.
//
// It is used instead of calling directly: what this group really proves is that the lock is "released **in time**", and without timeout protection
// a deadlock shows up as the whole test hanging until go test's 10-minute limit, with an error that does not point to the real cause.
func handleWithin(t *testing.T, in *Inbound, msg *lark.InboundMessage, limit time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- in.Handle(context.Background(), msg) }()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		t.Fatalf("处理 %s 超过 %v 还没返回——多半是流没自己结束、串行锁没放开", msg.MessageID, limit)
		return nil
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestTurnEndedRecognizesArkFrames pins the final-state test itself.
//
// This is the decision core of the whole path: misjudging one frame either keeps the lock (the end is not recognized),
// or cuts the answer in half (requires_action taken as the end).
func TestTurnEndedRecognizesArkFrames(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want bool
	}{
		{"end_turn 是结束", endTurnFrame(), true},
		{"terminated 是结束", terminatedFrame(), true},
		{"没有 stop_reason 的 idle 按结束处理", mustJSON(map[string]any{
			"type": "session.status_idle",
		}), true},
		{"requires_action 不是结束", requiresActionFrame("call_1"), false},
		{"agent.message 不是结束", mustJSON(map[string]any{
			"type": "agent.message",
		}), false},
		{"thread 级的 idle 不是会话结束", mustJSON(map[string]any{
			"type": "session.thread_status_idle",
		}), false},
		{"[DONE] 不是 JSON，安静跳过", []byte("[DONE]"), false},
		{"空字节不炸", []byte(""), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := arkframe.TurnEnded(c.raw); got != c.want {
				t.Errorf("arkframe.TurnEnded(%s) = %v，想要 %v", c.raw, got, c.want)
			}
		})
	}
}

// TestSecondMessageWorksAfterTurnEnds is the **regression test** for this incident.
//
// After the previous turn finished normally (end_turn), the next message must really start, not get
// "the previous message is still being processed". Before the change this hangs on the first message until runTimeout.
func TestSecondMessageWorksAfterTurnEnds(t *testing.T) {
	conv := &stickyConversations{
		frames: [][]byte{
			agentFrame("lark-cli 没装"),
			endTurnFrame(),
		},
		status: "idle",
	}
	sender := &fakeSender{}
	in, store := stickyFixture(t, conv, sender)

	if err := handleWithin(t, in, inboundMsg("om_1", "你有 lark-cli 吗"), 5*time.Second); err != nil {
		t.Fatalf("第一轮不该报错: %v", err)
	}

	// The lock must already be released: no unfinished turn may remain in the store.
	recs, err := store.ListInflightTurns(context.Background())
	if err != nil {
		t.Fatalf("列未结轮次失败: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("上一轮答完了，串行锁必须放开，却还剩 %d 条记录: %+v", len(recs), recs)
	}

	if err := handleWithin(t, in, inboundMsg("om_2", "你给我讲个故事"), 5*time.Second); err != nil {
		t.Fatalf("第二轮不该报错: %v", err)
	}

	if got := conv.streamCount(); got != 2 {
		t.Errorf("第二句话必须真的开跑（订阅 2 次），实际只订阅了 %d 次", got)
	}
	if busy := busyReplies(sender); busy != 0 {
		t.Errorf("上一轮已结束，第二句话不该收到「上一句还在处理中」，却收到 %d 次", busy)
	}
}

// TestRequiresActionDoesNotEndTurn: an idle with requires_action **must not** be taken as the end.
//
// In a real turn it appears in the middle (the agent is waiting for a tool result). Taking it as the end drops the stream right when the agent is about to continue,
// and the user gets half an answer, which is harder to notice than a leftover lock because it gives no error.
func TestRequiresActionDoesNotEndTurn(t *testing.T) {
	conv := &stickyConversations{
		frames: [][]byte{
			agentFrame("我查一下。"),
			requiresActionFrame("call_1"), // idle midway, the stream should not be dropped
		},
	}
	in, _ := stickyFixture(t, conv, &fakeSender{})

	done := make(chan error, 1)
	go func() { done <- in.Handle(context.Background(), inboundMsg("om_1", "查一下")) }()

	select {
	case <-done:
		t.Fatal("requires_action 被当成了结束——agent 还在等工具结果，这时断流会切掉后半段答案")
	case <-time.After(300 * time.Millisecond):
		// Expected: still hanging. On the normal path Ark keeps sending frames after the worker returns the result.
	}
}

// TestTurnLockReleasedAfterStreamError: after the previous turn exits with **an error**, the next message can still go through.
//
// This is one of the shapes of the incident: the previous turn hit a 403 on the execution side, and the path ended abnormally.
// Cleanup hung on defer should run just the same; there is no reason it should only run on success.
func TestTurnLockReleasedAfterStreamError(t *testing.T) {
	conv := &stickyConversations{
		appendErr: errors.New("方舟 500"),
		status:    "idle",
	}
	in, store := stickyFixture(t, conv, &fakeSender{})

	// Failing to send makes Handle return an error (Feishu will redeliver); here we only care whether the lock was released.
	_ = handleWithin(t, in, inboundMsg("om_1", "第一句"), 5*time.Second)

	recs, err := store.ListInflightTurns(context.Background())
	if err != nil {
		t.Fatalf("列未结轮次失败: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("这一轮以报错收尾，锁也必须放开，却还剩 %d 条: %+v", len(recs), recs)
	}
}

// TestStaleTurnReclaimedWhenRunFinished: a **leftover** lock must be able to heal itself.
//
// Simulates "the previous turn's process was killed with kill -9 and the defer never ran": the store has an inflight record,
// while the turn on the Ark side has long been final. Then the new message must not be blocked: after TryBeginTurn fails
// it must check the run's real state, and if final, reclaim the record and let it through.
//
// Why not rely on staleAfter: that would wait a full runTimeout (10 minutes),
// during which every question from the user would be blocked.
func TestStaleTurnReclaimedWhenRunFinished(t *testing.T) {
	conv := &stickyConversations{
		frames: [][]byte{agentFrame("好的"), endTurnFrame()},
		status: "idle", // Ark says: this session has ended
	}
	sender := &fakeSender{}
	in, store := stickyFixture(t, conv, &fakeSender{})
	in.cfg.Sender = sender

	// Plant a leftover record by hand, pretending "the previous turn was not cleaned up".
	if err := store.PutInflightTurn(context.Background(), InflightTurnRecord{
		RunID:        "sesn-sticky",
		LarkAppID:    "cli_1",
		ChatID:       "oc_1",
		SrcMessageID: "om_old",
		Status:       inflightOpen,
		StartedAt:    time.Now(), // very new, staleAfter cannot cover it
	}); err != nil {
		t.Fatalf("埋残留记录失败: %v", err)
	}
	// Point the scope binding at the same session, so the new message lands on it.
	if _, _, err := store.BindSessionScope(context.Background(), SessionScopeRecord{
		LarkAppID: "cli_1", ScopeKey: "chat:oc_1", AgentID: "agent_1",
		RunID: "sesn-sticky", WorkspaceID: "ws_1", ScopeType: "chat", ChatID: "oc_1",
		CreatedAt: time.Now(), LastUsedAt: time.Now(), TurnCount: 1,
	}); err != nil {
		t.Fatalf("预置作用域绑定失败: %v", err)
	}

	if err := handleWithin(t, in, inboundMsg("om_new", "讲个故事"), 5*time.Second); err != nil {
		t.Fatalf("不该报错: %v", err)
	}

	if busy := busyReplies(sender); busy != 0 {
		t.Errorf("残留记录关联的 run 已是终态，不该挡住用户，却回了 %d 次「还在处理中」", busy)
	}
	if got := conv.streamCount(); got != 1 {
		t.Errorf("这句话必须真的开跑，实际订阅 %d 次", got)
	}
}

// TestStaleTurnKeptWhenRunStillRunning: under real concurrency it **must** block.
//
// The mirror of the previous one: the linked run is still running, so that inflight record is real,
// and letting it through would let two concurrent turns mix up the replies (both sides collect what the agent says and cannot tell which is for whom).
// The self-healing logic must not be so loose that it lets this case through too.
func TestStaleTurnKeptWhenRunStillRunning(t *testing.T) {
	conv := &stickyConversations{status: "running"} // Ark says: still running
	sender := &fakeSender{}
	in, store := stickyFixture(t, conv, sender)

	if err := store.PutInflightTurn(context.Background(), InflightTurnRecord{
		RunID: "sesn-sticky", LarkAppID: "cli_1", ChatID: "oc_1",
		SrcMessageID: "om_old", Status: inflightOpen, StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("埋记录失败: %v", err)
	}
	if _, _, err := store.BindSessionScope(context.Background(), SessionScopeRecord{
		LarkAppID: "cli_1", ScopeKey: "chat:oc_1", AgentID: "agent_1",
		RunID: "sesn-sticky", WorkspaceID: "ws_1", ScopeType: "chat", ChatID: "oc_1",
		CreatedAt: time.Now(), LastUsedAt: time.Now(), TurnCount: 1,
	}); err != nil {
		t.Fatalf("预置作用域绑定失败: %v", err)
	}

	if err := handleWithin(t, in, inboundMsg("om_new", "再问一句"), 5*time.Second); err != nil {
		t.Fatalf("不该报错: %v", err)
	}

	if busy := busyReplies(sender); busy != 1 {
		t.Errorf("前一轮确实还在跑，必须回一次「还在处理中」，实际 %d 次", busy)
	}
	if got := conv.streamCount(); got != 0 {
		t.Errorf("前一轮还在跑，这句话不该开跑，却订阅了 %d 次", got)
	}
}

// TestStaleTurnKeptWhenStatusUnknown: when the truth cannot be found, keep the original judgment.
//
// RunStatus failing means we **do not know** whether the previous turn ended. Then better to block and let the user resend
// than to gamble on opening a second turn: guessing wrong costs two concurrent turns garbling the reply, worse than waiting a bit longer.
func TestStaleTurnKeptWhenStatusUnknown(t *testing.T) {
	conv := &stickyConversations{statusErr: errors.New("方舟不可达")}
	sender := &fakeSender{}
	in, store := stickyFixture(t, conv, sender)

	if err := store.PutInflightTurn(context.Background(), InflightTurnRecord{
		RunID: "sesn-sticky", LarkAppID: "cli_1", ChatID: "oc_1",
		SrcMessageID: "om_old", Status: inflightOpen, StartedAt: time.Now(),
	}); err != nil {
		t.Fatalf("埋记录失败: %v", err)
	}
	if _, _, err := store.BindSessionScope(context.Background(), SessionScopeRecord{
		LarkAppID: "cli_1", ScopeKey: "chat:oc_1", AgentID: "agent_1",
		RunID: "sesn-sticky", WorkspaceID: "ws_1", ScopeType: "chat", ChatID: "oc_1",
		CreatedAt: time.Now(), LastUsedAt: time.Now(), TurnCount: 1,
	}); err != nil {
		t.Fatalf("预置作用域绑定失败: %v", err)
	}

	if err := handleWithin(t, in, inboundMsg("om_new", "再问一句"), 5*time.Second); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if busy := busyReplies(sender); busy != 1 {
		t.Errorf("状态问不出来时应维持原判挡住，实际回了 %d 次", busy)
	}
}

// TestManyTurnsInARowAllGetThrough: ask five messages in a row, and every one must run.
//
// Looking at a single turn it is easy to be "right by luck"; if the lock fails to release even once, everything after is blocked.
// This turns it into one definite number.
func TestManyTurnsInARowAllGetThrough(t *testing.T) {
	conv := &stickyConversations{
		frames: [][]byte{agentFrame("好"), endTurnFrame()},
		status: "idle",
	}
	sender := &fakeSender{}
	in, _ := stickyFixture(t, conv, sender)

	for i := range 5 {
		msg := inboundMsg("om_"+string(rune('a'+i)), "第几句都行")
		if err := handleWithin(t, in, msg, 5*time.Second); err != nil {
			t.Fatalf("第 %d 句报错: %v", i+1, err)
		}
	}
	if got := conv.streamCount(); got != 5 {
		t.Errorf("五句话必须都开跑，实际只跑了 %d 句", got)
	}
	if busy := busyReplies(sender); busy != 0 {
		t.Errorf("每一轮都答完了才问下一句，不该有任何「还在处理中」，实际 %d 次", busy)
	}
}

// busyReplies counts how many times "the previous message is still being processed" was sent.
func busyReplies(sender *fakeSender) int {
	n := 0
	for _, c := range sender.sent() {
		if strings.Contains(c.text, "上一句还在处理中") {
			n++
		}
	}
	return n
}
