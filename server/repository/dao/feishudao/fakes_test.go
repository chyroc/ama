// Fakes for the inbound path, **copied from feishu/inbound_test.go**.
//
// # Why copied and not shared
//
// Both sides need these fakes: the inbound tests in the feishu package, and the scope / anchor tests moved into this package.
// But a shared package **does not work at the language level**: the shared package must import feishu (the fakes implement
// feishu.Conversations and build feishu.App), and feishu's own tests are package feishu,
// so `feishu(test) -> shared package -> feishu` is "import cycle not allowed in test".
// Confirmed by trying it, not a guess.
//
// So each side has its own copy. **When changing one, update the other too**: the other is in feishu/inbound_test.go.
// These two are fixtures for the same set of assertions; if one side adds a field and the other does not,
// the symptom is "the same behavior gives different results in the two packages".

package feishudao

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/feishu"
	"github.com/chyroc/ama/server/feishu/feishumodel"
	"github.com/chyroc/ama/server/shared/infra/lark"
)

type fakeConversations struct {
	mu sync.Mutex

	agentIDs []string
	agentErr error

	created   int
	createErr error

	appended  [][]json.RawMessage
	appendErr error
	// appendHook is called back at the moment of "sending the message to Ark", to assert the database state at that moment.
	appendHook func()

	// frames are the events to hand to the caller on subscribe.
	frames [][]byte
	// order records the call order, to assert "subscribe first, then send the message".
	order []string

	// -- The following are for the resume path (resume_test.go) --

	// syncCalls counts how many times SyncRunEvents was called.
	syncCalls int
	syncErr   error
	// status is the status RunStatus returns; an empty string counts as running.
	status    string
	statusErr error
	// syncHook is called back inside SyncRunEvents, to simulate "Ark finished this turn during the gap".
	// The catch-up itself computes the final state, so the status must change at this moment.
	syncHook func()
	// details records the source label each Feishu conversation was created with (the "Feishu DM" / "Feishu group chat" labels).
	details []string
	// agentsAddressed records **which agent** each conversation was created for.
	//
	// The rule "one inbound message goes to only one agent" is asserted with it. Without it, tests would be completely blind to
	// "the same message was dispatched to two agents"; conversation counts and event counts could all look normal.
	agentsAddressed []string

	// disabledAgents is the set of "disabled" agents; AgentDisabled answers from it.
	// Default empty = all enabled, so existing tests are unaffected.
	disabledAgents map[string]bool
	// Agents in queueingAgents use "queue"; the rest use "redirect" (the default mode).
	queueingAgents map[string]bool
	// Agents in briefAgents do not show detailed work (chain of thought).
	// **Default empty = all show it**, matching the default side of apimodel.ProgressDetail.
	briefAgents map[string]bool
	// redirects records the arguments of every redirect.
	redirects []redirectCall
	// redirectErr makes redirect fail, to check "failure must fall back to the queue notice, not stay silent".
	redirectErr error
	// onInterrupt is called back after InterruptRun succeeds.
	//
	// **It simulates the most important step of the real path**: the interrupt makes the running turn receive a final frame,
	// wrap up, and then delete the inflight record in its defer, **handing over the serial lock**. Without it,
	// nobody ever releases the lock in tests, and the next step of redirect is exactly "wait for the lock", so the test waits for the whole
	// redirectLockWait and then falls back to the queue notice, with an error saying "the body carried by the redirect is wrong",
	// which does not point at the real cause at all.
	onInterrupt func()
}

// addressedAgents returns the target agent of every conversation created.
func (f *fakeConversations) addressedAgents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.agentsAddressed...)
}

// CreateConversation returns a **different** conversation ID every time.
//
// Returning the same ID every time once made these tests completely blind to "conversation reuse": each message created its own conversation,
// but since the IDs were the same, the assertions could not tell. With an increasing suffix,
// "created a new one when it should have reused" shows up right away as different IDs.
func (f *fakeConversations) CreateConversation(_ context.Context, _, agentID string) (feishu.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	f.agentsAddressed = append(f.agentsAddressed, agentID)
	if f.createErr != nil {
		return feishu.Run{}, f.createErr
	}
	f.order = append(f.order, "create")
	return feishu.Run{ID: fmt.Sprintf("sesn-%d", f.created)}, nil
}

// CreateFeishuConversation uses the same counter as CreateConversation and also records detail,
// so tests can assert "the Feishu path uses the method that carries the source label".
func (f *fakeConversations) CreateFeishuConversation(
	ctx context.Context, workspaceID, agentID, detail string,
) (feishu.Run, error) {
	f.mu.Lock()
	f.details = append(f.details, detail)
	f.mu.Unlock()
	return f.CreateConversation(ctx, workspaceID, agentID)
}

// createdDetails returns the source labels used for every Feishu conversation created.
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

	// Called outside the lock, so a hook that touches the store does not collide with other goroutines on this lock.
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

// AgentFollowUpQueues defaults to false (redirect, matching the default side of apimodel.FollowUpMode).
// Tests for the queue branch set queueingAgents[agentID] to true. **It must be set explicitly**,
// otherwise the redirect path is tested, and it never goes through replyBusy.
func (f *fakeConversations) AgentFollowUpQueues(_ context.Context, agentID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queueingAgents[agentID]
}

// AgentShowsProgress defaults to true (show), matching the default side of apimodel.ProgressDetail
// (that enum's empty string means "show"). Tests for the "do not show" branch set briefAgents.
func (f *fakeConversations) AgentShowsProgress(_ context.Context, agentID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.briefAgents[agentID]
}

// redirectCall records the arguments of one redirect. Asserting "which conversation was interrupted" needs it;
// looking only at "was there an error", an implementation that interrupts the wrong conversation would still be all green.
//
// text is kept but always empty on the new path: the interrupt **carries no body**, and the new message is sent by the caller on the normal path
// (AppendRunEvents) after getting the serial lock. See the comment on interruptForRedirect in feishu.
type redirectCall struct {
	runID string
	text  string
}

func (f *fakeConversations) InterruptRun(_ context.Context, runID string) error {
	f.mu.Lock()
	f.redirects = append(f.redirects, redirectCall{runID: runID})
	err, hook := f.redirectErr, f.onInterrupt
	f.mu.Unlock()
	if err != nil {
		return err
	}
	// On the real path the interrupt makes the previous turn wrap up and hand back the serial lock. Called outside the lock because the callback touches the Store.
	if hook != nil {
		hook()
	}
	return nil
}

func (f *fakeConversations) redirectCalls() []redirectCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]redirectCall(nil), f.redirects...)
}

func (f *fakeConversations) SyncRunEvents(_ context.Context, _ string) error {
	f.mu.Lock()
	f.syncCalls++
	f.order = append(f.order, "sync")
	err := f.syncErr
	hook := f.syncHook
	f.mu.Unlock()

	// Called outside the lock so the hook can freely change the fake's fields (simulating the final state changing after catch-up).
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

// createCount is how many times CreateConversation was called.
//
// It is the core assertion of conversation reuse: after N rounds in the same group, this number must still be 1.
func (f *fakeConversations) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created
}

// appendedEvents returns the event batches already sent into conversations.
func (f *fakeConversations) appendedEvents() [][]json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]json.RawMessage(nil), f.appended...)
}

type sentCard struct {
	kind string // reply / update
	text string
	// idem is the idempotency key used when creating a card.
	//
	// It is recorded because **a clash gives no error**: Feishu drops the second card as a duplicate delivery and returns the first card's
	// message_id, so the second round's reply overwrites the first round's card. Only asserting this key locks that down.
	idem string
}

type fakeSender struct {
	mu    sync.Mutex
	cards []sentCard
	// replyErr makes ReplyCard fail.
	replyErr error
	// onReply is called back at the moment of sending, to assert the state "at that moment" (for example whether the chain of thought was closed).
	//
	// More reliable than comparing timestamps: the order assertion wants the cause "the chain of thought was final when the answer was sent",
	// not which of two times came first.
	onReply func()
}

func (f *fakeSender) ReplyCard(
	_ context.Context, _, _ string, card map[string]any, opt lark.ReplyCardOption,
) (*lark.SentMessage, error) {
	f.mu.Lock()
	hook := f.onReply
	f.mu.Unlock()
	// Called outside the lock: hooks often read the store, and holding the lock would collide with other goroutines.
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

// cardText digs the body text out of the card structure, for easy assertions.
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

// agentFrame builds one agent.message event frame.
func agentFrame(text string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"type":    "agent.message",
		"content": []map[string]any{{"type": "text", "text": text}},
	})
	return raw
}

// inboundFixture wires up a working inbound handler with one connected app preset.
func inboundFixture(t *testing.T, conv *fakeConversations, sender *fakeSender) (*feishu.Inbound, feishu.Store) {
	t.Helper()
	return inboundFixtureWithLogger(t, conv, sender, nil)
}

// inboundFixtureWithLogger is the same as inboundFixture, but a logger can be injected to assert log content.
//
// The multi-binding warning test needs it: the **only** observable output of that behavior is a log line
// (the message is still answered), so without looking at logs there is no way to check whether the warning was emitted.
func inboundFixtureWithLogger(
	t *testing.T, conv *fakeConversations, sender *fakeSender, logger *slog.Logger,
) (*feishu.Inbound, feishu.Store) {
	t.Helper()
	store := feishu.NewMemStore()
	app := &feishu.App{
		ID:          "fsa_1",
		WorkspaceID: "ws_1",
		AppID:       "cli_1",
		Name:        "测试应用",
	}
	if _, err := store.CreateApp(context.Background(), app, "secret"); err != nil {
		t.Fatalf("预置应用失败: %v", err)
	}
	in, err := feishu.NewInbound(feishu.InboundConfig{
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

// -- Fixtures for the two "sending the next message right away" paths ---------------------------------------------

// followUpReactor records which reactions were added, to assert queue and redirect use **different** ones.
//
// Reactions are the only instant feedback of these two modes (in queue mode the bot does not answer right away, in redirect mode the previous turn suddenly stops),
// so "which reaction was added" is itself the behavior under test, not decoration.
type followUpReactor struct {
	mu     sync.Mutex
	emojis []string
}

func (f *followUpReactor) AddReaction(
	_ context.Context, _, _, emojiType string,
) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emojis = append(f.emojis, emojiType)
	return "reaction_" + emojiType, nil
}

func (f *followUpReactor) RemoveReaction(context.Context, string, string, string) error {
	return nil
}

func (f *followUpReactor) added() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.emojis...)
}

// hasAdded answers "was this reaction ever added", not whether it was the last one.
//
// After a redirect this turn **goes on to finish normally** and adds DONE when wrapping up, so "the redirect reaction is the last one"
// can never hold on the new path, while what it really wants to guard is "the user got feedback at the moment of redirect".
func (f *followUpReactor) hasAdded(emoji string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.emojis {
		if e == emoji {
			return true
		}
	}
	return false
}

// inboundFixtureWithReactor is the same as inboundFixture, but with a Reactor attached.
//
// The default fixture **has no Reactor**, so every mark* is a no-op. Testing reactions with it,
// the assertions are always empty, and the failure points at "reaction not added", not at all at "the fixture has no Reactor".
func inboundFixtureWithReactor(
	t *testing.T, conv *fakeConversations, sender *fakeSender, reactor feishu.Reactor,
) (*feishu.Inbound, feishu.Store) {
	t.Helper()
	store := feishu.NewMemStore()
	if _, err := store.CreateApp(context.Background(), &feishu.App{
		ID: "fsa_1", WorkspaceID: "ws_1", AppID: "cli_1", Name: "测试应用",
	}, "secret"); err != nil {
		t.Fatalf("预置应用失败: %v", err)
	}
	in, err := feishu.NewInbound(feishu.InboundConfig{
		Store: store, Conversations: conv, Sender: sender,
		Tokens: fakeTokens{}, Reactor: reactor,
	})
	if err != nil {
		t.Fatalf("建入站处理器失败: %v", err)
	}
	return in, store
}

// occupyTurn manually holds a conversation, simulating "the previous turn is still running".
//
// It is extracted because the queue and redirect tests have exactly the same precondition, and that setup has three steps
// (run one turn to get runID → look up scope → hold it with TryBeginTurn); copying it twice easily drifts.
func occupyTurn(t *testing.T, in *feishu.Inbound, store feishu.Store) string {
	t.Helper()
	ctx := context.Background()
	if err := in.Handle(ctx, scopedMsg("om_1", "oc_1", "group", "", "第一个问题")); err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	bound, err := store.GetSessionScope(ctx, "cli_1", "chat:oc_1", "agent_1")
	if err != nil {
		t.Fatalf("查绑定失败: %v", err)
	}
	ok, err := store.TryBeginTurn(ctx, feishu.InflightTurnRecord{
		RunID: bound.RunID, LarkAppID: "cli_1", ChatID: "oc_1",
		SrcMessageID: "om_stuck", Status: feishumodel.InflightOpen, StartedAt: time.Now(),
	}, time.Hour)
	if err != nil || !ok {
		t.Fatalf("预置占用失败: ok=%v err=%v", ok, err)
	}
	return bound.RunID
}
