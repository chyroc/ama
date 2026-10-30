// Tests for the path "after an agent is saved, sync it to Ark right away in the background".
//
// # What these cases guard
//
// Before this path existed, editing the prompt and clicking save only wrote the database and
// did not touch Ark. The change took effect only when someone next opened a new chat: zero
// errors, and the user could not tell at all (see the file header of core/agentsync.go).
//
// The three most costly invariants, each with a mutation check (written in its own comment):
//
//  1. **Same fingerprint means no repeat sync** (that is, "if it was just synced, don't do
//     it again");
//  2. **A failed sync never writes back the fingerprint**. Writing it back makes the next
//     comparison match, and that agent never catches up again;
//  3. **Task state lives in the database, not in the process**, so another instance can
//     still find it.
//
// Number 2 is the most dangerous point here: when it breaks, **all tests still pass**, the
// symptom only shows up in production three weeks later, and the error does not point to the
// real cause. The three existing write-back points in chat.go exist for exactly this, and
// this file confirms the background path does not bypass them.

package core

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/repository/dao/coremem"
	"github.com/chyroc/ama/server/shared/infra/ma"
)

// runSyncerOnce runs one tick of the sync worker and waits for the running items to finish.
//
// It does not start a long-running goroutine: that would need sleeps to wait for a tick,
// which is slow and flaky. Calling tick directly and waiting on the wg is deterministic, and
// the claim SQL inside tick is the same one production uses.
func runSyncerOnce(t *testing.T, svc *Service) *AgentSyncer {
	t.Helper()
	syncer := NewAgentSyncer(AgentSyncerConfig{Service: svc})
	if syncer == nil {
		t.Fatal("装配同步执行器失败")
	}
	syncer.tick(context.Background())
	syncer.wg.Wait()
	return syncer
}

// latestTask gets the latest sync task of an agent, and calls Fatal if there is none.
func latestTask(t *testing.T, svc *Service, agentID string) apimodel.AgentSyncTask {
	t.Helper()
	task, err := svc.LatestAgentSyncTask(context.Background(), agentID)
	if err != nil {
		t.Fatalf("查同步任务失败：%v", err)
	}
	return task
}

// draftOf builds a full draft from the agent in the database (UpdateAgent takes a full draft).
func draftOf(a apimodel.Agent) apimodel.AgentDraft {
	return apimodel.AgentDraft{
		Name: a.Name, Description: a.Description, System: a.System, Model: a.Model,
		BuiltinTools: a.BuiltinTools, SkillIDs: a.SkillIDs, FeishuAppIDs: a.FeishuAppIDs,
		Status: a.Status, IntegrationIDs: a.IntegrationIDs, Envs: a.Envs,
		EnvAliases: a.EnvAliases, FollowUp: a.FollowUp, ProgressDetail: a.ProgressDetail,
	}
}

// TestAgentSaveEnqueuesSyncTask is the regression case for **the symptom being fixed here**.
//
// Saving an agent should not do nothing. Before this, PATCH returned 200 and the database was
// changed, while the Ark side only found out the next time someone opened a new chat.
//
// Mutation check: remove `s.EnqueueAgentSync(ctx, saved)` from UpdateAgent -> this case
// fails.
func TestAgentSaveEnqueuesSyncTask(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	a, err := store.GetAgent(ctx, seedChatAgent)
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	draft := draftOf(a)
	draft.System = "SYSTEM-V2"
	if _, err := svc.UpdateAgent(ctx, seedChatAgent, draft); err != nil {
		t.Fatalf("保存 agent 失败：%v", err)
	}

	task := latestTask(t, svc, seedChatAgent)
	if task.Status != apimodel.AgentSyncPending {
		t.Fatalf("保存之后该有一条待同步任务，实际状态 %q——"+
			"没有的话用户改了提示词要等下次开对话才生效，且零报错", task.Status)
	}
	if task.AgentID != seedChatAgent {
		t.Errorf("任务该指向这个 agent，实际 %q", task.AgentID)
	}
}

// TestAgentSyncPushesSpecToArk asserts that the task **really sent the content to Ark**.
//
// Asserting only "a task was queued" is not enough: an implementation that queues tasks but
// never runs them would pass, and that is no better than no fix. So here we look at the spec
// the runner received.
func TestAgentSyncPushesSpecToArk(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	// Open a chat first so the Ark Agent and the binding exist (simulates "this agent is
	// already in use").
	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("开对话失败：%v", err)
	}
	if len(runner.updatedSpecs) != 0 {
		t.Fatalf("建 Agent 那次就带上了 spec，不该有 update，实际 %d 次", len(runner.updatedSpecs))
	}

	a, err := store.GetAgent(ctx, seedChatAgent)
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	draft := draftOf(a)
	draft.System = "SYSTEM-ASYNC-V2"
	if _, err := svc.UpdateAgent(ctx, seedChatAgent, draft); err != nil {
		t.Fatalf("保存 agent 失败：%v", err)
	}

	runSyncerOnce(t, svc)

	if len(runner.updatedSpecs) != 1 {
		t.Fatalf("保存之后异步同步该追平一次，实际 update %d 次", len(runner.updatedSpecs))
	}
	// Assert **what Ark received**, not just "it was called": counting calls alone would let
	// an implementation that "calls it but still sends the old system prompt" pass.
	if got := runner.updatedSpecs[0].System; !strings.Contains(got, "SYSTEM-ASYNC-V2") {
		t.Errorf("发给方舟的 system 里该有新提示词，实际 %q", got)
	}
	if task := latestTask(t, svc, seedChatAgent); task.Status != apimodel.AgentSyncSucceeded {
		t.Errorf("同步成功后任务该是 succeeded，实际 %q", task.Status)
	}
}

// TestAgentSyncSkippedWhenFingerprintUnchanged is the target of **mutation check (1)**.
//
// The user's request "if it was just synced, don't sync again" lands here: when the
// fingerprint matches there are **zero Ark round trips**.
//
// Mutation check: change, in syncBindingSpec,
//
//	if fingerprint == b.SpecFingerprint { return b }
//
// to sync unconditionally (just delete that if) -> this case fails. That mutation compiles
// cleanly.
//
// ⚠️ The check is "how many times Ark was called", not "did the task succeed": an
// unconditional-sync implementation also succeeds, it just wastes a round trip. Only counting
// calls reveals it.
func TestAgentSyncSkippedWhenFingerprintUnchanged(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("开对话失败：%v", err)
	}

	a, err := store.GetAgent(ctx, seedChatAgent)
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	draft := draftOf(a)
	draft.System = "SYSTEM-ONCE"
	if _, err := svc.UpdateAgent(ctx, seedChatAgent, draft); err != nil {
		t.Fatalf("保存 agent 失败：%v", err)
	}
	runSyncerOnce(t, svc)
	if len(runner.updatedSpecs) != 1 {
		t.Fatalf("第一次保存该追平一次，实际 %d 次", len(runner.updatedSpecs))
	}

	// Save **exactly the same content** again (the user clicked save twice, or only changed a
	// field that is not in the fingerprint).
	if _, err := svc.UpdateAgent(ctx, seedChatAgent, draft); err != nil {
		t.Fatalf("再次保存失败：%v", err)
	}
	runSyncerOnce(t, svc)

	if len(runner.updatedSpecs) != 1 {
		t.Errorf("内容没变时不该再打方舟，实际累计 update %d 次——"+
			"每次保存都白打一个往返，用户越用越慢且方舟侧配额白烧", len(runner.updatedSpecs))
	}
	// But the task itself must end normally, not get stuck in running because "there was
	// nothing to do".
	if task := latestTask(t, svc, seedChatAgent); task.Status != apimodel.AgentSyncSucceeded {
		t.Errorf("没事可做也要落成 succeeded，实际 %q——"+
			"卡在 running 的话前端会一直转到超时", task.Status)
	}
}

// TestAgentSyncFailureKeepsRetryChannelOpen is the target of **mutation check (2)**, and the
// most important case in this file.
//
// # What this guards
//
// The fingerprint means "this is exactly what is on the Ark side right now". If a **failed**
// sync writes the fingerprint back, the record becomes false: the next sync compares the
// fingerprint -> same -> returns right away, and the UpdateAgent that should fix it is never
// called. So "not synced" becomes **permanent**, with zero errors.
//
// This is not hypothetical: the ma layer once failed to pass Skills, and ensureBinding wrote
// back the fingerprint unconditionally, so adding that field later could not save the agents
// already affected (see skillsFingerprintVersion in skillzip.go).
//
// # Mutation check
//
// Change the failure branch in syncBindingSpec to "also write back the fingerprint":
//
//	if err := s.runner.UpdateAgentSpec(...); err != nil {
//	    b.SpecFingerprint = fingerprint   // ← add this line
//	    _ = s.store.PutArkBinding(ctx, b)
//	    return b
//	}
//
// -> this case fails. That mutation compiles cleanly.
//
// ⚠️ **In practice it fails in a worse way than expected**: it does not fail in the second
// part (retry after recovery) but already in the first part, with "consecutive failures should
// end in failed, got "succeeded"". After the false fingerprint is written back, the second
// attempt sees the fingerprint "the same" and skips, so that **real failure is reported as a
// successful sync**. The user sees a green "synced", and Ark never got the new prompt.
//
// ⚠️ **The second part still must not be removed**: it checks "can it heal itself after
// recovery", which is exactly the long-term effect of a false fingerprint (the retry path is
// blocked forever). Checking only the first part would let an implementation that "does not
// write back the fingerprint on failure, but also never retries" pass. This is the rule
// "pick a check whose effect only the fix can produce".
func TestAgentSyncFailureKeepsRetryChannelOpen(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("开对话失败：%v", err)
	}

	// Make the spec update on the Ark side fail.
	runner.updateSpecErr = errors.New("方舟暂时不可用")

	a, err := store.GetAgent(ctx, seedChatAgent)
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	draft := draftOf(a)
	draft.System = "SYSTEM-RETRY"
	if _, err := svc.UpdateAgent(ctx, seedChatAgent, draft); err != nil {
		t.Fatalf("保存 agent 失败：%v", err)
	}

	// Run until retries are used up; the task ends in failed.
	for i := 0; i < syncMaxAttempts; i++ {
		runSyncerOnce(t, svc)
	}
	task := latestTask(t, svc, seedChatAgent)
	if task.Status != apimodel.AgentSyncFailed {
		t.Fatalf("连续失败该落 failed，实际 %q", task.Status)
	}
	if task.FailureReason == "" {
		t.Error("失败要留下原因，否则用户只看到一个「失败了」而不知道该做什么")
	}

	// ⚠️ Key assertion: the fingerprint must **not** be written back.
	binding, err := store.GetArkBinding(ctx, seedWS, seedChatAgent)
	if err != nil {
		t.Fatalf("取绑定失败：%v", err)
	}
	if binding.SpecFingerprint == ArkSpecFingerprint(arkSpecFor(mustAgent(t, store, seedChatAgent))) {
		t.Fatal("同步失败却回写了 spec 指纹——这会让下次比对直接命中，" +
			"那个 agent 从此永远不再追平，且零报错")
	}

	// Second part: Ark has recovered, and a retry should really catch up.
	runner.updateSpecErr = nil
	if _, err := svc.RetryAgentSync(ctx, seedChatAgent); err != nil {
		t.Fatalf("重试失败：%v", err)
	}
	runSyncerOnce(t, svc)

	if len(runner.updatedSpecs) != 1 {
		t.Fatalf("恢复之后重试该追平一次，实际 %d 次——"+
			"0 次说明失败时回写了指纹，把重试通道堵死了", len(runner.updatedSpecs))
	}
	if got := runner.updatedSpecs[0].System; !strings.Contains(got, "SYSTEM-RETRY") {
		t.Errorf("追平时发给方舟的 system 该是新那份，实际 %q", got)
	}
	if task := latestTask(t, svc, seedChatAgent); task.Status != apimodel.AgentSyncSucceeded {
		t.Errorf("重试成功后该落 succeeded，实际 %q", task.Status)
	}
}

// TestAgentSyncTaskSurvivesInstanceSwap is the target of **mutation check (3)**.
//
// Frontend polling lands on a random instance, so task state must be in the **database**,
// not in the process.
//
// Here two separate Services (sharing one store) simulate two instances: one instance
// enqueues, and the other can find it and run it to the end.
//
// Mutation check: change task state to an in-process map (for example add
// `syncTasks map[string]apimodel.AgentSyncTask` to Service, written by EnqueueAgentSync and
// read by LatestAgentSyncTask) -> this case fails, because svcB's map is empty.
func TestAgentSyncTaskSurvivesInstanceSwap(t *testing.T) {
	ctx := context.Background()
	svcA, store, _ := newChatService(t)

	// The second "instance": same store, a separate Service and runner.
	runnerB := &fakeRunner{}
	svcB, err := New(Config{Store: store, ArkRunner: runnerB})
	if err != nil {
		t.Fatalf("装配第二个实例失败：%v", err)
	}

	a, err := store.GetAgent(ctx, seedChatAgent)
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	draft := draftOf(a)
	draft.System = "SYSTEM-CROSS-INSTANCE"
	// Save on instance A.
	if _, err := svcA.UpdateAgent(ctx, seedChatAgent, draft); err != nil {
		t.Fatalf("保存 agent 失败：%v", err)
	}

	// Query on instance B; this is how frontend polling lands at random.
	taskOnB, err := svcB.LatestAgentSyncTask(ctx, seedChatAgent)
	if err != nil {
		t.Fatalf("换个实例查不到同步任务：%v——"+
			"线上是多实例，查状态那一下多半落在别的进程上，"+
			"状态放进程内会让任务凭空消失", err)
	}
	if taskOnB.Status != apimodel.AgentSyncPending {
		t.Errorf("实例 B 该看到 pending，实际 %q", taskOnB.Status)
	}

	// Instance B should also be able to claim it and run it (the instance that enqueued it
	// may already have been recreated).
	runSyncerOnce(t, svcB)
	if len(runnerB.updatedSpecs) == 0 && runnerB.createdAgents == 0 {
		t.Error("实例 B 该能认领并执行这条任务——" +
			"否则入队的实例一挂，任务永远没人跑")
	}
	if task := latestTask(t, svcA, seedChatAgent); task.Status != apimodel.AgentSyncSucceeded {
		t.Errorf("实例 B 跑完之后，实例 A 也该查到 succeeded，实际 %q", task.Status)
	}
}

// TestAgentSyncDoubleClickRunsOnce pins "two quick clicks queue only one task".
//
// The database decides (partial unique index + on conflict do nothing), not "check then
// create". The latter is TOCTOU and will miss with many instances.
func TestAgentSyncDoubleClickRunsOnce(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	a, err := store.GetAgent(ctx, seedChatAgent)
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	draft := draftOf(a)

	first := svc.EnqueueAgentSync(ctx, a)
	draft.System = "SYSTEM-SECOND-CLICK"
	saved, err := svc.UpdateAgent(ctx, seedChatAgent, draft)
	if err != nil {
		t.Fatalf("第二次保存失败：%v", err)
	}
	second := svc.EnqueueAgentSync(ctx, saved)

	if first.ID == "" || second.ID == "" {
		t.Fatal("两次入队都该拿到一条任务")
	}
	if first.ID != second.ID {
		t.Errorf("同一个 agent 没跑完时不该再排一条，两次拿到 %q 与 %q——"+
			"排两条会让两个实例同时改同一个方舟 Agent", first.ID, second.ID)
	}
}

// TestAgentSyncResyncWhenSavedWhileRunning pins "saved again while running".
//
// Timeline: t0 save A -> claimed -> syncing; t1 save B (hits the running one); t2 sync done.
// If t2 just ends in succeeded, the user sees "sync succeeded" while Ark actually has A, and
// B waits until the next chat. That is exactly the symptom this mechanism should remove, and
// this time it also misleads the user once more.
//
// Mutation check: remove the `a.requeueIfResyncWanted(...)` branch in runOne -> this case
// fails.
func TestAgentSyncResyncWhenSavedWhileRunning(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	a, err := store.GetAgent(ctx, seedChatAgent)
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	first := svc.EnqueueAgentSync(ctx, a)

	// Push it to running by hand (simulates "already claimed by some instance and running").
	claimed, err := store.ClaimDueAgentSyncTasks(ctx, "inst-a", time.Now().UTC(), time.Minute, 10)
	if err != nil {
		t.Fatalf("认领失败：%v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != first.ID {
		t.Fatalf("该认领到刚入队那条，实际 %+v", claimed)
	}

	// The user saves once more while it is running.
	draft := draftOf(a)
	draft.System = "SYSTEM-WHILE-RUNNING"
	saved, err := svc.UpdateAgent(ctx, seedChatAgent, draft)
	if err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	_ = saved

	row, err := store.LatestAgentSyncTask(ctx, seedChatAgent)
	if err != nil {
		t.Fatalf("查任务失败：%v", err)
	}
	if row.ID != first.ID {
		t.Fatalf("不该新建任务，实际换了一条 %q", row.ID)
	}
	if !row.ResyncRequested {
		t.Error("同步期间又保存了，该记下「跑完再跑一轮」——" +
			"不记的话这次保存的改动要等下次开对话才生效，而用户看到的是「同步成功」")
	}
}

// TestAgentSyncZombieReclaimedAfterLeaseExpiry pins "a task whose instance died does not stay
// in running forever".
//
// When the instance that claimed it is recreated (deploy / scale-in / failed health check),
// the write-back never comes. Only lease expiry lets someone else take over; this is the only
// way an in-process goroutine can heal.
func TestAgentSyncZombieReclaimedAfterLeaseExpiry(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	a, err := store.GetAgent(ctx, seedChatAgent)
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	task := svc.EnqueueAgentSync(ctx, a)

	// Instance A claimed it, then died (never writes back).
	now := time.Now().UTC()
	claimed, err := store.ClaimDueAgentSyncTasks(ctx, "dead-instance", now, time.Minute, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("认领失败：%v（%d 条）", err, len(claimed))
	}

	// While the lease has not expired, nobody else can take it.
	again, err := store.ClaimDueAgentSyncTasks(ctx, "inst-b", now.Add(30*time.Second), time.Minute, 10)
	if err != nil {
		t.Fatalf("认领失败：%v", err)
	}
	if len(again) != 0 {
		t.Errorf("租约还有效时不该被抢走，实际领到 %d 条——"+
			"那会让同一个 agent 被两个实例同时同步", len(again))
	}

	// After the lease expires it must be reclaimable, or the task stays in running forever.
	reclaimed, err := store.ClaimDueAgentSyncTasks(ctx, "inst-b", now.Add(2*time.Minute), time.Minute, 10)
	if err != nil {
		t.Fatalf("认领失败：%v", err)
	}
	if len(reclaimed) != 1 || reclaimed[0].ID != task.ID {
		t.Fatalf("租约过期后该被回收，实际领到 %d 条——"+
			"少了这一支，实例一挂那条任务就永远停在 running，前端一直转到超时",
			len(reclaimed))
	}
	if reclaimed[0].AttemptCount != 2 {
		t.Errorf("回收要计一次尝试（好在反复失败时能放弃），实际 %d", reclaimed[0].AttemptCount)
	}
}

// TestAgentSyncMissingCredentialIsActionable pins "the failure reason must tell the user what
// to do next".
//
// A missing Ark key is the most common case, and **the user can fix it themselves**, so the
// stage is marked credential and the frontend points the user to workspace settings. Just
// saying "sync failed" leaves the user with nothing to go on.
func TestAgentSyncMissingCredentialIsActionable(t *testing.T) {
	ctx := context.Background()
	store, err := coremem.NewSeededMemStore()
	if err != nil {
		t.Fatalf("装载种子失败：%v", err)
	}
	// Deliberately **do not set** an Ark key.
	svc, err := New(Config{Store: store, ArkRunner: &fakeRunner{}})
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}

	a, err := store.GetAgent(ctx, seedChatAgent)
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	svc.EnqueueAgentSync(ctx, a)
	for i := 0; i < syncMaxAttempts; i++ {
		runSyncerOnce(t, svc)
	}

	task := latestTask(t, svc, seedChatAgent)
	if task.Status != apimodel.AgentSyncFailed {
		t.Fatalf("没配 key 该落 failed，实际 %q", task.Status)
	}
	if task.FailureStage != apimodel.AgentSyncStageCredential {
		t.Errorf("没配 key 该标成 credential（前端据此把引导指向空间设置），实际 %q",
			task.FailureStage)
	}
	if !strings.Contains(task.FailureReason, "方舟 API Key") {
		t.Errorf("失败原因要说清该去配什么，实际 %q", task.FailureReason)
	}
}

// TestAgentSyncStatusTerminalCoversBothEnds pins that the terminal-state check exists in only
// one place.
//
// Frontend polling uses it to decide "should I schedule another poll". If each place writes
// its own `s == "succeeded" || ...`, adding a terminal state later will miss one of them, and
// the frontend will poll forever on that state.
func TestAgentSyncStatusTerminalCoversBothEnds(t *testing.T) {
	terminal := []apimodel.AgentSyncStatus{apimodel.AgentSyncSucceeded, apimodel.AgentSyncFailed}
	for _, s := range terminal {
		if !s.Terminal() {
			t.Errorf("%q 该是终态", s)
		}
	}
	for _, s := range []apimodel.AgentSyncStatus{apimodel.AgentSyncPending, apimodel.AgentSyncRunning} {
		if s.Terminal() {
			t.Errorf("%q 不该是终态——判成终态会让前端在同步还没跑完时就停止轮询", s)
		}
	}
}

// TestAgentSyncTasksClearedWithAgent pins that deleting an agent also deletes its tasks.
//
// If kept, an unfinished task would block enqueueing for a new agent with the same id
// (partial unique index), which shows up as "this newly created agent saves but never syncs".
func TestAgentSyncTasksClearedWithAgent(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	a, err := store.GetAgent(ctx, seedChatAgent)
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	svc.EnqueueAgentSync(ctx, a)
	if err := store.DeleteAgent(ctx, seedChatAgent); err != nil {
		t.Fatalf("删 agent 失败：%v", err)
	}

	if _, err := store.LatestAgentSyncTask(ctx, seedChatAgent); !errors.Is(err, persistmodel.ErrNotFound) {
		t.Errorf("删 agent 之后同步任务该一起清掉，实际 err=%v——"+
			"留着会挡住同 id 新 agent 的入队（部分唯一索引），且零报错", err)
	}
}

// mustAgent gets that agent from the database, and calls Fatal if it cannot.
func mustAgent(t *testing.T, store *coremem.MemStore, id string) apimodel.Agent {
	t.Helper()
	a, err := store.GetAgent(context.Background(), id)
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	return a
}

// TestAgentSyncEnqueueRetriesNarrowWindow pins that very narrow window.
//
// # What the window looks like
//
// EnqueueAgentSyncTask is two statements: first an insert (do nothing if it hits an existing
// task), and if it hit one, an update of the running one. If that task **finishes right
// between** the two statements, neither matches, and it returns "zero value + no error".
//
// ⚠️ **Without a retry, this save silently does not sync**, and it happens exactly at "the
// last round just ended", the moment that most needs a sync, with zero errors and zero logs:
// the user changed the config, and Ark stays old forever.
//
// Here a fake Store creates that window exactly: the first call returns a zero value
// (simulates neither statement matching), and the second enqueues normally. The check is
// **that it really ends up in the queue**.
//
// Mutation check: remove the `if err == nil && row.ID == ""` retry in EnqueueAgentSync
// -> this case fails.
func TestAgentSyncEnqueueRetriesNarrowWindow(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newChatService(t)

	gap := &narrowWindowStore{Store: store}
	svc.store = gap

	a, err := store.GetAgent(ctx, seedChatAgent)
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	row := svc.EnqueueAgentSync(ctx, a)

	if gap.calls != 2 {
		t.Errorf("撞上窄窗口该重试一次（共 2 次调用），实际 %d 次", gap.calls)
	}
	if row.ID == "" {
		t.Fatal("重试之后该拿到一条任务——拿不到意味着这次保存的改动悄悄不同步了，" +
			"且零报错、零日志")
	}
	if _, err := svc.LatestAgentSyncTask(ctx, seedChatAgent); err != nil {
		t.Errorf("重试之后该能查到任务：%v", err)
	}
}

// narrowWindowStore makes the first enqueue fall into that narrow window (zero value + no
// error), and works normally after that.
type narrowWindowStore struct {
	Store
	calls int
}

func (s *narrowWindowStore) EnqueueAgentSyncTask(
	ctx context.Context, task persistmodel.AgentSyncTaskRow,
) (persistmodel.AgentSyncTaskRow, bool, error) {
	s.calls++
	if s.calls == 1 {
		// Neither statement matched: nothing was created, and nothing running to mark.
		return persistmodel.AgentSyncTaskRow{}, false, nil
	}
	return s.Store.EnqueueAgentSyncTask(ctx, task)
}

// TestArkSyncStageOfManagedAgentNotOpen: Managed Agents not activated must go to the
// credential stage (an account/credential problem), and must not fall into the agent stage
// just because the arkFailure message does not mention "runtime environment".
func TestArkSyncStageOfManagedAgentNotOpen(t *testing.T) {
	err := arkFailure("准备方舟运行环境失败", &ma.APIError{
		Code:           "ManagedAgentNotOpen",
		Message:        "Your account 2100000001 has not activated the managed agent.",
		HTTPStatusCode: http.StatusNotFound,
	})
	if got := arkSyncStageOf(err); got != apimodel.AgentSyncStageCredential {
		t.Errorf("NotOpen 应归 credential 档，实际 %q", got)
	}
}
