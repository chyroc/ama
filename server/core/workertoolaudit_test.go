// Tests for tool call reconciliation: every tool_use Ark started must get a result sent back.
//
// # What these tests guard against
//
// Production run sesn-20260101000007-ex007: of 87 events, `agent.tool_use` had 15 and
// `user.tool_result` only 12; the three missing were exactly the three bash calls sent concurrently at the same last instant
// (seq 82/83/84). Their call ids appear in the `stop_reason.event_ids` of the last two idle events,
// in the shape `requires_action`. That turn stopped forever,
// and 30 minutes later reapStaleRuns marked it terminated; the user saw "it stopped halfway through".
//
// ⚠️ **Concurrency itself is not the root cause, and there is a counterexample**: in the same run, three earlier concurrent batches
// (2, 5 and 5 calls) were all answered normally. The real cause is that the runner had stopped listening before those three tool_use
// were consumed, and **nothing anywhere posts a tool_result to fill the gap**.
// So what is tested here is not "is concurrency right" but "does someone clean up when things do not add up".
package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/volcengine/ark-runtime-go/arkruntime/selfhosted"
)

// auditAPI is a selfhosted.API stand-in for reconciliation: lay out an event history by hand,
// and record every result SendEvent receives.
//
// **fakeWorkAPI is not reused**: its ListEvents always returns empty and SendEvent always succeeds,
// so testing reconciliation with it would test a world that never has orphans, always green.
type auditAPI struct {
	fakeWorkAPI

	mu sync.Mutex
	// events is this session's event history, laid out in Ark's order (ascending).
	events []selfhosted.Event
	// sent records every result event sent back.
	sent []selfhosted.Event
	// sendErrs injects errors by call order: the i-th SendEvent uses sendErrs[i] (out of range means nil).
	sendErrs []error
	sendN    int
	// When pageSize > 0, events are returned in pages, to check the paging logic misses no events.
	pageSize int
	// listErr makes ListEvents fail outright.
	listErr error
}

func (a *auditAPI) ListEvents(
	_ context.Context, req selfhosted.ListEventsRequest,
) (*selfhosted.ListEventsResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.listErr != nil {
		return nil, a.listErr
	}
	// No paging: give everything at once.
	if a.pageSize <= 0 {
		return &selfhosted.ListEventsResponse{
			Events: append([]selfhosted.Event(nil), a.events...),
		}, nil
	}
	// Paging: page is the start index of the next page (as a string); an empty string means the first page.
	start := 0
	if req.Page != "" {
		if _, err := fmt.Sscanf(req.Page, "%d", &start); err != nil {
			return nil, fmt.Errorf("坏的 page 游标 %q", req.Page)
		}
	}
	if start > len(a.events) {
		start = len(a.events)
	}
	end := min(start+a.pageSize, len(a.events))
	next := ""
	if end < len(a.events) {
		next = fmt.Sprintf("%d", end)
	}
	return &selfhosted.ListEventsResponse{
		Events:   append([]selfhosted.Event(nil), a.events[start:end]...),
		NextPage: next,
	}, nil
}

func (a *auditAPI) SendEvent(_ context.Context, req selfhosted.SendEventRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var err error
	if a.sendN < len(a.sendErrs) {
		err = a.sendErrs[a.sendN]
	}
	a.sendN++
	if err != nil {
		return err
	}
	a.sent = append(a.sent, req.Event)
	// On successful send, merge it into the history; real Ark does this, so the next ListEvents sees it.
	// Without merging, "reconciling again after posting" would post again, and that bug could not be detected.
	a.events = append(a.events, req.Event)
	return nil
}

// sentIDs returns the set of call ids the posted results point to.
func (a *auditAPI) sentIDs() map[string]bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]bool{}
	for _, ev := range a.sent {
		if id := toolResultCallID(ev); id != "" {
			out[id] = true
		}
	}
	return out
}

// toolUseEvent builds one agent.tool_use.
//
// ⚠️ **The event's own id and the call id are deliberately different values** (`evtu-` prefix vs `call_`).
//
// They used to be the same value, which made "using the bare `ev.ToolUseID`" and "using `toolCallID(ev)`"
// **completely equivalent in tests**, so the bare-field bug in orphanToolResult hid under all-green
// tests (see TestOrphanToolResultFallsBackToEventID). This kind of fixture
// is a shape this repo keeps hitting: **setting two fields to the same value erases the very divergence you want to test.**
func toolUseEvent(callID, name string) selfhosted.Event {
	return selfhosted.Event{
		ID:              "evtu-" + callID,
		Type:            selfhosted.EventTypeAgentToolUse,
		Name:            name,
		ToolUseID:       callID,
		SessionThreadID: "sthr-test",
	}
}

// toolResultEvent builds one user.tool_result.
func toolResultEvent(callID string) selfhosted.Event {
	return selfhosted.Event{
		ID:              "evt-" + callID,
		Type:            selfhosted.EventTypeUserToolResult,
		ToolUseID:       callID,
		SessionThreadID: "sthr-test",
	}
}

// TestAuditPostsResultForEveryParallelToolUse is the **main test** of this change:
// 3 tool_use started concurrently in one turn, none answered → all three must be filled in.
//
// The event history copies the tail shape of that production run directly (three bash sent at the same instant, no
// tool_result after), so if this test goes red, that incident would happen again.
//
// ⚠️ Mutation check: deleting the auditToolCalls block in pollLoop, or changing
// auditToolCalls to return 0 directly, must turn this red.
func TestAuditPostsResultForEveryParallelToolUse(t *testing.T) {
	api := &auditAPI{events: []selfhosted.Event{
		// One normally paired call first, to confirm reconciliation does not post again for one already answered.
		toolUseEvent("call_ok", "read"),
		toolResultEvent("call_ok"),
		// The three concurrent bash calls from production: sent, with no result at all.
		toolUseEvent("call_a", "bash"),
		toolUseEvent("call_b", "bash"),
		toolUseEvent("call_c", "bash"),
	}}

	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 4}, log: testLogger()}
	installFakeProcessor(pool)
	posted := auditToolCalls(context.Background(), api, "sesn-test", testLogger())

	if posted != 3 {
		t.Fatalf("补发了 %d 条，要 3 条——\n"+
			"方舟侧那一轮要等**全部** call id 都有结果才继续，少一条就永久停在 "+
			"requires_action（线上 sesn-20260101000007-ex007 正是如此：15 个 tool_use "+
			"只有 12 个 tool_result，30 分钟后被兜底回收成 terminated）", posted)
	}

	got := api.sentIDs()
	for _, want := range []string{"call_a", "call_b", "call_c"} {
		if !got[want] {
			t.Errorf("并发发起的 %s 没有被补上结果，那一轮仍会卡死；实际补了 %v", want, got)
		}
	}
	// The one that already has a result must not be posted again: a duplicate delivery would give Ark two results,
	// and the agent would see two different answers for the same call.
	if got["call_ok"] {
		t.Error("已经回传过结果的 call_ok 被重复补发了")
	}
}

// TestAuditPostsErrorResultNotSilentDrop is the pair of the test above:
// **what is posted must be a clear error result, not a silent drop, and not a fake success result.**
//
// It exists separately because "how many were posted" cannot tell whether the content is right. A fake success is worse than not posting:
// the agent would think the command ran and keep reasoning on (for example, think the file was already written).
func TestAuditPostsErrorResultNotSilentDrop(t *testing.T) {
	api := &auditAPI{events: []selfhosted.Event{toolUseEvent("call_x", "bash")}}

	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 1}, log: testLogger()}
	installFakeProcessor(pool)
	if posted := auditToolCalls(context.Background(), api, "sesn-test", testLogger()); posted != 1 {
		t.Fatalf("补发 %d 条，要 1 条", posted)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.sent) != 1 {
		t.Fatalf("回传了 %d 条事件，要 1 条", len(api.sent))
	}
	ev := api.sent[0]

	if ev.Type != selfhosted.EventTypeUserToolResult {
		t.Errorf("补发的事件类型是 %q，要 %q", ev.Type, selfhosted.EventTypeUserToolResult)
	}
	if ev.ToolUseID != "call_x" {
		t.Errorf("补发的结果指向 %q，要 call_x", ev.ToolUseID)
	}
	// **is_error must be true.** A fake success result would make the agent think the tool ran.
	if ev.IsError == nil || !*ev.IsError {
		t.Errorf("补发的结果 is_error = %v，必须是 true——"+
			"否则 agent 会以为这条命令真的执行成功了，然后基于一个没发生的结果继续推理", ev.IsError)
	}
	// The body must let the model understand "it did not run, it is not an argument problem, it can retry".
	if len(ev.Content) == 0 || !strings.Contains(ev.Content[0].Text, "没有被执行") {
		t.Errorf("补发的正文没说清「没被执行」：%+v", ev.Content)
	}
}

// TestAuditKeepsGoingWhenOneSendFails checks that one failed post does not drag down the others.
//
// Ark wants results for **all** call ids; with one missing it stays stuck. So when one in the middle fails,
// the rest must still be posted; an early return would turn "the first of three failed" into none posted at all.
func TestAuditKeepsGoingWhenOneSendFails(t *testing.T) {
	api := &auditAPI{
		events: []selfhosted.Event{
			toolUseEvent("call_a", "bash"),
			toolUseEvent("call_b", "bash"),
			toolUseEvent("call_c", "bash"),
		},
		// The first fails, the last two succeed.
		sendErrs: []error{errors.New("网络抖了")},
	}

	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 1}, log: testLogger()}
	installFakeProcessor(pool)
	posted := auditToolCalls(context.Background(), api, "sesn-test", testLogger())

	if posted != 2 {
		t.Fatalf("补发成功 %d 条，要 2 条（第一条注入了失败）——"+
			"一条失败就 return 会让剩下的也补不上，那一轮照样卡死", posted)
	}
	got := api.sentIDs()
	if !got["call_b"] || !got["call_c"] {
		t.Errorf("第一条失败后没有继续补后两条，实际 %v", got)
	}
}

// TestAuditHandlesCustomToolUse checks that custom tools use the right event type.
//
// The result event of a custom tool is `user.custom_tool_result` + `custom_tool_use_id`,
// a different set from normal tools. With the wrong type Ark does not accept it and the turn stays stuck, **while the log says "posted"**,
// so whoever debugs would rule this path out.
func TestAuditHandlesCustomToolUse(t *testing.T) {
	custom := selfhosted.Event{
		// ⚠️ The event id and call id take different values, for the same reason as the comment on toolUseEvent:
		// setting them to the same value would make the bare `ev.CustomToolUseID` and `toolCallID(ev)` indistinguishable.
		ID:              "evtu-call_wake",
		Type:            selfhosted.EventTypeAgentCustomToolUse,
		Name:            "some_custom_tool",
		CustomToolUseID: "call_wake",
		SessionThreadID: "sthr-test",
	}
	api := &auditAPI{events: []selfhosted.Event{custom}}

	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 1}, log: testLogger()}
	installFakeProcessor(pool)
	if posted := auditToolCalls(context.Background(), api, "sesn-test", testLogger()); posted != 1 {
		t.Fatalf("补发 %d 条，要 1 条", posted)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	ev := api.sent[0]
	if ev.Type != selfhosted.EventTypeUserCustomToolResult {
		t.Errorf("自定义工具的结果类型是 %q，要 %q——"+
			"用普通 user.tool_result 方舟不认，那一轮照样卡死",
			ev.Type, selfhosted.EventTypeUserCustomToolResult)
	}
	if ev.CustomToolUseID != "call_wake" {
		t.Errorf("custom_tool_use_id = %q，要 call_wake", ev.CustomToolUseID)
	}
}

// TestOrphanToolResultFallsBackToEventID pins "building the posted event uses toolCallID,
// not the bare field". **This was a real bug that hid under all-green tests at the time.**
//
// # What it guards against
//
// Orphan detection (`:200`), logging (`:144`) and the digest (`:274`) all use the three-way fallback
// `toolCallID(ev)`, while `orphanToolResult` alone once used the bare `ev.ToolUseID` /
// `ev.CustomToolUseID`. Both branches of the SDK's postResult pass `toolUseCallID(event)`
// (`session_tool_runner.go:858-864`), so the bare field was a deviation only on our side.
//
// The chain of consequences has **zero errors from start to end**: in sessions where `tool_use_id` is not sent, `ev.ToolUseID == ""`,
// so detection and logging work by falling back to `ev.ID` (**the logged call id is right and everything looks normal**),
// but that empty string meets `omitempty` when building the event → **the key disappears from the JSON entirely** → SendEvent
// does not validate, the server returns 200 → `posted++` → logs "tool result posted, Ark's turn can go on",
// while Ark gets a tool_result pointing at no call, and **that turn stays stuck in requires_action**.
// The log says posting succeeded, so whoever debugs rules this whole path out.
//
// # Why ToolUseID must be empty
//
// `toolCallID` prefers `ToolUseID`, so only changing the fixture's `ID` and `ToolUseID` to
// different values **cannot detect this bug**: with that fixture both ways get the same value.
// Only on the "fall back to `ev.ID`" branch do the two diverge, and that is exactly the shape that happens in production.
//
// ⚠️ Mutation check: changing `callID` in orphanToolResult back to `ev.ToolUseID` /
// `ev.CustomToolUseID` must turn both subtests of this red.
func TestOrphanToolResultFallsBackToEventID(t *testing.T) {
	cases := []struct {
		name    string
		ev      selfhosted.Event
		wantTyp string
		// idOf gets the field in the posted event that says "which call it points to".
		idOf func(selfhosted.Event) string
	}{
		{
			name: "普通工具：tool_use_id 没下发，回落到事件 id",
			ev: selfhosted.Event{
				ID:              "call_from_event_id",
				Type:            selfhosted.EventTypeAgentToolUse,
				Name:            "bash",
				SessionThreadID: "sthr-test",
				// ToolUseID deliberately left empty: production really has sessions that do not send it.
			},
			wantTyp: string(selfhosted.EventTypeUserToolResult),
			idOf:    func(ev selfhosted.Event) string { return ev.ToolUseID },
		},
		{
			name: "自定义工具：custom_tool_use_id 没下发，回落到事件 id",
			ev: selfhosted.Event{
				ID:              "call_wake_from_event_id",
				Type:            selfhosted.EventTypeAgentCustomToolUse,
				Name:            "some_custom_tool",
				SessionThreadID: "sthr-test",
			},
			wantTyp: string(selfhosted.EventTypeUserCustomToolResult),
			idOf:    func(ev selfhosted.Event) string { return ev.CustomToolUseID },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := orphanToolResult(tc.ev)

			if string(out.Type) != tc.wantTyp {
				t.Fatalf("补发的事件类型是 %q，要 %q", out.Type, tc.wantTyp)
			}
			got := tc.idOf(out)
			if got == "" {
				t.Fatalf("补发的结果没有指向任何 call id（字段是空串）——\n" +
					"这个键带 omitempty，空串会让它**整个从 JSON 里消失**，" +
					"于是方舟收到一条不指向任何调用的 tool_result，那一轮照样卡在 " +
					"requires_action；而 SendEvent 回 200、日志打出「已补发工具结果」，" +
					"排查的人会把这条路径排除掉。\n" +
					"修法：orphanToolResult 用 toolCallID(ev)，与 SDK 的 " +
					"toolUseCallID 同口径（判定与日志一直用的都是它）")
			}
			if got != tc.ev.ID {
				t.Errorf("补发的结果指向 %q，要回落到事件 id %q——"+
					"与判定孤儿时用的 toolCallID 分叉了，两边对「这是哪条调用」"+
					"给出不同答案", got, tc.ev.ID)
			}
			// Assert once directly that it agrees with toolCallID, so they do not diverge again next time.
			if got != toolCallID(tc.ev) {
				t.Errorf("补发结果指向 %q，而 toolCallID 判出的是 %q，两者必须一致",
					got, toolCallID(tc.ev))
			}
		})
	}
}

// TestAuditPostsResultCarryingCallIDWhenToolUseIDMissing is the **end-to-end version** of the test above:
// it runs the full auditToolCalls and confirms the orphan that "falls back to the event id" really gets filled in.
//
// The reason it exists separately is the same as the "wiring" tests in this file: the test above only checks the pure function
// orphanToolResult and cannot prove the whole reconciliation chain lines up with this event shape. The check here is
// **the second reconciliation run posts nothing more**, which requires the posted result to be recognized by `toolResultCallID`
// as the same call id, that is, "the key sent out" and "the key used for detection" really are one.
func TestAuditPostsResultCarryingCallIDWhenToolUseIDMissing(t *testing.T) {
	// The shape where tool_use_id is not sent: only the event's own id.
	orphan := selfhosted.Event{
		ID:              "call_no_tool_use_id",
		Type:            selfhosted.EventTypeAgentToolUse,
		Name:            "bash",
		SessionThreadID: "sthr-test",
	}
	api := &auditAPI{events: []selfhosted.Event{orphan}}
	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 1}, log: testLogger()}
	installFakeProcessor(pool)

	if posted := auditToolCalls(context.Background(), api, "sesn-test", testLogger()); posted != 1 {
		t.Fatalf("补发 %d 条，要 1 条", posted)
	}
	if !api.sentIDs()["call_no_tool_use_id"] {
		t.Fatalf("补发的结果没指向 call_no_tool_use_id，实际 %v——\n"+
			"构造事件时用了裸 ev.ToolUseID（空串，被 omitempty 抹掉），"+
			"而判定孤儿用的是回落到 ev.ID 的 toolCallID", api.sentIDs())
	}

	// Reconcile once more: the posted one must be recognized as "answered", otherwise every work round would post again.
	if second := auditToolCalls(context.Background(), api, "sesn-test", testLogger()); second != 0 {
		t.Errorf("第二次又补了 %d 条——补发的结果没被算成「已回传」，"+
			"说明发出去的键与判定用的键不是一个，同一个 call id 会积累多条结果事件", second)
	}
}

// TestAuditIsIdempotent checks that running reconciliation twice does not post twice.
//
// This will definitely happen in production: the same session is claimed by several work items in turn (one work per conversation turn),
// and each reconciles once when wrapping up. Posting twice would give the same call id two result events.
func TestAuditIsIdempotent(t *testing.T) {
	api := &auditAPI{events: []selfhosted.Event{toolUseEvent("call_a", "bash")}}
	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 1}, log: testLogger()}
	installFakeProcessor(pool)

	first := auditToolCalls(context.Background(), api, "sesn-test", testLogger())
	second := auditToolCalls(context.Background(), api, "sesn-test", testLogger())

	if first != 1 {
		t.Fatalf("第一次要补 1 条，实际 %d", first)
	}
	if second != 0 {
		t.Errorf("第二次又补了 %d 条——补发的结果没被算成「已回传」，"+
			"同一个 call id 会积累多条结果事件", second)
	}
}

// TestAuditNoOpWhenAllAnswered checks that nothing is done in the normal case.
//
// This is the guard for "the fallback must not disturb the normal path": when everything pairs up, not one SendEvent should be sent.
func TestAuditNoOpWhenAllAnswered(t *testing.T) {
	api := &auditAPI{events: []selfhosted.Event{
		toolUseEvent("call_a", "bash"), toolResultEvent("call_a"),
		toolUseEvent("call_b", "read"), toolResultEvent("call_b"),
	}}

	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 1}, log: testLogger()}
	installFakeProcessor(pool)
	if posted := auditToolCalls(context.Background(), api, "sesn-test", testLogger()); posted != 0 {
		t.Fatalf("全部已回传时补发了 %d 条，应该一条都不补", posted)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.sent) != 0 {
		t.Errorf("全部已回传时发出了 %d 条事件", len(api.sent))
	}
}

// TestAuditReadsAllPages checks that all pages are fetched.
//
// Missing a page has **opposite** effects: if the unread page has a tool_result, reconciliation would treat an answered call
// as an orphan and post again; if it has a tool_use, a real orphan would be missed. Both are wrong, and both give zero errors.
func TestAuditReadsAllPages(t *testing.T) {
	// 12 events, 5 per page → 3 pages. The orphan is put on the last page on purpose.
	events := []selfhosted.Event{}
	for i := range 5 {
		id := fmt.Sprintf("call_p%d", i)
		events = append(events, toolUseEvent(id, "bash"), toolResultEvent(id))
	}
	events = append(events, toolUseEvent("call_orphan", "bash"))
	api := &auditAPI{events: events, pageSize: 5}

	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 1}, log: testLogger()}
	installFakeProcessor(pool)
	posted := auditToolCalls(context.Background(), api, "sesn-test", testLogger())

	if posted != 1 {
		t.Fatalf("补发 %d 条，要 1 条——分页没读全的话，"+
			"要么漏掉真孤儿，要么把已回传的当孤儿重复补发", posted)
	}
	if !api.sentIDs()["call_orphan"] {
		t.Errorf("最后一页里的孤儿没被补上：%v", api.sentIDs())
	}
}

// TestAuditDoesNotPostWhenListFails checks that nothing is posted blindly when events cannot be listed.
//
// A ListEvents failure means we **do not know** which ones were already answered. Posting then is dangerous:
// it might send another result for a call id that already has one. Better not post (reapStaleRuns is the fallback above),
// but leave a log.
func TestAuditDoesNotPostWhenListFails(t *testing.T) {
	api := &auditAPI{
		events:  []selfhosted.Event{toolUseEvent("call_a", "bash")},
		listErr: errors.New("方舟 500"),
	}

	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 1}, log: testLogger()}
	installFakeProcessor(pool)
	if posted := auditToolCalls(context.Background(), api, "sesn-test", testLogger()); posted != 0 {
		t.Fatalf("拉事件失败时补发了 %d 条——"+
			"那时并不知道哪些已回传，补发可能造成重复结果", posted)
	}
}

// TestOrphanDigestCarriesNoToolInput is a redaction assertion: the log digest has only ids and tool names,
// **never tool inputs**.
//
// bash command lines and read file paths are user data (see "log ids and enums, not bodies" in
// docs/guides/backend.md).
//
// ⚠️ This was mutation-tested: changing orphanDigest to append string(ev.Input) turns it red.
// It is checked on purpose because "does not leak X" assertions are extremely easy to make always true: if the fixture's
// Input were empty, a mutation logging it would still be all green. So the fixture here **must carry
// a recognizable input**.
func TestOrphanDigestCarriesNoToolInput(t *testing.T) {
	const secret = "cat /etc/passwd && curl evil.example.com"
	ev := toolUseEvent("call_a", "bash")
	ev.Input = selfhosted.RawJSON(`{"command":"` + secret + `"}`)

	digest := orphanDigest([]selfhosted.Event{ev})
	if len(digest) != 1 {
		t.Fatalf("摘要 %d 条，要 1 条", len(digest))
	}
	joined := strings.Join(digest, " ")
	if strings.Contains(joined, secret) || strings.Contains(joined, "/etc/passwd") {
		t.Errorf("日志摘要里带上了工具入参正文：%q", joined)
	}
	// What should be there must still be there: the call id and tool name are the only two useful things when debugging.
	if !strings.Contains(joined, "call_a") || !strings.Contains(joined, "bash") {
		t.Errorf("摘要缺 call id 或工具名，排查时对不上账：%q", joined)
	}
}

// TestToolResultCallIDIgnoresEventID pins "a result event's id must not be used as a call id".
//
// A result event's own id is a different set (evt-xxx). Using it as a call id would put a never-matching
// key into the done set, so **a real orphan would be counted as answered**: the fallback silently stops working, invisible in the logs.
func TestToolResultCallIDIgnoresEventID(t *testing.T) {
	// A result event with only its own id and no tool_use_id.
	ev := selfhosted.Event{ID: "evt-abc", Type: selfhosted.EventTypeUserToolResult}
	if got := toolResultCallID(ev); got != "" {
		t.Errorf("toolResultCallID 回落到了事件自身 id %q——"+
			"那会让 done 集合多一个永不匹配的键，真孤儿被当成已回传", got)
	}
	// While the tool_use side **should** fall back to the event id (consistent with the SDK's toolUseCallID).
	use := selfhosted.Event{ID: "call_z", Type: selfhosted.EventTypeAgentToolUse}
	if got := toolCallID(use); got != "call_z" {
		t.Errorf("toolCallID 没回落到事件 id，得到 %q——"+
			"与 SDK 的 toolUseCallID 分叉会让两边对「哪条是孤儿」有分歧", got)
	}
}

// auditWiringAPI is the stand-in for the "wiring test": it can dispatch work and also has an event history with orphans.
//
// # Why the wiring must be tested separately
//
// All the tests above call auditToolCalls directly; **they cannot prove pollLoop really calls it**.
// This repo paid for that twice (once with workdir, once with shutdownSafeAPI):
// **both sides right on their own, the wiring wrong, and nobody watching in between**. So this goes through the real pollLoop + the real
// EnvironmentWorker, and only asserts "after that work wraps up, the orphans are filled in".
type auditWiringAPI struct {
	auditAPI

	mu    sync.Mutex
	queue []selfhosted.WorkItem
	acked []string
}

func (a *auditWiringAPI) PollWork(
	ctx context.Context, _ selfhosted.PollWorkRequest,
) (*selfhosted.WorkItem, error) {
	a.mu.Lock()
	if len(a.queue) > 0 {
		item := a.queue[0]
		a.queue = a.queue[1:]
		a.mu.Unlock()
		return &item, nil
	}
	a.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (a *auditWiringAPI) AckWork(_ context.Context, req selfhosted.AckWorkRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.acked = append(a.acked, req.WorkID)
	return nil
}

// GetSession returns an error so HandleItem wraps up quickly, simulating "the runner exited before consuming those
// tool_use events", exactly the shape of that production incident.
//
// ⚠️ **Wait for gate first** (if set). Forgetting this step means the "process exit" test never tests the
// exit path at all: the work finishes before we cancel, reconciliation happens as usual, and the test fails claiming "the fallback bypassed
// the guard", while the real cause is that the stand-in did not hold it. This was actually hit once.
func (a *auditWiringAPI) GetSession(
	ctx context.Context, _ selfhosted.GetSessionRequest,
) (*selfhosted.Session, error) {
	if gate := a.auditAPI.fakeWorkAPI.getSessionGate; gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, errors.New("fake: runner 立刻收尾")
}

// TestPollLoopAuditsToolCallsAfterWork is **the only test that can stop that production incident**.
//
// It does not test some function's behavior but **whether the wire is connected**: after a work item is handled,
// pollLoop must reconcile and fill in the orphans. Deleting the auditToolCalls block in pollLoop must turn it red.
func TestPollLoopAuditsToolCallsAfterWork(t *testing.T) {
	api := &auditWiringAPI{
		auditAPI: auditAPI{events: []selfhosted.Event{
			toolUseEvent("call_a", "bash"),
			toolUseEvent("call_b", "bash"),
			toolUseEvent("call_c", "bash"),
		}},
		queue: []selfhosted.WorkItem{workItem("work_1", "sesn-20260101000007-ex007")},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 4}, log: testLogger()}
	installFakeProcessor(pool)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = pool.pollLoop(ctx, api, "env_test", "worker_test", 4, testLogger())
	}()

	waitFor(t, 5*time.Second, func() bool {
		return len(api.sentIDs()) == 3
	}, "三条孤儿工具调用都被补上结果")

	cancel()
	<-done

	got := api.sentIDs()
	for _, want := range []string{"call_a", "call_b", "call_c"} {
		if !got[want] {
			t.Errorf("work 收尾后 %s 仍没有结果——pollLoop 没有调对账，"+
				"那一轮会永久停在 requires_action（线上 sesn-20260101000007-ex007）", want)
		}
	}
}

// TestPollLoopSkipsAuditOnShutdown is the **counterexample** of the test above, keeping the fallback from bypassing the guard.
//
// When the process exits, that work is **deliberately left for a new instance to reclaim** (see the comment on shutdownSafeAPI
// and TestPollLoopDoesNotStopWorkOnShutdown). Posting results then would push the turn forward before the reclaim:
// the tool did not actually run, yet the agent gets "could not run", turning a restart that could
// heal itself into a real failed turn.
//
// Removing that `if ctx.Err() == nil` check in pollLoop must turn this red.
func TestPollLoopSkipsAuditOnShutdown(t *testing.T) {
	gate := make(chan struct{})
	api := &auditWiringAPI{
		auditAPI: auditAPI{events: []selfhosted.Event{
			toolUseEvent("call_a", "bash"),
		}},
		queue: []selfhosted.WorkItem{workItem("work_1", "sesn-shutdown")},
	}
	// Use fakeWorkAPI's gate to hold the work on GetSession, so we can cancel ctx while it is
	// in the middle of processing, which is exactly the shape of "process exit".
	api.auditAPI.fakeWorkAPI.getSessionGate = gate

	ctx, cancel := context.WithCancel(context.Background())
	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 4}, log: testLogger()}
	installFakeProcessor(pool)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = pool.pollLoop(ctx, api, "env_test", "worker_test", 4, testLogger())
	}()

	waitFor(t, 3*time.Second, func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		return len(api.acked) == 1
	}, "work 已被 ack 并开始处理")

	// Simulate process exit: cancel first, then release so the SDK's defer and our wrap-up both run.
	cancel()
	close(gate)
	<-done

	if n := len(api.sentIDs()); n != 0 {
		t.Fatalf("进程退出途中补发了 %d 条工具结果——\n"+
			"那条 work 本该留给新实例 reclaim（工具会真的被执行一次），"+
			"补发会抢在 reclaim 之前把那一轮推进成「没能执行」，"+
			"把一次可自愈的重启变成一轮真的失败。护栏见 core.shutdownSafeAPI", n)
	}
}

// stuckCursorAPI is a stand-in where "the server cursor does not advance": it always returns the same NextPage.
//
// Real Ark should not do this, but **the point of a stand-in is to make "should not happen" testable**.
type stuckCursorAPI struct {
	auditAPI

	calls atomic.Int32
}

func (a *stuckCursorAPI) ListEvents(
	_ context.Context, _ selfhosted.ListEventsRequest,
) (*selfhosted.ListEventsResponse, error) {
	a.calls.Add(1)
	return &selfhosted.ListEventsResponse{
		Events:   []selfhosted.Event{toolUseEvent("call_a", "bash")},
		NextPage: "永远是我", // the cursor does not advance
	}, nil
}

// TestAuditStopsOnStuckCursor blocks the infinite paging loop.
//
// # Why it deserves a test
//
// The first version of paging was `for { ... page = resp.NextPage }` with no upper bound. When the server returns the same
// cursor it loops forever, and this runs in the work goroutine, **holding a concurrency slot**,
// showing up as "this environment's concurrency silently drops by one", with no error and nothing pointing here. Found during self-review.
//
// The check is "it returns", not "how many pages it read": the latter would pin implementation details, while we only care that it does not hang.
// It uses its own goroutine + timeout apart from t.Deadline, so an infinite loop does not drag the whole test process into a timeout
// (which would report "test timed out", not pointing at this test).
//
// ⚠️ Mutation check: removing the `resp.NextPage == page` check together with the toolAuditMaxPages bound
// (back to `for {}`) makes this fail with a timeout.
func TestAuditStopsOnStuckCursor(t *testing.T) {
	api := &stuckCursorAPI{}
	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 1}, log: testLogger()}
	installFakeProcessor(pool)

	done := make(chan int, 1)
	go func() {
		done <- auditToolCalls(context.Background(), api, "sesn-test", testLogger())
	}()

	select {
	case <-done:
		// Returning is a pass; how many were posted does not matter, what matters is that it did not hang.
	case <-time.After(10 * time.Second):
		t.Fatalf("对账在「服务端游标不前进」时挂住了（已调 ListEvents %d 次）——\n"+
			"这段跑在 work 的 goroutine 里、占着一个并发槽位，死循环的表现是"+
			"「这个环境的并发度悄悄少一格」，不报错也不指向这里。\n"+
			"闸门：toolAuditMaxPages 上界 + 「游标必须变化」判断", api.calls.Load())
	}
}

// TestAuditSurvivesPerWorkCtxCancellation guards against "a per-work ctx shadowing workerCtx"
// silently turning the fallback into dead code.
//
// # Why test this separately
//
// Each work goroutine in pollLoop may well get a per-work ctx (for example one derived just for
// HandleItem, to cut off its own SendEvent retries after the work wraps up).
// **Such a ctx is also canceled when it ends normally**, so once the check is written with a bare `ctx`:
//
//	if ctx.Err() == nil { post orphans }   ← always false, the fallback becomes dead code
//
// And this gives **zero errors**: not a single "tool call returned no result" log line appears, which looks just like
// "production is healthy and the root cause is fixed". The existing TestPollLoopSkipsAuditOnShutdown cannot stop it
// either: what that one expects is exactly "no posting".
//
// This test separates the two: **when work ends normally (even if some derived ctx is canceled) reconciliation must run as usual**,
// and it is skipped only when the worker's own ctx is canceled (process exit).
//
// ⚠️ Mutation check: changing `workerCtx.Err()` in pollLoop back to `ctx.Err()`, and adding a
// `ctx, cancel := context.WithCancel(ctx); defer cancel()` in that goroutine
// (the shape of the neighboring change), must turn this red.
func TestAuditSurvivesPerWorkCtxCancellation(t *testing.T) {
	api := &auditWiringAPI{
		auditAPI: auditAPI{events: []selfhosted.Event{
			toolUseEvent("call_a", "bash"),
		}},
		queue: []selfhosted.WorkItem{workItem("work_1", "sesn-perwork")},
	}

	// The worker's own ctx is **not canceled**: this is "work ended normally", not "process exit".
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := &WorkerPool{cfg: WorkerConfig{Concurrency: 4}, log: testLogger()}
	installFakeProcessor(pool)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = pool.pollLoop(ctx, api, "env_test", "worker_test", 4, testLogger())
	}()

	// HandleItem wraps up right away because GetSession errors (the internally derived workCtx is canceled with it),
	// but reconciliation must still happen.
	waitFor(t, 5*time.Second, func() bool {
		return api.sentIDs()["call_a"]
	}, "work 正常结束后孤儿被补上")

	cancel()
	<-done
}
