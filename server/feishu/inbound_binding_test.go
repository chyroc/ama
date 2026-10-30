// Tests for "one inbound message goes to exactly one agent".
//
// The source of truth for this constraint is on the write side (core.Service.ensureFeishuAppsFree rejects duplicate bindings),
// but this package must survive the constraint not holding: the constraint was added later, the store may have older 1:N
// data, and with multiple instances the write-side check has a very narrow TOCTOU window
// (PostgreSQL cannot express this unique constraint; see repository/schema/core_pg.sql for why).
//
// So what is pinned here is **the tolerance strategy itself**, all of the "wrong but no error, just wrong behavior" kind:
// how many agents it was dispatched to, which one, and whether multiple bindings raise an alert.

package feishu

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestInboundAddressesExactlyOneAgent: normal 1:1 binding; the message goes only to that agent.
func TestInboundAddressesExactlyOneAgent(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_owner"},
		frames:   [][]byte{agentFrame("好的")},
	}
	in, _ := inboundFixture(t, conv, &fakeSender{})

	if err := in.Handle(context.Background(), inboundMsg("om_1", "帮我查一下")); err != nil {
		t.Fatalf("处理失败: %v", err)
	}

	addressed := conv.addressedAgents()
	if len(addressed) != 1 {
		t.Fatalf("一条消息该只建一个会话，实际派给了 %d 个 agent：%v",
			len(addressed), addressed)
	}
	if addressed[0] != "agent_owner" {
		t.Errorf("该交给 agent_owner，实际给了 %q", addressed[0])
	}
}

// TestInboundPicksFirstAgentAndWarnsOnMultiBind: existing multi-binding data; still answer, but alert.
//
// # Why "answer as before + alert" instead of "refuse to answer"
//
// Refusing would turn a **config problem** into a **production outage**: users only see the bot ignoring them,
// while this 1:N data may have been sitting there a long time. An alert is visible; silence is not.
//
// The assertions include "the log level is ERROR" and "every candidate agent is named": if it only said "there is a conflict",
// the admin would still have to dig up which agents, and the log would be useless.
func TestInboundPicksFirstAgentAndWarnsOnMultiBind(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	conv := &fakeConversations{
		// A shape that is illegal for the business, but the store may well look like this.
		agentIDs: []string{"agent_first", "agent_second"},
		frames:   [][]byte{agentFrame("好的")},
	}
	in, _ := inboundFixtureWithLogger(t, conv, &fakeSender{}, logger)

	if err := in.Handle(context.Background(), inboundMsg("om_1", "帮我查一下")); err != nil {
		t.Fatalf("处理失败: %v", err)
	}

	// It still answered, and **only** to one agent: it must not run twice because two are bound.
	addressed := conv.addressedAgents()
	if len(addressed) != 1 {
		t.Fatalf("多绑时也只该建一个会话，实际派给了 %d 个：%v", len(addressed), addressed)
	}
	// Take the first one (by ord on the core side), same as before the change: existing users do not suddenly stop getting replies.
	if addressed[0] != "agent_first" {
		t.Errorf("该取第一个 agent_first，实际 %q", addressed[0])
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "level=ERROR") {
		t.Errorf("多绑该打 ERROR 级别的告警，实际日志：\n%s", logs)
	}
	for _, want := range []string{"agent_first", "agent_second", "cli_1"} {
		if !strings.Contains(logs, want) {
			t.Errorf("告警里该点出 %q，实际日志：\n%s", want, logs)
		}
	}
}

// TestInboundRepliesGuidanceWhenNoAgentBound: with no agent bound, guide the user to configure one; do not stay silent.
//
// If silent the user would just think the bot is broken, while it is really "not configured yet".
func TestInboundRepliesGuidanceWhenNoAgentBound(t *testing.T) {
	conv := &fakeConversations{agentIDs: nil}
	sender := &fakeSender{}
	in, _ := inboundFixture(t, conv, sender)

	if err := in.Handle(context.Background(), inboundMsg("om_1", "帮我查一下")); err != nil {
		t.Fatalf("没绑 agent 不该返回错误（会让飞书重投）: %v", err)
	}

	if len(conv.addressedAgents()) != 0 {
		t.Errorf("没绑 agent 时不该建会话，实际：%v", conv.addressedAgents())
	}
	// Must reply to the user with a sentence instead of doing nothing.
	if len(sender.sent()) == 0 {
		t.Fatal("没绑 agent 时该回一句引导语，实际什么都没发")
	}
}

// TestInboundRepliesGuidanceWhenAgentDisabled: when the bound agent is disabled, reply with a human sentence + log it.
//
// # This guards against the "message silently gets no reply" kind of failure
//
// The core side does block disabled agents (AppendRunEvents returns 422), but that is an error:
// when this package sees an error it only logs a line and returns, so **people in the chat get nothing**, which looks like
// "the bot is ignoring me". Users cannot find out why, and ops can only guess by digging through logs.
//
// So disabling must be caught before running, and **both things must be done**:
//
//   - reply with a card to people in the chat, telling them where to change it (otherwise users think the bot is broken)
//   - log it for ops (otherwise when "users report the bot does not reply" nobody can find that someone disabled it)
//
// Missing either one turns this test red.
func TestInboundRepliesGuidanceWhenAgentDisabled(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	conv := &fakeConversations{
		agentIDs:       []string{"agent_off"},
		disabledAgents: map[string]bool{"agent_off": true},
		// Give it frames: if the gate does not work, it will really run and answer, and only then do the assertions below catch it.
		frames: [][]byte{agentFrame("我不该回答")},
	}
	sender := &fakeSender{}
	in, _ := inboundFixtureWithLogger(t, conv, sender, logger)

	if err := in.Handle(context.Background(), inboundMsg("om_disabled", "在吗")); err != nil {
		// Must not propagate upwards: then Feishu would redeliver, the redelivery would still be disabled, and the user would get a string of cards.
		t.Fatalf("停用不是错误，该安静处理完: %v", err)
	}

	// 1. It should not create a session or send any event to Ark: the gate must come before running.
	if conv.created != 0 {
		t.Errorf("停用的 agent 不该建会话，实际建了 %d 个", conv.created)
	}
	if n := len(conv.addressedAgents()); n != 0 {
		t.Errorf("停用的 agent 不该被派活，实际派了 %d 次", n)
	}

	// 2. It must reply with a sentence that says where to change it. **This is the core assertion of "not silent".**
	cards := sender.sent()
	if len(cards) != 1 {
		t.Fatalf("停用时该回一句提示，实际发了 %d 张卡", len(cards))
	}
	if !strings.Contains(cards[0].text, "停用") {
		t.Errorf("提示要说清是停用了，得到 %q", cards[0].text)
	}
	if !strings.Contains(cards[0].text, "启用") {
		t.Errorf("提示要告诉用户怎么恢复（改回已启用），得到 %q", cards[0].text)
	}

	// 3. There must be a log that tells which agent; otherwise ops cannot find the cause.
	logs := logBuf.String()
	if !strings.Contains(logs, "停用") {
		t.Errorf("要打一条说明停用的日志，实际日志：\n%s", logs)
	}
	if !strings.Contains(logs, "agent_off") {
		t.Errorf("日志要点出是哪个 agent，实际日志：\n%s", logs)
	}
}

// TestInboundAnswersWhenAgentEnabled: an enabled agent answers as usual.
//
// Paired with the previous one: with only "disabled is blocked", an implementation that blocks every message would also pass.
// This pins that the gate does not hit the wrong target: preventing leaks and preventing false kills need two paired tests.
func TestInboundAnswersWhenAgentEnabled(t *testing.T) {
	conv := &fakeConversations{
		agentIDs: []string{"agent_on"},
		// Explicitly mark it as "not disabled", so the test is really exercised.
		disabledAgents: map[string]bool{"agent_on": false},
		frames:         [][]byte{agentFrame("在的")},
	}
	sender := &fakeSender{}
	in, _ := inboundFixture(t, conv, sender)

	if err := in.Handle(context.Background(), inboundMsg("om_enabled", "在吗")); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if conv.created != 1 {
		t.Errorf("启用的 agent 该建一条会话，实际建了 %d 个", conv.created)
	}
	addressed := conv.addressedAgents()
	if len(addressed) != 1 || addressed[0] != "agent_on" {
		t.Errorf("该派给 agent_on，实际 %v", addressed)
	}
}
