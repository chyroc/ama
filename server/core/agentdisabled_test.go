// Tests for the "disable agent" gate.
//
// # Why a separate file
//
// Disabling has to block **five entry points** (chat page, new Feishu conversation, continued
// Feishu conversation, scheduled task, self-wake), and they converge on two methods
// (createConversation and AppendRunEvents). Spreading these tests across chat_test.go would mix
// them with the "delete agent" group. The two look alike but mean different things: delete cannot
// be undone, disable can, so the latter has extra assertions like "re-enabling must take effect
// right away".
//
// # The most important one here is not "disabling blocks"
//
// It is TestEmptyStatusIsNotDisabled: **an empty string must mean enabled**. Getting this backwards
// would make all existing agents in production (whose doc has no status key at all) stop replying
// at once, with an error saying "disabled", while users never disabled anything, so the cause
// cannot be found from the symptoms. That test is the main reason this group exists.

package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/repository/dao/coremem"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
)

// disableAgent sets an agent to disabled.
//
// It goes straight through store instead of UpdateAgent: that path bumps version and refreshes
// updatedAt, which has nothing to do with what this group asserts, and would only make failure
// messages harder to read.
func disableAgent(t *testing.T, store *coremem.MemStore, id string) {
	t.Helper()
	a, err := store.GetAgent(context.Background(), id)
	if err != nil {
		t.Fatalf("查 agent 失败：%v", err)
	}
	a.Status = apimodel.StatusDisabled
	if _, err := store.SaveAgent(context.Background(), a); err != nil {
		t.Fatalf("停用 agent 失败：%v", err)
	}
}

// setAgentStatus sets an agent's status to any value (including empty and unknown values).
func setAgentStatus(t *testing.T, store *coremem.MemStore, id string, status apimodel.AgentStatus) {
	t.Helper()
	a, err := store.GetAgent(context.Background(), id)
	if err != nil {
		t.Fatalf("查 agent 失败：%v", err)
	}
	a.Status = status
	if _, err := store.SaveAgent(context.Background(), a); err != nil {
		t.Fatalf("改状态失败：%v", err)
	}
}

// TestCreateConversationRejectsDisabledAgent: a disabled agent cannot start a new conversation.
//
// **It asserts not only "an error was returned", but also that there are no side effects on the
// Ark side**: without this gate, creating a conversation would really call Ark CreateSession and
// really store a Run, and only the first message would be blocked. The user gets an empty
// conversation that can never send anything, while the session and container on the Ark side have
// already been created and waste resources.
func TestCreateConversationRejectsDisabledAgent(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)
	disableAgent(t, store, "agent_triage")

	_, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err == nil {
		t.Fatal("停用的 agent 不该能开新会话")
	}
	// Same 422 as "deleted": everything except the run is there; it is "not allowed right now", not "not found".
	if !errors.Is(err, ErrPrecondition) {
		t.Errorf("应当是 ErrPrecondition，实际 %v", err)
	}
	// 404 would be turned into null by NOT_FOUND_CODES in the frontend's http.ts, and the page becomes a silent empty state.
	if errors.Is(err, ErrNotFound) {
		t.Error("不能是 ErrNotFound：前端会把它转成 null，用户什么提示都看不到")
	}
	// The error must point at the control the user can change, otherwise they do not know where to fix it.
	if msg := userMessage(err); !strings.Contains(msg, "停用") {
		t.Errorf("报错要说清是停用了，实际 %q", msg)
	}

	// The gate is before the Ark call: not a single session should be created.
	if runner.createdSessions != 0 {
		t.Errorf("被拒的会话不该打方舟，实际建了 %d 个 session", runner.createdSessions)
	}
}

// TestCreateConversationRejectsDisabledAgentAddsNoRun: a rejected conversation is not stored.
//
// Separate from the previous one because "was a Run added" is checked by comparing counts before
// and after; mixing them would make it unclear from the failure message which part broke.
func TestCreateConversationRejectsDisabledAgentAddsNoRun(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	before, err := svc.ListRuns(ctx, seedWS, persistmodel.RunQuery{})
	if err != nil {
		t.Fatalf("列运行失败：%v", err)
	}
	disableAgent(t, store, "agent_triage")

	if _, err := svc.CreateConversation(ctx, seedWS, "agent_triage"); err == nil {
		t.Fatal("停用的 agent 不该能开新会话")
	}

	after, err := svc.ListRuns(ctx, seedWS, persistmodel.RunQuery{})
	if err != nil {
		t.Fatalf("列运行失败：%v", err)
	}
	if len(after) != len(before) {
		t.Errorf("被拒的会话不该落 Run：之前 %d 条，之后 %d 条", len(before), len(after))
	}
}

// TestAppendRunEventsRejectsDisabledAgent: after disabling, **existing conversations** cannot send
// messages either.
//
// # This guards the continued Feishu conversation path
//
// Once a Feishu group is bound to a scope, continued conversations reuse the runID directly (the
// hit branch of ensureScopedRun in feishu/inbound.go) and **never call CreateConversation**. So
// blocking only at conversation creation is not enough: that only stops the first message in each
// group, and from the second one on it answers as usual. This test is the regression test for
// exactly that hole.
func TestAppendRunEventsRejectsDisabledAgent(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	// Create the conversation while enabled, then disable: exactly the real order of "disabled mid-conversation".
	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}
	sentBefore := len(runner.sentEvents)
	disableAgent(t, store, "agent_triage")

	body := []json.RawMessage{
		json.RawMessage(`{"type":"user.message","content":[{"type":"text","text":"停用之后还能发吗"}]}`),
	}
	_, err = svc.AppendRunEvents(ctx, run.ID, body)
	if err == nil {
		t.Fatal("agent 已停用时发消息应当失败")
	}
	if !errors.Is(err, ErrPrecondition) {
		t.Errorf("应当是 ErrPrecondition，实际 %v", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("不能是 ErrNotFound：前端会把它转成 null，页面变静默空状态")
	}
	// The message text is a contract: LocalApiClient.sendMessage copies it word for word.
	msg := userMessage(err)
	if !strings.Contains(msg, "只能查看历史") {
		t.Errorf("要说清历史还能看，实际 %q", msg)
	}
	if !strings.Contains(msg, "停用") {
		t.Errorf("要说清原因是停用（与「已删除」区分开），实际 %q", msg)
	}

	// The gate is before SendEvents: not a single word should reach Ark.
	if len(runner.sentEvents) != sentBefore {
		t.Errorf("被拒的消息不该转给方舟，实际多转了 %d 条", len(runner.sentEvents)-sentBefore)
	}
	// There should be no side effects either (title backfill, status set to running).
	after, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("查 run 失败：%v", err)
	}
	if after.Title != "" {
		t.Errorf("被拒的消息不该回填标题，实际 %q", after.Title)
	}
	if after.Status == apimodel.RunRunning {
		t.Error("被拒的消息不该把状态置为 running")
	}
}

// TestAppendRunEventsAllowsInterruptAfterAgentDisabled: disabling only blocks writes; **interrupts
// must be let through**.
//
// # This is the runnable documentation of "why let it through"
//
// The session on the Ark side may be running. If everything were blocked, after disabling an agent
// the user could **never stop that turn**: Ark keeps burning tokens, and the container is only
// reclaimed after idle pause or even TTL. And while streaming, the frontend swaps "send" for
// "interrupt", and that button is not disabled; blocking it would put a button there that always
// errors.
//
// Disabling hits this trap more easily than deleting: deleting an agent is an explicit destructive
// action, while disabling looks light ("let me just pause it"), and then the running turn gets out
// of control.
func TestAppendRunEventsAllowsInterruptAfterAgentDisabled(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}
	sentBefore := len(runner.sentEvents)
	disableAgent(t, store, "agent_triage")

	body := []json.RawMessage{json.RawMessage(`{"type":"user.interrupt"}`)}
	if _, err := svc.AppendRunEvents(ctx, run.ID, body); err != nil {
		t.Fatalf("agent 已停用时中断应当放行，实际 %v", err)
	}
	// It must really be forwarded to Ark, not just "no error"; otherwise this test is blind to "the interrupt was silently dropped".
	if len(runner.sentEvents) != sentBefore+1 {
		t.Fatalf("中断要转给方舟，实际多转了 %d 条", len(runner.sentEvents)-sentBefore)
	}
}

// TestEmptyStatusIsNotDisabled: an empty status **must** be treated as enabled.
//
// # This is the most important test in this group
//
// If the check were written as `!= "active"` instead of `== "disabled"`, this would fail, and the
// production consequence would be: all existing agents (whose doc has no status key at all, which
// deserializes to an empty string) stop replying overnight, with an error saying "disabled", while
// users never disabled anything. Many unit tests also call
// `CreateAgent(apimodel.Agent{ID:...})` without setting Status, which is also an empty string.
//
// **Blocking wrongly costs far more than missing a block**: missing a block just keeps things as
// they are; blocking wrongly is a site-wide outage with an error that does not point at the real
// cause.
func TestEmptyStatusIsNotDisabled(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	setAgentStatus(t, store, "agent_triage", "")

	// Creating a conversation must work.
	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("空串状态必须按启用处理，建会话却失败了：%v", err)
	}
	// Sending a message must work too.
	body := []json.RawMessage{
		json.RawMessage(`{"type":"user.message","content":[{"type":"text","text":"你好"}]}`),
	}
	if _, err := svc.AppendRunEvents(ctx, run.ID, body); err != nil {
		t.Fatalf("空串状态必须按启用处理，发消息却失败了：%v", err)
	}
	// The derived field must not be marked disabled either, otherwise the frontend freezes the conversation.
	got, err := svc.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("查 run 失败：%v", err)
	}
	if got.AgentDisabled {
		t.Error("空串状态不该被标记为停用——存量 agent 全靠这条")
	}
}

// TestUnknownStatusIsNotDisabled: unknown status values are let through as well.
//
// Existing "draft" values (the enum value that was removed) land here: let through, behaving
// exactly as before the enum was removed (back then draft could run too). **So removing draft
// needs no data migration to be correct.**
//
// Same rule as "unknown fields in config files must not be fatal errors": when values are added in
// the future, old binaries should not treat them as disabled.
func TestUnknownStatusIsNotDisabled(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	// Use the removed old value literally: this is the shape that really exists in the database after the upgrade.
	setAgentStatus(t, store, "agent_triage", "draft")

	if _, err := svc.CreateConversation(ctx, seedWS, "agent_triage"); err != nil {
		t.Fatalf("存量 draft 必须照常能用（与删枚举前一致），实际 %v", err)
	}
}

// TestRunsMarkAgentDisabled: both GetRun and ListRuns must mark "the linked agent is disabled".
//
// The frozen state in the frontend ChatPage only trusts the derived field computed by the server
// (it does not infer it from the local agents list), so if this field is computed wrong, the UI
// shows no sign that the conversation is frozen, and the user only gets pushed back by a 422 after
// typing a message. **Checked one by one** rather than "disabling one turns everything red": in the
// seed, run_2/run_5 point at agent_triage, run_1/run_4 point at agent_analyst.
func TestRunsMarkAgentDisabled(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	before, err := svc.GetRun(ctx, "run_2")
	if err != nil {
		t.Fatalf("查 run 失败：%v", err)
	}
	if before.AgentDisabled {
		t.Error("agent 启用时不该标记停用")
	}

	disableAgent(t, store, "agent_triage")

	got, err := svc.GetRun(ctx, "run_2")
	if err != nil {
		t.Fatalf("查 run 失败：%v", err)
	}
	if !got.AgentDisabled {
		t.Error("agent 停用后 GetRun 要标记 AgentDisabled")
	}
	if got.AgentDeleted {
		t.Error("停用不是删除，AgentDeleted 不该被置上——两者要给用户的话不同")
	}

	// The list path must mark it too (it goes through a different enrich code path, which is easy to miss).
	runs, err := svc.ListRuns(ctx, seedWS, persistmodel.RunQuery{})
	if err != nil {
		t.Fatalf("列运行失败：%v", err)
	}
	var checked bool
	for _, r := range runs {
		switch r.AgentID {
		case "agent_triage":
			if !r.AgentDisabled {
				t.Errorf("run %s 关联停用的 agent，列表里要标出来", r.ID)
			}
			checked = true
		case "agent_analyst":
			if r.AgentDisabled {
				t.Errorf("run %s 关联的是别的 agent，不该被标停用", r.ID)
			}
		}
	}
	if !checked {
		t.Fatal("种子里应当有关联 agent_triage 的 run，用例前提不成立")
	}
}

// TestReenablingAgentUnfreezesConversation: after re-enabling, the conversation must work right
// away.
//
// **Disabling can be undone**; that is its biggest difference from "delete", and the easiest place
// to get wrong: any implementation that caches AgentDisabled or stores it in the database makes
// this test fail (the user re-enabled it, and the UI is still frozen).
func TestReenablingAgentUnfreezesConversation(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}
	disableAgent(t, store, "agent_triage")

	body := []json.RawMessage{
		json.RawMessage(`{"type":"user.message","content":[{"type":"text","text":"先停用"}]}`),
	}
	if _, err := svc.AppendRunEvents(ctx, run.ID, body); err == nil {
		t.Fatal("停用期间应当发不出消息，用例前提不成立")
	}

	// Re-enable.
	setAgentStatus(t, store, "agent_triage", apimodel.StatusActive)

	if _, err := svc.AppendRunEvents(ctx, run.ID, body); err != nil {
		t.Fatalf("启用回来之后应当能发消息，实际 %v", err)
	}
	got, err := svc.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("查 run 失败：%v", err)
	}
	if got.AgentDisabled {
		t.Error("启用回来之后派生字段要跟着变——否则界面一直冻着")
	}
}

// TestCreateAgentDefaultsToActive: a newly created agent is enabled by default.
//
// It used to default to draft (a state that blocked nothing). After that value was removed, the
// only sensible default is "usable": someone who fills in the form and clicks save clearly does
// not intend to "create one that cannot be used".
func TestCreateAgentDefaultsToActive(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newChatService(t)

	created, err := svc.CreateAgent(ctx, seedWS, apimodel.AgentDraft{
		Name:  "默认状态测试",
		Model: apimodel.ModelConfig{ID: "doubao-seed-2-1-pro-260628"},
	})
	if err != nil {
		t.Fatalf("建 agent 失败：%v", err)
	}
	if created.Status != apimodel.StatusActive {
		t.Errorf("新建 agent 应当默认启用，实际 %q", created.Status)
	}
	// Right after creation it should be able to start a conversation, without changing the status again.
	if _, err := svc.CreateConversation(ctx, seedWS, created.ID); err != nil {
		t.Fatalf("新建的 agent 应当直接可用，实际 %v", err)
	}
}

// TestValidateAgentDraftRejectsUnknownStatus: invalid status values must be blocked at the entry.
//
// Before, validateAgentDraft did not validate status at all, so any string could be stored, and
// since IsDisabled only recognizes "disabled", a misspelled "disable" would silently be treated as
// enabled (the user thinks it is stopped, and it keeps running).
//
// An empty string must be let through: that means "not passed", and CreateAgent fills in the
// default.
func TestValidateAgentDraftRejectsUnknownStatus(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newChatService(t)

	base := func(status apimodel.AgentStatus) apimodel.AgentDraft {
		return apimodel.AgentDraft{
			Name:   "状态校验",
			Model:  apimodel.ModelConfig{ID: "doubao-seed-2-1-pro-260628"},
			Status: status,
		}
	}

	// A misspelled value: the most realistic misuse, and completely silent without validation.
	if _, err := svc.CreateAgent(ctx, seedWS, base("disable")); err == nil {
		t.Error("拼错的状态值应当被拒——否则用户以为停用了，它照跑")
	} else if !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("应当是 ErrInvalidArgument，实际 %v", err)
	}

	// The removed old enum value: no longer a valid input (existing data is a different matter; that goes through the read path and is let through).
	if _, err := svc.CreateAgent(ctx, seedWS, base("draft")); err == nil {
		t.Error("draft 已经删掉了，不该还能作为入参写进来")
	}

	// An empty string must be let through.
	if _, err := svc.CreateAgent(ctx, seedWS, base("")); err != nil {
		t.Errorf("空串是「没传」，必须放行，实际 %v", err)
	}
	// Both valid values must be let through.
	if _, err := svc.CreateAgent(ctx, seedWS, base(apimodel.StatusDisabled)); err != nil {
		t.Errorf("disabled 是合法值，实际 %v", err)
	}
}

// TestSchedulerSkipsDisabledAgentAndRecordsWhy: when a scheduled task meets a disabled agent:
// do not run, record why, and **schedule the next run as usual**.
//
// # What each of the three assertions guards
//
// 1. **Do not run**: running means disabling did not take effect, and scheduled tasks are the path
// most easily overlooked. Nobody watches it, and it would quietly run a round every period, burn
// tokens, and even post into groups.
//
// 2. **Record why** (LastError): this is where "not silent" lands. The frontend SchedulesPage
// already shows lastError, so users can see for themselves "why this task has no result". Without
// it, the task looks like "the time came and nothing happened", which is the hardest kind to track
// down.
//
// 3. **Schedule the next run as usual**: disabling can be undone. Without scheduling the next run,
// after the user re-enables the agent this task **would never fire again**, a consequence far
// worse than "failed once", and completely invisible.
func TestSchedulerSkipsDisabledAgentAndRecordsWhy(t *testing.T) {
	ctx := context.Background()
	sched, svc, store, runner, _, clock := newTestScheduler(t)
	// Give it frames: if the gate does not work, it really runs and produces a reply, and only then can the assertions catch it.
	runner.frames = agentReplyFrames("我不该跑")

	s := seedSchedule(t, svc, nil)
	disableAgent(t, store, "agent_triage")
	makeDue(t, store, s.ID, clock.Add(-time.Minute))

	sched.tick(ctx)
	sched.wg.Wait()

	// 1. Not a single round should run: no session created, no events sent.
	if runner.createdSessions != 0 {
		t.Errorf("停用的 agent 不该被定时任务驱动，实际建了 %d 个 session", runner.createdSessions)
	}
	if len(runner.sentEvents) != 0 {
		t.Errorf("停用的 agent 不该收到 prompt，实际发了 %d 条", len(runner.sentEvents))
	}

	got, err := store.GetSchedule(ctx, s.ID)
	if err != nil {
		t.Fatalf("查任务失败：%v", err)
	}
	// 2. The reason must be left, and it must say "disabled"; writing only "failed" leaves the user guessing.
	if got.LastError == "" {
		t.Fatal("要留下失败原因，否则任务表现成「到点了什么都没发生」")
	}
	if !strings.Contains(got.LastError, "停用") {
		t.Errorf("原因要指向停用这件事，实际 %q", got.LastError)
	}
	// 3. Schedule the next run as usual: disabling can be undone, and once re-enabled it must keep running.
	if got.NextRunAt == "" {
		t.Error("失败也要排下一次触发，否则启用回来之后这条任务永远不动了")
	}
}

// TestSchedulerRunsEnabledAgent: an enabled agent is driven as usual.
//
// Paired with the previous one: with only the "disabled is skipped" test, an implementation that
// skips every task would also pass.
func TestSchedulerRunsEnabledAgent(t *testing.T) {
	ctx := context.Background()
	sched, svc, store, runner, _, clock := newTestScheduler(t)
	runner.frames = agentReplyFrames("巡检完成")

	s := seedSchedule(t, svc, nil)
	makeDue(t, store, s.ID, clock.Add(-time.Minute))

	sched.tick(ctx)
	sched.wg.Wait()

	if runner.createdSessions != 1 {
		t.Errorf("启用的 agent 该被驱动一轮，实际建了 %d 个 session", runner.createdSessions)
	}
	got, _ := store.GetSchedule(ctx, s.ID)
	if got.LastError != "" {
		t.Errorf("成功一轮不该留错误，实际 %q", got.LastError)
	}
}
