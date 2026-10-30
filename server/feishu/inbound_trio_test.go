// Tests for the trilogy orchestration: OnIt reaction → chain of thought → separate answer.
//
// # Why this suite deserves its own file
//
// The code for these three steps **once all existed in this repo, all had unit tests, and none of it was called**
// (reply.go and progress_driver.go were two purely additive commits that never touched inbound.go).
// So "are the components right" was all green, while no assertion looked at "are the components wired in":
// in production the chat only had one card, and the logs had not a single error.
//
// So what is pinned here is not the internal logic of each part (that is in reply_test.go / transport_test.go),
// but **the orchestration itself**: all three steps really happen, in the right order, and the answer still gets through when any step breaks.
// Each one is of the "wrong but no error, just wrong behavior" kind.

package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// reactionOp records one reaction operation.
type reactionOp struct {
	kind  string // add / remove
	emoji string
	msgID string
}

type fakeReactor struct {
	mu  sync.Mutex
	ops []reactionOp
	// addErr makes adding a reaction fail, to check "a broken reaction does not affect the other two steps".
	addErr error
}

func (f *fakeReactor) AddReaction(_ context.Context, _, messageID, emojiType string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return "", f.addErr
	}
	f.ops = append(f.ops, reactionOp{kind: "add", emoji: emojiType, msgID: messageID})
	return "reaction_1", nil
}

func (f *fakeReactor) RemoveReaction(_ context.Context, _, messageID, reactionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, reactionOp{kind: "remove", emoji: reactionID, msgID: messageID})
	return nil
}

func (f *fakeReactor) snapshot() []reactionOp {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]reactionOp(nil), f.ops...)
}

// trioFixture sets up a complete trilogy environment: the reaction, chain of thought, and answer paths all work.
func trioFixture(
	t *testing.T, conv *fakeConversations,
) (*Inbound, *fakeSender, *fakeReactor, *fakeCOT, ProgressStore) {
	t.Helper()
	store := NewMemStore()
	if _, err := store.CreateApp(context.Background(), &App{
		ID: "fsa_1", WorkspaceID: "ws_1", AppID: "cli_1", Name: "测试应用",
	}, "secret"); err != nil {
		t.Fatalf("预置应用失败: %v", err)
	}
	sender, reactor, cot := &fakeSender{}, &fakeReactor{}, &fakeCOT{}
	progress := NewMemProgressStore()
	in, err := NewInbound(InboundConfig{
		Store:         store,
		Conversations: conv,
		Sender:        sender,
		Tokens:        fakeTokens{},
		Reactor:       reactor,
		Progress:      progress,
		COT:           cot,
	})
	if err != nil {
		t.Fatalf("建入站处理器失败: %v", err)
	}
	return in, sender, reactor, cot, progress
}

// agentThinkingFrame builds an agent.thinking frame (goes into the chain of thought, **not into the answer**).
func agentThinkingFrame(text string) []byte {
	return mustJSON(map[string]any{
		"type":    "agent.thinking",
		"content": []map[string]any{{"type": "text", "text": text}},
	})
}

// toolUseFrame builds a tool call frame.
func toolUseFrame(id, name string) []byte {
	return mustJSON(map[string]any{
		"type": "agent.tool_use", "id": id, "name": name,
		"input": map[string]any{"path": "/tmp/x"},
	})
}

// idleFrame builds a frame of "this turn has ended".
func idleFrame() []byte {
	return mustJSON(map[string]any{"type": "session.status_idle"})
}

func mustJSON(v map[string]any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestInboundRunsFullTrio: a normal conversation turn must have **all three steps happen**, in the right order.
//
// This is the main test of this file, and exactly the path that was broken before the change: before it there was only a rough version of step 3
// (a card PATCHed over and over), and the reaction and chain-of-thought code existed but had no caller.
func TestInboundRunsFullTrio(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames: [][]byte{
			agentThinkingFrame("先看看日志。"),
			toolUseFrame("call_1", "bash"),
			agentFrame("查到了，"),
			agentFrame("有 3 处异常。"),
			idleFrame(),
		},
	}
	in, sender, reactor, cot, _ := trioFixture(t, conv)

	if err := in.Handle(context.Background(), inboundMsg("om_1", "帮我查一下")); err != nil {
		t.Fatalf("处理失败: %v", err)
	}

	// —— Step 1: OnIt reaction, on **the user's message** ——
	ops := reactor.snapshot()
	if len(ops) == 0 || ops[0].kind != "add" {
		t.Fatalf("第一步应给用户消息打表情，实际操作 %+v", ops)
	}
	if ops[0].emoji != lark.ReactionOnIt {
		t.Errorf("表情应是 %q，实际 %q", lark.ReactionOnIt, ops[0].emoji)
	}
	if ops[0].msgID != "om_1" {
		t.Errorf("表情要贴在用户那条消息上（om_1），实际贴在 %q", ops[0].msgID)
	}
	// When done, OnIt is switched to DONE: remove first, then add, so the message always has exactly one reaction, and it is the current state.
	//
	// Removing without adding would make the message look "ignored" again, and users looking back could not tell "answered" from "not seen".
	if len(ops) != 3 {
		t.Fatalf("表情应有三步（加 OnIt → 撤 OnIt → 加 DONE），实际 %+v", ops)
	}
	if ops[1].kind != "remove" {
		t.Errorf("第二步应撤掉 OnIt，实际 %+v", ops[1])
	}
	if ops[2].kind != "add" || ops[2].emoji != lark.ReactionDone {
		t.Errorf("第三步应打上 DONE，实际 %+v", ops[2])
	}
	if ops[2].msgID != "om_1" {
		t.Errorf("DONE 也要贴在用户那条消息上，实际贴在 %q", ops[2].msgID)
	}

	// —— Step 2: the chain of thought was really created, pushed, and closed ——
	events, reasons, creates := cot.snapshot()
	if creates != 1 {
		t.Fatalf("应建一条思维链，实际 %d 条", creates)
	}
	if len(events) == 0 {
		t.Fatal("思维链里应有事件——一条都没有说明 driver 没被喂过")
	}
	if len(reasons) != 1 || reasons[0] != lark.COTCompleteDone {
		t.Errorf("思维链应以 done 封口一次，实际 %+v", reasons)
	}
	// Both thinking text and tool calls should go into the chain of thought.
	if !hasCOTEvent(events, lark.COTEventReasoningMessageContent) {
		t.Error("思考文本应进思维链")
	}
	if !hasCOTEvent(events, lark.COTEventToolCallStart) {
		t.Error("工具调用应进思维链")
	}

	// —— Step 3: the final answer is a **separate message**, with only the agent's text ——
	cards := sender.sent()
	if len(cards) != 1 {
		t.Fatalf("最终答案应是一条独立消息，实际发了 %d 条：%+v", len(cards), cards)
	}
	if cards[0].kind != "reply" {
		t.Errorf("答案应是新消息（reply）而不是 PATCH 旧卡（update），实际 %q", cards[0].kind)
	}
	answer := cards[0].text
	if !strings.Contains(answer, "查到了") || !strings.Contains(answer, "3 处异常") {
		t.Errorf("答案应含 agent 说的正文，实际 %q", answer)
	}
	// **The thinking process must not leak into the answer**: that is chain-of-thought content, and mixing it in would make the answer long and messy.
	if strings.Contains(answer, "先看看日志") {
		t.Errorf("思考过程不该出现在最终答案里，实际 %q", answer)
	}
}

// TestInboundClosesCOTBeforeSendingAnswer: the answer must be sent only **after** the chain of thought is closed.
//
// In the reverse order users see the conclusion first and then the thinking, which is odd. This asserts "at the moment the answer is sent the delivery
// is already final", not by comparing timestamps, which are unstable in tests.
func TestInboundClosesCOTBeforeSendingAnswer(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("答案。"), idleFrame()},
	}
	in, sender, _, _, progress := trioFixture(t, conv)

	// At the moment of "sending the answer", look back at the delivery state. ReplyCard is the only exit for sending the answer.
	var terminalAtSend bool
	sender.onReply = func() {
		d, err := progress.GetProgressDelivery(context.Background(),
			ProgressDeliveryKey("sesn-1", "om_1"))
		if err == nil {
			terminalAtSend = d.TerminalApplied
		}
	}

	if err := in.Handle(context.Background(), inboundMsg("om_1", "问题")); err != nil {
		t.Fatalf("处理失败: %v", err)
	}

	if !terminalAtSend {
		t.Error("发最终答案时思维链应已封口，否则用户先看到结论再看到思考过程")
	}
}

// TestInboundAnswersEvenWhenCOTFails: when the chain of thought is completely broken, the answer must still be delivered.
//
// **This is the only one of the three steps that must not fail.** The chain of thought is an enhancement: whether the tenant has not enabled it, the API is down,
// or the lease was taken, the user should still get an answer. This is where the change is most easily written wrong: propagating the closing error
// upwards, so a failure of an optional feature eats the whole turn.
func TestInboundAnswersEvenWhenCOTFails(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("即便如此也要回答。"), idleFrame()},
	}
	in, sender, reactor, cot, _ := trioFixture(t, conv)
	// Creating the chain of thought fails, as a "definite rejection" (APIError): it degrades to the card.
	cot.createErr = &lark.APIError{Code: "230098", Message: "租户未开启该能力"}

	if err := in.Handle(context.Background(), inboundMsg("om_1", "问题")); err != nil {
		t.Fatalf("思维链失败不该让整轮对话失败: %v", err)
	}

	// The answer must be there. The degradation path sends an extra progress card, so here we only require "the answer message is there".
	var gotAnswer bool
	for _, c := range sender.sent() {
		if strings.Contains(c.text, "即便如此也要回答") {
			gotAnswer = true
		}
	}
	if !gotAnswer {
		t.Errorf("思维链坏了也必须把答案发出去，实际发出的是 %+v", sender.sent())
	}
	// The reaction should be added and removed as usual.
	if len(reactor.snapshot()) < 2 {
		t.Errorf("思维链坏掉不该影响表情，实际 %+v", reactor.snapshot())
	}
}

// TestInboundAnswersWithoutProgressStore: without ProgressStore it degrades to "reaction + answer".
//
// Local AMA_STORE=mem is exactly this config. **It must still answer**: otherwise local development would look like
// the feature is broken, while really only an optional dependency is missing.
func TestInboundAnswersWithoutProgressStore(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("没有思维链也能答。"), idleFrame()},
	}
	sender, reactor := &fakeSender{}, &fakeReactor{}
	store := NewMemStore()
	if _, err := store.CreateApp(context.Background(), &App{
		ID: "fsa_1", WorkspaceID: "ws_1", AppID: "cli_1", Name: "测试应用",
	}, "secret"); err != nil {
		t.Fatalf("预置应用失败: %v", err)
	}
	in, err := NewInbound(InboundConfig{
		Store: store, Conversations: conv, Sender: sender,
		Tokens: fakeTokens{}, Reactor: reactor,
		// Neither Progress nor COT is configured.
	})
	if err != nil {
		t.Fatalf("建入站处理器失败: %v", err)
	}

	if err := in.Handle(context.Background(), inboundMsg("om_1", "问题")); err != nil {
		t.Fatalf("处理失败: %v", err)
	}

	cards := sender.sent()
	if len(cards) != 1 || !strings.Contains(cards[0].text, "没有思维链也能答") {
		t.Errorf("没配思维链时也要发答案，实际 %+v", cards)
	}
	if len(reactor.snapshot()) < 2 {
		t.Errorf("表情该照常打与撤，实际 %+v", reactor.snapshot())
	}
}

// TestInboundAnswersWhenReactionFails: when the reaction cannot be added, the other two steps go on as usual.
//
// This happens when the im:message.reactions:write_only permission is missing. The reaction is a pure hint,
// and its failure must be swallowed.
func TestInboundAnswersWhenReactionFails(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("没表情也能答。"), idleFrame()},
	}
	in, sender, reactor, cot, _ := trioFixture(t, conv)
	reactor.addErr = errors.New("没有表情权限")

	if err := in.Handle(context.Background(), inboundMsg("om_1", "问题")); err != nil {
		t.Fatalf("表情失败不该让整轮对话失败: %v", err)
	}

	if _, _, creates := cot.snapshot(); creates != 1 {
		t.Errorf("表情失败不该影响思维链，实际建了 %d 条", creates)
	}
	cards := sender.sent()
	if len(cards) != 1 || !strings.Contains(cards[0].text, "没表情也能答") {
		t.Errorf("表情失败不该影响答案，实际 %+v", cards)
	}
	// If it was never added, do not remove it: removing a non-existent reaction is a wasted remote call.
	for _, op := range reactor.snapshot() {
		if op.kind == "remove" {
			t.Error("表情没打上时不该去撤")
		}
	}
}

// TestInboundSecondTurnAlsoGetsCOT: the second turn of the same session **must also have a chain of thought**.
//
// # This test guards against a silent failure
//
// The delivery table uses a "key" as the primary key, and with sessions reused by scope one runID runs many turns. If the key were only
// runID, the second turn's EnsureProgressDelivery would hit the first turn's **already final** record and read it back,
// and the driver would see Finished() and return directly: from the second turn on there would be no chain of thought, and:
//
//	clean logs, every call returns nil, and the answer is sent as usual.
//
// That is, apart from "the thinking process is gone" there is no clue at all. See the turnprogress.go file header.
func TestInboundSecondTurnAlsoGetsCOT(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentThinkingFrame("想一下。"), agentFrame("答案。"), idleFrame()},
	}
	in, _, _, cot, _ := trioFixture(t, conv)

	for _, id := range []string{"om_1", "om_2"} {
		if err := in.Handle(context.Background(), inboundMsg(id, "问题")); err != nil {
			t.Fatalf("处理 %s 失败: %v", id, err)
		}
	}

	// The session must be reused, otherwise this test does not check what it means to check.
	if got := conv.createCount(); got != 1 {
		t.Fatalf("前置条件不成立：同一个群该复用会话，实际建了 %d 条", got)
	}

	_, reasons, creates := cot.snapshot()
	if creates != 2 {
		t.Errorf("两轮各该有自己的思维链，实际只建了 %d 条——第二轮起用户看不到思考过程", creates)
	}
	if len(reasons) != 2 {
		t.Errorf("两轮都该封口，实际封了 %d 次", len(reasons))
	}
}

// TestInboundSplitsLongAnswerIntoSeparateMessages: an answer with too many tables must be split into several separate messages.
//
// Feishu limits the number of tables in one card, and **over the limit the whole card is silently dropped** (230099): what the user sees
// is "the bot thought for a long time and said nothing in the end".
func TestInboundSplitsLongAnswerIntoSeparateMessages(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxCardTables+2; i++ {
		b.WriteString("| 列 |\n| --- |\n| 值 |\n\n段落\n\n")
	}
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame(b.String()), idleFrame()},
	}
	in, sender, _, _, _ := trioFixture(t, conv)

	if err := in.Handle(context.Background(), inboundMsg("om_1", "给我表格")); err != nil {
		t.Fatalf("处理失败: %v", err)
	}

	cards := sender.sent()
	if len(cards) < 2 {
		t.Fatalf("表格超限的答案应切成多条，实际 %d 条", len(cards))
	}
	// Each must have its own idempotency key, or Feishu would drop the later ones as duplicate deliveries.
	seen := make(map[string]bool)
	for i, c := range cards {
		if c.idem == "" {
			t.Errorf("第 %d 条没有幂等键", i+1)
		}
		if seen[c.idem] {
			t.Errorf("第 %d 条的幂等键与前面重复（%q），飞书会把它丢掉", i+1, c.idem)
		}
		seen[c.idem] = true
	}
}

// TestInboundClosesCOTWhenSendingMessageFails: when the message never got into Ark, the chain of thought must still be closed.
//
// Without closing, the chat is left with a thinking process that spins forever, and nobody can touch it again: the cot_id only lived in
// the memory of that request.
func TestInboundClosesCOTWhenSendingMessageFails(t *testing.T) {
	conv := &fakeConversations{
		agentIDs:  []string{"agent_1"},
		appendErr: errors.New("方舟拒收"),
	}
	in, _, reactor, cot, _ := trioFixture(t, conv)

	// An agent failure must return nil: a redelivery usually fails again, and would only give the user a string of failure notices.
	if err := in.Handle(context.Background(), inboundMsg("om_1", "问题")); err != nil {
		t.Fatalf("agent 侧失败应被吞掉，实际返回 %v", err)
	}

	_, reasons, _ := cot.snapshot()
	if len(reasons) == 0 {
		t.Fatal("发消息失败时思维链也必须封口，否则群里留一条永远在转的思考过程")
	}
	if reasons[0] != lark.COTCompleteError {
		t.Errorf("这种失败该以 error 封口，实际 %q", reasons[0])
	}
	// The reaction must be removed too, or it shows "processing" forever.
	var removed bool
	for _, op := range reactor.snapshot() {
		if op.kind == "remove" {
			removed = true
		}
	}
	if !removed {
		t.Error("失败路径也要撤表情")
	}
}

// TestInboundPersistsAnswerForResume: the answer must be stored as it is collected, or a restart loses the part already produced.
//
// Ark **does not replay frames consumed by the previous process**, so without storing, recovery could only get the part after the gap,
// and the user would see an answer missing its beginning, with no sign that anything was missing.
func TestInboundPersistsAnswerForResume(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("前半段。")},
	}
	in, _, _, _, _ := trioFixture(t, conv)

	// At the moment of "sending the message into Ark", look back at the store: the first frame has not come yet, but the record must already be there.
	var recordExists bool
	conv.appendHook = func() {
		recs, err := in.cfg.Store.ListInflightTurns(context.Background())
		recordExists = err == nil && len(recs) == 1
	}

	if err := in.Handle(context.Background(), inboundMsg("om_1", "问题")); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if !recordExists {
		t.Error("发消息进方舟之前就该有 inflight 记录，否则那一刻崩掉这轮永久失联")
	}
}

// TestInboundDoesNotClaimDoneWhenSendFails: when the answer was not sent, **DONE must not be added**.
//
// DONE tells the user "answered". Adding it when sending failed means the user sees "done" while having nothing
// in hand, which is more misleading than no reaction. So this path only removes OnIt.
func TestInboundDoesNotClaimDoneWhenSendFails(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("答案。"), idleFrame()},
	}
	in, sender, reactor, _, _ := trioFixture(t, conv)
	sender.replyErr = errors.New("飞书拒收")

	// Failing to send is "it did not work this time"; the agent side has finished, and Handle still returns nil (so Feishu does not redeliver).
	if err := in.Handle(context.Background(), inboundMsg("om_1", "问题")); err != nil {
		t.Fatalf("发送失败应被吞掉，实际返回 %v", err)
	}

	for _, op := range reactor.snapshot() {
		if op.kind == "add" && op.emoji == lark.ReactionDone {
			t.Error("答案没发出去时不该打 DONE——那是在谎报「已完成」")
		}
	}
	// OnIt must still be removed: this turn really is no longer being processed.
	var removed bool
	for _, op := range reactor.snapshot() {
		if op.kind == "remove" {
			removed = true
		}
	}
	if !removed {
		t.Error("失败路径也要撤掉 OnIt，否则消息永远显示「正在处理」")
	}
}

// TestMarkDoneSkipsDoneWhenRemoveFails: when removing OnIt fails, do not add DONE.
//
// Otherwise the message would have both "processing" and "done" reactions, and the user would not know which to believe,
// more confusing than one reaction fewer.
func TestMarkDoneSkipsDoneWhenRemoveFails(t *testing.T) {
	reactor := &failRemoveReactor{}
	markDone(context.Background(), reactor, "t", "om_1", "reaction_1", slog.Default())

	if reactor.added {
		t.Error("撤 OnIt 失败后不该打 DONE，否则消息上两个表情并列")
	}
}

// failRemoveReactor's RemoveReaction always fails.
type failRemoveReactor struct{ added bool }

func (f *failRemoveReactor) AddReaction(context.Context, string, string, string) (string, error) {
	f.added = true
	return "r", nil
}

func (f *failRemoveReactor) RemoveReaction(context.Context, string, string, string) error {
	return errors.New("撤不掉")
}

// hasCOTEvent decides whether the event list has a given type.
func hasCOTEvent(events []lark.COTEvent, eventType string) bool {
	for _, e := range events {
		if e.EventType == eventType {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The "show detailed work" switch
// ---------------------------------------------------------------------------

// An agent with the switch off **does not create a CoT at all**; this turn only has "reaction + answer".
//
// The assertion is `cot.creates == 0`, not "the events have no thinking text": the latter also holds when "created but no content
// pushed", and would not reveal "an extra empty chain of thought in the chat". This difference is the whole value of this test.
//
// Why not pass cot_hidden: in practice that parameter makes the message stay at "thinking" forever after closing;
// see the comments in shared/infra/lark/cot.go and on apimodel.ProgressDetail.
func TestInboundSkipsCOTWhenProgressDetailBrief(t *testing.T) {
	conv := &fakeConversations{
		agentIDs:    []string{"agent_1"},
		frames:      [][]byte{agentFrame("不展示过程，只给结论。"), idleFrame()},
		briefAgents: map[string]bool{"agent_1": true},
	}
	in, sender, reactor, cot, _ := trioFixture(t, conv)

	if err := in.Handle(context.Background(), inboundMsg("om_1", "问题")); err != nil {
		t.Fatalf("不该失败: %v", err)
	}

	// **Not a single chain of thought should be created.**
	if cot.creates != 0 {
		t.Errorf("关掉开关就不该建 CoT，实际建了 %d 条", cot.creates)
	}
	if events, _, _ := cot.snapshot(); len(events) != 0 {
		t.Errorf("既然没建 CoT，就不该有任何事件：%d 条", len(events))
	}
	// But the answer and the reaction go on as usual: this is degradation, not turning the feature off.
	var gotAnswer bool
	for _, c := range sender.sent() {
		if strings.Contains(c.text, "不展示过程") {
			gotAnswer = true
		}
	}
	if !gotAnswer {
		t.Errorf("答案必须照发，实际 %+v", sender.sent())
	}
	if len(reactor.snapshot()) < 2 {
		t.Errorf("表情该照常打与撤，实际 %+v", reactor.snapshot())
	}
}

// By default (the field never configured) a CoT is still created.
//
// Paired with the previous one: with only "off means not created" and no "created by default", flipping the switch the wrong way would also be all green.
func TestInboundBuildsCOTByDefault(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentThinkingFrame("想一下"), agentFrame("结论。"), idleFrame()},
	}
	in, _, _, cot, _ := trioFixture(t, conv)

	if err := in.Handle(context.Background(), inboundMsg("om_1", "问题")); err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	if cot.creates != 1 {
		t.Errorf("默认该建一条思维链，实际建了 %d 条", cot.creates)
	}
}
