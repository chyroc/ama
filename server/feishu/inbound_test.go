// Tests for inbound orchestration. All offline: core and Feishu message sending use fakes.
//
// The key four things pinned, all of the "wrong but no error, just wrong behavior" kind:
//
//   - Dedup: the same message coming in twice runs the agent only once
//   - failed let through: the one that died last time is retried on redelivery
//   - Order: subscribe before sending, or the beginning of the reply is neither seen nor stored
//   - Return value meaning: returning error makes Feishu redeliver, so "ignored" and "already handled" must both return nil

package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/shared/gopkg/arkframe"
	"github.com/chyroc/ama/server/shared/infra/lark"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// redirectCall records the arguments of one redirect.
type redirectCall struct {
	runID string
	text  string
}

type fakeConversations struct {
	mu sync.Mutex

	agentIDs []string
	agentErr error

	created   int
	createErr error

	appended  [][]json.RawMessage
	appendErr error
	// appendHook is called back at the moment of "sending the message to Ark", to assert the store state at that moment.
	appendHook func()

	// frames are the events handed to the caller on subscribe.
	frames [][]byte
	// order records the call order, to assert "subscribe first, then send the message".
	order []string

	// -- The following are for the recovery path (resume_test.go) --

	// syncCalls records how many times SyncRunEvents was called.
	syncCalls int
	syncErr   error
	// Agents in queueingAgents take "queue"; the rest take "redirect" (the default mode).
	queueingAgents map[string]bool
	// redirects records the arguments of every redirect, to assert "which session was interrupted and what it was changed to".
	redirects []redirectCall
	// redirectErr makes the redirect fail, to check "on failure fall back to the queued notice, never stay silent".
	redirectErr error

	// status is the status RunStatus returns; an empty string is treated as running.
	status    string
	statusErr error
	// syncHook is called back in SyncRunEvents, to simulate "Ark finished this turn during the gap":
	// the fill-in itself computes the final state, so the status should change at this moment.
	syncHook func()
	// details records the source description carried by each Feishu session creation (Feishu direct chat / Feishu group chat).
	details []string
	// agentsAddressed records **which agent** each session was created for.
	//
	// The constraint "one inbound message goes to only one agent" is asserted with it: without recording it, the tests would be completely blind to
	// "the same message was dispatched to two agents"; session counts and event counts could both look normal.
	agentsAddressed []string

	// disabledAgents is the set of "disabled" agents, which AgentDisabled answers from.
	// Empty by default = all enabled, so existing tests are not affected.
	disabledAgents map[string]bool

	// briefAgents is the set of agents configured to "not show detailed work".
	//
	// **Empty by default = all shown**, matching the default side of apimodel.ProgressDetail (empty string = full),
	// so existing tests are not affected; this is exactly how a switch that is "on by default" should look.
	briefAgents map[string]bool
}

// addressedAgents returns the target agents of every session creation.
func (f *fakeConversations) addressedAgents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.agentsAddressed...)
}

// CreateConversation returns a **different** session ID each time.
//
// Always returning the same ID once made this suite completely blind to "session reuse": each message created its own session,
// but since the IDs were the same, the assertions could not tell the difference. With an incrementing suffix,
// "created a new one when it should have reused" immediately shows up as different IDs.
func (f *fakeConversations) CreateConversation(_ context.Context, _, agentID string) (Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	f.agentsAddressed = append(f.agentsAddressed, agentID)
	if f.createErr != nil {
		return Run{}, f.createErr
	}
	f.order = append(f.order, "create")
	return Run{ID: fmt.Sprintf("sesn-%d", f.created)}, nil
}

// CreateFeishuConversation uses the same counter as CreateConversation, and also records detail,
// so tests can assert "the Feishu path used the method with the source marker".
func (f *fakeConversations) CreateFeishuConversation(
	ctx context.Context, workspaceID, agentID, detail string,
) (Run, error) {
	f.mu.Lock()
	f.details = append(f.details, detail)
	f.mu.Unlock()
	return f.CreateConversation(ctx, workspaceID, agentID)
}

// createdDetails returns the source descriptions carried by every Feishu session creation.
func (f *fakeConversations) createdDetails() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.details...)
}

func (f *fakeConversations) AppendRunEvents(_ context.Context, _ string, raw []json.RawMessage) ([]json.RawMessage, error) {
	f.mu.Lock()
	f.order = append(f.order, "append")
	hook := f.appendHook
	err := f.appendErr
	f.mu.Unlock()

	// Called outside the lock, so that touching the store inside the hook does not collide with other goroutines on this lock.
	if hook != nil {
		hook()
	}
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.appended = append(f.appended, raw)
	return raw, nil
}

func (f *fakeConversations) StreamRunEvents(ctx context.Context, _ string, onFrame func(raw []byte) error) error {
	f.mu.Lock()
	f.order = append(f.order, "stream")
	frames := f.frames
	f.mu.Unlock()

	for _, frame := range frames {
		if err := onFrame(frame); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeConversations) AgentsBoundToFeishuApp(context.Context, string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.agentErr != nil {
		return nil, f.agentErr
	}
	return f.agentIDs, nil
}

func (f *fakeConversations) AgentDisabled(_ context.Context, agentID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.disabledAgents[agentID]
}

// AgentFollowUpQueues defaults to false (redirect), matching the default side of apimodel.FollowUpMode.
// Tests for the queue branch set queueingAgents[agentID] to true.
func (f *fakeConversations) AgentFollowUpQueues(_ context.Context, agentID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queueingAgents[agentID]
}

// AgentShowsProgress defaults to true (show), matching the default side of apimodel.ProgressDetail.
// Tests for the "do not show" branch set briefAgents[agentID] to true.
func (f *fakeConversations) AgentShowsProgress(_ context.Context, agentID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.briefAgents[agentID]
}

func (f *fakeConversations) InterruptRun(_ context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// text is left empty: the interrupt of the new path **carries no text** (the new message is sent by the caller through the normal path after getting the lock),
	// which is exactly how it differs from the old RedirectRun. Asserting text=="" guards this.
	f.redirects = append(f.redirects, redirectCall{runID: runID})
	return f.redirectErr
}

func (f *fakeConversations) SyncRunEvents(_ context.Context, _ string) error {
	f.mu.Lock()
	f.syncCalls++
	f.order = append(f.order, "sync")
	err := f.syncErr
	hook := f.syncHook
	f.mu.Unlock()

	// Called outside the lock, so the hook can freely change the fake's fields (simulating the final state changing after the fill-in).
	if hook != nil {
		hook()
	}
	return err
}

func (f *fakeConversations) RunStatus(_ context.Context, _ string) (string, error) {
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

func (f *fakeConversations) callOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

// createCount is the number of times CreateConversation was called.
//
// It is the core assertion of session reuse: after N turns in the same chat, this number must still be 1.
func (f *fakeConversations) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created
}

// appendedEvents returns the batches of events already sent into the session.
func (f *fakeConversations) appendedEvents() [][]json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]json.RawMessage(nil), f.appended...)
}

type sentCard struct {
	kind string // reply / update
	text string
	// idem is the idempotency key used when creating the card.
	//
	// It is recorded because **a collision gives no error**: Feishu drops the second card as a duplicate delivery and returns the first one's
	// message_id, so the second turn's reply overwrites the first turn's card. Only asserting on this key can pin it.
	idem string
	// inThread is whether reply_in_thread was set when this card was sent.
	//
	// **It decides whether the user's next sentence still counts as a main-line message**, so it is a hidden input of the session scope:
	// with it set, Feishu folds this question and answer into a topic, and the sentence the user types next in the client automatically lands in
	// this topic with root_id, so scopeForMessage computes the root message's key → reuses the same
	// session → hits the turn serial lock. The first link of the whole causal chain is this field, and it was not recorded before,
	// so "two p2p messages in a row, one blocked" looked like a bug (reading the code, two main-line messages compute two keys).
	inThread bool
}

type fakeSender struct {
	mu    sync.Mutex
	cards []sentCard
	// replyErr makes ReplyCard return an error.
	replyErr error
	// onReply is called back at the moment of sending, to assert the state "at that moment" (e.g. whether the chain of thought was closed).
	//
	// More reliable than comparing timestamps: the order assertion wants the causality "the chain of thought is final when the answer is sent",
	// not which of two moments came first.
	onReply func()
}

func (f *fakeSender) ReplyCard(
	_ context.Context, _, _ string, card map[string]any, opt lark.ReplyCardOption,
) (*lark.SentMessage, error) {
	f.mu.Lock()
	hook := f.onReply
	f.mu.Unlock()
	// Called outside the lock: the hook often reads the store, and holding the lock would collide with other goroutines.
	if hook != nil {
		hook()
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.replyErr != nil {
		return nil, f.replyErr
	}
	f.cards = append(f.cards, sentCard{
		kind: "reply", text: cardText(card), idem: opt.IdempotencyKey,
		inThread: opt.ReplyInThread,
	})
	return &lark.SentMessage{MessageID: "om_card"}, nil
}

func (f *fakeSender) UpdateCard(_ context.Context, _, _ string, card map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cards = append(f.cards, sentCard{kind: "update", text: cardText(card)})
	return nil
}

func (f *fakeSender) sent() []sentCard {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentCard(nil), f.cards...)
}

// cardText digs the body out of the card structure, for easy assertions.
func cardText(card map[string]any) string {
	body, _ := card["body"].(map[string]any)
	elements, _ := body["elements"].([]map[string]any)
	var b strings.Builder
	for _, el := range elements {
		if s, ok := el["content"].(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

type fakeTokens struct{ err error }

func (f fakeTokens) TenantAccessToken(context.Context, string, string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return "t-token", nil
}

// agentFrame builds an agent.message event frame.
func agentFrame(text string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"type":    "agent.message",
		"content": []map[string]any{{"type": "text", "text": text}},
	})
	return raw
}

// inboundFixture wires up a working inbound handler and pre-fills a connected app.
func inboundFixture(t *testing.T, conv *fakeConversations, sender *fakeSender) (*Inbound, Store) {
	t.Helper()
	return inboundFixtureWithLogger(t, conv, sender, nil)
}

// inboundFixtureWithLogger is like inboundFixture, but a logger can be injected to assert log content.
//
// The multi-binding alert test needs it: the **only** observable product of that behavior is a log line
// (the message is still answered), so without looking at the log there is no way to verify the alert was sent.
func inboundFixtureWithLogger(
	t *testing.T, conv *fakeConversations, sender *fakeSender, logger *slog.Logger,
) (*Inbound, Store) {
	t.Helper()
	store := NewMemStore()
	app := &App{
		ID:          "fsa_1",
		WorkspaceID: "ws_1",
		AppID:       "cli_1",
		Name:        "测试应用",
		// ⚠️ **It must be set**: otherwise selfBotOpenIDs computes an empty set, and isSelfMessage falls back to
		// the "block all bots" branch, so every test exercises the old logic, the new open_id comparison
		// never runs once, and the tests are all green. Hit this once: after changing trigger.go the whole package went green,
		// and it was nearly wrapped up as "fixed".
		BotOpenID: selfBotOpenID,
	}
	if _, err := store.CreateApp(context.Background(), app, "secret"); err != nil {
		t.Fatalf("预置应用失败: %v", err)
	}
	in, err := NewInbound(InboundConfig{
		Store:         store,
		Conversations: conv,
		Sender:        sender,
		Tokens:        fakeTokens{},
		Logger:        logger,
	})
	if err != nil {
		t.Fatalf("建入站处理器失败: %v", err)
	}
	return in, store
}

func inboundMsg(id, text string) *lark.InboundMessage {
	return &lark.InboundMessage{
		AppID:     "cli_1",
		MessageID: id,
		ChatID:    "oc_1",
		ChatType:  "group",
		Text:      text,
		Mentioned: true,
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestInboundDrivesAgentAndRepliesWithCard(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("好的，"), agentFrame("这就去查。")},
	}
	sender := &fakeSender{}
	in, _ := inboundFixture(t, conv, sender)

	if err := in.Handle(context.Background(), inboundMsg("om_1", "帮我查一下")); err != nil {
		t.Fatalf("处理失败: %v", err)
	}

	if conv.created != 1 {
		t.Errorf("该建一个会话，实际 %d 个", conv.created)
	}
	// What is sent into the session must be a user.message carrying the user's original words.
	if len(conv.appended) != 1 {
		t.Fatalf("该发一次消息，实际 %d 次", len(conv.appended))
	}
	var ev struct {
		Type    string `json:"type"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(conv.appended[0][0], &ev); err != nil {
		t.Fatalf("发出去的事件解不开: %v", err)
	}
	if ev.Type != "user.message" {
		t.Errorf("该发 user.message，得到 %q", ev.Type)
	}
	if ev.Content[0].Text != "帮我查一下" {
		t.Errorf("该带上用户原话，得到 %q", ev.Content[0].Text)
	}

	// The reply must land on the card, as the complete content of the two frames joined.
	cards := sender.sent()
	if len(cards) == 0 {
		t.Fatal("该发一张卡片")
	}
	last := cards[len(cards)-1]
	if last.text != "好的，这就去查。" {
		t.Errorf("卡片该是完整回复，得到 %q", last.text)
	}
	if cards[0].kind != "reply" {
		t.Errorf("第一张卡该是回复原消息，得到 %q", cards[0].kind)
	}
}

// Subscribe before sending: core events are only stored when someone is subscribed.
// TestInboundMarksRunAsFeishuSourced: sessions created from Feishu must carry a source description.
//
// It goes through CreateFeishuConversation rather than CreateConversation: core relies on this to mark the Run
// as trigger=feishu, so the web session list can tell "this one was a Feishu chat". Using the wrong method gives no error;
// it would just show every session in the run history as "manual", so it is pinned explicitly here.
func TestInboundMarksRunAsFeishuSourced(t *testing.T) {
	for _, tc := range []struct {
		name     string
		chatType string
		want     string
	}{
		{"群聊", "group", "飞书群聊"},
		{"单聊", ChatTypeP2P, "飞书单聊"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conv := &fakeConversations{
				agentIDs: []string{"agent_1"},
				frames:   [][]byte{agentFrame("好")},
			}
			in, _ := inboundFixture(t, conv, &fakeSender{})

			msg := inboundMsg("om_src", "在吗")
			msg.ChatType = tc.chatType
			if err := in.Handle(context.Background(), msg); err != nil {
				t.Fatalf("处理失败: %v", err)
			}

			details := conv.createdDetails()
			if len(details) != 1 {
				t.Fatalf("该走一次 CreateFeishuConversation，实际 %d 次"+
					"（0 次说明还在用 CreateConversation，Run 会被标成 manual）", len(details))
			}
			if details[0] != tc.want {
				t.Errorf("来源说明不对：想要 %q，得到 %q", tc.want, details[0])
			}
		})
	}
}

func TestInboundSubscribesBeforeSending(t *testing.T) {
	conv := &fakeConversations{agentIDs: []string{"agent_1"}, frames: [][]byte{agentFrame("ok")}}
	in, _ := inboundFixture(t, conv, &fakeSender{})

	if err := in.Handle(context.Background(), inboundMsg("om_1", "在吗")); err != nil {
		t.Fatalf("处理失败: %v", err)
	}

	order := conv.callOrder()
	streamAt, appendAt := -1, -1
	for i, op := range order {
		if op == "stream" && streamAt < 0 {
			streamAt = i
		}
		if op == "append" && appendAt < 0 {
			appendAt = i
		}
	}
	if streamAt < 0 || appendAt < 0 {
		t.Fatalf("订阅与发消息都该发生，实际顺序 %v", order)
	}
	if streamAt > appendAt {
		t.Errorf("必须先订阅再发消息，实际顺序 %v", order)
	}
}

// The same message coming in twice runs the agent only once.
func TestInboundDeduplicatesRepeatedDelivery(t *testing.T) {
	conv := &fakeConversations{agentIDs: []string{"agent_1"}, frames: [][]byte{agentFrame("ok")}}
	in, _ := inboundFixture(t, conv, &fakeSender{})
	ctx := context.Background()

	msg := inboundMsg("om_dup", "同一句话")
	if err := in.Handle(ctx, msg); err != nil {
		t.Fatalf("第一次处理失败: %v", err)
	}
	// Feishu redelivers the same message. **It must not return an error**; with an error it would keep redelivering.
	if err := in.Handle(ctx, msg); err != nil {
		t.Fatalf("重复投递该安静 ack，得到错误: %v", err)
	}

	if conv.created != 1 {
		t.Errorf("同一条消息只该跑一遍 agent，实际建了 %d 个会话", conv.created)
	}
}

// The one that died last time should be retried when Feishu redelivers it.
func TestInboundRetriesFailedMessage(t *testing.T) {
	store := NewMemStore()
	ctx := context.Background()
	rec := InboundMessageRecord{
		LarkAppID: "cli_1", MessageID: "om_x", ChatID: "oc_1",
		Status: inboundPending, CreatedAt: time.Now(),
	}
	if ok, err := store.ClaimInboundMessage(ctx, rec); err != nil || !ok {
		t.Fatalf("第一次该认领到: ok=%v err=%v", ok, err)
	}
	// Redelivered while pending: blocked.
	if ok, _ := store.ClaimInboundMessage(ctx, rec); ok {
		t.Error("pending 期间重投该被挡掉")
	}
	// Redelivered after being marked success: still blocked.
	if err := store.MarkInboundMessage(ctx, "om_x", inboundProcessed); err != nil {
		t.Fatalf("标记失败: %v", err)
	}
	if ok, _ := store.ClaimInboundMessage(ctx, rec); ok {
		t.Error("processed 之后重投该被挡掉")
	}
	// Redelivered after being marked failed: **let through**.
	if err := store.MarkInboundMessage(ctx, "om_x", inboundFailed); err != nil {
		t.Fatalf("标记失败: %v", err)
	}
	if ok, err := store.ClaimInboundMessage(ctx, rec); err != nil || !ok {
		t.Errorf("failed 之后重投该放行重试: ok=%v err=%v", ok, err)
	}
}

// Messages in a chat that do not @ the bot do not trigger, and must ack quietly.
func TestInboundIgnoresUnmentionedAndEmpty(t *testing.T) {
	conv := &fakeConversations{agentIDs: []string{"agent_1"}}
	in, _ := inboundFixture(t, conv, &fakeSender{})
	ctx := context.Background()

	unmentioned := inboundMsg("om_2", "随便聊聊")
	unmentioned.Mentioned = false
	if err := in.Handle(ctx, unmentioned); err != nil {
		t.Errorf("没 @ 该安静忽略，得到错误: %v", err)
	}

	empty := inboundMsg("om_3", "   ")
	if err := in.Handle(ctx, empty); err != nil {
		t.Errorf("空消息该安静忽略，得到错误: %v", err)
	}

	if err := in.Handle(ctx, nil); err != nil {
		t.Errorf("nil 该安静忽略，得到错误: %v", err)
	}

	if conv.created != 0 {
		t.Errorf("这些都不该建会话，实际建了 %d 个", conv.created)
	}
}

// With no agent bound, tell the user to configure one instead of silently ignoring them.
func TestInboundTellsUserWhenNoAgentBound(t *testing.T) {
	conv := &fakeConversations{agentIDs: nil}
	sender := &fakeSender{}
	in, _ := inboundFixture(t, conv, sender)

	if err := in.Handle(context.Background(), inboundMsg("om_4", "在吗")); err != nil {
		t.Fatalf("该安静处理完: %v", err)
	}
	if conv.created != 0 {
		t.Error("没绑 agent 不该建会话")
	}
	cards := sender.sent()
	if len(cards) != 1 {
		t.Fatalf("该回一句提示，实际发了 %d 张卡", len(cards))
	}
	if !strings.Contains(cards[0].text, "绑定") {
		t.Errorf("提示该告诉用户去绑定 agent，得到 %q", cards[0].text)
	}
}

// An agent failure must not propagate upwards: then Feishu would redeliver, the redelivery usually fails too, and the user gets a string of failure cards.
func TestInboundDoesNotBubbleAgentFailure(t *testing.T) {
	conv := &fakeConversations{
		agentIDs:  []string{"agent_1"},
		createErr: errors.New("方舟挂了"),
	}
	in, store := inboundFixture(t, conv, &fakeSender{})
	ctx := context.Background()

	if err := in.Handle(ctx, inboundMsg("om_5", "在吗")); err != nil {
		t.Errorf("agent 失败不该让飞书重投，得到错误: %v", err)
	}

	// But it must leave a failed state, so if Feishu really redelivers, it can try again.
	rec := InboundMessageRecord{
		LarkAppID: "cli_1", MessageID: "om_5", ChatID: "oc_1",
		Status: inboundPending, CreatedAt: time.Now(),
	}
	if ok, err := store.ClaimInboundMessage(ctx, rec); err != nil || !ok {
		t.Errorf("失败的消息该留 failed 状态以便重试: ok=%v err=%v", ok, err)
	}
}

// Even when not a single character was produced, reply with something; do not leave the user staring at "read but no reply".
func TestInboundRepliesEvenWithNoAgentOutput(t *testing.T) {
	conv := &fakeConversations{agentIDs: []string{"agent_1"}, frames: nil}
	sender := &fakeSender{}
	in, _ := inboundFixture(t, conv, sender)

	if err := in.Handle(context.Background(), inboundMsg("om_6", "在吗")); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	cards := sender.sent()
	if len(cards) == 0 {
		t.Fatal("没有输出时也该回一张卡")
	}
	if strings.TrimSpace(cards[len(cards)-1].text) == "" {
		t.Error("兜底卡片不能是空的")
	}
}

func TestNewInboundValidatesDependencies(t *testing.T) {
	full := InboundConfig{
		Store:         NewMemStore(),
		Conversations: &fakeConversations{},
		Sender:        &fakeSender{},
		Tokens:        fakeTokens{},
	}
	if _, err := NewInbound(full); err != nil {
		t.Fatalf("完整配置该能建: %v", err)
	}

	for name, mutate := range map[string]func(*InboundConfig){
		"Store":         func(c *InboundConfig) { c.Store = nil },
		"Conversations": func(c *InboundConfig) { c.Conversations = nil },
		"Sender":        func(c *InboundConfig) { c.Sender = nil },
		"Tokens":        func(c *InboundConfig) { c.Tokens = nil },
	} {
		t.Run("缺"+name, func(t *testing.T) {
			cfg := full
			mutate(&cfg)
			if _, err := NewInbound(cfg); err == nil {
				t.Errorf("缺 %s 该报错", name)
			}
		})
	}
}

// agentText only recognizes agent.message; other frames (thinking, status, [DONE]) must be skipped quietly.
func TestAgentTextOnlyPicksAgentMessages(t *testing.T) {
	if text, ok := arkframe.AgentText(agentFrame("你好")); !ok || text != "你好" {
		t.Errorf("该取到文本: text=%q ok=%v", text, ok)
	}

	thinking, _ := json.Marshal(map[string]any{
		"type":    "agent.thinking",
		"content": []map[string]any{{"type": "text", "text": "让我想想"}},
	})
	if _, ok := arkframe.AgentText(thinking); ok {
		t.Error("思考帧不该进卡片，那对群里的人是噪音")
	}

	// [DONE] is not JSON; it must not panic or be taken as text.
	if _, ok := arkframe.AgentText([]byte("[DONE]")); ok {
		t.Error("[DONE] 不该被当成文本")
	}
	if _, ok := arkframe.AgentText(nil); ok {
		t.Error("空帧不该被当成文本")
	}
}

// The body rendered from the card must be serialized correctly on the lark side, with update_multi true.
func TestCardTextRoundTrips(t *testing.T) {
	card := lark.TextCard("你好")
	if got := cardText(card); got != "你好" {
		t.Errorf("卡片正文取不出来，得到 %q", got)
	}
	if _, err := json.Marshal(card); err != nil {
		t.Fatalf("卡片该能序列化: %v", err)
	}
}

// When exchanging the token fails (credentials expired) it should not redeliver, but must leave failed.
func TestInboundHandlesTokenFailure(t *testing.T) {
	conv := &fakeConversations{agentIDs: []string{"agent_1"}}
	store := NewMemStore()
	if _, err := store.CreateApp(context.Background(),
		&App{ID: "fsa_1", WorkspaceID: "ws_1", AppID: "cli_1"}, "secret"); err != nil {
		t.Fatalf("预置应用失败: %v", err)
	}
	in, err := NewInbound(InboundConfig{
		Store:         store,
		Conversations: conv,
		Sender:        &fakeSender{},
		Tokens:        fakeTokens{err: fmt.Errorf("凭证失效")},
	})
	if err != nil {
		t.Fatalf("建入站处理器失败: %v", err)
	}

	if err := in.Handle(context.Background(), inboundMsg("om_7", "在吗")); err != nil {
		t.Errorf("换 token 失败不该让飞书重投，得到 %v", err)
	}
	if conv.created != 0 {
		t.Error("拿不到 token 就不该建会话")
	}
}

// ---------------------------------------------------------------------------
// Unfinished turn records (the write path of restart recovery)
// ---------------------------------------------------------------------------

// The record must be stored **before sending the message**.
//
// In the wrong order there is a real window: the message is already in Ark, the agent has started inferring, and we have not
// left any trace yet; that turn is lost forever, the card stays at thinking, and nobody can save it.
func TestInboundRecordsInflightBeforeSending(t *testing.T) {
	var orderAtAppend []string
	store := NewMemStore()
	if _, err := store.CreateApp(context.Background(),
		&App{ID: "fsa_1", WorkspaceID: "ws_1", AppID: "cli_1"}, "secret"); err != nil {
		t.Fatalf("预置应用失败: %v", err)
	}

	conv := &fakeConversations{agentIDs: []string{"agent_1"}, frames: [][]byte{agentFrame("ok")}}
	// At the moment of "sending the message to Ark", look back at whether the store has the record.
	conv.appendHook = func() {
		recs, err := store.ListInflightTurns(context.Background())
		if err != nil {
			t.Errorf("列未结轮次失败: %v", err)
			return
		}
		for _, rec := range recs {
			orderAtAppend = append(orderAtAppend, rec.RunID)
		}
	}

	in, err := NewInbound(InboundConfig{
		Store:         store,
		Conversations: conv,
		Sender:        &fakeSender{},
		Tokens:        fakeTokens{},
	})
	if err != nil {
		t.Fatalf("建入站处理器失败: %v", err)
	}
	if err := in.Handle(context.Background(), inboundMsg("om_inflight", "帮我查个东西")); err != nil {
		t.Fatalf("处理失败: %v", err)
	}

	if len(orderAtAppend) == 0 {
		t.Error("发消息到方舟时库里就该有未结轮次记录了，否则这一刻崩掉就永久失联")
	}
}

// A turn that finished successfully must clear its record.
//
// **This is a regression test.** A common mistake is hanging cleanup only off the failure path,
// so successful turns leave their records in the store, and the next restart answers already-answered questions again, giving the user two replies.
// So the cleanup point is gathered into the defer of runAndReply, and success/failure/timeout all end there.
func TestSuccessfulTurnClearsInflight(t *testing.T) {
	conv := &fakeConversations{agentIDs: []string{"agent_1"}, frames: [][]byte{agentFrame("查到了")}}
	in, store := inboundFixture(t, conv, &fakeSender{})

	if err := in.Handle(context.Background(), inboundMsg("om_done", "帮我查个东西")); err != nil {
		t.Fatalf("处理失败: %v", err)
	}

	recs, err := store.ListInflightTurns(context.Background())
	if err != nil {
		t.Fatalf("列未结轮次失败: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("跑完的轮次不该留下记录，实际还剩 %d 条——漏了会在下次重启时重答同一个问题", len(recs))
	}
}

// A failed turn also clears its record (through the same exit as the successful one).
func TestFailedTurnClearsInflight(t *testing.T) {
	conv := &fakeConversations{
		agentIDs:  []string{"agent_1"},
		appendErr: fmt.Errorf("方舟拒收"),
	}
	in, store := inboundFixture(t, conv, &fakeSender{})

	// Sending the message fails; Handle records it as failed and returns nil (so Feishu does not redeliver).
	if err := in.Handle(context.Background(), inboundMsg("om_fail", "帮我查个东西")); err != nil {
		t.Fatalf("agent 跑挂不该让飞书重投，得到 %v", err)
	}

	recs, err := store.ListInflightTurns(context.Background())
	if err != nil {
		t.Fatalf("列未结轮次失败: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("跑挂的轮次也该清掉记录，实际还剩 %d 条", len(recs))
	}
}
