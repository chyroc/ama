// Tests for the conversation path, all offline (Ark is replaced by fakeRunner).
//
// They watch the places that **raise no error when wrong, and only show up as weird problems on
// the page**:
//
//   - shape of SSE frames: what Ark sends and what the frontend expects are not the same; translate
//     it wrong and the whole conversation does not show;
//   - byte fidelity of events: escape it once and the same message is drawn twice;
//   - derived state of the run: wrong status / usage / event count, and run history is full of
//     "running forever".

package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chyroc/ama/server/repository/dao/coremem"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/service/arkrun"
	"github.com/chyroc/ama/server/shared/gopkg/arkframe"
	"github.com/chyroc/ama/server/shared/infra/ma"
)

// fakeRunner is a programmable Ark runner, so tests need no network.
//
// ⚠️ **It has a lock**: the resident subscription watcher (core/runwatcher.go) uses it from several
// goroutines at once. Without the lock, `go test -race` reports a DATA RACE on the counters. That
// is a problem in the fake, not in the code under test, but it makes -race fail for the whole package.
type fakeRunner struct {
	mu sync.Mutex

	// frames are the frames Stream emits, in order.
	frames [][]byte
	// sentEvents records the raw input of each SendEvents call, to assert pass-through is lossless.
	sentEvents []json.RawMessage
	// sendEventsErr makes SendEvents fail **permanently**, to check how Ark-side errors are
	// translated for the user.
	sendEventsErr error
	// sendEventsTransientErr + sendEventsFailTimes simulate the "tool execution window":
	// the first sendEventsFailTimes calls fail with this error, after that the window closes and
	// calls succeed.
	//
	// **Two separate fields from sendEventsErr is deliberate.** With one shared field, "succeed
	// after n failures" would have to be expressed as "stop looking at the error field once the
	// count runs out", and that is exactly what the first version got wrong: after the count hit
	// 0, the permanent error still matched, so the retry never got a success. The case reported
	// "retry had no effect", while the real cause was the fake itself failing all along.
	sendEventsTransientErr error
	sendEventsFailTimes    int
	// sendEventsCalls counts how many times SendEvents was called, including the failed ones.
	//
	// Only this shows "how many retries happened": looking only at the final success or failure,
	// succeeding after one retry and succeeding on the first try are **impossible to tell apart**,
	// and that is exactly what is being checked here.
	sendEventsCalls int
	// spec records the config used when creating the Ark Agent.
	spec ArkAgentSpec
	// sessionID is the ID returned by CreateSession.
	sessionID string
	// createdAgents / createdEnvs are counters, to assert resources are not created twice.
	createdAgents int
	createdEnvs   int
	// createdSessions counts how many times CreateSession was called.
	//
	// Assertions like "a rejected request must not create a single session" need it: cases that
	// only assert "an error was returned" are completely blind to an implementation that "really
	// calls Ark first, then reports an error", and such an implementation leaves sessions and
	// containers on the Ark side that waste resources.
	createdSessions int
	// agentErr / envErr / sessionErr let tests simulate Ark-side failures.
	agentErr   error
	envErr     error
	sessionErr error
	// sessionErrOnce makes only the **next** CreateSession fail; after that it returns to normal.
	// Used to simulate "the Ark-side resource was deleted; clearing the binding and rebuilding fixes it".
	sessionErrOnce error
	// sessionErrs is an error queue consumed in call order: the nth CreateSession takes the nth
	// one; nil means that call succeeds. When the queue is empty, sessionErrOnce / sessionErr are used.
	// Used to simulate hitting two kinds of 404 in a row during one self-heal (environment gone,
	// Agent gone too).
	sessionErrs []error
	// listEvents are the history event frames ListEvents returns.
	listEvents      [][]byte
	listEventsErr   error
	listEventsCalls int
	// listEventsDelay makes ListEvents hang for this long (or until ctx expires), to check
	// "when Ark is slow, does the user's request get dragged down".
	//
	// **It must really wait on ctx** (not a blind time.Sleep): the code under test caps this step
	// with context.WithTimeout. If the fake ignores ctx, that cap is **impossible to test**: it
	// still waits the full time after the timeout, and the case ends up measuring the sleep length.
	// The check must be "which one expires first"; that is what is being checked here.
	listEventsDelay time.Duration
	// listEventsHook runs once before ListEvents returns, so a case can simulate
	// "during this backfill, someone else (another pod / an SSE subscriber) just changed the DB".
	//
	// Production is multi-instance, so this is not hypothetical: the same Ark session is
	// subscribed to by browser SSE, Feishu inbound and the resident watcher at the same time,
	// and they all write events to the same run. Combined with listEventsErr it produces "the DB
	// already changed, but our backfill this time failed", which is exactly the window guarded by
	// "the re-read must always happen" in GetRun.
	listEventsHook func()

	// -- skills --------------------------------------------------------------

	// uploadedSkills records the package of each UploadSkill call, to assert dedup and content.
	uploadedSkills []fakeUpload
	// uploadErr makes uploads fail, to check "a failure must degrade, the conversation still opens".
	uploadErr error
	// updatedSkills records the set of refs received by UpdateAgentSkills (the last call).
	updatedSkills []persistmodel.ArkSkillRef
	// updateSkillsCalls is a counter, to assert "this call must not be made if the fingerprint
	// did not change".
	updateSkillsCalls int
	updateSkillsErr   error

	// createAgentSkillCount makes CreateAgent report a skill count different from the input,
	// to simulate "it was not actually attached on the Ark side", which is the real shape when the
	// ma layer drops Skills.
	//
	// nil means report truthfully (len(spec.Skills) attached); this is the default and normal behavior.
	createAgentSkillCount *int

	// -- spec (system / model / tools) catch-up ----------------------------------

	// updatedSpecs records the spec of each UpdateAgentSpec call, **keeping all of them in order**.
	//
	// Not just the last one: assertions like "it was called twice for nothing in one round" need
	// to see every call; keeping only the last would make that waste invisible in tests.
	updatedSpecs []ArkAgentSpec
	// updateSpecErr makes spec catch-up fail, to check "on failure, do not write back the
	// fingerprint; retry next time".
	updateSpecErr error

	// -- Ark byte-level pass-through (arkproxy.go) -------------------------------------
	// When not configured, RawDo/RawStream return an error, so cases other than the pass-through
	// tests do not falsely succeed.
	rawDoFunc   func(key ArkBindingKey, req ma.RawRequest) (*ma.RawResponse, error)
	rawStreamFn func(key ArkBindingKey, req ma.RawRequest) (*ma.RawStream, error)
	lastRawKey  ArkBindingKey
	lastRawReq  ma.RawRequest
	rawCalls    int
}

// reportedSkills makes the fake report "how many skills Ark confirmed as attached".
//
// **By default it reports the input truthfully**, not unconditional success: this fake must be
// able to tell "skills were passed" from "skills were really attached". That gap is exactly where
// this repo's zero-error failure lived (core computed the refs, the literal in the ma layer dropped
// them, and skills on the Ark side were empty).
func (f *fakeRunner) reportedSkills(spec ArkAgentSpec) int {
	if f.createAgentSkillCount != nil {
		return *f.createAgentSkillCount
	}
	return len(spec.Skills)
}

// fakeUpload is the record of one skill upload.
type fakeUpload struct {
	fileName     string
	displayTitle string
	zip          []byte
}

func (f *fakeRunner) UploadSkill(
	_ context.Context, _ ArkBindingKey, fileName, displayTitle string, zip []byte,
) (string, string, error) {
	if f.uploadErr != nil {
		return "", "", f.uploadErr
	}
	f.uploadedSkills = append(f.uploadedSkills, fakeUpload{
		fileName: fileName, displayTitle: displayTitle, zip: zip,
	})
	// Return a different id each time, same as the real Ark (CreateSkill always creates a new skill).
	// This matters: with a fixed id, the "duplicate upload" bug would not show in tests.
	return fmt.Sprintf("skill-ark-%d", len(f.uploadedSkills)), "1", nil
}

func (f *fakeRunner) UpdateAgentSkills(
	_ context.Context, _ ArkBindingKey, _ string, refs []persistmodel.ArkSkillRef,
) error {
	f.updateSkillsCalls++
	if f.updateSkillsErr != nil {
		return f.updateSkillsErr
	}
	f.updatedSkills = refs
	return nil
}

func (f *fakeRunner) UpdateAgentSpec(
	_ context.Context, _ ArkBindingKey, _ string, spec ArkAgentSpec,
) error {
	if f.updateSpecErr != nil {
		// **Do not record this call on failure**: otherwise assertions like "was there a retry
		// after the failure" would need subtraction, which easily mixes up the two cases.
		return f.updateSpecErr
	}
	f.updatedSpecs = append(f.updatedSpecs, spec)
	return nil
}

// The two byte-level pass-through methods are programmed via rawDoFunc in the pass-through's own
// tests (arkproxy_test.go); without a hook they return an error, to avoid false success.
func (f *fakeRunner) RawDo(_ context.Context, key ArkBindingKey, req ma.RawRequest) (*ma.RawResponse, error) {
	f.mu.Lock()
	f.rawCalls++
	f.lastRawKey = key
	f.lastRawReq = req
	fn := f.rawDoFunc
	f.mu.Unlock()
	if fn == nil {
		return nil, errors.New("fakeRunner: 未配置 RawDo")
	}
	return fn(key, req)
}

func (f *fakeRunner) RawStream(_ context.Context, key ArkBindingKey, req ma.RawRequest) (*ma.RawStream, error) {
	if f.rawStreamFn == nil {
		return nil, errors.New("fakeRunner: 未配置 RawStream")
	}
	return f.rawStreamFn(key, req)
}

func (f *fakeRunner) CreateAgent(_ context.Context, _ ArkBindingKey, spec ArkAgentSpec) (ArkCreatedAgent, error) {
	if f.agentErr != nil {
		return ArkCreatedAgent{}, f.agentErr
	}
	f.spec = spec
	f.createdAgents++
	return ArkCreatedAgent{
		ID:         fmt.Sprintf("agent-fake-%d", f.createdAgents),
		SkillCount: f.reportedSkills(spec),
	}, nil
}

func (f *fakeRunner) EnsureEnvironment(_ context.Context, _ ArkBindingKey, _ string) (string, error) {
	if f.envErr != nil {
		return "", f.envErr
	}
	f.createdEnvs++
	return fmt.Sprintf("env-fake-%d", f.createdEnvs), nil
}

func (f *fakeRunner) CreateSession(_ context.Context, _ ArkBindingKey, _ persistmodel.ArkBinding, _ string) (string, error) {
	f.mu.Lock()
	// The error queue consumed in call order goes first (see the sessionErrs field comment).
	if len(f.sessionErrs) > 0 {
		err := f.sessionErrs[0]
		f.sessionErrs = f.sessionErrs[1:]
		f.mu.Unlock()
		if err != nil {
			return "", err
		}
	} else {
		f.mu.Unlock()
	}
	if f.sessionErrOnce != nil {
		err := f.sessionErrOnce
		f.sessionErrOnce = nil
		return "", err
	}
	if f.sessionErr != nil {
		return "", f.sessionErr
	}
	if f.sessionID == "" {
		f.sessionID = "sesn-fake-0001"
	}
	// Count before the success return: a failed attempt does not count as "created".
	f.createdSessions++
	return f.sessionID, nil
}

func (f *fakeRunner) SendEvents(
	_ context.Context, _ ArkBindingKey, _ string, raw []json.RawMessage,
) ([]json.RawMessage, error) {
	f.sendEventsCalls++
	// The first n calls fail as "hit the tool execution window", then pass. See sendEventsTransientErr.
	if f.sendEventsFailTimes > 0 {
		f.sendEventsFailTimes--
		return nil, f.sendEventsTransientErr
	}
	if f.sendEventsErr != nil {
		return nil, f.sendEventsErr
	}
	f.sentEvents = append(f.sentEvents, raw...)
	// Shares the same assembly function with the real implementation (service/arkrun.AcceptedFrom):
	// if each wrote its own, tests would stay green on the old shape after the receipt shape
	// changed. See the comment on that function.
	return arkrun.AcceptedFrom(raw), nil
}

func (f *fakeRunner) Stream(
	ctx context.Context, _ ArkBindingKey, _ string, onFrame func(raw []byte) error,
) error {
	for _, frame := range f.frames {
		if ctx.Err() != nil {
			return nil
		}
		if err := onFrame(frame); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeRunner) ListEvents(
	ctx context.Context, _ ArkBindingKey, _ string,
) ([][]byte, error) {
	// **Lock**: the resident subscription (runwatcher) calls it from several goroutines at once,
	// and -race would report the counter reads/writes here as a DATA RACE. This is the fake's own
	// problem, not the code under test's, but without the fix -race fails for the whole package.
	f.mu.Lock()
	delay := f.listEventsDelay
	hook := f.listEventsHook
	f.listEventsCalls++
	f.mu.Unlock()

	// Simulate "Ark is slow". **Wait outside the lock**: sleeping while holding the lock would
	// serialize concurrent calls, and assertions like "two requests each call once" could no
	// longer test real concurrent behavior.
	//
	// select rather than time.Sleep: the code under test caps this step with a context; only
	// really responding to ctx makes that cap visible in tests (see the listEventsDelay comment).
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// Run outside the lock: the hook usually writes to Store, and holding the lock would serialize
	// it with other calls.
	if hook != nil {
		hook()
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listEventsErr != nil {
		return nil, f.listEventsErr
	}
	out := make([][]byte, len(f.listEvents))
	copy(out, f.listEvents)
	return out, nil
}

// newChatService builds a Service with a fakeRunner and an Ark credential installed.
func newChatService(t *testing.T) (*Service, *coremem.MemStore, *fakeRunner) {
	t.Helper()
	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败：%v", err)
	}
	runner := &fakeRunner{}
	// ⚠️ **BlobStore must be installed**: skill file bytes can now only go into object storage
	// (the content column is no longer written). Without it, every case that uploads skill files
	// would fail with "object storage not configured", while those cases test conversation and
	// skill sync, not object storage.
	svc, err := New(Config{Store: store, ArkRunner: runner, BlobStore: newFakeBlobStore()})
	if err != nil {
		t.Fatalf("装配 Service 失败：%v", err)
	}
	// Shrink the retry budget for the "tool execution window".
	//
	// The production value is 1 second (see windowRetryBudget), while fakeRunner's sendEventsErr
	// rejects **permanently**, which would make every such case wait the whole budget for nothing.
	//
	// **200ms is not arbitrary**: the interval is 40ms (windowRetryStep), so this budget fits about
	// 4 retries. Something smaller (say 5ms) would make the "succeed after two failures" case run
	// out of budget before the second retry, showing up as "retry had no effect", while the real
	// cause is just a budget smaller than the interval.
	// When changing windowRetryStep, take a look here too.
	svc.windowRetry = 200 * time.Millisecond
	// Conversations use the workspace's own key; without it the first step is blocked by the
	// precondition.
	if err := store.PutArkSecret(context.Background(), apimodel.ArkSecret{
		ArkCredential: apimodel.ArkCredential{WorkspaceID: seedWS, Region: apimodel.RegionCNBeijing},
		APIKey:        "ARK0123456789abcdef",
	}); err != nil {
		t.Fatalf("写凭证失败：%v", err)
	}
	return svc, store, runner
}

// TestFeishuConversationVisibleInWorkspaceList: a conversation created from Feishu must be found
// in the conversation list of the workspace it belongs to, and must show "from Feishu".
//
// # Which end-to-end path this case guards
//
// Feishu inbound → CreateFeishuConversation → lands in the agent's workspace → found by the web
// ListRuns. If any link breaks, the user sees the same thing: "messages sent in Feishu don't show
// on the web":
//
//   - wrong workspace → not found by workspaceID;
//   - trigger not set → run history cannot tell the source (and the frontend's
//     CONVERSATION_TRIGGERS cannot filter);
//   - lastActivityAt not refreshed → the conversation sits where it was ten hours ago, and the
//     user cannot find it.
//
// All three are pinned here at once.
func TestFeishuConversationVisibleInWorkspaceList(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newChatService(t)

	run, err := svc.CreateFeishuConversation(ctx, seedWS, "agent_triage", "飞书单聊")
	if err != nil {
		t.Fatalf("建飞书会话失败：%v", err)
	}

	if run.Trigger != apimodel.TriggerFeishu {
		t.Errorf("trigger 应为 feishu，实际 %q", run.Trigger)
	}
	if run.TriggerDetail != "飞书单聊" {
		t.Errorf("来源说明没带上：%q", run.TriggerDetail)
	}
	// It must land in the agent's workspace, otherwise that workspace's list cannot find it at all.
	if run.WorkspaceID != seedWS {
		t.Errorf("会话落错空间了：%q", run.WorkspaceID)
	}
	// A new conversation must have an activity time, otherwise sorting has to rely on a fallback.
	if run.LastActivityAt == "" {
		t.Error("新建会话没有 lastActivityAt，列表排序会退化成按创建时间")
	}

	// Key assertion: the web conversation list (the same endpoint) can find it.
	list, err := svc.ListRuns(ctx, seedWS, persistmodel.RunQuery{})
	if err != nil {
		t.Fatalf("列运行失败：%v", err)
	}
	var found *apimodel.Run
	for i := range list {
		if list[i].ID == run.ID {
			found = &list[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("飞书建的会话没出现在空间 %s 的列表里（共 %d 条）", seedWS, len(list))
	}
	if found.Trigger != apimodel.TriggerFeishu {
		t.Errorf("列表里那条的 trigger 不对：%q", found.Trigger)
	}
	// A just-created conversation has the newest activity time and must be first: the user should
	// see it as soon as they open the chat page.
	if list[0].ID != run.ID {
		t.Errorf("刚建的飞书会话没排在最前面，第一条是 %q", list[0].ID)
	}
}

// TestAppendRunEventsBumpsLastActivity: sending one message must refresh the activity time.
//
// For reused conversations (Feishu reuses by scope, the web continues from history) startedAt is
// always the first round's; only this field moves. Without refreshing it, "a conversation that
// just got a message floats to the top" does not hold for old conversations, which is exactly the
// failure being fixed here.
//
// It deliberately does not rely on a subscription: applyEventToRun also refreshes this field, but
// it is only called when there is a subscriber.
func TestAppendRunEventsBumpsLastActivity(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}
	// Make a conversation "created long ago": push both the activity time and the creation time back.
	run.StartedAt = "2026-01-01T00:00:00.000Z"
	run.LastActivityAt = "2026-01-01T00:00:00.000Z"
	if _, err := store.SaveRun(ctx, run); err != nil {
		t.Fatalf("改时间失败：%v", err)
	}

	msg := json.RawMessage(`{"type":"user.message","content":[{"type":"text","text":"今天周几"}]}`)
	if _, err := svc.AppendRunEvents(ctx, run.ID, []json.RawMessage{msg}); err != nil {
		t.Fatalf("发消息失败：%v", err)
	}

	got, err := svc.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("取 run 失败：%v", err)
	}
	if got.LastActivityAt <= "2026-01-01T00:00:00.000Z" {
		t.Errorf("发了消息但活跃时刻没往前走：%q", got.LastActivityAt)
	}
	// startedAt must not be touched: run history shows "when it started" from it.
	if got.StartedAt != "2026-01-01T00:00:00.000Z" {
		t.Errorf("startedAt 不该被改：%q", got.StartedAt)
	}
}

// TestCreateConversationWithoutCredential: without a key configured, give an actionable Chinese
// hint.
//
// This text is shown as-is on the chat page, so it is part of the contract: the user must know
// where to configure it next.
func TestCreateConversationWithoutCredential(t *testing.T) {
	ctx := context.Background()
	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败：%v", err)
	}
	svc, err := New(Config{Store: store, ArkRunner: &fakeRunner{}})
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}

	_, err = svc.CreateConversation(ctx, seedWS, "agent_triage")
	if !errors.Is(err, ErrPrecondition) {
		t.Fatalf("应返回 ErrPrecondition，实际 %v", err)
	}
	if msg := userMessage(err); !strings.Contains(msg, "方舟 API Key") {
		t.Errorf("文案要指明去配 key，实际 %q", msg)
	}
}

// TestCreateConversationShape: the shape of a new conversation must match what the frontend expects.
func TestCreateConversationShape(t *testing.T) {
	ctx := context.Background()
	svc, _, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}

	// trigger must be manual: ChatPage uses it to filter the left-side conversation list; get it
	// wrong and new conversations do not show.
	if run.Trigger != triggerManual {
		t.Errorf("trigger 应为 manual，实际 %q", run.Trigger)
	}
	// run.id is the Ark session id; RunDetailPage shows it as "Session ID".
	if run.ID != "sesn-fake-0001" {
		t.Errorf("run.id 应该就是方舟 session id，实际 %q", run.ID)
	}
	if run.AgentID != "agent_triage" || run.AgentName != "工单分诊助手" {
		t.Errorf("agent 信息没填对：%+v", run)
	}
	// Leave the title empty; it is filled from the first message (copied from LocalApiClient).
	if run.Title != "" {
		t.Errorf("新对话的标题应为空，实际 %q", run.Title)
	}
	// Timestamps must have the fixed three-decimal shape, otherwise listRuns' string sort breaks.
	if !strings.HasSuffix(run.StartedAt, "Z") || len(run.StartedAt) != len("2026-08-26T11:14:42.189Z") {
		t.Errorf("startedAt 形状不对：%q", run.StartedAt)
	}
	// An unfinished run must not have endedAt / durationMs.
	if run.EndedAt != "" || run.DurationMs != nil {
		t.Errorf("新对话不该有结束时间：%+v", run)
	}
	if run.Threads == nil {
		t.Error("threads 不能是 nil（前端会直接 .map()）")
	}

	// AMA's builtin_tools are single tool names, while Ark takes toolset types; they cannot be
	// passed through directly.
	if len(runner.spec.Tools) != 1 || runner.spec.Tools[0] != "agent_toolset_20260401" {
		t.Errorf("方舟 Agent 的 tools 应该是工具集类型，实际 %v", runner.spec.Tools)
	}
	if !strings.Contains(runner.spec.Name, "agent_triage") {
		t.Errorf("方舟 Agent 名里要带 AMA 的 agent id，实际 %q", runner.spec.Name)
	}
}

// TestSendMessageFillsTitleAndStatus: the first message fills the title and sets the status to running.
func TestSendMessageFillsTitleAndStatus(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}

	body := []json.RawMessage{
		json.RawMessage(`{"type":"user.message","content":[{"type":"text","text":"结算页面打不开了，很多商家反馈"}]}`),
	}
	accepted, err := svc.AppendRunEvents(ctx, run.ID, body)
	if err != nil {
		t.Fatalf("发消息失败：%v", err)
	}
	// Receipts must map one-to-one to the events in the request; the frontend draws the first one.
	if len(accepted) != 1 {
		t.Fatalf("回执数应为 1，实际 %d", len(accepted))
	}
	var ack struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(accepted[0], &ack); err != nil {
		t.Fatalf("回执不是合法 JSON：%v（%s）", err, accepted[0])
	}
	if ack.ID == "" || ack.Type != "user.message" {
		t.Errorf("回执信封不对：%s", accepted[0])
	}
	if len(ack.Content) != 1 || ack.Content[0].Text != "结算页面打不开了，很多商家反馈" {
		t.Errorf("回执里的正文变形了：%s", accepted[0])
	}

	// Upstream events are forwarded to Ark as-is, with no second processing.
	if len(runner.sentEvents) != 1 {
		t.Fatalf("转给方舟的事件数应为 1，实际 %d", len(runner.sentEvents))
	}

	after, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("查 run 失败：%v", err)
	}
	if after.Title != "结算页面打不开了，很多商家反馈" {
		t.Errorf("标题应由首条消息回填，实际 %q", after.Title)
	}
	if after.Status != apimodel.RunRunning {
		t.Errorf("发完消息状态应为 running，实际 %q", after.Status)
	}

	// The second message must not change the title.
	second := []json.RawMessage{
		json.RawMessage(`{"type":"user.message","content":[{"type":"text","text":"再补充一点"}]}`),
	}
	if _, err := svc.AppendRunEvents(ctx, run.ID, second); err != nil {
		t.Fatalf("发第二条失败：%v", err)
	}
	after2, _ := store.GetRun(ctx, run.ID)
	if after2.Title != after.Title {
		t.Errorf("后续消息不该改标题：%q → %q", after.Title, after2.Title)
	}
}

// TestTruncateTitle: titles are truncated by rune, matching the frontend's UTF-16 behavior.
func TestTruncateTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"你好", "你好"},
		{"  多个   空白   压成一个 ", "多个 空白 压成一个"},
		{"换行\n也算空白", "换行 也算空白"},
		// 31 Chinese characters must become 30 + an ellipsis (counting by bytes would cut at 10).
		{strings.Repeat("字", 31), strings.Repeat("字", 30) + "…"},
		{strings.Repeat("字", 30), strings.Repeat("字", 30)},
	}
	for _, c := range cases {
		if got := truncateTitle(c.in); got != c.want {
			t.Errorf("truncateTitle(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestStreamPersistsEventsAndUpdatesRun: after one pass of streaming, both history and the run's
// derived state must be right.
//
// This covers the "conversation content is restored after refresh" path: the frontend first
// pulls listRunEvents when entering the page; if events were not stored, the conversation is empty.
func TestStreamPersistsEventsAndUpdatesRun(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}

	// Copied from one round pushed by the real Ark (shape captured with curl): no sequence,
	// session-level events have no session_thread_id, with best-effort delta frames in between.
	runner.frames = [][]byte{
		[]byte(`{"type":"session.status_running","id":"sevt-1","processed_at":"2026-08-26T11:14:42.189Z"}`),
		[]byte(`{"type":"session.thread_status_running","id":"sevt-2","agent_name":"工单分诊助手","session_thread_id":"sthr-1"}`),
		[]byte(`{"type":"user.message","id":"sevt-3","content":[{"type":"text","text":"你好"}]}`),
		[]byte(`{"event":{"id":"sevt-4","type":"agent.message"},"type":"event_start"}`),
		[]byte(`{"type":"event_delta","event_id":"sevt-4","delta":{"type":"content_delta","index":0,"content":{"type":"text","text":"我是"}}}`),
		[]byte(`{"id":"sevt-4","type":"agent.message","session_thread_id":"sthr-1","content":[{"type":"text","text":"我是分诊助手"}]}`),
		[]byte(`{"type":"span.model_request_end","id":"sevt-5","session_thread_id":"sthr-1","model_usage":{"input_tokens":5831,"output_tokens":74,"cache_read_input_tokens":10,"cache_creation_input_tokens":0},"is_error":false}`),
		[]byte(`{"type":"session.thread_status_idle","id":"sevt-6","agent_name":"工单分诊助手","session_thread_id":"sthr-1","stop_reason":{"type":"end_turn"}}`),
		[]byte(`{"type":"session.status_idle","id":"sevt-7","stop_reason":{"type":"end_turn"}}`),
		[]byte(`[DONE]`),
	}

	var got [][]byte
	if err := svc.StreamRunEvents(ctx, run.ID, func(raw []byte) error {
		got = append(got, append([]byte(nil), raw...))
		return nil
	}); err != nil {
		t.Fatalf("订阅失败：%v", err)
	}
	if len(got) != len(runner.frames) {
		t.Errorf("转发的帧数应为 %d，实际 %d", len(runner.frames), len(got))
	}

	// Delta frames and [DONE] **do not go into history**: Ark states clearly that they are not in
	// event history, and storing them would add types to history that the frontend RunEvent union
	// does not even have.
	events, err := store.ListRunEvents(ctx, run.ID, persistmodel.EventQuery{})
	if err != nil {
		t.Fatalf("列事件失败：%v", err)
	}
	if len(events) != 7 {
		t.Fatalf("落库事件数应为 7（10 帧去掉 2 个增量帧和 [DONE]），实际 %d", len(events))
	}
	for _, ev := range events {
		if ev.Type == "event_start" || ev.Type == "event_delta" || ev.Type == "" {
			t.Errorf("增量帧不该落库：%s", ev.Raw)
		}
	}

	after, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("查 run 失败：%v", err)
	}
	if after.Status != apimodel.RunIdle {
		t.Errorf("收到 status_idle 后状态应为 idle，实际 %q", after.Status)
	}
	// Once finished it must have endedAt / durationMs (they are absent while unfinished).
	if after.EndedAt == "" || after.DurationMs == nil {
		t.Errorf("结束后要补上 endedAt/durationMs：%+v", after)
	}
	if after.Usage.InputTokens != 5831 || after.Usage.OutputTokens != 74 ||
		after.Usage.CacheReadInputTokens != 10 {
		t.Errorf("用量没累加对：%+v", after.Usage)
	}
	if after.EventCount != 7 {
		t.Errorf("eventCount 应为 7，实际 %d", after.EventCount)
	}
	if len(after.Threads) != 1 || after.Threads[0].ID != "sthr-1" ||
		after.Threads[0].Status != "idle" {
		t.Errorf("线程没收集对：%+v", after.Threads)
	}
	// The main thread's parent_thread_id must be null, not a missing key.
	if after.Threads[0].ParentThreadID != nil {
		t.Errorf("主线程的 parent 应为 null，实际 %v", after.Threads[0].ParentThreadID)
	}
}

// TestStreamMapsRequiresAction: stop_reason requires_action is a separate status.
//
// The Ark side is still idle, but "stuck waiting for confirmation" and "normally idle" are two
// different things to the user.
func TestStreamMapsRequiresAction(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}
	runner.frames = [][]byte{
		[]byte(`{"type":"session.status_idle","id":"sevt-1","stop_reason":{"type":"requires_action","event_ids":["sevt-x"]}}`),
	}
	if err := svc.StreamRunEvents(ctx, run.ID, func([]byte) error { return nil }); err != nil {
		t.Fatalf("订阅失败：%v", err)
	}
	after, _ := store.GetRun(ctx, run.ID)
	if after.Status != apimodel.RunRequiresAction {
		t.Errorf("状态应为 requires_action，实际 %q", after.Status)
	}
}

// TestStreamEndpointEmitsSSE goes through HTTP end to end: headers, frames and [DONE] must all be right.
func TestStreamEndpointEmitsSSE(t *testing.T) {
	ctx := context.Background()
	svc, _, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}
	runner.frames = [][]byte{
		[]byte(`{"event":{"id":"sevt-1","type":"agent.message"},"type":"event_start"}`),
		[]byte(`{"type":"event_delta","event_id":"sevt-1","delta":{"content":{"type":"text","text":"好"}}}`),
		[]byte(`{"id":"sevt-1","type":"agent.message","session_thread_id":"sthr-1","content":[{"type":"text","text":"好"}]}`),
	}

	h := newTestHandler(t, svc, nil)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest(http.MethodGet,
		"/api/runs/"+run.ID+"/events/stream?event_deltas=agent.message", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type 应为 text/event-stream，实际 %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("要禁缓存，实际 %q", cc)
	}
	body := rec.Body.String()
	// three frames + [DONE]
	if !strings.Contains(body, `"kind":"event_start"`) {
		t.Errorf("缺 event_start 帧：%s", body)
	}
	if !strings.Contains(body, `"kind":"event_delta"`) {
		t.Errorf("缺 event_delta 帧：%s", body)
	}
	if !strings.Contains(body, `"type":"agent.message"`) {
		t.Errorf("缺最终事件：%s", body)
	}
	// Without [DONE] the frontend thinks the connection dropped and reconnects automatically.
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("缺 [DONE] 帧：%s", body)
	}
}

// TestStreamEndpointRejectsMissingRunWith404: subscribing to a run that does not exist must return
// a real 404, not 200 + an in-stream error.
//
// **This is the regression test for the "SSE error storm".** The old approach called
// WriteHeader(200) as usual and sent the error as one session.error frame. The problem is that a
// missing run is a **permanent** error, while 200 + an in-stream error looks exactly like
// "connected, then dropped" at the HTTP layer. The frontend cannot tell whether to retry, so
// every remount triggers another round. In practice the same invalid runId was subscribed 3
// times in 6 seconds, each logging an ERROR.
//
// It asserts four things, none can be dropped:
//
//	status 404          — the browser then fails the connection and **does not reconnect** (observed readyState=2)
//	Content-Type JSON   — must not be text/event-stream, otherwise the browser treats it as a normal stream
//	code:"not_found"    — the frontend http.ts decides by code, not by status code (see that file's header)
//	no [DONE] in body   — this path should not produce any SSE frame at all
func TestStreamEndpointRejectsMissingRunWith404(t *testing.T) {
	svc, _, _ := newChatService(t)

	h := newTestHandler(t, svc, nil)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest(http.MethodGet,
		"/api/runs/sesn-does-not-exist/events/stream?event_deltas=agent.message", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("订阅不存在的 run 应回 404，实际 %d，响应体 %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("错误响应应是 JSON 而不是 SSE，实际 Content-Type=%q", ct)
	}
	var body struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是 JSON：%v（原文 %s）", err, rec.Body.String())
	}
	// code is the frontend's only stable basis for deciding; the text may change, the code may not.
	if body.Code != "not_found" {
		t.Errorf(`code 应为 "not_found"，实际 %q`, body.Code)
	}
	if body.Message == "" {
		t.Error("message 不能为空，前端会把它显示给用户")
	}
	// This path must not produce any SSE frame; a [DONE] means it went back to "write headers
	// first, then report the error".
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("404 路径不该产出 SSE 帧：%s", rec.Body.String())
	}
}

// TestArkBindingIsReusedAndPersisted: Ark resources are created only once, and the mapping must
// be stored.
//
// **This is the regression test for a restart incident.** Bindings used to live only in process
// memory, and the symptom was: the first run worked fine, and after a restart it failed 100% of
// the time. Ark Environment names are unique within a project, so creating one with the same
// name again returns 409 ResourceConflict, and that workspace could never open a new
// conversation again. On top of that every restart created one more Ark Agent (Agents may share
// names), piling up over time.
//
// So this asserts three things: the same agent is not created twice, a second agent in the same
// workspace reuses the environment, and a new Service (simulating a restart) creates nothing either.
func TestArkBindingIsReusedAndPersisted(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	if _, err := svc.CreateConversation(ctx, seedWS, "agent_triage"); err != nil {
		t.Fatalf("第一次建对话失败：%v", err)
	}
	if runner.createdAgents != 1 || runner.createdEnvs != 1 {
		t.Fatalf("第一次应各建一个：agents=%d envs=%d", runner.createdAgents, runner.createdEnvs)
	}

	// The binding must really be stored.
	binding, err := store.GetArkBinding(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("绑定没落库：%v", err)
	}
	if binding.ArkAgentID != "agent-fake-1" || binding.ArkEnvironmentID != "env-fake-1" {
		t.Errorf("绑定内容不对：%+v", binding)
	}

	// Open another conversation with the same agent: nothing should be created again.
	if _, err := svc.CreateConversation(ctx, seedWS, "agent_triage"); err != nil {
		t.Fatalf("第二次建对话失败：%v", err)
	}
	if runner.createdAgents != 1 || runner.createdEnvs != 1 {
		t.Errorf("同一个 agent 不该重复建：agents=%d envs=%d",
			runner.createdAgents, runner.createdEnvs)
	}

	// Another agent in the same workspace: a new Ark Agent must be created, but **the same
	// environment is reused**.
	if _, err := svc.CreateConversation(ctx, seedWS, "agent_analyst"); err != nil {
		t.Fatalf("建第二个 agent 的对话失败：%v", err)
	}
	if runner.createdAgents != 2 {
		t.Errorf("新 agent 应该建一个方舟 Agent，实际共 %d 个", runner.createdAgents)
	}
	if runner.createdEnvs != 1 {
		t.Errorf("环境要按空间复用，不该再建：envs=%d", runner.createdEnvs)
	}
	second, err := store.GetArkBinding(ctx, seedWS, "agent_analyst")
	if err != nil {
		t.Fatalf("第二个绑定没落库：%v", err)
	}
	if second.ArkEnvironmentID != binding.ArkEnvironmentID {
		t.Errorf("两个 agent 的环境应相同：%q vs %q",
			second.ArkEnvironmentID, binding.ArkEnvironmentID)
	}

	// **Simulate a process restart**: a new Service (new in-process state), but the same Store.
	// The old implementation would rebuild resources here and hit 409; now nothing should be created.
	restarted, err := New(Config{Store: store, ArkRunner: runner})
	if err != nil {
		t.Fatalf("重建 Service 失败：%v", err)
	}
	if _, err := restarted.CreateConversation(ctx, seedWS, "agent_triage"); err != nil {
		t.Fatalf("重启后建对话失败：%v", err)
	}
	if runner.createdAgents != 2 || runner.createdEnvs != 1 {
		t.Errorf("重启后不该重建任何方舟资源：agents=%d envs=%d",
			runner.createdAgents, runner.createdEnvs)
	}
}

// TestStaleBindingSelfHeals: after Ark-side resources are deleted, the binding must heal itself.
//
// Once stored, a binding never expires on its own. After someone deletes the Ark Agent in the
// console (or switches to a key pointing at another project), creating a session returns 404.
// Without clearing the binding, this agent can **never** open a new conversation again, and the
// error is just an unexplained 404.
func TestStaleBindingSelfHeals(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	if _, err := svc.CreateConversation(ctx, seedWS, "agent_triage"); err != nil {
		t.Fatalf("第一次建对话失败：%v", err)
	}
	before, err := store.GetArkBinding(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("绑定没落库：%v", err)
	}

	// Simulate "the Ark-side Agent was deleted": the next session creation returns 404, then
	// back to normal.
	runner.sessionErrOnce = &ma.APIError{HTTPStatusCode: http.StatusNotFound}

	if _, err := svc.CreateConversation(ctx, seedWS, "agent_triage"); err != nil {
		t.Fatalf("自愈后建对话仍失败：%v", err)
	}

	after, err := store.GetArkBinding(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("自愈后绑定应该重建：%v", err)
	}
	if after.ArkAgentID == before.ArkAgentID {
		t.Errorf("失效的方舟 Agent 应被换掉：仍是 %q", after.ArkAgentID)
	}
	if runner.createdAgents != 2 {
		t.Errorf("应该重建一个方舟 Agent，实际共建了 %d 个", runner.createdAgents)
	}
}

// TestEnvironmentNotFoundHealsAllSiblingBindings: when the Ark Environment is deleted (404
// InvalidEnvironment.NotFound), self-heal must create a new environment and repoint **all sibling
// bindings in the same workspace** to it, not just delete its own row. Otherwise the rebuild picks
// the same dead environment back up from a sibling row, and the second session creation hits the
// same 404.
//
// This is the real shape of a production scheduled task that failed once an hour: two agents
// shared env-..., the platform reclaimed the environment, the old self-heal only deleted its own
// row, so every round replayed the failure and created a useless Agent each time.
func TestEnvironmentNotFoundHealsAllSiblingBindings(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	// Two agents each open one conversation: they share env-fake-1.
	if _, err := svc.CreateConversation(ctx, seedWS, "agent_triage"); err != nil {
		t.Fatalf("agent_triage 第一次建对话失败：%v", err)
	}
	if _, err := svc.CreateConversation(ctx, seedWS, "agent_analyst"); err != nil {
		t.Fatalf("agent_analyst 第一次建对话失败：%v", err)
	}
	if runner.createdEnvs != 1 || runner.createdAgents != 2 {
		t.Fatalf("前置状态不对：envs=%d agents=%d，应为 1/2",
			runner.createdEnvs, runner.createdAgents)
	}

	// Environment deleted: the next session creation returns 404 InvalidEnvironment.NotFound,
	// then recovers.
	runner.sessionErrOnce = &ma.APIError{
		HTTPStatusCode: http.StatusNotFound,
		Code:           "InvalidEnvironment.NotFound",
		Message:        "The environment env-fake-1 does not exist or you do not have access to it.",
	}

	if _, err := svc.CreateConversation(ctx, seedWS, "agent_triage"); err != nil {
		t.Fatalf("环境自愈后建对话仍失败：%v", err)
	}

	// Both its own row and the sibling row must point at the new environment.
	for _, agentID := range []string{"agent_triage", "agent_analyst"} {
		b, err := store.GetArkBinding(ctx, seedWS, agentID)
		if err != nil {
			t.Fatalf("取 %s 的绑定失败：%v", agentID, err)
		}
		if b.ArkEnvironmentID != "env-fake-2" {
			t.Errorf("%s 应被改指到 env-fake-2，实际 %q", agentID, b.ArkEnvironmentID)
		}
	}

	// The environment changed, but the Ark Agent is an independent resource and works once
	// attached to the new environment: **the Agent must not be rebuilt**.
	if runner.createdAgents != 2 {
		t.Errorf("环境 404 不该重建 Agent，实际共建了 %d 个（应为 2）",
			runner.createdAgents)
	}
	if runner.createdEnvs != 2 {
		t.Errorf("应该新建一个环境，实际共 %d 个（应为 2）", runner.createdEnvs)
	}

	// The sibling agent opens a conversation next round: zero new resources, it reuses the
	// repointed environment directly.
	if _, err := svc.CreateConversation(ctx, seedWS, "agent_analyst"); err != nil {
		t.Fatalf("兄弟 agent 自愈后建对话失败：%v", err)
	}
	if runner.createdEnvs != 2 || runner.createdAgents != 2 {
		t.Errorf("兄弟 agent 不该新建任何资源：envs=%d agents=%d，应为 2/2",
			runner.createdEnvs, runner.createdAgents)
	}
}

// TestEnvironmentNotFoundThenAgentAlsoGone: after the environment-404 self-heal, the retry still
// gets 404 (the Agent happens to be deleted too). It must fall into the "delete its own binding,
// rebuild the Agent too" branch, rather than throwing the second 404 straight at the user.
func TestEnvironmentNotFoundThenAgentAlsoGone(t *testing.T) {
	ctx := context.Background()
	svc, _, runner := newChatService(t)

	if _, err := svc.CreateConversation(ctx, seedWS, "agent_triage"); err != nil {
		t.Fatalf("第一次建对话失败：%v", err)
	}

	// 1st CreateSession: environment 404; 2nd try after the environment self-heal: a 404 with no
	// code (Agent gone); 3rd after deleting the binding and rebuilding: queue exhausted, default success.
	runner.sessionErrs = []error{
		&ma.APIError{
			HTTPStatusCode: http.StatusNotFound,
			Code:           "InvalidEnvironment.NotFound",
		},
		&ma.APIError{HTTPStatusCode: http.StatusNotFound},
	}

	if _, err := svc.CreateConversation(ctx, seedWS, "agent_triage"); err != nil {
		t.Fatalf("环境+Agent 双失后应自愈成功，实际：%v", err)
	}
	if runner.createdAgents != 2 {
		t.Errorf("Agent 没了应重建一个，实际共建 %d 个（应为 2）", runner.createdAgents)
	}
	// The workspace's only binding: repoint first changed its own row; after the binding is
	// deleted, ensureBinding finds no sibling environment to reuse, so it creates another one
	// (env-fake-3); env-fake-2 becomes an empty orphan environment. A double delete is very rare;
	// see the comment on healMissingArkResource for this tradeoff.
	if runner.createdEnvs != 3 {
		t.Errorf("双失窄口应共建 3 个环境（自愈 1 + 重建 1 + 首个 1），实际 %d",
			runner.createdEnvs)
	}
}

// TestCreateConversationManagedAgentNotOpen: when the account has not activated Managed Agents,
// it must return ErrPrecondition + actionable Chinese text (with account id and request_id),
// rather than falling into the handler default and becoming 500 "internal server error".
//
// In production the global credential was once switched to a key belonging to a non-activated
// account; every workspace inheriting the global credential got 500 on session creation, and the
// only fix, "go activate it in the Ark console", was only in the server logs.
func TestCreateConversationManagedAgentNotOpen(t *testing.T) {
	ctx := context.Background()
	svc, _, runner := newChatService(t)
	runner.envErr = &ma.APIError{
		Code:           "ManagedAgentNotOpen",
		Message:        "Your account 2100000001 has not activated the managed agent. Please activate the service in the Ark Console.",
		Type:           "Not Found",
		HTTPStatusCode: http.StatusNotFound,
		RequestId:      "req-xyz-123",
	}

	_, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if !errors.Is(err, ErrPrecondition) {
		t.Fatalf("应当是 ErrPrecondition（422，用户可修），实际 %v", err)
	}
	msg := userMessage(err)
	if !strings.Contains(msg, "Managed Agents") {
		t.Errorf("文案要说明是没开通 Managed Agents，实际 %q", msg)
	}
	if !strings.Contains(msg, "2100000001") {
		t.Errorf("文案要带方舟账号 id，实际 %q", msg)
	}
	if !strings.Contains(msg, "req-xyz-123") {
		t.Errorf("文案要带 request_id 便于报障，实际 %q", msg)
	}

	// ⭐ After translation, the structured error chain must not break (a P1 caught in code
	// review): the error returned by arkFailure must both match the sentinel with errors.Is (the
	// handler relies on it to return 422) and still let Ark's original *ma.APIError be found by
	// errors.As (arkSyncStageOf relies on it to decide the failure stage). Attaching only the
	// sentinel would make the latter always false, and string matching cannot recover the
	// not-activated meaning.
	if !ma.IsManagedAgentNotOpen(err) {
		t.Error("经 arkFailure 翻译后 ma.IsManagedAgentNotOpen 仍应为 true（错误链被丢了）")
	}
	if got := ma.RequestID(err); got != "req-xyz-123" {
		t.Errorf("经翻译后 request_id 应仍可取到，实际 %q", got)
	}
}

// TestNotOpenMessageWithoutRequestID: when request_id is missing, the text must not include a
// dangling half sentence, but "the account is not activated" and "go activate it in the console"
// must still be clear.
func TestNotOpenMessageWithoutRequestID(t *testing.T) {
	ctx := context.Background()
	svc, _, runner := newChatService(t)
	runner.envErr = &ma.APIError{
		Code:           "ManagedAgentNotOpen",
		Message:        "Your account 2100000001 has not activated the managed agent.",
		HTTPStatusCode: http.StatusNotFound,
	}
	_, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if !errors.Is(err, ErrPrecondition) {
		t.Fatalf("应当是 ErrPrecondition，实际 %v", err)
	}
	msg := userMessage(err)
	if strings.Contains(msg, "request_id  ") || strings.HasSuffix(strings.TrimSpace(msg), "request_id") {
		t.Errorf("request_id 缺失时不该留空话，实际 %q", msg)
	}
	if !strings.Contains(msg, "2100000001") {
		t.Errorf("仍要带账号 id，实际 %q", msg)
	}
}

// TestNotOpenDoesNotTriggerSelfHeal: hitting a NotOpen 404 when creating a session must **not**
// take the "delete the binding and rebuild" self-heal path: the binding is fine, and rebuilding
// would only create a pair of orphan resources and hit the same error again.
func TestNotOpenDoesNotTriggerSelfHeal(t *testing.T) {
	ctx := context.Background()
	svc, _, runner := newChatService(t)

	// The first conversation is created successfully and the binding is stored.
	if _, err := svc.CreateConversation(ctx, seedWS, "agent_triage"); err != nil {
		t.Fatalf("第一次建对话失败：%v", err)
	}
	// After that CreateSession always returns NotOpen (simulating the account's service being
	// turned off midway).
	runner.sessionErr = &ma.APIError{
		Code:           "ManagedAgentNotOpen",
		Message:        "Your account 2100000001 has not activated the managed agent.",
		HTTPStatusCode: http.StatusNotFound,
	}

	if _, err := svc.CreateConversation(ctx, seedWS, "agent_triage"); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("应当是 ErrPrecondition，实际 %v", err)
	}
	// Self-heal did not trigger: the environment and the Agent were each created only once.
	if runner.createdEnvs != 1 || runner.createdAgents != 1 {
		t.Errorf("NotOpen 不该触发删绑定重建：envs=%d agents=%d，都应为 1",
			runner.createdEnvs, runner.createdAgents)
	}
}

// TestCreateConversationModelEndpointNotOpen: when the account has not activated that model, Ark
// returns 400 InvalidParameter + service not open. It must be translated into ErrPrecondition +
// actionable Chinese text (activate the model / switch model), rather than falling into the
// handler default and becoming 500.
//
// In production: the global credential was switched to a new account that had not activated the
// doubao-seed-evolving model the agent used. Creating the Ark Agent got this 400, and users only
// saw "internal server error, please retry later".
func TestCreateConversationModelEndpointNotOpen(t *testing.T) {
	ctx := context.Background()
	svc, _, runner := newChatService(t)
	runner.agentErr = &ma.APIError{
		Code: "InvalidParameter",
		Message: `model.id: endpoint "doubao-seed-evolving" is invalid: ` +
			`get endpoint meta doubao-seed-evolving: service not open ` +
			`Request id: 021789198385722a77e5a4c9aa25c0e7eca6d226bba90a0b61158`,
		Type:           "Bad Request",
		HTTPStatusCode: http.StatusBadRequest,
		RequestId:      "021789198385722a77e5a4c9aa25c0e7eca6d226bba90a0b61158",
	}

	_, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if !errors.Is(err, ErrPrecondition) {
		t.Fatalf("应当是 ErrPrecondition（422，用户可修），实际 %v", err)
	}
	msg := userMessage(err)
	for _, want := range []string{"doubao-seed-evolving", "开通", "换", "021789198385722a77e5a4c9aa25c0e7eca6d226bba90a0b61158"} {
		if !strings.Contains(msg, want) {
			t.Errorf("文案要包含 %q，实际 %q", want, msg)
		}
	}

	// Same requirement as NotOpen: after translation the original Ark error chain must not break,
	// and request_id must still be retrievable.
	if id, ok := ma.ModelEndpointNotOpen(err); !ok || id != "doubao-seed-evolving" {
		t.Errorf("翻译后仍应能识别未开通的模型 id，got=%q ok=%v", id, ok)
	}
}

// TestCreateConversationPlainInvalidParameterStays500: also a 400 InvalidParameter, but with a
// message other than "service not open"; it must be passed up unchanged (the handler returns 500).
// That is a bug in our own request body; disguising it as a user config problem would hide a
// real failure.
func TestCreateConversationPlainInvalidParameterStays500(t *testing.T) {
	ctx := context.Background()
	svc, _, runner := newChatService(t)
	runner.agentErr = &ma.APIError{
		Code:           "InvalidParameter",
		Message:        "model.id is required",
		HTTPStatusCode: http.StatusBadRequest,
	}

	_, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if errors.Is(err, ErrPrecondition) {
		t.Fatalf("普通参数错误不该翻成 422，实际 %v", err)
	}
	if _, ok := ma.ModelEndpointNotOpen(err); ok {
		t.Errorf("普通参数错误不该被认成模型未开通")
	}
}

// TestStreamedFinalEventsCarrySequence: final events forwarded over SSE must carry sequence, and
// must be **byte-for-byte identical** to the copy from listRunEvents.
//
// **This pitfall only showed up when running end to end against the real Ark**; reading the code
// alone, both paths look "right": Ark's raw event has no sequence, it is added when stored, but
// the raw one was forwarded, so the same event had two shapes in the frontend. timeline.ts sorts
// by sequence, and undefined makes `a.sequence - b.sequence` compute NaN, so the order of the
// whole timeline becomes undefined. Drafts also could not be replaced because the bytes
// differed, and the same reply was drawn twice.
func TestStreamedFinalEventsCarrySequence(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}
	// The shape the real Ark pushes: **no sequence**.
	runner.frames = [][]byte{
		[]byte(`{"type":"session.status_running","id":"sevt-1"}`),
		[]byte(`{"type":"event_delta","event_id":"sevt-2","delta":{"content":{"type":"text","text":"好"}}}`),
		[]byte(`{"id":"sevt-2","type":"agent.message","session_thread_id":"sthr-1","content":[{"type":"text","text":"好"}]}`),
	}

	var forwarded [][]byte
	if err := svc.StreamRunEvents(ctx, run.ID, func(raw []byte) error {
		forwarded = append(forwarded, append([]byte(nil), raw...))
		return nil
	}); err != nil {
		t.Fatalf("订阅失败：%v", err)
	}

	stored, err := store.ListRunEvents(ctx, run.ID, persistmodel.EventQuery{})
	if err != nil {
		t.Fatalf("列事件失败：%v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("落库事件数应为 2，实际 %d", len(stored))
	}

	// The forwarded final events (frames 1 and 3) must be byte-for-byte identical to the stored ones.
	byteEqual := func(a, b []byte) bool { return string(a) == string(b) }
	if !byteEqual(forwarded[0], stored[0].Raw) {
		t.Errorf("转发的第一条与库里的不一致：\nSSE  %s\n库里 %s", forwarded[0], stored[0].Raw)
	}
	if !byteEqual(forwarded[2], stored[1].Raw) {
		t.Errorf("转发的最终事件与库里的不一致：\nSSE  %s\n库里 %s", forwarded[2], stored[1].Raw)
	}

	// Every final event must carry sequence, otherwise the frontend's sort computes NaN.
	for i, raw := range [][]byte{forwarded[0], forwarded[2]} {
		var ev struct {
			Sequence *int `json:"sequence"`
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("第 %d 条不是合法 JSON：%v", i, err)
		}
		if ev.Sequence == nil {
			t.Errorf("转发的最终事件缺 sequence：%s", raw)
		}
	}

	// The delta frame (frame 2) is not stored; forward it as-is. It never had a sequence anyway.
	if !strings.Contains(string(forwarded[1]), `"event_delta"`) {
		t.Errorf("增量帧应原样转发：%s", forwarded[1])
	}
}

// sseDataPayload strips the payload after data: from one SSE frame.
func sseDataPayload(t *testing.T, frame []byte) []byte {
	t.Helper()
	line := strings.TrimSuffix(string(frame), "\n\n")
	payload, ok := strings.CutPrefix(line, "data: ")
	if !ok {
		t.Fatalf("不是一帧合法 SSE：%q", frame)
	}
	return []byte(payload)
}

// TestSyncRunEventsBackfillsMissingEvents: after the stream drops the run record stays at running;
// GetRun automatically backfills the trailing events.
func TestSyncRunEventsBackfillsMissingEvents(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}

	// Simulate sending a message and entering the running state
	body := []json.RawMessage{
		json.RawMessage(`{"type":"user.message","content":[{"type":"text","text":"测试提问"}]}`),
	}
	if _, err := svc.AppendRunEvents(ctx, run.ID, body); err != nil {
		t.Fatalf("发消息失败：%v", err)
	}

	// Simulate Ark finishing completely, while the SSE drop means nothing after that was received locally
	runner.listEvents = [][]byte{
		[]byte(`{"id":"sevt-1","type":"session.status_running"}`),
		[]byte(`{"id":"sevt-2","type":"user.message","content":[{"type":"text","text":"测试提问"}]}`),
		[]byte(`{"id":"sevt-3","type":"agent.thinking","content":[{"type":"text","text":"思考中"}]}`),
		[]byte(`{"id":"sevt-4","type":"agent.message","content":[{"type":"text","text":"这是最终回答"}]}`),
		[]byte(`{"id":"sevt-5","type":"span.model_request_end","model_usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":20}}`),
		[]byte(`{"id":"sevt-6","type":"session.status_idle","stop_reason":{"type":"end_turn"}}`),
	}

	// The user opens the detail page and reads the Run: it should trigger the backfill sync and
	// return the latest state
	syncedRun, err := svc.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun 失败：%v", err)
	}

	if syncedRun.Status != apimodel.RunIdle {
		t.Errorf("补偿同步后状态应为 idle，实际 %q", syncedRun.Status)
	}
	if syncedRun.EndedAt == "" {
		t.Errorf("补偿同步后应记录 endedAt")
	}
	if syncedRun.DurationMs == nil || *syncedRun.DurationMs < 0 {
		t.Errorf("补偿同步后应计算 durationMs")
	}
	if syncedRun.Usage.InputTokens != 100 || syncedRun.Usage.OutputTokens != 50 || syncedRun.Usage.CacheReadInputTokens != 20 {
		t.Errorf("用量未正确累加：%+v", syncedRun.Usage)
	}

	events, err := store.ListRunEvents(ctx, run.ID, persistmodel.EventQuery{})
	if err != nil {
		t.Fatalf("ListRunEvents 失败：%v", err)
	}
	if len(events) != 6 {
		t.Fatalf("同步后事件数应为 6，实际 %d", len(events))
	}
	// Check the final message content
	if !strings.Contains(string(events[3].Raw), "这是最终回答") {
		t.Errorf("缺最终回答事件：%s", events[3].Raw)
	}
	// **The thinking text must be kept intact too.**
	//
	// Ark's official docs (the session event structure reference) say agent.thinking "does not
	// return the actual thinking content". The frontend once removed content from the type based
	// on that sentence, so the chat page showed only the final reply and lost the whole thinking
	// process. But in practice Ark does send the text (in the production DB, 69 of 72
	// agent.thinking events had non-empty content), and this backend path never dropped it. This
	// pins it, so nobody filters it out one day "following the docs".
	if !strings.Contains(string(events[2].Raw), "思考中") {
		t.Errorf("思考正文被丢了（文档说不返回，实测会返回，别按文档过滤）：%s", events[2].Raw)
	}
	// The step that serializes into the HTTP response must keep it intact too: it builds bytes by
	// hand, not with json.Marshal.
	if !strings.Contains(string(apimodel.AppendJSONArray(events)), "思考中") {
		t.Errorf("思考正文在序列化时被丢了")
	}
}

// TestSyncRunEventsDeduplicatesExistingEvents: sync does not write events that already exist.
func TestSyncRunEventsDeduplicatesExistingEvents(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}

	// Write the first 2 events locally first
	if _, _, err := store.AppendRunEvent(ctx, run.ID, []byte(`{"id":"sevt-1","type":"session.status_running"}`)); err != nil {
		t.Fatalf("写事件 1 失败：%v", err)
	}
	if _, _, err := store.AppendRunEvent(ctx, run.ID, []byte(`{"id":"sevt-2","type":"user.message","content":[{"type":"text","text":"hello"}]}`)); err != nil {
		t.Fatalf("写事件 2 失败：%v", err)
	}

	// Ark returns 3 events (the first 2 duplicate local ones, the 3rd is new)
	runner.listEvents = [][]byte{
		[]byte(`{"id":"sevt-1","type":"session.status_running"}`),
		[]byte(`{"id":"sevt-2","type":"user.message","content":[{"type":"text","text":"hello"}]}`),
		[]byte(`{"id":"sevt-3","type":"agent.message","content":[{"type":"text","text":"world"}]}`),
	}

	if err := svc.SyncRunEvents(ctx, run.ID); err != nil {
		t.Fatalf("SyncRunEvents 失败：%v", err)
	}

	events, err := store.ListRunEvents(ctx, run.ID, persistmodel.EventQuery{})
	if err != nil {
		t.Fatalf("ListRunEvents 失败：%v", err)
	}
	if len(events) != 3 {
		t.Fatalf("去重后事件数应为 3，实际 %d", len(events))
	}
	for i, ev := range events {
		if ev.Sequence != i+1 {
			t.Errorf("第 %d 条事件 sequence 应为 %d，实际 %d", i, i+1, ev.Sequence)
		}
	}
}

// TestConcurrentSubscribersDoNotDuplicateEvents: with two subscribers online at the same time,
// events are stored only once.
//
// # This reproduces a production scene
//
// One Ark session can have several subscribers at once: Feishu inbound (feishu/inbound.go)
// starts an internal stream, the user opening the chat page in a browser starts an SSE
// (streamRunEvents in core/handler.go), and scheduled tasks and Feishu resume each have one too.
// **They are not mutually exclusive**: hasActiveStream only decides "whether to backfill history
// once before opening the stream", and never stops a second subscriber from attaching.
//
// So the same event is received once by each of the two streams, and each calls persistArkEvent
// → AppendRunEvent. The sequence number is issued by AppendRunEvent itself; if it is not
// idempotent, the two calls get two numbers, both rows are stored, and the primary key
// (run_id, sequence) does not conflict at all. Observed in production: a conversation started
// from Feishu, and from the moment the chat page was opened in a browser every later event was
// stored as 2 rows, and the UI showed the same reply several times.
//
// **It also asserts that the bytes forwarded to browsers are identical**: persistArkEvent forwards
// the stored bytes (with sequence), and both subscribers must forward the same number, otherwise
// the frontend's sort by sequence puts the same message in two places.
func TestConcurrentSubscribersDoNotDuplicateEvents(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}

	// Each of the two streams receives these three frames; that is how the real Ark pushes
	// (every subscriber of the same session gets all events).
	runner.frames = [][]byte{
		[]byte(`{"id":"sevt-a","type":"session.status_running"}`),
		[]byte(`{"id":"sevt-b","type":"agent.message","content":[{"type":"text","text":"让我检查一下当前环境中是否安装了这些工具。"}],"session_thread_id":"sthr-1"}`),
		[]byte(`{"id":"sevt-c","type":"session.status_idle","stop_reason":{"type":"end_turn"}}`),
	}

	// Two subscribers subscribe concurrently, each recording the bytes forwarded to it.
	const subscribers = 2
	forwarded := make([][][]byte, subscribers)
	var wg sync.WaitGroup
	for i := 0; i < subscribers; i++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			err := svc.StreamRunEvents(ctx, run.ID, func(raw []byte) error {
				forwarded[k] = append(forwarded[k], append([]byte(nil), raw...))
				return nil
			})
			if err != nil {
				t.Errorf("第 %d 个订阅者出错：%v", k, err)
			}
		}(i)
	}
	wg.Wait()

	events, err := store.ListRunEvents(ctx, run.ID, persistmodel.EventQuery{})
	if err != nil {
		t.Fatalf("列事件失败：%v", err)
	}
	if len(events) != len(runner.frames) {
		t.Fatalf("两个订阅者收到同样的 %d 帧，库里也只该有 %d 条，实际 %d 条"+
			"（这正是对话页同一句话显示多遍的原因）",
			len(runner.frames), len(runner.frames), len(events))
	}

	// Each event id may appear only once.
	seen := map[string]int{}
	for _, ev := range events {
		seen[ev.EventID]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("事件 %s 存了 %d 行，应为 1 行", id, n)
		}
	}

	// The bytes the two subscribers got must be byte-for-byte identical, including the same sequence.
	if len(forwarded[0]) != len(forwarded[1]) {
		t.Fatalf("两个订阅者收到的帧数不一致：%d vs %d",
			len(forwarded[0]), len(forwarded[1]))
	}
	for i := range forwarded[0] {
		if !bytes.Equal(forwarded[0][i], forwarded[1][i]) {
			t.Errorf("第 %d 帧转发给两个订阅者的字节不一致（sequence 会对不上，"+
				"前端排序会把同一条消息排到两个位置）：\n甲 %s\n乙 %s",
				i, forwarded[0][i], forwarded[1][i])
		}
	}
}

// TestActiveStreamSkipsSyncInGetRun: when an active SSE stream exists, GetRun does not actively
// sync from Ark.
func TestActiveStreamSkipsSyncInGetRun(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}
	run.Status = apimodel.RunRunning
	if _, err := store.SaveRun(ctx, run); err != nil {
		t.Fatalf("保存 run 失败：%v", err)
	}

	// Simulate the stream being active
	cleanup := svc.markStreamActive(run.ID)
	if !svc.hasActiveStream(run.ID) {
		t.Fatalf("应该识别为活跃流")
	}

	if _, err := svc.GetRun(ctx, run.ID); err != nil {
		t.Fatalf("GetRun 失败：%v", err)
	}
	if runner.listEventsCalls != 0 {
		t.Errorf("流活跃时不应调用 ListEvents 同步，实际调用 %d 次", runner.listEventsCalls)
	}

	// After the stream disconnects
	cleanup()
	if svc.hasActiveStream(run.ID) {
		t.Fatalf("流应已非活跃")
	}

	if _, err := svc.GetRun(ctx, run.ID); err != nil {
		t.Fatalf("GetRun 失败：%v", err)
	}
	if runner.listEventsCalls != 1 {
		t.Errorf("流断开后应调用 ListEvents 同步 1 次，实际调用 %d 次", runner.listEventsCalls)
	}
}

// TestListRunEventsTriggersSyncWhenRunning: listing events also backfills automatically when running.
func TestListRunEventsTriggersSyncWhenRunning(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}
	run.Status = apimodel.RunRunning
	if _, err := store.SaveRun(ctx, run); err != nil {
		t.Fatalf("保存 run 失败：%v", err)
	}

	runner.listEvents = [][]byte{
		[]byte(`{"id":"sevt-1","type":"agent.message","content":[{"type":"text","text":"hello"}]}`),
	}

	events, err := svc.ListRunEvents(ctx, run.ID, persistmodel.EventQuery{})
	if err != nil {
		t.Fatalf("ListRunEvents 失败：%v", err)
	}
	if len(events) != 1 {
		t.Fatalf("应同步出 1 条事件，实际 %d", len(events))
	}
	if runner.listEventsCalls != 1 {
		t.Errorf("应调用 1 次 ListEvents 同步，实际 %d", runner.listEventsCalls)
	}
}

// TestSyncCoversAllActiveStatuses: the status allowlist for backfill must cover all three
// "still alive" statuses.
//
// # This case blocks a zero-error hole
//
// Backfill once only accepted `running`. But under self_hosted, Ark moves to
// `idle + requires_action` on **every tool call**, and the run is set to `requires_action`
// accordingly (applyEventToRun). That means the most common drop point, "SSE dropped during tool
// execution", falls exactly on the excluded status. As a result the trailing events (including
// the idle of `end_turn`) **could never be backfilled**: the API returned 200, logs showed
// nothing wrong, the data was just missing a piece, and the frontend `groupTurns()` therefore
// kept `done` false for the last round forever, so the intermediate steps never collapsed again.
//
// The check matches the three-status allowlist of Store.ListActiveRuns. **Each status is asserted
// separately**: testing only requires_action would stay all green even if the allowlist were
// wrongly written as "only requires_action".
func TestSyncCoversAllActiveStatuses(t *testing.T) {
	for _, tc := range []struct {
		status   apimodel.RunStatus
		wantSync bool
		why      string
	}{
		{apimodel.RunRunning, true, "跑着的当然要补"},
		{apimodel.RunRequiresAction, true, "工具在我们容器里执行，agent 还会继续产出事件"},
		{apimodel.RunRescheduling, true, "从瞬时错误恢复中，后面还有事件"},
		{apimodel.RunIdle, false, "这一轮答完了，没有尾巴可补"},
		{apimodel.RunTerminated, false, "会话已终止，再拉也不会有新事件"},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			ctx := context.Background()
			svc, store, runner := newChatService(t)

			run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
			if err != nil {
				t.Fatalf("建对话失败：%v", err)
			}
			run.Status = tc.status
			if _, err := store.SaveRun(ctx, run); err != nil {
				t.Fatalf("保存 run 失败：%v", err)
			}

			if _, err := svc.ListRunEvents(ctx, run.ID, persistmodel.EventQuery{}); err != nil {
				t.Fatalf("ListRunEvents 失败：%v", err)
			}

			got := runner.listEventsCalls > 0
			if got != tc.wantSync {
				t.Errorf("status=%s 补拉=%v，期望 %v（%s）", tc.status, got, tc.wantSync, tc.why)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Freezing after the agent is deleted
// ---------------------------------------------------------------------------

// TestAppendRunEventsRejectsDeletedAgent: a conversation whose agent was deleted may not send
// messages anymore.
//
// Agents are hard-deleted with no cascade, so before this gate was added, "still able to chat
// after deletion" was real behavior: AppendRunEvents only did GetRun and then computed arkKey,
// and arkKey reads the **workspace-level** credential and never looks at the agent table. The Ark
// session was still alive and the conversation went on as usual.
func TestAppendRunEventsRejectsDeletedAgent(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}
	if err := store.DeleteAgent(ctx, "agent_triage"); err != nil {
		t.Fatalf("删 agent 失败：%v", err)
	}

	body := []json.RawMessage{
		json.RawMessage(`{"type":"user.message","content":[{"type":"text","text":"还能发出去吗"}]}`),
	}
	_, err = svc.AppendRunEvents(ctx, run.ID, body)
	if err == nil {
		t.Fatal("agent 已删除时发消息应当失败")
	}

	// **It must be Precondition (422), not NotFound (404).** The frontend http.ts turns not_found
	// into null, and the page shows a silent empty state; the cause cannot be found from the
	// symptom at all. This assertion guards against "someone switching it to 404 to save effort".
	if !errors.Is(err, ErrPrecondition) {
		t.Errorf("应当是 ErrPrecondition，实际 %v", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("不能是 ErrNotFound：前端会把它转成 null，页面变静默空状态")
	}
	// The text is a contract: LocalApiClient.sendMessage copies it word for word, and e2e asserts it too.
	if msg := userMessage(err); !strings.Contains(msg, "只能查看历史") {
		t.Errorf("文案要说清还能做什么，实际 %q", msg)
	}

	// The gate is before SendEvents: not a single character should reach Ark.
	if len(runner.sentEvents) != 0 {
		t.Errorf("被拒的消息不该转给方舟，实际转了 %d 条", len(runner.sentEvents))
	}
	// There must be no side effects either (title fill, status set to running).
	after, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("查 run 失败：%v", err)
	}
	if after.Title != "" {
		t.Errorf("被拒的消息不该回填标题，实际 %q", after.Title)
	}
	if after.Status != apimodel.RunIdle {
		t.Errorf("被拒的消息不该改状态，实际 %q", after.Status)
	}
}

// TestAppendRunEventsAllowsInterruptAfterAgentDeleted: freezing only blocks writes; interrupts
// must pass.
//
// This is the executable doc for "why let it pass": the Ark-side session may still be running;
// blocking everything means the user can never stop it. Ark keeps burning tokens, and the
// container is only reclaimed at idle pause or even TTL. An interrupt creates no new work; it
// stops work.
func TestAppendRunEventsAllowsInterruptAfterAgentDeleted(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}
	if err := store.DeleteAgent(ctx, "agent_triage"); err != nil {
		t.Fatalf("删 agent 失败：%v", err)
	}

	body := []json.RawMessage{json.RawMessage(`{"type":"user.interrupt"}`)}
	if _, err := svc.AppendRunEvents(ctx, run.ID, body); err != nil {
		t.Fatalf("agent 已删除时中断应当放行，实际 %v", err)
	}
	// It must really be forwarded to Ark, not just "no error".
	if len(runner.sentEvents) != 1 {
		t.Fatalf("中断要转给方舟，实际转了 %d 条", len(runner.sentEvents))
	}
	// A pure interrupt must not set the run back to running.
	after, _ := store.GetRun(ctx, run.ID)
	if after.Status == apimodel.RunRunning {
		t.Error("纯中断不该把状态置为 running")
	}
}

// TestRedirectRunSendsInterruptBeforeMessage: a redirect must **interrupt first, then send the new
// message**, and both must really reach Ark.
//
// # The order is the whole point of this case
//
// The other way round (send the message first, then interrupt) means something completely
// different: the new message queues **after** the current round, and then the interrupt cuts it
// off too. The user clicked "send now", and both messages are gone. And the two implementations
// **return exactly the same thing** (both the new message's receipt, neither reports an error);
// only the event order shows the difference.
//
// It also pins "a redirect is not an interjection": the two events must not be sent to Ark in the
// same batch; see the comment on RedirectRun.
func TestRedirectRunSendsInterruptBeforeMessage(t *testing.T) {
	ctx := context.Background()
	svc, _, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}
	runner.sendEventsCalls = 0

	accepted, err := svc.RedirectRun(ctx, run.ID, "别查那个了，先看 CI")
	if err != nil {
		t.Fatalf("转向失败：%v", err)
	}

	// The receipt is only the new message's; the interrupt's receipt carries no information (see
	// the comment on RedirectRun).
	if len(accepted) != 1 {
		t.Fatalf("回执应当只有新消息那一条，实际 %d", len(accepted))
	}
	// Two events, sent in two batches. In one batch, the message might be consumed before the agent
	// has stopped; that is an "interjection", not a "redirect".
	if runner.sendEventsCalls != 2 {
		t.Errorf("应当分两次发（中断、消息），实际调了 %d 次", runner.sendEventsCalls)
	}
	if len(runner.sentEvents) != 2 {
		t.Fatalf("方舟应当收到 2 个事件，实际 %d", len(runner.sentEvents))
	}
	if got := arkframe.Type(runner.sentEvents[0]); got != "user.interrupt" {
		t.Errorf("第一个事件必须是 user.interrupt，实际 %q", got)
	}
	if got := arkframe.Type(runner.sentEvents[1]); got != "user.message" {
		t.Errorf("第二个事件必须是 user.message，实际 %q", got)
	}
	// The text must be carried over unchanged; do not lose it while building the event.
	if text := TitleFromEvents(runner.sentEvents[1:]); !strings.Contains(text, "先看 CI") {
		t.Errorf("新消息的正文没带过去，实际 %q", text)
	}
}

// TestRedirectRunFailsWhenInterruptFails: if the interrupt fails, the whole thing fails; it must
// **not** fall back to a normal send.
//
// The consequence of falling back is concrete: the user clicked "send now" and got "queued at the
// end". The message was sent, but the agent is still busy with the previous thing, and nothing in
// the UI shows that the redirect failed.
func TestRedirectRunFailsWhenInterruptFails(t *testing.T) {
	ctx := context.Background()
	svc, _, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}
	runner.sendEventsErr = &ma.APIError{
		Code:           "InvalidParameter",
		Message:        "nope",
		HTTPStatusCode: 400,
	}
	runner.sendEventsCalls = 0

	if _, err := svc.RedirectRun(ctx, run.ID, "改做这个"); err == nil {
		t.Fatal("中断失败时转向应当返回错误")
	}
	// If even the interrupt failed, the new message must not be sent.
	if runner.sendEventsCalls != 1 {
		t.Errorf("中断失败后不该继续发消息，实际调了 %d 次", runner.sendEventsCalls)
	}
	if len(runner.sentEvents) != 0 {
		t.Errorf("中断失败时不该有任何事件送达，实际 %d 条", len(runner.sentEvents))
	}
}

// TestRedirectRunRejectsEmptyText: empty text must not cause an "interrupt only" side effect.
//
// The check is **before** the interrupt: the other way round, a user sending an empty message
// would interrupt the agent and then get a 400. That interrupt was for nothing, and cannot be undone.
func TestRedirectRunRejectsEmptyText(t *testing.T) {
	ctx := context.Background()
	svc, _, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}
	runner.sendEventsCalls = 0

	if _, err := svc.RedirectRun(ctx, run.ID, "   "); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("空文案应当回 ErrInvalidArgument，实际 %v", err)
	}
	if runner.sendEventsCalls != 0 {
		t.Errorf("空文案不该打断 agent，实际调了 %d 次方舟", runner.sendEventsCalls)
	}
}

// TestRunsMarkAgentDeleted: both GetRun and ListRuns must mark "the linked agent was deleted".
//
// It asserts **a per-agent decision**, not "delete one and everything turns red": in the seed,
// run_1 / run_4 point at agent_analyst, run_2 / run_5 at agent_triage, and run_3 at agent_reviewer.
func TestRunsMarkAgentDeleted(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	before, err := svc.GetRun(ctx, "run_4")
	if err != nil {
		t.Fatalf("查 run 失败：%v", err)
	}
	if before.AgentDeleted {
		t.Error("agent 还在时不该标记已删除")
	}

	if err := store.DeleteAgent(ctx, "agent_analyst"); err != nil {
		t.Fatalf("删 agent 失败：%v", err)
	}

	after, err := svc.GetRun(ctx, "run_4")
	if err != nil {
		t.Fatalf("查 run 失败：%v", err)
	}
	if !after.AgentDeleted {
		t.Error("agent 删掉后 GetRun 应标记已删除")
	}
	// The name must still be there; otherwise nobody knows whom the conversation belonged to,
	// which is the other half of this requirement.
	if after.AgentName != "数据分析师" {
		t.Errorf("agent 名字快照应保留，实际 %q", after.AgentName)
	}

	runs, err := svc.ListRuns(ctx, seedWS, persistmodel.RunQuery{})
	if err != nil {
		t.Fatalf("列运行失败：%v", err)
	}
	want := map[string]bool{
		"run_1": true,  // agent_analyst, deleted
		"run_2": false, // agent_triage, still exists
		"run_3": false, // agent_reviewer, still exists
		"run_4": true,  // agent_analyst, deleted
		"run_5": false, // agent_triage, still exists
	}
	seen := 0
	for _, r := range runs {
		exp, ok := want[r.ID]
		if !ok {
			continue
		}
		seen++
		if r.AgentDeleted != exp {
			t.Errorf("%s 的 AgentDeleted 应为 %v，实际 %v（agent %s）", r.ID, exp, r.AgentDeleted, r.AgentID)
		}
	}
	if seen != len(want) {
		t.Errorf("种子里那 5 条 run 应该都在列表里，实际见到 %d 条", seen)
	}
}

// TestAgentDeletedIsNeverPersisted: derived fields must never be stored.
//
// **Getting this wrong has no symptom at all**; days later it just becomes a stale snapshot: the
// run's doc stores "agent deleted", while that agent was actually recreated later with the same
// id (or the other way round). The runs table is "scalar columns + doc jsonb holding the whole
// entity", and SaveRun marshals the whole Run into doc, so as soon as someone hands an enriched
// Run to SaveRun, this value is stored.
func TestAgentDeletedIsNeverPersisted(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}
	if err := store.DeleteAgent(ctx, "agent_triage"); err != nil {
		t.Fatalf("删 agent 失败：%v", err)
	}

	enriched, err := svc.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("查 run 失败：%v", err)
	}
	if !enriched.AgentDeleted {
		t.Fatal("前提不成立：Service 层应当标记已删除")
	}
	raw, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("查 run 失败：%v", err)
	}
	if raw.AgentDeleted {
		t.Error("Store 里的 run 不该带 AgentDeleted——派生值落库就会变成陈旧快照")
	}

	// Go through a path that really calls SaveRun (interrupts are allowed on frozen
	// conversations), then check again.
	if _, err := svc.AppendRunEvents(ctx, run.ID,
		[]json.RawMessage{json.RawMessage(`{"type":"user.interrupt"}`)}); err != nil {
		t.Fatalf("中断失败：%v", err)
	}
	raw2, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("查 run 失败：%v", err)
	}
	if raw2.AgentDeleted {
		t.Error("AppendRunEvents 的 SaveRun 把派生值写进库了")
	}
}

// TestStreamStillWorksAfterAgentDeleted: freezing only blocks writes, not reads.
//
// It pins a decision that later people might "helpfully fill in": subscribing to a frozen
// conversation must work as usual. The user needs to browse history, and the receipt of the
// interrupt just allowed comes back on this very stream.
func TestStreamStillWorksAfterAgentDeleted(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}
	if err := store.DeleteAgent(ctx, "agent_triage"); err != nil {
		t.Fatalf("删 agent 失败：%v", err)
	}

	runner.frames = [][]byte{
		[]byte(`{"id":"sevt-1","type":"user.message","processed_at":null,"session_thread_id":null,"content":[{"type":"text","text":"历史"}]}`),
		[]byte(`{"id":"sevt-2","type":"session.status_idle","processed_at":null,"session_thread_id":null,"stop_reason":{"type":"user_interrupt"}}`),
	}
	var got int
	if err := svc.StreamRunEvents(ctx, run.ID, func([]byte) error {
		got++
		return nil
	}); err != nil {
		t.Fatalf("agent 已删除时订阅应当照常工作，实际 %v", err)
	}
	if got != len(runner.frames) {
		t.Errorf("应转发 %d 帧，实际 %d", len(runner.frames), got)
	}
}

// TestSendEventsRetriesThroughToolWindow: when hitting the tool execution window, the server
// retries by itself and the user notices nothing.
//
// # This guards the boundary between "(A) millisecond race" and "(B) the user deliberately queuing"
//
// The two look alike but need opposite things: here it is a race of tens of milliseconds (48ms
// observed in production); the user has no intent to "wait a bit", and one or two retries fix
// it. When the user sends a second message while the agent runs a long task, what they wait for
// is a whole round; they must see it queued and be able to cancel. That **cannot** be solved by
// sleeping here.
//
// So this case asserts two things: the retry really happened (call count > 1), and it **finally
// succeeded**. Without the second assertion, an implementation that "still errors after retrying"
// would pass too.
func TestSendEventsRetriesThroughToolWindow(t *testing.T) {
	ctx := context.Background()
	svc, _, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}

	// Hit the window twice; on the third try the window is closed. The shape is copied from the
	// production error.
	runner.sendEventsTransientErr = &ma.APIError{
		Code:           "RequiresActionRejected",
		Message:        "waiting on responses to events [call_yt9jlkp7piumx86t34i2owi8]",
		Type:           "Bad Request",
		HTTPStatusCode: 400,
	}
	runner.sendEventsFailTimes = 2
	runner.sendEventsCalls = 0

	accepted, err := svc.AppendRunEvents(ctx, run.ID, []json.RawMessage{
		json.RawMessage(`{"type":"user.message","id":"uevt-1","content":"撞窗口的那条"}`),
	})
	if err != nil {
		t.Fatalf("服务端应当重试到成功，实际报错：%v", err)
	}
	if len(accepted) != 1 {
		t.Errorf("回执应当有 1 条，实际 %d", len(accepted))
	}
	// The retry really happened. Without this, a false green where "the window was never hit"
	// would pass too.
	if runner.sendEventsCalls != 3 {
		t.Errorf("应当撞两次后第三次成功（共 3 次调用），实际 %d 次", runner.sendEventsCalls)
	}
	// The message must finally reach Ark; retries must not lose content.
	if len(runner.sentEvents) != 1 {
		t.Fatalf("最终应当有 1 条事件送达方舟，实际 %d", len(runner.sentEvents))
	}
}

// TestSendEventsDoesNotRetryOtherErrors: only that window is retried; no other error is retried
// even once.
//
// It pairs with the previous one. A parameter error does not get better however many times you
// retry; waiting for nothing just makes the user stare at a request that must fail for a whole
// budget. And **the returned error alone cannot tell them apart** (the error after 25 retries is
// exactly the same as with no retry); only the call count shows it.
func TestSendEventsDoesNotRetryOtherErrors(t *testing.T) {
	ctx := context.Background()
	svc, _, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}

	// Also a 400, but with a different code; retrying will not fix this.
	runner.sendEventsErr = &ma.APIError{
		Code:           "InvalidParameter",
		Message:        "events[0].type is not recognized",
		HTTPStatusCode: 400,
	}
	runner.sendEventsCalls = 0

	if _, err := svc.AppendRunEvents(ctx, run.ID, []json.RawMessage{
		json.RawMessage(`{"type":"user.message","id":"uevt-1","content":"你好"}`),
	}); err == nil {
		t.Fatal("参数错误应当返回错误")
	}
	if runner.sendEventsCalls != 1 {
		t.Errorf("参数错误只该打一次方舟，实际 %d 次", runner.sendEventsCalls)
	}
}

// TestAppendRunEventsRequiresActionIsConflictNot500: when Ark says "waiting for a tool result",
// the user must see a sentence they can act on, not 500 "internal server error".
//
// # This test pins a real failure
//
// Under self_hosted, tools run in our own containers. Between Ark sending tool_use and the worker
// returning `user.tool_result` there is a window in which Ark rejects user.message (HTTP 400 +
// code=RequiresActionRejected). In that production log, the tool result came back **48
// milliseconds** after the error; it was purely a timing issue from typing fast, and resending
// once fixed it.
//
// Before the fix, this error fell into the default branch of handler.fail, and became 500 +
// "internal server error, please retry later". Misleading three ways: it sounds like the service
// is down, it does not say what to do, and the 500 pollutes error-rate alerts.
func TestAppendRunEventsRequiresActionIsConflictNot500(t *testing.T) {
	ctx := context.Background()
	svc, _, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}

	// Copy the shape of the production error: HTTP 400 + Ark's own code.
	// Use a real *ma.APIError rather than errors.New, so this turns red if the SDK changes its
	// error type.
	runner.sendEventsErr = &ma.APIError{
		Code:           "RequiresActionRejected",
		Message:        "waiting on responses to events [call_yt9jlkp7piumx86t34i2owi8]",
		Type:           "Bad Request",
		HTTPStatusCode: 400,
		RequestId:      "0217880069323733560",
	}

	_, err = svc.AppendRunEvents(ctx, run.ID, []json.RawMessage{
		json.RawMessage(`{"type":"user.message","id":"uevt-1","content":"再来一条"}`),
	})
	if err == nil {
		t.Fatal("方舟拒收时应当返回错误")
	}

	// **It must be ErrConflict (409).** Falling into the default branch means 500, which is
	// exactly this failure.
	if !errors.Is(err, ErrConflict) {
		t.Errorf("应当是 ErrConflict，实际 %v", err)
	}

	// The text must tell the user "wait a moment and send again", not make them think the service
	// is broken.
	msg := userMessage(err)
	if !strings.Contains(msg, "工具") {
		t.Errorf("文案要说清是工具还在跑，实际 %q", msg)
	}
	// Never show the SDK's raw English text to the user; that is for logs.
	if strings.Contains(msg, "RequiresActionRejected") || strings.Contains(msg, "waiting on") {
		t.Errorf("不该把方舟的英文错误摊给用户，实际 %q", msg)
	}
}

// TestAppendRunEventsOtherArkErrorsStay500: once "waiting for tool result" is recognized, other
// Ark 400s must still take the service-failure path.
//
// This pairs with the previous one: checking "RequiresActionRejected is recognized" is not
// enough; we must also check that **not every 400 is treated as it**. Telling the user "wait for
// the tool to finish and resend" for "invalid event format" would make them resend again and
// again, and no number of resends would fix it.
func TestAppendRunEventsOtherArkErrorsStay500(t *testing.T) {
	ctx := context.Background()
	svc, _, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}

	// Also a 400, but Ark's code is different; retrying will not fix this.
	runner.sendEventsErr = &ma.APIError{
		Code:           "InvalidParameter",
		Message:        "events[0].type is not recognized",
		Type:           "Bad Request",
		HTTPStatusCode: 400,
	}

	_, err = svc.AppendRunEvents(ctx, run.ID, []json.RawMessage{
		json.RawMessage(`{"type":"user.message","id":"uevt-1","content":"你好"}`),
	})
	if err == nil {
		t.Fatal("方舟拒收时应当返回错误")
	}
	if errors.Is(err, ErrConflict) {
		t.Error("参数错误不该被当成「等工具跑完」——用户会一直重发，而重发不会好")
	}
	if errors.Is(err, ErrPrecondition) {
		t.Error("参数错误不该被当成鉴权失败")
	}
}

// TestConcurrentSubscribersDoNotDoubleCountUsage: when two subscribers receive the same usage
// event, tokens may be counted only once.
//
// # This is the half that TestConcurrentSubscribersDoNotDuplicateEvents missed
//
// That one pins "only one row in the DB" (via AppendRunEvent's unique index on event_id), but
// **before the fix, applyEventToRun in persistArkEvent did not look at created**:
//
//	ev, created, _ := s.store.AppendRunEvent(...)
//	s.applyEventToRun(...)              ← duplicate events got here too (missing gate)
//	if created { s.reportEvent(...) }   ← this one checked from the start
//
// And addUsage in applyEventToRun accumulates with `+=`. So when the same
// span.model_request_end was delivered once by each of two subscribers, tokens doubled: **the
// row count is right, the numbers are wrong**, and that existing case cannot catch it at all.
//
// Reporting (reportEvent) thought of this back then and added the created gate; usage missed it.
// It affects the usage dashboard and billing numbers, with zero errors.
//
// ⚠️ After "server-side resident subscriptions" ship, this goes from occasional to certain: every
// conversation will then have at least one resident subscriber, and a user opening the chat page
// is the second.
func TestConcurrentSubscribersDoNotDoubleCountUsage(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	run, err := svc.CreateConversation(ctx, seedWS, "agent_triage")
	if err != nil {
		t.Fatalf("建对话失败：%v", err)
	}

	// One event carrying model_usage. Both subscribers receive it.
	runner.frames = [][]byte{
		[]byte(`{"id":"sevt-u1","type":"span.model_request_end","model_usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":5,"cache_creation_input_tokens":3}}`),
		[]byte(`{"id":"sevt-end","type":"session.status_idle","stop_reason":{"type":"end_turn"}}`),
	}

	/*
	 * **Subscribe twice serially, not concurrently.**
	 *
	 * The concurrent version is flaky (passed 2 of 6 runs in practice): the order of the two
	 * streams is not fixed. The fast one may finish before the slow one starts, so the slow one
	 * hits `hasActiveStream == false` at the StreamRunEvents entry and does a SyncRunEvents
	 * first. That path has its own in-memory seenIDs dedup, which happens to block the double
	 * usage count.
	 *
	 * Serially, every run is sure to replay the same batch of frames and reach applyEventToRun
	 * twice, which is also the real shape of "resident subscriber + user opens the chat page"
	 * (one after the other, not at the same time).
	 * The check must reproduce reliably, not rely on lucky scheduling.
	 */
	for i := 0; i < 2; i++ {
		if err := svc.StreamRunEvents(ctx, run.ID, func([]byte) error { return nil }); err != nil {
			t.Fatalf("第 %d 次订阅出错：%v", i+1, err)
		}
	}

	after, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("读 run 失败：%v", err)
	}
	// Assert all four fields: checking only one would let an implementation that "dedups only some
	// field" slip through.
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"input_tokens", after.Usage.InputTokens, 100},
		{"output_tokens", after.Usage.OutputTokens, 20},
		{"cache_read_input_tokens", after.Usage.CacheReadInputTokens, 5},
		{"cache_creation_input_tokens", after.Usage.CacheCreationInputTokens, 3},
	} {
		if tc.got != tc.want {
			t.Errorf("%s 应为 %d，实际 %d —— 同一条事件被两个订阅者各算了一次，"+
				"用量看板与计费口径会虚高", tc.name, tc.want, tc.got)
		}
	}
}
