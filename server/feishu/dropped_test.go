// Tests that "the message arrived but did not trigger" is **visible in production**.
//
// # What this group pins
//
// Not "should it trigger": that is trigger_test.go's job, which already enumerates it, correctly.
// What is pinned here is **the observability of the decision**: after a message is rejected, is there a trace in the production logs.
//
// The difference matters, because in the 2026-09-01 investigation the trigger decision **was right from start to finish**;
// what blocked us was "we could not find out whether the message came in at all". A correct decision ≠ a diagnosable failure.
//
// # ⚠️ Every test asserts the level is INFO; this is the core of the file
//
// Those three branches already had `log.Debug`, with the right reason. But the production log level is Info
// (newLogger in `cmd/amad/main.go`, whose comment says plainly "the source of truth for the level is here"),
// so those Debug lines **do not exist in production at all**.
//
// So "we rejected the message" and "Feishu never pushed it" looked exactly the same in the logs,
// while the two call for opposite actions (check the code vs check Feishu permissions).
//
// **Asserting only "there is a log" is not enough**: changing Info back to Debug would keep the tests green,
// and production would go back to today's undiagnosable state. So every test must check `level=INFO`.
//
// # Why there is also a reverse "no flooding" test
//
// Without it, deleting the rate limiting entirely would also make the tests above green, and that would log one Info line for every chat message in an active big chat:
// flooding is itself a failure, and it drowns the lines that really matter.
// See TestDroppedMessageLogIsRateLimited and TestDroppedMessageLogSeparatesReasons.

package feishu

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/shared/infra/lark"
)

// countLines counts how many "did not trigger" log lines appear in buf.
func countDropLines(out string) int {
	return strings.Count(out, "收到飞书消息但没有触发 agent")
}

// ⭐ When a chat message without an @ to the bot is rejected, it must be visible in production.
//
// This is the most common kind: bots posting notices in alert chats, or others chatting.
// It **should** be rejected, but when debugging "why does the bot ignore people", the logs must show
// "the message came in, and we rejected it by the rules", not a blank.
//
// Mutation check: change log.Info in report in dropped.go to log.Debug, and this turns red.
func TestUntriggeredGroupMessageIsLoggedAtInfo(t *testing.T) {
	log, buf := captureLogs()
	conv := &fakeConversations{agentIDs: []string{"agent_1"}}
	in, _ := inboundFixtureWithLogger(t, conv, &fakeSender{}, log)

	msg := inboundMsg("om_1", "P1 报警：xxx 服务错误率上升")
	msg.Mentioned = false // no @ in the chat
	msg.SenderType = "user"

	if err := in.Handle(context.Background(), msg); err != nil {
		t.Fatalf("不触发的消息应返回 nil（返回 error 会让飞书重投）: %v", err)
	}
	if conv.created != 0 {
		t.Fatalf("前提没满足：这条不该触发 agent，实际建了 %d 个会话", conv.created)
	}

	out := buf.String()
	// ⚠️ The level must be INFO. Production is at Info, so Debug does not exist; that is exactly what this change fixes.
	if !strings.Contains(out, "level=INFO") {
		t.Fatalf("「收到但没触发」必须打 Info，Debug 在线上查不到。实际输出：%s", out)
	}
	if countDropLines(out) != 1 {
		t.Fatalf("该有且只有一条「没有触发」的日志。实际输出：%s", out)
	}
	// The metadata needed for debugging must be there: which app, which message, which chat, and why.
	for _, want := range []string{
		"cli_1",              // which Feishu app
		"om_1",               // which message: to confirm "it is the one I just sent"
		"oc_1",               // which chat
		dropReasonIneligible, // why it did not trigger
		"senderType=user",    // sent by a person or a bot
	} {
		if !strings.Contains(out, want) {
			t.Errorf("日志里缺 %q，排查时要多猜一步。实际输出：%s", want, out)
		}
	}
}

// ⭐ A real person's message in a topic, while the app has not turned on no-@, must also be visible.
//
// This matches the scenario users find most confusing: "I clearly turned on topic auto reply".
// If the actual config is mention_only (e.g. changed but not saved, or another app was changed),
// the log must say "this app's mode is the default", otherwise one can only check the store.
func TestTopicMessageWithModeOffIsLoggedAtInfo(t *testing.T) {
	log, buf := captureLogs()
	conv := &fakeConversations{agentIDs: []string{"agent_1"}}
	in, store := inboundFixtureWithLogger(t, conv, &fakeSender{}, log)
	setTriggerMode(t, store, TriggerMentionOnly) // default mode

	msg := inboundMsg("om_1", "这个报警怎么处理")
	msg.Mentioned = false
	msg.SenderType = "user"
	msg.ThreadID = "omt_0000ed75c64fea21" // in a topic (the test is thread_id, see trigger.go)

	if err := in.Handle(context.Background(), msg); err != nil {
		t.Fatalf("不触发应返回 nil: %v", err)
	}
	if conv.created != 0 {
		t.Fatalf("前提没满足：mention_only 下这条不该触发，实际建了 %d 个会话", conv.created)
	}

	out := buf.String()
	if !strings.Contains(out, "level=INFO") {
		t.Fatalf("必须打 Info。实际输出：%s", out)
	}
	if !strings.Contains(out, dropReasonModeOff) {
		t.Errorf("要说清是「模式没开」而不是别的原因，否则会去查飞书权限。实际输出：%s", out)
	}
	// thread_id must be included: it is the only test for "does this count as in a topic",
	// and that is exactly the field misjudged on 2026-08-28 (it was judged by chat_type then, with zero hits).
	if !strings.Contains(out, "omt_0000ed75c64fea21") {
		t.Errorf("要带 thread_id，它是判「在不在话题里」的真源。实际输出：%s", out)
	}
}

// ⭐ Pure images/pure files (empty body) must also be visible.
//
// This is checked separately because its reason differs from the two above, and **so does the handling**:
// the first two are "just the rules", while this one often means the message shape produced no text (e.g. a new message type),
// which is something we need to follow up on. Reporting them together would send people in the wrong direction.
func TestEmptyTextMessageIsLoggedAtInfo(t *testing.T) {
	log, buf := captureLogs()
	conv := &fakeConversations{agentIDs: []string{"agent_1"}}
	in, _ := inboundFixtureWithLogger(t, conv, &fakeSender{}, log)

	msg := inboundMsg("om_1", "   ") // @-mentioned, but said nothing
	msg.Mentioned = true

	if err := in.Handle(context.Background(), msg); err != nil {
		t.Fatalf("不触发应返回 nil: %v", err)
	}
	if conv.created != 0 {
		t.Fatalf("前提没满足：空正文不该触发，实际建了 %d 个会话", conv.created)
	}

	out := buf.String()
	if !strings.Contains(out, "level=INFO") {
		t.Fatalf("必须打 Info。实际输出：%s", out)
	}
	if !strings.Contains(out, dropReasonEmptyText) {
		t.Errorf("要说清是正文为空。实际输出：%s", out)
	}
}

// ⭐ **Messages that trigger normally do not write this log.**
//
// Reverse control. Without it, changing report to "always log" would also make the three above green,
// and every normal turn would get an extra "did not trigger" line, making the logs read the opposite.
func TestTriggeredMessageDoesNotLogDrop(t *testing.T) {
	log, buf := captureLogs()
	conv := &fakeConversations{
		agentIDs: []string{"agent_1"},
		frames:   [][]byte{agentFrame("好的")},
	}
	in, _ := inboundFixtureWithLogger(t, conv, &fakeSender{}, log)

	if err := in.Handle(context.Background(), inboundMsg("om_1", "帮我查一下")); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if conv.created != 1 {
		t.Fatalf("前提没满足：这条该触发，实际建了 %d 个会话", conv.created)
	}
	if countDropLines(buf.String()) != 0 {
		t.Errorf("正常触发的消息不该打「没有触发」，否则日志读起来是反的。实际输出：%s", buf.String())
	}
}

// ⭐ **Rate limiting: same app, same reason, only one line per window.**
//
// Without this, deleting the rate limiting would keep the tests above green, and every chat message in an active big chat would log an Info line.
// Flooding is itself a failure: it drowns the lines that really matter, costing no less than not being able to find anything.
//
// Use an injected fake clock instead of really waiting a minute.
func TestDroppedMessageLogIsRateLimited(t *testing.T) {
	log, buf := captureLogs()
	conv := &fakeConversations{agentIDs: []string{"agent_1"}}
	in, _ := inboundFixtureWithLogger(t, conv, &fakeSender{}, log)

	// Swap the rate limiter for one driven by a fake clock, with time advanced by the test.
	base := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
	clock := base
	in.drops = newDropReporter(func() time.Time { return clock })

	send := func(id string) {
		msg := inboundMsg(id, "群里的闲聊")
		msg.Mentioned = false
		msg.SenderType = "user"
		if err := in.Handle(context.Background(), msg); err != nil {
			t.Fatalf("不触发应返回 nil: %v", err)
		}
	}

	// 5 in a row within one window: only the first should leave a log.
	for _, id := range []string{"om_1", "om_2", "om_3", "om_4", "om_5"} {
		send(id)
	}
	if got := countDropLines(buf.String()); got != 1 {
		t.Fatalf("一个窗口内同因只该打 1 条，实际 %d 条——活跃群会刷屏。输出：%s",
			got, buf.String())
	}

	// Send one more after crossing the window: this one must be logged, and account for the 4 held back.
	buf.Reset()
	clock = base.Add(dropWindow + time.Second)
	send("om_6")

	out := buf.String()
	if got := countDropLines(out); got != 1 {
		t.Fatalf("跨过窗口后该重新打一条，实际 %d 条。输出：%s", got, out)
	}
	// suppressed must be reported truthfully, otherwise seeing "one" makes you think only one came,
	// which is another kind of misleading, in the opposite direction.
	if !strings.Contains(out, "suppressed=4") {
		t.Errorf("要报告窗口内被挡掉的条数（应为 4），否则会低估流量。实际输出：%s", out)
	}
}

// ⭐ **Different reasons are rate limited separately and must not hide each other.**
//
// In alert chats having both reasons at the same time is normal: notices sent by bots (not qualified) and pure images sent by people
// (empty body). Rate limiting only by app would let whichever comes first cover the other for a whole minute,
// with no hint that "what was covered happens to be the one you are looking for".
func TestDroppedMessageLogSeparatesReasons(t *testing.T) {
	log, buf := captureLogs()
	conv := &fakeConversations{agentIDs: []string{"agent_1"}}
	in, _ := inboundFixtureWithLogger(t, conv, &fakeSender{}, log)

	clock := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
	in.drops = newDropReporter(func() time.Time { return clock })

	// 1. No @ in the chat and not in a topic → dropReasonIneligible.
	//
	// ⚠️ Since 2026-09-01 this **can no longer be built with "sent by a bot"**: third-party bots are now treated the same as real people,
	// so such a message would really trigger the agent, and this assertion would miss.
	// Now it relies on "not in a topic" (inboundMsg has no thread_id).
	plain := inboundMsg("om_1", "群里的闲聊")
	plain.Mentioned = false
	plain.SenderType = "user"
	if err := in.Handle(context.Background(), plain); err != nil {
		t.Fatalf("处理失败: %v", err)
	}

	// 2. Same moment, same app, but a different reason: @-mentioned but no body.
	empty := inboundMsg("om_2", "  ")
	empty.Mentioned = true
	if err := in.Handle(context.Background(), empty); err != nil {
		t.Fatalf("处理失败: %v", err)
	}

	out := buf.String()
	if got := countDropLines(out); got != 2 {
		t.Fatalf("两种原因该各打一条（共 2 条），实际 %d 条——一种原因被另一种遮蔽了。输出：%s",
			got, out)
	}
	for _, want := range []string{dropReasonIneligible, dropReasonEmptyText} {
		if !strings.Contains(out, want) {
			t.Errorf("缺原因 %q，它被同窗口的另一种原因盖掉了。实际输出：%s", want, out)
		}
	}
}

// ⭐ **Different apps do not affect each other.**
//
// In production several Feishu apps share one process. The other half of grouping by app: one noisy app
// should not push out the only clue of another quiet app.
func TestDroppedMessageLogSeparatesApps(t *testing.T) {
	log, buf := captureLogs()
	conv := &fakeConversations{agentIDs: []string{"agent_1"}}
	in, _ := inboundFixtureWithLogger(t, conv, &fakeSender{}, log)

	clock := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
	in.drops = newDropReporter(func() time.Time { return clock })

	for _, appID := range []string{"cli_1", "cli_2"} {
		msg := inboundMsg("om_"+appID, "闲聊")
		msg.AppID = appID
		msg.Mentioned = false
		msg.SenderType = "user"
		if err := in.Handle(context.Background(), msg); err != nil {
			t.Fatalf("处理失败: %v", err)
		}
	}

	if got := countDropLines(buf.String()); got != 2 {
		t.Fatalf("两个应用该各打一条，实际 %d 条——一个应用把另一个盖住了。输出：%s",
			got, buf.String())
	}
}

// ⭐ **When blocking our own, the reason must say "self-trigger prevention", not "mode not enabled".**
//
// These two kinds of rejection call for opposite actions: the former is the defense working normally, the latter needs the user to change config.
// Merging them would make whoever is debugging change a switch that is perfectly fine, a switch that is clearly on,
// so they would think "the change did not take effect", and go check the long connection and permissions all over.
func TestSelfMessageDropHasItsOwnReason(t *testing.T) {
	log, buf := captureLogs()
	conv := &fakeConversations{agentIDs: []string{"agent_1"}}
	in, store := inboundFixtureWithLogger(t, conv, &fakeSender{}, log)
	setTriggerMode(t, store, TriggerTopicAndMention) // the mode is **on**

	msg := topicMsg("om_self", "这是我刚才的回答")
	msg.SenderType = senderTypeApp
	msg.SenderOpenID = selfBotOpenID

	if err := in.Handle(context.Background(), msg); err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if n := conv.createCount(); n != 0 {
		t.Fatalf("前提没满足：自己发的不该触发，实际建了 %d 个会话", n)
	}

	out := buf.String()
	if !strings.Contains(out, dropReasonSelfMessage) {
		t.Errorf("要报「防自触发」这个原因。实际输出：%s", out)
	}
	if strings.Contains(out, dropReasonModeOff) {
		t.Errorf("模式明明是开着的，不能报成「模式没开」——那会让人去改一个没问题的开关。实际输出：%s", out)
	}
}

// ⭐ The message body must not appear in the log.
//
// A security assertion, for the same reason as the one in progress_logging_test.go: the body is user data,
// kept long-term and searchable by the whole team. Diagnosis only needs metadata.
func TestDroppedMessageLogOmitsUserContent(t *testing.T) {
	log, buf := captureLogs()
	conv := &fakeConversations{agentIDs: []string{"agent_1"}}
	in, _ := inboundFixtureWithLogger(t, conv, &fakeSender{}, log)

	const secretText = "内部项目代号 Alpha 的库存数字是 12345"
	msg := inboundMsg("om_1", secretText)
	msg.Mentioned = false
	msg.SenderType = "user"

	if err := in.Handle(context.Background(), msg); err != nil {
		t.Fatalf("处理失败: %v", err)
	}

	out := buf.String()
	if countDropLines(out) != 1 {
		t.Fatalf("前提没满足：应当打了一条「没有触发」。实际输出：%s", out)
	}
	for _, leaked := range []string{secretText, "内部项目代号", "12345"} {
		if strings.Contains(out, leaked) {
			t.Errorf("用户正文 %q 泄漏进日志了。实际输出：%s", leaked, out)
		}
	}
}

// ⭐ A nil message does not blow up.
//
// A defensive check of dropReporter. Handle blocks nil at the start, but report can be called from elsewhere,
// and it is on the logging path, where a panic would take down the whole turn with it.
func TestDropReporterHandlesNil(t *testing.T) {
	log, buf := captureLogs()
	var nilReporter *dropReporter
	nilReporter.report(log, &lark.InboundMessage{AppID: "cli_1"}, dropReasonEmptyText)
	newDropReporter(nil).report(log, nil, dropReasonEmptyText)

	if countDropLines(buf.String()) != 0 {
		t.Errorf("nil 情况不该打日志，也不该 panic。实际输出：%s", buf.String())
	}
}

// ⭐ **A zero-value dropReporter must not panic.**
//
// A real bug found during self-review: the type comment used to say "the zero value is usable", but the first line of `admit` was
// `r.now()`, and with the zero value `now` is nil, **an immediate SIGSEGV**. The docs said the opposite of the implementation, and it was wrong on
// the more dangerous side: using it per the comment (e.g. someone later writing `Inbound{...}` without NewInbound)
// would blow up the whole turn on the logging path.
//
// Now now / window / state all have fallbacks in admit. This test pins that,
// so the fallbacks are not "simplified" away later and it regresses.
//
// Mutation check: replace the three `if r.now != nil` lines in admit with `now := r.now()`, and this test panics.
func TestZeroValueDropReporterDoesNotPanic(t *testing.T) {
	log, buf := captureLogs()
	var zero dropReporter // deliberately not using newDropReporter

	msg := inboundMsg("om_1", "闲聊")
	msg.Mentioned = false
	msg.SenderType = "user"

	// Not panicking is a pass; also confirm it still works normally (logs lines, rate limits).
	zero.report(log, msg, dropReasonIneligible)
	zero.report(log, msg, dropReasonIneligible)

	if got := countDropLines(buf.String()); got != 1 {
		t.Fatalf("零值也该正常限流（第一条打、第二条挡），实际 %d 条。输出：%s",
			got, buf.String())
	}
	// window falls back to the default value instead of printing "0s", which would make log readers think rate limiting is not in effect.
	if !strings.Contains(buf.String(), "window=1m0s") {
		t.Errorf("window 该兜底成 %s，否则日志里是 0s。实际输出：%s", dropWindow, buf.String())
	}
}
