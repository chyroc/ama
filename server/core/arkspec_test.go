// Tests for the catch-up path "when an agent's system / model changes, the Ark side must change
// with it".
//
// # The shape of this bug: all unit tests green, permanently broken in production
//
// An Ark Agent is created once and cached forever (ensureBinding returns right away when it hits
// an existing binding), so if the user changes the system prompt after the first chat, the Ark
// side never knows. Catch-up was added for **skills** back then (syncBindingSkills), but the same
// statement holds word for word for system / model, and that half was never done.
//
// It is harder to track down than the skills one:
//
//   - **no error at all**: PATCH returns 200 and the database really changed; only the Ark side
//     did not move;
//   - **permanent**: on the skills side, any add/remove changes the fingerprint and heals it as a
//     side effect, while nothing else ever touches system; change it a hundred times, same result;
//   - **the error does not point at the real cause**: users think the model is not listening or
//     the prompt is badly written, so they keep rewriting the prompt.
//
// # So the assertions here are deliberately pinned on "what the Ark side received", not "the function was called"
//
// The lesson from the skills case was that **all assertions were upstream of the bug, and the gap
// sat right in the middle**. This file is the core-side half (catch-up logic and fingerprint
// meaning); the wire-side half is in arkrun_test.go. Both are required, because the core-side fake
// receives an ArkAgentSpec, and filling it correctly does not mean the ma layer passed it on.

package core

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
	"github.com/chyroc/ama/server/model/persistmodel"
	"github.com/chyroc/ama/server/repository/dao/coremem"
	"github.com/chyroc/ama/server/shared/infra/ma"
)

// setAgentSystem changes the system prompt of that agent directly in the database.
//
// It goes through store instead of Service.UpdateAgent: that method needs a complete draft, and
// these tests only care about "what happens when a chat starts after system changed".
func setAgentSystem(t *testing.T, store *coremem.MemStore, agentID, system string) {
	t.Helper()
	ctx := context.Background()
	a, err := store.GetAgent(ctx, agentID)
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	a.System = system
	if _, err := store.SaveAgent(ctx, a); err != nil {
		t.Fatalf("存 agent 失败：%v", err)
	}
}

// TestArkSpecSyncedWhenBindingAlreadyExists is the regression test for **the symptom being fixed
// this time**.
//
// The scenario is the four steps walked through in a real test: create agent (system=V1) → start
// a chat (the Ark Agent is only created now) → change to V2 → start another new chat. It was
// confirmed that before the fix the Ark side was still V1, with version stuck at 1.
//
// Mutation check: change `return s.syncBindingSpec(...)` in ensureBinding back to
// `return b, nil` → this test fails.
func TestArkSpecSyncedWhenBindingAlreadyExists(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	setAgentSystem(t, store, seedChatAgent, "SYSTEM-V1")
	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("第一次开对话失败：%v", err)
	}
	// The first one was carried by CreateAgent; there should be no update.
	if len(runner.updatedSpecs) != 0 {
		t.Fatalf("建 Agent 那次就把 spec 带上了，不该再 update，实际 %d 次", len(runner.updatedSpecs))
	}
	if !strings.Contains(runner.spec.System, "SYSTEM-V1") {
		t.Fatalf("建 Agent 时的 system 里应当有 SYSTEM-V1，实际 %q", runner.spec.System)
	}

	// Change the prompt, then start another new chat.
	setAgentSystem(t, store, seedChatAgent, "SYSTEM-V2")
	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("再开对话失败：%v", err)
	}

	if len(runner.updatedSpecs) != 1 {
		t.Fatalf("改了 system 之后再开对话应当追平一次，实际 update %d 次——"+
			"0 次意味着用户改了提示词而方舟侧永远是旧那份，且零报错", len(runner.updatedSpecs))
	}
	// Assert **the content Ark received**, not just "it was called": counting calls alone would
	// let an implementation that "calls but still sends the old system" pass, which is the same as
	// not fixing it.
	if got := runner.updatedSpecs[0].System; !strings.Contains(got, "SYSTEM-V2") {
		t.Errorf("追平时发给方舟的 system 里应当有 SYSTEM-V2，实际 %q", got)
	}
	if got := runner.updatedSpecs[0].System; strings.Contains(got, "SYSTEM-V1") {
		t.Errorf("追平时发给方舟的 system 里不该还留着 SYSTEM-V1，实际 %q", got)
	}
	// The Ark Agent must not be rebuilt (rebuilding replaces ArkAgentID, and old sessions would point at an Agent that no longer updates).
	if runner.createdAgents != 1 {
		t.Errorf("追平走的是 UpdateAgent，不该重建方舟 Agent，实际建了 %d 个", runner.createdAgents)
	}
}

// TestArkSpecSyncCarriesFullSystemNotJustUserText is **the easiest place to get wrong** this time.
//
// The system that goes into Ark is assembled: user text + self-wake instructions + the
// `<harness:env>` block (see arkSpecFor). Sending only `agent.System` during catch-up would
// **erase** the last two parts. That is a regression with no error: from then on the model does
// not know it can wake itself (when it has to wait for something, it sleeps in place until it is
// cut off), and it loses the runtime facts in env.
//
// And it is very hard to notice: the system does contain the user's text, so it looks like "the
// update worked".
//
// Mutation check: replace `arkSpecFor(agent)` in syncBindingSpec with
// `ArkAgentSpec{System: agent.System, Model: agent.Model.ID, Name: arkAgentName(agent)}`
// → this test fails (that mutation compiles cleanly).
func TestArkSpecSyncCarriesFullSystemNotJustUserText(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	// Give this agent a non-sensitive env, so the <harness:env> block really has content.
	a, err := store.GetAgent(ctx, seedChatAgent)
	if err != nil {
		t.Fatalf("取 agent 失败：%v", err)
	}
	a.System = "SYSTEM-V1"
	a.Envs = []apimodel.AgentEnv{{Key: "REGION", Value: "cn-north"}}
	if _, err := store.SaveAgent(ctx, a); err != nil {
		t.Fatalf("存 agent 失败：%v", err)
	}

	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("第一次开对话失败：%v", err)
	}
	setAgentSystem(t, store, seedChatAgent, "SYSTEM-V2")
	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("再开对话失败：%v", err)
	}

	if len(runner.updatedSpecs) != 1 {
		t.Fatalf("应当追平一次，实际 %d 次", len(runner.updatedSpecs))
	}
	got := runner.updatedSpecs[0].System

	// Both parts must be there: user text and the env block.
	if !strings.Contains(got, "SYSTEM-V2") {
		t.Errorf("用户正文丢了，system=%q", got)
	}
	if !strings.Contains(got, "REGION") || !strings.Contains(got, "cn-north") {
		t.Errorf("<harness:env> 块被抹掉了——模型拿不到运行时事实了。system=%q", got)
	}
}

// TestArkSpecNotResentWhenUnchanged pins down "if nothing changed, do not call Ark".
//
// Chat is a hot path. Calling UpdateAgent unconditionally every time would add a round trip to
// every session, and make the Ark-side Agent version keep growing for no reason.
func TestArkSpecNotResentWhenUnchanged(t *testing.T) {
	ctx := context.Background()
	svc, _, runner := newChatService(t)

	for i := 0; i < 3; i++ {
		if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
			t.Fatalf("第 %d 次开对话失败：%v", i+1, err)
		}
	}
	if len(runner.updatedSpecs) != 0 {
		t.Errorf("spec 没变就不该打方舟，实际 update %d 次（每次对话白加一个往返）",
			len(runner.updatedSpecs))
	}
}

// TestArkSpecFingerprintNotStoredWhenUpdateFails pins down "if the update did not succeed, do not
// write the fingerprint back".
//
// This is a known trap in this repo, showing up on the spec path: writing it back would make the
// next chat hit the fingerprint and return right away with **zero network calls**, so the Update
// that should correct it is never called. "Prompt change has no effect" then goes from "will be
// fine next time" to **permanent**, with no error at all.
//
// Mutation check: change syncBindingSpec to "write the fingerprint back whether Update succeeded
// or not" → this test fails.
func TestArkSpecFingerprintNotStoredWhenUpdateFails(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("第一次开对话失败：%v", err)
	}
	before, err := store.GetArkBinding(ctx, seedWS, seedChatAgent)
	if err != nil {
		t.Fatalf("取绑定失败：%v", err)
	}

	// Change system, and make catch-up fail.
	setAgentSystem(t, store, seedChatAgent, "SYSTEM-V2")
	runner.updateSpecErr = errors.New("方舟拒了")
	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("追平失败不该让对话开不出来：%v", err)
	}

	after, err := store.GetArkBinding(ctx, seedWS, seedChatAgent)
	if err != nil {
		t.Fatalf("取绑定失败：%v", err)
	}
	if after.SpecFingerprint != before.SpecFingerprint {
		t.Fatalf("更新失败时不该回写指纹（否则下次连重试都没有，故障变永久），"+
			"改前 %q 改后 %q", before.SpecFingerprint, after.SpecFingerprint)
	}

	// **And the next chat must really retry.** Checking only the previous point would let an
	// implementation that "never writes back the fingerprint but also never retries" pass, which is
	// the same as not fixing it.
	runner.updateSpecErr = nil
	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("再开对话失败：%v", err)
	}
	if len(runner.updatedSpecs) != 1 {
		t.Fatalf("上次失败了，这次必须重试一遍，实际成功 update %d 次", len(runner.updatedSpecs))
	}
	if got := runner.updatedSpecs[0].System; !strings.Contains(got, "SYSTEM-V2") {
		t.Errorf("重试时发的应当是新那份，实际 %q", got)
	}
	// This time it worked, so the fingerprint must be stored (otherwise every chat calls Ark for nothing).
	final, err := store.GetArkBinding(ctx, seedWS, seedChatAgent)
	if err != nil {
		t.Fatalf("取绑定失败：%v", err)
	}
	if final.SpecFingerprint == before.SpecFingerprint || final.SpecFingerprint == "" {
		t.Errorf("追平成功之后要回写指纹，实际 %q", final.SpecFingerprint)
	}
}

// TestArkSpecLegacyBindingHealsOnce pins down self-healing of existing bindings.
//
// Existing bindings (created before this change) have **no specFingerprint key at all** in doc,
// which deserializes to an empty string. The empty string must not equal any real fingerprint, so
// the next chat catches up once, exactly once. This is a self-healing approach that does not touch
// DDL (same as the skillsFingerprintVersion section).
//
// ⚠️ "Exactly once" is the point of this test: if ArkSpecFingerprint returned an empty string for
// some spec (e.g. an optimization like "return empty when all fields are empty"), that agent would
// be permanently misjudged as "caught up", or the other way around, every chat would call Ark for
// nothing.
func TestArkSpecLegacyBindingHealsOnce(t *testing.T) {
	ctx := context.Background()
	svc, store, runner := newChatService(t)

	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("第一次开对话失败：%v", err)
	}

	// Make the binding look "legacy": clear the fingerprint, leave the rest as is.
	b, err := store.GetArkBinding(ctx, seedWS, seedChatAgent)
	if err != nil {
		t.Fatalf("取绑定失败：%v", err)
	}
	b.SpecFingerprint = ""
	if err := store.PutArkBinding(ctx, b); err != nil {
		t.Fatalf("写绑定失败：%v", err)
	}

	// Second time: should catch up once (even though the agent changed nothing).
	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("再开对话失败：%v", err)
	}
	if len(runner.updatedSpecs) != 1 {
		t.Fatalf("存量绑定应当自愈一次，实际 update %d 次", len(runner.updatedSpecs))
	}

	// Third time: the fingerprint is stored now, so it should not call again.
	if _, err := svc.CreateConversation(ctx, seedWS, seedChatAgent); err != nil {
		t.Fatalf("第三次开对话失败：%v", err)
	}
	if len(runner.updatedSpecs) != 1 {
		t.Errorf("自愈应当恰好一次，实际 update %d 次——多于一次意味着每个 agent "+
			"每次对话都白打一个往返", len(runner.updatedSpecs))
	}
}

// TestArkSpecFingerprintChangesWithEveryField watches the inputs of the fingerprint.
//
// If the fingerprint misses a field, "changing that field has no effect", and **only for that one
// field**. So partial failures like "changing system works, changing model does not" are the
// hardest to track down.
//
// Here each field is changed once, and the fingerprint must change with it. **When adding a
// field, add a row to this table too**, otherwise the cross-check below fails.
func TestArkSpecFingerprintChangesWithEveryField(t *testing.T) {
	base := ArkAgentSpec{
		Name:        "n",
		Description: "d",
		System:      "s",
		Model:       "m",
		Speed:       "",
		Tools:       []string{"toolset-a"},
		CustomTools: []ma.CustomToolSpec{{
			Name: "ct", Description: "ctd",
			InputSchema: map[string]any{"a": map[string]any{"type": "string"}},
			Required:    []string{"a"},
		}},
	}
	baseFP := ArkSpecFingerprint(base)

	// One "change it" per field. Values are deliberately different from base, otherwise the
	// "mutation" does not actually change anything and the test passes falsely (a known trap in
	// this repo).
	mutate := map[string]func(*ArkAgentSpec){
		"Name":        func(s *ArkAgentSpec) { s.Name = "n2" },
		"Description": func(s *ArkAgentSpec) { s.Description = "d2" },
		"System":      func(s *ArkAgentSpec) { s.System = "s2" },
		"Model":       func(s *ArkAgentSpec) { s.Model = "m2" },
		"Speed":       func(s *ArkAgentSpec) { s.Speed = "fast" },
		"Tools":       func(s *ArkAgentSpec) { s.Tools = []string{"toolset-b"} },
		"CustomTools": func(s *ArkAgentSpec) {
			s.CustomTools = []ma.CustomToolSpec{{Name: "ct2", Description: "ctd2"}}
		},
		// Skills deliberately do **not** go into the spec fingerprint (skills are handled
		// separately by SkillsFingerprint), so it has its own reverse assertion, see below.
		"Skills": nil,
	}

	specType := reflect.TypeOf(ArkAgentSpec{})
	for i := 0; i < specType.NumField(); i++ {
		name := specType.Field(i).Name
		if _, ok := mutate[name]; !ok {
			t.Errorf("ArkAgentSpec 新增了字段 %s，但本用例的表里没有它——请补一条。"+
				"指纹漏掉一个字段的表现是「改那个字段不生效」，且只对那一个字段成立，"+
				"是最难查的一类。", name)
		}
	}

	for field, fn := range mutate {
		if fn == nil {
			continue
		}
		t.Run(field, func(t *testing.T) {
			got := base
			fn(&got)
			if ArkSpecFingerprint(got) == baseFP {
				t.Errorf("改了 %s 但指纹没变——那个字段的改动永远不会同步到方舟", field)
			}
		})
	}

	t.Run("Skills 不进 spec 指纹", func(t *testing.T) {
		got := base
		got.Skills = []persistmodel.ArkSkillRef{{ArkSkillID: "skill-ark-x", Version: "1"}}
		if ArkSpecFingerprint(got) != baseFP {
			t.Error("技能不该影响 spec 指纹——技能有自己的指纹（SkillsFingerprint），" +
				"混进来会让「换个技能」顺带触发一次什么都没改的 spec 更新")
		}
	})
}

// TestArkSpecFingerprintIsStableAndNeverEmpty pins down two basic properties of the fingerprint.
//
// **Stable**: computing it repeatedly for the same spec must give the same result.
// CustomTools.InputSchema is a map, and Go's iteration order is random. Without sorting, every
// computation gives a different fingerprint, so every chat calls UpdateAgent for nothing (and this
// bug is invisible in a test that only runs once).
//
// **Never empty**: the empty string is the special value for "unknown which version Ark has"
// (legacy bindings have no such key). If some spec also computed to an empty string, that agent
// would be permanently misjudged as "already caught up".
func TestArkSpecFingerprintIsStableAndNeverEmpty(t *testing.T) {
	spec := ArkAgentSpec{
		Name:   "n",
		Model:  "m",
		System: "s",
		CustomTools: []ma.CustomToolSpec{{
			Name: "ct",
			InputSchema: map[string]any{
				"a": map[string]any{"type": "string"},
				"b": map[string]any{"type": "number"},
				"c": map[string]any{"type": "boolean"},
				"d": map[string]any{"type": "object"},
				"e": map[string]any{"type": "array"},
			},
		}},
	}
	first := ArkSpecFingerprint(spec)
	// Compute several times: map iteration order is random, and one or two runs might match by chance.
	for i := 0; i < 50; i++ {
		if got := ArkSpecFingerprint(spec); got != first {
			t.Fatalf("同一份 spec 算出两个指纹（第 %d 次）：%q vs %q——"+
				"这会让每次开对话都白打一次 UpdateAgent", i+1, first, got)
		}
	}

	if ArkSpecFingerprint(ArkAgentSpec{}) == "" {
		t.Error("空 spec 的指纹也不能是空串：空串表示「不知道方舟侧是哪一份」，" +
			"两者相等会让那个 agent 被永久误判成已追平")
	}
}

// TestArkSpecFingerprintIsLengthPrefixed pins down the length prefix.
//
// Without a length prefix, moving a piece of text from the end of system to the start of
// description (or the other way around) gives the same fingerprint, so that change does not
// trigger catch-up.
func TestArkSpecFingerprintIsLengthPrefixed(t *testing.T) {
	a := ArkSpecFingerprint(ArkAgentSpec{Name: "ab", Description: "c"})
	b := ArkSpecFingerprint(ArkAgentSpec{Name: "a", Description: "bc"})
	if a == b {
		t.Error("相邻字段的内容挪一个字符过去算出了同一个指纹——" +
			"说明拼接时没带长度前缀，那类改动会静默不同步")
	}
}

// TestArkSpecFingerprintCarriesVersionPrefix pins down the version prefix.
//
// When the meaning of the fingerprint changes, changing the prefix makes all existing bindings
// mismatch once (see the reasoning in the skillsFingerprintVersion section: no SQL migration;
// "has it run" is encoded in the data itself).
func TestArkSpecFingerprintCarriesVersionPrefix(t *testing.T) {
	fp := ArkSpecFingerprint(ArkAgentSpec{Name: "n"})
	if !strings.HasPrefix(fp, arkSpecFingerprintVersion) {
		t.Errorf("指纹应当带版本前缀 %q，实际 %q——没有它，以后「指纹含义变了」"+
			"就只能走 SQL 迁移", arkSpecFingerprintVersion, fp)
	}
}
