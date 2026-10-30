// Pins "one turn = one reply" with the event sequence of **a real production session**.
//
// # Why the fixture must be a real sequence
//
// The three bugs on this path were not logic mistakes but **wrong assumptions about the shape of the event stream**, and a wrong assumption
// cannot be revealed by a hand-made fixture (when making it you follow the same wrong assumption). The three assumptions:
//
//  1. "An interrupt produces a final state that is recognizably an interrupt": **wrong**. An interrupt produces
//     `session.status_idle + stop_reason=end_turn`, exactly the same as a normal finish.
//  2. "The interrupted turn finishes itself and sends out the new answer": **wrong**. It drops the stream as soon as it sees the final frame
//     and leaves, and nobody is waiting for the new message's answer.
//  3. "agent.message is the reply": **wrong**. The sentence before calling a tool, "let me pull the chat history first",
//     is also an agent.message.
//
// The incident: the user sent "1" "2" "3" in a row in Feishu (the first two were redirected) and got one reply; then sent "4",
// and got another. **Exactly two were expected**; three were received, with mixed-up content.
// That is what the trueSequence* groups of frames below reconstruct.
//
// The frames are redacted: only types, stop_reason, and the parts of the text without identity info are kept (the original
// chat_id / thread_id / emails never go into code).

package feishu

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Real event sequences (redacted)
// ---------------------------------------------------------------------------

// The two expected replies, taken verbatim from the production event stream (seq 29 and seq 57).
const (
	answerForThree = "你说的「1、2、3」是指哪几个选项？我这边没有看到之前列出的选项列表，能帮我补充一下上下文吗？"
	answerForFour  = "抱歉，我看不到你之前发的消息，所以不太确定你说的「1、2、3、4」指的是什么。能简单说一下你在选什么吗？这样我就能帮上忙了。"
)

// The two **intermediate narration** sentences, also taken verbatim from production (seq 34 and seq 47).
// They are agent.message but not replies; the user explicitly said these two should not appear in Feishu.
const (
	narrationPullHistory = "让我先拉一下聊天历史，看看你之前说了什么。\n\n"
	narrationPullThread  = "让我拉一下这个线程的消息，看看上下文。\n\n"
)

// interruptedTurnFrames reconstructs the turn for "1": redirected by the next message after being sent.
//
// Matches production seq 12→16. **Not a single agent.message**, and the final stop_reason is
// end_turn (not requires_action), so the old code judged "this turn ended with no content",
// and sent that "something went wrong during execution".
func interruptedTurnFrames() [][]byte {
	return [][]byte{
		mustJSON(map[string]any{"type": "session.status_running"}),
		mustJSON(map[string]any{"type": "user.interrupt"}),
		mustJSON(map[string]any{
			"type":        "session.status_idle",
			"stop_reason": map[string]any{"type": "end_turn"},
		}),
	}
}

// toolUsingTurnFrames reconstructs the turn for "4": two tool calls with narration in between.
//
// Matches production seq 31→60. Of four agent.message segments only the last is the reply;
// the first two are narration before calling tools (the third, answerForThree, belonged to the turn for "3" in production
// and was brought in by error 2; it is deliberately included here too, to check it no longer mixes in after segmenting).
func toolUsingTurnFrames() [][]byte {
	requiresAction := map[string]any{
		"type":        "session.status_idle",
		"stop_reason": map[string]any{"type": "requires_action"},
	}
	return [][]byte{
		mustJSON(map[string]any{"type": "session.status_running"}),
		agentFrame(narrationPullHistory),
		toolUseFrame("call_read_skill", "read"),
		mustJSON(requiresAction),
		mustJSON(map[string]any{"type": "user.tool_result"}),
		mustJSON(map[string]any{"type": "session.status_running"}),
		agentFrame(narrationPullThread),
		toolUseFrame("call_list_thread", "bash"),
		mustJSON(requiresAction),
		mustJSON(map[string]any{"type": "user.tool_result"}),
		mustJSON(map[string]any{"type": "session.status_running"}),
		agentFrame(answerForFour),
		mustJSON(map[string]any{
			"type":        "session.status_idle",
			"stop_reason": map[string]any{"type": "end_turn"},
		}),
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestInterruptedTurnSendsNothing [error 1]: the redirected turn **should send no message at all**.
//
// Old behavior: answer empty → sendFinalReply fallback → one more line in the chat, "this turn produced no reply,
// something may have gone wrong during execution. Try again later." Yet nothing went wrong in this turn; it was just interrupted by the user's own
// next message. The user explicitly said this should not be there.
func TestInterruptedTurnSendsNothing(t *testing.T) {
	conv := &fakeConversations{agentIDs: []string{"agent_1"}, frames: interruptedTurnFrames()}
	in, sender, _, _, _ := trioFixture(t, conv)

	if err := in.Handle(context.Background(), inboundMsg("om_interrupted", "1")); err != nil {
		t.Fatalf("这一轮不该返回错误（会让飞书重投）: %v", err)
	}

	if got := sender.sent(); len(got) != 0 {
		var texts []string
		for _, c := range got {
			texts = append(texts, c.text)
		}
		t.Errorf("被转向的轮次不该发任何消息，实际发了 %d 条: %q", len(got), texts)
	}
}

// TestInterruptedTurnDoesNotClaimExecutionFailure: same as above, but **only watches that error text**.
//
// It is deliberately separate from the previous one: the previous guards "send nothing at all", this guards "even if some future reason
// makes this path send something, it must never be 'something went wrong during execution'". That sentence calls a normal interrupt a failure,
// and users would go chasing a problem that does not exist.
func TestInterruptedTurnDoesNotClaimExecutionFailure(t *testing.T) {
	conv := &fakeConversations{agentIDs: []string{"agent_1"}, frames: interruptedTurnFrames()}
	in, sender, _, _, _ := trioFixture(t, conv)

	if err := in.Handle(context.Background(), inboundMsg("om_interrupted", "1")); err != nil {
		t.Fatalf("这一轮不该返回错误: %v", err)
	}

	for _, c := range sender.sent() {
		if strings.Contains(c.text, "执行出了问题") {
			t.Errorf("被中断不是故障，不该说「执行出了问题」：%q", c.text)
		}
	}
}

// TestEmptyAnswerStillReportsRealFailure, the flip side: when **really** nothing was produced the notice must still be sent.
//
// This pairs with the two above; without it the "fix" could degrade into "delete the fallback", turning
// "execution really failed" into a silent failure (the user staring at a blank, waiting for a reply that never comes).
// The recovery path in resume.go also relies on this sentence (see the comment on sendFinalReply).
//
// The **only** basis for telling the two apart is whether there is a user.interrupt: there is none here, so it is a real failure.
func TestEmptyAnswerStillReportsRealFailure(t *testing.T) {
	conv := &fakeConversations{agentIDs: []string{"agent_1"}, frames: [][]byte{
		mustJSON(map[string]any{"type": "session.status_running"}),
		// No agent.message and no user.interrupt: Ark just ended like that.
		mustJSON(map[string]any{
			"type":        "session.status_idle",
			"stop_reason": map[string]any{"type": "end_turn"},
		}),
	}}
	in, sender, _, _, _ := trioFixture(t, conv)

	if err := in.Handle(context.Background(), inboundMsg("om_broken", "帮我查一下")); err != nil {
		t.Fatalf("这一轮不该返回错误: %v", err)
	}

	var told bool
	for _, c := range sender.sent() {
		if strings.Contains(c.text, "没有产生回复") {
			told = true
		}
	}
	if !told {
		t.Error("真的没产出时必须告诉用户，否则他对着空白等一个永远不来的回复")
	}
}

// TestFinalReplyIsOnlyTheLastSegment [error 3]: send only "the last segment", not intermediate narration.
//
// The user explicitly said Feishu should not show "let me pull the chat history first" or "let me pull the messages of this thread":
// those are the agent's narration when getting ready to call a tool. The old code joined all agent.message in a turn,
// so they were sent glued to the reply in one message.
func TestFinalReplyIsOnlyTheLastSegment(t *testing.T) {
	conv := &fakeConversations{agentIDs: []string{"agent_1"}, frames: toolUsingTurnFrames()}
	in, sender, _, _, _ := trioFixture(t, conv)

	if err := in.Handle(context.Background(), inboundMsg("om_four", "4")); err != nil {
		t.Fatalf("这一轮不该返回错误: %v", err)
	}

	sent := sender.sent()
	if len(sent) != 1 {
		t.Fatalf("一轮应当只发一条回复，实际 %d 条", len(sent))
	}
	body := sent[0].text

	// 1. **Not a single line** of intermediate narration may appear.
	for _, narration := range []string{narrationPullHistory, narrationPullThread} {
		if strings.Contains(body, strings.TrimSpace(narration)) {
			t.Errorf("中间过程叙述不该发给用户：%q 出现在 %q 里", strings.TrimSpace(narration), body)
		}
	}
	// 2. The real reply must be there.
	if !strings.Contains(body, answerForFour) {
		t.Errorf("最终答复没发出去，实际发的是 %q", body)
	}
	// 3. Also guard "not all segments joined together": the joined length would clearly exceed the reply itself.
	if len(body) > len(answerForFour)+16 {
		t.Errorf("回复里带了额外内容（疑似把多段拼在一起）：%q", body)
	}
}

// TestTurnEndedTreatsInterruptIdleAsEnd: the idle brought by an interrupt must still **drop the stream**.
//
// This guards the boundary of the fix: telling "interrupted" apart uses user.interrupt,
// and it **must not** change into "make turnEnded return false for interrupts": then the stream would never close,
// and the serial lock would be held until runTimeout (10 minutes), blocking every question from the user. That was another production incident.
func TestTurnEndedTreatsInterruptIdleAsEnd(t *testing.T) {
	idleAfterInterrupt := mustJSON(map[string]any{
		"type":        "session.status_idle",
		"stop_reason": map[string]any{"type": "end_turn"},
	})
	if !turnEnded(idleAfterInterrupt) {
		t.Error("中断带来的 idle 必须算结束，否则流不关、串行锁握满 10 分钟")
	}
	if !turnInterrupted(mustJSON(map[string]any{"type": "user.interrupt"})) {
		t.Error("user.interrupt 必须被认成中断")
	}
	// A normal end must not be misjudged as an interrupt, or the normal turn's answer cannot be sent.
	if turnInterrupted(idleAfterInterrupt) {
		t.Error("终态帧本身不是中断标记——判据只能是 user.interrupt")
	}
}

// TestTurnDoesNotInheritPreviousTurnAnswer [error 2]: the previous turn's answer must not leak into this turn.
//
// # This reconstructs that mix-up in production
//
// The answer of the turn for "3" (answerForThree) was seq 29 in production, and the moment it arrived (23.055) fell within
// the subscription window of the turn for "4" (the subscription was set up at about 22.98), so it was picked up by the buffer of "4",
// and the user saw the answers of two questions glued together in one message.
//
// Here it is placed at the **beginning** of the turn for "4" (before the tool calls) to reconstruct that position. After the fix
// the first tool_use clears it together with the narration, and only answerForFour remains.
//
// ⚠️ The real cure is "every message has its own owner" (see the assertion in followup_test.go that
// the new message must go through AppendRunEvents itself). This guards the second line of defense:
// even if a frame slips in, it is not sent along with the answer.
func TestTurnDoesNotInheritPreviousTurnAnswer(t *testing.T) {
	frames := append(
		// The previous turn's answer arrives first (that was the order in production).
		[][]byte{agentFrame(answerForThree)},
		toolUsingTurnFrames()...,
	)
	conv := &fakeConversations{agentIDs: []string{"agent_1"}, frames: frames}
	in, sender, _, _, _ := trioFixture(t, conv)

	if err := in.Handle(context.Background(), inboundMsg("om_four", "4")); err != nil {
		t.Fatalf("这一轮不该返回错误: %v", err)
	}

	sent := sender.sent()
	if len(sent) != 1 {
		t.Fatalf("一轮应当只发一条回复，实际 %d 条", len(sent))
	}
	if strings.Contains(sent[0].text, answerForThree) {
		t.Errorf("上一轮的答案串进了这一轮：%q", sent[0].text)
	}
	if !strings.Contains(sent[0].text, answerForFour) {
		t.Errorf("这一轮自己的答复没发出去，实际发的是 %q", sent[0].text)
	}
}

// TestAnswerBufferKeepsOnlyLastSegment pins the segmenting behavior of answerBuffer on its own.
//
// The test above goes through the whole Handle path, this one hits the buffer directly; both are needed: the path one proves "it is wired in",
// this one proves "the segmenting rule itself is right", and its failure message points straight at reset.
func TestAnswerBufferKeepsOnlyLastSegment(t *testing.T) {
	var persisted []string
	var mu sync.Mutex
	a := &answerBuffer{persist: func(_, text string) {
		mu.Lock()
		defer mu.Unlock()
		persisted = append(persisted, text)
	}}

	a.write("让我先查一下。")
	a.reset() // agent.tool_use
	a.write("查完了，")
	a.write("结论是这样。")

	if got := a.text(); got != "查完了，结论是这样。" {
		t.Errorf("只该留工具调用之后那段，实际 %q", got)
	}
	// The stored value must be cleared too: otherwise if the process dies at this moment, the recovery path would take the obsolete narration as the start of the answer.
	mu.Lock()
	defer mu.Unlock()
	var sawEmpty bool
	for _, p := range persisted {
		if p == "" {
			sawEmpty = true
		}
	}
	if !sawEmpty {
		t.Error("reset 必须把清空也落库，否则恢复时会把作废的旁白当成答案开头")
	}
}

// TestConcurrentMessagesDoNotDuplicateReplies [found on a real device]: a session must not have two subscribers.
//
// # Symptom: three identical replies that glue two turns' answers together
//
// Reproduced on a real device (local sesn-example-07, 23:41): the user sent 9 numbers in a row in a direct-chat topic,
// and Feishu received **3 identical** replies, each containing the content of two turns
// ("you sent 1, 2, 4, 7..." + "haha, got 8 and 3 again..." glued together).
//
// # Root cause: the reclaim test does not hold under session reuse
//
// 9 messages produced only 3 interrupts; the other 6 slipped in through reclaimFinishedTurn: it uses
// "is the run final" to decide whether a lock is left over, while **a turn lives longer than one inference**: after the agent answers
// (the run becomes idle), the goroutine holding the lock still has to close the chain of thought, wait for the ordering gate, and send the answer.
// In that window the run is idle, so the second message reclaimed this **live** lock,
// and two goroutines subscribed to the same event stream at once, each collecting the full text.
//
// The fix is the localTurns gate: nothing is reclaimed while this process itself holds the lock. Real leftover locks across instances can still
// be reclaimed (that is the problem reclaim solves), guarded by TestStaleTurnReclaimedWhenRunFinished.
func TestConcurrentMessagesDoNotDuplicateReplies(t *testing.T) {
	// status=idle is the key: it simulates "the agent finished answering, but the turn holding the lock is still finishing up",
	// exactly the window on the real device that made reclaim misjudge.
	conv := &stickyConversations{
		frames: [][]byte{agentFrame("第一轮的答复"), endTurnFrame()},
		status: "idle",
	}
	sender := &fakeSender{}
	in, _ := stickyFixture(t, conv, sender)

	// Two messages come in at once, landing on the same session (group chats reuse the scope by chat).
	var wg sync.WaitGroup
	for _, id := range []string{"om_a", "om_b"} {
		wg.Add(1)
		go func(msgID string) {
			defer wg.Done()
			_ = in.Handle(context.Background(), inboundMsg(msgID, "数字"))
		}(id)
	}
	wg.Wait()

	// **A session may have only one subscriber.** Two subscribers are the very mechanism of that mix-up bug:
	// both sides collect "what the agent said", and neither can tell which sentence is for whom.
	if got := conv.streamCount(); got > 1 {
		t.Errorf("一条会话上同时开了 %d 个订阅——回复会重复且串味", got)
	}

	// And there must not be two replies with the same content (the 3 identical ones on the real device).
	seen := map[string]int{}
	for _, c := range sender.sent() {
		if strings.Contains(c.text, "上一句还在处理") {
			continue // the queued notice is expected and does not count as a reply
		}
		seen[c.text]++
	}
	for text, n := range seen {
		if n > 1 {
			t.Errorf("同一条回复发了 %d 次：%q", n, text)
		}
	}
}

// TestBeginTurnClaimsLocalSlotBeforeStore ⭐⭐: no gap is allowed between grabbing the lock and the "this process holds the lock" marker.
//
// # What it guards
//
// `localTurns` is the gate that "does not reclaim our own live lock" (see the long comment on beginTurn). Whether the gate works
// depends entirely on **when it is registered**: as long as there is a moment where "the store lock has been grabbed, but localTurns does not yet have
// this run", a concurrent second message sees `holdsTurn == false` at that moment,
// reclaims a **live** lock as a leftover, and two subscribers are opened on the same session:
// the user gets several identical replies that glue two turns' answers together.
//
// # Why a separate test, instead of relying on TestConcurrentMessagesDoNotDuplicateReplies
//
// That one is **probabilistic**: it only turns red when two goroutines really collide in that window, with a measured reproduction rate
// of only about 9% (8 hits in 6 rounds × count=15). So a half fix where "the window got narrower but was not closed"
// could make it green most of the time: **that is exactly how I missed it once**: the first version put registration right after TryBeginTurn,
// it looked fixed, and it was still red 13/90 in practice.
//
// This one uses a **deterministic** test instead: ask directly "at the moment the lock is grabbed, is holdsTurn already true".
// It does not depend on scheduling, and one run tells right from wrong.
//
// # Mutation check (done)
//
// Change beginTurn back to "TryBeginTurn first, then localTurns.Store on success",
// and the second subtest turns red on the spot (that is exactly the shape of the first fix).
func TestBeginTurnClaimsLocalSlotBeforeStore(t *testing.T) {
	newIn := func(t *testing.T) *Inbound {
		t.Helper()
		in, _ := stickyFixture(t, &stickyConversations{status: "idle"}, &fakeSender{})
		return in
	}
	rec := func(runID string) InflightTurnRecord {
		return InflightTurnRecord{RunID: runID, LarkAppID: "cli_1", ChatID: "oc_1", AgentID: "a1"}
	}

	// ⚠️⚠️ **The test must happen "during TryBeginTurn", not just after it returns.**
	//
	// After it returns both implementations are true (register first / register later are the same), so such an assertion **cannot tell right from wrong**:
	// that is how I wrote it the first time, and it stayed green against "the first half fix".
	// The real difference only exists in that moment: the store lock is already mine, but am I in localTurns?
	// So a decorator is used here, asking holdsTurn back at the moment Store really takes the lock.
	t.Run("库里落锁的那一刻:holdsTurn 必须已经为真", func(t *testing.T) {
		in := newIn(t)
		var duringLock, sawCall bool
		in.cfg.Store = &probeTurnStore{
			Store: in.cfg.Store,
			onTryBegin: func(runID string) {
				sawCall = true
				// At this moment Store is about to (or just did) record the lock in the store. A concurrent second message
				// sees exactly this value at this moment; false means the live lock would be reclaimed.
				duringLock = in.holdsTurn(runID)
			},
		}
		begun, err := in.beginTurn(context.Background(), rec("sesn-x"), runTimeout)
		if err != nil || !begun {
			t.Fatalf("第一次抢锁应当成功: begun=%v err=%v", begun, err)
		}
		if !sawCall {
			t.Fatal("前置没成立：装饰器没被调用，这条用例什么都没测到")
		}
		if !duringLock {
			t.Error("❌ 库里落锁的那一刻 holdsTurn 还是 false。" +
				"并发的第二条消息就在这个窗口里把这条**活着的**锁当成残留回收掉，" +
				"于是同一条会话上开出两个订阅者——用户收到重复且串味的回复。" +
				"登记必须发生在抢库锁**之前**（LoadOrStore 占位），" +
				"紧贴在它之后是不够的。")
		}
	})

	t.Run("同一条会话再抢:抢不到，且不许把已有的占位摘掉", func(t *testing.T) {
		in := newIn(t)
		if begun, err := in.beginTurn(context.Background(), rec("sesn-y"), runTimeout); !begun || err != nil {
			t.Fatalf("前置：第一次抢锁应当成功，begun=%v err=%v", begun, err)
		}
		begun, err := in.beginTurn(context.Background(), rec("sesn-y"), runTimeout)
		if err != nil {
			t.Fatalf("第二次抢锁不该报错: %v", err)
		}
		if begun {
			t.Error("❌ 同一条会话被抢到了两次——串行锁失效")
		}
		// ⚠️⚠️ The most important one: the second failure **must not remove the first one's reservation**.
		// If removed, the first goroutine would disappear from holdsTurn,
		// while it is still running, back to the old "live lock reclaimed" bug.
		if !in.holdsTurn("sesn-y") {
			t.Error("❌ 第二次抢锁失败时把第一次的占位摘掉了：持锁那个 goroutine " +
				"就此对 holdsTurn 隐形，reclaim 会回收它的活锁")
		}
	})

	t.Run("没抢到的那次:不留下自己的占位", func(t *testing.T) {
		in := newIn(t)
		if begun, _ := in.beginTurn(context.Background(), rec("sesn-z"), runTimeout); !begun {
			t.Fatal("前置：第一次抢锁应当成功")
		}
		_, _ = in.beginTurn(context.Background(), rec("sesn-z"), runTimeout)
		// Finish the turn holding the lock normally (the caller's defer does exactly these two steps).
		in.localTurns.Delete("sesn-z")
		// ⚠️ localTurns must now be clean. **If the attempt that did not get the lock left its reservation in the table**,
		// this session could never start a new turn in this process until restart, with zero errors,
		// and users would just feel "the bot is ignoring me".
		if in.holdsTurn("sesn-z") {
			t.Error("❌ 抢锁失败的那次把占位漏在了表里：这条会话被自己锁死，" +
				"且没有任何报错")
		}
	})
}

// probeTurnStore is the Store decorator for the test above: it calls back once **during** TryBeginTurn,
// so the test can check the in-process state "at that moment".
//
// ⚠️ It only wraps the one method TryBeginTurn and passes everything else through to the embedded Store,
// so it does not quietly change other behavior, and when the test fails there is no need to suspect the stand-in is wrong.
type probeTurnStore struct {
	Store
	onTryBegin func(runID string)
}

func (p *probeTurnStore) TryBeginTurn(
	ctx context.Context, rec InflightTurnRecord, staleAfter time.Duration,
) (bool, error) {
	if p.onTryBegin != nil {
		p.onTryBegin(rec.RunID)
	}
	return p.Store.TryBeginTurn(ctx, rec, staleAfter)
}
