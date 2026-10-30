// Entity history revisions: recording, rollback, and dropping past the limit.
//
// This group of cases watches three things that "give no error when written wrong", which
// are also the three mutation-check targets of this feature:
//
//  1. **Rollback must not restore the monotonic counters too** (Agent.Version /
//     Skill.LatestVersion). Done wrong, the version number goes backwards while the UI is
//     showing it.
//  2. **Past the limit, old ones are dropped**, or history in the database grows forever;
//     users explicitly asked for this.
//  3. **A rollback itself also creates a snapshot**, or there is nothing to "undo this
//     rollback" with, and picking the wrong version is exactly a second mistake while
//     recovering from a first one.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
	agentsvc "github.com/chyroc/ama/server/service/agent"
)

// baseAgentDraft builds a full draft.
//
// **Every field gets a non-zero value**: a field left at zero is useless in assertions like
// "did the rollback really change it back", because it is zero both before and after, and
// that assertion is always true (same as the rule "fixture fields must all differ from each
// other" in CLAUDE.md).
func baseAgentDraft(name string) apimodel.AgentDraft {
	return apimodel.AgentDraft{
		Name:           name,
		Description:    "描述-" + name,
		System:         "系统提示词-" + name,
		Model:          apimodel.ModelConfig{ID: "doubao-" + name},
		BuiltinTools:   []string{"bash"},
		SkillIDs:       []string{},
		FeishuAppIDs:   []string{},
		Status:         apimodel.StatusActive,
		IntegrationIDs: []string{},
		Envs:           []apimodel.AgentEnv{},
		EnvAliases:     []apimodel.AgentEnvAlias{},
		FollowUp:       apimodel.FollowUpRedirect,
		ProgressDetail: apimodel.ProgressDetailFull,
	}
}

// ---------------------------------------------------------------------------
// 1. Recording
// ---------------------------------------------------------------------------

// TestUpdateAgentRecordsRevision: editing an agent must keep how it looked **before** the edit.
func TestUpdateAgentRecordsRevision(t *testing.T) {
	svc, _ := newService(t)
	ctx := asActor("alice@example.com")

	created, err := svc.CreateAgent(ctx, seedWS, baseAgentDraft("v1"))
	if err != nil {
		t.Fatalf("建 agent 失败：%v", err)
	}
	// There should be no history at creation; at that moment there is no "before".
	list, err := svc.ListRevisions(ctx, apimodel.RevisionKindAgent, created.ID)
	if err != nil {
		t.Fatalf("列历史失败：%v", err)
	}
	if len(list) != 0 {
		t.Fatalf("刚建出来不该有历史，实际 %d 条", len(list))
	}

	if _, err := svc.UpdateAgent(ctx, created.ID, baseAgentDraft("v2")); err != nil {
		t.Fatalf("改 agent 失败：%v", err)
	}

	list, err = svc.ListRevisions(ctx, apimodel.RevisionKindAgent, created.ID)
	if err != nil {
		t.Fatalf("列历史失败：%v", err)
	}
	if len(list) != 1 {
		t.Fatalf("改了一次应有 1 条历史，实际 %d 条", len(list))
	}
	// The snapshot stores the version **before** the edit. Storing the one after is a very
	// natural slip (calling recordRevision after the assignment), and then rolling back does
	// nothing.
	rev, err := svc.GetRevision(ctx, apimodel.RevisionKindAgent, created.ID, list[0].ID)
	if err != nil {
		t.Fatalf("取单条历史失败：%v", err)
	}
	var snap apimodel.AgentDraft
	if err := json.Unmarshal(rev.Snapshot, &snap); err != nil {
		t.Fatalf("快照反序列化失败：%v", err)
	}
	if snap.Name != "v1" {
		t.Errorf("快照应是改动**前**的 v1，实际 %q", snap.Name)
	}
	if snap.System != "系统提示词-v1" {
		t.Errorf("快照的提示词应是 v1 那份，实际 %q", snap.System)
	}
	if rev.Reason != apimodel.RevisionReasonEdit {
		t.Errorf("普通编辑的 reason 应为 edit，实际 %q", rev.Reason)
	}
	if rev.CreatedBy != "alice@example.com" {
		t.Errorf("createdBy 应记录改的人，实际 %q", rev.CreatedBy)
	}
}

// TestUpdateAgentSkipsRevisionWhenUnchanged: no history is recorded when nothing changed.
//
// The frontend save button sends back the full draft, so "open the detail page and just click
// save" reaches UpdateAgent. Without this check, those empty saves would fill up the
// 20-entry limit for nothing and push out the version you actually want, which destroys the
// very reason this feature exists.
func TestUpdateAgentSkipsRevisionWhenUnchanged(t *testing.T) {
	svc, _ := newService(t)
	ctx := asActor("alice@example.com")

	created, err := svc.CreateAgent(ctx, seedWS, baseAgentDraft("v1"))
	if err != nil {
		t.Fatalf("建 agent 失败：%v", err)
	}
	// Save it unchanged three more times.
	for i := 0; i < 3; i++ {
		if _, err := svc.UpdateAgent(ctx, created.ID, baseAgentDraft("v1")); err != nil {
			t.Fatalf("第 %d 次空保存失败：%v", i+1, err)
		}
	}
	list, _ := svc.ListRevisions(ctx, apimodel.RevisionKindAgent, created.ID)
	if len(list) != 0 {
		t.Fatalf("内容没变不该记历史，实际 %d 条", len(list))
	}
}

// TestUpdateAgentRecordsWhenOnlySliceOrderChanges: it only counts as changed if it still
// differs after normalization.
//
// ⚠️ This watches the "normalize both sides" check in sameAgentDraft in agentsvc: if only one
// side is normalized, the `[]` in the database and the `nil` in the request body are judged
// "changed", so every save records one entry. The symptom is the same as "no check at all",
// with zero errors.
func TestUpdateAgentTreatsNilAndEmptySliceAsSame(t *testing.T) {
	svc, _ := newService(t)
	ctx := asActor("alice@example.com")

	created, err := svc.CreateAgent(ctx, seedWS, baseAgentDraft("v1"))
	if err != nil {
		t.Fatalf("建 agent 失败：%v", err)
	}
	// Old frontends do not send these keys, so they decode to nil; the database stores [].
	draft := baseAgentDraft("v1")
	draft.SkillIDs = nil
	draft.FeishuAppIDs = nil
	draft.IntegrationIDs = nil
	draft.Envs = nil
	draft.EnvAliases = nil
	if _, err := svc.UpdateAgent(ctx, created.ID, draft); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	list, _ := svc.ListRevisions(ctx, apimodel.RevisionKindAgent, created.ID)
	if len(list) != 0 {
		t.Fatalf("nil 与空切片是同一个意思，不该记历史，实际 %d 条", len(list))
	}
}

// ---------------------------------------------------------------------------
// 2. Rollback (including the hard rule "must not rewind the counters")
// ---------------------------------------------------------------------------

// TestRollbackAgentRestoresContent: rollback must change the content back.
func TestRollbackAgentRestoresContent(t *testing.T) {
	svc, _ := newService(t)
	ctx := asActor("alice@example.com")

	created, _ := svc.CreateAgent(ctx, seedWS, baseAgentDraft("v1"))
	if _, err := svc.UpdateAgent(ctx, created.ID, baseAgentDraft("v2")); err != nil {
		t.Fatalf("改 agent 失败：%v", err)
	}
	list, _ := svc.ListRevisions(ctx, apimodel.RevisionKindAgent, created.ID)
	if len(list) != 1 {
		t.Fatalf("应有 1 条历史，实际 %d", len(list))
	}

	out, err := svc.RollbackRevision(ctx, apimodel.RevisionKindAgent, created.ID, list[0].ID)
	if err != nil {
		t.Fatalf("回滚失败：%v", err)
	}
	got, ok := out.(apimodel.Agent)
	if !ok {
		t.Fatalf("回滚应返回 apimodel.Agent，实际 %T", out)
	}
	if got.Name != "v1" {
		t.Errorf("回滚后名字应是 v1，实际 %q", got.Name)
	}
	if got.System != "系统提示词-v1" {
		t.Errorf("回滚后提示词应是 v1 那份，实际 %q", got.System)
	}
	if got.Model.ID != "doubao-v1" {
		t.Errorf("回滚后模型应是 v1 那个，实际 %q", got.Model.ID)
	}
}

// TestRollbackAgentDoesNotRewindVersion: rollback must **not** make Version go backwards.
//
// # ⚠️ This is mutation-check target (1)
//
// Agent.Version is a monotonic counter, shown in the UI as a `v7` badge. After rolling back
// to version 3 it must keep moving forward (7 → 8), not be restored to 3.
//
// It is already protected by structure: the snapshot stores an AgentDraft, and that type
// **has no Version field at all**, so "restore it too" cannot even be written at the type
// level. This case pins that structure: if someone changes the snapshot to store the whole
// apimodel.Agent and writes it all back on rollback, this fails right away.
//
// ⚠️ Also correcting a common belief: Agent.Version is **not** Ark's optimistic lock (that one
// is read fresh from Ark every time, see updateAgent in core/arkrun.go). The reason to protect
// it does not go away, it just becomes "it is a monotonic counter and the UI shows it".
// Skill.LatestVersion is a harder case: that value really is sent to Ark (skill references
// carry a version number).
func TestRollbackAgentDoesNotRewindVersion(t *testing.T) {
	svc, _ := newService(t)
	ctx := asActor("alice@example.com")

	created, _ := svc.CreateAgent(ctx, seedWS, baseAgentDraft("v1"))
	// Edit three times to push Version up.
	for i := 2; i <= 4; i++ {
		if _, err := svc.UpdateAgent(ctx, created.ID, baseAgentDraft(fmt.Sprintf("v%d", i))); err != nil {
			t.Fatalf("第 %d 次改动失败：%v", i, err)
		}
	}
	before, _ := svc.GetAgent(ctx, created.ID)
	if before.Version <= created.Version {
		t.Fatalf("前置条件不成立：改了三次 Version 应该涨了，%d → %d",
			created.Version, before.Version)
	}

	// Roll back to the earliest one (content = v1).
	list, _ := svc.ListRevisions(ctx, apimodel.RevisionKindAgent, created.ID)
	oldest := list[len(list)-1]
	out, err := svc.RollbackRevision(ctx, apimodel.RevisionKindAgent, created.ID, oldest.ID)
	if err != nil {
		t.Fatalf("回滚失败：%v", err)
	}
	got := out.(apimodel.Agent)

	// The content went back...
	if got.Name != "v1" {
		t.Fatalf("前置条件不成立：回滚后内容应是 v1，实际 %q", got.Name)
	}
	// ...but the counter must keep moving forward.
	if got.Version <= before.Version {
		t.Errorf("回滚后 Version 必须继续递增（%d → 应 > %d），实际 %d——"+
			"版本号倒退会让界面上的 v 徽章往回跳", before.Version, before.Version, got.Version)
	}
}

// TestRollbackSkillDoesNotRewindLatestVersion: same as above, for the skill's **string**
// counter.
//
// ⚠️ Skill.LatestVersion and Agent.Version have opposite types (string vs int), the pair in
// this repo most easily mixed up. And it matters more than Agent.Version: that value really
// is sent to Ark (skill references carry a version number, see toMASkillRefs in arkrun.go),
// and going back to an old value makes Ark reference a skill version that is no longer the
// latest.
func TestRollbackSkillDoesNotRewindLatestVersion(t *testing.T) {
	svc, _ := newService(t)
	ctx := asActor("alice@example.com")

	created, err := svc.CreateSkill(ctx, seedWS, apimodel.SkillDraft{
		Name: "writer", DisplayTitle: "写手", Instructions: "第一版", Tags: []string{"a"},
	})
	if err != nil {
		t.Fatalf("建技能失败：%v", err)
	}
	for i := 2; i <= 4; i++ {
		_, err := svc.UpdateSkill(ctx, created.ID, apimodel.SkillDraft{
			Name: "writer", DisplayTitle: "写手", Tags: []string{"a"},
			Instructions: fmt.Sprintf("第 %d 版", i),
		})
		if err != nil {
			t.Fatalf("第 %d 次改动失败：%v", i, err)
		}
	}
	before, _ := svc.GetSkill(ctx, created.ID)

	list, _ := svc.ListRevisions(ctx, apimodel.RevisionKindSkill, created.ID)
	oldest := list[len(list)-1]
	out, err := svc.RollbackRevision(ctx, apimodel.RevisionKindSkill, created.ID, oldest.ID)
	if err != nil {
		t.Fatalf("回滚失败：%v", err)
	}
	got := out.(apimodel.Skill)

	if got.Instructions != "第一版" {
		t.Fatalf("前置条件不成立：回滚后正文应是第一版，实际 %q", got.Instructions)
	}
	// String comparison does not work; compare as numbers.
	beforeN, _ := strconv.Atoi(before.LatestVersion)
	afterN, _ := strconv.Atoi(got.LatestVersion)
	if afterN <= beforeN {
		t.Errorf("回滚后 LatestVersion 必须继续递增（%q → 应 > 它），实际 %q——"+
			"这个值会发给方舟，退回旧值会让它引用到过期的技能版本",
			before.LatestVersion, got.LatestVersion)
	}
}

// TestRollbackAlsoRecordsRevision: a rollback **itself** must also keep a snapshot.
//
// # ⚠️ This is mutation-check target (3)
//
// Without it there is nothing to "undo this rollback" with, and picking the wrong version is
// exactly a second mistake while recovering from a first one. That snapshot's reason is
// rollback, which the UI uses to tell it apart from normal edits.
func TestRollbackAlsoRecordsRevision(t *testing.T) {
	svc, _ := newService(t)
	ctx := asActor("alice@example.com")

	created, _ := svc.CreateAgent(ctx, seedWS, baseAgentDraft("v1"))
	if _, err := svc.UpdateAgent(ctx, created.ID, baseAgentDraft("v2")); err != nil {
		t.Fatalf("改 agent 失败：%v", err)
	}
	list, _ := svc.ListRevisions(ctx, apimodel.RevisionKindAgent, created.ID)
	if len(list) != 1 {
		t.Fatalf("前置条件不成立：应有 1 条历史，实际 %d", len(list))
	}

	if _, err := svc.RollbackRevision(ctx, apimodel.RevisionKindAgent, created.ID, list[0].ID); err != nil {
		t.Fatalf("回滚失败：%v", err)
	}

	after, _ := svc.ListRevisions(ctx, apimodel.RevisionKindAgent, created.ID)
	if len(after) != 2 {
		t.Fatalf("回滚要留下「回滚前的样子」，应有 2 条历史，实际 %d 条——"+
			"少了这条，用户挑错版本之后就回不去了", len(after))
	}
	newest := after[0]
	if newest.Reason != apimodel.RevisionReasonRollback {
		t.Errorf("回滚产生的那条 reason 应为 rollback，实际 %q——"+
			"不标出来的话历史里会多一条看不出来历的记录", newest.Reason)
	}
	// That snapshot should store the content **before the rollback** (v2).
	full, err := svc.GetRevision(ctx, apimodel.RevisionKindAgent, created.ID, newest.ID)
	if err != nil {
		t.Fatalf("取快照失败：%v", err)
	}
	var snap apimodel.AgentDraft
	if err := json.Unmarshal(full.Snapshot, &snap); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if snap.Name != "v2" {
		t.Errorf("回滚快照应记录回滚**前**的 v2，实际 %q", snap.Name)
	}

	// Roll back to that one again and we should be back at v2; that is what "undoing the
	// rollback just now" means.
	out, err := svc.RollbackRevision(ctx, apimodel.RevisionKindAgent, created.ID, newest.ID)
	if err != nil {
		t.Fatalf("二次回滚失败：%v", err)
	}
	if got := out.(apimodel.Agent); got.Name != "v2" {
		t.Errorf("撤销回滚后应回到 v2，实际 %q", got.Name)
	}
}

// TestRollbackScheduleRecomputesNextRun: rolling back to an old spec must recompute the next
// fire time.
//
// Rollback goes through UpdateSchedule, where `specChanged` triggers a recompute, and
// **rolling back to an old spec is a spec change**. Without the recompute the task keeps
// running on the current expression while the UI shows the old one.
func TestRollbackScheduleRecomputesNextRun(t *testing.T) {
	svc, _ := newService(t)
	ctx := asActor("alice@example.com")

	created, err := svc.CreateSchedule(ctx, seedWS, apimodel.ScheduleDraft{
		AgentID: "agent_triage", Name: "巡检",
		Spec:     apimodel.ScheduleSpec{Kind: apimodel.ScheduleInterval, EveryMs: 3_600_000},
		Prompt:   "看看 CI",
		Delivery: apimodel.DeliveryTarget{Kind: apimodel.DeliveryRunOnly},
	})
	if err != nil {
		t.Fatalf("建任务失败：%v", err)
	}
	// Change it to every 5 minutes.
	if _, err := svc.UpdateSchedule(ctx, created.ID, apimodel.ScheduleDraft{
		AgentID: "agent_triage", Name: "巡检",
		Spec:     apimodel.ScheduleSpec{Kind: apimodel.ScheduleInterval, EveryMs: 300_000},
		Prompt:   "看看 CI",
		Delivery: apimodel.DeliveryTarget{Kind: apimodel.DeliveryRunOnly},
	}); err != nil {
		t.Fatalf("改任务失败：%v", err)
	}

	list, _ := svc.ListRevisions(ctx, apimodel.RevisionKindSchedule, created.ID)
	if len(list) != 1 {
		t.Fatalf("应有 1 条历史，实际 %d", len(list))
	}
	out, err := svc.RollbackRevision(ctx, apimodel.RevisionKindSchedule, created.ID, list[0].ID)
	if err != nil {
		t.Fatalf("回滚失败：%v", err)
	}
	got := out.(apimodel.Schedule)
	if got.Spec.EveryMs != 3_600_000 {
		t.Errorf("回滚后间隔应回到 60 分钟，实际 %d ms", got.Spec.EveryMs)
	}
	if got.NextRunAt == "" {
		t.Error("回滚一个旧 spec 之后必须重算 NextRunAt，实际是空的——" +
			"空的意味着这条任务再也不会被认领")
	}
	// Runtime counters must not be brought back by the snapshot (the snapshot does not have
	// them at all).
	if got.RunCount != 0 || got.ConsecutiveFailures != 0 {
		t.Errorf("运行时计数器不该被回滚影响，实际 runCount=%d consecutive=%d",
			got.RunCount, got.ConsecutiveFailures)
	}
}

// ---------------------------------------------------------------------------
// 3. Dropping past the limit
// ---------------------------------------------------------------------------

// TestPurgeExcessRevisionsKeepsNewest: past the limit, the oldest ones must be dropped.
//
// # ⚠️ This is mutation-check target (2)
//
// Users explicitly asked to "limit the max number of versions per entity, so the database
// does not keep collecting junk". There are two checks, and missing either one means the
// feature is broken: **the ones kept are the newest N** (not the oldest), and **other
// entities are not affected** (grouped per entity, not N for the whole table).
func TestPurgeExcessRevisionsKeepsNewest(t *testing.T) {
	svc, _ := newService(t)
	ctx := asActor("alice@example.com")

	a, _ := svc.CreateAgent(ctx, seedWS, baseAgentDraft("a1"))
	b, _ := svc.CreateAgent(ctx, seedWS, baseAgentDraft("b1"))

	// a edited 6 times -> 6 history entries; b edited 2 times -> 2.
	for i := 2; i <= 7; i++ {
		if _, err := svc.UpdateAgent(ctx, a.ID, baseAgentDraft(fmt.Sprintf("a%d", i))); err != nil {
			t.Fatalf("改 a 第 %d 次失败：%v", i, err)
		}
	}
	for i := 2; i <= 3; i++ {
		if _, err := svc.UpdateAgent(ctx, b.ID, baseAgentDraft(fmt.Sprintf("b%d", i))); err != nil {
			t.Fatalf("改 b 第 %d 次失败：%v", i, err)
		}
	}
	if list, _ := svc.ListRevisions(ctx, apimodel.RevisionKindAgent, a.ID); len(list) != 6 {
		t.Fatalf("前置条件不成立：a 应有 6 条历史，实际 %d", len(list))
	}

	// Keep only 3 per entity.
	n, err := svc.store.PurgeExcessRevisions(ctx, 3)
	if err != nil {
		t.Fatalf("清理失败：%v", err)
	}
	if n != 3 {
		t.Errorf("a 有 6 条留 3 条，应删 3 条，实际删了 %d 条", n)
	}

	got, _ := svc.ListRevisions(ctx, apimodel.RevisionKindAgent, a.ID)
	if len(got) != 3 {
		t.Fatalf("清理后 a 应剩 3 条，实际 %d 条——上限没生效，库里会一直长", len(got))
	}
	// **The ones kept must be the newest three**: keeping the oldest three is a wrong
	// direction (order by reversed), and it still "has a limit", it just keeps the wrong
	// ones. What users want to find is always the last few versions.
	for i, rev := range got {
		wantNo := 6 - i // newest is 6, then 5, 4
		if rev.No != wantNo {
			t.Errorf("第 %d 条应是 revision_no=%d（最新的那几条），实际 %d",
				i, wantNo, rev.No)
		}
	}
	// b has only 2 and should not be touched. **Grouping per entity** is the check here: if
	// written as keeping 3 for the whole table, one agent edited often would push out all
	// other entities' history, with no error.
	gotB, _ := svc.ListRevisions(ctx, apimodel.RevisionKindAgent, b.ID)
	if len(gotB) != 2 {
		t.Errorf("b 只有 2 条、没到上限，不该被清理，实际剩 %d 条——"+
			"清理必须按实体分组，不是全表留 N 条", len(gotB))
	}
}

// TestPurgeExcessRevisionsRefusesZeroKeep: with keep=0, **delete nothing**.
//
// Misconfiguring it as 0 is possible, and "keep=0 -> wipe all history" cannot be undone.
// Better not to delete.
func TestPurgeExcessRevisionsRefusesZeroKeep(t *testing.T) {
	svc, _ := newService(t)
	ctx := asActor("alice@example.com")

	a, _ := svc.CreateAgent(ctx, seedWS, baseAgentDraft("v1"))
	if _, err := svc.UpdateAgent(ctx, a.ID, baseAgentDraft("v2")); err != nil {
		t.Fatalf("改 agent 失败：%v", err)
	}

	n, err := svc.store.PurgeExcessRevisions(ctx, 0)
	if err != nil {
		t.Fatalf("清理失败：%v", err)
	}
	if n != 0 {
		t.Errorf("keep=0 应什么都不删，实际删了 %d 条", n)
	}
	if list, _ := svc.ListRevisions(ctx, apimodel.RevisionKindAgent, a.ID); len(list) != 1 {
		t.Errorf("历史应原样保留 1 条，实际 %d 条", len(list))
	}
}

// ---------------------------------------------------------------------------
// 4. Isolation and edge cases
// ---------------------------------------------------------------------------

// TestGetRevisionRejectsForeignEntity: a snapshot id of another entity reads nothing.
//
// ⚠️ Without the check, anyone holding a rev_ id could read the full system prompt of some
// agent in another workspace. Authorization works out the workspace from entityID, and the
// snapshot id itself takes no part in that check. This would be an unauthorized read with
// zero errors.
func TestGetRevisionRejectsForeignEntity(t *testing.T) {
	svc, _ := newService(t)
	ctx := asActor("alice@example.com")

	a, _ := svc.CreateAgent(ctx, seedWS, baseAgentDraft("a1"))
	b, _ := svc.CreateAgent(ctx, seedWS, baseAgentDraft("b1"))
	if _, err := svc.UpdateAgent(ctx, a.ID, baseAgentDraft("a2")); err != nil {
		t.Fatalf("改 a 失败：%v", err)
	}
	list, _ := svc.ListRevisions(ctx, apimodel.RevisionKindAgent, a.ID)

	// Read a's snapshot id under b.
	if _, err := svc.GetRevision(ctx, apimodel.RevisionKindAgent, b.ID, list[0].ID); err == nil {
		t.Error("拿别的实体的快照 id 应该读不出来，实际成功了——这是越权读")
	}
	// Same for rollback.
	if _, err := svc.RollbackRevision(ctx, apimodel.RevisionKindAgent, b.ID, list[0].ID); err == nil {
		t.Error("拿别的实体的快照 id 应该回滚不了，实际成功了")
	}
}

// TestListRevisionsOnMissingEntity: a missing entity must return 404, not an empty list.
//
// An empty list would make "this agent was deleted" and "it has no history yet" look the
// same.
func TestListRevisionsOnMissingEntity(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()

	if _, err := svc.ListRevisions(ctx, apimodel.RevisionKindAgent, "agent_nope"); err == nil {
		t.Error("实体不存在应回错误（→404），实际回了空列表")
	}
}

// TestDeleteAgentDropsRevisions: deleting an agent must also clear its history.
func TestDeleteAgentDropsRevisions(t *testing.T) {
	svc, store := newService(t)
	ctx := asActor("alice@example.com")

	a, _ := svc.CreateAgent(ctx, seedWS, baseAgentDraft("v1"))
	if _, err := svc.UpdateAgent(ctx, a.ID, baseAgentDraft("v2")); err != nil {
		t.Fatalf("改 agent 失败：%v", err)
	}
	if err := svc.DeleteAgent(ctx, a.ID); err != nil {
		t.Fatalf("删 agent 失败：%v", err)
	}
	// The entity is gone and ListRevisions would return 404, so ask the Store directly.
	left, err := store.ListEntityRevisions(ctx, apimodel.RevisionKindAgent, a.ID, 0)
	if err != nil {
		t.Fatalf("查历史失败：%v", err)
	}
	if len(left) != 0 {
		t.Errorf("agent 删了它的历史也该清掉，实际残留 %d 条", len(left))
	}
}

// TestBuiltinSkillHasNoRevisions: built-in skills have no history, and must not be reported
// as "does not exist".
//
// It is visible in the list, so returning 404 is wrong (same as the UpdateSkill case).
func TestBuiltinSkillHasNoRevisions(t *testing.T) {
	svc, _ := newService(t)
	ctx := asActor("alice@example.com")

	builtins := BuiltinSkills()
	if len(builtins) == 0 {
		t.Skip("没有内置技能，跳过")
	}
	_, err := svc.ListRevisions(ctx, apimodel.RevisionKindSkill, builtins[0].ID)
	if err == nil {
		t.Fatal("内置技能不该有历史，应回错误")
	}
	if !errors.Is(err, ErrPrecondition) {
		t.Errorf("应回 ErrPrecondition（它在列表里看得见，说「不存在」是错的），实际 %v", err)
	}
}

// ---------------------------------------------------------------------------
// 5. Snapshot field coverage (guards against "added a field but forgot to make it rollbackable")
// ---------------------------------------------------------------------------

// TestSnapshotCoversEveryDraftField: the snapshot must cover every field of the Draft.
//
// # Why this is written with reflection
//
// agentsvc.Snapshot / snapshotSkill / schedulesvc.Snapshot copy field by field by hand. When
// adding a field to an entity, it is natural to change the Draft, UpdateX and the frontend,
// **and then forget one of these three functions**. The result of that miss is "after a
// rollback that field stays as it is": no error, no crash, and it only shows months later
// when someone really rolls back, when it is also hard to guess that the snapshot missed a
// field.
//
// The check: run an entity with **every field non-zero** through snapshotX, and no field of
// the resulting Draft may still be zero. A zero value means that field was not copied.
//
// ⚠️ This only proves "the field was copied", not "copied correctly" (for example copying
// Name into Description). The per-field assertion cases above cover the latter.
func TestSnapshotCoversEveryDraftField(t *testing.T) {
	t.Run("agent", func(t *testing.T) {
		full := apimodel.Agent{
			Name: "n", Description: "d", System: "s",
			Model:          apimodel.ModelConfig{ID: "m"},
			BuiltinTools:   []string{"bash"},
			SkillIDs:       []string{"sk"},
			FeishuAppIDs:   []string{"fs"},
			Status:         apimodel.StatusActive,
			IntegrationIDs: []string{"lark-user"},
			Envs:           []apimodel.AgentEnv{{Key: "K", Value: "V"}},
			EnvAliases:     []apimodel.AgentEnvAlias{{Key: "A", From: "lark-user"}},
			FollowUp:       apimodel.FollowUpQueue,
			ProgressDetail: apimodel.ProgressDetailFull,
			// The three executor fields must also be snapshotted with the version and be
			// rollbackable.
			ExecutorType: "host",
			HostID:       "hst_test",
			HostSource:   string(apimodel.HostSourcePersonal),
		}
		assertNoZeroField(t, agentsvc.Snapshot(full))
	})

	t.Run("skill", func(t *testing.T) {
		full := apimodel.Skill{
			Name: "n", DisplayTitle: "t", Description: "d",
			Instructions: "i", Tags: []string{"x"},
		}
		assertNoZeroField(t, snapshotSkill(full))
	})

	t.Run("schedule", func(t *testing.T) {
		full := apimodel.Schedule{
			AgentID: "a", Name: "n",
			Spec:           apimodel.ScheduleSpec{Kind: apimodel.ScheduleInterval, EveryMs: 60_000},
			Prompt:         "p",
			ContextSummary: "c",
			Delivery:       apimodel.DeliveryTarget{Kind: apimodel.DeliveryRunOnly},
			Status:         apimodel.ScheduleActive,
		}
		assertNoZeroField(t, snapshotSchedule(full))
	})
}

// assertNoZeroField asserts that no field of the struct is a zero value.
func assertNoZeroField(t *testing.T, v any) {
	t.Helper()
	rv := reflect.ValueOf(v)
	rt := rv.Type()
	for i := 0; i < rv.NumField(); i++ {
		if rv.Field(i).IsZero() {
			t.Errorf("%s.%s 是零值——snapshot 函数漏抄了这个字段，"+
				"回滚时它会保持现状而不是被还原（且不报错）",
				rt.Name(), rt.Field(i).Name)
		}
	}
}

// TestRevisionSnapshotSerializesAsJSONObject: the snapshot in the HTTP response must be an
// **object**, not a base64 string.
//
// # ⚠️ This one was added later, because all the cases above missed it
//
// If the type of `Snapshot` is `[]byte` (instead of `json.RawMessage`), `encoding/json`
// **base64-encodes** it. The frontend gets a string like `"eyJuYW1lIjoi…"`, whose `typeof` is
// string, not object, and "expand to view this version" renders a mess.
//
// Both types are `[]byte` underneath, **assignable to each other and indistinguishable to
// the compiler**, so it is easy to get wrong. And every case above checks via
// `json.Unmarshal(rev.Snapshot, &draft)`, which behaves exactly the same for both types and
// **happens to hide the difference**, so everything passes.
//
// What actually caught it was hitting the real service with curl. This case locks in that
// manual check: the check is on **the JSON shape after serialization**, not on field values
// on the Go side.
func TestRevisionSnapshotSerializesAsJSONObject(t *testing.T) {
	rev := apimodel.Revision{
		ID:       "rev_1",
		Kind:     apimodel.RevisionKindAgent,
		EntityID: "agent_1",
		Snapshot: []byte(`{"name":"x"}`),
	}
	blob, err := json.Marshal(rev)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}

	var back map[string]any
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	got, ok := back["snapshot"]
	if !ok {
		t.Fatal("响应里没有 snapshot 字段")
	}
	if _, isString := got.(string); isString {
		t.Fatalf("snapshot 被编码成了字符串（多半是 base64）：%v\n"+
			"→ 字段类型要用 json.RawMessage，不能用 []byte。"+
			"前端拿到字符串会把「看这一版」渲染成乱码，而 Go 侧的用例全是绿的。", got)
	}
	obj, isObject := got.(map[string]any)
	if !isObject {
		t.Fatalf("snapshot 应该是一个 JSON 对象，实际 %T", got)
	}
	if obj["name"] != "x" {
		t.Errorf("snapshot 内容不对：%v", obj)
	}
}
